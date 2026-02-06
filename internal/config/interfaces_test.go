package config

import (
	"context"
	"reflect"
	"testing"
)

// mockSecretProvider is a test implementation of SecretProvider that returns
// pre-configured values.
type mockSecretProvider struct {
	values map[string]string
	err    error
}

func (m *mockSecretProvider) GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error) {
	if m.err != nil {
		return nil, m.err
	}
	result := make(map[string]string)
	for _, k := range keys {
		if v, ok := m.values[k]; ok {
			result[k] = v
		}
	}
	return result, nil
}

// TestSecretProviderInterface verifies that the SecretProvider interface has
// the expected method signature.
func TestSecretProviderInterface(t *testing.T) {
	iface := reflect.TypeOf((*SecretProvider)(nil)).Elem()

	if iface.Kind() != reflect.Interface {
		t.Fatalf("SecretProvider is not an interface, got %v", iface.Kind())
	}

	if iface.NumMethod() != 1 {
		t.Fatalf("SecretProvider has %d methods, want 1", iface.NumMethod())
	}

	method, ok := iface.MethodByName("GetParametersBatch")
	if !ok {
		t.Fatal("SecretProvider is missing method GetParametersBatch")
	}

	// Method signature: GetParametersBatch(ctx context.Context, keys []string) (map[string]string, error)
	// In the interface reflection, the method type has no receiver, so:
	// NumIn = 2 (context.Context, []string)
	// NumOut = 2 (map[string]string, error)
	methodType := method.Type
	if methodType.NumIn() != 2 {
		t.Errorf("GetParametersBatch has %d input params, want 2", methodType.NumIn())
	}
	if methodType.NumOut() != 2 {
		t.Errorf("GetParametersBatch has %d output params, want 2", methodType.NumOut())
	}

	// Verify input types
	if methodType.NumIn() >= 1 {
		ctxType := reflect.TypeOf((*context.Context)(nil)).Elem()
		if methodType.In(0) != ctxType {
			t.Errorf("GetParametersBatch param 0 type = %v, want context.Context", methodType.In(0))
		}
	}
	if methodType.NumIn() >= 2 {
		keysType := reflect.TypeOf([]string{})
		if methodType.In(1) != keysType {
			t.Errorf("GetParametersBatch param 1 type = %v, want []string", methodType.In(1))
		}
	}

	// Verify output types
	if methodType.NumOut() >= 1 {
		mapType := reflect.TypeOf(map[string]string{})
		if methodType.Out(0) != mapType {
			t.Errorf("GetParametersBatch return 0 type = %v, want map[string]string", methodType.Out(0))
		}
	}
	if methodType.NumOut() >= 2 {
		errType := reflect.TypeOf((*error)(nil)).Elem()
		if methodType.Out(1) != errType {
			t.Errorf("GetParametersBatch return 1 type = %v, want error", methodType.Out(1))
		}
	}
}

// TestMockSecretProviderSatisfiesInterface verifies that a concrete mock type
// can satisfy the SecretProvider interface, confirming the interface is usable.
func TestMockSecretProviderSatisfiesInterface(t *testing.T) {
	var provider SecretProvider = &mockSecretProvider{
		values: map[string]string{
			"/dev/watchpoint/database/url": "postgres://localhost/test",
		},
	}

	result, err := provider.GetParametersBatch(context.Background(), []string{"/dev/watchpoint/database/url"})
	if err != nil {
		t.Fatalf("GetParametersBatch returned error: %v", err)
	}
	if got := result["/dev/watchpoint/database/url"]; got != "postgres://localhost/test" {
		t.Errorf("result = %q, want %q", got, "postgres://localhost/test")
	}
}

// TestMockSecretProviderMissingKeys verifies that the provider only returns
// values for keys that exist in its store.
func TestMockSecretProviderMissingKeys(t *testing.T) {
	var provider SecretProvider = &mockSecretProvider{
		values: map[string]string{
			"/dev/watchpoint/database/url": "postgres://localhost/test",
		},
	}

	result, err := provider.GetParametersBatch(context.Background(), []string{"/dev/watchpoint/nonexistent"})
	if err != nil {
		t.Fatalf("GetParametersBatch returned error: %v", err)
	}
	if len(result) != 0 {
		t.Errorf("expected empty result for missing key, got %v", result)
	}
}
