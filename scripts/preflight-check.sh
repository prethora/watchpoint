#!/usr/bin/env bash
# preflight-check.sh
#
# Pre-flight validation script for WatchPoint cloud deployments.
# Validates that required infrastructure prerequisites are met before
# initiating a SAM deploy, preventing failed CloudFormation stacks.
#
# Checks performed:
#   1. AWS CLI availability and credentials
#   2. SAM CLI availability
#   3. Docker daemon accessibility (required for container image builds)
#   4. Custom domain ACM certificate validation (if DOMAIN_NAME is set)
#
# Environment variables:
#   DOMAIN_NAME       - (Optional) Custom domain for API Gateway (e.g., api.watchpoint.io).
#                       If set, the script verifies an ISSUED ACM certificate exists in us-east-1.
#                       If unset or empty, custom domain checks are skipped with a warning.
#   CERTIFICATE_ARN   - (Optional) Specific ACM Certificate ARN to validate.
#                       If set alongside DOMAIN_NAME, validates this specific certificate.
#                       If unset, searches for any ISSUED certificate matching DOMAIN_NAME.
#   AWS_REGION        - (Optional) Default region for non-ACM operations. Defaults to us-east-1.
#
# Usage:
#   ./scripts/preflight-check.sh
#   DOMAIN_NAME=api.watchpoint.io ./scripts/preflight-check.sh
#   DOMAIN_NAME=api.watchpoint.io CERTIFICATE_ARN=arn:aws:acm:... ./scripts/preflight-check.sh
#
# Exit codes:
#   0 - All checks passed
#   1 - One or more checks failed

set -uo pipefail
# Note: We intentionally do NOT use 'set -e'. The script accumulates pass/fail
# counts and must continue executing after individual check failures to produce
# a complete verification report. The exit code is determined by the final summary.

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
# ACM certificates for API Gateway custom domains must be in us-east-1
ACM_REGION="us-east-1"

# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

info()    { echo -e "${BLUE}[INFO]${NC}  $*"; }
ok()      { echo -e "${GREEN}[OK]${NC}    $*"; }
warn()    { echo -e "${YELLOW}[WARN]${NC}  $*"; }
fail()    { echo -e "${RED}[FAIL]${NC}  $*"; }

# ---------------------------------------------------------------------------
# State tracking
# ---------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0
WARN_COUNT=0

check_pass() {
    ok "$1"
    PASS_COUNT=$((PASS_COUNT + 1))
}

check_fail() {
    fail "$1"
    FAIL_COUNT=$((FAIL_COUNT + 1))
}

check_warn() {
    warn "$1"
    WARN_COUNT=$((WARN_COUNT + 1))
}

# ---------------------------------------------------------------------------
# Check 1: AWS CLI
# ---------------------------------------------------------------------------
check_aws_cli() {
    info "Checking AWS CLI availability..."

    if ! command -v aws &>/dev/null; then
        check_fail "AWS CLI is not installed. Install it from https://aws.amazon.com/cli/"
        return
    fi

    local aws_version
    aws_version=$(aws --version 2>&1)
    check_pass "AWS CLI found: ${aws_version}"

    # Verify credentials are configured and valid
    info "Verifying AWS credentials..."

    local caller_identity
    if ! caller_identity=$(aws sts get-caller-identity --output json 2>&1); then
        check_fail "AWS credentials are invalid or expired. Run 'aws configure' or refresh SSO session."
        echo "  Error: ${caller_identity}"
        return
    fi

    local account_id
    account_id=$(echo "${caller_identity}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Account'])" 2>/dev/null || echo "unknown")
    local arn
    arn=$(echo "${caller_identity}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Arn'])" 2>/dev/null || echo "unknown")

    check_pass "AWS credentials valid. Account: ${account_id}, Identity: ${arn}"
}

# ---------------------------------------------------------------------------
# Check 2: SAM CLI
# ---------------------------------------------------------------------------
check_sam_cli() {
    info "Checking SAM CLI availability..."

    if ! command -v sam &>/dev/null; then
        check_fail "SAM CLI is not installed. Install it from https://docs.aws.amazon.com/serverless-application-model/latest/developerguide/install-sam-cli.html"
        return
    fi

    local sam_version
    sam_version=$(sam --version 2>&1)
    check_pass "SAM CLI found: ${sam_version}"
}

# ---------------------------------------------------------------------------
# Check 3: Docker
# ---------------------------------------------------------------------------
check_docker() {
    info "Checking Docker availability..."

    if ! command -v docker &>/dev/null; then
        check_fail "Docker is not installed. Required for building container images (eval-worker)."
        return
    fi

    # Check if Docker daemon is running
    if ! docker info &>/dev/null; then
        check_fail "Docker daemon is not running. Start Docker Desktop or the Docker service."
        return
    fi

    check_pass "Docker is available and running."
}

# ---------------------------------------------------------------------------
# Check 4: Custom Domain ACM Certificate
# ---------------------------------------------------------------------------
check_acm_certificate() {
    local domain_name="${DOMAIN_NAME:-}"
    local certificate_arn="${CERTIFICATE_ARN:-}"

    info "Checking custom domain configuration..."

    # If no domain name is set, this is a Dev/Staging deployment without custom domain
    if [[ -z "${domain_name}" ]]; then
        check_warn "DOMAIN_NAME is not set. Deploying without a custom domain (API will use default AWS API Gateway URL)."
        return
    fi

    info "Custom domain requested: ${domain_name}"
    info "Searching for ACM certificates in ${ACM_REGION}..."

    # If a specific certificate ARN is provided, validate that directly
    if [[ -n "${certificate_arn}" ]]; then
        validate_specific_certificate "${certificate_arn}" "${domain_name}"
        return
    fi

    # Otherwise, search for a certificate matching the domain name
    search_certificate_by_domain "${domain_name}"
}

validate_specific_certificate() {
    local arn="$1"
    local domain_name="$2"

    info "Validating specific certificate: ${arn}"

    local cert_details
    if ! cert_details=$(aws acm describe-certificate \
        --certificate-arn "${arn}" \
        --region "${ACM_REGION}" \
        --output json 2>&1); then
        check_fail "Failed to describe certificate '${arn}'. Error: ${cert_details}"
        return
    fi

    local cert_status
    cert_status=$(echo "${cert_details}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Certificate']['Status'])" 2>/dev/null || echo "UNKNOWN")

    local cert_domain
    cert_domain=$(echo "${cert_details}" | python3 -c "import sys,json; print(json.load(sys.stdin)['Certificate']['DomainName'])" 2>/dev/null || echo "UNKNOWN")

    if [[ "${cert_status}" != "ISSUED" ]]; then
        check_fail "Certificate '${arn}' has status '${cert_status}' (expected 'ISSUED'). Cannot deploy with custom domain."
        echo "  Certificate domain: ${cert_domain}"
        echo "  If pending validation, complete DNS or email validation first."
        return
    fi

    # Verify the certificate covers the requested domain
    # Check both the primary domain and SANs (Subject Alternative Names)
    local covers_domain="false"

    # Check primary domain name
    if domain_matches "${cert_domain}" "${domain_name}"; then
        covers_domain="true"
    fi

    # Check SANs if primary doesn't match
    if [[ "${covers_domain}" == "false" ]]; then
        local san_list
        san_list=$(echo "${cert_details}" | python3 -c "
import sys, json
cert = json.load(sys.stdin)['Certificate']
sans = cert.get('SubjectAlternativeNames', [])
for san in sans:
    print(san)
" 2>/dev/null || true)

        while IFS= read -r san; do
            if [[ -n "${san}" ]] && domain_matches "${san}" "${domain_name}"; then
                covers_domain="true"
                break
            fi
        done <<< "${san_list}"
    fi

    if [[ "${covers_domain}" == "false" ]]; then
        check_fail "Certificate '${arn}' (domain: ${cert_domain}) does not cover '${domain_name}'."
        return
    fi

    check_pass "Certificate '${arn}' is ISSUED and covers '${domain_name}'."
}

search_certificate_by_domain() {
    local domain_name="$1"

    local cert_list
    if ! cert_list=$(aws acm list-certificates \
        --region "${ACM_REGION}" \
        --certificate-statuses ISSUED \
        --output json 2>&1); then
        check_fail "Failed to list ACM certificates in ${ACM_REGION}. Error: ${cert_list}"
        return
    fi

    # Parse the certificate list and find a match
    # Note: domain_name is passed via environment variable to avoid shell injection
    local match_arn=""
    match_arn=$(echo "${cert_list}" | TARGET_DOMAIN="${domain_name}" python3 -c "
import sys, json, os

data = json.load(sys.stdin)
certs = data.get('CertificateSummaryList', [])
domain = os.environ['TARGET_DOMAIN']

for cert in certs:
    cert_domain = cert.get('DomainName', '')
    # Exact match
    if cert_domain == domain:
        print(cert['CertificateArn'])
        sys.exit(0)
    # Wildcard match: *.example.com covers sub.example.com
    if cert_domain.startswith('*.'):
        wildcard_base = cert_domain[2:]  # e.g., example.com
        # The domain must be a direct subdomain: sub.example.com matches *.example.com
        # but sub.sub.example.com does not
        if domain.endswith(wildcard_base) and domain.count('.') == wildcard_base.count('.') + 1:
            print(cert['CertificateArn'])
            sys.exit(0)

# No match found
sys.exit(1)
" 2>/dev/null) || true

    if [[ -z "${match_arn}" ]]; then
        check_fail "No ISSUED ACM certificate found in ${ACM_REGION} covering '${domain_name}'."
        echo "  To fix this:"
        echo "    1. Request a certificate: aws acm request-certificate --domain-name ${domain_name} --validation-method DNS --region ${ACM_REGION}"
        echo "    2. Complete DNS validation (add the CNAME record to your DNS provider)"
        echo "    3. Wait for status to become ISSUED"
        echo "    4. Re-run this preflight check"
        return
    fi

    # We found a matching certificate -- validate it fully
    validate_specific_certificate "${match_arn}" "${domain_name}"
}

# ---------------------------------------------------------------------------
# Helper: domain_matches
# Checks if a certificate domain (possibly wildcard) matches the target domain
# ---------------------------------------------------------------------------
domain_matches() {
    local cert_domain="$1"
    local target_domain="$2"

    # Exact match
    if [[ "${cert_domain}" == "${target_domain}" ]]; then
        return 0
    fi

    # Wildcard match: *.example.com covers api.example.com
    if [[ "${cert_domain}" == \*.* ]]; then
        local wildcard_base="${cert_domain#\*.}"
        # Target must be exactly one level deeper
        local target_base="${target_domain#*.}"
        if [[ "${target_base}" == "${wildcard_base}" ]] && [[ "${target_domain}" != "${wildcard_base}" ]]; then
            return 0
        fi
    fi

    return 1
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    echo ""
    echo "============================================"
    echo "  Pre-flight Check Summary"
    echo "============================================"
    echo -e "  Passed:   ${GREEN}${PASS_COUNT}${NC}"
    echo -e "  Warnings: ${YELLOW}${WARN_COUNT}${NC}"
    echo -e "  Failed:   ${RED}${FAIL_COUNT}${NC}"
    echo "============================================"

    if [[ ${FAIL_COUNT} -gt 0 ]]; then
        echo ""
        fail "Pre-flight checks FAILED. Resolve the issues above before deploying."
        exit 1
    fi

    echo ""
    ok "All pre-flight checks passed. Ready to deploy."
    exit 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo "============================================"
    echo "  WatchPoint Pre-flight Deployment Check"
    echo "============================================"
    echo ""

    check_aws_cli
    check_sam_cli
    check_docker
    check_acm_certificate

    print_summary
}

main "$@"
