"""
Tests for Eval Worker Repository.

Validates:
1. ``fetch_watchpoints`` SQL query construction and row-to-model mapping.
2. ``fetch_states`` SQL query and dict-based return mapping.
3. ``commit_batch`` transactional flow:
   - Stale write protection (CONC-006): out-of-order messages are rejected.
   - Channel config snapshotting into ``notification_deliveries``.
   - SQS publish of NotificationEvent messages after DB commit.
   - Event sequence logic: only incremented when notifications are inserted.
4. Edge cases: empty batches, missing channels, disabled channels.

Uses ``unittest.mock`` to mock ``psycopg`` connections/cursors and ``boto3`` SQS.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any
from unittest.mock import MagicMock, call, patch

import pytest

from worker.eval.models import (
    BatchResult,
    Channel,
    Condition,
    ConditionLogic,
    EvaluationState,
    Location,
    MonitorConfig,
    NotificationEvent,
    NotificationOrdering,
    Preferences,
    SeenThreat,
    Status,
    TimeWindow,
    WatchPoint,
)
from worker.eval.repo import (
    PostgresRepository,
    _row_to_evaluation_state,
    _row_to_watchpoint,
    _state_to_params,
    _is_fifo_queue,
)


# ---------------------------------------------------------------------------
# Fixtures / Helpers
# ---------------------------------------------------------------------------

NOW = datetime(2026, 2, 6, 12, 0, 0, tzinfo=timezone.utc)
LATER = datetime(2026, 2, 7, 12, 0, 0, tzinfo=timezone.utc)
EARLIER = datetime(2026, 2, 5, 12, 0, 0, tzinfo=timezone.utc)

CONNINFO = "postgresql://test:test@localhost:5432/testdb"
QUEUE_URL = "https://sqs.us-east-1.amazonaws.com/123456789/notification-queue"


def _make_db_watchpoint_row(**overrides: Any) -> dict[str, Any]:
    """Create a realistic database row dict for a WatchPoint."""
    defaults: dict[str, Any] = {
        "id": "wp_001",
        "organization_id": "org_001",
        "name": "Test WatchPoint",
        "location_lat": 40.7128,
        "location_lon": -74.0060,
        "location_display_name": "New York City",
        "timezone": "America/New_York",
        "tile_id": "2.6",
        "time_window_start": NOW,
        "time_window_end": LATER,
        "monitor_config": None,
        "conditions": [
            {
                "variable": "temperature_c",
                "operator": ">",
                "threshold": [35.0],
                "unit": "celsius",
            }
        ],
        "condition_logic": "ALL",
        "channels": [
            {
                "id": "ch_001",
                "type": "webhook",
                "config": {"url": "https://example.com/hook", "secret": "s3cr3t"},
                "enabled": True,
            },
            {
                "id": "ch_002",
                "type": "email",
                "config": {"address": "alerts@example.com"},
                "enabled": True,
            },
        ],
        "preferences": {"notify_on_clear": True, "notify_on_forecast_change": False},
        "template_set": "default",
        "status": "active",
        "test_mode": False,
        "tags": ["weather", "critical"],
        "config_version": 3,
        "source": "api",
        "created_at": EARLIER,
        "updated_at": NOW,
        "archived_at": None,
        "archived_reason": None,
    }
    defaults.update(overrides)
    return defaults


def _make_db_state_row(**overrides: Any) -> dict[str, Any]:
    """Create a realistic database row dict for an EvaluationState."""
    defaults: dict[str, Any] = {
        "watchpoint_id": "wp_001",
        "config_version": 3,
        "last_evaluated_at": NOW,
        "last_forecast_run": NOW,
        "previous_trigger_state": False,
        "trigger_state_changed_at": None,
        "trigger_value": None,
        "escalation_level": 0,
        "last_escalated_at": None,
        "last_notified_at": None,
        "last_notified_state": None,
        "notification_count_24h": 0,
        "last_forecast_summary": None,
        "last_digest_sent_at": None,
        "last_digest_content": None,
        "seen_threats": [],
        "event_sequence": 5,
    }
    defaults.update(overrides)
    return defaults


def _make_watchpoint(**overrides: Any) -> WatchPoint:
    """Create a WatchPoint model instance."""
    defaults: dict[str, Any] = {
        "id": "wp_001",
        "organization_id": "org_001",
        "name": "Test WatchPoint",
        "location": Location(lat=40.7128, lon=-74.0060, display_name="NYC"),
        "timezone": "America/New_York",
        "conditions": [
            Condition(
                variable="temperature_c",
                operator=">",
                threshold=[35.0],
                unit="celsius",
            )
        ],
        "condition_logic": ConditionLogic.ALL,
        "channels": [
            Channel(
                id="ch_001",
                type="webhook",
                config={"url": "https://example.com/hook", "secret": "s3cr3t"},
                enabled=True,
            ),
            Channel(
                id="ch_002",
                type="email",
                config={"address": "alerts@example.com"},
                enabled=True,
            ),
        ],
        "template_set": "default",
        "config_version": 3,
        "test_mode": False,
    }
    defaults.update(overrides)
    return WatchPoint(**defaults)


def _make_notification_event(**overrides: Any) -> NotificationEvent:
    """Create a NotificationEvent model instance."""
    defaults: dict[str, Any] = {
        "id": "notif_001",
        "watchpoint_id": "wp_001",
        "organization_id": "org_001",
        "trace_id": "trace_001",
        "event_type": "threshold_crossed",
        "urgency": "critical",
        "payload": {"temperature_c": 42.5},
        "ordering": NotificationOrdering(
            event_sequence=6,
            forecast_timestamp=NOW,
            evaluation_timestamp=NOW,
        ),
        "test_mode": False,
        "created_at": NOW,
    }
    defaults.update(overrides)
    return NotificationEvent(**defaults)


def _make_evaluation_state(**overrides: Any) -> EvaluationState:
    """Create an EvaluationState model instance."""
    defaults: dict[str, Any] = {
        "watchpoint_id": "wp_001",
        "config_version": 3,
        "last_evaluated_at": NOW,
        "last_forecast_run": NOW,
        "previous_trigger_state": True,
        "escalation_level": 0,
        "event_sequence": 6,
    }
    defaults.update(overrides)
    return EvaluationState(**defaults)


def _create_mock_repo(
    cursor_factory: Any = None,
) -> tuple[PostgresRepository, MagicMock, MagicMock, MagicMock]:
    """Create a PostgresRepository with mocked connection and SQS.

    Returns (repo, mock_connect, mock_cursor, mock_sqs_client).
    """
    mock_sqs = MagicMock()
    repo = PostgresRepository(
        conninfo=CONNINFO,
        sqs_client=mock_sqs,
        queue_url=QUEUE_URL,
    )

    # Create mock connection and cursor
    mock_cursor = MagicMock()
    mock_conn = MagicMock()
    mock_conn.cursor.return_value = mock_cursor
    mock_conn.__enter__ = MagicMock(return_value=mock_conn)
    mock_conn.__exit__ = MagicMock(return_value=False)

    # Mock the transaction context manager
    mock_tx = MagicMock()
    mock_tx.__enter__ = MagicMock(return_value=mock_tx)
    mock_tx.__exit__ = MagicMock(return_value=False)
    mock_conn.transaction.return_value = mock_tx

    return repo, mock_conn, mock_cursor, mock_sqs


# ---------------------------------------------------------------------------
# Row Mapping Tests
# ---------------------------------------------------------------------------


class TestRowToWatchpoint:
    """Test _row_to_watchpoint mapping from DB rows to models."""

    def test_basic_mapping(self) -> None:
        row = _make_db_watchpoint_row()
        wp = _row_to_watchpoint(row)

        assert wp.id == "wp_001"
        assert wp.organization_id == "org_001"
        assert wp.name == "Test WatchPoint"
        assert wp.location.lat == 40.7128
        assert wp.location.lon == -74.0060
        assert wp.location.display_name == "New York City"
        assert wp.timezone == "America/New_York"
        assert wp.tile_id == "2.6"
        assert wp.condition_logic == ConditionLogic.ALL
        assert wp.status == Status.ACTIVE
        assert wp.config_version == 3
        assert wp.test_mode is False
        assert wp.tags == ["weather", "critical"]
        assert wp.source == "api"

    def test_time_window_mapping(self) -> None:
        row = _make_db_watchpoint_row()
        wp = _row_to_watchpoint(row)

        assert wp.time_window is not None
        assert wp.time_window.start == NOW
        assert wp.time_window.end == LATER
        assert wp.monitor_config is None

    def test_monitor_config_mapping(self) -> None:
        row = _make_db_watchpoint_row(
            time_window_start=None,
            time_window_end=None,
            monitor_config={
                "window_hours": 24,
                "active_hours": [[6, 18]],
                "active_days": [1, 2, 3, 4, 5],
            },
        )
        wp = _row_to_watchpoint(row)

        assert wp.time_window is None
        assert wp.monitor_config is not None
        assert wp.monitor_config.window_hours == 24
        assert wp.monitor_config.active_hours == [[6, 18]]
        assert wp.monitor_config.active_days == [1, 2, 3, 4, 5]

    def test_conditions_mapping(self) -> None:
        row = _make_db_watchpoint_row(
            conditions=[
                {
                    "variable": "temperature_c",
                    "operator": ">",
                    "threshold": [35.0],
                    "unit": "celsius",
                },
                {
                    "variable": "wind_speed_kmh",
                    "operator": "between",
                    "threshold": [20.0, 60.0],
                    "unit": "kmh",
                },
            ]
        )
        wp = _row_to_watchpoint(row)

        assert len(wp.conditions) == 2
        assert wp.conditions[0].variable == "temperature_c"
        assert wp.conditions[0].operator == ">"
        assert wp.conditions[0].threshold == [35.0]
        assert wp.conditions[1].operator == "between"
        assert wp.conditions[1].threshold == [20.0, 60.0]

    def test_channels_mapping(self) -> None:
        row = _make_db_watchpoint_row()
        wp = _row_to_watchpoint(row)

        assert len(wp.channels) == 2
        assert wp.channels[0].id == "ch_001"
        assert wp.channels[0].type == "webhook"
        assert wp.channels[0].config["url"] == "https://example.com/hook"
        assert wp.channels[0].config["secret"] == "s3cr3t"
        assert wp.channels[0].enabled is True
        assert wp.channels[1].type == "email"

    def test_preferences_mapping(self) -> None:
        row = _make_db_watchpoint_row()
        wp = _row_to_watchpoint(row)

        assert wp.preferences is not None
        assert wp.preferences.notify_on_clear is True
        assert wp.preferences.notify_on_forecast_change is False

    def test_null_preferences(self) -> None:
        row = _make_db_watchpoint_row(preferences=None)
        wp = _row_to_watchpoint(row)
        assert wp.preferences is None

    def test_json_string_conditions(self) -> None:
        """Handle JSONB columns that arrive as strings (some drivers)."""
        row = _make_db_watchpoint_row(
            conditions=json.dumps(
                [
                    {
                        "variable": "temperature_c",
                        "operator": ">",
                        "threshold": [35.0],
                        "unit": "celsius",
                    }
                ]
            ),
            channels=json.dumps([]),
        )
        wp = _row_to_watchpoint(row)
        assert len(wp.conditions) == 1
        assert wp.conditions[0].variable == "temperature_c"

    def test_null_name_and_source(self) -> None:
        row = _make_db_watchpoint_row(name=None, source=None)
        wp = _row_to_watchpoint(row)
        assert wp.name == ""
        assert wp.source == ""

    def test_null_tags(self) -> None:
        row = _make_db_watchpoint_row(tags=None)
        wp = _row_to_watchpoint(row)
        assert wp.tags == []


class TestRowToEvaluationState:
    """Test _row_to_evaluation_state mapping."""

    def test_basic_mapping(self) -> None:
        row = _make_db_state_row()
        state = _row_to_evaluation_state(row)

        assert state.watchpoint_id == "wp_001"
        assert state.config_version == 3
        assert state.last_evaluated_at == NOW
        assert state.last_forecast_run == NOW
        assert state.previous_trigger_state is False
        assert state.escalation_level == 0
        assert state.event_sequence == 5
        assert state.seen_threats == []

    def test_seen_threats_mapping(self) -> None:
        row = _make_db_state_row(
            seen_threats=[
                {
                    "start": "2026-02-06T10:00:00Z",
                    "end": "2026-02-06T14:00:00Z",
                    "type": "precipitation",
                },
            ]
        )
        state = _row_to_evaluation_state(row)

        assert len(state.seen_threats) == 1
        assert state.seen_threats[0].type == "precipitation"

    def test_json_string_seen_threats(self) -> None:
        """Handle seen_threats as JSON string."""
        row = _make_db_state_row(
            seen_threats=json.dumps(
                [
                    {
                        "start": "2026-02-06T10:00:00Z",
                        "end": "2026-02-06T14:00:00Z",
                        "type": "wind",
                    },
                ]
            )
        )
        state = _row_to_evaluation_state(row)
        assert len(state.seen_threats) == 1
        assert state.seen_threats[0].type == "wind"

    def test_null_seen_threats(self) -> None:
        row = _make_db_state_row(seen_threats=None)
        state = _row_to_evaluation_state(row)
        assert state.seen_threats == []

    def test_nullable_fields(self) -> None:
        row = _make_db_state_row(
            trigger_value=85.3,
            last_escalated_at=NOW,
            last_notified_at=NOW,
            last_notified_state="threshold_crossed",
            last_forecast_summary={"key": "value"},
        )
        state = _row_to_evaluation_state(row)

        assert state.trigger_value == 85.3
        assert state.last_escalated_at == NOW
        assert state.last_notified_at == NOW
        assert state.last_notified_state == "threshold_crossed"
        assert state.last_forecast_summary == {"key": "value"}

    def test_json_string_summary(self) -> None:
        row = _make_db_state_row(
            last_forecast_summary=json.dumps({"key": "value"})
        )
        state = _row_to_evaluation_state(row)
        assert state.last_forecast_summary == {"key": "value"}


class TestStateToParams:
    """Test _state_to_params serialization."""

    def test_basic_serialization(self) -> None:
        state = _make_evaluation_state()
        params = _state_to_params(state, NOW)

        assert params["watchpoint_id"] == "wp_001"
        assert params["config_version"] == 3
        assert params["msg_timestamp"] == NOW
        assert params["event_sequence"] == 6

    def test_seen_threats_serialized_to_json_string(self) -> None:
        state = _make_evaluation_state(
            seen_threats=[SeenThreat(start=NOW, end=LATER, type="wind")]
        )
        params = _state_to_params(state, NOW)

        seen_threats = json.loads(params["seen_threats"])
        assert len(seen_threats) == 1
        assert seen_threats[0]["type"] == "wind"

    def test_null_forecast_summary(self) -> None:
        state = _make_evaluation_state(last_forecast_summary=None)
        params = _state_to_params(state, NOW)
        assert params["last_forecast_summary"] is None

    def test_non_null_forecast_summary(self) -> None:
        state = _make_evaluation_state(
            last_forecast_summary={"temp": 25.0}
        )
        params = _state_to_params(state, NOW)
        summary = json.loads(params["last_forecast_summary"])
        assert summary == {"temp": 25.0}


class TestIsFifoQueue:
    """Test FIFO queue detection."""

    def test_fifo_queue(self) -> None:
        assert _is_fifo_queue("https://sqs.us-east-1.amazonaws.com/123/queue.fifo") is True

    def test_standard_queue(self) -> None:
        assert _is_fifo_queue("https://sqs.us-east-1.amazonaws.com/123/queue") is False

    def test_localstack_queue(self) -> None:
        assert _is_fifo_queue("http://localhost:4566/000000000000/notification-queue") is False


# ---------------------------------------------------------------------------
# PostgresRepository.fetch_watchpoints Tests
# ---------------------------------------------------------------------------


class TestFetchWatchpoints:
    """Test fetch_watchpoints query and result mapping."""

    @patch("worker.eval.repo.psycopg.connect")
    def test_fetches_all_when_page_size_zero(self, mock_connect: MagicMock) -> None:
        """When page_size=0, fetch all WPs without LIMIT/OFFSET."""
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = [_make_db_watchpoint_row()]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_watchpoints("2.6", page=0, page_size=0)

        assert len(result) == 1
        assert result[0].id == "wp_001"

        # Verify no LIMIT/OFFSET in the SQL
        executed_sql = mock_cursor.execute.call_args[0][0]
        assert "LIMIT" not in executed_sql
        assert "OFFSET" not in executed_sql

        # Verify params
        executed_params = mock_cursor.execute.call_args[0][1]
        assert executed_params == ("2.6",)

    @patch("worker.eval.repo.psycopg.connect")
    def test_fetches_paginated(self, mock_connect: MagicMock) -> None:
        """When page_size>0, use LIMIT/OFFSET."""
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = [
            _make_db_watchpoint_row(id="wp_501"),
        ]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_watchpoints("3.4", page=1, page_size=500)

        assert len(result) == 1
        assert result[0].id == "wp_501"

        # Verify LIMIT and OFFSET in SQL
        executed_sql = mock_cursor.execute.call_args[0][0]
        assert "LIMIT" in executed_sql
        assert "OFFSET" in executed_sql

        # Verify params: tile_id, page_size (LIMIT), offset (page * page_size)
        executed_params = mock_cursor.execute.call_args[0][1]
        assert executed_params == ("3.4", 500, 500)  # page=1 * page_size=500 = offset 500

    @patch("worker.eval.repo.psycopg.connect")
    def test_returns_empty_list_for_no_results(self, mock_connect: MagicMock) -> None:
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = []

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_watchpoints("0.0", page=0, page_size=0)

        assert result == []

    @patch("worker.eval.repo.psycopg.connect")
    def test_sql_filters_active_non_deleted(self, mock_connect: MagicMock) -> None:
        """Verify the SQL contains the required filters."""
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = []

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        repo.fetch_watchpoints("2.6", page=0, page_size=0)

        executed_sql = mock_cursor.execute.call_args[0][0]
        assert "status = 'active'" in executed_sql
        assert "deleted_at IS NULL" in executed_sql
        assert "ORDER BY id" in executed_sql


# ---------------------------------------------------------------------------
# PostgresRepository.fetch_states Tests
# ---------------------------------------------------------------------------


class TestFetchStates:
    """Test fetch_states query and mapping."""

    @patch("worker.eval.repo.psycopg.connect")
    def test_fetches_states_for_ids(self, mock_connect: MagicMock) -> None:
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = [
            _make_db_state_row(watchpoint_id="wp_001", event_sequence=5),
            _make_db_state_row(watchpoint_id="wp_002", event_sequence=10),
        ]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_states(["wp_001", "wp_002"])

        assert len(result) == 2
        assert "wp_001" in result
        assert "wp_002" in result
        assert result["wp_001"].event_sequence == 5
        assert result["wp_002"].event_sequence == 10

        # Verify ANY(%s) is used for bulk fetch
        executed_sql = mock_cursor.execute.call_args[0][0]
        assert "ANY" in executed_sql

    @patch("worker.eval.repo.psycopg.connect")
    def test_returns_empty_dict_for_empty_input(self, mock_connect: MagicMock) -> None:
        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_states([])

        assert result == {}
        # Should not even connect to DB
        mock_connect.assert_not_called()

    @patch("worker.eval.repo.psycopg.connect")
    def test_missing_states_not_in_result(self, mock_connect: MagicMock) -> None:
        """States that don't exist in DB are simply absent from the dict."""
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = [
            _make_db_state_row(watchpoint_id="wp_001"),
        ]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value.__enter__ = MagicMock(return_value=mock_cursor)
        mock_conn.cursor.return_value.__exit__ = MagicMock(return_value=False)
        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)
        result = repo.fetch_states(["wp_001", "wp_999"])

        assert "wp_001" in result
        assert "wp_999" not in result


# ---------------------------------------------------------------------------
# PostgresRepository.commit_batch Tests
# ---------------------------------------------------------------------------


class TestCommitBatch:
    """Test commit_batch transactional behavior."""

    @patch("worker.eval.repo.psycopg.connect")
    def test_empty_batch_is_noop(self, mock_connect: MagicMock) -> None:
        """Empty batch should not open a DB connection."""
        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        result = BatchResult(state_updates=[], notifications=[])
        repo.commit_batch(result, NOW, {})

        mock_connect.assert_not_called()
        mock_sqs.send_message.assert_not_called()

    @patch("worker.eval.repo.psycopg.connect")
    def test_state_update_only_no_notifications(self, mock_connect: MagicMock) -> None:
        """State update without notifications should update DB but not send SQS."""
        mock_cursor = MagicMock()
        # UPSERT returns the watchpoint_id (state was updated)
        mock_cursor.fetchall.return_value = [{"watchpoint_id": "wp_001"}]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state = _make_evaluation_state()
        result = BatchResult(state_updates=[state], notifications=[])

        repo.commit_batch(result, NOW, {"wp_001": _make_watchpoint()})

        # State UPSERT should be executed
        assert mock_cursor.execute.call_count == 1
        # No SQS messages
        mock_sqs.send_message.assert_not_called()

    @patch("worker.eval.repo.psycopg.connect")
    def test_stale_write_protection_skips_notification(
        self, mock_connect: MagicMock
    ) -> None:
        """CONC-006: If state update returns 0 rows (stale), skip notification."""
        mock_cursor = MagicMock()
        # UPSERT returns no rows (stale write protection triggered)
        mock_cursor.fetchall.return_value = []

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state = _make_evaluation_state()
        notif = _make_notification_event()
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, EARLIER, {"wp_001": _make_watchpoint()})

        # State UPSERT executed (1 call)
        assert mock_cursor.execute.call_count == 1

        # No notification INSERT should happen (stale)
        # No SQS message should be sent
        mock_sqs.send_message.assert_not_called()

    @patch("worker.eval.repo.uuid.uuid4")
    @patch("worker.eval.repo.psycopg.connect")
    def test_successful_commit_with_notification_and_deliveries(
        self, mock_connect: MagicMock, mock_uuid: MagicMock
    ) -> None:
        """Full commit: state update -> notification insert -> delivery inserts -> SQS."""
        mock_uuid.return_value = "test-uuid-1234"

        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],  # State UPSERT returns success
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state = _make_evaluation_state()
        notif = _make_notification_event()
        wp = _make_watchpoint()
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # Total execute calls:
        # 1 state UPSERT + 1 notification INSERT + 2 delivery INSERTs (2 channels)
        assert mock_cursor.execute.call_count == 4

        # Verify notification INSERT call
        notif_call = mock_cursor.execute.call_args_list[1]
        notif_sql = notif_call[0][0]
        assert "INSERT INTO notifications" in notif_sql
        notif_params = notif_call[0][1]
        assert notif_params[0] == "notif_001"  # id
        assert notif_params[1] == "wp_001"  # watchpoint_id
        assert notif_params[2] == "org_001"  # organization_id
        assert notif_params[3] == "threshold_crossed"  # event_type
        assert notif_params[4] == "critical"  # urgency
        assert notif_params[6] is False  # test_mode
        assert notif_params[7] == "default"  # template_set

        # Verify delivery INSERT calls contain channel config snapshots
        delivery_call_1 = mock_cursor.execute.call_args_list[2]
        delivery_params_1 = delivery_call_1[0][1]
        assert delivery_params_1[1] == "notif_001"  # notification_id
        assert delivery_params_1[2] == "webhook"  # channel_type
        # Channel config should be a JSON snapshot
        channel_snapshot_1 = json.loads(delivery_params_1[3])
        assert channel_snapshot_1["type"] == "webhook"
        assert channel_snapshot_1["config"]["url"] == "https://example.com/hook"
        assert channel_snapshot_1["config"]["secret"] == "s3cr3t"

        delivery_call_2 = mock_cursor.execute.call_args_list[3]
        delivery_params_2 = delivery_call_2[0][1]
        assert delivery_params_2[2] == "email"  # channel_type
        channel_snapshot_2 = json.loads(delivery_params_2[3])
        assert channel_snapshot_2["config"]["address"] == "alerts@example.com"

        # Verify SQS publish
        mock_sqs.send_message.assert_called_once()
        sqs_call = mock_sqs.send_message.call_args
        assert sqs_call.kwargs["QueueUrl"] == QUEUE_URL
        sqs_body = json.loads(sqs_call.kwargs["MessageBody"])
        assert sqs_body["id"] == "notif_001"
        assert sqs_body["watchpoint_id"] == "wp_001"
        # Standard queue - MessageGroupId should not be present at all
        assert "MessageGroupId" not in sqs_call.kwargs

    @patch("worker.eval.repo.psycopg.connect")
    def test_disabled_channels_not_inserted(self, mock_connect: MagicMock) -> None:
        """Disabled channels should not get delivery rows."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],  # State UPSERT
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        wp = _make_watchpoint(
            channels=[
                Channel(
                    id="ch_001",
                    type="webhook",
                    config={"url": "https://example.com"},
                    enabled=True,
                ),
                Channel(
                    id="ch_disabled",
                    type="email",
                    config={"address": "disabled@example.com"},
                    enabled=False,
                ),
            ]
        )
        state = _make_evaluation_state()
        notif = _make_notification_event()
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # 1 state UPSERT + 1 notification INSERT + 1 delivery INSERT (only enabled)
        assert mock_cursor.execute.call_count == 3

    @patch("worker.eval.repo.psycopg.connect")
    def test_multiple_watchpoints_in_batch(self, mock_connect: MagicMock) -> None:
        """Test batch with multiple WPs, one stale and one fresh."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],  # wp_001 state update succeeds
            [],  # wp_002 state update fails (stale)
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state1 = _make_evaluation_state(watchpoint_id="wp_001")
        state2 = _make_evaluation_state(watchpoint_id="wp_002")
        notif1 = _make_notification_event(
            id="notif_001", watchpoint_id="wp_001"
        )
        notif2 = _make_notification_event(
            id="notif_002", watchpoint_id="wp_002"
        )

        wp1 = _make_watchpoint(id="wp_001", channels=[])
        wp2 = _make_watchpoint(id="wp_002", channels=[])

        result = BatchResult(
            state_updates=[state1, state2],
            notifications=[notif1, notif2],
        )

        repo.commit_batch(result, NOW, {"wp_001": wp1, "wp_002": wp2})

        # 2 state UPSERTs + 1 notification INSERT (only wp_001)
        assert mock_cursor.execute.call_count == 3

        # Only wp_001's notification should be published to SQS
        assert mock_sqs.send_message.call_count == 1
        sqs_body = json.loads(
            mock_sqs.send_message.call_args.kwargs["MessageBody"]
        )
        assert sqs_body["id"] == "notif_001"

    @patch("worker.eval.repo.psycopg.connect")
    def test_sqs_failure_after_db_commit_raises(
        self, mock_connect: MagicMock
    ) -> None:
        """SQS failure after DB commit should raise (caller handles retry)."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        mock_sqs.send_message.side_effect = Exception("SQS unavailable")
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state = _make_evaluation_state()
        notif = _make_notification_event()
        wp = _make_watchpoint(channels=[])
        result = BatchResult(state_updates=[state], notifications=[notif])

        with pytest.raises(Exception, match="SQS unavailable"):
            repo.commit_batch(result, NOW, {"wp_001": wp})

    @patch("worker.eval.repo.psycopg.connect")
    def test_channel_config_snapshot_captures_full_config(
        self, mock_connect: MagicMock
    ) -> None:
        """Verify channel config snapshot includes all fields including secrets."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        wp = _make_watchpoint(
            channels=[
                Channel(
                    id="ch_webhook",
                    type="webhook",
                    config={
                        "url": "https://secure.example.com/hook",
                        "secret": "hmac_secret_key_123",
                        "headers": {"X-Custom": "value"},
                    },
                    enabled=True,
                ),
            ]
        )
        state = _make_evaluation_state()
        notif = _make_notification_event()
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # Find the delivery INSERT call (3rd call: state UPSERT, notif INSERT, delivery INSERT)
        delivery_call = mock_cursor.execute.call_args_list[2]
        delivery_params = delivery_call[0][1]
        channel_snapshot = json.loads(delivery_params[3])

        # Full channel config must be captured
        assert channel_snapshot["id"] == "ch_webhook"
        assert channel_snapshot["type"] == "webhook"
        assert channel_snapshot["enabled"] is True
        assert channel_snapshot["config"]["url"] == "https://secure.example.com/hook"
        assert channel_snapshot["config"]["secret"] == "hmac_secret_key_123"
        assert channel_snapshot["config"]["headers"]["X-Custom"] == "value"

    @patch("worker.eval.repo.psycopg.connect")
    def test_fifo_queue_sends_message_group_id(
        self, mock_connect: MagicMock
    ) -> None:
        """FIFO queues should include MessageGroupId."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        fifo_queue_url = "https://sqs.us-east-1.amazonaws.com/123/notification-queue.fifo"
        repo = PostgresRepository(CONNINFO, mock_sqs, fifo_queue_url)

        state = _make_evaluation_state()
        notif = _make_notification_event()
        wp = _make_watchpoint(channels=[])
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        sqs_call = mock_sqs.send_message.call_args
        assert sqs_call.kwargs["MessageGroupId"] == "wp_001"

    @patch("worker.eval.repo.psycopg.connect")
    def test_notification_payload_serialized_as_json(
        self, mock_connect: MagicMock
    ) -> None:
        """Notification payload JSONB column must be a JSON string."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        notif = _make_notification_event(
            payload={"temperature_c": 42.5, "conditions": [{"matched": True}]}
        )
        state = _make_evaluation_state()
        wp = _make_watchpoint(channels=[])
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # Notification INSERT is the 2nd execute call
        notif_call = mock_cursor.execute.call_args_list[1]
        payload_param = notif_call[0][1][5]  # 6th param is payload

        # Should be a valid JSON string
        parsed = json.loads(payload_param)
        assert parsed["temperature_c"] == 42.5
        assert parsed["conditions"][0]["matched"] is True

    @patch("worker.eval.repo.psycopg.connect")
    def test_test_mode_notification_preserves_flag(
        self, mock_connect: MagicMock
    ) -> None:
        """Test mode notifications should preserve test_mode=True in DB."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        notif = _make_notification_event(test_mode=True)
        state = _make_evaluation_state()
        wp = _make_watchpoint(channels=[], test_mode=True)
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # Notification INSERT: test_mode is the 7th param
        notif_call = mock_cursor.execute.call_args_list[1]
        test_mode_param = notif_call[0][1][6]
        assert test_mode_param is True

    @patch("worker.eval.repo.psycopg.connect")
    def test_upsert_sql_includes_stale_write_condition(
        self, mock_connect: MagicMock
    ) -> None:
        """Verify the UPSERT SQL contains the CONC-006 stale write protection."""
        mock_cursor = MagicMock()
        mock_cursor.fetchall.return_value = [{"watchpoint_id": "wp_001"}]

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        state = _make_evaluation_state()
        result = BatchResult(state_updates=[state], notifications=[])

        repo.commit_batch(result, NOW, {})

        executed_sql = mock_cursor.execute.call_args_list[0][0][0]
        # Must contain the conditional update clause
        assert "last_forecast_run IS NULL" in executed_sql
        assert "last_forecast_run <=" in executed_sql
        assert "RETURNING watchpoint_id" in executed_sql

    @patch("worker.eval.repo.psycopg.connect")
    def test_no_channels_means_no_delivery_inserts(
        self, mock_connect: MagicMock
    ) -> None:
        """WatchPoint with no channels should not produce delivery rows."""
        call_count = 0
        execute_results: list[list[dict]] = [
            [{"watchpoint_id": "wp_001"}],
        ]

        mock_cursor = MagicMock()

        def side_effect_fetchall() -> list[dict]:
            nonlocal call_count
            if call_count < len(execute_results):
                result = execute_results[call_count]
                call_count += 1
                return result
            return []

        mock_cursor.fetchall.side_effect = side_effect_fetchall

        mock_conn = MagicMock()
        mock_conn.__enter__ = MagicMock(return_value=mock_conn)
        mock_conn.__exit__ = MagicMock(return_value=False)
        mock_conn.cursor.return_value = mock_cursor

        mock_tx = MagicMock()
        mock_tx.__enter__ = MagicMock(return_value=mock_tx)
        mock_tx.__exit__ = MagicMock(return_value=False)
        mock_conn.transaction.return_value = mock_tx

        mock_connect.return_value = mock_conn

        mock_sqs = MagicMock()
        repo = PostgresRepository(CONNINFO, mock_sqs, QUEUE_URL)

        wp = _make_watchpoint(channels=[])
        state = _make_evaluation_state()
        notif = _make_notification_event()
        result = BatchResult(state_updates=[state], notifications=[notif])

        repo.commit_batch(result, NOW, {"wp_001": wp})

        # 1 state UPSERT + 1 notification INSERT (no delivery INSERTs)
        assert mock_cursor.execute.call_count == 2
