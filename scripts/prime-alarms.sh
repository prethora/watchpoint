#!/usr/bin/env bash
# prime-alarms.sh
#
# Post-deployment alarm priming script for WatchPoint Dead Man's Switch alarms.
#
# The Dead Man's Switch alarms (OBS-001: MediumRangeStaleAlarm, OBS-002:
# NowcastStaleAlarm) trigger on *missing* data (TreatMissingData: Breaching).
# After a fresh deployment, these alarms are in INSUFFICIENT_DATA state, which
# the Breaching treatment interprets as an alarm condition. This script pushes
# an initial heartbeat metric to CloudWatch to transition both alarms to OK.
#
# What it does:
#   1. Publishes ForecastReady metric (Value=1) for ForecastType=medium_range.
#   2. Publishes ForecastReady metric (Value=1) for ForecastType=nowcast.
#   3. Waits for CloudWatch to evaluate the alarms.
#   4. Verifies both alarms transition to OK state.
#
# Prerequisites:
#   - AWS CLI v2 installed and configured
#   - IAM permissions: cloudwatch:PutMetricData, cloudwatch:DescribeAlarms
#   - The WatchPoint stack must be deployed (alarms must exist)
#
# Usage:
#   ./scripts/prime-alarms.sh --env dev
#   ./scripts/prime-alarms.sh --env staging
#   ./scripts/prime-alarms.sh --env prod
#   ./scripts/prime-alarms.sh --env prod --skip-verify
#
# Environment variables:
#   AWS_REGION   - (Optional) AWS region. Defaults to us-east-1.
#   AWS_PROFILE  - (Optional) AWS CLI profile to use.
#
# Exit codes:
#   0 - Alarms successfully primed and (if verified) transitioned to OK
#   1 - Fatal error (missing tools, failed to push metrics, alarms did not
#       transition within the timeout)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
METRIC_NAMESPACE="WatchPoint"
METRIC_NAME="ForecastReady"
METRIC_VALUE=1

# Forecast types matching the alarm dimensions (04-sam-template.md Section 7.2)
FORECAST_TYPES=("medium_range" "nowcast")

# Verification settings
# CloudWatch evaluates alarms once per period. For nowcast (30 min period),
# the metric must be present in the current evaluation window. We poll for
# up to 3 minutes to allow CloudWatch to pick up the data point and
# re-evaluate the alarm.
VERIFY_TIMEOUT_SECS=180
VERIFY_POLL_INTERVAL_SECS=15

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
# Usage
# ---------------------------------------------------------------------------
usage() {
    echo "Usage: $0 --env <environment> [--skip-verify]"
    echo ""
    echo "Options:"
    echo "  --env           Target environment (dev, staging, prod)"
    echo "  --skip-verify   Skip alarm state verification after pushing metrics"
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
SKIP_VERIFY=false

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
            --skip-verify)
                SKIP_VERIFY=true
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
}

# ---------------------------------------------------------------------------
# Alarm names follow the pattern set in template.yaml:
#   watchpoint-medium-range-stale-{env}
#   watchpoint-nowcast-stale-{env}
# ---------------------------------------------------------------------------
alarm_name_for_type() {
    local forecast_type="$1"
    case "${forecast_type}" in
        medium_range)
            echo "watchpoint-medium-range-stale-${ENV}"
            ;;
        nowcast)
            echo "watchpoint-nowcast-stale-${ENV}"
            ;;
        *)
            fail "Unknown forecast type: ${forecast_type}"
            exit 1
            ;;
    esac
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
    info "Verifying AWS credentials..."
    if ! aws sts get-caller-identity --region "${REGION}" &>/dev/null; then
        fail "AWS credentials are invalid or expired. Run 'aws configure' or refresh SSO session."
        exit 1
    fi

    ok "AWS credentials valid."
}

# ---------------------------------------------------------------------------
# Verify alarms exist before attempting to prime them
# ---------------------------------------------------------------------------
verify_alarms_exist() {
    info "Checking that Dead Man's Switch alarms exist in CloudWatch..."

    local alarm_names=()
    for ft in "${FORECAST_TYPES[@]}"; do
        alarm_names+=("$(alarm_name_for_type "${ft}")")
    done

    local describe_output
    if ! describe_output=$(aws cloudwatch describe-alarms \
        --alarm-names "${alarm_names[@]}" \
        --region "${REGION}" \
        --output json 2>&1); then
        fail "Failed to describe alarms."
        fail "Error: ${describe_output}"
        fail ""
        fail "Ensure the WatchPoint stack is deployed and alarms are created."
        exit 1
    fi

    # Count the alarms returned
    local found_count
    found_count=$(echo "${describe_output}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])
print(len(alarms))
" 2>/dev/null || echo "0")

    if [[ "${found_count}" -ne "${#FORECAST_TYPES[@]}" ]]; then
        fail "Expected ${#FORECAST_TYPES[@]} alarms but found ${found_count}."

        # Show which alarms were found
        local found_names
        found_names=$(echo "${describe_output}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])
for a in alarms:
    print(f'  - {a[\"AlarmName\"]} (State: {a[\"StateValue\"]})')
" 2>/dev/null || echo "  (none)")
        info "Found alarms:"
        echo "${found_names}"

        # Show which are missing
        for name in "${alarm_names[@]}"; do
            local present
            present=$(echo "${describe_output}" | NAME="${name}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
name = os.environ['NAME']
alarms = data.get('MetricAlarms', [])
found = any(a['AlarmName'] == name for a in alarms)
print('yes' if found else 'no')
" 2>/dev/null || echo "no")
            if [[ "${present}" == "no" ]]; then
                fail "Missing alarm: ${name}"
            fi
        done

        fail ""
        fail "Deploy the WatchPoint stack first: sam deploy --stack-name watchpoint-${ENV} ..."
        exit 1
    fi

    # Display current alarm states
    for ft in "${FORECAST_TYPES[@]}"; do
        local name
        name=$(alarm_name_for_type "${ft}")
        local state
        state=$(echo "${describe_output}" | NAME="${name}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
name = os.environ['NAME']
alarms = data.get('MetricAlarms', [])
for a in alarms:
    if a['AlarmName'] == name:
        print(a['StateValue'])
        break
" 2>/dev/null || echo "UNKNOWN")
        info "  ${name}: ${state}"
    done

    ok "Both Dead Man's Switch alarms found."
}

# ---------------------------------------------------------------------------
# Push heartbeat metrics to CloudWatch
# ---------------------------------------------------------------------------
push_heartbeat_metrics() {
    info "Publishing ForecastReady heartbeat metrics..."
    echo ""

    local timestamp
    timestamp=$(date -u +"%Y-%m-%dT%H:%M:%SZ")

    for ft in "${FORECAST_TYPES[@]}"; do
        info "  Pushing ${METRIC_NAME} for ForecastType=${ft}..."

        if ! aws cloudwatch put-metric-data \
            --namespace "${METRIC_NAMESPACE}" \
            --metric-name "${METRIC_NAME}" \
            --dimensions "ForecastType=${ft}" \
            --value "${METRIC_VALUE}" \
            --timestamp "${timestamp}" \
            --region "${REGION}" 2>&1; then
            fail "Failed to push metric for ForecastType=${ft}."
            fail "Ensure IAM permissions include cloudwatch:PutMetricData."
            exit 1
        fi

        ok "  ${METRIC_NAME} (ForecastType=${ft}) = ${METRIC_VALUE} at ${timestamp}"
    done

    echo ""
    ok "Heartbeat metrics published successfully."
}

# ---------------------------------------------------------------------------
# Verify alarm state transitions to OK
# ---------------------------------------------------------------------------
verify_alarm_states() {
    if [[ "${SKIP_VERIFY}" == true ]]; then
        warn "Skipping alarm state verification (--skip-verify)."
        warn "Alarms should transition to OK within 2 minutes."
        return 0
    fi

    echo ""
    info "Verifying alarm state transitions (timeout: ${VERIFY_TIMEOUT_SECS}s)..."
    info "CloudWatch may take up to 2 minutes to re-evaluate alarms."
    echo ""

    local start_time
    start_time=$(date +%s)
    local all_ok=false

    while true; do
        local elapsed
        elapsed=$(( $(date +%s) - start_time ))

        if [[ ${elapsed} -ge ${VERIFY_TIMEOUT_SECS} ]]; then
            break
        fi

        # Collect alarm names
        local alarm_names=()
        for ft in "${FORECAST_TYPES[@]}"; do
            alarm_names+=("$(alarm_name_for_type "${ft}")")
        done

        # Describe all alarms in one call
        local describe_output
        if ! describe_output=$(aws cloudwatch describe-alarms \
            --alarm-names "${alarm_names[@]}" \
            --region "${REGION}" \
            --output json 2>&1); then
            warn "Failed to describe alarms (will retry): ${describe_output}"
            sleep "${VERIFY_POLL_INTERVAL_SECS}"
            continue
        fi

        # Check if all alarms are in OK state
        local states_json
        states_json=$(echo "${describe_output}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
alarms = data.get('MetricAlarms', [])
result = {}
for a in alarms:
    result[a['AlarmName']] = a['StateValue']
print(json.dumps(result))
" 2>/dev/null || echo "{}")

        local ok_count=0
        local total=${#FORECAST_TYPES[@]}

        for ft in "${FORECAST_TYPES[@]}"; do
            local name
            name=$(alarm_name_for_type "${ft}")
            local state
            state=$(echo "${states_json}" | NAME="${name}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
name = os.environ['NAME']
print(data.get(name, 'UNKNOWN'))
" 2>/dev/null || echo "UNKNOWN")

            if [[ "${state}" == "OK" ]]; then
                ok_count=$((ok_count + 1))
            fi
        done

        if [[ ${ok_count} -eq ${total} ]]; then
            all_ok=true
            break
        fi

        local remaining=$((VERIFY_TIMEOUT_SECS - elapsed))
        info "  ${ok_count}/${total} alarms in OK state. Waiting... (${remaining}s remaining)"
        sleep "${VERIFY_POLL_INTERVAL_SECS}"
    done

    echo ""

    if [[ "${all_ok}" == true ]]; then
        ok "All alarms transitioned to OK state."
        echo ""
        # Print final state
        for ft in "${FORECAST_TYPES[@]}"; do
            local name
            name=$(alarm_name_for_type "${ft}")
            ok "  ${name}: OK"
        done
        return 0
    else
        # Print final states for diagnosis
        local alarm_names=()
        for ft in "${FORECAST_TYPES[@]}"; do
            alarm_names+=("$(alarm_name_for_type "${ft}")")
        done

        local describe_output
        describe_output=$(aws cloudwatch describe-alarms \
            --alarm-names "${alarm_names[@]}" \
            --region "${REGION}" \
            --output json 2>/dev/null || echo '{"MetricAlarms":[]}')

        fail "Not all alarms transitioned to OK within ${VERIFY_TIMEOUT_SECS} seconds."
        fail "Current alarm states:"

        for ft in "${FORECAST_TYPES[@]}"; do
            local name
            name=$(alarm_name_for_type "${ft}")
            local state
            state=$(echo "${describe_output}" | NAME="${name}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
name = os.environ['NAME']
alarms = data.get('MetricAlarms', [])
for a in alarms:
    if a['AlarmName'] == name:
        print(a['StateValue'])
        break
else:
    print('NOT_FOUND')
" 2>/dev/null || echo "UNKNOWN")
            fail "  ${name}: ${state}"
        done

        fail ""
        fail "Troubleshooting:"
        fail "  - CloudWatch may need more time. Wait 2-3 minutes and check manually:"
        fail "    aws cloudwatch describe-alarms --alarm-names ${alarm_names[*]} --region ${REGION}"
        fail "  - Verify the alarm Period matches the metric timestamp window."
        fail "  - Check CloudWatch console for alarm history."
        return 1
    fi
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo ""
    echo "============================================"
    echo "  WatchPoint Alarm Primer"
    echo "============================================"
    echo ""

    parse_args "$@"

    info "Target environment: ${ENV}"
    info "AWS region:         ${REGION}"
    info "Metric namespace:   ${METRIC_NAMESPACE}"
    info "Metric name:        ${METRIC_NAME}"
    info "Skip verification:  ${SKIP_VERIFY}"
    echo ""

    check_prerequisites
    verify_alarms_exist
    push_heartbeat_metrics
    verify_alarm_states

    echo ""
    echo "============================================"
    ok "Alarm priming complete for environment '${ENV}'."
    echo "============================================"
    echo ""
}

main "$@"
