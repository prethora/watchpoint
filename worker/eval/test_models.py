"""
Tests for Eval Worker domain models.

Validates:
1. JSON serialization round-trips produce correct keys (snake_case).
2. Field names exactly match the Go struct JSON tags from ``internal/types/domain.go``.
3. Default values are applied correctly.
4. Enum values match Go constants.
5. Optional fields serialize/deserialize with ``None`` handling.
"""

from __future__ import annotations

import json
from datetime import datetime, timezone
from typing import Any

from pydantic import BaseModel

from worker.eval.models import (
    Channel,
    Condition,
    ConditionLogic,
    ConditionResult,
    EvalAction,
    EvalMessage,
    EvaluationState,
    EventType,
    ForecastSnapshot,
    ForecastType,
    Location,
    LocationSnapshot,
    MonitorConfig,
    MonitorSummary,
    NotificationEvent,
    NotificationOrdering,
    NotificationPayload,
    OrderingMetadata,
    Preferences,
    SeenThreat,
    Status,
    TimeRange,
    TimeWindow,
    UrgencyLevel,
    WatchPoint,
)


# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

NOW = datetime(2026, 2, 6, 12, 0, 0, tzinfo=timezone.utc)
LATER = datetime(2026, 2, 7, 12, 0, 0, tzinfo=timezone.utc)


def _round_trip(model_instance: BaseModel) -> dict[str, Any]:
    """Serialize to JSON string and parse back to dict for key inspection."""
    json_str = model_instance.model_dump_json()
    result: dict[str, Any] = json.loads(json_str)
    return result


# ---------------------------------------------------------------------------
# Enum Tests
# ---------------------------------------------------------------------------


class TestEnums:
    """Verify enum values match Go constants exactly."""

    def test_forecast_type_values(self) -> None:
        assert ForecastType.MEDIUM_RANGE.value == "medium_range"
        assert ForecastType.NOWCAST.value == "nowcast"

    def test_eval_action_values(self) -> None:
        assert EvalAction.EVALUATE.value == "evaluate"
        assert EvalAction.GENERATE_SUMMARY.value == "generate_summary"

    def test_condition_logic_values(self) -> None:
        assert ConditionLogic.ANY.value == "ANY"
        assert ConditionLogic.ALL.value == "ALL"

    def test_status_values(self) -> None:
        assert Status.ACTIVE.value == "active"
        assert Status.PAUSED.value == "paused"
        assert Status.ARCHIVED.value == "archived"

    def test_event_type_values(self) -> None:
        assert EventType.THRESHOLD_CROSSED.value == "threshold_crossed"
        assert EventType.THRESHOLD_CLEARED.value == "threshold_cleared"
        assert EventType.FORECAST_CHANGED.value == "forecast_changed"
        assert EventType.IMMINENT_ALERT.value == "imminent_alert"
        assert EventType.MONITOR_DIGEST.value == "monitor_digest"
        assert EventType.SYSTEM_ALERT.value == "system_alert"
        assert EventType.BILLING_WARNING.value == "billing_warning"
        assert EventType.BILLING_RECEIPT.value == "billing_receipt"

    def test_urgency_level_values(self) -> None:
        assert UrgencyLevel.ROUTINE.value == "routine"
        assert UrgencyLevel.WATCH.value == "watch"
        assert UrgencyLevel.WARNING.value == "warning"
        assert UrgencyLevel.CRITICAL.value == "critical"


# ---------------------------------------------------------------------------
# EvalMessage Tests
# ---------------------------------------------------------------------------


class TestEvalMessage:
    """Test EvalMessage serialization matches Go EvalMessage JSON tags."""

    def _make_eval_message(self, **overrides: Any) -> EvalMessage:
        defaults: dict[str, Any] = {
            "batch_id": "batch_abc123",
            "trace_id": "trace_xyz789",
            "forecast_type": ForecastType.MEDIUM_RANGE,
            "run_timestamp": NOW,
            "tile_id": "tile_42_87",
            "page": 0,
            "page_size": 500,
            "total_items": 1200,
        }
        defaults.update(overrides)
        return EvalMessage(**defaults)

    def test_json_keys_match_go_struct_tags(self) -> None:
        msg = self._make_eval_message()
        data = _round_trip(msg)

        expected_keys = {
            "batch_id",
            "trace_id",
            "forecast_type",
            "run_timestamp",
            "tile_id",
            "page",
            "page_size",
            "total_items",
            "action",
            "specific_watchpoint_ids",
        }
        assert set(data.keys()) == expected_keys

    def test_default_action_is_evaluate(self) -> None:
        msg = self._make_eval_message()
        assert msg.action == EvalAction.EVALUATE
        data = _round_trip(msg)
        assert data["action"] == "evaluate"

    def test_generate_summary_action(self) -> None:
        msg = self._make_eval_message(action=EvalAction.GENERATE_SUMMARY)
        data = _round_trip(msg)
        assert data["action"] == "generate_summary"

    def test_specific_watchpoint_ids_default_empty(self) -> None:
        msg = self._make_eval_message()
        assert msg.specific_watchpoint_ids == []
        data = _round_trip(msg)
        assert data["specific_watchpoint_ids"] == []

    def test_specific_watchpoint_ids_populated(self) -> None:
        msg = self._make_eval_message(
            specific_watchpoint_ids=["wp_abc", "wp_def"]
        )
        data = _round_trip(msg)
        assert data["specific_watchpoint_ids"] == ["wp_abc", "wp_def"]

    def test_deserialization_from_go_json(self) -> None:
        """Simulate receiving a JSON payload as Go would produce it."""
        go_json = json.dumps(
            {
                "batch_id": "batch_001",
                "trace_id": "trace_001",
                "forecast_type": "medium_range",
                "run_timestamp": "2026-02-06T12:00:00Z",
                "tile_id": "tile_10_20",
                "page": 1,
                "page_size": 500,
                "total_items": 1500,
                "action": "evaluate",
                "specific_watchpoint_ids": ["wp_123"],
            }
        )
        msg = EvalMessage.model_validate_json(go_json)
        assert msg.batch_id == "batch_001"
        assert msg.forecast_type == ForecastType.MEDIUM_RANGE
        assert msg.action == EvalAction.EVALUATE
        assert msg.specific_watchpoint_ids == ["wp_123"]
        assert msg.page == 1

    def test_deserialization_without_optional_fields(self) -> None:
        """Go may omit fields with 'omitempty' if they have zero values."""
        go_json = json.dumps(
            {
                "batch_id": "batch_002",
                "trace_id": "trace_002",
                "forecast_type": "nowcast",
                "run_timestamp": "2026-02-06T12:00:00Z",
                "tile_id": "tile_5_10",
                "page": 0,
                "page_size": 0,
                "total_items": 0,
            }
        )
        msg = EvalMessage.model_validate_json(go_json)
        assert msg.action == EvalAction.EVALUATE
        assert msg.specific_watchpoint_ids == []
        assert msg.forecast_type == ForecastType.NOWCAST


# ---------------------------------------------------------------------------
# NotificationEvent Tests
# ---------------------------------------------------------------------------


class TestNotificationEvent:
    """Test NotificationEvent serialization."""

    def _make_notification_event(self) -> NotificationEvent:
        return NotificationEvent(
            id="notif_abc123",
            watchpoint_id="wp_001",
            organization_id="org_001",
            trace_id="trace_xyz",
            event_type="threshold_crossed",
            urgency="critical",
            payload={"temperature_c": 42.5},
            ordering=NotificationOrdering(
                event_sequence=5,
                forecast_timestamp=NOW,
                evaluation_timestamp=NOW,
            ),
            test_mode=False,
            created_at=NOW,
        )

    def test_json_keys(self) -> None:
        evt = self._make_notification_event()
        data = _round_trip(evt)
        expected_keys = {
            "id",
            "watchpoint_id",
            "organization_id",
            "trace_id",
            "event_type",
            "urgency",
            "payload",
            "ordering",
            "test_mode",
            "created_at",
        }
        assert set(data.keys()) == expected_keys

    def test_ordering_keys(self) -> None:
        evt = self._make_notification_event()
        data = _round_trip(evt)
        ordering = data["ordering"]
        expected_ordering_keys = {
            "event_sequence",
            "forecast_timestamp",
            "evaluation_timestamp",
        }
        assert set(ordering.keys()) == expected_ordering_keys

    def test_test_mode_defaults_false(self) -> None:
        evt = NotificationEvent(
            id="notif_test",
            watchpoint_id="wp_002",
            organization_id="org_002",
            trace_id="trace_test",
            event_type="threshold_crossed",
            urgency="warning",
            payload={},
            ordering=NotificationOrdering(
                event_sequence=1,
                forecast_timestamp=NOW,
                evaluation_timestamp=NOW,
            ),
        )
        assert evt.test_mode is False

    def test_created_at_auto_populated(self) -> None:
        evt = NotificationEvent(
            id="notif_auto",
            watchpoint_id="wp_003",
            organization_id="org_003",
            trace_id="trace_auto",
            event_type="threshold_crossed",
            urgency="routine",
            payload={},
            ordering=NotificationOrdering(
                event_sequence=1,
                forecast_timestamp=NOW,
                evaluation_timestamp=NOW,
            ),
        )
        assert evt.created_at is not None


# ---------------------------------------------------------------------------
# Condition Tests
# ---------------------------------------------------------------------------


class TestCondition:
    """Test Condition model matches Go Condition struct."""

    def test_json_keys(self) -> None:
        c = Condition(
            variable="temperature_c",
            operator=">",
            threshold=[35.0],
            unit="celsius",
        )
        data = _round_trip(c)
        assert set(data.keys()) == {"variable", "operator", "threshold", "unit"}

    def test_between_operator_threshold(self) -> None:
        c = Condition(
            variable="temperature_c",
            operator="between",
            threshold=[15.0, 25.0],
            unit="celsius",
        )
        data = _round_trip(c)
        assert data["threshold"] == [15.0, 25.0]
        assert data["operator"] == "between"

    def test_deserialization_from_go_json(self) -> None:
        go_json = '{"variable":"wind_speed_kmh","operator":">=","threshold":[60.0],"unit":"kmh"}'
        c = Condition.model_validate_json(go_json)
        assert c.variable == "wind_speed_kmh"
        assert c.operator == ">="
        assert c.threshold == [60.0]
        assert c.unit == "kmh"


# ---------------------------------------------------------------------------
# WatchPoint Tests
# ---------------------------------------------------------------------------


class TestWatchPoint:
    """Test WatchPoint model matches Go WatchPoint struct."""

    def _make_watchpoint(self, **overrides: Any) -> WatchPoint:
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
            "condition_logic": ConditionLogic.ANY,
            "config_version": 1,
            "test_mode": False,
        }
        defaults.update(overrides)
        return WatchPoint(**defaults)

    def test_json_keys_core(self) -> None:
        wp = self._make_watchpoint()
        data = _round_trip(wp)
        # Verify critical keys exist and are snake_case
        for key in [
            "id",
            "organization_id",
            "name",
            "location",
            "timezone",
            "conditions",
            "condition_logic",
            "config_version",
            "test_mode",
            "status",
        ]:
            assert key in data, f"Missing key: {key}"

    def test_monitor_config_present(self) -> None:
        wp = self._make_watchpoint(
            monitor_config=MonitorConfig(
                window_hours=24,
                active_hours=[[6, 18]],
                active_days=[1, 2, 3, 4, 5],
            )
        )
        data = _round_trip(wp)
        mc = data["monitor_config"]
        assert mc is not None
        assert mc["window_hours"] == 24
        assert mc["active_hours"] == [[6, 18]]
        assert mc["active_days"] == [1, 2, 3, 4, 5]

    def test_monitor_config_absent(self) -> None:
        wp = self._make_watchpoint()
        data = _round_trip(wp)
        assert data["monitor_config"] is None

    def test_channels_serialization(self) -> None:
        wp = self._make_watchpoint(
            channels=[
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
            ]
        )
        data = _round_trip(wp)
        assert len(data["channels"]) == 2
        assert data["channels"][0]["type"] == "webhook"
        assert data["channels"][0]["config"]["url"] == "https://example.com/hook"

    def test_time_window_serialization(self) -> None:
        wp = self._make_watchpoint(
            time_window=TimeWindow(start=NOW, end=LATER),
        )
        data = _round_trip(wp)
        tw = data["time_window"]
        assert tw is not None
        assert "start" in tw
        assert "end" in tw

    def test_deserialization_from_go_json(self) -> None:
        go_json = json.dumps(
            {
                "id": "wp_go_test",
                "organization_id": "org_go",
                "name": "Go WatchPoint",
                "location": {"lat": 34.0522, "lon": -118.2437, "display_name": "LA"},
                "timezone": "America/Los_Angeles",
                "conditions": [
                    {
                        "variable": "precipitation_probability",
                        "operator": ">=",
                        "threshold": [70.0],
                        "unit": "percent",
                    }
                ],
                "condition_logic": "ALL",
                "status": "active",
                "test_mode": True,
                "config_version": 3,
                "tags": ["weather", "alert"],
                "channels": [],
            }
        )
        wp = WatchPoint.model_validate_json(go_json)
        assert wp.id == "wp_go_test"
        assert wp.condition_logic == ConditionLogic.ALL
        assert wp.test_mode is True
        assert wp.config_version == 3
        assert wp.tags == ["weather", "alert"]
        assert len(wp.conditions) == 1
        assert wp.conditions[0].variable == "precipitation_probability"


# ---------------------------------------------------------------------------
# EvaluationState Tests
# ---------------------------------------------------------------------------


class TestEvaluationState:
    """Test EvaluationState model matches DB schema."""

    def _make_state(self, **overrides: Any) -> EvaluationState:
        defaults: dict[str, Any] = {
            "watchpoint_id": "wp_001",
            "config_version": 1,
        }
        defaults.update(overrides)
        return EvaluationState(**defaults)

    def test_json_keys_match_db_columns(self) -> None:
        state = self._make_state(
            last_evaluated_at=NOW,
            last_forecast_run=NOW,
            previous_trigger_state=True,
            trigger_value=42.5,
            escalation_level=2,
            last_escalated_at=NOW,
            last_notified_at=NOW,
            event_sequence=10,
            seen_threats=[
                SeenThreat(start=NOW, end=LATER, type="precipitation"),
            ],
        )
        data = _round_trip(state)

        # Verify all DB column names are present as JSON keys
        for key in [
            "watchpoint_id",
            "config_version",
            "last_evaluated_at",
            "last_forecast_run",
            "previous_trigger_state",
            "trigger_value",
            "escalation_level",
            "last_escalated_at",
            "last_notified_at",
            "event_sequence",
            "seen_threats",
        ]:
            assert key in data, f"Missing key: {key}"

    def test_default_values(self) -> None:
        state = self._make_state()
        assert state.previous_trigger_state is False
        assert state.escalation_level == 0
        assert state.event_sequence == 0
        assert state.seen_threats == []
        assert state.last_evaluated_at is None
        assert state.last_forecast_run is None
        assert state.trigger_value is None

    def test_seen_threats_serialization(self) -> None:
        state = self._make_state(
            seen_threats=[
                SeenThreat(start=NOW, end=LATER, type="wind"),
                SeenThreat(start=NOW, end=LATER, type="precipitation"),
            ]
        )
        data = _round_trip(state)
        threats = data["seen_threats"]
        assert len(threats) == 2
        assert threats[0]["type"] == "wind"
        assert "start" in threats[0]
        assert "end" in threats[0]

    def test_trigger_value_nullable(self) -> None:
        state = self._make_state(trigger_value=None)
        data = _round_trip(state)
        assert data["trigger_value"] is None

        state_with_value = self._make_state(trigger_value=85.3)
        data_with_value = _round_trip(state_with_value)
        assert data_with_value["trigger_value"] == 85.3

    def test_last_forecast_summary_nullable(self) -> None:
        state = self._make_state()
        data = _round_trip(state)
        assert data["last_forecast_summary"] is None

    def test_digest_fields(self) -> None:
        state = self._make_state(
            last_digest_sent_at=NOW,
            last_digest_content={"key": "value"},
        )
        data = _round_trip(state)
        assert data["last_digest_sent_at"] is not None
        assert data["last_digest_content"] == {"key": "value"}


# ---------------------------------------------------------------------------
# MonitorSummary Tests
# ---------------------------------------------------------------------------


class TestMonitorSummary:
    """Test MonitorSummary serialization matches Go MonitorSummary."""

    def test_json_keys(self) -> None:
        ms = MonitorSummary(
            window_start=NOW,
            window_end=LATER,
            max_values={"precipitation_probability": 80.0, "temperature_c": 35.0},
            triggered_periods=[TimeRange(start=NOW, end=LATER)],
        )
        data = _round_trip(ms)
        expected_keys = {
            "window_start",
            "window_end",
            "max_values",
            "triggered_periods",
        }
        assert set(data.keys()) == expected_keys

    def test_triggered_periods_structure(self) -> None:
        ms = MonitorSummary(
            window_start=NOW,
            window_end=LATER,
            max_values={},
            triggered_periods=[
                TimeRange(start=NOW, end=LATER),
            ],
        )
        data = _round_trip(ms)
        periods = data["triggered_periods"]
        assert len(periods) == 1
        assert set(periods[0].keys()) == {"start", "end"}

    def test_empty_defaults(self) -> None:
        ms = MonitorSummary(window_start=NOW, window_end=LATER)
        assert ms.max_values == {}
        assert ms.triggered_periods == []


# ---------------------------------------------------------------------------
# Supporting Types Tests
# ---------------------------------------------------------------------------


class TestForecastSnapshot:
    """Test ForecastSnapshot matches Go ForecastSnapshot JSON tags."""

    def test_json_keys_match_go_tags(self) -> None:
        fs = ForecastSnapshot(
            precipitation_probability=75.0,
            precipitation_mm=12.5,
            temperature_c=28.0,
            wind_speed_kmh=45.0,
            humidity_percent=65.0,
        )
        data = _round_trip(fs)
        expected_keys = {
            "precipitation_probability",
            "precipitation_mm",
            "temperature_c",
            "wind_speed_kmh",
            "humidity_percent",
        }
        assert set(data.keys()) == expected_keys


class TestConditionResult:
    """Test ConditionResult matches Go ConditionResult JSON tags."""

    def test_json_keys(self) -> None:
        cr = ConditionResult(
            variable="temperature_c",
            operator=">",
            threshold=[35.0],
            actual_value=42.5,
            previous_value=38.0,
            matched=True,
        )
        data = _round_trip(cr)
        expected_keys = {
            "variable",
            "operator",
            "threshold",
            "actual_value",
            "previous_value",
            "matched",
        }
        assert set(data.keys()) == expected_keys

    def test_previous_value_nullable(self) -> None:
        cr = ConditionResult(
            variable="wind_speed_kmh",
            operator=">=",
            threshold=[60.0],
            actual_value=75.0,
            matched=True,
        )
        data = _round_trip(cr)
        assert data["previous_value"] is None


class TestOrderingMetadata:
    """Test OrderingMetadata matches Go OrderingMetadata JSON tags."""

    def test_json_keys(self) -> None:
        om = OrderingMetadata(
            event_sequence=5,
            forecast_timestamp=NOW,
            eval_timestamp=NOW,
        )
        data = _round_trip(om)
        expected_keys = {"event_sequence", "forecast_timestamp", "eval_timestamp"}
        assert set(data.keys()) == expected_keys


class TestNotificationPayload:
    """Test NotificationPayload matches Go NotificationPayload JSON tags."""

    def test_json_keys(self) -> None:
        np = NotificationPayload(
            notification_id="notif_001",
            watchpoint_id="wp_001",
            watchpoint_name="Test WP",
            timezone="America/New_York",
            event_type="threshold_crossed",
            triggered_at=NOW,
            location=LocationSnapshot(lat=40.7, lon=-74.0, display_name="NYC"),
            forecast_snapshot=ForecastSnapshot(
                precipitation_probability=80.0,
                precipitation_mm=15.0,
                temperature_c=30.0,
                wind_speed_kmh=50.0,
                humidity_percent=70.0,
            ),
            conditions_evaluated=[
                ConditionResult(
                    variable="temperature_c",
                    operator=">",
                    threshold=[35.0],
                    actual_value=42.5,
                    matched=True,
                ),
            ],
            urgency="critical",
            source_model="medium_range",
            ordering=OrderingMetadata(
                event_sequence=3,
                forecast_timestamp=NOW,
                eval_timestamp=NOW,
            ),
        )
        data = _round_trip(np)

        # Verify top-level keys match Go struct JSON tags
        for key in [
            "notification_id",
            "watchpoint_id",
            "watchpoint_name",
            "timezone",
            "event_type",
            "triggered_at",
            "location",
            "forecast_snapshot",
            "conditions_evaluated",
            "urgency",
            "source_model",
            "ordering",
        ]:
            assert key in data, f"Missing key: {key}"

    def test_location_snapshot_keys(self) -> None:
        np = NotificationPayload(
            notification_id="notif_002",
            watchpoint_id="wp_002",
            watchpoint_name="Test",
            timezone="UTC",
            event_type="threshold_crossed",
            triggered_at=NOW,
            location=LocationSnapshot(lat=40.7, lon=-74.0, display_name="NYC"),
            forecast_snapshot=ForecastSnapshot(),
            conditions_evaluated=[],
            urgency="routine",
            source_model="nowcast",
            ordering=OrderingMetadata(
                event_sequence=1,
                forecast_timestamp=NOW,
                eval_timestamp=NOW,
            ),
        )
        data = _round_trip(np)
        loc = data["location"]
        assert set(loc.keys()) == {"lat", "lon", "display_name"}


class TestLocationSnapshot:
    """Test LocationSnapshot matches Go LocationSnapshot JSON tags."""

    def test_json_keys(self) -> None:
        ls = LocationSnapshot(lat=40.7, lon=-74.0, display_name="NYC")
        data = _round_trip(ls)
        assert set(data.keys()) == {"lat", "lon", "display_name"}


# ---------------------------------------------------------------------------
# Full Round-Trip Integration Test
# ---------------------------------------------------------------------------


class TestFullRoundTrip:
    """End-to-end serialization/deserialization tests simulating SQS message flow."""

    def test_eval_message_round_trip(self) -> None:
        """Serialize EvalMessage to JSON, parse back, verify equality."""
        original = EvalMessage(
            batch_id="batch_rt",
            trace_id="trace_rt",
            forecast_type=ForecastType.MEDIUM_RANGE,
            run_timestamp=NOW,
            tile_id="tile_10_20",
            page=2,
            page_size=500,
            total_items=1500,
            action=EvalAction.EVALUATE,
            specific_watchpoint_ids=["wp_a", "wp_b"],
        )
        json_str = original.model_dump_json()
        restored = EvalMessage.model_validate_json(json_str)
        assert restored == original

    def test_notification_event_round_trip(self) -> None:
        """Serialize NotificationEvent to JSON, parse back, verify equality."""
        original = NotificationEvent(
            id="notif_rt",
            watchpoint_id="wp_rt",
            organization_id="org_rt",
            trace_id="trace_rt",
            event_type="threshold_crossed",
            urgency="critical",
            payload={"temperature_c": 42.5, "matched": True},
            ordering=NotificationOrdering(
                event_sequence=7,
                forecast_timestamp=NOW,
                evaluation_timestamp=NOW,
            ),
            test_mode=True,
            created_at=NOW,
        )
        json_str = original.model_dump_json()
        restored = NotificationEvent.model_validate_json(json_str)
        assert restored == original

    def test_watchpoint_round_trip(self) -> None:
        """Serialize WatchPoint to JSON, parse back, verify equality."""
        original = WatchPoint(
            id="wp_rt",
            organization_id="org_rt",
            name="Round Trip Test",
            location=Location(lat=40.7, lon=-74.0, display_name="NYC"),
            timezone="America/New_York",
            conditions=[
                Condition(
                    variable="temperature_c",
                    operator=">",
                    threshold=[35.0],
                    unit="celsius",
                ),
                Condition(
                    variable="wind_speed_kmh",
                    operator="between",
                    threshold=[20.0, 60.0],
                    unit="kmh",
                ),
            ],
            condition_logic=ConditionLogic.ALL,
            channels=[
                Channel(
                    id="ch_rt",
                    type="webhook",
                    config={"url": "https://example.com"},
                    enabled=True,
                ),
            ],
            config_version=5,
            test_mode=True,
            tags=["production", "critical"],
        )
        json_str = original.model_dump_json()
        restored = WatchPoint.model_validate_json(json_str)
        assert restored == original

    def test_evaluation_state_round_trip(self) -> None:
        """Serialize EvaluationState to JSON, parse back, verify equality."""
        original = EvaluationState(
            watchpoint_id="wp_rt",
            config_version=3,
            last_evaluated_at=NOW,
            last_forecast_run=NOW,
            previous_trigger_state=True,
            trigger_value=85.3,
            escalation_level=2,
            last_escalated_at=NOW,
            last_notified_at=NOW,
            event_sequence=15,
            seen_threats=[
                SeenThreat(start=NOW, end=LATER, type="precipitation"),
            ],
        )
        json_str = original.model_dump_json()
        restored = EvaluationState.model_validate_json(json_str)
        assert restored == original

    def test_monitor_summary_round_trip(self) -> None:
        """Serialize MonitorSummary to JSON, parse back, verify equality."""
        original = MonitorSummary(
            window_start=NOW,
            window_end=LATER,
            max_values={"precipitation_probability": 85.0, "wind_speed_kmh": 70.0},
            triggered_periods=[
                TimeRange(start=NOW, end=LATER),
            ],
        )
        json_str = original.model_dump_json()
        restored = MonitorSummary.model_validate_json(json_str)
        assert restored == original

    def test_notification_payload_round_trip(self) -> None:
        """Serialize NotificationPayload to JSON, parse back, verify equality."""
        original = NotificationPayload(
            notification_id="notif_rt",
            watchpoint_id="wp_rt",
            watchpoint_name="Test WP",
            timezone="America/New_York",
            event_type="threshold_crossed",
            triggered_at=NOW,
            location=LocationSnapshot(lat=40.7, lon=-74.0, display_name="NYC"),
            time_window=TimeWindow(start=NOW, end=LATER),
            forecast_snapshot=ForecastSnapshot(
                precipitation_probability=80.0,
                precipitation_mm=15.0,
                temperature_c=30.0,
                wind_speed_kmh=50.0,
                humidity_percent=70.0,
            ),
            conditions_evaluated=[
                ConditionResult(
                    variable="temperature_c",
                    operator=">",
                    threshold=[35.0],
                    actual_value=42.5,
                    previous_value=38.0,
                    matched=True,
                ),
            ],
            urgency="critical",
            source_model="medium_range",
            ordering=OrderingMetadata(
                event_sequence=3,
                forecast_timestamp=NOW,
                eval_timestamp=NOW,
            ),
        )
        json_str = original.model_dump_json()
        restored = NotificationPayload.model_validate_json(json_str)
        assert restored == original
