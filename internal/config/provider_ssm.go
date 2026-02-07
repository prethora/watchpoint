package config

import (
	"context"
	"fmt"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
)

// ssmMaxBatchSize is the maximum number of parameters that can be retrieved
// in a single SSM GetParameters API call. This is an AWS service limit.
const ssmMaxBatchSize = 10

// ssmClient is the subset of the SSM SDK client used by SSMProvider.
// This interface enables testing with a mock client.
type ssmClient interface {
	GetParameters(ctx context.Context, params *ssm.GetParametersInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersOutput, error)
}

// SSMProvider implements SecretProvider by resolving secret values from
// AWS Systems Manager (SSM) Parameter Store. This is the primary provider
// for non-local environments (dev, staging, prod) where secrets are stored
// as SecureString parameters in SSM.
//
// It performs batch GetParameters calls with decryption, respecting the
// SSM API limit of 10 parameters per request. Context cancellation is
// checked between batches for clean Lambda timeout handling.
type SSMProvider struct {
	// region is the AWS region where SSM parameters are stored.
	// Secrets are assumed to exist in the same region as the running Lambda
	// (Same-Region Policy).
	region string

	// client is the SSM API client. If nil, a new client is created
	// lazily using the configured region.
	client ssmClient
}

// NewSSMProvider creates a new SSMProvider configured for the specified
// AWS region. The region must match the region where SSM parameters are
// stored (Same-Region Policy enforced architecturally).
func NewSSMProvider(region string) *SSMProvider {
	return &SSMProvider{
		region: region,
	}
}

// newSSMProviderWithClient creates a new SSMProvider with an injected SSM client.
// This constructor is used for testing with a mock client.
func newSSMProviderWithClient(region string, client ssmClient) *SSMProvider {
	return &SSMProvider{
		region: region,
		client: client,
	}
}

// ensureClient initializes the SSM client if it has not been created yet.
// Uses the AWS SDK default config loader with the configured region.
func (p *SSMProvider) ensureClient(ctx context.Context) error {
	if p.client != nil {
		return nil
	}

	cfg, err := awsconfig.LoadDefaultConfig(ctx,
		awsconfig.WithRegion(p.region),
	)
	if err != nil {
		return fmt.Errorf("loading AWS config for SSM (region=%s): %w", p.region, err)
	}

	p.client = ssm.NewFromConfig(cfg)
	return nil
}

// GetParametersBatch retrieves multiple secret values from AWS SSM Parameter
// Store in batches. The keys slice contains SSM parameter paths to resolve.
//
// Implementation details:
//   - Batches keys into groups of 10 (SSM API limit)
//   - Calls ssm.GetParameters with WithDecryption for each batch
//   - Respects context cancellation between batches for timeout control
//   - Returns a map of parameter path -> decrypted plaintext value
//   - Reports any parameters that SSM flagged as invalid (not found)
func (p *SSMProvider) GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error) {
	if len(keys) == 0 {
		return make(map[string]string), nil
	}

	// Initialize the SSM client if needed.
	if err := p.ensureClient(ctx); err != nil {
		return nil, err
	}

	result := make(map[string]string, len(keys))

	// Process keys in batches of ssmMaxBatchSize.
	for i := 0; i < len(keys); i += ssmMaxBatchSize {
		// Check context cancellation before each batch.
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("context cancelled during SSM parameter retrieval: %w", ctx.Err())
		default:
		}

		end := i + ssmMaxBatchSize
		if end > len(keys) {
			end = len(keys)
		}
		batch := keys[i:end]

		output, err := p.client.GetParameters(ctx, &ssm.GetParametersInput{
			Names:          batch,
			WithDecryption: aws.Bool(true),
		})
		if err != nil {
			return nil, fmt.Errorf("SSM GetParameters failed (batch %d-%d of %d): %w",
				i, end-1, len(keys), err)
		}

		// Collect resolved parameters.
		for _, param := range output.Parameters {
			if param.Name != nil && param.Value != nil {
				result[*param.Name] = *param.Value
			}
		}

		// Check for invalid (not found) parameters.
		if len(output.InvalidParameters) > 0 {
			return nil, fmt.Errorf("SSM parameters not found: %v", output.InvalidParameters)
		}
	}

	return result, nil
}

