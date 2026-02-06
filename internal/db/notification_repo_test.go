package db

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// Note: mockDBTX, mockRow, and mockRows are defined in session_repo_test.go
// and usage_repo_test.go and reused here.

// notifMockRows implements pgx.Rows for testing notification List queries.
// It supports the specific column types produced by the JOIN query:
// (id string, event_type string, created_at time.Time, payload []byte,
// channel_type *string, status *string, delivered_at *time.Time)
type notifMockRows struct {
	data    []notifRowData
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

type notifRowData struct {
	id          string
	eventType   string
	createdAt   time.Time
	payload     []byte
	channelType *string
	status      *string
	deliveredAt *time.Time
}

func (r *notifMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *notifMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	*dest[0].(*string) = row.id
	*dest[1].(*string) = row.eventType
	*dest[2].(*time.Time) = row.createdAt
	*dest[3].(*[]byte) = row.payload
	*dest[4].(**string) = row.channelType
	*dest[5].(**string) = row.status
	*dest[6].(**time.Time) = row.deliveredAt
	return nil
}

func (r *notifMockRows) Close()                                     { r.closed = true }
func (r *notifMockRows) Err() error                                 { return r.errVal }
func (r *notifMockRows) CommandTag() pgconn.CommandTag               { return pgconn.CommandTag{} }
func (r *notifMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *notifMockRows) RawValues() [][]byte                         { return nil }
func (r *notifMockRows) Values() ([]any, error)                      { return nil, nil }
func (r *notifMockRows) Conn() *pgx.Conn                            { return nil }

// deliveryMockRows implements pgx.Rows for testing GetPendingDeliveries.
type deliveryMockRows struct {
	data    []deliveryRowData
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

type deliveryRowData struct {
	id            string
	notifID       string
	channelType   string
	status        string
	attemptCount  int
	lastAttemptAt *time.Time
	failureReason *string
	providerMsgID *string
	nextRetryAt   *time.Time
	deliveredAt   *time.Time
	channelConfig []byte
}

func (r *deliveryMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *deliveryMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	row := r.data[r.idx]
	*dest[0].(*string) = row.id
	*dest[1].(*string) = row.notifID
	*dest[2].(*string) = row.channelType
	*dest[3].(*string) = row.status
	*dest[4].(*int) = row.attemptCount
	*dest[5].(**time.Time) = row.lastAttemptAt
	*dest[6].(**string) = row.failureReason
	*dest[7].(**string) = row.providerMsgID
	*dest[8].(**time.Time) = row.nextRetryAt
	*dest[9].(**time.Time) = row.deliveredAt
	*dest[10].(*[]byte) = row.channelConfig
	return nil
}

func (r *deliveryMockRows) Close()                                     { r.closed = true }
func (r *deliveryMockRows) Err() error                                 { return r.errVal }
func (r *deliveryMockRows) CommandTag() pgconn.CommandTag               { return pgconn.CommandTag{} }
func (r *deliveryMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *deliveryMockRows) RawValues() [][]byte                         { return nil }
func (r *deliveryMockRows) Values() ([]any, error)                      { return nil, nil }
func (r *deliveryMockRows) Conn() *pgx.Conn                            { return nil }

// ============================================================
// Create Notification Tests
// ============================================================

func TestNotificationRepository_Create_WithID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	n := &types.Notification{
		ID:             "notif_abc123",
		WatchPointID:   "wp_1",
		OrganizationID: "org_1",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyCritical,
		Payload:        map[string]interface{}{"key": "value"},
		TestMode:       false,
		TemplateSet:    "default",
		CreatedAt:      now,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, n)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_Create_WithoutID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	n := &types.Notification{
		WatchPointID:   "wp_1",
		OrganizationID: "org_1",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyRoutine,
		Payload:        map[string]interface{}{"key": "value"},
		TestMode:       false,
	}

	generatedID := "notif_generated456"
	generatedAt := time.Date(2026, 2, 6, 12, 0, 1, 0, time.UTC)

	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = generatedID
			*dest[1].(*time.Time) = generatedAt
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	err := repo.Create(ctx, n)
	require.NoError(t, err)
	assert.Equal(t, generatedID, n.ID)
	assert.Equal(t, generatedAt, n.CreatedAt)
	db.AssertExpectations(t)
}

func TestNotificationRepository_Create_DefaultsTemplateSet(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	n := &types.Notification{
		ID:             "notif_ts_test",
		WatchPointID:   "wp_1",
		OrganizationID: "org_1",
		EventType:      types.EventDigest,
		Urgency:        types.UrgencyRoutine,
		Payload:        map[string]interface{}{},
		// TemplateSet is empty - should default to "default"
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sqlArgs := args.Get(2).([]any)
			// template_set is the 8th argument ($8)
			assert.Equal(t, "default", sqlArgs[7], "template_set should default to 'default'")
		}).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.Create(ctx, n)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_Create_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	n := &types.Notification{
		ID:             "notif_fail1",
		WatchPointID:   "wp_1",
		OrganizationID: "org_1",
		EventType:      types.EventThresholdCrossed,
		Urgency:        types.UrgencyCritical,
		Payload:        map[string]interface{}{},
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.Create(ctx, n)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// Create Delivery Tests
// ============================================================

func TestNotificationRepository_CreateDelivery_WithID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	d := &types.NotificationDelivery{
		ID:             "del_abc123",
		NotificationID: "notif_1",
		ChannelType:    types.ChannelEmail,
		ChannelConfig:  map[string]any{"email": "test@example.com"},
		Status:         "pending",
		AttemptCount:   0,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.CreateDelivery(ctx, d)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_CreateDelivery_WithoutID(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	d := &types.NotificationDelivery{
		NotificationID: "notif_1",
		ChannelType:    types.ChannelWebhook,
		ChannelConfig:  map[string]any{"url": "https://hook.example.com"},
	}

	generatedID := "del_generated789"
	mockRowResult := &mockRow{
		scanFn: func(dest ...any) error {
			*dest[0].(*string) = generatedID
			return nil
		},
	}

	db.On("QueryRow", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(mockRowResult)

	err := repo.CreateDelivery(ctx, d)
	require.NoError(t, err)
	assert.Equal(t, generatedID, d.ID)
	db.AssertExpectations(t)
}

func TestNotificationRepository_CreateDelivery_DefaultsStatusToPending(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	d := &types.NotificationDelivery{
		ID:             "del_default_status",
		NotificationID: "notif_1",
		ChannelType:    types.ChannelEmail,
		// Status is empty - should default to "pending"
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sqlArgs := args.Get(2).([]any)
			// status is the 5th argument ($5)
			assert.Equal(t, "pending", sqlArgs[4], "status should default to 'pending'")
		}).
		Return(pgconn.NewCommandTag("INSERT 0 1"), nil)

	err := repo.CreateDelivery(ctx, d)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_CreateDelivery_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	d := &types.NotificationDelivery{
		ID:             "del_fail1",
		NotificationID: "notif_1",
		ChannelType:    types.ChannelEmail,
	}

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.CreateDelivery(ctx, d)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// UpdateDeliveryStatus Tests
// ============================================================

func TestNotificationRepository_UpdateDeliveryStatus_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateDeliveryStatus(ctx, "del_1", "sent", "")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_UpdateDeliveryStatus_WithFailureReason(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 1"), nil)

	err := repo.UpdateDeliveryStatus(ctx, "del_2", "failed", "provider returned 503")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_UpdateDeliveryStatus_NotFound(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.UpdateDeliveryStatus(ctx, "del_nonexistent", "sent", "")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeNotFoundNotification, appErr.Code)
	db.AssertExpectations(t)
}

func TestNotificationRepository_UpdateDeliveryStatus_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("deadlock"))

	err := repo.UpdateDeliveryStatus(ctx, "del_1", "sent", "")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// List Tests (JOIN Logic)
// ============================================================

func TestNotificationRepository_List_WatchPointScoped_WithDeliveries(t *testing.T) {
	// INFO-001: WatchPoint-scoped notification history.
	// Validates that the JOIN correctly populates delivery statuses.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	sentAt := now.Add(-1 * time.Hour)

	emailChannel := "email"
	webhookChannel := "webhook"
	sentStatus := "sent"
	failedStatus := "failed"

	payload := buildTestPayload(t, 25.5, 75.0, 40.0)

	rows := &notifMockRows{
		data: []notifRowData{
			// Notification 1, delivery 1 (email, sent)
			{
				id:          "notif_1",
				eventType:   "threshold_crossed",
				createdAt:   now,
				payload:     payload,
				channelType: &emailChannel,
				status:      &sentStatus,
				deliveredAt: &sentAt,
			},
			// Notification 1, delivery 2 (webhook, failed)
			{
				id:          "notif_1",
				eventType:   "threshold_crossed",
				createdAt:   now,
				payload:     payload,
				channelType: &webhookChannel,
				status:      &failedStatus,
				deliveredAt: nil,
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		WatchPointID:   "wp_1",
	}

	results, pageInfo, err := repo.List(ctx, filter)
	require.NoError(t, err)
	require.Len(t, results, 1, "should aggregate into 1 notification")
	assert.False(t, pageInfo.HasMore)

	item := results[0]
	assert.Equal(t, "notif_1", item.ID)
	assert.Equal(t, types.EventThresholdCrossed, item.EventType)
	assert.Equal(t, now, item.SentAt)
	require.Len(t, item.Channels, 2, "should have 2 delivery channels")

	// Verify delivery summaries
	assert.Equal(t, "email", item.Channels[0].Channel)
	assert.Equal(t, "sent", item.Channels[0].Status)
	require.NotNil(t, item.Channels[0].SentAt)
	assert.Equal(t, sentAt, *item.Channels[0].SentAt)

	assert.Equal(t, "webhook", item.Channels[1].Channel)
	assert.Equal(t, "failed", item.Channels[1].Status)
	assert.Nil(t, item.Channels[1].SentAt)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_OrgScoped_WithEventTypeFilter(t *testing.T) {
	// INFO-002: Organization-scoped notification history with event_type filter.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	emailChannel := "email"
	sentStatus := "sent"
	sentAt := now.Add(-30 * time.Minute)

	payload := buildTestPayload(t, 22.0, 50.0, 30.0)

	rows := &notifMockRows{
		data: []notifRowData{
			{
				id:          "notif_2",
				eventType:   "threshold_crossed",
				createdAt:   now,
				payload:     payload,
				channelType: &emailChannel,
				status:      &sentStatus,
				deliveredAt: &sentAt,
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sql := args.Get(1).(string)
			// Verify the SQL contains event_type filter
			assert.Contains(t, sql, "event_type IN")
		}).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		EventTypes:     []types.EventType{types.EventThresholdCrossed},
	}

	results, _, err := repo.List(ctx, filter)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "notif_2", results[0].ID)
	assert.Equal(t, types.EventThresholdCrossed, results[0].EventType)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_MultipleNotificationsWithDeliveries(t *testing.T) {
	// Tests grouping logic: multiple notifications, each with multiple deliveries.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	earlier := now.Add(-2 * time.Hour)
	emailChannel := "email"
	webhookChannel := "webhook"
	sentStatus := "sent"
	pendingStatus := "pending"
	sentAt := now.Add(-5 * time.Minute)

	payload1 := buildTestPayload(t, 30.0, 80.0, 50.0)
	payload2 := buildTestPayload(t, 15.0, 40.0, 20.0)

	rows := &notifMockRows{
		data: []notifRowData{
			// Notification 1 (newer), email sent
			{id: "notif_a", eventType: "threshold_crossed", createdAt: now,
				payload: payload1, channelType: &emailChannel, status: &sentStatus, deliveredAt: &sentAt},
			// Notification 1 (newer), webhook pending
			{id: "notif_a", eventType: "threshold_crossed", createdAt: now,
				payload: payload1, channelType: &webhookChannel, status: &pendingStatus, deliveredAt: nil},
			// Notification 2 (older), email sent
			{id: "notif_b", eventType: "monitor_digest", createdAt: earlier,
				payload: payload2, channelType: &emailChannel, status: &sentStatus, deliveredAt: &sentAt},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	results, _, err := repo.List(ctx, filter)
	require.NoError(t, err)
	require.Len(t, results, 2, "should have 2 distinct notifications")

	// First notification (newer)
	assert.Equal(t, "notif_a", results[0].ID)
	require.Len(t, results[0].Channels, 2)
	assert.Equal(t, "email", results[0].Channels[0].Channel)
	assert.Equal(t, "sent", results[0].Channels[0].Status)
	assert.Equal(t, "webhook", results[0].Channels[1].Channel)
	assert.Equal(t, "pending", results[0].Channels[1].Status)

	// Second notification (older)
	assert.Equal(t, "notif_b", results[1].ID)
	require.Len(t, results[1].Channels, 1)
	assert.Equal(t, "email", results[1].Channels[0].Channel)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_NotificationWithNoDeliveries(t *testing.T) {
	// Tests LEFT JOIN behavior: notification exists but has no deliveries yet.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	payload := buildTestPayload(t, 20.0, 60.0, 35.0)

	rows := &notifMockRows{
		data: []notifRowData{
			// Notification with NULL delivery columns (LEFT JOIN no match)
			{id: "notif_no_del", eventType: "threshold_crossed", createdAt: now,
				payload: payload, channelType: nil, status: nil, deliveredAt: nil},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		WatchPointID:   "wp_1",
	}

	results, _, err := repo.List(ctx, filter)
	require.NoError(t, err)
	require.Len(t, results, 1)
	assert.Equal(t, "notif_no_del", results[0].ID)
	assert.Empty(t, results[0].Channels, "should have empty channels when no deliveries exist")

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_EmptyResult(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	rows := &notifMockRows{data: []notifRowData{}, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	results, pageInfo, err := repo.List(ctx, filter)
	require.NoError(t, err)
	assert.Empty(t, results)
	assert.False(t, pageInfo.HasMore)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_Pagination(t *testing.T) {
	// Tests the limit+1 strategy for HasMore detection.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	payload := buildTestPayload(t, 20.0, 60.0, 35.0)

	// Default limit is 20, so we create 21 unique notifications.
	var rowData []notifRowData
	for i := 0; i < 21; i++ {
		ts := now.Add(-time.Duration(i) * time.Minute)
		id := "notif_page_" + time.Duration(i).String()
		// Each notification has no deliveries (NULL join columns)
		rowData = append(rowData, notifRowData{
			id:          id,
			eventType:   "threshold_crossed",
			createdAt:   ts,
			payload:     payload,
			channelType: nil,
			status:      nil,
			deliveredAt: nil,
		})
	}

	rows := &notifMockRows{data: rowData, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	results, pageInfo, err := repo.List(ctx, filter)
	require.NoError(t, err)
	assert.Len(t, results, 20, "should return exactly 20 items (limit)")
	assert.True(t, pageInfo.HasMore, "should indicate more results available")
	assert.NotEmpty(t, pageInfo.NextCursor, "should provide a cursor for next page")

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_WithCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	payload := buildTestPayload(t, 20.0, 60.0, 35.0)

	rows := &notifMockRows{
		data: []notifRowData{
			{id: "notif_page2_1", eventType: "threshold_crossed", createdAt: now.Add(-25 * time.Minute),
				payload: payload, channelType: nil, status: nil, deliveredAt: nil},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sql := args.Get(1).(string)
			assert.Contains(t, sql, "n.created_at <")
		}).
		Return(rows, nil)

	cursorTime := now.Add(-20 * time.Minute)
	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		Pagination: types.PageInfo{
			NextCursor: cursorTime.Format(time.RFC3339Nano),
		},
	}

	results, pageInfo, err := repo.List(ctx, filter)
	require.NoError(t, err)
	assert.Len(t, results, 1)
	assert.False(t, pageInfo.HasMore)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_InvalidCursor(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		Pagination: types.PageInfo{
			NextCursor: "not-a-valid-timestamp",
		},
	}

	_, _, err := repo.List(ctx, filter)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeValidationMissingField, appErr.Code)
}

func TestNotificationRepository_List_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(nil, errors.New("connection reset"))

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	_, _, err := repo.List(ctx, filter)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_PayloadParsing(t *testing.T) {
	// Verifies that ForecastSnapshot and TriggeredConditions are correctly
	// extracted from the payload JSONB.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	// Construct a payload with forecast_snapshot and conditions_evaluated.
	payloadMap := map[string]interface{}{
		"forecast_snapshot": map[string]interface{}{
			"precipitation_probability": 85.0,
			"precipitation_mm":          12.5,
			"temperature_c":             22.0,
			"wind_speed_kmh":            30.0,
			"humidity_percent":          70.0,
		},
		"conditions_evaluated": []map[string]interface{}{
			{
				"variable":  "precipitation_probability",
				"operator":  ">",
				"threshold": []float64{50.0},
				"matched":   true,
			},
			{
				"variable":  "temperature_c",
				"operator":  "<",
				"threshold": []float64{25.0},
				"matched":   false, // Not triggered, should NOT appear in TriggeredConditions
			},
		},
	}
	payload, _ := json.Marshal(payloadMap)

	emailChannel := "email"
	sentStatus := "sent"

	rows := &notifMockRows{
		data: []notifRowData{
			{id: "notif_payload", eventType: "threshold_crossed", createdAt: now,
				payload: payload, channelType: &emailChannel, status: &sentStatus, deliveredAt: nil},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	results, _, err := repo.List(ctx, filter)
	require.NoError(t, err)
	require.Len(t, results, 1)

	item := results[0]

	// Verify forecast snapshot was parsed
	assert.Equal(t, 85.0, item.ForecastSnapshot.PrecipitationProb)
	assert.Equal(t, 12.5, item.ForecastSnapshot.PrecipitationMM)
	assert.Equal(t, 22.0, item.ForecastSnapshot.TemperatureC)
	assert.Equal(t, 30.0, item.ForecastSnapshot.WindSpeedKmh)
	assert.Equal(t, 70.0, item.ForecastSnapshot.Humidity)

	// Verify only matched conditions appear in TriggeredConditions
	require.Len(t, item.TriggeredConditions, 1, "should only include matched conditions")
	assert.Equal(t, "precipitation_probability", item.TriggeredConditions[0].Variable)
	assert.Equal(t, types.ConditionOperator(">"), item.TriggeredConditions[0].Operator)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_MalformedPayload(t *testing.T) {
	// Verifies graceful degradation when payload is not valid JSON.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	emailChannel := "email"
	sentStatus := "sent"

	rows := &notifMockRows{
		data: []notifRowData{
			{id: "notif_bad_payload", eventType: "threshold_crossed", createdAt: now,
				payload: []byte("not valid json"),
				channelType: &emailChannel, status: &sentStatus, deliveredAt: nil},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
	}

	results, _, err := repo.List(ctx, filter)
	require.NoError(t, err, "should not fail on malformed payload")
	require.Len(t, results, 1)

	// Forecast should be zero-valued
	assert.Equal(t, float64(0), results[0].ForecastSnapshot.TemperatureC)
	assert.Nil(t, results[0].TriggeredConditions)

	db.AssertExpectations(t)
}

func TestNotificationRepository_List_UrgencyFilter(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	rows := &notifMockRows{data: []notifRowData{}, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sql := args.Get(1).(string)
			assert.Contains(t, sql, "urgency IN")
		}).
		Return(rows, nil)

	filter := types.NotificationFilter{
		OrganizationID: "org_1",
		Urgency:        []types.UrgencyLevel{types.UrgencyCritical, types.UrgencyWarning},
	}

	_, _, err := repo.List(ctx, filter)
	require.NoError(t, err)
	db.AssertExpectations(t)
}

// ============================================================
// CancelDeferredDeliveries Tests
// ============================================================

func TestNotificationRepository_CancelDeferredDeliveries_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sql := args.Get(1).(string)
			// Verify the SQL targets deferred deliveries by watchpoint
			assert.Contains(t, sql, "status = 'skipped'")
			assert.Contains(t, sql, "status = 'deferred'")
			assert.Contains(t, sql, "watchpoint_id")
		}).
		Return(pgconn.NewCommandTag("UPDATE 3"), nil)

	err := repo.CancelDeferredDeliveries(ctx, "wp_1")
	require.NoError(t, err)
	db.AssertExpectations(t)
}

func TestNotificationRepository_CancelDeferredDeliveries_NoneFound(t *testing.T) {
	// Not an error if no deferred deliveries exist.
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("UPDATE 0"), nil)

	err := repo.CancelDeferredDeliveries(ctx, "wp_no_deferred")
	require.NoError(t, err, "should not error when no deferred deliveries exist")
	db.AssertExpectations(t)
}

func TestNotificationRepository_CancelDeferredDeliveries_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("connection refused"))

	err := repo.CancelDeferredDeliveries(ctx, "wp_1")
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	db.AssertExpectations(t)
}

// ============================================================
// GetPendingDeliveries Tests
// ============================================================

func TestNotificationRepository_GetPendingDeliveries_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	now := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
	lastAttempt := now.Add(-10 * time.Minute)
	retryAt := now.Add(-1 * time.Minute)

	rows := &deliveryMockRows{
		data: []deliveryRowData{
			{
				id:            "del_1",
				notifID:       "notif_1",
				channelType:   "email",
				status:        "pending",
				attemptCount:  0,
				lastAttemptAt: nil,
				failureReason: nil,
				providerMsgID: nil,
				nextRetryAt:   nil,
				deliveredAt:   nil,
				channelConfig: []byte(`{"email":"test@example.com"}`),
			},
			{
				id:            "del_2",
				notifID:       "notif_2",
				channelType:   "webhook",
				status:        "retrying",
				attemptCount:  2,
				lastAttemptAt: &lastAttempt,
				failureReason: strPtr("timeout"),
				providerMsgID: nil,
				nextRetryAt:   &retryAt,
				deliveredAt:   nil,
				channelConfig: []byte(`{"url":"https://hook.example.com"}`),
			},
		},
		idx: -1,
	}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	results, err := repo.GetPendingDeliveries(ctx, 50)
	require.NoError(t, err)
	require.Len(t, results, 2)

	assert.Equal(t, "del_1", results[0].ID)
	assert.Equal(t, types.ChannelEmail, results[0].ChannelType)
	assert.Equal(t, "pending", results[0].Status)
	assert.Equal(t, 0, results[0].AttemptCount)

	assert.Equal(t, "del_2", results[1].ID)
	assert.Equal(t, types.ChannelWebhook, results[1].ChannelType)
	assert.Equal(t, "retrying", results[1].Status)
	assert.Equal(t, 2, results[1].AttemptCount)
	assert.Equal(t, "timeout", results[1].FailureReason)

	db.AssertExpectations(t)
}

func TestNotificationRepository_GetPendingDeliveries_Empty(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	rows := &deliveryMockRows{data: []deliveryRowData{}, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(rows, nil)

	results, err := repo.GetPendingDeliveries(ctx, 50)
	require.NoError(t, err)
	assert.Empty(t, results)

	db.AssertExpectations(t)
}

func TestNotificationRepository_GetPendingDeliveries_DefaultLimit(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	rows := &deliveryMockRows{data: []deliveryRowData{}, idx: -1}

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Run(func(args mock.Arguments) {
			sqlArgs := args.Get(2).([]any)
			// With limit=0, should default to 50
			assert.Equal(t, 50, sqlArgs[0])
		}).
		Return(rows, nil)

	_, err := repo.GetPendingDeliveries(ctx, 0)
	require.NoError(t, err)

	db.AssertExpectations(t)
}

func TestNotificationRepository_GetPendingDeliveries_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	db.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(nil, errors.New("connection refused"))

	_, err := repo.GetPendingDeliveries(ctx, 50)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// DeleteBefore Tests
// ============================================================

func TestNotificationRepository_DeleteBefore_Success(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("DELETE 42"), nil)

	count, err := repo.DeleteBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(42), count)

	db.AssertExpectations(t)
}

func TestNotificationRepository_DeleteBefore_NothingToDelete(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	cutoff := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.NewCommandTag("DELETE 0"), nil)

	count, err := repo.DeleteBefore(ctx, cutoff)
	require.NoError(t, err)
	assert.Equal(t, int64(0), count)

	db.AssertExpectations(t)
}

func TestNotificationRepository_DeleteBefore_DBError(t *testing.T) {
	db := new(mockDBTX)
	repo := NewNotificationRepository(db)
	ctx := context.Background()

	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	db.On("Exec", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return(pgconn.CommandTag{}, errors.New("table locked"))

	_, err := repo.DeleteBefore(ctx, cutoff)
	require.Error(t, err)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)

	db.AssertExpectations(t)
}

// ============================================================
// extractPayloadFields Tests
// ============================================================

func TestExtractPayloadFields_ValidPayload(t *testing.T) {
	payload := map[string]interface{}{
		"forecast_snapshot": map[string]interface{}{
			"precipitation_probability": 85.0,
			"precipitation_mm":          12.5,
			"temperature_c":             22.0,
			"wind_speed_kmh":            30.0,
			"humidity_percent":          70.0,
		},
		"conditions_evaluated": []map[string]interface{}{
			{"variable": "temperature_c", "operator": ">", "threshold": []float64{20.0}, "matched": true},
			{"variable": "humidity_percent", "operator": "<", "threshold": []float64{80.0}, "matched": false},
		},
	}
	b, _ := json.Marshal(payload)

	var forecast types.ForecastSnapshot
	var conditions []types.Condition
	extractPayloadFields(b, &forecast, &conditions)

	assert.Equal(t, 85.0, forecast.PrecipitationProb)
	assert.Equal(t, 22.0, forecast.TemperatureC)
	require.Len(t, conditions, 1)
	assert.Equal(t, "temperature_c", conditions[0].Variable)
}

func TestExtractPayloadFields_EmptyPayload(t *testing.T) {
	var forecast types.ForecastSnapshot
	var conditions []types.Condition
	extractPayloadFields(nil, &forecast, &conditions)

	assert.Equal(t, float64(0), forecast.TemperatureC)
	assert.Nil(t, conditions)
}

func TestExtractPayloadFields_MalformedJSON(t *testing.T) {
	var forecast types.ForecastSnapshot
	var conditions []types.Condition
	extractPayloadFields([]byte("not json"), &forecast, &conditions)

	assert.Equal(t, float64(0), forecast.TemperatureC)
	assert.Nil(t, conditions)
}

// ============================================================
// Helpers
// ============================================================

func buildTestPayload(t *testing.T, tempC, precipProb, windSpeed float64) []byte {
	t.Helper()
	payload := map[string]interface{}{
		"forecast_snapshot": map[string]interface{}{
			"precipitation_probability": precipProb,
			"temperature_c":             tempC,
			"wind_speed_kmh":            windSpeed,
		},
		"conditions_evaluated": []map[string]interface{}{},
	}
	b, err := json.Marshal(payload)
	require.NoError(t, err)
	return b
}

func strPtr(s string) *string {
	return &s
}
