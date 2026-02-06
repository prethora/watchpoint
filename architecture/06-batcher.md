# 06 - Batcher Architecture

> **Purpose**: Defines the Batcher Lambda function, which acts as the high-throughput bridge between the Forecast System (S3) and the Evaluation Engine (SQS). It groups WatchPoints by geographic tile and schedules evaluation jobs.
> **Package**: `cmd/batcher` (Entrypoint), `internal/batcher` (Core Logic)
> **Dependencies**: `01-foundation-types.md`, `02-foundation-db.md`, `03-config.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Schema Extensions (Migrations)](#2-schema-extensions-migrations)
3. [Configuration & Types](#3-configuration--types)
4. [External Interfaces](#4-external-interfaces)
5. [Core Service Logic](#5-core-service-logic)
6. [Lambda Handler](#6-lambda-handler)
7. [Error Handling & Resilience](#7-error-handling--resilience)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

The Batcher is an event-driven component triggered by the completion of a forecast run. Its primary responsibility is to efficiently translate a "Forecast Ready" signal into thousands of granular "Evaluate Tile" jobs.

### Responsibilities
1.  **Trigger Validation**: Validates S3 events to ensure they originate from the production bucket and represent valid forecast models.
2.  **Grouping Strategy**: Uses an **index-only database scan** to group active WatchPoints by their pre-calculated `tile_id`.
3.  **Pagination (Hot Tiles)**: Detects tiles containing >500 WatchPoints and splits them into multiple pages to prevent downstream timeouts.
4.  **Routing**: Routes Nowcast jobs to the Urgent SQS queue and Medium-Range jobs to the Standard SQS queue.
5.  **Observability**: Emits the `ForecastReady` metric, serving as the heartbeat for the "Dead Man's Switch" alarm.

---

## 2. Schema Extensions (Migrations)

This document defines types and repository methods that must be added to the Foundation layers to support the Batcher.

### 2.1 Type Extensions: `01-foundation-types.md`

*`EvalMessage` is consolidated in `01-foundation-types.md` (Section 10.6).*

This module uses `types.EvalMessage` for the strict contract between the Go Batcher and the Python Evaluation Worker. The type includes:
- Traceability fields (`BatchID`, `TraceID`)
- Forecast metadata (`ForecastType`, `RunTimestamp`)
- Evaluation scope (`TileID`)
- Pagination for hot tiles (`Page`, `PageSize`, `TotalItems`)

### 2.2 Repository Extensions: `02-foundation-db.md`

*Repository method extensions are consolidated in `02-foundation-db.md` (Section 4).*

This module requires the following repository methods:
- `WatchPointRepository.GetActiveTileCounts(ctx, ...) (map[string]int, error)` - Uses index-only scan on `idx_watchpoints_tile_active`
- `ForecastRunRepository.GetByTimestamp(ctx, model, ts) (*types.ForecastRun, error)`

---

## 3. Configuration & Types

### 3.1 Batcher Configuration

Mapped from `03-config.md`.

```go
type BatcherConfig struct {
    // S3 Validation
    ForecastBucket   string // Strictly validate input event bucket

    // Queue Routing
    UrgentQueueURL   string // For Nowcast
    StandardQueueURL string // For Medium-Range

    // Tunables
    MaxPageSize      int    // Default: 500
    MaxTiles         int    // Safety guardrail (Default: 100)
}
```

### 3.2 Internal Types

```go
// RunContext encapsulates the metadata extracted from the trigger
type RunContext struct {
    Model     types.ForecastType
    Timestamp time.Time
    Bucket    string
    Key       string
}
```

---

## 4. External Interfaces

The Batcher uses dependency injection to abstract AWS services and Observability.

### 4.1 SQS Client

Abstracts the `SendMessageBatch` chunking logic (limit 10 messages per API call) and error handling.

```go
type SQSClient interface {
    // SendBatch chunks messages into groups of 10 and sends them.
    // Must respect ctx.Done() to abort cleanly on timeout.
    SendBatch(ctx context.Context, queueURL string, messages []types.EvalMessage) error
}
```

### 4.2 Metric Publisher

Handles high-cardinality and heartbeat metrics.

```go
type MetricPublisher interface {
    // PublishForecastReady emits "ForecastReady=1" (Dead Man's Switch).
    PublishForecastReady(ctx context.Context, ft types.ForecastType) error
    
    // PublishStats emits "BatchSizeTiles" and "BatchSizeWatchPoints".
    PublishStats(ctx context.Context, ft types.ForecastType, tiles int, watchpoints int) error
}
```

---

## 5. Core Service Logic

The `Batcher` struct orchestrates the business logic, decoupled from the specific trigger mechanism (S3 vs Manual JSON).

### 5.1 Structure

```go
type Batcher struct {
    Config    BatcherConfig
    Log       *slog.Logger
    Repo      db.ForecastRunRepository
    WPRepo    db.WatchPointRepository
    SQS       SQSClient
    Metrics   MetricPublisher
}
```

### 5.2 Methods

```go
// ProcessRun is the core logic.
// 1. **Phantom Run Detection**: Query `forecast_runs` for the specific `model` and
//    `run_timestamp`. If no record is found, the Batcher MUST log a Critical Error
//    ('Phantom Forecast detected in S3') and **discard** the event without processing.
//    Do not auto-heal or create records. This enforces the `DataPoller` as the sole
//    authority for initiating forecast runs.
// 2. UPDATES forecast_runs SET status='complete', storage_path=...
//    (This signals the API that data is ready to serve).
// 3. Resolves target queue based on Model.
// 4. Queries active tile counts (Index-Only Scan).
// 5. Generates ephemeral BatchID and TraceID (UUIDs) for this execution.
//    These must be injected into every EvalMessage created in step 6 to ensure
//    distributed traceability.
// 6. Generates paginated EvalMessages (Hot Tile Handling).
// 7. Sends batches to SQS.
// 8. Emits metrics.
func (b *Batcher) ProcessRun(ctx context.Context, rc RunContext) error

// generateMessages handles the pagination logic.
// Logic:
//   For each tile:
//     pages = ceil(count / MaxPageSize)
//     For p in 0..pages:
//       Create EvalMessage with Page=p, PageSize=MaxPageSize
func (b *Batcher) generateMessages(tileCounts map[string]int, rc RunContext) []types.EvalMessage

// resolveQueue returns the URL based on ForecastType.
// Returns ErrUnknownModel if no match found.
func (b *Batcher) resolveQueue(ft types.ForecastType) (string, error)
```

---

## 6. Lambda Handler

The entrypoint handles the AWS-specific event parsing and validation.

```go
// Handler is the lambda.Start entrypoint.
// Supports both events.S3Event and custom JSON (for manual recovery).
func (b *Batcher) Handler(ctx context.Context, payload json.RawMessage) error {
    // 1. Attempt to parse as S3Event
    // 2. If S3Event.Records is empty, attempt to parse as manual RunContext
    
    // 3. Validate Bucket Name (Security)
    //    If s3.Bucket.Name != Config.ForecastBucket:
    //    Log Critical Error & Return nil (Do not retry invalid inputs)
    
    // 4. Parse Key (extract Model/Timestamp) using ParseRunContext helper
    
    // 5. Call b.ProcessRun()
}

// ParseRunContext extracts metadata from S3 Key.
// Format: "forecasts/{model}/{timestamp}/_SUCCESS"
func ParseRunContext(key string) (types.ForecastType, time.Time, error)
```

---

## 7. Error Handling & Resilience

### 7.1 Retry Strategy
The Batcher operates with **"At Least Once"** delivery semantics.
*   **Partial Failures**: If `SQS.SendBatch` fails midway (e.g., network timeout), the Handler returns an error.
*   **Lambda Retry**: The AWS Runtime retries the *entire* execution.
*   **Result**: Some messages may be sent twice. Downstream Eval Workers MUST be idempotent.

### 7.2 Safety Guardrails
*   **Zero State**: If `GetActiveTileCounts` returns 0 rows (empty DB), the Batcher proceeds successfully, sends 0 messages, but **still emits** `ForecastReady`. This prevents false positives in the Dead Man's Switch during system bootstrap.
*   **Unknown Models**: If the S3 key contains an unknown model string, the Batcher returns a hard error to trigger `LambdaErrors` alarms, signaling a configuration drift between Forecast and API layers.
*   **Context Cancellation**: The SQS loop rigorously checks `ctx.Done()`. If the Lambda timeout is imminent, it aborts immediately to fail the function cleanly, relying on the Lambda Retry to re-process the batch from scratch.

---

## 8. Flow Coverage

| Flow ID | Description | Implementation |
|---|---|---|
| `EVAL-001` | Evaluation Pipeline (Batcher Phase) | `Handler` -> `ProcessRun` -> `SQS.SendBatch` |
| `EVAL-002` | Hot Tile Pagination | `generateMessages` logic (page calculation) |
| `OBS-001` | Dead Man's Switch Heartbeat | `MetricPublisher.PublishForecastReady` |
| `IMPL-003` | Forecast Run Idempotency | `ProcessRun` checks `Repo.GetByTimestamp` |
| `FAIL-011` | Lambda Failure Recovery | Standard Lambda Retry + Idempotent Downstream |