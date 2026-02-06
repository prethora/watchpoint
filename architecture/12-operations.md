# 12 - Operations & Deployment

> **Purpose**: Defines the operational lifecycle of the WatchPoint platform, including local development environments, CI/CD deployment strategies, observability configurations, incident response runbooks, and disaster recovery procedures.
> **Package**: Root / Operations
> **Dependencies**: `01-foundation-types.md`, `03-config.md`, `04-sam-template.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Local Development Environment](#2-local-development-environment)
3. [Deployment Strategy](#3-deployment-strategy)
4. [Monitoring & Observability](#4-monitoring--observability)
5. [Incident Response Runbooks](#5-incident-response-runbooks)
6. [Disaster Recovery (DR)](#6-disaster-recovery-dr)
7. [Maintenance & Compliance](#7-maintenance--compliance)
8. [Capacity Planning](#8-capacity-planning)
9. [Required Migrations & Updates](#9-required-migrations--updates)
10. [Flow Coverage](#10-flow-coverage)

---

## 1. Overview

The WatchPoint platform embraces a "Serverless-First" operational model. Operations focuses on **Observability** (knowing when it breaks) and **Resilience** (recovering automatically), rather than server management.

### Operational Principles
1.  **Immutable Infrastructure**: Configuration changes require redeployment or explicit environment variable updates; no hot-patching.
2.  **Fail Loudly**: Silent failures (e.g., missing forecast data) are treated as critical incidents via "Dead Man's Switch" alarms.
3.  **Least Privilege**: Developers and Support staff have restricted access to production data via specific database roles.

---

## 2. Local Development Environment

To simulate the AWS ecosystem without cost or latency, local development relies on `docker-compose` and **LocalStack**.

### 2.1 Service Architecture

The local stack mirrors production infrastructure:

| Service | Image | Production Equivalent | Port |
| :--- | :--- | :--- | :--- |
| `postgres` | `postgres:15` | Supabase (RDS) | 5432 |
| `localstack` | `localstack/localstack` | AWS (S3, SQS, EventBridge, SSM) | 4566 |
| `minio-init` | `minio/mc` | (Script) Bucket Seeder | N/A |

### 2.2 Docker Compose Configuration

Create `docker-compose.yml` in the project root:

```yaml
version: '3.8'
services:
  postgres:
    image: postgres:15
    ports: ["5432:5432"]
    environment:
      POSTGRES_DB: watchpoint
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: localdev
    volumes:
      - ./migrations:/docker-entrypoint-initdb.d

  localstack:
    image: localstack/localstack:latest
    ports: ["4566:4566"]
    environment:
      SERVICES: sqs,s3,ssm,cloudwatch,events
      DEBUG: 0
    volumes:
      - ./localstack_data:/var/lib/localstack

  # Ephemeral container to seed LocalStack buckets/queues
  setup:
    image: amazon/aws-cli
    depends_on: [localstack]
    environment:
      AWS_ACCESS_KEY_ID: test
      AWS_SECRET_ACCESS_KEY: test
      AWS_DEFAULT_REGION: us-east-1
      AWS_ENDPOINT_URL: http://localstack:4566
    entrypoint: /bin/sh -c
    command: >
      "aws s3 mb s3://watchpoint-forecasts-local &&
       aws sqs create-queue --queue-name eval-queue-urgent &&
       aws sqs create-queue --queue-name eval-queue-standard &&
       aws ssm put-parameter --name /local/watchpoint/database/url --value postgres://postgres:localdev@postgres:5432/watchpoint --type SecureString"
```

### 2.3 Setup Scripts

**`scripts/local-setup.sh`**:
1.  Starts containers: `docker-compose up -d`.
2.  Waits for health checks.
3.  Runs DB migrations using `golang-migrate` (assumes CLI is installed on host, or can be run via temporary container).
    ```bash
    migrate -path ./migrations -database "postgres://postgres:localdev@localhost:5432/watchpoint?sslmode=disable" up
    ```
4.  Seeds test data (1 dummy ForecastRun, 10 WatchPoints).

---

## 3. Deployment Strategy

Deployment is managed via AWS SAM and GitHub Actions.

### 3.1 Environments

| Environment | Branch | AWS Account | Secrets Path | Validation |
| :--- | :--- | :--- | :--- | :--- |
| **Dev** | `dev` | Dev | `/dev/watchpoint/*` | Automated Tests |
| **Staging** | `staging` | Prod | `/staging/watchpoint/*` | Integration Tests |
| **Prod** | `main` | Prod | `/prod/watchpoint/*` | Manual Approval + Canary |

### 3.2 Secret Injection (SSM)

CI/CD pipelines never handle plaintext secrets. They inject **SSM Reference Paths** into the SAM template.

*   **Config Loading**: The Go application (`03-config.md`) resolves these paths at runtime (Cold Start).
*   **Rotation**:
    1.  Operator updates SSM Parameter value (AWS Console/CLI).
    2.  Operator triggers `aws lambda update-function-configuration` to force a cold start.
    3.  New instances pick up the new secret.

### 3.3 Emergency Rollback

If a deployment succeeds technically but introduces a logical bug:

**Procedure**:
1.  **Identify**: Find the previous stable CloudFormation stack ID.
2.  **Re-deploy**: Use SAM to deploy the *previous* build artifact.
    ```bash
    sam deploy --template-file .aws-sam/build/template.yaml.prev \
               --stack-name watchpoint-prod \
               --capabilities CAPABILITY_IAM
    ```
    *Note: Standard `rollback` only works during a failed update. Post-success, we must re-deploy the old state.*

---

## 4. Monitoring & Observability

### 4.1 Dead Man's Switch (Forecast Pipeline)

We alert on the *absence* of success signals.

**Alarm Configuration**:
*   **Metric**: `ForecastReady` (Namespace: `WatchPoint`).
*   **Dimensions**: `ForecastType=medium_range` or `ForecastType=nowcast`.
*   **Logic**: `TreatMissingData: Breaching`.

| Alarm Name | Period | Threshold | Severity |
| :--- | :--- | :--- | :--- |
| `MediumRangeStale` | 8 Hours | Sum < 1 | **Critical** |
| `NowcastStale` | 30 Mins | Sum < 1 | **Critical** |

### 4.2 External Probes (The "Watcher")

To detect total AWS outages where CloudWatch fails, use an external provider (e.g., UptimeRobot) to probe the API.

*   **Target**: `GET https://api.watchpoint.io/v1/health`
*   **Criteria**: Status `200`, Latency `< 2s`.
*   **Alert**: PagerDuty (Non-AWS channel).

### 4.3 Cost Anomaly Detection

*   **AWS Budgets**: Alert at 80% of monthly budget ($400).
*   **Cost Anomaly**: Alert on spikes > $20/day (e.g., Lambda recursion loops, GPU cost runaway).

---

## 5. Incident Response Runbooks

### SOP-001: Database Unavailable

**Trigger**: High rate of HTTP 500s + Health Check failure.

1.  **Check Status**: Verify AWS RDS/Supabase status page.
2.  **Verify Pooler**: Check Supabase PgBouncer metrics (ClientWaiting).
3.  **Failover**:
    *   If AWS AZ failure: Wait for Multi-AZ auto-failover (60-120s).
    *   If stuck: Initiate "Force Failover" via Console.
4.  **Verify**: Run `curl https://api.watchpoint.io/v1/health`.

### SOP-002: Upstream Data Missing (GFS)

**Trigger**: `MediumRangeStale` alarm fires.

1.  **Check Upstream**: Verify [NOAA NCEP Status](https://www.nco.ncep.noaa.gov/pmb/nwprod/prodstat/).
2.  **Check RunPod**: Verify GPU worker logs for crashes.
3.  **Action**:
    *   If NOAA outage: System will automatically serve stale data (Flow `FCST-005`). Verify API responses contain `warning: "Forecast data is stale"`.
    *   If RunPod stuck: Manually invoke `data-poller` Lambda with payload `{"force_retry": true}` (See Section 9.4).

### SOP-003: Customer "Missed Alert" Debugging

**Trigger**: Support ticket claiming missing notification.

**Diagnostic Query**:
Run this SQL via the Support Admin role to trace the event lifecycle.

```sql
-- Input: :watchpoint_id
SELECT 
    wp.status,
    wes.last_evaluated_at,
    wes.previous_trigger_state,
    n.created_at AS notification_time,
    n.event_type,
    nd.status AS delivery_status,
    nd.failure_reason
FROM watchpoints wp
LEFT JOIN watchpoint_evaluation_state wes ON wp.id = wes.watchpoint_id
LEFT JOIN notifications n ON wp.id = n.watchpoint_id
LEFT JOIN notification_deliveries nd ON n.id = nd.notification_id
WHERE wp.id = :watchpoint_id
ORDER BY n.created_at DESC LIMIT 5;
```

---

## 6. Disaster Recovery (DR)

**Strategy**: "Pilot Light" (Passive DR).
**RTO**: 4 Hours.
**RPO**: 24 Hours (Database Snapshot).

### Failover Procedure (Region Failure)
1.  **Infrastructure**: Deploy SAM stack to DR Region (e.g., `us-west-2`).
2.  **Database**: Restore latest Supabase Snapshot to DR region.
3.  **Forecast State Reset**: Execute the following SQL to invalidate records pointing to the dead region's S3 bucket:
    ```sql
    UPDATE forecast_runs SET status='failed', storage_path='' WHERE run_timestamp > NOW() - INTERVAL '48 hours';
    ```
    This forces the DataPoller to re-ingest data.
4.  **Forecast Data**:
    *   Do **not** replicate S3 buckets (cost prohibitive).
    *   **Action**: Manually trigger `data-poller` in DR region with payload `{"backfill_hours": 48, "limit": 10}`. Repeat until caught up.
5.  **Switchover**: Update Route53 DNS to point `api.watchpoint.io` to DR API Gateway.

---

## 7. Maintenance & Compliance

### 7.1 Audit Log Lifecycle (`MAINT-004`)

To prevent database bloat, Audit Logs are moved to Cold Storage.

*   **Schedule**: Monthly (via `ArchiverFunction`).
*   **Logic**:
    1.  Select logs > 2 years old.
    2.  Export to `s3://watchpoint-archives/audit/`.
    3.  `DELETE` from DB.

### 7.2 GDPR Hard Delete

**Trigger**: Verified user request.
**Action**: Execute `scripts/ops/hard_delete_org.sql` (Manual Admin Task).

**Script Logic (`hard_delete_org.sql`)**:
Since the system uses foreign keys with varying cascade rules, the deletion order is critical to avoid constraint violations.

```sql
-- Input: :org_id
BEGIN;

-- 1. Archive Audit Logs (Optional: Copy to cold storage before delete)
-- (Skipped in this script, audit logs are typically retained for compliance)

-- 2. Delete Notifications & Deliveries (Cascade from Notifications)
DELETE FROM notifications WHERE organization_id = :org_id;

-- 3. Delete WatchPoints (Cascades to evaluation_state)
DELETE FROM watchpoints WHERE organization_id = :org_id;

-- 4. Delete API Keys
DELETE FROM api_keys WHERE organization_id = :org_id;

-- 5. Delete Sessions
DELETE FROM sessions WHERE organization_id = :org_id;

-- 6. Delete Users
DELETE FROM users WHERE organization_id = :org_id;

-- 7. Delete Rate Limits (if separated)
DELETE FROM rate_limits WHERE organization_id = :org_id;

-- 8. Delete Organization
DELETE FROM organizations WHERE id = :org_id;

COMMIT;
```

### 7.3 Emergency Feature Flags

Configured via SSM Parameters loaded by `03-config.md`.

*   `/prod/watchpoint/features/enable_nowcast`: Set `false` to stop expensive GPU runs.
*   `/prod/watchpoint/features/enable_email`: Set `false` to stop SendGrid output during incidents.

---

## 8. Capacity Planning

**Hard Limits & Thresholds**

| Resource | Limit (v1) | Bottleneck | Scaling Action |
| :--- | :--- | :--- | :--- |
| **Postgres Connections** | ~500 | `max_connections` | Tune PgBouncer / Upgrade DB |
| **Lambda Concurrency** | 1,000 | Account Quota | Request Quota Increase |
| **SQS Payload** | 256 KB | SQS Standard | Use S3 Pointer pattern |
| **API Payload** | 6 MB | API Gateway | Reject large batches |
| **Zarr Chunk** | ~50 MB | Lambda RAM | Resize Tile Grid |

---

## 9. Consolidated References

*The following configurations have been consolidated in the foundation documents.*

### 9.1 Database Admin Roles
*Defined in `02-foundation-db.md` (Section 6.6).*

The `watchpoint_support_ro` role provides read-only access for Support staff to debug customer issues (SOP-003).

### 9.2 Feature Kill Switches
*Defined in `03-config.md` (FeatureConfig struct).*

Emergency kill switches for `FEATURE_ENABLE_NOWCAST` and `FEATURE_ENABLE_EMAIL` are loaded via SSM Parameters.

### 9.3 Archive Bucket
The `ArchiveBucket` resource should be added to `04-sam-template.md` for Audit Log cold storage and DR backups.

```yaml
ArchiveBucket:
  Type: AWS::S3::Bucket
  Properties:
    LifecycleConfiguration:
      Rules:
        - Id: GlacierTransition
          Status: Enabled
          Transitions:
            - StorageClass: GLACIER
              TransitionInDays: 1
```

### 9.4 DataPoller Manual Invocation
*`DataPollerInput` is defined in `09-scheduled-jobs.md` (Section 5).*

Supports manual intervention for SOP-002 (GFS Data Missing) and DR Failover procedures via `force_retry`, `backfill_hours`, and `limit` parameters. The `limit` parameter controls the maximum number of RunPod jobs triggered per execution, preventing Lambda timeouts during large backfills.

---

### 10. Flow Coverage

| Flow ID | Description | Implementation Section |
|---|---|---|
| `OBS-001` | Dead Man's Switch (Medium Range) | §4.1 Monitoring & Observability |
| `OBS-002` | Dead Man's Switch (Nowcast) | §4.1 Monitoring & Observability |
| `OBS-003` | Queue Depth Alarm | §4.3 (Implicit in Cost/Anomaly), §5 SOP-002 |
| `OBS-004` | Dead Letter Queue Alarm | §2.3 (Local DLQ), §5 SOP-001 |
| `FAIL-001` | Database Failover | §5 Incident Response (SOP-001) |
| `FAIL-004` | GFS Data Unavailable | §5 Incident Response (SOP-002) |
| `FAIL-013` | Region Failure (DR) | §6 Disaster Recovery |
| `MAINT-004` | Audit Log Retention | §7.1 Maintenance & Compliance |
| `MAINT-005` | Soft-Deleted Org Cleanup | §7.2 (GDPR Hard Delete extension) |
| `SEC-003` | API Key Compromise | §7.2 (Via Manual Hard Delete) |