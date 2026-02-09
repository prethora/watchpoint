# =============================================================================
# RunPod Inference Worker Dockerfile (GPU / x86_64)
# =============================================================================
# Builds the Python-based GPU inference worker for RunPod Serverless.
#
# Architecture: linux/amd64 (NVIDIA GPU instances)
# Base image:   nvidia/cuda:12.4.1-cudnn-runtime-ubuntu22.04
# Entrypoint:   handler.py (RunPod Serverless handler)
#
# Build context: Project root (.)
# Build command: docker build --platform linux/amd64 -f runpod/Dockerfile .
#
# Model weights are NOT baked into the image. They are auto-downloaded by
# Earth-2 Studio on first inference and cached at EARTH2STUDIO_CACHE on the
# persistent RunPod Network Volume.
#
# NATTEN strategy: Pre-built CUDA wheels from whl.natten.org for
# natten 0.17.5 + torch 2.5 + CUDA 12.4. These wheels include compiled
# libnatten with CUDA kernels (FNA + unfused NA), avoiding the "could not
# find a suitable backend" error that occurs with PyPI's CPU-only wheel.
# No source compilation required — fast build times.
#
# References:
#   - 11-runpod.md Section 6.1 (Docker Specification)
#   - 11-runpod.md Section 6.2 (Model Weights Storage)
#   - 11-runpod.md Section 4.1 (Class Structure)
# =============================================================================

# ---------------------------------------------------------------------------
# Base: NVIDIA CUDA 12.4 Runtime with cuDNN
# ---------------------------------------------------------------------------
# Using runtime (not devel) since we install NATTEN from pre-built CUDA wheels
# instead of compiling from source. Smaller image, faster builds.
FROM nvidia/cuda:12.4.1-cudnn-runtime-ubuntu22.04

# ---------------------------------------------------------------------------
# Environment
# ---------------------------------------------------------------------------
ENV PYTHONUNBUFFERED=1 \
    DEBIAN_FRONTEND=noninteractive \
    MODEL_WEIGHTS_PATH="/runpod-volume/weights" \
    EARTH2STUDIO_CACHE="/runpod-volume/weights/earth2studio" \
    CUDA_CACHE_PATH="/runpod-volume/cuda_cache"

# ---------------------------------------------------------------------------
# System dependencies
# ---------------------------------------------------------------------------
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

# Make python3.12 the default and bootstrap pip.
RUN update-alternatives --install /usr/bin/python3 python3 /usr/bin/python3.12 1 && \
    ln -sf /usr/bin/python3.12 /usr/bin/python && \
    python3.12 -m ensurepip --upgrade

# ---------------------------------------------------------------------------
# Python dependencies
# ---------------------------------------------------------------------------
COPY runpod/requirements.txt /tmp/requirements.txt

RUN python3 -m pip install --no-cache-dir --upgrade pip setuptools

# Step 1: Install PyTorch 2.5 with CUDA 12.4 support.
# Matches the NATTEN 0.17.5 pre-built wheel (torch250cu124).
RUN python3 -m pip install --no-cache-dir "torch>=2.5.0,<2.6.0"

# Step 2: Install earth2grid (hide CUDA to skip unnecessary C++ extensions).
RUN mv /usr/local/cuda /usr/local/_cuda_hidden && \
    CUDA_HOME="" python3 -m pip install --no-cache-dir --no-build-isolation \
    "earth2grid @ git+https://github.com/NVlabs/earth2grid@11dcf1b0787a7eb6a8497a3a5a5e1fdcc31232d3" && \
    mv /usr/local/_cuda_hidden /usr/local/cuda

# Step 3: Install NATTEN 0.17.5 from pre-built CUDA wheel (includes libnatten).
# The whl.natten.org wheel includes compiled CUDA kernels for FNA + unfused NA.
# PyPI's CPU-only wheel lacks libnatten, causing "could not find suitable backend"
# errors at runtime. This wheel fixes that while keeping fast build times.
# See: https://github.com/SHI-Labs/NATTEN/blob/main/docs/install.md
RUN python3 -m pip install --no-cache-dir \
    "natten==0.17.5+torch250cu124" -f https://whl.natten.org

# Step 4: Install remaining requirements (torch + earth2grid + natten satisfied).
RUN python3 -m pip install --no-cache-dir \
    -r /tmp/requirements.txt \
    && rm /tmp/requirements.txt

# ---------------------------------------------------------------------------
# Application code
# ---------------------------------------------------------------------------
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
CMD [ "python3", "-u", "handler.py" ]
