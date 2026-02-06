package webhook

import (
	"context"

	"watchpoint/internal/types"
)

// Platform identifies a webhook destination platform.
type Platform string

const (
	// PlatformGeneric is the default platform for unknown webhook URLs.
	PlatformGeneric Platform = "generic"

	// PlatformSlack represents Slack incoming webhooks.
	PlatformSlack Platform = "slack"

	// PlatformDiscord represents Discord webhook endpoints.
	PlatformDiscord Platform = "discord"

	// PlatformTeams represents Microsoft Teams Power Automate Workflows.
	PlatformTeams Platform = "teams"

	// PlatformGoogleChat represents Google Chat webhook endpoints.
	PlatformGoogleChat Platform = "google_chat"
)

// PlatformFormatter transforms a Notification into platform-specific JSON.
type PlatformFormatter interface {
	// Format transforms the core Notification into the platform-specific JSON payload.
	// The config map is passed to extract platform-specific overrides if needed.
	Format(ctx context.Context, n *types.Notification, config map[string]any) ([]byte, error)

	// Platform returns the enum identifier for metrics.
	Platform() Platform

	// ValidateResponse interprets the HTTP response body to catch "soft failures"
	// (e.g., Slack returning HTTP 200 with "ok": false).
	ValidateResponse(statusCode int, body []byte) error
}

// --- Slack Payload Types (Block Kit) ---
// Architecture reference: 08c-webhook-worker.md Section 4.1

// SlackPayload is the top-level structure for Slack Block Kit messages.
type SlackPayload struct {
	Text   string       `json:"text"`   // Fallback text for push notifications
	Blocks []SlackBlock `json:"blocks"` // Rich layout
}

// SlackBlock represents a single block in a Slack Block Kit message.
type SlackBlock struct {
	Type     string       `json:"type"`               // "section", "header", "context"
	Text     *SlackText   `json:"text,omitempty"`      // Primary text element
	Fields   []*SlackText `json:"fields,omitempty"`    // Multi-column fields
	Elements []*SlackText `json:"elements,omitempty"`  // Context elements
}

// SlackText is a text composition object for Slack Block Kit.
type SlackText struct {
	Type string `json:"type"` // "plain_text", "mrkdwn"
	Text string `json:"text"` // Actual text content
}

// --- Microsoft Teams Payload Types (Adaptive Cards) ---
// Architecture reference: 08c-webhook-worker.md Section 4.2

// TeamsPayload is the top-level structure for Teams Power Automate messages.
type TeamsPayload struct {
	Type        string            `json:"type"` // "message"
	Attachments []TeamsAttachment `json:"attachments"`
}

// TeamsAttachment wraps an Adaptive Card for Teams delivery.
type TeamsAttachment struct {
	ContentType string       `json:"contentType"` // "application/vnd.microsoft.card.adaptive"
	Content     AdaptiveCard `json:"content"`
}

// AdaptiveCard is the Microsoft Adaptive Card structure.
type AdaptiveCard struct {
	Type    string         `json:"type"`    // "AdaptiveCard"
	Version string         `json:"version"` // "1.4"
	Body    []AdaptiveItem `json:"body"`
}

// AdaptiveItem represents an element in the Adaptive Card body.
type AdaptiveItem struct {
	Type   string `json:"type"`             // "TextBlock", "FactSet", etc.
	Text   string `json:"text,omitempty"`   // For TextBlock
	Size   string `json:"size,omitempty"`   // "Large", "Medium", "Small"
	Weight string `json:"weight,omitempty"` // "Bolder", "Lighter"
	Wrap   bool   `json:"wrap,omitempty"`   // Allow text wrapping
	Facts  []Fact `json:"facts,omitempty"`  // For FactSet
}

// Fact is a key-value pair in a Teams FactSet.
type Fact struct {
	Title string `json:"title"`
	Value string `json:"value"`
}

// --- Discord Payload Types (Embeds) ---
// Architecture reference: 08c-webhook-worker.md Section 4.3

// DiscordPayload is the top-level structure for Discord webhook messages.
type DiscordPayload struct {
	Username  string         `json:"username"`
	AvatarURL string         `json:"avatar_url"`
	Content   string         `json:"content"` // Fallback/Ping text
	Embeds    []DiscordEmbed `json:"embeds"`
}

// DiscordEmbed represents an embed in a Discord webhook message.
type DiscordEmbed struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	Color       int            `json:"color"` // Decimal color code
	Fields      []DiscordField `json:"fields"`
	Footer      *DiscordFooter `json:"footer,omitempty"`
}

// DiscordField is a field within a Discord embed.
type DiscordField struct {
	Name   string `json:"name"`
	Value  string `json:"value"`
	Inline bool   `json:"inline,omitempty"`
}

// DiscordFooter is the footer of a Discord embed.
type DiscordFooter struct {
	Text string `json:"text"`
}

// --- Google Chat Payload Types (Cards v2) ---
// Architecture reference: 08c-webhook-worker.md Section 4.4

// GoogleChatPayload is the top-level structure for Google Chat card messages.
type GoogleChatPayload struct {
	Cards []GoogleCard `json:"cards"`
}

// GoogleCard represents a card in a Google Chat message.
type GoogleCard struct {
	Header   GoogleHeader    `json:"header"`
	Sections []GoogleSection `json:"sections"`
}

// GoogleHeader is the header of a Google Chat card.
type GoogleHeader struct {
	Title    string `json:"title"`
	Subtitle string `json:"subtitle,omitempty"`
}

// GoogleSection is a section within a Google Chat card.
type GoogleSection struct {
	Header  string         `json:"header,omitempty"`
	Widgets []GoogleWidget `json:"widgets"`
}

// GoogleWidget is a widget in a Google Chat card section.
type GoogleWidget struct {
	KeyValue *GoogleKeyValue `json:"keyValue,omitempty"`
	TextParagraph *GoogleTextParagraph `json:"textParagraph,omitempty"`
}

// GoogleKeyValue represents a key-value widget.
type GoogleKeyValue struct {
	TopLabel string `json:"topLabel"`
	Content  string `json:"content"`
}

// GoogleTextParagraph represents a text paragraph widget.
type GoogleTextParagraph struct {
	Text string `json:"text"`
}
