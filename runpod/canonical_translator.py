"""
CanonicalTranslator: Convert raw model output to WatchPoint canonical variables.

Translates native model variables (Kelvin temps, m/s winds, specific humidity,
raw precipitation) into the 5 canonical variables expected by the downstream
Zarr schema:
  - temperature_c (Celsius)
  - wind_speed_kmh (km/h)
  - precipitation_mm (mm)
  - precipitation_probability (0-100%)
  - humidity_percent (0-100%)

Pure numpy/xarray — no Earth-2 Studio dependency, fully unit-testable.

References:
  - 11-runpod.md Section 3.4 (Canonical Variables)
  - 11-runpod.md Section 7.3 (Output Validation)
"""

from __future__ import annotations

import logging
from typing import Any

import numpy as np
import xarray as xr

logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Physical bounds for validation (11-runpod.md Section 7.3)
# ---------------------------------------------------------------------------
VARIABLE_BOUNDS: dict[str, tuple[float, float]] = {
    "temperature_c": (-90.0, 65.0),
    "wind_speed_kmh": (0.0, 500.0),
    "precipitation_mm": (0.0, 1000.0),
    "precipitation_probability": (0.0, 100.0),
    "humidity_percent": (0.0, 100.0),
}


def translate_atlas(raw: xr.Dataset) -> xr.Dataset:
    """
    Translate Atlas (medium-range) raw output to canonical variables.

    Expected raw variables (E2S Atlas output, 76 variables total):
      - t2m: 2-meter temperature (K)
      - u10m: 10-meter U-wind component (m/s)
      - v10m: 10-meter V-wind component (m/s)
      - tp: total precipitation (m)
      - q1000: specific humidity at 1000 hPa (kg/kg) — near-surface level
      - sp: surface pressure (Pa)

    Returns
    -------
    xr.Dataset
        Dataset with 5 canonical variables, all float32.
    """
    # Earth2studio outputs dims (time=1, lead_time=N, lat, lon) where time is
    # the batch/init dimension. Squeeze the batch dim and rename lead_time→time
    # to get (time, lat, lon) for our canonical format.
    raw = _normalize_e2s_dims(raw)

    # Temperature: Kelvin -> Celsius
    temperature_c = raw["t2m"].values - 273.15

    # Wind speed: component vectors -> magnitude in km/h
    u10 = raw["u10m"].values
    v10 = raw["v10m"].values
    wind_speed_kmh = np.sqrt(u10**2 + v10**2) * 3.6

    # Precipitation: meters -> millimeters
    precipitation_mm = raw["tp"].values * 1000.0

    # Humidity: specific humidity -> relative humidity via Tetens formula
    # Atlas outputs q at pressure levels (q50..q1000), not bare q.
    # Use q1000 (1000 hPa, near surface) for consistency with t2m and sp.
    humidity_percent = _specific_to_relative_humidity(
        q=raw["q1000"].values,
        t2m=raw["t2m"].values,
        sp=raw["sp"].values,
    )

    # Precipitation probability: sigmoid calibration on precipitation amount
    # v1 approximation — maps precipitation_mm to a 0-100% probability
    precipitation_probability = _sigmoid_precip_probability(precipitation_mm)

    # Assemble canonical dataset
    coords = {
        "time": raw["time"],
        "lat": raw["lat"],
        "lon": raw["lon"],
    }
    dims = ["time", "lat", "lon"]

    ds = xr.Dataset(
        {
            "temperature_c": (dims, temperature_c.astype(np.float32)),
            "wind_speed_kmh": (dims, wind_speed_kmh.astype(np.float32)),
            "precipitation_mm": (dims, np.clip(precipitation_mm, 0.0, None).astype(np.float32)),
            "precipitation_probability": (dims, precipitation_probability.astype(np.float32)),
            "humidity_percent": (dims, humidity_percent.astype(np.float32)),
        },
        coords=coords,
    )

    return ds


def translate_nowcast(
    stormscope_refc: np.ndarray,
    gfs_data: xr.Dataset,
    calibration: dict[str, Any],
    *,
    nowcast_coords: dict[str, Any],
) -> xr.Dataset:
    """
    Translate StormScope nowcast output + GFS backfill to canonical variables.

    Parameters
    ----------
    stormscope_refc : np.ndarray
        Composite reflectivity predictions (dBZ), shape (time, lat, lon).
        Already regridded to the target 361x240 lat/lon grid.
    gfs_data : xr.Dataset
        GFS analysis data with t2m, u10m, v10m, q, sp — regridded to 361x240 CONUS.
    calibration : dict
        Calibration coefficients from input_config (spec Section 5.2).
    nowcast_coords : dict
        Must contain 'time', 'lat', 'lon' coordinate arrays.

    Returns
    -------
    xr.Dataset
        Dataset with 5 canonical variables, all float32.
    """
    n_times = stormscope_refc.shape[0]

    # Precipitation from reflectivity: Marshall-Palmer Z-R relation
    # R (mm/h) = (10^(dBZ/10) / 200) ^ (1/1.6)
    # Accumulated over 1h steps -> mm
    z_linear = np.power(10.0, stormscope_refc / 10.0)
    rain_rate_mmh = np.power(z_linear / 200.0, 1.0 / 1.6)
    # Negative reflectivity (no echo) -> 0 rain
    rain_rate_mmh = np.where(stormscope_refc > 0, rain_rate_mmh, 0.0)
    precipitation_mm = rain_rate_mmh  # 1h steps, so rate = accumulation

    # Precipitation probability from reflectivity (spec Section 5.2)
    a = calibration.get("refc_prob_a", 0.08)
    b = calibration.get("refc_prob_b", 20.0)
    precipitation_probability = 100.0 / (1.0 + np.exp(-a * (stormscope_refc - b)))

    # GFS backfill for temp, wind, humidity — replicate across nowcast time steps
    gfs_t2m = gfs_data["t2m"].values  # (1, lat, lon) or (lat, lon)
    gfs_u10 = gfs_data["u10m"].values
    gfs_v10 = gfs_data["v10m"].values
    gfs_q = gfs_data["q"].values
    gfs_sp = gfs_data["sp"].values

    # Squeeze to 2D if needed, then tile across time
    def _to_3d(arr: np.ndarray) -> np.ndarray:
        arr = np.squeeze(arr)
        if arr.ndim == 2:
            return np.tile(arr[np.newaxis, :, :], (n_times, 1, 1))
        return arr

    gfs_t2m = _to_3d(gfs_t2m)
    gfs_u10 = _to_3d(gfs_u10)
    gfs_v10 = _to_3d(gfs_v10)
    gfs_q = _to_3d(gfs_q)
    gfs_sp = _to_3d(gfs_sp)

    temperature_c = gfs_t2m - 273.15
    wind_speed_kmh = np.sqrt(gfs_u10**2 + gfs_v10**2) * 3.6
    humidity_percent = _specific_to_relative_humidity(gfs_q, gfs_t2m, gfs_sp)

    # Assemble canonical dataset
    dims = ["time", "lat", "lon"]
    ds = xr.Dataset(
        {
            "temperature_c": (dims, temperature_c.astype(np.float32)),
            "wind_speed_kmh": (dims, wind_speed_kmh.astype(np.float32)),
            "precipitation_mm": (dims, np.clip(precipitation_mm, 0.0, None).astype(np.float32)),
            "precipitation_probability": (dims, precipitation_probability.astype(np.float32)),
            "humidity_percent": (dims, humidity_percent.astype(np.float32)),
        },
        coords=nowcast_coords,
    )

    return ds


def validate(ds: xr.Dataset) -> None:
    """
    Validate canonical dataset against physical bounds (Section 7.3).

    Raises ValueError if any variable is out of bounds or contains NaN/Inf.
    """
    for var_name, (lo, hi) in VARIABLE_BOUNDS.items():
        if var_name not in ds.data_vars:
            raise ValueError(f"Missing canonical variable: {var_name}")

        data = ds[var_name].values
        if not np.all(np.isfinite(data)):
            non_finite = int(np.sum(~np.isfinite(data)))
            raise ValueError(
                f"Variable '{var_name}' contains {non_finite} non-finite values"
            )

        below = np.sum(data < lo)
        above = np.sum(data > hi)
        if below > 0 or above > 0:
            raise ValueError(
                f"Variable '{var_name}' out of bounds [{lo}, {hi}]: "
                f"{int(below)} below, {int(above)} above"
            )


# ---------------------------------------------------------------------------
# Internal helpers
# ---------------------------------------------------------------------------

def _normalize_e2s_dims(ds: xr.Dataset) -> xr.Dataset:
    """
    Normalize earth2studio output dimensions to (time, lat, lon).

    E2S deterministic() outputs with dims (time, lead_time, lat, lon) where
    'time' is the batch/init dimension (size 1) and 'lead_time' holds the
    actual forecast steps. Squeeze batch and rename lead_time→time.
    """
    sample_var = next(iter(ds.data_vars))
    dims = ds[sample_var].dims
    logger.info("Raw e2s dimensions: %s, shape: %s", dims, ds[sample_var].shape)

    if "lead_time" in dims and "time" in dims:
        # Squeeze the batch 'time' dimension (size 1) and rename lead_time
        ds = ds.isel(time=0, drop=True)
        ds = ds.rename({"lead_time": "time"})
        logger.info("Normalized dims: squeezed batch, renamed lead_time→time")
    elif "lead_time" in dims:
        ds = ds.rename({"lead_time": "time"})
        logger.info("Normalized dims: renamed lead_time→time")

    sample_var_after = next(iter(ds.data_vars))
    logger.info("After normalization: dims=%s, shape=%s",
                ds[sample_var_after].dims, ds[sample_var_after].shape)
    return ds


def _specific_to_relative_humidity(
    q: np.ndarray,
    t2m: np.ndarray,
    sp: np.ndarray,
) -> np.ndarray:
    """
    Convert specific humidity to relative humidity using the Tetens formula.

    Parameters
    ----------
    q : np.ndarray
        Specific humidity (kg/kg).
    t2m : np.ndarray
        2-meter temperature (K).
    sp : np.ndarray
        Surface pressure (Pa).

    Returns
    -------
    np.ndarray
        Relative humidity in percent (0-100), clipped.
    """
    # Tetens formula for saturation vapor pressure (Pa)
    t_celsius = t2m - 273.15
    e_sat = 611.2 * np.exp(17.67 * t_celsius / (t_celsius + 243.5))

    # Actual vapor pressure from specific humidity
    # e = q * sp / (0.622 + 0.378 * q)
    e_actual = q * sp / (0.622 + 0.378 * q)

    # Relative humidity
    rh = (e_actual / e_sat) * 100.0

    return np.clip(rh, 0.0, 100.0)


def _sigmoid_precip_probability(precip_mm: np.ndarray) -> np.ndarray:
    """
    Map precipitation amount to probability using a sigmoid function.

    v1 calibration: probability increases with precipitation amount.
    At 0 mm -> ~2.7%, at 1 mm -> ~50%, at 5 mm -> ~99%.

    Returns values in [0, 100].
    """
    # Sigmoid: 1 / (1 + exp(-k * (x - x0)))
    # k=3.0, x0=1.0 gives reasonable mapping
    prob = 1.0 / (1.0 + np.exp(-3.0 * (precip_mm - 1.0)))
    return np.clip(prob * 100.0, 0.0, 100.0)
