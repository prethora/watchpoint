package types

import (
	"encoding/json"
	"time"
)

// PageInfo contains pagination metadata for list responses.
type PageInfo struct {
	HasMore    bool   `json:"has_more"`
	NextCursor string `json:"next_cursor,omitempty"`
	TotalItems *int   `json:"total_items,omitempty"`
}

// ListResponse is a generic paginated response wrapper.
type ListResponse[T any] struct {
	Data     []T      `json:"data"`
	PageInfo PageInfo `json:"pagination"`
}

// ResponseMeta contains non-blocking metadata returned with API responses.
type ResponseMeta struct {
	Warnings   []string  `json:"warnings,omitempty"`
	Pagination *PageInfo `json:"pagination,omitempty"`
}

// BulkFilter defines criteria for selecting WatchPoints in bulk operations.
type BulkFilter struct {
	Status []Status `json:"status,omitempty"`
	Tags   []string `json:"tags,omitempty"`
}

// AuditEvent records an action taken on a resource for auditing purposes.
type AuditEvent struct {
	ID           string          `json:"id"`
	Actor        Actor           `json:"actor"`
	Action       string          `json:"action"`
	ResourceID   string          `json:"resource_id"`
	ResourceType string          `json:"resource_type"`
	OldValue     json.RawMessage `json:"old_value,omitempty"`
	NewValue     json.RawMessage `json:"new_value,omitempty"`
	Timestamp    time.Time       `json:"timestamp"`
}

// Standard Audit Action Strings.
// Handlers MUST use these action strings for consistency.
const (
	// Single-resource actions
	AuditActionWatchPointCreated = "watchpoint.created"
	AuditActionWatchPointUpdated = "watchpoint.updated"
	AuditActionWatchPointPaused  = "watchpoint.paused"
	AuditActionWatchPointResumed = "watchpoint.resumed"
	AuditActionWatchPointDeleted = "watchpoint.deleted"
	AuditActionWatchPointCloned  = "watchpoint.cloned" // WPLC-010

	// Bulk actions - emit AGGREGATED events containing count/filter in metadata.
	// Do NOT emit individual events per item in bulk operations.
	AuditActionWatchPointBulkCreated = "watchpoint.bulk_created"
	AuditActionWatchPointBulkPaused  = "watchpoint.bulk_paused"
	AuditActionWatchPointBulkResumed = "watchpoint.bulk_resumed"
	AuditActionWatchPointBulkDeleted    = "watchpoint.bulk_deleted"
	AuditActionWatchPointBulkCloned     = "watchpoint.bulk_cloned"
	AuditActionWatchPointBulkTagUpdated = "watchpoint.bulk_tags_updated"
)

// AuditQueryFilters defines parameters for querying audit log entries.
type AuditQueryFilters struct {
	OrganizationID string    `json:"organization_id"`
	ActorID        string    `json:"actor_id,omitempty"`
	ResourceType   string    `json:"resource_type,omitempty"`
	StartTime      time.Time `json:"start_time,omitempty"`
	EndTime        time.Time `json:"end_time,omitempty"`
	Limit          int       `json:"limit,omitempty"`
	Cursor         string    `json:"cursor,omitempty"`
}
