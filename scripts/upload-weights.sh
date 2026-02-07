#!/usr/bin/env bash
# upload-weights.sh
#
# Uploads model weight files to the S3 artifact bucket for RunPod inference
# workers. The weights are stored at s3://{bucket}/weights/ and are
# auto-hydrated by the inference worker on cold start (see 11-runpod.md,
# Section 6.2).
#
# Two models are supported:
#   - Atlas (medium-range forecasting):  weights/atlas.pt
#   - StormScope (nowcast):              weights/stormscope.pt
#
# In mock mode, the script generates 100MB dummy files instead of requiring
# real model weights. This is useful for testing the pipeline end-to-end
# without access to actual trained models.
#
# Arguments:
#   --bucket <name>    (Required) S3 bucket name (the artifact bucket).
#   --atlas <path>     (Optional) Local path to the Atlas model weight file.
#   --nowcast <path>   (Optional) Local path to the StormScope model weight file.
#   --mock             (Optional) Generate and upload 100MB dummy weight files.
#
# At least one of --atlas, --nowcast, or --mock must be provided.
# When --mock is used, --atlas and --nowcast are ignored.
#
# Usage:
#   # Upload real weights
#   ./scripts/upload-weights.sh --bucket watchpoint-artifacts-123-us-east-1 \
#       --atlas /path/to/atlas.pt --nowcast /path/to/stormscope.pt
#
#   # Upload mock weights for testing
#   ./scripts/upload-weights.sh --bucket watchpoint-artifacts-123-us-east-1 --mock
#
# Exit codes:
#   0 - All weight files uploaded and verified successfully
#   1 - Fatal error (missing tools, invalid arguments, upload failure)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
S3_WEIGHTS_PREFIX="weights"
ATLAS_S3_KEY="${S3_WEIGHTS_PREFIX}/atlas.pt"
STORMSCOPE_S3_KEY="${S3_WEIGHTS_PREFIX}/stormscope.pt"
MOCK_FILE_SIZE_MB=100

# ---------------------------------------------------------------------------
# Output helpers (all to stderr to keep stdout clean)
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info()    { echo -e "${BLUE}[INFO]${NC}  $*" >&2; }
ok()      { echo -e "${GREEN}[OK]${NC}    $*" >&2; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*" >&2; }
err()     { echo -e "${RED}[ERROR]${NC} $*" >&2; }

# ---------------------------------------------------------------------------
# Argument parsing
# ---------------------------------------------------------------------------
BUCKET=""
ATLAS_PATH=""
NOWCAST_PATH=""
MOCK_MODE="false"

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --bucket)
                if [[ $# -lt 2 ]]; then
                    err "--bucket requires a value."
                    usage
                    exit 1
                fi
                BUCKET="$2"
                shift 2
                ;;
            --atlas)
                if [[ $# -lt 2 ]]; then
                    err "--atlas requires a file path."
                    usage
                    exit 1
                fi
                ATLAS_PATH="$2"
                shift 2
                ;;
            --nowcast)
                if [[ $# -lt 2 ]]; then
                    err "--nowcast requires a file path."
                    usage
                    exit 1
                fi
                NOWCAST_PATH="$2"
                shift 2
                ;;
            --mock)
                MOCK_MODE="true"
                shift
                ;;
            -h|--help)
                usage
                exit 0
                ;;
            *)
                err "Unknown argument: $1"
                usage
                exit 1
                ;;
        esac
    done
}

usage() {
    cat >&2 <<'USAGE'

Usage: ./scripts/upload-weights.sh --bucket <bucket-name> [OPTIONS]

Required:
  --bucket <name>    S3 bucket name (artifact bucket)

Options:
  --atlas <path>     Path to Atlas model weight file
  --nowcast <path>   Path to StormScope model weight file
  --mock             Generate and upload 100MB dummy weight files
  -h, --help         Show this help message

At least one of --atlas, --nowcast, or --mock must be provided.
When --mock is used, --atlas and --nowcast paths are ignored.

Examples:
  # Upload real weights
  ./scripts/upload-weights.sh --bucket my-bucket --atlas ./atlas.pt --nowcast ./stormscope.pt

  # Upload mock weights for testing
  ./scripts/upload-weights.sh --bucket my-bucket --mock

USAGE
}

# ---------------------------------------------------------------------------
# Validation
# ---------------------------------------------------------------------------
validate_args() {
    if [[ -z "${BUCKET}" ]]; then
        err "--bucket is required."
        usage
        exit 1
    fi

    if [[ "${MOCK_MODE}" == "false" && -z "${ATLAS_PATH}" && -z "${NOWCAST_PATH}" ]]; then
        err "At least one of --atlas, --nowcast, or --mock must be provided."
        usage
        exit 1
    fi

    # Validate local file paths exist (only in non-mock mode)
    if [[ "${MOCK_MODE}" == "false" ]]; then
        if [[ -n "${ATLAS_PATH}" && ! -f "${ATLAS_PATH}" ]]; then
            err "Atlas weight file not found: ${ATLAS_PATH}"
            exit 1
        fi
        if [[ -n "${NOWCAST_PATH}" && ! -f "${NOWCAST_PATH}" ]]; then
            err "StormScope weight file not found: ${NOWCAST_PATH}"
            exit 1
        fi
    fi
}

check_prerequisites() {
    if ! command -v aws &>/dev/null; then
        err "AWS CLI is not installed. Install it from https://aws.amazon.com/cli/"
        exit 1
    fi

    if ! command -v dd &>/dev/null; then
        err "dd is required for mock file generation but was not found."
        exit 1
    fi
}

validate_bucket() {
    info "Validating bucket '${BUCKET}' exists and is accessible..."

    if ! aws s3api head-bucket --bucket "${BUCKET}" 2>/dev/null; then
        err "Bucket '${BUCKET}' does not exist or is not accessible."
        err "  Run ./scripts/provision-artifact-bucket.sh first, or verify the bucket name."
        exit 1
    fi

    ok "Bucket '${BUCKET}' is accessible."
}

# ---------------------------------------------------------------------------
# Mock file generation
# ---------------------------------------------------------------------------
generate_mock_weights() {
    MOCK_TEMP_DIR=$(mktemp -d)
    # Ensure cleanup on exit
    trap 'rm -rf "${MOCK_TEMP_DIR}"' EXIT

    # Determine the correct block-size flag for dd:
    #   macOS (BSD dd): bs=1m (lowercase)
    #   Linux (GNU dd): bs=1M (uppercase)
    local bs_flag="1M"
    if [[ "$(uname)" == "Darwin" ]]; then
        bs_flag="1m"
    fi

    info "Generating mock Atlas weight file (${MOCK_FILE_SIZE_MB}MB)..."
    dd if=/dev/urandom of="${MOCK_TEMP_DIR}/atlas.pt" bs="${bs_flag}" count="${MOCK_FILE_SIZE_MB}" 2>/dev/null
    ok "Mock Atlas weight file generated: ${MOCK_TEMP_DIR}/atlas.pt"

    info "Generating mock StormScope weight file (${MOCK_FILE_SIZE_MB}MB)..."
    dd if=/dev/urandom of="${MOCK_TEMP_DIR}/stormscope.pt" bs="${bs_flag}" count="${MOCK_FILE_SIZE_MB}" 2>/dev/null
    ok "Mock StormScope weight file generated: ${MOCK_TEMP_DIR}/stormscope.pt"

    # Point paths to mock files
    ATLAS_PATH="${MOCK_TEMP_DIR}/atlas.pt"
    NOWCAST_PATH="${MOCK_TEMP_DIR}/stormscope.pt"
}

# ---------------------------------------------------------------------------
# Upload
# ---------------------------------------------------------------------------
upload_file() {
    local local_path="$1"
    local s3_key="$2"
    local label="$3"
    local s3_uri="s3://${BUCKET}/${s3_key}"

    info "Uploading ${label} to ${s3_uri}..."

    local file_size
    file_size=$(get_file_size "${local_path}")
    info "  File size: ${file_size}"

    if ! aws s3 cp "${local_path}" "${s3_uri}" >&2; then
        err "Failed to upload ${label} to ${s3_uri}."
        exit 1
    fi

    ok "${label} uploaded successfully to ${s3_uri}."
}

get_file_size() {
    local path="$1"
    local bytes

    # Use stat with macOS or Linux syntax
    if [[ "$(uname)" == "Darwin" ]]; then
        bytes=$(stat -f%z "${path}" 2>/dev/null || echo "0")
    else
        bytes=$(stat -c%s "${path}" 2>/dev/null || echo "0")
    fi

    # Convert to human-readable
    if [[ ${bytes} -ge 1073741824 ]]; then
        echo "$(( bytes / 1073741824 ))GB"
    elif [[ ${bytes} -ge 1048576 ]]; then
        echo "$(( bytes / 1048576 ))MB"
    elif [[ ${bytes} -ge 1024 ]]; then
        echo "$(( bytes / 1024 ))KB"
    else
        echo "${bytes}B"
    fi
}

# ---------------------------------------------------------------------------
# Verification
# ---------------------------------------------------------------------------
verify_uploads() {
    info "Verifying uploaded weight files..."

    local verified=0
    local expected=0

    if [[ -n "${ATLAS_PATH}" ]]; then
        expected=$((expected + 1))
        if aws s3api head-object --bucket "${BUCKET}" --key "${ATLAS_S3_KEY}" &>/dev/null; then
            ok "Verified: s3://${BUCKET}/${ATLAS_S3_KEY}"
            verified=$((verified + 1))
        else
            err "Verification failed: s3://${BUCKET}/${ATLAS_S3_KEY} not found after upload."
        fi
    fi

    if [[ -n "${NOWCAST_PATH}" ]]; then
        expected=$((expected + 1))
        if aws s3api head-object --bucket "${BUCKET}" --key "${STORMSCOPE_S3_KEY}" &>/dev/null; then
            ok "Verified: s3://${BUCKET}/${STORMSCOPE_S3_KEY}"
            verified=$((verified + 1))
        else
            err "Verification failed: s3://${BUCKET}/${STORMSCOPE_S3_KEY} not found after upload."
        fi
    fi

    if [[ ${verified} -ne ${expected} ]]; then
        err "Upload verification failed: ${verified}/${expected} files verified."
        exit 1
    fi

    ok "All ${verified} weight file(s) verified successfully."
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint Model Weight Uploader" >&2
    echo "============================================" >&2
    echo "" >&2

    parse_args "$@"
    validate_args
    check_prerequisites
    validate_bucket

    if [[ "${MOCK_MODE}" == "true" ]]; then
        warn "Mock mode enabled. Generating dummy weight files."
        generate_mock_weights
    fi

    # Upload Atlas weights (if path is set)
    if [[ -n "${ATLAS_PATH}" ]]; then
        upload_file "${ATLAS_PATH}" "${ATLAS_S3_KEY}" "Atlas model weights"
    else
        warn "No Atlas weight file specified. Skipping."
    fi

    # Upload StormScope weights (if path is set)
    if [[ -n "${NOWCAST_PATH}" ]]; then
        upload_file "${NOWCAST_PATH}" "${STORMSCOPE_S3_KEY}" "StormScope model weights"
    else
        warn "No StormScope weight file specified. Skipping."
    fi

    verify_uploads

    echo "" >&2
    echo "============================================" >&2
    ok "Model weights uploaded successfully."
    echo "  Bucket:  ${BUCKET}" >&2
    echo "  Prefix:  ${S3_WEIGHTS_PREFIX}/" >&2
    echo "============================================" >&2
    echo "" >&2
}

main "$@"
