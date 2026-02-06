# 07 - Evaluation Worker

> **Purpose**: Defines the Python-based Lambda function responsible for reading heavy forecast data (Zarr), evaluating user conditions against that data, and queueing notifications.
> **Package**: `worker/eval` (Python 3.11)
> **Dependencies**: `01-foundation-types.md`, `02-foundation-db.md`, `03-config.md`, `06-batcher.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Schema Extensions (Migrations)](#2-schema-extensions-migrations)
3. [Domain Models (Pydantic)](#3-domain-models-pydantic)
4. [Data Access Interfaces](#4-data-access-interfaces)
5. [Core Logic: Evaluator](#5-core-logic-evaluator)
6. [Lambda Handler & Execution](#6-lambda-handler--execution)
7. [Configuration & Environment](#7-configuration--environment)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

The Evaluation Worker is the compute engine of the platform. Unlike the Go-based API and Batcher, this worker is written in **Python** to leverage the rich ecosystem of scientific computing libraries (`xarray`, `zarr`, `numpy`) required for efficient meteorological data processing.

### Responsibilities
*   **Data Ingestion**: Reading slice-and-dice Zarr arrays from S3 with geospatial precision.
*   **Evaluation**: Checking WatchPoint conditions (Event Mode) or rolling windows (Monitor Mode) against forecast data.
*   **State Management**: Atomically updating evaluation state (including event sequencing) and queuing notifications.
*   **Resilience**: Handling partial batch failures and graceful timeouts within Lambda constraints.

---

## 2. Schema Extensions (Migrations)

*All schema extensions are consolidated in `02-foundation-db.md` (Section 6.5).*

The `event_sequence` column on `watchpoint_evaluation_state` table enables reliable ordering of notifications for webhooks delivered via standard SQS.

---

## 3. Domain Models (Pydantic)

Since this component uses Python, we define strict Pydantic `v2` models that mirror the Go structs defined in `01-foundation-types.md` and `06-batcher.md`.

*Cross-language synchronization rules are defined in `01-foundation-types.md` (Section 12).*

### 3.1 Input: Evaluation Message

Defined in `06-batcher.md`, received via SQS.

```python
from enum import Enum
from datetime import datetime
from pydantic import BaseModel, Field

class ForecastType(str, Enum):
    MEDIUM_RANGE = "medium_range"
    NOWCAST = "nowcast"

class EvalMessage(BaseModel):
    batch_id: str
    trace_id: str
    forecast_type: ForecastType
    run_timestamp: datetime
    tile_id: str
    page: int = 0
    page_size: int = 0
    total_items: int = 0
    # If populated, the worker must filter execution to only these IDs.
    # Used for immediate Resume/Update triggers.
    specific_watchpoint_ids: list[str] = []
```

### 3.2 Output: Notification Event

Sent to the Notification Queue (`08a-notification-core.md`).

```python
class NotificationOrdering(BaseModel):
    event_sequence: int
    forecast_timestamp: datetime
    evaluation_timestamp: datetime

class NotificationEvent(BaseModel):
    id: str  # "notif_..."
    watchpoint_id: str
    organization_id: str
    trace_id: str  # Propagated from EvalMessage for distributed tracing

    event_type: str  # e.g., "threshold_crossed"
    urgency: str     # e.g., "critical"

    # Snapshot of data at time of trigger
    payload: dict

    # Ordering metadata for client-side sorting
    ordering: NotificationOrdering

    test_mode: bool = False
    created_at: datetime = Field(default_factory=datetime.utcnow)
```

### 3.3 Domain Entities

Mirrors `01-foundation-types.md`.

```python
class Condition(BaseModel):
    variable: str
    operator: str
    threshold: float | list[float] # List used for "between"
    unit: str

class WatchPoint(BaseModel):
    id: str
    organization_id: str
    location_lat: float
    location_lon: float
    # ... other fields ...
    conditions: list[Condition]
    condition_logic: str # "ANY" or "ALL"
    config_version: int
    test_mode: bool
    monitor_config: dict | None = None # If present, Monitor Mode

class EvaluationState(BaseModel):
    watchpoint_id: str
    config_version: int
    previous_trigger_state: bool
    escalation_level: int
    event_sequence: int
    # List of dicts: [{"start": datetime, "end": datetime, "type": str}]
    seen_threats: list[dict] = []
    last_evaluated_at: datetime | None = None
    last_forecast_run: datetime | None = None
```

---

## 4. Data Access Interfaces

### 4.1 Forecast Reader (Zarr Abstraction)

Abstracts S3 I/O and handles the complexity of "Halo Regions" (fetching data from adjacent tiles to allow interpolation at the edges).

```python
import xarray as xr

class TileData:
    """
    Wrapper around xarray/numpy data for a specific geospatial tile.

    Implementations MUST convert the forecast time index from UTC to the
    WatchPoint's local timezone *before* applying Active Hours filtering.

    Note: Data assumes Write-Side Halo (1px padding). Readers should fetch
    exactly one chunk per tile.
    """

    def __init__(self, dataset: xr.Dataset):
        self.ds = dataset

    def extract_point(self, lat: float, lon: float) -> dict[str, float]:
        """
        Performs bilinear interpolation.
        Returns dictionary of canonical variables (e.g., {'temperature_c': 24.5}).
        Handles variable name mapping (e.g., 't2m' -> 'temperature_c').
        """
        pass

class ValidationResult(BaseModel):
    valid: bool
    errors: list[str]

class SchemaValidator(ABC):
    @abstractmethod
    def validate(self, dataset: xr.Dataset) -> ValidationResult:
        """Checks for required variables, dimensions, and NaN values."""
        pass

class ForecastReader(ABC):
    @abstractmethod
    def load_tile(self,
                  model: ForecastType,
                  timestamp: datetime,
                  tile_id: str) -> TileData:
        """
        Fetches Zarr chunk from S3.
        MUST detect if WatchPoints require halo data (edge of tile)
        and fetch adjacent slices if necessary using xarray.sel(method='nearest').
        MUST run SchemaValidator before returning.

        Error Handling: If the Zarr store/key is missing (FileNotFoundError),
        the Reader MUST raise a specific ForecastExpiredError. The Worker handler
        MUST catch this, log a warning (not error), and successfully acknowledge
        the SQS message to prevent DLQ loops.

        Data Corruption Handling: If Zarr data is corrupt (Checksum Error or Format
        Error detected during deserialization), the Reader MUST:
          1. Log a critical error with full context (tile_id, timestamp, error details)
          2. Emit a `CorruptForecast` metric to CloudWatch for alerting
          3. Raise a terminal `ForecastCorruptError` exception
        The Worker handler MUST catch `ForecastCorruptError` and ACK the SQS message
        (removing it from the queue) to prevent infinite retry loops. Corrupt data
        requires manual intervention or rebuild and cannot be recovered by retrying.
        """
        pass

class ForecastExpiredError(Exception):
    """Raised when the forecast data has been deleted or is unavailable."""
    pass

class ForecastCorruptError(Exception):
    """Raised when the forecast data is corrupt (checksum or format error)."""
    pass
```

### 4.2 Repository (Atomic Persistence)

Manages database interactions using a "Unit of Work" pattern.

```python
class BatchResult(BaseModel):
    state_updates: list[EvaluationState]
    notifications: list[NotificationEvent]

class Repository(ABC):
    @abstractmethod
    def fetch_watchpoints(self, tile_id: str, page: int, page_size: int) -> list[WatchPoint]:
        """Fetches active WPs for the tile using index-only scan optimization."""
        pass

    @abstractmethod
    def fetch_states(self, wp_ids: list[str]) -> dict[str, EvaluationState]:
        """Fetches current evaluation state for the batch."""
        pass

    @abstractmethod
    def commit_batch(self, result: BatchResult):
        """
        Executes a single transaction:
        1. UPDATE watchpoint_evaluation_state SET event_sequence = ?, ...
        2. INSERT INTO notifications ...
        3. INSERT INTO notification_deliveries ...

        **Event Sequence Logic**: The `event_sequence` column in `watchpoint_evaluation_state`
        MUST ONLY be incremented when a row is inserted into the `notifications` table. It
        MUST NOT be incremented for evaluations that result in no notification. This ensures
        the sequence number represents a gap-free timeline of alerts for the client.

        **Stale Write Protection**: The `UPDATE watchpoint_evaluation_state` query MUST include
        the condition: `AND (last_forecast_run IS NULL OR last_forecast_run <= $msg_ts)`. This
        prevents out-of-order messages (e.g., from SQS retries) from overwriting state written
        by a newer forecast run. The method MUST capture the IDs of rows actually updated using
        `RETURNING watchpoint_id` and filter the subsequent `INSERT INTO notifications` to ONLY
        those IDs. This prevents stale messages from triggering duplicate alerts.

        **Channel Config Snapshot**: The `INSERT INTO notification_deliveries` statement MUST
        populate the `channel_config` JSONB column with a **snapshot** of the channel configuration
        from the `WatchPoint` object at evaluation time, not a reference or foreign key. This
        ensures delivery workers have the exact config (including secrets, URLs, and settings)
        that was valid when the notification was created, even if the user modifies the channel
        configuration between evaluation and delivery.
        """
        pass
```

---

## 5. Core Logic: Evaluator

The `Evaluator` determines if alerts should be sent. It handles both Event Mode (point-in-time) and Monitor Mode (time-series).

### 5.1 Condition Logic

Implementation of the logical operators defined in `01-foundation-types.md`.

```python
def check_condition(actual: float, c: Condition) -> bool:
    if c.operator == ">":
        return actual > c.threshold
    elif c.operator == ">=":
        return actual >= c.threshold
    elif c.operator == "<":
        return actual < c.threshold
    elif c.operator == "<=":
        return actual <= c.threshold
    elif c.operator == "==":
        return abs(actual - c.threshold) < 0.001 # Float equality safety
    elif c.operator == "between":
        # Threshold is [min, max]
        return c.threshold[0] <= actual <= c.threshold[1]
    return False
```

### 5.2 Evaluator Interface & Monitor Mode

```python
class EvaluationContext(BaseModel):
    watchpoint: WatchPoint
    state: EvaluationState
    forecast_snapshot: dict

class Evaluator(ABC):
    @abstractmethod
    def evaluate_batch(
        self,
        wps: list[WatchPoint],
        states: dict[str, EvaluationState],
        tile_data: TileData,
        timeout_guard: 'TimeoutGuard',
        trace_id: str
    ) -> BatchResult:
        """
        Orchestrates evaluation.

        **Context Propagation**: The Evaluator MUST copy the `trace_id` from the incoming
        `EvalMessage` into every generated `NotificationEvent`. This ensures that X-Ray
        traces or correlation IDs are preserved across the SQS boundary.

        **Granular Idempotency**: Idempotency checks (comparing `last_forecast_run` against
        the message timestamp) MUST be performed **individually** for each WatchPoint within
        the batch loop, not at the global message level. This ensures that retrying a
        partially-successful page (e.g., due to timeout) correctly processes pending items
        without duplicating side-effects for already-completed items.

        **Targeted Evaluation Filtering**:
          If `msg.specific_watchpoint_ids` is not empty, filtering MUST be applied
          immediately after fetching WatchPoints for the tile. Discard WatchPoints
          not in the list before evaluation.

        **Test Mode Propagation**:
          When constructing `NotificationEvent` output payloads, the evaluator MUST map
          `WatchPoint.TestMode` to `NotificationEvent.test_mode`. This ensures downstream
          workers correctly identify and suppress delivery for test resources.

        Logic for Event Mode:
          1. Extract single point from TileData.
          2. Check conditions.
          3. If State Change: Generate Notification (with `test_mode` field set from `WatchPoint.TestMode`).

        Logic for Monitor Mode:
          1. Extract time-series from TileData (Rolling Window).
          2. Filter by Active Hours/Days.
          3. Find contiguous blocks where conditions are met.
          4. **Temporal Overlap Detection** (Deduplication): Compare identified
             violation periods against `seen_threats`. Overlapping periods are
             treated as the same threat. Prune entries where `end_time` is older
             than the rolling window.
          5. If New Threat: Generate Notification.

        First Evaluation Rule:
          On First Evaluation (`state.last_evaluated_at` is None), if Mode is Monitor:
          - **Suppress notification** (do not generate alert)
          - Persist the `MonitorSummary` baseline to `last_forecast_summary`
          This establishes a baseline to compare against for future evaluations,
          preventing false alerts on initial setup.

        **Lazy State Initialization**:
          The Worker is the sole authority for creating `watchpoint_evaluation_state` records.
          If state is missing during fetch, instantiate default state in memory and mark
          for insertion during commit.

        Hysteresis: When evaluating 'Clear' conditions, implementations MUST apply
        the Hysteresis Constants defined in `01-foundation-types.md` (e.g.,
        threshold - 10%) to prevent alert flapping.

        **Trigger Persistence**: When a WatchPoint transitions to `Triggered` state, the
        worker MUST store the `ActualValue` of the primary condition into the `trigger_value`
        column in `watchpoint_evaluation_state`.

        **Clearance Payload**: When generating a `threshold_cleared` notification, the
        Evaluator MUST populate the `PreviousValue` field in `ConditionResult` using the
        stored `trigger_value`. After generating the notification, it MUST reset
        `trigger_value` to NULL. Example: "Wind dropped from 80km/h to 20km/h".

        **Escalation Updates**: When triggering an escalation notification, the worker MUST
        update `escalation_level` AND `last_escalated_at` in `watchpoint_evaluation_state`.
        Cooldown checks for escalation (2-hour minimum) must reference `last_escalated_at`,
        not `last_notified_at`, to enforce escalation timing independently.

        **Escalation Tier Logic**: The logic for calculating `escalation_level` is hardcoded:
          - **Level 0 (Initial)**: Condition met.
          - **Level 1 (Elevated)**: Value exceeds threshold by ≥ 25%.
          - **Level 2 (Severe)**: Value exceeds threshold by ≥ 50%.
          - **Level 3 (Critical)**: Value exceeds threshold by ≥ 100%.
          Formula: `overage = (actual - threshold) / threshold`.
        """
        pass
```

### 5.3 Config Policy (Race Conditions)

Handles race conditions where a user updates a WatchPoint while evaluation is in flight.

```python
class ConfigPolicy(ABC):
    def resolve(self, wp: WatchPoint, state: EvaluationState, current_tile_id: str) -> str:
        """
        Handles config version mismatches between WatchPoint and EvaluationState.

        Logic:
        1. If versions mismatch, the worker MUST calculate the `tile_id` for the
           WatchPoint's current location using the same formula as the DB generated column.
        2. If the calculated tile_id differs from `current_tile_id` (the tile being processed),
           **abort evaluation** and update ONLY the DB state's config_version (Fast Fail).
           This prevents using loaded forecast data for the wrong geographic region.
        3. If tile matches but versions differ: Return "RESET" (clear hysteresis, re-eval).
        4. If versions match: Return "CONTINUE" (apply hysteresis/suppression normally).

        **State Preservation on ABORT/RESET**: When resetting state due to Version/Tile
        mismatch, the worker MUST **preserve** the `seen_threats` JSONB array. Clearing
        this history would break temporal deduplication and cause 'Alert Shock' (duplicate
        notifications for ongoing threats). Only `previous_trigger_state`, `trigger_value`,
        and `escalation_level` should be reset. The `seen_threats` array maintains the
        threat timeline across configuration changes.

        Returns:
            "ABORT" - Tile mismatch, skip this WatchPoint entirely
            "RESET" - Version changed but same tile, re-evaluate from scratch
            "CONTINUE" - Normal evaluation with hysteresis
        """
        pass
```

---

## 6. Lambda Handler & Execution

The handler manages the lifecycle, error handling, and partial batch responses for SQS.

### 6.1 Timeout Guard

Prevents the worker from being killed mid-batch by AWS Lambda, which would cause the entire batch to be retried (and potentially duplicate notifications).

```python
class TimeoutGuard:
    def __init__(self, context: Any):
        self._context = context

    def check_remaining(self):
        """
        Raises TimeBudgetExceededError if < 5 seconds remain.
        """
        if self._context.get_remaining_time_in_millis() < 5000:
            raise TimeBudgetExceededError("Lambda timeout imminent")
```

### 6.2 Handler Structure

```python
class SQSBatchItemFailure(BaseModel):
    itemIdentifier: str

class SQSBatchResponse(BaseModel):
    batchItemFailures: list[SQSBatchItemFailure]

class EvalWorker:
    def __init__(self):
        # Initialize DB pool, S3 client, etc.
        self.repo = PostgresRepository(...)
        self.reader = ZarrReader(...)
        self.evaluator = StandardEvaluator(...)

    def handler(self, event: dict, context: Any) -> dict:
        """
        1. Parse SQS Batch (extract messages).
        2. Group messages by Tile ID (optimization: load tile once if SQS batches align).
        3. Iterate messages:
           a. try: process_tile()
           b. catch TimeBudgetExceeded: Mark remaining items in SQS batch as failed to retry.
           c. catch Exception: Mark specific item as failed.
        4. Return SQSBatchResponse.
        """
        pass

    def process_tile(self, msg: EvalMessage, context: Any):
        """
        **Action Routing**:
        The handler MUST check `msg.action` and route to the appropriate component:
        - If `msg.action == 'generate_summary'`: Route to `SummaryGenerator`.
        - Otherwise (default 'evaluate'): Route to standard `Evaluator`.

        Idempotency Logic: Before evaluating, check if
        `watchpoint_evaluation_state.last_forecast_run` equals the current
        message's timestamp. If match found, check `notifications` table for
        recent entry. If notification exists, **re-publish** the existing
        payload to SQS (if needed) but **DO NOT** increment event sequence
        or update DB state.

        1. Load Tile Data (Reader).
        2. Fetch WPs and State (Repo).
        3. Evaluate (Evaluator) OR Generate Summary (SummaryGenerator).
        4. Commit (Repo) - Updates DB and queues Notification SQS.

        Telemetry Requirement:
        Worker MUST emit `MetricEvaluationLag` (Now - ForecastTimestamp) for every
        tile processed. This metric tracks how far behind real-time the evaluation
        pipeline is running, enabling alerting on processing delays.

        Example:
            lag_seconds = (datetime.utcnow() - msg.run_timestamp).total_seconds()
            metrics.put_metric(MetricEvaluationLag, lag_seconds, unit='Seconds',
                              dimensions={'ForecastType': msg.forecast_type})
        """
        pass

class SummaryGenerator:
    """
    Generates post-event accuracy summaries for archived WatchPoints.

    Invoked when `msg.action == 'generate_summary'`. This component uses the
    worker's scientific libraries (xarray, numpy) to compute accuracy metrics
    by comparing historical forecast data against observed data.

    **Responsibilities**:
    1. Fetch historical forecast Zarr data for the WatchPoint's location and time window.
    2. Fetch observed/verification data (NOAA ISD/MRMS) for the same period.
    3. Compute accuracy metrics:
       - For precipitation: Brier score, hit/miss rate
       - For temperature: RMSE, bias
       - For timing: Lead time accuracy (hours early/late)
    4. Persist the computed summary to `watchpoints.summary` JSONB column.

    **Summary Schema** (stored in `watchpoints.summary`):
    ```json
    {
      "generated_at": "2026-02-03T12:00:00Z",
      "forecast_accuracy": {
        "precipitation_prob": {"brier_score": 0.15, "predicted": 75, "actual_occurred": true},
        "temperature_c": {"rmse": 2.1, "bias": -0.5, "predicted": 28.5, "actual": 26.4}
      },
      "timing_accuracy": {
        "predicted_peak_hour": 14,
        "actual_peak_hour": 15,
        "lead_time_error_hours": 1
      },
      "conditions_evaluated": [
        {"variable": "precipitation_probability", "threshold": 70, "predicted": 75, "actual": 82, "correct": true}
      ]
    }
    ```
    """

    def generate(self, watchpoint_ids: list[str], tile_data: TileData) -> list[dict]:
        """
        Generates summaries for the specified WatchPoints.

        Args:
            watchpoint_ids: List of archived WatchPoint IDs to process.
            tile_data: Loaded forecast data for the tile.

        Returns:
            List of dicts mapping watchpoint_id to computed summary.
        """
        pass
```

---

## 7. Configuration & Environment

To maintain parity with the Go services (`03-config.md`), the Python worker must intercept environment variable loading to resolve SSM parameters.

### 7.1 Python Requirements

**File**: `requirements.txt`
```text
# Data Science Stack
numpy>=1.24.0
xarray>=2023.0.0
zarr>=2.16.0
s3fs>=2023.6.0
scipy>=1.11.0
pandas>=2.0.0

# Infrastructure
boto3>=1.28.0
psycopg[binary]>=3.1.0
pydantic>=2.0.0
pydantic-settings>=2.0.0
aws-lambda-powertools>=2.0.0  # Structured logging & metrics
```

> **Implementation Note (Phase 9 — Integration Testing):**
> `scipy` was added as an explicit dependency. It is required at runtime by
> `xarray.Dataset.interp()` (used in `TileData.extract_point` for bilinear
> interpolation). Without it, the eval worker fails with
> `ModuleNotFoundError: No module named 'scipy'` when processing any
> WatchPoint whose coordinates do not land exactly on a grid cell. This
> dependency was implicit in the original specification — the architecture
> correctly calls for bilinear interpolation (Section 4.1) but the
> requirements listing did not include the library that implements it.
>
> Additionally, code that reads or writes Zarr stores must handle both
> Zarr v2 and v3 APIs, as `zarr>=2.16.0` permits installation of Zarr 3.x,
> which has breaking changes to the codec and timestamp handling APIs.
> The forecast seeder and monitor evaluator were updated during integration
> testing to be backward-compatible with both versions.

### 7.2 Settings Loader

```python
from pydantic_settings import BaseSettings
from pydantic import SecretStr

class Settings(BaseSettings):
    database_url: SecretStr
    forecast_bucket: str
    queue_url: str
    aws_region: str = "us-east-1"
    
    # Feature Flags
    enable_threat_dedup: bool = True

def load_settings() -> Settings:
    """
    1. Check APP_ENV.
    2. If != 'local', scan env vars for *_SSM_PARAM suffix.
    3. Batch fetch secrets from AWS SSM.
    4. Inject into Settings object.
    """
    pass
```

---

## 8. Flow Coverage

| Flow ID | Description | Implementation |
|---|---|---|
| `EVAL-001` | Evaluation Pipeline | `EvalWorker.process_tile` -> `Evaluator.evaluate_batch` |
| `EVAL-002` | Hot Tile Pagination | Handled via `EvalMessage.page/page_size` passed to `Repo` |
| `EVAL-003` | Monitor Mode | `Evaluator` checks `wp.monitor_config` -> uses `ThreatDedup` |
| `EVAL-004` | Config Mismatch | `ConfigPolicy.resolve` resets state on version mismatch |
| `IMPL-004` | Notification Ordering | `event_sequence` incremented in `Repository.commit_batch` |
| `FAIL-008` | Zarr Corruption | `SchemaValidator` raises error -> Item marked failed in SQS batch |
| `FAIL-011` | Lambda Timeout | `TimeoutGuard` triggers graceful abort, remaining items retried |