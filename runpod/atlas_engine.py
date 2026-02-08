"""
AtlasEngine: Medium-range global forecast engine using NVIDIA Earth-2 Studio Atlas.

Wraps the E2S Atlas model via `earth2studio.run.deterministic` for a simple
end-to-end workflow: data fetch (GFS) -> inference -> canonical variable output.

Atlas produces 15-day global forecasts at 0.25-degree resolution (721x1440 grid)
with 6-hour time steps (60 steps for full 15-day horizon).

GPU requirement: A100 80GB (Atlas has 4.3B parameters, ~40GB VRAM needed).

References:
  - 11-runpod.md Section 4.1 (ModelEngine ABC)
  - earth2studio docs: Atlas deterministic workflow
"""

from __future__ import annotations

import logging
import os
from typing import Any

import numpy as np
import xarray as xr

from mock_engine import ModelEngine
from canonical_translator import translate_atlas, validate

logger = logging.getLogger(__name__)

# Atlas produces forecasts at 6h intervals, 60 steps = 15 days
ATLAS_NSTEPS = 60


class AtlasEngine(ModelEngine):
    """
    Real GPU inference engine for Atlas (medium-range global forecasts).

    Uses Earth-2 Studio's deterministic workflow which handles:
      - Automatic GFS data fetching from AWS
      - Model inference on GPU
      - Output as in-memory Zarr store
    """

    def __init__(self) -> None:
        self._model = None

    def load_weights(self, path: str | None) -> None:
        """
        Load Atlas model weights via Earth-2 Studio.

        Weights are auto-downloaded from HuggingFace (hf://nvidia/atlas-era5)
        and cached at EARTH2STUDIO_CACHE. Subsequent loads use the cache.

        Parameters
        ----------
        path : str or None
            Cache directory for E2S. If None, uses the default
            /runpod-volume/weights/earth2studio.
        """
        cache_dir = path or "/runpod-volume/weights/earth2studio"
        os.environ["EARTH2STUDIO_CACHE"] = cache_dir
        logger.info("EARTH2STUDIO_CACHE set to %s", cache_dir)

        from earth2studio.models.px import Atlas

        package = Atlas.load_default_package()
        self._model = Atlas.load_model(package)
        logger.info("Atlas model loaded successfully")

    def predict(
        self,
        run_timestamp: str,
        input_config: dict[str, Any] | None = None,
    ) -> xr.Dataset:
        """
        Run Atlas inference for the given timestamp.

        Parameters
        ----------
        run_timestamp : str
            ISO 8601 timestamp for the forecast initialization time.
            E2S uses this to fetch the corresponding GFS analysis data.
        input_config : dict, optional
            Not used by Atlas (calibration is for nowcast only).

        Returns
        -------
        xr.Dataset
            Canonical-variable dataset with dimensions (time, lat, lon).
            time: 61 steps (T+0 through T+360h at 6h intervals)
            lat: 721 points (90.0 to -90.0 at 0.25 degree)
            lon: 1440 points (-180.0 to 179.75 at 0.25 degree)
        """
        if self._model is None:
            raise RuntimeError("Model not loaded. Call load_weights() first.")

        import earth2studio.run as run
        from earth2studio.data import GFS
        from earth2studio.io import ZarrBackend

        logger.info("Starting Atlas inference for run_timestamp=%s", run_timestamp)

        # E2S deterministic workflow: fetch data + run inference + write output
        io = ZarrBackend()
        io = run.deterministic(
            time=[run_timestamp],
            nsteps=ATLAS_NSTEPS,
            prognostic=self._model,
            data=GFS(),
            io=io,
        )

        # Extract as xarray Dataset from the in-memory Zarr store
        raw_ds = xr.open_zarr(io.root.store)
        logger.info("Raw Atlas output shape: %s", dict(raw_ds.sizes))

        # Handle longitude convention: E2S Atlas uses 0-360, we use -180 to 180
        raw_ds = self._convert_longitude(raw_ds)

        # Translate raw variables to canonical format
        canonical_ds = translate_atlas(raw_ds)

        # Validate physical bounds
        validate(canonical_ds)

        logger.info("Atlas inference complete: %s", dict(canonical_ds.sizes))
        return canonical_ds

    @staticmethod
    def _convert_longitude(ds: xr.Dataset) -> xr.Dataset:
        """
        Convert longitude from 0-360 to -180..180 convention.

        E2S Atlas outputs longitude in [0, 360). Our system uses [-180, 180).
        This rolls the data arrays so that the Prime Meridian is in the center
        and longitude increases from -180 to 179.75.
        """
        lon = ds["lon"].values
        if lon.max() > 180.0:
            logger.info("Converting longitude from 0-360 to -180..180")
            # Convert lon values
            new_lon = np.where(lon > 180, lon - 360, lon)
            # Sort to get -180..180 order
            sort_idx = np.argsort(new_lon)
            new_lon = new_lon[sort_idx]

            # Roll data arrays along lon dimension
            data_vars = {}
            for var_name in ds.data_vars:
                data = ds[var_name].values
                # Roll along the lon axis (last axis)
                data_vars[var_name] = (ds[var_name].dims, data[..., sort_idx])

            ds = xr.Dataset(
                data_vars,
                coords={
                    "time": ds["time"],
                    "lat": ds["lat"],
                    "lon": ("lon", new_lon.astype(np.float32)),
                },
            )

        return ds
