package config

import (
	"context"
	"os"
	"testing"
)

// TestEnvVarProviderSatisfiesSecretProvider verifies that EnvVarProvider
// implements the SecretProvider interface at compile time.
func TestEnvVarProviderSatisfiesSecretProvider(t *testing.T) {
	var _ SecretProvider = (*EnvVarProvider)(nil)
	var _ SecretProvider = NewEnvVarProvider()
}

// TestEnvVarProviderReturnsSetVariables verifies that GetParametersBatch
// returns values for environment variables that are set.
func TestEnvVarProviderReturnsSetVariables(t *testing.T) {
	const (
		key1 = "WATCHPOINT_TEST_SECRET_A"
		key2 = "WATCHPOINT_TEST_SECRET_B"
		val1 = "value-alpha"
		val2 = "value-beta"
	)

	t.Setenv(key1, val1)
	t.Setenv(key2, val2)

	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), []string{key1, key2})
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}

	if len(result) != 2 {
		t.Fatalf("expected 2 results, got %d", len(result))
	}
	if got := result[key1]; got != val1 {
		t.Errorf("result[%q] = %q, want %q", key1, got, val1)
	}
	if got := result[key2]; got != val2 {
		t.Errorf("result[%q] = %q, want %q", key2, got, val2)
	}
}

// TestEnvVarProviderOmitsMissingVariables verifies that GetParametersBatch
// silently omits keys that are not set in the environment.
func TestEnvVarProviderOmitsMissingVariables(t *testing.T) {
	// Ensure the variable does not exist
	const missingKey = "WATCHPOINT_TEST_DEFINITELY_NOT_SET_XYZ"
	os.Unsetenv(missingKey)

	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), []string{missingKey})
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}

	if len(result) != 0 {
		t.Errorf("expected empty result for missing key, got %v", result)
	}
}

// TestEnvVarProviderMixedKeys verifies correct behavior when some keys are
// set and others are not.
func TestEnvVarProviderMixedKeys(t *testing.T) {
	const (
		setKey     = "WATCHPOINT_TEST_MIXED_SET"
		missingKey = "WATCHPOINT_TEST_MIXED_MISSING"
		setVal     = "found-value"
	)

	t.Setenv(setKey, setVal)
	os.Unsetenv(missingKey)

	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), []string{setKey, missingKey})
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d: %v", len(result), result)
	}
	if got := result[setKey]; got != setVal {
		t.Errorf("result[%q] = %q, want %q", setKey, got, setVal)
	}
	if _, ok := result[missingKey]; ok {
		t.Errorf("result should not contain missing key %q", missingKey)
	}
}

// TestEnvVarProviderEmptyKeys verifies that an empty keys slice returns
// an empty map without error.
func TestEnvVarProviderEmptyKeys(t *testing.T) {
	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), []string{})
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for empty keys, got %v", result)
	}
}

// TestEnvVarProviderNilKeys verifies that a nil keys slice returns an
// empty map without error.
func TestEnvVarProviderNilKeys(t *testing.T) {
	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}
	if result == nil {
		t.Error("expected non-nil map, got nil")
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for nil keys, got %v", result)
	}
}

// TestEnvVarProviderEmptyValue verifies that an environment variable set
// to an empty string is still returned (it exists, just has empty value).
func TestEnvVarProviderEmptyValue(t *testing.T) {
	const key = "WATCHPOINT_TEST_EMPTY_VALUE"
	t.Setenv(key, "")

	provider := NewEnvVarProvider()
	result, err := provider.GetParametersBatch(context.Background(), []string{key})
	if err != nil {
		t.Fatalf("GetParametersBatch returned unexpected error: %v", err)
	}

	if len(result) != 1 {
		t.Fatalf("expected 1 result, got %d", len(result))
	}
	if got, ok := result[key]; !ok {
		t.Error("expected key to be present in result")
	} else if got != "" {
		t.Errorf("result[%q] = %q, want empty string", key, got)
	}
}
