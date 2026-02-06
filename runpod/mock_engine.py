"""
MockEngine: Generates synthetic forecast data for local development and CI/CD.

Implements the ModelEngine ABC from the RunPod architecture (11-runpod.md Section 4).
Bypasses GPU inference by generating random numpy arrays matching the Zarr schema
for both Atlas (global medium-range) and StormScope (CONUS nowcast) models.

Triggered by:
  - options.mock_inference = true in the inference payload
  - MOCK_INFERENCE=true environment variable
"""

from __future__ import annotations

import os
from abc import ABC, abstractmethod
from datetime import datetime, timezone
from typing import Optional

import numpy as np
import xarray as xr


# ---------------------------------------------------------------------------
# Canonical variables as defined in 11-runpod.md Section 3.4
# ---------------------------------------------------------------------------
CANONICAL_VARIABLES: list[str] = [
    "precipitation_mm",
    "precipitation_probability",
    "temperature_c",
    "wind_speed_kmh",
    "humidity_percent",
]

# ---------------------------------------------------------------------------
# Model grid specifications
# ---------------------------------------------------------------------------
# Atlas (Global, 0.25-degree resolution)
ATLAS_LAT_SIZE = 721       # 90.0 to -90.0 at 0.25 degree steps
ATLAS_LON_SIZE = 1440      # -180.0 to 179.75 at 0.25 degree steps

# StormScope / HRRR (CONUS region)
# Task spec: lat=361 points covering 20N-65N, lon=240 points covering 130W-70W
STORMSCOPE_LAT_SIZE = 361
STORMSCOPE_LON_SIZE = 240

# Physical bounds for realistic mock data generation
VARIABLE_BOUNDS: dict[str, tuple[float, float]] = {
    "precipitation_mm":          (0.0, 50.0),
    "precipitation_probability": (0.0, 100.0),
    "temperature_c":             (-40.0, 50.0),
    "wind_speed_kmh":            (0.0, 150.0),
    "humidity_percent":          (0.0, 100.0),
}


class ModelEngine(ABC):
    """
    Abstract base for GPU inference engines.

    Defined in 11-runpod.md Section 4.1 -- reproduced here as the ABC
    that MockEngine implements.
    """

    @abstractmethod
    def load_weights(self, path: str) -> None:
        """Load model weights from disk or network volume."""
        ...

    @abstractmethod
    def predict(self, input_xr: xr.Dataset) -> xr.Dataset:
        """Run inference and return a canonical-variable Dataset."""
        ...


class MockEngine(ModelEngine):
    """
    Mock implementation of ModelEngine for testing without GPU.

    Generates physically-bounded random data matching the exact grid
    dimensions required by each model type (Atlas / StormScope).
    """

    def __init__(
        self,
        model: str = "medium_range",
        num_time_steps: int = 60,
        seed: Optional[int] = None,
    ) -> None:
        """
        Parameters
        ----------
        model : str
            One of "medium_range" (Atlas) or "nowcast" (StormScope).
        num_time_steps : int
            Number of forecast time steps to generate (default 60).
        seed : int, optional
            Random seed for reproducibility. If None, a fresh random state
            is used each time.
        """
        self.model = model
        self.num_time_steps = num_time_steps
        self._rng = np.random.default_rng(seed)

    # ------------------------------------------------------------------
    # ModelEngine interface
    # ------------------------------------------------------------------

    def load_weights(self, path: str) -> None:
        """No-op for mock engine -- no weights to load."""
        pass

    def predict(self, input_xr: xr.Dataset) -> xr.Dataset:
        """
        Generate a synthetic forecast Dataset.

        The *input_xr* parameter is accepted for interface compatibility
        but is ignored; the mock engine synthesizes data from scratch.

        Returns an xarray.Dataset with:
          - coords: time (Unix int64), lat (float32), lon (float32)
          - data_vars: all CANONICAL_VARIABLES as float32 arrays
        """
        lat_coords, lon_coords = self._build_coordinates()
        time_coords = self._build_time_coords()

        data_vars: dict[str, tuple] = {}
        for var_name in CANONICAL_VARIABLES:
            lo, hi = VARIABLE_BOUNDS[var_name]
            arr = self._rng.uniform(
                lo,
                hi,
                size=(self.num_time_steps, len(lat_coords), len(lon_coords)),
            ).astype(np.float32)
            data_vars[var_name] = (["time", "lat", "lon"], arr)

        ds = xr.Dataset(
            data_vars=data_vars,
            coords={
                "time": ("time", time_coords),
                "lat": ("lat", lat_coords),
                "lon": ("lon", lon_coords),
            },
        )

        return ds

    # ------------------------------------------------------------------
    # Internal helpers
    # ------------------------------------------------------------------

    def _build_coordinates(self) -> tuple[np.ndarray, np.ndarray]:
        """
        Build lat/lon coordinate arrays for the configured model.

        Atlas (medium_range):
          lat: 90.0 to -90.0 (descending), 721 points at 0.25 degree
          lon: -180.0 to 179.75 (ascending), 1440 points at 0.25 degree

        StormScope (nowcast):
          lat: 65.0 to 20.0 (descending), 361 points (CONUS 20N-65N)
          lon: -130.0 to -70.25 (ascending), 240 points (130W-70W)
        """
        if self.model == "medium_range":
            lat = np.linspace(90.0, -90.0, ATLAS_LAT_SIZE, dtype=np.float32)
            lon = np.linspace(-180.0, 179.75, ATLAS_LON_SIZE, dtype=np.float32)
        elif self.model == "nowcast":
            lat = np.linspace(65.0, 20.0, STORMSCOPE_LAT_SIZE, dtype=np.float32)
            lon = np.linspace(-130.0, -70.25, STORMSCOPE_LON_SIZE, dtype=np.float32)
        else:
            raise ValueError(
                f"Unknown model type: {self.model!r}. "
                f"Expected 'medium_range' or 'nowcast'."
            )
        return lat, lon

    def _build_time_coords(self) -> np.ndarray:
        """
        Build time coordinate array as int64 Unix timestamps.

        Starts from the current UTC hour, stepping by 1 hour.
        """
        now = datetime.now(timezone.utc).replace(minute=0, second=0, microsecond=0)
        base_ts = int(now.timestamp())
        return np.arange(
            base_ts,
            base_ts + self.num_time_steps * 3600,
            3600,
            dtype=np.int64,
        )


def is_mock_mode(options: Optional[dict] = None) -> bool:
    """
    Determine whether mock inference mode is active.

    Returns True if:
      - options dict has mock_inference=True, OR
      - MOCK_INFERENCE environment variable is set to a truthy value
    """
    if options and options.get("mock_inference", False):
        return True
    return os.environ.get("MOCK_INFERENCE", "").lower() in ("true", "1", "yes")
