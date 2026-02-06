# =============================================================================
# RunPod Inference Worker Dockerfile (GPU / x86_64)
# =============================================================================
# Builds the Python-based GPU inference worker for RunPod Serverless.
#
# Architecture: linux/amd64 (NVIDIA GPU instances)
# Base image:   nvidia/cuda:11.8.0-cudnn8-runtime-ubuntu22.04
# Entrypoint:   handler.py (RunPod Serverless handler)
#
# Build context: Project root (.)
# Build command: docker build --platform linux/amd64 -f runpod/Dockerfile .
#
# Model weights are NOT baked into the image. They are loaded at runtime
# from a persistent RunPod Network Volume mounted at /runpod-volume/weights.
# On cold start, the worker auto-hydrates missing weights from S3.
#
# References:
#   - 11-runpod.md Section 6.1 (Docker Specification)
#   - 11-runpod.md Section 6.2 (Model Weights Storage)
#   - 11-runpod.md Section 4.1 (Class Structure)
# =============================================================================

# ---------------------------------------------------------------------------
# Base: NVIDIA CUDA Runtime with cuDNN (required for PyTorch/ONNX inference)
# ---------------------------------------------------------------------------
FROM nvidia/cuda:11.8.0-cudnn8-runtime-ubuntu22.04

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------
# PYTHONUNBUFFERED: Ensure real-time log output in RunPod console.
# DEBIAN_FRONTEND: Suppress interactive prompts during apt-get.
# MODEL_WEIGHTS_PATH: Default network volume mount point for model weights.
ENV PYTHONUNBUFFERED=1 \
    DEBIAN_FRONTEND=noninteractive \
    MODEL_WEIGHTS_PATH="/runpod-volume/weights"

# ---------------------------------------------------------------------------
# System dependencies
# ---------------------------------------------------------------------------
# python3.11: Runtime interpreter matching the project standard.
# python3-pip: Package installer for Python dependencies.
# python3.11-venv: Required for pip to function correctly on Ubuntu 22.04.
# git: Needed by some Python packages that install from git repos.
# Clean up apt caches to minimize layer size.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        python3.11 \
        python3-pip \
        python3.11-venv \
        git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Make python3.11 the default python3.
RUN update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.11 1

# ---------------------------------------------------------------------------
# Python dependencies
# ---------------------------------------------------------------------------
# Copy requirements first to leverage Docker layer caching.
# runpod/requirements.txt contains all needed dependencies:
#   - RunPod SDK, data science stack (numpy, xarray, zarr, s3fs, pandas)
#   - ML inference (torch, onnxruntime-gpu)
#   - Infrastructure (boto3, pydantic)
#   - Local simulator deps (fastapi, uvicorn, httpx)
COPY runpod/requirements.txt /tmp/requirements.txt

# Upgrade pip to a version that supports all needed features, then install deps.
# --no-cache-dir: Minimize image size by not caching pip downloads.
RUN pip install --no-cache-dir --upgrade pip && \
    pip install --no-cache-dir \
    -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

# ---------------------------------------------------------------------------
# Application code
# ---------------------------------------------------------------------------
# Copy only production Python files (exclude tests, caches, venvs).
# The .dockerignore already filters __pycache__, .venv, .pytest_cache, etc.
COPY runpod/__init__.py /app/__init__.py
COPY runpod/handler.py /app/handler.py
COPY runpod/mock_engine.py /app/mock_engine.py
COPY runpod/zarr_writer.py /app/zarr_writer.py
COPY runpod/sim.py /app/sim.py

WORKDIR /app

# ---------------------------------------------------------------------------
# Entrypoint: RunPod Serverless handler
# ---------------------------------------------------------------------------
# Matches 11-runpod.md Section 6.1.
# The -u flag ensures unbuffered stdout/stderr for real-time log streaming.
CMD [ "python3", "-u", "handler.py" ]
