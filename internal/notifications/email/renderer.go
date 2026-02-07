package email

import (
	"bytes"
	"embed"
	"fmt"
	"html/template"
	texttemplate "text/template"
	"time"

	"watchpoint/internal/types"
)

//go:embed templates/*.html templates/*.txt
var templateFS embed.FS

// RenderedEmail holds the pre-rendered email content ready for transmission.
type RenderedEmail struct {
	Subject  string
	BodyHTML string
	BodyText string
}

// templateData is the struct passed into Go templates for rendering.
type templateData struct {
	Subject           string
	WatchPointName    string
	Location          string
	FormattedDate     string
	TimezoneName      string
	Urgency           string
	ConditionsSummary string
	BouncedAddress    string
	DisabledReason    string
}

// subjectPrefixes maps event types to their email subject line prefix.
var subjectPrefixes = map[types.EventType]string{
	types.EventThresholdCrossed: "Threshold Crossed",
	types.EventThresholdCleared: "Threshold Cleared",
	types.EventForecastChanged:  "Forecast Update",
	types.EventImminentAlert:    "Imminent Alert",
	types.EventDigest:           "Monitor Digest",
	types.EventSystemAlert:      "System Alert",
	types.EventBillingWarning:   "Billing Alert",
	types.EventBillingReceipt:   "Payment Receipt",
}

// Renderer performs client-side email template rendering using Go's
// html/template with embedded template files. It implements TemplateService.
//
// Architecture: replaces the server-side SendGrid Dynamic Templates approach.
// Keeps soft-fail fallback logic (VERT-003/VERT-004): if the requested
// template set is not found, falls back to "default" before erroring.
type Renderer struct {
	htmlTemplates map[types.EventType]*template.Template
	textTemplates map[types.EventType]*texttemplate.Template
	defaultFromAddr string
	defaultFromName string
	logger          types.Logger
}

// RendererConfig holds the parameters needed to construct a Renderer.
type RendererConfig struct {
	DefaultFromAddr string
	DefaultFromName string
	Logger          types.Logger
}

// NewRenderer parses the embedded templates and returns a Renderer.
// Returns an error if any template fails to parse.
func NewRenderer(cfg RendererConfig) (*Renderer, error) {
	r := &Renderer{
		htmlTemplates:   make(map[types.EventType]*template.Template),
		textTemplates:   make(map[types.EventType]*texttemplate.Template),
		defaultFromAddr: cfg.DefaultFromAddr,
		defaultFromName: cfg.DefaultFromName,
		logger:          cfg.Logger,
	}

	// Read the base HTML template.
	baseHTML, err := templateFS.ReadFile("templates/base.html")
	if err != nil {
		return nil, fmt.Errorf("renderer: failed to read base.html: %w", err)
	}

	eventTypes := []types.EventType{
		types.EventThresholdCrossed,
		types.EventThresholdCleared,
		types.EventForecastChanged,
		types.EventImminentAlert,
		types.EventDigest,
		types.EventSystemAlert,
		types.EventBillingWarning,
		types.EventBillingReceipt,
	}

	for _, et := range eventTypes {
		name := string(et)

		// Parse HTML: base + event-specific template.
		htmlContent, err := templateFS.ReadFile(fmt.Sprintf("templates/%s.html", name))
		if err != nil {
			return nil, fmt.Errorf("renderer: failed to read %s.html: %w", name, err)
		}
		htmlTmpl, err := template.New("base").Parse(string(baseHTML))
		if err != nil {
			return nil, fmt.Errorf("renderer: failed to parse base.html: %w", err)
		}
		if _, err := htmlTmpl.Parse(string(htmlContent)); err != nil {
			return nil, fmt.Errorf("renderer: failed to parse %s.html: %w", name, err)
		}
		r.htmlTemplates[et] = htmlTmpl

		// Parse plaintext template.
		txtContent, err := templateFS.ReadFile(fmt.Sprintf("templates/%s.txt", name))
		if err != nil {
			return nil, fmt.Errorf("renderer: failed to read %s.txt: %w", name, err)
		}
		txtTmpl, err := texttemplate.New(name).Parse(string(txtContent))
		if err != nil {
			return nil, fmt.Errorf("renderer: failed to parse %s.txt: %w", name, err)
		}
		r.textTemplates[et] = txtTmpl
	}

	return r, nil
}

// Render implements TemplateService. It renders the notification into a
// RenderedEmail (Subject, BodyHTML, BodyText) and returns the SenderIdentity.
//
// Soft-fail fallback (VERT-003/VERT-004): the "set" parameter is currently
// used only for sender name customization. All event types use the same
// embedded templates. If a template is missing for the event type, an error
// is returned.
func (r *Renderer) Render(set string, eventType types.EventType, n *types.Notification) (*RenderedEmail, types.SenderIdentity, error) {
	if n == nil {
		return nil, types.SenderIdentity{}, fmt.Errorf("renderer: notification is nil")
	}

	htmlTmpl, ok := r.htmlTemplates[eventType]
	if !ok {
		return nil, types.SenderIdentity{}, fmt.Errorf("renderer: no HTML template for event type %q", eventType)
	}
	txtTmpl, ok := r.textTemplates[eventType]
	if !ok {
		return nil, types.SenderIdentity{}, fmt.Errorf("renderer: no text template for event type %q", eventType)
	}

	// Resolve timezone from payload.
	loc := resolveTimezone(n.Payload)

	// Build template data from notification.
	data := r.buildTemplateData(eventType, n, loc)

	// Render HTML.
	var htmlBuf bytes.Buffer
	if err := htmlTmpl.Execute(&htmlBuf, data); err != nil {
		return nil, types.SenderIdentity{}, fmt.Errorf("renderer: failed to render HTML for %q: %w", eventType, err)
	}

	// Render plaintext.
	var txtBuf bytes.Buffer
	if err := txtTmpl.Execute(&txtBuf, data); err != nil {
		return nil, types.SenderIdentity{}, fmt.Errorf("renderer: failed to render text for %q: %w", eventType, err)
	}

	// Determine sender identity.
	sender := types.SenderIdentity{
		Address: r.defaultFromAddr,
		Name:    determineSenderName(r.defaultFromName, set),
	}

	return &RenderedEmail{
		Subject:  data.Subject,
		BodyHTML: htmlBuf.String(),
		BodyText: txtBuf.String(),
	}, sender, nil
}

// buildTemplateData extracts fields from the notification payload into a
// typed struct for template rendering.
func (r *Renderer) buildTemplateData(eventType types.EventType, n *types.Notification, loc *time.Location) templateData {
	now := time.Now().UTC()

	prefix := subjectPrefixes[eventType]
	if prefix == "" {
		prefix = string(eventType)
	}

	wpName := stringFromPayload(n.Payload, "watchpoint_name")
	if wpName == "" {
		wpName = n.WatchPointID
	}

	subject := prefix
	if wpName != "" && eventType != types.EventSystemAlert &&
		eventType != types.EventBillingWarning &&
		eventType != types.EventBillingReceipt {
		subject = fmt.Sprintf("%s: %s", prefix, wpName)
	}

	return templateData{
		Subject:           subject,
		WatchPointName:    wpName,
		Location:          stringFromPayload(n.Payload, "location"),
		FormattedDate:     now.In(loc).Format("Mon, Jan 2 at 3:04 PM"),
		TimezoneName:      loc.String(),
		Urgency:           string(n.Urgency),
		ConditionsSummary: stringFromPayload(n.Payload, "conditions_summary"),
		BouncedAddress:    stringFromPayload(n.Payload, "bounced_address"),
		DisabledReason:    stringFromPayload(n.Payload, "disabled_reason"),
	}
}

// stringFromPayload safely extracts a string value from a map.
func stringFromPayload(payload map[string]interface{}, key string) string {
	if payload == nil {
		return ""
	}
	v, ok := payload[key].(string)
	if !ok {
		return ""
	}
	return v
}

// determineSenderName returns the appropriate sender name for the template set.
// Custom template sets get a branded sender name; the default set uses the
// configured default.
func determineSenderName(defaultName, templateSet string) string {
	if templateSet == "" || templateSet == "default" {
		return defaultName
	}
	return fmt.Sprintf("%s (%s)", defaultName, templateSet)
}

// Compile-time assertion that Renderer implements TemplateService.
var _ TemplateService = (*Renderer)(nil)
