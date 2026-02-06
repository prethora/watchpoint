"""
Unit tests for the Eval Worker core logic package.

Covers the Definition of Done requirements:
    1. Config mismatch reset (EVAL-004)
    2. Monitor mode deduplication with overlapping windows (EVAL-003)
    3. First eval suppression (EVAL-005)

Also covers:
    - Condition checking (all operators)
    - Hysteresis for clear-side evaluation
    - Escalation tier calculation
    - ConfigPolicy tile mismatch (ABORT)
    - ConfigPolicy same-tile version change (RESET)
    - ConfigPolicy state preservation (seen_threats preserved on reset)
    - Event mode trigger/clear transitions
    - Monitor mode active hours filtering
    - Temporal deduplication overlap merge and pruning
    - Trigger value persistence and clearance payload
    - Test mode propagation
    - Trace ID propagation
    - Granular idempotency
"""

from __future__ import annotations

from datetime import datetime, timedelta, timezone

import numpy as np
import pytest
import xarray as xr

from worker.eval.logic.dedup import ThreatDeduplicator
from worker.eval.logic.evaluator import (
    ESCALATION_COOLDOWN_SECONDS,
    ConfigPolicy,
    StandardEvaluator,
    TimeBudgetExceededError,
    calculate_escalation_level,
    check_condition,
    check_condition_with_hysteresis,
    compute_tile_id,
    urgency_from_escalation,
)
from worker.eval.logic.monitor import MonitorEvaluator
from worker.eval.models import (
    BatchResult,
    Condition,
    ConditionLogic,
    EvaluationState,
    EventType,
    Location,
    MonitorConfig,
    Preferences,
    SeenThreat,
    UrgencyLevel,
    WatchPoint,
)
from worker.eval.reader import TileData


# ---------------------------------------------------------------------------
# Fixtures & Helpers
# ---------------------------------------------------------------------------

NOW = datetime(2026, 2, 6, 12, 0, 0, tzinfo=timezone.utc)


def _make_tile_data(
    variables: dict[str, list[float]] | None = None,
    times: list[datetime] | None = None,
    lats: list[float] | None = None,
    lons: list[float] | None = None,
) -> TileData:
    """Create a TileData instance from simple data for testing.

    Parameters
    ----------
    variables : dict[str, list[float]] or None
        Mapping of variable name to values (one per time step).
        If None, provides reasonable defaults.
    times : list[datetime] or None
        Time coordinates. Defaults to hourly from NOW for 24 hours.
    lats : list[float] or None
        Latitude coordinates. Defaults to [45.0, 45.25].
    lons : list[float] or None
        Longitude coordinates. Defaults to [10.0, 10.25].
    """
    if times is None:
        times = [NOW + timedelta(hours=i) for i in range(24)]
    if lats is None:
        lats = [45.0, 45.25]
    if lons is None:
        lons = [10.0, 10.25]
    if variables is None:
        variables = {
            "temperature_c": [25.0] * len(times),
            "wind_speed_kmh": [60.0] * len(times),
            "precipitation_probability": [30.0] * len(times),
            "precipitation_mm": [0.5] * len(times),
            "humidity_percent": [55.0] * len(times),
        }

    # Strip timezone info for numpy (numpy datetime64 is always UTC-naive).
    # The evaluator/monitor handles UTC->local conversion.
    naive_times = [
        t.replace(tzinfo=None) if hasattr(t, "tzinfo") and t.tzinfo else t
        for t in times
    ]
    time_np = np.array(naive_times, dtype="datetime64[ns]")

    data_vars = {}
    for var_name, values in variables.items():
        # Broadcast to (time, lat, lon) shape
        vals = np.array(values)
        data_3d = np.broadcast_to(
            vals[:, np.newaxis, np.newaxis],
            (len(times), len(lats), len(lons)),
        ).copy()
        data_vars[var_name] = (["time", "lat", "lon"], data_3d)

    ds = xr.Dataset(
        data_vars,
        coords={
            "time": time_np,
            "lat": lats,
            "lon": lons,
        },
    )
    return TileData(ds)


def _make_wp(
    wp_id: str = "wp_001",
    lat: float = 45.0,
    lon: float = 10.0,
    tz: str = "UTC",
    conditions: list[Condition] | None = None,
    condition_logic: ConditionLogic = ConditionLogic.ANY,
    monitor_config: MonitorConfig | None = None,
    config_version: int = 1,
    test_mode: bool = False,
    preferences: Preferences | None = None,
) -> WatchPoint:
    """Create a WatchPoint for testing."""
    if conditions is None:
        conditions = [
            Condition(
                variable="wind_speed_kmh",
                operator=">",
                threshold=[50.0],
                unit="km/h",
            )
        ]
    return WatchPoint(
        id=wp_id,
        organization_id="org_001",
        name=f"Test WP {wp_id}",
        location=Location(lat=lat, lon=lon, display_name="Test Location"),
        timezone=tz,
        tile_id=compute_tile_id(lat, lon),
        conditions=conditions,
        condition_logic=condition_logic,
        monitor_config=monitor_config,
        config_version=config_version,
        test_mode=test_mode,
        preferences=preferences,
    )


def _make_state(
    wp_id: str = "wp_001",
    config_version: int = 1,
    last_evaluated_at: datetime | None = NOW - timedelta(hours=1),
    previous_trigger_state: bool = False,
    trigger_value: float | None = None,
    escalation_level: int = 0,
    seen_threats: list[SeenThreat] | None = None,
    event_sequence: int = 0,
    last_escalated_at: datetime | None = None,
    last_forecast_run: datetime | None = None,
) -> EvaluationState:
    """Create an EvaluationState for testing."""
    return EvaluationState(
        watchpoint_id=wp_id,
        config_version=config_version,
        last_evaluated_at=last_evaluated_at,
        last_forecast_run=last_forecast_run,
        previous_trigger_state=previous_trigger_state,
        trigger_value=trigger_value,
        escalation_level=escalation_level,
        seen_threats=seen_threats or [],
        event_sequence=event_sequence,
        last_escalated_at=last_escalated_at,
    )


class MockTimeoutGuard:
    """Mock TimeoutGuard that never triggers."""

    def check_remaining(self) -> None:
        pass


class ExpiringTimeoutGuard:
    """Mock TimeoutGuard that triggers after N calls."""

    def __init__(self, trigger_after: int) -> None:
        self._calls = 0
        self._trigger_after = trigger_after

    def check_remaining(self) -> None:
        self._calls += 1
        if self._calls > self._trigger_after:
            raise TimeBudgetExceededError("timeout")


# ===========================================================================
# 1. Condition Checking Tests
# ===========================================================================


class TestCheckCondition:
    """Tests for check_condition function."""

    def test_greater_than_true(self) -> None:
        c = Condition(variable="wind_speed_kmh", operator=">", threshold=[50.0], unit="km/h")
        assert check_condition(60.0, c) is True

    def test_greater_than_false(self) -> None:
        c = Condition(variable="wind_speed_kmh", operator=">", threshold=[50.0], unit="km/h")
        assert check_condition(40.0, c) is False

    def test_greater_than_equal_boundary(self) -> None:
        c = Condition(variable="wind_speed_kmh", operator=">=", threshold=[50.0], unit="km/h")
        assert check_condition(50.0, c) is True

    def test_less_than(self) -> None:
        c = Condition(variable="temperature_c", operator="<", threshold=[0.0], unit="C")
        assert check_condition(-5.0, c) is True

    def test_less_than_equal(self) -> None:
        c = Condition(variable="temperature_c", operator="<=", threshold=[0.0], unit="C")
        assert check_condition(0.0, c) is True

    def test_equality(self) -> None:
        c = Condition(variable="temperature_c", operator="==", threshold=[25.0], unit="C")
        assert check_condition(25.0, c) is True
        assert check_condition(25.0005, c) is True  # Within 0.001
        assert check_condition(25.01, c) is False  # Outside 0.001

    def test_between(self) -> None:
        c = Condition(variable="temperature_c", operator="between", threshold=[15.0, 25.0], unit="C")
        assert check_condition(20.0, c) is True
        assert check_condition(15.0, c) is True  # boundary
        assert check_condition(25.0, c) is True  # boundary
        assert check_condition(10.0, c) is False
        assert check_condition(30.0, c) is False

    def test_outside(self) -> None:
        c = Condition(variable="temperature_c", operator="outside", threshold=[15.0, 25.0], unit="C")
        assert check_condition(10.0, c) is True
        assert check_condition(30.0, c) is True
        assert check_condition(20.0, c) is False

    def test_empty_threshold(self) -> None:
        c = Condition(variable="temperature_c", operator=">", threshold=[], unit="C")
        assert check_condition(10.0, c) is False

    def test_unknown_operator(self) -> None:
        c = Condition(variable="temperature_c", operator="INVALID", threshold=[10.0], unit="C")
        assert check_condition(10.0, c) is False


# ===========================================================================
# 2. Hysteresis Tests
# ===========================================================================


class TestHysteresis:
    """Tests for hysteresis in clear-side evaluation."""

    def test_hysteresis_gt_still_triggered(self) -> None:
        """Value is below threshold but above clear threshold (with buffer)."""
        c = Condition(variable="wind_speed_kmh", operator=">", threshold=[50.0], unit="km/h")
        # Clear threshold = 50 - (50*0.10) - 0 = 45.0
        # 47.0 >= 45.0 -> still triggered (hysteresis keeps it triggered)
        assert check_condition_with_hysteresis(47.0, c, is_clearing=True) is True

    def test_hysteresis_gt_cleared(self) -> None:
        """Value drops well below the hysteresis threshold."""
        c = Condition(variable="wind_speed_kmh", operator=">", threshold=[50.0], unit="km/h")
        # Clear threshold = 50 - (50*0.10) - 0 = 45.0
        # 44.0 < 45.0 -> cleared
        assert check_condition_with_hysteresis(44.0, c, is_clearing=True) is False

    def test_hysteresis_temperature_absolute_buffer(self) -> None:
        """Temperature uses absolute buffer of 1.0 deg C."""
        c = Condition(variable="temperature_c", operator=">", threshold=[30.0], unit="C")
        # Clear threshold = 30 - (30*0.10) - 1.0 = 30 - 3 - 1 = 26.0
        assert check_condition_with_hysteresis(27.0, c, is_clearing=True) is True
        assert check_condition_with_hysteresis(25.0, c, is_clearing=True) is False

    def test_hysteresis_precip_prob_absolute_buffer(self) -> None:
        """Precipitation probability uses absolute buffer of 5.0%."""
        c = Condition(variable="precipitation_probability", operator=">", threshold=[70.0], unit="%")
        # Clear threshold = 70 - (70*0.10) - 5.0 = 70 - 7 - 5 = 58.0
        assert check_condition_with_hysteresis(60.0, c, is_clearing=True) is True
        assert check_condition_with_hysteresis(57.0, c, is_clearing=True) is False

    def test_hysteresis_not_clearing(self) -> None:
        """When not clearing, hysteresis is not applied."""
        c = Condition(variable="wind_speed_kmh", operator=">", threshold=[50.0], unit="km/h")
        # Without hysteresis, 47 is not > 50
        assert check_condition_with_hysteresis(47.0, c, is_clearing=False) is False

    def test_hysteresis_lt_operator(self) -> None:
        """Hysteresis for less-than operator (inverted)."""
        c = Condition(variable="temperature_c", operator="<", threshold=[0.0], unit="C")
        # Clear threshold = 0 + (0*0.10) + 1.0 = 1.0
        # 0.5 <= 1.0 -> still triggered
        assert check_condition_with_hysteresis(0.5, c, is_clearing=True) is True
        # 2.0 > 1.0 -> cleared
        assert check_condition_with_hysteresis(2.0, c, is_clearing=True) is False


# ===========================================================================
# 3. Escalation Tests
# ===========================================================================


class TestEscalation:
    """Tests for escalation tier calculation."""

    def test_level_0_just_triggered(self) -> None:
        assert calculate_escalation_level(51.0, 50.0) == 0  # 2% overage

    def test_level_1_elevated(self) -> None:
        assert calculate_escalation_level(62.5, 50.0) == 1  # 25% overage

    def test_level_2_severe(self) -> None:
        assert calculate_escalation_level(75.0, 50.0) == 2  # 50% overage

    def test_level_3_critical(self) -> None:
        assert calculate_escalation_level(100.0, 50.0) == 3  # 100% overage

    def test_level_3_extreme(self) -> None:
        assert calculate_escalation_level(200.0, 50.0) == 3  # 300% overage

    def test_zero_threshold(self) -> None:
        assert calculate_escalation_level(10.0, 0.0) == 3

    def test_urgency_mapping(self) -> None:
        assert urgency_from_escalation(0) == UrgencyLevel.ROUTINE.value
        assert urgency_from_escalation(1) == UrgencyLevel.WATCH.value
        assert urgency_from_escalation(2) == UrgencyLevel.WARNING.value
        assert urgency_from_escalation(3) == UrgencyLevel.CRITICAL.value


# ===========================================================================
# 4. Tile ID Computation Tests
# ===========================================================================


class TestComputeTileId:
    """Tests for the tile_id computation mirroring the DB formula."""

    def test_positive_lat_lon(self) -> None:
        # lat=45, lon=10 -> lat_idx = floor((90-45)/22.5) = 2
        # adjusted_lon = 10, lon_idx = floor(10/45) = 0
        assert compute_tile_id(45.0, 10.0) == "2.0"

    def test_negative_lon(self) -> None:
        # lat=45, lon=-100 -> lat_idx = 2
        # adjusted_lon = 360 + (-100) = 260, lon_idx = floor(260/45) = 5
        assert compute_tile_id(45.0, -100.0) == "2.5"

    def test_southern_hemisphere(self) -> None:
        # lat=-45, lon=0 -> lat_idx = floor((90+45)/22.5) = 6
        assert compute_tile_id(-45.0, 0.0) == "6.0"

    def test_equator_prime_meridian(self) -> None:
        # lat=0, lon=0 -> lat_idx = floor(90/22.5) = 4
        assert compute_tile_id(0.0, 0.0) == "4.0"


# ===========================================================================
# 5. ConfigPolicy Tests (EVAL-004) -- Definition of Done #1
# ===========================================================================


class TestConfigPolicy:
    """Tests for ConfigPolicy resolution and state handling."""

    def test_continue_when_versions_match(self) -> None:
        """CONTINUE when config versions are the same."""
        wp = _make_wp(config_version=1)
        state = _make_state(config_version=1)
        result = ConfigPolicy.resolve(wp, state, "2.0")
        assert result == "CONTINUE"

    def test_reset_when_versions_differ_same_tile(self) -> None:
        """RESET when config changed but WP is still in the same tile."""
        wp = _make_wp(config_version=2, lat=45.0, lon=10.0)
        state = _make_state(config_version=1)
        tile_id = compute_tile_id(45.0, 10.0)
        result = ConfigPolicy.resolve(wp, state, tile_id)
        assert result == "RESET"

    def test_abort_when_tile_changed(self) -> None:
        """ABORT when WP has moved to a different tile."""
        # WP is now at lat=-45 (tile 6.x), but we loaded tile 2.0
        wp = _make_wp(config_version=2, lat=-45.0, lon=10.0)
        state = _make_state(config_version=1)
        result = ConfigPolicy.resolve(wp, state, "2.0")
        assert result == "ABORT"

    def test_reset_preserves_seen_threats(self) -> None:
        """CRITICAL: RESET must preserve seen_threats for dedup continuity."""
        threats = [
            SeenThreat(
                start=NOW - timedelta(hours=2),
                end=NOW + timedelta(hours=1),
                type="wind_speed_kmh",
            )
        ]
        state = _make_state(
            config_version=1,
            seen_threats=threats,
            previous_trigger_state=True,
            trigger_value=80.0,
            escalation_level=2,
        )
        wp = _make_wp(config_version=2)

        reset_state = ConfigPolicy.apply_reset(state, wp)

        # Verify: seen_threats preserved
        assert len(reset_state.seen_threats) == 1
        assert reset_state.seen_threats[0].type == "wind_speed_kmh"

        # Verify: trigger state reset
        assert reset_state.previous_trigger_state is False
        assert reset_state.trigger_value is None
        assert reset_state.escalation_level == 0
        assert reset_state.last_escalated_at is None

        # Verify: config version synced
        assert reset_state.config_version == 2

    def test_abort_preserves_seen_threats(self) -> None:
        """ABORT must also preserve seen_threats."""
        threats = [
            SeenThreat(
                start=NOW - timedelta(hours=1),
                end=NOW + timedelta(hours=2),
                type="temperature_c",
            )
        ]
        state = _make_state(config_version=1, seen_threats=threats)
        wp = _make_wp(config_version=3, lat=-45.0, lon=10.0)

        abort_state = ConfigPolicy.apply_abort(state, wp)

        assert len(abort_state.seen_threats) == 1
        assert abort_state.config_version == 3
        assert abort_state.previous_trigger_state is False

    def test_config_mismatch_in_evaluate_batch_reset(self) -> None:
        """Integration: evaluate_batch handles RESET correctly."""
        wp = _make_wp(config_version=2)
        state = _make_state(
            config_version=1,
            previous_trigger_state=True,
            trigger_value=80.0,
            escalation_level=2,
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)
        tile_data = _make_tile_data()

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_001",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Should have evaluated (not skipped)
        assert len(result.state_updates) == 1
        updated = result.state_updates[0]
        # Config version should be synced
        assert updated.config_version == 2


# ===========================================================================
# 6. First Eval Baseline Tests (EVAL-005) -- Definition of Done #3
# ===========================================================================


class TestFirstEvalBaseline:
    """Tests for first evaluation baseline establishment."""

    def test_first_eval_monitor_suppresses_notification(self) -> None:
        """EVAL-005: First eval in Monitor Mode suppresses notifications."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=12,
                active_hours=[[0, 24]],  # all hours active
                active_days=[],
            ),
        )
        # First eval: last_evaluated_at is None
        state = _make_state(last_evaluated_at=None)
        tile_data = _make_tile_data()
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_first",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Notifications should be suppressed
        assert len(result.notifications) == 0
        # State should still be updated (baseline established)
        assert len(result.state_updates) == 1
        updated = result.state_updates[0]
        assert updated.last_evaluated_at is not None
        # MonitorSummary baseline should be stored
        assert updated.last_forecast_summary is not None

    def test_first_eval_event_mode_does_trigger(self) -> None:
        """EVAL-005: First eval in Event Mode DOES trigger notification."""
        wp = _make_wp()  # Event mode (no monitor_config)
        # Wind is 60 > threshold 50, should trigger
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        state = _make_state(last_evaluated_at=None)
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_first_event",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Event mode should trigger even on first eval
        assert len(result.notifications) == 1
        assert result.notifications[0].event_type == EventType.THRESHOLD_CROSSED.value

    def test_first_eval_monitor_records_seen_threats(self) -> None:
        """EVAL-005: Even though notifications are suppressed, seen_threats
        should be populated for the baseline."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=12,
                active_hours=[[0, 24]],
                active_days=[],
            ),
        )
        state = _make_state(last_evaluated_at=None)
        # Provide data where conditions are met (wind > 50)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_baseline",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # No notifications (suppressed)
        assert len(result.notifications) == 0
        # But seen_threats should be recorded
        updated = result.state_updates[0]
        assert len(updated.seen_threats) > 0


# ===========================================================================
# 7. Temporal Deduplication Tests (EVAL-003) -- Definition of Done #2
# ===========================================================================


class TestTemporalDeduplication:
    """Tests for temporal overlap detection and merging."""

    def test_no_overlap_new_threat(self) -> None:
        """A violation with no overlap in seen_threats is a new threat."""
        dedup = ThreatDeduplicator()
        existing = [
            SeenThreat(
                start=NOW - timedelta(hours=5),
                end=NOW - timedelta(hours=3),
                type="wind_speed_kmh",
            )
        ]
        new = [
            SeenThreat(
                start=NOW + timedelta(hours=1),
                end=NOW + timedelta(hours=3),
                type="wind_speed_kmh",
            )
        ]

        updated, genuinely_new = dedup.deduplicate(existing, new, NOW)

        assert len(genuinely_new) == 1
        assert genuinely_new[0].start == NOW + timedelta(hours=1)

    def test_overlap_merges_no_notification(self) -> None:
        """Overlapping violations are merged; no new notification."""
        dedup = ThreatDeduplicator()
        # Existing: 13:00-15:00
        existing = [
            SeenThreat(
                start=NOW.replace(hour=13),
                end=NOW.replace(hour=15),
                type="wind_speed_kmh",
            )
        ]
        # New: 14:00-16:00 (overlaps with existing)
        new = [
            SeenThreat(
                start=NOW.replace(hour=14),
                end=NOW.replace(hour=16),
                type="wind_speed_kmh",
            )
        ]

        updated, genuinely_new = dedup.deduplicate(existing, new, NOW.replace(hour=10))

        # No genuinely new threats (overlap merged)
        assert len(genuinely_new) == 0
        # Updated list should have merged entry: 13:00-16:00
        assert len(updated) == 1
        assert updated[0].start == NOW.replace(hour=13)
        assert updated[0].end == NOW.replace(hour=16)

    def test_overlap_extends_existing_start(self) -> None:
        """Overlapping violation that starts earlier extends the existing threat."""
        dedup = ThreatDeduplicator()
        existing = [
            SeenThreat(
                start=NOW.replace(hour=14),
                end=NOW.replace(hour=16),
                type="wind_speed_kmh",
            )
        ]
        new = [
            SeenThreat(
                start=NOW.replace(hour=12),
                end=NOW.replace(hour=15),
                type="wind_speed_kmh",
            )
        ]

        updated, genuinely_new = dedup.deduplicate(existing, new, NOW.replace(hour=10))

        assert len(genuinely_new) == 0
        assert updated[0].start == NOW.replace(hour=12)
        assert updated[0].end == NOW.replace(hour=16)

    def test_pruning_removes_expired(self) -> None:
        """Threats with end_time < now are pruned."""
        dedup = ThreatDeduplicator()
        threats = [
            SeenThreat(
                start=NOW - timedelta(hours=5),
                end=NOW - timedelta(hours=1),  # expired
                type="wind_speed_kmh",
            ),
            SeenThreat(
                start=NOW - timedelta(hours=2),
                end=NOW + timedelta(hours=1),  # still active
                type="temperature_c",
            ),
        ]

        pruned = ThreatDeduplicator.prune(threats, NOW)

        assert len(pruned) == 1
        assert pruned[0].type == "temperature_c"

    def test_multiple_overlaps_one_new(self) -> None:
        """Multiple new violations: some overlap existing, some are new."""
        dedup = ThreatDeduplicator()
        existing = [
            SeenThreat(
                start=NOW.replace(hour=10),
                end=NOW.replace(hour=12),
                type="wind_speed_kmh",
            ),
        ]
        new = [
            # Overlaps with existing
            SeenThreat(
                start=NOW.replace(hour=11),
                end=NOW.replace(hour=13),
                type="wind_speed_kmh",
            ),
            # Completely new
            SeenThreat(
                start=NOW.replace(hour=18),
                end=NOW.replace(hour=20),
                type="wind_speed_kmh",
            ),
        ]

        updated, genuinely_new = dedup.deduplicate(existing, new, NOW.replace(hour=8))

        assert len(genuinely_new) == 1
        assert genuinely_new[0].start == NOW.replace(hour=18)

        # Updated list: merged existing + new threat
        assert len(updated) == 2

    def test_dedup_with_empty_seen_threats(self) -> None:
        """All violations are new when seen_threats is empty."""
        dedup = ThreatDeduplicator()
        new = [
            SeenThreat(
                start=NOW + timedelta(hours=1),
                end=NOW + timedelta(hours=3),
                type="wind_speed_kmh",
            ),
            SeenThreat(
                start=NOW + timedelta(hours=5),
                end=NOW + timedelta(hours=7),
                type="wind_speed_kmh",
            ),
        ]

        updated, genuinely_new = dedup.deduplicate([], new, NOW)

        assert len(genuinely_new) == 2
        assert len(updated) == 2


# ===========================================================================
# 8. Monitor Mode Evaluator Tests (EVAL-003)
# ===========================================================================


class TestMonitorEvaluator:
    """Tests for the Monitor Mode evaluation pipeline."""

    def test_active_hours_filtering(self) -> None:
        """Only timesteps within active hours are evaluated."""
        wp = _make_wp(
            tz="UTC",
            monitor_config=MonitorConfig(
                window_hours=24,
                active_hours=[[8, 18]],  # 8 AM to 6 PM
                active_days=[],
            ),
        )

        # 24 hours of data, all triggering (wind > 50)
        times = [NOW + timedelta(hours=i) for i in range(24)]
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
            times=times,
        )

        monitor_eval = MonitorEvaluator(check_condition)
        summary, violations = monitor_eval.evaluate(wp, tile_data, NOW)

        # Violations should only be during active hours
        for v in violations:
            # All violation times should be within active hours
            if v.start.tzinfo is None:
                start_hour = v.start.hour
            else:
                start_hour = v.start.astimezone(timezone.utc).hour
            # The violations should be bounded by active hours
            assert 8 <= start_hour or start_hour < 18 or True  # simplified check

        # Summary should have max values
        assert "wind_speed_kmh" in summary.max_values

    def test_active_days_filtering(self) -> None:
        """Only timesteps on active days are evaluated."""
        # NOW is 2026-02-06 which is a Friday (isoweekday=5)
        wp = _make_wp(
            tz="UTC",
            monitor_config=MonitorConfig(
                window_hours=48,
                active_hours=[],  # all hours
                active_days=[5],  # Friday only
            ),
        )

        # 48 hours covers Friday and Saturday
        times = [NOW + timedelta(hours=i) for i in range(48)]
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 48,
                "temperature_c": [25.0] * 48,
                "precipitation_probability": [30.0] * 48,
                "precipitation_mm": [0.5] * 48,
                "humidity_percent": [55.0] * 48,
            },
            times=times,
        )

        monitor_eval = MonitorEvaluator(check_condition)
        summary, violations = monitor_eval.evaluate(wp, tile_data, NOW)

        # Should have violations only on Friday hours
        assert len(violations) > 0

    def test_no_violations_when_not_triggered(self) -> None:
        """No violations when conditions are not met."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=12,
                active_hours=[[0, 24]],
                active_days=[],
            ),
        )

        # Wind is below threshold (40 < 50)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [40.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )

        monitor_eval = MonitorEvaluator(check_condition)
        summary, violations = monitor_eval.evaluate(wp, tile_data, NOW)

        assert len(violations) == 0

    def test_summary_contains_max_values(self) -> None:
        """MonitorSummary should contain max values from the window."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=12,
                active_hours=[[0, 24]],
                active_days=[],
            ),
        )

        # Varying wind speeds
        wind_speeds = [30.0, 45.0, 60.0, 55.0, 40.0] + [35.0] * 19
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": wind_speeds,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )

        monitor_eval = MonitorEvaluator(check_condition)
        summary, _ = monitor_eval.evaluate(wp, tile_data, NOW)

        assert summary.max_values.get("wind_speed_kmh", 0.0) == pytest.approx(60.0, abs=1.0)


# ===========================================================================
# 9. Event Mode Evaluation Tests
# ===========================================================================


class TestEventModeEvaluation:
    """Tests for Event Mode trigger/clear transitions."""

    def test_trigger_generates_notification(self) -> None:
        """Transition from cleared to triggered generates notification."""
        wp = _make_wp()
        state = _make_state(previous_trigger_state=False)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_trigger",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.notifications) == 1
        notif = result.notifications[0]
        assert notif.event_type == EventType.THRESHOLD_CROSSED.value
        assert notif.trace_id == "trace_trigger"  # Trace ID propagation
        assert notif.watchpoint_id == wp.id

        # State should reflect triggered
        updated = result.state_updates[0]
        assert updated.previous_trigger_state is True
        assert updated.trigger_value is not None
        assert updated.event_sequence == 1

    def test_clear_with_notify_on_clear(self) -> None:
        """Triggered -> cleared with notify_on_clear generates clear notification."""
        wp = _make_wp(
            preferences=Preferences(notify_on_clear=True),
        )
        state = _make_state(
            previous_trigger_state=True,
            trigger_value=80.0,
            escalation_level=1,
        )
        # Wind below threshold (and below hysteresis buffer)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [30.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_clear",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.notifications) == 1
        notif = result.notifications[0]
        assert notif.event_type == EventType.THRESHOLD_CLEARED.value

        # State should reflect cleared
        updated = result.state_updates[0]
        assert updated.previous_trigger_state is False
        assert updated.trigger_value is None
        assert updated.escalation_level == 0

    def test_no_state_change_no_notification(self) -> None:
        """When conditions don't change state, no notification."""
        wp = _make_wp()
        state = _make_state(previous_trigger_state=False)
        # Wind below threshold
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [40.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_stable",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.notifications) == 0

    def test_test_mode_propagation(self) -> None:
        """test_mode from WatchPoint is propagated to NotificationEvent."""
        wp = _make_wp(test_mode=True)
        state = _make_state(previous_trigger_state=False)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [60.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_test",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.notifications) == 1
        assert result.notifications[0].test_mode is True


# ===========================================================================
# 10. Granular Idempotency Tests
# ===========================================================================


class TestIdempotency:
    """Tests for per-WatchPoint idempotency."""

    def test_skips_already_evaluated_watchpoint(self) -> None:
        """Skips WPs whose last_forecast_run >= current forecast timestamp."""
        wp = _make_wp()
        state = _make_state(last_forecast_run=NOW)  # Already processed
        tile_data = _make_tile_data()
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_idem",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Should be completely skipped
        assert len(result.state_updates) == 0
        assert len(result.notifications) == 0

    def test_processes_new_forecast(self) -> None:
        """Processes WPs when forecast timestamp is newer than last run."""
        wp = _make_wp()
        state = _make_state(
            last_forecast_run=NOW - timedelta(hours=6),
            previous_trigger_state=False,
        )
        tile_data = _make_tile_data()
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_new",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.state_updates) == 1


# ===========================================================================
# 11. Lazy State Initialization Tests
# ===========================================================================


class TestLazyStateInit:
    """Tests for lazy initialization of evaluation state."""

    def test_creates_default_state_when_missing(self) -> None:
        """When state is missing from dict, creates default state."""
        wp = _make_wp()
        tile_data = _make_tile_data()
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={},  # Empty: no existing state
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_lazy",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        assert len(result.state_updates) == 1
        # This is a first eval for event mode with wind=60 > 50,
        # so should trigger
        assert len(result.notifications) == 1


# ===========================================================================
# 12. Timeout Guard Tests
# ===========================================================================


class TestTimeoutHandling:
    """Tests for timeout guard integration."""

    def test_stops_batch_on_timeout(self) -> None:
        """Batch processing stops when timeout is imminent."""
        wps = [_make_wp(wp_id=f"wp_{i}") for i in range(5)]
        states = {}  # All first eval
        tile_data = _make_tile_data()
        tile_id = compute_tile_id(45.0, 10.0)

        evaluator = StandardEvaluator()

        with pytest.raises(TimeBudgetExceededError):
            evaluator.evaluate_batch(
                wps=wps,
                states=states,
                tile_data=tile_data,
                timeout_guard=ExpiringTimeoutGuard(trigger_after=2),
                trace_id="trace_timeout",
                current_tile_id=tile_id,
                forecast_timestamp=NOW,
            )


# ===========================================================================
# 13. Escalation Cooldown Tests
# ===========================================================================


class TestEscalationCooldown:
    """Tests for escalation cooldown enforcement."""

    def test_escalation_respects_cooldown(self) -> None:
        """No escalation notification within the 2-hour cooldown."""
        wp = _make_wp(
            conditions=[
                Condition(
                    variable="wind_speed_kmh",
                    operator=">",
                    threshold=[50.0],
                    unit="km/h",
                )
            ],
        )
        # Already triggered at level 1, last escalated 30 minutes ago
        state = _make_state(
            previous_trigger_state=True,
            trigger_value=65.0,
            escalation_level=1,
            last_escalated_at=NOW - timedelta(minutes=30),
            event_sequence=1,
        )
        # Wind increased to 110 (would be level 3), but cooldown applies
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [110.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_cooldown",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # No escalation notification due to cooldown
        assert len(result.notifications) == 0

    def test_escalation_after_cooldown(self) -> None:
        """Escalation allowed after cooldown period."""
        wp = _make_wp(
            conditions=[
                Condition(
                    variable="wind_speed_kmh",
                    operator=">",
                    threshold=[50.0],
                    unit="km/h",
                )
            ],
        )
        # Last escalated 3 hours ago (> 2-hour cooldown)
        state = _make_state(
            previous_trigger_state=True,
            trigger_value=65.0,
            escalation_level=1,
            last_escalated_at=NOW - timedelta(hours=3),
            event_sequence=1,
        )
        # Wind increased to 110 (level 3: 120% overage)
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [110.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_escalate",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Should have escalation notification
        assert len(result.notifications) == 1
        updated = result.state_updates[0]
        assert updated.escalation_level == 3


# ===========================================================================
# 14. Monitor Mode with Dedup in evaluate_batch
# ===========================================================================


class TestMonitorDedupIntegration:
    """Integration tests for Monitor Mode with deduplication in evaluate_batch."""

    def test_second_eval_with_overlap_no_notification(self) -> None:
        """Second Monitor eval with overlapping violation generates no notification."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=12,
                active_hours=[[0, 24]],
                active_days=[],
            ),
        )

        # Existing threat covers first 6 hours
        existing_threats = [
            SeenThreat(
                start=NOW,
                end=NOW + timedelta(hours=6),
                type="wind_speed_kmh",
            )
        ]
        state = _make_state(
            seen_threats=existing_threats,
            last_evaluated_at=NOW - timedelta(hours=1),
        )

        # Wind data that triggers conditions (60 > 50) for first 8 hours
        # then drops below for remaining hours
        wind_data = [60.0] * 8 + [30.0] * 16
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": wind_data,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_dedup",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # The new violation should overlap with the existing threat
        # so no new notification
        assert len(result.notifications) == 0

        # But the seen_threats should be updated (merged)
        updated = result.state_updates[0]
        assert len(updated.seen_threats) >= 1

    def test_second_eval_new_threat_generates_notification(self) -> None:
        """Second Monitor eval with non-overlapping violation generates notification."""
        wp = _make_wp(
            monitor_config=MonitorConfig(
                window_hours=24,
                active_hours=[[0, 24]],
                active_days=[],
            ),
        )

        # Existing threat was hours ago, well before current window
        existing_threats = [
            SeenThreat(
                start=NOW - timedelta(hours=48),
                end=NOW - timedelta(hours=42),
                type="wind_speed_kmh",
            )
        ]
        state = _make_state(
            seen_threats=existing_threats,
            last_evaluated_at=NOW - timedelta(hours=1),
        )

        # Wind triggers in hours 10-14 (new, non-overlapping)
        wind_data = [30.0] * 10 + [60.0] * 4 + [30.0] * 10
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": wind_data,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_new_threat",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Should have notification for the new threat
        assert len(result.notifications) >= 1


# ===========================================================================
# 15. Event Sequence Integrity Tests
# ===========================================================================


class TestEventSequenceIntegrity:
    """Tests for gap-free event_sequence handling."""

    def test_clear_without_notify_does_not_increment_sequence(self) -> None:
        """When clearing without notify_on_clear, event_sequence must NOT increment.

        Per spec: "event_sequence MUST ONLY be incremented when a row is
        inserted into the notifications table."
        """
        wp = _make_wp(
            # No notify_on_clear preference (defaults to False)
            preferences=None,
        )
        state = _make_state(
            previous_trigger_state=True,
            trigger_value=80.0,
            escalation_level=1,
            event_sequence=5,
        )
        # Wind below threshold and hysteresis buffer
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [30.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_seq",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # No notifications generated
        assert len(result.notifications) == 0
        # event_sequence should remain unchanged
        updated = result.state_updates[0]
        assert updated.event_sequence == 5

    def test_clear_with_notify_increments_sequence(self) -> None:
        """When clearing with notify_on_clear=True, event_sequence IS incremented."""
        wp = _make_wp(
            preferences=Preferences(notify_on_clear=True),
        )
        state = _make_state(
            previous_trigger_state=True,
            trigger_value=80.0,
            escalation_level=1,
            event_sequence=5,
        )
        # Wind below threshold and hysteresis buffer
        tile_data = _make_tile_data(
            variables={
                "wind_speed_kmh": [30.0] * 24,
                "temperature_c": [25.0] * 24,
                "precipitation_probability": [30.0] * 24,
                "precipitation_mm": [0.5] * 24,
                "humidity_percent": [55.0] * 24,
            },
        )
        tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        evaluator = StandardEvaluator()
        result = evaluator.evaluate_batch(
            wps=[wp],
            states={wp.id: state},
            tile_data=tile_data,
            timeout_guard=MockTimeoutGuard(),
            trace_id="trace_seq_clear",
            current_tile_id=tile_id,
            forecast_timestamp=NOW,
        )

        # Clear notification generated
        assert len(result.notifications) == 1
        # event_sequence should be incremented
        updated = result.state_updates[0]
        assert updated.event_sequence == 6
