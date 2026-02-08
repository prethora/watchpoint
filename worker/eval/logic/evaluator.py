"""
Core Evaluator logic for the Eval Worker.

Implements ``evaluate_batch`` which orchestrates the evaluation of a batch
of WatchPoints against loaded forecast data. Handles both Event Mode
(point-in-time threshold checking) and Monitor Mode (rolling-window
time-series analysis).

Architecture References:
    - 07-eval-worker.md Section 5 (Core Logic: Evaluator)
    - flow-simulations.md EVAL-001 (Evaluation Pipeline)
    - flow-simulations.md EVAL-003 (Monitor Mode)
    - flow-simulations.md EVAL-004 (Config Version Mismatch)
    - flow-simulations.md EVAL-005 (First Evaluation Baseline)
    - flow-simulations.md NOTIF-008 (Escalation)
    - flow-simulations.md NOTIF-009 (Cleared Notification)

Key Responsibilities:
    - ConfigPolicy (EVAL-004): Detect and handle config version mismatches.
    - Condition Evaluation: Compare forecast values against thresholds.
    - Hysteresis: Apply clear-side buffering to prevent alert flapping.
    - Escalation Tiers (NOTIF-008): Calculate escalation level based on overage.
    - Clear Notifications (NOTIF-009): Emit THRESHOLD_CLEARED events when conditions clear.
    - First Eval Baseline (EVAL-005): Suppress Monitor Mode on first eval.
    - Trigger Persistence: Store trigger_value for clearance payloads.
    - Test Mode Propagation: Copy WP.test_mode to NotificationEvent.
    - Trace ID Propagation: Copy trace_id into every notification.
"""

from __future__ import annotations

import logging
import math
import uuid
from datetime import datetime, timezone
from typing import Any, Protocol

from worker.eval.logic.dedup import ThreatDeduplicator
from worker.eval.logic.monitor import MonitorEvaluator
from worker.eval.models import (
    BatchResult,
    Condition,
    ConditionLogic,
    ConditionResult,
    EvaluationState,
    EventType,
    ForecastSnapshot,
    LocationSnapshot,
    MonitorSummary,
    NotificationEvent,
    NotificationOrdering,
    NotificationPayload,
    OrderingMetadata,
    SeenThreat,
    TimeRange,
    UrgencyLevel,
    WatchPoint,
)
from worker.eval.reader import TileData

logger = logging.getLogger(__name__)


# ---------------------------------------------------------------------------
# Hysteresis Constants (from 01-foundation-types.md Section 4.5)
# ---------------------------------------------------------------------------

HYSTERESIS_FACTOR_DEFAULT: float = 0.10  # 10% buffer
HYSTERESIS_TEMP: float = 1.0  # 1 deg C absolute buffer
HYSTERESIS_PRECIP_PROB: float = 5.0  # 5% absolute buffer

# Variable-specific absolute hysteresis buffers
_HYSTERESIS_ABSOLUTE: dict[str, float] = {
    "temperature_c": HYSTERESIS_TEMP,
    "precipitation_probability": HYSTERESIS_PRECIP_PROB,
}

# ---------------------------------------------------------------------------
# Escalation Constants (07-eval-worker.md Section 5.2)
# ---------------------------------------------------------------------------

ESCALATION_COOLDOWN_SECONDS: int = 7200  # 2 hours
ESCALATION_THRESHOLDS: list[tuple[int, float]] = [
    # (level, minimum overage fraction)
    (3, 1.00),  # Critical: >= 100% over threshold
    (2, 0.50),  # Severe:   >= 50% over threshold
    (1, 0.25),  # Elevated: >= 25% over threshold
    (0, 0.00),  # Initial:  condition met
]


# ---------------------------------------------------------------------------
# Tile ID Computation (mirrors DB generated column)
# ---------------------------------------------------------------------------


def compute_tile_id(lat: float, lon: float) -> str:
    """Compute tile_id for a given lat/lon, mirroring the DB generated column.

    Formula from ``02-foundation-db.md``::

        tile_id = FLOOR((90.0 - lat) / 22.5) || '.' || FLOOR(adjusted_lon / 45.0)
        where adjusted_lon = lon if lon >= 0 else 360 + lon

    Parameters
    ----------
    lat : float
        Latitude in degrees (-90 to 90).
    lon : float
        Longitude in degrees (-180 to 180).

    Returns
    -------
    str
        Tile identifier in "lat_index.lon_index" format.
    """
    lat_index = int(math.floor((90.0 - lat) / 22.5))
    adjusted_lon = lon if lon >= 0 else 360.0 + lon
    lon_index = int(math.floor(adjusted_lon / 45.0))

    # Clamp to valid range (guard against floating-point edge cases)
    lat_index = max(0, min(7, lat_index))
    lon_index = max(0, min(7, lon_index))

    return f"{lat_index}.{lon_index}"


# ---------------------------------------------------------------------------
# Condition Checking (07-eval-worker.md Section 5.1)
# ---------------------------------------------------------------------------


def check_condition(actual: float, c: Condition) -> bool:
    """Evaluate a single condition against an actual value.

    Implements the operators defined in ``07-eval-worker.md`` Section 5.1.
    The ``threshold`` field is always a list. For single-value operators,
    ``threshold[0]`` is used. For ``between``/``outside``, both elements
    are used as ``[min, max]``.

    Parameters
    ----------
    actual : float
        The actual forecast value for the variable.
    c : Condition
        The condition to evaluate.

    Returns
    -------
    bool
        True if the condition is met (threshold crossed).
    """
    op = c.operator
    th = c.threshold

    if not th:
        return False

    if op == ">":
        return actual > th[0]
    elif op == ">=":
        return actual >= th[0]
    elif op == "<":
        return actual < th[0]
    elif op == "<=":
        return actual <= th[0]
    elif op == "==":
        return abs(actual - th[0]) < 0.001  # Float equality safety
    elif op == "between":
        if len(th) < 2:
            return False
        return th[0] <= actual <= th[1]
    elif op == "outside":
        if len(th) < 2:
            return False
        return actual < th[0] or actual > th[1]
    else:
        logger.warning("Unknown operator '%s' in condition", op)
        return False


def check_condition_with_hysteresis(
    actual: float, c: Condition, is_clearing: bool
) -> bool:
    """Check condition with hysteresis applied for clear-side evaluation.

    When checking if a condition is still triggered (not clearing), uses
    standard thresholds. When checking for clearance, applies a buffer
    to prevent flapping.

    Hysteresis formula (from ``01-foundation-types.md`` Section 4.5)::

        Clear when: actual < threshold - (threshold * FACTOR) - ABSOLUTE_BUFFER

    For less-than operators, the logic is inverted (clear when above
    threshold + buffer).

    Parameters
    ----------
    actual : float
        The actual forecast value.
    c : Condition
        The condition to evaluate.
    is_clearing : bool
        If True, apply hysteresis buffer for clear-side check.

    Returns
    -------
    bool
        True if the condition is met (triggered).
    """
    if not is_clearing:
        return check_condition(actual, c)

    # Apply hysteresis for clearance check
    th = c.threshold
    if not th:
        return False

    absolute_buffer = _HYSTERESIS_ABSOLUTE.get(c.variable, 0.0)
    factor = HYSTERESIS_FACTOR_DEFAULT

    op = c.operator

    if op in (">", ">="):
        # Trigger: actual > threshold
        # Clear:   actual < threshold - buffer
        clear_threshold = th[0] - (th[0] * factor) - absolute_buffer
        return actual >= clear_threshold
    elif op in ("<", "<="):
        # Trigger: actual < threshold
        # Clear:   actual > threshold + buffer
        clear_threshold = th[0] + (th[0] * factor) + absolute_buffer
        return actual <= clear_threshold
    elif op == "between":
        if len(th) < 2:
            return False
        lower = th[0] - (abs(th[0]) * factor) - absolute_buffer
        upper = th[1] + (abs(th[1]) * factor) + absolute_buffer
        return lower <= actual <= upper
    elif op == "outside":
        if len(th) < 2:
            return False
        lower = th[0] + (abs(th[0]) * factor) + absolute_buffer
        upper = th[1] - (abs(th[1]) * factor) - absolute_buffer
        return actual < lower or actual > upper
    else:
        return check_condition(actual, c)


# ---------------------------------------------------------------------------
# Escalation Tier Logic (07-eval-worker.md Section 5.2)
# ---------------------------------------------------------------------------


def calculate_escalation_level(actual: float, threshold: float) -> int:
    """Calculate the escalation level based on how far actual exceeds threshold.

    Formula: ``overage = (actual - threshold) / threshold``

    Levels:
        - 0 (Initial):  condition met
        - 1 (Elevated): overage >= 25%
        - 2 (Severe):   overage >= 50%
        - 3 (Critical): overage >= 100%

    Parameters
    ----------
    actual : float
        The actual forecast value.
    threshold : float
        The condition threshold.

    Returns
    -------
    int
        Escalation level (0-3).
    """
    if threshold == 0:
        # Avoid division by zero; if threshold is 0 and actual > 0, max level
        return 3 if actual > 0 else 0

    overage = abs(actual - threshold) / abs(threshold)

    for level, min_overage in ESCALATION_THRESHOLDS:
        if overage >= min_overage:
            return level

    return 0


def urgency_from_escalation(level: int) -> str:
    """Map escalation level to urgency string.

    Parameters
    ----------
    level : int
        Escalation level (0-3).

    Returns
    -------
    str
        Urgency level string.
    """
    mapping = {
        0: UrgencyLevel.ROUTINE.value,
        1: UrgencyLevel.WATCH.value,
        2: UrgencyLevel.WARNING.value,
        3: UrgencyLevel.CRITICAL.value,
    }
    return mapping.get(level, UrgencyLevel.ROUTINE.value)


# ---------------------------------------------------------------------------
# ConfigPolicy (EVAL-004)
# ---------------------------------------------------------------------------


class ConfigPolicy:
    """Resolves config version mismatches between WatchPoint and EvaluationState.

    Per EVAL-004: When a user updates a WatchPoint (changing conditions or
    location) while evaluation is in flight, the config_version in the
    WatchPoint will differ from the state. This class determines how to
    handle the mismatch.

    Returns:
        "ABORT"    - Tile mismatch, skip this WatchPoint entirely.
        "RESET"    - Same tile but config changed, re-evaluate from scratch.
        "CONTINUE" - Versions match, normal evaluation with hysteresis.
    """

    @staticmethod
    def resolve(
        wp: WatchPoint,
        state: EvaluationState,
        current_tile_id: str,
    ) -> str:
        """Determine the action for a config version mismatch.

        Parameters
        ----------
        wp : WatchPoint
            The current WatchPoint (may have been updated).
        state : EvaluationState
            The evaluation state from the previous cycle.
        current_tile_id : str
            The tile_id of the Zarr data currently loaded in memory.

        Returns
        -------
        str
            One of "ABORT", "RESET", or "CONTINUE".
        """
        if wp.config_version == state.config_version:
            return "CONTINUE"

        # Config version mismatch detected
        # Step 1: Calculate the WP's current tile_id
        wp_tile_id = compute_tile_id(wp.location.lat, wp.location.lon)

        # Step 2: Compare with the tile we have data for
        if wp_tile_id != current_tile_id:
            logger.info(
                "ConfigPolicy ABORT: wp=%s moved from tile %s to %s "
                "(config_version %d -> %d)",
                wp.id,
                current_tile_id,
                wp_tile_id,
                state.config_version,
                wp.config_version,
            )
            return "ABORT"

        logger.info(
            "ConfigPolicy RESET: wp=%s config changed "
            "(version %d -> %d) but same tile %s",
            wp.id,
            state.config_version,
            wp.config_version,
            current_tile_id,
        )
        return "RESET"

    @staticmethod
    def apply_reset(state: EvaluationState, wp: WatchPoint) -> EvaluationState:
        """Apply a RESET to the evaluation state.

        Per EVAL-004 and architecture spec: Resets trigger state, trigger_value,
        and escalation_level, but PRESERVES ``seen_threats`` to maintain
        temporal deduplication history and prevent Alert Shock.

        Parameters
        ----------
        state : EvaluationState
            Current state to reset.
        wp : WatchPoint
            WatchPoint with the new config_version.

        Returns
        -------
        EvaluationState
            State with reset fields and updated config_version.
        """
        return EvaluationState(
            watchpoint_id=state.watchpoint_id,
            config_version=wp.config_version,
            last_evaluated_at=state.last_evaluated_at,
            last_forecast_run=state.last_forecast_run,
            # Reset trigger/hysteresis
            previous_trigger_state=False,
            trigger_state_changed_at=None,
            trigger_value=None,
            # Reset escalation
            escalation_level=0,
            last_escalated_at=None,
            # Preserve notification tracking
            last_notified_at=state.last_notified_at,
            last_notified_state=state.last_notified_state,
            notification_count_24h=state.notification_count_24h,
            # Preserve digest data
            last_forecast_summary=state.last_forecast_summary,
            last_digest_sent_at=state.last_digest_sent_at,
            last_digest_content=state.last_digest_content,
            # PRESERVE seen_threats (critical for dedup continuity)
            seen_threats=state.seen_threats,
            event_sequence=state.event_sequence,
        )

    @staticmethod
    def apply_abort(state: EvaluationState, wp: WatchPoint) -> EvaluationState:
        """Apply an ABORT to the evaluation state.

        Per EVAL-004: Updates only config_version and resets trigger state.
        Preserves ``seen_threats`` to maintain temporal dedup.

        Parameters
        ----------
        state : EvaluationState
            Current state to update.
        wp : WatchPoint
            WatchPoint with the new config_version.

        Returns
        -------
        EvaluationState
            State with synced config_version.
        """
        return EvaluationState(
            watchpoint_id=state.watchpoint_id,
            config_version=wp.config_version,
            last_evaluated_at=state.last_evaluated_at,
            last_forecast_run=state.last_forecast_run,
            # Reset trigger/hysteresis
            previous_trigger_state=False,
            trigger_state_changed_at=None,
            trigger_value=None,
            # Reset escalation
            escalation_level=0,
            last_escalated_at=None,
            # Preserve notification tracking
            last_notified_at=state.last_notified_at,
            last_notified_state=state.last_notified_state,
            notification_count_24h=state.notification_count_24h,
            # Preserve digest data
            last_forecast_summary=state.last_forecast_summary,
            last_digest_sent_at=state.last_digest_sent_at,
            last_digest_content=state.last_digest_content,
            # PRESERVE seen_threats
            seen_threats=state.seen_threats,
            event_sequence=state.event_sequence,
        )


# ---------------------------------------------------------------------------
# TimeoutGuard Protocol
# ---------------------------------------------------------------------------


class TimeoutGuard(Protocol):
    """Protocol for the timeout guard passed from the Lambda handler."""

    def check_remaining(self) -> None:
        """Raise TimeBudgetExceededError if < 5 seconds remain."""
        ...


class TimeBudgetExceededError(Exception):
    """Raised when Lambda timeout is imminent."""

    pass


# ---------------------------------------------------------------------------
# Notification Builder Helpers
# ---------------------------------------------------------------------------


def _build_forecast_snapshot(point_data: dict[str, float]) -> ForecastSnapshot:
    """Build a ForecastSnapshot from extracted point data."""
    return ForecastSnapshot(
        precipitation_probability=point_data.get(
            "precipitation_probability", 0.0
        ),
        precipitation_mm=point_data.get("precipitation_mm", 0.0),
        temperature_c=point_data.get("temperature_c", 0.0),
        wind_speed_kmh=point_data.get("wind_speed_kmh", 0.0),
        humidity_percent=point_data.get("humidity_percent", 0.0),
    )


def _build_condition_results(
    conditions: list[Condition],
    point_data: dict[str, float],
    previous_trigger_value: float | None = None,
) -> list[ConditionResult]:
    """Build ConditionResult list from conditions and forecast data."""
    results: list[ConditionResult] = []
    for c in conditions:
        actual = point_data.get(c.variable, 0.0)
        matched = check_condition(actual, c)
        results.append(
            ConditionResult(
                variable=c.variable,
                operator=c.operator,
                threshold=c.threshold,
                actual_value=actual,
                previous_value=previous_trigger_value,
                matched=matched,
            )
        )
    return results


def _build_notification_event(
    wp: WatchPoint,
    state: EvaluationState,
    event_type: str,
    urgency: str,
    point_data: dict[str, float],
    trace_id: str,
    forecast_timestamp: datetime,
    now: datetime,
    previous_trigger_value: float | None = None,
    source_model: str = "",
) -> NotificationEvent:
    """Build a NotificationEvent with full payload.

    Parameters
    ----------
    wp : WatchPoint
        WatchPoint that triggered.
    state : EvaluationState
        Current evaluation state (with updated event_sequence).
    event_type : str
        The event type (e.g., "threshold_crossed").
    urgency : str
        Urgency level string.
    point_data : dict[str, float]
        Extracted forecast values at the WP's location.
    trace_id : str
        Trace ID from the EvalMessage.
    forecast_timestamp : datetime
        The forecast run timestamp.
    now : datetime
        Current evaluation time.
    previous_trigger_value : float or None
        The previous trigger_value for clearance notifications.
    source_model : str
        Source forecast model name.

    Returns
    -------
    NotificationEvent
        Fully populated notification event.
    """
    notif_id = f"notif_{uuid.uuid4()}"

    condition_results = _build_condition_results(
        wp.conditions, point_data, previous_trigger_value
    )

    payload = NotificationPayload(
        notification_id=notif_id,
        watchpoint_id=wp.id,
        watchpoint_name=wp.name,
        timezone=wp.timezone,
        event_type=event_type,
        triggered_at=now,
        location=LocationSnapshot(
            lat=wp.location.lat,
            lon=wp.location.lon,
            display_name=wp.location.display_name,
        ),
        time_window=wp.time_window,
        forecast_snapshot=_build_forecast_snapshot(point_data),
        conditions_evaluated=condition_results,
        urgency=urgency,
        source_model=source_model,
        ordering=OrderingMetadata(
            event_sequence=state.event_sequence,
            forecast_timestamp=forecast_timestamp,
            eval_timestamp=now,
        ),
    )

    return NotificationEvent(
        id=notif_id,
        watchpoint_id=wp.id,
        organization_id=wp.organization_id,
        trace_id=trace_id,
        event_type=event_type,
        urgency=urgency,
        payload=payload.model_dump(mode="json"),
        ordering=NotificationOrdering(
            event_sequence=state.event_sequence,
            forecast_timestamp=forecast_timestamp,
            evaluation_timestamp=now,
        ),
        test_mode=wp.test_mode,
        created_at=now,
    )


# ---------------------------------------------------------------------------
# StandardEvaluator
# ---------------------------------------------------------------------------


class StandardEvaluator:
    """Concrete evaluator implementing ``evaluate_batch``.

    Orchestrates evaluation for a batch of WatchPoints against loaded
    forecast data. Handles both Event Mode and Monitor Mode, applying
    ConfigPolicy, hysteresis, escalation, deduplication, and first-eval
    baseline logic.

    Parameters
    ----------
    enable_threat_dedup : bool
        Feature flag to enable/disable temporal deduplication.
    """

    def __init__(self, enable_threat_dedup: bool = True) -> None:
        self._enable_threat_dedup = enable_threat_dedup
        self._config_policy = ConfigPolicy()
        self._dedup = ThreatDeduplicator()
        self._monitor_evaluator = MonitorEvaluator(check_condition)

    def evaluate_batch(
        self,
        wps: list[WatchPoint],
        states: dict[str, EvaluationState],
        tile_data: TileData,
        timeout_guard: TimeoutGuard | None,
        trace_id: str,
        current_tile_id: str,
        forecast_timestamp: datetime,
        source_model: str = "",
    ) -> BatchResult:
        """Evaluate a batch of WatchPoints against forecast data.

        Per ``07-eval-worker.md`` Section 5.2: Orchestrates evaluation with
        granular idempotency (per-WP, not per-message), ConfigPolicy checks,
        and test mode propagation.

        Parameters
        ----------
        wps : list[WatchPoint]
            WatchPoints to evaluate (already filtered by tile and page).
        states : dict[str, EvaluationState]
            Current evaluation states keyed by watchpoint_id.
        tile_data : TileData
            Loaded forecast data for the tile.
        timeout_guard : TimeoutGuard or None
            Lambda timeout guard. If None, no timeout checking.
        trace_id : str
            Trace ID from the EvalMessage for distributed tracing.
        current_tile_id : str
            The tile_id of the currently loaded forecast data.
        forecast_timestamp : datetime
            The forecast run timestamp from the message.
        source_model : str
            Source forecast model identifier.

        Returns
        -------
        BatchResult
            Contains state_updates for all evaluated WPs and notifications
            for WPs that triggered.
        """
        now = datetime.now(timezone.utc)
        state_updates: list[EvaluationState] = []
        notifications: list[NotificationEvent] = []

        for wp in wps:
            # Timeout check per WatchPoint (granular)
            if timeout_guard is not None:
                try:
                    timeout_guard.check_remaining()
                except TimeBudgetExceededError:
                    logger.warning(
                        "Timeout imminent, stopping batch at wp=%s. "
                        "Processed %d/%d WPs.",
                        wp.id,
                        len(state_updates),
                        len(wps),
                    )
                    raise

            # Lazy State Initialization: create default if missing
            state = states.get(wp.id)
            if state is None:
                state = EvaluationState(watchpoint_id=wp.id)
                logger.debug(
                    "Lazy-init state for wp=%s (first evaluation)", wp.id
                )

            # Granular Idempotency: skip if already processed this forecast
            if (
                state.last_forecast_run is not None
                and forecast_timestamp <= state.last_forecast_run
            ):
                logger.debug(
                    "Skipping wp=%s: already evaluated for forecast %s",
                    wp.id,
                    forecast_timestamp.isoformat(),
                )
                continue

            # ConfigPolicy check (EVAL-004)
            policy_action = self._config_policy.resolve(
                wp, state, current_tile_id
            )

            if policy_action == "ABORT":
                # Tile mismatch: update config version only, skip evaluation
                updated_state = ConfigPolicy.apply_abort(state, wp)
                updated_state.last_forecast_run = forecast_timestamp
                updated_state.last_evaluated_at = now
                state_updates.append(updated_state)
                continue

            if policy_action == "RESET":
                # Same tile but config changed: reset hysteresis
                state = ConfigPolicy.apply_reset(state, wp)

            # Determine mode and evaluate
            is_first_eval = state.last_evaluated_at is None

            if wp.monitor_config is not None:
                # Monitor Mode
                result_state, result_notifs = self._evaluate_monitor(
                    wp=wp,
                    state=state,
                    tile_data=tile_data,
                    trace_id=trace_id,
                    forecast_timestamp=forecast_timestamp,
                    now=now,
                    is_first_eval=is_first_eval,
                    source_model=source_model,
                    policy_action=policy_action,
                )
            else:
                # Event Mode
                result_state, result_notifs = self._evaluate_event(
                    wp=wp,
                    state=state,
                    tile_data=tile_data,
                    trace_id=trace_id,
                    forecast_timestamp=forecast_timestamp,
                    now=now,
                    is_first_eval=is_first_eval,
                    source_model=source_model,
                    policy_action=policy_action,
                )

            # Update common state fields
            result_state.config_version = wp.config_version
            result_state.last_evaluated_at = now
            result_state.last_forecast_run = forecast_timestamp

            state_updates.append(result_state)
            notifications.extend(result_notifs)

        return BatchResult(
            state_updates=state_updates,
            notifications=notifications,
        )

    def _evaluate_event(
        self,
        wp: WatchPoint,
        state: EvaluationState,
        tile_data: TileData,
        trace_id: str,
        forecast_timestamp: datetime,
        now: datetime,
        is_first_eval: bool,
        source_model: str,
        policy_action: str,
    ) -> tuple[EvaluationState, list[NotificationEvent]]:
        """Evaluate a single WatchPoint in Event Mode.

        Event Mode logic:
            1. Extract point data from tile.
            2. Check conditions (with hysteresis if previously triggered).
            3. Detect state change (triggered -> cleared or cleared -> triggered).
            4. Generate notification on state change.
            5. Calculate escalation level.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint to evaluate.
        state : EvaluationState
            Current evaluation state.
        tile_data : TileData
            Loaded forecast data.
        trace_id : str
            Trace ID for distributed tracing.
        forecast_timestamp : datetime
            Forecast run timestamp.
        now : datetime
            Current evaluation time.
        is_first_eval : bool
            True if this is the first evaluation for this WP.
        source_model : str
            Source model identifier.
        policy_action : str
            ConfigPolicy action ("CONTINUE" or "RESET").

        Returns
        -------
        tuple[EvaluationState, list[NotificationEvent]]
            Updated state and any generated notifications.
        """
        notifications: list[NotificationEvent] = []

        # Extract point data
        point_data = tile_data.extract_point(wp.location.lat, wp.location.lon)

        # Check conditions
        is_clearing = state.previous_trigger_state
        use_hysteresis = is_clearing and policy_action == "CONTINUE"

        triggered = self._check_all_conditions(
            wp, point_data, use_hysteresis=use_hysteresis
        )

        # Detect state change
        was_triggered = state.previous_trigger_state
        state_changed = triggered != was_triggered

        # Get primary condition value for trigger persistence
        primary_value = self._get_primary_value(wp, point_data)

        if state_changed:
            if triggered:
                # Cleared -> Triggered transition
                new_escalation = self._calculate_escalation_for_wp(
                    wp, point_data
                )
                urgency = urgency_from_escalation(new_escalation)

                # Increment event_sequence for notification
                state.event_sequence += 1

                # Build notification (Event Mode always notifies on first trigger)
                notif = _build_notification_event(
                    wp=wp,
                    state=state,
                    event_type=EventType.THRESHOLD_CROSSED.value,
                    urgency=urgency,
                    point_data=point_data,
                    trace_id=trace_id,
                    forecast_timestamp=forecast_timestamp,
                    now=now,
                    source_model=source_model,
                )
                notifications.append(notif)

                # Update state
                state.previous_trigger_state = True
                state.trigger_state_changed_at = now
                state.trigger_value = primary_value
                state.escalation_level = new_escalation
                state.last_escalated_at = now
                state.last_notified_at = now
                state.last_notified_state = EventType.THRESHOLD_CROSSED.value

            else:
                # Triggered -> Cleared transition
                previous_trigger_val = state.trigger_value

                # Check if user wants clear notifications
                notify_on_clear = (
                    wp.preferences is not None and wp.preferences.notify_on_clear
                )
                if notify_on_clear:
                    # Only increment event_sequence when generating a notification
                    # Per spec: "MUST ONLY be incremented when a row is inserted
                    # into the notifications table"
                    state.event_sequence += 1

                    notif = _build_notification_event(
                        wp=wp,
                        state=state,
                        event_type=EventType.THRESHOLD_CLEARED.value,
                        urgency=UrgencyLevel.ROUTINE.value,
                        point_data=point_data,
                        trace_id=trace_id,
                        forecast_timestamp=forecast_timestamp,
                        now=now,
                        previous_trigger_value=previous_trigger_val,
                        source_model=source_model,
                    )
                    notifications.append(notif)
                    state.last_notified_at = now
                    state.last_notified_state = EventType.THRESHOLD_CLEARED.value

                # Reset trigger state
                state.previous_trigger_state = False
                state.trigger_state_changed_at = now
                state.trigger_value = None
                state.escalation_level = 0
                state.last_escalated_at = None

        elif triggered and was_triggered:
            # Still triggered: check for escalation
            new_escalation = self._calculate_escalation_for_wp(
                wp, point_data
            )
            if new_escalation > state.escalation_level:
                # Check escalation cooldown (2-hour minimum)
                can_escalate = True
                if state.last_escalated_at is not None:
                    elapsed = (now - state.last_escalated_at).total_seconds()
                    if elapsed < ESCALATION_COOLDOWN_SECONDS:
                        can_escalate = False

                if can_escalate:
                    urgency = urgency_from_escalation(new_escalation)
                    state.event_sequence += 1

                    notif = _build_notification_event(
                        wp=wp,
                        state=state,
                        event_type=EventType.THRESHOLD_CROSSED.value,
                        urgency=urgency,
                        point_data=point_data,
                        trace_id=trace_id,
                        forecast_timestamp=forecast_timestamp,
                        now=now,
                        source_model=source_model,
                    )
                    notifications.append(notif)

                    state.escalation_level = new_escalation
                    state.last_escalated_at = now
                    state.last_notified_at = now

            # Update trigger_value to latest
            state.trigger_value = primary_value

        return state, notifications

    def _evaluate_monitor(
        self,
        wp: WatchPoint,
        state: EvaluationState,
        tile_data: TileData,
        trace_id: str,
        forecast_timestamp: datetime,
        now: datetime,
        is_first_eval: bool,
        source_model: str,
        policy_action: str,
    ) -> tuple[EvaluationState, list[NotificationEvent]]:
        """Evaluate a single WatchPoint in Monitor Mode.

        Monitor Mode logic:
            1. Extract time-series from TileData.
            2. Filter by active hours/days.
            3. Find contiguous violation periods.
            4. Run temporal deduplication against seen_threats.
            5. First Eval (EVAL-005): suppress notification, establish baseline.
            6. Generate notifications for new threats.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint to evaluate (must have monitor_config).
        state : EvaluationState
            Current evaluation state.
        tile_data : TileData
            Loaded forecast data.
        trace_id : str
            Trace ID.
        forecast_timestamp : datetime
            Forecast run timestamp.
        now : datetime
            Current evaluation time.
        is_first_eval : bool
            True if first evaluation.
        source_model : str
            Source model identifier.
        policy_action : str
            ConfigPolicy action.

        Returns
        -------
        tuple[EvaluationState, list[NotificationEvent]]
            Updated state and any generated notifications.
        """
        notifications: list[NotificationEvent] = []

        # Run monitor evaluation pipeline
        summary, violations = self._monitor_evaluator.evaluate(
            wp, tile_data, now
        )

        # Populate triggered_periods from violations
        summary.triggered_periods = [
            TimeRange(start=v.start, end=v.end) for v in violations
        ]

        # Store summary as baseline
        state.last_forecast_summary = summary.model_dump(mode="json")

        # Extract point data for notification payloads
        point_data = tile_data.extract_point(wp.location.lat, wp.location.lon)

        if violations and self._enable_threat_dedup:
            # Run temporal deduplication (EVAL-003c)
            updated_threats, new_threats = self._dedup.deduplicate(
                seen_threats=state.seen_threats,
                new_violations=violations,
                now=now,
            )
            state.seen_threats = updated_threats
        elif violations:
            # Dedup disabled: all violations are new threats
            new_threats = violations
            state.seen_threats = list(state.seen_threats) + violations
        else:
            new_threats = []

        # First Eval Baseline (EVAL-005)
        if is_first_eval:
            logger.info(
                "First evaluation for wp=%s (monitor mode): "
                "suppressing %d notifications, establishing baseline.",
                wp.id,
                len(new_threats),
            )
            # Suppress all notifications but still record seen_threats
            # and the MonitorSummary baseline
            return state, []

        # Generate notifications for genuinely new threats
        for threat in new_threats:
            new_escalation = self._calculate_escalation_for_wp(
                wp, point_data
            )
            urgency = urgency_from_escalation(new_escalation)

            state.event_sequence += 1

            notif = _build_notification_event(
                wp=wp,
                state=state,
                event_type=EventType.MONITOR_DIGEST.value,
                urgency=urgency,
                point_data=point_data,
                trace_id=trace_id,
                forecast_timestamp=forecast_timestamp,
                now=now,
                source_model=source_model,
            )
            notifications.append(notif)

            state.escalation_level = new_escalation
            state.last_escalated_at = now
            state.last_notified_at = now
            state.last_notified_state = EventType.MONITOR_DIGEST.value

        # Update trigger state based on whether there are active violations
        if violations:
            state.previous_trigger_state = True
            primary_value = self._get_primary_value(wp, point_data)
            state.trigger_value = primary_value
        else:
            if state.previous_trigger_state:
                state.previous_trigger_state = False
                state.trigger_value = None
                state.escalation_level = 0

        return state, notifications

    def _check_all_conditions(
        self,
        wp: WatchPoint,
        point_data: dict[str, float],
        use_hysteresis: bool = False,
    ) -> bool:
        """Check all conditions with appropriate logic (ANY/ALL).

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint with conditions and condition_logic.
        point_data : dict[str, float]
            Extracted forecast values.
        use_hysteresis : bool
            If True, apply hysteresis for clear-side checking.

        Returns
        -------
        bool
            True if conditions are met.
        """
        results: list[bool] = []

        for c in wp.conditions:
            actual = point_data.get(c.variable, 0.0)
            if use_hysteresis:
                matched = check_condition_with_hysteresis(
                    actual, c, is_clearing=True
                )
            else:
                matched = check_condition(actual, c)
            results.append(matched)

        if not results:
            return False

        if wp.condition_logic == ConditionLogic.ALL:
            return all(results)
        else:  # ANY
            return any(results)

    def _calculate_escalation_for_wp(
        self,
        wp: WatchPoint,
        point_data: dict[str, float],
    ) -> int:
        """Calculate the maximum escalation level across all conditions.

        Uses the primary (first) condition's threshold and actual value.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint with conditions.
        point_data : dict[str, float]
            Forecast values.

        Returns
        -------
        int
            Maximum escalation level (0-3).
        """
        if not wp.conditions:
            return 0

        max_level = 0
        for c in wp.conditions:
            actual = point_data.get(c.variable, 0.0)
            if not c.threshold:
                continue
            # Use the first threshold value for escalation calculation
            level = calculate_escalation_level(actual, c.threshold[0])
            max_level = max(max_level, level)

        return max_level

    def _get_primary_value(
        self,
        wp: WatchPoint,
        point_data: dict[str, float],
    ) -> float | None:
        """Get the actual value of the primary (first) condition.

        Used for trigger_value persistence and clearance payloads.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint with conditions.
        point_data : dict[str, float]
            Forecast values.

        Returns
        -------
        float or None
            The actual value of the primary condition's variable.
        """
        if not wp.conditions:
            return None
        return point_data.get(wp.conditions[0].variable)
