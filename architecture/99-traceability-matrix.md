# 99 - Traceability Matrix & Gap Analysis

> **Purpose**: Serves as the master verification checklist for the platform. It maps every requirements flow to a specific architectural component, defines data access authority to ensure least privilege, and specifies "Gap-Filling" migrations identified during the architectural review process.
> **Status**: **Final** (Merged findings from architectural review)

---

## Table of Contents

1. [Legend & Implementation Types](#1-legend--implementation-types)
2. [Master Flow Traceability Matrix](#2-master-flow-traceability-matrix)
3. [Component Interface Map](#3-component-interface-map)
4. [Data Authority Matrix](#4-data-authority-matrix)
5. [Metrics Inventory](#5-metrics-inventory)
6. [Gap-Filling Specifications (Patches)](#6-gap-filling-specifications-patches)

---

## 1. Legend & Implementation Types

The **Implementation Type** column indicates *where* the logic resides:

| Type | Description | Location |
|---|---|---|
| **`AUTO`** | Automated Logic | Go/Python Code (Handlers, Workers, Services) |
| **`INFRA`** | Infrastructure | AWS SAM Template (Alarms, Queue Config, IAM) |
| **`PROC`** | Procedural | Operations Manual / Runbooks (Manual Intervention) |
| **`DELEG`** | Delegated | Managed Service Behavior (e.g., SQS Retry, AWS SDK) |

---

## 2. Master Flow Traceability Matrix

### 2.1 Forecast Domain (FCST)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `FCST-001` | Medium-Range Generation | `AUTO` | `11-runpod` (Inference), `09-scheduled` (Trigger) | |
| `FCST-002` | Nowcast Generation | `AUTO` | `11-runpod` (Inference), `09-scheduled` (Trigger) | |
| `FCST-003` | Data Poller Execution | `AUTO` | `09-scheduled-jobs` (`DataPoller`) | |
| `FCST-004` | Forecast Retry | `AUTO` | `09-scheduled-jobs` (`DataPoller` retry logic) | |
| `FCST-005` | Fallback to Stale | `AUTO` | `05c-api-forecasts` (Service Logic) | Returns stale with warning flag |
| `FCST-006` | Forecast Cleanup | `AUTO` | `09-scheduled-jobs` (`TierTransitionService`) | |

### 2.2 Evaluation Domain (EVAL)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `EVAL-001` | Evaluation Pipeline | `AUTO` | `06-batcher` → `07-eval-worker` | Core Loop |
| `EVAL-002` | Hot Tile Pagination | `AUTO` | `06-batcher` (Logic), `07-eval-worker` (Handling) | |
| `EVAL-003` | Monitor Mode Eval | `AUTO` | `07-eval-worker` (`Evaluator`) | |
| `EVAL-004` | Config Version Mismatch | `AUTO` | `07-eval-worker` (`ConfigPolicy`) | Resets state on mismatch |
| `EVAL-005` | First Evaluation | `AUTO` | `07-eval-worker` (State Init) | |
| `EVAL-006` | Queue Backlog Recovery | `PROC` | `12-operations` (Runbooks) | Manual scaling/purging |

### 2.3 Notification Domain (NOTIF)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `NOTIF-001` | Immediate Delivery | `AUTO` | `08b-email`, `08c-webhook` | |
| `NOTIF-002` | Quiet Hours | `AUTO` | `08a-notification-core` (`PolicyEngine`) | |
| `NOTIF-003` | Digest Generation | `AUTO` | `09-scheduled-jobs` (`DigestScheduler`) | |
| `NOTIF-004` | Webhook Retry | `DELEG` | SQS Visibility Timeout (Standard) | |
| `NOTIF-005` | Rate Limit Handling | `AUTO` | `08c-webhook-worker` (Retry-After parser) | |
| `NOTIF-006` | Bounce Handling | `AUTO` | `08b-email-worker` (`BounceProcessor`) | |
| `NOTIF-007` | Platform Formatting | `AUTO` | `08c-webhook-worker` (`PlatformRegistry`) | |
| `NOTIF-008` | Escalation | `AUTO` | `07-eval-worker` (Logic) | |
| `NOTIF-009` | Cleared Notification | `AUTO` | `07-eval-worker` (Logic) | |
| `NOTIF-010` | All Channels Failed | `AUTO` | `08a-notification-core` (`DeliveryManager`) | Logic identifies terminal failures |

### 2.4 WatchPoint Lifecycle (WPLC)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `WPLC-001` | Create (Event Mode) | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-002` | Create (Monitor Mode) | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-003` | Update | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-004` | Update Triggered | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-005` | Pause | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-006` | Resume | `AUTO` | `05b-api-watchpoints` | Triggers eval via `05b` logic |
| `WPLC-007` | Resume w/ Backlog | `AUTO` | `05b-api-watchpoints` | Consolidated digest logic |
| `WPLC-008` | Delete | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-009` | Auto Archival | `AUTO` | `09-scheduled-jobs` (`ArchiverService`) | |
| `WPLC-010` | Clone | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-011` | Bulk Import | `AUTO` | `05b-api-watchpoints` | |
| `WPLC-012` | Validation Reject | `AUTO` | `05a-api-core` (`Validator`) | |

### 2.5 Billing & Plans (BILL)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `BILL-001` | Stripe Customer Create | `AUTO` | `05e-api-billing` (`BillingService`) | |
| `BILL-002` | Subscription Create | `AUTO` | `05e-api-billing` | |
| `BILL-003` | Plan Upgrade | `AUTO` | `05e-api-billing` (Webhook) | |
| `BILL-004` | Plan Downgrade | `AUTO` | `05e-api-billing` (Webhook) | |
| `BILL-005` | Payment Failure | `AUTO` | `05e-api-billing` (Webhook) | |
| `BILL-006` | Grace Period Expire | `AUTO` | `09-scheduled-jobs` (`BillingAggregator`) | |
| `BILL-007` | Stripe Webhook | `AUTO` | `05e-api-billing` (`StripeWebhookHandler`) | |
| `BILL-008` | WatchPoint Limit | `AUTO` | `05b-api-watchpoints` (`UsageEnforcer`) | |
| `BILL-009` | API Rate Limit | `AUTO` | `05a-api-core` (`RateLimitMiddleware`) | |
| `BILL-010` | Overage Warning | `AUTO` | `09-scheduled-jobs` (**See Patch 6.1**) | |

### 2.6 Observability (OBS)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `OBS-001` | DMS (Medium Range) | `INFRA` | `04-sam-template` (Alarms) | Triggered by `06-batcher` metric |
| `OBS-002` | DMS (Nowcast) | `INFRA` | `04-sam-template` (Alarms) | |
| `OBS-003` | Queue Depth | `INFRA` | `04-sam-template` (Alarms) | |
| `OBS-004` | DLQ Alarm | `INFRA` | `04-sam-template` (Alarms) | |
| `OBS-005` | Verification Pipe | `AUTO` | `09-scheduled-jobs` (`VerificationService`) | |
| `OBS-006` | Calibration Update | `AUTO` | `09-scheduled-jobs` (`CalibrationService`) | |
| `OBS-007` | Post-Event Summary | `AUTO` | `09-scheduled-jobs` (`ArchiverService`) | **See Patch 6.2** |
| `OBS-010` | Metrics Emission | `AUTO` | Multiple (See Section 5) | |
| `OBS-011` | Audit Log Query | `AUTO` | `05a-api-core` (Log) / Admin Tool | |

### 2.7 Security & Auth (SEC/AUTH)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `SEC-001` | Brute Force (Login) | `AUTO` | `05f-api-auth` (`BruteForceProtector`) | DB-based |
| `SEC-002` | Brute Force (API) | `AUTO` | `05a-api-core` (`RateLimit`) | IP-based Memory |
| `SEC-003` | Key Compromise | `PROC` | `12-operations` | Revocation via Admin API |
| `SEC-006` | Webhook Signing | `AUTO` | `08c-webhook-worker` | |
| `API-002` | SSRF Protection | `AUTO` | `08c-webhook-worker` (`SafeTransport`) | |
| `HOOK-001` | Secret Rotation | `AUTO` | `05b-api-watchpoints` (Write), `08c` (Read) | |

### 2.8 Failure & Recovery (FAIL)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `FAIL-001` | DB Auto-Failover | `DELEG` | AWS RDS / Supabase | |
| `FAIL-008` | Zarr Corruption | `AUTO` | `07-eval-worker` (Catch & Retry) | |
| `FAIL-013` | Region Failover | `PROC` | `12-operations` (DR Plan) | |

### 2.9 User & Organization Domain (USER)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `USER-001` | Org Creation (Signup) | `AUTO` | `05d-api-organization` (`OrganizationHandler`) | |
| `USER-002` | Org Update | `AUTO` | `05d-api-organization` | |
| `USER-003` | Org Soft Delete | `AUTO` | `05d-api-organization` | |
| `USER-004` | User Invite | `AUTO` | `05d-api-organization` (`UserHandler`) | |
| `USER-005` | User Accept Invite | `AUTO` | `05f-api-auth` (`AuthHandler`) | |
| `USER-006` | Login (Password) | `AUTO` | `05f-api-auth` | |
| `USER-007` | Login (OAuth) | `AUTO` | `05f-api-auth` | |
| `USER-008` | Password Reset | `AUTO` | `05f-api-auth` | |
| `USER-009` | Role Change | `AUTO` | `05d-api-organization` | |
| `USER-010` | User Removal | `AUTO` | `05d-api-organization` | |
| `USER-011` | API Key Create | `AUTO` | `05d-api-organization` (`APIKeyHandler`) | |
| `USER-012` | API Key Rotate | `AUTO` | `05d-api-organization` | |
| `USER-013` | API Key Revoke | `AUTO` | `05d-api-organization` | |
| `USER-014` | Bounce Notify Owner | `AUTO` | `08b-email-worker` (Triggered by bounce) | |

### 2.10 Forecast Query Domain (FQRY)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `FQRY-001` | Point Forecast | `AUTO` | `05c-api-forecasts` | |
| `FQRY-002` | Batch Forecast | `AUTO` | `05c-api-forecasts` | |
| `FQRY-003` | Variable List | `AUTO` | `05c-api-forecasts` | |
| `FQRY-004` | System Status | `AUTO` | `05c-api-forecasts` | |
| `FQRY-005` | WP Context Forecast | `AUTO` | `05b-api-watchpoints` (via `ForecastService`) | |

### 2.11 Information & Dashboard (INFO/DASH)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `INFO-001` | Notif History (WP) | `AUTO` | `05b-api-watchpoints` | |
| `INFO-002` | Notif History (Org) | `AUTO` | `05d-api-organization` | |
| `INFO-003` | Usage Current | `AUTO` | `05e-api-billing` | |
| `INFO-004` | Usage History | `AUTO` | `05e-api-billing` | |
| `INFO-005` | Invoices | `AUTO` | `05e-api-billing` | |
| `INFO-006` | Subscription State | `AUTO` | `05e-api-billing` | |
| `INFO-008` | WP Eval State | `AUTO` | `05b-api-watchpoints` | |
| `DASH-001` | Login | `AUTO` | `05f-api-auth` | |
| `DASH-002` | OAuth Callback | `AUTO` | `05f-api-auth` | |
| `DASH-003` | Session Refresh | `AUTO` | `05a-api-core` (Auth Middleware) | |
| `DASH-004` | Logout | `AUTO` | `05f-api-auth` | |
| `DASH-005` | Checkout Session | `AUTO` | `05e-api-billing` | |
| `DASH-006` | Portal Session | `AUTO` | `05e-api-billing` | |

### 2.12 Bulk Operations (BULK)

| Flow ID | Flow Name | Impl Type | Component / Document | Notes |
|---|---|---|---|---|
| `BULK-001` | Bulk Import | `AUTO` | `05b-api-watchpoints` | |
| `BULK-002` | Bulk Pause | `AUTO` | `05b-api-watchpoints` | |
| `BULK-003` | Bulk Resume | `AUTO` | `05b-api-watchpoints` | |
| `BULK-004` | Bulk Delete | `AUTO` | `05b-api-watchpoints` | |
| `BULK-005` | Bulk Tag Update | `AUTO` | `05b-api-watchpoints` | |
| `BULK-006` | Bulk Clone | `AUTO` | `05b-api-watchpoints` | |

---

## 3. Component Interface Map

| Component | Inputs (Triggers) | Outputs (Writes/Events) | Key Dependencies |
|---|---|---|---|
| **API** | HTTP Request | DB Write, Audit Log | DB, SSM, Redis (optional) |
| **Batcher** | S3 Event (`_SUCCESS`) | SQS Message, `ForecastReady` Metric | DB (Read), SQS |
| **Eval Worker** | SQS Message | DB Write (State), SQS (Notif), S3 (Read) | S3, DB, SQS |
| **Notification** | SQS Message | DB Write (Delivery), HTTP/SMTP Out | SendGrid, Ext. Webhooks |
| **Data Poller** | EventBridge (Time) | RunPod API Call, DB Write (Run Status) | RunPod, AWS S3 (List) |
| **RunPod** | HTTP Request | S3 Write (Zarr) | S3 |

---

## 4. Data Authority Matrix

**Principle**: Define strict Read/Write boundaries to prevent architectural drift.

| Resource | Writers (Authoritative) | Readers | Forbidden Writes |
|---|---|---|---|
| **`forecast_runs` (Status)** | `data-poller`, `runpod` (via callback), `scheduled-jobs` | API, Batcher | API (Must not manually set status) |
| **`organizations` (Plan)** | `api-billing` (Stripe Hook), Admin API | Batcher, Workers | User API (Cannot self-promote without Stripe) |
| **`watchpoints` (Config)** | `api-watchpoints` | Batcher, Eval Worker | Eval Worker (Read-only on config) |
| **`evaluation_state`** | `eval-worker` | API | API (State managed by worker only) |
| **`api_keys` (Hash)** | `api-auth` (Create/Rotate) | API Middleware (Verify) | Middleware (Verify only) |

---

## 5. Metrics Inventory

This table explicitly assigns ownership for the "Distributed" `OBS-010` flow.

| Metric Name | Dimension | Emitter Component | Purpose |
|---|---|---|---|
| `ForecastReady` | `ForecastType` | `06-batcher` | Dead Man's Switch Heartbeat |
| `EvaluationLag` | `Queue` | `07-eval-worker` | Auto-scaling trigger |
| `NotificationFailure` | `Channel` | `08a-notification-core` | Channel health monitoring |
| `BillingWarning` | `OrgID` | `09-scheduled-jobs` | Usage alert tracking |
| `APILatency` | `Endpoint` | `05a-api-core` | SLA monitoring |
| `ExternalAPIFailure` | `Provider` | `10-external-integrations` | Circuit Breaker trigger |

---

## 6. Gap-Filling Specifications (Patches)

The following patches were **REQUIRED** to satisfy the flows defined above. All patches have been **IMPLEMENTED** and consolidated into the foundation documents.

### 6.1 Patch: Rate Limits Table (Support `BILL-010`) ✅ IMPLEMENTED
**Target**: `02-foundation-db.md` (Section 6.5)
**Status**: Consolidated in foundation database schema.

The `rate_limits` table now includes `warning_sent_at` and `last_reset_at` columns to prevent spamming 80% usage warnings.

### 6.2 Patch: Archiver Interface (Support `OBS-007`) ✅ IMPLEMENTED
**Target**: `09-scheduled-jobs.md`
**Status**: The `ArchiverService` interface includes `GenerateSummary` method.

### 6.3 Patch: MonitorConfig Validation (Support `VALID-001`) ✅ IMPLEMENTED
**Target**: `01-foundation-types.md` (Section 5.2)
**Status**: Consolidated in foundation types with validation tags.

```go
type MonitorConfig struct {
    WindowHours int `json:"window_hours" validate:"required,min=6,max=168"`
    // ... other fields with full validation
}
```

### 6.4 Patch: Delivery Manager (Support `NOTIF-010`) ✅ IMPLEMENTED
**Target**: `08a-notification-core.md` (Section 5.2)
**Status**: `CheckAggregateFailure` method added to `DeliveryManager` interface.

---

## 7. Consolidation Summary

The architecture documents have been consolidated to establish a **Single Source of Truth** for:

| Category | Primary Document | Dependents |
|---|---|---|
| **Types** | `01-foundation-types.md` | All Go/Python components |
| **Schema** | `02-foundation-db.md` | All database-dependent components |
| **Config** | `03-config.md` | All Lambda functions |

All component documents (05x, 06, 07, 08x, 09, 10) now reference the foundation documents rather than defining duplicate types or schema.
