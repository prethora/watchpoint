# 03 - Configuration & Secrets

> **Purpose**: Defines the global configuration structure, environment variable inventory, secrets management strategy, and loading patterns for the WatchPoint platform.
> **Package**: `package config`
> **Dependencies**: `01-foundation-types.md`

---

## Table of Contents

1. [Overview](#1-overview)
2. [Configuration Structure](#2-configuration-structure)
3. [Environment Variable Inventory](#3-environment-variable-inventory)
4. [Secret Management Strategy](#4-secret-management-strategy)
5. [Loading Lifecycle & Validation](#5-loading-lifecycle--validation)
6. [Local Development & Testing](#6-local-development--testing)
7. [Python Worker Parity](#7-python-worker-parity)
8. [Observability & Safety](#8-observability--safety)

---

## 1. Overview

The configuration system acts as the **Single Source of Truth** for the application. It enforces the **12-Factor App** principles by strictly separating code from configuration.

### Core Principles
1.  **Immutability**: Configuration is loaded once at process initialization (Lambda Cold Start) and never modified. Rotation requires redeployment.
2.  **Hybrid Resolution**: Values are resolved via a priority chain: OS Environment (Highest) -> Dotenv File -> AWS SSM Parameter Store (Lowest).
3.  **Fail Fast**: Any missing required value or invalid format causes the application to panic immediately on startup.
4.  **Least Privilege**: Sub-components receive only the specific config subsets they require.

---

## 2. Configuration Structure

The configuration is defined as a hierarchical Go struct. Tags are used to bind environment variables (`envconfig`) and enforce validation rules (`validate`).

### 2.1 Global Config

```go
type Config struct {
    // System Metadata
    Environment string    `envconfig:"APP_ENV" validate:"required,oneof=local dev staging prod"`
    Service     string    `envconfig:"OTEL_SERVICE_NAME" default:"watchpoint-service"`
    LogLevel    string    `envconfig:"LOG_LEVEL" default:"info"`
    IsTestMode  bool      `envconfig:"IS_TEST_MODE" default:"false"`

    // Domain Configurations
    Server        ServerConfig
    Database      DatabaseConfig
    AWS           AWSConfig
    Billing       BillingConfig
    Email         EmailConfig
    Forecast      ForecastConfig
    Auth          AuthConfig
    Security      SecurityConfig
    Observability ObservabilityConfig
    Webhook       WebhookConfig   // Added: Webhook delivery settings
    Feature       FeatureConfig   // Added: Emergency kill switches

    // Build Metadata (Injected via ldflags, not Env)
    Build BuildInfo
}
```

### 2.2 Domain Sub-Structs

```go
type ServerConfig struct {
    Port           string `envconfig:"PORT" default:"8080"`
    // Public URLs for redirects and emails (no trailing slash)
    APIExternalURL string `envconfig:"API_EXTERNAL_URL" validate:"required,url"` // e.g., https://api.watchpoint.io
    DashboardURL   string `envconfig:"DASHBOARD_URL" validate:"required,url"`    // e.g., https://app.watchpoint.io
}

type DatabaseConfig struct {
    // Resolved from SSM or Env
    URL SecretString `envconfig:"DATABASE_URL" validate:"required,url"`

    // Tuning Parameters
    MaxConns          int           `envconfig:"DB_MAX_CONNS" default:"10"`
    MinConns          int           `envconfig:"DB_MIN_CONNS" default:"2"`
    MaxConnLifetime   time.Duration `envconfig:"DB_MAX_CONN_LIFETIME" default:"30m"`
    AcquireTimeout    time.Duration `envconfig:"DB_ACQUIRE_TIMEOUT" default:"2s"`    // Fail fast when pool exhausted
    HealthCheckPeriod time.Duration `envconfig:"DB_HEALTH_CHECK_PERIOD" default:"1m"` // Detect dead connections during failover
}

type AWSConfig struct {
    Region string `envconfig:"AWS_REGION" default:"us-east-1"`

    // Resource Identifiers
    ForecastBucket       string `envconfig:"FORECAST_BUCKET" validate:"required"`
    ArchiveBucket        string `envconfig:"ARCHIVE_BUCKET"` // Cold storage for audit logs (MAINT-004)
    EvalQueueUrgent      string `envconfig:"SQS_EVAL_URGENT" validate:"required,url"`
    EvalQueueStandard    string `envconfig:"SQS_EVAL_STANDARD" validate:"required,url"`
    NotificationQueue    string `envconfig:"SQS_NOTIFICATIONS" validate:"required,url"`
    DlqURL               string `envconfig:"SQS_DLQ" validate:"required,url"`

    // LocalStack Support (Empty in Prod)
    EndpointURL string `envconfig:"AWS_ENDPOINT_URL"`
}

type BillingConfig struct {
    StripeSecretKey      SecretString `envconfig:"STRIPE_SECRET_KEY" validate:"required"`
    StripeWebhookSecret  SecretString `envconfig:"STRIPE_WEBHOOK_SECRET" validate:"required"`
    StripePublishableKey string       `envconfig:"STRIPE_PUBLISHABLE_KEY" validate:"required"`
}

type WebhookConfig struct {
    UserAgent      string        `envconfig:"WEBHOOK_USER_AGENT" default:"WatchPoint-Webhook/1.0"`
    DefaultTimeout time.Duration `envconfig:"WEBHOOK_TIMEOUT" default:"10s"`
    MaxRedirects   int           `envconfig:"WEBHOOK_MAX_REDIRECTS" default:"3"`
}

type FeatureConfig struct {
    EnableNowcast bool `envconfig:"FEATURE_ENABLE_NOWCAST" default:"true"`
    EnableEmail   bool `envconfig:"FEATURE_ENABLE_EMAIL" default:"true"`
}

type EmailConfig struct {
    SendGridAPIKey SecretString `envconfig:"SENDGRID_API_KEY" validate:"required"`
    FromAddress    string       `envconfig:"EMAIL_FROM_ADDRESS" default:"alerts@watchpoint.io"`
    FromName       string       `envconfig:"EMAIL_FROM_NAME" default:"WatchPoint Alerts"`
    // Templates: JSON mapping "template_set" -> "event_type" -> "provider_id"
    // Example: {"default": {"threshold_crossed": "d-123..."}}
    Templates      string       `envconfig:"EMAIL_TEMPLATES_JSON" validate:"required,json"`
    Provider       string       `envconfig:"EMAIL_PROVIDER" default:"sendgrid"`
}

type ForecastConfig struct {
    RunPodAPIKey      SecretString  `envconfig:"RUNPOD_API_KEY" validate:"required"`
    RunPodEndpointID  string        `envconfig:"RUNPOD_ENDPOINT_ID" validate:"required"`
    UpstreamMirrors   []string      `envconfig:"UPSTREAM_MIRRORS" default:"noaa-gfs-bdp-pds,aws-noaa-gfs"`
    TimeoutMediumRange time.Duration `envconfig:"TIMEOUT_MEDIUM_RANGE" default:"3h"`
    TimeoutNowcast     time.Duration `envconfig:"TIMEOUT_NOWCAST" default:"20m"`
}

type AuthConfig struct {
    GoogleClientID     string       `envconfig:"GOOGLE_CLIENT_ID"`
    GoogleClientSecret SecretString `envconfig:"GOOGLE_CLIENT_SECRET"`
    GithubClientID     string       `envconfig:"GITHUB_CLIENT_ID"`
    GithubClientSecret SecretString `envconfig:"GITHUB_CLIENT_SECRET"`
    SessionKey         SecretString `envconfig:"SESSION_KEY" validate:"required,min=32"`
}

type SecurityConfig struct {
    AdminAPIKey        SecretString `envconfig:"ADMIN_API_KEY" validate:"required"`
    CorsAllowedOrigins []string     `envconfig:"CORS_ALLOWED_ORIGINS" default:"*"`
}

type ObservabilityConfig struct {
    MetricNamespace string `envconfig:"METRIC_NAMESPACE" default:"WatchPoint"`
    EnableTracing   bool   `envconfig:"ENABLE_TRACING" default:"true"`
}

type BuildInfo struct {
    Version   string
    Commit    string
    BuildTime string
}
```

### 2.3 Secret String Type

To prevent accidental logging of sensitive values, we define a custom type that overrides string formatting.

```go
type SecretString string

// String returns a redacted placeholder "***REDACTED***"
func (s SecretString) String() string {
    return "***REDACTED***"
}

// MarshalJSON returns the redacted placeholder in JSON
func (s SecretString) MarshalJSON() ([]byte, error) {
    return []byte(`"***REDACTED***"`), nil
}

// Unmask returns the raw plaintext value (usage strictly audited)
func (s SecretString) Unmask() string {
    return string(s)
}
```

---

## 3. Environment Variable Inventory

This section defines the contract between the Infrastructure (SAM `template.yaml`) and the Application.

### 3.1 Direct Values (System & Infrastructure)

These variables contain non-sensitive configuration or resource identifiers.

| Variable Name | Type | Description |
| :--- | :--- | :--- |
| `APP_ENV` | String | `dev`, `staging`, or `prod`. |
| `LOG_LEVEL` | String | Logging verbosity (debug, info, warn, error). |
| `AWS_REGION` | String | AWS Region (Injected by Runtime). |
| `PORT` | Number | Server port (default 8080). |
| `API_EXTERNAL_URL` | URL | Public API URL for OAuth callbacks. |
| `DASHBOARD_URL` | URL | Public UI URL for email links/redirects. |
| `FORECAST_BUCKET` | String | S3 bucket name. |
| `SQS_EVAL_URGENT` | URL | SQS URL. |
| `SQS_EVAL_STANDARD` | URL | SQS URL. |
| `SQS_NOTIFICATIONS` | URL | SQS URL. |
| `SQS_DLQ` | URL | SQS URL. |
| `CORS_ALLOWED_ORIGINS`| List | Comma-separated allowed origins. |
| `EMAIL_TEMPLATES_JSON`| JSON | Template ID mapping for email provider. |
| `EMAIL_PROVIDER`      | String | Email provider name (default: sendgrid). |
| `WEBHOOK_USER_AGENT`  | String | User-Agent header for webhook requests. |
| `WEBHOOK_TIMEOUT`     | Duration | Timeout for webhook HTTP requests. |
| `WEBHOOK_MAX_REDIRECTS`| Number | Max redirects to follow for webhooks. |
| `FEATURE_ENABLE_NOWCAST`| Boolean | Emergency kill switch for Nowcast processing. |
| `FEATURE_ENABLE_EMAIL`| Boolean | Emergency kill switch for email delivery. |

### 3.2 Secret Pointers (SSM References)

These variables contain **Path Strings** pointing to AWS SSM Parameter Store values. The `LoadConfig` function uses these keys to fetch the actual secrets at runtime.

| Variable Name | SSM Path Example (Value) |
| :--- | :--- |
| `DATABASE_URL_SSM_PARAM` | `/{env}/watchpoint/database/url` |
| `STRIPE_SECRET_KEY_SSM_PARAM` | `/{env}/watchpoint/billing/stripe_secret_key` |
| `STRIPE_WEBHOOK_SECRET_SSM_PARAM`| `/{env}/watchpoint/billing/stripe_webhook_secret` |
| `SENDGRID_API_KEY_SSM_PARAM` | `/{env}/watchpoint/email/sendgrid_api_key` |
| `RUNPOD_API_KEY_SSM_PARAM` | `/{env}/watchpoint/forecast/runpod_api_key` |
| `SESSION_KEY_SSM_PARAM` | `/{env}/watchpoint/auth/session_key` |
| `ADMIN_API_KEY_SSM_PARAM` | `/{env}/watchpoint/security/admin_api_key` |
| `GOOGLE_CLIENT_SECRET_SSM_PARAM` | `/{env}/watchpoint/auth/google_secret` |
| `GITHUB_CLIENT_SECRET_SSM_PARAM` | `/{env}/watchpoint/auth/github_secret` |
| `EMAIL_TEMPLATES_JSON_SSM_PARAM` | `/{env}/watchpoint/email/templates_json` |

---

## 4. Secret Management Strategy

### 4.1 SSM Path Hierarchy

We enforce a strict naming convention for SSM parameters to ensure deterministic retrieval and IAM policy scoping.

**Format**: `/{environment}/{application}/{category}/{key}`

### 4.2 Parameter Type Matrix

This matrix guides the "Human Setup" (Doc 13) process.

| Configuration Field | SSM Type |
| :--- | :--- |
| `Database.URL` | **SecureString** |
| `Billing.StripeSecretKey` | **SecureString** |
| `Billing.StripeWebhookSecret` | **SecureString** |
| `Auth.SessionKey` | **SecureString** |
| `Email.SendGridAPIKey` | **SecureString** |
| `Forecast.RunPodAPIKey` | **SecureString** |
| `Security.AdminAPIKey` | **SecureString** |
| `Forecast.RunPodEndpointID` | *String* |

---

## 5. Loading Lifecycle & Validation

Configuration loading occurs during the **Lambda Initialization Phase** to minimize per-request latency.

### 5.1 The SecretProvider Interface

Abstracts the retrieval of secrets to support both AWS SSM (Prod) and Env Vars (Local).

```go
type SecretProvider interface {
    // GetParametersBatch retrieves multiple keys in parallel/batches to avoid throttling
    GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error)
}
```

### 5.2 Loading Logic

1.  **Initialize**: `LoadConfig` checks `APP_ENV`.
2.  **Scan**: It iterates over all environment variables ending in `_SSM_PARAM`.
3.  **Fetch**:
    *   If `APP_ENV != local`, it uses the `SecretProvider` (AWS SSM) to fetch values.
    *   It uses **Explicit Batch Retrieval** (grouping keys) rather than recursive path crawling.
    *   It uses **Client-Side Retry with Jitter** to handle throttling during massive cold starts.
4.  **Bind**: It maps the resolved values (and direct env vars) to the `Config` struct using `envconfig`.
5.  **Validate**: It runs the `Validator` against the struct.

### 5.3 Error Handling

Failures during loading return a diagnostic error type to aid debugging.

```go
type ConfigErrorType string

const (
    ErrMissingEnv       ConfigErrorType = "MISSING_ENV"
    ErrSSMResolution    ConfigErrorType = "SSM_FAILURE"
    ErrValidation       ConfigErrorType = "VALIDATION_FAILED"
    ErrParsing          ConfigErrorType = "PARSING_FAILED"
)
```

---

## 6. Local Development & Testing

### 6.1 Dotenv Priority

For local execution (`go run` or Docker), the system bypasses SSM.

**Priority Order**:
1.  OS Environment Variables (Flags)
2.  `.env` file (Local Secrets)
3.  SSM Parameter Store (Only if `APP_ENV != local`)

### 6.2 Testing Parity

To ensure unit tests are isolated:
1.  **EnvProvider Interface**: `LoadConfig` accepts an interface wrapping `os.Getenv` / `os.LookupEnv`.
2.  **Mock Implementation**: Tests inject a map-based provider, avoiding global state mutation.

---

## 7. Python Worker Parity

The Python Eval Worker must adhere to the same contract. We use `pydantic` but explicitly define field mappings to match the Go `envconfig` tags.

```python
from pydantic import BaseSettings, Field, SecretStr

class Settings(BaseSettings):
    # Explicit mapping prevents auto-inference drift
    database_url: SecretStr = Field(..., env="DATABASE_URL")
    sqs_eval_urgent: str = Field(..., env="SQS_EVAL_URGENT")
    sqs_eval_standard: str = Field(..., env="SQS_EVAL_STANDARD")
    sqs_notifications: str = Field(..., env="SQS_NOTIFICATIONS")
    
    class Config:
        env_file = ".env"
        case_sensitive = True
```

**Resolution Logic**:
A Python helper `resolve_secret` mimics the Go logic: checks for the direct Env Var first, then falls back to the `_SSM_PARAM` pointer using `boto3`.

---

## 8. Observability & Safety

### 8.1 Build Metadata
Metadata is injected via `ldflags` at compile time, not via environment variables.

```go
// Injected by compiler
var (
    version   = "dev"
    commit    = "none"
    buildTime = "unknown"
)
```

### 8.2 Timezone Enforcement
To prevent timezone drift bugs, the configuration loader explicitly sets the process timezone to UTC.

```go
time.Local = time.UTC
```

### 8.3 Cross-Region Policy
Configuration loading adheres to a **Same-Region Policy**. Secrets are assumed to exist in the same AWS Region as the running Lambda. Cross-region fetching is architecturally prohibited to ensure isolation and performance.