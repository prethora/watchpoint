// loader.go implements the configuration loading lifecycle for the WatchPoint platform.
//
// The loading sequence is:
//  1. Enforce UTC timezone to prevent drift bugs.
//  2. Load .env file via godotenv (non-fatal if absent).
//  3. Scan environment for _SSM_PARAM suffix variables.
//  4. If APP_ENV != "local", resolve SSM parameters via the SecretProvider
//     and inject the resolved values back into the environment.
//  5. Use envconfig to process struct tags and populate the Config struct.
//  6. Populate BuildInfo from linker-injected variables.
//  7. Validate the struct using go-playground/validator.
package config

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
	"github.com/joho/godotenv"
	"github.com/kelseyhightower/envconfig"
)

// ConfigError is a diagnostic error type returned by LoadConfig to aid debugging.
// It wraps a ConfigErrorType and an underlying error message.
type ConfigError struct {
	Type    ConfigErrorType
	Message string
	Err     error
}

// Error implements the error interface.
func (e *ConfigError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("[%s] %s: %v", e.Type, e.Message, e.Err)
	}
	return fmt.Sprintf("[%s] %s", e.Type, e.Message)
}

// Unwrap returns the underlying error for use with errors.Is/errors.As.
func (e *ConfigError) Unwrap() error {
	return e.Err
}

// ssmParamSuffix is the environment variable suffix used to identify SSM
// parameter pointer variables. For example, DATABASE_URL_SSM_PARAM points
// to the SSM path for the DATABASE_URL secret.
const ssmParamSuffix = "_SSM_PARAM"

// localEnv is the APP_ENV value that bypasses SSM resolution.
const localEnv = "local"

// envLookup is a function type for looking up environment variables.
// It matches the signature of os.LookupEnv and allows injection for testing.
type envLookup func(key string) (string, bool)

// envSet is a function type for setting environment variables.
// It matches the signature of os.Setenv and allows injection for testing.
type envSet func(key, value string) error

// environ is a function type for listing all environment variables.
// It matches the signature of os.Environ and allows injection for testing.
type environ func() []string

// loaderDeps holds the injectable dependencies for the loader, enabling
// testing without mutating global state.
type loaderDeps struct {
	lookupEnv envLookup
	setEnv    envSet
	environ   environ
}

// defaultDeps returns the standard OS-backed dependencies.
func defaultDeps() loaderDeps {
	return loaderDeps{
		lookupEnv: os.LookupEnv,
		setEnv:    os.Setenv,
		environ:   os.Environ,
	}
}

// LoadConfig loads and validates the WatchPoint configuration.
//
// It performs the following steps in order:
//  1. Sets the process timezone to UTC.
//  2. Loads a .env file if present (non-fatal if missing).
//  3. Scans environment for _SSM_PARAM variables.
//  4. If APP_ENV != "local", resolves the SSM parameters via the provider
//     and injects resolved values as environment variables.
//  5. Processes envconfig tags to populate the Config struct.
//  6. Populates Config.Build from linker-injected variables.
//  7. Validates the Config struct.
//
// The provider parameter is the SecretProvider to use for SSM resolution.
// For local development, the provider may be nil (SSM resolution is skipped).
// For non-local environments, the provider must be non-nil.
func LoadConfig(provider SecretProvider) (*Config, error) {
	return loadConfigWithDeps(provider, defaultDeps())
}

// loadConfigWithDeps is the internal implementation of LoadConfig that accepts
// injectable dependencies for testing.
func loadConfigWithDeps(provider SecretProvider, deps loaderDeps) (*Config, error) {
	// Step 1: Enforce UTC timezone to prevent drift bugs.
	time.Local = time.UTC

	// Step 2: Load .env file (non-fatal if absent).
	// godotenv.Load() will silently succeed if no .env file exists in the
	// working directory. It does NOT override existing environment variables.
	_ = godotenv.Load()

	// Step 3: Determine the environment.
	appEnv, _ := deps.lookupEnv("APP_ENV")

	// Step 4: Scan for _SSM_PARAM variables and resolve if non-local.
	if appEnv != localEnv {
		if err := resolveSSMParams(provider, deps); err != nil {
			return nil, err
		}
	}

	// Step 5: Process envconfig tags to populate the Config struct.
	// The empty prefix "" means envconfig will use the exact tag values
	// (e.g., envconfig:"APP_ENV" reads APP_ENV directly).
	var cfg Config
	if err := envconfig.Process("", &cfg); err != nil {
		return nil, &ConfigError{
			Type:    ErrParsing,
			Message: "failed to process environment configuration",
			Err:     err,
		}
	}

	// Step 6: Populate build metadata from linker-injected variables.
	cfg.Build = NewBuildInfo()

	// Step 7: Validate the populated struct.
	validate := validator.New()
	if err := validate.Struct(cfg); err != nil {
		return nil, &ConfigError{
			Type:    ErrValidation,
			Message: "configuration validation failed",
			Err:     err,
		}
	}

	return &cfg, nil
}

// ResolveSecrets performs the SSM secret resolution step in isolation, without
// loading or validating the full Config struct. It scans environment variables
// for _SSM_PARAM suffixed entries, fetches the secret values via the provider,
// and injects the resolved values back into the OS environment.
//
// This function is intended for Lambda entry points (e.g., batcher, data-poller)
// that read individual env vars via os.Getenv() instead of using LoadConfig().
// It should be called early in main(), before any os.Getenv() calls that depend
// on SSM-resolved values.
//
// If APP_ENV is "local", this function is a no-op (SSM resolution is skipped).
// If there are no _SSM_PARAM variables in the environment, this function is also
// a no-op.
func ResolveSecrets(provider SecretProvider) error {
	appEnv, _ := os.LookupEnv("APP_ENV")
	if appEnv == localEnv {
		return nil
	}
	return resolveSSMParams(provider, defaultDeps())
}

// resolveSSMParams scans the environment for variables ending in _SSM_PARAM,
// fetches the corresponding secret values via the SecretProvider, and injects
// them back into the environment so that envconfig can process them.
//
// For example, if DATABASE_URL_SSM_PARAM=/prod/watchpoint/database/url is set,
// this function will:
//  1. Extract the SSM path: /prod/watchpoint/database/url
//  2. Derive the target env var name: DATABASE_URL
//  3. Use the provider to fetch the secret value
//  4. Set DATABASE_URL=<resolved value> in the environment
//
// If the target variable is already set in the environment (via direct env var
// or .env file), the SSM resolution is skipped for that variable. This respects
// the priority chain: OS Environment > Dotenv > SSM.
func resolveSSMParams(provider SecretProvider, deps loaderDeps) error {
	// Collect all _SSM_PARAM variables and their target env var names.
	type ssmBinding struct {
		targetEnvVar string // e.g., DATABASE_URL
		ssmPath      string // e.g., /prod/watchpoint/database/url
	}

	var bindings []ssmBinding
	// ssmPathToTarget maps SSM path -> target env var for reverse lookup
	// after batch retrieval.
	ssmPathToTarget := make(map[string]string)

	envVars := deps.environ()
	for _, envEntry := range envVars {
		// Each entry is "KEY=VALUE"
		eqIdx := strings.IndexByte(envEntry, '=')
		if eqIdx < 0 {
			continue
		}
		key := envEntry[:eqIdx]

		if !strings.HasSuffix(key, ssmParamSuffix) {
			continue
		}

		// Derive the target env var name by stripping the _SSM_PARAM suffix.
		targetEnvVar := strings.TrimSuffix(key, ssmParamSuffix)

		// Skip if the target variable is already set (priority: Env > SSM).
		if _, exists := deps.lookupEnv(targetEnvVar); exists {
			continue
		}

		// Extract the SSM path from the variable value.
		ssmPath := envEntry[eqIdx+1:]
		if ssmPath == "" {
			continue // Skip empty SSM paths
		}

		bindings = append(bindings, ssmBinding{
			targetEnvVar: targetEnvVar,
			ssmPath:      ssmPath,
		})
		ssmPathToTarget[ssmPath] = targetEnvVar
	}

	// No SSM parameters to resolve.
	if len(bindings) == 0 {
		return nil
	}

	// A provider is required if there are SSM parameters to resolve.
	if provider == nil {
		targetVars := make([]string, 0, len(bindings))
		for _, b := range bindings {
			targetVars = append(targetVars, b.targetEnvVar)
		}
		return &ConfigError{
			Type:    ErrSSMResolution,
			Message: fmt.Sprintf("SecretProvider is required for non-local environments (need to resolve: %s)", strings.Join(targetVars, ", ")),
		}
	}

	// Collect SSM paths for batch retrieval.
	ssmPaths := make([]string, 0, len(bindings))
	for _, b := range bindings {
		ssmPaths = append(ssmPaths, b.ssmPath)
	}

	// Fetch all SSM values in a single batch call.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	resolved, err := provider.GetParametersBatch(ctx, ssmPaths)
	if err != nil {
		return &ConfigError{
			Type:    ErrSSMResolution,
			Message: fmt.Sprintf("failed to resolve %d SSM parameters", len(ssmPaths)),
			Err:     err,
		}
	}

	// Inject resolved values into the environment.
	for ssmPath, value := range resolved {
		targetEnvVar, ok := ssmPathToTarget[ssmPath]
		if !ok {
			continue // Shouldn't happen, but be defensive
		}
		if err := deps.setEnv(targetEnvVar, value); err != nil {
			return &ConfigError{
				Type:    ErrSSMResolution,
				Message: fmt.Sprintf("failed to set resolved value for %s", targetEnvVar),
				Err:     err,
			}
		}
	}

	// Check for any SSM paths that were not resolved.
	var missing []string
	for _, b := range bindings {
		if _, ok := resolved[b.ssmPath]; !ok {
			missing = append(missing, b.targetEnvVar)
		}
	}
	if len(missing) > 0 {
		return &ConfigError{
			Type:    ErrSSMResolution,
			Message: fmt.Sprintf("SSM parameters not found for: %s", strings.Join(missing, ", ")),
		}
	}

	return nil
}
