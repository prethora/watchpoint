package config

import "context"

// SecretProvider abstracts the retrieval of secrets to support both
// AWS SSM Parameter Store (production) and environment variables (local development).
// This interface enables dependency injection for testing and environment-specific
// secret resolution.
type SecretProvider interface {
	// GetParametersBatch retrieves multiple secret values in parallel/batches
	// to avoid throttling. The keys slice contains the SSM parameter paths
	// (or equivalent identifiers) to resolve. Returns a map of key -> plaintext
	// value for all successfully resolved parameters.
	//
	// Implementations should handle batching and retry with jitter internally
	// to cope with API rate limits during massive cold starts.
	GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error)
}
