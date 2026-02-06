package config

import (
	"context"
	"fmt"
)

// SSMProvider implements SecretProvider by resolving secret values from
// AWS Systems Manager (SSM) Parameter Store. This is the primary provider
// for non-local environments (dev, staging, prod) where secrets are stored
// as SecureString parameters in SSM.
//
// The full implementation will use aws-sdk-go-v2/service/ssm to perform
// batch GetParameters calls with decryption, client-side retry with jitter
// to handle throttling during massive cold starts, and batching to respect
// the SSM API limit of 10 parameters per request.
//
// This is currently a stub implementation. The real SSM retrieval logic
// will be implemented in a later task.
type SSMProvider struct {
	// region is the AWS region where SSM parameters are stored.
	// Secrets are assumed to exist in the same region as the running Lambda
	// (Same-Region Policy).
	region string
}

// NewSSMProvider creates a new SSMProvider configured for the specified
// AWS region. The region must match the region where SSM parameters are
// stored (Same-Region Policy enforced architecturally).
func NewSSMProvider(region string) *SSMProvider {
	return &SSMProvider{
		region: region,
	}
}

// GetParametersBatch retrieves multiple secret values from AWS SSM Parameter
// Store in batches. The keys slice contains SSM parameter paths to resolve.
//
// STUB IMPLEMENTATION: This method currently returns an error indicating
// that SSM retrieval is not yet implemented. The full implementation will:
//   - Batch keys into groups of 10 (SSM API limit)
//   - Call ssm.GetParameters with WithDecryption for each batch
//   - Implement client-side retry with exponential backoff and jitter
//   - Respect context cancellation for timeout control
//   - Return a map of parameter path -> decrypted plaintext value
func (p *SSMProvider) GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return make(map[string]string), nil
	}
	return nil, fmt.Errorf("SSMProvider.GetParametersBatch: not yet implemented (region=%s, keys=%d)", p.region, len(keys))
}
