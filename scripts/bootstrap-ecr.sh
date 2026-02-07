#!/usr/bin/env bash
# bootstrap-ecr.sh
#
# Idempotently creates the ECR repository required for Python-based
# container image Lambdas (EvalWorkerUrgent, EvalWorkerStandard).
#
# This script resolves the "chicken-and-egg" dependency: the SAM template
# references an ECR repository URI for PackageType: Image functions, but
# that repository must exist before `sam build` can push images to it.
# Running this script before the first `sam deploy` ensures the repository
# is available.
#
# The repository is created with a lifecycle policy that retains only the
# last 10 images (tagged or untagged), matching the specification in
# 04-sam-template.md Section 3.4.
#
# Environment variables:
#   AWS_REGION        - (Optional) AWS region. Defaults to us-east-1.
#   AWS_ENDPOINT_URL  - (Optional) Override endpoint for LocalStack.
#   ECR_REPO_NAME     - (Optional) Repository name. Defaults to watchpoint/eval-worker.
#
# Usage:
#   ./scripts/bootstrap-ecr.sh
#   AWS_REGION=us-west-2 ./scripts/bootstrap-ecr.sh
#   ECR_URI=$(./scripts/bootstrap-ecr.sh)
#
# Output:
#   On success, the last line of stdout is the full ECR Repository URI
#   (e.g., 123456789012.dkr.ecr.us-east-1.amazonaws.com/watchpoint/eval-worker).
#   All informational/diagnostic messages are written to stderr so that
#   callers can capture the URI cleanly:
#     ECR_URI=$(./scripts/bootstrap-ecr.sh)
#
# Exit codes:
#   0 - Repository exists (created or already present) with lifecycle policy applied
#   1 - Fatal error (missing tools, invalid credentials, API failure)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
DEFAULT_REPO_NAME="watchpoint/eval-worker"
LIFECYCLE_MAX_IMAGES=10

# ---------------------------------------------------------------------------
# Output helpers (all to stderr to keep stdout clean for the URI)
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
# Compute repository name and expected URI
# ---------------------------------------------------------------------------
compute_repo_details() {
    REPO_NAME="${ECR_REPO_NAME:-${DEFAULT_REPO_NAME}}"
    EXPECTED_URI="${ACCOUNT_ID}.dkr.ecr.${REGION}.amazonaws.com/${REPO_NAME}"

    info "Target repository: ${REPO_NAME}"
    info "Expected URI:      ${EXPECTED_URI}"
}

# ---------------------------------------------------------------------------
# Create the ECR repository (idempotent)
# ---------------------------------------------------------------------------
create_repository() {
    # Check if the repository already exists
    local describe_output
    if describe_output=$(aws ecr describe-repositories \
        --repository-names "${REPO_NAME}" \
        --region "${REGION}" \
        --output json 2>&1); then

        # Repository exists -- extract the URI from the response
        REPO_URI=$(echo "${describe_output}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['repositories'][0]['repositoryUri'])
" 2>/dev/null)

        if [[ -z "${REPO_URI}" ]]; then
            err "Repository exists but failed to parse URI from describe response."
            exit 1
        fi

        ok "Repository '${REPO_NAME}' already exists."
        ok "URI: ${REPO_URI}"
        return 0
    fi

    # Repository does not exist -- create it
    info "Creating ECR repository '${REPO_NAME}'..."

    local create_output
    if ! create_output=$(aws ecr create-repository \
        --repository-name "${REPO_NAME}" \
        --region "${REGION}" \
        --image-scanning-configuration scanOnPush=true \
        --image-tag-mutability MUTABLE \
        --output json 2>&1); then
        err "Failed to create ECR repository '${REPO_NAME}'."
        err "  Detail: ${create_output}"
        exit 1
    fi

    REPO_URI=$(echo "${create_output}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data['repository']['repositoryUri'])
" 2>/dev/null)

    if [[ -z "${REPO_URI}" ]]; then
        err "Repository created but failed to parse URI from create response."
        exit 1
    fi

    ok "Repository '${REPO_NAME}' created successfully."
    ok "URI: ${REPO_URI}"
}

# ---------------------------------------------------------------------------
# Apply lifecycle policy (retain only last N images)
# ---------------------------------------------------------------------------
apply_lifecycle_policy() {
    info "Applying lifecycle policy (retain last ${LIFECYCLE_MAX_IMAGES} images)..."

    # Build the lifecycle policy JSON.
    # This policy matches both tagged and untagged images and keeps only
    # the most recent N, as specified in 04-sam-template.md Section 3.4.
    local lifecycle_policy
    lifecycle_policy=$(python3 -c "
import json
policy = {
    'rules': [
        {
            'rulePriority': 1,
            'description': 'Retain only the last ${LIFECYCLE_MAX_IMAGES} images (any tag status)',
            'selection': {
                'tagStatus': 'any',
                'countType': 'imageCountMoreThan',
                'countNumber': ${LIFECYCLE_MAX_IMAGES}
            },
            'action': {
                'type': 'expire'
            }
        }
    ]
}
print(json.dumps(policy))
")

    if ! aws ecr put-lifecycle-policy \
        --repository-name "${REPO_NAME}" \
        --region "${REGION}" \
        --lifecycle-policy-text "${lifecycle_policy}" \
        --output json 1>&2; then
        err "Failed to apply lifecycle policy to '${REPO_NAME}'."
        exit 1
    fi

    ok "Lifecycle policy applied: retain last ${LIFECYCLE_MAX_IMAGES} images."
}

# ---------------------------------------------------------------------------
# Verify the repository is accessible and policy is in place
# ---------------------------------------------------------------------------
verify_repository() {
    info "Verifying repository is accessible..."

    if ! aws ecr describe-repositories \
        --repository-names "${REPO_NAME}" \
        --region "${REGION}" \
        --output json &>/dev/null; then
        err "Repository '${REPO_NAME}' is not accessible after provisioning."
        exit 1
    fi

    # Verify lifecycle policy is applied
    local policy_check
    if policy_check=$(aws ecr get-lifecycle-policy \
        --repository-name "${REPO_NAME}" \
        --region "${REGION}" \
        --output json 2>&1); then
        local rule_count
        rule_count=$(echo "${policy_check}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
policy = json.loads(data['lifecyclePolicyText'])
print(len(policy.get('rules', [])))
" 2>/dev/null || echo "0")
        ok "Repository verified. Lifecycle rules: ${rule_count}."
    else
        warn "Could not verify lifecycle policy. This may be a permissions issue."
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint ECR Bootstrap" >&2
    echo "============================================" >&2
    echo "" >&2

    check_prerequisites
    resolve_identity
    compute_repo_details
    create_repository
    apply_lifecycle_policy
    verify_repository

    echo "" >&2
    echo "============================================" >&2
    ok "ECR repository provisioned successfully."
    echo "============================================" >&2
    echo "" >&2

    # Output ONLY the repository URI to stdout for programmatic consumption
    echo "${REPO_URI}"
}

main "$@"
