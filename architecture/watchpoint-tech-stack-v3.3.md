# WatchPoint Platform — Technology Stack v3.3

> Implementation decisions for building WatchPoint on AWS with Go, Python, and RunPod.
> **Final version** — Ready for implementation.

**Companion to**: `watchpoint-platform-design.md` + `watchpoint-platform-design-addendum-v2.md`

---

## Changelog

| Version | Changes |
|---------|---------|
| v3.0 | Initial Zarr + Python eval design |
| v3.1 | Batcher optimization, provisioned concurrency, Stripe webhooks |
| v3.2 | Hot tile pagination, SSRF protection, atomicity guarantees |
| **v3.3** | Remove active_tiles (simplify), dead man's switch, webhook ordering, local dev |

---

## Table of Contents

1. [Overview](#overview)
2. [Architecture Summary](#architecture-summary)
3. [Compute Layer](#compute-layer)
4. [Data Layer](#data-layer)
5. [Batcher Architecture](#batcher-architecture)
6. [Forecast Pipeline](#forecast-pipeline)
7. [Evaluation Engine](#evaluation-engine)
8. [Notification System](#notification-system)
9. [Security](#security)
10. [Observability & Dead Man's Switch](#observability--dead-mans-switch)
11. [Billing Integration](#billing-integration)
12. [Local Development](#local-development)
13. [Project Structure](#project-structure)
14. [Cost Estimates](#cost-estimates)
15. [Risk Mitigations](#risk-mitigations)
16. [Bootstrap Checklist](#bootstrap-checklist)

---

## Overview

### Guiding Principles

1. **Minimize committed spend**: Lambda-only compute, pay-per-use services
2. **Use the right tool**: Go for API/workers, Python for data processing
3. **Efficient data formats**: Zarr with halo regions for single-GET reads
4. **Scale-aware queries**: O(tiles) via indexed GROUP BY, not O(watchpoints)
5. **Warm when it matters**: Provisioned concurrency for latency-critical paths
6. **Paginate hot tiles**: No single Lambda processes >500 WatchPoints
7. **Fail loudly**: Dead man's switch alerts on silent failures

### Key Simplification in v3.3

**Removed `active_tiles` table.** The trigger-based approach caused contention on bulk imports. Instead, the Batcher computes tile counts directly via an index-only GROUP BY scan (~10ms for 500K WatchPoints).

---

## Architecture Summary

```
┌─────────────────────────────────────────────────────────────────────────────┐
│                              AWS ACCOUNT                                    │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                   API Gateway + API Lambda (Go)                     │   │
│  │  • Webhook URL validation with SSRF protection                      │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                      │                                      │
│  ┌───────────────────────────────────┼───────────────────────────────────┐ │
│  │                    S3 (Zarr — Immutable Paths)                        │ │
│  │  • Each run writes to unique timestamped prefix                      │ │
│  │  • _SUCCESS marker written LAST                                      │ │
│  └───────────────────────────────────┬───────────────────────────────────┘ │
│                                      │ S3 Event                             │
│                                      ▼                                      │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                     Batcher Lambda (Go)                              │   │
│  │  • GROUP BY tile_id (index-only scan, ~10ms)                        │   │
│  │  • Paginates hot tiles (>500 WatchPoints)                           │   │
│  │  • Emits ForecastReady metric (dead man's switch)                   │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                    │                              │                         │
│                    ▼                              ▼                         │
│  ┌────────────────────────────┐    ┌────────────────────────────┐         │
│  │  eval-queue-urgent (SQS)   │    │  eval-queue-standard (SQS) │         │
│  │  Provisioned concurrency   │    │  Reserved concurrency      │         │
│  └─────────────┬──────────────┘    └─────────────┬──────────────┘         │
│                │                                  │                         │
│                ▼                                  ▼                         │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │              Eval Worker Lambda (PYTHON)                            │   │
│  │  • Queries WatchPoints with LIMIT/OFFSET                            │   │
│  │  • Max 500 WatchPoints per invocation                               │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                      │                                      │
│                                      ▼                                      │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │              Notification Workers (Go)                              │   │
│  │  • SSRF-safe HTTP client                                            │   │
│  │  • Includes event_sequence for ordering                             │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
│                                                                             │
│  ┌─────────────────────────────────────────────────────────────────────┐   │
│  │                    CloudWatch Alarms                                │   │
│  │  • Dead Man's Switch: Alert if no forecast in 6 hours              │   │
│  │  • Queue depth alarms                                              │   │
│  └─────────────────────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

## Compute Layer

### Lambda Functions

| Function | Language | Trigger | Memory | Timeout | Concurrency |
|----------|----------|---------|--------|---------|-------------|
| `api` | Go | API Gateway | 256 MB | 30s | Default |
| `batcher` | Go | S3 Event | 256 MB | 30s | Default |
| `eval-worker-urgent` | Python | SQS | 1024 MB | 5 min | **Provisioned: 5** |
| `eval-worker-standard` | Python | SQS | 1024 MB | 5 min | Reserved: 50 |
| `email-worker` | Go | SQS | 256 MB | 30s | Default |
| `webhook-worker` | Go | SQS | 256 MB | 30s | Default |
| `data-poller` | Go | EventBridge | 256 MB | 1 min | 1 |
| `archiver` | Go | EventBridge | 256 MB | 1 min | 1 |

---

## Data Layer

### Database Schema

#### Required Extensions

```sql
-- migrations/001_extensions.up.sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "cube";
CREATE EXTENSION IF NOT EXISTS "earthdistance";
```

#### WatchPoints Table (with tile_id index)

```sql
-- migrations/002_watchpoints.up.sql

CREATE TABLE watchpoints (
    id TEXT PRIMARY KEY DEFAULT 'wp_' || gen_random_uuid()::text,
    organization_id TEXT NOT NULL REFERENCES organizations(id),
    
    -- Location
    location_lat DOUBLE PRECISION NOT NULL,
    location_lon DOUBLE PRECISION NOT NULL,
    location_display_name TEXT,
    
    -- Computed tile (for efficient GROUP BY)
    tile_id TEXT GENERATED ALWAYS AS (
        CONCAT(
            FLOOR((90.0 - location_lat) / 22.5)::INT,
            '.',
            FLOOR(
                CASE 
                    WHEN location_lon >= 0 THEN location_lon 
                    ELSE 360.0 + location_lon 
                END / 45.0
            )::INT
        )
    ) STORED,
    
    -- Time window (null = Monitor Mode)
    time_window_start TIMESTAMPTZ,
    time_window_end TIMESTAMPTZ,
    
    -- Conditions
    conditions JSONB NOT NULL,
    condition_logic TEXT NOT NULL DEFAULT 'ALL',
    
    -- Channels
    channels JSONB NOT NULL,
    
    -- Config versioning (race condition protection)
    config_version INTEGER NOT NULL DEFAULT 1,
    
    -- Status
    status TEXT NOT NULL DEFAULT 'active',
    
    -- Timestamps
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- CRITICAL INDEX: Enables fast GROUP BY tile_id
-- This is an index-only scan for the Batcher query
CREATE INDEX idx_watchpoints_tile_active 
ON watchpoints(tile_id, status) 
WHERE status = 'active';

-- For fetching WatchPoints within a tile (Eval Worker)
CREATE INDEX idx_watchpoints_tile_id 
ON watchpoints(tile_id, id) 
WHERE status = 'active';
```

**Note:** No `active_tiles` table. Removed in v3.3 to avoid trigger contention on bulk imports.

#### Evaluation State Table

```sql
-- migrations/003_evaluation_state.up.sql

CREATE TABLE watchpoint_evaluation_state (
    watchpoint_id TEXT PRIMARY KEY REFERENCES watchpoints(id) ON DELETE CASCADE,
    
    -- Config version at last evaluation (race condition detection)
    config_version INTEGER NOT NULL,
    
    -- Trigger state
    previous_trigger_state BOOLEAN,
    trigger_state_changed_at TIMESTAMPTZ,
    trigger_value DOUBLE PRECISION,  -- Value at last state change (for hysteresis)
    
    -- Evaluation tracking
    last_evaluated_at TIMESTAMPTZ,
    last_forecast_run TIMESTAMPTZ,
    
    -- Monitor mode: seen threat hashes (deduplication)
    seen_threat_hashes TEXT[] DEFAULT '{}',
    
    -- Event sequence counter (for webhook ordering)
    event_sequence BIGINT NOT NULL DEFAULT 0
);
```

#### Config Version Trigger

```sql
-- migrations/004_config_version.up.sql

CREATE OR REPLACE FUNCTION increment_config_version()
RETURNS TRIGGER AS $$
BEGIN
    IF OLD.conditions IS DISTINCT FROM NEW.conditions
       OR OLD.condition_logic IS DISTINCT FROM NEW.condition_logic
       OR OLD.time_window_start IS DISTINCT FROM NEW.time_window_start
       OR OLD.time_window_end IS DISTINCT FROM NEW.time_window_end
    THEN
        NEW.config_version = OLD.config_version + 1;
        NEW.updated_at = NOW();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER watchpoint_config_version_trigger
BEFORE UPDATE ON watchpoints
FOR EACH ROW EXECUTE FUNCTION increment_config_version();
```

---

## Batcher Architecture

### Simplified Design (v3.3)

**Key change:** No `active_tiles` table. The Batcher computes tile counts directly.

```go
// cmd/batcher/main.go

const MAX_WATCHPOINTS_PER_PAGE = 500

func handler(ctx context.Context, s3Event events.S3Event) error {
    forecastType, runTimestamp := parseS3Key(s3Event)
    queueURL := getQueueURL(forecastType)
    
    // Query tiles with counts — INDEX-ONLY SCAN
    // With 500K WatchPoints across 64 tiles, this returns 64 rows in ~10ms
    rows, err := db.Query(ctx, `
        SELECT tile_id, COUNT(*) as wp_count
        FROM watchpoints
        WHERE status = 'active'
        GROUP BY tile_id
    `)
    if err != nil {
        return fmt.Errorf("failed to query tiles: %w", err)
    }
    defer rows.Close()
    
    var messages []SQSMessage
    
    for rows.Next() {
        var tileID string
        var count int
        rows.Scan(&tileID, &count)
        
        if count == 0 {
            continue
        }
        
        if count <= MAX_WATCHPOINTS_PER_PAGE {
            // Small tile: single message
            messages = append(messages, SQSMessage{
                TileID:       tileID,
                ForecastType: forecastType,
                RunTimestamp: runTimestamp,
                Page:         0,
                PageSize:     0, // 0 = no pagination
            })
        } else {
            // Hot tile: paginate
            numPages := (count + MAX_WATCHPOINTS_PER_PAGE - 1) / MAX_WATCHPOINTS_PER_PAGE
            log.Info("Hot tile detected", "tile_id", tileID, "count", count, "pages", numPages)
            
            for page := 0; page < numPages; page++ {
                messages = append(messages, SQSMessage{
                    TileID:       tileID,
                    ForecastType: forecastType,
                    RunTimestamp: runTimestamp,
                    Page:         page,
                    PageSize:     MAX_WATCHPOINTS_PER_PAGE,
                })
            }
        }
    }
    
    // Batch send to SQS
    for _, msg := range messages {
        _, err := sqsClient.SendMessage(ctx, &sqs.SendMessageInput{
            QueueUrl:    &queueURL,
            MessageBody: aws.String(toJSON(msg)),
        })
        if err != nil {
            log.Error("Failed to enqueue", "tile_id", msg.TileID, "error", err)
        }
    }
    
    // Emit metric for dead man's switch
    cloudwatchClient.PutMetricData(ctx, &cloudwatch.PutMetricDataInput{
        Namespace: aws.String("WatchPoint"),
        MetricData: []types.MetricDatum{{
            MetricName: aws.String("ForecastReady"),
            Value:      aws.Float64(1),
            Unit:       types.StandardUnitCount,
            Dimensions: []types.Dimension{{
                Name:  aws.String("ForecastType"),
                Value: aws.String(forecastType),
            }},
        }},
    })
    
    log.Info("Batcher complete",
        "forecast_type", forecastType,
        "tiles", len(messages),
        "run_timestamp", runTimestamp)
    
    return nil
}
```

### Query Performance

| WatchPoints | Tiles | Query Time | Notes |
|-------------|-------|------------|-------|
| 10,000 | ~20 | ~2ms | Index-only scan |
| 100,000 | ~40 | ~5ms | Index-only scan |
| 500,000 | ~64 | ~10ms | Index-only scan |
| 1,000,000 | ~64 | ~15ms | Index-only scan |

The query is O(tiles), not O(watchpoints), because the index groups by tile_id.

---

## Notification System

### Webhook Payload with Ordering

Standard SQS doesn't guarantee order. Include fields for client-side ordering:

```json
{
  "watchpoint_id": "wp_abc123",
  "organization_id": "org_xyz",
  "event_type": "condition_triggered",
  
  "ordering": {
    "forecast_timestamp": "2026-01-31T12:00:00Z",
    "event_sequence": 47,
    "evaluation_timestamp": "2026-01-31T12:05:23Z"
  },
  
  "alert": {
    "triggered": true,
    "escalation_level": 2,
    "conditions_met": ["precipitation_probability > 50%"],
    "forecast_summary": {
      "precipitation_probability": {"max": 75, "min": 20},
      "precipitation_mm": {"max": 12, "min": 0}
    }
  },
  
  "watchpoint": {
    "name": "Downtown Job Site",
    "location": {"lat": 40.7128, "lon": -74.006}
  }
}
```

**Client guidance:**
- Sort by `ordering.event_sequence` for strict ordering
- Use `ordering.forecast_timestamp` to identify which forecast generated the alert
- Handle out-of-order delivery gracefully (SQS Standard doesn't guarantee order)

### Event Sequence Tracking

```python
# In eval worker: increment and return sequence number
def get_next_event_sequence(conn, watchpoint_id: str) -> int:
    with conn.cursor() as cur:
        cur.execute("""
            UPDATE watchpoint_evaluation_state
            SET event_sequence = event_sequence + 1
            WHERE watchpoint_id = %s
            RETURNING event_sequence
        """, (watchpoint_id,))
        return cur.fetchone()[0]
```

---

## Observability & Dead Man's Switch

### The Problem

If RunPod fails silently, no `_SUCCESS` marker is written, no S3 event fires, no alerts go out. The system goes dark without anyone noticing.

### Solution: Dead Man's Switch Alarm

```yaml
# template.yaml

Resources:
  # Metric emitted by Batcher on every successful forecast
  ForecastReadyMetric:
    Type: AWS::Logs::MetricFilter
    Properties:
      LogGroupName: !Ref BatcherLogGroup
      FilterPattern: '"Batcher complete"'
      MetricTransformations:
        - MetricName: ForecastProcessed
          MetricNamespace: WatchPoint
          MetricValue: "1"

  # Alarm: No medium-range forecast in 8 hours (expected every 6)
  MediumRangeForecastStaleAlarm:
    Type: AWS::CloudWatch::Alarm
    Properties:
      AlarmName: watchpoint-medium-range-forecast-stale
      AlarmDescription: No medium-range forecast processed in 8 hours
      Namespace: WatchPoint
      MetricName: ForecastReady
      Dimensions:
        - Name: ForecastType
          Value: medium_range
      Statistic: Sum
      Period: 28800  # 8 hours
      EvaluationPeriods: 1
      Threshold: 1
      ComparisonOperator: LessThanThreshold
      TreatMissingDataAsBreaching: true
      AlarmActions:
        - !Ref AlertSNSTopic

  # Alarm: No nowcast in 30 minutes (expected every 15)
  NowcastForecastStaleAlarm:
    Type: AWS::CloudWatch::Alarm
    Properties:
      AlarmName: watchpoint-nowcast-forecast-stale
      AlarmDescription: No nowcast forecast processed in 30 minutes
      Namespace: WatchPoint
      MetricName: ForecastReady
      Dimensions:
        - Name: ForecastType
          Value: nowcast
      Statistic: Sum
      Period: 1800  # 30 minutes
      EvaluationPeriods: 1
      Threshold: 1
      ComparisonOperator: LessThanThreshold
      TreatMissingDataAsBreaching: true
      AlarmActions:
        - !Ref AlertSNSTopic

  # SNS Topic for alerts
  AlertSNSTopic:
    Type: AWS::SNS::Topic
    Properties:
      TopicName: watchpoint-alerts

  # Email subscription (configure your email)
  AlertEmailSubscription:
    Type: AWS::SNS::Subscription
    Properties:
      TopicArn: !Ref AlertSNSTopic
      Protocol: email
      Endpoint: !Ref AlertEmail

Parameters:
  AlertEmail:
    Type: String
    Description: Email address for operational alerts
```

### Additional Alarms

```yaml
  # Alarm: Evaluation queue backing up
  EvalQueueDepthAlarm:
    Type: AWS::CloudWatch::Alarm
    Properties:
      AlarmName: watchpoint-eval-queue-depth
      AlarmDescription: Evaluation queue has >100 messages (processing may be stuck)
      Namespace: AWS/SQS
      MetricName: ApproximateNumberOfMessagesVisible
      Dimensions:
        - Name: QueueName
          Value: !GetAtt EvalQueueStandard.QueueName
      Statistic: Maximum
      Period: 300
      EvaluationPeriods: 2
      Threshold: 100
      ComparisonOperator: GreaterThanThreshold
      AlarmActions:
        - !Ref AlertSNSTopic

  # Alarm: DLQ has messages (something is failing)
  DLQMessagesAlarm:
    Type: AWS::CloudWatch::Alarm
    Properties:
      AlarmName: watchpoint-dlq-messages
      AlarmDescription: Dead letter queue has messages (processing failures)
      Namespace: AWS/SQS
      MetricName: ApproximateNumberOfMessagesVisible
      Dimensions:
        - Name: QueueName
          Value: !GetAtt EvalQueueDLQ.QueueName
      Statistic: Sum
      Period: 300
      EvaluationPeriods: 1
      Threshold: 1
      ComparisonOperator: GreaterThanOrEqualToThreshold
      AlarmActions:
        - !Ref AlertSNSTopic
```

---

## Local Development

### Docker Compose Setup

```yaml
# docker-compose.yml

version: '3.8'

services:
  # MinIO (S3 compatible)
  minio:
    image: minio/minio:latest
    command: server /data --console-address ":9001"
    ports:
      - "9000:9000"   # S3 API
      - "9001:9001"   # Console
    environment:
      MINIO_ROOT_USER: minioadmin
      MINIO_ROOT_PASSWORD: minioadmin
    volumes:
      - minio_data:/data
    healthcheck:
      test: ["CMD", "curl", "-f", "http://localhost:9000/minio/health/live"]
      interval: 30s
      timeout: 20s
      retries: 3

  # PostgreSQL (matching Supabase)
  postgres:
    image: postgres:15
    ports:
      - "5432:5432"
    environment:
      POSTGRES_DB: watchpoint
      POSTGRES_USER: postgres
      POSTGRES_PASSWORD: localdev
    volumes:
      - postgres_data:/var/lib/postgresql/data
      - ./migrations:/docker-entrypoint-initdb.d
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U postgres"]
      interval: 10s
      timeout: 5s
      retries: 5

  # LocalStack (SQS, EventBridge)
  localstack:
    image: localstack/localstack:latest
    ports:
      - "4566:4566"
    environment:
      SERVICES: sqs,events,cloudwatch
      DEBUG: 0
    volumes:
      - localstack_data:/var/lib/localstack
      - /var/run/docker.sock:/var/run/docker.sock

  # MinIO bucket initialization
  minio-init:
    image: minio/mc:latest
    depends_on:
      minio:
        condition: service_healthy
    entrypoint: >
      /bin/sh -c "
      mc alias set local http://minio:9000 minioadmin minioadmin;
      mc mb local/watchpoint-forecasts --ignore-existing;
      mc anonymous set download local/watchpoint-forecasts;
      "

volumes:
  minio_data:
  postgres_data:
  localstack_data:
```

### Local Environment Variables

```bash
# .env.local

# Database
DATABASE_URL=postgres://postgres:localdev@localhost:5432/watchpoint

# S3 (MinIO)
AWS_ENDPOINT_URL=http://localhost:9000
AWS_ACCESS_KEY_ID=minioadmin
AWS_SECRET_ACCESS_KEY=minioadmin
FORECAST_BUCKET=watchpoint-forecasts

# SQS (LocalStack)
SQS_ENDPOINT_URL=http://localhost:4566
EVAL_QUEUE_URGENT_URL=http://localhost:4566/000000000000/eval-queue-urgent
EVAL_QUEUE_STANDARD_URL=http://localhost:4566/000000000000/eval-queue-standard
NOTIFICATION_QUEUE_URL=http://localhost:4566/000000000000/notification-queue
```

### Helper Scripts

```bash
# scripts/local-setup.sh
#!/bin/bash

# Start services
docker-compose up -d

# Wait for services
echo "Waiting for services..."
sleep 5

# Create SQS queues in LocalStack
aws --endpoint-url=http://localhost:4566 sqs create-queue --queue-name eval-queue-urgent
aws --endpoint-url=http://localhost:4566 sqs create-queue --queue-name eval-queue-standard
aws --endpoint-url=http://localhost:4566 sqs create-queue --queue-name notification-queue

# Run migrations
psql $DATABASE_URL -f migrations/001_extensions.up.sql
psql $DATABASE_URL -f migrations/002_watchpoints.up.sql
psql $DATABASE_URL -f migrations/003_evaluation_state.up.sql
psql $DATABASE_URL -f migrations/004_config_version.up.sql

echo "Local environment ready!"
```

```bash
# scripts/seed-forecast.sh
#!/bin/bash

# Generate sample Zarr forecast and upload to MinIO
python3 scripts/generate_sample_forecast.py

# Trigger fake S3 event to test Batcher
aws --endpoint-url=http://localhost:4566 events put-events \
  --entries '[{
    "Source": "aws.s3",
    "DetailType": "Object Created",
    "Detail": "{\"bucket\": \"watchpoint-forecasts\", \"key\": \"medium_range/2026-01-31T06:00:00Z/_SUCCESS\"}"
  }]'
```

```python
# scripts/generate_sample_forecast.py
import numpy as np
import zarr
import s3fs

def generate_sample_forecast():
    """Generate a sample Zarr forecast for local testing."""
    
    # Connect to MinIO
    fs = s3fs.S3FileSystem(
        endpoint_url='http://localhost:9000',
        key='minioadmin',
        secret='minioadmin'
    )
    
    # Sample data (small for testing)
    # Shape: (lat, lon, time, variables)
    data = np.random.rand(92, 182, 24, 8).astype(np.float32)
    
    # Write to MinIO
    prefix = "watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z"
    store = s3fs.S3Map(root=prefix, s3=fs)
    
    z = zarr.open_array(
        store,
        mode='w',
        shape=data.shape,
        chunks=(92, 182, 24, 8),
        dtype='<f4',
        compressor=zarr.Zstd(level=3),
        filters=None
    )
    z[:] = data
    
    zarr.consolidate_metadata(store)
    fs.touch(f"{prefix}/_SUCCESS")
    
    print(f"Sample forecast written to {prefix}")

if __name__ == "__main__":
    generate_sample_forecast()
```

### Running Locally

```bash
# Terminal 1: Start services
./scripts/local-setup.sh

# Terminal 2: Run API
cd cmd/api && go run main.go

# Terminal 3: Run Batcher (manual trigger)
cd cmd/batcher && go run main.go

# Terminal 4: Run Eval Worker
cd cmd/eval-worker && python handler.py

# Seed test data and trigger
./scripts/seed-forecast.sh
```

---

## Project Structure (Final)

```
watchpoint/
├── cmd/
│   ├── api/
│   │   └── main.go
│   ├── batcher/
│   │   └── main.go
│   ├── eval-worker/
│   │   ├── handler.py
│   │   ├── forecast_reader.py
│   │   ├── evaluation.py
│   │   └── requirements.txt
│   ├── email-worker/
│   │   └── main.go
│   ├── webhook-worker/
│   │   └── main.go
│   ├── data-poller/
│   │   └── main.go
│   └── archiver/
│       └── main.go
│
├── internal/
│   ├── api/
│   │   └── handlers/
│   │       ├── watchpoints.go
│   │       ├── organizations.go
│   │       └── stripe.go
│   ├── auth/
│   ├── db/
│   ├── notifications/
│   │   ├── formatter.go
│   │   ├── digest.go
│   │   └── quiet_hours.go
│   ├── security/
│   │   └── ssrf.go
│   └── config/
│
├── runpod/
│   ├── Dockerfile
│   ├── inference.py
│   └── zarr_writer.py
│
├── migrations/
│   ├── 001_extensions.up.sql
│   ├── 002_watchpoints.up.sql
│   ├── 003_evaluation_state.up.sql
│   └── 004_config_version.up.sql
│
├── scripts/
│   ├── local-setup.sh
│   ├── seed-forecast.sh
│   └── generate_sample_forecast.py
│
├── docker-compose.yml
├── template.yaml
├── samconfig.toml
├── go.mod
├── Makefile
└── README.md
```

---

## Cost Estimates (Final)

### Monthly Costs at Launch

| Service | Cost | Notes |
|---------|------|-------|
| RunPod | ~$260 | Zarr write is fast |
| S3 | ~$20 | Immutable paths |
| Lambda | ~$15 | Standard eval |
| Lambda (Provisioned) | ~$75 | 5 warm instances |
| SQS | ~$5 | |
| CloudWatch | ~$20 | Includes alarms |
| Supabase | $0 | Free tier |
| **Total** | **~$395/month** | |

### At Scale (100K WatchPoints)

| Service | Cost |
|---------|------|
| RunPod | ~$260 (fixed) |
| S3 | ~$40 |
| Lambda | ~$150 |
| Lambda (Provisioned) | ~$75 |
| CloudWatch | ~$30 |
| Supabase | $25 (Pro) |
| **Total** | **~$580/month** |

---

## Risk Mitigations (Final)

| Risk | Mitigation | Status |
|------|------------|--------|
| **Hot tile timeout** | Pagination (max 500 WPs per Lambda) | ✅ Fixed |
| **Webhook SSRF** | IP blocklist validation | ✅ Fixed |
| **Zarr atomicity** | Immutable timestamped paths | ✅ Explicit |
| **Quiet hours spam** | Consolidated digest on resume | ✅ Fixed |
| **Monitor mode re-alerts** | Hash-based threat deduplication | ✅ Fixed |
| **Batcher contention** | Removed active_tiles, use GROUP BY | ✅ Fixed (v3.3) |
| **Silent failures** | Dead man's switch alarms | ✅ Fixed (v3.3) |
| **Webhook ordering** | event_sequence + forecast_timestamp | ✅ Fixed (v3.3) |
| Nowcast cold starts | Provisioned concurrency | ✅ Addressed |
| Tile boundary interpolation | Halo regions | ✅ Addressed |
| Config race condition | config_version tracking | ✅ Addressed |
| DB connection exhaustion | Lambda concurrency cap | ✅ Addressed |
| Notification flapping | Hysteresis (10% buffer) | ✅ Addressed |

---

## Bootstrap Checklist (Final)

### Manual Setup

1. **Supabase**: Create project, get transaction pooler URL (port 6543)
2. **AWS**: Create account, set billing alerts
3. **Stripe**: Create account, configure webhook endpoint
4. **Domain**: Register, create Route53 hosted zone
5. **RunPod**: Create account, deploy inference container
6. **GitHub**: Add secrets (AWS, Stripe, Supabase)
7. **Alert Email**: Configure SNS subscription

### Local Development

```bash
# Clone and setup
git clone <repo>
cd watchpoint
cp .env.example .env.local

# Start local services
./scripts/local-setup.sh

# Seed test data
./scripts/seed-forecast.sh
```

### Deploy to AWS

```bash
sam build
sam deploy --config-env dev --guided

# Verify
curl https://api.watchpoint.io/v1/health
```

### Verify Alarms

```bash
# Check alarms are configured
aws cloudwatch describe-alarms --alarm-names \
  watchpoint-medium-range-forecast-stale \
  watchpoint-nowcast-forecast-stale \
  watchpoint-eval-queue-depth \
  watchpoint-dlq-messages
```

---

## Summary: Changes from v3.2 → v3.3

| Change | v3.2 | v3.3 |
|--------|------|------|
| Tile counting | `active_tiles` table + triggers | **GROUP BY query** (simpler, no contention) |
| Silent failure detection | None | **Dead man's switch** alarms |
| Webhook ordering | Not specified | **event_sequence** + timestamps |
| Local development | Not specified | **docker-compose** + scripts |
| Hysteresis tracking | Boolean only | Added **trigger_value** column |

---

*Document Version: 3.3 (Final)*
*Updated: 2026-02-01*
*Status: Approved for implementation*
