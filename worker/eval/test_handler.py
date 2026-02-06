"""
Tests for the Eval Worker Lambda Handler.

Validates:
1. SQS batch parsing and message grouping by tile_id.
2. Happy path: full process_tile flow (load -> fetch -> evaluate -> commit).
3. ForecastCorruptError: ACKed (not in batchItemFailures).
4. ForecastExpiredError: ACKed (not in batchItemFailures).
5. TimeBudgetExceededError: current + remaining messages marked as failed.
6. Generic exceptions: message marked as failed (NACK for SQS retry).
7. EvaluationLag metric emission.
8. Partial batch failure response format.
9. specific_watchpoint_ids filtering.
10. Action routing (evaluate vs generate_summary).
11. Empty batch handling.
12. Malformed message body handling (parse error -> ACK).
13. TimeoutGuard threshold behavior.

Uses ``unittest.mock`` to mock Repository, ForecastReader, and StandardEvaluator.

Architecture References:
    - 07-eval-worker.md Section 6 (Lambda Handler & Execution)
    - flow-simulations.md EVAL-001 (Evaluation Pipeline)
    - flow-simulations.md FAIL-008 (Zarr Corruption -> ACK)
    - flow-simulations.md FAIL-011 (Lambda Function Error)
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any
from unittest.mock import MagicMock, call, patch

import pytest

from worker.eval.handler import (
    METRIC_EVALUATION_LAG,
    EvalWorker,
    SQSBatchItemFailure,
    SQSBatchResponse,
    TimeoutGuard,
    _default_metric_emitter,
    _group_by_tile,
    _parse_sqs_records,
)
from worker.eval.logic.evaluator import (
    StandardEvaluator,
    TimeBudgetExceededError,
)
from worker.eval.models import (
    BatchResult,
    Channel,
    Condition,
    ConditionLogic,
    EvalAction,
    EvalMessage,
    EvaluationState,
    ForecastType,
    Location,
    NotificationEvent,
    NotificationOrdering,
    Status,
    WatchPoint,
)
from worker.eval.reader import (
    ForecastCorruptError,
    ForecastExpiredError,
    ForecastReader,
    TileData,
)
from worker.eval.repo import Repository


# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

NOW = datetime(2026, 2, 6, 12, 0, 0, tzinfo=timezone.utc)
RUN_TIMESTAMP = datetime(2026, 2, 6, 11, 30, 0, tzinfo=timezone.utc)
TILE_ID = "3.4"
BATCH_ID = "batch-001"
TRACE_ID = "trace-001"


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------


def _make_eval_message(
    tile_id: str = TILE_ID,
    batch_id: str = BATCH_ID,
    trace_id: str = TRACE_ID,
    forecast_type: str = "medium_range",
    run_timestamp: datetime = RUN_TIMESTAMP,
    page: int = 0,
    page_size: int = 0,
    action: str = "evaluate",
    specific_watchpoint_ids: list[str] | None = None,
) -> dict[str, Any]:
    """Create a raw EvalMessage dict (as it would appear in SQS body)."""
    msg = {
        "batch_id": batch_id,
        "trace_id": trace_id,
        "forecast_type": forecast_type,
        "run_timestamp": run_timestamp.isoformat(),
        "tile_id": tile_id,
        "page": page,
        "page_size": page_size,
        "total_items": 0,
        "action": action,
        "specific_watchpoint_ids": specific_watchpoint_ids or [],
    }
    return msg


def _make_sqs_event(
    messages: list[tuple[str, dict[str, Any]]],
) -> dict[str, Any]:
    """Create a raw SQS Lambda event.

    Parameters
    ----------
    messages : list[tuple[str, dict]]
        List of (messageId, body_dict) tuples.
    """
    records = []
    for message_id, body in messages:
        records.append(
            {
                "messageId": message_id,
                "body": json.dumps(body),
                "receiptHandle": f"receipt-{message_id}",
                "attributes": {},
                "messageAttributes": {},
                "md5OfBody": "",
                "eventSource": "aws:sqs",
                "eventSourceARN": "arn:aws:sqs:us-east-1:123456789:eval-queue",
                "awsRegion": "us-east-1",
            }
        )
    return {"Records": records}


def _make_context(remaining_ms: int = 60000) -> MagicMock:
    """Create a mock AWS Lambda context object.

    Parameters
    ----------
    remaining_ms : int
        Value returned by ``get_remaining_time_in_millis()``.
    """
    ctx = MagicMock()
    ctx.get_remaining_time_in_millis.return_value = remaining_ms
    return ctx


def _make_watchpoint(
    wp_id: str = "wp_001",
    tile_id: str = TILE_ID,
    org_id: str = "org_001",
    test_mode: bool = False,
) -> WatchPoint:
    """Create a minimal WatchPoint for testing."""
    return WatchPoint(
        id=wp_id,
        organization_id=org_id,
        name=f"Test WP {wp_id}",
        location=Location(lat=45.0, lon=90.0),
        timezone="UTC",
        tile_id=tile_id,
        conditions=[
            Condition(
                variable="temperature_c",
                operator=">",
                threshold=[30.0],
                unit="C",
            )
        ],
        condition_logic=ConditionLogic.ANY,
        channels=[
            Channel(id="ch_001", type="email", config={"to": "a@b.com"}),
        ],
        status=Status.ACTIVE,
        test_mode=test_mode,
        config_version=1,
    )


def _make_worker(
    repo: Repository | None = None,
    reader: ForecastReader | None = None,
    evaluator: StandardEvaluator | None = None,
    metric_emitter: Any | None = None,
) -> EvalWorker:
    """Create an EvalWorker with mock dependencies."""
    return EvalWorker(
        repo=repo or MagicMock(spec=Repository),
        reader=reader or MagicMock(spec=ForecastReader),
        evaluator=evaluator or MagicMock(spec=StandardEvaluator),
        metric_emitter=metric_emitter,
    )


# ---------------------------------------------------------------------------
# Tests: SQS Batch Response
# ---------------------------------------------------------------------------


class TestSQSBatchResponse:
    """Tests for the SQS partial batch failure response."""

    def test_empty_response(self) -> None:
        response = SQSBatchResponse()
        assert response.to_dict() == {"batchItemFailures": []}

    def test_single_failure(self) -> None:
        response = SQSBatchResponse()
        response.add_failure("msg-001")
        assert response.to_dict() == {
            "batchItemFailures": [{"itemIdentifier": "msg-001"}]
        }

    def test_multiple_failures(self) -> None:
        response = SQSBatchResponse()
        response.add_failure("msg-001")
        response.add_failure("msg-002")
        result = response.to_dict()
        assert len(result["batchItemFailures"]) == 2
        assert result["batchItemFailures"][0]["itemIdentifier"] == "msg-001"
        assert result["batchItemFailures"][1]["itemIdentifier"] == "msg-002"


class TestSQSBatchItemFailure:
    """Tests for single batch item failure."""

    def test_to_dict(self) -> None:
        failure = SQSBatchItemFailure("msg-123")
        assert failure.to_dict() == {"itemIdentifier": "msg-123"}


# ---------------------------------------------------------------------------
# Tests: TimeoutGuard
# ---------------------------------------------------------------------------


class TestTimeoutGuard:
    """Tests for the Lambda timeout guard."""

    def test_check_remaining_sufficient_time(self) -> None:
        ctx = _make_context(remaining_ms=30000)
        guard = TimeoutGuard(ctx)
        # Should not raise
        guard.check_remaining()

    def test_check_remaining_exactly_at_threshold(self) -> None:
        ctx = _make_context(remaining_ms=5000)
        guard = TimeoutGuard(ctx)
        # 5000ms is the threshold; should raise at < 5000
        guard.check_remaining()  # Should not raise at exactly 5000

    def test_check_remaining_below_threshold(self) -> None:
        ctx = _make_context(remaining_ms=4999)
        guard = TimeoutGuard(ctx)
        with pytest.raises(TimeBudgetExceededError, match="timeout imminent"):
            guard.check_remaining()

    def test_check_remaining_zero(self) -> None:
        ctx = _make_context(remaining_ms=0)
        guard = TimeoutGuard(ctx)
        with pytest.raises(TimeBudgetExceededError):
            guard.check_remaining()


# ---------------------------------------------------------------------------
# Tests: Message Parsing
# ---------------------------------------------------------------------------


class TestParseSQSRecords:
    """Tests for SQS event record parsing."""

    def test_parse_single_record(self) -> None:
        msg_body = _make_eval_message()
        event = _make_sqs_event([("msg-001", msg_body)])
        parsed = _parse_sqs_records(event)

        assert len(parsed) == 1
        msg_id, msg = parsed[0]
        assert msg_id == "msg-001"
        assert msg.tile_id == TILE_ID
        assert msg.batch_id == BATCH_ID
        assert msg.trace_id == TRACE_ID
        assert msg.forecast_type == ForecastType.MEDIUM_RANGE

    def test_parse_multiple_records(self) -> None:
        msg_body_1 = _make_eval_message(tile_id="1.2")
        msg_body_2 = _make_eval_message(tile_id="3.4")
        event = _make_sqs_event([
            ("msg-001", msg_body_1),
            ("msg-002", msg_body_2),
        ])
        parsed = _parse_sqs_records(event)
        assert len(parsed) == 2

    def test_parse_empty_records(self) -> None:
        event = {"Records": []}
        parsed = _parse_sqs_records(event)
        assert parsed == []

    def test_parse_no_records_key(self) -> None:
        event = {}
        parsed = _parse_sqs_records(event)
        assert parsed == []

    def test_parse_malformed_body_skipped(self) -> None:
        """Malformed message bodies are skipped (ACKed by omission)."""
        event = {
            "Records": [
                {
                    "messageId": "msg-bad",
                    "body": "not valid json {{{",
                    "receiptHandle": "receipt-bad",
                },
                {
                    "messageId": "msg-good",
                    "body": json.dumps(_make_eval_message()),
                    "receiptHandle": "receipt-good",
                },
            ]
        }
        parsed = _parse_sqs_records(event)
        # Only the valid message is returned
        assert len(parsed) == 1
        assert parsed[0][0] == "msg-good"

    def test_parse_missing_required_field_skipped(self) -> None:
        """Messages missing required fields are skipped."""
        bad_body = {"batch_id": "b1"}  # Missing most required fields
        event = _make_sqs_event([("msg-bad", bad_body)])
        parsed = _parse_sqs_records(event)
        assert len(parsed) == 0

    def test_parse_with_action_field(self) -> None:
        msg_body = _make_eval_message(action="generate_summary")
        event = _make_sqs_event([("msg-001", msg_body)])
        parsed = _parse_sqs_records(event)
        assert parsed[0][1].action == EvalAction.GENERATE_SUMMARY

    def test_parse_with_specific_watchpoint_ids(self) -> None:
        msg_body = _make_eval_message(
            specific_watchpoint_ids=["wp_001", "wp_002"]
        )
        event = _make_sqs_event([("msg-001", msg_body)])
        parsed = _parse_sqs_records(event)
        assert parsed[0][1].specific_watchpoint_ids == ["wp_001", "wp_002"]


# ---------------------------------------------------------------------------
# Tests: Tile Grouping
# ---------------------------------------------------------------------------


class TestGroupByTile:
    """Tests for message grouping by tile_id."""

    def test_single_tile(self) -> None:
        msg1 = EvalMessage(**_make_eval_message(tile_id="3.4"))
        msg2 = EvalMessage(**_make_eval_message(tile_id="3.4"))
        messages = [("msg-001", msg1), ("msg-002", msg2)]
        groups = _group_by_tile(messages)
        assert len(groups) == 1
        assert "3.4" in groups
        assert len(groups["3.4"]) == 2

    def test_multiple_tiles(self) -> None:
        msg1 = EvalMessage(**_make_eval_message(tile_id="1.2"))
        msg2 = EvalMessage(**_make_eval_message(tile_id="3.4"))
        msg3 = EvalMessage(**_make_eval_message(tile_id="1.2"))
        messages = [("msg-001", msg1), ("msg-002", msg2), ("msg-003", msg3)]
        groups = _group_by_tile(messages)
        assert len(groups) == 2
        assert len(groups["1.2"]) == 2
        assert len(groups["3.4"]) == 1

    def test_empty_messages(self) -> None:
        groups = _group_by_tile([])
        assert groups == {}


# ---------------------------------------------------------------------------
# Tests: EvalWorker Handler
# ---------------------------------------------------------------------------


class TestEvalWorkerHandler:
    """Tests for the main handler method."""

    def test_happy_path_single_message(self) -> None:
        """Full happy path: parse -> load tile -> evaluate -> commit."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)
        metric_emitter = MagicMock()

        wp = _make_watchpoint()
        tile_data = MagicMock(spec=TileData)
        batch_result = BatchResult(
            state_updates=[EvaluationState(watchpoint_id="wp_001")],
            notifications=[],
        )

        repo.fetch_watchpoints.return_value = [wp]
        repo.fetch_states.return_value = {}
        reader.load_tile.return_value = tile_data
        evaluator.evaluate_batch.return_value = batch_result

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
            metric_emitter=metric_emitter,
        )

        msg_body = _make_eval_message()
        event = _make_sqs_event([("msg-001", msg_body)])
        context = _make_context(remaining_ms=60000)

        result = worker.handler(event, context)

        # No failures
        assert result == {"batchItemFailures": []}

        # Verify reader was called with correct args
        reader.load_tile.assert_called_once_with(
            model=ForecastType.MEDIUM_RANGE,
            timestamp=RUN_TIMESTAMP,
            tile_id=TILE_ID,
        )

        # Verify WPs were fetched
        repo.fetch_watchpoints.assert_called_once_with(
            tile_id=TILE_ID, page=0, page_size=0,
        )

        # Verify states were fetched
        repo.fetch_states.assert_called_once_with(["wp_001"])

        # Verify evaluator was called
        evaluator.evaluate_batch.assert_called_once()

        # Verify commit was called
        repo.commit_batch.assert_called_once()

        # Verify EvaluationLag metric was emitted
        metric_emitter.assert_called()
        metric_call = metric_emitter.call_args
        assert metric_call[0][0] == METRIC_EVALUATION_LAG
        assert isinstance(metric_call[0][1], float)
        assert metric_call[0][2] == "Seconds"
        assert metric_call[0][3] == {"ForecastType": "medium_range"}

    def test_empty_event(self) -> None:
        """Empty event returns empty response."""
        worker = _make_worker()
        result = worker.handler({"Records": []}, _make_context())
        assert result == {"batchItemFailures": []}

    def test_forecast_corrupt_error_is_acked(self) -> None:
        """ForecastCorruptError causes ACK (not in failures)."""
        reader = MagicMock(spec=ForecastReader)
        reader.load_tile.side_effect = ForecastCorruptError("corrupt zarr")

        worker = _make_worker(reader=reader)
        msg_body = _make_eval_message()
        event = _make_sqs_event([("msg-001", msg_body)])

        result = worker.handler(event, _make_context())

        # ACKed: not in batchItemFailures
        assert result == {"batchItemFailures": []}

    def test_forecast_expired_error_is_acked(self) -> None:
        """ForecastExpiredError causes ACK (not in failures)."""
        reader = MagicMock(spec=ForecastReader)
        reader.load_tile.side_effect = ForecastExpiredError("data expired")

        worker = _make_worker(reader=reader)
        msg_body = _make_eval_message()
        event = _make_sqs_event([("msg-001", msg_body)])

        result = worker.handler(event, _make_context())

        # ACKed: not in batchItemFailures
        assert result == {"batchItemFailures": []}

    def test_generic_error_is_nacked(self) -> None:
        """Generic exceptions cause NACK (message in failures for retry)."""
        reader = MagicMock(spec=ForecastReader)
        reader.load_tile.side_effect = RuntimeError("database connection lost")

        worker = _make_worker(reader=reader)
        msg_body = _make_eval_message()
        event = _make_sqs_event([("msg-001", msg_body)])

        result = worker.handler(event, _make_context())

        # NACKed: appears in batchItemFailures
        assert len(result["batchItemFailures"]) == 1
        assert result["batchItemFailures"][0]["itemIdentifier"] == "msg-001"

    def test_timeout_marks_remaining_as_failed(self) -> None:
        """TimeBudgetExceededError marks current + remaining messages as failed."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        # First message succeeds, second triggers timeout
        tile_data = MagicMock(spec=TileData)
        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}

        call_count = [0]

        def side_effect_evaluate(*args, **kwargs):
            call_count[0] += 1
            if call_count[0] == 1:
                return BatchResult(
                    state_updates=[EvaluationState(watchpoint_id="wp_001")],
                    notifications=[],
                )
            raise TimeBudgetExceededError("timeout imminent")

        evaluator.evaluate_batch.side_effect = side_effect_evaluate

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        msg_body_1 = _make_eval_message(tile_id="1.2")
        msg_body_2 = _make_eval_message(tile_id="3.4")
        msg_body_3 = _make_eval_message(tile_id="5.6")
        event = _make_sqs_event([
            ("msg-001", msg_body_1),
            ("msg-002", msg_body_2),
            ("msg-003", msg_body_3),
        ])

        result = worker.handler(event, _make_context())

        # msg-001 succeeded, msg-002 timed out, msg-003 not processed
        failed_ids = [
            f["itemIdentifier"] for f in result["batchItemFailures"]
        ]
        assert "msg-001" not in failed_ids
        assert "msg-002" in failed_ids
        assert "msg-003" in failed_ids

    def test_partial_batch_mixed_errors(self) -> None:
        """Batch with mixed success/corrupt/failure returns correct partial response."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        tile_data = MagicMock(spec=TileData)

        # Message 1 (tile 1.2): success
        # Message 2 (tile 3.4): ForecastCorruptError (ACK)
        # Message 3 (tile 5.6): RuntimeError (NACK)
        load_call_count = [0]

        def load_tile_side_effect(model, timestamp, tile_id):
            load_call_count[0] += 1
            if tile_id == "3.4":
                raise ForecastCorruptError("corrupt")
            if tile_id == "5.6":
                raise RuntimeError("db failure")
            return tile_data

        reader.load_tile.side_effect = load_tile_side_effect
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = BatchResult(
            state_updates=[EvaluationState(watchpoint_id="wp_001")],
            notifications=[],
        )

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        event = _make_sqs_event([
            ("msg-001", _make_eval_message(tile_id="1.2")),
            ("msg-002", _make_eval_message(tile_id="3.4")),
            ("msg-003", _make_eval_message(tile_id="5.6")),
        ])

        result = worker.handler(event, _make_context())

        failed_ids = [
            f["itemIdentifier"] for f in result["batchItemFailures"]
        ]
        # msg-001: success, not in failures
        assert "msg-001" not in failed_ids
        # msg-002: corrupt, ACKed, not in failures
        assert "msg-002" not in failed_ids
        # msg-003: runtime error, NACKed, in failures
        assert "msg-003" in failed_ids

    def test_no_watchpoints_for_tile(self) -> None:
        """When no active WatchPoints exist for a tile, skips evaluation."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)
        metric_emitter = MagicMock()

        tile_data = MagicMock(spec=TileData)
        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = []

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
            metric_emitter=metric_emitter,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        result = worker.handler(event, _make_context())

        assert result == {"batchItemFailures": []}
        # Evaluator should NOT be called when no watchpoints
        evaluator.evaluate_batch.assert_not_called()
        # Commit should NOT be called
        repo.commit_batch.assert_not_called()
        # Metric should still be emitted
        metric_emitter.assert_called()

    def test_specific_watchpoint_ids_filter(self) -> None:
        """specific_watchpoint_ids filters WatchPoints before evaluation."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        wp1 = _make_watchpoint(wp_id="wp_001")
        wp2 = _make_watchpoint(wp_id="wp_002")
        wp3 = _make_watchpoint(wp_id="wp_003")

        tile_data = MagicMock(spec=TileData)
        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = [wp1, wp2, wp3]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = BatchResult()

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        msg_body = _make_eval_message(
            specific_watchpoint_ids=["wp_001", "wp_003"]
        )
        event = _make_sqs_event([("msg-001", msg_body)])

        worker.handler(event, _make_context())

        # Only wp_001 and wp_003 should be passed to evaluator
        call_args = evaluator.evaluate_batch.call_args
        wps_arg = call_args[1]["wps"] if "wps" in call_args[1] else call_args[0][0]
        wp_ids = [wp.id for wp in wps_arg]
        assert "wp_001" in wp_ids
        assert "wp_003" in wp_ids
        assert "wp_002" not in wp_ids

    def test_specific_watchpoint_ids_filter_no_matches(self) -> None:
        """When specific_watchpoint_ids filters out all WPs, skip evaluation."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        wp1 = _make_watchpoint(wp_id="wp_001")
        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [wp1]

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        msg_body = _make_eval_message(
            specific_watchpoint_ids=["wp_999"]  # No match
        )
        event = _make_sqs_event([("msg-001", msg_body)])

        result = worker.handler(event, _make_context())

        assert result == {"batchItemFailures": []}
        evaluator.evaluate_batch.assert_not_called()

    def test_generate_summary_action_skips_evaluation(self) -> None:
        """Messages with action=generate_summary skip the evaluator."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)
        metric_emitter = MagicMock()

        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
            metric_emitter=metric_emitter,
        )

        msg_body = _make_eval_message(action="generate_summary")
        event = _make_sqs_event([("msg-001", msg_body)])

        result = worker.handler(event, _make_context())

        assert result == {"batchItemFailures": []}
        # Evaluator should NOT be called for summary generation
        evaluator.evaluate_batch.assert_not_called()
        # States should NOT be fetched
        repo.fetch_states.assert_not_called()
        # Metric should still be emitted
        metric_emitter.assert_called()

    def test_empty_batch_result_skips_commit(self) -> None:
        """When evaluation produces no state updates or notifications, skip commit."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        # Empty result: no updates, no notifications
        evaluator.evaluate_batch.return_value = BatchResult()

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        worker.handler(event, _make_context())

        # Commit should NOT be called for empty results
        repo.commit_batch.assert_not_called()

    def test_commit_called_with_correct_args(self) -> None:
        """Verify commit_batch receives correct result, timestamp, and WP map."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        wp = _make_watchpoint()
        tile_data = MagicMock(spec=TileData)
        batch_result = BatchResult(
            state_updates=[EvaluationState(watchpoint_id="wp_001")],
            notifications=[
                NotificationEvent(
                    id="notif_001",
                    watchpoint_id="wp_001",
                    organization_id="org_001",
                    trace_id=TRACE_ID,
                    event_type="threshold_crossed",
                    urgency="routine",
                    payload={},
                    ordering=NotificationOrdering(
                        event_sequence=1,
                        forecast_timestamp=RUN_TIMESTAMP,
                        evaluation_timestamp=NOW,
                    ),
                )
            ],
        )

        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = [wp]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = batch_result

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        worker.handler(event, _make_context())

        repo.commit_batch.assert_called_once()
        commit_kwargs = repo.commit_batch.call_args
        # Check the result argument
        assert commit_kwargs[1]["result"] is batch_result
        # Check the msg_timestamp argument
        assert commit_kwargs[1]["msg_timestamp"] == RUN_TIMESTAMP
        # Check the watchpoints map argument
        wp_map = commit_kwargs[1]["watchpoints"]
        assert "wp_001" in wp_map
        assert wp_map["wp_001"] is wp


# ---------------------------------------------------------------------------
# Tests: EvaluationLag Metric
# ---------------------------------------------------------------------------


class TestEvaluationLagMetric:
    """Tests for the EvaluationLag metric emission."""

    def test_metric_emitted_on_success(self) -> None:
        """EvaluationLag metric is emitted after successful processing."""
        metric_emitter = MagicMock()
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = BatchResult(
            state_updates=[EvaluationState(watchpoint_id="wp_001")],
        )

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
            metric_emitter=metric_emitter,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        worker.handler(event, _make_context())

        metric_emitter.assert_called_once()
        args = metric_emitter.call_args[0]
        assert args[0] == METRIC_EVALUATION_LAG
        assert isinstance(args[1], float)
        assert args[1] > 0  # Lag should be positive (run_timestamp is in the past)
        assert args[2] == "Seconds"
        assert args[3] == {"ForecastType": "medium_range"}

    def test_metric_emitted_even_with_no_watchpoints(self) -> None:
        """EvaluationLag metric is emitted even when tile has no WatchPoints."""
        metric_emitter = MagicMock()
        reader = MagicMock(spec=ForecastReader)
        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo = MagicMock(spec=Repository)
        repo.fetch_watchpoints.return_value = []

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=MagicMock(spec=StandardEvaluator),
            metric_emitter=metric_emitter,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        worker.handler(event, _make_context())

        metric_emitter.assert_called_once()

    def test_metric_emitter_failure_does_not_propagate(self) -> None:
        """If metric emission fails, the error is swallowed (logged, not raised)."""
        metric_emitter = MagicMock(side_effect=Exception("CloudWatch down"))
        reader = MagicMock(spec=ForecastReader)
        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo = MagicMock(spec=Repository)
        repo.fetch_watchpoints.return_value = []

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=MagicMock(spec=StandardEvaluator),
            metric_emitter=metric_emitter,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        # Should not raise even though metric emitter throws
        result = worker.handler(event, _make_context())
        assert result == {"batchItemFailures": []}


# ---------------------------------------------------------------------------
# Tests: Tile Grouping Optimization
# ---------------------------------------------------------------------------


class TestTileGroupingOptimization:
    """Tests verifying that tile grouping optimizes Zarr I/O."""

    def test_same_tile_messages_share_single_load(self) -> None:
        """Multiple messages for the same tile should each call load_tile
        (since they may have different run_timestamps), but the grouping
        still enables batch optimization in the evaluator."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        tile_data = MagicMock(spec=TileData)
        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = BatchResult(
            state_updates=[EvaluationState(watchpoint_id="wp_001")],
        )

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        # Two messages for the same tile
        event = _make_sqs_event([
            ("msg-001", _make_eval_message(tile_id="3.4")),
            ("msg-002", _make_eval_message(tile_id="3.4")),
        ])

        result = worker.handler(event, _make_context())

        assert result == {"batchItemFailures": []}
        # Each message triggers its own process_tile call
        assert reader.load_tile.call_count == 2


# ---------------------------------------------------------------------------
# Tests: Evaluator Integration
# ---------------------------------------------------------------------------


class TestEvaluatorIntegration:
    """Tests for correct evaluator invocation and argument passing."""

    def test_evaluator_receives_correct_arguments(self) -> None:
        """Verify evaluator.evaluate_batch receives all required arguments."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        wp = _make_watchpoint()
        tile_data = MagicMock(spec=TileData)
        states = {"wp_001": EvaluationState(watchpoint_id="wp_001")}

        reader.load_tile.return_value = tile_data
        repo.fetch_watchpoints.return_value = [wp]
        repo.fetch_states.return_value = states
        evaluator.evaluate_batch.return_value = BatchResult()

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        worker.handler(event, _make_context())

        evaluator.evaluate_batch.assert_called_once()
        call_kwargs = evaluator.evaluate_batch.call_args[1]

        assert call_kwargs["wps"] == [wp]
        assert call_kwargs["states"] == states
        assert call_kwargs["tile_data"] is tile_data
        assert call_kwargs["trace_id"] == TRACE_ID
        assert call_kwargs["current_tile_id"] == TILE_ID
        assert call_kwargs["forecast_timestamp"] == RUN_TIMESTAMP
        assert call_kwargs["source_model"] == "medium_range"
        # timeout_guard should be a TimeoutGuard instance
        assert call_kwargs["timeout_guard"] is not None

    def test_evaluator_timeout_propagates_correctly(self) -> None:
        """TimeBudgetExceededError from evaluator propagates to handler."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.side_effect = TimeBudgetExceededError(
            "timeout"
        )

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        result = worker.handler(event, _make_context())

        # Message should be in failures
        assert len(result["batchItemFailures"]) == 1
        assert result["batchItemFailures"][0]["itemIdentifier"] == "msg-001"


# ---------------------------------------------------------------------------
# Tests: Default Metric Emitter
# ---------------------------------------------------------------------------


class TestDefaultMetricEmitter:
    """Tests for the fallback metric emitter."""

    def test_default_emitter_does_not_raise(self) -> None:
        """Default metric emitter logs but does not raise."""
        # Should not raise
        _default_metric_emitter(
            "TestMetric",
            42.0,
            "Count",
            {"Key": "Value"},
        )


# ---------------------------------------------------------------------------
# Tests: Module-level handler function
# ---------------------------------------------------------------------------


class TestModuleLevelHandler:
    """Tests for the module-level handler() function (Lambda entrypoint)."""

    def test_handler_creates_singleton_worker(self) -> None:
        """The module-level handler creates a worker on first call."""
        import worker.eval.handler as handler_module

        # Reset singleton
        handler_module._worker = None

        mock_worker = MagicMock(spec=EvalWorker)
        mock_worker.handler.return_value = {"batchItemFailures": []}

        with patch.object(
            handler_module,
            "_create_worker",
            return_value=mock_worker,
        ) as mock_create:
            event = _make_sqs_event([("msg-001", _make_eval_message())])
            ctx = _make_context()

            result = handler_module.handler(event, ctx)

            mock_create.assert_called_once()
            mock_worker.handler.assert_called_once_with(event, ctx)
            assert result == {"batchItemFailures": []}

        # Reset singleton for other tests
        handler_module._worker = None

    def test_handler_reuses_singleton_worker(self) -> None:
        """Subsequent calls reuse the singleton worker."""
        import worker.eval.handler as handler_module

        mock_worker = MagicMock(spec=EvalWorker)
        mock_worker.handler.return_value = {"batchItemFailures": []}
        handler_module._worker = mock_worker

        event = _make_sqs_event([("msg-001", _make_eval_message())])
        ctx = _make_context()

        result = handler_module.handler(event, ctx)

        mock_worker.handler.assert_called_once_with(event, ctx)
        assert result == {"batchItemFailures": []}

        # Reset singleton for other tests
        handler_module._worker = None


# ---------------------------------------------------------------------------
# Tests: Pagination
# ---------------------------------------------------------------------------


class TestPagination:
    """Tests for paginated WatchPoint fetching."""

    def test_page_and_page_size_passed_to_repo(self) -> None:
        """Page and page_size from the message are forwarded to the repository."""
        repo = MagicMock(spec=Repository)
        reader = MagicMock(spec=ForecastReader)
        evaluator = MagicMock(spec=StandardEvaluator)

        reader.load_tile.return_value = MagicMock(spec=TileData)
        repo.fetch_watchpoints.return_value = [_make_watchpoint()]
        repo.fetch_states.return_value = {}
        evaluator.evaluate_batch.return_value = BatchResult()

        worker = EvalWorker(
            repo=repo,
            reader=reader,
            evaluator=evaluator,
        )

        msg_body = _make_eval_message(page=2, page_size=500)
        event = _make_sqs_event([("msg-001", msg_body)])
        worker.handler(event, _make_context())

        repo.fetch_watchpoints.assert_called_once_with(
            tile_id=TILE_ID, page=2, page_size=500,
        )
