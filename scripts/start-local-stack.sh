#!/usr/bin/env bash
# start-local-stack.sh
#
# Local Service Orchestrator for the WatchPoint platform.
#
# Starts all services required for local end-to-end integration testing:
#   1. Verifies Docker Compose services (postgres, localstack, minio) are healthy.
#   2. Applies database migrations if not already applied.
#   3. Builds Go binaries for the API and notification workers.
#   4. Starts the API server, email-worker (SQS poller), webhook-worker (SQS poller),
#      and the Python eval dev_runner as background processes.
#   5. Redirects all process output to test_artifacts/system.log with service prefixes.
#   6. Traps SIGINT/SIGTERM to kill all background PIDs on exit.
#
# The Batcher is NOT started as a daemon. It is designed for on-demand invocation
# by integration tests (e.g., piping an S3 event to stdin in local mode).
#
# Usage:
#   ./scripts/start-local-stack.sh
#
# Prerequisites:
#   - Docker Compose services must be running: docker compose up -d
#   - Python 3 with required packages (xarray, numpy, boto3, etc.) for eval worker.
#   - Go toolchain installed.
#   - .env file present in project root (copy from .env.example).
#
# Environment Variables (optional overrides):
#   SKIP_DOCKER_CHECK    - Set to "true" to skip Docker health verification.
#   SKIP_MIGRATIONS      - Set to "true" to skip migration step.
#   SKIP_BUILD           - Set to "true" to skip Go build step.
#   API_PORT             - Override the API server port (default: 8080).
#
# Architecture reference: architecture/12-operations.md Section 2
# Flow reference: INT-002 (Forecast Generation to Customer Notification)

set -uo pipefail

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
ARTIFACTS_DIR="$PROJECT_ROOT/test_artifacts"
LOG_FILE="$ARTIFACTS_DIR/system.log"
PID_FILE="$ARTIFACTS_DIR/.pids"

# Docker network used by docker-compose
DOCKER_NETWORK="watchpoint_net"

# Service endpoints (host-accessible)
POSTGRES_HOST="${POSTGRES_HOST:-localhost}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
POSTGRES_DB="${POSTGRES_DB:-watchpoint}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-localdev}"

LOCALSTACK_ENDPOINT="${LOCALSTACK_ENDPOINT:-http://localhost:4566}"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-http://localhost:9000}"

API_PORT="${API_PORT:-8080}"

# Background process PIDs (accumulated for cleanup)
PIDS=()
SERVICE_NAMES=()

# ---------------------------------------------------------------------------
# Logging helpers
# ---------------------------------------------------------------------------

log_info() {
    echo "[orchestrator] $(date '+%Y-%m-%dT%H:%M:%S') INFO  $*"
}

log_error() {
    echo "[orchestrator] $(date '+%Y-%m-%dT%H:%M:%S') ERROR $*" >&2
}

log_warn() {
    echo "[orchestrator] $(date '+%Y-%m-%dT%H:%M:%S') WARN  $*"
}

# ---------------------------------------------------------------------------
# Cleanup: kill all background processes on exit
# ---------------------------------------------------------------------------

cleanup() {
    local exit_code=$?
    echo ""
    log_info "Shutting down local stack..."

    for i in "${!PIDS[@]}"; do
        local pid="${PIDS[$i]}"
        local name="${SERVICE_NAMES[$i]}"
        if kill -0 "$pid" 2>/dev/null; then
            log_info "  Stopping $name (PID $pid)..."
            kill "$pid" 2>/dev/null || true
        fi
    done

    # Give processes a moment to terminate gracefully
    sleep 1

    # Force-kill any that are still running
    for i in "${!PIDS[@]}"; do
        local pid="${PIDS[$i]}"
        local name="${SERVICE_NAMES[$i]}"
        if kill -0 "$pid" 2>/dev/null; then
            log_warn "  Force-killing $name (PID $pid)..."
            kill -9 "$pid" 2>/dev/null || true
        fi
    done

    # Clean up PID file
    rm -f "$PID_FILE"

    log_info "All services stopped."
    exit "$exit_code"
}

trap cleanup SIGINT SIGTERM EXIT

# ---------------------------------------------------------------------------
# Helper: start a background process with prefixed log output
# ---------------------------------------------------------------------------

start_service() {
    local name="$1"
    shift
    local cmd=("$@")

    log_info "Starting $name..."
    log_info "  Command: ${cmd[*]}"

    # Run the command, prefixing each line of stdout and stderr with the
    # service name and timestamp. Output is appended to the log file.
    (
        "${cmd[@]}" 2>&1 | while IFS= read -r line; do
            echo "[$name] $(date '+%H:%M:%S') $line"
        done
    ) >> "$LOG_FILE" 2>&1 &

    local pid=$!
    PIDS+=("$pid")
    SERVICE_NAMES+=("$name")

    # Record PID to file for external tooling
    echo "$name=$pid" >> "$PID_FILE"

    log_info "  $name started (PID $pid)"
}

# ---------------------------------------------------------------------------
# Helper: wait for a service to become ready
# ---------------------------------------------------------------------------

wait_for_url() {
    local url="$1"
    local name="$2"
    local max_attempts="${3:-30}"
    local delay="${4:-1}"

    local attempt=1
    while [ "$attempt" -le "$max_attempts" ]; do
        if curl -sf "$url" > /dev/null 2>&1; then
            return 0
        fi
        sleep "$delay"
        attempt=$((attempt + 1))
    done

    log_error "$name did not become ready at $url after $max_attempts attempts"
    return 1
}

wait_for_pg() {
    local max_attempts="${1:-30}"
    local delay="${2:-1}"

    local attempt=1
    while [ "$attempt" -le "$max_attempts" ]; do
        if PGPASSWORD="$POSTGRES_PASSWORD" psql -h "$POSTGRES_HOST" -p "$POSTGRES_PORT" \
            -U "$POSTGRES_USER" -d "$POSTGRES_DB" -c "SELECT 1" > /dev/null 2>&1; then
            return 0
        fi
        # Fall back to docker exec if psql is not installed on host
        local container_id
        container_id=$(docker compose ps -q postgres 2>/dev/null || true)
        if [ -n "$container_id" ]; then
            if docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" "$container_id" \
                psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "SELECT 1" > /dev/null 2>&1; then
                return 0
            fi
        fi
        sleep "$delay"
        attempt=$((attempt + 1))
    done

    log_error "PostgreSQL did not become ready after $max_attempts attempts"
    return 1
}

# ---------------------------------------------------------------------------
# Step 0: Banner
# ---------------------------------------------------------------------------

echo "============================================================"
echo " WatchPoint Local Service Orchestrator"
echo "============================================================"
echo ""
echo "  Project Root:  $PROJECT_ROOT"
echo "  Log File:      $LOG_FILE"
echo "  API Port:      $API_PORT"
echo ""

# ---------------------------------------------------------------------------
# Step 1: Create artifacts directory and initialize log
# ---------------------------------------------------------------------------

mkdir -p "$ARTIFACTS_DIR"
rm -f "$PID_FILE"

echo "# WatchPoint Local Stack Log" > "$LOG_FILE"
echo "# Started: $(date -u '+%Y-%m-%dT%H:%M:%SZ')" >> "$LOG_FILE"
echo "" >> "$LOG_FILE"

log_info "Log file initialized at $LOG_FILE"

# ---------------------------------------------------------------------------
# Step 2: Verify Docker services are running and healthy
# ---------------------------------------------------------------------------

if [ "${SKIP_DOCKER_CHECK:-}" = "true" ]; then
    log_warn "Skipping Docker health checks (SKIP_DOCKER_CHECK=true)"
else
    log_info "Checking Docker Compose services..."

    check_service_healthy() {
        local service_name="$1"
        local status
        status=$(docker compose ps "$service_name" 2>&1 || true)
        if echo "$status" | grep -q "(healthy)"; then
            log_info "  $service_name: healthy"
            return 0
        elif echo "$status" | grep -q "Up"; then
            log_warn "  $service_name: running but not yet healthy"
            return 1
        else
            log_error "  $service_name: not running"
            return 1
        fi
    }

    all_healthy=true
    for svc in postgres localstack minio; do
        if ! check_service_healthy "$svc"; then
            all_healthy=false
        fi
    done

    if [ "$all_healthy" = false ]; then
        log_error "Not all Docker services are healthy."
        log_error "Please run: docker compose up -d"
        log_error "Then wait for all services to pass health checks."
        log_error "You can check with: docker compose ps"
        exit 1
    fi

    log_info "All Docker services healthy."
    echo ""
fi

# ---------------------------------------------------------------------------
# Step 3: Apply database migrations
# ---------------------------------------------------------------------------

if [ "${SKIP_MIGRATIONS:-}" = "true" ]; then
    log_warn "Skipping database migrations (SKIP_MIGRATIONS=true)"
else
    log_info "Applying database migrations..."

    # Use the dockerized migrate tool via the watchpoint_net network,
    # matching the Makefile target.
    MIGRATE_DOCKER_URL="postgres://$POSTGRES_USER:$POSTGRES_PASSWORD@postgres:$POSTGRES_PORT/$POSTGRES_DB?sslmode=disable"

    if docker run --rm --network "$DOCKER_NETWORK" \
        -v "$PROJECT_ROOT/migrations:/migrations" \
        migrate/migrate \
        -path=/migrations \
        -database "$MIGRATE_DOCKER_URL" \
        up 2>&1; then
        log_info "Migrations applied successfully."
    else
        # migrate returns non-zero if "no change" (already up-to-date).
        # Check if this is just "no change" vs a real error.
        log_warn "Migration command returned non-zero (may be 'no change' if already applied)."
    fi

    echo ""
fi

# ---------------------------------------------------------------------------
# Step 4: Build Go binaries
# ---------------------------------------------------------------------------

if [ "${SKIP_BUILD:-}" = "true" ]; then
    log_warn "Skipping Go build step (SKIP_BUILD=true)"
else
    log_info "Building Go binaries..."

    # Build the API and notification workers.
    # The batcher is not built here (invoked on-demand), but we build it
    # anyway so it is ready for test scripts.
    cd "$PROJECT_ROOT"
    if go build -o "$ARTIFACTS_DIR/api-server" ./cmd/api/ 2>&1; then
        log_info "  Built: api-server"
    else
        log_error "  Failed to build api-server"
        exit 1
    fi

    if go build -o "$ARTIFACTS_DIR/email-worker" ./cmd/email-worker/ 2>&1; then
        log_info "  Built: email-worker"
    else
        log_error "  Failed to build email-worker"
        exit 1
    fi

    if go build -o "$ARTIFACTS_DIR/webhook-worker" ./cmd/webhook-worker/ 2>&1; then
        log_info "  Built: webhook-worker"
    else
        log_error "  Failed to build webhook-worker"
        exit 1
    fi

    if go build -o "$ARTIFACTS_DIR/batcher" ./cmd/batcher/ 2>&1; then
        log_info "  Built: batcher (on-demand invocation only)"
    else
        log_error "  Failed to build batcher"
        exit 1
    fi

    log_info "Go builds complete."
    echo ""
fi

# ---------------------------------------------------------------------------
# Step 5: Load environment variables
# ---------------------------------------------------------------------------

log_info "Loading environment variables..."

# Source .env file if it exists (godotenv style).
# The .env file contains all the configuration needed for local dev.
if [ -f "$PROJECT_ROOT/.env" ]; then
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_ROOT/.env"
    set +a
    log_info "  Loaded .env"
elif [ -f "$PROJECT_ROOT/.env.local" ]; then
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_ROOT/.env.local"
    set +a
    log_info "  Loaded .env.local"
else
    log_warn "  No .env file found. Using .env.example defaults."
    set -a
    # shellcheck disable=SC1091
    source "$PROJECT_ROOT/.env.example"
    set +a
    log_info "  Loaded .env.example"
fi

# Ensure APP_ENV is set to local for all services
export APP_ENV=local

echo ""

# ---------------------------------------------------------------------------
# Step 6: Start the API server
# ---------------------------------------------------------------------------

log_info "--- Starting Services ---"
echo ""

start_service "api" "$ARTIFACTS_DIR/api-server"

# Wait for the API to become ready
log_info "Waiting for API server to be ready..."
if wait_for_url "http://localhost:${API_PORT}/v1/health" "api" 30 1; then
    log_info "API server is ready at http://localhost:${API_PORT}"
else
    log_error "API server failed to start. Check $LOG_FILE for details."
    exit 1
fi

echo ""

# ---------------------------------------------------------------------------
# Step 7: Start the Python Eval Worker (dev_runner.py)
# ---------------------------------------------------------------------------

# The eval worker polls SQS queues (eval-queue-urgent, eval-queue-standard)
# and processes evaluation messages locally.
start_service "eval-worker" python3 "$PROJECT_ROOT/worker/eval/dev_runner.py"

echo ""

# ---------------------------------------------------------------------------
# Step 8: Start notification workers (email-worker and webhook-worker)
#
# NOTE: The email-worker and webhook-worker are Lambda functions that in
# production are triggered by SQS. In local mode (APP_ENV=local), they
# read a single SQS event from stdin and exit. For continuous local
# operation, we wrap them in a polling loop that reads from the
# notification-queue via the AWS CLI and pipes messages to the workers.
#
# This is a simplified local-dev polling approach. The eval dev_runner.py
# does its own internal SQS polling. For the Go notification workers,
# we poll externally and pipe events to them.
# ---------------------------------------------------------------------------

# Create the notification worker polling scripts as named pipes (FIFOs)
# that read from the notification-queue and dispatch to the workers.

NOTIF_QUEUE_URL="${SQS_NOTIFICATIONS:-http://localhost:4566/000000000000/notification-queue}"

# notification-poller: polls the notification queue and dispatches to
# email-worker and webhook-worker via their stdin-based local mode.
cat > "$ARTIFACTS_DIR/notification-poller.sh" << 'POLLER_SCRIPT'
#!/usr/bin/env bash
# notification-poller.sh
# Polls the notification SQS queue and dispatches messages to
# email-worker and webhook-worker binaries via stdin (APP_ENV=local).
#
# This script is auto-generated by start-local-stack.sh.

set -uo pipefail

LOCALSTACK_ENDPOINT="${LOCALSTACK_ENDPOINT:-http://localhost:4566}"
QUEUE_URL="$1"
EMAIL_WORKER="$2"
WEBHOOK_WORKER="$3"
POLL_INTERVAL="${POLL_INTERVAL:-2}"
AWS_REGION="${AWS_REGION:-us-east-1}"

log() {
    echo "[notif-poller] $(date '+%H:%M:%S') $*"
}

while true; do
    # Receive up to 5 messages with long polling (5 second wait)
    RESPONSE=$(aws --endpoint-url "$LOCALSTACK_ENDPOINT" \
        --region "$AWS_REGION" \
        --no-cli-pager \
        sqs receive-message \
        --queue-url "$QUEUE_URL" \
        --max-number-of-messages 5 \
        --wait-time-seconds 5 \
        --attribute-names All \
        2>/dev/null || echo '{}')

    # Check if we got any messages
    MESSAGES=$(echo "$RESPONSE" | python3 -c "
import sys, json
try:
    data = json.load(sys.stdin)
    msgs = data.get('Messages', [])
    print(len(msgs))
except:
    print('0')
" 2>/dev/null || echo "0")

    if [ "$MESSAGES" = "0" ] || [ -z "$MESSAGES" ]; then
        sleep "$POLL_INTERVAL"
        continue
    fi

    log "Received $MESSAGES message(s) from notification queue"

    # Build an SQS event JSON for the Lambda workers
    SQS_EVENT=$(echo "$RESPONSE" | python3 -c "
import sys, json

data = json.load(sys.stdin)
messages = data.get('Messages', [])

records = []
for msg in messages:
    record = {
        'messageId': msg['MessageId'],
        'receiptHandle': msg['ReceiptHandle'],
        'body': msg['Body'],
        'attributes': msg.get('Attributes', {}),
        'messageAttributes': msg.get('MessageAttributes', {}),
        'md5OfBody': msg.get('MD5OfBody', ''),
        'eventSource': 'aws:sqs',
        'eventSourceARN': 'arn:aws:sqs:us-east-1:000000000000:notification-queue',
        'awsRegion': 'us-east-1'
    }
    records.append(record)

print(json.dumps({'Records': records}))
" 2>/dev/null)

    if [ -z "$SQS_EVENT" ]; then
        log "ERROR: Failed to build SQS event"
        sleep "$POLL_INTERVAL"
        continue
    fi

    # Dispatch to email-worker
    log "Dispatching to email-worker..."
    echo "$SQS_EVENT" | APP_ENV=local "$EMAIL_WORKER" 2>&1 || log "WARN: email-worker returned non-zero"

    # Dispatch to webhook-worker
    log "Dispatching to webhook-worker..."
    echo "$SQS_EVENT" | APP_ENV=local "$WEBHOOK_WORKER" 2>&1 || log "WARN: webhook-worker returned non-zero"

    # Delete processed messages from queue.
    # Receipt handles may contain special characters, so we iterate
    # line-by-line to avoid word-splitting issues.
    echo "$RESPONSE" | python3 -c "
import sys, json
data = json.load(sys.stdin)
for msg in data.get('Messages', []):
    print(msg['ReceiptHandle'])
" 2>/dev/null | while IFS= read -r receipt_handle; do
        if [ -n "$receipt_handle" ]; then
            aws --endpoint-url "$LOCALSTACK_ENDPOINT" \
                --region "$AWS_REGION" \
                --no-cli-pager \
                sqs delete-message \
                --queue-url "$QUEUE_URL" \
                --receipt-handle "$receipt_handle" 2>/dev/null || true
        fi
    done

    log "Batch processing complete"
done
POLLER_SCRIPT

chmod +x "$ARTIFACTS_DIR/notification-poller.sh"

start_service "notif-poller" bash "$ARTIFACTS_DIR/notification-poller.sh" \
    "$NOTIF_QUEUE_URL" \
    "$ARTIFACTS_DIR/email-worker" \
    "$ARTIFACTS_DIR/webhook-worker"

echo ""

# ---------------------------------------------------------------------------
# Step 9: Summary and wait
# ---------------------------------------------------------------------------

echo "============================================================"
echo " WatchPoint Local Stack: RUNNING"
echo "============================================================"
echo ""
echo "  Services:"
for i in "${!PIDS[@]}"; do
    echo "    ${SERVICE_NAMES[$i]}: PID ${PIDS[$i]}"
done
echo ""
echo "  API:            http://localhost:${API_PORT}"
echo "  Health:         http://localhost:${API_PORT}/v1/health"
echo "  Log File:       $LOG_FILE"
echo ""
echo "  Batcher (on-demand):"
echo "    echo '{...}' | APP_ENV=local $ARTIFACTS_DIR/batcher"
echo ""
echo "  Press Ctrl+C to stop all services."
echo "============================================================"
echo ""

# Append startup summary to log
{
    echo ""
    echo "# Services started:"
    for i in "${!PIDS[@]}"; do
        echo "#   ${SERVICE_NAMES[$i]}: PID ${PIDS[$i]}"
    done
    echo ""
} >> "$LOG_FILE"

# Wait for any background process to exit (or until interrupted)
# We use 'wait -n' if available (bash 4.3+), otherwise poll.
bash_major="${BASH_VERSINFO[0]}"
bash_minor="${BASH_VERSINFO[1]}"
has_wait_n=false
if [ "$bash_major" -gt 4 ]; then
    has_wait_n=true
elif [ "$bash_major" -eq 4 ] && [ "$bash_minor" -ge 3 ]; then
    has_wait_n=true
fi

if [ "$has_wait_n" = true ]; then
    # Modern bash: wait for any child to exit
    while true; do
        wait -n 2>/dev/null || true

        # Check if any service died unexpectedly
        for i in "${!PIDS[@]}"; do
            if ! kill -0 "${PIDS[$i]}" 2>/dev/null; then
                log_warn "${SERVICE_NAMES[$i]} (PID ${PIDS[$i]}) exited unexpectedly."
            fi
        done

        # If all processes have exited, stop waiting
        all_dead=true
        for pid in "${PIDS[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                all_dead=false
                break
            fi
        done
        if [ "$all_dead" = true ]; then
            log_warn "All services have exited."
            break
        fi
    done
else
    # Older bash: poll periodically
    while true; do
        sleep 5
        all_dead=true
        for pid in "${PIDS[@]}"; do
            if kill -0 "$pid" 2>/dev/null; then
                all_dead=false
                break
            fi
        done
        if [ "$all_dead" = true ]; then
            log_warn "All services have exited."
            break
        fi
    done
fi
