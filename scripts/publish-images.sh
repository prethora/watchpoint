#!/usr/bin/env bash
# publish-images.sh - Push Docker images to AWS ECR
#
# Pushes the eval-worker and runpod-worker Docker images to a single ECR
# repository with distinct tag prefixes, as required by 04-sam-template.md
# Section 3.4 (single EvalWorkerRepository for all container images).
#
# Usage:
#   ECR_URI=123456789.dkr.ecr.us-east-1.amazonaws.com/watchpoint-eval-repo ./scripts/publish-images.sh
#   ./scripts/publish-images.sh 123456789.dkr.ecr.us-east-1.amazonaws.com/watchpoint-eval-repo
#   ./scripts/publish-images.sh --dry-run 123456789.dkr.ecr.us-east-1.amazonaws.com/watchpoint-eval-repo
#
# Environment Variables:
#   ECR_URI   - Full ECR repository URI (can also be passed as positional argument)
#   DRY_RUN   - Set to "true" to print commands without executing them
#
# Exit Codes:
#   0 - Success (or dry-run completed)
#   1 - Missing ECR_URI or prerequisites
#   2 - AWS authentication failure
#   3 - ECR login failure
#   4 - Docker tag failure
#   5 - Docker push failure

set -euo pipefail

# ---------------------------------------------------------------------------
# Globals
# ---------------------------------------------------------------------------

DRY_RUN="${DRY_RUN:-false}"
SHA="$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")"

# Colors for terminal output (disabled if not a TTY)
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[0;33m'
    BLUE='\033[0;34m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    BLUE=''
    NC=''
fi

# ---------------------------------------------------------------------------
# Helper Functions
# ---------------------------------------------------------------------------

log_info() {
    echo -e "${BLUE}[INFO]${NC} $*"
}

log_ok() {
    echo -e "${GREEN}[OK]${NC} $*"
}

log_warn() {
    echo -e "${YELLOW}[WARN]${NC} $*"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $*" >&2
}

# Executes a command, or prints it in dry-run mode.
run_cmd() {
    if [ "${DRY_RUN}" = "true" ]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} $*"
        return 0
    fi
    "$@"
}

# ---------------------------------------------------------------------------
# Argument Parsing
# ---------------------------------------------------------------------------

parse_args() {
    while [[ $# -gt 0 ]]; do
        case "$1" in
            --dry-run)
                DRY_RUN="true"
                shift
                ;;
            --help|-h)
                usage
                exit 0
                ;;
            -*)
                log_error "Unknown option: $1"
                usage
                exit 1
                ;;
            *)
                # Positional argument: treat as ECR_URI
                ECR_URI="$1"
                shift
                ;;
        esac
    done
}

usage() {
    cat <<'USAGE'
Usage: publish-images.sh [OPTIONS] [ECR_URI]

Push eval-worker and runpod-worker Docker images to a single AWS ECR repository
with distinct tag prefixes.

Arguments:
  ECR_URI    Full ECR repository URI. Can also be set via ECR_URI env var.
             Example: 123456789.dkr.ecr.us-east-1.amazonaws.com/watchpoint-eval-repo

Options:
  --dry-run  Print commands without executing them
  --help     Show this help message

Environment Variables:
  ECR_URI    Alternative to positional argument
  DRY_RUN    Set to "true" to enable dry-run mode

Tags pushed:
  <ECR_URI>:eval-<SHA>      Eval worker (git SHA)
  <ECR_URI>:eval-latest     Eval worker (latest)
  <ECR_URI>:runpod-<SHA>    RunPod worker (git SHA)
  <ECR_URI>:runpod-latest   RunPod worker (latest)
USAGE
}

# ---------------------------------------------------------------------------
# Prerequisite Checks
# ---------------------------------------------------------------------------

check_prerequisites() {
    local missing=0

    if ! command -v aws &>/dev/null; then
        log_error "AWS CLI is not installed or not in PATH."
        missing=1
    fi

    if ! command -v docker &>/dev/null; then
        log_error "Docker is not installed or not in PATH."
        missing=1
    fi

    if [ -z "${ECR_URI:-}" ]; then
        log_error "ECR_URI is required. Pass it as an argument or set the ECR_URI environment variable."
        usage
        missing=1
    fi

    if [ "$missing" -ne 0 ]; then
        exit 1
    fi

    # Validate ECR_URI format: account.dkr.ecr.region.amazonaws.com/repo-name
    if ! echo "${ECR_URI}" | grep -qE '^[0-9]+\.dkr\.ecr\.[a-z0-9-]+\.amazonaws\.com/.+$'; then
        log_warn "ECR_URI does not match expected format (account.dkr.ecr.region.amazonaws.com/repo)."
        log_warn "Proceeding anyway, but verify the URI is correct."
    fi
}

# Verify the local Docker images exist before attempting to tag/push.
check_local_images() {
    local missing=0

    if ! docker image inspect watchpoint/eval:latest &>/dev/null; then
        log_error "Local image 'watchpoint/eval:latest' not found. Run 'make build-images' first."
        missing=1
    fi

    if ! docker image inspect watchpoint/runpod:latest &>/dev/null; then
        log_error "Local image 'watchpoint/runpod:latest' not found. Run 'make build-images' first."
        missing=1
    fi

    if [ "$missing" -ne 0 ]; then
        exit 1
    fi

    log_ok "Local images found: watchpoint/eval:latest, watchpoint/runpod:latest"
}

# ---------------------------------------------------------------------------
# AWS Authentication
# ---------------------------------------------------------------------------

verify_aws_identity() {
    log_info "Verifying AWS identity..."

    if [ "${DRY_RUN}" = "true" ]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} aws sts get-caller-identity"
        return 0
    fi

    local identity
    if ! identity="$(aws sts get-caller-identity 2>&1)"; then
        log_error "AWS authentication failed. Check your credentials."
        log_error "Output: ${identity}"
        exit 2
    fi

    local account arn
    account="$(echo "${identity}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Account'])" 2>/dev/null || echo "unknown")"
    arn="$(echo "${identity}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Arn'])" 2>/dev/null || echo "unknown")"
    log_ok "Authenticated as ${arn} (Account: ${account})"
}

login_to_ecr() {
    # Extract the registry URI (everything before the first slash) for ECR login
    local registry
    registry="$(echo "${ECR_URI}" | cut -d'/' -f1)"
    local region
    region="$(echo "${registry}" | sed -n 's/.*\.ecr\.\([^.]*\)\..*/\1/p')"

    log_info "Logging into ECR registry: ${registry} (region: ${region:-auto})..."

    if [ "${DRY_RUN}" = "true" ]; then
        echo -e "${YELLOW}[DRY-RUN]${NC} aws ecr get-login-password --region ${region:-us-east-1} | docker login --username AWS --password-stdin ${registry}"
        return 0
    fi

    if ! aws ecr get-login-password --region "${region:-us-east-1}" | docker login --username AWS --password-stdin "${registry}" 2>&1; then
        log_error "ECR login failed. The registry may not be reachable or you may lack permissions."
        log_error "If infrastructure is not yet provisioned, this is expected. Deploy the SAM stack first."
        exit 3
    fi

    log_ok "ECR login successful."
}

# ---------------------------------------------------------------------------
# Tag & Push
# ---------------------------------------------------------------------------

tag_and_push() {
    local source_image="$1"
    local target_tag="$2"
    local full_target="${ECR_URI}:${target_tag}"

    log_info "Tagging: ${source_image} -> ${full_target}"
    if ! run_cmd docker tag "${source_image}" "${full_target}"; then
        log_error "Failed to tag ${source_image} as ${full_target}"
        exit 4
    fi

    log_info "Pushing: ${full_target}"
    if ! run_cmd docker push "${full_target}"; then
        log_error "Failed to push ${full_target}"
        log_error "If infrastructure is not yet provisioned, this is expected."
        exit 5
    fi

    log_ok "Pushed ${full_target}"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

main() {
    parse_args "$@"

    echo ""
    echo "========================================="
    echo "  WatchPoint Image Publisher"
    echo "========================================="
    echo ""

    if [ "${DRY_RUN}" = "true" ]; then
        log_warn "DRY-RUN mode enabled. No changes will be made."
        echo ""
    fi

    # Step 1: Validate prerequisites
    check_prerequisites

    log_info "ECR_URI: ${ECR_URI}"
    log_info "Git SHA: ${SHA}"
    echo ""

    # Step 2: Check local images exist (skip in dry-run since images may not be built)
    if [ "${DRY_RUN}" != "true" ]; then
        check_local_images
    else
        log_info "Skipping local image check in dry-run mode."
    fi

    # Step 3: Verify AWS identity
    verify_aws_identity

    # Step 4: Login to ECR
    login_to_ecr

    echo ""
    log_info "--- Pushing Eval Worker ---"

    # Step 5: Tag and push eval worker
    tag_and_push "watchpoint/eval:latest" "eval-${SHA}"
    tag_and_push "watchpoint/eval:latest" "eval-latest"

    echo ""
    log_info "--- Pushing RunPod Worker ---"

    # Step 6: Tag and push runpod worker
    tag_and_push "watchpoint/runpod:latest" "runpod-${SHA}"
    tag_and_push "watchpoint/runpod:latest" "runpod-latest"

    echo ""
    echo "========================================="
    log_ok "All images published successfully."
    echo "========================================="
    echo ""
    echo "Images pushed to ${ECR_URI}:"
    echo "  eval-${SHA}      (Eval Worker, pinned)"
    echo "  eval-latest      (Eval Worker, rolling)"
    echo "  runpod-${SHA}    (RunPod Worker, pinned)"
    echo "  runpod-latest    (RunPod Worker, rolling)"
    echo ""
}

main "$@"
