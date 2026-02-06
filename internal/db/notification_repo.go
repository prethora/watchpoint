package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// NotificationRepository provides data access for the notifications and
// notification_deliveries tables. It implements the NotificationRepository
// interface defined in 02-foundation-db.md Section 9.2.
//
// The List method performs a JOIN between notifications and notification_deliveries
// to hydrate the Channels field in NotificationHistoryItem DTOs (INFO-001, INFO-002).
type NotificationRepository struct {
	db DBTX
}

// NewNotificationRepository creates a new NotificationRepository backed by the
// given database connection (pool or transaction).
func NewNotificationRepository(db DBTX) *NotificationRepository {
	return &NotificationRepository{db: db}
}

// Create inserts a new notification record. The caller must set the ID
// (prefixed UUID, e.g. "notif_...") and required fields before calling.
// If the ID is empty, the database generates it via the DEFAULT expression.
//
// The template_set is snapshot at creation time from the WatchPoint config to
// ensure consistent rendering even if WatchPoint config changes later.
func (r *NotificationRepository) Create(ctx context.Context, n *types.Notification) error {
	if n.ID != "" {
		_, err := r.db.Exec(ctx,
			`INSERT INTO notifications
			 (id, watchpoint_id, organization_id, event_type, urgency, payload,
			  test_mode, template_set, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, COALESCE($9, NOW()))`,
			n.ID,
			n.WatchPointID,
			n.OrganizationID,
			string(n.EventType),
			string(n.Urgency),
			n.Payload,
			n.TestMode,
			notifTemplateSet(n),
			nilIfZeroTime(n.CreatedAt),
		)
		if err != nil {
			return types.NewAppError(types.ErrCodeInternalDB, "failed to create notification", err)
		}
		return nil
	}

	// Let the database generate the ID via DEFAULT.
	row := r.db.QueryRow(ctx,
		`INSERT INTO notifications
		 (watchpoint_id, organization_id, event_type, urgency, payload,
		  test_mode, template_set, created_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8, NOW()))
		 RETURNING id, created_at`,
		n.WatchPointID,
		n.OrganizationID,
		string(n.EventType),
		string(n.Urgency),
		n.Payload,
		n.TestMode,
		notifTemplateSet(n),
		nilIfZeroTime(n.CreatedAt),
	)
	if err := row.Scan(&n.ID, &n.CreatedAt); err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create notification", err)
	}
	return nil
}

// CreateDelivery inserts a new notification delivery record. The caller must
// set the NotificationID and ChannelType at minimum. If the ID is empty,
// the database generates it via the DEFAULT expression.
//
// The channel_config column stores a snapshot of the channel configuration at
// the time of delivery creation for audit purposes.
func (r *NotificationRepository) CreateDelivery(ctx context.Context, d *types.NotificationDelivery) error {
	if d.ID != "" {
		_, err := r.db.Exec(ctx,
			`INSERT INTO notification_deliveries
			 (id, notification_id, channel_type, channel_config, status,
			  attempt_count, next_retry_at, last_attempt_at, delivered_at,
			  failure_reason, provider_message_id)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
			d.ID,
			d.NotificationID,
			string(d.ChannelType),
			deliveryChannelConfig(d),
			deliveryStatusOrDefault(d.Status),
			d.AttemptCount,
			nilIfZeroTime(d.NextRetryAt),
			nilIfZeroTime(d.LastAttemptAt),
			nilIfZeroTime(d.DeliveredAt),
			nilIfEmpty(d.FailureReason),
			nilIfEmpty(d.ProviderMsgID),
		)
		if err != nil {
			return types.NewAppError(types.ErrCodeInternalDB, "failed to create notification delivery", err)
		}
		return nil
	}

	// Let the database generate the ID via DEFAULT.
	row := r.db.QueryRow(ctx,
		`INSERT INTO notification_deliveries
		 (notification_id, channel_type, channel_config, status,
		  attempt_count, next_retry_at, last_attempt_at, delivered_at,
		  failure_reason, provider_message_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
		 RETURNING id`,
		d.NotificationID,
		string(d.ChannelType),
		deliveryChannelConfig(d),
		deliveryStatusOrDefault(d.Status),
		d.AttemptCount,
		nilIfZeroTime(d.NextRetryAt),
		nilIfZeroTime(d.LastAttemptAt),
		nilIfZeroTime(d.DeliveredAt),
		nilIfEmpty(d.FailureReason),
		nilIfEmpty(d.ProviderMsgID),
	)
	if err := row.Scan(&d.ID); err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create notification delivery", err)
	}
	return nil
}

// UpdateDeliveryStatus updates the status and optional failure reason for a
// notification delivery. Also updates last_attempt_at to the current time and
// increments the attempt_count. If status is "sent", delivered_at is set.
func (r *NotificationRepository) UpdateDeliveryStatus(ctx context.Context, deliveryID string, status string, reason string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE notification_deliveries SET
			status = $1,
			failure_reason = $2,
			attempt_count = attempt_count + 1,
			last_attempt_at = NOW(),
			delivered_at = CASE WHEN $1 = 'sent' THEN NOW() ELSE delivered_at END
		 WHERE id = $3`,
		status,
		nilIfEmpty(reason),
		deliveryID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update delivery status", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundNotification, "notification delivery not found", nil)
	}
	return nil
}

// GetPendingDeliveries retrieves deliveries that are ready for processing.
// Returns deliveries with status 'pending' or 'retrying' where next_retry_at
// has passed (or is NULL for pending). Ordered by creation time for FIFO
// processing. Leverages the idx_delivery_queue partial index.
func (r *NotificationRepository) GetPendingDeliveries(ctx context.Context, limit int) ([]*types.NotificationDelivery, error) {
	if limit <= 0 {
		limit = 50
	}

	rows, err := r.db.Query(ctx,
		`SELECT id, notification_id, channel_type, status,
		        attempt_count, last_attempt_at, failure_reason, provider_message_id,
		        next_retry_at, delivered_at, channel_config
		 FROM notification_deliveries
		 WHERE status IN ('pending', 'retrying')
		   AND (next_retry_at IS NULL OR next_retry_at <= NOW())
		 ORDER BY id
		 LIMIT $1`,
		limit,
	)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to get pending deliveries", err)
	}
	defer rows.Close()

	var results []*types.NotificationDelivery
	for rows.Next() {
		d, scanErr := scanDeliveryFromRows(rows)
		if scanErr != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan delivery row", scanErr)
		}
		results = append(results, d)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating delivery rows", err)
	}

	return results, nil
}

// List retrieves notification history with filtering support. It performs a
// LEFT JOIN between notifications and notification_deliveries to populate the
// Channels list in each NotificationHistoryItem.
//
// Supports both WatchPoint-scoped (filter.WatchPointID set) and Organization-
// scoped (filter.WatchPointID empty) queries, as specified in flows INFO-001
// and INFO-002.
//
// The query groups by notification_id to aggregate delivery statuses into the
// Channels slice (DeliverySummary). Pagination is cursor-based using created_at.
func (r *NotificationRepository) List(ctx context.Context, filter types.NotificationFilter) ([]*types.NotificationHistoryItem, types.PageInfo, error) {
	// Determine effective limit. The NotificationFilter uses PageInfo which
	// doesn't have a dedicated Limit field, so we use TotalItems as a hint
	// when explicitly set by the caller, defaulting to 20.
	limit := 20
	if filter.Pagination.TotalItems != nil && *filter.Pagination.TotalItems > 0 {
		limit = *filter.Pagination.TotalItems
	}
	if limit > 100 {
		limit = 100
	}

	var conditions []string
	var args []any
	argIdx := 1

	// Organization scope is always required.
	conditions = append(conditions, fmt.Sprintf("n.organization_id = $%d", argIdx))
	args = append(args, filter.OrganizationID)
	argIdx++

	// WatchPoint scope (optional - if set, scopes to a single WatchPoint).
	if filter.WatchPointID != "" {
		conditions = append(conditions, fmt.Sprintf("n.watchpoint_id = $%d", argIdx))
		args = append(args, filter.WatchPointID)
		argIdx++
	}

	// Event type filter: match any of the provided event types.
	if len(filter.EventTypes) > 0 {
		placeholders := make([]string, len(filter.EventTypes))
		for i, et := range filter.EventTypes {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, string(et))
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("n.event_type IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Urgency filter: match any of the provided urgency levels.
	if len(filter.Urgency) > 0 {
		placeholders := make([]string, len(filter.Urgency))
		for i, u := range filter.Urgency {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, string(u))
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("n.urgency IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Cursor-based pagination: fetch items older than the cursor timestamp.
	if filter.Pagination.NextCursor != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, filter.Pagination.NextCursor)
		if err != nil {
			return nil, types.PageInfo{}, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"invalid cursor format; expected RFC3339 timestamp",
				err,
			)
		}
		conditions = append(conditions, fmt.Sprintf("n.created_at < $%d", argIdx))
		args = append(args, cursorTime)
		argIdx++
	}

	whereClause := ""
	if len(conditions) > 0 {
		whereClause = "WHERE " + strings.Join(conditions, " AND ")
	}

	// The query uses a LEFT JOIN to get delivery information.
	// We fetch limit+1 notification rows to detect HasMore.
	// The LIMIT applies to the outer notification query, not the join.
	//
	// Query pattern from flow simulation INFO-001:
	// SELECT n.id, n.event_type, n.created_at, n.payload,
	//        nd.channel_type, nd.status, nd.delivered_at
	// FROM notifications n
	// LEFT JOIN notification_deliveries nd ON n.id = nd.notification_id
	// WHERE n.watchpoint_id = $1 AND n.organization_id = $2
	// ORDER BY n.created_at DESC
	// LIMIT 20;
	//
	// We use a subquery approach to correctly limit the notification rows
	// before joining to prevent the JOIN from inflating the row count.
	query := fmt.Sprintf(
		`SELECT sub.id, sub.event_type, sub.created_at, sub.payload,
		        nd.channel_type, nd.status, nd.delivered_at
		 FROM (
		     SELECT n.id, n.event_type, n.created_at, n.payload
		     FROM notifications n
		     %s
		     ORDER BY n.created_at DESC
		     LIMIT $%d
		 ) sub
		 LEFT JOIN notification_deliveries nd ON sub.id = nd.notification_id
		 ORDER BY sub.created_at DESC, nd.id`,
		whereClause,
		argIdx,
	)
	args = append(args, limit+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "failed to list notifications", err)
	}
	defer rows.Close()

	// Aggregate rows by notification_id. Multiple rows per notification
	// exist when a notification has multiple deliveries.
	type notifAccumulator struct {
		item  *types.NotificationHistoryItem
		order int // insertion order for stable sorting
	}

	itemMap := make(map[string]*notifAccumulator)
	var orderedIDs []string

	for rows.Next() {
		var (
			id          string
			eventType   string
			createdAt   time.Time
			payloadJSON []byte

			// Delivery columns (nullable from LEFT JOIN)
			channelType *string
			status      *string
			deliveredAt *time.Time
		)

		if err := rows.Scan(&id, &eventType, &createdAt, &payloadJSON,
			&channelType, &status, &deliveredAt); err != nil {
			return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "failed to scan notification row", err)
		}

		acc, exists := itemMap[id]
		if !exists {
			// Parse the payload to extract ForecastSnapshot and TriggeredConditions.
			var forecastSnapshot types.ForecastSnapshot
			var triggeredConditions []types.Condition
			extractPayloadFields(payloadJSON, &forecastSnapshot, &triggeredConditions)

			acc = &notifAccumulator{
				item: &types.NotificationHistoryItem{
					ID:                  id,
					EventType:           types.EventType(eventType),
					SentAt:              createdAt,
					ForecastSnapshot:    forecastSnapshot,
					TriggeredConditions: triggeredConditions,
					Channels:            []types.DeliverySummary{},
				},
				order: len(orderedIDs),
			}
			itemMap[id] = acc
			orderedIDs = append(orderedIDs, id)
		}

		// Add delivery summary if the join produced a non-null delivery row.
		if channelType != nil && status != nil {
			acc.item.Channels = append(acc.item.Channels, types.DeliverySummary{
				Channel: *channelType,
				Status:  *status,
				SentAt:  deliveredAt,
			})
		}
	}
	if err := rows.Err(); err != nil {
		return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "error iterating notification rows", err)
	}

	// Build the result slice in the original order.
	var results []*types.NotificationHistoryItem
	for _, id := range orderedIDs {
		results = append(results, itemMap[id].item)
	}

	// Determine pagination info using limit+1 strategy.
	pageInfo := types.PageInfo{}
	if len(results) > limit {
		pageInfo.HasMore = true
		pageInfo.NextCursor = results[limit-1].SentAt.Format(time.RFC3339Nano)
		results = results[:limit]
	}

	return results, pageInfo, nil
}

// CancelDeferredDeliveries sets status='skipped' for all 'deferred' deliveries
// associated with the given WatchPoint. Used during Resume to prevent stale
// alert floods from Quiet Hours.
//
// This cancels deliveries across ALL notifications for the WatchPoint, not just
// a single notification, because the entire WatchPoint's deferred queue should
// be cleared on resume.
func (r *NotificationRepository) CancelDeferredDeliveries(ctx context.Context, watchpointID string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE notification_deliveries SET
			status = 'skipped',
			failure_reason = 'cancelled_on_resume'
		 WHERE notification_id IN (
			SELECT id FROM notifications WHERE watchpoint_id = $1
		 )
		 AND status = 'deferred'`,
		watchpointID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to cancel deferred deliveries", err)
	}
	return nil
}

// DeleteBefore hard-deletes notifications older than the cutoff time.
// Also cascade-deletes associated notification_deliveries (via ON DELETE CASCADE).
// Used for retention cleanup (MAINT-003). Returns the count of deleted records.
func (r *NotificationRepository) DeleteBefore(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`DELETE FROM notifications WHERE created_at < $1`,
		cutoff,
	)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to delete old notifications", err)
	}
	return tag.RowsAffected(), nil
}

// extractPayloadFields parses the notification payload JSONB to extract
// ForecastSnapshot and TriggeredConditions for the history DTO.
// If parsing fails, fields are left at their zero values (graceful degradation).
func extractPayloadFields(payloadJSON []byte, forecast *types.ForecastSnapshot, conditions *[]types.Condition) {
	if len(payloadJSON) == 0 {
		return
	}

	// The payload follows NotificationPayload schema.
	// We extract forecast_snapshot and conditions_evaluated.
	var payload struct {
		ForecastSnapshot types.ForecastSnapshot `json:"forecast_snapshot"`
		Conditions       []struct {
			Variable  string  `json:"variable"`
			Operator  string  `json:"operator"`
			Threshold []float64 `json:"threshold"`
			Matched   bool    `json:"matched"`
			Unit      string  `json:"unit"`
		} `json:"conditions_evaluated"`
	}

	if err := json.Unmarshal(payloadJSON, &payload); err != nil {
		// Graceful degradation: if payload is malformed, return zero values.
		return
	}

	*forecast = payload.ForecastSnapshot

	// Convert conditions_evaluated to []Condition. Only include matched conditions,
	// as the history DTO's field is "TriggeredConditions".
	for _, c := range payload.Conditions {
		if c.Matched {
			*conditions = append(*conditions, types.Condition{
				Variable:  c.Variable,
				Operator:  types.ConditionOperator(c.Operator),
				Threshold: c.Threshold,
				Unit:      c.Unit,
			})
		}
	}
}

// scanDeliveryFromRows scans a single notification_deliveries row from a
// pgx.Rows result set. Handles nullable columns using pointer types.
func scanDeliveryFromRows(rows pgx.Rows) (*types.NotificationDelivery, error) {
	var (
		d             types.NotificationDelivery
		channelType   string
		lastAttemptAt *time.Time
		failureReason *string
		providerMsgID *string
		nextRetryAt   *time.Time
		deliveredAt   *time.Time
		channelConfig []byte // JSONB column, read but stored in extended struct
	)

	err := rows.Scan(
		&d.ID,
		&d.NotificationID,
		&channelType,
		&d.Status,
		&d.AttemptCount,
		&lastAttemptAt,
		&failureReason,
		&providerMsgID,
		&nextRetryAt,
		&deliveredAt,
		&channelConfig,
	)
	if err != nil {
		return nil, err
	}

	d.ChannelType = types.ChannelType(channelType)
	if lastAttemptAt != nil {
		d.LastAttemptAt = *lastAttemptAt
	}
	if failureReason != nil {
		d.FailureReason = *failureReason
	}
	if providerMsgID != nil {
		d.ProviderMsgID = *providerMsgID
	}
	if nextRetryAt != nil {
		d.NextRetryAt = *nextRetryAt
	}
	if deliveredAt != nil {
		d.DeliveredAt = *deliveredAt
	}
	if channelConfig != nil {
		_ = json.Unmarshal(channelConfig, &d.ChannelConfig)
	}

	return &d, nil
}

// notifTemplateSet returns the template_set value for a notification,
// defaulting to "default" if not set in the notification's payload.
func notifTemplateSet(n *types.Notification) string {
	if n.TemplateSet != "" {
		return n.TemplateSet
	}
	return "default"
}

// deliveryStatusOrDefault returns the status or "pending" if empty.
func deliveryStatusOrDefault(status string) string {
	if status == "" {
		return "pending"
	}
	return status
}

// deliveryChannelConfig returns the channel_config JSONB value for a delivery.
// Returns an empty JSON object if no config is set.
func deliveryChannelConfig(d *types.NotificationDelivery) []byte {
	if d.ChannelConfig != nil {
		b, err := json.Marshal(d.ChannelConfig)
		if err == nil {
			return b
		}
	}
	return []byte("{}")
}

