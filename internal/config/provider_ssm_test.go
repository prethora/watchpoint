package config

import (
	"context"
	"strings"
	"testing"
)

// TestSSMProviderSatisfiesSecretProvider verifies that SSMProvider
// implements the SecretProvider interface at compile time.
func TestSSMProviderSatisfiesSecretProvider(t *testing.T) {
	var _ SecretProvider = (*SSMProvider)(nil)
	var _ SecretProvider = NewSSMProvider("us-east-1")
}

// TestSSMProviderStubReturnsError verifies that the stub implementation
// returns an error when called with actual keys, since the SSM retrieval
// logic is not yet implemented.
func TestSSMProviderStubReturnsError(t *testing.T) {
	provider := NewSSMProvider("us-east-1")
	keys := []string{"/dev/watchpoint/database/url", "/dev/watchpoint/auth/session_key"}

	result, err := provider.GetParametersBatch(context.Background(), keys)
	if err == nil {
		t.Fatal("expected error from stub SSMProvider, got nil")
	}
	if result != nil {
		t.Errorf("expected nil result from stub SSMProvider, got %v", result)
	}

	// Verify the error message is descriptive
	errMsg := err.Error()
	if !strings.Contains(errMsg, "not yet implemented") {
		t.Errorf("error message should contain 'not yet implemented', got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "us-east-1") {
		t.Errorf("error message should contain region, got: %s", errMsg)
	}
	if !strings.Contains(errMsg, "2") {
		t.Errorf("error message should contain key count, got: %s", errMsg)
	}
}

// TestSSMProviderEmptyKeysReturnsEmptyMap verifies that calling
// GetParametersBatch with an empty keys slice returns an empty map
// without error, even in the stub implementation. This is an
// optimization: no SSM call is needed when there are no keys to resolve.
func TestSSMProviderEmptyKeysReturnsEmptyMap(t *testing.T) {
	provider := NewSSMProvider("us-east-1")
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
}

// TestSSMProviderNilKeysReturnsEmptyMap verifies that calling
// GetParametersBatch with nil keys returns an empty map without error.
func TestSSMProviderNilKeysReturnsEmptyMap(t *testing.T) {
	provider := NewSSMProvider("us-east-1")
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

// TestNewSSMProviderStoresRegion verifies that the constructor correctly
// stores the provided region.
func TestNewSSMProviderStoresRegion(t *testing.T) {
	provider := NewSSMProvider("eu-west-1")
	if provider.region != "eu-west-1" {
		t.Errorf("provider.region = %q, want %q", provider.region, "eu-west-1")
	}
}

// TestSSMProviderContextCancellation verifies that the stub implementation
// returns an error even when the context is cancelled. The real
// implementation should respect context cancellation.
func TestSSMProviderContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	provider := NewSSMProvider("us-east-1")
	_, err := provider.GetParametersBatch(ctx, []string{"/dev/watchpoint/test"})
	if err == nil {
		t.Fatal("expected error from stub SSMProvider with cancelled context, got nil")
	}
}
