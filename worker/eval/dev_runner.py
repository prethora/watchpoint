#!/usr/bin/env python3
"""
dev_runner.py -- Local development harness for the Eval Worker.

Since the Eval Worker is designed as a Lambda handler triggered by SQS, it
cannot run standalone. This script simulates the AWS Lambda runtime locally
by:

1. Initializing the ``EvalWorker`` with connections to local services
   (LocalStack SQS, MinIO S3, PostgreSQL).
2. Polling two SQS queues (``eval-queue-urgent`` and ``eval-queue-standard``)
   in a continuous loop.
3. Wrapping received messages into a mock Lambda event structure
   (``{'Records': [...]}``) matching the SQS-Lambda integration contract.
4. Invoking ``handler.handler(event, context)`` with a mock Lambda context.
5. Handling ACKs/NACKs based on the handler's ``batchItemFailures`` response:
   - Messages NOT in ``batchItemFailures`` are deleted (ACKed).
   - Messages IN ``batchItemFailures`` are left for retry (NACKed).

This simulates the AWS Lambda SQS event source mapping behavior where only
failed messages are retried.

Environment Variables (with defaults for docker-compose local dev):
    APP_ENV                 - Must be "local" (default: "local")
    DATABASE_URL            - PostgreSQL DSN (default: local docker-compose)
    FORECAST_BUCKET         - S3 bucket name (default: "watchpoint-forecasts")
    QUEUE_URL               - Notification queue URL (default: LocalStack)
    AWS_REGION              - AWS region (default: "us-east-1")
    LOCALSTACK_ENDPOINT     - LocalStack URL (default: "http://localhost:4566")
    MINIO_ENDPOINT          - MinIO URL (default: "http://localhost:9000")
    POLL_INTERVAL_SECONDS   - Seconds between poll cycles (default: 2)
    SQS_MAX_MESSAGES        - Max messages per ReceiveMessage (default: 5)
    SQS_WAIT_TIME_SECONDS   - Long-poll wait time in seconds (default: 5)

Usage:
    # From project root with docker-compose services running:
    python worker/eval/dev_runner.py

    # With custom settings:
    LOCALSTACK_ENDPOINT=http://localhost:4566 \\
    DATABASE_URL=postgres://postgres:localdev@localhost:5432/watchpoint \\
    python worker/eval/dev_runner.py

Architecture References:
    - 07-eval-worker.md Section 6, 7 (Lambda Handler, Configuration)
    - flow-simulations.md EVAL-001 (Evaluation Pipeline)
"""

from __future__ import annotations

import logging
import os
import signal
import sys
import time
from pathlib import Path
from typing import Any

# ---------------------------------------------------------------------------
# Ensure project root is on sys.path so we can import worker modules.
# ---------------------------------------------------------------------------
PROJECT_ROOT = Path(__file__).resolve().parent.parent.parent
sys.path.insert(0, str(PROJECT_ROOT))

import boto3  # noqa: E402

from worker.eval.handler import EvalWorker  # noqa: E402
from worker.eval.reader import create_forecast_reader  # noqa: E402
from worker.eval.repo import PostgresRepository  # noqa: E402
from worker.eval.logic.evaluator import StandardEvaluator  # noqa: E402

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(name)s: %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
logger = logging.getLogger("dev_runner")


# ---------------------------------------------------------------------------
# Configuration (with sensible local-dev defaults)
# ---------------------------------------------------------------------------

LOCALSTACK_ENDPOINT = os.environ.get("LOCALSTACK_ENDPOINT", "http://localhost:4566")
MINIO_ENDPOINT = os.environ.get("MINIO_ENDPOINT", "http://localhost:9000")
DATABASE_URL = os.environ.get(
    "DATABASE_URL",
    "postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable",
)
FORECAST_BUCKET = os.environ.get("FORECAST_BUCKET", "watchpoint-forecasts")
NOTIFICATION_QUEUE_URL = os.environ.get(
    "QUEUE_URL",
    f"{LOCALSTACK_ENDPOINT}/000000000000/notification-queue",
)
AWS_REGION = os.environ.get("AWS_REGION", "us-east-1")

# Polling configuration
POLL_INTERVAL_SECONDS = float(os.environ.get("POLL_INTERVAL_SECONDS", "2"))
SQS_MAX_MESSAGES = int(os.environ.get("SQS_MAX_MESSAGES", "5"))
SQS_WAIT_TIME_SECONDS = int(os.environ.get("SQS_WAIT_TIME_SECONDS", "5"))

# SQS queue names (must match local-stack-init.sh)
EVAL_QUEUE_URGENT = "eval-queue-urgent"
EVAL_QUEUE_STANDARD = "eval-queue-standard"


# ---------------------------------------------------------------------------
# Mock Lambda Context
# ---------------------------------------------------------------------------


class MockLambdaContext:
    """Simulates the AWS Lambda context object for local execution.

    Provides ``get_remaining_time_in_millis()`` which the ``TimeoutGuard``
    uses to check whether the Lambda timeout is imminent. In local dev,
    we simulate a generous 5-minute timeout (300,000 ms) that decreases
    realistically based on elapsed wall-clock time. This is long enough
    to avoid timeout guard triggers during normal processing, but still
    allows testing timeout behavior by passing a short ``timeout_ms``.

    Attributes:
        function_name: Simulated Lambda function name.
        function_version: Simulated function version.
        invoked_function_arn: Simulated ARN.
        memory_limit_in_mb: Simulated memory limit.
        aws_request_id: Simulated request ID.
        log_group_name: Simulated log group.
        log_stream_name: Simulated log stream.
    """

    def __init__(self, timeout_ms: int = 300_000) -> None:
        self._timeout_ms = timeout_ms
        self._start_time = time.monotonic()

        self.function_name = "eval-worker-local"
        self.function_version = "$LATEST"
        self.invoked_function_arn = (
            "arn:aws:lambda:us-east-1:000000000000:function:eval-worker-local"
        )
        self.memory_limit_in_mb = 1024
        self.aws_request_id = "local-dev-request-id"
        self.log_group_name = "/aws/lambda/eval-worker-local"
        self.log_stream_name = "local-dev-stream"

    def get_remaining_time_in_millis(self) -> int:
        """Return simulated remaining execution time.

        Computes remaining time based on elapsed time since context creation.
        This allows the TimeoutGuard to function realistically during local
        testing if desired (e.g., for testing timeout behavior with a short
        timeout_ms value).
        """
        elapsed_ms = int((time.monotonic() - self._start_time) * 1000)
        remaining = max(0, self._timeout_ms - elapsed_ms)
        return remaining


# ---------------------------------------------------------------------------
# SQS Helpers
# ---------------------------------------------------------------------------


def get_queue_url(sqs_client: Any, queue_name: str) -> str:
    """Resolve a queue name to its URL via the SQS API.

    Parameters
    ----------
    sqs_client : boto3 SQS client
        Pre-configured SQS client pointing at LocalStack.
    queue_name : str
        The SQS queue name (e.g., "eval-queue-urgent").

    Returns
    -------
    str
        The queue URL.

    Raises
    ------
    Exception
        If the queue does not exist (likely means local-stack-init.sh
        has not been run).
    """
    response = sqs_client.get_queue_url(QueueName=queue_name)
    return response["QueueUrl"]


def poll_queue(
    sqs_client: Any,
    queue_url: str,
    max_messages: int = SQS_MAX_MESSAGES,
    wait_time_seconds: int = SQS_WAIT_TIME_SECONDS,
) -> list[dict[str, Any]]:
    """Poll an SQS queue for messages using long polling.

    Parameters
    ----------
    sqs_client : boto3 SQS client
        Pre-configured SQS client.
    queue_url : str
        The queue URL to poll.
    max_messages : int
        Maximum number of messages to receive (1-10).
    wait_time_seconds : int
        Long-poll wait time in seconds (0-20).

    Returns
    -------
    list[dict]
        List of SQS message dicts, each with 'MessageId', 'ReceiptHandle',
        'Body', etc.
    """
    response = sqs_client.receive_message(
        QueueUrl=queue_url,
        MaxNumberOfMessages=max_messages,
        WaitTimeSeconds=wait_time_seconds,
        AttributeNames=["All"],
        MessageAttributeNames=["All"],
    )
    return response.get("Messages", [])


def build_lambda_event(sqs_messages: list[dict[str, Any]]) -> dict[str, Any]:
    """Convert raw SQS messages into a Lambda SQS event structure.

    The Lambda SQS event source mapping wraps SQS messages in a specific
    format with a ``Records`` array. Each record has fields like
    ``messageId``, ``receiptHandle``, ``body``, ``attributes``, etc.

    This function faithfully reproduces that structure so the handler
    receives exactly the same input as it would in production.

    Parameters
    ----------
    sqs_messages : list[dict]
        Raw SQS messages from ``ReceiveMessage``.

    Returns
    -------
    dict
        Lambda event with ``Records`` array matching the SQS-Lambda
        integration contract.
    """
    records = []
    for msg in sqs_messages:
        # Extract queue name from QueueArn attribute if available,
        # otherwise fall back to "unknown".
        queue_arn = msg.get("Attributes", {}).get("QueueArn", "")
        if queue_arn:
            event_source_arn = queue_arn
        else:
            event_source_arn = (
                f"arn:aws:sqs:{AWS_REGION}:000000000000:unknown"
            )

        record = {
            "messageId": msg["MessageId"],
            "receiptHandle": msg["ReceiptHandle"],
            "body": msg["Body"],
            "attributes": msg.get("Attributes", {}),
            "messageAttributes": msg.get("MessageAttributes", {}),
            "md5OfBody": msg.get("MD5OfBody", ""),
            "eventSource": "aws:sqs",
            "eventSourceARN": event_source_arn,
            "awsRegion": AWS_REGION,
        }
        records.append(record)

    return {"Records": records}


def handle_batch_response(
    sqs_client: Any,
    queue_url: str,
    sqs_messages: list[dict[str, Any]],
    response: dict[str, Any],
) -> tuple[int, int]:
    """Process the handler's batch response, ACKing/NACKing as appropriate.

    In the real Lambda SQS integration, messages NOT listed in
    ``batchItemFailures`` are automatically deleted from the queue (ACKed).
    Messages IN ``batchItemFailures`` have their visibility timeout reset
    so they can be retried (NACKed).

    This function simulates that behavior by:
    - Deleting successfully processed messages from the queue.
    - Leaving failed messages for retry (they will become visible again
      after the queue's VisibilityTimeout expires).

    Parameters
    ----------
    sqs_client : boto3 SQS client
        Pre-configured SQS client.
    queue_url : str
        The queue URL the messages came from.
    sqs_messages : list[dict]
        The original SQS messages (needed for ReceiptHandle).
    response : dict
        The handler's return value with ``batchItemFailures``.

    Returns
    -------
    tuple[int, int]
        (ack_count, nack_count) -- number of messages ACKed and NACKed.
    """
    # Build a set of failed message IDs from the handler response
    failed_ids: set[str] = set()
    for failure in response.get("batchItemFailures", []):
        failed_ids.add(failure["itemIdentifier"])

    # Build a mapping of messageId -> ReceiptHandle for deletion
    receipt_map: dict[str, str] = {
        msg["MessageId"]: msg["ReceiptHandle"] for msg in sqs_messages
    }

    ack_count = 0
    nack_count = 0

    for message_id, receipt_handle in receipt_map.items():
        if message_id in failed_ids:
            # NACK: leave the message for retry
            logger.debug(
                "NACK: messageId=%s (will be retried after visibility timeout)",
                message_id,
            )
            nack_count += 1
        else:
            # ACK: delete the message from the queue
            try:
                sqs_client.delete_message(
                    QueueUrl=queue_url,
                    ReceiptHandle=receipt_handle,
                )
                logger.debug("ACK: messageId=%s (deleted from queue)", message_id)
                ack_count += 1
            except Exception:
                logger.exception(
                    "Failed to delete (ACK) messageId=%s from queue",
                    message_id,
                )
                nack_count += 1

    return ack_count, nack_count


# ---------------------------------------------------------------------------
# Worker Initialization
# ---------------------------------------------------------------------------


def create_local_worker() -> EvalWorker:
    """Create an EvalWorker configured for local development.

    Connects to:
    - PostgreSQL on localhost:5432 (docker-compose)
    - MinIO on localhost:9000 (docker-compose)
    - LocalStack SQS on localhost:4566 (docker-compose)

    Returns
    -------
    EvalWorker
        Fully configured worker instance.
    """
    logger.info("Initializing EvalWorker for local development...")

    # SQS client for notification publishing (pointed at LocalStack)
    sqs_client = boto3.client(
        "sqs",
        region_name=AWS_REGION,
        endpoint_url=LOCALSTACK_ENDPOINT,
        aws_access_key_id="test",
        aws_secret_access_key="test",
    )

    # Repository (PostgreSQL)
    repo = PostgresRepository(
        conninfo=DATABASE_URL,
        sqs_client=sqs_client,
        queue_url=NOTIFICATION_QUEUE_URL,
    )
    logger.info("  Repository: PostgreSQL @ %s", DATABASE_URL.split("@")[-1])

    # Forecast Reader (MinIO via s3fs)
    reader = create_forecast_reader(
        bucket=FORECAST_BUCKET,
        aws_region=AWS_REGION,
        endpoint_url=MINIO_ENDPOINT,
        aws_access_key_id="minioadmin",
        aws_secret_access_key="minioadmin",
    )
    logger.info("  Reader: MinIO @ %s, bucket=%s", MINIO_ENDPOINT, FORECAST_BUCKET)

    # Evaluator
    evaluator = StandardEvaluator(enable_threat_dedup=True)
    logger.info("  Evaluator: StandardEvaluator (threat_dedup=True)")

    # Metric emitter (log-only in local dev)
    def local_metric_emitter(
        name: str, value: float, unit: str, dimensions: dict[str, str]
    ) -> None:
        logger.info(
            "METRIC: %s=%.3f %s dimensions=%s", name, value, unit, dimensions
        )

    worker = EvalWorker(
        repo=repo,
        reader=reader,
        evaluator=evaluator,
        metric_emitter=local_metric_emitter,
    )

    logger.info("  EvalWorker initialized successfully.")
    return worker


# ---------------------------------------------------------------------------
# Main Loop
# ---------------------------------------------------------------------------

# Global flag for graceful shutdown
_shutdown_requested = False


def _signal_handler(signum: int, frame: Any) -> None:
    """Handle SIGINT/SIGTERM for graceful shutdown."""
    global _shutdown_requested
    sig_name = signal.Signals(signum).name
    logger.info("Received %s. Requesting graceful shutdown...", sig_name)
    _shutdown_requested = True


def main() -> None:
    """Main entry point: initialize worker, poll queues, process messages."""
    global _shutdown_requested

    # Register signal handlers for graceful shutdown
    signal.signal(signal.SIGINT, _signal_handler)
    signal.signal(signal.SIGTERM, _signal_handler)

    # Ensure APP_ENV is set to local
    os.environ.setdefault("APP_ENV", "local")

    print("=" * 60)
    print(" WatchPoint Eval Worker - Local Development Harness")
    print("=" * 60)
    print()
    print(f"  LocalStack:  {LOCALSTACK_ENDPOINT}")
    print(f"  MinIO:       {MINIO_ENDPOINT}")
    print(f"  PostgreSQL:  {DATABASE_URL.split('@')[-1]}")
    print(f"  Bucket:      {FORECAST_BUCKET}")
    print(f"  Notification Queue: {NOTIFICATION_QUEUE_URL}")
    print(f"  Poll Interval: {POLL_INTERVAL_SECONDS}s")
    print(f"  SQS Max Messages: {SQS_MAX_MESSAGES}")
    print(f"  SQS Wait Time: {SQS_WAIT_TIME_SECONDS}s")
    print()

    # Create the worker
    worker = create_local_worker()

    # Create SQS client for polling (pointed at LocalStack)
    sqs_client = boto3.client(
        "sqs",
        region_name=AWS_REGION,
        endpoint_url=LOCALSTACK_ENDPOINT,
        aws_access_key_id="test",
        aws_secret_access_key="test",
    )

    # Resolve queue URLs
    try:
        urgent_url = get_queue_url(sqs_client, EVAL_QUEUE_URGENT)
        standard_url = get_queue_url(sqs_client, EVAL_QUEUE_STANDARD)
    except Exception as exc:
        logger.error(
            "Failed to resolve SQS queue URLs. "
            "Is LocalStack running and local-stack-init.sh completed? "
            "Error: %s",
            str(exc),
        )
        sys.exit(1)

    logger.info("Resolved queue URLs:")
    logger.info("  Urgent:   %s", urgent_url)
    logger.info("  Standard: %s", standard_url)

    # Queues to poll, in priority order (urgent first)
    queues = [
        ("urgent", urgent_url),
        ("standard", standard_url),
    ]

    print()
    print("=" * 60)
    print(" Polling for messages... (Ctrl+C to stop)")
    print("=" * 60)
    print()

    total_processed = 0
    total_acked = 0
    total_nacked = 0

    while not _shutdown_requested:
        messages_found = False

        for queue_label, queue_url in queues:
            if _shutdown_requested:
                break

            try:
                sqs_messages = poll_queue(
                    sqs_client,
                    queue_url,
                    max_messages=SQS_MAX_MESSAGES,
                    wait_time_seconds=SQS_WAIT_TIME_SECONDS,
                )
            except Exception:
                logger.exception(
                    "Error polling %s queue (%s)", queue_label, queue_url
                )
                continue

            if not sqs_messages:
                continue

            messages_found = True
            msg_count = len(sqs_messages)
            logger.info(
                "Received %d message(s) from %s queue",
                msg_count,
                queue_label,
            )

            # Build the Lambda event
            event = build_lambda_event(sqs_messages)

            # Create a fresh mock context for each invocation
            context = MockLambdaContext(timeout_ms=300_000)

            # Invoke the handler
            try:
                response = worker.handler(event, context)
            except Exception:
                logger.exception(
                    "Unhandled exception from worker.handler() "
                    "(all %d messages will be retried)",
                    msg_count,
                )
                # If the handler itself raises (should not happen per design),
                # treat all messages as failed (they stay in the queue).
                total_nacked += msg_count
                total_processed += msg_count
                continue

            # Handle ACK/NACK based on batchItemFailures
            ack_count, nack_count = handle_batch_response(
                sqs_client, queue_url, sqs_messages, response
            )

            total_acked += ack_count
            total_nacked += nack_count
            total_processed += msg_count

            logger.info(
                "Batch complete: %d ACKed, %d NACKed (cumulative: "
                "%d processed, %d ACKed, %d NACKed)",
                ack_count,
                nack_count,
                total_processed,
                total_acked,
                total_nacked,
            )

        # If no messages were found on any queue, sleep before next cycle
        if not messages_found and not _shutdown_requested:
            time.sleep(POLL_INTERVAL_SECONDS)

    # Graceful shutdown
    print()
    print("=" * 60)
    print(" Shutting down.")
    print(f"  Total processed: {total_processed}")
    print(f"  Total ACKed:     {total_acked}")
    print(f"  Total NACKed:    {total_nacked}")
    print("=" * 60)


if __name__ == "__main__":
    main()
