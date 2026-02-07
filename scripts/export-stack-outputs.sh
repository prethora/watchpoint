#!/usr/bin/env bash
# export-stack-outputs.sh
#
# Post-deployment script that extracts CloudFormation stack outputs and writes
# them to two files for downstream consumption:
#
#   stack-outputs.json  - Full JSON map of output keys to values (for tools,
#                         scripts, and CI pipelines).
#   .env.cloud          - Shell-sourceable environment variables (for local
#                         testing against cloud resources).
#
# The script queries the deployed CloudFormation stack using
# `aws cloudformation describe-stacks` and parses the Outputs array.
#
# Expected output keys (from 04-sam-template.md Section 8):
#   ApiEndpoint            - URL of the HTTP API
#   ForecastBucketName     - Name of the S3 forecast bucket
#   ArchiveBucketName      - Name of the S3 archive bucket
#   RunPodUserArn          - ARN of the IAM user for RunPod S3 access
#   EvalWorkerRepositoryUri - ECR URI for pushing Python eval-worker images
#   NotificationQueueUrl   - URL of the SQS notification queue
#
# Environment variables:
#   STACK_NAME   - (Optional) CloudFormation stack name. Defaults to
#                  "watchpoint-dev". Common values: watchpoint-dev,
#                  watchpoint-staging, watchpoint-prod.
#   AWS_REGION   - (Optional) AWS region. Defaults to us-east-1.
#   OUTPUT_DIR   - (Optional) Directory to write output files. Defaults to
#                  the project root.
#
# Usage:
#   ./scripts/export-stack-outputs.sh
#   STACK_NAME=watchpoint-prod ./scripts/export-stack-outputs.sh
#   STACK_NAME=watchpoint-staging AWS_REGION=us-west-2 ./scripts/export-stack-outputs.sh
#
# Output:
#   Writes stack-outputs.json and .env.cloud to the project root (or OUTPUT_DIR).
#   All diagnostic messages go to stderr. On success, the path to
#   stack-outputs.json is printed to stdout for programmatic consumption.
#
# Exit codes:
#   0 - Outputs exported successfully
#   1 - Fatal error (missing tools, stack not found, no outputs)

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------
DEFAULT_REGION="us-east-1"
DEFAULT_STACK_NAME="watchpoint-dev"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "${SCRIPT_DIR}/.." && pwd)"

# The output keys we expect from the stack, per 04-sam-template.md Section 8.
# Used for validation -- a warning is emitted for any missing key.
EXPECTED_KEYS=(
    "ApiEndpoint"
    "ForecastBucketName"
    "ArchiveBucketName"
    "RunPodUserArn"
    "EvalWorkerRepositoryUri"
    "NotificationQueueUrl"
)

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
        err "python3 is required for JSON processing but was not found."
        exit 1
    fi
}

# ---------------------------------------------------------------------------
# Resolve configuration
# ---------------------------------------------------------------------------
resolve_config() {
    STACK_NAME="${STACK_NAME:-${DEFAULT_STACK_NAME}}"
    REGION="${AWS_REGION:-${DEFAULT_REGION}}"
    OUTPUT_DIR="${OUTPUT_DIR:-${PROJECT_ROOT}}"

    JSON_FILE="${OUTPUT_DIR}/stack-outputs.json"
    ENV_FILE="${OUTPUT_DIR}/.env.cloud"

    info "Stack name:  ${STACK_NAME}"
    info "AWS Region:  ${REGION}"
    info "Output dir:  ${OUTPUT_DIR}"
}

# ---------------------------------------------------------------------------
# Fetch stack outputs from CloudFormation
# ---------------------------------------------------------------------------
fetch_stack_outputs() {
    info "Fetching outputs from stack '${STACK_NAME}'..."

    local stack_response
    if ! stack_response=$(aws cloudformation describe-stacks \
        --stack-name "${STACK_NAME}" \
        --region "${REGION}" \
        --output json 2>&1); then
        err "Failed to describe stack '${STACK_NAME}' in region '${REGION}'."
        err "Detail: ${stack_response}"
        err ""
        err "Possible causes:"
        err "  - The stack has not been deployed yet (run 'sam deploy' first)."
        err "  - The stack name is wrong (check STACK_NAME env var)."
        err "  - AWS credentials are invalid or expired."
        exit 1
    fi

    # Extract the Outputs array. CloudFormation returns Outputs as:
    #   [{"OutputKey": "...", "OutputValue": "...", "Description": "..."}, ...]
    # We transform this into a flat {"key": "value"} map.
    OUTPUTS_JSON=$(echo "${stack_response}" | python3 -c "
import sys, json

data = json.load(sys.stdin)
stacks = data.get('Stacks', [])
if not stacks:
    print('{}')
    sys.exit(0)

stack = stacks[0]
outputs = stack.get('Outputs', [])
if not outputs:
    print('{}')
    sys.exit(0)

result = {}
for output in outputs:
    key = output.get('OutputKey', '')
    value = output.get('OutputValue', '')
    if key:
        result[key] = value

print(json.dumps(result, indent=2, sort_keys=True))
" 2>/dev/null)

    if [[ -z "${OUTPUTS_JSON}" || "${OUTPUTS_JSON}" == "{}" ]]; then
        err "Stack '${STACK_NAME}' has no outputs."
        err "This usually means the stack is still being created or the template"
        err "does not define an Outputs section."
        exit 1
    fi

    # Also extract the stack status for informational purposes
    local stack_status
    stack_status=$(echo "${stack_response}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
stacks = data.get('Stacks', [])
if stacks:
    print(stacks[0].get('StackStatus', 'UNKNOWN'))
else:
    print('UNKNOWN')
" 2>/dev/null || echo "UNKNOWN")

    info "Stack status: ${stack_status}"

    # Count the outputs
    local output_count
    output_count=$(echo "${OUTPUTS_JSON}" | python3 -c "
import sys, json
data = json.load(sys.stdin)
print(len(data))
" 2>/dev/null || echo "0")

    ok "Retrieved ${output_count} output(s) from stack."
}

# ---------------------------------------------------------------------------
# Validate expected output keys are present
# ---------------------------------------------------------------------------
validate_outputs() {
    info "Validating expected output keys..."

    local missing_count=0
    for key in "${EXPECTED_KEYS[@]}"; do
        local present
        present=$(echo "${OUTPUTS_JSON}" | KEY="${key}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
key = os.environ['KEY']
if key in data:
    print('yes')
else:
    print('no')
" 2>/dev/null || echo "no")

        if [[ "${present}" == "yes" ]]; then
            local value
            value=$(echo "${OUTPUTS_JSON}" | KEY="${key}" python3 -c "
import sys, json, os
data = json.load(sys.stdin)
print(data[os.environ['KEY']])
" 2>/dev/null || echo "")
            ok "  ${key} = ${value}"
        else
            warn "  ${key} -- MISSING (not found in stack outputs)"
            missing_count=$((missing_count + 1))
        fi
    done

    if [[ ${missing_count} -gt 0 ]]; then
        warn "${missing_count} expected output key(s) missing. The stack may be partially deployed."
        warn "The script will continue and export whatever outputs are available."
    else
        ok "All expected output keys present."
    fi
}

# ---------------------------------------------------------------------------
# Write stack-outputs.json
# ---------------------------------------------------------------------------
write_json_file() {
    info "Writing ${JSON_FILE}..."

    echo "${OUTPUTS_JSON}" > "${JSON_FILE}"

    if [[ ! -f "${JSON_FILE}" ]]; then
        err "Failed to write ${JSON_FILE}."
        exit 1
    fi

    ok "stack-outputs.json written ($(wc -c < "${JSON_FILE}" | tr -d ' ') bytes)."
}

# ---------------------------------------------------------------------------
# Write .env.cloud
# ---------------------------------------------------------------------------
write_env_file() {
    info "Writing ${ENV_FILE}..."

    # Convert the JSON map to KEY=VALUE lines suitable for shell sourcing.
    # Keys are converted from PascalCase to SCREAMING_SNAKE_CASE.
    # For example: ApiEndpoint -> API_ENDPOINT
    #              ForecastBucketName -> FORECAST_BUCKET_NAME
    local env_content
    env_content=$(echo "${OUTPUTS_JSON}" | python3 -c "
import sys, json, re

data = json.load(sys.stdin)

def pascal_to_screaming_snake(name):
    \"\"\"Convert PascalCase to SCREAMING_SNAKE_CASE.\"\"\"
    # Insert underscore before uppercase letters that follow lowercase letters
    # or before uppercase letters that are followed by lowercase letters (for
    # sequences like 'URI' -> 'URI' not 'U_R_I').
    s = re.sub(r'([a-z0-9])([A-Z])', r'\1_\2', name)
    s = re.sub(r'([A-Z]+)([A-Z][a-z])', r'\1_\2', s)
    return s.upper()

lines = []
lines.append('# .env.cloud')
lines.append('#')
lines.append('# Auto-generated by scripts/export-stack-outputs.sh')
lines.append('# Do NOT edit manually -- re-run the export script to update.')
lines.append('#')
lines.append('# Source this file to set cloud resource variables in your shell:')
lines.append('#   source .env.cloud')
lines.append('')

for key in sorted(data.keys()):
    env_key = pascal_to_screaming_snake(key)
    value = data[key]
    # Shell-escape the value: wrap in double quotes, escape existing quotes
    escaped_value = value.replace('\\\\', '\\\\\\\\').replace('\"', '\\\\\"')
    lines.append(f'{env_key}=\"{escaped_value}\"')

print('\\n'.join(lines))
" 2>/dev/null)

    echo "${env_content}" > "${ENV_FILE}"

    if [[ ! -f "${ENV_FILE}" ]]; then
        err "Failed to write ${ENV_FILE}."
        exit 1
    fi

    ok ".env.cloud written ($(wc -c < "${ENV_FILE}" | tr -d ' ') bytes)."
}

# ---------------------------------------------------------------------------
# Display summary
# ---------------------------------------------------------------------------
display_summary() {
    echo "" >&2
    info "Exported stack outputs for '${STACK_NAME}':"
    info "  JSON:  ${JSON_FILE}"
    info "  ENV:   ${ENV_FILE}"
    echo "" >&2
    info "Usage:"
    info "  # Read a value programmatically:"
    info "  python3 -c \"import json; print(json.load(open('stack-outputs.json'))['ApiEndpoint'])\""
    info ""
    info "  # Source cloud variables into your shell:"
    info "  source .env.cloud"
    info "  echo \$API_ENDPOINT"
}

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------
main() {
    echo "" >&2
    echo "============================================" >&2
    echo "  WatchPoint Stack Output Exporter" >&2
    echo "============================================" >&2
    echo "" >&2

    check_prerequisites
    resolve_config
    fetch_stack_outputs
    validate_outputs
    write_json_file
    write_env_file
    display_summary

    echo "" >&2
    echo "============================================" >&2
    ok "Stack outputs exported successfully."
    echo "============================================" >&2
    echo "" >&2

    # Output ONLY the JSON file path to stdout for programmatic consumption
    echo "${JSON_FILE}"
}

main "$@"
