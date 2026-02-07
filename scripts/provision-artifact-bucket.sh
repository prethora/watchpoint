#!/usr/bin/env bash
# provision-artifact-bucket.sh
#
# Idempotently provisions the S3 artifact bucket used for SAM deployment
# artifacts (Lambda zip packages, CloudFormation templates). Applies a
# lifecycle policy to expire objects after 30 days, preventing cost
# accumulation from stale deployment artifacts.
#
# The bucket name follows the convention:
#   watchpoint-artifacts-{account_id}-{region}
#
# This naming guarantees global uniqueness (account + region) while
# remaining predictable for automation.
#
# Environment variables:
#   AWS_REGION        - (Optional) AWS region. Defaults to us-east-1.
#   AWS_ENDPOINT_URL  - (Optional) Override endpoint for LocalStack.
#
# Usage:
#   ./scripts/provision-artifact-bucket.sh
#   AWS_REGION=us-west-2 ./scripts/provision-artifact-bucket.sh
#
# Output:
#   On success, the last line of stdout is the bucket name. All
#   informational/diagnostic messages are written to stderr so that
#   callers can capture the bucket name cleanly:
#     BUCKET=$(./scripts/provision-artifact-bucket.sh)
#
# Exit codes:
#   0 - Bucket exists (created or already present) with lifecycle policy applied
#   1 - Fatal error (missing tools, invalid credentials, API failure)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
LIFECYCLE_EXPIRATION_DAYS=30

# ---------------------------------------------------------------------------
# Output helpers (all to stderr to keep stdout clean for the bucket name)
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
# Compute the bucket name
# ---------------------------------------------------------------------------
compute_bucket_name() {
    BUCKET_NAME="watchpoint-artifacts-${ACCOUNT_ID}-${REGION}"
    info "Target bucket: ${BUCKET_NAME}"
}

# ---------------------------------------------------------------------------
# Create the bucket (idempotent)
# ---------------------------------------------------------------------------
create_bucket() {
    # Check if the bucket already exists and is owned by us
    if aws s3api head-bucket --bucket "${BUCKET_NAME}" 2>/dev/null; then
        ok "Bucket '${BUCKET_NAME}' already exists."
        return 0
    fi

    info "Creating bucket '${BUCKET_NAME}'..."

    # The CreateBucket API requires a LocationConstraint for all regions
    # except us-east-1. For us-east-1, the constraint must be omitted entirely.
    if [[ "${REGION}" == "us-east-1" ]]; then
        if ! aws s3api create-bucket \
            --bucket "${BUCKET_NAME}" \
            --region "${REGION}" 1>&2; then
            err "Failed to create bucket '${BUCKET_NAME}'."
            exit 1
        fi
    else
        if ! aws s3api create-bucket \
            --bucket "${BUCKET_NAME}" \
            --region "${REGION}" \
            --create-bucket-configuration "LocationConstraint=${REGION}" 1>&2; then
            err "Failed to create bucket '${BUCKET_NAME}'."
            exit 1
        fi
    fi

    ok "Bucket '${BUCKET_NAME}' created successfully."
}

# ---------------------------------------------------------------------------
# Apply lifecycle configuration
# ---------------------------------------------------------------------------
apply_lifecycle_policy() {
    info "Applying lifecycle policy (expire after ${LIFECYCLE_EXPIRATION_DAYS} days)..."

    local lifecycle_config
    lifecycle_config=$(python3 -c "
import json
config = {
    'Rules': [
        {
            'ID': 'ExpireArtifacts',
            'Status': 'Enabled',
            'Filter': {
                'Prefix': ''
            },
            'Expiration': {
                'Days': ${LIFECYCLE_EXPIRATION_DAYS}
            }
        }
    ]
}
print(json.dumps(config))
")

    if ! aws s3api put-bucket-lifecycle-configuration \
        --bucket "${BUCKET_NAME}" \
        --lifecycle-configuration "${lifecycle_config}" 1>&2; then
        err "Failed to apply lifecycle configuration to '${BUCKET_NAME}'."
        exit 1
    fi

    ok "Lifecycle policy applied: objects expire after ${LIFECYCLE_EXPIRATION_DAYS} days."
}

# ---------------------------------------------------------------------------
# Enable versioning (SAM best practice for artifact buckets)
# ---------------------------------------------------------------------------
enable_versioning() {
    info "Enabling bucket versioning..."

    if ! aws s3api put-bucket-versioning \
        --bucket "${BUCKET_NAME}" \
        --versioning-configuration "Status=Enabled" 1>&2; then
        warn "Failed to enable versioning on '${BUCKET_NAME}'. Non-fatal, continuing."
        return 0
    fi

    ok "Bucket versioning enabled."
}

# ---------------------------------------------------------------------------
# Block public access (security hardening)
# ---------------------------------------------------------------------------
block_public_access() {
    info "Blocking all public access..."

    if ! aws s3api put-public-access-block \
        --bucket "${BUCKET_NAME}" \
        --public-access-block-configuration \
            "BlockPublicAcls=true,IgnorePublicAcls=true,BlockPublicPolicy=true,RestrictPublicBuckets=true" 1>&2; then
        warn "Failed to set public access block on '${BUCKET_NAME}'. Non-fatal, continuing."
        return 0
    fi

    ok "Public access blocked."
}

# ---------------------------------------------------------------------------
# Verify the bucket is operational
# ---------------------------------------------------------------------------
verify_bucket() {
    info "Verifying bucket is accessible..."

    if ! aws s3api head-bucket --bucket "${BUCKET_NAME}" 2>/dev/null; then
        err "Bucket '${BUCKET_NAME}' is not accessible after provisioning."
        exit 1
    fi

    # Verify lifecycle is applied
    local lifecycle_check
    if lifecycle_check=$(aws s3api get-bucket-lifecycle-configuration \
        --bucket "${BUCKET_NAME}" --output json 2>&1); then
        local rule_count
        rule_count=$(echo "${lifecycle_check}" | python3 -c "import sys,json; print(len(json.load(sys.stdin).get('Rules',[])))" 2>/dev/null || echo "0")
        ok "Bucket verified. Lifecycle rules: ${rule_count}."
    else
        warn "Could not verify lifecycle configuration. This may be a permissions issue."
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint Artifact Bucket Provisioner" >&2
    echo "============================================" >&2
    echo "" >&2

    check_prerequisites
    resolve_identity
    compute_bucket_name
    create_bucket
    apply_lifecycle_policy
    enable_versioning
    block_public_access
    verify_bucket

    echo "" >&2
    echo "============================================" >&2
    ok "Artifact bucket provisioned successfully."
    echo "============================================" >&2
    echo "" >&2

    # Output ONLY the bucket name to stdout for programmatic consumption
    echo "${BUCKET_NAME}"
}

main "$@"
