#!/usr/bin/env bash
# provision-runpod-iam.sh
#
# Post-deployment script that creates IAM access keys for the RunPodServiceUser
# and stores them securely in AWS Systems Manager Parameter Store.
#
# The RunPod inference worker (running outside AWS) needs IAM credentials to
# write forecast Zarr data to the WatchPoint S3 ForecastBucket. The SAM
# template creates the IAM user with scoped S3 permissions (04-sam-template.md
# Section 6), but deliberately does NOT generate access keys to prevent secret
# leakage in CloudFormation outputs.
#
# This script bridges that gap:
#   1. Reads the RunPodUserArn from stack-outputs.json
#   2. Extracts the IAM user name from the ARN
#   3. Creates a new access key pair via `aws iam create-access-key`
#   4. Pushes both credentials to SSM Parameter Store as SecureStrings
#   5. Verifies the parameters were stored successfully
#
# SECURITY: Access keys are NEVER printed to stdout or stderr. The script
# captures them in memory, pushes them to SSM, and then discards them. Only
# a character-length confirmation is displayed.
#
# Prerequisites:
#   - stack-outputs.json must exist (run scripts/export-stack-outputs.sh first)
#   - AWS credentials must have iam:CreateAccessKey and ssm:PutParameter perms
#
# Environment variables:
#   ENV              - (Required) Target environment (dev, staging, prod).
#   STACK_OUTPUTS    - (Optional) Path to stack-outputs.json. Defaults to
#                      stack-outputs.json in the project root.
#   AWS_REGION       - (Optional) AWS region. Defaults to us-east-1.
#
# Usage:
#   ENV=dev ./scripts/provision-runpod-iam.sh
#   ENV=prod STACK_OUTPUTS=/path/to/stack-outputs.json ./scripts/provision-runpod-iam.sh
#
# Output:
#   All diagnostic messages go to stderr. No secrets are printed.
#   On success, the SSM parameter path prefix is printed to stdout for
#   programmatic consumption.
#
# Exit codes:
#   0 - Access keys created and stored in SSM successfully
#   1 - Fatal error (missing tools, missing outputs file, IAM failure, SSM failure)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
VALID_ENVS="dev staging prod"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

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
# Validate and resolve configuration
# ---------------------------------------------------------------------------
resolve_config() {
    # Validate ENV is provided and is one of the allowed values
    if [[ -z "${ENV:-}" ]]; then
        err "ENV is required. Set it to one of: ${VALID_ENVS}"
        err "Example: ENV=dev ./scripts/provision-runpod-iam.sh"
        exit 1
    fi

    if ! echo "${VALID_ENVS}" | grep -qw "${ENV}"; then
        err "ENV must be one of: ${VALID_ENVS} (got '${ENV}')"
        exit 1
    fi

    REGION="${AWS_REGION:-${DEFAULT_REGION}}"
    OUTPUTS_FILE="${STACK_OUTPUTS:-${PROJECT_ROOT}/stack-outputs.json}"

    # SSM parameter paths for the RunPod IAM credentials
    SSM_ACCESS_KEY_PATH="/${ENV}/runpod/aws_access_key_id"
    SSM_SECRET_KEY_PATH="/${ENV}/runpod/aws_secret_access_key"

    info "Environment:    ${ENV}"
    info "AWS Region:     ${REGION}"
    info "Stack outputs:  ${OUTPUTS_FILE}"
    info "SSM prefix:     /${ENV}/runpod/"
}

# ---------------------------------------------------------------------------
# Read RunPodUserArn from stack-outputs.json
# ---------------------------------------------------------------------------
read_runpod_user_arn() {
    if [[ ! -f "${OUTPUTS_FILE}" ]]; then
        err "Stack outputs file not found: ${OUTPUTS_FILE}"
        err "Run scripts/export-stack-outputs.sh first to generate it."
        exit 1
    fi

    RUNPOD_USER_ARN=$(python3 -c "
import sys, json

try:
    with open('${OUTPUTS_FILE}') as f:
        data = json.load(f)
except (json.JSONDecodeError, FileNotFoundError) as e:
    print(f'ERROR: {e}', file=sys.stderr)
    sys.exit(1)

arn = data.get('RunPodUserArn', '')
if not arn:
    print('ERROR: RunPodUserArn not found in stack outputs.', file=sys.stderr)
    print('Available keys: ' + ', '.join(sorted(data.keys())), file=sys.stderr)
    sys.exit(1)

print(arn)
" 2>&2)

    if [[ -z "${RUNPOD_USER_ARN}" ]]; then
        err "Failed to extract RunPodUserArn from ${OUTPUTS_FILE}."
        exit 1
    fi

    ok "RunPodUserArn: ${RUNPOD_USER_ARN}"
}

# ---------------------------------------------------------------------------
# Extract IAM user name from ARN
# ---------------------------------------------------------------------------
extract_user_name() {
    # ARN format: arn:aws:iam::123456789012:user/RunPodServiceUser
    # We need to extract "RunPodServiceUser" (everything after the last /)
    IAM_USER_NAME=$(python3 -c "
arn = '${RUNPOD_USER_ARN}'
# IAM user ARNs have the format: arn:aws:iam::{account}:user/{username}
# or with a path: arn:aws:iam::{account}:user/path/{username}
parts = arn.split('/')
if len(parts) < 2:
    import sys
    print('ERROR: Could not parse user name from ARN: ' + arn, file=sys.stderr)
    sys.exit(1)
# The username is always the last segment after the final /
print(parts[-1])
" 2>&2)

    if [[ -z "${IAM_USER_NAME}" ]]; then
        err "Failed to extract IAM user name from ARN: ${RUNPOD_USER_ARN}"
        exit 1
    fi

    ok "IAM user name: ${IAM_USER_NAME}"
}

# ---------------------------------------------------------------------------
# Check if credentials already exist in SSM
# ---------------------------------------------------------------------------
check_existing_credentials() {
    local existing_key=""

    if aws ssm get-parameter \
        --name "${SSM_ACCESS_KEY_PATH}" \
        --region "${REGION}" \
        --output json &>/dev/null; then
        existing_key="true"
    fi

    if [[ "${existing_key}" == "true" ]]; then
        warn "SSM parameter ${SSM_ACCESS_KEY_PATH} already exists."
        warn "Creating new access keys will NOT invalidate the old ones."
        warn "If you want to rotate credentials, delete the old access key"
        warn "from IAM first, then re-run this script."
        warn ""
        warn "Continuing will create a NEW access key pair and OVERWRITE"
        warn "the SSM parameters with the new values."
        warn ""

        # In non-interactive / CI mode, we proceed (idempotent overwrite).
        # The old IAM access key remains active in IAM -- the operator must
        # manually deactivate it if rotation is intended.
        info "Proceeding with credential creation (SSM parameters will be overwritten)."
    fi
}

# ---------------------------------------------------------------------------
# Create IAM access key (SECURITY CRITICAL: no secrets to stdout/stderr)
# ---------------------------------------------------------------------------
create_access_key() {
    info "Creating IAM access key for user '${IAM_USER_NAME}'..."

    local create_response
    if ! create_response=$(aws iam create-access-key \
        --user-name "${IAM_USER_NAME}" \
        --region "${REGION}" \
        --output json 2>&1); then

        # Check for common failure: too many access keys (limit is 2)
        if echo "${create_response}" | grep -q "LimitExceeded"; then
            err "IAM user '${IAM_USER_NAME}' already has the maximum number of access keys (2)."
            err "To create a new key, first delete an existing one:"
            err "  aws iam list-access-keys --user-name ${IAM_USER_NAME}"
            err "  aws iam delete-access-key --user-name ${IAM_USER_NAME} --access-key-id <KEY_ID>"
            exit 1
        fi

        err "Failed to create access key for user '${IAM_USER_NAME}'."
        # Sanitize the error output to avoid leaking any partial key material
        err "AWS CLI returned a non-zero exit code. Check IAM permissions."
        exit 1
    fi

    # Extract the access key ID and secret from the response.
    # SECURITY: These values are captured into shell variables and NEVER echoed.
    ACCESS_KEY_ID=$(echo "${create_response}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['AccessKey']['AccessKeyId'])
" 2>/dev/null)

    SECRET_ACCESS_KEY=$(echo "${create_response}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['AccessKey']['SecretAccessKey'])
" 2>/dev/null)

    if [[ -z "${ACCESS_KEY_ID}" || -z "${SECRET_ACCESS_KEY}" ]]; then
        err "Failed to parse access key credentials from IAM response."
        exit 1
    fi

    # Confirm receipt without revealing the actual values
    local key_id_len=${#ACCESS_KEY_ID}
    local secret_len=${#SECRET_ACCESS_KEY}
    ok "Access key created (KeyId: ${key_id_len} chars, Secret: ${secret_len} chars)."
}

# ---------------------------------------------------------------------------
# Push credentials to SSM Parameter Store as SecureStrings
# ---------------------------------------------------------------------------
push_to_ssm() {
    info "Storing credentials in SSM Parameter Store..."

    # Store the Access Key ID
    if ! aws ssm put-parameter \
        --name "${SSM_ACCESS_KEY_PATH}" \
        --value "${ACCESS_KEY_ID}" \
        --type SecureString \
        --overwrite \
        --region "${REGION}" \
        --output json &>/dev/null; then
        err "Failed to store AccessKeyId at ${SSM_ACCESS_KEY_PATH}."
        err "The access key was created in IAM but NOT stored in SSM."
        err "You may need to manually store it or delete the key and retry."
        exit 1
    fi

    ok "Stored AccessKeyId at ${SSM_ACCESS_KEY_PATH}"

    # Store the Secret Access Key
    if ! aws ssm put-parameter \
        --name "${SSM_SECRET_KEY_PATH}" \
        --value "${SECRET_ACCESS_KEY}" \
        --type SecureString \
        --overwrite \
        --region "${REGION}" \
        --output json &>/dev/null; then
        err "Failed to store SecretAccessKey at ${SSM_SECRET_KEY_PATH}."
        err "The access key was created in IAM and the KeyId was stored in SSM,"
        err "but the SecretAccessKey was NOT stored. This is a partial failure."
        err "You should delete the IAM access key and retry:"
        err "  aws iam delete-access-key --user-name ${IAM_USER_NAME} --access-key-id <KEY_ID>"
        exit 1
    fi

    ok "Stored SecretAccessKey at ${SSM_SECRET_KEY_PATH}"
}

# ---------------------------------------------------------------------------
# Verify SSM parameters were stored correctly
# ---------------------------------------------------------------------------
verify_ssm_parameters() {
    info "Verifying SSM parameters..."

    local verify_key
    if ! verify_key=$(aws ssm get-parameter \
        --name "${SSM_ACCESS_KEY_PATH}" \
        --region "${REGION}" \
        --output json 2>&1); then
        err "Verification failed: could not read ${SSM_ACCESS_KEY_PATH} from SSM."
        exit 1
    fi

    local verify_secret
    if ! verify_secret=$(aws ssm get-parameter \
        --name "${SSM_SECRET_KEY_PATH}" \
        --region "${REGION}" \
        --output json 2>&1); then
        err "Verification failed: could not read ${SSM_SECRET_KEY_PATH} from SSM."
        exit 1
    fi

    # Confirm both parameters exist and are SecureString type (without decrypting)
    local key_type
    key_type=$(echo "${verify_key}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['Parameter']['Type'])
" 2>/dev/null || echo "UNKNOWN")

    local secret_type
    secret_type=$(echo "${verify_secret}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['Parameter']['Type'])
" 2>/dev/null || echo "UNKNOWN")

    if [[ "${key_type}" != "SecureString" ]]; then
        warn "AccessKeyId parameter type is '${key_type}' (expected SecureString)."
    else
        ok "AccessKeyId parameter verified (type: SecureString)."
    fi

    if [[ "${secret_type}" != "SecureString" ]]; then
        warn "SecretAccessKey parameter type is '${secret_type}' (expected SecureString)."
    else
        ok "SecretAccessKey parameter verified (type: SecureString)."
    fi
}

# ---------------------------------------------------------------------------
# Clear sensitive variables from memory
# ---------------------------------------------------------------------------
clear_secrets() {
    # Overwrite sensitive variables before they go out of scope.
    # This is a best-effort measure -- bash doesn't guarantee secure erasure.
    ACCESS_KEY_ID="CLEARED"
    SECRET_ACCESS_KEY="CLEARED"
    unset ACCESS_KEY_ID
    unset SECRET_ACCESS_KEY
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint RunPod IAM Provisioner" >&2
    echo "============================================" >&2
    echo "" >&2

    check_prerequisites
    resolve_config
    read_runpod_user_arn
    extract_user_name
    check_existing_credentials
    create_access_key
    push_to_ssm
    verify_ssm_parameters
    clear_secrets

    echo "" >&2
    echo "============================================" >&2
    ok "RunPod IAM credentials provisioned successfully."
    echo "============================================" >&2
    echo "" >&2
    info "SSM Parameters:"
    info "  ${SSM_ACCESS_KEY_PATH}"
    info "  ${SSM_SECRET_KEY_PATH}"
    info ""
    info "Configure these in the RunPod worker environment as:"
    info "  AWS_ACCESS_KEY_ID     -> value from ${SSM_ACCESS_KEY_PATH}"
    info "  AWS_SECRET_ACCESS_KEY -> value from ${SSM_SECRET_KEY_PATH}"
    echo "" >&2

    # Output ONLY the SSM prefix to stdout for programmatic consumption
    echo "/${ENV}/runpod/"
}

main "$@"
