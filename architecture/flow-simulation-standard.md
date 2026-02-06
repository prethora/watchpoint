# Flow Simulation Standard

> **Purpose**: Defines the strict format and level of detail required for simulating architectural flows. These simulations are not high-level descriptions; they are **low-level code execution paths** used to verify architectural completeness and identify gaps.

---

## 1. Simulation Structure

Each flow simulation must adhere to the following structure.

### 1.1 Header
*   **ID**: The Flow ID (e.g., `FCST-001`).
*   **Name**: The descriptive name of the flow.
*   **Primary Components**: List of architectural packages/functions involved (e.g., `cmd/data-poller`, `worker/runpod`, `package db`).

### 1.2 Trigger & Context
*   **Trigger**: The exact event that initiates the flow (e.g., `EventBridge Rule: rate(15 minutes)`, `HTTP POST /v1/watchpoints`, `S3 Event: ObjectCreated`).
*   **Preconditions**: State that must exist for the flow to proceed (e.g., "User is authenticated", "Upstream GFS data exists in S3").

### 1.3 Execution Sequence
A numbered list of atomic steps representing the code execution path.
*   **Format**: `[Component] Action`
*   **Detail Level**:
    *   **Good**: "1. `DataPoller` calls `RunRepository.GetLatest(ctx, model)`."
    *   **Bad**: "1. The system checks for new data."
*   **Logic Branches**: Briefly note critical if/else conditions (e.g., "If `last_run > upstream_time`, abort").

### 1.4 Data State Changes
Explicitly list every read and mutation to persistent storage.
*   **Database**: `INSERT INTO table (col1, col2)`, `UPDATE table SET ...`, `SELECT ...`
*   **Storage (S3)**: `PutObject`, `GetObject`, `ListObjects`
*   **Queues (SQS)**: `SendMessage`, `DeleteMessage`
*   **Cache**: `Set`, `Get`

### 1.5 External Interactions
List API calls to third-party services (Stripe, SendGrid, RunPod).
*   **Request**: Key data sent.
*   **Response**: Key data received.

### 1.6 Error Paths
Define handling for **known/expected** failures.
*   *Example*: "If RunPod returns 500: Increment `retry_count` in DB, do not delete SQS message."

### 1.7 Gap Analysis (CRITICAL)
This section is the primary output of the simulation exercise. It lists discrepancies between the flow requirements and the current architecture documents.
*   **Missing Schema**: Columns or tables required by the logic but missing in `02-foundation-db.md`.
*   **Missing Config**: Environment variables or secrets missing in `03-config.md`.
*   **Logic Holes**: Infinite loops, race conditions, or undefined behaviors.
*   **Type Mismatches**: Go struct vs. JSON payload discrepancies.

---

## 2. Example Simulation

### FCST-XXX: Example Forecast Flow

**Primary Components**: `cmd/data-poller`, `internal/db`

**Trigger**: EventBridge Schedule (15 mins)

**Execution Sequence**:
1.  **[DataPoller]** Main handler starts. Loads config `UPSTREAM_BUCKET`.
2.  **[DataPoller]** Calls `RunRepository.GetLatest(ctx, "medium_range")`.
3.  **[DB]** Executes `SELECT * FROM forecast_runs WHERE model='medium_range' ORDER BY run_timestamp DESC LIMIT 1`.
4.  **[DataPoller]** Compares DB timestamp (`T_db`) with Upstream S3 timestamp (`T_up`).
5.  **[Logic]** If `T_up > T_db`:
    *   Generate new `RunID`.
    *   Calls `RunRepository.Create(...)` with status `detected`.
    *   Calls `RunPodClient.Trigger(...)`.

**Data State Changes**:
*   `INSERT INTO forecast_runs (id, model, run_timestamp, status) VALUES (..., 'detected')`

**Gap Analysis**:
*   **[MISSING]**: `forecast_runs` table in `02-foundation-db.md` lacks `external_id` column to store the RunPod Job ID returned in step 5.
*   **[AMBIGUOUS]**: No timeout specified for the RunPod client call in `03-config.md`.
