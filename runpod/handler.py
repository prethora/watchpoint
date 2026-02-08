"""
InferenceHandler: Orchestrates the RunPod inference pipeline.

Implements the InferenceHandler class from 11-runpod.md Section 4.1.
Responsible for:
  1. Validating the incoming InferencePayload (Section 2)
  2. Selecting the appropriate engine (MockEngine or real)
  3. Running prediction
  4. Writing results via ZarrWriter
  5. Returning a standard response with execution time and output path

The handler implements the Clean Slate Policy (Section 7.1) and supports
mock inference mode (Section 4.2) for local development and CI/CD.
"""

from __future__ import annotations

import logging
import math
import os
import time
import uuid
from dataclasses import dataclass, field
from typing import Any, Optional

from mock_engine import MockEngine, ModelEngine, is_mock_mode
from zarr_writer import SUCCESS_MARKER, TILE_LAT_CHUNK, TILE_LON_CHUNK, ZarrWriter

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Payload data classes
# ---------------------------------------------------------------------------

@dataclass
class InputConfig:
    """
    Input configuration for the inference job.

    Maps to the input_config field in the InferencePayload (Section 2.1).
    """
    gfs_source_paths: list[str] = field(default_factory=list)
    calibration: dict[str, Any] = field(default_factory=dict)
    verification_window: Optional[dict[str, str]] = None
    calibration_time_range: Optional[dict[str, str]] = None


@dataclass
class InferenceOptions:
    """
    Options controlling inference behavior.

    Maps to the options field in the InferencePayload (Section 2.1).
    """
    force_rebuild: bool = False
    mock_inference: bool = False


@dataclass
class InferencePayload:
    """
    The complete inference payload as defined in Section 2.1 of 11-runpod.md.

    Wraps all fields from the RunPod input schema:
      - task_type: routing key (inference, verification, calibration)
      - model: model identifier (medium_range, nowcast)
      - run_timestamp: ISO 8601 timestamp of the forecast run
      - output_destination: S3 URI or local path for Zarr output
      - input_config: model-specific input configuration
      - options: behavioral flags (force_rebuild, mock_inference)
    """
    model: str
    run_timestamp: str
    output_destination: str
    task_type: str = "inference"
    input_config: InputConfig = field(default_factory=InputConfig)
    options: InferenceOptions = field(default_factory=InferenceOptions)


def parse_payload(raw: dict[str, Any]) -> InferencePayload:
    """
    Parse a raw dictionary (from the HTTP request body) into an InferencePayload.

    Handles the nested RunPod envelope where the actual payload lives under
    the "input" key. If the raw dict already contains the payload fields
    directly (no "input" wrapper), it is used as-is.

    Raises
    ------
    ValueError
        If required fields (model, run_timestamp, output_destination) are missing.
    """
    # Unwrap RunPod envelope if present
    data = raw.get("input", raw)

    # Extract and validate required fields
    model = data.get("model")
    if not model:
        raise ValueError("Missing required field: 'model'")

    run_timestamp = data.get("run_timestamp")
    if not run_timestamp:
        raise ValueError("Missing required field: 'run_timestamp'")

    output_destination = data.get("output_destination")
    if not output_destination:
        raise ValueError("Missing required field: 'output_destination'")

    task_type = data.get("task_type", "inference")

    # Parse input_config
    raw_config = data.get("input_config", {})
    input_config = InputConfig(
        gfs_source_paths=raw_config.get("gfs_source_paths", []),
        calibration=raw_config.get("calibration", {}),
        verification_window=raw_config.get("verification_window"),
        calibration_time_range=raw_config.get("calibration_time_range"),
    )

    # Parse options
    raw_options = data.get("options", {})
    options = InferenceOptions(
        force_rebuild=raw_options.get("force_rebuild", False),
        mock_inference=raw_options.get("mock_inference", False),
    )

    return InferencePayload(
        task_type=task_type,
        model=model,
        run_timestamp=run_timestamp,
        output_destination=output_destination,
        input_config=input_config,
        options=options,
    )


# ---------------------------------------------------------------------------
# Response structure
# ---------------------------------------------------------------------------

@dataclass
class InferenceResult:
    """
    Result of an inference run.

    Maps to the response schema in Section 2.2 of 11-runpod.md.
    """
    status: str
    job_id: str
    inference_duration_ms: int
    chunks_written: int
    output_path: str
    error: Optional[str] = None

    def to_dict(self) -> dict[str, Any]:
        """Convert to a JSON-serializable dictionary."""
        return {
            "status": self.status,
            "job_id": self.job_id,
            "inference_duration_ms": self.inference_duration_ms,
            "chunks_written": self.chunks_written,
            "output_path": self.output_path,
            "error": self.error,
        }


# ---------------------------------------------------------------------------
# Model name mapping
# ---------------------------------------------------------------------------
MODEL_DISPLAY_NAMES: dict[str, str] = {
    "medium_range": "atlas",
    "nowcast": "stormscope",
}


# ---------------------------------------------------------------------------
# InferenceHandler
# ---------------------------------------------------------------------------

class InferenceHandler:
    """
    Orchestrator for the inference pipeline.

    Defined in 11-runpod.md Section 4.1. Coordinates:
      Fetch -> Predict -> Translate -> Write

    For mock mode, the Fetch and Translate steps are bypassed since
    MockEngine generates canonical-variable data directly.

    Usage::

        handler = InferenceHandler()
        result = handler.run(payload_dict)
    """

    def __init__(
        self,
        engine: Optional[ModelEngine] = None,
        writer: Optional[ZarrWriter] = None,
    ) -> None:
        """
        Parameters
        ----------
        engine : ModelEngine, optional
            Override the engine used for inference. If None, the handler
            selects MockEngine or raises an error (real engines are not
            yet implemented).
        writer : ZarrWriter, optional
            Override the ZarrWriter. If None, a default writer is created.
        """
        self._engine = engine
        self._writer = writer

    def run(self, raw_payload: dict[str, Any]) -> InferenceResult:
        """
        Execute the full inference pipeline.

        Parameters
        ----------
        raw_payload : dict
            The raw JSON payload from the HTTP request, following the
            schema defined in Section 2.1 of 11-runpod.md.

        Returns
        -------
        InferenceResult
            The result of the inference run, including execution time
            and output path.
        """
        job_id = f"sim_{uuid.uuid4().hex[:12]}"
        start_time = time.monotonic()

        try:
            # 1. Parse and validate the payload
            payload = parse_payload(raw_payload)
            logger.info(
                "Job %s: Parsed payload - model=%s, task_type=%s, destination=%s",
                job_id, payload.model, payload.task_type, payload.output_destination,
            )

            # Only inference task_type is supported by this handler
            if payload.task_type != "inference":
                raise ValueError(
                    f"Unsupported task_type: {payload.task_type!r}. "
                    f"Only 'inference' is supported by InferenceHandler."
                )

            # 2. Select the engine
            engine = self._resolve_engine(payload)

            # 3. Select (or create) the writer
            model_display = MODEL_DISPLAY_NAMES.get(payload.model, payload.model)
            writer = self._writer or ZarrWriter(
                model_name=model_display,
                model_version="v1.0",
            )

            # 4. Clean Slate Policy (Section 7.1):
            #    If force_rebuild or no _SUCCESS marker, purge destination
            if payload.options.force_rebuild:
                logger.info("Job %s: Force rebuild - cleaning destination", job_id)
                writer.clean_slate(payload.output_destination)
            elif not self._success_marker_exists(payload.output_destination):
                logger.info("Job %s: No existing _SUCCESS - cleaning destination", job_id)
                writer.clean_slate(payload.output_destination)
            else:
                # _SUCCESS exists and force_rebuild is false: return immediately
                # per Section 7.1 idempotency check
                elapsed_ms = int((time.monotonic() - start_time) * 1000)
                logger.info(
                    "Job %s: _SUCCESS exists and force_rebuild=false, returning cached",
                    job_id,
                )
                return InferenceResult(
                    status="success",
                    job_id=job_id,
                    inference_duration_ms=elapsed_ms,
                    chunks_written=0,
                    output_path=payload.output_destination,
                )

            # 5. Run prediction
            logger.info("Job %s: Running prediction with %s", job_id, type(engine).__name__)
            dataset = engine.predict(
                run_timestamp=payload.run_timestamp,
                input_config={"calibration": payload.input_config.calibration},
            )

            # 6. Write Zarr output
            logger.info("Job %s: Writing Zarr to %s", job_id, payload.output_destination)
            writer.write(dataset, payload.output_destination)

            # 7. Calculate chunks written (approximate from dataset dimensions)
            lat_chunks = math.ceil(dataset.sizes["lat"] / TILE_LAT_CHUNK)
            lon_chunks = math.ceil(dataset.sizes["lon"] / TILE_LON_CHUNK)
            num_variables = len(dataset.data_vars)
            chunks_written = lat_chunks * lon_chunks * num_variables

            elapsed_ms = int((time.monotonic() - start_time) * 1000)
            logger.info(
                "Job %s: Completed in %dms, %d chunks written",
                job_id, elapsed_ms, chunks_written,
            )

            return InferenceResult(
                status="success",
                job_id=job_id,
                inference_duration_ms=elapsed_ms,
                chunks_written=chunks_written,
                output_path=payload.output_destination,
            )

        except Exception as exc:
            elapsed_ms = int((time.monotonic() - start_time) * 1000)
            logger.error("Job %s: Failed after %dms - %s", job_id, elapsed_ms, exc)
            return InferenceResult(
                status="error",
                job_id=job_id,
                inference_duration_ms=elapsed_ms,
                chunks_written=0,
                output_path="",
                error=str(exc),
            )

    # ------------------------------------------------------------------
    # Private helpers
    # ------------------------------------------------------------------

    def _resolve_engine(self, payload: InferencePayload) -> ModelEngine:
        """
        Determine which engine to use for this inference run.

        If an engine was injected via the constructor, use that.
        Otherwise, use MockEngine when mock mode is active.
        Raises RuntimeError if a real engine is needed but not available.
        """
        if self._engine is not None:
            return self._engine

        options_dict = {
            "mock_inference": payload.options.mock_inference,
        }

        if is_mock_mode(options_dict):
            logger.info("Using MockEngine for model=%s", payload.model)
            return MockEngine(model=payload.model)

        # Real inference engines — lazy imports to avoid loading E2S in mock mode
        if payload.model == "medium_range":
            from atlas_engine import AtlasEngine

            engine = AtlasEngine()
            engine.load_weights(os.environ.get("EARTH2STUDIO_CACHE"))
            return engine
        elif payload.model == "nowcast":
            from nowcast_engine import NowcastEngine

            engine = NowcastEngine()
            engine.load_weights(os.environ.get("EARTH2STUDIO_CACHE"))
            return engine

        raise RuntimeError(
            f"Unknown model: {payload.model!r}. "
            f"Expected 'medium_range' or 'nowcast'."
        )

    @staticmethod
    def _success_marker_exists(destination: str) -> bool:
        """
        Check if a _SUCCESS marker exists at the destination.

        Part of the Clean Slate Policy (Section 7.1): If _SUCCESS exists
        and force_rebuild=false, return success immediately.
        """
        if destination.startswith("s3://"):
            return InferenceHandler._success_marker_exists_s3(destination)
        else:
            marker_path = os.path.join(destination, SUCCESS_MARKER)
            return os.path.exists(marker_path)

    @staticmethod
    def _success_marker_exists_s3(destination: str) -> bool:
        """Check for _SUCCESS marker in S3."""
        try:
            import boto3

            parts = destination.replace("s3://", "").split("/", 1)
            bucket = parts[0]
            prefix = (parts[1] if len(parts) > 1 else "").rstrip("/")
            key = f"{prefix}/{SUCCESS_MARKER}" if prefix else SUCCESS_MARKER

            s3 = boto3.client("s3")
            s3.head_object(Bucket=bucket, Key=key)
            return True
        except Exception as exc:
            logger.debug("_SUCCESS marker check failed for %s: %s", destination, exc)
            return False


# ---------------------------------------------------------------------------
# RunPod Serverless SDK entry point
# ---------------------------------------------------------------------------

def _runpod_handler(job: dict) -> dict:
    """RunPod serverless handler entry point.

    Called by the RunPod SDK for each incoming job. The job dict has the form:
    {"id": "...", "input": {"model": "...", ...}}

    parse_payload() inside InferenceHandler.run() does raw.get("input", raw)
    to unwrap the RunPod envelope, so the full job dict works directly.
    """
    handler = InferenceHandler()
    result = handler.run(job)
    return result.to_dict()


if __name__ == "__main__":
    import runpod

    logging.basicConfig(
        level=logging.INFO,
        format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    )
    logger.info("Starting RunPod serverless worker...")
    runpod.serverless.start({"handler": _runpod_handler})
