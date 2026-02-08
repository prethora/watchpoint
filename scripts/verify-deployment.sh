#!/usr/bin/env bash
# verify-deployment.sh
#
# Post-deployment verification suite for the WatchPoint platform.
# Performs final sanity checks on a live deployment to confirm it is healthy
# and functioning correctly.
#
# Checks performed:
#   1. Version Check: Compares the local git tag (via git describe) against
#      the deployed API version reported by the /health endpoint. Fails if
#      they do not match.
#   2. Alarm Check: Queries all CloudWatch alarms with the "watchpoint-" prefix
#      for the target environment. Asserts all alarms are in OK state. If any
#      are in INSUFFICIENT_DATA state, polls for up to 60 seconds to allow
#      alarm priming to take effect. Fails if any remain non-OK after polling.
#
# Usage:
#   ./scripts/verify-deployment.sh --env dev --api-url https://api.watchpoint.dev
#   ./scripts/verify-deployment.sh --env staging --api-url https://api.staging.watchpoint.io
#   ./scripts/verify-deployment.sh --env prod --api-url https://api.watchpoint.io
#   ./scripts/verify-deployment.sh --env dev --api-url https://... --skip-version
#
# Environment variables:
#   AWS_REGION   - (Optional) AWS region. Defaults to us-east-1.
#   AWS_PROFILE  - (Optional) AWS CLI profile to use.
#
# Exit codes:
#   0 - All checks passed; deployment is verified healthy
#   1 - One or more checks failed, or a fatal error occurred

set -uo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
ALARM_PREFIX="watchpoint-"

# Polling settings for INSUFFICIENT_DATA alarms.
# After alarm priming, CloudWatch may take up to 60 seconds to evaluate.
ALARM_POLL_TIMEOUT_SECS=60
ALARM_POLL_INTERVAL_SECS=10

# Health endpoint path
HEALTH_PATH="/health"

# Curl timeout for API requests (seconds)
CURL_TIMEOUT=10

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
# Usage
# ---------------------------------------------------------------------------
usage() {
    echo "Usage: $0 --env <environment> --api-url <url> [--skip-version]"
    echo ""
    echo "Options:"
    echo "  --env            Target environment (dev, staging, prod)"
    echo "  --api-url        Base URL of the deployed API (e.g., https://api.watchpoint.io)"
    echo "  --skip-version   Skip the version comparison check"
    echo ""
    echo "Environment variables:"
    echo "  AWS_REGION    AWS region (default: us-east-1)"
    echo "  AWS_PROFILE   AWS CLI profile (optional)"
    exit 1
}

# ---------------------------------------------------------------------------
# Parse arguments
# ---------------------------------------------------------------------------
ENV=""
API_URL=""
SKIP_VERSION=false

parse_args() {
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
            --api-url)
                if [[ -z "${2:-}" ]]; then
                    fail "--api-url requires a value"
                    usage
                fi
                API_URL="$2"
                shift 2
                ;;
            --skip-version)
                SKIP_VERSION=true
                shift
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

    if [[ -z "${API_URL}" ]]; then
        fail "--api-url flag is required"
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

    # Strip trailing slash from API URL
    API_URL="${API_URL%/}"

    REGION="${AWS_REGION:-${DEFAULT_REGION}}"
}

# ---------------------------------------------------------------------------
# Pre-checks
# ---------------------------------------------------------------------------
check_prerequisites() {
    local missing=false

    if ! command -v aws &>/dev/null; then
        fail "AWS CLI is not installed. Install it from https://aws.amazon.com/cli/"
        missing=true
    fi

    if ! command -v curl &>/dev/null; then
        fail "curl is not installed."
        missing=true
    fi

    if ! command -v python3 &>/dev/null; then
        fail "python3 is required for JSON processing but was not found."
        missing=true
    fi

    if ! command -v git &>/dev/null; then
        fail "git is not installed."
        missing=true
    fi

    if [[ "${missing}" == true ]]; then
        exit 1
    fi

    # Verify AWS credentials are valid
    info "Verifying AWS credentials..."
    if ! aws sts get-caller-identity --region "${REGION}" &>/dev/null; then
        fail "AWS credentials are invalid or expired. Run 'aws configure' or refresh SSO session."
        exit 1
    fi

    ok "Prerequisites verified."
}

# ---------------------------------------------------------------------------
# Check 1: Version Check
# ---------------------------------------------------------------------------
check_version() {
    echo ""
    echo "--- [1/2] Version Check ---"
    echo ""

    if [[ "${SKIP_VERSION}" == true ]]; then
        check_warn "Version check skipped (--skip-version)."
        return 0
    fi

    # Get local version from git
    local local_version
    if ! local_version=$(git describe --tags --always 2>/dev/null); then
        check_warn "Could not determine local git version (no tags?). Skipping version check."
        return 0
    fi

    info "Local version (git describe): ${local_version}"

    # Get remote version from API health endpoint
    local health_url="${API_URL}${HEALTH_PATH}"
    info "Querying API health endpoint: ${health_url}"

    local health_response
    local http_code
    local tmp_response
    tmp_response=$(mktemp "${TMPDIR:-/tmp}/watchpoint-health-XXXXXX.json")
    # Ensure temp file is cleaned up on any exit path
    trap "rm -f '${tmp_response}'" RETURN

    http_code=$(curl -s -o "${tmp_response}" -w "%{http_code}" \
        --max-time "${CURL_TIMEOUT}" \
        "${health_url}" 2>/dev/null || echo "000")

    if [[ "${http_code}" == "000" ]]; then
        check_fail "API health endpoint unreachable at ${health_url} (connection failed or timed out)."
        return 0
    fi

    if [[ "${http_code}" != "200" ]]; then
        check_fail "API health endpoint returned HTTP ${http_code} (expected 200). Deployment may be unhealthy."
        return 0
    fi

    health_response=$(cat "${tmp_response}" 2>/dev/null || echo "{}")

    # Check if the health response reports healthy status
    local health_status
    health_status=$(echo "${health_response}" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    print(data.get('status', 'unknown'))
except:
    print('unknown')
" 2>/dev/null || echo "unknown")

    if [[ "${health_status}" != "healthy" ]]; then
        check_fail "API reports status '${health_status}' (expected 'healthy')."
        return 0
    fi

    check_pass "API health endpoint returned HTTP 200 with status 'healthy'."

    # Extract version from the health response
    local remote_version
    remote_version=$(echo "${health_response}" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    version = data.get('version', '')
    if not version:
        print('')
    else:
        print(version)
except:
    print('')
" 2>/dev/null || echo "")

    if [[ -z "${remote_version}" ]]; then
        check_warn "API health endpoint does not include a 'version' field. Version comparison skipped."
        info "  To enable version verification, ensure the health response includes a 'version' field"
        info "  matching the build version (set via -ldflags at compile time)."
        return 0
    fi

    info "Remote version (API /health): ${remote_version}"

    # Compare versions
    if [[ "${local_version}" == "${remote_version}" ]]; then
        check_pass "Version match: local '${local_version}' == remote '${remote_version}'."
    else
        check_fail "Version mismatch: local '${local_version}' != remote '${remote_version}'."
        info "  This may indicate the deployment is running an older build."
        info "  Re-deploy or verify the correct git tag is checked out locally."
    fi
}

# ---------------------------------------------------------------------------
# Check 2: CloudWatch Alarm Check
# ---------------------------------------------------------------------------
check_alarms() {
    echo ""
    echo "--- [2/2] CloudWatch Alarm Check ---"
    echo ""

    # Query all alarms with the watchpoint- prefix for this environment
    local alarm_prefix="${ALARM_PREFIX}"
    info "Querying CloudWatch alarms with prefix '${alarm_prefix}' in region '${REGION}'..."

    local describe_output
    if ! describe_output=$(aws cloudwatch describe-alarms \
        --alarm-name-prefix "${alarm_prefix}" \
        --region "${REGION}" \
        --output json 2>&1); then
        check_fail "Failed to describe CloudWatch alarms."
        fail "Error: ${describe_output}"
        return 0
    fi

    # Filter alarms to only those matching the target environment
    # Alarm names follow the pattern: watchpoint-<name>-<env>
    local alarm_data
    alarm_data=$(echo "${describe_output}" | ENV="${ENV}" python3 -c "
import sys, json, os

env = os.environ['ENV']
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])

# Filter alarms that end with the environment suffix
env_suffix = '-' + env
matching = [a for a in alarms if a['AlarmName'].endswith(env_suffix)]

result = {
    'total': len(matching),
    'alarms': []
}
for a in matching:
    result['alarms'].append({
        'name': a['AlarmName'],
        'state': a['StateValue'],
        'description': a.get('AlarmDescription', '')
    })

print(json.dumps(result))
" 2>/dev/null)

    if [[ -z "${alarm_data}" ]]; then
        check_fail "Failed to parse alarm data."
        return 0
    fi

    local total_alarms
    total_alarms=$(echo "${alarm_data}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(data.get('total', 0))
" 2>/dev/null || echo "0")

    if [[ "${total_alarms}" -eq 0 ]]; then
        check_fail "No CloudWatch alarms found matching prefix '${alarm_prefix}' for environment '${ENV}'."
        info "  Ensure the WatchPoint stack is deployed and alarms are created."
        return 0
    fi

    info "Found ${total_alarms} alarm(s) for environment '${ENV}'."

    # Check initial alarm states
    local initial_states
    initial_states=$(echo "${alarm_data}" | python3 -c "
import sys, json

data = json.load(sys.stdin)
alarms = data.get('alarms', [])

ok_count = 0
alarm_count = 0
insufficient_count = 0

for a in alarms:
    state = a['state']
    if state == 'OK':
        ok_count += 1
    elif state == 'ALARM':
        alarm_count += 1
    elif state == 'INSUFFICIENT_DATA':
        insufficient_count += 1

print(json.dumps({
    'ok': ok_count,
    'alarm': alarm_count,
    'insufficient': insufficient_count,
    'total': len(alarms)
}))
" 2>/dev/null)

    local ok_count alarm_count insufficient_count
    ok_count=$(echo "${initial_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['ok'])" 2>/dev/null || echo "0")
    alarm_count=$(echo "${initial_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['alarm'])" 2>/dev/null || echo "0")
    insufficient_count=$(echo "${initial_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['insufficient'])" 2>/dev/null || echo "0")

    info "Initial alarm states: OK=${ok_count}, ALARM=${alarm_count}, INSUFFICIENT_DATA=${insufficient_count}"

    # If all alarms are OK, pass immediately
    if [[ "${ok_count}" -eq "${total_alarms}" ]]; then
        # Print each alarm state
        echo "${alarm_data}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for a in sorted(data['alarms'], key=lambda x: x['name']):
    print(f\"  {a['name']}: {a['state']}\")
" 2>/dev/null
        check_pass "All ${total_alarms} CloudWatch alarm(s) are in OK state."
        return 0
    fi

    # If any are in ALARM state, fail immediately (don't poll)
    if [[ "${alarm_count}" -gt 0 ]]; then
        fail "  ${alarm_count} alarm(s) are in ALARM state:"
        echo "${alarm_data}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for a in sorted(data['alarms'], key=lambda x: x['name']):
    if a['state'] == 'ALARM':
        print(f\"    {a['name']}: ALARM -- {a['description']}\")
" 2>/dev/null
        check_fail "${alarm_count} CloudWatch alarm(s) are in ALARM state."
        return 0
    fi

    # Some alarms are INSUFFICIENT_DATA -- poll for up to ALARM_POLL_TIMEOUT_SECS
    info "Some alarms are in INSUFFICIENT_DATA state. Polling for up to ${ALARM_POLL_TIMEOUT_SECS}s..."
    info "  (This allows time for alarm priming to take effect.)"

    local start_time
    start_time=$(date +%s)
    local all_ok=false

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))

        if [[ ${elapsed} -ge ${ALARM_POLL_TIMEOUT_SECS} ]]; then
            break
        fi

        sleep "${ALARM_POLL_INTERVAL_SECS}"

        # Re-query alarms
        local poll_output
        if ! poll_output=$(aws cloudwatch describe-alarms \
            --alarm-name-prefix "${alarm_prefix}" \
            --region "${REGION}" \
            --output json 2>&1); then
            warn "Failed to describe alarms during polling (will retry)."
            continue
        fi

        # Re-filter for environment and check states
        local poll_states
        poll_states=$(echo "${poll_output}" | ENV="${ENV}" python3 -c "
import sys, json, os

env = os.environ['ENV']
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])

env_suffix = '-' + env
matching = [a for a in alarms if a['AlarmName'].endswith(env_suffix)]

ok_count = sum(1 for a in matching if a['StateValue'] == 'OK')
alarm_count = sum(1 for a in matching if a['StateValue'] == 'ALARM')
insufficient_count = sum(1 for a in matching if a['StateValue'] == 'INSUFFICIENT_DATA')
total = len(matching)

alarms_detail = []
for a in matching:
    alarms_detail.append({
        'name': a['AlarmName'],
        'state': a['StateValue'],
        'description': a.get('AlarmDescription', '')
    })

print(json.dumps({
    'ok': ok_count,
    'alarm': alarm_count,
    'insufficient': insufficient_count,
    'total': total,
    'alarms': alarms_detail
}))
" 2>/dev/null)

        local poll_ok poll_alarm poll_insufficient poll_total
        poll_ok=$(echo "${poll_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['ok'])" 2>/dev/null || echo "0")
        poll_alarm=$(echo "${poll_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['alarm'])" 2>/dev/null || echo "0")
        poll_insufficient=$(echo "${poll_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['insufficient'])" 2>/dev/null || echo "0")
        poll_total=$(echo "${poll_states}" | python3 -c "import sys,json; print(json.load(sys.stdin)['total'])" 2>/dev/null || echo "0")

        local remaining=$((ALARM_POLL_TIMEOUT_SECS - elapsed))
        info "  Polling: OK=${poll_ok}, ALARM=${poll_alarm}, INSUFFICIENT_DATA=${poll_insufficient} (${remaining}s remaining)"

        # If any alarm has transitioned to ALARM, fail immediately
        if [[ "${poll_alarm}" -gt 0 ]]; then
            echo ""
            fail "  Alarm(s) transitioned to ALARM state during polling:"
            echo "${poll_states}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for a in sorted(data['alarms'], key=lambda x: x['name']):
    if a['state'] == 'ALARM':
        print(f\"    {a['name']}: ALARM -- {a['description']}\")
" 2>/dev/null
            check_fail "${poll_alarm} CloudWatch alarm(s) are in ALARM state."
            return 0
        fi

        # If all OK, success
        if [[ "${poll_ok}" -eq "${poll_total}" ]]; then
            alarm_data="${poll_states}"
            all_ok=true
            break
        fi
    done

    echo ""

    if [[ "${all_ok}" == true ]]; then
        echo "${alarm_data}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for a in sorted(data['alarms'], key=lambda x: x['name']):
    print(f\"  {a['name']}: {a['state']}\")
" 2>/dev/null
        check_pass "All ${total_alarms} CloudWatch alarm(s) are in OK state."
    else
        # Print final states for diagnosis
        fail "Not all alarms transitioned to OK within ${ALARM_POLL_TIMEOUT_SECS} seconds."
        fail "Final alarm states:"

        # Re-query one final time for accurate display
        local final_output
        if final_output=$(aws cloudwatch describe-alarms \
            --alarm-name-prefix "${alarm_prefix}" \
            --region "${REGION}" \
            --output json 2>/dev/null); then

            echo "${final_output}" | ENV="${ENV}" python3 -c "
import sys, json, os

env = os.environ['ENV']
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])

env_suffix = '-' + env
matching = [a for a in alarms if a['AlarmName'].endswith(env_suffix)]

for a in sorted(matching, key=lambda x: x['AlarmName']):
    state = a['StateValue']
    name = a['AlarmName']
    marker = '  ' if state == 'OK' else '  **'
    suffix = '**' if state != 'OK' else ''
    print(f\"    {name}: {state}{suffix}\")
" 2>/dev/null
        fi

        check_fail "Some CloudWatch alarms remain in non-OK state."
        echo ""
        info "Troubleshooting:"
        info "  - Run ./scripts/prime-alarms.sh --env ${ENV} to prime Dead Man's Switch alarms."
        info "  - Check CloudWatch console for alarm history and evaluation details."
        info "  - Verify the WatchPoint stack is fully deployed: sam list stack-outputs --stack-name watchpoint-${ENV}"
    fi
}

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
print_summary() {
    echo ""
    echo "============================================"
    echo "  Deployment Verification Summary"
    echo "============================================"
    echo -e "  Environment: ${BLUE}${ENV}${NC}"
    echo -e "  API URL:     ${BLUE}${API_URL}${NC}"
    echo -e "  Region:      ${BLUE}${REGION}${NC}"
    echo -e "  Passed:      ${GREEN}${PASS_COUNT}${NC}"
    echo -e "  Warnings:    ${YELLOW}${WARN_COUNT}${NC}"
    echo -e "  Failed:      ${RED}${FAIL_COUNT}${NC}"
    echo "============================================"

    if [[ ${FAIL_COUNT} -gt 0 ]]; then
        echo ""
        fail "Deployment verification FAILED. ${FAIL_COUNT} check(s) did not pass."
        echo ""
        exit 1
    fi

    echo ""
    ok "Deployment verification PASSED. All checks successful."
    echo ""
    exit 0
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo "============================================"
    echo "  WatchPoint Deployment Verification"
    echo "============================================"
    echo ""

    parse_args "$@"

    info "Target environment: ${ENV}"
    info "API URL:           ${API_URL}"
    info "AWS region:        ${REGION}"
    info "Skip version:      ${SKIP_VERSION}"
    echo ""

    check_prerequisites
    check_version
    check_alarms
    print_summary
}

main "$@"
