# =============================================================================
# Eval Worker Dockerfile (Lambda / ARM64)
# =============================================================================
# Builds the Python-based evaluation worker as an AWS Lambda container image.
#
# Architecture: linux/arm64 (Graviton2) -- matches SAM template Architectures.
# Base image:   AWS Lambda Python 3.11 runtime (Amazon Linux 2).
# Handler:      worker.eval.handler.handler
#
# Build context: Project root (.)
# Build command: docker build --platform linux/arm64 -f worker/eval/Dockerfile .
#
# References:
#   - 07-eval-worker.md Section 6, 7
#   - 04-sam-template.md Section 1 (Architectures: arm64)
#   - 04-sam-template.md Section 5.2 (EvalWorkerUrgent/Standard)
# =============================================================================

# ---------------------------------------------------------------------------
# Stage 1: Builder -- install deps + run contract verification tests
# ---------------------------------------------------------------------------
FROM public.ecr.aws/lambda/python:3.11 AS builder

# Copy requirements first for Docker layer caching.
COPY worker/requirements.txt /tmp/requirements.txt

# Install Python dependencies into the Lambda task root.
# --only-binary :all: forces pre-built wheels (faster, avoids needing
# system compilers). All dependencies have aarch64 manylinux wheels.
# --no-cache-dir minimizes image size.
# --target installs directly into LAMBDA_TASK_ROOT so the Lambda runtime
# can find packages without PYTHONPATH manipulation.
RUN pip install --no-cache-dir --only-binary :all: \
    -r /tmp/requirements.txt \
    -t "${LAMBDA_TASK_ROOT}/"

# Install pytest (needed for contract verification only, not shipped).
RUN pip install --no-cache-dir pytest -t /tmp/test-deps/

# Copy the full worker/ directory for testing.
COPY worker/ ${LAMBDA_TASK_ROOT}/worker/

# ---------------------------------------------------------------------------
# Contract Verification: Run Pydantic model tests during build.
# If the models are out of sync with the schema, the build fails.
# This catches cross-language type drift before deployment.
# ---------------------------------------------------------------------------
ENV PYTHONPATH="${LAMBDA_TASK_ROOT}:/tmp/test-deps"
RUN python -m pytest \
    ${LAMBDA_TASK_ROOT}/worker/eval/test_models.py \
    -v --tb=short --no-header \
    && echo "Contract verification passed."

# Clean worker source and __pycache__ from the builder layer.
# Only pip-installed packages should survive into the runtime stage.
RUN rm -rf ${LAMBDA_TASK_ROOT}/worker/

# ---------------------------------------------------------------------------
# Stage 2: Runtime -- lean image with only production artifacts
# ---------------------------------------------------------------------------
FROM public.ecr.aws/lambda/python:3.11

# Copy pip-installed packages from the builder stage (no worker source,
# no pytest, no test artifacts, no __pycache__).
COPY --from=builder ${LAMBDA_TASK_ROOT}/ ${LAMBDA_TASK_ROOT}/

# Copy production worker source from build context.
# Individual files are listed explicitly to exclude test files (test_*.py)
# and __pycache__/ directories from the runtime image.
COPY worker/__init__.py ${LAMBDA_TASK_ROOT}/worker/__init__.py
COPY worker/eval/__init__.py ${LAMBDA_TASK_ROOT}/worker/eval/__init__.py
COPY worker/eval/config.py ${LAMBDA_TASK_ROOT}/worker/eval/config.py
COPY worker/eval/handler.py ${LAMBDA_TASK_ROOT}/worker/eval/handler.py
COPY worker/eval/models.py ${LAMBDA_TASK_ROOT}/worker/eval/models.py
COPY worker/eval/reader.py ${LAMBDA_TASK_ROOT}/worker/eval/reader.py
COPY worker/eval/repo.py ${LAMBDA_TASK_ROOT}/worker/eval/repo.py
COPY worker/eval/dev_runner.py ${LAMBDA_TASK_ROOT}/worker/eval/dev_runner.py
COPY worker/eval/logic/__init__.py ${LAMBDA_TASK_ROOT}/worker/eval/logic/__init__.py
COPY worker/eval/logic/dedup.py ${LAMBDA_TASK_ROOT}/worker/eval/logic/dedup.py
COPY worker/eval/logic/evaluator.py ${LAMBDA_TASK_ROOT}/worker/eval/logic/evaluator.py
COPY worker/eval/logic/monitor.py ${LAMBDA_TASK_ROOT}/worker/eval/logic/monitor.py

# Set the Lambda handler.
# Format: <module_path>.<function_name>
# The Lambda runtime will import worker.eval.handler and call handler().
CMD ["worker.eval.handler.handler"]
