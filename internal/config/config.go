// Package config defines the global configuration structure for the WatchPoint platform.
// Configuration is loaded once at process initialization (Lambda Cold Start) and is
// immutable thereafter. It follows 12-Factor App principles by strictly separating
// code from configuration.
//
// Values are resolved via a priority chain:
//
//	OS Environment (Highest) -> Dotenv File -> AWS SSM Parameter Store (Lowest)
//
// Any missing required value or invalid format causes the application to panic
// immediately on startup (fail fast).
package config

import (
	"time"

	"watchpoint/internal/types"
)

// SecretString is an alias for types.SecretString, the redacted secret type used
// throughout configuration to prevent accidental logging of sensitive values.
type SecretString = types.SecretString

// Config is the top-level configuration struct for the WatchPoint platform.
// It is populated once during process initialization and never modified.
// Sub-components receive only the specific config subsets they require
// (Least Privilege principle).
type Config struct {
	// System Metadata
	Environment string `envconfig:"APP_ENV" validate:"required,oneof=local dev staging prod"`
	Service     string `envconfig:"OTEL_SERVICE_NAME" default:"watchpoint-service"`
	LogLevel    string `envconfig:"LOG_LEVEL" default:"info"`
	IsTestMode  bool   `envconfig:"IS_TEST_MODE" default:"false"`

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
	Webhook       WebhookConfig
	Feature       FeatureConfig

	// Build Metadata (Injected via ldflags, not Env)
	Build BuildInfo
}

// ServerConfig holds HTTP server and public URL configuration.
type ServerConfig struct {
	Port string `envconfig:"PORT" default:"8080"`
	// Public URLs for redirects and emails (no trailing slash)
	APIExternalURL string `envconfig:"API_EXTERNAL_URL" validate:"required,url"` // e.g., https://api.watchpoint.io
	DashboardURL   string `envconfig:"DASHBOARD_URL" validate:"required,url"`    // e.g., https://app.watchpoint.io
}

// DatabaseConfig holds database connection and pool tuning parameters.
type DatabaseConfig struct {
	// Resolved from SSM or Env
	URL SecretString `envconfig:"DATABASE_URL" validate:"required,url"`

	// Tuning Parameters
	MaxConns          int           `envconfig:"DB_MAX_CONNS" default:"10"`
	MinConns          int           `envconfig:"DB_MIN_CONNS" default:"2"`
	MaxConnLifetime   time.Duration `envconfig:"DB_MAX_CONN_LIFETIME" default:"30m"`
	AcquireTimeout    time.Duration `envconfig:"DB_ACQUIRE_TIMEOUT" default:"2s"`     // Fail fast when pool exhausted
	HealthCheckPeriod time.Duration `envconfig:"DB_HEALTH_CHECK_PERIOD" default:"1m"` // Detect dead connections during failover
}

// AWSConfig holds AWS resource identifiers and regional configuration.
type AWSConfig struct {
	Region string `envconfig:"AWS_REGION" default:"us-east-1"`

	// Resource Identifiers
	ForecastBucket    string `envconfig:"FORECAST_BUCKET" validate:"required"`
	ArchiveBucket     string `envconfig:"ARCHIVE_BUCKET"` // Cold storage for audit logs (MAINT-004)
	EvalQueueUrgent   string `envconfig:"SQS_EVAL_URGENT" validate:"required,url"`
	EvalQueueStandard string `envconfig:"SQS_EVAL_STANDARD" validate:"required,url"`
	NotificationQueue string `envconfig:"SQS_NOTIFICATIONS" validate:"required,url"`
	DlqURL            string `envconfig:"SQS_DLQ" validate:"required,url"`

	// LocalStack Support (Empty in Prod)
	EndpointURL string `envconfig:"AWS_ENDPOINT_URL"`
}

// BillingConfig holds Stripe payment integration credentials and keys.
type BillingConfig struct {
	StripeSecretKey      SecretString `envconfig:"STRIPE_SECRET_KEY" validate:"required"`
	StripeWebhookSecret  SecretString `envconfig:"STRIPE_WEBHOOK_SECRET" validate:"required"`
	StripePublishableKey string       `envconfig:"STRIPE_PUBLISHABLE_KEY" validate:"required"`
}

// WebhookConfig holds settings for outbound webhook delivery.
type WebhookConfig struct {
	UserAgent      string        `envconfig:"WEBHOOK_USER_AGENT" default:"WatchPoint-Webhook/1.0"`
	DefaultTimeout time.Duration `envconfig:"WEBHOOK_TIMEOUT" default:"10s"`
	MaxRedirects   int           `envconfig:"WEBHOOK_MAX_REDIRECTS" default:"3"`
}

// FeatureConfig holds emergency kill switches for system capabilities.
type FeatureConfig struct {
	EnableNowcast bool `envconfig:"FEATURE_ENABLE_NOWCAST" default:"true"`
	EnableEmail   bool `envconfig:"FEATURE_ENABLE_EMAIL" default:"true"`
}

// EmailConfig holds email delivery provider credentials and template configuration.
type EmailConfig struct {
	SendGridAPIKey SecretString `envconfig:"SENDGRID_API_KEY" validate:"required"`
	FromAddress    string       `envconfig:"EMAIL_FROM_ADDRESS" default:"alerts@watchpoint.io"`
	FromName       string       `envconfig:"EMAIL_FROM_NAME" default:"WatchPoint Alerts"`
	// Templates is a JSON mapping: "template_set" -> "event_type" -> "provider_id"
	// Example: {"default": {"threshold_crossed": "d-123..."}}
	Templates string `envconfig:"EMAIL_TEMPLATES_JSON" validate:"required,json"`
	Provider  string `envconfig:"EMAIL_PROVIDER" default:"sendgrid"`
}

// ForecastConfig holds forecast data source and inference configuration.
type ForecastConfig struct {
	RunPodAPIKey       SecretString  `envconfig:"RUNPOD_API_KEY" validate:"required"`
	RunPodEndpointID   string        `envconfig:"RUNPOD_ENDPOINT_ID" validate:"required"`
	UpstreamMirrors    []string      `envconfig:"UPSTREAM_MIRRORS" default:"noaa-gfs-bdp-pds,aws-noaa-gfs"`
	TimeoutMediumRange time.Duration `envconfig:"TIMEOUT_MEDIUM_RANGE" default:"3h"`
	TimeoutNowcast     time.Duration `envconfig:"TIMEOUT_NOWCAST" default:"20m"`
}

// AuthConfig holds OAuth provider credentials and session management secrets.
type AuthConfig struct {
	GoogleClientID     string       `envconfig:"GOOGLE_CLIENT_ID"`
	GoogleClientSecret SecretString `envconfig:"GOOGLE_CLIENT_SECRET"`
	GithubClientID     string       `envconfig:"GITHUB_CLIENT_ID"`
	GithubClientSecret SecretString `envconfig:"GITHUB_CLIENT_SECRET"`
	SessionKey         SecretString `envconfig:"SESSION_KEY" validate:"required,min=32"`
}

// SecurityConfig holds security-related configuration including admin access
// and CORS settings.
type SecurityConfig struct {
	AdminAPIKey        SecretString `envconfig:"ADMIN_API_KEY" validate:"required"`
	CorsAllowedOrigins []string     `envconfig:"CORS_ALLOWED_ORIGINS" default:"*"`
}

// ObservabilityConfig holds telemetry and monitoring settings.
type ObservabilityConfig struct {
	MetricNamespace string `envconfig:"METRIC_NAMESPACE" default:"WatchPoint"`
	EnableTracing   bool   `envconfig:"ENABLE_TRACING" default:"true"`
}

// BuildInfo holds build-time metadata injected via ldflags.
// These values are NOT populated from environment variables.
type BuildInfo struct {
	Version   string
	Commit    string
	BuildTime string
}

// ConfigErrorType categorizes configuration loading failures to aid debugging.
type ConfigErrorType string

const (
	// ErrMissingEnv indicates a required environment variable was not found.
	ErrMissingEnv ConfigErrorType = "MISSING_ENV"
	// ErrSSMResolution indicates a failure when fetching secrets from AWS SSM.
	ErrSSMResolution ConfigErrorType = "SSM_FAILURE"
	// ErrValidation indicates the configuration failed struct validation rules.
	ErrValidation ConfigErrorType = "VALIDATION_FAILED"
	// ErrParsing indicates a failure when parsing environment variable values
	// into their target types.
	ErrParsing ConfigErrorType = "PARSING_FAILED"
)
