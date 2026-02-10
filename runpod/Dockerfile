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
# Performance strategy (validated on A100 SXM 80GB):
#   The main bottleneck is FP32 computation, NOT NATTEN (<2% of inference
#   time). atlas_engine.py applies 4 runtime optimizations:
#   1. TF32 matmul (1.85x) — PyTorch 2.5 defaults this off
#   2. bf16 autocast on model + autoencoder (4.4x cumulative)
#   3. EM sampler: 1 model eval/step vs rk_roberts' 2 (7.3x cumulative)
#   4. torch.compile DiT blocks (8.4x cumulative = 29.9s/step)
#   NATTEN is pulled from PyPI by earth2studio — any working version is fine.
#
# References:
#   - 11-runpod.md Section 6.1 (Docker Specification)
#   - 11-runpod.md Section 6.2 (Model Weights Storage)
#   - 11-runpod.md Section 4.1 (Class Structure)
#   - runpod_experiment_results/REPORT.md (A100 investigation findings)
# =============================================================================

# ---------------------------------------------------------------------------
# Base: NVIDIA CUDA 12.4 Runtime with cuDNN
# ---------------------------------------------------------------------------
# Runtime image is sufficient — no CUDA compilation at build time.
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
RUN python3 -m pip install --no-cache-dir "torch>=2.5.0,<2.6.0"

# Step 2: Install earth2grid (hide CUDA to skip unnecessary C++ extensions).
RUN mv /usr/local/cuda /usr/local/_cuda_hidden && \
    CUDA_HOME="" python3 -m pip install --no-cache-dir --no-build-isolation \
    "earth2grid @ git+https://github.com/NVlabs/earth2grid@11dcf1b0787a7eb6a8497a3a5a5e1fdcc31232d3" && \
    mv /usr/local/_cuda_hidden /usr/local/cuda

# Step 3: Install remaining requirements (earth2studio pulls natten from PyPI).
# NATTEN accounts for <2% of inference time — any working version is fine.
# Performance comes from TF32 + bf16 + EM sampler + torch.compile in atlas_engine.py.
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
