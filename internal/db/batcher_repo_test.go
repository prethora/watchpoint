package db

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"strconv"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"watchpoint/internal/types"
)

// ============================================================
// Helper: Tile ID computation (mirrors the DB GENERATED ALWAYS column)
// ============================================================

// computeTileID replicates the PostgreSQL generated column logic:
//
//	FLOOR((90.0 - lat) / 22.5)::INT || '.' || FLOOR(
//	  CASE WHEN lon >= 0 THEN lon ELSE 360.0 + lon END / 45.0
//	)::INT
//
// This is used in tests to compute expected tile distributions from random
// lat/lon values, ensuring our test expectations match what PostgreSQL would
// produce.
func computeTileID(lat, lon float64) string {
	latIdx := int(math.Floor((90.0 - lat) / 22.5))
	adjustedLon := lon
	if adjustedLon < 0 {
		adjustedLon = 360.0 + adjustedLon
	}
	lonIdx := int(math.Floor(adjustedLon / 45.0))

	return strconv.Itoa(latIdx) + "." + strconv.Itoa(lonIdx)
}

// batcherTileMockRows implements pgx.Rows for tile count queries (tile_id, count).
// Reuses the same pattern as tileMockRows from watchpoint_repo_test.go but is
// defined here to keep the batcher tests self-contained and avoid cross-file
// dependency issues.
type batcherTileMockRows struct {
	data []struct {
		tileID string
		count  int
	}
	idx     int
	closed  bool
	scanErr error
	errVal  error
}

func (r *batcherTileMockRows) Next() bool {
	if r.closed {
		return false
	}
	r.idx++
	return r.idx < len(r.data)
}

func (r *batcherTileMockRows) Scan(dest ...any) error {
	if r.scanErr != nil {
		return r.scanErr
	}
	if r.idx >= 0 && r.idx < len(r.data) {
		*dest[0].(*string) = r.data[r.idx].tileID
		*dest[1].(*int) = r.data[r.idx].count
		return nil
	}
	return errors.New("no current row")
}

func (r *batcherTileMockRows) Close()                                       { r.closed = true }
func (r *batcherTileMockRows) Err() error                                   { return r.errVal }
func (r *batcherTileMockRows) CommandTag() pgconn.CommandTag                 { return pgconn.CommandTag{} }
func (r *batcherTileMockRows) FieldDescriptions() []pgconn.FieldDescription { return nil }
func (r *batcherTileMockRows) RawValues() [][]byte                           { return nil }
func (r *batcherTileMockRows) Values() ([]any, error)                        { return nil, nil }
func (r *batcherTileMockRows) Conn() *pgx.Conn                              { return nil }

// ============================================================
// GetActiveTileCounts Tests
// ============================================================

// TestBatcherRepository_GetActiveTileCounts_1000WatchPoints simulates the
// scenario described in the definition of done: seed 1000 random WatchPoints,
// compute the expected tile distribution using the same tile ID formula as
// PostgreSQL, and verify the repository returns the matching map.
//
// Since we run with mocked DBTX (no Docker DB), we:
//  1. Generate 1000 random (lat, lon) pairs.
//  2. Compute tile IDs using computeTileID (mirrors PostgreSQL's GENERATED ALWAYS).
//  3. Build the expected distribution map.
//  4. Feed that distribution into mock rows (simulating what the DB would return).
//  5. Assert the returned map matches exactly.
func TestBatcherRepository_GetActiveTileCounts_1000WatchPoints(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	// Step 1: Generate 1000 random WatchPoint locations and compute expected
	// tile distribution.
	rng := rand.New(rand.NewSource(42)) // Deterministic seed for reproducibility
	expectedDistribution := make(map[string]int)

	for i := 0; i < 1000; i++ {
		lat := rng.Float64()*180 - 90   // [-90, 90)
		lon := rng.Float64()*360 - 180  // [-180, 180)
		tileID := computeTileID(lat, lon)
		expectedDistribution[tileID]++
	}

	// Step 2: Convert expected distribution to mock row data.
	rowData := make([]struct {
		tileID string
		count  int
	}, 0, len(expectedDistribution))
	for tileID, count := range expectedDistribution {
		rowData = append(rowData, struct {
			tileID string
			count  int
		}{tileID, count})
	}

	mockRows := &batcherTileMockRows{data: rowData, idx: -1}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	// Step 3: Execute and verify.
	result, err := repo.GetActiveTileCounts(ctx)
	require.NoError(t, err)

	// Verify total count sums to 1000.
	totalCount := 0
	for _, count := range result {
		totalCount += count
	}
	assert.Equal(t, 1000, totalCount, "total WatchPoint count should be 1000")

	// Verify the returned map matches the expected distribution exactly.
	assert.Equal(t, len(expectedDistribution), len(result),
		"number of distinct tiles should match")

	for tileID, expectedCount := range expectedDistribution {
		actualCount, exists := result[tileID]
		require.True(t, exists, "tile %s should exist in result", tileID)
		assert.Equal(t, expectedCount, actualCount,
			"count for tile %s should match", tileID)
	}

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_Success verifies basic happy path
// with a small set of known tiles.
func TestBatcherRepository_GetActiveTileCounts_Success(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	mockRows := &batcherTileMockRows{
		data: []struct {
			tileID string
			count  int
		}{
			{"1.5", 10},
			{"2.3", 5},
			{"0.0", 1},
			{"7.7", 500}, // Hot tile
		},
		idx: -1,
	}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 4)
	assert.Equal(t, 10, result["1.5"])
	assert.Equal(t, 5, result["2.3"])
	assert.Equal(t, 1, result["0.0"])
	assert.Equal(t, 500, result["7.7"])

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_Empty verifies the zero state
// behavior per 06-batcher.md Section 7.2: empty result is not an error.
func TestBatcherRepository_GetActiveTileCounts_Empty(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	mockRows := &batcherTileMockRows{idx: -1}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.NoError(t, err)
	assert.Empty(t, result, "empty DB should return empty map, not nil")

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_DBError verifies that database
// connection errors are properly wrapped in AppError with ErrCodeInternalDB.
func TestBatcherRepository_GetActiveTileCounts_DBError(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).
		Return((*batcherTileMockRows)(nil), errors.New("connection refused"))

	result, err := repo.GetActiveTileCounts(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	assert.Contains(t, appErr.Message, "failed to get active tile counts")

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_ScanError verifies that errors
// during row scanning are properly handled and wrapped.
func TestBatcherRepository_GetActiveTileCounts_ScanError(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	mockRows := &batcherTileMockRows{
		data: []struct {
			tileID string
			count  int
		}{
			{"1.5", 10},
		},
		idx:     -1,
		scanErr: errors.New("unexpected column type"),
	}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	assert.Contains(t, appErr.Message, "failed to scan tile count row")

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_RowIterationError verifies that
// errors surfaced by rows.Err() (e.g., network issues mid-fetch) are handled.
func TestBatcherRepository_GetActiveTileCounts_RowIterationError(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	mockRows := &batcherTileMockRows{
		data: []struct {
			tileID string
			count  int
		}{
			{"1.5", 10},
		},
		idx:    -1,
		errVal: errors.New("network timeout during fetch"),
	}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.Error(t, err)
	assert.Nil(t, result)

	var appErr *types.AppError
	require.True(t, errors.As(err, &appErr))
	assert.Equal(t, types.ErrCodeInternalDB, appErr.Code)
	assert.Contains(t, appErr.Message, "error iterating tile count rows")

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_SingleTile verifies the case where
// all WatchPoints fall into a single tile (extreme hot tile scenario).
func TestBatcherRepository_GetActiveTileCounts_SingleTile(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	mockRows := &batcherTileMockRows{
		data: []struct {
			tileID string
			count  int
		}{
			{"3.4", 5000}, // Single hot tile with 5000 WatchPoints
		},
		idx: -1,
	}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 1)
	assert.Equal(t, 5000, result["3.4"])

	dbMock.AssertExpectations(t)
}

// TestBatcherRepository_GetActiveTileCounts_AllTiles verifies the maximum
// possible tile grid (8 lat bands x 8 lon bands = 64 tiles) all populated.
func TestBatcherRepository_GetActiveTileCounts_AllTiles(t *testing.T) {
	dbMock := new(mockDBTX)
	repo := NewBatcherRepository(dbMock)
	ctx := context.Background()

	// The tile grid is 8x8 (lat: 0-7, lon: 0-7) based on 22.5-degree lat
	// bands and 45-degree lon bands.
	rowData := make([]struct {
		tileID string
		count  int
	}, 0, 64)
	expectedTotal := 0
	for latIdx := 0; latIdx < 8; latIdx++ {
		for lonIdx := 0; lonIdx < 8; lonIdx++ {
			tileID := strconv.Itoa(latIdx) + "." + strconv.Itoa(lonIdx)
			count := (latIdx+1)*(lonIdx+1) + 1 // Arbitrary non-zero distribution
			rowData = append(rowData, struct {
				tileID string
				count  int
			}{tileID, count})
			expectedTotal += count
		}
	}

	mockRows := &batcherTileMockRows{data: rowData, idx: -1}
	dbMock.On("Query", ctx, mock.AnythingOfType("string"), mock.Anything).Return(mockRows, nil)

	result, err := repo.GetActiveTileCounts(ctx)
	require.NoError(t, err)
	assert.Len(t, result, 64, "should have all 64 tiles")

	// Verify total.
	totalCount := 0
	for _, count := range result {
		totalCount += count
	}
	assert.Equal(t, expectedTotal, totalCount)

	dbMock.AssertExpectations(t)
}

// ============================================================
// Tile ID Computation Tests (validates test helper correctness)
// ============================================================

func TestComputeTileID_KnownValues(t *testing.T) {
	tests := []struct {
		name     string
		lat      float64
		lon      float64
		expected string
	}{
		{
			name:     "Origin (0,0) - Equator, Prime Meridian",
			lat:      0,
			lon:      0,
			expected: "4.0",
		},
		{
			name:     "North Pole",
			lat:      89.99,
			lon:      0,
			expected: "0.0",
		},
		{
			name:     "South Pole",
			lat:      -89.99,
			lon:      0,
			expected: "7.0",
		},
		{
			name:     "Portland OR (45.5, -122.7)",
			lat:      45.5231,
			lon:      -122.6765,
			expected: "1.5", // lat band 1, lon band 5
		},
		{
			name:     "Tokyo (35.7, 139.7)",
			lat:      35.6762,
			lon:      139.6503,
			expected: "2.3", // lat band 2, lon band 3
		},
		{
			name:     "Sydney (-33.9, 151.2)",
			lat:      -33.8688,
			lon:      151.2093,
			expected: "5.3", // lat band 5, lon band 3
		},
		{
			name:     "International Date Line positive",
			lat:      0,
			lon:      179.99,
			expected: "4.3",
		},
		{
			name:     "International Date Line negative",
			lat:      0,
			lon:      -179.99,
			expected: "4.4",
		},
		{
			name:     "Maximum negative coordinates",
			lat:      -90,
			lon:      -180,
			expected: "8.4", // Edge: floor((90+90)/22.5) = 8, floor((360-180)/45) = 4
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := computeTileID(tc.lat, tc.lon)
			assert.Equal(t, tc.expected, result,
				"computeTileID(%f, %f) should be %s", tc.lat, tc.lon, tc.expected)
		})
	}
}

// TestComputeTileID_BoundaryValues checks tile boundaries to ensure the
// tile ID formula handles boundary points correctly.
func TestComputeTileID_BoundaryValues(t *testing.T) {
	// At exact tile boundary (22.5 degree increments)
	assert.Equal(t, "2.0", computeTileID(45.0, 0))   // floor((90-45)/22.5) = 2
	assert.Equal(t, "3.0", computeTileID(22.5, 0))    // floor((90-22.5)/22.5) = 3
	assert.Equal(t, "4.0", computeTileID(0, 0))       // floor((90-0)/22.5) = 4
	assert.Equal(t, "5.0", computeTileID(-22.5, 0))   // floor((90+22.5)/22.5) = 5
	assert.Equal(t, "4.1", computeTileID(0, 45))      // floor(45/45) = 1
	assert.Equal(t, "4.2", computeTileID(0, 90))      // floor(90/45) = 2
}
