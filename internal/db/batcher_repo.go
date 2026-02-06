package db

import (
	"context"

	"watchpoint/internal/types"
)

// BatcherRepository provides data access methods specific to the Batcher Lambda.
// It encapsulates the optimized queries needed for tile-based evaluation dispatch,
// separated from the general WatchPointRepository to maintain clean ownership boundaries.
//
// The primary query uses an Index-Only Scan on the idx_watchpoints_tile_active
// partial index: (tile_id, status) WHERE status = 'active' AND deleted_at IS NULL.
//
// Referenced by:
//   - 06-batcher.md Section 5 (Core Service Logic)
//   - Flow EVAL-001 Step 4 (Queries Active Tiles)
type BatcherRepository struct {
	db DBTX
}

// NewBatcherRepository creates a new BatcherRepository backed by the given
// database connection (pool or transaction).
func NewBatcherRepository(db DBTX) *BatcherRepository {
	return &BatcherRepository{db: db}
}

// GetActiveTileCounts returns a map of tile_id -> count of active WatchPoints.
// This is the critical query for the Batcher's evaluation dispatch loop.
//
// The query is designed to use an Index-Only Scan on the partial index
// idx_watchpoints_tile_active, which covers (tile_id, status) and is filtered
// to status = 'active' AND deleted_at IS NULL. This means the query can be
// satisfied entirely from the index without touching the heap (table data),
// making it extremely fast even with millions of WatchPoints.
//
// The query explicitly matches the EVAL-001 flow simulation Step 4:
//
//	SELECT tile_id, COUNT(*) FROM watchpoints
//	WHERE status='active' AND deleted_at IS NULL
//	GROUP BY tile_id
//
// Per 06-batcher.md Section 7.2 (Zero State): If this returns an empty map
// (0 rows), the Batcher proceeds successfully with 0 messages but still
// emits the ForecastReady metric to prevent Dead Man's Switch false positives.
//
// NOTE: This does NOT use the deprecated active_tiles table strategy.
// The tile_id is a GENERATED ALWAYS column computed from location coordinates
// and the partial index ensures optimal performance.
func (r *BatcherRepository) GetActiveTileCounts(ctx context.Context) (map[string]int, error) {
	rows, err := r.db.Query(ctx,
		`SELECT tile_id, COUNT(*) as cnt
		 FROM watchpoints
		 WHERE status = 'active' AND deleted_at IS NULL
		 GROUP BY tile_id`)
	if err != nil {
		return nil, types.NewAppError(types.ErrCodeInternalDB, "failed to get active tile counts", err)
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
