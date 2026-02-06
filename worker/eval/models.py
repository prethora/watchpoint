"""
Domain models for the Eval Worker.

Pydantic v2 models that mirror the Go structs defined in
``01-foundation-types.md`` and the SQS message schemas defined in
``06-batcher.md`` / ``07-eval-worker.md``.

**CRITICAL**: All JSON field names use ``snake_case`` and MUST exactly match
the Go struct ``json:`` tags defined in ``internal/types/domain.go`` to ensure
cross-language serialization compatibility over SQS.
"""

from __future__ import annotations

from datetime import datetime, timezone
from enum import Enum
from typing import Any

from pydantic import BaseModel, Field


# ---------------------------------------------------------------------------
# Enums
# ---------------------------------------------------------------------------


class ForecastType(str, Enum):
    """Forecast model type. Mirrors Go ``ForecastType`` constants."""

    MEDIUM_RANGE = "medium_range"
    NOWCAST = "nowcast"


class EvalAction(str, Enum):
    """Determines the worker logic for processing a message.

    Mirrors Go ``EvalAction`` constants from ``internal/types/enums.go``.
    """

    EVALUATE = "evaluate"
    GENERATE_SUMMARY = "generate_summary"


class ConditionLogic(str, Enum):
    """How multiple conditions are combined. Mirrors Go ``ConditionLogic``."""

    ANY = "ANY"
    ALL = "ALL"


class Status(str, Enum):
    """WatchPoint lifecycle status. Mirrors Go ``Status`` constants."""

    ACTIVE = "active"
    PAUSED = "paused"
    ARCHIVED = "archived"


class EventType(str, Enum):
    """Notification event type. Mirrors Go ``EventType`` constants."""

    THRESHOLD_CROSSED = "threshold_crossed"
    THRESHOLD_CLEARED = "threshold_cleared"
    FORECAST_CHANGED = "forecast_changed"
    IMMINENT_ALERT = "imminent_alert"
    MONITOR_DIGEST = "monitor_digest"
    SYSTEM_ALERT = "system_alert"
    BILLING_WARNING = "billing_warning"
    BILLING_RECEIPT = "billing_receipt"


class UrgencyLevel(str, Enum):
    """Notification urgency. Mirrors Go ``UrgencyLevel`` constants."""

    ROUTINE = "routine"
    WATCH = "watch"
    WARNING = "warning"
    CRITICAL = "critical"


# ---------------------------------------------------------------------------
# Value Objects
# ---------------------------------------------------------------------------


class Condition(BaseModel):
    """A threshold-based rule for a weather variable.

    Mirrors Go ``Condition`` struct from ``internal/types/conditions.go``.

    ``threshold`` is always a list to support both single values (``[50.0]``)
    and range values for the ``between`` operator (``[15.0, 25.0]``).
    """

    model_config = {"populate_by_name": True}

    variable: str
    operator: str
    threshold: list[float]
    unit: str


class MonitorConfig(BaseModel):
    """Configuration for Monitor Mode WatchPoints.

    Mirrors Go ``MonitorConfig`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    window_hours: int
    active_hours: list[list[int]] = []
    active_days: list[int] = []


class TimeWindow(BaseModel):
    """Event Mode time window.

    Mirrors Go ``TimeWindow`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    start: datetime
    end: datetime


class Location(BaseModel):
    """Geographic coordinate.

    Mirrors Go ``Location`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    lat: float
    lon: float
    display_name: str = ""


class Channel(BaseModel):
    """Notification delivery channel configuration.

    Mirrors Go ``Channel`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    id: str
    type: str
    config: dict[str, Any] = {}
    enabled: bool = True


class Preferences(BaseModel):
    """WatchPoint notification preferences.

    Mirrors Go ``Preferences`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    notify_on_clear: bool = False
    notify_on_forecast_change: bool = False


# ---------------------------------------------------------------------------
# Input: EvalMessage (SQS message from Batcher -> Eval Worker)
# ---------------------------------------------------------------------------


class EvalMessage(BaseModel):
    """SQS payload sent from Batcher to Eval Worker.

    Mirrors Go ``EvalMessage`` from ``internal/types/domain.go``.

    JSON keys MUST match the Go struct ``json:`` tags exactly:
      - ``batch_id``, ``trace_id``, ``forecast_type``, ``run_timestamp``,
        ``tile_id``, ``page``, ``page_size``, ``total_items``, ``action``,
        ``specific_watchpoint_ids``
    """

    model_config = {"populate_by_name": True}

    batch_id: str
    trace_id: str
    forecast_type: ForecastType
    run_timestamp: datetime
    tile_id: str
    page: int = 0
    page_size: int = 0
    total_items: int = 0

    # Action determines the worker logic. Defaults to "evaluate".
    action: EvalAction = EvalAction.EVALUATE

    # If populated, the worker must filter execution to only these IDs.
    # Used for immediate Resume/Update triggers.
    specific_watchpoint_ids: list[str] = []


# ---------------------------------------------------------------------------
# Output: NotificationEvent (Eval Worker -> Notification SQS Queue)
# ---------------------------------------------------------------------------


class NotificationOrdering(BaseModel):
    """Ordering metadata for client-side sorting of notifications.

    Mirrors the ``ordering`` field within ``NotificationEvent`` as specified
    in ``07-eval-worker.md`` Section 3.2.
    """

    model_config = {"populate_by_name": True}

    event_sequence: int
    forecast_timestamp: datetime
    evaluation_timestamp: datetime


class NotificationEvent(BaseModel):
    """Notification event sent to the Notification Queue.

    Defined in ``07-eval-worker.md`` Section 3.2.

    The ``payload`` dict follows the ``NotificationPayload`` schema from
    ``01-foundation-types.md`` Section 10.1.
    """

    model_config = {"populate_by_name": True}

    id: str  # "notif_..."
    watchpoint_id: str
    organization_id: str
    trace_id: str  # Propagated from EvalMessage for distributed tracing

    event_type: str  # e.g., "threshold_crossed"
    urgency: str  # e.g., "critical"

    # Snapshot of data at time of trigger
    payload: dict[str, Any]

    # Ordering metadata for client-side sorting
    ordering: NotificationOrdering

    test_mode: bool = False
    created_at: datetime = Field(
        default_factory=lambda: datetime.now(timezone.utc),
    )


# ---------------------------------------------------------------------------
# Domain Entities
# ---------------------------------------------------------------------------


class WatchPoint(BaseModel):
    """WatchPoint domain entity as loaded from the database.

    Mirrors the Go ``WatchPoint`` struct from ``internal/types/domain.go``.

    Only includes the fields that the Eval Worker needs for evaluation.
    Additional fields (e.g., ``template_set``, ``notification_prefs``) are
    included so the worker can propagate them into notification payloads and
    channel config snapshots.
    """

    model_config = {"populate_by_name": True}

    id: str
    organization_id: str

    # Core Identity
    name: str
    location: Location
    timezone: str
    tile_id: str = ""

    # Modes (Mutually Exclusive)
    time_window: TimeWindow | None = None
    monitor_config: MonitorConfig | None = None

    # Logic
    conditions: list[Condition]
    condition_logic: ConditionLogic

    # Delivery
    channels: list[Channel] = []
    template_set: str = ""
    preferences: Preferences | None = None

    # Meta
    status: Status = Status.ACTIVE
    test_mode: bool = False
    tags: list[str] = []
    config_version: int = 0
    source: str = ""

    created_at: datetime | None = None
    updated_at: datetime | None = None
    archived_at: datetime | None = None
    archived_reason: str = ""


class SeenThreat(BaseModel):
    """A single entry in the ``seen_threats`` JSONB array.

    Used for Temporal Overlap Detection (deduplication) in Monitor Mode.
    Schema: ``{"start": datetime, "end": datetime, "type": str}``.
    """

    model_config = {"populate_by_name": True}

    start: datetime
    end: datetime
    type: str = ""


class EvaluationState(BaseModel):
    """Per-WatchPoint evaluation state.

    Mirrors the ``watchpoint_evaluation_state`` DB table schema from
    ``02-foundation-db.md`` Section 4.1.

    Includes all columns the Eval Worker reads and writes during evaluation.
    """

    model_config = {"populate_by_name": True}

    watchpoint_id: str
    config_version: int = 0

    # Evaluation Metadata
    last_evaluated_at: datetime | None = None
    last_forecast_run: datetime | None = None

    # Trigger Logic & Hysteresis
    previous_trigger_state: bool = False
    trigger_state_changed_at: datetime | None = None
    trigger_value: float | None = None

    # Escalation Logic
    escalation_level: int = 0
    last_escalated_at: datetime | None = None

    # Notification Tracking
    last_notified_at: datetime | None = None
    last_notified_state: str | None = None
    notification_count_24h: int = 0

    # Digest Support
    last_forecast_summary: dict[str, Any] | None = None
    last_digest_sent_at: datetime | None = None
    last_digest_content: dict[str, Any] | None = None

    # Monitor Mode Deduplication
    seen_threats: list[SeenThreat] = []
    event_sequence: int = 0


# ---------------------------------------------------------------------------
# Monitor Summary
# ---------------------------------------------------------------------------


class TimeRange(BaseModel):
    """A start/end time pair.

    Mirrors Go ``TimeRange`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    start: datetime
    end: datetime


class MonitorSummary(BaseModel):
    """Aggregation of a rolling window for Monitor Mode digests.

    Mirrors Go ``MonitorSummary`` from ``internal/types/domain.go``.

    CONTRACT: Defines the strict schema for the ``last_forecast_summary``
    JSONB column. Acts as the data contract between the Python Eval Worker
    (writer) and Go Digest Generator (reader).
    """

    model_config = {"populate_by_name": True}

    window_start: datetime
    window_end: datetime
    max_values: dict[str, float] = {}
    triggered_periods: list[TimeRange] = []


# ---------------------------------------------------------------------------
# Supporting Types for Notification Payload Construction
# ---------------------------------------------------------------------------


class LocationSnapshot(BaseModel):
    """Captures a location at a point in time for notification payloads.

    Mirrors Go ``LocationSnapshot`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    lat: float
    lon: float
    display_name: str = ""


class ForecastSnapshot(BaseModel):
    """Weather data values at a point in time and space.

    Mirrors Go ``ForecastSnapshot`` from ``internal/types/forecast.go``.
    JSON keys match Go struct tags exactly.
    """

    model_config = {"populate_by_name": True}

    precipitation_probability: float = 0.0
    precipitation_mm: float = 0.0
    temperature_c: float = 0.0
    wind_speed_kmh: float = 0.0
    humidity_percent: float = 0.0


class ConditionResult(BaseModel):
    """Evaluation outcome of a single condition.

    Mirrors Go ``ConditionResult`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    variable: str
    operator: str
    threshold: list[float]
    actual_value: float
    previous_value: float | None = None
    matched: bool


class OrderingMetadata(BaseModel):
    """Sequencing information for notification deduplication.

    Mirrors Go ``OrderingMetadata`` from ``internal/types/domain.go``.
    """

    model_config = {"populate_by_name": True}

    event_sequence: int
    forecast_timestamp: datetime
    eval_timestamp: datetime


class NotificationPayload(BaseModel):
    """Full notification payload schema stored in the ``payload`` JSONB column.

    Mirrors Go ``NotificationPayload`` from ``internal/types/domain.go``.

    This is the CRITICAL contract between Eval Worker and Notification Workers.
    """

    model_config = {"populate_by_name": True}

    notification_id: str
    watchpoint_id: str
    watchpoint_name: str
    timezone: str

    event_type: str
    triggered_at: datetime

    # Context
    location: LocationSnapshot
    time_window: TimeWindow | None = None

    # Data
    forecast_snapshot: ForecastSnapshot
    conditions_evaluated: list[ConditionResult]

    # Metadata
    urgency: str
    source_model: str
    ordering: OrderingMetadata
