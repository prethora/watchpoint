#!/usr/bin/env bash
# generate-sam-config.sh
#
# Dynamically generates samconfig.toml for SAM deployments. Resolves the
# artifact bucket name and ECR repository URI from the AWS account, then
# writes environment-specific deployment profiles (dev, staging, prod).
#
# The generated samconfig.toml is listed in .gitignore because it contains
# account-specific values. This script must be run on each developer machine
# or CI environment before the first `sam deploy`.
#
# What it produces:
#   - [dev.deploy.parameters]   -> watchpoint-dev   stack
#   - [staging.deploy.parameters] -> watchpoint-staging stack
#   - [prod.deploy.parameters]  -> watchpoint-prod  stack
#
# Each profile includes:
#   - s3_bucket:           Artifact bucket (watchpoint-artifacts-{account}-{region})
#   - stack_name:          CloudFormation stack name (watchpoint-{env})
#   - region:              Target AWS region
#   - image_repositories:  ECR URI bindings for EvalWorkerUrgent/Standard
#   - parameter_overrides: Template parameters from 04-sam-template.md Section 2.1
#   - capabilities:        CAPABILITY_IAM (for IAM user creation)
#   - confirm_changeset:   true for prod, false for dev/staging
#
# Environment variables:
#   AWS_REGION        - (Optional) AWS region. Defaults to us-east-1.
#   ALERT_EMAIL       - (Optional) Alert email for SNS alarms. Defaults to empty string.
#   RUNPOD_ENDPOINT   - (Optional) RunPod endpoint ID. Defaults to "pending_setup".
#   DOMAIN_NAME       - (Optional) Custom domain for prod. Defaults to empty string.
#   CERTIFICATE_ARN   - (Optional) ACM certificate ARN for custom domain.
#   GOOGLE_CLIENT_ID  - (Optional) Google OAuth client ID.
#   GITHUB_CLIENT_ID  - (Optional) GitHub OAuth client ID.
#   CORS_ORIGINS      - (Optional) CORS allowed origins. Defaults to "*".
#   OUTPUT_FILE       - (Optional) Output path. Defaults to samconfig.toml in project root.
#   ECR_REPO_NAME     - (Optional) ECR repository name. Defaults to watchpoint/eval-worker.
#
# Usage:
#   ./scripts/generate-sam-config.sh
#   ALERT_EMAIL=ops@example.com ./scripts/generate-sam-config.sh
#   AWS_REGION=us-west-2 ALERT_EMAIL=ops@example.com ./scripts/generate-sam-config.sh
#
# Output:
#   Writes samconfig.toml to the project root (or OUTPUT_FILE path).
#   The file path is printed to stdout on success for programmatic consumption.
#   All diagnostic messages go to stderr.
#
# Exit codes:
#   0 - samconfig.toml generated successfully
#   1 - Fatal error (missing tools, invalid credentials, API failure)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
DEFAULT_REPO_NAME="watchpoint/eval-worker"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# ---------------------------------------------------------------------------
# Output helpers (all to stderr to keep stdout clean for the output path)
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
# Pre-checks
# ---------------------------------------------------------------------------
check_prerequisites() {
    if ! command -v aws &>/dev/null; then
        err "AWS CLI is not installed. Install it from https://aws.amazon.com/cli/"
        exit 1
    fi

    if ! command -v python3 &>/dev/null; then
        err "python3 is required for JSON parsing but was not found."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Resolve AWS account ID and region
# ---------------------------------------------------------------------------
resolve_identity() {
    local caller_identity
    if ! caller_identity=$(aws sts get-caller-identity --output json 2>&1); then
        err "Failed to get AWS caller identity. Are credentials configured?"
        err "  Detail: ${caller_identity}"
        exit 1
    fi

    ACCOUNT_ID=$(echo "${caller_identity}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Account'])" 2>/dev/null)
    if [[ -z "${ACCOUNT_ID}" ]]; then
        err "Failed to parse AWS Account ID from caller identity response."
        exit 1
    fi

    REGION="${AWS_REGION:-${DEFAULT_REGION}}"

    info "AWS Account: ${ACCOUNT_ID}"
    info "AWS Region:  ${REGION}"
}

# ---------------------------------------------------------------------------
# Compute artifact bucket name and ECR URI
# ---------------------------------------------------------------------------
compute_resource_names() {
    # Artifact bucket follows the convention from provision-artifact-bucket.sh
    ARTIFACT_BUCKET="watchpoint-artifacts-${ACCOUNT_ID}-${REGION}"
    info "Artifact bucket: ${ARTIFACT_BUCKET}"

    # ECR URI follows the convention from bootstrap-ecr.sh
    local repo_name="${ECR_REPO_NAME:-${DEFAULT_REPO_NAME}}"
    ECR_URI="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/${repo_name}"
    info "ECR URI:         ${ECR_URI}"
}

# ---------------------------------------------------------------------------
# Validate that the artifact bucket exists
# ---------------------------------------------------------------------------
validate_artifact_bucket() {
    info "Validating artifact bucket exists..."

    if aws s3api head-bucket --bucket "${ARTIFACT_BUCKET}" 2>/dev/null; then
        ok "Artifact bucket '${ARTIFACT_BUCKET}' verified."
    else
        warn "Artifact bucket '${ARTIFACT_BUCKET}' not found. Run ./scripts/provision-artifact-bucket.sh first."
        warn "Continuing with generation -- the bucket must exist before 'sam deploy'."
    fi
}

# ---------------------------------------------------------------------------
# Validate that the ECR repository exists
# ---------------------------------------------------------------------------
validate_ecr_repository() {
    info "Validating ECR repository exists..."

    local repo_name="${ECR_REPO_NAME:-${DEFAULT_REPO_NAME}}"
    if aws ecr describe-repositories \
        --repository-names "${repo_name}" \
        --region "${REGION}" \
        --output json &>/dev/null; then
        ok "ECR repository '${repo_name}' verified."
    else
        warn "ECR repository '${repo_name}' not found. Run ./scripts/bootstrap-ecr.sh first."
        warn "Continuing with generation -- the repository must exist before 'sam deploy'."
    fi
}

# ---------------------------------------------------------------------------
# Resolve configuration values from environment with defaults
# ---------------------------------------------------------------------------
resolve_config() {
    ALERT_EMAIL="${ALERT_EMAIL:-}"
    RUNPOD_ENDPOINT="${RUNPOD_ENDPOINT:-pending_setup}"
    DOMAIN_NAME="${DOMAIN_NAME:-}"
    CERTIFICATE_ARN="${CERTIFICATE_ARN:-}"
    GOOGLE_CLIENT_ID="${GOOGLE_CLIENT_ID:-}"
    GITHUB_CLIENT_ID="${GITHUB_CLIENT_ID:-}"
    CORS_ORIGINS="${CORS_ORIGINS:-*}"
    OUTPUT_FILE="${OUTPUT_FILE:-${PROJECT_ROOT}/samconfig.toml}"

    if [[ -z "${ALERT_EMAIL}" ]]; then
        warn "ALERT_EMAIL is not set. SNS alarm notifications will not be delivered."
        warn "Set ALERT_EMAIL=you@example.com to receive Dead Man's Switch and queue depth alerts."
    else
        info "Alert email:     ${ALERT_EMAIL}"
    fi

    info "RunPod endpoint: ${RUNPOD_ENDPOINT}"

    if [[ -n "${DOMAIN_NAME}" ]]; then
        info "Custom domain:   ${DOMAIN_NAME}"
        if [[ -z "${CERTIFICATE_ARN}" ]]; then
            warn "DOMAIN_NAME is set but CERTIFICATE_ARN is empty. Custom domain requires a valid ACM certificate."
        fi
    fi

    info "CORS origins:    ${CORS_ORIGINS}"
    info "Output file:     ${OUTPUT_FILE}"
}

# ---------------------------------------------------------------------------
# Generate a single environment section for samconfig.toml
# ---------------------------------------------------------------------------
# Arguments:
#   $1 - environment name (dev, staging, prod)
#   $2 - confirm_changeset (true/false)
#   $3 - domain_name (empty string for non-prod)
#   $4 - certificate_arn (empty string for non-prod)
#   $5 - cors_origins
generate_env_section() {
    local env_name="$1"
    local confirm_changeset="$2"
    local env_domain="$3"
    local env_cert="$4"
    local env_cors="$5"

    local stack_name="watchpoint-${env_name}"

    # Build parameter_overrides string.
    # Each parameter is a key=value pair. SAM expects them as a space-separated
    # list in a single quoted string within the TOML array.
    local param_overrides="Environment=${env_name}"
    param_overrides="${param_overrides} RunPodEndpointId=${RUNPOD_ENDPOINT}"

    if [[ -n "${ALERT_EMAIL}" ]]; then
        param_overrides="${param_overrides} AlertEmail=${ALERT_EMAIL}"
    fi

    if [[ -n "${env_domain}" ]]; then
        param_overrides="${param_overrides} DomainName=${env_domain}"
    fi

    if [[ -n "${env_cert}" ]]; then
        param_overrides="${param_overrides} CertificateArn=${env_cert}"
    fi

    if [[ -n "${GOOGLE_CLIENT_ID}" ]]; then
        param_overrides="${param_overrides} GoogleClientId=${GOOGLE_CLIENT_ID}"
    fi

    if [[ -n "${GITHUB_CLIENT_ID}" ]]; then
        param_overrides="${param_overrides} GithubClientId=${GITHUB_CLIENT_ID}"
    fi

    if [[ "${env_cors}" != "*" ]]; then
        param_overrides="${param_overrides} CorsAllowedOrigins=${env_cors}"
    fi

    # Write the TOML section
    cat <<TOML_SECTION

[${env_name}]
[${env_name}.deploy]
[${env_name}.deploy.parameters]
stack_name = "${stack_name}"
s3_bucket = "${ARTIFACT_BUCKET}"
s3_prefix = "${stack_name}"
region = "${REGION}"
confirm_changeset = ${confirm_changeset}
capabilities = "CAPABILITY_IAM"
image_repositories = ["EvalWorkerUrgent=${ECR_URI}", "EvalWorkerStandard=${ECR_URI}"]
parameter_overrides = "${param_overrides}"
TOML_SECTION
}

# ---------------------------------------------------------------------------
# Generate the complete samconfig.toml
# ---------------------------------------------------------------------------
generate_samconfig() {
    info "Generating samconfig.toml..."

    local output_content
    output_content=$(cat <<'TOML_HEADER'
# samconfig.toml
#
# Auto-generated by scripts/generate-sam-config.sh
# Do NOT edit manually -- re-run the generator script to update.
#
# This file is listed in .gitignore because it contains account-specific values.
#
# Usage:
#   sam build && sam deploy --config-env dev
#   sam build && sam deploy --config-env staging
#   sam build && sam deploy --config-env prod

version = 0.1
TOML_HEADER
)

    # Dev: no custom domain, no changeset confirmation (fast iteration)
    output_content+=$(generate_env_section "dev" "false" "" "" "${CORS_ORIGINS}")

    # Staging: no custom domain, no changeset confirmation
    output_content+=$(generate_env_section "staging" "false" "" "" "${CORS_ORIGINS}")

    # Prod: custom domain if configured, require changeset confirmation
    output_content+=$(generate_env_section "prod" "true" "${DOMAIN_NAME}" "${CERTIFICATE_ARN}" "${CORS_ORIGINS}")

    # Write to output file
    echo "${output_content}" > "${OUTPUT_FILE}"

    ok "samconfig.toml written to ${OUTPUT_FILE}"
}

# ---------------------------------------------------------------------------
# Verify the generated file
# ---------------------------------------------------------------------------
verify_output() {
    info "Verifying generated samconfig.toml..."

    if [[ ! -f "${OUTPUT_FILE}" ]]; then
        err "Output file '${OUTPUT_FILE}' was not created."
        exit 1
    fi

    local file_size
    file_size=$(wc -c < "${OUTPUT_FILE}" | tr -d ' ')
    if [[ "${file_size}" -lt 100 ]]; then
        err "Output file is suspiciously small (${file_size} bytes). Something went wrong."
        exit 1
    fi

    # Verify all three environments are present
    local env_count=0
    for env_name in dev staging prod; do
        if grep -qF "[${env_name}]" "${OUTPUT_FILE}"; then
            env_count=$((env_count + 1))
        else
            err "Missing [${env_name}] section in generated samconfig.toml."
            exit 1
        fi
    done

    ok "All ${env_count} environment profiles present (dev, staging, prod)."

    # Verify image_repositories contains both EvalWorker mappings
    if grep -q "EvalWorkerUrgent=" "${OUTPUT_FILE}" && grep -q "EvalWorkerStandard=" "${OUTPUT_FILE}"; then
        ok "image_repositories mappings verified (EvalWorkerUrgent, EvalWorkerStandard)."
    else
        err "image_repositories is missing EvalWorker mappings."
        exit 1
    fi

    # Verify artifact bucket is referenced
    if grep -q "${ARTIFACT_BUCKET}" "${OUTPUT_FILE}"; then
        ok "Artifact bucket reference verified."
    else
        err "Artifact bucket '${ARTIFACT_BUCKET}' not found in generated file."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Display summary of what was generated
# ---------------------------------------------------------------------------
display_summary() {
    echo "" >&2
    info "Generated deployment profiles:"
    info "  sam deploy --config-env dev      -> watchpoint-dev"
    info "  sam deploy --config-env staging  -> watchpoint-staging"
    info "  sam deploy --config-env prod     -> watchpoint-prod"
    echo "" >&2

    if [[ -n "${DOMAIN_NAME}" ]]; then
        info "Custom domain '${DOMAIN_NAME}' configured for prod profile only."
    else
        info "No custom domain configured. All profiles use default API Gateway URLs."
    fi

    if [[ "${RUNPOD_ENDPOINT}" == "pending_setup" ]]; then
        warn "RunPodEndpointId is 'pending_setup'. Update after creating the RunPod endpoint:"
        warn "  RUNPOD_ENDPOINT=<your-endpoint-id> ./scripts/generate-sam-config.sh"
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint SAM Config Generator" >&2
    echo "============================================" >&2
    echo "" >&2

    check_prerequisites
    resolve_identity
    compute_resource_names
    validate_artifact_bucket
    validate_ecr_repository
    resolve_config
    generate_samconfig
    verify_output
    display_summary

    echo "" >&2
    echo "============================================" >&2
    ok "samconfig.toml generated successfully."
    echo "============================================" >&2
    echo "" >&2

    # Output ONLY the file path to stdout for programmatic consumption
    echo "${OUTPUT_FILE}"
}

main "$@"
