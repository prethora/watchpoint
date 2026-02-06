"""
Repository: Atomic persistence layer for the Eval Worker.

Implements the ``Repository`` interface from ``07-eval-worker.md`` Section 4.2.
Uses ``psycopg`` (v3) for PostgreSQL access with proper transaction handling,
and ``boto3`` for SQS message publishing.

Key Design Decisions:
    - **Stale Write Protection (CONC-006)**: The ``commit_batch`` method uses a
      conditional UPDATE on ``last_forecast_run`` to prevent out-of-order SQS
      messages from overwriting newer state. Only WatchPoints whose state was
      actually updated (returned by ``RETURNING``) get notifications inserted.
    - **Channel Config Snapshot**: Notification deliveries capture a snapshot of
      the channel configuration at evaluation time, not a reference. This ensures
      delivery workers have the exact config that was valid when the notification
      was created.
    - **Event Sequence**: The ``event_sequence`` counter is only incremented when
      a notification is actually inserted, ensuring a gap-free timeline of alerts.

Architecture References:
    - 07-eval-worker.md Section 4.2 (Repository interface)
    - 02-foundation-db.md Section 4 (Evaluation & Notification schema)
    - Flow EVAL-001 steps 10-12 (fetch/evaluate/commit)
    - Flow CONC-006 (stale write protection)
"""

from __future__ import annotations

import json
import logging
import uuid
from abc import ABC, abstractmethod
from datetime import datetime, timezone

import boto3
import psycopg
from psycopg.rows import dict_row

from worker.eval.models import (
    BatchResult,
    Channel,
    Condition,
    ConditionLogic,
    EvaluationState,
    Location,
    MonitorConfig,
    NotificationEvent,
    Preferences,
    SeenThreat,
    Status,
    TimeWindow,
    WatchPoint,
)

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Repository Interface (07-eval-worker.md Section 4.2)
# ---------------------------------------------------------------------------


class Repository(ABC):
    """Abstract base for Eval Worker data access."""

    @abstractmethod
    def fetch_watchpoints(
        self, tile_id: str, page: int, page_size: int
    ) -> list[WatchPoint]:
        """Fetch active WatchPoints for a tile using index-only scan optimization.

        Parameters
        ----------
        tile_id : str
            Tile identifier (e.g., "3.4").
        page : int
            Zero-based page index for pagination.
        page_size : int
            Number of WatchPoints per page. If 0, fetch all.

        Returns
        -------
        list[WatchPoint]
            Active WatchPoints in the specified tile page.
        """
        ...

    @abstractmethod
    def fetch_states(self, wp_ids: list[str]) -> dict[str, EvaluationState]:
        """Fetch current evaluation state for a batch of WatchPoints.

        Parameters
        ----------
        wp_ids : list[str]
            WatchPoint IDs to fetch state for.

        Returns
        -------
        dict[str, EvaluationState]
            Mapping of watchpoint_id to its evaluation state.
            Missing entries indicate first evaluation (lazy init).
        """
        ...

    @abstractmethod
    def commit_batch(
        self,
        result: BatchResult,
        msg_timestamp: datetime,
        watchpoints: dict[str, WatchPoint],
    ) -> None:
        """Atomically commit evaluation results.

        Executes a single transaction:
        1. UPDATE ``watchpoint_evaluation_state`` (conditional on ``last_forecast_run``).
        2. INSERT INTO ``notifications`` (only for WPs whose state was updated).
        3. INSERT INTO ``notification_deliveries`` (with channel config snapshot).
        4. Publish ``NotificationEvent`` messages to SQS.

        Parameters
        ----------
        result : BatchResult
            The evaluation output containing state updates and notifications.
        msg_timestamp : datetime
            The ``run_timestamp`` from the EvalMessage. Used for stale write
            protection (CONC-006).
        watchpoints : dict[str, WatchPoint]
            Mapping of watchpoint_id -> WatchPoint for channel config snapshots.
        """
        ...


# ---------------------------------------------------------------------------
# SQL Constants
# ---------------------------------------------------------------------------

_FETCH_WATCHPOINTS_SQL = """\
SELECT
    id,
    organization_id,
    name,
    location_lat,
    location_lon,
    location_display_name,
    timezone,
    tile_id,
    time_window_start,
    time_window_end,
    monitor_config,
    conditions,
    condition_logic,
    channels,
    preferences,
    template_set,
    status,
    test_mode,
    tags,
    config_version,
    source,
    created_at,
    updated_at,
    archived_at,
    archived_reason
FROM watchpoints
WHERE tile_id = %s
  AND status = 'active'
  AND deleted_at IS NULL
ORDER BY id
"""

_FETCH_WATCHPOINTS_PAGINATED_SQL = _FETCH_WATCHPOINTS_SQL + """\
LIMIT %s OFFSET %s
"""

_FETCH_STATES_SQL = """\
SELECT
    watchpoint_id,
    config_version,
    last_evaluated_at,
    last_forecast_run,
    previous_trigger_state,
    trigger_state_changed_at,
    trigger_value,
    escalation_level,
    last_escalated_at,
    last_notified_at,
    last_notified_state,
    notification_count_24h,
    last_forecast_summary,
    last_digest_sent_at,
    last_digest_content,
    seen_threats,
    event_sequence
FROM watchpoint_evaluation_state
WHERE watchpoint_id = ANY(%s)
"""

# Stale Write Protection (CONC-006):
# Uses UPSERT (INSERT ... ON CONFLICT DO UPDATE) to handle both new and existing
# state rows. The ON CONFLICT clause includes the stale write protection condition:
# only update if last_forecast_run IS NULL or <= the new value.
# RETURNING watchpoint_id lets us know which rows were actually written.
_INSERT_STATE_SQL = """\
INSERT INTO watchpoint_evaluation_state (
    watchpoint_id,
    config_version,
    last_evaluated_at,
    last_forecast_run,
    previous_trigger_state,
    trigger_state_changed_at,
    trigger_value,
    escalation_level,
    last_escalated_at,
    last_notified_at,
    last_notified_state,
    notification_count_24h,
    last_forecast_summary,
    last_digest_sent_at,
    last_digest_content,
    seen_threats,
    event_sequence
) VALUES (
    %(watchpoint_id)s,
    %(config_version)s,
    %(last_evaluated_at)s,
    %(last_forecast_run)s,
    %(previous_trigger_state)s,
    %(trigger_state_changed_at)s,
    %(trigger_value)s,
    %(escalation_level)s,
    %(last_escalated_at)s,
    %(last_notified_at)s,
    %(last_notified_state)s,
    %(notification_count_24h)s,
    %(last_forecast_summary)s,
    %(last_digest_sent_at)s,
    %(last_digest_content)s,
    %(seen_threats)s,
    %(event_sequence)s
)
ON CONFLICT (watchpoint_id) DO UPDATE SET
    config_version = EXCLUDED.config_version,
    last_evaluated_at = EXCLUDED.last_evaluated_at,
    last_forecast_run = EXCLUDED.last_forecast_run,
    previous_trigger_state = EXCLUDED.previous_trigger_state,
    trigger_state_changed_at = EXCLUDED.trigger_state_changed_at,
    trigger_value = EXCLUDED.trigger_value,
    escalation_level = EXCLUDED.escalation_level,
    last_escalated_at = EXCLUDED.last_escalated_at,
    last_notified_at = EXCLUDED.last_notified_at,
    last_notified_state = EXCLUDED.last_notified_state,
    notification_count_24h = EXCLUDED.notification_count_24h,
    last_forecast_summary = EXCLUDED.last_forecast_summary,
    last_digest_sent_at = EXCLUDED.last_digest_sent_at,
    last_digest_content = EXCLUDED.last_digest_content,
    seen_threats = EXCLUDED.seen_threats,
    event_sequence = EXCLUDED.event_sequence
WHERE watchpoint_evaluation_state.last_forecast_run IS NULL
   OR watchpoint_evaluation_state.last_forecast_run <= EXCLUDED.last_forecast_run
RETURNING watchpoint_id
"""

_INSERT_NOTIFICATION_SQL = """\
INSERT INTO notifications (
    id,
    watchpoint_id,
    organization_id,
    event_type,
    urgency,
    payload,
    test_mode,
    template_set,
    created_at
) VALUES (%s, %s, %s, %s, %s, %s, %s, %s, %s)
"""

_INSERT_DELIVERY_SQL = """\
INSERT INTO notification_deliveries (
    id,
    notification_id,
    channel_type,
    channel_config,
    status,
    attempt_count
) VALUES (%s, %s, %s, %s, 'pending', 0)
"""


# ---------------------------------------------------------------------------
# Row -> Model Mapping Helpers
# ---------------------------------------------------------------------------


def _row_to_watchpoint(row: dict) -> WatchPoint:
    """Convert a database row (dict) to a WatchPoint model.

    Handles the mapping from flat DB columns to nested Pydantic models:
    - ``location_lat``, ``location_lon``, ``location_display_name`` -> ``Location``
    - ``time_window_start``, ``time_window_end`` -> ``TimeWindow``
    - JSONB columns (``conditions``, ``channels``, ``monitor_config``, ``preferences``)
      are parsed from their JSON/dict representations.
    """
    # Build Location from flat columns
    location = Location(
        lat=row["location_lat"],
        lon=row["location_lon"],
        display_name=row.get("location_display_name") or "",
    )

    # Build TimeWindow if present
    time_window = None
    if row.get("time_window_start") and row.get("time_window_end"):
        time_window = TimeWindow(
            start=row["time_window_start"],
            end=row["time_window_end"],
        )

    # Parse JSONB columns
    monitor_config = None
    mc_raw = row.get("monitor_config")
    if mc_raw is not None:
        if isinstance(mc_raw, str):
            mc_raw = json.loads(mc_raw)
        monitor_config = MonitorConfig(**mc_raw)

    conditions_raw = row.get("conditions", [])
    if isinstance(conditions_raw, str):
        conditions_raw = json.loads(conditions_raw)
    conditions = [Condition(**c) for c in conditions_raw]

    channels_raw = row.get("channels", [])
    if isinstance(channels_raw, str):
        channels_raw = json.loads(channels_raw)
    channels = [Channel(**ch) for ch in channels_raw]

    preferences = None
    prefs_raw = row.get("preferences")
    if prefs_raw is not None:
        if isinstance(prefs_raw, str):
            prefs_raw = json.loads(prefs_raw)
        preferences = Preferences(**prefs_raw)

    tags = row.get("tags") or []

    return WatchPoint(
        id=row["id"],
        organization_id=row["organization_id"],
        name=row.get("name") or "",
        location=location,
        timezone=row["timezone"],
        tile_id=row.get("tile_id") or "",
        time_window=time_window,
        monitor_config=monitor_config,
        conditions=conditions,
        condition_logic=ConditionLogic(row["condition_logic"]),
        channels=channels,
        template_set=row.get("template_set") or "default",
        preferences=preferences,
        status=Status(row["status"]),
        test_mode=row.get("test_mode", False),
        tags=tags,
        config_version=row.get("config_version", 1),
        source=row.get("source") or "",
        created_at=row.get("created_at"),
        updated_at=row.get("updated_at"),
        archived_at=row.get("archived_at"),
        archived_reason=row.get("archived_reason") or "",
    )


def _row_to_evaluation_state(row: dict) -> EvaluationState:
    """Convert a database row (dict) to an EvaluationState model.

    Handles parsing of JSONB columns (``seen_threats``, ``last_forecast_summary``,
    ``last_digest_content``).
    """
    seen_threats_raw = row.get("seen_threats") or []
    if isinstance(seen_threats_raw, str):
        seen_threats_raw = json.loads(seen_threats_raw)
    seen_threats = [SeenThreat(**st) for st in seen_threats_raw]

    last_forecast_summary = row.get("last_forecast_summary")
    if isinstance(last_forecast_summary, str):
        last_forecast_summary = json.loads(last_forecast_summary)

    last_digest_content = row.get("last_digest_content")
    if isinstance(last_digest_content, str):
        last_digest_content = json.loads(last_digest_content)

    return EvaluationState(
        watchpoint_id=row["watchpoint_id"],
        config_version=row.get("config_version", 0),
        last_evaluated_at=row.get("last_evaluated_at"),
        last_forecast_run=row.get("last_forecast_run"),
        previous_trigger_state=row.get("previous_trigger_state", False),
        trigger_state_changed_at=row.get("trigger_state_changed_at"),
        trigger_value=row.get("trigger_value"),
        escalation_level=row.get("escalation_level", 0),
        last_escalated_at=row.get("last_escalated_at"),
        last_notified_at=row.get("last_notified_at"),
        last_notified_state=row.get("last_notified_state"),
        notification_count_24h=row.get("notification_count_24h", 0),
        last_forecast_summary=last_forecast_summary,
        last_digest_sent_at=row.get("last_digest_sent_at"),
        last_digest_content=last_digest_content,
        seen_threats=seen_threats,
        event_sequence=row.get("event_sequence", 0),
    )


def _state_to_params(
    state: EvaluationState, msg_timestamp: datetime
) -> dict:
    """Convert an EvaluationState to a parameter dict for SQL execution.

    Serializes JSONB fields (``seen_threats``, ``last_forecast_summary``,
    ``last_digest_content``) to JSON strings for psycopg.
    """
    seen_threats_json = json.dumps(
        [st.model_dump(mode="json") for st in state.seen_threats]
    )
    last_forecast_summary_json = (
        json.dumps(state.last_forecast_summary)
        if state.last_forecast_summary is not None
        else None
    )
    last_digest_content_json = (
        json.dumps(state.last_digest_content)
        if state.last_digest_content is not None
        else None
    )

    return {
        "watchpoint_id": state.watchpoint_id,
        "config_version": state.config_version,
        "last_evaluated_at": state.last_evaluated_at,
        "last_forecast_run": state.last_forecast_run,
        "previous_trigger_state": state.previous_trigger_state,
        "trigger_state_changed_at": state.trigger_state_changed_at,
        "trigger_value": state.trigger_value,
        "escalation_level": state.escalation_level,
        "last_escalated_at": state.last_escalated_at,
        "last_notified_at": state.last_notified_at,
        "last_notified_state": state.last_notified_state,
        "notification_count_24h": state.notification_count_24h,
        "last_forecast_summary": last_forecast_summary_json,
        "last_digest_sent_at": state.last_digest_sent_at,
        "last_digest_content": last_digest_content_json,
        "seen_threats": seen_threats_json,
        "event_sequence": state.event_sequence,
        "msg_timestamp": msg_timestamp,
    }


# ---------------------------------------------------------------------------
# Concrete Implementation: PostgresRepository
# ---------------------------------------------------------------------------


class PostgresRepository(Repository):
    """PostgreSQL-backed Repository using psycopg v3.

    Manages database interactions for the Eval Worker using a connection
    string (DSN) approach. Connection pooling is handled externally by
    PgBouncer (Supabase Transaction Pooler).

    Parameters
    ----------
    conninfo : str
        PostgreSQL connection string (DSN).
    sqs_client : boto3 SQS client
        Pre-configured boto3 SQS client for publishing notifications.
    queue_url : str
        SQS queue URL for the notification queue.
    """

    def __init__(
        self,
        conninfo: str,
        sqs_client: object,
        queue_url: str,
    ) -> None:
        self._conninfo = conninfo
        self._sqs_client = sqs_client
        self._queue_url = queue_url

    def _connect(self) -> psycopg.Connection:
        """Create a new database connection.

        Uses ``row_factory=dict_row`` for convenient dict-based row access.
        ``autocommit=False`` is the default; we manage transactions explicitly.
        """
        return psycopg.connect(
            self._conninfo,
            row_factory=dict_row,
            autocommit=False,
        )

    def fetch_watchpoints(
        self, tile_id: str, page: int, page_size: int
    ) -> list[WatchPoint]:
        """Fetch active WatchPoints for a tile.

        Uses the ``idx_watchpoints_tile_active`` index for efficient lookups.
        Results are ordered by ``id`` for deterministic pagination.

        Per EVAL-002: Pagination uses LIMIT/OFFSET. OFFSET is acceptable
        here because max practical pages is ~10-20.
        """
        with self._connect() as conn:
            with conn.cursor() as cur:
                if page_size > 0:
                    offset = page * page_size
                    cur.execute(
                        _FETCH_WATCHPOINTS_PAGINATED_SQL,
                        (tile_id, page_size, offset),
                    )
                else:
                    cur.execute(_FETCH_WATCHPOINTS_SQL, (tile_id,))

                rows = cur.fetchall()

        return [_row_to_watchpoint(row) for row in rows]

    def fetch_states(self, wp_ids: list[str]) -> dict[str, EvaluationState]:
        """Fetch evaluation states for the given WatchPoint IDs.

        Returns a dict mapping ``watchpoint_id`` -> ``EvaluationState``.
        Missing entries indicate first evaluation (the evaluator will
        create default state via lazy initialization).
        """
        if not wp_ids:
            return {}

        with self._connect() as conn:
            with conn.cursor() as cur:
                cur.execute(_FETCH_STATES_SQL, (wp_ids,))
                rows = cur.fetchall()

        return {
            row["watchpoint_id"]: _row_to_evaluation_state(row)
            for row in rows
        }

    def commit_batch(
        self,
        result: BatchResult,
        msg_timestamp: datetime,
        watchpoints: dict[str, WatchPoint],
    ) -> None:
        """Atomically commit evaluation results.

        Transaction flow:
        1. For each state update, execute either INSERT (new state) or
           UPDATE (existing) with stale write protection (CONC-006).
        2. Collect the set of WatchPoint IDs that were actually written.
        3. For notifications whose WatchPoint ID is in the written set,
           INSERT into ``notifications`` and ``notification_deliveries``.
        4. After the DB transaction commits, publish NotificationEvents to SQS.

        The SQS publish happens AFTER the DB commit to ensure we never
        publish notifications for data that was rolled back. If SQS publish
        fails after DB commit, the notification records exist in the DB
        with ``pending`` deliveries, enabling recovery via a sweep job.

        Raises
        ------
        Exception
            Any database or SQS error is propagated after logging.
        """
        if not result.state_updates and not result.notifications:
            logger.debug("commit_batch: nothing to commit")
            return

        # Build a lookup for which WP IDs have notifications
        notif_by_wp: dict[str, list[NotificationEvent]] = {}
        for notif in result.notifications:
            notif_by_wp.setdefault(notif.watchpoint_id, []).append(notif)

        # Track which WP state updates actually succeeded (stale write protection)
        updated_wp_ids: set[str] = set()

        # Notifications to publish to SQS after successful DB commit
        sqs_messages: list[NotificationEvent] = []

        with self._connect() as conn:
            with conn.transaction():
                cur = conn.cursor()
                try:
                    # Step 1: Update/Insert evaluation states
                    for state in result.state_updates:
                        params = _state_to_params(state, msg_timestamp)
                        # Use UPSERT to handle both new and existing states.
                        # The ON CONFLICT clause includes the stale write
                        # protection condition.
                        cur.execute(_INSERT_STATE_SQL, params)
                        returned = cur.fetchall()
                        if returned:
                            updated_wp_ids.add(returned[0]["watchpoint_id"])
                            logger.debug(
                                "State updated for wp=%s",
                                state.watchpoint_id,
                            )
                        else:
                            logger.info(
                                "Stale write skipped for wp=%s "
                                "(last_forecast_run > msg_timestamp=%s)",
                                state.watchpoint_id,
                                msg_timestamp.isoformat(),
                            )

                    # Step 2: Insert notifications only for successfully updated WPs
                    for notif in result.notifications:
                        if notif.watchpoint_id not in updated_wp_ids:
                            logger.info(
                                "Skipping notification %s for wp=%s "
                                "(state update was stale)",
                                notif.id,
                                notif.watchpoint_id,
                            )
                            continue

                        # Get the WatchPoint for channel config snapshot
                        wp = watchpoints.get(notif.watchpoint_id)

                        # Get template_set from WatchPoint
                        template_set = wp.template_set if wp else "default"

                        # Serialize payload to JSON
                        payload_json = json.dumps(notif.payload)

                        cur.execute(
                            _INSERT_NOTIFICATION_SQL,
                            (
                                notif.id,
                                notif.watchpoint_id,
                                notif.organization_id,
                                notif.event_type,
                                notif.urgency,
                                payload_json,
                                notif.test_mode,
                                template_set,
                                notif.created_at,
                            ),
                        )

                        # Step 3: Insert notification deliveries with channel
                        # config snapshot for each enabled channel
                        if wp and wp.channels:
                            for channel in wp.channels:
                                if not channel.enabled:
                                    continue
                                delivery_id = f"del_{uuid.uuid4()}"
                                # Snapshot the full channel config at this
                                # point in time. This includes secrets, URLs,
                                # and settings. Per spec: "snapshot of the
                                # channel configuration from the WatchPoint
                                # object at evaluation time."
                                channel_config_snapshot = json.dumps(
                                    channel.model_dump(mode="json")
                                )
                                cur.execute(
                                    _INSERT_DELIVERY_SQL,
                                    (
                                        delivery_id,
                                        notif.id,
                                        channel.type,
                                        channel_config_snapshot,
                                    ),
                                )

                        # Mark this notification for SQS publishing
                        sqs_messages.append(notif)

                finally:
                    cur.close()

        # Step 4: Publish to SQS AFTER DB commit
        # If SQS fails, the DB records exist with 'pending' delivery status,
        # enabling recovery. We log and raise so the caller can handle retry.
        is_fifo = _is_fifo_queue(self._queue_url)
        for notif in sqs_messages:
            try:
                message_body = notif.model_dump_json()
                send_kwargs: dict = {
                    "QueueUrl": self._queue_url,
                    "MessageBody": message_body,
                }
                if is_fifo:
                    send_kwargs["MessageGroupId"] = notif.watchpoint_id
                self._sqs_client.send_message(**send_kwargs)
                logger.info(
                    "Published notification %s to SQS for wp=%s",
                    notif.id,
                    notif.watchpoint_id,
                )
            except Exception:
                logger.exception(
                    "Failed to publish notification %s to SQS for wp=%s. "
                    "DB records exist with 'pending' delivery status.",
                    notif.id,
                    notif.watchpoint_id,
                )
                raise


def _is_fifo_queue(queue_url: str) -> bool:
    """Check if a queue URL indicates a FIFO queue."""
    return queue_url.endswith(".fifo")
