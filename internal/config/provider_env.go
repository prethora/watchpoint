package config

import (
	"context"
	"os"
)

// EnvVarProvider implements SecretProvider by resolving secret values from
// OS environment variables. This is the primary provider for local development
// where secrets are set directly in the environment or via a .env file,
// bypassing AWS SSM Parameter Store.
type EnvVarProvider struct{}

// NewEnvVarProvider creates a new EnvVarProvider.
func NewEnvVarProvider() *EnvVarProvider {
	return &EnvVarProvider{}
}

// GetParametersBatch resolves each key by looking it up as an OS environment
// variable via os.LookupEnv. Only keys that are found in the environment are
// included in the returned map; missing keys are silently omitted.
//
// The context parameter is accepted for interface compatibility but is not
// used, as environment variable lookups are synchronous and non-cancellable.
func (p *EnvVarProvider) GetParametersBatch(_ context.Context, keys []string) (map[string]string, error) {
	result := make(map[string]string, len(keys))
	for _, key := range keys {
		if val, ok := os.LookupEnv(key); ok {
			result[key] = val
		}
	}
	return result, nil
}
