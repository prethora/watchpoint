package webhook

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPlatformRegistry_Detect_URLPatterns(t *testing.T) {
	registry := NewPlatformRegistry()

	tests := []struct {
		name     string
		url      string
		config   map[string]any
		expected Platform
	}{
		// Slack
		{
			name:     "Slack incoming webhook",
			url:      "https://hooks.slack.com/services/T00000000/B00000000/XXXXXXXXXXXXXXXXXXXXXXXX",
			expected: PlatformSlack,
		},
		{
			name:     "Slack webhook case insensitive",
			url:      "https://HOOKS.SLACK.COM/services/T123/B456/secret",
			expected: PlatformSlack,
		},

		// Discord
		{
			name:     "Discord webhook",
			url:      "https://discord.com/api/webhooks/1234567890/token",
			expected: PlatformDiscord,
		},
		{
			name:     "Discord webhook case insensitive",
			url:      "https://DISCORD.COM/API/WEBHOOKS/1234567890/token",
			expected: PlatformDiscord,
		},

		// Teams - Office 365 Connector (deprecated)
		{
			name:     "Teams legacy connector",
			url:      "https://outlook.webhook.office.com/webhookb2/some-guid",
			expected: PlatformTeams,
		},

		// Teams - Power Automate Workflow
		{
			name:     "Teams Power Automate",
			url:      "https://prod-123.westus.logic.azure.com:443/workflows/some-guid",
			expected: PlatformTeams,
		},

		// Google Chat
		{
			name:     "Google Chat webhook",
			url:      "https://chat.googleapis.com/v1/spaces/AAAA/messages?key=xxx&token=yyy",
			expected: PlatformGoogleChat,
		},

		// Generic
		{
			name:     "unknown URL",
			url:      "https://my-server.example.com/webhook",
			expected: PlatformGeneric,
		},
		{
			name:     "empty URL",
			url:      "",
			expected: PlatformGeneric,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := registry.Detect(tt.url, tt.config)
			assert.Equal(t, tt.expected, got)
		})
	}
}

func TestPlatformRegistry_Detect_PlatformOverride(t *testing.T) {
	registry := NewPlatformRegistry()

	// Override takes precedence over URL detection (NOTIF-007 flow).
	t.Run("override to slack for proxy URL", func(t *testing.T) {
		config := map[string]any{"platform_override": "slack"}
		got := registry.Detect("https://my-proxy.com/slack", config)
		assert.Equal(t, PlatformSlack, got)
	})

	t.Run("override to teams", func(t *testing.T) {
		config := map[string]any{"platform_override": "teams"}
		got := registry.Detect("https://some-generic-url.com/hook", config)
		assert.Equal(t, PlatformTeams, got)
	})

	t.Run("override to discord", func(t *testing.T) {
		config := map[string]any{"platform_override": "discord"}
		got := registry.Detect("https://some-url.com/hook", config)
		assert.Equal(t, PlatformDiscord, got)
	})

	t.Run("invalid override falls back to URL detection", func(t *testing.T) {
		config := map[string]any{"platform_override": "nonexistent_platform"}
		got := registry.Detect("https://hooks.slack.com/services/T/B/X", config)
		assert.Equal(t, PlatformSlack, got, "should fall back to URL pattern detection")
	})

	t.Run("empty override falls back to URL detection", func(t *testing.T) {
		config := map[string]any{"platform_override": ""}
		got := registry.Detect("https://discord.com/api/webhooks/123/token", config)
		assert.Equal(t, PlatformDiscord, got)
	})

	t.Run("nil config falls back to URL detection", func(t *testing.T) {
		got := registry.Detect("https://hooks.slack.com/services/T/B/X", nil)
		assert.Equal(t, PlatformSlack, got)
	})
}

func TestPlatformRegistry_Get(t *testing.T) {
	registry := NewPlatformRegistry()

	t.Run("returns registered formatter", func(t *testing.T) {
		f := registry.Get(PlatformSlack)
		require.NotNil(t, f)
		assert.Equal(t, PlatformSlack, f.Platform())
	})

	t.Run("returns generic for unknown platform", func(t *testing.T) {
		f := registry.Get(Platform("unknown"))
		require.NotNil(t, f)
		assert.Equal(t, PlatformGeneric, f.Platform())
	})

	t.Run("all platforms registered", func(t *testing.T) {
		platforms := []Platform{
			PlatformSlack,
			PlatformTeams,
			PlatformDiscord,
			PlatformGoogleChat,
			PlatformGeneric,
		}
		for _, p := range platforms {
			f := registry.Get(p)
			require.NotNil(t, f, "formatter for %s should not be nil", p)
			assert.Equal(t, p, f.Platform())
		}
	})
}

func TestPlatformRegistry_CheckDeprecation(t *testing.T) {
	registry := NewPlatformRegistry()

	tests := []struct {
		name         string
		url          string
		wantWarning  bool
		wantContains string
	}{
		{
			name:         "deprecated Office 365 connector",
			url:          "https://outlook.webhook.office.com/webhookb2/some-guid",
			wantWarning:  true,
			wantContains: "retiring December 2025",
		},
		{
			name:         "another deprecated connector URL",
			url:          "https://company.webhook.office.com/webhookb2/guid-here",
			wantWarning:  true,
			wantContains: "Migrate to Power Automate",
		},
		{
			name:        "Power Automate Workflow (not deprecated)",
			url:         "https://prod-123.westus.logic.azure.com:443/workflows/guid",
			wantWarning: false,
		},
		{
			name:        "Slack (not deprecated)",
			url:         "https://hooks.slack.com/services/T/B/X",
			wantWarning: false,
		},
		{
			name:        "generic URL (not deprecated)",
			url:         "https://my-server.com/webhook",
			wantWarning: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warning, isDeprecated := registry.CheckDeprecation(tt.url)
			assert.Equal(t, tt.wantWarning, isDeprecated)
			if tt.wantWarning {
				assert.NotEmpty(t, warning)
				assert.Contains(t, warning, tt.wantContains)
			} else {
				assert.Empty(t, warning)
			}
		})
	}
}
