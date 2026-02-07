# 04 - Infrastructure (SAM Template)

> **Purpose**: Defines the AWS Serverless Application Model (SAM) template structure. This acts as the "Infrastructure as Code" source of truth, defining all Lambda functions, queues, buckets, alarms, and permissions required to run the WatchPoint platform.
> **Package**: Root Level (`template.yaml`)
> **Dependencies**: `03-config.md` (Env Vars), `02-foundation-db.md` (DB Access)

---

## Table of Contents

1. [Overview & Globals](#1-overview--globals)
2. [Parameters & Conditions](#2-parameters--conditions)
3. [Storage & Networking](#3-storage--networking)
4. [Queues & Messaging](#4-queues--messaging)
5. [Compute Resources (Lambdas)](#5-compute-resources-lambdas)
6. [External Access (IAM)](#6-external-access-iam)
7. [Observability & Alarms](#7-observability--alarms)
8. [Outputs](#8-outputs)

---

## 1. Overview & Globals

The infrastructure is defined using **AWS SAM**. It deploys a serverless stack utilizing Go (Zip) for orchestration/API and Python (Container Image) for heavy data processing.

### Global Configuration
Applied to all `AWS::Serverless::Function` resources unless overridden.

| Setting | Value | Rationale |
|---|---|---|
| **Runtime (Go)** | `provided.al2023` | Low latency, standard for Go. |
| **Timeout** | `30` seconds | Default safety cap; extended for specific workers. |
| **MemorySize** | `256` MB | Baseline for Go binary performance. |
| **Tracing** | `Active` | Enables AWS X-Ray for distributed debugging. |
| **Architectures** | `arm64` | Graviton2 for cost/performance efficiency. |
| **Log Retention** | `30` days | Prevents infinite log storage costs. |

### Global Environment Variables
These variables inject the **SSM Parameter Paths** defined in `03-config.md`. The application code uses these paths to fetch the actual secrets at runtime.

```yaml
Globals:
  Function:
    LogGroupRetentionInDays: 30
    Environment:
      Variables:
        APP_ENV: !Ref Environment
        AWS_ENDPOINT_URL: !Ref AwsEndpointUrl # For LocalStack support
        
        # --- Service Discovery ---
        # Calculates the public API URL (Custom Domain or default AWS URL)
        API_EXTERNAL_URL: !If 
          - HasCustomDomain
          - !Sub "https://${DomainName}"
          - !Sub "https://${HttpApi}.execute-api.${AWS::Region}.amazonaws.com"
        
        # --- Core Infrastructure (SSM Pointers) ---
        DATABASE_URL_SSM_PARAM: !Sub /${Environment}/watchpoint/database/url
        
        # --- Billing (Stripe) ---
        STRIPE_SECRET_KEY_SSM_PARAM: !Sub /${Environment}/watchpoint/billing/stripe_secret_key
        STRIPE_WEBHOOK_SECRET_SSM_PARAM: !Sub /${Environment}/watchpoint/billing/stripe_webhook_secret
        # Publishable key is usually public/non-sensitive, but kept in SSM for consistency
        STRIPE_PUBLISHABLE_KEY_SSM_PARAM: !Sub /${Environment}/watchpoint/billing/stripe_publishable_key

        # --- Email (SES) ---
        # SES uses IAM-based authentication; no API key needed.
        # SES_REGION defaults to us-east-1 in EmailConfig.

        # --- Forecast (RunPod) ---
        RUNPOD_API_KEY_SSM_PARAM: !Sub /${Environment}/watchpoint/forecast/runpod_api_key
        RUNPOD_ENDPOINT_ID: !Ref RunPodEndpointId # Direct value, not a secret
        
        # --- Auth & Security ---
        SESSION_KEY_SSM_PARAM: !Sub /${Environment}/watchpoint/auth/session_key
        ADMIN_API_KEY_SSM_PARAM: !Sub /${Environment}/watchpoint/security/admin_api_key
        GOOGLE_CLIENT_ID: !Ref GoogleClientId
        GOOGLE_CLIENT_SECRET_SSM_PARAM: !Sub /${Environment}/watchpoint/auth/google_secret
        GITHUB_CLIENT_ID: !Ref GithubClientId
        GITHUB_CLIENT_SECRET_SSM_PARAM: !Sub /${Environment}/watchpoint/auth/github_secret
        
        # --- Queues & Metrics ---
        METRIC_NAMESPACE: WatchPoint
        SQS_EVAL_URGENT: !Ref EvalQueueUrgent
        SQS_EVAL_STANDARD: !Ref EvalQueueStandard
        SQS_NOTIFICATIONS: !Ref NotificationQueue
        SQS_DLQ: !Ref DeadLetterQueueShared
```

---

## 2. Parameters & Conditions

Inputs required when deploying the stack (`sam deploy --parameter-overrides ...`).

### 2.1 Parameters

| Parameter | Type | Default | Description |
|---|---|---|---|
| `Environment` | String | `dev` | Deployment environment (`dev`, `staging`, `prod`). |
| `DomainName` | String | `""` | Optional. Custom domain (e.g., `api.watchpoint.io`). |
| `CertificateArn` | String | `""` | Optional. ACM Certificate ARN for custom domain. |
| `AlertEmail` | String | (Required) | Email address to receive critical alarms (Dead Man's Switch). |
| `AwsEndpointUrl` | String | `""` | Optional. Overrides AWS SDK endpoint (used for LocalStack). |
| `CorsAllowedOrigins`| String | `*` | CORS origin allow list. |
| `RunPodEndpointId` | String | (Required) | ID of the deployed RunPod serverless endpoint. |
| `GoogleClientId` | String | `""` | OAuth Client ID for Google. |
| `GithubClientId` | String | `""` | OAuth Client ID for GitHub. |

### 2.2 Conditions

Logic to handle optional resources (e.g., Custom Domains).

```yaml
Conditions:
  HasCustomDomain: !Not [!Equals [!Ref DomainName, ""]]
```

---

## 3. Storage & Networking

### 3.1 API Gateway
**Resource ID**: `HttpApi`
*   **Type**: `AWS::Serverless::HttpApi` (v2)
*   **Properties**:
    *   **CorsConfiguration**:
        *   `AllowOrigins`: `!Split [",", !Ref CorsAllowedOrigins]`
        *   `AllowMethods`: `[GET, POST, PATCH, DELETE, OPTIONS]`
        *   `AllowHeaders`: `[Content-Type, Authorization, Idempotency-Key]`
    *   **Domain**:
        *   **Condition**: `HasCustomDomain`
        *   `DomainName`: `!Ref DomainName`
        *   `CertificateArn`: `!Ref CertificateArn`

### 3.2 S3 Buckets
**Resource ID**: `ForecastBucket`
*   **Type**: `AWS::S3::Bucket`
*   **Properties**:
    *   **VersioningConfiguration**: `Status: Enabled`
    *   **LifecycleConfiguration**:
        *   **Rules**:
            *   *Rule 1 (Intelligent Tiering)*:
                *   `Status`: `Enabled`
                *   `Transitions`:
                    *   `StorageClass`: `INTELLIGENT_TIERING`
                    *   `TransitionInDays`: `0` (immediate optimization)
            *   *Rule 2 (Nowcast Expiration)*:
                *   `Status`: `Enabled`
                *   `Prefix`: `nowcast/`
                *   `ExpirationInDays`: `7`
            *   *Rule 3 (Medium Range Expiration)*:
                *   `Status`: `Enabled`
                *   `Prefix`: `medium_range/`
                *   `ExpirationInDays`: `90`

### 3.3 Archive Bucket (Cold Storage)
**Resource ID**: `ArchiveBucket`
*   **Type**: `AWS::S3::Bucket`
*   **Properties**:
    *   **LifecycleConfiguration**:
        *   **Rules**:
            *   *Rule 1 (Glacier Transition)*:
                *   `Status`: `Enabled`
                *   `Transitions`:
                    *   `StorageClass`: `GLACIER`
                    *   `TransitionInDays`: `1`

*Note: Used for cold storage of archived audit logs (MAINT-004). Objects transition to Glacier after 1 day to minimize storage costs while preserving compliance data.*

### 3.4 Container Repositories (ECR)
Required for Python Eval Workers (`PackageType: Image`).

**Resource ID**: `EvalWorkerRepository`
*   **Type**: `AWS::ECR::Repository`
*   **Properties**:
    *   **LifecyclePolicy**: Retain only last 10 images (untagged or tagged).

---

## 4. Queues & Messaging

### 4.1 Dead Letter Queues (DLQ)
**Resource ID**: `DeadLetterQueueShared`
*   **Type**: `AWS::SQS::Queue`
*   **Properties**:
    *   `MessageRetentionPeriod`: 1209600 (14 days)

### 4.2 Processing Queues
Defined with strict visibility timeouts to exceed Lambda timeouts.

**Resource ID**: `EvalQueueUrgent`
*   **Type**: `AWS::SQS::Queue`
*   **Properties**:
    *   `VisibilityTimeout`: **360** (6 minutes). Matches 5-min Lambda timeout + buffer.
    *   `RedrivePolicy`: 
        *   `deadLetterTargetArn`: `!GetAtt DeadLetterQueueShared.Arn`
        *   `maxReceiveCount`: 3

**Resource ID**: `EvalQueueStandard`
*   **Type**: `AWS::SQS::Queue`
*   **Properties**:
    *   `VisibilityTimeout`: **360**.
    *   `RedrivePolicy`: 
        *   `deadLetterTargetArn`: `!GetAtt DeadLetterQueueShared.Arn`
        *   `maxReceiveCount`: 3

**Resource ID**: `NotificationQueue`
*   **Type**: `AWS::SQS::Queue`
*   **Properties**:
    *   `VisibilityTimeout`: **60** (1 min). Matches 30s Lambda timeout + buffer.
    *   `RedrivePolicy`: 
        *   `deadLetterTargetArn`: `!GetAtt DeadLetterQueueShared.Arn`
        *   `maxReceiveCount`: 3

### 4.3 SNS Topic (Alerts)
**Resource ID**: `AlertSNSTopic`
*   **Type**: `AWS::SNS::Topic`
*   **Subscription**:
    *   Protocol: `email`
    *   Endpoint: `!Ref AlertEmail`

---

## 5. Compute Resources (Lambdas)

### 5.1 Orchestration Layer (Go)

#### **ApiFunction**
*   **Code**: `cmd/api/`
*   **Handler**: `bootstrap`
*   **Events**: `HttpApi` (Path: `/{proxy+}`, Method: `ANY`)
*   **Policies**:
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/*`
    *   `SQSSendMessagePolicy`: `QueueName: !GetAtt NotificationQueue.QueueName`
    *   **SES Send Policy** (Inline):
        *   Action: `ses:SendEmail`, `ses:SendRawEmail`
        *   Resource: `*` (or scoped to verified identities ARN)

#### **BatcherFunction**
*   **Code**: `cmd/batcher/`
*   **Handler**: `bootstrap`
*   **Events**:
    *   **S3Event**: Bucket: `ForecastBucket`, Events: `s3:ObjectCreated:*`, Filter: `suffix: _SUCCESS`
*   **Policies**:
    *   `SQSSendMessagePolicy`: `QueueName: !GetAtt EvalQueueUrgent.QueueName`
    *   `SQSSendMessagePolicy`: `QueueName: !GetAtt EvalQueueStandard.QueueName`
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/database/*`

#### **DataPollerFunction**
*   **Code**: `cmd/data-poller/`
*   **Handler**: `bootstrap`
*   **Timeout**: 60s
*   **Events**:
    *   **Schedule**: `rate(15 minutes)`
*   **Policies**:
    *   `S3ReadPolicy`: `BucketName: !Ref ForecastBucket`
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/forecast/*` (RunPod API Key)
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/database/*` (Write forecast records)

#### **ArchiverFunction** (Multiplexed Maintenance)
*   **Code**: `cmd/archiver/`
*   **Handler**: `bootstrap`
*   **Timeout**: 60s
*   **Environment** (in addition to Globals):
    *   `ARCHIVE_BUCKET`: `!Ref ArchiveBucket`
*   **Events**:
    *   **ArchiveRule**: `rate(15 minutes)`, Input: `{"task": "archive"}`
    *   **DigestRule**: `rate(1 minute)`, Input: `{"task": "digest_trigger"}`
    *   **BillingRule**: `cron(0 0 * * ? *)`, Input: `{"task": "billing_sync"}`
    *   **CleanupRule**: `cron(0 2 * * ? *)`, Input: `{"task": "cleanup"}`
*   **Policies**:
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/*`
    *   `SQSSendMessagePolicy`: `QueueName: !GetAtt NotificationQueue.QueueName`
    *   `S3CrudPolicy`: `BucketName: !Ref ArchiveBucket` (s3:PutObject for audit log archival)

### 5.2 Processing Layer (Python)

#### **EvalWorkerUrgent**
*   **PackageType**: `Image`
*   **Metadata**: `Dockerfile: runpod/Dockerfile`, Context: `.`
*   **MemorySize**: 1024 MB
*   **Timeout**: 300s (5 minutes)
*   **AutoPublishAlias**: `live`
*   **ProvisionedConcurrencyConfig**: `ProvisionedConcurrentExecutions: 5`
*   **Events**:
    *   **SQS**: `EvalQueueUrgent`, BatchSize: 1
*   **Policies**:
    *   `S3ReadPolicy`: `BucketName: !Ref ForecastBucket`
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/database/*`
    *   `SQSSendMessagePolicy`: `QueueName: !GetAtt NotificationQueue.QueueName`

#### **EvalWorkerStandard**
*   **PackageType**: `Image`
*   **MemorySize**: 1024 MB
*   **Timeout**: 300s
*   **ReservedConcurrentExecutions**: 50 (Cost control cap)
*   **Events**:
    *   **SQS**: `EvalQueueStandard`, BatchSize: 10
*   **Policies**: Same as Urgent.

### 5.3 Notification Layer (Go)

#### **EmailWorkerFunction**
*   **Code**: `cmd/email-worker/`
*   **Handler**: `bootstrap`
*   **Events**:
    *   **SQS**: `NotificationQueue`, BatchSize: 10
        # Filtering happens inside the Lambda code or can be done via SQS attribute filtering if implemented in batcher
*   **Policies**:
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/*`
    *   **SES Send Policy** (Inline):
        *   Action: `ses:SendEmail`, `ses:SendRawEmail`
        *   Resource: `*` (or scoped to verified identities ARN)

#### **WebhookWorkerFunction**
*   **Code**: `cmd/webhook-worker/`
*   **Handler**: `bootstrap`
*   **Events**:
    *   **SQS**: `NotificationQueue`, BatchSize: 10
*   **Policies**:
    *   `SSMParameterReadPolicy`: `ParameterName: !Sub /${Environment}/watchpoint/*`

---

## 6. External Access (IAM)

Required for RunPod to write forecasts to S3.

**Resource ID**: `RunPodServiceUser`
*   **Type**: `AWS::IAM::User`
*   **Policies**:
    *   **RunPodS3Access**:
        *   Action: `s3:PutObject`, `s3:AbortMultipartUpload`
        *   Resource: `!Sub ${ForecastBucket.Arn}/*`

*Note: Access Keys are NOT generated by SAM to prevent secret leakage. They must be generated manually in the console after deployment.*

---

## 7. Observability & Alarms

### 7.1 Metric Filters
**Resource ID**: `ForecastReadyMetricFilter`
*   **Type**: `AWS::Logs::MetricFilter`
*   **Properties**:
    *   **LogGroupName**: `!Ref BatcherFunctionLogGroup`
    *   **FilterPattern**: `"ForecastReady"`
    *   **MetricTransformations**:
        *   `MetricName`: `ForecastReady`
        *   `MetricNamespace`: `WatchPoint`
        *   `MetricValue`: "1"

### 7.2 Alarms (Dead Man's Switch)
**Resource ID**: `MediumRangeStaleAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `ForecastReady`
    *   **Namespace**: `WatchPoint`
    *   **Dimensions**: 
        - Name: `ForecastType`
          Value: `medium_range`
    *   **Statistic**: `Sum`
    *   **Period**: `28800` (8 hours)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 1
    *   **ComparisonOperator**: `LessThanThreshold`
    *   **TreatMissingData**: `Breaching`
    *   **AlarmActions**: 
        - `!Ref AlertSNSTopic`

**Resource ID**: `NowcastStaleAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `ForecastReady`
    *   **Namespace**: `WatchPoint`
    *   **Dimensions**: 
        - Name: `ForecastType`
          Value: `nowcast`
    *   **Statistic**: `Sum`
    *   **Period**: `1800` (30 minutes)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 1
    *   **ComparisonOperator**: `LessThanThreshold`
    *   **TreatMissingData**: `Breaching`
    *   **AlarmActions**: 
        - `!Ref AlertSNSTopic`

### 7.3 Alarms (Queue Depth)
**Resource ID**: `EvalQueueDepthAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `ApproximateNumberOfMessagesVisible`
    *   **Namespace**: `AWS/SQS`
    *   **Dimensions**:
        - Name: `QueueName`
          Value: `!GetAtt EvalQueueStandard.QueueName`
    *   **Statistic**: `Maximum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 1000
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`

**Resource ID**: `EvalQueueUrgentDepthAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `ApproximateNumberOfMessagesVisible`
    *   **Namespace**: `AWS/SQS`
    *   **Dimensions**:
        - Name: `QueueName`
          Value: `!GetAtt EvalQueueUrgent.QueueName`
    *   **Statistic**: `Maximum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 100
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`

**Resource ID**: `NotificationQueueDepthAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `ApproximateNumberOfMessagesVisible`
    *   **Namespace**: `AWS/SQS`
    *   **Dimensions**:
        - Name: `QueueName`
          Value: `!GetAtt NotificationQueue.QueueName`
    *   **Statistic**: `Maximum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 500
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`

### 7.4 Alarms (Notification Spike)
**Resource ID**: `NotificationSpikeAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `DeliveryAttempt` (matches `01-foundation-types.md` MetricDeliveryAttempt)
    *   **Namespace**: `WatchPoint`
    *   **Statistic**: `Sum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 1000
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **TreatMissingData**: `NotBreaching`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`
    *   **AlarmDescription**: "Triggers when notification delivery attempts exceed 1000 in 5 minutes, indicating potential notification loop or spam."

### 7.5 Security Abuse Monitoring

**Resource ID**: `SecurityAbuseMetricFilter`
*   **Type**: `AWS::Logs::MetricFilter`
*   **Properties**:
    *   **LogGroupName**: `!Ref ApiFunctionLogGroup`
    *   **FilterPattern**: `{ $.action = "apikey.created" }`
    *   **MetricTransformations**:
        *   `MetricName`: `APIKeyCreated`
        *   `MetricNamespace`: `WatchPoint`
        *   `MetricValue`: "1"

**Resource ID**: `SuspiciousKeyCreationAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `APIKeyCreated`
    *   **Namespace**: `WatchPoint`
    *   **Statistic**: `Sum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 10
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **TreatMissingData**: `NotBreaching`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`
    *   **AlarmDescription**: "Triggers when API key creation exceeds 10 in 5 minutes, indicating potential account compromise or abuse."

### 7.6 Data Corruption Monitoring

**Resource ID**: `CorruptForecastMetricFilter`
*   **Type**: `AWS::Logs::MetricFilter`
*   **Properties**:
    *   **LogGroupName**: `!Ref EvalWorkerUrgentLogGroup` (and `!Ref EvalWorkerStandardLogGroup`)
    *   **FilterPattern**: `"ZarrChecksumError"`
    *   **MetricTransformations**:
        *   `MetricName`: `CorruptForecast`
        *   `MetricNamespace`: `WatchPoint`
        *   `MetricValue`: "1"

**Resource ID**: `CorruptForecastAlarm`
*   **Type**: `AWS::CloudWatch::Alarm`
*   **Properties**:
    *   **MetricName**: `CorruptForecast`
    *   **Namespace**: `WatchPoint`
    *   **Statistic**: `Sum`
    *   **Period**: `300` (5 min)
    *   **EvaluationPeriods**: 1
    *   **Threshold**: 0
    *   **ComparisonOperator**: `GreaterThanThreshold`
    *   **TreatMissingData**: `NotBreaching`
    *   **AlarmActions**:
        - `!Ref AlertSNSTopic`
    *   **AlarmDescription**: "**CRITICAL**: Alerts on forecast data corruption (Zarr checksum or format errors). Requires manual intervention to investigate and rebuild affected forecast data. Corrupted tiles cannot be recovered by automatic retry."

---

## 8. Outputs

The stack exposes the following values for use in `13-human-setup.md` or client configuration.

| Output Key | Description | Example |
|---|---|---|
| `ApiEndpoint` | URL of the HTTP API. | `https://xyz.execute-api.us-east-1.amazonaws.com` |
| `ForecastBucketName` | Name of the S3 bucket. | `watchpoint-forecasts-prod` |
| `ArchiveBucketName` | Name of the archive bucket for cold storage. | `watchpoint-archive-prod` |
| `RunPodUserArn` | ARN of the IAM User for RunPod. | `arn:aws:iam::123:user/RunPodServiceUser` |
| `EvalWorkerRepositoryUri` | ECR URI for pushing Python images. | `123.dkr.ecr.../eval-worker` |
| `NotificationQueueUrl` | URL of the notification queue. | `https://sqs...` |