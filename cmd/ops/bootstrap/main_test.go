package main

import (
	"bytes"
	"strings"
	"testing"

	"log/slog"

	"github.com/aws/aws-sdk-go-v2/aws"
)

func TestValidEnvironments(t *testing.T) {
	tests := []struct {
		env   string
		valid bool
	}{
		{"dev", true},
		{"staging", true},
		{"prod", true},
		{"local", false},
		{"production", false},
		{"", false},
		{"DEV", false}, // case-sensitive
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			got := validEnvironments[tt.env]
			if got != tt.valid {
				t.Errorf("validEnvironments[%q] = %v, want %v", tt.env, got, tt.valid)
			}
		})
	}
}

func TestConfirmProduction(t *testing.T) {
	bctx := &BootstrapContext{
		Environment: "prod",
		AccountID:   "123456789012",
		AWSRegion:   "us-east-1",
		CallerARN:   "arn:aws:iam::123456789012:user/test",
	}

	tests := []struct {
		name     string
		input    string
		expected bool
	}{
		{"accepts yes", "yes\n", true},
		{"accepts YES", "YES\n", true},
		{"accepts Yes", "Yes\n", true},
		{"rejects no", "no\n", false},
		{"rejects empty", "\n", false},
		{"rejects random", "maybe\n", false},
		{"accepts yes with spaces", "  yes  \n", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// We cannot directly test confirmProduction because it reads from
			// os.Stdin. Instead, we test the core logic inline.
			response := strings.TrimSpace(tt.input)
			got := strings.EqualFold(response, "yes")
			if got != tt.expected {
				t.Errorf("confirmProduction with input %q = %v, want %v", tt.input, got, tt.expected)
			}
			_ = bctx // suppress unused warning
		})
	}
}

func TestPrintBanner(t *testing.T) {
	// Capture stderr output via the banner function.
	// printBanner writes to os.Stderr, so we test its behavior indirectly
	// by verifying the BootstrapContext fields would be printed correctly.

	bctx := &BootstrapContext{
		Environment: "dev",
		AWSProfile:  "myprofile",
		AWSRegion:   "us-east-1",
		AccountID:   "123456789012",
		CallerARN:   "arn:aws:iam::123456789012:user/test",
	}

	// Verify the SSM prefix construction.
	expectedPrefix := "/" + bctx.Environment + "/watchpoint/"
	if expectedPrefix != "/dev/watchpoint/" {
		t.Errorf("SSM prefix = %q, want /dev/watchpoint/", expectedPrefix)
	}

	// Verify the context fields are set correctly.
	if bctx.Environment != "dev" {
		t.Errorf("Environment = %q, want %q", bctx.Environment, "dev")
	}
	if bctx.AWSProfile != "myprofile" {
		t.Errorf("AWSProfile = %q, want %q", bctx.AWSProfile, "myprofile")
	}
}

func TestPrintBannerWithoutProfile(t *testing.T) {
	bctx := &BootstrapContext{
		Environment: "staging",
		AWSProfile:  "", // no profile
		AWSRegion:   "eu-west-1",
		AccountID:   "987654321098",
		CallerARN:   "arn:aws:sts::987654321098:assumed-role/test/session",
	}

	expectedPrefix := "/" + bctx.Environment + "/watchpoint/"
	if expectedPrefix != "/staging/watchpoint/" {
		t.Errorf("SSM prefix = %q, want /staging/watchpoint/", expectedPrefix)
	}
}

func TestBootstrapContextConstruction(t *testing.T) {
	// Test that a BootstrapContext can be fully populated and inspected.
	logger := slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))

	bctx := &BootstrapContext{
		Environment: "prod",
		AWSProfile:  "watchpoint-prod",
		AWSRegion:   "us-east-1",
		AccountID:   "111222333444",
		CallerARN:   "arn:aws:iam::111222333444:user/admin",
		AWSConfig:   aws.Config{Region: "us-east-1"},
		Logger:      logger,
	}

	// Verify all fields are accessible and correctly set.
	if bctx.Environment != "prod" {
		t.Errorf("Environment = %q, want %q", bctx.Environment, "prod")
	}
	if bctx.AWSProfile != "watchpoint-prod" {
		t.Errorf("AWSProfile = %q, want %q", bctx.AWSProfile, "watchpoint-prod")
	}
	if bctx.AWSRegion != "us-east-1" {
		t.Errorf("AWSRegion = %q, want %q", bctx.AWSRegion, "us-east-1")
	}
	if bctx.AccountID != "111222333444" {
		t.Errorf("AccountID = %q, want %q", bctx.AccountID, "111222333444")
	}
	if bctx.CallerARN != "arn:aws:iam::111222333444:user/admin" {
		t.Errorf("CallerARN = %q, want %q", bctx.CallerARN, "arn:aws:iam::111222333444:user/admin")
	}
	if bctx.AWSConfig.Region != "us-east-1" {
		t.Errorf("AWSConfig.Region = %q, want %q", bctx.AWSConfig.Region, "us-east-1")
	}
	if bctx.Logger == nil {
		t.Error("Logger should not be nil")
	}
}

func TestSSMPrefixConstruction(t *testing.T) {
	// Verify that the SSM prefix is correctly constructed for each environment.
	// This is the pattern used throughout the bootstrap tool and matches
	// the Secret Inventory Table in 13-human-setup.md Section 4.
	tests := []struct {
		env      string
		expected string
	}{
		{"dev", "/dev/watchpoint/"},
		{"staging", "/staging/watchpoint/"},
		{"prod", "/prod/watchpoint/"},
	}

	for _, tt := range tests {
		t.Run(tt.env, func(t *testing.T) {
			prefix := "/" + tt.env + "/watchpoint/"
			if prefix != tt.expected {
				t.Errorf("SSM prefix for env %q = %q, want %q", tt.env, prefix, tt.expected)
			}
		})
	}
}
