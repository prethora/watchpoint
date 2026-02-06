package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"watchpoint/internal/types"
)

// ListWatchPointsParams defines the filtering and pagination parameters for
// listing WatchPoints. Matches the struct defined in 05b-api-watchpoints.md
// Section 4.4.
type ListWatchPointsParams struct {
	Status []types.Status `json:"status"`
	Tags   []string       `json:"tags"`
	TileID string         `json:"tile_id"`
	Limit  int            `json:"limit"`
	Cursor string         `json:"cursor"`
}

// WatchPointRepository provides data access for the watchpoints table.
// It implements the WatchPointRepository interface defined in
// 02-foundation-db.md Section 9.2 and extended in 05b-api-watchpoints.md Section 3.1.
//
// The tile_id column is a DB-generated column computed from location coordinates
// and is NEVER written by Go code. It is read-only in the Go struct.
type WatchPointRepository struct {
	db DBTX
}

// NewWatchPointRepository creates a new WatchPointRepository backed by the
// given database connection (pool or transaction).
func NewWatchPointRepository(db DBTX) *WatchPointRepository {
	return &WatchPointRepository{db: db}
}

// wpColumns defines the standard set of columns selected for watchpoint queries.
// NOTE: tile_id is included for reads but NEVER in INSERT/UPDATE statements
// because it is a GENERATED ALWAYS column in PostgreSQL.
const wpColumns = `w.id, w.organization_id, w.name,
	w.location_lat, w.location_lon, w.location_display_name,
	w.timezone, w.tile_id,
	w.time_window_start, w.time_window_end, w.monitor_config,
	w.conditions, w.condition_logic,
	w.channels, w.template_set, w.preferences,
	w.status, w.test_mode, w.tags, w.config_version, w.source,
	w.created_at, w.updated_at, w.archived_at, w.archived_reason`

// scanWatchPoint scans a single watchpoint row into a types.WatchPoint struct.
// The columns must match the order defined in wpColumns.
func scanWatchPoint(row pgx.Row) (*types.WatchPoint, error) {
	var wp types.WatchPoint
	var (
		locationDisplayName *string
		timeWindowStart     *time.Time
		timeWindowEnd       *time.Time
		monitorConfig       *types.MonitorConfig
		templateSet         *string
		preferences         *types.Preferences
		source              *string
		archivedReason      *string
	)

	err := row.Scan(
		&wp.ID,
		&wp.OrganizationID,
		&wp.Name,
		&wp.Location.Lat,
		&wp.Location.Lon,
		&locationDisplayName,
		&wp.Timezone,
		&wp.TileID,
		&timeWindowStart,
		&timeWindowEnd,
		&monitorConfig,
		&wp.Conditions,
		&wp.ConditionLogic,
		&wp.Channels,
		&templateSet,
		&preferences,
		&wp.Status,
		&wp.TestMode,
		&wp.Tags,
		&wp.ConfigVersion,
		&source,
		&wp.CreatedAt,
		&wp.UpdatedAt,
		&wp.ArchivedAt,
		&archivedReason,
	)
	if err != nil {
		return nil, err
	}

	// Hydrate optional fields from nullable columns.
	if locationDisplayName != nil {
		wp.Location.DisplayName = *locationDisplayName
	}
	if timeWindowStart != nil && timeWindowEnd != nil {
		wp.TimeWindow = &types.TimeWindow{
			Start: *timeWindowStart,
			End:   *timeWindowEnd,
		}
	}
	if monitorConfig != nil {
		wp.MonitorConfig = monitorConfig
	}
	if templateSet != nil {
		wp.TemplateSet = *templateSet
	}
	if preferences != nil {
		wp.NotificationPrefs = preferences
	}
	if source != nil {
		wp.Source = *source
	}
	if archivedReason != nil {
		wp.ArchivedReason = *archivedReason
	}

	return &wp, nil
}

// scanWatchPointFromRows scans a single row from a pgx.Rows result set.
// Uses the same column ordering as scanWatchPoint but operates on pgx.Rows.
func scanWatchPointFromRows(rows pgx.Rows) (*types.WatchPoint, error) {
	var wp types.WatchPoint
	var (
		locationDisplayName *string
		timeWindowStart     *time.Time
		timeWindowEnd       *time.Time
		monitorConfig       *types.MonitorConfig
		templateSet         *string
		preferences         *types.Preferences
		source              *string
		archivedReason      *string
	)

	err := rows.Scan(
		&wp.ID,
		&wp.OrganizationID,
		&wp.Name,
		&wp.Location.Lat,
		&wp.Location.Lon,
		&locationDisplayName,
		&wp.Timezone,
		&wp.TileID,
		&timeWindowStart,
		&timeWindowEnd,
		&monitorConfig,
		&wp.Conditions,
		&wp.ConditionLogic,
		&wp.Channels,
		&templateSet,
		&preferences,
		&wp.Status,
		&wp.TestMode,
		&wp.Tags,
		&wp.ConfigVersion,
		&source,
		&wp.CreatedAt,
		&wp.UpdatedAt,
		&wp.ArchivedAt,
		&archivedReason,
	)
	if err != nil {
		return nil, err
	}

	if locationDisplayName != nil {
		wp.Location.DisplayName = *locationDisplayName
	}
	if timeWindowStart != nil && timeWindowEnd != nil {
		wp.TimeWindow = &types.TimeWindow{
			Start: *timeWindowStart,
			End:   *timeWindowEnd,
		}
	}
	if monitorConfig != nil {
		wp.MonitorConfig = monitorConfig
	}
	if templateSet != nil {
		wp.TemplateSet = *templateSet
	}
	if preferences != nil {
		wp.NotificationPrefs = preferences
	}
	if source != nil {
		wp.Source = *source
	}
	if archivedReason != nil {
		wp.ArchivedReason = *archivedReason
	}

	return &wp, nil
}

// Create inserts a new WatchPoint record. The caller must set the ID (prefixed
// UUID, e.g. "wp_...") and required fields before calling.
//
// IMPORTANT: tile_id is NOT included in the INSERT statement because it is a
// GENERATED ALWAYS column computed by PostgreSQL from location_lat and location_lon.
//
// Used by WPLC-001 (Event Mode) and WPLC-002 (Monitor Mode) creation flows.
func (r *WatchPointRepository) Create(ctx context.Context, wp *types.WatchPoint) error {
	_, err := r.db.Exec(ctx,
		`INSERT INTO watchpoints (
			id, organization_id, name,
			location_lat, location_lon, location_display_name,
			timezone,
			time_window_start, time_window_end, monitor_config,
			conditions, condition_logic,
			channels, template_set, preferences,
			status, test_mode, tags, config_version, source,
			created_at, updated_at
		) VALUES (
			$1, $2, $3,
			$4, $5, $6,
			$7,
			$8, $9, $10,
			$11, $12,
			$13, $14, $15,
			$16, $17, $18, $19, $20,
			COALESCE($21, NOW()), COALESCE($22, NOW())
		)`,
		wp.ID,
		wp.OrganizationID,
		wp.Name,
		wp.Location.Lat,
		wp.Location.Lon,
		nilIfEmpty(wp.Location.DisplayName),
		wp.Timezone,
		timeWindowStart(wp.TimeWindow),
		timeWindowEnd(wp.TimeWindow),
		wp.MonitorConfig,
		wp.Conditions,
		wp.ConditionLogic,
		wp.Channels,
		nilIfEmpty(wp.TemplateSet),
		wp.NotificationPrefs,
		wp.Status,
		wp.TestMode,
		wp.Tags,
		wp.ConfigVersion,
		nilIfEmpty(wp.Source),
		nilIfZeroTime(wp.CreatedAt),
		nilIfZeroTime(wp.UpdatedAt),
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to create watchpoint", err)
	}
	return nil
}

// GetByID retrieves a WatchPoint by its ID, scoped to the given organization.
// Excludes soft-deleted records. Returns ErrCodeNotFoundWatchpoint if not found.
//
// The orgID parameter enforces access control at the DB level, ensuring a
// WatchPoint cannot be accessed across organization boundaries.
func (r *WatchPointRepository) GetByID(ctx context.Context, id string, orgID string) (*types.WatchPoint, error) {
	row := r.db.QueryRow(ctx,
		`SELECT `+wpColumns+`
		 FROM watchpoints w
		 WHERE w.id = $1 AND w.organization_id = $2 AND w.deleted_at IS NULL`,
		id, orgID,
	)

	wp, err := scanWatchPoint(row)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
		}
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to retrieve watchpoint", err)
	}
	return wp, nil
}

// Update applies changes to an existing WatchPoint. The caller passes the full
// WatchPoint struct; mutable fields are written. The updated_at timestamp is
// set by the database. config_version is managed by a DB trigger and is NOT
// updated here.
//
// IMPORTANT: tile_id is NOT included in the UPDATE because it is a GENERATED
// ALWAYS column. The DB recomputes it automatically if location changes.
//
// Used by WPLC-003 (partial update) and WPLC-004 (update while triggered).
func (r *WatchPointRepository) Update(ctx context.Context, wp *types.WatchPoint) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			name = $1,
			location_lat = $2,
			location_lon = $3,
			location_display_name = $4,
			timezone = $5,
			time_window_start = $6,
			time_window_end = $7,
			monitor_config = $8,
			conditions = $9,
			condition_logic = $10,
			channels = $11,
			template_set = $12,
			preferences = $13,
			status = $14,
			tags = $15,
			source = $16,
			updated_at = NOW()
		 WHERE id = $17 AND organization_id = $18 AND deleted_at IS NULL`,
		wp.Name,
		wp.Location.Lat,
		wp.Location.Lon,
		nilIfEmpty(wp.Location.DisplayName),
		wp.Timezone,
		timeWindowStart(wp.TimeWindow),
		timeWindowEnd(wp.TimeWindow),
		wp.MonitorConfig,
		wp.Conditions,
		wp.ConditionLogic,
		wp.Channels,
		nilIfEmpty(wp.TemplateSet),
		wp.NotificationPrefs,
		wp.Status,
		wp.Tags,
		nilIfEmpty(wp.Source),
		wp.ID,
		wp.OrganizationID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update watchpoint", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
	}
	return nil
}

// Delete performs a soft delete by setting deleted_at = NOW() and status = 'archived'.
// Scoped to the organization for access control.
// Setting status to 'archived' ensures immediate exclusion from Batcher active indexes.
//
// Used by WPLC-008 flow.
func (r *WatchPointRepository) Delete(ctx context.Context, id string, orgID string) error {
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			deleted_at = NOW(),
			status = 'archived',
			updated_at = NOW()
		 WHERE id = $1 AND organization_id = $2 AND deleted_at IS NULL`,
		id, orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to delete watchpoint", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
	}
	return nil
}

// List retrieves WatchPoints for an organization with optional filtering and
// cursor-based pagination. Results are ordered by created_at DESC (newest first).
//
// Supports the following filters:
//   - Status: filter by one or more statuses
//   - Tags: filter by tag containment (all specified tags must be present)
//   - TileID: filter by tile ID (used by batcher)
//   - Cursor: created_at timestamp of the last item from the previous page
//
// Uses limit+1 fetch strategy to determine HasMore without a separate COUNT query.
// Excludes soft-deleted records.
func (r *WatchPointRepository) List(ctx context.Context, orgID string, params ListWatchPointsParams) ([]*types.WatchPoint, types.PageInfo, error) {
	limit := params.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit > 100 {
		limit = 100
	}

	var conditions []string
	var args []any
	argIdx := 1

	// Organization scope is always enforced.
	conditions = append(conditions, fmt.Sprintf("w.organization_id = $%d", argIdx))
	args = append(args, orgID)
	argIdx++

	// Always exclude soft-deleted records.
	conditions = append(conditions, "w.deleted_at IS NULL")

	// Status filter: match any of the provided statuses.
	if len(params.Status) > 0 {
		placeholders := make([]string, len(params.Status))
		for i, s := range params.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, s)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("w.status IN (%s)", strings.Join(placeholders, ", ")))
	}

	// Tag filter: require all specified tags to be present (array containment).
	if len(params.Tags) > 0 {
		conditions = append(conditions, fmt.Sprintf("w.tags @> $%d", argIdx))
		args = append(args, params.Tags)
		argIdx++
	}

	// Tile ID filter.
	if params.TileID != "" {
		conditions = append(conditions, fmt.Sprintf("w.tile_id = $%d", argIdx))
		args = append(args, params.TileID)
		argIdx++
	}

	// Cursor-based pagination: fetch items older than the cursor timestamp.
	if params.Cursor != "" {
		cursorTime, err := time.Parse(time.RFC3339Nano, params.Cursor)
		if err != nil {
			return nil, types.PageInfo{}, types.NewAppError(
				types.ErrCodeValidationMissingField,
				"invalid cursor format; expected RFC3339 timestamp",
				err,
			)
		}
		conditions = append(conditions, fmt.Sprintf("w.created_at < $%d", argIdx))
		args = append(args, cursorTime)
		argIdx++
	}

	whereClause := "WHERE " + strings.Join(conditions, " AND ")

	// Fetch limit+1 to detect if there are more results.
	query := fmt.Sprintf(
		`SELECT %s
		 FROM watchpoints w
		 %s
		 ORDER BY w.created_at DESC
		 LIMIT $%d`,
		wpColumns,
		whereClause,
		argIdx,
	)
	args = append(args, limit+1)

	rows, err := r.db.Query(ctx, query, args...)
	if err != nil {
		return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "failed to list watchpoints", err)
	}
	defer rows.Close()

	var results []*types.WatchPoint
	for rows.Next() {
		wp, scanErr := scanWatchPointFromRows(rows)
		if scanErr != nil {
			return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "failed to scan watchpoint row", scanErr)
		}
		results = append(results, wp)
	}
	if err := rows.Err(); err != nil {
		return nil, types.PageInfo{}, types.NewAppError(types.ErrCodeInternalDB, "error iterating watchpoint rows", err)
	}

	// Determine pagination info.
	pageInfo := types.PageInfo{}
	if len(results) > limit {
		pageInfo.HasMore = true
		// The cursor is the created_at of the last item we will return.
		pageInfo.NextCursor = results[limit-1].CreatedAt.Format(time.RFC3339Nano)
		results = results[:limit] // Trim the extra row
	}

	return results, pageInfo, nil
}

// CreateBatch inserts multiple WatchPoints in a single atomic INSERT statement.
// The caller must pre-populate all required fields (including IDs) before calling.
//
// Returns:
//   - createdIndices: indices (0-based) of successfully inserted WatchPoints
//   - failedIndices: map of index -> error for WatchPoints that failed DB insertion
//   - err: non-nil only if a systemic error prevents the entire batch (not per-item)
//
// Per WPLC-011 flow simulation: Uses a single INSERT statement for atomicity.
// tile_id is NOT included because it is a GENERATED ALWAYS column.
func (r *WatchPointRepository) CreateBatch(ctx context.Context, wps []*types.WatchPoint) ([]int, map[int]error, error) {
	if len(wps) == 0 {
		return nil, nil, nil
	}

	// Build a multi-row INSERT with numbered placeholders.
	const colCount = 22 // Number of columns in the INSERT
	var sb strings.Builder
	sb.WriteString(`INSERT INTO watchpoints (
		id, organization_id, name,
		location_lat, location_lon, location_display_name,
		timezone,
		time_window_start, time_window_end, monitor_config,
		conditions, condition_logic,
		channels, template_set, preferences,
		status, test_mode, tags, config_version, source,
		created_at, updated_at
	) VALUES `)

	args := make([]any, 0, len(wps)*colCount)
	for i, wp := range wps {
		if i > 0 {
			sb.WriteString(", ")
		}
		base := i * colCount
		sb.WriteString("(")
		for j := 0; j < colCount; j++ {
			if j > 0 {
				sb.WriteString(", ")
			}
			if j == 20 { // created_at
				sb.WriteString(fmt.Sprintf("COALESCE($%d, NOW())", base+j+1))
			} else if j == 21 { // updated_at
				sb.WriteString(fmt.Sprintf("COALESCE($%d, NOW())", base+j+1))
			} else {
				sb.WriteString(fmt.Sprintf("$%d", base+j+1))
			}
		}
		sb.WriteString(")")

		args = append(args,
			wp.ID,
			wp.OrganizationID,
			wp.Name,
			wp.Location.Lat,
			wp.Location.Lon,
			nilIfEmpty(wp.Location.DisplayName),
			wp.Timezone,
			timeWindowStart(wp.TimeWindow),
			timeWindowEnd(wp.TimeWindow),
			wp.MonitorConfig,
			wp.Conditions,
			wp.ConditionLogic,
			wp.Channels,
			nilIfEmpty(wp.TemplateSet),
			wp.NotificationPrefs,
			wp.Status,
			wp.TestMode,
			wp.Tags,
			wp.ConfigVersion,
			nilIfEmpty(wp.Source),
			nilIfZeroTime(wp.CreatedAt),
			nilIfZeroTime(wp.UpdatedAt),
		)
	}

	_, err := r.db.Exec(ctx, sb.String(), args...)
	if err != nil {
		return nil, nil, types.NewAppError(types.ErrCodeInternalDB, "failed to batch create watchpoints", err)
	}

	// All rows inserted successfully in the atomic statement.
	createdIndices := make([]int, len(wps))
	for i := range wps {
		createdIndices[i] = i
	}
	return createdIndices, nil, nil
}

// UpdateStatusBatch updates the status of multiple WatchPoints in a single query.
// The testMode parameter enforces strict environment isolation, ensuring Live and
// Test Mode WatchPoints are never mixed in a single batch update.
//
// Returns the number of rows affected.
//
// SQL: UPDATE watchpoints SET status=$status, updated_at=NOW()
//
//	WHERE id = ANY($ids) AND organization_id = $orgID AND test_mode = $testMode
func (r *WatchPointRepository) UpdateStatusBatch(ctx context.Context, ids []string, orgID string, status types.Status, testMode bool) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			status = $1,
			updated_at = NOW()
		 WHERE id = ANY($2)
		   AND organization_id = $3
		   AND test_mode = $4
		   AND deleted_at IS NULL`,
		status, ids, orgID, testMode,
	)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to batch update watchpoint status", err)
	}
	return tag.RowsAffected(), nil
}

// DeleteBatch soft-deletes multiple WatchPoints in a single query.
// Sets deleted_at=NOW() and status='archived' for immediate exclusion from
// Batcher active indexes. The testMode parameter enforces environment isolation.
//
// Returns the number of rows affected.
func (r *WatchPointRepository) DeleteBatch(ctx context.Context, ids []string, orgID string, testMode bool) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			deleted_at = NOW(),
			status = 'archived',
			updated_at = NOW()
		 WHERE id = ANY($1)
		   AND organization_id = $2
		   AND test_mode = $3
		   AND deleted_at IS NULL`,
		ids, orgID, testMode,
	)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to batch delete watchpoints", err)
	}
	return tag.RowsAffected(), nil
}

// UpdateChannelConfig performs a JSONB update on a specific channel within the
// channels array. The mutationFn receives the current channel config
// (map[string]any) and must return the updated config. This enables
// zero-downtime secret rotation (HOOK-001 flow) without overwriting the entire
// channels array.
//
// The approach:
//  1. Read the current channels JSONB from the row
//  2. Find the channel by ID within the array
//  3. Pass the channel's config to mutationFn
//  4. Reconstruct the channels array with the mutated config
//  5. Write the updated array back in a single UPDATE
//
// IMPORTANT: For true atomicity under concurrent access, the caller MUST wrap
// this call in a serializable transaction. The DBTX interface accepts both
// pool connections and transactions, so the handler should use a pgx.Tx when
// calling this method. The interface contract (mutationFn in Go) requires
// reading data into Go before mutation, making a single-statement SQL approach
// impractical.
//
// This method does NOT increment config_version (separation of concerns per
// HOOK-001 flow specification).
func (r *WatchPointRepository) UpdateChannelConfig(ctx context.Context, wpID string, channelID string, mutationFn func(map[string]any) (map[string]any, error)) error {
	// Step 1: Read the current channels JSONB.
	var channelsJSON []byte
	err := r.db.QueryRow(ctx,
		`SELECT channels FROM watchpoints WHERE id = $1 AND deleted_at IS NULL`,
		wpID,
	).Scan(&channelsJSON)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found", nil)
		}
		return types.NewAppError(types.ErrCodeInternalDB, "failed to read watchpoint channels", err)
	}

	// Step 2: Unmarshal into channel array to find the target channel.
	var channels []map[string]any
	if err := json.Unmarshal(channelsJSON, &channels); err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to parse channels JSONB", err)
	}

	// Step 3: Find the target channel and apply the mutation.
	found := false
	for i, ch := range channels {
		if id, ok := ch["id"].(string); ok && id == channelID {
			found = true
			config, ok := ch["config"].(map[string]any)
			if !ok {
				config = make(map[string]any)
			}
			updatedConfig, err := mutationFn(config)
			if err != nil {
				return err
			}
			channels[i]["config"] = updatedConfig
			break
		}
	}
	if !found {
		return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "channel not found in watchpoint", nil)
	}

	// Step 4: Marshal and write back the updated channels.
	updatedJSON, err := json.Marshal(channels)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to marshal updated channels", err)
	}

	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET channels = $1, updated_at = NOW()
		 WHERE id = $2 AND deleted_at IS NULL`,
		updatedJSON, wpID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to update watchpoint channels", err)
	}
	if tag.RowsAffected() == 0 {
		return types.NewAppError(types.ErrCodeNotFoundWatchpoint, "watchpoint not found during channel update", nil)
	}

	return nil
}

// GetBatch performs a vectorized fetch of multiple WatchPoints by ID.
// Used by bulk cloning operations to efficiently retrieve source WatchPoints
// in a single query rather than N individual fetches.
func (r *WatchPointRepository) GetBatch(ctx context.Context, ids []string, orgID string) ([]*types.WatchPoint, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+wpColumns+`
		 FROM watchpoints w
		 WHERE w.id = ANY($1)
		   AND w.organization_id = $2
		   AND w.deleted_at IS NULL`,
		ids, orgID,
	)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to batch get watchpoints", err)
	}
	defer rows.Close()

	var results []*types.WatchPoint
	for rows.Next() {
		wp, scanErr := scanWatchPointFromRows(rows)
		if scanErr != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan watchpoint row in batch", scanErr)
		}
		results = append(results, wp)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating watchpoint batch rows", err)
	}

	return results, nil
}

// UpdateTagsBatch atomically updates tags for WatchPoints matching the given filter.
// Uses PostgreSQL array operators for atomic tag manipulation without fetch-modify-save loops.
// Returns the count of WatchPoints updated.
func (r *WatchPointRepository) UpdateTagsBatch(ctx context.Context, orgID string, filter types.BulkFilter, addTags []string, removeTags []string) (int64, error) {
	var setClauses []string
	var conditions []string
	var args []any
	argIdx := 1

	// Build the tag update expression.
	// Start with the existing tags column.
	tagsExpr := "tags"

	// Add tags using array concatenation (tags || $addTags).
	if len(addTags) > 0 {
		tagsExpr = fmt.Sprintf("(%s || $%d)", tagsExpr, argIdx)
		args = append(args, addTags)
		argIdx++
	}

	// Remove tags using array subtraction (... - $removeTags elements).
	// PostgreSQL does not have a native array subtraction operator for text[],
	// so we use array_remove for each tag to remove. For simplicity and
	// correctness, we use a subquery approach.
	if len(removeTags) > 0 {
		tagsExpr = fmt.Sprintf("array(SELECT unnest(%s) EXCEPT SELECT unnest($%d::text[]))", tagsExpr, argIdx)
		args = append(args, removeTags)
		argIdx++
	}

	setClauses = append(setClauses, fmt.Sprintf("tags = %s", tagsExpr))
	setClauses = append(setClauses, "updated_at = NOW()")

	// Build WHERE conditions.
	conditions = append(conditions, fmt.Sprintf("organization_id = $%d", argIdx))
	args = append(args, orgID)
	argIdx++

	conditions = append(conditions, "deleted_at IS NULL")

	if len(filter.Status) > 0 {
		placeholders := make([]string, len(filter.Status))
		for i, s := range filter.Status {
			placeholders[i] = fmt.Sprintf("$%d", argIdx)
			args = append(args, s)
			argIdx++
		}
		conditions = append(conditions, fmt.Sprintf("status IN (%s)", strings.Join(placeholders, ", ")))
	}

	if len(filter.Tags) > 0 {
		conditions = append(conditions, fmt.Sprintf("tags @> $%d", argIdx))
		args = append(args, filter.Tags)
		argIdx++
	}

	query := fmt.Sprintf(
		`UPDATE watchpoints SET %s WHERE %s`,
		strings.Join(setClauses, ", "),
		strings.Join(conditions, " AND "),
	)

	tag, err := r.db.Exec(ctx, query, args...)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to batch update watchpoint tags", err)
	}
	return tag.RowsAffected(), nil
}

// PauseAllByOrgID sets status='paused' for all active WatchPoints belonging to
// the organization. Used during organization deletion to immediately stop
// evaluation overhead. The reason parameter distinguishes user pauses from
// billing enforcements.
func (r *WatchPointRepository) PauseAllByOrgID(ctx context.Context, orgID string, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			status = 'paused',
			paused_reason = $1,
			updated_at = NOW()
		 WHERE organization_id = $2
		   AND status = 'active'
		   AND deleted_at IS NULL`,
		reason, orgID,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to pause all watchpoints for organization", err)
	}
	return nil
}

// ResumeAllByOrgID resumes only WatchPoints paused for the specific reason
// (e.g., billing_delinquency) to prevent reactivating user-paused items.
func (r *WatchPointRepository) ResumeAllByOrgID(ctx context.Context, orgID string, reason string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			status = 'active',
			paused_reason = NULL,
			updated_at = NOW()
		 WHERE organization_id = $1
		   AND status = 'paused'
		   AND paused_reason = $2
		   AND deleted_at IS NULL`,
		orgID, reason,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to resume watchpoints for organization", err)
	}
	return nil
}

// PauseExcessByOrgID pauses the most recently created WatchPoints until count <= limit.
// Uses LIFO (Last-In-First-Out) strategy, pausing the most recently created
// WatchPoints first to preserve long-standing monitors.
// Sets paused_reason='billing_delinquency'.
func (r *WatchPointRepository) PauseExcessByOrgID(ctx context.Context, orgID string, limit int) error {
	_, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			status = 'paused',
			paused_reason = 'billing_delinquency',
			updated_at = NOW()
		 WHERE id IN (
			SELECT id FROM watchpoints
			WHERE organization_id = $1
			  AND status = 'active'
			  AND deleted_at IS NULL
			ORDER BY created_at DESC
			OFFSET $2
		 )`,
		orgID, limit,
	)
	if err != nil {
		return types.NewAppError(types.ErrCodeInternalDB, "failed to pause excess watchpoints", err)
	}
	return nil
}

// ListByTile retrieves WatchPoints for a specific tile with pagination.
// Used by the Batcher to iterate through WatchPoints in a tile.
func (r *WatchPointRepository) ListByTile(ctx context.Context, tileID string, limit, offset int) ([]*types.WatchPoint, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+wpColumns+`
		 FROM watchpoints w
		 WHERE w.tile_id = $1
		   AND w.status = 'active'
		   AND w.deleted_at IS NULL
		 ORDER BY w.id
		 LIMIT $2 OFFSET $3`,
		tileID, limit, offset,
	)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to list watchpoints by tile", err)
	}
	defer rows.Close()

	var results []*types.WatchPoint
	for rows.Next() {
		wp, scanErr := scanWatchPointFromRows(rows)
		if scanErr != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan watchpoint row", scanErr)
		}
		results = append(results, wp)
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating watchpoint tile rows", err)
	}

	return results, nil
}

// GetTileCounts returns a map of tile_id -> count of active WatchPoints.
// Used by the Batcher to determine which tiles need evaluation.
func (r *WatchPointRepository) GetTileCounts(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT tile_id, COUNT(*) as cnt
		 FROM watchpoints
		 WHERE status = 'active' AND deleted_at IS NULL
		 GROUP BY tile_id`)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to get tile counts", err)
	}
	defer rows.Close()

	result := make(map[string]int)
	for rows.Next() {
		var tileID string
		var count int
		if err := rows.Scan(&tileID, &count); err != nil {
			return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to scan tile count row", err)
		}
		result[tileID] = count
	}
	if err := rows.Err(); err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "error iterating tile count rows", err)
	}

	return result, nil
}

// ArchiveExpired sets status='archived' and archived_at=NOW() for event-mode
// WatchPoints whose time_window_end has passed the cutoff time.
// Returns the number of archived records.
func (r *WatchPointRepository) ArchiveExpired(ctx context.Context, cutoff time.Time) (int64, error) {
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoints SET
			status = 'archived',
			archived_at = NOW(),
			archived_reason = 'expired',
			updated_at = NOW()
		 WHERE time_window_end IS NOT NULL
		   AND time_window_end < $1
		   AND status != 'archived'
		   AND deleted_at IS NULL`,
		cutoff,
	)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to archive expired watchpoints", err)
	}
	return tag.RowsAffected(), nil
}

// PruneSeenThreats removes expired threat entries from the seen_threats JSONB
// array in watchpoint_evaluation_state. Supports MAINT-007 by trimming stale
// threat hashes older than maxAge to prevent unbounded growth.
func (r *WatchPointRepository) PruneSeenThreats(ctx context.Context, maxAge time.Duration) (int64, error) {
	cutoff := time.Now().Add(-maxAge)
	tag, err := r.db.Exec(ctx,
		`UPDATE watchpoint_evaluation_state SET
			seen_threat_hashes = '[]'::jsonb
		 WHERE last_evaluated_at < $1
		   AND seen_threat_hashes IS NOT NULL
		   AND seen_threat_hashes != '[]'::jsonb`,
		cutoff,
	)
	if err != nil {
		return 0, types.NewAppError(types.ErrCodeInternalDB, "failed to prune seen threats", err)
	}
	return tag.RowsAffected(), nil
}

// timeWindowStart extracts the Start time from a TimeWindow pointer, returning nil
// if the pointer is nil. Used for nullable time_window_start column.
func timeWindowStart(tw *types.TimeWindow) *time.Time {
	if tw == nil {
		return nil
	}
	return &tw.Start
}

// timeWindowEnd extracts the End time from a TimeWindow pointer, returning nil
// if the pointer is nil. Used for nullable time_window_end column.
func timeWindowEnd(tw *types.TimeWindow) *time.Time {
	if tw == nil {
		return nil
	}
	return &tw.End
}
