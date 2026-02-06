package core

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"watchpoint/internal/config"
)

// --- Mock Health Probe ---

// mockHealthProbe implements HealthProbe for testing.
type mockHealthProbe struct {
	name     string
	checkErr error
	// delay simulates slow subsystems; Check blocks for this duration.
	delay time.Duration
	// checkFunc allows dynamic behavior per-call (overrides checkErr).
	checkFunc func(ctx context.Context) error
	// called tracks whether Check was invoked.
	called atomic.Bool
}

func (m *mockHealthProbe) Name() string { return m.name }

func (m *mockHealthProbe) Check(ctx context.Context) error {
	m.called.Store(true)
	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.checkFunc != nil {
		return m.checkFunc(ctx)
	}
	return m.checkErr
}

// --- Helper ---

func newTestServerForHealth(probes []HealthProbe) *Server {
	cfg := &config.Config{Environment: "local"}
	logger := slog.Default()
	srv, _ := NewServer(cfg, &mockRepositoryRegistry{}, logger)
	srv.HealthProbes = probes
	return srv
}

// --- Tests ---

func TestHandleHealth_AllHealthy(t *testing.T) {
	probes := []HealthProbe{
		&mockHealthProbe{name: "database"},
		&mockHealthProbe{name: "s3"},
		&mockHealthProbe{name: "sqs"},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}

	for _, name := range []string{"database", "s3", "sqs"} {
		comp, ok := resp.Components[name]
		if !ok {
			t.Errorf("expected component %q in response", name)
			continue
		}
		if comp.Status != "healthy" {
			t.Errorf("component %q: expected 'healthy', got %q", name, comp.Status)
		}
		if comp.Message != "" {
			t.Errorf("component %q: expected empty message, got %q", name, comp.Message)
		}
	}
}

func TestHandleHealth_OneUnhealthy(t *testing.T) {
	probes := []HealthProbe{
		&mockHealthProbe{name: "database"},
		&mockHealthProbe{name: "s3", checkErr: errors.New("bucket not found")},
		&mockHealthProbe{name: "sqs"},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "unhealthy" {
		t.Errorf("expected status 'unhealthy', got %q", resp.Status)
	}

	// Database and SQS should be healthy.
	for _, name := range []string{"database", "sqs"} {
		comp, ok := resp.Components[name]
		if !ok {
			t.Errorf("expected component %q in response", name)
			continue
		}
		if comp.Status != "healthy" {
			t.Errorf("component %q: expected 'healthy', got %q", name, comp.Status)
		}
	}

	// S3 should be unhealthy with the error message.
	s3Comp, ok := resp.Components["s3"]
	if !ok {
		t.Fatal("expected 's3' component in response")
	}
	if s3Comp.Status != "unhealthy" {
		t.Errorf("s3 component: expected 'unhealthy', got %q", s3Comp.Status)
	}
	if s3Comp.Message != "bucket not found" {
		t.Errorf("s3 component: expected message 'bucket not found', got %q", s3Comp.Message)
	}
}

func TestHandleHealth_AllUnhealthy(t *testing.T) {
	probes := []HealthProbe{
		&mockHealthProbe{name: "database", checkErr: errors.New("connection refused")},
		&mockHealthProbe{name: "s3", checkErr: errors.New("access denied")},
		&mockHealthProbe{name: "sqs", checkErr: errors.New("queue not found")},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "unhealthy" {
		t.Errorf("expected status 'unhealthy', got %q", resp.Status)
	}

	for _, name := range []string{"database", "s3", "sqs"} {
		comp, ok := resp.Components[name]
		if !ok {
			t.Errorf("expected component %q in response", name)
			continue
		}
		if comp.Status != "unhealthy" {
			t.Errorf("component %q: expected 'unhealthy', got %q", name, comp.Status)
		}
		if comp.Message == "" {
			t.Errorf("component %q: expected non-empty error message", name)
		}
	}
}

func TestHandleHealth_Timeout(t *testing.T) {
	// One probe blocks longer than the health check timeout.
	probes := []HealthProbe{
		&mockHealthProbe{name: "database"},
		&mockHealthProbe{name: "s3", delay: 5 * time.Second}, // Exceeds 2s timeout.
		&mockHealthProbe{name: "sqs"},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "unhealthy" {
		t.Errorf("expected status 'unhealthy', got %q", resp.Status)
	}

	// S3 should be timed out or report context error.
	s3Comp, ok := resp.Components["s3"]
	if !ok {
		t.Fatal("expected 's3' component in response")
	}
	if s3Comp.Status != "unhealthy" {
		t.Errorf("s3 component: expected 'unhealthy', got %q", s3Comp.Status)
	}
}

func TestHandleHealth_NoProbes(t *testing.T) {
	srv := newTestServerForHealth(nil)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "healthy" {
		t.Errorf("expected status 'healthy', got %q", resp.Status)
	}
}

func TestHandleHealth_ConcurrentExecution(t *testing.T) {
	// Verify probes run concurrently by using probes that each take ~100ms.
	// If sequential, total would be ~300ms; if concurrent, ~100ms.
	const probeDelay = 100 * time.Millisecond

	probes := []HealthProbe{
		&mockHealthProbe{name: "database", delay: probeDelay},
		&mockHealthProbe{name: "s3", delay: probeDelay},
		&mockHealthProbe{name: "sqs", delay: probeDelay},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	start := time.Now()
	srv.HandleHealth(rec, req)
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Errorf("expected status 200, got %d", rec.Code)
	}

	// Allow generous margin but verify it's not sequential.
	maxAllowed := 3 * probeDelay // Sequential would take 3x the delay.
	if elapsed >= maxAllowed {
		t.Errorf("health check took %v, expected less than %v (probes should run concurrently)", elapsed, maxAllowed)
	}
}

func TestHandleHealth_ContentType(t *testing.T) {
	probes := []HealthProbe{
		&mockHealthProbe{name: "database"},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	contentType := rec.Header().Get("Content-Type")
	if contentType != "application/json" {
		t.Errorf("expected Content-Type 'application/json', got %q", contentType)
	}
}

func TestHandleHealth_ProbeRespectsContextCancellation(t *testing.T) {
	// Verify that a probe with a checkFunc properly receives a cancelled context.
	ctxCancelled := make(chan bool, 1)

	probes := []HealthProbe{
		&mockHealthProbe{
			name: "slow_probe",
			checkFunc: func(ctx context.Context) error {
				select {
				case <-time.After(10 * time.Second):
					ctxCancelled <- false
					return nil
				case <-ctx.Done():
					ctxCancelled <- true
					return ctx.Err()
				}
			},
		},
	}

	srv := newTestServerForHealth(probes)

	// Use a request with an already-short context to force cancellation.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	req := httptest.NewRequest(http.MethodGet, "/health", nil).WithContext(ctx)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	// The probe should have been cancelled.
	select {
	case cancelled := <-ctxCancelled:
		if !cancelled {
			t.Error("probe should have received context cancellation")
		}
	case <-time.After(3 * time.Second):
		t.Error("timed out waiting for probe cancellation signal")
	}
}

func TestHandleHealth_AllProbesCalled(t *testing.T) {
	db := &mockHealthProbe{name: "database"}
	s3 := &mockHealthProbe{name: "s3"}
	sqs := &mockHealthProbe{name: "sqs"}

	probes := []HealthProbe{db, s3, sqs}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	srv.HandleHealth(rec, req)

	if !db.called.Load() {
		t.Error("database probe was not called")
	}
	if !s3.called.Load() {
		t.Error("s3 probe was not called")
	}
	if !sqs.called.Load() {
		t.Error("sqs probe was not called")
	}
}

func TestHandleHealth_ProbePanic(t *testing.T) {
	// A probe that panics should be caught and reported as unhealthy,
	// not crash the entire process.
	probes := []HealthProbe{
		&mockHealthProbe{name: "database"},
		&mockHealthProbe{
			name: "s3",
			checkFunc: func(ctx context.Context) error {
				panic("s3 client nil pointer")
			},
		},
		&mockHealthProbe{name: "sqs"},
	}

	srv := newTestServerForHealth(probes)
	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	rec := httptest.NewRecorder()

	// Should not panic.
	srv.HandleHealth(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("expected status 503, got %d", rec.Code)
	}

	var resp healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("failed to decode response: %v", err)
	}

	if resp.Status != "unhealthy" {
		t.Errorf("expected status 'unhealthy', got %q", resp.Status)
	}

	s3Comp, ok := resp.Components["s3"]
	if !ok {
		t.Fatal("expected 's3' component in response")
	}
	if s3Comp.Status != "unhealthy" {
		t.Errorf("s3 component: expected 'unhealthy', got %q", s3Comp.Status)
	}
	if s3Comp.Message == "" {
		t.Error("s3 component: expected non-empty error message for panicked probe")
	}

	// Other probes should still be healthy.
	for _, name := range []string{"database", "sqs"} {
		comp, ok := resp.Components[name]
		if !ok {
			t.Errorf("expected component %q in response", name)
			continue
		}
		if comp.Status != "healthy" {
			t.Errorf("component %q: expected 'healthy', got %q", name, comp.Status)
		}
	}
}

// --- HealthProbe Interface Conformance ---

func TestMockHealthProbe_ImplementsHealthProbe(t *testing.T) {
	var _ HealthProbe = (*mockHealthProbe)(nil)
}
