package core

import (
	"testing"
	"time"
)

func TestCalculateNextRetry_WebhookPolicy(t *testing.T) {
	// WebhookRetryPolicy: BaseDelay=1s, BackoffFactor=5.0, MaxDelay=30s
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Second},      // 1s * 5^0 = 1s
		{1, 5 * time.Second},      // 1s * 5^1 = 5s
		{2, 25 * time.Second},     // 1s * 5^2 = 25s
		{3, 30 * time.Second},     // 1s * 5^3 = 125s, capped at 30s
	}

	for _, tt := range tests {
		d := CalculateNextRetry(WebhookRetryPolicy, tt.attempt)
		if d != tt.expected {
			t.Errorf("attempt %d: expected %v, got %v", tt.attempt, tt.expected, d)
		}
	}
}

func TestCalculateNextRetry_EmailPolicy(t *testing.T) {
	// EmailRetryPolicy: BaseDelay=1s, BackoffFactor=2.0, MaxDelay=10s
	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 1 * time.Second},  // 1s * 2^0 = 1s
		{1, 2 * time.Second},  // 1s * 2^1 = 2s
		{2, 4 * time.Second},  // 1s * 2^2 = 4s
		{3, 8 * time.Second},  // 1s * 2^3 = 8s
		{4, 10 * time.Second}, // 1s * 2^4 = 16s, capped at 10s
	}

	for _, tt := range tests {
		d := CalculateNextRetry(EmailRetryPolicy, tt.attempt)
		if d != tt.expected {
			t.Errorf("attempt %d: expected %v, got %v", tt.attempt, tt.expected, d)
		}
	}
}

func TestCalculateNextRetry_NegativeAttempt(t *testing.T) {
	// Negative attempt should be treated as 0.
	d := CalculateNextRetry(WebhookRetryPolicy, -1)
	if d != 1*time.Second {
		t.Errorf("expected 1s for negative attempt, got %v", d)
	}
}

func TestCalculateNextRetry_CustomPolicy(t *testing.T) {
	policy := RetryPolicy{
		MaxAttempts:   5,
		BaseDelay:     500 * time.Millisecond,
		MaxDelay:      1 * time.Minute,
		BackoffFactor: 3.0,
	}

	tests := []struct {
		attempt  int
		expected time.Duration
	}{
		{0, 500 * time.Millisecond},              // 500ms * 3^0
		{1, 1500 * time.Millisecond},             // 500ms * 3^1
		{2, 4500 * time.Millisecond},             // 500ms * 3^2
		{3, 13500 * time.Millisecond},            // 500ms * 3^3
		{4, 40500 * time.Millisecond},            // 500ms * 3^4
		{5, 1 * time.Minute},                     // 500ms * 3^5 = 121.5s, capped at 60s
	}

	for _, tt := range tests {
		d := CalculateNextRetry(policy, tt.attempt)
		if d != tt.expected {
			t.Errorf("attempt %d: expected %v, got %v", tt.attempt, tt.expected, d)
		}
	}
}
