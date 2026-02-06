"""
RunPod Local Simulator: FastAPI server that simulates the RunPod Serverless API.

Exposes a POST /run endpoint that accepts InferencePayload requests,
invokes the InferenceHandler (with MockEngine + ZarrWriter), and returns
the standard RunPod response JSON envelope.

For local development, this writes Zarr stores to MinIO (S3-compatible)
using s3fs and environment variables for endpoint configuration.

Usage:
    python runpod/sim.py

Environment Variables:
    AWS_ENDPOINT_URL        - S3/MinIO endpoint (default: http://localhost:9000)
    AWS_ACCESS_KEY_ID       - S3/MinIO access key (default: minioadmin)
    AWS_SECRET_ACCESS_KEY   - S3/MinIO secret key (default: minioadmin)
    FORECAST_BUCKET         - Target bucket name (default: watchpoint-forecasts)
    MOCK_INFERENCE          - Force mock mode globally (default: true)
    SIM_PORT                - Port to run on (default: 8001)

Architecture Reference:
    11-runpod.md Section 2 (Interface Contract)
    11-runpod.md Section 4.2 (Mock Mode)
    watchpoint-tech-stack-v3.3.md Section 12 (Local Development)
"""

from __future__ import annotations

import logging
import os
import uuid
from typing import Any, Optional

from fastapi import FastAPI
from fastapi.responses import JSONResponse
from pydantic import BaseModel, Field

from handler import InferenceHandler

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
)
logger = logging.getLogger("runpod.sim")


# ---------------------------------------------------------------------------
# Configure S3/MinIO environment for s3fs and boto3
# ---------------------------------------------------------------------------
def _configure_s3_environment() -> None:
    """
    Set AWS environment variables and fsspec config for local MinIO access.

    boto3 reads AWS_ENDPOINT_URL from the environment to route S3 API calls
    to MinIO. s3fs (used by xarray/zarr for S3 writes) does NOT read endpoint
    configuration from environment variables, so we configure it explicitly
    via fsspec.config.conf.
    """
    defaults = {
        "AWS_ENDPOINT_URL": "http://localhost:9000",
        "AWS_ACCESS_KEY_ID": "minioadmin",
        "AWS_SECRET_ACCESS_KEY": "minioadmin",
    }
    for key, default_value in defaults.items():
        if key not in os.environ:
            os.environ[key] = default_value
            logger.info("Set default %s=%s", key, default_value)

    # Configure s3fs via fsspec's global config so that xarray.to_zarr()
    # and zarr.open() with S3 paths route to MinIO automatically.
    # This is necessary because s3fs does not read AWS_ENDPOINT_URL
    # from the environment like boto3 does.
    import fsspec
    endpoint_url = os.environ.get("AWS_ENDPOINT_URL", "http://localhost:9000")
    access_key = os.environ.get("AWS_ACCESS_KEY_ID", "minioadmin")
    secret_key = os.environ.get("AWS_SECRET_ACCESS_KEY", "minioadmin")

    fsspec.config.conf["s3"] = {
        "endpoint_url": endpoint_url,
        "key": access_key,
        "secret": secret_key,
    }
    logger.info("Configured s3fs endpoint: %s", endpoint_url)


_configure_s3_environment()


# ---------------------------------------------------------------------------
# Pydantic models for request/response validation
# ---------------------------------------------------------------------------

class InputConfigModel(BaseModel):
    """Input configuration nested within the payload."""
    gfs_source_paths: list[str] = Field(default_factory=list)
    calibration: dict[str, Any] = Field(default_factory=dict)
    verification_window: Optional[dict[str, str]] = None
    calibration_time_range: Optional[dict[str, str]] = None


class OptionsModel(BaseModel):
    """Inference options."""
    force_rebuild: bool = False
    mock_inference: bool = False


class RunPodInputModel(BaseModel):
    """
    The 'input' field of a RunPod request.

    Maps to Section 2.1 of 11-runpod.md.
    """
    task_type: str = "inference"
    model: str
    run_timestamp: str
    output_destination: str
    input_config: InputConfigModel = Field(default_factory=InputConfigModel)
    options: OptionsModel = Field(default_factory=OptionsModel)


class RunPodRequestModel(BaseModel):
    """
    Top-level RunPod request envelope.

    RunPod wraps all job input under an "input" key.
    """
    input: RunPodInputModel


class RunPodOutputModel(BaseModel):
    """
    The output portion of a RunPod response.

    Maps to Section 2.2 of 11-runpod.md.
    """
    status: str
    job_id: str
    inference_duration_ms: int
    chunks_written: int
    output_path: str
    error: Optional[str] = None


class RunPodResponseModel(BaseModel):
    """
    Standard RunPod response envelope.

    Wraps the handler's output in the RunPod envelope format:
    {"id": "...", "status": "COMPLETED", "output": {...}}
    """
    id: str
    status: str
    output: RunPodOutputModel


# ---------------------------------------------------------------------------
# FastAPI application
# ---------------------------------------------------------------------------

app = FastAPI(
    title="WatchPoint RunPod Simulator",
    description=(
        "Local simulator for the RunPod inference worker. "
        "Accepts inference payloads and writes Zarr stores to MinIO."
    ),
    version="1.0.0",
)


# Shared handler instance
_handler = InferenceHandler()


@app.get("/health")
async def health() -> dict[str, str]:
    """Health check endpoint."""
    return {"status": "ok", "service": "runpod-simulator"}


@app.post("/run", response_model=RunPodResponseModel)
async def run_inference(request: RunPodRequestModel) -> JSONResponse:
    """
    Execute an inference job.

    Accepts the standard RunPod request format with the payload under
    the "input" key. Routes to InferenceHandler which orchestrates
    the full pipeline: validate -> engine select -> predict -> write.

    Returns the standard RunPod response envelope:
    {"id": "...", "status": "COMPLETED"|"FAILED", "output": {...}}
    """
    request_id = f"req_{uuid.uuid4().hex[:8]}"
    logger.info("Received /run request %s for model=%s", request_id, request.input.model)

    # Convert the validated Pydantic model back to a dict for the handler.
    # The handler expects the RunPod envelope format (with "input" key).
    raw_payload = request.model_dump()

    # If mock_inference is not explicitly set in the request, default to True
    # for the local simulator (Section 4.2: local dev uses mock mode).
    if not raw_payload["input"]["options"]["mock_inference"]:
        env_mock = os.environ.get("MOCK_INFERENCE", "true").lower()
        if env_mock in ("true", "1", "yes"):
            raw_payload["input"]["options"]["mock_inference"] = True
            logger.info("Request %s: Defaulting to mock_inference=true", request_id)

    # Run the inference pipeline
    result = _handler.run(raw_payload)

    # Determine RunPod-level status
    runpod_status = "COMPLETED" if result.status == "success" else "FAILED"

    response = {
        "id": request_id,
        "status": runpod_status,
        "output": result.to_dict(),
    }

    status_code = 200 if result.status == "success" else 500
    return JSONResponse(content=response, status_code=status_code)


@app.post("/runsync", response_model=RunPodResponseModel)
async def run_inference_sync(request: RunPodRequestModel) -> JSONResponse:
    """
    Synchronous variant of /run.

    RunPod offers both /run (async) and /runsync (sync) endpoints.
    For the local simulator, both behave synchronously.
    """
    return await run_inference(request)


# ---------------------------------------------------------------------------
# Entry point
# ---------------------------------------------------------------------------

def main() -> None:
    """Start the FastAPI server using uvicorn."""
    import uvicorn

    port = int(os.environ.get("SIM_PORT", "8001"))
    logger.info("Starting RunPod Simulator on port %d", port)
    logger.info("MinIO endpoint: %s", os.environ.get("AWS_ENDPOINT_URL"))
    logger.info("Mock inference: %s", os.environ.get("MOCK_INFERENCE", "true"))

    uvicorn.run(
        "sim:app",
        host="0.0.0.0",
        port=port,
        reload=False,
        log_level="info",
    )


if __name__ == "__main__":
    main()
