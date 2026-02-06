// Package forecasts implements the Zarr-based forecast data reader.
// It provides the ForecastReader interface that abstracts low-level Zarr v2
// chunk reading from S3, including grid math, zstd decompression, float32
// parsing, and bilinear interpolation.
package forecasts

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/klauspost/compress/zstd"

	"watchpoint/internal/types"
)

// Grid constants for the global 0.25-degree forecast grid.
// These match the Zarr output contract defined in architecture/11-runpod.md.
const (
	// GridResolution is the spacing between grid points in degrees.
	GridResolution = 0.25

	// GridLatCount is the number of latitude points (90.0 to -90.0 inclusive).
	GridLatCount = 721

	// GridLonCount is the number of longitude points (-180.0 to 179.75 inclusive).
	GridLonCount = 1440

	// GridLatStart is the northernmost latitude (grid index 0).
	// Latitude is descending: index 0 = 90.0, index 720 = -90.0.
	GridLatStart = 90.0

	// GridLonStart is the westernmost longitude (grid index 0).
	// Longitude is ascending: index 0 = -180.0, index 1439 = 179.75.
	GridLonStart = -180.0

	// TileLatDeg is the latitude extent of a single tile in degrees.
	TileLatDeg = 22.5

	// TileLonDeg is the longitude extent of a single tile in degrees.
	TileLonDeg = 45.0

	// ChunkLatSize is the number of latitude grid points per chunk (22.5 / 0.25 = 90).
	ChunkLatSize = 90

	// ChunkLonSize is the number of longitude grid points per chunk (45.0 / 0.25 = 180).
	ChunkLonSize = 180

	// TileLatCount is the number of tile rows (180.0 / 22.5 = 8).
	TileLatCount = 8

	// TileLonCount is the number of tile columns (360.0 / 45.0 = 8).
	TileLonCount = 8

	// float32ByteSize is the number of bytes per float32 value.
	float32ByteSize = 4

	// haloPixels is the 1-pixel overlap padding on each side of a chunk
	// to support bilinear interpolation at tile edges.
	haloPixels = 1
)

// RunContext identifies the specific dataset to read.
// Matches the spec from architecture/05c-api-forecasts.md Section 4.2.
type RunContext struct {
	Model       types.ForecastType
	Timestamp   time.Time
	StoragePath string
	Version     string // Handles schema evolution
}

// ForecastDataPoint represents a single timestep in the forecast series.
// Fields correspond to canonical variables defined in architecture/01-foundation-types.md.
type ForecastDataPoint struct {
	ValidTime                time.Time `json:"valid_time"`
	Source                   string    `json:"source"`
	PrecipitationProbability *float64  `json:"precipitation_probability,omitempty"`
	PrecipitationMM          *float64  `json:"precipitation_mm,omitempty"`
	TemperatureC             *float64  `json:"temperature_c,omitempty"`
	WindSpeedKmh             *float64  `json:"wind_speed_kmh,omitempty"`
	Humidity                 *float64  `json:"humidity_percent,omitempty"`
	CloudCover               *float64  `json:"cloud_cover_percent,omitempty"`
}

// ForecastReader abstracts low-level Zarr array reading.
// Implementations must handle AWS SDK context propagation.
type ForecastReader interface {
	// ReadPoint extracts time series for a single coordinate.
	ReadPoint(ctx context.Context, rc RunContext, loc types.Location, vars []string) ([]ForecastDataPoint, error)

	// ReadTile extracts data for multiple points within the same tile (batch optimization).
	ReadTile(ctx context.Context, rc RunContext, tileID string, locs []types.LocationIdent, vars []string) (map[string][]ForecastDataPoint, error)
}

// S3Client abstracts S3 object retrieval for testability.
type S3Client interface {
	// GetObject fetches an object from S3 by bucket and key.
	// Returns the object body as an io.ReadCloser.
	GetObject(ctx context.Context, bucket, key string) (io.ReadCloser, error)
}

// ZarrArrayMeta holds the minimal .zarray metadata we need.
// We only parse what ReadPoint/ReadTile requires.
type ZarrArrayMeta struct {
	Chunks      []int  `json:"chunks"`       // [time, lat, lon]
	Shape       []int  `json:"shape"`        // [time, lat, lon]
	DType       string `json:"dtype"`        // e.g., "<f4"
	Compressor  any    `json:"compressor"`   // {"id": "zstd", ...}
	FillValue   any    `json:"fill_value"`   // null or a number
	Order       string `json:"order"`        // "C" (row-major)
	ZarrFormat  int    `json:"zarr_format"`  // 2
}

// zarrReader implements ForecastReader using S3 + Zarr v2.
type zarrReader struct {
	s3     S3Client
	bucket string
	logger *slog.Logger

	// decoderPool provides reusable zstd decoders to avoid repeated allocations.
	decoderPool sync.Pool
}

// NewZarrReader creates a new ForecastReader backed by S3-hosted Zarr stores.
func NewZarrReader(s3Client S3Client, bucket string, logger *slog.Logger) ForecastReader {
	return &zarrReader{
		s3:     s3Client,
		bucket: bucket,
		logger: logger,
		decoderPool: sync.Pool{
			New: func() any {
				d, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1))
				if err != nil {
					// This should never fail with nil input and default options.
					panic(fmt.Sprintf("failed to create zstd decoder: %v", err))
				}
				return d
			},
		},
	}
}

// CalculateTileID computes the tile ID for a given lat/lon, matching the
// PostgreSQL generated column formula from architecture/02-foundation-db.md.
//
// Formula:
//
//	lat_index = FLOOR((90.0 - lat) / 22.5)
//	lon_norm  = lon >= 0 ? lon : 360.0 + lon
//	lon_index = FLOOR(lon_norm / 45.0)
//	tile_id   = "{lat_index}.{lon_index}"
func CalculateTileID(lat, lon float64) string {
	latIndex := int(math.Floor((90.0 - lat) / TileLatDeg))
	lonNorm := lon
	if lonNorm < 0 {
		lonNorm = 360.0 + lonNorm
	}
	lonIndex := int(math.Floor(lonNorm / TileLonDeg))

	// Clamp to valid range.
	if latIndex < 0 {
		latIndex = 0
	}
	if latIndex >= TileLatCount {
		latIndex = TileLatCount - 1
	}
	if lonIndex < 0 {
		lonIndex = 0
	}
	if lonIndex >= TileLonCount {
		lonIndex = TileLonCount - 1
	}

	return fmt.Sprintf("%d.%d", latIndex, lonIndex)
}

// gridIndex represents row/col indices into the global grid.
type gridIndex struct {
	row int // latitude index (0 = 90.0N, 720 = 90.0S)
	col int // longitude index (0 = -180.0, 1439 = 179.75)
}

// latLonToGridIndex maps a latitude/longitude pair to the nearest grid indices
// using the global 0.25-degree grid.
//
// Latitude is descending: row 0 = 90.0, row 720 = -90.0
// Longitude is ascending: col 0 = -180.0, col 1439 = 179.75
//
// Returns fractional grid indices for use in bilinear interpolation.
func latLonToFractionalGrid(lat, lon float64) (fracRow, fracCol float64) {
	// Latitude: descending from 90.0
	fracRow = (GridLatStart - lat) / GridResolution

	// Longitude: ascending from -180.0
	fracCol = (lon - GridLonStart) / GridResolution

	return fracRow, fracCol
}

// clampGridIndex ensures a grid index is within valid bounds.
func clampGridIndex(idx, max int) int {
	if idx < 0 {
		return 0
	}
	if idx >= max {
		return max - 1
	}
	return idx
}

// chunkIndex identifies which Zarr chunk contains a given grid point.
type chunkIndex struct {
	latChunk  int // chunk row index
	lonChunk  int // chunk column index
	localRow  int // row within the chunk (0-based, excluding halo)
	localCol  int // col within the chunk (0-based, excluding halo)
}

// gridToChunkIndex maps a global grid index to its containing chunk
// and the local position within that chunk.
func gridToChunkIndex(row, col int) chunkIndex {
	return chunkIndex{
		latChunk: row / ChunkLatSize,
		lonChunk: col / ChunkLonSize,
		localRow: row % ChunkLatSize,
		localCol: col % ChunkLonSize,
	}
}

// chunkKey constructs the S3 key for a specific Zarr chunk.
// Format: {storage_path}/{variable}/{lat_chunk}.{lon_chunk}
//
// For a Zarr v2 store with dimensions [time, lat, lon], the chunk key
// uses the pattern: {var_name}/{time_chunk}.{lat_chunk}.{lon_chunk}
// Since time chunk size = full horizon, time_chunk is always 0.
func chunkKey(storagePath, variable string, latChunk, lonChunk int) string {
	return fmt.Sprintf("%s/%s/0.%d.%d", storagePath, variable, latChunk, lonChunk)
}

// zarrMetaKey returns the S3 key for a variable's .zarray metadata.
func zarrMetaKey(storagePath, variable string) string {
	return fmt.Sprintf("%s/%s/.zarray", storagePath, variable)
}

// ReadPoint extracts time series for a single coordinate using bilinear interpolation.
func (r *zarrReader) ReadPoint(ctx context.Context, rc RunContext, loc types.Location, vars []string) ([]ForecastDataPoint, error) {
	if err := validateLocation(loc.Lat, loc.Lon); err != nil {
		return nil, err
	}

	// Step 1: Load array metadata to determine time steps.
	if len(vars) == 0 {
		return nil, &types.AppError{
			Code:    types.ErrCodeValidationMissingField,
			Message: "at least one variable is required",
		}
	}

	meta, err := r.loadArrayMeta(ctx, rc.StoragePath, vars[0])
	if err != nil {
		return nil, err
	}

	timeSteps := meta.Shape[0]

	// Step 2: Map lat/lon to fractional grid indices.
	fracRow, fracCol := latLonToFractionalGrid(loc.Lat, loc.Lon)

	// Step 3: Determine the 4 corner grid points for bilinear interpolation.
	row0 := clampGridIndex(int(math.Floor(fracRow)), GridLatCount)
	row1 := clampGridIndex(row0+1, GridLatCount)
	col0 := clampGridIndex(int(math.Floor(fracCol)), GridLonCount)
	col1 := clampGridIndex(col0+1, GridLonCount)

	// Fractional distances for weighting.
	rowFrac := fracRow - math.Floor(fracRow)
	colFrac := fracCol - math.Floor(fracCol)

	// If clamped to the same point, set fractions to 0 (nearest neighbor at edge).
	if row0 == row1 {
		rowFrac = 0
	}
	if col0 == col1 {
		colFrac = 0
	}

	// Step 4: Determine which chunks we need.
	// All 4 corners might be in the same chunk, or span up to 4 chunks.
	cornerPoints := [4]gridIndex{
		{row: row0, col: col0},
		{row: row0, col: col1},
		{row: row1, col: col0},
		{row: row1, col: col1},
	}

	// Step 5: For each variable, fetch required chunks and interpolate.
	// Build result data points indexed by time step.
	results := make([]ForecastDataPoint, timeSteps)
	for t := range results {
		results[t].Source = string(rc.Model)
	}

	for _, varName := range vars {
		// Collect unique chunks needed for this variable.
		chunkData, err := r.fetchChunksForCorners(ctx, rc.StoragePath, varName, cornerPoints[:])
		if err != nil {
			return nil, fmt.Errorf("reading variable %s: %w", varName, err)
		}

		// Extract interpolated values for each time step.
		for t := 0; t < timeSteps; t++ {
			val, err := bilinearInterpolateFromChunks(
				chunkData, cornerPoints, meta,
				t, rowFrac, colFrac,
			)
			if err != nil {
				r.logger.Warn("interpolation failed",
					"variable", varName,
					"time_step", t,
					"error", err,
				)
				continue // Leave as nil (omitted in JSON)
			}

			setDataPointField(&results[t], varName, val)
		}
	}

	return results, nil
}

// ReadTile extracts data for multiple points within the same tile.
// This is the batch optimization path: fetch the chunk once, extract for all points.
func (r *zarrReader) ReadTile(ctx context.Context, rc RunContext, tileID string, locs []types.LocationIdent, vars []string) (map[string][]ForecastDataPoint, error) {
	if len(locs) == 0 {
		return make(map[string][]ForecastDataPoint), nil
	}
	if len(vars) == 0 {
		return nil, &types.AppError{
			Code:    types.ErrCodeValidationMissingField,
			Message: "at least one variable is required",
		}
	}

	// Load metadata from the first variable to get time steps.
	meta, err := r.loadArrayMeta(ctx, rc.StoragePath, vars[0])
	if err != nil {
		return nil, err
	}

	timeSteps := meta.Shape[0]

	// Initialize results for each location.
	results := make(map[string][]ForecastDataPoint, len(locs))
	for _, loc := range locs {
		pts := make([]ForecastDataPoint, timeSteps)
		for t := range pts {
			pts[t].Source = string(rc.Model)
		}
		results[loc.ID] = pts
	}

	// For each variable, fetch the tile's chunk(s) and extract for all locations.
	for _, varName := range vars {
		// Pre-compute interpolation setup for each location.
		type locInterp struct {
			id      string
			corners [4]gridIndex
			rowFrac float64
			colFrac float64
		}

		interpSetups := make([]locInterp, 0, len(locs))
		// Collect all unique chunks needed across all locations.
		allCorners := make([]gridIndex, 0, len(locs)*4)

		for _, loc := range locs {
			if err := validateLocation(loc.Lat, loc.Lon); err != nil {
				// Skip invalid locations; leave their results empty.
				r.logger.Warn("skipping invalid location in tile read",
					"location_id", loc.ID,
					"lat", loc.Lat,
					"lon", loc.Lon,
					"error", err,
				)
				continue
			}

			fracRow, fracCol := latLonToFractionalGrid(loc.Lat, loc.Lon)
			row0 := clampGridIndex(int(math.Floor(fracRow)), GridLatCount)
			row1 := clampGridIndex(row0+1, GridLatCount)
			col0 := clampGridIndex(int(math.Floor(fracCol)), GridLonCount)
			col1 := clampGridIndex(col0+1, GridLonCount)

			rowFrac := fracRow - math.Floor(fracRow)
			colFrac := fracCol - math.Floor(fracCol)
			if row0 == row1 {
				rowFrac = 0
			}
			if col0 == col1 {
				colFrac = 0
			}

			corners := [4]gridIndex{
				{row: row0, col: col0},
				{row: row0, col: col1},
				{row: row1, col: col0},
				{row: row1, col: col1},
			}

			interpSetups = append(interpSetups, locInterp{
				id:      loc.ID,
				corners: corners,
				rowFrac: rowFrac,
				colFrac: colFrac,
			})
			allCorners = append(allCorners, corners[:]...)
		}

		// Fetch all unique chunks needed for this variable.
		chunkData, err := r.fetchChunksForCorners(ctx, rc.StoragePath, varName, allCorners)
		if err != nil {
			return nil, fmt.Errorf("reading variable %s for tile %s: %w", varName, tileID, err)
		}

		// Extract interpolated values for each location and time step.
		for _, setup := range interpSetups {
			pts := results[setup.id]
			for t := 0; t < timeSteps; t++ {
				val, err := bilinearInterpolateFromChunks(
					chunkData, setup.corners, meta,
					t, setup.rowFrac, setup.colFrac,
				)
				if err != nil {
					continue // Leave as nil
				}
				setDataPointField(&pts[t], varName, val)
			}
		}
	}

	return results, nil
}

// chunkCacheKey uniquely identifies a chunk for deduplication.
type chunkCacheKey struct {
	latChunk int
	lonChunk int
}

// fetchChunksForCorners fetches all unique chunks needed for a set of corner points.
// Returns a map from chunkCacheKey to decompressed float32 data.
func (r *zarrReader) fetchChunksForCorners(
	ctx context.Context,
	storagePath, variable string,
	corners []gridIndex,
) (map[chunkCacheKey][]float32, error) {
	// Deduplicate chunks.
	needed := make(map[chunkCacheKey]struct{})
	for _, corner := range corners {
		ci := gridToChunkIndex(corner.row, corner.col)
		needed[chunkCacheKey{latChunk: ci.latChunk, lonChunk: ci.lonChunk}] = struct{}{}
	}

	result := make(map[chunkCacheKey][]float32, len(needed))
	for key := range needed {
		data, err := r.fetchAndDecompressChunk(ctx, storagePath, variable, key.latChunk, key.lonChunk)
		if err != nil {
			return nil, err
		}
		result[key] = data
	}

	return result, nil
}

// fetchAndDecompressChunk fetches a single Zarr chunk from S3 and decompresses it.
func (r *zarrReader) fetchAndDecompressChunk(
	ctx context.Context,
	storagePath, variable string,
	latChunk, lonChunk int,
) ([]float32, error) {
	key := chunkKey(storagePath, variable, latChunk, lonChunk)

	body, err := r.s3.GetObject(ctx, r.bucket, key)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to fetch chunk %s: %v", key, err),
			Err:     err,
		}
	}
	defer body.Close()

	compressed, err := io.ReadAll(body)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to read chunk body %s: %v", key, err),
			Err:     err,
		}
	}

	// Decompress with zstd.
	decompressed, err := r.decompressZstd(compressed)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeInternalUnexpected,
			Message: fmt.Sprintf("failed to decompress chunk %s: %v", key, err),
			Err:     err,
		}
	}

	// Parse float32 values from raw bytes (little-endian, matching Zarr "<f4" dtype).
	floats, err := parseFloat32s(decompressed)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeInternalUnexpected,
			Message: fmt.Sprintf("failed to parse chunk %s: %v", key, err),
			Err:     err,
		}
	}

	return floats, nil
}

// decompressZstd decompresses zstd-compressed data using pooled decoders.
func (r *zarrReader) decompressZstd(data []byte) ([]byte, error) {
	decoder := r.decoderPool.Get().(*zstd.Decoder)
	defer r.decoderPool.Put(decoder)

	result, err := decoder.DecodeAll(data, nil)
	if err != nil {
		return nil, fmt.Errorf("zstd decompression failed: %w", err)
	}

	return result, nil
}

// parseFloat32s converts raw little-endian bytes into a slice of float32 values.
func parseFloat32s(data []byte) ([]float32, error) {
	if len(data)%float32ByteSize != 0 {
		return nil, fmt.Errorf("data length %d is not a multiple of %d bytes", len(data), float32ByteSize)
	}

	count := len(data) / float32ByteSize
	result := make([]float32, count)
	for i := 0; i < count; i++ {
		bits := binary.LittleEndian.Uint32(data[i*float32ByteSize : (i+1)*float32ByteSize])
		result[i] = math.Float32frombits(bits)
	}

	return result, nil
}

// bilinearInterpolateFromChunks performs bilinear interpolation across potentially
// multiple chunks for a single time step.
//
// The 4 corner points (topLeft, topRight, bottomLeft, bottomRight) are:
//
//	corners[0] = (row0, col0) - top-left
//	corners[1] = (row0, col1) - top-right
//	corners[2] = (row1, col0) - bottom-left
//	corners[3] = (row1, col1) - bottom-right
func bilinearInterpolateFromChunks(
	chunkData map[chunkCacheKey][]float32,
	corners [4]gridIndex,
	meta *ZarrArrayMeta,
	timeStep int,
	rowFrac, colFrac float64,
) (float64, error) {
	// Extract values from the 4 corners.
	vals := [4]float64{}
	for i, corner := range corners {
		ci := gridToChunkIndex(corner.row, corner.col)
		key := chunkCacheKey{latChunk: ci.latChunk, lonChunk: ci.lonChunk}
		chunk, ok := chunkData[key]
		if !ok {
			return 0, fmt.Errorf("missing chunk data for chunk (%d, %d)", ci.latChunk, ci.lonChunk)
		}

		// Compute flat index within chunk.
		// Chunk dimensions with halo: [timeSteps, ChunkLatSize + 2*halo, ChunkLonSize + 2*halo]
		// The local position within the chunk needs halo offset.
		chunkLatSize := meta.Chunks[1]
		chunkLonSize := meta.Chunks[2]

		localRow := ci.localRow + haloPixels
		localCol := ci.localCol + haloPixels

		flatIdx := timeStep*chunkLatSize*chunkLonSize + localRow*chunkLonSize + localCol

		if flatIdx < 0 || flatIdx >= len(chunk) {
			return 0, fmt.Errorf("chunk index %d out of bounds (chunk size %d)", flatIdx, len(chunk))
		}

		val := float64(chunk[flatIdx])
		if math.IsNaN(val) {
			return 0, fmt.Errorf("NaN value at grid point (%d, %d)", corner.row, corner.col)
		}

		vals[i] = val
	}

	// Bilinear interpolation:
	// f(x,y) = f(0,0)(1-x)(1-y) + f(1,0)(x)(1-y) + f(0,1)(1-x)(y) + f(1,1)(x)(y)
	// Where x = colFrac, y = rowFrac
	// corners: [0]=TL(r0,c0), [1]=TR(r0,c1), [2]=BL(r1,c0), [3]=BR(r1,c1)
	result := vals[0]*(1-rowFrac)*(1-colFrac) +
		vals[1]*(1-rowFrac)*colFrac +
		vals[2]*rowFrac*(1-colFrac) +
		vals[3]*rowFrac*colFrac

	return result, nil
}

// loadArrayMeta fetches and parses the .zarray metadata for a variable.
func (r *zarrReader) loadArrayMeta(ctx context.Context, storagePath, variable string) (*ZarrArrayMeta, error) {
	key := zarrMetaKey(storagePath, variable)

	body, err := r.s3.GetObject(ctx, r.bucket, key)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to fetch array metadata %s: %v", key, err),
			Err:     err,
		}
	}
	defer body.Close()

	data, err := io.ReadAll(body)
	if err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("failed to read array metadata %s: %v", key, err),
			Err:     err,
		}
	}

	var meta ZarrArrayMeta
	if err := json.Unmarshal(data, &meta); err != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeInternalUnexpected,
			Message: fmt.Sprintf("failed to parse .zarray metadata: %v", err),
			Err:     err,
		}
	}

	// Validate minimal expectations.
	if len(meta.Shape) != 3 || len(meta.Chunks) != 3 {
		return nil, &types.AppError{
			Code:    types.ErrCodeInternalUnexpected,
			Message: fmt.Sprintf("unexpected array dimensions: shape=%v chunks=%v", meta.Shape, meta.Chunks),
		}
	}

	return &meta, nil
}

// setDataPointField sets the appropriate field on a ForecastDataPoint
// based on the canonical variable name.
func setDataPointField(dp *ForecastDataPoint, variable string, val float64) {
	switch variable {
	case types.ZarrVarTemperatureC:
		dp.TemperatureC = &val
	case types.ZarrVarPrecipitationMM:
		dp.PrecipitationMM = &val
	case types.ZarrVarPrecipitationProb:
		dp.PrecipitationProbability = &val
	case types.ZarrVarWindSpeedKmh:
		dp.WindSpeedKmh = &val
	case types.ZarrVarHumidityPercent:
		dp.Humidity = &val
	case types.ZarrVarCloudCoverPercent:
		dp.CloudCover = &val
	}
}

// validateLocation checks that lat/lon are within valid bounds.
func validateLocation(lat, lon float64) error {
	if lat < -90 || lat > 90 {
		return &types.AppError{
			Code:    types.ErrCodeValidationInvalidLat,
			Message: fmt.Sprintf("latitude %f out of range [-90, 90]", lat),
		}
	}
	if lon < -180 || lon > 180 {
		return &types.AppError{
			Code:    types.ErrCodeValidationInvalidLon,
			Message: fmt.Sprintf("longitude %f out of range [-180, 180]", lon),
		}
	}
	return nil
}
