# =============================================================================
# RunPod Inference Worker Dockerfile (GPU / x86_64)
# =============================================================================
# Builds the Python-based GPU inference worker for RunPod Serverless.
#
# Architecture: linux/amd64 (NVIDIA GPU instances)
# Base image:   nvidia/cuda:12.4.1-cudnn-devel-ubuntu22.04
# Entrypoint:   handler.py (RunPod Serverless handler)
#
# Build context: Project root (.)
# Build command: docker build --platform linux/amd64 -f runpod/Dockerfile .
#
# Model weights are NOT baked into the image. They are auto-downloaded by
# Earth-2 Studio on first inference and cached at EARTH2STUDIO_CACHE on the
# persistent RunPod Network Volume.
#
# References:
#   - 11-runpod.md Section 6.1 (Docker Specification)
#   - 11-runpod.md Section 6.2 (Model Weights Storage)
#   - 11-runpod.md Section 4.1 (Class Structure)
# =============================================================================

# ---------------------------------------------------------------------------
# Base: NVIDIA CUDA 12.x Devel with cuDNN (required for Earth-2 Studio)
# ---------------------------------------------------------------------------
# Must use -devel- (not -runtime-) because NATTEN (Atlas dependency) compiles
# CUDA kernels during pip install and needs nvcc from the CUDA toolkit.
FROM nvidia/cuda:12.4.1-cudnn-devel-ubuntu22.04

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------
# PYTHONUNBUFFERED: Ensure real-time log output in RunPod console.
# DEBIAN_FRONTEND: Suppress interactive prompts during apt-get.
# MODEL_WEIGHTS_PATH: Default network volume mount point for model weights.
# EARTH2STUDIO_CACHE: Cache directory for E2S model weights (auto-download).
# TORCH_CUDA_ARCH_LIST: Compile NATTEN kernels for A100 (8.0) and H100 (9.0).
ENV PYTHONUNBUFFERED=1 \
    DEBIAN_FRONTEND=noninteractive \
    MODEL_WEIGHTS_PATH="/runpod-volume/weights" \
    EARTH2STUDIO_CACHE="/runpod-volume/weights/earth2studio" \
    TORCH_CUDA_ARCH_LIST="8.0;9.0"

# ---------------------------------------------------------------------------
# System dependencies
# ---------------------------------------------------------------------------
# python3.12: Runtime interpreter (E2S recommendation).
# python3-pip: Package installer for Python dependencies.
# python3.12-venv: Required for pip to function correctly on Ubuntu 22.04.
# build-essential: Required for natten compilation (Atlas dependency).
# git: Needed by some Python packages that install from git repos.
# Clean up apt caches to minimize layer size.
RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        software-properties-common \
    && add-apt-repository ppa:deadsnakes/ppa \
    && apt-get update && \
    apt-get install -y --no-install-recommends \
        python3.12 \
        python3.12-dev \
        python3-pip \
        python3.12-venv \
        build-essential \
        git \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Make python3.12 the default python3 and bootstrap pip for 3.12.
# The system pip is linked to Ubuntu's Python 3.10 which has distutils;
# Python 3.12 removed distutils, so we must use ensurepip to get a working pip.
RUN update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.12 1 && \
    python3.12 -m ensurepip --upgrade

# ---------------------------------------------------------------------------
# Python dependencies
# ---------------------------------------------------------------------------
# Copy requirements first to leverage Docker layer caching.
# runpod/requirements.txt contains all needed dependencies:
#   - RunPod SDK, data science stack (numpy, xarray, zarr, s3fs, pandas)
#   - ML inference (earth2studio[atlas,stormscope], scipy)
#   - Infrastructure (boto3, pydantic)
#   - Local simulator deps (fastapi, uvicorn, httpx)
COPY runpod/requirements.txt /tmp/requirements.txt

# Use python3 -m pip to ensure we use the correct pip for Python 3.12.
# --no-cache-dir: Minimize image size by not caching pip downloads.
#
# earth2grid has CUDA C++ extensions that require nvcc (only in -devel-
# images). The runtime image has /usr/local/cuda (detected by torch's
# cpp_extension.py), so we temporarily hide it during the earth2grid
# build. WatchPoint uses lat/lon grids not healpix, so the CUDA
# extensions are not needed.
RUN python3 -m pip install --no-cache-dir --upgrade pip setuptools

# Step 1: Install torch first (earth2grid needs it as a build dep).
RUN python3 -m pip install --no-cache-dir "torch>=2.5.0"

# Step 2: Hide /usr/local/cuda entirely so torch cannot find CUDA during
# earth2grid's extension build. With --no-build-isolation we reuse the
# torch + setuptools already installed above.
RUN mv /usr/local/cuda /usr/local/_cuda_hidden && \
    CUDA_HOME="" python3 -m pip install --no-cache-dir --no-build-isolation \
    "earth2grid @ git+https://github.com/NVlabs/earth2grid@11dcf1b0787a7eb6a8497a3a5a5e1fdcc31232d3" && \
    mv /usr/local/_cuda_hidden /usr/local/cuda

# Step 3: Install remaining requirements (torch + earth2grid satisfied).
# --no-binary natten forces NATTEN to compile from source using nvcc instead
# of downloading a CPU-only wheel from PyPI. This produces CUDA kernels for
# the architectures listed in TORCH_CUDA_ARCH_LIST (set in ENV above).
RUN python3 -m pip install --no-cache-dir --no-binary natten \
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
COPY runpod/atlas_engine.py /app/atlas_engine.py
COPY runpod/nowcast_engine.py /app/nowcast_engine.py
COPY runpod/canonical_translator.py /app/canonical_translator.py
COPY runpod/regridder.py /app/regridder.py
COPY runpod/zarr_writer.py /app/zarr_writer.py
COPY runpod/sim.py /app/sim.py

WORKDIR /app

# ---------------------------------------------------------------------------
# Entrypoint: RunPod Serverless handler
# ---------------------------------------------------------------------------
# Matches 11-runpod.md Section 6.1.
# The -u flag ensures unbuffered stdout/stderr for real-time log streaming.
CMD [ "python3", "-u", "handler.py" ]
