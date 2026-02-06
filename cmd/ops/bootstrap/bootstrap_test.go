package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ssm"
	ssmtypes "github.com/aws/aws-sdk-go-v2/service/ssm/types"
)

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// mockGetParameterExisting returns a function that checks a set of existing
// parameter paths and returns ParameterNotFound for missing ones.
func mockGetParameterExisting(existing map[string]bool) func(context.Context, *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
	return func(_ context.Context, input *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
		path := aws.ToString(input.Name)
		if existing[path] {
			return &ssm.GetParameterOutput{
				Parameter: &ssmtypes.Parameter{
					Name:  aws.String(path),
					Value: aws.String("***"),
				},
			}, nil
		}
		return nil, &ssmtypes.ParameterNotFound{Message: aws.String("not found")}
	}
}

// newBootstrapTestRunner creates a BootstrapRunner with mock SSM and injected
// stdin content. The validator uses nil HTTP/DB deps (no network calls).
func newBootstrapTestRunner(mock *mockSSMClient, stdin string) (*BootstrapRunner, *bytes.Buffer) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stderr := &bytes.Buffer{}

	ssmMgr := NewSSMManagerWithClient(mock, "dev", logger)
	validator := NewValidatorWithDeps(nil, nil)

	return &BootstrapRunner{
		SSM:       ssmMgr,
		Validator: validator,
		Stdin:     strings.NewReader(stdin),
		Stderr:    stderr,
	}, stderr
}

// newTestRunnerWithSimpleValidation creates a runner where all prompted steps
// use always-valid validators, so we don't need real network connectivity.
// It also pre-fills stdin with test values for all prompted steps.
func newTestRunnerWithSimpleValidation(mock *mockSSMClient) (*BootstrapRunner, *bytes.Buffer) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	stderr := &bytes.Buffer{}

	ssmMgr := NewSSMManagerWithClient(mock, "dev", logger)
	validator := NewValidatorWithDeps(nil, nil)

	// Build inventory with overridden validators that always pass.
	alwaysValid := func(_ context.Context, _ string) ValidationResult {
		return ValidationResult{Valid: true, Message: "test-accepted"}
	}

	inventory := BuildInventory(validator)
	for i := range inventory {
		if inventory[i].ValidateFn != nil {
			inventory[i].ValidateFn = alwaysValid
		}
	}

	// Build stdin with test values for all prompted steps.
	var inputs []string
	for _, step := range inventory {
		if step.Source == SourcePrompt {
			if step.IsSecret {
				inputs = append(inputs, "test-secret-value-1234567890")
			} else {
				inputs = append(inputs, "test-public-value-1234567890")
			}
		}
	}

	runner := &BootstrapRunner{
		SSM:               ssmMgr,
		Validator:         validator,
		Stdin:             strings.NewReader(strings.Join(inputs, "\n") + "\n"),
		Stderr:            stderr,
		inventoryOverride: inventory,
	}

	return runner, stderr
}

// ---------------------------------------------------------------------------
// BuildInventory tests
// ---------------------------------------------------------------------------

func TestBuildInventory_ReturnsCorrectCount(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	// DB URL, Stripe Secret, Stripe Pub, SendGrid, RunPod Key,
	// Google Client ID, Google Secret, GitHub Client ID, GitHub Secret,
	// Session Key (generated), Admin API Key (generated), RunPod Endpoint ID (fixed)
	expectedCount := 12
	if len(inventory) != expectedCount {
		t.Errorf("inventory count = %d, want %d", len(inventory), expectedCount)
		for i, step := range inventory {
			t.Logf("  [%d] %s (%s)", i, step.HumanLabel, step.SSMCategoryKey)
		}
	}
}

func TestBuildInventory_SSMPaths(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	expectedPaths := map[string]bool{
		"database/url":                   true,
		"billing/stripe_secret_key":      true,
		"billing/stripe_publishable_key": true,
		"email/sendgrid_api_key":         true,
		"forecast/runpod_api_key":        true,
		"auth/google_client_id":          true,
		"auth/google_secret":             true,
		"auth/github_client_id":          true,
		"auth/github_secret":             true,
		"auth/session_key":               true,
		"security/admin_api_key":         true,
		"forecast/runpod_endpoint_id":    true,
	}

	for _, step := range inventory {
		if !expectedPaths[step.SSMCategoryKey] {
			t.Errorf("unexpected SSM path in inventory: %s", step.SSMCategoryKey)
		}
		delete(expectedPaths, step.SSMCategoryKey)
	}

	for path := range expectedPaths {
		t.Errorf("missing expected SSM path: %s", path)
	}
}

func TestBuildInventory_ParameterTypes(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	expectedTypes := map[string]ParameterType{
		"database/url":                   ParamSecureString,
		"billing/stripe_secret_key":      ParamSecureString,
		"billing/stripe_publishable_key": ParamString,
		"email/sendgrid_api_key":         ParamSecureString,
		"forecast/runpod_api_key":        ParamSecureString,
		"auth/google_client_id":          ParamString,
		"auth/google_secret":             ParamSecureString,
		"auth/github_client_id":          ParamString,
		"auth/github_secret":             ParamSecureString,
		"auth/session_key":               ParamSecureString,
		"security/admin_api_key":         ParamSecureString,
		"forecast/runpod_endpoint_id":    ParamString,
	}

	for _, step := range inventory {
		expected, ok := expectedTypes[step.SSMCategoryKey]
		if !ok {
			continue
		}
		if step.ParamType != expected {
			t.Errorf("step %q: ParamType = %v, want %v", step.SSMCategoryKey, step.ParamType, expected)
		}
	}
}

func TestBuildInventory_SourceTypes(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	expectedSources := map[string]InputSource{
		"database/url":                   SourcePrompt,
		"billing/stripe_secret_key":      SourcePrompt,
		"billing/stripe_publishable_key": SourcePrompt,
		"email/sendgrid_api_key":         SourcePrompt,
		"forecast/runpod_api_key":        SourcePrompt,
		"auth/google_client_id":          SourcePrompt,
		"auth/google_secret":             SourcePrompt,
		"auth/github_client_id":          SourcePrompt,
		"auth/github_secret":             SourcePrompt,
		"auth/session_key":               SourceGenerated,
		"security/admin_api_key":         SourceGenerated,
		"forecast/runpod_endpoint_id":    SourceFixed,
	}

	for _, step := range inventory {
		expected, ok := expectedSources[step.SSMCategoryKey]
		if !ok {
			continue
		}
		if step.Source != expected {
			t.Errorf("step %q: Source = %v, want %v", step.SSMCategoryKey, step.Source, expected)
		}
	}
}

func TestBuildInventory_RunPodEndpointHasFixedValue(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	for _, step := range inventory {
		if step.SSMCategoryKey == "forecast/runpod_endpoint_id" {
			if step.FixedValue != "pending_setup" {
				t.Errorf("RunPod Endpoint ID FixedValue = %q, want %q", step.FixedValue, "pending_setup")
			}
			return
		}
	}
	t.Error("RunPod Endpoint ID step not found in inventory")
}

func TestBuildInventory_GeneratedStepsHaveNoPrompt(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	for _, step := range inventory {
		if step.Source == SourceGenerated && step.Prompt != "" {
			t.Errorf("generated step %q should not have a prompt, got %q", step.HumanLabel, step.Prompt)
		}
	}
}

func TestBuildInventory_SecretFlagsCorrect(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	for _, step := range inventory {
		if step.Source == SourcePrompt && step.ParamType == ParamSecureString {
			if !step.IsSecret {
				t.Errorf("step %q is SecureString+Prompt but IsSecret=false", step.HumanLabel)
			}
		}
	}
}

func TestBuildInventory_OrderMatchesSpec(t *testing.T) {
	v := NewValidatorWithDeps(nil, nil)
	inventory := BuildInventory(v)

	// The spec defines External Accounts first, then Internal Secrets, then placeholders.
	// Verify the phases appear in the correct order.
	var phases []string
	lastPhase := ""
	for _, step := range inventory {
		if step.Phase != lastPhase {
			phases = append(phases, step.Phase)
			lastPhase = step.Phase
		}
	}

	expectedPhases := []string{"External Accounts", "Internal Secrets", "Infrastructure Placeholders"}
	if len(phases) != len(expectedPhases) {
		t.Fatalf("phase order = %v, want %v", phases, expectedPhases)
	}
	for i, p := range phases {
		if p != expectedPhases[i] {
			t.Errorf("phase[%d] = %q, want %q", i, p, expectedPhases[i])
		}
	}
}

// ---------------------------------------------------------------------------
// processStep tests
// ---------------------------------------------------------------------------

func TestProcessStep_NewParameterWritten(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamSecureString,
		Source:         SourcePrompt,
		Prompt:         "Enter test key:",
		IsSecret:       true,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			return ValidationResult{Valid: true, Message: "ok"}
		},
	}

	runner, _ := newBootstrapTestRunner(mock, "my-secret-value\n")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "written" {
		t.Errorf("action = %q, want %q", result.Action, "written")
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if aws.ToString(call.Name) != "/dev/watchpoint/test/key" {
		t.Errorf("put path = %q, want %q", aws.ToString(call.Name), "/dev/watchpoint/test/key")
	}
	if aws.ToString(call.Value) != "my-secret-value" {
		t.Errorf("put value = %q, want %q", aws.ToString(call.Value), "my-secret-value")
	}
	if call.Type != ssmtypes.ParameterTypeSecureString {
		t.Errorf("put type = %v, want SecureString", call.Type)
	}
	if aws.ToBool(call.Overwrite) {
		t.Error("overwrite should be false for new parameter")
	}
}

func TestProcessStep_ExistingParameterSkipped(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{
			"/dev/watchpoint/test/key": true,
		}),
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamSecureString,
		Source:         SourcePrompt,
	}

	// User chooses "skip".
	runner, _ := newBootstrapTestRunner(mock, "s\n")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "skipped" {
		t.Errorf("action = %q, want %q", result.Action, "skipped")
	}

	if len(mock.putCalls) != 0 {
		t.Errorf("expected no put calls when skipping, got %d", len(mock.putCalls))
	}
}

func TestProcessStep_ExistingParameterOverwritten(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{
			"/dev/watchpoint/test/key": true,
		}),
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamSecureString,
		Source:         SourcePrompt,
		Prompt:         "Enter value:",
		IsSecret:       true,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			return ValidationResult{Valid: true, Message: "ok"}
		},
	}

	// User chooses "overwrite" then provides a new value.
	runner, _ := newBootstrapTestRunner(mock, "o\nnew-secret-value\n")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "overwritten" {
		t.Errorf("action = %q, want %q", result.Action, "overwritten")
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if !aws.ToBool(call.Overwrite) {
		t.Error("overwrite should be true for existing parameter")
	}
	if aws.ToString(call.Value) != "new-secret-value" {
		t.Errorf("put value = %q, want %q", aws.ToString(call.Value), "new-secret-value")
	}
}

func TestProcessStep_GeneratedParameter(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	step := BootstrapStep{
		HumanLabel:     "Session Key",
		SSMCategoryKey: "auth/session_key",
		ParamType:      ParamSecureString,
		Source:         SourceGenerated,
	}

	runner, _ := newBootstrapTestRunner(mock, "")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "generated" {
		t.Errorf("action = %q, want %q", result.Action, "generated")
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if aws.ToString(call.Name) != "/dev/watchpoint/auth/session_key" {
		t.Errorf("put path = %q, want %q", aws.ToString(call.Name), "/dev/watchpoint/auth/session_key")
	}
	// The value should be a 64-char hex string.
	if len(aws.ToString(call.Value)) != 64 {
		t.Errorf("generated value length = %d, want 64", len(aws.ToString(call.Value)))
	}
	if call.Type != ssmtypes.ParameterTypeSecureString {
		t.Errorf("put type = %v, want SecureString", call.Type)
	}
}

func TestProcessStep_FixedParameter(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	step := BootstrapStep{
		HumanLabel:     "RunPod Endpoint ID",
		SSMCategoryKey: "forecast/runpod_endpoint_id",
		ParamType:      ParamString,
		Source:         SourceFixed,
		FixedValue:     "pending_setup",
	}

	runner, _ := newBootstrapTestRunner(mock, "")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "written" {
		t.Errorf("action = %q, want %q", result.Action, "written")
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}

	call := mock.putCalls[0]
	if aws.ToString(call.Value) != "pending_setup" {
		t.Errorf("put value = %q, want %q", aws.ToString(call.Value), "pending_setup")
	}
	if call.Type != ssmtypes.ParameterTypeString {
		t.Errorf("put type = %v, want String", call.Type)
	}
}

func TestProcessStep_ValidationRetry(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	callCount := 0
	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamString,
		Source:         SourcePrompt,
		Prompt:         "Enter value:",
		IsSecret:       false,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			callCount++
			if callCount < 3 {
				return ValidationResult{Valid: false, Message: "invalid"}
			}
			return ValidationResult{Valid: true, Message: "ok"}
		},
	}

	// First two inputs fail validation, third succeeds.
	runner, _ := newBootstrapTestRunner(mock, "bad1\nbad2\ngood\n")

	result, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if result.Action != "written" {
		t.Errorf("action = %q, want %q", result.Action, "written")
	}

	if callCount != 3 {
		t.Errorf("validation called %d times, want 3", callCount)
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}
	if aws.ToString(mock.putCalls[0].Value) != "good" {
		t.Errorf("put value = %q, want %q", aws.ToString(mock.putCalls[0].Value), "good")
	}
}

func TestProcessStep_MaxRetriesExceeded(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamString,
		Source:         SourcePrompt,
		Prompt:         "Enter value:",
		IsSecret:       false,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			return ValidationResult{Valid: false, Message: "always invalid"}
		},
	}

	// Provide maxRetries worth of bad inputs.
	inputs := ""
	for i := 0; i < maxRetries; i++ {
		inputs += fmt.Sprintf("bad%d\n", i)
	}

	runner, _ := newBootstrapTestRunner(mock, inputs)

	_, err := runner.processStep(context.Background(), step)
	if err == nil {
		t.Fatal("expected error for exceeded retries")
	}
	if !strings.Contains(err.Error(), "maximum retries") {
		t.Errorf("error = %q, want to contain 'maximum retries'", err.Error())
	}
}

func TestProcessStep_EmptyInputRetries(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamString,
		Source:         SourcePrompt,
		Prompt:         "Enter value:",
		IsSecret:       false,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			return ValidationResult{Valid: true, Message: "ok"}
		},
	}

	// First input is empty, second is valid.
	runner, _ := newBootstrapTestRunner(mock, "\nvalid-input\n")

	_, err := runner.processStep(context.Background(), step)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(mock.putCalls) != 1 {
		t.Fatalf("expected 1 put call, got %d", len(mock.putCalls))
	}
	if aws.ToString(mock.putCalls[0].Value) != "valid-input" {
		t.Errorf("put value = %q, want %q", aws.ToString(mock.putCalls[0].Value), "valid-input")
	}
}

func TestProcessStep_SSMCheckError(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: func(_ context.Context, _ *ssm.GetParameterInput) (*ssm.GetParameterOutput, error) {
			return nil, fmt.Errorf("IAM permission denied")
		},
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamString,
		Source:         SourcePrompt,
	}

	runner, _ := newBootstrapTestRunner(mock, "")

	_, err := runner.processStep(context.Background(), step)
	if err == nil {
		t.Fatal("expected error when SSM check fails")
	}
	if !strings.Contains(err.Error(), "checking existence") {
		t.Errorf("error = %q, want to contain 'checking existence'", err.Error())
	}
}

func TestProcessStep_SSMWriteError(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
		putParameterFn: func(_ context.Context, _ *ssm.PutParameterInput) (*ssm.PutParameterOutput, error) {
			return nil, fmt.Errorf("KMS encryption failed")
		},
	}

	step := BootstrapStep{
		HumanLabel:     "Test Key",
		SSMCategoryKey: "test/key",
		ParamType:      ParamSecureString,
		Source:         SourcePrompt,
		Prompt:         "Enter value:",
		IsSecret:       true,
		ValidateFn: func(_ context.Context, _ string) ValidationResult {
			return ValidationResult{Valid: true, Message: "ok"}
		},
	}

	runner, _ := newBootstrapTestRunner(mock, "my-value\n")

	_, err := runner.processStep(context.Background(), step)
	if err == nil {
		t.Fatal("expected error when SSM write fails")
	}
	if !strings.Contains(err.Error(), "writing SSM parameter") {
		t.Errorf("error = %q, want to contain 'writing SSM parameter'", err.Error())
	}
}

// ---------------------------------------------------------------------------
// promptSkipOrOverwrite tests
// ---------------------------------------------------------------------------

func TestPromptSkipOrOverwrite_Skip(t *testing.T) {
	tests := []struct {
		input string
	}{
		{"s\n"},
		{"S\n"},
		{"skip\n"},
		{"Skip\n"},
		{"SKIP\n"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			runner := &BootstrapRunner{
				Stdin:  strings.NewReader(tt.input),
				Stderr: &bytes.Buffer{},
			}

			choice, err := runner.promptSkipOrOverwrite()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if choice != "skip" {
				t.Errorf("choice = %q, want %q", choice, "skip")
			}
		})
	}
}

func TestPromptSkipOrOverwrite_Overwrite(t *testing.T) {
	tests := []struct {
		input string
	}{
		{"o\n"},
		{"O\n"},
		{"overwrite\n"},
		{"Overwrite\n"},
		{"OVERWRITE\n"},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			runner := &BootstrapRunner{
				Stdin:  strings.NewReader(tt.input),
				Stderr: &bytes.Buffer{},
			}

			choice, err := runner.promptSkipOrOverwrite()
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if choice != "overwrite" {
				t.Errorf("choice = %q, want %q", choice, "overwrite")
			}
		})
	}
}

func TestPromptSkipOrOverwrite_InvalidThenValid(t *testing.T) {
	runner := &BootstrapRunner{
		Stdin:  strings.NewReader("x\ninvalid\ns\n"),
		Stderr: &bytes.Buffer{},
	}

	choice, err := runner.promptSkipOrOverwrite()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if choice != "skip" {
		t.Errorf("choice = %q, want %q", choice, "skip")
	}
}

func TestPromptSkipOrOverwrite_EOF(t *testing.T) {
	runner := &BootstrapRunner{
		Stdin:  strings.NewReader(""),
		Stderr: &bytes.Buffer{},
	}

	_, err := runner.promptSkipOrOverwrite()
	if err == nil {
		t.Fatal("expected error on EOF")
	}
}

// ---------------------------------------------------------------------------
// readInput tests
// ---------------------------------------------------------------------------

func TestReadInput_ReadsLine(t *testing.T) {
	runner := &BootstrapRunner{
		Stdin:  strings.NewReader("hello world\n"),
		Stderr: &bytes.Buffer{},
	}

	input, err := runner.readInput("> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input != "hello world" {
		t.Errorf("input = %q, want %q", input, "hello world")
	}
}

func TestReadInput_EOF(t *testing.T) {
	runner := &BootstrapRunner{
		Stdin:  strings.NewReader(""),
		Stderr: &bytes.Buffer{},
	}

	_, err := runner.readInput("> ")
	if err == nil {
		t.Fatal("expected error on EOF")
	}
}

// ---------------------------------------------------------------------------
// readSecretInput tests (non-terminal path)
// ---------------------------------------------------------------------------

func TestReadSecretInput_NonTerminal(t *testing.T) {
	// When stdin is not a terminal (strings.Reader), it falls back to readInput.
	runner := &BootstrapRunner{
		Stdin:  strings.NewReader("secret-value\n"),
		Stderr: &bytes.Buffer{},
	}

	input, err := runner.readSecretInput("> ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if input != "secret-value" {
		t.Errorf("input = %q, want %q", input, "secret-value")
	}
}

// ---------------------------------------------------------------------------
// Full Run integration tests
// ---------------------------------------------------------------------------

func TestRun_AllNewParameters(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr: %s", err, stderr.String())
	}

	// Verify all 12 parameters were written.
	if len(mock.putCalls) != 12 {
		t.Errorf("put calls = %d, want 12", len(mock.putCalls))
		for i, call := range mock.putCalls {
			t.Logf("  [%d] %s", i, aws.ToString(call.Name))
		}
	}

	// Verify specific paths were written.
	paths := make(map[string]bool)
	for _, call := range mock.putCalls {
		paths[aws.ToString(call.Name)] = true
	}

	expectedPaths := []string{
		"/dev/watchpoint/database/url",
		"/dev/watchpoint/billing/stripe_secret_key",
		"/dev/watchpoint/billing/stripe_publishable_key",
		"/dev/watchpoint/email/sendgrid_api_key",
		"/dev/watchpoint/forecast/runpod_api_key",
		"/dev/watchpoint/auth/google_client_id",
		"/dev/watchpoint/auth/google_secret",
		"/dev/watchpoint/auth/github_client_id",
		"/dev/watchpoint/auth/github_secret",
		"/dev/watchpoint/auth/session_key",
		"/dev/watchpoint/security/admin_api_key",
		"/dev/watchpoint/forecast/runpod_endpoint_id",
	}

	for _, path := range expectedPaths {
		if !paths[path] {
			t.Errorf("missing expected SSM write: %s", path)
		}
	}
}

func TestRun_AllParametersExist_AllSkipped(t *testing.T) {
	existing := map[string]bool{
		"/dev/watchpoint/database/url":                   true,
		"/dev/watchpoint/billing/stripe_secret_key":      true,
		"/dev/watchpoint/billing/stripe_publishable_key": true,
		"/dev/watchpoint/email/sendgrid_api_key":         true,
		"/dev/watchpoint/forecast/runpod_api_key":        true,
		"/dev/watchpoint/auth/google_client_id":          true,
		"/dev/watchpoint/auth/google_secret":             true,
		"/dev/watchpoint/auth/github_client_id":          true,
		"/dev/watchpoint/auth/github_secret":             true,
		"/dev/watchpoint/auth/session_key":               true,
		"/dev/watchpoint/security/admin_api_key":         true,
		"/dev/watchpoint/forecast/runpod_endpoint_id":    true,
	}

	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(existing),
	}

	// All 12 steps will ask skip/overwrite -- provide "s" for all.
	skipInputs := strings.Repeat("s\n", 12)

	runner, stderr := newTestRunnerWithSimpleValidation(mock)
	runner.Stdin = strings.NewReader(skipInputs)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr: %s", err, stderr.String())
	}

	// No parameters should have been written.
	if len(mock.putCalls) != 0 {
		t.Errorf("expected no put calls when all skipped, got %d", len(mock.putCalls))
	}
}

func TestRun_SummaryContainsAllParameters(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stderr.String()
	if !strings.Contains(output, "Bootstrap Summary") {
		t.Error("output missing Bootstrap Summary header")
	}
	if !strings.Contains(output, "Total: 12 parameters") {
		t.Errorf("output missing total count, got:\n%s", output)
	}
}

func TestRun_PhaseHeadersPresent(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stderr.String()
	if !strings.Contains(output, "External Accounts") {
		t.Error("output missing 'External Accounts' phase header")
	}
	if !strings.Contains(output, "Internal Secrets") {
		t.Error("output missing 'Internal Secrets' phase header")
	}
	if !strings.Contains(output, "Infrastructure Placeholders") {
		t.Error("output missing 'Infrastructure Placeholders' phase header")
	}
}

func TestRun_SecretInputNotEchoed(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stderr.String()

	// The test secret value should NOT appear in stderr output.
	if strings.Contains(output, "test-secret-value-1234567890") {
		t.Error("secret input value was echoed to stderr")
	}
}

func TestRun_NextStepInstructionShown(t *testing.T) {
	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(map[string]bool{}),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stderr.String()
	if !strings.Contains(output, "sam build") {
		t.Error("output missing next step SAM deployment instruction")
	}
}

func TestRun_MixedSkipAndWrite(t *testing.T) {
	// Some parameters exist, others don't.
	existing := map[string]bool{
		"/dev/watchpoint/database/url":              true,
		"/dev/watchpoint/billing/stripe_secret_key": true,
		"/dev/watchpoint/auth/session_key":          true,
	}

	mock := &mockSSMClient{
		getParameterFn: mockGetParameterExisting(existing),
	}

	runner, stderr := newTestRunnerWithSimpleValidation(mock)

	// Build stdin: skip for existing, values for new.
	// Order: DB URL (exists->skip), Stripe Secret (exists->skip), Stripe Pub (new->value),
	// SendGrid (new->value), RunPod Key (new->value), Google ID (new->value),
	// Google Secret (new->value), GitHub ID (new->value), GitHub Secret (new->value),
	// Session Key (exists->skip), Admin API Key (new->generated), RunPod Endpoint (new->fixed)
	var inputLines []string
	inventory := runner.inventoryOverride
	for _, step := range inventory {
		path := runner.SSM.SSMPath(step.SSMCategoryKey)
		if existing[path] {
			inputLines = append(inputLines, "s") // skip
		} else if step.Source == SourcePrompt {
			if step.IsSecret {
				inputLines = append(inputLines, "test-secret-value-1234567890")
			} else {
				inputLines = append(inputLines, "test-public-value-1234567890")
			}
		}
		// Generated and Fixed don't need stdin input
	}
	runner.Stdin = strings.NewReader(strings.Join(inputLines, "\n") + "\n")

	err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v\nstderr: %s", err, stderr.String())
	}

	// 3 skipped, 9 written/generated.
	// Actually: 3 skipped (DB, Stripe Secret, Session Key)
	// 7 prompted (Stripe Pub, SendGrid, RunPod Key, Google ID, Google Secret, GitHub ID, GitHub Secret)
	// 1 generated (Admin API Key)
	// 1 fixed (RunPod Endpoint ID)
	// Total: 9 put calls
	if len(mock.putCalls) != 9 {
		t.Errorf("put calls = %d, want 9", len(mock.putCalls))
		for i, call := range mock.putCalls {
			t.Logf("  [%d] %s", i, aws.ToString(call.Name))
		}
	}
}

// ---------------------------------------------------------------------------
// Constant tests
// ---------------------------------------------------------------------------

func TestMaxRetries(t *testing.T) {
	if maxRetries < 3 {
		t.Errorf("maxRetries = %d, should be at least 3 to give the operator a fair chance", maxRetries)
	}
	if maxRetries > 10 {
		t.Errorf("maxRetries = %d, should not exceed 10 to avoid infinite loops", maxRetries)
	}
}
