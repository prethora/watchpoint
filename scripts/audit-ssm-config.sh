#!/usr/bin/env bash
# audit-ssm-config.sh
#
# Configuration Integrity Auditor for WatchPoint SSM parameters.
# Verifies that a target environment is fully configured and contains no
# bootstrap placeholders or empty values.
#
# The script recursively lists all SSM parameters under /{env}/watchpoint/
# and asserts that no parameter value equals a known placeholder sentinel
# or is an empty string.
#
# Forbidden values:
#   - "pending_setup"   (initial bootstrap placeholder, see 13-human-setup.md)
#   - "placeholder"     (generic placeholder)
#   - "change_me"       (common unconfigured sentinel)
#   - ""                (empty string)
#
# Usage:
#   ./scripts/audit-ssm-config.sh --env dev
#   ./scripts/audit-ssm-config.sh --env staging
#   ./scripts/audit-ssm-config.sh --env prod
#
# Environment variables:
#   AWS_REGION   - (Optional) AWS region to query. Defaults to us-east-1.
#   AWS_PROFILE  - (Optional) AWS CLI profile to use.
#
# Exit codes:
#   0 - All parameters have valid, non-placeholder values
#   1 - One or more parameters have invalid values, or a fatal error occurred

set -uo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"

# Forbidden placeholder values (case-insensitive comparison)
FORBIDDEN_VALUES=("pending_setup" "placeholder" "change_me")

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
TOTAL_COUNT=0

check_pass() {
    ok "$1"
    PASS_COUNT=$((PASS_COUNT + 1))
}

check_fail() {
    fail "$1"
    FAIL_COUNT=$((FAIL_COUNT + 1))
}

# ---------------------------------------------------------------------------
# Usage
# ---------------------------------------------------------------------------
usage() {
    echo "Usage: $0 --env <environment>"
    echo ""
    echo "Options:"
    echo "  --env    Target environment (dev, staging, prod)"
    echo ""
    echo "Environment variables:"
    echo "  AWS_REGION    AWS region (default: us-east-1)"
    echo "  AWS_PROFILE   AWS CLI profile (optional)"
    exit 1
}

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
parse_args() {
    ENV=""

    while [[ $# -gt 0 ]]; do
        case "$1" in
            --env)
                if [[ -z "${2:-}" ]]; then
                    fail "--env requires a value"
                    usage
                fi
                ENV="$2"
                shift 2
                ;;
            --help|-h)
                usage
                ;;
            *)
                fail "Unknown argument: $1"
                usage
                ;;
        esac
    done

    if [[ -z "${ENV}" ]]; then
        fail "--env flag is required"
        usage
    fi

    # Validate environment value
    case "${ENV}" in
        dev|staging|prod)
            ;;
        *)
            fail "Invalid environment '${ENV}'. Must be one of: dev, staging, prod"
            exit 1
            ;;
    esac

    REGION="${AWS_REGION:-${DEFAULT_REGION}}"
    SSM_PATH="/${ENV}/watchpoint/"
}

# ---------------------------------------------------------------------------
# Pre-checks
# ---------------------------------------------------------------------------
check_prerequisites() {
    if ! command -v aws &>/dev/null; then
        fail "AWS CLI is not installed. Install it from https://aws.amazon.com/cli/"
        exit 1
    fi

    # Verify credentials are valid
    if ! aws sts get-caller-identity &>/dev/null; then
        fail "AWS credentials are invalid or expired. Run 'aws configure' or refresh SSO session."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Fetch all parameter names under the path
# ---------------------------------------------------------------------------
fetch_parameter_names() {
    info "Listing SSM parameters under '${SSM_PATH}'..."

    # Accumulate all parameters across paginated responses.
    # We extract the Parameters array from each page and merge them.
    local all_params="[]"
    local next_token=""

    # Paginate through all parameters using get-parameters-by-path.
    # We use --recursive to find all parameters at any depth under the path.
    while true; do
        local cmd_args=(
            aws ssm get-parameters-by-path
            --path "${SSM_PATH}"
            --recursive
            --with-decryption
            --region "${REGION}"
            --output json
        )

        if [[ -n "${next_token}" ]]; then
            cmd_args+=(--next-token "${next_token}")
        fi

        local response
        if ! response=$("${cmd_args[@]}" 2>&1); then
            fail "Failed to list SSM parameters under '${SSM_PATH}'."
            fail "Error: ${response}"
            fail ""
            fail "Possible causes:"
            fail "  - The environment '${ENV}' has not been bootstrapped yet."
            fail "  - IAM permissions are insufficient (ssm:GetParametersByPath required)."
            fail "  - The AWS region '${REGION}' is incorrect."
            exit 1
        fi

        # Extract Parameters from this page and merge with accumulated results
        all_params=$(echo "${response}" | EXISTING="${all_params}" python3 -c "
import sys, json, os
existing = json.loads(os.environ['EXISTING'])
page = json.load(sys.stdin)
existing.extend(page.get('Parameters', []))
print(json.dumps(existing))
" 2>/dev/null)

        # Check for pagination token
        next_token=$(echo "${response}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data.get('NextToken', ''))
" 2>/dev/null || echo "")

        if [[ -z "${next_token}" ]]; then
            break
        fi
    done

    # Wrap into final format
    PARAMS_JSON=$(echo "${all_params}" | python3 -c "
import sys, json
params = json.load(sys.stdin)
print(json.dumps({'Parameters': params}))
" 2>/dev/null)

    # Count parameters
    TOTAL_COUNT=$(echo "${PARAMS_JSON}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(len(data.get('Parameters', [])))
" 2>/dev/null || echo "0")

    if [[ "${TOTAL_COUNT}" -eq 0 ]]; then
        fail "No SSM parameters found under '${SSM_PATH}'."
        fail "The environment '${ENV}' may not have been bootstrapped."
        fail "Run the bootstrap process (see architecture/13-human-setup.md) first."
        exit 1
    fi

    ok "Found ${TOTAL_COUNT} parameter(s) under '${SSM_PATH}'."
}

# ---------------------------------------------------------------------------
# Audit each parameter value
# ---------------------------------------------------------------------------
audit_parameters() {
    info "Auditing parameter values for placeholders and empty values..."
    echo ""

    # Build the forbidden values pattern for Python (passed via environment)
    local forbidden_csv
    forbidden_csv=$(printf "%s," "${FORBIDDEN_VALUES[@]}")
    forbidden_csv="${forbidden_csv%,}"  # Remove trailing comma

    # Extract parameter names and values, check each one
    local audit_output
    audit_output=$(echo "${PARAMS_JSON}" | FORBIDDEN="${forbidden_csv}" python3 -c "
import sys, json, os

data = json.load(sys.stdin)
params = data.get('Parameters', [])
forbidden = [v.strip().lower() for v in os.environ['FORBIDDEN'].split(',')]

results = []
for param in sorted(params, key=lambda p: p['Name']):
    name = param['Name']
    value = param.get('Value', '')
    param_type = param.get('Type', 'String')

    # Check for empty value
    if value.strip() == '':
        results.append(('FAIL', name, param_type, 'empty value'))
        continue

    # Check for forbidden placeholder values (case-insensitive)
    value_lower = value.strip().lower()
    matched = False
    for fv in forbidden:
        if value_lower == fv:
            results.append(('FAIL', name, param_type, f'contains placeholder: {value}'))
            matched = True
            break

    if not matched:
        results.append(('PASS', name, param_type, ''))

for status, name, ptype, reason in results:
    if status == 'FAIL':
        print(f'FAIL|{name}|{ptype}|{reason}')
    else:
        print(f'PASS|{name}|{ptype}|')
" 2>/dev/null)

    # Process results line by line
    while IFS='|' read -r status name ptype reason; do
        if [[ -z "${status}" ]]; then
            continue
        fi

        if [[ "${status}" == "FAIL" ]]; then
            check_fail "${name} (${ptype}) -- ${reason}"
        else
            # For SecureString params, indicate value is redacted
            if [[ "${ptype}" == "SecureString" ]]; then
                check_pass "${name} (${ptype}, value: ***REDACTED***)"
            else
                check_pass "${name} (${ptype})"
            fi
        fi
    done <<< "${audit_output}"
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    echo ""
    echo "============================================"
    echo "  SSM Configuration Audit Summary"
    echo "============================================"
    echo -e "  Environment: ${BLUE}${ENV}${NC}"
    echo -e "  Path:        ${BLUE}${SSM_PATH}${NC}"
    echo -e "  Total:       ${TOTAL_COUNT}"
    echo -e "  Passed:      ${GREEN}${PASS_COUNT}${NC}"
    echo -e "  Failed:      ${RED}${FAIL_COUNT}${NC}"
    echo "============================================"

    if [[ ${FAIL_COUNT} -gt 0 ]]; then
        echo ""
        fail "Configuration audit FAILED. ${FAIL_COUNT} parameter(s) have invalid values."
        fail "Update the flagged parameters using:"
        fail "  aws ssm put-parameter --name <name> --value <real-value> --type <type> --overwrite --region ${REGION}"
        exit 1
    fi

    echo ""
    ok "All ${TOTAL_COUNT} parameter(s) are properly configured. No placeholders found."
    exit 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo "============================================"
    echo "  WatchPoint SSM Configuration Auditor"
    echo "============================================"
    echo ""

    parse_args "$@"

    info "Target environment: ${ENV}"
    info "SSM path prefix:   ${SSM_PATH}"
    info "AWS region:        ${REGION}"
    echo ""

    check_prerequisites
    fetch_parameter_names
    audit_parameters
    print_summary
}

main "$@"
