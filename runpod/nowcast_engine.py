"""
NowcastEngine: Short-range precipitation nowcast using NVIDIA StormScope.

Implements a custom inference loop with two coupled models:
  1. StormScopeGOES: Satellite forecasting, conditioned on GFS forecast data
  2. StormScopeMRMS: Radar forecasting, conditioned on GOES predictions

The two models run sequentially in an autoregressive loop: each step, GOES
produces predicted satellite channels, then MRMS produces predicted radar
(composite reflectivity) conditioned on the GOES output.

Temperature, wind, and humidity are backfilled from GFS analysis data since
StormScope only predicts radar/satellite variables.

GPU requirement: Moderate (~260M params, fits easily on A100).

References:
  - 11-runpod.md Section 4.1 (ModelEngine ABC)
  - E2S StormScope example: coupled GOES+MRMS workflow
  - 11-runpod.md Section 5.2 (Nowcast calibration)
"""

from __future__ import annotations

import logging
import os
from datetime import datetime, timedelta, timezone
from typing import Any

import numpy as np
import torch
import xarray as xr

from mock_engine import ModelEngine
from canonical_translator import translate_nowcast, validate
from regridder import Regridder

logger = logging.getLogger(__name__)

# Nowcast produces 6 hourly steps (6h total horizon)
NOWCAST_STEPS = 6

# StormScope model variant: 6km resolution, 60-minute steps
GOES_MODEL_NAME = "6km_60min_natten_cos_zenith_input_eoe_v2"
MRMS_MODEL_NAME = "6km_60min_natten_cos_zenith_input_mrms_eoe"

# GFS variables needed for backfill
GFS_BACKFILL_VARS = ["t2m", "u10m", "v10m", "q", "sp"]


class NowcastEngine(ModelEngine):
    """
    Real GPU inference engine for nowcast (StormScope + GFS backfill).

    Uses a custom autoregressive loop with coupled StormScopeGOES and
    StormScopeMRMS models, unlike Atlas which uses the simpler
    run.deterministic workflow.
    """

    def __init__(self) -> None:
        self._goes_model = None
        self._mrms_model = None
        self._regridder = Regridder()
        self._device = torch.device("cuda" if torch.cuda.is_available() else "cpu")

    def load_weights(self, path: str | None) -> None:
        """
        Load StormScope model weights via Earth-2 Studio.

        Loads both StormScopeGOES and StormScopeMRMS from the same base
        package. Weights are auto-downloaded from HuggingFace
        (hf://nvidia/stormscope-goes-mrms) and cached at EARTH2STUDIO_CACHE.

        Parameters
        ----------
        path : str or None
            Cache directory for E2S. If None, uses the default.
        """
        cache_dir = path or "/runpod-volume/weights/earth2studio"
        os.environ["EARTH2STUDIO_CACHE"] = cache_dir
        logger.info("EARTH2STUDIO_CACHE set to %s", cache_dir)

        from earth2studio.models.px.stormscope import (
            StormScopeBase,
            StormScopeGOES,
            StormScopeMRMS,
        )
        from earth2studio.data import GFS_FX, GOES

        # Load the shared base package
        package = StormScopeBase.load_default_package()

        # GOES model: satellite forecasting, conditioned on GFS forecast
        self._goes_model = StormScopeGOES.load_model(
            package=package,
            conditioning_data_source=GFS_FX(),
            model_name=GOES_MODEL_NAME,
        )
        self._goes_model = self._goes_model.to(self._device).eval()
        logger.info("StormScopeGOES loaded (%s)", GOES_MODEL_NAME)

        # MRMS model: radar forecasting, conditioned on GOES predictions
        self._mrms_model = StormScopeMRMS.load_model(
            package=package,
            conditioning_data_source=GOES(),
            model_name=MRMS_MODEL_NAME,
        )
        self._mrms_model = self._mrms_model.to(self._device).eval()
        logger.info("StormScopeMRMS loaded (%s)", MRMS_MODEL_NAME)

    def predict(
        self,
        run_timestamp: str,
        input_config: dict[str, Any] | None = None,
    ) -> xr.Dataset:
        """
        Run StormScope nowcast inference + GFS backfill.

        Parameters
        ----------
        run_timestamp : str
            ISO 8601 timestamp for the nowcast initialization time.
        input_config : dict, optional
            Must contain 'calibration' key with reflectivity-to-probability
            coefficients (spec Section 5.2).

        Returns
        -------
        xr.Dataset
            Canonical-variable dataset with dimensions (time, lat, lon).
            time: 6 steps (T+1h through T+6h at 1h intervals)
            lat: 361 points (65.0 to 20.0, CONUS)
            lon: 240 points (-130.0 to -70.25, CONUS)
        """
        if self._goes_model is None or self._mrms_model is None:
            raise RuntimeError("Models not loaded. Call load_weights() first.")

        from earth2studio.data import GFS, GOES, MRMS, fetch_data

        logger.info("Starting StormScope nowcast for run_timestamp=%s", run_timestamp)

        # ---- 1. Fetch initial observation data ----
        goes_source = GOES(satellite="goes16", scan_mode="C")
        mrms_source = MRMS()

        # Get source grid coordinates
        goes_lat, goes_lon = GOES.grid(satellite="goes16", scan_mode="C")

        # Build interpolators for both models
        from earth2studio.data import GFS_FX

        self._goes_model.build_input_interpolator(goes_lat, goes_lon)
        self._goes_model.build_conditioning_interpolator(
            GFS_FX.GFS_LAT, GFS_FX.GFS_LON,
        )

        mrms_lat, mrms_lon = MRMS.grid()
        self._mrms_model.build_input_interpolator(mrms_lat, mrms_lon)
        self._mrms_model.build_conditioning_interpolator(goes_lat, goes_lon)

        # Fetch initial GOES and MRMS observations
        x_goes, x_goes_coords = fetch_data(
            goes_source,
            time=[run_timestamp],
            variable=self._goes_model.input_variables,
        )
        x_mrms, x_mrms_coords = fetch_data(
            mrms_source,
            time=[run_timestamp],
            variable=self._mrms_model.input_variables,
        )

        logger.info("Initial data fetched — GOES shape: %s, MRMS shape: %s",
                     x_goes.shape, x_mrms.shape)

        # ---- 2. Autoregressive prediction loop (6 steps x 1h = 6h) ----
        refc_predictions = []
        y, y_coords = x_goes, x_goes_coords
        y_mrms, y_mrms_coords = x_mrms, x_mrms_coords

        with torch.no_grad():
            for step in range(NOWCAST_STEPS):
                logger.info("Nowcast step %d/%d", step + 1, NOWCAST_STEPS)

                # Run GOES model -> predicted satellite channels
                y_pred, y_pred_coords = self._goes_model(y, y_coords)

                # Run MRMS model conditioned on GOES predictions -> predicted radar
                y_mrms_pred, y_mrms_pred_coords = self._mrms_model.call_with_conditioning(
                    y_mrms, y_mrms_coords,
                    conditioning=y, conditioning_coords=y_coords,
                )

                # Extract composite reflectivity (refc) for this step
                refc_idx = list(self._mrms_model.output_variables).index("refc")
                refc_step = y_mrms_pred[..., refc_idx, :, :].cpu().numpy()
                refc_predictions.append(np.squeeze(refc_step))

                # Prepare next input (sliding window)
                y, y_coords = self._goes_model.next_input(
                    y_pred, y_pred_coords, y, y_coords,
                )
                y_mrms, y_mrms_coords = self._mrms_model.next_input(
                    y_mrms_pred, y_mrms_pred_coords, y_mrms, y_mrms_coords,
                )

        logger.info("Autoregressive loop complete, %d refc predictions", len(refc_predictions))

        # ---- 3. Fetch GFS analysis for temp/wind/humidity backfill ----
        gfs_source = GFS()
        gfs_raw, _ = fetch_data(
            gfs_source,
            time=[run_timestamp],
            variable=GFS_BACKFILL_VARS,
        )

        # Convert to dict of arrays for regridding
        gfs_dict = {}
        for i, var_name in enumerate(GFS_BACKFILL_VARS):
            gfs_dict[var_name] = gfs_raw[..., i, :, :].cpu().numpy().squeeze()

        logger.info("GFS backfill data fetched: %s", list(gfs_dict.keys()))

        # ---- 4. Regrid ----
        # StormScope refc: HRRR grid -> 361x240 target
        src_lat = self._goes_model.latitudes.detach().cpu().numpy()
        src_lon = self._goes_model.longitudes.detach().cpu().numpy()

        regridded_refc = self._regridder.regrid_hrrr(
            refc_predictions, src_lat, src_lon,
        )

        # GFS: 0.25-degree global -> 361x240 CONUS subset
        regridded_gfs = self._regridder.regrid_gfs(gfs_dict)

        # Convert regridded GFS to xr.Dataset for translator
        gfs_ds = xr.Dataset(
            {name: (["lat", "lon"], arr) for name, arr in regridded_gfs.items()},
            coords={
                "lat": self._regridder.target_lat,
                "lon": self._regridder.target_lon,
            },
        )

        # ---- 5. Build time coordinates and assemble canonical output ----
        base_time = datetime.fromisoformat(run_timestamp.replace("Z", "+00:00"))
        time_coords = np.array([
            int((base_time + timedelta(hours=h + 1)).timestamp())
            for h in range(NOWCAST_STEPS)
        ], dtype=np.int64)

        nowcast_coords = {
            "time": ("time", time_coords),
            "lat": ("lat", self._regridder.target_lat.astype(np.float32)),
            "lon": ("lon", self._regridder.target_lon.astype(np.float32)),
        }

        calibration = (input_config or {}).get("calibration", {})
        canonical_ds = translate_nowcast(
            stormscope_refc=regridded_refc,
            gfs_data=gfs_ds,
            calibration=calibration,
            nowcast_coords=nowcast_coords,
        )

        # Validate physical bounds
        validate(canonical_ds)

        logger.info("Nowcast inference complete: %s", dict(canonical_ds.sizes))
        return canonical_ds
