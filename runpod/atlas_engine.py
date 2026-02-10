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
import time
from typing import Any

import numpy as np
import torch
import torch._dynamo
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

        # Optimizations are toggleable via ATLAS_OPTIMIZATIONS env var:
        #   "all"         — TF32 + bf16 + EM sampler (default)
        #   "all_compile" — TF32 + bf16 + EM + torch.compile with periodic dynamo reset
        #   "tf32"        — TF32 only
        #   "none"        — vanilla earth2studio, no modifications
        opt_mode = os.environ.get("ATLAS_OPTIMIZATIONS", "all")
        logger.info("ATLAS_OPTIMIZATIONS=%s", opt_mode)

        if opt_mode != "none":
            self._apply_optimizations(opt_mode)

        # Always install the monitoring hook regardless of optimization mode
        self._install_step_monitor_hook(
            dynamo_reset_interval=10 if opt_mode == "all_compile" else 0,
        )
        logger.info("Step monitor hook installed (timing + memory logging)")

    def _apply_optimizations(self, mode: str = "all") -> None:
        """
        Apply GPU performance optimizations to the loaded Atlas model.

        Three optimizations (torch.compile removed — it caused progressive slowdown
        from dynamo cache accumulation over 60 steps, 52s→89s/step):

        1. TF32 matmul: PyTorch 2.5 defaults TF32 off. A100 FP32=19.5 TFLOPS vs TF32=156 TFLOPS.
        2. bf16 autocast: Wrap model + autoencoder forward in bfloat16 autocast.
        3. EM sampler: Euler-Maruyama uses 1 model eval/step vs rk_roberts' 2.

        Modes:
          "all"         — TF32 + bf16 autocast + EM sampler
          "all_compile" — all + torch.compile DiT blocks (with periodic dynamo reset)
          "tf32"        — TF32 only (test if bf16/EM are hurting)

        Note: torch.cuda.empty_cache() was tested but destroys the CUDA caching
        allocator's memory pool, causing 2-5x slowdown per step.
        """
        # 1. Enable TF32 matmul precision (always, in any optimization mode)
        torch.set_float32_matmul_precision("high")
        torch.backends.cuda.matmul.allow_tf32 = True
        torch.backends.cudnn.allow_tf32 = True
        logger.info("TF32 matmul enabled")

        if mode == "tf32":
            logger.info("TF32-only mode — skipping bf16 and EM sampler")
            return

        # 2. bf16 autocast on model (SInterpolantLatentDiT) + autoencoder (NattenCombineDiT)
        _orig_model_fwd = self._model.model.forward

        def _bf16_model_fwd(*args, **kwargs):
            with torch.amp.autocast("cuda", dtype=torch.bfloat16):
                return _orig_model_fwd(*args, **kwargs)

        self._model.model.forward = _bf16_model_fwd

        _orig_ae_fwd = self._model.autoencoders[0].forward

        def _bf16_ae_fwd(*args, **kwargs):
            with torch.amp.autocast("cuda", dtype=torch.bfloat16):
                return _orig_ae_fwd(*args, **kwargs)

        self._model.autoencoders[0].forward = _bf16_ae_fwd
        logger.info("bf16 autocast enabled for model + autoencoder")

        # 3. Switch to EM sampler (1 model eval/step instead of 2)
        self._model.sinterpolant.sample_step = self._model.sinterpolant.em_step
        self._model.sinterpolant_sample_steps = 100
        logger.info("EM sampler enabled (100 sample steps)")

        # 4. torch.compile DiT blocks (all_compile mode only)
        if mode == "all_compile":
            self._apply_torch_compile()

    def _apply_torch_compile(self) -> None:
        """
        Apply torch.compile to DiT blocks for kernel fusion speedup.

        Used with periodic dynamo reset (in the monitor hook) to prevent
        cache accumulation that caused progressive slowdown in earlier tests.
        """
        torch._dynamo.config.suppress_errors = True
        num_blocks = len(self._model.model.blocks)
        self._compiled_block_originals = []
        for i in range(num_blocks):
            self._compiled_block_originals.append(self._model.model.blocks[i])
            self._model.model.blocks[i] = torch.compile(
                self._model.model.blocks[i], mode="default", fullgraph=False,
            )
        logger.info("torch.compile applied to %d DiT blocks", num_blocks)

    def _recompile_blocks(self) -> None:
        """Re-compile DiT blocks from originals after a dynamo reset."""
        if not hasattr(self, "_compiled_block_originals"):
            return
        for i, orig in enumerate(self._compiled_block_originals):
            self._model.model.blocks[i] = torch.compile(
                orig, mode="default", fullgraph=False,
            )
        logger.info("Re-compiled %d DiT blocks after dynamo reset",
                     len(self._compiled_block_originals))

    def _install_step_monitor_hook(self, dynamo_reset_interval: int = 0) -> None:
        """
        Install a front_hook on the Atlas model that logs per-step timing and
        GPU memory stats.

        Parameters
        ----------
        dynamo_reset_interval : int
            If > 0, call torch._dynamo.reset() every N steps to prevent
            dynamo cache accumulation, then re-compile the DiT blocks.
            Set to 0 to disable (default).
        """
        self._step_timer = time.monotonic()
        self._step_count = 0

        _orig_front_hook = self._model.front_hook

        def _monitor_front_hook(x, coords):
            now = time.monotonic()
            elapsed = now - self._step_timer
            self._step_timer = now
            self._step_count += 1

            mem_alloc = torch.cuda.memory_allocated() / (1024**3)
            mem_reserved = torch.cuda.memory_reserved() / (1024**3)

            logger.info(
                "Step %d | %.1fs | GPU: %.2f GB alloc, %.2f GB reserved",
                self._step_count, elapsed, mem_alloc, mem_reserved,
            )

            # Periodic dynamo reset to prevent cache accumulation
            if (dynamo_reset_interval > 0
                    and self._step_count > 1
                    and self._step_count % dynamo_reset_interval == 0):
                logger.info("Resetting torch._dynamo cache (every %d steps)",
                            dynamo_reset_interval)
                torch._dynamo.reset()
                self._recompile_blocks()

            return _orig_front_hook(x, coords)

        self._model.front_hook = _monitor_front_hook

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

        import shutil

        import earth2studio.run as run
        from earth2studio.data import GFS
        from earth2studio.io import ZarrBackend

        logger.info("Starting Atlas inference for run_timestamp=%s", run_timestamp)

        # Use file-backed ZarrBackend on tmpfs to avoid ~17.4 GB Python heap pressure.
        # /dev/shm is RAM-backed (tmpfs) so I/O is fast, but data lives outside Python's
        # GC-scanned heap, reducing GC pause overhead over 60 steps.
        zarr_tmp = "/dev/shm/atlas_output.zarr"
        if os.path.exists(zarr_tmp):
            shutil.rmtree(zarr_tmp)
        logger.info("Using file-backed ZarrBackend at %s", zarr_tmp)

        io = ZarrBackend(file_name=zarr_tmp)
        io = run.deterministic(
            time=[run_timestamp],
            nsteps=ATLAS_NSTEPS,
            prognostic=self._model,
            data=GFS(),
            io=io,
        )

        # Extract as xarray Dataset from the file-backed Zarr store
        raw_ds = xr.open_zarr(zarr_tmp)
        logger.info("Raw Atlas output shape: %s", dict(raw_ds.sizes))

        # Handle longitude convention: E2S Atlas uses 0-360, we use -180 to 180
        raw_ds = self._convert_longitude(raw_ds)

        # Translate raw variables to canonical format
        canonical_ds = translate_atlas(raw_ds)

        # Validate physical bounds
        validate(canonical_ds)

        # Clean up temp zarr store
        if os.path.exists(zarr_tmp):
            shutil.rmtree(zarr_tmp)
            logger.info("Cleaned up temp Zarr at %s", zarr_tmp)

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

            # Preserve all original coordinates, only replacing lon
            new_coords = {k: v for k, v in ds.coords.items() if k != "lon"}
            new_coords["lon"] = ("lon", new_lon.astype(np.float32))

            ds = xr.Dataset(data_vars, coords=new_coords)

        return ds
