#!/usr/bin/env bash
# verify-infra.sh
#
# Verification script for the WatchPoint local development infrastructure.
# Checks that all docker-compose services are healthy and properly configured:
#   1. Docker Compose service health
#   2. Postgres: connection and table count (expects >10 tables after migrations)
#   3. LocalStack SQS: queue existence
#   4. MinIO: health endpoint
#
# Usage:
#   ./scripts/verify-infra.sh
#
# Prerequisites:
#   - Docker and docker compose must be running
#   - docker-compose.yml services must be started: docker compose up -d
#   - Migrations must have been applied to Postgres
#
# Exit codes:
#   0 - All checks passed
#   1 - One or more checks failed

set -uo pipefail
# Note: We intentionally do NOT use 'set -e' here. The script accumulates
# pass/fail counts and must continue executing after individual check failures
# to produce a complete verification report.

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
# POSTGRES_HOST and POSTGRES_PORT are provided for documentation/overriding,
# but the default verification uses docker exec (no local psql needed).
POSTGRES_HOST="${POSTGRES_HOST:-localhost}"
POSTGRES_PORT="${POSTGRES_PORT:-5432}"
POSTGRES_DB="${POSTGRES_DB:-watchpoint}"
POSTGRES_USER="${POSTGRES_USER:-postgres}"
POSTGRES_PASSWORD="${POSTGRES_PASSWORD:-localdev}"

LOCALSTACK_ENDPOINT="${LOCALSTACK_ENDPOINT:-http://localhost:4566}"
MINIO_ENDPOINT="${MINIO_ENDPOINT:-http://localhost:9000}"

AWS_REGION="${AWS_REGION:-us-east-1}"

# Expected SQS queues (created by local-stack-init.sh)
EXPECTED_QUEUES=(
    "eval-queue-urgent"
    "eval-queue-standard"
    "notification-queue"
    "dead-letter-queue-shared"
)

# Minimum expected table count after all migrations have run
MIN_TABLE_COUNT=10

# ---------------------------------------------------------------------------
# State tracking
# ---------------------------------------------------------------------------
PASS_COUNT=0
FAIL_COUNT=0

pass() {
    echo "  [PASS] $1"
    PASS_COUNT=$((PASS_COUNT + 1))
}

fail() {
    echo "  [FAIL] $1"
    FAIL_COUNT=$((FAIL_COUNT + 1))
}

# ---------------------------------------------------------------------------
# 1. Docker Compose Service Health
# ---------------------------------------------------------------------------
echo "============================================"
echo " WatchPoint Infrastructure Verification"
echo "============================================"
echo ""
echo "--- [1/4] Docker Compose Service Health ---"
echo ""

check_docker_service() {
    local service_name="$1"
    # docker compose ps returns status info for the named service.
    # We look for "(healthy)" in the output.
    local status
    if ! status=$(docker compose ps "$service_name" 2>&1); then
        fail "$service_name: docker compose ps failed"
        return
    fi

    if echo "$status" | grep -q "(healthy)"; then
        pass "$service_name: healthy"
    elif echo "$status" | grep -q "Up"; then
        # Running but no healthcheck or not yet healthy
        fail "$service_name: running but not healthy (check healthcheck status)"
    else
        fail "$service_name: not running or not found"
    fi
}

check_docker_service "postgres"
check_docker_service "localstack"
check_docker_service "minio"

echo ""

# ---------------------------------------------------------------------------
# 2. Postgres: Connection and Table Count
# ---------------------------------------------------------------------------
echo "--- [2/4] Postgres Database ---"
echo ""

# Check if we can connect and query the database.
# Uses docker exec to run psql inside the postgres container, avoiding the need
# for a local psql installation.
POSTGRES_CONTAINER_ID=$(docker compose ps -q postgres 2>/dev/null || true)

if [ -z "$POSTGRES_CONTAINER_ID" ]; then
    fail "Postgres container not found (is docker compose up?)"
    fail "Table count: unable to check (no container)"
elif docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
    "$POSTGRES_CONTAINER_ID" \
    psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc "SELECT 1" \
    > /dev/null 2>&1; then
    pass "Postgres connection successful"

    # Count user-created tables (exclude system tables)
    TABLE_COUNT=$(docker exec -e PGPASSWORD="$POSTGRES_PASSWORD" \
        "$POSTGRES_CONTAINER_ID" \
        psql -h 127.0.0.1 -U "$POSTGRES_USER" -d "$POSTGRES_DB" -tAc \
        "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema = 'public' AND table_type = 'BASE TABLE'" 2>/dev/null || echo "0")

    # Trim whitespace
    TABLE_COUNT=$(echo "$TABLE_COUNT" | tr -d '[:space:]')

    if [ -z "$TABLE_COUNT" ] || [ "$TABLE_COUNT" = "0" ]; then
        fail "Table count: ${TABLE_COUNT:-0} (expected >$MIN_TABLE_COUNT) -- have migrations been applied?"
    elif [ "$TABLE_COUNT" -gt "$MIN_TABLE_COUNT" ]; then
        pass "Table count: $TABLE_COUNT (expected >$MIN_TABLE_COUNT)"
    else
        fail "Table count: $TABLE_COUNT (expected >$MIN_TABLE_COUNT) -- have migrations been applied?"
    fi
else
    fail "Postgres connection failed (container exists but psql query failed)"
    fail "Table count: unable to check (connection failed)"
fi

echo ""

# ---------------------------------------------------------------------------
# 3. LocalStack SQS: Queue Existence
# ---------------------------------------------------------------------------
echo "--- [3/4] LocalStack SQS Queues ---"
echo ""

# First check LocalStack health endpoint
if curl -sf "${LOCALSTACK_ENDPOINT}/_localstack/health" > /dev/null 2>&1; then
    pass "LocalStack health endpoint reachable"

    # List all queues
    QUEUE_LIST=""
    if QUEUE_LIST=$(aws --endpoint-url "$LOCALSTACK_ENDPOINT" \
        --region "$AWS_REGION" \
        --no-cli-pager \
        sqs list-queues \
        --query 'QueueUrls' \
        --output text 2>&1); then

        for queue_name in "${EXPECTED_QUEUES[@]}"; do
            if echo "$QUEUE_LIST" | grep -q "$queue_name"; then
                pass "SQS queue exists: $queue_name"
            else
                fail "SQS queue missing: $queue_name"
            fi
        done
    else
        fail "Could not list SQS queues (aws cli error)"
        for queue_name in "${EXPECTED_QUEUES[@]}"; do
            fail "SQS queue unchecked: $queue_name"
        done
    fi
else
    fail "LocalStack health endpoint unreachable at $LOCALSTACK_ENDPOINT"
    for queue_name in "${EXPECTED_QUEUES[@]}"; do
        fail "SQS queue unchecked: $queue_name (LocalStack unreachable)"
    done
fi

echo ""

# ---------------------------------------------------------------------------
# 4. MinIO Health Check
# ---------------------------------------------------------------------------
echo "--- [4/4] MinIO Object Storage ---"
echo ""

if curl -sf "${MINIO_ENDPOINT}/minio/health/live" > /dev/null 2>&1; then
    pass "MinIO health endpoint: live"

    # Check that the expected buckets exist
    for bucket_name in "watchpoint-forecasts" "watchpoint-archive"; do
        if aws --endpoint-url "$MINIO_ENDPOINT" \
            --region "$AWS_REGION" \
            --no-cli-pager \
            s3 ls "s3://$bucket_name" > /dev/null 2>&1; then
            pass "S3 bucket exists: $bucket_name"
        else
            fail "S3 bucket missing: $bucket_name"
        fi
    done
else
    fail "MinIO health endpoint unreachable at $MINIO_ENDPOINT"
    fail "S3 bucket unchecked: watchpoint-forecasts (MinIO unreachable)"
    fail "S3 bucket unchecked: watchpoint-archive (MinIO unreachable)"
fi

echo ""

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------
echo "============================================"
echo " Verification Summary"
echo "============================================"
echo ""
echo "  Passed: $PASS_COUNT"
echo "  Failed: $FAIL_COUNT"
echo ""

if [ "$FAIL_COUNT" -eq 0 ]; then
    echo "Infrastructure Verified"
    echo ""
    exit 0
else
    echo "Infrastructure verification FAILED ($FAIL_COUNT check(s) did not pass)."
    echo "Review the output above for details."
    echo ""
    exit 1
fi
