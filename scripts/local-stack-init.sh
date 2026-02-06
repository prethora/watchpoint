#!/bin/sh
# local-stack-init.sh
#
# Infrastructure setup script for the WatchPoint local development environment.
# Runs inside the 'setup' container (amazon/aws-cli) via docker-compose.
#
# Creates:
#   1. S3 buckets on MinIO (watchpoint-forecasts, watchpoint-archive)
#   2. SQS queues on LocalStack (eval-queue-urgent, eval-queue-standard,
#      notification-queue, dead-letter-queue-shared)
#   3. SSM Parameters on LocalStack with dummy values for local development
#
# Endpoints:
#   MinIO:      http://minio:9000
#   LocalStack: http://localstack:4566

set -e

LOCALSTACK_ENDPOINT="http://localstack:4566"
MINIO_ENDPOINT="http://minio:9000"
REGION="us-east-1"
ENV="local"

# ---------------------------------------------------------------------------
# Helper: retry a command up to N times with a delay between attempts.
# Progress messages are written to stderr so they do not contaminate stdout
# when used inside $() command substitution.
# Usage: retry <max_attempts> <delay_seconds> <command...>
# ---------------------------------------------------------------------------
retry() {
    max_attempts=$1
    delay=$2
    shift 2
    attempt=1
    while [ "$attempt" -le "$max_attempts" ]; do
        if "$@"; then
            return 0
        fi
        echo "  Attempt $attempt/$max_attempts failed. Retrying in ${delay}s..." >&2
        sleep "$delay"
        attempt=$((attempt + 1))
    done
    echo "  ERROR: Command failed after $max_attempts attempts: $*" >&2
    return 1
}

# ---------------------------------------------------------------------------
# Helper: create or verify an S3 bucket on MinIO.
# Handles the case where the bucket already exists (idempotent).
# ---------------------------------------------------------------------------
create_bucket() {
    bucket=$1
    echo "Creating bucket: $bucket"
    if aws --endpoint-url "$MINIO_ENDPOINT" s3 mb "s3://$bucket" --region "$REGION" 2>/dev/null; then
        echo "  OK: $bucket created."
    else
        # Bucket may already exist from a previous run
        if aws --endpoint-url "$MINIO_ENDPOINT" s3 ls "s3://$bucket" --region "$REGION" 2>/dev/null; then
            echo "  OK: $bucket already exists."
        else
            echo "  FAIL: Could not create $bucket." >&2
            return 1
        fi
    fi
}

# ---------------------------------------------------------------------------
# Helper: create an SQS queue and return its URL via stdout.
# ---------------------------------------------------------------------------
create_queue() {
    queue_name=$1
    attributes=$2
    url=$(aws --endpoint-url "$LOCALSTACK_ENDPOINT" sqs create-queue \
        --queue-name "$queue_name" \
        --attributes "$attributes" \
        --region "$REGION" \
        --query 'QueueUrl' \
        --output text)
    if [ -z "$url" ]; then
        echo "  FAIL: Could not create queue $queue_name." >&2
        return 1
    fi
    echo "$url"
}

# ---------------------------------------------------------------------------
# Helper: create an SSM parameter (idempotent via --overwrite).
# Checks return code explicitly rather than relying on set -e in functions.
# ---------------------------------------------------------------------------
put_param() {
    param_name=$1
    param_value=$2
    param_type=${3:-SecureString}

    if ! aws --endpoint-url "$LOCALSTACK_ENDPOINT" ssm put-parameter \
        --name "$param_name" \
        --value "$param_value" \
        --type "$param_type" \
        --overwrite \
        --region "$REGION" \
        > /dev/null 2>&1; then
        echo "  FAIL: Could not set SSM parameter $param_name" >&2
        return 1
    fi

    echo "  OK: $param_name ($param_type)"
}

echo "============================================"
echo " WatchPoint Local Infrastructure Setup"
echo "============================================"
echo ""

# ===========================================================================
# 1. Create S3 Buckets on MinIO
# ===========================================================================
echo "--- [1/3] Creating S3 buckets on MinIO ---"
echo ""

# MinIO is S3-compatible but requires explicit endpoint override.
retry 5 3 create_bucket watchpoint-forecasts
retry 5 3 create_bucket watchpoint-archive

echo ""
echo "--- S3 buckets ready. ---"
echo ""

# ===========================================================================
# 2. Create SQS Queues on LocalStack
# ===========================================================================
echo "--- [2/3] Creating SQS queues on LocalStack ---"
echo ""

# 2a. Create shared Dead Letter Queue first (other queues reference it)
echo "Creating queue: dead-letter-queue-shared"
DLQ_URL=$(retry 5 3 create_queue dead-letter-queue-shared \
    '{"MessageRetentionPeriod":"1209600"}')
echo "  OK: $DLQ_URL"

# Get the DLQ ARN for redrive policies
DLQ_ARN=$(aws --endpoint-url "$LOCALSTACK_ENDPOINT" sqs get-queue-attributes \
    --queue-url "$DLQ_URL" \
    --attribute-names QueueArn \
    --region "$REGION" \
    --query 'Attributes.QueueArn' \
    --output text)
echo "  DLQ ARN: $DLQ_ARN"
echo ""

# 2b. Build the redrive policy JSON (shared by all processing queues)
REDRIVE_POLICY="{\\\"deadLetterTargetArn\\\":\\\"${DLQ_ARN}\\\",\\\"maxReceiveCount\\\":\\\"3\\\"}"

# EvalQueueUrgent: VisibilityTimeout=360s (6 min), maxReceiveCount=3
echo "Creating queue: eval-queue-urgent"
EVAL_URGENT_URL=$(create_queue eval-queue-urgent \
    "{\"VisibilityTimeout\":\"360\",\"RedrivePolicy\":\"${REDRIVE_POLICY}\"}")
echo "  OK: $EVAL_URGENT_URL"

# EvalQueueStandard: VisibilityTimeout=360s (6 min), maxReceiveCount=3
echo "Creating queue: eval-queue-standard"
EVAL_STANDARD_URL=$(create_queue eval-queue-standard \
    "{\"VisibilityTimeout\":\"360\",\"RedrivePolicy\":\"${REDRIVE_POLICY}\"}")
echo "  OK: $EVAL_STANDARD_URL"

# NotificationQueue: VisibilityTimeout=60s (1 min), maxReceiveCount=3
echo "Creating queue: notification-queue"
NOTIFICATION_URL=$(create_queue notification-queue \
    "{\"VisibilityTimeout\":\"60\",\"RedrivePolicy\":\"${REDRIVE_POLICY}\"}")
echo "  OK: $NOTIFICATION_URL"

echo ""
echo "--- SQS queues ready. ---"
echo ""

# ===========================================================================
# 3. Populate SSM Parameters on LocalStack with dummy/local values
#
# These match the SSM parameter paths defined in:
#   - 04-sam-template.md (Section 1, Global Environment Variables)
#   - 03-config.md (Section 3.2, Secret Pointers)
# ===========================================================================
echo "--- [3/3] Creating SSM parameters on LocalStack ---"
echo ""

# Core Infrastructure
put_param "/${ENV}/watchpoint/database/url" \
    "postgres://postgres:localdev@postgres:5432/watchpoint?sslmode=disable" \
    "SecureString"

# Billing (Stripe) - dummy values for local development
put_param "/${ENV}/watchpoint/billing/stripe_secret_key" \
    "sk_test_local_dummy_stripe_secret_key" \
    "SecureString"

put_param "/${ENV}/watchpoint/billing/stripe_webhook_secret" \
    "whsec_local_dummy_stripe_webhook_secret" \
    "SecureString"

put_param "/${ENV}/watchpoint/billing/stripe_publishable_key" \
    "pk_test_local_dummy_stripe_publishable_key" \
    "String"

# Email (SendGrid)
put_param "/${ENV}/watchpoint/email/sendgrid_api_key" \
    "SG.local_dummy_sendgrid_api_key" \
    "SecureString"

# Email Templates JSON (minimal valid mapping for local dev)
put_param "/${ENV}/watchpoint/email/templates_json" \
    '{"default":{"threshold_crossed":"d-local-template-001","alert_resolved":"d-local-template-002"}}' \
    "String"

# Forecast (RunPod)
put_param "/${ENV}/watchpoint/forecast/runpod_api_key" \
    "rp_local_dummy_runpod_api_key" \
    "SecureString"

# Auth & Security
# Session key must be at least 32 characters (validate:"required,min=32")
put_param "/${ENV}/watchpoint/auth/session_key" \
    "local-dev-session-key-minimum-32-chars-long-for-validation" \
    "SecureString"

put_param "/${ENV}/watchpoint/security/admin_api_key" \
    "local-dev-admin-api-key-for-testing" \
    "SecureString"

put_param "/${ENV}/watchpoint/auth/google_secret" \
    "GOCSPX-local-dummy-google-secret" \
    "SecureString"

put_param "/${ENV}/watchpoint/auth/github_secret" \
    "ghp_local_dummy_github_secret_value" \
    "SecureString"

# Feature Flags (enabled by default for local development)
# Referenced in 12-operations.md Section 7.3 and 03-config.md FeatureConfig
put_param "/${ENV}/watchpoint/features/enable_nowcast" "true" "String"
put_param "/${ENV}/watchpoint/features/enable_email" "true" "String"

echo ""
echo "--- SSM parameters ready. ---"
echo ""

# ===========================================================================
# Summary
# ===========================================================================
echo "============================================"
echo " WatchPoint Local Infrastructure: READY"
echo "============================================"
echo ""
echo " S3 Buckets (MinIO @ $MINIO_ENDPOINT):"
echo "   - watchpoint-forecasts"
echo "   - watchpoint-archive"
echo ""
echo " SQS Queues (LocalStack @ $LOCALSTACK_ENDPOINT):"
echo "   - eval-queue-urgent       (vis. timeout: 360s)"
echo "   - eval-queue-standard     (vis. timeout: 360s)"
echo "   - notification-queue      (vis. timeout: 60s)"
echo "   - dead-letter-queue-shared (retention: 14 days)"
echo ""
echo " SSM Parameters (LocalStack @ $LOCALSTACK_ENDPOINT):"
echo "   - /${ENV}/watchpoint/database/url"
echo "   - /${ENV}/watchpoint/billing/stripe_secret_key"
echo "   - /${ENV}/watchpoint/billing/stripe_webhook_secret"
echo "   - /${ENV}/watchpoint/billing/stripe_publishable_key"
echo "   - /${ENV}/watchpoint/email/sendgrid_api_key"
echo "   - /${ENV}/watchpoint/email/templates_json"
echo "   - /${ENV}/watchpoint/forecast/runpod_api_key"
echo "   - /${ENV}/watchpoint/auth/session_key"
echo "   - /${ENV}/watchpoint/security/admin_api_key"
echo "   - /${ENV}/watchpoint/auth/google_secret"
echo "   - /${ENV}/watchpoint/auth/github_secret"
echo "   - /${ENV}/watchpoint/features/enable_nowcast"
echo "   - /${ENV}/watchpoint/features/enable_email"
echo ""
echo "============================================"
