package webhook

import (
	"strings"
)

// PlatformRegistry manages the mapping of webhook URLs to platform-specific
// formatters. It supports both URL pattern-based auto-detection and explicit
// platform_override configuration.
//
// Architecture reference: 08c-webhook-worker.md Section 3.2
type PlatformRegistry struct {
	formatters map[Platform]PlatformFormatter
}

// NewPlatformRegistry creates a PlatformRegistry with all built-in formatters.
func NewPlatformRegistry() *PlatformRegistry {
	r := &PlatformRegistry{
		formatters: make(map[Platform]PlatformFormatter),
	}

	// Register all built-in platform formatters.
	r.formatters[PlatformSlack] = &SlackFormatter{}
	r.formatters[PlatformTeams] = &TeamsFormatter{}
	r.formatters[PlatformDiscord] = &DiscordFormatter{}
	r.formatters[PlatformGoogleChat] = &GoogleChatFormatter{}
	r.formatters[PlatformGeneric] = &GenericFormatter{}

	return r
}

// Detect inspects the URL string and channel config to determine the target
// Platform for formatting.
//
// Detection logic (priority order):
//  1. Check config["platform_override"]. If present and valid, use that platform.
//  2. Fallback: Inspect URL patterns:
//     - "hooks.slack.com" -> PlatformSlack
//     - "discord.com/api/webhooks" -> PlatformDiscord
//     - ".webhook.office.com" OR ".logic.azure.com" -> PlatformTeams
//     - "chat.googleapis.com" -> PlatformGoogleChat
//  3. Fallback: Use PlatformGeneric.
func (r *PlatformRegistry) Detect(url string, config map[string]any) Platform {
	// 1. Check for explicit platform override in channel config.
	if config != nil {
		if override, ok := config["platform_override"].(string); ok && override != "" {
			p := Platform(override)
			if _, exists := r.formatters[p]; exists {
				return p
			}
		}
	}

	// 2. URL pattern matching.
	lowerURL := strings.ToLower(url)

	if strings.Contains(lowerURL, "hooks.slack.com") {
		return PlatformSlack
	}
	if strings.Contains(lowerURL, "discord.com/api/webhooks") {
		return PlatformDiscord
	}
	if strings.Contains(lowerURL, ".webhook.office.com") || strings.Contains(lowerURL, ".logic.azure.com") {
		return PlatformTeams
	}
	if strings.Contains(lowerURL, "chat.googleapis.com") {
		return PlatformGoogleChat
	}

	// 3. Default to generic.
	return PlatformGeneric
}

// Get returns the PlatformFormatter for the given platform.
// Returns the GenericFormatter if the platform is not registered.
func (r *PlatformRegistry) Get(p Platform) PlatformFormatter {
	if f, ok := r.formatters[p]; ok {
		return f
	}
	return r.formatters[PlatformGeneric]
}

// CheckDeprecation examines a webhook URL for platform-specific deprecation
// patterns. Returns a warning message and deprecation status based on known
// platform lifecycle information.
//
// Deprecation Patterns:
//   - Legacy Teams Connectors (*.webhook.office.com): "Teams Connectors are
//     retiring December 2025. Migrate to Power Automate Workflows
//     (*.logic.azure.com)."
//
// Usage: Called by the API layer (GET /watchpoints/{id}) to populate response
// Meta.Warnings for proactive user notification. Also usable during
// Create/Update validation to warn about deprecated configurations.
func (r *PlatformRegistry) CheckDeprecation(url string) (warning string, isDeprecated bool) {
	// Legacy Teams Office 365 Connectors (retiring December 2025).
	if strings.Contains(url, ".webhook.office.com") {
		return "Teams Connectors are retiring December 2025. Migrate to Power Automate Workflows.", true
	}

	// Additional deprecation checks can be added as platforms announce changes.
	return "", false
}
