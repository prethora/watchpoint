"""
Unit tests for CanonicalTranslator.

Tests each conversion formula with synthetic data to verify correctness
of unit conversions and physical bounds validation.
"""

import numpy as np
import pytest
import xarray as xr

from canonical_translator import (
    VARIABLE_BOUNDS,
    _sigmoid_precip_probability,
    _specific_to_relative_humidity,
    translate_atlas,
    translate_nowcast,
    validate,
)


# ---------------------------------------------------------------------------
# Helper to build synthetic Atlas-like raw datasets
# ---------------------------------------------------------------------------

def _make_atlas_raw(
    t2m_k: float = 300.0,
    u10m: float = 5.0,
    v10m: float = 3.0,
    tp_m: float = 0.002,
    q: float = 0.008,
    sp: float = 101325.0,
    n_time: int = 3,
    n_lat: int = 5,
    n_lon: int = 5,
) -> xr.Dataset:
    """Create a minimal Atlas-style raw dataset with uniform values."""
    shape = (n_time, n_lat, n_lon)
    return xr.Dataset(
        {
            "t2m": (["time", "lat", "lon"], np.full(shape, t2m_k, dtype=np.float32)),
            "u10m": (["time", "lat", "lon"], np.full(shape, u10m, dtype=np.float32)),
            "v10m": (["time", "lat", "lon"], np.full(shape, v10m, dtype=np.float32)),
            "tp": (["time", "lat", "lon"], np.full(shape, tp_m, dtype=np.float32)),
            "q": (["time", "lat", "lon"], np.full(shape, q, dtype=np.float32)),
            "sp": (["time", "lat", "lon"], np.full(shape, sp, dtype=np.float32)),
        },
        coords={
            "time": np.arange(n_time, dtype=np.int64),
            "lat": np.linspace(90, -90, n_lat, dtype=np.float32),
            "lon": np.linspace(-180, 179, n_lon, dtype=np.float32),
        },
    )


# ---------------------------------------------------------------------------
# translate_atlas tests
# ---------------------------------------------------------------------------

class TestTranslateAtlas:
    """Tests for Atlas raw -> canonical translation."""

    def test_temperature_conversion(self):
        """t2m in Kelvin -> temperature_c in Celsius."""
        raw = _make_atlas_raw(t2m_k=300.0)
        ds = translate_atlas(raw)

        expected = 300.0 - 273.15  # 26.85
        np.testing.assert_allclose(ds["temperature_c"].values, expected, atol=0.01)

    def test_temperature_freezing(self):
        """0C = 273.15K."""
        raw = _make_atlas_raw(t2m_k=273.15)
        ds = translate_atlas(raw)

        np.testing.assert_allclose(ds["temperature_c"].values, 0.0, atol=0.01)

    def test_wind_speed_conversion(self):
        """sqrt(u^2 + v^2) * 3.6 gives km/h."""
        raw = _make_atlas_raw(u10m=3.0, v10m=4.0)
        ds = translate_atlas(raw)

        # sqrt(9+16) = 5 m/s -> 18 km/h
        expected = 5.0 * 3.6  # 18.0
        np.testing.assert_allclose(ds["wind_speed_kmh"].values, expected, atol=0.01)

    def test_wind_speed_zero(self):
        """Zero wind components -> zero wind speed."""
        raw = _make_atlas_raw(u10m=0.0, v10m=0.0)
        ds = translate_atlas(raw)

        np.testing.assert_allclose(ds["wind_speed_kmh"].values, 0.0, atol=0.01)

    def test_precipitation_conversion(self):
        """tp in meters -> precipitation_mm in millimeters."""
        raw = _make_atlas_raw(tp_m=0.005)
        ds = translate_atlas(raw)

        expected = 5.0  # 0.005m * 1000
        np.testing.assert_allclose(ds["precipitation_mm"].values, expected, atol=0.01)

    def test_precipitation_no_negative(self):
        """Negative precipitation should be clipped to 0."""
        raw = _make_atlas_raw(tp_m=-0.001)
        ds = translate_atlas(raw)

        assert np.all(ds["precipitation_mm"].values >= 0.0)

    def test_all_canonical_variables_present(self):
        """Output should have all 5 canonical variables."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)

        for var_name in VARIABLE_BOUNDS:
            assert var_name in ds.data_vars, f"Missing: {var_name}"

    def test_output_dtype_float32(self):
        """All output variables should be float32."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)

        for var_name in ds.data_vars:
            assert ds[var_name].dtype == np.float32

    def test_output_dimensions(self):
        """Output should have (time, lat, lon) dimensions."""
        raw = _make_atlas_raw(n_time=4, n_lat=7, n_lon=10)
        ds = translate_atlas(raw)

        assert ds.sizes["time"] == 4
        assert ds.sizes["lat"] == 7
        assert ds.sizes["lon"] == 10


# ---------------------------------------------------------------------------
# Humidity conversion tests
# ---------------------------------------------------------------------------

class TestHumidityConversion:
    """Tests for specific -> relative humidity via Tetens formula."""

    def test_reasonable_tropical_humidity(self):
        """Warm + moist conditions should give high RH."""
        # Tropical: 30C, high specific humidity
        rh = _specific_to_relative_humidity(
            q=np.array([0.020]),
            t2m=np.array([303.15]),  # 30C
            sp=np.array([101325.0]),
        )
        # Should be high but not necessarily 100%
        assert rh[0] > 50.0
        assert rh[0] <= 100.0

    def test_dry_conditions(self):
        """Very low specific humidity should give low RH."""
        rh = _specific_to_relative_humidity(
            q=np.array([0.001]),
            t2m=np.array([300.0]),
            sp=np.array([101325.0]),
        )
        assert rh[0] < 50.0
        assert rh[0] >= 0.0

    def test_clipped_to_bounds(self):
        """Output should be clipped to [0, 100]."""
        rh = _specific_to_relative_humidity(
            q=np.array([0.05]),  # Extreme humidity
            t2m=np.array([270.0]),  # Cold
            sp=np.array([101325.0]),
        )
        assert np.all(rh >= 0.0)
        assert np.all(rh <= 100.0)


# ---------------------------------------------------------------------------
# Precipitation probability tests
# ---------------------------------------------------------------------------

class TestSigmoidPrecipProbability:
    """Tests for sigmoid-based precipitation probability."""

    def test_zero_precip_low_probability(self):
        """No precipitation -> low probability."""
        prob = _sigmoid_precip_probability(np.array([0.0]))
        assert prob[0] < 10.0

    def test_moderate_precip_mid_probability(self):
        """~1mm precipitation -> ~50% probability."""
        prob = _sigmoid_precip_probability(np.array([1.0]))
        assert 40.0 < prob[0] < 60.0

    def test_heavy_precip_high_probability(self):
        """Heavy precipitation -> high probability."""
        prob = _sigmoid_precip_probability(np.array([5.0]))
        assert prob[0] > 90.0

    def test_range_bounds(self):
        """Output should always be in [0, 100]."""
        precip = np.linspace(0, 100, 200)
        prob = _sigmoid_precip_probability(precip)
        assert np.all(prob >= 0.0)
        assert np.all(prob <= 100.0)


# ---------------------------------------------------------------------------
# translate_nowcast tests
# ---------------------------------------------------------------------------

class TestTranslateNowcast:
    """Tests for nowcast (StormScope + GFS) -> canonical translation."""

    def _make_nowcast_inputs(self):
        """Create synthetic nowcast inputs."""
        n_time, n_lat, n_lon = 6, 10, 10

        # StormScope refc (dBZ) — moderate reflectivity
        refc = np.full((n_time, n_lat, n_lon), 30.0, dtype=np.float64)

        # GFS backfill data
        gfs = xr.Dataset(
            {
                "t2m": (["lat", "lon"], np.full((n_lat, n_lon), 295.0)),
                "u10m": (["lat", "lon"], np.full((n_lat, n_lon), 2.0)),
                "v10m": (["lat", "lon"], np.full((n_lat, n_lon), 3.0)),
                "q": (["lat", "lon"], np.full((n_lat, n_lon), 0.01)),
                "sp": (["lat", "lon"], np.full((n_lat, n_lon), 101325.0)),
            },
        )

        coords = {
            "time": ("time", np.arange(n_time, dtype=np.int64)),
            "lat": ("lat", np.linspace(65, 20, n_lat, dtype=np.float32)),
            "lon": ("lon", np.linspace(-130, -70, n_lon, dtype=np.float32)),
        }

        calibration = {"refc_prob_a": 0.08, "refc_prob_b": 20.0}

        return refc, gfs, calibration, coords

    def test_all_variables_present(self):
        """Output should have all 5 canonical variables."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        for var_name in VARIABLE_BOUNDS:
            assert var_name in ds.data_vars

    def test_output_shape(self):
        """Output dimensions match inputs."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        assert ds.sizes["time"] == 6
        assert ds.sizes["lat"] == 10
        assert ds.sizes["lon"] == 10

    def test_temperature_from_gfs(self):
        """Temperature should come from GFS (295K -> 21.85C)."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        expected = 295.0 - 273.15
        np.testing.assert_allclose(ds["temperature_c"].values, expected, atol=0.01)

    def test_wind_from_gfs(self):
        """Wind speed should come from GFS (u=2, v=3 -> sqrt(13)*3.6)."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        expected = np.sqrt(4 + 9) * 3.6
        np.testing.assert_allclose(ds["wind_speed_kmh"].values, expected, atol=0.01)

    def test_no_echo_no_precip(self):
        """Zero reflectivity -> zero precipitation."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        refc[:] = 0.0
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        np.testing.assert_allclose(ds["precipitation_mm"].values, 0.0, atol=0.01)

    def test_negative_refc_no_precip(self):
        """Negative reflectivity (no echo) -> zero precipitation."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        refc[:] = -10.0
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        np.testing.assert_allclose(ds["precipitation_mm"].values, 0.0, atol=0.01)

    def test_output_dtype(self):
        """All variables should be float32."""
        refc, gfs, cal, coords = self._make_nowcast_inputs()
        ds = translate_nowcast(refc, gfs, cal, nowcast_coords=coords)

        for var_name in ds.data_vars:
            assert ds[var_name].dtype == np.float32


# ---------------------------------------------------------------------------
# validate tests
# ---------------------------------------------------------------------------

class TestValidate:
    """Tests for physical bounds validation."""

    def test_valid_dataset_passes(self):
        """A dataset within bounds should not raise."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)
        validate(ds)  # Should not raise

    def test_nan_fails(self):
        """NaN values should fail validation."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)
        ds["temperature_c"].values[0, 0, 0] = np.nan

        with pytest.raises(ValueError, match="non-finite"):
            validate(ds)

    def test_inf_fails(self):
        """Inf values should fail validation."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)
        ds["temperature_c"].values[0, 0, 0] = np.inf

        with pytest.raises(ValueError, match="non-finite"):
            validate(ds)

    def test_out_of_bounds_temp_fails(self):
        """Temperature outside [-90, 65] should fail."""
        raw = _make_atlas_raw()
        ds = translate_atlas(raw)
        ds["temperature_c"].values[0, 0, 0] = 100.0  # > 65

        with pytest.raises(ValueError, match="out of bounds"):
            validate(ds)

    def test_missing_variable_fails(self):
        """Missing canonical variable should fail."""
        ds = xr.Dataset(
            {"temperature_c": (["time", "lat", "lon"], np.zeros((1, 2, 2)))},
        )
        with pytest.raises(ValueError, match="Missing"):
            validate(ds)
