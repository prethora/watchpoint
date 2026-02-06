# 11 - RunPod Inference Worker

> **Purpose**: Defines the architecture for the GPU-accelerated inference worker hosted on RunPod Serverless. This component is responsible for running heavy ML models (Atlas/StormScope), translating raw outputs into canonical variables, and writing optimized Zarr stores to S3.
> **Package**: `worker/runpod` (Python 3.11)
> **Dependencies**: `01-foundation-types.md` (Variable Definitions), `03-config.md` (Secrets)

---

## Table of Contents

1. [Overview](#1-overview)
2. [Interface Contract (Trigger API)](#2-interface-contract-trigger-api)
3. [Output Data Contract (Zarr)](#3-output-data-contract-zarr)
4. [Internal Architecture](#4-internal-architecture)
5. [Translation & Calibration](#5-translation--calibration)
6. [Execution Environment](#6-execution-environment)
7. [Resilience & Validation](#7-resilience--validation)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

The RunPod Inference Worker is a stateless, containerized Python application that executes on-demand when triggered by the `data-poller`. It decouples heavy compute from the AWS Lambda environment.

### Key Responsibilities
*   **Inference**: Loading and executing PyTorch/ONNX models on GPU.
*   **Translation**: Converting model-specific physics units (e.g., Kelvin) to system canonicals (e.g., Celsius).
*   **Storage**: Writing `Zstd`-compressed Zarr arrays to S3 with a strict chunking strategy compatible with the Evaluation Engine.
*   **Validation**: Ensuring input data integrity and output physical sanity before committing results.

---

## 2. Interface Contract (Trigger API)

The worker accepts a polymorphic JSON payload via HTTP POST. This contract is strictly enforced by the `data-poller` (the client).

### 2.1 Request Schema

The worker accepts a polymorphic payload where `task_type` determines routing to internal handlers:
- `inference` (default): Standard forecast generation (`inference.py`)
- `verification`: Forecast accuracy verification against observations (`verification.py`)
- `calibration`: Coefficient regression analysis (`calibration.py`)

```json
{
  "input": {
    "task_type": "inference",
    "model": "medium_range",
    "run_timestamp": "2026-01-31T06:00:00Z",
    "output_destination": "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z",
    "input_config": {
      "gfs_source_paths": ["s3://noaa-gfs.../gfs.t06z.pgrb2.0p25.f000", "..."],
      "calibration": {
        "high_threshold": 45.0,
        "high_slope": 0.8,
        "..." : "..."
      },
      "verification_window": {
        "start": "2026-01-30T00:00:00Z",
        "end": "2026-01-31T00:00:00Z"
      },
      "calibration_time_range": {
        "start": "2026-01-01T00:00:00Z",
        "end": "2026-01-31T00:00:00Z"
      }
    },
    "options": {
      "force_rebuild": false,
      "mock_inference": false
    }
  }
}
```

**Task Type Routing**:
| `task_type` | Handler | Input Fields Used | Output |
|---|---|---|---|
| `inference` | `inference.py` | `gfs_source_paths`, `calibration` | Zarr forecast store |
| `verification` | `verification.py` | `verification_window` | Metrics to `verification_results` table |
| `calibration` | `calibration.py` | `calibration_time_range` | Updated coefficients or `calibration_candidates` |
```

### 2.2 Response Schema

The worker returns a JSON object. RunPod wraps this in its own envelope.

```json
{
  "status": "success",
  "job_id": "run_abc123",
  "inference_duration_ms": 45000,
  "chunks_written": 64,
  "error": null
}
```

---

## 3. Output Data Contract (Zarr)

The worker produces a Zarr store that acts as the single source of truth for the Evaluation Engine.

### 3.1 Global Structure

*   **Format**: Zarr v2
*   **Compression**: `Zstd(level=3)` (Optimized for read throughput)
*   **Commit Marker**: A 0-byte file named `_SUCCESS` written to the root only after all chunks persist.

### 3.2 Dimensions & Coordinates

| Dimension | Type | Size (Global) | Direction |
| :--- | :--- | :--- | :--- |
| `time` | `int64` (Unix) | Variable (e.g., 60) | Ascending |
| `lat` | `float32` | 721 (0.25°) | Descending (90.0 to -90.0) |
| `lon` | `float32` | 1440 (0.25°) | Ascending (-180.0 to 180.0) |

### 3.3 Chunking Strategy

Chunks align strictly with the 22.5° × 45° tile grid defined in `06-batcher.md`. This allows the Eval Worker to fetch an entire tile's worth of data in a single S3 GET request.

*   **Lat Chunk Size**: 90
*   **Lon Chunk Size**: 180
*   **Time Chunk Size**: Full Horizon (e.g., 60)

**Write-Side Halo**: To enable single-chunk reads for bilinear interpolation at tile edges, Zarr chunks MUST be written with a **1-pixel overlap padding** on all sides where data exists (e.g., a 90x180 tile is written as a 92x182 chunk).

### 3.4 Canonical Variables

The root group contains arrays mapped to `01-foundation-types.md`.

*   `precipitation_mm` (`float32`)
*   `precipitation_probability` (`float32`)
*   `temperature_c` (`float32`)
*   `wind_speed_kmh` (`float32`)
*   `humidity_percent` (`float32`)

### 3.5 Metadata (`.zattrs`)

```json
{
  "model_name": "atlas",
  "model_version": "v1.2",
  "schema_version": "1.0",
  "generated_at": "2026-01-31T06:05:00Z"
}
```

---

## 4. Internal Architecture

The worker uses a modular Python architecture to separate I/O from Inference.

### 4.1 Class Structure

```python
from abc import ABC, abstractmethod
import xarray as xr

class DataSource(ABC):
    """Abstracts S3 I/O for upstream data (GFS/GOES)."""
    @abstractmethod
    def fetch(self, paths: list[str]) -> xr.Dataset: pass

class ModelEngine(ABC):
    """Abstracts the GPU Inference (Torch/ONNX)."""
    @abstractmethod
    def load_weights(self, path: str): pass
    
    @abstractmethod
    def predict(self, input_xr: xr.Dataset) -> xr.Dataset: pass

class CanonicalTranslator:
    """Handles unit conversion and probability derivation."""
    def translate(self, raw: xr.Dataset, config: dict) -> xr.Dataset: pass

class ZarrWriter:
    """Handles chunking, compression, and S3 writes."""
    def write(self, dataset: xr.Dataset, destination: str): pass

class InferenceHandler:
    """Orchestrator: Fetch -> Predict -> Translate -> Write."""
    def run(self, request: dict) -> dict: pass
```

### 4.2 Mock Mode

To enable local development and CI/CD without GPU costs:
*   **Trigger**: `options.mock_inference = true` in payload or `MOCK_INFERENCE=true` env var.
*   **Behavior**: Bypasses `ModelEngine`. Generates random `numpy` arrays matching the Zarr schema. Writes to the target S3 path.

---

## 5. Translation & Calibration

The worker creates an abstraction layer over model specifics.

### 5.1 Variable Mapping

| Canonical Variable | Source (Atlas/GFS) | Transformation |
| :--- | :--- | :--- |
| `temperature_c` | `t2m` (Kelvin) | `t2m - 273.15` |
| `wind_speed_kmh` | `u10`, `v10` (m/s) | `sqrt(u^2 + v^2) * 3.6` |
| `precipitation_mm` | `tp` (m) | `tp * 1000` |

### 5.2 Probability Derivation (Nowcast)

For deterministic Nowcast models, probability is derived using injected `CalibrationCoefficients`.

**Formula**:
```python
# Logic applied inside CanonicalTranslator
if reflectivity >= cal.high_threshold:
    prob = min(95, cal.high_base + (reflectivity * cal.high_slope))
elif reflectivity >= cal.mid_threshold:
    prob = cal.mid_base + (reflectivity * cal.mid_slope)
else:
    prob = cal.low_base
```

---

## 6. Execution Environment

### 6.1 Docker Specification

Uses a Multi-Stage build to minimize image size.

```dockerfile
# Base: NVIDIA CUDA Runtime
FROM nvidia/cuda:11.8.0-cudnn8-runtime-ubuntu22.04

# Env
ENV PYTHONUNBUFFERED=1
ENV MODEL_WEIGHTS_PATH="/runpod-volume/weights"

# Dependencies
RUN apt-get update && apt-get install -y python3.11 pip
COPY requirements.txt .
RUN pip install -r requirements.txt

# Code
COPY . /app
WORKDIR /app

# Entrypoint compatible with RunPod Serverless
CMD [ "python3", "-u", "handler.py" ]
```

### 6.2 Model Weights Storage

To prevent OOM during build and reduce cold start latency:
*   **Network Volume**: Weights are stored on a persistent RunPod Network Volume mounted at `/runpod-volume/weights`.
*   **Auto-Hydration**: On startup, if weights are missing, the worker downloads them from a secured S3 Artifact Bucket.

---

## 7. Resilience & Validation

### 7.1 Clean Slate Policy (Idempotency)
To handle partial failures without S3 transactions:
1.  **Check**: If `_SUCCESS` exists and `force_rebuild=false`, return success immediately.
2.  **Purge**: If running, issue `DeletePrefix` on `output_destination` to remove potentially corrupt chunks from previous failed runs.
3.  **Write**: Generate and upload new chunks.
4.  **Commit**: Write `_SUCCESS`.

### 7.2 Memory Management (OOM Prevention)
The `InferenceHandler` processes data in **Time Slices** (e.g., 6-hour batches) rather than loading the full horizon into GPU memory at once.
1.  Load Input Slice (RAM).
2.  Inference (VRAM).
3.  Write Output Slice (Network).
4.  Clear Buffers.

### 7.3 Input & Output Validation

*   **InputValidator**: Checks S3 path existence and file size minimums before loading weights.
*   **OutputValidator**: Scans generated arrays for `NaN`, `Inf`, or physical impossibilities (e.g., Temperature > 60°C) before writing `_SUCCESS`. Failures trigger a job error to prevent cache poisoning.

---

## 8. Flow Coverage

| Flow ID | Description | Implementation |
|---|---|---|
| `FCST-001` | Medium-Range Generation | `InferenceHandler` with `AtlasEngine` |
| `FCST-002` | Nowcast Generation | `InferenceHandler` with `StormScopeEngine` |
| `FCST-004` | Retry Logic | Handled by `data-poller` (client) on 5xx/Timeout |
| `OBS-010` | Metrics | Worker returns metadata; Client emits to CloudWatch |
| `TEST-003` | Mock Inference | `MockEngine` implementation |