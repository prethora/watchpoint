package email

import (
	"strings"
	"testing"

	"watchpoint/internal/types"
)

func TestNewRenderer(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}
	if r == nil {
		t.Fatal("expected non-nil renderer")
	}
}

func TestRendererRenderAllEventTypes(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
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
		t.Run(string(et), func(t *testing.T) {
			n := &types.Notification{
				ID:             "notif-123",
				WatchPointID:   "wp-456",
				OrganizationID: "org-789",
				EventType:      et,
				Urgency:        types.UrgencyWarning,
				TemplateSet:    "default",
				Payload: map[string]interface{}{
					"timezone":       "America/New_York",
					"watchpoint_name": "Downtown Sensor",
					"location":       "New York, NY",
				},
			}

			rendered, sender, err := r.Render("default", et, n)
			if err != nil {
				t.Fatalf("Render() error: %v", err)
			}

			if rendered.Subject == "" {
				t.Error("Subject should not be empty")
			}
			if rendered.BodyHTML == "" {
				t.Error("BodyHTML should not be empty")
			}
			if rendered.BodyText == "" {
				t.Error("BodyText should not be empty")
			}
			if sender.Address != "alerts@watchpoint.io" {
				t.Errorf("sender.Address = %q, want alerts@watchpoint.io", sender.Address)
			}
			if sender.Name != "WatchPoint Alerts" {
				t.Errorf("sender.Name = %q, want WatchPoint Alerts", sender.Name)
			}

			// HTML should contain basic structure.
			if !strings.Contains(rendered.BodyHTML, "<!DOCTYPE html>") {
				t.Error("BodyHTML should contain DOCTYPE")
			}
		})
	}
}

func TestRendererRenderThresholdCrossed(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}

	n := &types.Notification{
		ID:           "notif-tc-1",
		WatchPointID: "wp-1",
		EventType:    types.EventThresholdCrossed,
		Urgency:      types.UrgencyCritical,
		Payload: map[string]interface{}{
			"watchpoint_name":    "Beach Monitor",
			"location":           "Miami Beach, FL",
			"timezone":           "America/New_York",
			"conditions_summary": "Wind > 50km/h",
		},
	}

	rendered, _, err := r.Render("default", types.EventThresholdCrossed, n)
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}

	if !strings.Contains(rendered.Subject, "Beach Monitor") {
		t.Errorf("Subject should contain watchpoint name, got: %q", rendered.Subject)
	}
	if !strings.Contains(rendered.BodyHTML, "Miami Beach, FL") {
		t.Error("HTML body should contain location")
	}
	if !strings.Contains(rendered.BodyText, "Beach Monitor") {
		t.Error("Text body should contain watchpoint name")
	}
}

func TestRendererRenderTimezone(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}

	t.Run("valid timezone", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-tz",
			EventType: types.EventThresholdCrossed,
			Payload: map[string]interface{}{
				"timezone":       "America/Chicago",
				"watchpoint_name": "Test",
			},
		}

		rendered, _, err := r.Render("default", types.EventThresholdCrossed, n)
		if err != nil {
			t.Fatalf("Render() error: %v", err)
		}

		if !strings.Contains(rendered.BodyHTML, "America/Chicago") {
			t.Error("HTML should contain timezone name")
		}
	})

	t.Run("missing timezone defaults to UTC", func(t *testing.T) {
		n := &types.Notification{
			ID:        "notif-no-tz",
			EventType: types.EventThresholdCleared,
			Payload:   map[string]interface{}{},
		}

		rendered, _, err := r.Render("default", types.EventThresholdCleared, n)
		if err != nil {
			t.Fatalf("Render() error: %v", err)
		}

		if !strings.Contains(rendered.BodyHTML, "UTC") {
			t.Error("HTML should contain UTC when no timezone set")
		}
	})
}

func TestRendererRenderCustomSetSenderName(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}

	n := &types.Notification{
		ID:          "notif-custom",
		EventType:   types.EventThresholdCrossed,
		TemplateSet: "wedding",
		Payload:     map[string]interface{}{},
	}

	_, sender, err := r.Render("wedding", types.EventThresholdCrossed, n)
	if err != nil {
		t.Fatalf("Render() error: %v", err)
	}

	want := "WatchPoint Alerts (wedding)"
	if sender.Name != want {
		t.Errorf("sender.Name = %q, want %q", sender.Name, want)
	}
}

func TestRendererRenderNilNotification(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}

	_, _, err = r.Render("default", types.EventThresholdCrossed, nil)
	if err == nil {
		t.Error("expected error for nil notification")
	}
}

func TestRendererRenderUnknownEventType(t *testing.T) {
	r, err := NewRenderer(RendererConfig{
		DefaultFromAddr: "alerts@watchpoint.io",
		DefaultFromName: "WatchPoint Alerts",
		Logger:          newTestLogger(),
	})
	if err != nil {
		t.Fatalf("NewRenderer() error: %v", err)
	}

	n := &types.Notification{
		ID:        "notif-unknown",
		EventType: types.EventType("unknown_event"),
		Payload:   map[string]interface{}{},
	}

	_, _, err = r.Render("default", types.EventType("unknown_event"), n)
	if err == nil {
		t.Error("expected error for unknown event type")
	}
}

func TestDetermineSenderName(t *testing.T) {
	tests := []struct {
		name        string
		defaultName string
		set         string
		want        string
	}{
		{"default set", "WatchPoint", "default", "WatchPoint"},
		{"empty set", "WatchPoint", "", "WatchPoint"},
		{"custom set", "WatchPoint", "wedding", "WatchPoint (wedding)"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := determineSenderName(tt.defaultName, tt.set)
			if got != tt.want {
				t.Errorf("determineSenderName() = %q, want %q", got, tt.want)
			}
		})
	}
}
