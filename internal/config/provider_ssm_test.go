package config

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmTypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// mockSSMClient is a test implementation of the ssmClient interface.
type mockSSMClient struct {
	// getParametersFn allows tests to inject custom behavior.
	getParametersFn func(ctx context.Context, params *ssm.GetParametersInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersOutput, error)
	// callCount tracks how many times GetParameters was called.
	callCount int
	// receivedBatches records the Names from each GetParameters call.
	receivedBatches [][]string
}

func (m *mockSSMClient) GetParameters(ctx context.Context, params *ssm.GetParametersInput, optFns ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
	m.callCount++
	m.receivedBatches = append(m.receivedBatches, params.Names)
	if m.getParametersFn != nil {
		return m.getParametersFn(ctx, params, optFns...)
	}
	return &ssm.GetParametersOutput{}, nil
}

// TestSSMProviderSatisfiesSecretProvider verifies that SSMProvider
// implements the SecretProvider interface at compile time.
func TestSSMProviderSatisfiesSecretProvider(t *testing.T) {
	var _ SecretProvider = (*SSMProvider)(nil)
	var _ SecretProvider = NewSSMProvider("us-east-1")
}

// TestSSMProviderEmptyKeysReturnsEmptyMap verifies that calling
// GetParametersBatch with an empty keys slice returns an empty map
// without error. No SSM call should be made.
func TestSSMProviderEmptyKeysReturnsEmptyMap(t *testing.T) {
	mock := &mockSSMClient{}
	provider := newSSMProviderWithClient("us-east-1", mock)

	result, err := provider.GetParametersBatch(context.Background(), []string{})
	if err != nil {
		t.Fatalf("GetParametersBatch with empty keys returned unexpected error: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty keys, got %v", result)
	}
	if mock.callCount != 0 {
		t.Errorf("expected no SSM calls for empty keys, got %d", mock.callCount)
	}
}

// TestSSMProviderNilKeysReturnsEmptyMap verifies that calling
// GetParametersBatch with nil keys returns an empty map without error.
func TestSSMProviderNilKeysReturnsEmptyMap(t *testing.T) {
	mock := &mockSSMClient{}
	provider := newSSMProviderWithClient("us-east-1", mock)

	result, err := provider.GetParametersBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetParametersBatch with nil keys returned unexpected error: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for nil keys, got %v", result)
	}
}

// TestSSMProviderSingleBatch verifies that a small number of keys (<=10)
// results in a single SSM GetParameters call.
func TestSSMProviderSingleBatch(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			parameters := make([]ssmTypes.Parameter, len(params.Names))
			for i, name := range params.Names {
				parameters[i] = ssmTypes.Parameter{
					Name:  aws.String(name),
					Value: aws.String("secret-value-for-" + name),
				}
			}
			return &ssm.GetParametersOutput{
				Parameters: parameters,
			}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := []string{
		"/dev/watchpoint/database/url",
		"/dev/watchpoint/auth/session_key",
		"/dev/watchpoint/billing/stripe_secret_key",
	}

	result, err := provider.GetParametersBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.callCount != 1 {
		t.Errorf("expected 1 SSM call for %d keys, got %d", len(keys), mock.callCount)
	}

	if len(result) != len(keys) {
		t.Fatalf("expected %d results, got %d", len(keys), len(result))
	}

	for _, key := range keys {
		expected := "secret-value-for-" + key
		if got := result[key]; got != expected {
			t.Errorf("result[%q] = %q, want %q", key, got, expected)
		}
	}
}

// TestSSMProviderMultipleBatches verifies that keys are batched into groups
// of 10 (the SSM API limit) and each batch results in a separate API call.
func TestSSMProviderMultipleBatches(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			parameters := make([]ssmTypes.Parameter, len(params.Names))
			for i, name := range params.Names {
				parameters[i] = ssmTypes.Parameter{
					Name:  aws.String(name),
					Value: aws.String("value-" + name),
				}
			}
			return &ssm.GetParametersOutput{
				Parameters: parameters,
			}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	// Create 23 keys to test batching (should be 3 batches: 10, 10, 3).
	keys := make([]string, 23)
	for i := range keys {
		keys[i] = fmt.Sprintf("/dev/watchpoint/param/%d", i)
	}

	result, err := provider.GetParametersBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if mock.callCount != 3 {
		t.Errorf("expected 3 SSM calls for 23 keys, got %d", mock.callCount)
	}

	// Verify batch sizes.
	if len(mock.receivedBatches) != 3 {
		t.Fatalf("expected 3 batches, got %d", len(mock.receivedBatches))
	}
	if len(mock.receivedBatches[0]) != 10 {
		t.Errorf("first batch should have 10 keys, got %d", len(mock.receivedBatches[0]))
	}
	if len(mock.receivedBatches[1]) != 10 {
		t.Errorf("second batch should have 10 keys, got %d", len(mock.receivedBatches[1]))
	}
	if len(mock.receivedBatches[2]) != 3 {
		t.Errorf("third batch should have 3 keys, got %d", len(mock.receivedBatches[2]))
	}

	// Verify all results are present.
	if len(result) != 23 {
		t.Fatalf("expected 23 results, got %d", len(result))
	}
}

// TestSSMProviderExactlyTenKeys verifies the boundary case where keys == 10
// results in exactly one batch.
func TestSSMProviderExactlyTenKeys(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			parameters := make([]ssmTypes.Parameter, len(params.Names))
			for i, name := range params.Names {
				parameters[i] = ssmTypes.Parameter{
					Name:  aws.String(name),
					Value: aws.String("v"),
				}
			}
			return &ssm.GetParametersOutput{Parameters: parameters}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := make([]string, 10)
	for i := range keys {
		keys[i] = fmt.Sprintf("/param/%d", i)
	}

	_, err := provider.GetParametersBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.callCount != 1 {
		t.Errorf("expected 1 SSM call for exactly 10 keys, got %d", mock.callCount)
	}
}

// TestSSMProviderElevenKeys verifies the boundary case where keys == 11
// results in exactly two batches (10 + 1).
func TestSSMProviderElevenKeys(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			parameters := make([]ssmTypes.Parameter, len(params.Names))
			for i, name := range params.Names {
				parameters[i] = ssmTypes.Parameter{
					Name:  aws.String(name),
					Value: aws.String("v"),
				}
			}
			return &ssm.GetParametersOutput{Parameters: parameters}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := make([]string, 11)
	for i := range keys {
		keys[i] = fmt.Sprintf("/param/%d", i)
	}

	_, err := provider.GetParametersBatch(context.Background(), keys)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if mock.callCount != 2 {
		t.Errorf("expected 2 SSM calls for 11 keys, got %d", mock.callCount)
	}
}

// TestSSMProviderInvalidParameters verifies that the provider returns an error
// when SSM reports invalid (not found) parameters.
func TestSSMProviderInvalidParameters(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			return &ssm.GetParametersOutput{
				Parameters: []ssmTypes.Parameter{
					{Name: aws.String(params.Names[0]), Value: aws.String("found")},
				},
				InvalidParameters: params.Names[1:],
			}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := []string{"/dev/found", "/dev/missing1", "/dev/missing2"}
	_, err := provider.GetParametersBatch(context.Background(), keys)
	if err == nil {
		t.Fatal("expected error for invalid parameters, got nil")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("error should mention 'not found', got: %s", err.Error())
	}
}

// TestSSMProviderAPIError verifies that the provider propagates SSM API errors.
func TestSSMProviderAPIError(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, _ *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			return nil, fmt.Errorf("access denied: no permission to call GetParameters")
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := []string{"/dev/watchpoint/database/url"}
	_, err := provider.GetParametersBatch(context.Background(), keys)
	if err == nil {
		t.Fatal("expected error from SSM API failure, got nil")
	}
	if !strings.Contains(err.Error(), "access denied") {
		t.Errorf("error should contain original API error, got: %s", err.Error())
	}
	if !strings.Contains(err.Error(), "SSM GetParameters failed") {
		t.Errorf("error should contain context about the failure, got: %s", err.Error())
	}
}

// TestSSMProviderContextCancellation verifies that the provider respects
// context cancellation between batches.
func TestSSMProviderContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately.

	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, _ *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			t.Error("GetParameters should not be called after context cancellation")
			return &ssm.GetParametersOutput{}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	keys := []string{"/dev/watchpoint/test"}
	_, err := provider.GetParametersBatch(ctx, keys)
	if err == nil {
		t.Fatal("expected error from cancelled context, got nil")
	}
	if !strings.Contains(err.Error(), "context cancelled") {
		t.Errorf("error should mention context cancellation, got: %s", err.Error())
	}
}

// TestSSMProviderWithDecryption verifies that GetParameters is called with
// WithDecryption=true, which is required for SecureString parameters.
func TestSSMProviderWithDecryption(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			if params.WithDecryption == nil || !*params.WithDecryption {
				t.Error("WithDecryption should be true")
			}
			return &ssm.GetParametersOutput{
				Parameters: []ssmTypes.Parameter{
					{Name: aws.String(params.Names[0]), Value: aws.String("decrypted")},
				},
			}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	result, err := provider.GetParametersBatch(context.Background(), []string{"/dev/secret"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result["/dev/secret"] != "decrypted" {
		t.Errorf("expected decrypted value, got %q", result["/dev/secret"])
	}
}

// TestSSMProviderAPIErrorOnSecondBatch verifies that an API error on the
// second batch returns an error, even though the first batch succeeded.
func TestSSMProviderAPIErrorOnSecondBatch(t *testing.T) {
	callNum := 0
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, params *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			callNum++
			if callNum == 2 {
				return nil, fmt.Errorf("throttling: rate exceeded")
			}
			parameters := make([]ssmTypes.Parameter, len(params.Names))
			for i, name := range params.Names {
				parameters[i] = ssmTypes.Parameter{
					Name:  aws.String(name),
					Value: aws.String("value"),
				}
			}
			return &ssm.GetParametersOutput{Parameters: parameters}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	// 15 keys = 2 batches (10 + 5).
	keys := make([]string, 15)
	for i := range keys {
		keys[i] = fmt.Sprintf("/param/%d", i)
	}

	_, err := provider.GetParametersBatch(context.Background(), keys)
	if err == nil {
		t.Fatal("expected error from second batch, got nil")
	}
	if !strings.Contains(err.Error(), "throttling") {
		t.Errorf("error should contain 'throttling', got: %s", err.Error())
	}
}

// TestNewSSMProviderStoresRegion verifies that the constructor correctly
// stores the provided region.
func TestNewSSMProviderStoresRegion(t *testing.T) {
	provider := NewSSMProvider("eu-west-1")
	if provider.region != "eu-west-1" {
		t.Errorf("provider.region = %q, want %q", provider.region, "eu-west-1")
	}
}

// TestSSMProviderNilNameOrValue verifies that parameters with nil Name or
// Value are safely skipped without panic.
func TestSSMProviderNilNameOrValue(t *testing.T) {
	mock := &mockSSMClient{
		getParametersFn: func(_ context.Context, _ *ssm.GetParametersInput, _ ...func(*ssm.Options)) (*ssm.GetParametersOutput, error) {
			return &ssm.GetParametersOutput{
				Parameters: []ssmTypes.Parameter{
					{Name: nil, Value: aws.String("orphan-value")},                      // nil Name
					{Name: aws.String("/param/good"), Value: aws.String("good-value")},  // valid
					{Name: aws.String("/param/novalue"), Value: nil},                     // nil Value
				},
			}, nil
		},
	}
	provider := newSSMProviderWithClient("us-east-1", mock)

	result, err := provider.GetParametersBatch(context.Background(), []string{"/param/good", "/param/novalue"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Only the parameter with both Name and Value should be in the result.
	if len(result) != 1 {
		t.Errorf("expected 1 result, got %d: %v", len(result), result)
	}
	if result["/param/good"] != "good-value" {
		t.Errorf("result[/param/good] = %q, want 'good-value'", result["/param/good"])
	}
}
