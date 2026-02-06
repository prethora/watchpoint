package config

// Linker-injected build metadata variables. These are set at compile time via
// -ldflags, for example:
//
//	go build -ldflags "-X watchpoint/internal/config.version=1.2.3 \
//	    -X watchpoint/internal/config.commit=$(git rev-parse --short HEAD) \
//	    -X watchpoint/internal/config.buildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// Default values are used during local development when ldflags are not set.
var (
	version   = "dev"
	commit    = "none"
	buildTime = "unknown"
)

// NewBuildInfo constructs a BuildInfo from the linker-injected global variables.
// This should be called once during initialization to populate the Config.Build field.
func NewBuildInfo() BuildInfo {
	return BuildInfo{
		Version:   version,
		Commit:    commit,
		BuildTime: buildTime,
	}
}
