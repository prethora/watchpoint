package config

import "testing"

// TestNewBuildInfoDefaults verifies that NewBuildInfo returns the linker-injected
// default values when ldflags have not been set (i.e., during normal test runs).
func TestNewBuildInfoDefaults(t *testing.T) {
	info := NewBuildInfo()

	if info.Version != "dev" {
		t.Errorf("NewBuildInfo().Version = %q, want %q", info.Version, "dev")
	}
	if info.Commit != "none" {
		t.Errorf("NewBuildInfo().Commit = %q, want %q", info.Commit, "none")
	}
	if info.BuildTime != "unknown" {
		t.Errorf("NewBuildInfo().BuildTime = %q, want %q", info.BuildTime, "unknown")
	}
}

// TestNewBuildInfoReturnsStruct verifies that NewBuildInfo returns a value type
// (not a pointer), consistent with how BuildInfo is embedded in Config.
func TestNewBuildInfoReturnsStruct(t *testing.T) {
	info := NewBuildInfo()

	// Verify the returned BuildInfo can be directly assigned to Config.Build
	cfg := Config{Build: info}
	if cfg.Build.Version != "dev" {
		t.Errorf("Config.Build.Version after assignment = %q, want %q", cfg.Build.Version, "dev")
	}
}

// TestLinkerVariablesExist verifies the package-level variables used for linker
// injection have the expected default values. These variables are unexported
// because they are only meant to be set via -ldflags.
func TestLinkerVariablesExist(t *testing.T) {
	// Access the package-level variables directly (same package due to _test.go
	// being in package config, not config_test).
	if version != "dev" {
		t.Errorf("version = %q, want %q", version, "dev")
	}
	if commit != "none" {
		t.Errorf("commit = %q, want %q", commit, "none")
	}
	if buildTime != "unknown" {
		t.Errorf("buildTime = %q, want %q", buildTime, "unknown")
	}
}
