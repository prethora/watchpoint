package webhook

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// testNotification creates a standard notification for testing.
func testNotification() *types.Notification {
	return &types.Notification{
		ID:             "notif-001",
		WatchPointID:   "wp-001",
		OrganizationID: "org-001",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyWarning,
		Payload: map[string]interface{}{
			"watchpoint_name": "My Garden Monitor",
			"location": map[string]interface{}{
				"label": "Portland, OR",
				"lat":   45.5155,
				"lon":   -122.6789,
			},
			"conditions": []interface{}{
				map[string]interface{}{
					"variable":      "precipitation_probability",
					"current_value": 85.0,
					"threshold":     50.0,
					"operator":      ">",
				},
			},
		},
		TestMode:    false,
		TemplateSet: "default",
	}
}

// --- Slack Formatter Tests ---

func TestSlackFormatter_Format_BasicStructure(t *testing.T) {
	f := &SlackFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	// Verify valid JSON.
	var payload SlackPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	// Verify fallback text.
	assert.Contains(t, payload.Text, "WARNING")
	assert.Contains(t, payload.Text, "Threshold Alert")

	// Verify blocks structure.
	require.True(t, len(payload.Blocks) >= 2, "should have at least header + fields + context blocks")

	// First block should be header.
	assert.Equal(t, "header", payload.Blocks[0].Type)
	assert.NotNil(t, payload.Blocks[0].Text)
	assert.Equal(t, "plain_text", payload.Blocks[0].Text.Type)
	assert.Contains(t, payload.Blocks[0].Text.Text, "Threshold Alert")
	assert.Contains(t, payload.Blocks[0].Text.Text, "My Garden Monitor")
}

func TestSlackFormatter_Format_FieldsIncluded(t *testing.T) {
	f := &SlackFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload SlackPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	// Find the section block with fields.
	var fieldsBlock *SlackBlock
	for i, block := range payload.Blocks {
		if block.Type == "section" && len(block.Fields) > 0 {
			fieldsBlock = &payload.Blocks[i]
			break
		}
	}

	require.NotNil(t, fieldsBlock, "should have a section block with fields")
	assert.True(t, len(fieldsBlock.Fields) >= 2, "should have WatchPoint name, location, and condition fields")

	// Verify specific field content.
	fieldTexts := make([]string, len(fieldsBlock.Fields))
	for i, f := range fieldsBlock.Fields {
		fieldTexts[i] = f.Text
	}
	assert.Contains(t, fieldTexts[0], "My Garden Monitor")
	assert.Contains(t, fieldTexts[1], "Portland, OR")
}

func TestSlackFormatter_Format_NilNotification(t *testing.T) {
	f := &SlackFormatter{}
	_, err := f.Format(context.Background(), nil, nil)
	assert.Error(t, err)
}

func TestSlackFormatter_Format_ContextFooter(t *testing.T) {
	f := &SlackFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload SlackPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	// Last block should be context.
	lastBlock := payload.Blocks[len(payload.Blocks)-1]
	assert.Equal(t, "context", lastBlock.Type)
	require.NotEmpty(t, lastBlock.Elements)
	assert.Contains(t, lastBlock.Elements[0].Text, "warning")
	assert.Contains(t, lastBlock.Elements[0].Text, "threshold_crossed")
}

func TestSlackFormatter_ValidateResponse(t *testing.T) {
	f := &SlackFormatter{}

	tests := []struct {
		name       string
		statusCode int
		body       []byte
		wantErr    bool
		errMsg     string
	}{
		{
			name:       "success - ok text",
			statusCode: 200,
			body:       []byte("ok"),
			wantErr:    false,
		},
		{
			name:       "soft failure - no_text",
			statusCode: 200,
			body:       []byte("no_text"),
			wantErr:    true,
			errMsg:     "no_text",
		},
		{
			name:       "soft failure - channel_not_found",
			statusCode: 200,
			body:       []byte("channel_not_found"),
			wantErr:    true,
			errMsg:     "channel_not_found",
		},
		{
			name:       "soft failure - JSON ok false",
			statusCode: 200,
			body:       []byte(`{"ok":false,"error":"invalid_token"}`),
			wantErr:    true,
			errMsg:     "invalid_token",
		},
		{
			name:       "HTTP error status",
			statusCode: 500,
			body:       []byte("internal error"),
			wantErr:    true,
		},
		{
			name:       "JSON ok true",
			statusCode: 200,
			body:       []byte(`{"ok":true}`),
			wantErr:    false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := f.ValidateResponse(tt.statusCode, tt.body)
			if tt.wantErr {
				assert.Error(t, err)
				if tt.errMsg != "" {
					assert.Contains(t, err.Error(), tt.errMsg)
				}
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// --- Teams Formatter Tests ---

func TestTeamsFormatter_Format_AdaptiveCardStructure(t *testing.T) {
	f := &TeamsFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)
	require.NotEmpty(t, data)

	var payload TeamsPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	// Verify top-level structure.
	assert.Equal(t, "message", payload.Type)
	require.Len(t, payload.Attachments, 1)

	att := payload.Attachments[0]
	assert.Equal(t, "application/vnd.microsoft.card.adaptive", att.ContentType)
	assert.Equal(t, "AdaptiveCard", att.Content.Type)
	assert.Equal(t, "1.4", att.Content.Version)

	// Verify body has title, facts, and footer.
	require.True(t, len(att.Content.Body) >= 2, "should have at least title and facts")

	// First item should be the title TextBlock.
	titleBlock := att.Content.Body[0]
	assert.Equal(t, "TextBlock", titleBlock.Type)
	assert.Contains(t, titleBlock.Text, "Threshold Alert")
	assert.Contains(t, titleBlock.Text, "My Garden Monitor")
	assert.Equal(t, "Large", titleBlock.Size)
	assert.Equal(t, "Bolder", titleBlock.Weight)
}

func TestTeamsFormatter_Format_FactSet(t *testing.T) {
	f := &TeamsFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload TeamsPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	// Find the FactSet.
	var factSet *AdaptiveItem
	for i, item := range payload.Attachments[0].Content.Body {
		if item.Type == "FactSet" {
			factSet = &payload.Attachments[0].Content.Body[i]
			break
		}
	}

	require.NotNil(t, factSet, "should have a FactSet")
	assert.True(t, len(factSet.Facts) >= 2, "should have multiple facts")

	// Verify fact content.
	factMap := make(map[string]string)
	for _, fact := range factSet.Facts {
		factMap[fact.Title] = fact.Value
	}

	assert.Equal(t, "My Garden Monitor", factMap["WatchPoint"])
	assert.Equal(t, "Portland, OR", factMap["Location"])
}

func TestTeamsFormatter_Format_NilNotification(t *testing.T) {
	f := &TeamsFormatter{}
	_, err := f.Format(context.Background(), nil, nil)
	assert.Error(t, err)
}

func TestTeamsFormatter_ValidateResponse(t *testing.T) {
	f := &TeamsFormatter{}

	assert.NoError(t, f.ValidateResponse(200, []byte("ok")))
	assert.NoError(t, f.ValidateResponse(202, []byte("")))
	assert.Error(t, f.ValidateResponse(400, []byte("bad request")))
	assert.Error(t, f.ValidateResponse(500, []byte("internal error")))
}

// --- Discord Formatter Tests ---

func TestDiscordFormatter_Format_EmbedStructure(t *testing.T) {
	f := &DiscordFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload DiscordPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	assert.Equal(t, "WatchPoint", payload.Username)
	assert.Contains(t, payload.Content, "WARNING")
	require.Len(t, payload.Embeds, 1)

	embed := payload.Embeds[0]
	assert.Contains(t, embed.Title, "Threshold Alert")
	assert.Contains(t, embed.Description, "My Garden Monitor")
	assert.Equal(t, colorWarning, embed.Color) // Warning urgency -> orange
	assert.NotNil(t, embed.Footer)
	assert.Contains(t, embed.Footer.Text, "warning")
}

func TestDiscordFormatter_Format_UrgencyColors(t *testing.T) {
	f := &DiscordFormatter{}

	tests := []struct {
		urgency   types.UrgencyLevel
		eventType types.EventType
		wantColor int
	}{
		{types.UrgencyCritical, types.EventThresholdCrossed, colorCritical},
		{types.UrgencyWarning, types.EventThresholdCrossed, colorWarning},
		{types.UrgencyWatch, types.EventThresholdCrossed, colorWatch},
		{types.UrgencyRoutine, types.EventThresholdCrossed, colorRoutine},
		// Cleared events should always be green.
		{types.UrgencyCritical, types.EventThresholdCleared, colorCleared},
	}

	for _, tt := range tests {
		t.Run(string(tt.urgency)+"_"+string(tt.eventType), func(t *testing.T) {
			n := testNotification()
			n.Urgency = tt.urgency
			n.EventType = tt.eventType

			data, err := f.Format(context.Background(), n, nil)
			require.NoError(t, err)

			var payload DiscordPayload
			err = json.Unmarshal(data, &payload)
			require.NoError(t, err)

			require.Len(t, payload.Embeds, 1)
			assert.Equal(t, tt.wantColor, payload.Embeds[0].Color)
		})
	}
}

func TestDiscordFormatter_Format_NilNotification(t *testing.T) {
	f := &DiscordFormatter{}
	_, err := f.Format(context.Background(), nil, nil)
	assert.Error(t, err)
}

// --- Google Chat Formatter Tests ---

func TestGoogleChatFormatter_Format_CardStructure(t *testing.T) {
	f := &GoogleChatFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload GoogleChatPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	require.Len(t, payload.Cards, 1)
	card := payload.Cards[0]

	assert.Contains(t, card.Header.Title, "Threshold Alert")
	assert.Contains(t, card.Header.Subtitle, "warning")
	require.NotEmpty(t, card.Sections)
	require.NotEmpty(t, card.Sections[0].Widgets)
}

func TestGoogleChatFormatter_Format_NilNotification(t *testing.T) {
	f := &GoogleChatFormatter{}
	_, err := f.Format(context.Background(), nil, nil)
	assert.Error(t, err)
}

// --- Generic Formatter Tests ---

func TestGenericFormatter_Format_StableContract(t *testing.T) {
	f := &GenericFormatter{}
	n := testNotification()

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload GenericPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	assert.Equal(t, string(types.EventThresholdCrossed), payload.EventType)
	assert.Equal(t, "wp-001", payload.WatchPointID)
	assert.Equal(t, "org-001", payload.OrganizationID)
	assert.Equal(t, "notif-001", payload.NotificationID)
	assert.Equal(t, string(types.UrgencyWarning), payload.Urgency)
	assert.False(t, payload.TestMode)
	assert.NotNil(t, payload.Payload)
	assert.Equal(t, "My Garden Monitor", payload.Payload["watchpoint_name"])
}

func TestGenericFormatter_Format_TestMode(t *testing.T) {
	f := &GenericFormatter{}
	n := testNotification()
	n.TestMode = true

	data, err := f.Format(context.Background(), n, nil)
	require.NoError(t, err)

	var payload GenericPayload
	err = json.Unmarshal(data, &payload)
	require.NoError(t, err)

	assert.True(t, payload.TestMode)
}

func TestGenericFormatter_Format_NilNotification(t *testing.T) {
	f := &GenericFormatter{}
	_, err := f.Format(context.Background(), nil, nil)
	assert.Error(t, err)
}

func TestGenericFormatter_ValidateResponse(t *testing.T) {
	f := &GenericFormatter{}

	assert.NoError(t, f.ValidateResponse(200, []byte("ok")))
	assert.NoError(t, f.ValidateResponse(204, []byte("")))
	assert.Error(t, f.ValidateResponse(400, []byte("bad request")))
	assert.Error(t, f.ValidateResponse(500, []byte("error")))
}

// --- Cross-Formatter Tests ---

func TestAllFormatters_HandleMinimalNotification(t *testing.T) {
	// Ensure all formatters handle a notification with no payload gracefully.
	n := &types.Notification{
		ID:             "notif-min",
		WatchPointID:   "wp-min",
		OrganizationID: "org-min",
		EventType:      types.EventSystemAlert,
		Urgency:        types.UrgencyRoutine,
		Payload:        nil,
	}

	formatters := []PlatformFormatter{
		&SlackFormatter{},
		&TeamsFormatter{},
		&DiscordFormatter{},
		&GoogleChatFormatter{},
		&GenericFormatter{},
	}

	for _, f := range formatters {
		t.Run(string(f.Platform()), func(t *testing.T) {
			data, err := f.Format(context.Background(), n, nil)
			require.NoError(t, err, "formatter %s should handle nil payload", f.Platform())
			require.NotEmpty(t, data)

			// Verify it is valid JSON.
			var raw map[string]interface{}
			err = json.Unmarshal(data, &raw)
			assert.NoError(t, err, "output should be valid JSON for %s", f.Platform())
		})
	}
}

func TestAllFormatters_HandleEmptyPayload(t *testing.T) {
	n := &types.Notification{
		ID:             "notif-empty",
		WatchPointID:   "wp-empty",
		OrganizationID: "org-empty",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyCritical,
		Payload:        map[string]interface{}{},
	}

	formatters := []PlatformFormatter{
		&SlackFormatter{},
		&TeamsFormatter{},
		&DiscordFormatter{},
		&GoogleChatFormatter{},
		&GenericFormatter{},
	}

	for _, f := range formatters {
		t.Run(string(f.Platform()), func(t *testing.T) {
			data, err := f.Format(context.Background(), n, nil)
			require.NoError(t, err, "formatter %s should handle empty payload", f.Platform())
			require.NotEmpty(t, data)
		})
	}
}
