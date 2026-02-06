# 05c - API Forecasts

> **Purpose**: Defines the HTTP handlers, service logic, and data access patterns for retrieving forecast data. This package bridges the gap between raw Zarr files stored in S3 and the standardized JSON API, handling model blending (Nowcast vs. Medium-Range), batch optimization, and metadata exposure.
> **Package**: `package forecasts`
> **Dependencies**: `05a-api-core.md` (Server/Middleware), `01-foundation-types.md` (Common Types), `02-foundation-db.md` (Run Repository)

---

## Table of Contents

1. [Overview](#1-overview)
2. [Domain Models](#2-domain-models)
3. [Service Interface](#3-service-interface)
4. [Data Access Interfaces](#4-data-access-interfaces)
5. [Handler Definition](#5-handler-definition)
6. [Business Logic & Behaviors](#6-business-logic--behaviors)
7. [Configuration](#7-configuration)
8. [Flow Coverage](#8-flow-coverage)

---

## 1. Overview

The Forecasts package is responsible for serving weather data to the API. Unlike the Evaluation Engine (which processes data internally), this package serves external read requests.

### Key Responsibilities
*   **Abstraction**: Hides the complexity of Zarr chunking and S3 storage from the API consumer.
*   **Model Blending**: Automatically selects the best data source (Nowcast vs. Medium-Range) based on location and time.
*   **Optimization**: Groups batch requests by geographic tile to minimize S3 read operations.
*   **Resilience**: Handles graceful degradation (serving stale data or fallback models) when primary sources are unavailable.

---

## 2. Domain Models

These structures define the JSON contract with the client.

### 2.1 Forecast Data

```go
// ForecastResponse represents a time-series forecast for a single location.
type ForecastResponse struct {
    Location    types.Location          `json:"location"`
    GeneratedAt time.Time               `json:"generated_at"`

    // ModelRuns tracks the specific model execution used for traceability
    ModelRuns   map[string]time.Time    `json:"model_runs"`

    // Metadata contains warnings (e.g., "Data is stale")
    Metadata    ResponseMetadata        `json:"metadata,omitempty"`

    Data        []ForecastDataPoint     `json:"forecast"`
}

type ResponseMetadata struct {
    Status   string          `json:"status"` // "fresh", "stale", "degraded"
    Warnings []ResponseWarning `json:"warnings,omitempty"`
}

type ResponseWarning struct {
    Code    string `json:"code"`
    Message string `json:"message"`
}

// ForecastDataPoint represents a single timestep in the series.
// Fields correspond to the canonical variables defined in 01-foundation-types.
type ForecastDataPoint struct {
    ValidTime                time.Time `json:"valid_time"`
    Source                   string    `json:"source"` // "nowcast", "medium_range", "blended"
    
    // Pointers allow omitted (null) values if a variable is unavailable
    PrecipitationProbability *float64  `json:"precipitation_probability,omitempty"` // %
    PrecipitationMM          *float64  `json:"precipitation_mm,omitempty"`          // mm
    TemperatureC             *float64  `json:"temperature_c,omitempty"`             // 2m AGL
    WindSpeedKmh             *float64  `json:"wind_speed_kmh,omitempty"`            // 10m AGL
    Humidity                 *float64  `json:"humidity_percent,omitempty"`          // %
    CloudCover               *float64  `json:"cloud_cover_percent,omitempty"`       // %
    // ... extensible for other canonical variables
}
```

### 2.2 Metadata & Status

*`VariableMetadata` is consolidated in `01-foundation-types.md` (Section 11).*

This module uses `types.VariableMetadata` for variable validation. The following extends it for API responses:

```go
// VariableResponseMetadata extends the canonical VariableMetadata with API-specific fields.
type VariableResponseMetadata struct {
    types.VariableMetadata
    Name            string               `json:"name"`        // Human-readable name, e.g., "Temperature"
    SupportedModels []types.ForecastType `json:"supported_models"`
}

// SystemStatus reports the health of the forecast pipeline.
type SystemStatus struct {
    Timestamp time.Time              `json:"timestamp"`
    Models    map[string]ModelStatus `json:"models"`
}

type ModelStatus struct {
    Status      StatusEnum `json:"status"` // "healthy", "degraded", "stale"
    LatestRun   time.Time  `json:"latest_run_at"`
    Horizon     string     `json:"horizon"`  // e.g., "6h"
    Coverage    string     `json:"coverage"` // "CONUS", "Global"
}

type StatusEnum string
const (
    StatusHealthy  StatusEnum = "healthy"
    StatusDegraded StatusEnum = "degraded"
    StatusStale    StatusEnum = "stale"
)
```

### 2.3 Batch Request Models

```go
// BatchForecastRequest allows querying multiple points in one call.
type BatchForecastRequest struct {
    Locations []types.LocationIdent `json:"locations" validate:"max=50"`
    Start     time.Time             `json:"start"`
    End       time.Time             `json:"end"`
    Variables []string              `json:"variables"` // Optional filter
}

// BatchForecastResult separates successes from failures.
type BatchForecastResult struct {
    Forecasts map[string]*ForecastResponse `json:"forecasts"`
    Errors    map[string]types.ErrorDetail `json:"errors,omitempty"`
}

// VerificationReport contains aggregated verification metrics for API response.
// Groups metrics by variable for dashboard consumption.
type VerificationReport struct {
    Model     string                         `json:"model"`
    StartTime time.Time                      `json:"start_time"`
    EndTime   time.Time                      `json:"end_time"`
    Metrics   []types.VerificationMetric     `json:"metrics"`
}
```

---

## 3. Service Interface

The Service layer encapsulates business logic, including model blending and tile grouping.

```go
type ForecastService interface {
    // FQRY-001: Get single point forecast
    GetPointForecast(ctx context.Context, lat, lon float64, start, end time.Time) (*ForecastResponse, error)

    // FQRY-002: Get multiple point forecasts (optimized)
    GetBatchForecast(ctx context.Context, req BatchForecastRequest) (*BatchForecastResult, error)

    // FQRY-003: List supported variables
    GetVariables(ctx context.Context) ([]VariableMetadata, error)

    // FQRY-004: Get pipeline health status
    GetStatus(ctx context.Context) (*SystemStatus, error)

    // Implements ForecastProvider for WPLC-001 (05b-api-watchpoints)
    //
    // **Model Selection**: GetSnapshot MUST strictly query the **Medium-Range (Atlas)**
    // model to ensure consistent global coverage and variable availability for
    // WatchPoint creation context. This prevents failures for locations outside CONUS
    // where Nowcast is unavailable.
    GetSnapshot(ctx context.Context, lat, lon float64) (*types.ForecastSnapshot, error)

    // INFO-007: Get verification metrics for dashboard display
    // Returns aggregated forecast accuracy metrics for the specified model and time range.
    // Delegates to VerificationRepository.GetAggregatedMetrics for SQL-level aggregation.
    GetVerificationMetrics(ctx context.Context, model string, start, end time.Time) (*VerificationReport, error)
}
```

---

## 4. Data Access Interfaces

These interfaces abstract the storage implementation (Postgres and S3/Zarr).

### 4.1 Run Repository Extension

Extends the repository defined in `02-foundation-db.md`.

```go
type ForecastRunRepository interface {
    // GetLatestServing returns the most recent 'complete' run.
    // It returns stale runs if no recent run is available, to support degradation.
    GetLatestServing(ctx context.Context, model types.ForecastType) (*types.ForecastRun, error)
}
```

### 4.2 Forecast Reader (Zarr Abstraction)

Abstracts low-level Zarr array reading. Implementations must handle AWS SDK context propagation.

```go
// RunContext identifies the specific dataset to read.
type RunContext struct {
    Model       types.ForecastType
    Timestamp   time.Time
    StoragePath string
    Version     string // Handles schema evolution
}

type ForecastReader interface {
    // ReadPoint extracts time series for a single coordinate.
    ReadPoint(ctx context.Context, rc RunContext, loc types.Location, vars []string) ([]ForecastDataPoint, error)
    
    // ReadTile extracts data for multiple points within the same tile (Batch optimization).
    ReadTile(ctx context.Context, rc RunContext, tileID string, locs []types.LocationIdent, vars []string) (map[string][]ForecastDataPoint, error)
}
```

### 4.3 Variable Registry

Provides metadata definitions. Typically implemented as a static in-memory registry.

```go
type VariableRegistry interface {
    List() []VariableMetadata
    Get(id string) (VariableMetadata, bool)
    IsSupported(id string, model types.ForecastType) bool
}
```

---

## 5. Handler Definition

The Handler maps HTTP requests to Service methods. It does **not** access S3 or DB directly.

```go
type ForecastHandler struct {
    service   ForecastService
    validator *core.Validator
    logger    *slog.Logger
}

func NewForecastHandler(
    svc ForecastService,
    val *core.Validator,
    logger *slog.Logger,
) *ForecastHandler {
    return &ForecastHandler{
        service:   svc,
        validator: val,
        logger:    logger,
    }
}

// RegisterRoutes mounts the endpoints onto the mux.
// All routes assume Authentication Middleware is already applied.
func (h *ForecastHandler) RegisterRoutes(r chi.Router) {
    r.Get("/point", h.HandleGetPoint)             // GET /v1/forecasts/point
    r.Post("/points", h.HandleGetBatch)           // POST /v1/forecasts/points
    r.Get("/variables", h.HandleListVars)         // GET /v1/forecasts/variables
    r.Get("/status", h.HandleGetStatus)           // GET /v1/forecasts/status
    r.Get("/verification", h.HandleGetVerification) // GET /v1/forecasts/verification
}

// HandleGetVerification (GET /v1/forecasts/verification)
// Returns aggregated verification metrics for dashboard display.
// Query params: model (required), start (optional), end (optional).
// Defaults to last 7 days if time range not specified.
func (h *ForecastHandler) HandleGetVerification(w http.ResponseWriter, r *http.Request)
```

---

## 6. Business Logic & Behaviors

### 6.1 Model Blending Strategy

The Service determines which model to use for each timestep in a requested range:

1.  **Validation**:
    *   If `Nowcast` is requested/implied, check if location is within CONUS bounds (Lat 24-50, Lon -125 to -66).
    *   If outside CONUS, fallback to `Medium-Range` (log warning if Nowcast was strictly requested).

2.  **Selection**:
    *   **T < 6 Hours**: Prefer `Nowcast` (StormScope) if available and fresh.
    *   **T > 6 Hours**: Use `Medium-Range` (Atlas).
    *   **Fallback**: If Nowcast data is missing (chunk read error) or incomplete (missing variable), seamlessly fall back to Medium-Range for that specific timestep.

3.  **Resolution**:
    *   Return data in native resolution (e.g., 15-min steps for Nowcast, 3-hour steps for Medium-Range). Do not interpolate temporally on the server.

### 6.2 Batch Optimization (Tile Grouping)

For `GetBatchForecast` (POST /points):

1.  **Grouping**: The Service iterates inputs and groups locations by `TileID` (using the same geospatial logic as the Batcher).
2.  **Parallel IO**: `Reader.ReadTile` is called concurrently for each unique tile.
3.  **IO Efficiency**: The Reader fetches the Zarr chunk for the tile **once**, extracts data for all points in that tile from memory, and then discards the chunk.
4.  **Error Isolation**: If one tile fails (e.g., S3 timeout), only points in that tile return errors; others succeed.

### 6.3 Caching

1.  **HTTP Caching**:
    *   `Cache-Control: private, max-age=300` (5 minutes).
    *   `Last-Modified`: Set to `inference_completed_at` of the freshest model run used.
    *   Allows clients to poll frequently but receive `304 Not Modified` if the backend model hasn't updated.

2.  **Internal Metadata Cache**:
    *   The `ForecastReader` maintains a short-lived (LRU) cache of Zarr array metadata (offsets/shapes) to prevent repeated S3 metadata lookups for the same run ID.

### 6.4 Error Mapping

Mapping from internal errors to `types.AppError`:

| Condition | Error Code | HTTP Status |
|---|---|---|
| Location invalid (lat/lon) | `validation_invalid_latitude` | 400 |
| Variable not found | `validation_invalid_variable` | 400 |
| Batch size > 50 | `validation_batch_size_exceeded` | 400 |
| S3/Zarr unavailable | `upstream_forecast_unavailable` | 503 |
| Run data corrupt | `internal_forecast_corruption` | 500 |

### 6.5 Response Handling

*   **Canonical Units**: All responses use C, mm, km/h. No server-side conversion.
*   **Format**: JSON only (GeoJSON support deferred).
*   **Rate Limits**: 1 Batch Request = 1 Rate Limit Token.

---

## 7. Configuration

```go
type ServiceConfig struct {
    ForecastBucket string        `envconfig:"FORECAST_BUCKET" validate:"required"`
    Region         string        `envconfig:"AWS_REGION" default:"us-east-1"`
    CacheTTL       time.Duration `envconfig:"FORECAST_CACHE_TTL" default:"5m"`
}
```

The Handler/Service is initialized in `05a-api-core` using values from the global `config.Config`.

---

## 8. Flow Coverage

| Flow ID | Description | Component | Method |
|---|---|---|---|
| `FQRY-001` | Point Forecast Retrieval | `ForecastService` | `GetPointForecast` |
| `FQRY-002` | Batch Forecast Retrieval | `ForecastService` | `GetBatchForecast` |
| `FQRY-003` | Variable Metadata | `ForecastHandler` | `HandleListVars` |
| `FQRY-004` | System Status | `ForecastService` | `GetStatus` |
| `FCST-005` | Serve Stale Fallback | `ForecastRunRepo` | `GetLatestServing` |
| `WPLC-001` | Snapshot for WatchPoint | `ForecastService` | `GetSnapshot` |
| `INFO-007` | Verification Metrics | `ForecastService` | `GetVerificationMetrics` |