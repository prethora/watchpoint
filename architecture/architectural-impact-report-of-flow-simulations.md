# Architectural Impact Report: Forecast Generation Domain

> **Range**: FCST-001 to FCST-005
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of the Forecast Generation flows.

## 1. Database Schema Extensions (`02-foundation-db.md`)

To support the Recovery State Machine (`FCST-004`) and external job tracking, the `forecast_runs` table was expanded.

| Change | Purpose |
| :--- | :--- |
| **Added `external_id`** | Stores the RunPod Job ID, enabling the Reconciler to query remote status for stuck jobs. |
| **Added `retry_count`** | Tracks recovery attempts to enforce a maximum retry limit (3) before failing. |
| **Added `failure_reason`** | Provides observability into why a run failed (e.g., "RunPod timeout", "Validation error"). |

## 2. Type System & Contracts (`01-foundation-types.md`)

Strict typing was introduced to harden the interface between the Go Data Poller and the Python RunPod Worker.

*   **New `InferencePayload` Struct**: Defines the strict JSON contract for triggering RunPod, replacing loose maps. Includes fields for `InputConfig` (Calibration/SourcePaths) and `Options` (ForceRebuild).
*   **JSON Tags**: Added `json:"snake_case"` tags to `CalibrationCoefficients` to ensure compatibility when serialized for the Python worker.
*   **Struct Updates**: Updated `ForecastRun` to include the new database fields.

## 3. Responsibility Shifts

Simulation revealed gaps in "who writes what" logic, resulting in two major responsibility shifts:

*   **Batcher Write Authority (`06-batcher.md`)**:
    *   *Previous*: Ambiguous (implied RunPod might call back).
    *   *New*: The **Batcher** is now explicitly responsible for marking a run as `complete` in the database upon receiving the S3 `_SUCCESS` event. This ensures database state matches S3 reality.
*   **Poller Logic (`09-scheduled-jobs.md`)**:
    *   *Previous*: Generic trigger logic.
    *   *New*: The **Data Poller** is explicitly responsible for querying `calibration_coefficients` and injecting them into the `InferencePayload`. This keeps the RunPod worker stateless and network-isolated (no DB access required).

## 4. API & Service Enhancements (`05c-api-forecasts.md`)

To support "Serving Stale Data" (`FCST-005`) without breaking clients, the API response structure was evolved.

*   **New `Metadata` Field**: Added to `ForecastResponse`. Contains `Status` ("fresh"/"stale") and a `Warnings` array.
*   **New Repository Method**: Added `ForecastRunRepository.GetLatestServing`, which specifically filters for the last *successful* run, ensuring the API never serves failed/partial data.


# Architectural Impact Report: Evaluation Domain

> **Range**: FCST-006 to EVAL-003
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of the Forecast Lifecycle and Evaluation flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To support complex Monitor Mode logic and strict lifecycle management, specific schema definitions were evolved.

| Change | Purpose |
| :--- | :--- |
| **`seen_threats` (JSONB)** | Replaced `seen_threat_hashes` (TEXT[]). This enables **Temporal Overlap Detection** (storing Start/End times) instead of brittle hash-based deduplication which caused alert flapping on minor time shifts. |
| **`status='deleted'`** | Added to the `forecast_runs` CHECK constraint. This explicitly models the state where S3 data has been purged (`FCST-006`), allowing the API to gracefully handle 404s rather than attempting invalid fetches. |

## 2. Type System & Constants (`01-foundation-types.md`)

New types were introduced to ensure type safety between the Python Evaluation Worker and the Go Digest Generator.

*   **New `MonitorSummary` Struct**: Defines the strict contract for the `last_forecast_summary` JSONB column. Encapsulates `MaxValues` and `TriggeredPeriods` to ensure the Digest Generator never encounters malformed data.
*   **Hysteresis Constants**: Defined standard constants (e.g., `HysteresisFactorDefault = 0.10`) to enforce consistent "Clear" logic across the platform, preventing rapid toggling of alert states.

## 3. Algorithmic Optimizations

Simulation of the high-throughput evaluation loop (`EVAL-001`) necessitated significant optimizations to reduce latency and S3 costs.

*   **Write-Side Halo (`11-runpod.md`)**:
    *   *Problem*: Bilinear interpolation at tile edges required fetching 4 separate Zarr chunks, quadrupling latency for edge-case WatchPoints.
    *   *Solution*: Mandated a **1-pixel overlap padding** when writing Zarr chunks. This ensures every tile is self-contained, allowing the Reader to fetch exactly one chunk per tile.
*   **Temporal Deduplication (`07-eval-worker.md`)**:
    *   *Problem*: Forecast updates often shift event start times slightly (e.g., 14:00 -> 14:15), changing the hash and triggering false "New Threat" alerts.
    *   *Solution*: Implemented time-based overlap detection. If a new violation period overlaps with a known threat, it is treated as the same event.
*   **Standardized Interpolation**: Explicitly defined **Bilinear Interpolation** as the mathematical standard for both the Python Worker and Go API to prevent "Dashboard says X, Alert says Y" discrepancies.

## 4. Resilience & Observability

*   **Batcher Traceability (`06-batcher.md`)**: Added ephemeral generation of `BatchID` and `TraceID` to satisfy the `EvalMessage` contract, ensuring logs can be correlated across distributed workers.
*   **Poison Pill Prevention (`07-eval-worker.md`)**: Added specific error handling for `ForecastExpiredError` (FileNotFound). If a worker attempts to read a deleted forecast (rare race condition), it now logs a warning and **skips/acknowledges** the message rather than failing and spiraling into a Dead Letter Queue loop.
*   **Timezone Correctness**: Explicitly mandated that the Forecast Reader convert UTC indices to Local Time *before* applying "Active Hours" filters in Monitor Mode.


# Architectural Impact Report: Evaluation & Notification Logic

> **Range**: EVAL-004 to NOTIF-002
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of the Advanced Evaluation and Notification Delivery flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To support robust delivery auditing and the "Quiet Hours" feature, the notification schema was expanded.

| Change | Purpose |
| :--- | :--- |
| **`notification_deliveries.status` Enum** | Expanded `CHECK` constraint to include `'skipped'` (Test Mode/Suppression) and `'deferred'` (Quiet Hours), formally supporting the non-binary states of delivery. |
| **`notifications.template_set`** | Added column to snapshot the template configuration *at the time of generation*. This ensures that if a user changes their preference (e.g., "Wedding" -> "Default"), historical audit logs correctly reflect the template used for past alerts. |

## 2. Type System & Contracts (`01-foundation-types.md`)

Strict typing was introduced to bridge the gap between the Python Evaluation Worker and the Go Notification/Digest components.

*   **`MonitorSummary` Struct**: Defined the strict schema for the `last_forecast_summary` JSONB column. This acts as the binding contract between the Python worker (writer) and the Go Digest Generator (reader), ensuring type safety for aggregated window statistics (Max/Min values).
*   **`DeliveryStatus` Enum**: Formally defined the valid states for a delivery attempt in the Go codebase to match the database constraints.

## 3. Evaluation Logic Hardening (`07-eval-worker.md`)

Simulation of edge cases revealed race conditions and user experience pitfalls.

*   **Fast Fail on Config Mismatch (`EVAL-004`)**:
    *   *Problem*: If a user updates a WatchPoint location between the Batcher generation and Worker execution, the Worker holds forecast data for the wrong geographic tile.
    *   *Solution*: Mandated a geometric consistency check. If the tile calculated from the *current* location differs from the loaded tile, the Worker aborts evaluation and only updates the version pointer.
*   **Baseline Establishment (`EVAL-005`)**:
    *   *Problem*: Monitor Mode WatchPoints alerted immediately upon creation because "Current Conditions" matched the trigger, causing "Alert Shock."
    *   *Solution*: Defined a "First Evaluation" logic path where Monitor Mode explicitly suppresses notifications to establish a baseline, ensuring users are only alerted on *changes* or *new* threats.

## 4. Notification Architecture Evolution (`08a-notification-core.md`)

The delivery mechanism was evolved from a simple loop to a resilient, non-blocking architecture.

*   **Publish-Subscribe Retry Pattern**:
    *   *Previous*: Implicit retry or blocking waits.
    *   *New*: Adopted a stateless pattern where the Worker **ACKs** a failed message and **Publishes** a new message to SQS with `DelaySeconds` calculated for backoff. This prevents worker thread exhaustion during provider outages.
    *   *Artifact*: Added `retry_count` field to the `NotificationMessage` SQS payload to track depth across re-queues.
*   **Persistent Deferral (`NOTIF-002`)**:
    *   *Problem*: Keeping messages in SQS for long "Quiet Hours" windows (e.g., 8 hours) risks visibility timeouts or DLQ pollution.
    *   *Solution*: Implemented a "Parking" pattern. Deferred notifications are written to the DB with `status='deferred'` and removed from SQS. A new scheduled job (`RequeueDeferredNotifications`) resurrects them when the quiet window ends.
*   **Template Fallback**: Defined a "Soft-Fail" policy where missing named templates fallback to `"default"` rather than failing the delivery, prioritizing safety over aesthetics.


# Architectural Impact Report: Notification & Digest Domain

> **Range**: NOTIF-003 to NOTIF-007
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of the Notification Delivery, Retry, and Digest generation flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To resolve O(N) scaling issues identified during the Digest Scheduling simulation (`NOTIF-003`), the database schema was optimized for high-frequency polling.

| Change | Purpose |
| :--- | :--- |
| **`organizations.next_digest_at`** | A denormalized `TIMESTAMPTZ` column (Indexed). Prevents the Scheduler from performing full-table JSONB scans to find "Orgs due for digest now." |

## 2. Type System & Configuration (`01-foundation-types.md`)

New configuration fields were introduced to handle edge cases discovered in platform formatting and digest user experience.

*   **`DigestConfig` Expansion**: Added `SendEmpty` (bool) to allow suppressing "nothing happened" emails, and `TemplateSet` (string) to decouple digest styling from individual WatchPoint templates.
*   **`Channel.Config` Formalization**: Explicitly defined the `platform_override` key in the configuration map. This resolves `NOTIF-007` where regex-based platform detection fails for users behind proxies (e.g., Ngrok).

## 3. Resilience & Delivery Logic (`08a-notification-core.md`, `08c-webhook-worker.md`)

Simulation of failure modes (`NOTIF-004`, `NOTIF-005`) necessitated a more robust state machine for deliveries.

*   **Retry Visibility**: Mandated that workers **MUST update the database** (`status='retrying'`) *before* re-queuing a message to SQS. This ensures the "Last Attempted" timestamp in the dashboard reflects reality during backoff periods, rather than staying stale for minutes.
*   **Long-Delay Parking**: Defined a protocol for Rate Limits exceeding SQS visibility caps (15 minutes). Workers now "Park" these deliveries in the DB with `status='deferred'`, relying on the `RequeueDeferredNotifications` job to wake them up, rather than looping or dropping them.
*   **Payload Truncation**: Enforced a "Top N + Link" truncation strategy for both Webhooks and Email Digests to prevent downstream rejection (Slack block limits) or rendering timeouts (SendGrid).

## 4. Concurrency Strategy (`08b-email-worker.md`)

Simulation of the Bounce Feedback loop (`NOTIF-006`) revealed a race condition when disabling a specific channel within a JSONB array while a user might be editing the WatchPoint.

*   **Optimistic Locking**: Defined a strict pattern for the Bounce Processor. It reads the `config_version`, modifies the JSON in memory, and performs an UPDATE with `WHERE id=$id AND config_version=$version`. If the update affects 0 rows, it refetches and retries, ensuring user edits are never silently overwritten by background processes.

## 5. Responsibility Shifts

*   **Digest Scheduling**: Responsibility for calculating the "Next Run" was shared. The **API Handler** updates `next_digest_at` when preferences change, and the **Scheduler** updates it after a run. This keeps the field eventually consistent without complex database triggers.


# Architectural Impact Report: Escalation, Clearance, & Lifecycle

> **Range**: NOTIF-008 to WPLC-002
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Escalation logic, Clearance events, and WatchPoint Lifecycle flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To support independent cooldowns for escalation and context-rich clearance alerts, the evaluation state schema was expanded.

| Change | Purpose |
| :--- | :--- |
| **`trigger_value`** | Added to `watchpoint_evaluation_state`. Persists the actual value that caused the initial trigger. Critical for Hysteresis calculations and providing "Wind dropped from X to Y" context in clearance alerts. |
| **`last_escalated_at`** | Added to `watchpoint_evaluation_state`. Tracks the timestamp of the last *escalation* event specifically, allowing the Policy Engine to enforce a 2-hour cooldown on escalations independent of routine trigger notifications. |
| **`escalation_level`** | Formalized as a persistent column (Integer) to track the current severity tier (0-3). |

## 2. Type System & Validation (`01-foundation-types.md`)

Simulation of `WPLC-001` (Create) revealed a duplication risk for validation logic between the Go API and Python Worker.

*   **Hoisted `StandardVariables`**: Moved the variable definitions (ID, Unit, Range) from the Forecast Service (`05c`) to Foundation Types (`01`). This establishes a single source of truth used by the API validator (`POST /watchpoints`) and the Python Eval Worker (`GetSnapshot`).
*   **Enriched `ConditionResult`**: Added `PreviousValue` field. This allows the Python Worker to inject the stored `trigger_value` into the notification payload, enabling rich "Clearance" messages without requiring database lookups by the Notification Worker.

## 3. Resilience & Self-Healing (`08a-notification-core.md`)

Simulation of `NOTIF-010` (All Channels Failed) identified a critical "Zombie State" risk where the system believed a user was notified when they were not.

*   **Compensatory Transaction**: Implemented `ResetNotificationState`. If the Notification Worker detects that *all* configured channels have failed terminally (e.g., Email Blocked + Webhook 410), it executes a rollback on `watchpoint_evaluation_state` (clearing `last_notified_at`). This forces the Evaluation Engine to re-attempt the alert in the next cycle.
*   **Aggregate Failure Detection**: Added `CheckAggregateFailure` logic to the Delivery Manager to efficiently query if any sibling deliveries are still viable.

## 4. API & Lifecycle Logic (`05b-api-watchpoints.md`)

*   **Soft Forecast Dependency**: Mandated that `GetSnapshot` failures (e.g., S3 timeout) during WatchPoint creation must **not** block creation. The API now logs a warning and returns `current_forecast: null` to ensure high availability for the write path.
*   **Monitor Mode "Quiet Create"**: Enforced a policy that creating a Monitor Mode WatchPoint does **not** trigger an immediate async evaluation. It waits for the next scheduled Batcher run. This aligns with `EVAL-005` (Baseline Establishment) and prevents "Alert Shock" where a user is immediately bombarded with notifications upon setup.

## 5. Event Sequencing

*   **Ordering Logic**: Clarified that `event_sequence` is incremented **only** when a notification is inserted, not on every evaluation tick. This ensures client-side webhook ordering represents a gap-free timeline of alerts.


# Architectural Impact Report: WatchPoint Lifecycle & Efficiency

> **Range**: WPLC-003 to WPLC-007
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of WatchPoint updates, bulk operations, and resume logic.

## 1. Targeted Evaluation Mechanism (`01-foundation-types.md`, `07-eval-worker.md`)

To prevent the inefficiency of re-evaluating an entire geographic tile (potentially 500+ WatchPoints) when a single user updates or resumes one WatchPoint, the messaging contract was refined.

*   **`EvalMessage` Expansion**: Added `SpecificWatchPointIDs` field to the SQS message struct.
*   **Worker Filtering**: The Eval Worker logic was updated to check this field immediately after fetching data. If populated, the worker filters the processing batch to *only* the specified IDs, enabling low-latency, targeted re-evaluation for lifecycle events.

## 2. State Initialization Authority (`05b-api-watchpoints.md`, `07-eval-worker.md`)

Simulation of Bulk Import (`WPLC-011`) revealed that synchronous insertion of `watchpoint_evaluation_state` rows created unnecessary database contention and latency.

*   **Lazy Initialization**: Shifted the authority for creating state records from the API to the Eval Worker. The API no longer inserts into `watchpoint_evaluation_state` during creation.
*   **Worker Logic**: The Eval Worker now detects missing state records during its fetch phase and creates them on-the-fly (`EVAL-005`), decoupling the write path from the read/eval path.

## 3. Resume Hygiene & Stale Alert Prevention (`02-foundation-db.md`, `08a-notification-core.md`)

A user experience risk was identified where resuming a WatchPoint that had been paused during "Quiet Hours" would release a flood of stale, deferred notifications.

*   **New Repository Method**: Added `CancelDeferredDeliveries` to the `NotificationRepository` and `DeliveryManager`.
*   **Resume Logic**: The `Resume` handler in the API now explicitly purges any pending `deferred` deliveries for that WatchPoint before triggering a fresh evaluation. This ensures the user receives one up-to-date alert rather than a backlog of obsolete ones.

## 4. Bulk Operation Optimization (`05b-api-watchpoints.md`)

To ensure stability under load, the Bulk Import flow was strictly defined to favor throughput over immediate consistency.

*   **Quiet Create Policy**: Formalized a policy where `POST /bulk` does **not** fetch forecast snapshots (returns `null`) and does **not** trigger immediate evaluation. These items wait for the next scheduled Batcher cycle, preventing S3 latency spikes from impacting API availability.


# Architectural Impact Report: User & Organization Management

> **Range**: USER-001 to USER-005
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of the User Signup, Organization Management, and Invitation flows.

## 1. Repository Interface Extensions (`02-foundation-db.md`)

To support the immediate cessation of evaluation costs upon organization deletion, a new bulk operation was added to the data access layer.

| Change | Purpose |
| :--- | :--- |
| **`PauseAllByOrgID`** | Added to `WatchPointRepository`. This allows the `OrganizationHandler` to synchronously transition all WatchPoints belonging to a specific Org ID to `paused` status within the deletion transaction, ensuring no zombie evaluation load persists after soft-delete. |

## 2. Consistency & Transaction Patterns (`05d-api-organization.md`)

Simulation of the Signup (`USER-001`) and Delete (`USER-003`) flows revealed critical risks regarding distributed state consistency between the local database and Stripe.

*   **Signup Sequence**: Formalized a "Database First, Best-Effort External" pattern. The handler now commits the Organization and User to Postgres *before* calling Stripe. Failures in Stripe are logged as warnings rather than rolling back the user account, ensuring the user can still access the platform (repair handled lazily).
*   **Zombie Billing Prevention**: For Organization Deletion, the logic was hardened to "Fail Fast". If the `BillingService.CancelSubscription` call returns an error (e.g., Stripe API down), the entire deletion request aborts with a 500 error. This guarantees we never delete a user's access locally while they continue to be billed externally.
*   **Cascading Pause**: The `Delete` handler now explicitly calls `PauseAllByOrgID` before soft-deleting the organization record.

## 3. Dependency Injection Enhancements (`05d-api-organization.md`)

To decouple hardcoded logic from handlers, dependency injection was expanded.

*   **`PlanRegistry`**: Injected into `OrganizationHandler` to drive plan limits during creation, removing hardcoded "Free Tier" constants from the handler code.
*   **`WatchPointRepository`**: Added as a dependency to `OrganizationHandler` to enable the cascading pause logic described above.

## 4. Authentication Atomicity (`05f-api-auth.md`)

Simulation of the Invite Acceptance flow (`USER-005`) identified a potential split-brain state where a token could be consumed without successfully logging the user in.

*   **Transactional Acceptance**: The `AuthService.AcceptInvite` method was redefined to execute the User Status Update (`invited` -> `active`) and Session Creation within a **single database transaction**. If session creation fails, the entire operation rolls back, keeping the invite token valid for a retry.
*   **Implicit OAuth Acceptance**: The OAuth linking logic was updated to explicitly handle `invited` users. Successful OAuth login now transitions these users to `active` and clears the invite token, removing friction for users who prefer social login over password setup.

## 5. Error Handling Standards (`05d-api-organization.md`)

*   **Invite Conflict**: The Invite handler now explicitly catches unique constraint violations on the email column and maps them to a **409 Conflict** response with a "User already exists" message, replacing generic 500 errors.
*   **Synchronous Email**: Transactional emails (Invites) are now sent synchronously. If the provider fails, the API returns a 500 error, ensuring the admin is aware that the invite was not delivered.


# Architectural Impact Report: User Authentication & Management

> **Range**: USER-006 to USER-010
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of User Login, Password Reset, and Role Management flows.

## 1. Security & Session Architecture (`05f-api-auth.md`, `05a-api-core.md`)

Simulation of the Login (`USER-006`) and Password Reset (`USER-008`) flows necessitated stricter security controls to prevent credential theft and flooding attacks.

| Change | Purpose |
| :--- | :--- |
| **Cookie-Only Session IDs** | Removed `SessionID` from the `AuthResponse` JSON struct. Mandated that session identifiers be delivered **only** via `HttpOnly`, `Secure`, `SameSite=Lax` cookies to prevent XSS exfiltration. |
| **Hard Session Invalidation** | The `CompletePasswordReset` logic was hardened to perform a **hard delete** of *all* active sessions for the user (mobile and desktop) to ensure immediate access revocation upon credential compromise. |
| **Email-Based Rate Limiting** | Introduced a specific `RateLimiter` key strategy (`email_reset_limit:{email}`) for the Password Reset flow. This decouples flood protection from IP addresses, preventing Denial-of-Service attacks against specific user inboxes. |
| **Live Role Evaluation** | Updated `AuthMiddleware` to execute a database `JOIN` on every request to fetch the user's *current* Role and OrganizationID. This replaces reliance on potentially stale session data, ensuring role demotions or bans take effect immediately. |

## 2. Authentication Integrity (`05f-api-auth.md`)

To prevent account hijacking and split-brain states during OAuth flows (`USER-007`), the authentication logic was formalized.

*   **Strict Provider Linking**: Defined a policy where `LoginWithOAuth` MUST return a `409 Conflict` if a user attempts to sign in with a provider (e.g., Google) that differs from their linked provider (e.g., GitHub). Overwriting or merging identities is explicitly prohibited.
*   **Atomic Implicit Acceptance**: The OAuth login flow for *invited* users was redefined as a single transaction. It must verify the invite, transition the user status to `active`, clear invite tokens, link the provider, and create the session atomically. Failure at any step rolls back the entire operation, preserving the invite for retry.

## 3. Concurrency & Data Ownership (`05d-api-organization.md`)

Simulation of User Removal (`USER-010`) and Role Updates (`USER-009`) revealed race conditions and data loss risks.

*   **Owner Safety Locking**: The logic for counting owners (to prevent removing the last one) was updated to require `SELECT ... FOR UPDATE` or serializable transaction isolation. This prevents concurrent requests from race-conditioning the owner count down to zero.
*   **API Key Persistence Policy**: Explicitly defined API Keys as **organizational assets**. When a user is deleted, their keys are **not** revoked. Instead, the `created_by_user_id` field is set to `NULL` (via database `ON DELETE SET NULL`), preventing operational outages for CI/CD pipelines created by departing employees.


# Architectural Impact Report: User Credentials & Billing Resilience

> **Range**: USER-011 to BILL-001
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of API Key lifecycle management and Stripe customer provisioning flows.

## 1. Security & Credential Management (`05d-api-organization.md`, `05a-api-core.md`)

Simulation of API Key Creation (`USER-011`) and Revocation (`USER-013`) necessitated stricter definitions for secret handling and state precedence to prevent leakage and unauthorized access.

| Change | Purpose |
| :--- | :--- |
| **Memory-Only Generation** | Explicitly defined that `APIKeyHandler` generates plaintext secrets in memory, hashes them immediately, and **never** passes the plaintext to the repository layer. The plaintext is injected directly into the response object and discarded. |
| **Revocation Precedence** | Updated `Authenticator` logic to strictly enforce that `revoked_at IS NULL` overrides any future `expires_at` timestamp. This ensures that compromised keys can be killed immediately even if they were recently rotated with a grace period. |

## 2. Type System Refinements (`01-foundation-types.md`)

To resolve circular dependencies and support internal operational alerts, type definitions were reorganized.

*   **Hoisted `AllScopes`**: Moved the authoritative list of valid scopes from the Organization Handler to Foundation Types. This allows the Core Middleware (`05a`) to validate scopes without depending on the domain logic layer.
*   **`EventSystemAlert`**: Added to the `EventType` enum. This formalizes a channel for internal system messages (like "Channel Disabled" warnings) distinct from customer-facing weather alerts.

## 3. System Notification Architecture (`08b-email-worker.md`, `02-foundation-db.md`)

Simulation of the Bounce Handling flow (`USER-014`) revealed that the Worker layer lacked the context required to notify administrators of failures.

*   **Owner Resolution**: Added `GetOwnerEmail` to the `UserRepository` interface. This allows the `BounceProcessor` to look up the organization owner's contact info dynamically when a channel is disabled.
*   **Dependency Injection**: Updated the `EmailWorker` structure to accept `UserRepository`, bridging the gap between the delivery and user domains for operational recovery flows.

## 4. Billing Consistency & Self-Healing (`10-external-integrations.md`, `09-scheduled-jobs.md`)

Simulation of the "Best-Effort" Stripe provisioning flow (`BILL-001`) identified race conditions and potential data inconsistency risks.

*   **Search-Before-Create**: The `BillingService.EnsureCustomer` logic was hardened to query the Stripe Search API for existing metadata (`org_id`) before attempting creation. This prevents duplicate customer records if a user double-clicks or triggers checkout concurrently with signup.
*   **Headless Repair Strategy**: Expanded the `StripeSyncer` scheduled job to include a specific sweep for active organizations with `NULL` Stripe IDs, ensuring that failures in the initial signup flow are eventually consistent without manual intervention.


# Architectural Impact Report: Billing & Subscription Lifecycle

> **Range**: BILL-002 to BILL-006
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Billing, Subscription, and Dunning flows.

## 1. Database Schema Extensions (`02-foundation-db.md`)

To support granular lifecycle management and accurate grace period tracking, the schema was expanded to distinguish between user intent and system enforcement.

| Change | Purpose |
| :--- | :--- |
| **`overage_started_at`** | Added to `organizations`. Tracks the start of the 14-day grace period for usage overages (e.g., after a plan downgrade). Cleared automatically when usage drops below limits. |
| **`paused_reason`** | Added to `watchpoints`. Valid values (`user_action`, `billing_delinquency`) allow the system to differentiate between resources paused by the user vs. those paused for non-payment. |

## 2. Type System Refinements (`01-foundation-types.md`)

*   **`PausedReason` Enum**: Formalized constants for the new `paused_reason` column to ensure strict typing across the API and Scheduler components.

## 3. Repository & Service Capabilities (`02-foundation-db.md`, `09-scheduled-jobs.md`)

Simulation of the Downgrade Enforcement flow (`BILL-011`) and Payment Recovery (`BILL-013`) revealed the need for targeted bulk operations.

*   **Smart Bulk Resume**: Added `ResumeAllByOrgID(ctx, orgID, reason)` to the `WatchPointRepository`. This ensures that when an invoice is paid, the system *only* resumes WatchPoints paused for `billing_delinquency`, leaving user-paused items untouched.
*   **Excess Pruning**: Added `PauseExcessByOrgID(ctx, orgID, limit)` to atomically pause the most recently created WatchPoints until the account complies with new plan limits.
*   **Subscription Enforcer**: Defined a new `SubscriptionEnforcer` service interface in the Scheduler domain. This isolates the business logic for negative lifecycle events (enforcing payment failures and overages) from the generic job scheduling infrastructure.

## 4. API & Webhook Logic Hardening (`05e-api-billing.md`)

To resolve UX race conditions and ensure reliable alerting during payment failures, the handler logic was evolved.

*   **Opportunistic Sync**: The `GetSubscription` endpoint was updated to synchronously update the local database if it detects a mismatch with Stripe. This fixes the race condition where a user pays and immediately redirects to the dashboard before the webhook arrives.
*   **Explicit Alerting**: The `invoice.payment_failed` webhook handler was updated to explicitly insert a `billing_alert` record into the `notifications` table. This replaces reliance on passive audit logs, ensuring critical dunning emails are delivered via the robust `EmailWorker` infrastructure.
*   **Auto-Resume Trigger**: The `invoice.paid` webhook handler was mandated to call the new `ResumeAllByOrgID` method, enabling a "zero-touch" service restoration experience for customers.


# Architectural Impact Report: Billing Integration & Enforcement

> **Range**: BILL-007 to BILL-011
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Billing, Usage Warning, and Overage Enforcement flows.

## 1. Database Schema Extensions (`02-foundation-db.md`)

To support efficient rate limit warning tracking and precise bulk operation ordering, the schema was expanded.

| Change | Purpose |
| :--- | :--- |
| **`warning_sent_at`** | Added to `rate_limits`. Tracks whether an 80% usage warning has been sent for the current cycle, preventing alert spam on subsequent API calls. |
| **`last_reset_at`** | Added to `rate_limits`. Used to validate when the warning flag should be cleared (on cycle reset). |

## 2. Type System Refinements (`01-foundation-types.md`)

*   **`EventBillingWarning`**: Added to the `EventType` enum. This formalizes the specific event used when usage exceeds safe thresholds, allowing the Notification Worker to route it to the correct email template.

## 3. Middleware & API Logic Hardening (`05a-api-core.md`, `05b-api-watchpoints.md`)

Simulation of high-frequency API flows (`BILL-009`) and critical resource creation (`BILL-008`) necessitated strict consistency strategies.

*   **Direct Database Rate Limiting**: Explicitly defined the v1 strategy as "Serverless-First / No-Redis". The middleware performs atomic updates directly on the `rate_limits` table using row-level locking, rather than relying on a cache layer.
*   **Overage Warning Logic**: Implemented the "80% Threshold Check" directly within the middleware. It atomically updates `warning_sent_at` and inserts a notification record, ensuring users are alerted before service interruption.
*   **Strict Limit Enforcement**: The `Create` endpoint's limit check was hardened to use a direct `SELECT COUNT(*)` query. This avoids the drift risks associated with cached counters, ensuring strict compliance with plan limits.

## 4. Repository & Service Capabilities (`02-foundation-db.md`, `09-scheduled-jobs.md`)

Simulation of the Overage Enforcement flow (`BILL-011`) defined the rules for handling non-compliant accounts.

*   **LIFO Pause Strategy**: The `PauseExcessByOrgID` repository method was explicitly documented to use a **Last-In-First-Out** strategy (`ORDER BY created_at DESC`). This ensures that enforcement actions pause the newest resources first, preserving the user's long-standing monitors.
*   **Overage Resolution Authority**: The `UsageAggregator` service was designated as the sole authority for clearing the `overage_started_at` flag. It compares daily usage against limits and clears the flag if the user has remediated their overage, closing the compliance loop.

## 5. Billing Auto-Recovery (`05e-api-billing.md`)

To provide a seamless user experience upon payment recovery (`BILL-007`):

*   **Auto-Resume**: The `invoice.paid` webhook handler was mandated to call `ResumeAllByOrgID` with the specific reason code `PausedReasonBillingDelinquency`. This restores service immediately without manual intervention while respecting user-initiated pauses.


# Architectural Impact Report: Billing & Observability

> **Range**: BILL-012 to OBS-003
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Billing History, Receipt Delivery, and Observability flows.

## 1. Infrastructure Enhancements (`04-sam-template.md`)

To meet high-availability requirements and ensure no critical path goes unmonitored (`OBS-003`), the alarm configuration was significantly expanded.

| Change | Purpose |
| :--- | :--- |
| **`EvalQueueUrgentDepthAlarm`** | Added specific monitoring for the Nowcast queue. Since Nowcast alerts are time-critical (<60s latency target), this alarm triggers at a much lower threshold (100 messages) than the standard queue to detect backlogs instantly. |
| **`NotificationQueueDepthAlarm`** | Added monitoring for the delivery queue. Ensures that even if evaluation succeeds, users aren't impacted by a stuck notification worker pool. |

## 2. Type System Refinements (`01-foundation-types.md`)

To support distinct templating for financial transactions (`BILL-013`), the event taxonomy was extended.

*   **`EventBillingReceipt`**: Added to the `EventType` enum. This formalizes the event triggered by successful payments, allowing the Notification Worker to map it specifically to the `receipt_default` template set, separating it from operational alerts.

## 3. API Logic Hardening (`05e-api-billing.md`)

Simulation of the Usage Reporting flow (`BILL-012`) and Invoice Payment flow (`BILL-013`) clarified the logic required for data accuracy and user communication.

*   **Usage Data Continuity**: The `UsageHandler` logic was refined to explicitly require a **Union** operation. It must combine immutable historical data (from `usage_history`) with real-time partial stats (from `rate_limits` and `watchpoints` count) for the current day. This prevents "data lag" where the dashboard shows 0 usage for today until the nightly job runs.
*   **Explicit Receipt Trigger**: The `StripeWebhookHandler` logic for `invoice.paid` was expanded. Beyond just resuming paused resources, it must now explicitly insert a notification record with `event_type='billing_receipt'`. This delegates the actual email delivery to the robust `EmailWorker` infrastructure rather than attempting synchronous sending within the webhook.


# Architectural Impact Report: Observability & Maintenance

> **Range**: OBS-004 to OBS-008
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Observability flows, specifically regarding Verification, Calibration, and Post-Event Summaries.

## 1. Database Schema Extensions (`02-foundation-db.md`)

To support long-term persistence of accuracy reports and safe automation of model calibration, the schema was expanded.

| Change | Purpose |
| :--- | :--- |
| **`watchpoints.summary` (JSONB)** | Added to persist the "Predicted vs Actual" accuracy report generated upon archival. This allows the API to serve historical performance data directly from the WatchPoint record without re-computing heavy metrics. |
| **`calibration_candidates` Table** | Created to store calibration coefficient updates that exceed safety bounds (e.g., >15% delta). This table acts as a holding area for manual operator review, preventing automated regression jobs from destabilizing production models. |

## 2. Type System & Contracts (`01-foundation-types.md`)

Strict typing was introduced to support polymorphic behaviors in the RunPod and Evaluation workers.

*   **Polymorphic `InferencePayload`**: Added a `TaskType` enum (`inference`, `verification`, `calibration`) to the RunPod contract. This formally repurposes the GPU worker as a general-purpose "Scientific Compute" node, allowing it to handle heavy verification math alongside forecasting.
*   **`EvalMessage` Actions**: Added an `Action` field (`evaluate`, `generate_summary`) to the SQS message struct. This allows the Archiver to task the Evaluation Worker with generating post-event summaries, reusing its existing Zarr/S3 I/O capabilities.
*   **Canary Metadata Standard**: Defined a strict JSON schema (`system_role: canary`) for WatchPoint metadata to enable generic, hardcoded-logic-free verification jobs.

## 3. Worker Capability Evolution

Simulation revealed that heavy mathematical operations (Regression, RMSE calculation) and heavy I/O (Historical Zarr reads) were ill-suited for the lightweight Go `Archiver` Lambda.

*   **RunPod Expansion (`11-runpod.md`)**: The interface contract was updated to accept `task_type`. Internal routing was defined to dispatch specific handlers (`verification.py`, `calibration.py`) based on this type, offloading heavy compute from the orchestration layer.
*   **Eval Worker Scope (`07-eval-worker.md`)**: The worker's responsibility was expanded beyond simple condition evaluation. It now includes a `SummaryGenerator` component, triggered by the `generate_summary` action, which leverages the worker's specialized Python environment for historical data analysis.

## 4. Service Logic Refinements (`09-scheduled-jobs.md`)

*   **Orchestration vs. Execution**: The `VerificationService` and `CalibrationService` interfaces were explicitly redefined as orchestrators. Their implementation responsibility shifted from "computing results" to "constructing payloads and triggering RunPod," ensuring the Go layer remains lightweight and Python layers remain specialized.


# Architectural Impact Report: Operational Health & Maintenance

> **Range**: OBS-009 to MAINT-002
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Observability, Anomaly Detection, and Maintenance flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To enable efficient, atomic cleanup of expired resources without complex application-side recursion, the referential integrity constraints were tightened.

| Change | Purpose |
| :--- | :--- |
| **`ON DELETE CASCADE`** | Added to foreign keys on `notifications` and `notification_deliveries` tables pointing to `watchpoints`. This allows the `PurgeArchivedWatchPoints` job (`MAINT-002`) to delete a single parent record and rely on the database engine to clean up thousands of child records efficiently, preventing orphan data and constraint violations. |

## 2. API & Telemetry Evolution (`05a-api-core.md`, `01-foundation-types.md`)

Simulation of the Metrics Emission flow (`OBS-010`) and Audit Querying (`OBS-011`) revealed gaps in the middleware stack and administrative capabilities.

*   **New `MetricsMiddleware`**: Introduced a dedicated middleware layer to capture request duration and status codes. It explicitly uses the `MetricAPILatency` and `MetricAPIRequestCount` constants to ensure standardized emission to CloudWatch, closing the observability loop.
*   **Audit Log Querying**: Formalized the `AuditQueryFilters` struct and added the `ListAuditLogs` handler logic to `05d-api-organization.md`. This exposes a new `GET /organization/audit-logs` endpoint, allowing administrators to inspect the `audit_log` table via the API, which was previously write-only.

## 3. Infrastructure Hardening (`04-sam-template.md`)

To support proactive anomaly detection and automated cost optimization, the infrastructure definition was expanded.

*   **Notification Spike Alarm**: Added `NotificationSpikeAlarm` monitoring the `WatchPoint/DeliveryAttempt` metric. This satisfies the trigger condition for `OBS-009` (Anomaly Detection), alerting operators to potential spam loops or denial-of-service events.
*   **S3 Lifecycle Rules**: Explicitly defined the `LifecycleConfiguration` for the Forecast Bucket. This automates `MAINT-001` (Forecast Tier Transition) by enforcing physical deletion of `nowcast/` objects after 7 days and `medium_range/` objects after 90 days at the bucket level, ensuring storage costs remain bounded.


# Architectural Impact Report: Maintenance & Data Lifecycle

> **Range**: MAINT-003 to MAINT-007
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Data Retention, Archival, and Cleanup flows.

## 1. Database Schema Refinements (`02-foundation-db.md`)

To enable efficient, atomic cleanup of expired resources without complex application-side recursion, the referential integrity constraints were significantly tightened.

| Change | Purpose |
| :--- | :--- |
| **`ON DELETE CASCADE`** | Added to foreign keys on `users` and `watchpoints` pointing to `organizations`. This allows the `PurgeSoftDeletedOrgs` job (`MAINT-005`) to issue a single `DELETE` statement for an organization and rely on the database engine to clean up all child resources, preventing orphan data and constraint violations. |
| **`audit_log` Independence** | Modified `organization_id` to be **nullable** and changed the foreign key to `ON DELETE SET NULL`. This ensures that when an organization is hard-deleted for GDPR/cleanup (`MAINT-005`), the compliance audit trail remains intact and accessible, rather than being deleted along with the tenant. |

## 2. Infrastructure Hardening (`04-sam-template.md`)

To support cost-effective long-term retention of compliance data (`MAINT-004`), dedicated cold storage infrastructure was provisioned.

*   **`ArchiveBucket` Resource**: Added a specific S3 bucket resource with a `LifecycleConfiguration` that automatically transitions objects to `GLACIER` storage class after 1 day. This optimizes costs for audit logs that must be kept for 2 years but are rarely accessed.
*   **Archiver Permissions**: Explicitly granted the `ArchiverFunction` IAM role `s3:PutObject` permissions on this new bucket, resolving a permission gap identified during simulation.

## 3. Repository & Service Capabilities (`09-scheduled-jobs.md`, `02-foundation-db.md`)

Simulation of the bulk deletion flows revealed the need for optimized data access patterns.

*   **Retention Interfaces**: Added `DeleteBefore` to `NotificationRepository` and `ListOlderThan` / `DeleteIDs` to `AuditRepository`. These allow the cleanup services to operate on time-based windows efficiently.
*   **JSONB Pruning**: Added `PruneSeenThreats` to `WatchPointRepository`. This supports `MAINT-007` by executing specialized JSONB path updates to remove expired threat objects from the `watchpoint_evaluation_state`, preventing unbounded growth of the deduplication history.


# Architectural Impact Report: API Mechanics & Scheduled Maintenance

> **Range**: SCHED-006 to API-004
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of API Core mechanics and Scheduled Maintenance flows.

## 1. Middleware Architecture Evolution (`05a-api-core.md`)

To resolve dependency conflicts discovered during the simulation of Idempotency and Rate Limiting flows, the middleware stack was restructured.

| Change | Purpose |
| :--- | :--- |
| **Middleware Reordering** | Moved `AuthMiddleware` **before** `RateLimit` and `Idempotency` middleware. This ensures that the `OrganizationID` (injected by Auth) is available in the context when Rate Limits and Idempotency keys are evaluated, correcting a critical dependency gap. |
| **Response Capturer Pattern** | Explicitly defined the `ResponseCapturer` wrapper within `IdempotencyMiddleware`. This component buffers the status code, headers, and body stream, enabling the system to persist the full response to the `idempotency_keys` table for future replays. |

## 2. Security & Validation Hardening (`05a-api-core.md`)

Simulation of SSRF protection and Authentication flows necessitated stricter validation logic and error handling precision.

*   **Fail-Closed SSRF Validation**: The `ssrf_url` custom validator was refined to use a **strict 500ms timeout** on DNS resolution. If resolution times out, the validator returns an error (Fail Closed) rather than hanging the API request, protecting system availability.
*   **Precise Token Expiration**: The `Authenticator.ResolveToken` logic was updated to decouple database filtering from expiry checks. The query now filters only on `revoked_at`, allowing the application logic to distinguish between `ErrAuthTokenInvalid` (hash mismatch) and `ErrAuthTokenExpired` (valid hash, old timestamp), improving client feedback.

## 3. Operational Stability (`05a-api-core.md`, `09-scheduled-jobs.md`)

To prevent resource exhaustion and unbounded data growth, operational boundaries were tightened.

*   **Concurrent Health Checks**: The `HandleHealth` endpoint was redefined to execute Database, S3, and Queue probes in parallel goroutines with a **global 2-second timeout**. This prevents cascading failures from blocking the health endpoint and causing load balancer failures.
*   **Idempotency Cleanup**: A new scheduled task `TaskCleanupIdempotencyKeys` was added to the Archiver/CleanupService. This daily job purges expired records from the `idempotency_keys` table, preventing unbounded storage growth.

## 4. Scope Adjustments

*   **SCHED-006 Deferred**: The "Webhook Health Pre-Check" flow was explicitly deferred to v2. Simulation determined that proactive probing introduces excessive complexity (privacy/blocklisting) relative to the value of reactive circuit breaking already implemented in the Webhook Workers.


# Architectural Impact Report: Informational & History Domain

> **Range**: INFO-001 to INFO-005
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Informational flows (Notification History, Usage Reporting, and Invoices).

## 1. Database Schema & Repository Evolution (`02-foundation-db.md`)

To support efficient organization-wide querying and dynamic usage aggregation, the data access layer was optimized.

| Change | Purpose |
| :--- | :--- |
| **`idx_notif_org` Index** | Added `CREATE INDEX idx_notif_org ON notifications(organization_id, created_at DESC)` to enable performant pagination for the new Organization Notification History endpoint. |
| **Repo Consolidation** | Moved `ListNotifications` logic from `WatchPointRepository` to `NotificationRepository`. Added a unified `List` method accepting a `NotificationFilter`, centralizing the complex join logic required to hydrate delivery statuses. |
| **Dynamic Aggregation** | Updated `UsageHistoryRepository.Query` to accept a `TimeGranularity` parameter, mandating that aggregation (e.g., `date_trunc`) happens at the database level rather than in memory. |

## 2. Type System Refinements (`01-foundation-types.md`)

Simulation of dual-scope notification retrieval (WatchPoint vs. Organization) revealed a need for shared Data Transfer Objects (DTOs).

*   **Hoisted `NotificationHistoryItem`**: Moved this struct from the WatchPoint Handler to Foundation Types. Enhanced it with a `Channels` field (slice of `DeliverySummary`) to provide delivery status visibility (e.g., "Email: Sent", "Webhook: Failed") directly in the listing response.
*   **New `NotificationFilter`**: Defined a standardized filter struct for repository queries, supporting filtering by Organization, WatchPoint, Event Type, and Urgency.

## 3. API Logic & Permissions

To ensure data consistency and appropriate access control, handler logic was hardened.

*   **Organization History (`05d-api-organization.md`)**: Added a new endpoint `GET /organization/notifications`. It utilizes the consolidated `NotificationRepository` and requires `RoleMember` permission, promoting transparency.
*   **Strict RBAC for Invoices (`05e-api-billing.md`)**: Explicitly locked the `GET /billing/invoices` endpoint to `RoleAdmin` and `RoleOwner`, enforcing stricter security for financial data.
*   **Direct Count Policy (`05e-api-billing.md`)**: Mandated that the Current Usage endpoint (`INFO-003`) must use a **Direct Count** query (`SELECT COUNT(*)`) against the `watchpoints` table rather than relying on cached counters in `rate_limits`. This prioritizes accuracy over read-optimization for the billing dashboard.

## 4. Integration Contracts (`10-external-integrations.md`)

*   **Pagination Mapping**: Clarified the `BillingService` contract for Stripe proxies. The implementation must map the domain `Cursor` to Stripe's `starting_after` parameter and derive the `NextCursor` from the last ID in the result set, bridging the gap between internal and external pagination styles.


# Architectural Impact Report: Informational & Dashboard Domain

> **Range**: INFO-006 to DASH-002
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Dashboard Authentication, Billing Synchronization, and Forecast Verification flows.

## 1. Database Schema & Repository Evolution (`02-foundation-db.md`)

To support high-performance dashboard reporting, the data access layer was expanded to handle heavy aggregation at the database level.

| Change | Purpose |
| :--- | :--- |
| **`VerificationRepository`** | Added a specialized interface for accessing forecast accuracy data. |
| **`GetAggregatedMetrics`** | Defined a method to perform SQL-level aggregation (`AVG`, `GROUP BY`) on the `verification_results` table. This prevents fetching thousands of raw rows to the application layer, optimizing the `INFO-007` dashboard view. |

## 2. Type System Refinements (`01-foundation-types.md`)

New structures were defined to standardize the exchange of accuracy metrics between the repository and the API.

*   **`VerificationMetric` Struct**: Defined to capture aggregated results (e.g., "Average RMSE for Temperature on Date X"). This acts as the DTO for the new repository method.
*   **`VerificationReport`**: Defined as the API response envelope, grouping metrics by variable for frontend consumption.

## 3. Security & Middleware Hardening (`05a-api-core.md`)

Simulation of the Dashboard Login flows (`DASH-001/002`) highlighted the need for browser-specific protections that were previously implicit.

*   **`CSRFMiddleware`**: Explicitly added a middleware function that validates `X-CSRF-Token` headers. Crucially, this middleware logic is **conditional on Actor Type**: it enforces validation only for Session-based (cookie) requests while skipping it for API Key requests, ensuring API clients are not burdened with browser security semantics.
*   **Middleware Ordering**: Updated the global stack to place `CSRFMiddleware` *after* `AuthMiddleware`, as it depends on the injected Actor context to determine enforcement logic.

## 4. API Logic & Consistency

To ensure data integrity and seamless user onboarding, handler logic was significantly evolved.

*   **Opportunistic Billing Sync (`05e-api-billing.md`)**: The `GetSubscription` handler was updated to include a synchronous call to Stripe. It compares the remote state with the local DB and issues an immediate `UPDATE` if drift is detected (e.g., webhook lag), ensuring the user sees their paid status immediately after checkout.
*   **JIT Provisioning (`05f-api-auth.md`)**: The OAuth login flow was expanded to handle new users by automatically creating an Organization (Free Tier) and linking the provider in a single transaction. This removes the need for a separate signup flow for social logins.
*   **Lazy Session Cleanup (`05f-api-auth.md`)**: The Login transaction was updated to include a `DELETE` statement for expired sessions associated with the user. This "lazy maintenance" strategy prevents `sessions` table bloat without requiring aggressive background jobs.

## 5. New Capabilities (`05c-api-forecasts.md`)

*   **Verification Endpoint**: Added `GET /v1/forecasts/verification` to the Forecast API. This endpoint exposes the aggregated accuracy metrics computed by the new repository method, allowing the dashboard to display model performance transparency charts.


# Architectural Impact Report: Dashboard & UI Domain

> **Range**: DASH-003 to DASH-007
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Dashboard Session management and Billing UI integration flows.

## 1. Middleware Optimization & Logic (`05a-api-core.md`)

To prevent database write thrashing on high-traffic endpoints while maintaining accurate session expiry, the authentication middleware was refined.

| Change | Purpose |
| :--- | :--- |
| **Sliding Window Updates** | Updated `AuthMiddleware` to strictly enforce a "Sliding Window" strategy. Database updates for `last_activity_at` now occur only if the timestamp is older than 1 hour, utilizing a specific SQL `AND` condition to let the database engine filter redundant writes efficiently. |
| **Cookie Synchronization** | Mandated that if a session write occurs (extension), the middleware **must** re-issue the `Set-Cookie` header with the new expiration timestamp. This ensures the browser's local state remains synchronized with the server-side validity window. |

## 2. Security & State Management (`05f-api-auth.md`)

Simulation of the Logout flow (`DASH-004`) revealed a potential for "Zombie Cookie" risks where client-side artifacts persisted after server-side invalidation.

*   **Explicit Client Cleanup**: The `HandleLogout` logic was expanded to require sending a specific `Set-Cookie` header with `Max-Age=0` and an epoch expiration date. This forces the browser to purge the session credential immediately, rather than relying solely on server-side rejection.

## 3. Billing Resilience & Safety (`05e-api-billing.md`)

Simulation of the Checkout (`DASH-005`) and Portal (`DASH-006`) flows identified data consistency gaps and potential security vulnerabilities in redirect handling.

*   **Self-Healing Data Pattern**: Both `CreateCheckoutSession` and `CreatePortalSession` handlers were updated to invoke `BillingService.EnsureCustomer` inline before communicating with Stripe. This lazily repairs organizations with missing Stripe IDs (e.g., from failed signups), preventing 500 errors during critical revenue events.
*   **Open Redirect Prevention**: Removed client-controlled URL fields (`SuccessURL`, `CancelURL`, `ReturnURL`) from the request structs. Handlers now construct these URLs server-side using the authoritative `DashboardURL` from configuration, eliminating the risk of attackers redirecting users to malicious sites after payment.
*   **Plan Validation**: Added strict validation to `CreateCheckoutSession` to reject requests for the `'free'` plan tier with a 400 error, enforcing the architectural decision that downgrades must occur via the self-serve portal.


# Architectural Impact Report: Dashboard Optimization & Webhook Lifecycle

> **Range**: DASH-008 to HOOK-004
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Dashboard Scaling, Webhook Secret Rotation, and Platform Deprecation flows.

## 1. Type System & Schema Extensions (`01-foundation-types.md`, `02-foundation-db.md`)

To support high-performance dashboard rendering and robust sub-resource management within JSONB columns, the core types were refined.

| Change | Purpose |
| :--- | :--- |
| **`Channel.ID` (UUID)** | Added a stable identifier to the `Channel` struct. This enables precise targeting for rotation (`.../channels/{id}/rotate-secret`) without relying on brittle array indices. |
| **`WatchPointSummary` DTO** | Defined a lightweight projection for dashboard grids. Explicitly excludes heavy JSONB fields (`conditions`, `channels`) and includes joined runtime state (`trigger_status`), solving the N+1 query problem. |
| **`APIResponse.Meta.Warnings`** | Extended the standard response envelope to support non-blocking alerts (e.g., "Teams Connector deprecated"), allowing valid requests to succeed while signaling migration needs. |

## 2. Repository & Query Optimization (`02-foundation-db.md`)

Simulation of the Dashboard Load flow (`DASH-008`) and Rotation flow (`HOOK-001`) necessitated specialized data access patterns.

*   **`ListSummaries` Method**: Added a repository method performing a `LEFT JOIN` on `watchpoint_evaluation_state` to fetch identity and status in a single query, optimized for high-volume dashboards.
*   **Atomic `UpdateChannelConfig`**: Implemented a specialized update method using Postgres `jsonb_set`. This allows modifying specific channel fields (like secrets) atomically without overwriting concurrent edits to other parts of the WatchPoint configuration.
*   **`GetStats` Aggregation**: Added a dedicated method for dashboard badges (`Active: 12`, `Error: 1`) using efficient filtered `COUNT(*)` queries.

## 3. Validation & Deprecation Strategy (`05a-api-core.md`, `08c-webhook-worker.md`)

To manage the lifecycle of external integrations (e.g., Microsoft Teams connectors retiring), the validation logic was evolved.

*   **Warning-Aware Validator**: Updated the `Validator` interface to return a `ValidationResult` containing both `Errors` (blocking) and `Warnings` (non-blocking).
*   **Passive Deprecation Checks**: Mandated that `GET` (Read) handlers invoke `PlatformRegistry.CheckDeprecation`. This ensures users see migration warnings in the dashboard even if they aren't actively updating their configuration.

## 4. Operational Stability & Write Optimization (`08a-notification-core.md`, `02-foundation-db.md`)

Simulation of high-throughput notification delivery and configuration updates revealed write churn and state stability risks.

*   **Lazy Reset Strategy**: Defined a policy where Notification Workers **only** write to the database to reset `failure_count = 0` if the channel's in-memory state indicates previous failures. Successful deliveries to already-healthy channels do not trigger database writes, significantly reducing I/O operations.
*   **Config Version Exclusion**: Explicitly defined that the database trigger for `increment_config_version` MUST EXCLUDE the `channels` column. This ensures that operational changes (like rotating a webhook secret) do not invalidate the meteorological evaluation state, preserving hysteresis continuity.


# Architectural Impact Report: Webhook & Vertical Integration

> **Range**: HOOK-005 to VERT-004
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Webhook Delivery Tracking and Vertical App Integration flows.

## 1. Database Schema Extensions (`02-foundation-db.md`)

To support granular usage attribution for vertical apps and secure source tracking, the schema was significantly evolved.

| Change | Purpose |
| :--- | :--- |
| **`api_keys.source`** | Added column to persist the origin identity (e.g., "wedding_app") associated with an API key. |
| **Composite Primary Keys** | Updated `rate_limits` and `usage_history` tables to include a `source` column as part of their primary keys. This allows independent usage counters for different verticals within the same organization. |

## 2. Type System & Identity (`01-foundation-types.md`)

Strict identity propagation was introduced to ensure that resources created by vertical apps are immutably attributed.

*   **`Actor.Source` Field**: Added to the core authentication context. This field is populated by the Authenticator during token resolution and ensures the "Origin" of a request flows through to all handlers.
*   **`MetricDeliverySuccess`**: Added to standard constants to support the `HOOK-005` delivery confirmation flow without requiring database writes.

## 3. API & Middleware Logic (`05a-api-core.md`, `05b-api-watchpoints.md`)

Simulation of the Vertical WatchPoint Creation (`VERT-002`) and Template Scaling (`VERT-003`) flows necessitated strict logic boundaries.

*   **Immutable Source Attribution**: The WatchPoint `Create` handler was mandated to inject `Actor.Source` into the resource upon creation, ignoring any client-provided source field. Crucially, the `Update` handler was explicitly defined to *preserve* this original source, ensuring long-term attribution even if modified via the Dashboard.
*   **Loose Template Coupling**: Defined a "Loose Coupling" validation strategy for `template_set`. The API validates string format only, deferring existence checks to the Worker layer to avoid deployment dependencies.

## 4. Resilience & Aggregation Logic (`08b-email-worker.md`, `05e-api-billing.md`)

To ensure robustness and accurate reporting in a multi-vertical environment:

*   **Template Soft-Fail**: The Email Worker's `TemplateEngine` was updated to include a mandatory fallback logic. If a specific vertical template (e.g., "wedding") fails to render, the engine automatically retries with the "default" set, prioritizing delivery over aesthetics.
*   **Usage Aggregation**: The Billing API's `UsageReporter` was refined to use `SUM()` aggregation when checking plan limits. This ensures that while usage is *tracked* per-source, it is *enforced* against the organization's total global limit.


# Architectural Impact Report: Validation & Security

> **Range**: VALID-001 to SEC-004
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Validation, Brute Force Protection, and Security Abuse flows.

## 1. Database Schema Consolidation (`02-foundation-db.md`)

To simplify security monitoring and unify retention policies across different attack vectors, the schema was consolidated.

| Change | Purpose |
| :--- | :--- |
| **Replaced `login_attempts` with `security_events`** | Deprecated the specific `login_attempts` table in favor of a unified `security_events` table. This structure now tracks both login failures (`SEC-001`) and invalid API key attempts (`SEC-002`) in a single location with optimized indexes for IP-based blocking. |
| **Added `SecurityRepository`** | Introduced a unified interface for logging security events and querying recent failure counts by IP or Identifier. |

## 2. Middleware Architecture Evolution (`05a-api-core.md`)

Simulation of the API Key Enumeration flow (`SEC-002`) revealed a critical gap: standard rate limiting only applied *after* successful authentication.

*   **`IPSecurityMiddleware`**: Introduced a new middleware layer positioned **before** `AuthMiddleware`. This component checks the `SecurityService` for blocked IPs and rejects requests immediately, protecting the authentication subsystem from resource exhaustion during brute force attacks.
*   **Middleware Ordering**: Explicitly reordered the global stack to ensure `IPSecurityMiddleware` executes prior to any database-heavy authentication checks.

## 3. API Logic & Abuse Prevention (`05d-api-organization.md`)

To mitigate the risks of compromised user accounts rapidly generating credentials (`SEC-004`) and to facilitate rapid response to key leaks (`SEC-003`), the API logic was hardened.

*   **Proactive Rate Limit**: The `APIKeyHandler.Create` logic was updated to enforce a hard limit (e.g., 5 keys/hour) on key creation per user. This prevents a compromised account from flooding the system with new credentials.
*   **Prefix Filtering**: The `ListAPIKeys` endpoint was updated to support filtering by `key_prefix`. This allows administrators to quickly locate the ID of a compromised key knowing only the leaked plaintext prefix (e.g., `sk_live_abc...`), enabling faster revocation.

## 4. Infrastructure & Maintenance (`04-sam-template.md`, `09-scheduled-jobs.md`)

To ensure long-term stability and automated detection of anomalies, the operational architecture was expanded.

*   **Automated Alerting**: Added a `SecurityAbuseMetric` filter and `SuspiciousKeyCreationAlarm` to the CloudWatch configuration. This provides alerting on patterns like "rapid key creation" without requiring changes to application code.
*   **Security Event Retention**: Added `TaskCleanupSecurityEvents` to the Archiver schedule. This ensures high-volume security logs are pruned after 7 days, balancing forensic needs with storage costs.


# Architectural Impact Report: Security & Test Mode

> **Range**: SEC-005 to TEST-003
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Security Headers, Webhook Signing, and Test Mode Isolation flows.

## 1. Middleware Architecture Evolution (`05a-api-core.md`)

To mitigate potential risks associated with storing unsanitized user input (Flow `SEC-005`), the API middleware stack was enhanced to enforce browser-side security controls.

| Change | Purpose |
| :--- | :--- |
| **`SecurityHeadersMiddleware`** | Introduced a dedicated middleware function to inject security headers (`X-Content-Type-Options: nosniff`, `X-XSS-Protection`, `X-Frame-Options`). This prevents browsers from interpreting unsanitized JSON string payloads as executable content, acting as a defense-in-depth measure against Stored XSS. |

## 2. Security Logic Refinements (`08a-notification-core.md`)

Simulation of the Webhook Secret Rotation flow (`SEC-006`) identified ambiguity regarding the validity window of rotated secrets.

*   **Strict Expiry Enforcement**: The `SignatureManager.SignPayload` interface contract was updated to mandate checking `previous_secret_expires_at` against the current timestamp. Implementations must explicitly omit the `v1_old` signature header if the grace period has expired, preventing the indefinite validity of compromised rotated keys.

## 3. Data Isolation Strategy (`05b-api-watchpoints.md`)

To support the dual requirements of strict API data isolation and flexible Dashboard testing (`TEST-003`), the listing logic was branched based on Actor type.

*   **Actor-Based Filtering**: The `List` handler logic was refined to enforce distinct behaviors:
    *   **API Keys**: The handler **MUST** overwrite the `TestMode` filter with the immutable `Actor.IsTestMode` flag derived from the key prefix. API clients cannot cross the boundary between Test and Live data.
    *   **Dashboard Users**: The handler **MAY** accept a `test_mode` query parameter (defaulting to `false`) to populate the filter, allowing human operators to toggle between views within the UI.


# Architectural Impact Report: Testing & Bulk Operations

> **Range**: TEST-004 to BULK-004
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Test Mode Isolation and Bulk Operation flows.

## 1. Repository Interface Evolution (`02-foundation-db.md`)

To enforce strict data isolation between Live and Test environments during high-impact batch operations, the data access layer was refined.

| Change | Purpose |
| :--- | :--- |
| **`testMode` Parameter** | Added to `UpdateStatusBatch` and `DeleteBatch` method signatures. The SQL logic was updated to strictly filter by `AND test_mode = $testMode`, ensuring that a Test API key cannot inadvertently modify Live resources (and vice versa) even if valid IDs are provided. |
| **Explicit Archive Status** | The `DeleteBatch` logic was hardened to set `status='archived'` in addition to `deleted_at=NOW()`. This ensures immediate exclusion from the Batcher's active index (`status='active'`), preventing race conditions where soft-deleted items might still be evaluated once more.

## 2. API Logic Hardening (`05b-api-watchpoints.md`)

Simulation of the Bulk Import (`BULK-001`) and Pause (`BULK-002`) flows revealed performance bottlenecks and state consistency gaps.

*   **Parallel Validation**: The `BulkCreate` handler logic was mandated to use an error group (e.g., `errgroup`) for parallel execution of `Validator.ValidateStruct`. This prevents serial DNS resolution latencies (for webhook validation) from causing API Gateway timeouts during large batch processing.
*   **Deferred Cleanup**: The `Pause` and `BulkPause` handlers were updated to explicitly call `DeliveryManager.CancelDeferred`. This ensures that when a user pauses a WatchPoint to stop alerts, any notifications currently "parked" due to Quiet Hours are immediately cancelled, preventing them from flushing unexpectedly when Quiet Hours end.

## 3. Observability & Audit Refinements (`01-foundation-types.md`, `05b-api-watchpoints.md`)

To prevent audit log flooding during bulk operations, the logging strategy was optimized.

*   **Aggregated Audit Events**: Defined specific bulk event types (e.g., `watchpoint.bulk_paused`, `watchpoint.bulk_imported`). Instead of emitting 100 individual events for a batch operation, the system now emits a single event containing the `count` and the `filter` or `ids` list in the metadata, significantly reducing noise and storage costs.

## 4. Worker Logic & Propagation (`07-eval-worker.md`)

Simulation of Test Mode notification handling (`TEST-002`) highlighted a critical propagation requirement.

*   **Test Mode Propagation**: The `Evaluator` logic was explicitly updated to map the `WatchPoint.TestMode` boolean to the `NotificationEvent.TestMode` field when constructing the SQS payload. This ensures the downstream Notification Worker can reliably identify and suppress delivery for test alerts without needing to re-query the database.


# Architectural Impact Report: Bulk Operations & Database Resilience

> **Range**: BULK-005 to FAIL-003
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Bulk Tag Updates, Bulk Cloning, and Database Failure Recovery scenarios.

## 1. Database Schema & Repository Extensions (`02-foundation-db.md`)

To support high-performance batch operations and connection resilience, the data access layer was significantly enhanced.

| Change | Purpose |
| :--- | :--- |
| **`UpdateTagsBatch`** | Added to `WatchPointRepository`. Implements atomic array manipulation using Postgres operators (`||` and `-`) within a single `UPDATE` statement. This eliminates the race conditions inherent in a fetch-modify-save loop for bulk tag edits. |
| **`GetBatch` (Vectorized)** | Added to `WatchPointRepository`. Replaces N+1 lookups with a single `WHERE id = ANY($ids)` query, essential for preventing API Gateway timeouts during bulk clone operations. |
| **Connection Tuning** | Added `AcquireTimeout` and `HealthCheckPeriod` to the `DBConfig` struct. These parameters explicitly control `pgxpool` behavior to ensure fast failure during exhaustion (`FAIL-003`) and rapid recovery during DNS failover (`FAIL-001`). |

## 2. Type System Refinements (`01-foundation-types.md`)

To resolve circular dependencies between the API handler and the Repository layer, shared structures were hoisted.

*   **Hoisted `BulkFilter`**: Moved the `BulkFilter` struct (containing Status and Tags filtering logic) from the API layer to Foundation Types. This allows the Repository interface to reference it directly without import cycles.

## 3. Configuration & Infrastructure (`03-config.md`)

To operationalize the database resilience strategies, the configuration schema was expanded.

*   **New Database Parameters**: Added `DB_ACQUIRE_TIMEOUT` (default 2s) and `DB_HEALTH_CHECK_PERIOD` (default 1m) to the `DatabaseConfig` struct. This exposes the tuning knobs required to handle traffic spikes and infrastructure maintenance events without redeploying code.

## 4. API Logic Optimization (`05b-api-watchpoints.md`)

Simulation of the Bulk Clone (`BULK-006`) flow revealed a logic gap regarding WatchPoint modes.

*   **Mode-Aware Cloning**: The `BulkClone` handler logic was refined to explicitly ignore the `TimeShift` parameter for **Monitor Mode** WatchPoints. Since monitor configs use rolling windows relative to "now", shifting the absolute timestamps is semantically invalid and is now programmatically suppressed.
*   **Atomic Tag Updates**: The `BulkTagUpdate` handler was updated to call the new `Repo.UpdateTagsBatch` method, prioritizing data integrity over the previous iterative approach.


# Architectural Impact Report: Failure & Recovery Strategies

> **Range**: FAIL-004 to FAIL-008
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of upstream outages, hanging jobs, and data corruption scenarios.

## 1. Configuration & Redundancy Strategy (`03-config.md`)

To ensure the system remains resilient when primary data sources (like NOAA's GFS bucket) fail, the configuration schema was expanded to support failover.

| Change | Purpose |
| :--- | :--- |
| **`UpstreamMirrors`** | Added a string list configuration field to define ordered failover targets (e.g., `["noaa-gfs-bdp-pds", "aws-noaa-gfs"]`). |
| **Model-Specific Timeouts** | Added `TimeoutMediumRange` and `TimeoutNowcast` fields. This replaces hardcoded thresholds with tunable parameters, acknowledging the vast difference in expected execution time between models (hours vs minutes). |

## 2. Job Lifecycle & Cost Control (`09-scheduled-jobs.md`)

Simulation of the "Hanging Job" scenario (`FAIL-006`) identified a financial risk where stalled GPU containers could bill indefinitely. The reconciliation logic was hardened to prevent this.

*   **RunPod Client Extension**: Added `CancelJob(ctx, externalID)` to the client interface.
*   **Active Termination**: The `ForecastReconciler` logic was updated to explicitly call `CancelJob` when a run exceeds its configured timeout, prioritizing cost control before marking the record as failed in the database.

## 3. Data Integrity & Observability (`07-eval-worker.md`, `04-sam-template.md`)

Simulation of Zarr file corruption (`FAIL-008`) revealed that standard retry logic would cause "poison pill" loops in the SQS queue.

*   **"Skip and Alert" Protocol**: The `ForecastReader` logic was refined to treat checksum/format errors as **terminal**. Workers are now mandated to emit a specific metric and ACK the message (removing it from the queue) rather than returning an error to the Lambda runtime.
*   **Infrastructure Alerts**: Added `CorruptForecastMetricFilter` and `CorruptForecastAlarm` to the SAM template. This ensures that while the system self-heals by skipping bad data, Operations is immediately paged to perform manual repairs or backfills.

## 4. Synchronization Logic (`09-scheduled-jobs.md`)

To prevent inference failures caused by partial input data (e.g., Satellite available but Radar missing), the polling logic was made stricter.

*   **Multi-Source Alignment**: The `DataPoller` logic for Nowcast was updated to manage two distinct `UpstreamSource` instances (GOES and MRMS). It now calculates the intersection of available timestamps (within a 5-minute tolerance) and triggers inference *only* for fully aligned data sets.


# Architectural Impact Report: Failure & Recovery Strategies (Advanced)

> **Range**: FAIL-009 to FAIL-013
> **Date**: 2026-02-03
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Kill Switches, Generic Crashes, and Disaster Recovery flows.

## 1. Job Scheduling & Recovery Logic (`09-scheduled-jobs.md`)

To prevent Lambda timeouts during high-volume data backfills (e.g., re-ingesting 48 hours of forecast data in a new region), the polling architecture was refined.

| Change | Purpose |
| :--- | :--- |
| **`DataPollerInput` Struct** | Defined the strict JSON contract for manual Lambda invocation. Includes `ForceRetry`, `BackfillHours`, and crucially, `Limit`. |
| **Rate-Limited Backfill** | Updated the Data Poller service logic to respect the `Limit` parameter. This forces the poller to stop triggering new RunPod jobs once the limit is reached, allowing operators to "chunk" the recovery process (e.g., 10 jobs per run) and stay within the 1-minute execution timeout. |

## 2. Operational Procedures (`12-operations.md`)

Simulation of the Region Failure (`FAIL-013`) flow identified a critical data consistency gap where the restored database pointed to S3 objects that did not exist in the new region.

*   **DR State Reset**: The Disaster Recovery runbook was updated to include a mandatory SQL step: `UPDATE forecast_runs SET status='failed' ...`. This explicitly invalidates records pointing to the dead region's bucket, forcing the Data Poller to treat them as missing and re-trigger ingestion.
*   **Documentation Alignment**: Fixed cross-references for manual invocation payloads to point to the new definition in the Scheduled Jobs document.

## 3. Procedural Acceptance

For certain failure modes, the architecture explicitly defers to procedural or infrastructure-level handling rather than code complexity:

*   **Kill Switch Latency (`FAIL-009`)**: Accepted the ~60s latency of SSM Parameter updates + Lambda configuration flushing as a valid trade-off for simplicity, rejecting the need for a hot-path database check.
*   **Generic Crash Reconciliation (`FAIL-011`)**: Accepted that `attempt_count` in the database may drift during hard crashes (OOM). Reconciliation is delegated to the Dead Letter Queue processing procedure (`SCHED-005`) rather than introducing a complex supervisor component.


# Architectural Impact Report: Concurrency & Implementation Edge Cases

> **Range**: CONC-004 to IMPL-002
> **Date**: 2026-02-04
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Concurrency Race Conditions, Stale Writes, and Partial Failure scenarios.

## 1. Evaluation Integrity & Idempotency (`07-eval-worker.md`)

To prevent data corruption from out-of-order message delivery and ensure safe retries for hot tiles, the evaluation logic was hardened.

*   **Stale Write Protection**: The `Repository.commit_batch` method was updated to enforce a **Conditional Update** pattern. State updates now include the clause `AND (last_forecast_run IS NULL OR last_forecast_run <= $msg_ts)`. The worker uses the `RETURNING watchpoint_id` clause to identify successful updates and strictly filters notification inserts to match, ensuring that stale messages (delayed in SQS) cannot trigger alerts or overwrite fresh state.
*   **Granular Idempotency**: Defined a requirement for **Individual Item Checks** within the evaluation loop. Workers must verify `last_forecast_run` for *each* WatchPoint in a batch (rather than once for the whole message) to support partial retries of large pages without duplicating side effects.

## 2. Notification Consistency (`07-eval-worker.md`)

Simulation of configuration updates during active delivery (`CONC-005`) highlighted the need for immutable delivery contexts.

*   **Channel Configuration Snapshotting**: Mandated that the Evaluation Worker must serialize and store a **snapshot** of the channel configuration into `notification_deliveries.channel_config` at the moment of trigger. Notification Workers read this snapshot rather than the live `watchpoints` table, ensuring alerts go to the destination defined *at trigger time*, preventing race conditions if a user deletes a channel mid-process.

## 3. State Management & User Experience (`07-eval-worker.md`)

To prevent "Alert Shock" when the system resolves configuration version mismatches (`IMPL-001`), the reset logic was refined.

*   **Deduplication History Preservation**: The `ConfigPolicy` resolution logic was updated to explicitly **preserve** the `seen_threats` JSONB array when resetting state due to a version or tile mismatch. Only the trigger state and escalation levels are reset. This ensures that temporal deduplication remains active, preventing the system from re-alerting users about ongoing weather threats they have already been notified about.

## 4. API Consistency Strategy (`05b-api-watchpoints.md`)

Simulation of the Limit Race Condition (`CONC-004`) led to a formal architectural decision regarding consistency guarantees.

*   **Soft Consistency Acceptance**: Explicitly documented that the API accepts race conditions during Limit Enforcement (e.g., creating resources slightly over the plan limit during high concurrency). Instead of implementing complex distributed locks, the architecture relies on the daily `BILL-011` (Downgrade Enforcement) scheduled job to detect and pause excess resources, favoring API availability and latency over strict instantaneous consistency.


# Architectural Impact Report: Implementation Edge Cases & Integration

> **Range**: IMPL-003 to INT-004
> **Date**: 2026-02-04
> **Status**: Applied via Master Refactoring Prompt

This report documents the structural and logical changes applied to the WatchPoint architecture resulting from the low-level simulation of Idempotency, Traceability, and End-to-End Integration flows.

## 1. Traceability & Observability (`07-eval-worker.md`)

To ensure complete visibility into the end-to-end latency of a forecast (`INT-002`), the distributed tracing context was repaired.

*   **`NotificationEvent` Expansion**: Added the `trace_id` field to the Python Pydantic model. This bridges the gap between the Evaluation Engine and the Notification Queue.
*   **Propagation Logic**: Mandated that the `Evaluator` interface must explicitly copy the `trace_id` from the incoming `EvalMessage` to the outgoing `NotificationEvent`, ensuring X-Ray traces remain unbroken across the SQS boundary.

## 2. Data Authority & Integrity (`06-batcher.md`)

Simulation of the Idempotency flow (`IMPL-003`) identified a risk where un-tracked files in S3 (e.g., manual uploads) could trigger unmonitored evaluation cycles.

*   **Phantom Run Rejection**: Implemented a strict validation step in the Batcher's `ProcessRun` logic. The Batcher must now query `forecast_runs` for the specific model/timestamp tuple. If no record is found, it must log a critical error and **discard** the event rather than attempting to process it. This enforces the Data Poller as the sole authority for initiating forecast lifecycles.

## 3. API Behavior & User Experience (`05b-api-watchpoints.md`)

Simulation of the New User Journey (`INT-001`) required explicit clarification regarding the "First Alert" experience to prevent user confusion.

*   **"Quiet Create" Policy**: The Create Endpoint logic was updated to explicitly document that **Monitor Mode** WatchPoints do *not* trigger an immediate evaluation upon creation. Unlike Event Mode (which provides instant snapshots), Monitor Mode relies on the next scheduled Batcher cycle (up to 15m latency) to establish a baseline. This decision prioritizes system stability over instant gratification for long-running monitors.