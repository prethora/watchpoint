package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
	"time"

	"watchpoint/internal/types"
)

// TestSecretStringAlias verifies that config.SecretString is the same type
// as types.SecretString and retains its redaction behavior.
func TestSecretStringAlias(t *testing.T) {
	secret := SecretString("my-api-key")

	// Verify redaction via String()
	if got := secret.String(); got != "***REDACTED***" {
		t.Errorf("SecretString.String() = %q, want %q", got, "***REDACTED***")
	}

	// Verify redaction via MarshalJSON()
	jsonBytes, err := secret.MarshalJSON()
	if err != nil {
		t.Fatalf("SecretString.MarshalJSON() returned error: %v", err)
	}
	if got := string(jsonBytes); got != `"***REDACTED***"` {
		t.Errorf("SecretString.MarshalJSON() = %q, want %q", got, `"***REDACTED***"`)
	}

	// Verify Unmask() returns raw value
	if got := secret.Unmask(); got != "my-api-key" {
		t.Errorf("SecretString.Unmask() = %q, want %q", got, "my-api-key")
	}

	// Verify type identity with types.SecretString
	var typesSecret types.SecretString = "test"
	var configSecret SecretString = typesSecret
	if configSecret != typesSecret {
		t.Error("config.SecretString and types.SecretString should be the same type")
	}
}

// TestSecretStringFmtRedaction verifies that SecretString is redacted when
// used with fmt formatting functions.
func TestSecretStringFmtRedaction(t *testing.T) {
	secret := SecretString("super-secret-value")

	// fmt.Sprintf with %v should use String()
	if got := fmt.Sprintf("%v", secret); got != "***REDACTED***" {
		t.Errorf("fmt.Sprintf(%%v) = %q, want %q", got, "***REDACTED***")
	}

	// fmt.Sprintf with %s should use String()
	if got := fmt.Sprintf("%s", secret); got != "***REDACTED***" {
		t.Errorf("fmt.Sprintf(%%s) = %q, want %q", got, "***REDACTED***")
	}
}

// TestConfigStructFields verifies that the Config struct has all expected fields
// with the correct types.
func TestConfigStructFields(t *testing.T) {
	expectedFields := map[string]string{
		"Environment":   "string",
		"Service":       "string",
		"LogLevel":      "string",
		"IsTestMode":    "bool",
		"Server":        "config.ServerConfig",
		"Database":      "config.DatabaseConfig",
		"AWS":           "config.AWSConfig",
		"Billing":       "config.BillingConfig",
		"Email":         "config.EmailConfig",
		"Forecast":      "config.ForecastConfig",
		"Auth":          "config.AuthConfig",
		"Security":      "config.SecurityConfig",
		"Observability": "config.ObservabilityConfig",
		"Webhook":       "config.WebhookConfig",
		"Feature":       "config.FeatureConfig",
		"Build":         "config.BuildInfo",
	}

	configType := reflect.TypeOf(Config{})
	for fieldName, expectedType := range expectedFields {
		field, ok := configType.FieldByName(fieldName)
		if !ok {
			t.Errorf("Config is missing field %q", fieldName)
			continue
		}
		if got := field.Type.String(); got != expectedType {
			t.Errorf("Config.%s type = %q, want %q", fieldName, got, expectedType)
		}
	}

	// Verify total field count matches expected
	if got := configType.NumField(); got != len(expectedFields) {
		t.Errorf("Config has %d fields, want %d", got, len(expectedFields))
	}
}

// TestEnvconfigTags verifies that critical envconfig tags are correctly applied
// to the top-level Config struct and all sub-structs.
func TestEnvconfigTags(t *testing.T) {
	tests := []struct {
		structType reflect.Type
		fieldName  string
		tagKey     string
		wantValue  string
	}{
		// Config top-level
		{reflect.TypeOf(Config{}), "Environment", "envconfig", "APP_ENV"},
		{reflect.TypeOf(Config{}), "Service", "envconfig", "OTEL_SERVICE_NAME"},
		{reflect.TypeOf(Config{}), "LogLevel", "envconfig", "LOG_LEVEL"},
		{reflect.TypeOf(Config{}), "IsTestMode", "envconfig", "IS_TEST_MODE"},

		// ServerConfig
		{reflect.TypeOf(ServerConfig{}), "Port", "envconfig", "PORT"},
		{reflect.TypeOf(ServerConfig{}), "APIExternalURL", "envconfig", "API_EXTERNAL_URL"},
		{reflect.TypeOf(ServerConfig{}), "DashboardURL", "envconfig", "DASHBOARD_URL"},

		// DatabaseConfig
		{reflect.TypeOf(DatabaseConfig{}), "URL", "envconfig", "DATABASE_URL"},
		{reflect.TypeOf(DatabaseConfig{}), "MaxConns", "envconfig", "DB_MAX_CONNS"},
		{reflect.TypeOf(DatabaseConfig{}), "MinConns", "envconfig", "DB_MIN_CONNS"},
		{reflect.TypeOf(DatabaseConfig{}), "MaxConnLifetime", "envconfig", "DB_MAX_CONN_LIFETIME"},
		{reflect.TypeOf(DatabaseConfig{}), "AcquireTimeout", "envconfig", "DB_ACQUIRE_TIMEOUT"},
		{reflect.TypeOf(DatabaseConfig{}), "HealthCheckPeriod", "envconfig", "DB_HEALTH_CHECK_PERIOD"},

		// AWSConfig
		{reflect.TypeOf(AWSConfig{}), "Region", "envconfig", "AWS_REGION"},
		{reflect.TypeOf(AWSConfig{}), "ForecastBucket", "envconfig", "FORECAST_BUCKET"},
		{reflect.TypeOf(AWSConfig{}), "ArchiveBucket", "envconfig", "ARCHIVE_BUCKET"},
		{reflect.TypeOf(AWSConfig{}), "EvalQueueUrgent", "envconfig", "SQS_EVAL_URGENT"},
		{reflect.TypeOf(AWSConfig{}), "EvalQueueStandard", "envconfig", "SQS_EVAL_STANDARD"},
		{reflect.TypeOf(AWSConfig{}), "NotificationQueue", "envconfig", "SQS_NOTIFICATIONS"},
		{reflect.TypeOf(AWSConfig{}), "DlqURL", "envconfig", "SQS_DLQ"},
		{reflect.TypeOf(AWSConfig{}), "EndpointURL", "envconfig", "AWS_ENDPOINT_URL"},

		// BillingConfig
		{reflect.TypeOf(BillingConfig{}), "StripeSecretKey", "envconfig", "STRIPE_SECRET_KEY"},
		{reflect.TypeOf(BillingConfig{}), "StripeWebhookSecret", "envconfig", "STRIPE_WEBHOOK_SECRET"},
		{reflect.TypeOf(BillingConfig{}), "StripePublishableKey", "envconfig", "STRIPE_PUBLISHABLE_KEY"},

		// WebhookConfig
		{reflect.TypeOf(WebhookConfig{}), "UserAgent", "envconfig", "WEBHOOK_USER_AGENT"},
		{reflect.TypeOf(WebhookConfig{}), "DefaultTimeout", "envconfig", "WEBHOOK_TIMEOUT"},
		{reflect.TypeOf(WebhookConfig{}), "MaxRedirects", "envconfig", "WEBHOOK_MAX_REDIRECTS"},

		// FeatureConfig
		{reflect.TypeOf(FeatureConfig{}), "EnableNowcast", "envconfig", "FEATURE_ENABLE_NOWCAST"},
		{reflect.TypeOf(FeatureConfig{}), "EnableEmail", "envconfig", "FEATURE_ENABLE_EMAIL"},

		// EmailConfig
		{reflect.TypeOf(EmailConfig{}), "FromAddress", "envconfig", "EMAIL_FROM_ADDRESS"},
		{reflect.TypeOf(EmailConfig{}), "FromName", "envconfig", "EMAIL_FROM_NAME"},
		{reflect.TypeOf(EmailConfig{}), "SESRegion", "envconfig", "SES_REGION"},
		{reflect.TypeOf(EmailConfig{}), "Provider", "envconfig", "EMAIL_PROVIDER"},

		// ForecastConfig
		{reflect.TypeOf(ForecastConfig{}), "RunPodAPIKey", "envconfig", "RUNPOD_API_KEY"},
		{reflect.TypeOf(ForecastConfig{}), "RunPodEndpointID", "envconfig", "RUNPOD_ENDPOINT_ID"},
		{reflect.TypeOf(ForecastConfig{}), "UpstreamMirrors", "envconfig", "UPSTREAM_MIRRORS"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutMediumRange", "envconfig", "TIMEOUT_MEDIUM_RANGE"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutNowcast", "envconfig", "TIMEOUT_NOWCAST"},

		// AuthConfig
		{reflect.TypeOf(AuthConfig{}), "GoogleClientID", "envconfig", "GOOGLE_CLIENT_ID"},
		{reflect.TypeOf(AuthConfig{}), "GoogleClientSecret", "envconfig", "GOOGLE_CLIENT_SECRET"},
		{reflect.TypeOf(AuthConfig{}), "GithubClientID", "envconfig", "GITHUB_CLIENT_ID"},
		{reflect.TypeOf(AuthConfig{}), "GithubClientSecret", "envconfig", "GITHUB_CLIENT_SECRET"},
		{reflect.TypeOf(AuthConfig{}), "SessionKey", "envconfig", "SESSION_KEY"},

		// SecurityConfig
		{reflect.TypeOf(SecurityConfig{}), "AdminAPIKey", "envconfig", "ADMIN_API_KEY"},
		{reflect.TypeOf(SecurityConfig{}), "CorsAllowedOrigins", "envconfig", "CORS_ALLOWED_ORIGINS"},

		// ObservabilityConfig
		{reflect.TypeOf(ObservabilityConfig{}), "MetricNamespace", "envconfig", "METRIC_NAMESPACE"},
		{reflect.TypeOf(ObservabilityConfig{}), "EnableTracing", "envconfig", "ENABLE_TRACING"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			got := field.Tag.Get(tt.tagKey)
			if got != tt.wantValue {
				t.Errorf("%s.%s tag %q = %q, want %q", tt.structType.Name(), tt.fieldName, tt.tagKey, got, tt.wantValue)
			}
		})
	}
}

// TestValidateTags verifies that validation tags are correctly set on fields
// that require them.
func TestValidateTags(t *testing.T) {
	tests := []struct {
		structType reflect.Type
		fieldName  string
		wantTag    string
	}{
		{reflect.TypeOf(Config{}), "Environment", "required,oneof=local dev staging prod"},
		{reflect.TypeOf(ServerConfig{}), "APIExternalURL", "required,url"},
		{reflect.TypeOf(ServerConfig{}), "DashboardURL", "required,url"},
		{reflect.TypeOf(DatabaseConfig{}), "URL", "required,url"},
		{reflect.TypeOf(AWSConfig{}), "ForecastBucket", "required"},
		{reflect.TypeOf(AWSConfig{}), "EvalQueueUrgent", "required,url"},
		{reflect.TypeOf(AWSConfig{}), "EvalQueueStandard", "required,url"},
		{reflect.TypeOf(AWSConfig{}), "NotificationQueue", "required,url"},
		{reflect.TypeOf(AWSConfig{}), "DlqURL", "required,url"},
		{reflect.TypeOf(BillingConfig{}), "StripeSecretKey", "required"},
		{reflect.TypeOf(BillingConfig{}), "StripeWebhookSecret", "required"},
		{reflect.TypeOf(BillingConfig{}), "StripePublishableKey", "required"},
		{reflect.TypeOf(ForecastConfig{}), "RunPodAPIKey", "required"},
		{reflect.TypeOf(ForecastConfig{}), "RunPodEndpointID", "required"},
		{reflect.TypeOf(AuthConfig{}), "SessionKey", "required,min=32"},
		{reflect.TypeOf(SecurityConfig{}), "AdminAPIKey", "required"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			got := field.Tag.Get("validate")
			if got != tt.wantTag {
				t.Errorf("%s.%s validate tag = %q, want %q", tt.structType.Name(), tt.fieldName, got, tt.wantTag)
			}
		})
	}
}

// TestDefaultTags verifies that default values are correctly specified in
// struct tags for fields that have them.
func TestDefaultTags(t *testing.T) {
	tests := []struct {
		structType reflect.Type
		fieldName  string
		wantTag    string
	}{
		{reflect.TypeOf(Config{}), "Service", "watchpoint-service"},
		{reflect.TypeOf(Config{}), "LogLevel", "info"},
		{reflect.TypeOf(Config{}), "IsTestMode", "false"},
		{reflect.TypeOf(ServerConfig{}), "Port", "8080"},
		{reflect.TypeOf(DatabaseConfig{}), "MaxConns", "10"},
		{reflect.TypeOf(DatabaseConfig{}), "MinConns", "2"},
		{reflect.TypeOf(DatabaseConfig{}), "MaxConnLifetime", "30m"},
		{reflect.TypeOf(DatabaseConfig{}), "AcquireTimeout", "2s"},
		{reflect.TypeOf(DatabaseConfig{}), "HealthCheckPeriod", "1m"},
		{reflect.TypeOf(AWSConfig{}), "Region", "us-east-1"},
		{reflect.TypeOf(WebhookConfig{}), "UserAgent", "WatchPoint-Webhook/1.0"},
		{reflect.TypeOf(WebhookConfig{}), "DefaultTimeout", "10s"},
		{reflect.TypeOf(WebhookConfig{}), "MaxRedirects", "3"},
		{reflect.TypeOf(FeatureConfig{}), "EnableNowcast", "true"},
		{reflect.TypeOf(FeatureConfig{}), "EnableEmail", "true"},
		{reflect.TypeOf(EmailConfig{}), "FromAddress", "alerts@watchpoint.io"},
		{reflect.TypeOf(EmailConfig{}), "FromName", "WatchPoint Alerts"},
		{reflect.TypeOf(EmailConfig{}), "SESRegion", "us-east-1"},
		{reflect.TypeOf(EmailConfig{}), "Provider", "ses"},
		{reflect.TypeOf(ForecastConfig{}), "UpstreamMirrors", "noaa-gfs-bdp-pds,aws-noaa-gfs"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutMediumRange", "3h"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutNowcast", "20m"},
		{reflect.TypeOf(ObservabilityConfig{}), "MetricNamespace", "WatchPoint"},
		{reflect.TypeOf(ObservabilityConfig{}), "EnableTracing", "true"},
		{reflect.TypeOf(SecurityConfig{}), "CorsAllowedOrigins", "*"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			got := field.Tag.Get("default")
			if got != tt.wantTag {
				t.Errorf("%s.%s default tag = %q, want %q", tt.structType.Name(), tt.fieldName, got, tt.wantTag)
			}
		})
	}
}

// TestDurationFieldTypes verifies that time-based configuration fields use
// time.Duration as their Go type.
func TestDurationFieldTypes(t *testing.T) {
	durationType := reflect.TypeOf(time.Duration(0))

	tests := []struct {
		structType reflect.Type
		fieldName  string
	}{
		{reflect.TypeOf(DatabaseConfig{}), "MaxConnLifetime"},
		{reflect.TypeOf(DatabaseConfig{}), "AcquireTimeout"},
		{reflect.TypeOf(DatabaseConfig{}), "HealthCheckPeriod"},
		{reflect.TypeOf(WebhookConfig{}), "DefaultTimeout"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutMediumRange"},
		{reflect.TypeOf(ForecastConfig{}), "TimeoutNowcast"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			if field.Type != durationType {
				t.Errorf("%s.%s type = %v, want time.Duration", tt.structType.Name(), tt.fieldName, field.Type)
			}
		})
	}
}

// TestSecretStringFields verifies that all fields holding sensitive values
// use the SecretString type, which provides redaction.
func TestSecretStringFields(t *testing.T) {
	secretType := reflect.TypeOf(SecretString(""))

	tests := []struct {
		structType reflect.Type
		fieldName  string
	}{
		{reflect.TypeOf(DatabaseConfig{}), "URL"},
		{reflect.TypeOf(BillingConfig{}), "StripeSecretKey"},
		{reflect.TypeOf(BillingConfig{}), "StripeWebhookSecret"},
		{reflect.TypeOf(ForecastConfig{}), "RunPodAPIKey"},
		{reflect.TypeOf(AuthConfig{}), "GoogleClientSecret"},
		{reflect.TypeOf(AuthConfig{}), "GithubClientSecret"},
		{reflect.TypeOf(AuthConfig{}), "SessionKey"},
		{reflect.TypeOf(SecurityConfig{}), "AdminAPIKey"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			if field.Type != secretType {
				t.Errorf("%s.%s type = %v, want SecretString", tt.structType.Name(), tt.fieldName, field.Type)
			}
		})
	}
}

// TestConfigErrorTypeConstants verifies that all configuration error type
// constants are defined with the expected values.
func TestConfigErrorTypeConstants(t *testing.T) {
	tests := []struct {
		constant ConfigErrorType
		want     string
	}{
		{ErrMissingEnv, "MISSING_ENV"},
		{ErrSSMResolution, "SSM_FAILURE"},
		{ErrValidation, "VALIDATION_FAILED"},
		{ErrParsing, "PARSING_FAILED"},
	}

	for _, tt := range tests {
		if got := string(tt.constant); got != tt.want {
			t.Errorf("ConfigErrorType constant = %q, want %q", got, tt.want)
		}
	}
}

// TestBuildInfoZeroValue verifies that BuildInfo has a clean zero value
// with empty strings (not nil), which is important for JSON serialization.
func TestBuildInfoZeroValue(t *testing.T) {
	var info BuildInfo
	if info.Version != "" || info.Commit != "" || info.BuildTime != "" {
		t.Errorf("BuildInfo zero value should have empty strings, got: %+v", info)
	}
}

// TestConfigSecretFieldsJSONRedaction verifies that marshaling a Config
// with secret fields redacts all sensitive values.
func TestConfigSecretFieldsJSONRedaction(t *testing.T) {
	cfg := Config{
		Database: DatabaseConfig{
			URL: "postgres://user:password@host/db",
		},
		Billing: BillingConfig{
			StripeSecretKey:     "sk_test_123",
			StripeWebhookSecret: "whsec_test_456",
		},
		Auth: AuthConfig{
			SessionKey:         "a-very-long-session-key-that-is-at-least-32-chars",
			GoogleClientSecret: "google-secret",
			GithubClientSecret: "github-secret",
		},
		Security: SecurityConfig{
			AdminAPIKey: "admin-key-123",
		},
		Email: EmailConfig{},
		Forecast: ForecastConfig{
			RunPodAPIKey: "runpod-key-123",
		},
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatalf("json.Marshal(Config) returned error: %v", err)
	}

	jsonStr := string(data)

	// Verify no raw secrets appear in JSON
	secrets := []string{
		"postgres://user:password@host/db",
		"sk_test_123",
		"whsec_test_456",
		"a-very-long-session-key",
		"google-secret",
		"github-secret",
		"admin-key-123",
		"runpod-key-123",
	}

	for _, secret := range secrets {
		if contains(jsonStr, secret) {
			t.Errorf("JSON output contains raw secret value: %q", secret)
		}
	}
}

// contains checks if s contains substr. Defined here to avoid importing strings
// in a test file that focuses on reflection.
func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// TestSliceFieldTypes verifies that fields declared as slices have the correct
// element types.
func TestSliceFieldTypes(t *testing.T) {
	tests := []struct {
		structType  reflect.Type
		fieldName   string
		wantElemStr string
	}{
		{reflect.TypeOf(ForecastConfig{}), "UpstreamMirrors", "string"},
		{reflect.TypeOf(SecurityConfig{}), "CorsAllowedOrigins", "string"},
	}

	for _, tt := range tests {
		t.Run(tt.structType.Name()+"."+tt.fieldName, func(t *testing.T) {
			field, ok := tt.structType.FieldByName(tt.fieldName)
			if !ok {
				t.Fatalf("field %q not found on %s", tt.fieldName, tt.structType.Name())
			}
			if field.Type.Kind() != reflect.Slice {
				t.Fatalf("%s.%s is not a slice, got %v", tt.structType.Name(), tt.fieldName, field.Type.Kind())
			}
			if got := field.Type.Elem().String(); got != tt.wantElemStr {
				t.Errorf("%s.%s element type = %q, want %q", tt.structType.Name(), tt.fieldName, got, tt.wantElemStr)
			}
		})
	}
}
