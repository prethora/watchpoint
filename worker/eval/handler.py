"""
Lambda handler for the Eval Worker.

Implements the ``handler(event, context)`` entrypoint for the SQS-triggered
Lambda function. Processes evaluation messages in batches, grouping by tile_id
to optimize Zarr I/O, and returns partial batch failure responses to enable
selective SQS retry.

Architecture References:
    - 07-eval-worker.md Section 6 (Lambda Handler & Execution)
    - flow-simulations.md EVAL-001 (Evaluation Pipeline)
    - flow-simulations.md EVAL-006 (Queue Backlog Recovery -- manual scaling/purging)
    - flow-simulations.md FAIL-011 (Lambda Function Error / Generic Crash)
    - flow-simulations.md FAIL-008 (Zarr Corruption -> ACK)

Key Design Decisions:
    - **Partial Batch Failure**: Uses the ``batchItemFailures`` response format
      so that only failed messages are retried, not the entire batch.
    - **Tile Grouping**: Messages are grouped by ``tile_id`` so that the Zarr
      chunk is loaded only once per tile within a batch.
    - **Terminal Error ACK**: ``ForecastCorruptError`` and ``ForecastExpiredError``
      are ACKed (not retried) because corrupt/expired data cannot be fixed by retry.
    - **TimeBudgetExceeded**: When the Lambda timeout is imminent, all remaining
      unprocessed messages in the batch are marked as failed for retry.
    - **EvaluationLag Metric**: Emitted for every successfully processed tile to
      track pipeline latency (time from forecast run to evaluation completion).
"""

from __future__ import annotations

import json
import logging
from collections import defaultdict
from datetime import datetime, timezone
from typing import Any

from worker.eval.config import load_settings
from worker.eval.logic.evaluator import (
    StandardEvaluator,
    TimeBudgetExceededError,
)
from worker.eval.models import (
    BatchResult,
    EvalAction,
    EvalMessage,
)
from worker.eval.reader import (
    ForecastCorruptError,
    ForecastExpiredError,
    ForecastReader,
)
from worker.eval.repo import Repository

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Metric Constants
# ---------------------------------------------------------------------------

METRIC_EVALUATION_LAG = "EvaluationLag"


# ---------------------------------------------------------------------------
# SQS Batch Response Models
# ---------------------------------------------------------------------------


class SQSBatchItemFailure:
    """A single failed item in the SQS partial batch response."""

    __slots__ = ("item_identifier",)

    def __init__(self, item_identifier: str) -> None:
        self.item_identifier = item_identifier

    def to_dict(self) -> dict[str, str]:
        return {"itemIdentifier": self.item_identifier}


class SQSBatchResponse:
    """Partial batch failure response for SQS Lambda integration."""

    __slots__ = ("batch_item_failures",)

    def __init__(self) -> None:
        self.batch_item_failures: list[SQSBatchItemFailure] = []

    def add_failure(self, message_id: str) -> None:
        self.batch_item_failures.append(SQSBatchItemFailure(message_id))

    def to_dict(self) -> dict[str, list[dict[str, str]]]:
        return {
            "batchItemFailures": [
                f.to_dict() for f in self.batch_item_failures
            ]
        }


# ---------------------------------------------------------------------------
# Timeout Guard (07-eval-worker.md Section 6.1)
# ---------------------------------------------------------------------------

# Threshold in milliseconds below which we consider timeout imminent.
_TIMEOUT_THRESHOLD_MS = 5000


class TimeoutGuard:
    """Prevents the worker from being killed mid-batch by AWS Lambda.

    Checks the remaining execution time via the Lambda context object.
    Raises ``TimeBudgetExceededError`` when less than 5 seconds remain,
    giving the handler time to return a partial batch failure response
    rather than being hard-killed (which would cause the entire batch
    to be retried).

    Parameters
    ----------
    context : Any
        AWS Lambda context object. Must expose ``get_remaining_time_in_millis()``.
    """

    def __init__(self, context: Any) -> None:
        self._context = context

    def check_remaining(self) -> None:
        """Raise TimeBudgetExceededError if < 5 seconds remain."""
        remaining = self._context.get_remaining_time_in_millis()
        if remaining < _TIMEOUT_THRESHOLD_MS:
            raise TimeBudgetExceededError(
                f"Lambda timeout imminent: {remaining}ms remaining "
                f"(threshold: {_TIMEOUT_THRESHOLD_MS}ms)"
            )


# ---------------------------------------------------------------------------
# Metric Emitter
# ---------------------------------------------------------------------------


def _default_metric_emitter(
    name: str, value: float, unit: str, dimensions: dict[str, str]
) -> None:
    """Default metric emitter that logs metrics when no CloudWatch emitter is configured."""
    logger.info(
        "Metric: %s=%.3f %s dimensions=%s",
        name,
        value,
        unit,
        dimensions,
    )


# ---------------------------------------------------------------------------
# Message Parsing
# ---------------------------------------------------------------------------


def _parse_sqs_records(
    event: dict[str, Any],
) -> list[tuple[str, EvalMessage]]:
    """Parse SQS event records into (message_id, EvalMessage) pairs.

    Parameters
    ----------
    event : dict
        The raw SQS Lambda event containing ``Records``.

    Returns
    -------
    list[tuple[str, EvalMessage]]
        List of (SQS messageId, parsed EvalMessage) tuples.

    Raises
    ------
    ValueError
        If the event has no Records or a record body cannot be parsed.
    """
    records = event.get("Records", [])
    if not records:
        logger.warning("SQS event contains no Records")
        return []

    parsed: list[tuple[str, EvalMessage]] = []
    for record in records:
        message_id = record["messageId"]
        body_str = record["body"]
        try:
            body = json.loads(body_str)
            msg = EvalMessage(**body)
            parsed.append((message_id, msg))
        except Exception:
            logger.exception(
                "Failed to parse SQS record messageId=%s body=%s",
                message_id,
                body_str[:500],  # Truncate for safety
            )
            # Parse failures are terminal -- cannot retry garbage data.
            # Log and ACK by not adding to failures.
            # This prevents poison-pill messages from blocking the queue.
            continue

    return parsed


def _group_by_tile(
    messages: list[tuple[str, EvalMessage]],
) -> dict[str, list[tuple[str, EvalMessage]]]:
    """Group parsed messages by tile_id for optimized tile loading.

    Parameters
    ----------
    messages : list[tuple[str, EvalMessage]]
        Parsed (message_id, EvalMessage) pairs.

    Returns
    -------
    dict[str, list[tuple[str, EvalMessage]]]
        Messages grouped by tile_id. Insertion order is preserved.
    """
    groups: dict[str, list[tuple[str, EvalMessage]]] = defaultdict(list)
    for message_id, msg in messages:
        groups[msg.tile_id].append((message_id, msg))
    return dict(groups)


# ---------------------------------------------------------------------------
# EvalWorker (07-eval-worker.md Section 6.2)
# ---------------------------------------------------------------------------


class EvalWorker:
    """Lambda handler for the Eval Worker.

    Manages the lifecycle of SQS batch processing including:
    - Parsing and grouping messages by tile
    - Loading forecast data (once per tile)
    - Routing to evaluator or summary generator
    - Error handling with appropriate ACK/NACK behavior
    - Emitting telemetry metrics

    Parameters
    ----------
    repo : Repository
        Data access layer for WatchPoints and evaluation state.
    reader : ForecastReader
        Zarr I/O abstraction for loading forecast data.
    evaluator : StandardEvaluator
        Core evaluation logic.
    metric_emitter : callable or None
        Callback for emitting CloudWatch metrics. If None, metrics are
        logged but not sent to CloudWatch.
    """

    def __init__(
        self,
        repo: Repository,
        reader: ForecastReader,
        evaluator: StandardEvaluator,
        metric_emitter: Any | None = None,
    ) -> None:
        self._repo = repo
        self._reader = reader
        self._evaluator = evaluator
        self._metric_emitter = metric_emitter or _default_metric_emitter

    def handler(self, event: dict[str, Any], context: Any) -> dict[str, Any]:
        """Lambda handler entrypoint.

        Processing flow:
        1. Parse SQS batch (extract messages from event).
        2. Group messages by tile_id (optimization: load tile once per group).
        3. For each tile group:
           a. try: process each message in the group via ``process_tile``.
           b. catch TimeBudgetExceeded: Mark all remaining messages as failed.
           c. catch ForecastCorruptError: ACK the message (log critical, no retry).
           d. catch ForecastExpiredError: ACK the message (log warning, no retry).
           e. catch Exception: Mark the specific message as failed (NACK for retry).
        4. Return ``SQSBatchResponse`` with partial failures.

        Parameters
        ----------
        event : dict
            Raw SQS Lambda event with ``Records`` array.
        context : Any
            AWS Lambda context object.

        Returns
        -------
        dict
            SQS batch response with ``batchItemFailures`` array.
        """
        response = SQSBatchResponse()
        timeout_guard = TimeoutGuard(context)

        # Step 1: Parse SQS batch
        parsed_messages = _parse_sqs_records(event)
        if not parsed_messages:
            return response.to_dict()

        # Step 2: Group by tile_id
        tile_groups = _group_by_tile(parsed_messages)

        # Track all message IDs for timeout handling
        # We need an ordered list of remaining message IDs so we can mark
        # all unprocessed ones as failed when timeout is imminent.
        all_message_ids = [msg_id for msg_id, _ in parsed_messages]
        processed_message_ids: set[str] = set()

        # Step 3: Process each tile group
        timeout_hit = False

        for tile_id, tile_messages in tile_groups.items():
            if timeout_hit:
                # Mark all remaining messages in this group as failed
                for message_id, _ in tile_messages:
                    if message_id not in processed_message_ids:
                        response.add_failure(message_id)
                        processed_message_ids.add(message_id)
                continue

            for message_id, msg in tile_messages:
                if timeout_hit:
                    response.add_failure(message_id)
                    processed_message_ids.add(message_id)
                    continue

                try:
                    self.process_tile(msg, context, timeout_guard)
                    processed_message_ids.add(message_id)

                except TimeBudgetExceededError:
                    logger.warning(
                        "Timeout imminent while processing tile=%s "
                        "messageId=%s. Marking remaining messages as failed.",
                        tile_id,
                        message_id,
                    )
                    timeout_hit = True
                    # The current message was not fully processed -- mark as failed
                    response.add_failure(message_id)
                    processed_message_ids.add(message_id)

                except ForecastCorruptError as exc:
                    # Per FAIL-008: ACK corrupt data messages.
                    # Corrupt data cannot be fixed by retrying.
                    logger.critical(
                        "Forecast data corrupt for tile=%s messageId=%s: %s. "
                        "ACKing message to prevent infinite retry loop.",
                        tile_id,
                        message_id,
                        str(exc),
                    )
                    processed_message_ids.add(message_id)
                    # Do NOT add to failures -- ACK by omission

                except ForecastExpiredError as exc:
                    # Per EVAL-001 step 9: ACK expired data messages.
                    # Data has been replaced or deleted.
                    logger.warning(
                        "Forecast data expired for tile=%s messageId=%s: %s. "
                        "ACKing message (data replaced or deleted).",
                        tile_id,
                        message_id,
                        str(exc),
                    )
                    processed_message_ids.add(message_id)
                    # Do NOT add to failures -- ACK by omission

                except Exception as exc:
                    # All other errors: NACK for retry via SQS visibility timeout
                    logger.exception(
                        "Error processing tile=%s messageId=%s: %s. "
                        "Message will be retried.",
                        tile_id,
                        message_id,
                        str(exc),
                    )
                    response.add_failure(message_id)
                    processed_message_ids.add(message_id)

        # Mark any remaining unprocessed messages as failures
        # (safety net for edge cases)
        for msg_id in all_message_ids:
            if msg_id not in processed_message_ids:
                response.add_failure(msg_id)

        return response.to_dict()

    def process_tile(
        self,
        msg: EvalMessage,
        context: Any,
        timeout_guard: TimeoutGuard,
    ) -> None:
        """Process a single tile evaluation message.

        Action Routing:
        - If ``msg.action == 'generate_summary'``: Route to SummaryGenerator (stub).
        - Otherwise (default 'evaluate'): Route to standard Evaluator.

        Processing flow:
        1. Load tile data from Zarr via ForecastReader.
        2. Fetch WatchPoints for the tile from the database.
        3. Apply specific_watchpoint_ids filter if present.
        4. Fetch evaluation states for the WatchPoints.
        5. Run evaluation (Evaluator) or generate summary (SummaryGenerator).
        6. Commit results atomically (Repository).
        7. Emit EvaluationLag metric.

        Parameters
        ----------
        msg : EvalMessage
            The parsed evaluation message.
        context : Any
            AWS Lambda context object.
        timeout_guard : TimeoutGuard
            Guard for checking remaining Lambda execution time.

        Raises
        ------
        ForecastCorruptError
            If the forecast data is corrupt (propagated from reader).
        ForecastExpiredError
            If the forecast data has expired (propagated from reader).
        TimeBudgetExceededError
            If the Lambda timeout is imminent (propagated from evaluator).
        """
        logger.info(
            "Processing tile=%s batch_id=%s trace_id=%s forecast_type=%s "
            "run_timestamp=%s action=%s page=%d page_size=%d",
            msg.tile_id,
            msg.batch_id,
            msg.trace_id,
            msg.forecast_type.value,
            msg.run_timestamp.isoformat(),
            msg.action.value,
            msg.page,
            msg.page_size,
        )

        # Step 1: Load tile data
        tile_data = self._reader.load_tile(
            model=msg.forecast_type,
            timestamp=msg.run_timestamp,
            tile_id=msg.tile_id,
        )

        # Step 2: Fetch WatchPoints for the tile
        watchpoints = self._repo.fetch_watchpoints(
            tile_id=msg.tile_id,
            page=msg.page,
            page_size=msg.page_size,
        )

        if not watchpoints:
            logger.info(
                "No active WatchPoints for tile=%s page=%d. Skipping.",
                msg.tile_id,
                msg.page,
            )
            self._emit_evaluation_lag(msg)
            return

        # Step 3: Apply specific_watchpoint_ids filter if present
        # Per spec: "If populated, the worker must filter execution to only these IDs."
        if msg.specific_watchpoint_ids:
            target_ids = set(msg.specific_watchpoint_ids)
            watchpoints = [wp for wp in watchpoints if wp.id in target_ids]
            if not watchpoints:
                logger.info(
                    "No matching WatchPoints after specific_watchpoint_ids "
                    "filter for tile=%s. Skipping.",
                    msg.tile_id,
                )
                self._emit_evaluation_lag(msg)
                return

        # Step 4: Route based on action
        if msg.action == EvalAction.GENERATE_SUMMARY:
            # Summary generation (stub -- implemented in separate task)
            logger.info(
                "Summary generation requested for tile=%s. "
                "Routing to SummaryGenerator (stub).",
                msg.tile_id,
            )
            # SummaryGenerator is not part of this task's scope.
            # When implemented, it will be invoked here.
            self._emit_evaluation_lag(msg)
            return

        # Default: evaluate
        # Step 5: Fetch evaluation states
        wp_ids = [wp.id for wp in watchpoints]
        states = self._repo.fetch_states(wp_ids)

        # Step 6: Run evaluation
        result: BatchResult = self._evaluator.evaluate_batch(
            wps=watchpoints,
            states=states,
            tile_data=tile_data,
            timeout_guard=timeout_guard,
            trace_id=msg.trace_id,
            current_tile_id=msg.tile_id,
            forecast_timestamp=msg.run_timestamp,
            source_model=msg.forecast_type.value,
        )

        # Step 7: Commit results atomically
        if result.state_updates or result.notifications:
            wp_map = {wp.id: wp for wp in watchpoints}
            self._repo.commit_batch(
                result=result,
                msg_timestamp=msg.run_timestamp,
                watchpoints=wp_map,
            )
            logger.info(
                "Committed batch for tile=%s: %d state updates, "
                "%d notifications.",
                msg.tile_id,
                len(result.state_updates),
                len(result.notifications),
            )

        # Step 8: Emit EvaluationLag metric
        self._emit_evaluation_lag(msg)

    def _emit_evaluation_lag(self, msg: EvalMessage) -> None:
        """Emit the EvaluationLag metric.

        Per spec: Worker MUST emit ``MetricEvaluationLag``
        (Now - ForecastTimestamp) for every tile processed. This metric
        tracks how far behind real-time the evaluation pipeline is running.

        Parameters
        ----------
        msg : EvalMessage
            The evaluation message (contains run_timestamp and forecast_type).
        """
        now = datetime.now(timezone.utc)
        # Ensure run_timestamp is timezone-aware for subtraction
        run_ts = msg.run_timestamp
        if run_ts.tzinfo is None:
            run_ts = run_ts.replace(tzinfo=timezone.utc)
        lag_seconds = (now - run_ts).total_seconds()

        try:
            self._metric_emitter(
                METRIC_EVALUATION_LAG,
                lag_seconds,
                "Seconds",
                {"ForecastType": msg.forecast_type.value},
            )
        except Exception:
            logger.warning(
                "Failed to emit EvaluationLag metric for tile=%s",
                msg.tile_id,
                exc_info=True,
            )

        logger.info(
            "EvaluationLag: %.1fs for tile=%s forecast_type=%s "
            "run_timestamp=%s",
            lag_seconds,
            msg.tile_id,
            msg.forecast_type.value,
            msg.run_timestamp.isoformat(),
        )


# ---------------------------------------------------------------------------
# Module-level handler (Lambda entrypoint)
# ---------------------------------------------------------------------------

# Singleton worker instance, initialized on first cold start.
# This follows the Lambda best practice of initializing expensive resources
# (DB connections, S3 clients) outside the handler function so they persist
# across warm invocations.
_worker: EvalWorker | None = None


def _create_worker() -> EvalWorker:
    """Create and configure the EvalWorker singleton.

    Reads configuration from environment variables (via Settings),
    initializes the repository, forecast reader, and evaluator.

    Returns
    -------
    EvalWorker
        Configured worker instance.
    """
    from worker.eval.reader import create_forecast_reader
    from worker.eval.repo import PostgresRepository

    import boto3

    settings = load_settings()

    # Initialize SQS client
    sqs_client = boto3.client("sqs", region_name=settings.aws_region)

    # Initialize repository
    repo = PostgresRepository(
        conninfo=settings.database_url.get_secret_value(),
        sqs_client=sqs_client,
        queue_url=settings.queue_url,
    )

    # Initialize forecast reader with metric emitter
    # In production, this would use aws-lambda-powertools Metrics
    metric_emitter = _default_metric_emitter

    reader = create_forecast_reader(
        bucket=settings.forecast_bucket,
        aws_region=settings.aws_region,
        metric_emitter=metric_emitter,
    )

    # Initialize evaluator
    evaluator = StandardEvaluator(
        enable_threat_dedup=settings.enable_threat_dedup,
    )

    return EvalWorker(
        repo=repo,
        reader=reader,
        evaluator=evaluator,
        metric_emitter=metric_emitter,
    )


def handler(event: dict[str, Any], context: Any) -> dict[str, Any]:
    """AWS Lambda handler entrypoint.

    This is the function configured as the Lambda handler in the SAM
    template. It delegates to the ``EvalWorker`` singleton.

    Parameters
    ----------
    event : dict
        SQS Lambda event containing ``Records``.
    context : Any
        AWS Lambda context object.

    Returns
    -------
    dict
        SQS batch response with ``batchItemFailures``.
    """
    global _worker
    if _worker is None:
        _worker = _create_worker()

    return _worker.handler(event, context)
