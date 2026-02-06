package forecasts

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"os"
	"testing"

	"github.com/klauspost/compress/zstd"

	"watchpoint/internal/types"
)

// --- Mock S3 Client ---

// mockS3Client implements S3Client for testing.
type mockS3Client struct {
	// objects maps "bucket/key" to raw bytes.
	objects map[string][]byte
	// failKeys causes GetObject to return an error for these keys.
	failKeys map[string]error
}

func newMockS3Client() *mockS3Client {
	return &mockS3Client{
		objects:  make(map[string][]byte),
		failKeys: make(map[string]error),
	}
}

func (m *mockS3Client) putObject(bucket, key string, data []byte) {
	m.objects[bucket+"/"+key] = data
}

func (m *mockS3Client) setFailKey(bucket, key string, err error) {
	m.failKeys[bucket+"/"+key] = err
}

func (m *mockS3Client) GetObject(_ context.Context, bucket, key string) (io.ReadCloser, error) {
	fullKey := bucket + "/" + key
	if err, ok := m.failKeys[fullKey]; ok {
		return nil, err
	}
	data, ok := m.objects[fullKey]
	if !ok {
		return nil, fmt.Errorf("s3: NoSuchKey: %s", fullKey)
	}
	return io.NopCloser(bytes.NewReader(data)), nil
}

// --- Test Helpers ---

// testLogger returns a discard logger for tests.
func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelError}))
}

// makeZarrayMeta creates a .zarray metadata JSON for testing.
func makeZarrayMeta(timeSteps, latChunkSize, lonChunkSize int) []byte {
	meta := ZarrArrayMeta{
		Chunks:     []int{timeSteps, latChunkSize, lonChunkSize},
		Shape:      []int{timeSteps, GridLatCount, GridLonCount},
		DType:      "<f4",
		Compressor: map[string]any{"id": "zstd", "level": 3},
		FillValue:  nil,
		Order:      "C",
		ZarrFormat: 2,
	}
	data, _ := json.Marshal(meta)
	return data
}

// compressZstd compresses data using zstd.
func compressZstd(data []byte) []byte {
	w, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest))
	if err != nil {
		panic(err)
	}
	defer w.Close()
	return w.EncodeAll(data, nil)
}

// makeChunkData creates a synthetic Zarr chunk with known float32 values.
// The chunk has dimensions [timeSteps, latSize, lonSize] and fills values
// using the provided function.
func makeChunkData(timeSteps, latSize, lonSize int, fillFn func(t, lat, lon int) float32) []byte {
	total := timeSteps * latSize * lonSize
	buf := make([]byte, total*float32ByteSize)
	for t := 0; t < timeSteps; t++ {
		for lat := 0; lat < latSize; lat++ {
			for lon := 0; lon < lonSize; lon++ {
				idx := t*latSize*lonSize + lat*lonSize + lon
				val := fillFn(t, lat, lon)
				binary.LittleEndian.PutUint32(buf[idx*4:(idx+1)*4], math.Float32bits(val))
			}
		}
	}
	return buf
}

// setupTestStore creates a mock S3 client with a complete Zarr store for one variable.
// Returns the mock client and the storage path used.
func setupTestStore(
	bucket, storagePath, variable string,
	timeSteps int,
	chunkLatSize, chunkLonSize int,
	fillFn func(t, lat, lon int) float32,
) *mockS3Client {
	s3 := newMockS3Client()

	// Put .zarray metadata.
	metaKey := zarrMetaKey(storagePath, variable)
	s3.putObject(bucket, metaKey, makeZarrayMeta(timeSteps, chunkLatSize, chunkLonSize))

	// Generate and put chunks for the entire grid.
	latChunks := (GridLatCount + ChunkLatSize - 1) / ChunkLatSize
	lonChunks := (GridLonCount + ChunkLonSize - 1) / ChunkLonSize

	for lc := 0; lc < latChunks; lc++ {
		for lnc := 0; lnc < lonChunks; lnc++ {
			raw := makeChunkData(timeSteps, chunkLatSize, chunkLonSize, fillFn)
			compressed := compressZstd(raw)
			key := chunkKey(storagePath, variable, lc, lnc)
			s3.putObject(bucket, key, compressed)
		}
	}

	return s3
}

// --- Test: CalculateTileID ---

func TestCalculateTileID(t *testing.T) {
	tests := []struct {
		name     string
		lat, lon float64
		want     string
	}{
		// Match the tile grid from architecture/watchpoint-platform-design-final.md
		{
			name: "NYC (40.7, -74.0) -> tile 2.2 area",
			lat:  40.7,
			lon:  -74.0,
			// lat_index = floor((90 - 40.7)/22.5) = floor(2.19) = 2
			// lon_norm = 360 + (-74.0) = 286.0
			// lon_index = floor(286.0/45.0) = floor(6.35) = 6
			want: "2.6",
		},
		{
			name: "Equator, Prime Meridian (0, 0)",
			lat:  0.0,
			lon:  0.0,
			// lat_index = floor((90-0)/22.5) = floor(4) = 4
			// lon_norm = 0.0
			// lon_index = floor(0/45) = 0
			want: "4.0",
		},
		{
			name: "North Pole (90, 0)",
			lat:  90.0,
			lon:  0.0,
			// lat_index = floor((90-90)/22.5) = floor(0) = 0
			// lon_index = floor(0/45) = 0
			want: "0.0",
		},
		{
			name: "South Pole (-90, 0)",
			lat:  -90.0,
			lon:  0.0,
			// lat_index = floor((90-(-90))/22.5) = floor(8) = 8 -> clamped to 7
			want: "7.0",
		},
		{
			name: "Date Line West (-89, -180)",
			lat:  -89.0,
			lon:  -180.0,
			// lat_index = floor((90-(-89))/22.5) = floor(7.95) = 7
			// lon_norm = 360 + (-180) = 180.0
			// lon_index = floor(180/45) = 4
			want: "7.4",
		},
		{
			name: "Positive Longitude (45, 135)",
			lat:  45.0,
			lon:  135.0,
			// lat_index = floor((90-45)/22.5) = floor(2) = 2
			// lon_index = floor(135/45) = 3
			want: "2.3",
		},
		{
			name: "Tile boundary exactly at 67.5N, 45E",
			lat:  67.5,
			lon:  45.0,
			// lat_index = floor((90-67.5)/22.5) = floor(1) = 1
			// lon_index = floor(45/45) = 1
			want: "1.1",
		},
		{
			name: "Slight offset inside tile",
			lat:  67.4,
			lon:  44.9,
			// lat_index = floor((90-67.4)/22.5) = floor(1.0044...) = 1
			// lon_index = floor(44.9/45) = 0
			want: "1.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := CalculateTileID(tt.lat, tt.lon)
			if got != tt.want {
				t.Errorf("CalculateTileID(%f, %f) = %q, want %q", tt.lat, tt.lon, got, tt.want)
			}
		})
	}
}

// --- Test: latLonToFractionalGrid ---

func TestLatLonToFractionalGrid(t *testing.T) {
	tests := []struct {
		name    string
		lat     float64
		lon     float64
		wantRow float64
		wantCol float64
	}{
		{
			name:    "North Pole",
			lat:     90.0,
			lon:     -180.0,
			wantRow: 0.0,
			wantCol: 0.0,
		},
		{
			name:    "South Pole",
			lat:     -90.0,
			lon:     179.75,
			wantRow: 720.0,
			wantCol: 1439.0,
		},
		{
			name:    "Equator, Date Line",
			lat:     0.0,
			lon:     0.0,
			wantRow: 360.0,
			wantCol: 720.0,
		},
		{
			name:    "Half step",
			lat:     89.875,
			lon:     -179.875,
			wantRow: 0.5,
			wantCol: 0.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotRow, gotCol := latLonToFractionalGrid(tt.lat, tt.lon)
			if math.Abs(gotRow-tt.wantRow) > 1e-9 {
				t.Errorf("fracRow = %f, want %f", gotRow, tt.wantRow)
			}
			if math.Abs(gotCol-tt.wantCol) > 1e-9 {
				t.Errorf("fracCol = %f, want %f", gotCol, tt.wantCol)
			}
		})
	}
}

// --- Test: gridToChunkIndex ---

func TestGridToChunkIndex(t *testing.T) {
	tests := []struct {
		name     string
		row, col int
		wantCI   chunkIndex
	}{
		{
			name: "Origin",
			row:  0, col: 0,
			wantCI: chunkIndex{latChunk: 0, lonChunk: 0, localRow: 0, localCol: 0},
		},
		{
			name: "Inside first chunk",
			row:  45, col: 90,
			wantCI: chunkIndex{latChunk: 0, lonChunk: 0, localRow: 45, localCol: 90},
		},
		{
			name: "Second chunk row start",
			row:  90, col: 0,
			wantCI: chunkIndex{latChunk: 1, lonChunk: 0, localRow: 0, localCol: 0},
		},
		{
			name: "Second chunk col start",
			row:  0, col: 180,
			wantCI: chunkIndex{latChunk: 0, lonChunk: 1, localRow: 0, localCol: 0},
		},
		{
			name: "Last grid point",
			row:  720, col: 1439,
			wantCI: chunkIndex{latChunk: 8, lonChunk: 7, localRow: 0, localCol: 179},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := gridToChunkIndex(tt.row, tt.col)
			if got != tt.wantCI {
				t.Errorf("gridToChunkIndex(%d, %d) = %+v, want %+v", tt.row, tt.col, got, tt.wantCI)
			}
		})
	}
}

// --- Test: parseFloat32s ---

func TestParseFloat32s(t *testing.T) {
	t.Run("valid data", func(t *testing.T) {
		values := []float32{1.0, 2.5, -3.14, 0.0}
		buf := make([]byte, len(values)*4)
		for i, v := range values {
			binary.LittleEndian.PutUint32(buf[i*4:(i+1)*4], math.Float32bits(v))
		}

		result, err := parseFloat32s(buf)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != len(values) {
			t.Fatalf("expected %d values, got %d", len(values), len(result))
		}
		for i, v := range values {
			if result[i] != v {
				t.Errorf("result[%d] = %f, want %f", i, result[i], v)
			}
		}
	})

	t.Run("invalid length", func(t *testing.T) {
		_, err := parseFloat32s([]byte{1, 2, 3}) // 3 bytes, not divisible by 4
		if err == nil {
			t.Error("expected error for invalid data length")
		}
	})

	t.Run("empty data", func(t *testing.T) {
		result, err := parseFloat32s([]byte{})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(result) != 0 {
			t.Errorf("expected empty result, got %d values", len(result))
		}
	})
}

// --- Test: bilinear interpolation ---

func TestBilinearInterpolation(t *testing.T) {
	// Create a simple 2x2 chunk with known values for one time step.
	// Values: TL=10, TR=20, BL=30, BR=40
	// With halo, the chunk is [1, 4, 4] (1 time step, 2+2 halo lat, 2+2 halo lon)
	chunkLatSize := 4 // 2 data + 2 halo
	chunkLonSize := 4

	meta := &ZarrArrayMeta{
		Chunks: []int{1, chunkLatSize, chunkLonSize},
		Shape:  []int{1, GridLatCount, GridLonCount},
	}

	// Fill: row=haloPixels, col=haloPixels => val = 10 (TL)
	//       row=haloPixels, col=haloPixels+1 => val = 20 (TR)
	//       row=haloPixels+1, col=haloPixels => val = 30 (BL)
	//       row=haloPixels+1, col=haloPixels+1 => val = 40 (BR)
	data := make([]float32, chunkLatSize*chunkLonSize)
	data[1*chunkLonSize+1] = 10 // (halo+0, halo+0) = TL
	data[1*chunkLonSize+2] = 20 // (halo+0, halo+1) = TR
	data[2*chunkLonSize+1] = 30 // (halo+1, halo+0) = BL
	data[2*chunkLonSize+2] = 40 // (halo+1, halo+1) = BR

	chunkMap := map[chunkCacheKey][]float32{
		{latChunk: 0, lonChunk: 0}: data,
	}

	corners := [4]gridIndex{
		{row: 0, col: 0}, // TL
		{row: 0, col: 1}, // TR
		{row: 1, col: 0}, // BL
		{row: 1, col: 1}, // BR
	}

	tests := []struct {
		name    string
		rowFrac float64
		colFrac float64
		want    float64
	}{
		{
			name:    "Exact top-left",
			rowFrac: 0.0, colFrac: 0.0,
			want: 10.0,
		},
		{
			name:    "Exact top-right",
			rowFrac: 0.0, colFrac: 1.0,
			want: 20.0,
		},
		{
			name:    "Exact bottom-left",
			rowFrac: 1.0, colFrac: 0.0,
			want: 30.0,
		},
		{
			name:    "Exact bottom-right",
			rowFrac: 1.0, colFrac: 1.0,
			want: 40.0,
		},
		{
			name:    "Center (0.5, 0.5)",
			rowFrac: 0.5, colFrac: 0.5,
			// 10*0.5*0.5 + 20*0.5*0.5 + 30*0.5*0.5 + 40*0.5*0.5 = 25
			want: 25.0,
		},
		{
			name:    "Top edge center (0.0, 0.5)",
			rowFrac: 0.0, colFrac: 0.5,
			// 10*1*0.5 + 20*1*0.5 = 15
			want: 15.0,
		},
		{
			name:    "Left edge center (0.5, 0.0)",
			rowFrac: 0.5, colFrac: 0.0,
			// 10*0.5*1 + 30*0.5*1 = 20
			want: 20.0,
		},
		{
			name:    "Quarter point (0.25, 0.75)",
			rowFrac: 0.25, colFrac: 0.75,
			// 10*0.75*0.25 + 20*0.75*0.75 + 30*0.25*0.25 + 40*0.25*0.75
			// = 1.875 + 11.25 + 1.875 + 7.5 = 22.5
			want: 22.5,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := bilinearInterpolateFromChunks(chunkMap, corners, meta, 0, tt.rowFrac, tt.colFrac)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("got %f, want %f", got, tt.want)
			}
		})
	}
}

// --- Test: ReadPoint end-to-end ---

func TestReadPoint(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"

	// Chunk dimensions WITH halo.
	chunkLatWithHalo := ChunkLatSize + 2*haloPixels  // 92
	chunkLonWithHalo := ChunkLonSize + 2*haloPixels  // 182

	timeSteps := 3

	// Fill function: value = timeStep*1000 + globalRow + globalCol*0.001
	// This gives unique values per (time, lat, lon) that we can verify.
	fillFn := func(t, localLat, localLon int) float32 {
		return float32(t*1000) + float32(localLat) + float32(localLon)*0.001
	}

	s3 := newMockS3Client()

	// Put metadata.
	metaKey := zarrMetaKey(storagePath, types.ZarrVarTemperatureC)
	s3.putObject(bucket, metaKey, makeZarrayMeta(timeSteps, chunkLatWithHalo, chunkLonWithHalo))

	// Populate chunks. For this test we only need the chunk containing (0,0) area.
	// Lat 90.0 maps to row 0, chunk (0,0). Lon -180.0 maps to col 0, chunk (0,0).
	// We test with lat=90.0, lon=-180.0 which is exactly at grid point (0,0).
	raw := makeChunkData(timeSteps, chunkLatWithHalo, chunkLonWithHalo, fillFn)
	compressed := compressZstd(raw)
	key := chunkKey(storagePath, types.ZarrVarTemperatureC, 0, 0)
	s3.putObject(bucket, key, compressed)

	reader := NewZarrReader(s3, bucket, testLogger())

	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	loc := types.Location{Lat: 90.0, Lon: -180.0}

	results, err := reader.ReadPoint(context.Background(), rc, loc, []string{types.ZarrVarTemperatureC})
	if err != nil {
		t.Fatalf("ReadPoint failed: %v", err)
	}

	if len(results) != timeSteps {
		t.Fatalf("expected %d data points, got %d", timeSteps, len(results))
	}

	// At grid point (0,0), the value in the chunk is at local position (haloPixels, haloPixels).
	// fillFn(t, haloPixels, haloPixels) = t*1000 + 1 + 1*0.001 = t*1000 + 1.001
	for ti := 0; ti < timeSteps; ti++ {
		if results[ti].TemperatureC == nil {
			t.Errorf("time step %d: TemperatureC is nil", ti)
			continue
		}
		// The value at local (1, 1) with halo:
		expected := float64(fillFn(ti, haloPixels, haloPixels))
		if math.Abs(*results[ti].TemperatureC-expected) > 0.01 {
			t.Errorf("time step %d: TemperatureC = %f, want ~%f", ti, *results[ti].TemperatureC, expected)
		}
	}

	// Verify source is set.
	for ti, dp := range results {
		if dp.Source != string(types.ForecastMediumRange) {
			t.Errorf("time step %d: Source = %q, want %q", ti, dp.Source, types.ForecastMediumRange)
		}
	}
}

// --- Test: ReadPoint with bilinear interpolation ---

func TestReadPointBilinearInterpolation(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"
	variable := types.ZarrVarTemperatureC

	chunkLatWithHalo := ChunkLatSize + 2*haloPixels
	chunkLonWithHalo := ChunkLonSize + 2*haloPixels
	timeSteps := 1

	// Fill the chunk with a linear gradient:
	// value = localLat * 10 + localLon * 1
	// This makes interpolation easy to verify.
	fillFn := func(t, localLat, localLon int) float32 {
		return float32(localLat*10 + localLon)
	}

	s3 := newMockS3Client()
	metaKey := zarrMetaKey(storagePath, variable)
	s3.putObject(bucket, metaKey, makeZarrayMeta(timeSteps, chunkLatWithHalo, chunkLonWithHalo))

	raw := makeChunkData(timeSteps, chunkLatWithHalo, chunkLonWithHalo, fillFn)
	compressed := compressZstd(raw)
	key := chunkKey(storagePath, variable, 0, 0)
	s3.putObject(bucket, key, compressed)

	reader := NewZarrReader(s3, bucket, testLogger())
	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	// Test at a point between grid cells.
	// Lat 89.875 = row 0.5, Lon -179.875 = col 0.5
	// This should interpolate between grid points (0,0), (0,1), (1,0), (1,1).
	loc := types.Location{Lat: 89.875, Lon: -179.875}

	results, err := reader.ReadPoint(context.Background(), rc, loc, []string{variable})
	if err != nil {
		t.Fatalf("ReadPoint failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(results))
	}

	if results[0].TemperatureC == nil {
		t.Fatal("TemperatureC is nil")
	}

	// The 4 corners in the chunk (with halo offset of 1):
	// TL: (halo+0, halo+0) = (1, 1) => 1*10 + 1 = 11
	// TR: (halo+0, halo+1) = (1, 2) => 1*10 + 2 = 12
	// BL: (halo+1, halo+0) = (2, 1) => 2*10 + 1 = 21
	// BR: (halo+1, halo+1) = (2, 2) => 2*10 + 2 = 22
	// Bilinear at (0.5, 0.5): 11*0.25 + 12*0.25 + 21*0.25 + 22*0.25 = 16.5
	expected := 16.5
	if math.Abs(*results[0].TemperatureC-expected) > 0.01 {
		t.Errorf("TemperatureC = %f, want %f", *results[0].TemperatureC, expected)
	}
}

// --- Test: ReadTile ---

func TestReadTile(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"
	variable := types.ZarrVarWindSpeedKmh

	chunkLatWithHalo := ChunkLatSize + 2*haloPixels
	chunkLonWithHalo := ChunkLonSize + 2*haloPixels
	timeSteps := 2

	// Constant value chunk for simplicity.
	constValue := float32(42.0)
	fillFn := func(t, lat, lon int) float32 { return constValue }

	s3 := newMockS3Client()
	metaKey := zarrMetaKey(storagePath, variable)
	s3.putObject(bucket, metaKey, makeZarrayMeta(timeSteps, chunkLatWithHalo, chunkLonWithHalo))

	raw := makeChunkData(timeSteps, chunkLatWithHalo, chunkLonWithHalo, fillFn)
	compressed := compressZstd(raw)
	key := chunkKey(storagePath, variable, 0, 0)
	s3.putObject(bucket, key, compressed)

	reader := NewZarrReader(s3, bucket, testLogger())
	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	// Two locations in the same tile (0.0) - both in the North Pole area.
	locs := []types.LocationIdent{
		{ID: "loc1", Lat: 89.0, Lon: -179.0},
		{ID: "loc2", Lat: 88.0, Lon: -178.0},
	}

	results, err := reader.ReadTile(context.Background(), rc, "0.0", locs, []string{variable})
	if err != nil {
		t.Fatalf("ReadTile failed: %v", err)
	}

	if len(results) != 2 {
		t.Fatalf("expected 2 location results, got %d", len(results))
	}

	for _, locID := range []string{"loc1", "loc2"} {
		pts, ok := results[locID]
		if !ok {
			t.Errorf("missing results for %s", locID)
			continue
		}
		if len(pts) != timeSteps {
			t.Errorf("%s: expected %d time steps, got %d", locID, timeSteps, len(pts))
			continue
		}
		for ti, dp := range pts {
			if dp.WindSpeedKmh == nil {
				t.Errorf("%s time step %d: WindSpeedKmh is nil", locID, ti)
				continue
			}
			if math.Abs(*dp.WindSpeedKmh-float64(constValue)) > 0.01 {
				t.Errorf("%s time step %d: WindSpeedKmh = %f, want %f", locID, ti, *dp.WindSpeedKmh, constValue)
			}
		}
	}
}

// --- Test: Error handling ---

func TestReadPointS3Error(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"
	variable := types.ZarrVarTemperatureC

	s3 := newMockS3Client()
	// Put metadata but make chunk fetch fail.
	metaKey := zarrMetaKey(storagePath, variable)
	s3.putObject(bucket, metaKey, makeZarrayMeta(1, ChunkLatSize+2, ChunkLonSize+2))
	s3.setFailKey(bucket, chunkKey(storagePath, variable, 0, 0), fmt.Errorf("connection timeout"))

	reader := NewZarrReader(s3, bucket, testLogger())
	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	loc := types.Location{Lat: 90.0, Lon: -180.0}
	_, err := reader.ReadPoint(context.Background(), rc, loc, []string{variable})

	if err == nil {
		t.Fatal("expected error, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
	if appErr.Code != types.ErrCodeUpstreamForecast {
		t.Errorf("error code = %q, want %q", appErr.Code, types.ErrCodeUpstreamForecast)
	}
}

func TestReadPointMetadataError(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"
	variable := types.ZarrVarTemperatureC

	s3 := newMockS3Client()
	// No metadata at all.

	reader := NewZarrReader(s3, bucket, testLogger())
	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	loc := types.Location{Lat: 45.0, Lon: 0.0}
	_, err := reader.ReadPoint(context.Background(), rc, loc, []string{variable})

	if err == nil {
		t.Fatal("expected error for missing metadata, got nil")
	}

	var appErr *types.AppError
	if !errors.As(err, &appErr) {
		t.Fatalf("expected *types.AppError, got %T: %v", err, err)
	}
}

func TestReadPointInvalidLocation(t *testing.T) {
	s3 := newMockS3Client()
	reader := NewZarrReader(s3, "bucket", testLogger())
	rc := RunContext{Model: types.ForecastMediumRange, StoragePath: "path"}

	tests := []struct {
		name string
		lat  float64
		lon  float64
	}{
		{"lat too high", 91.0, 0.0},
		{"lat too low", -91.0, 0.0},
		{"lon too high", 0.0, 181.0},
		{"lon too low", 0.0, -181.0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loc := types.Location{Lat: tt.lat, Lon: tt.lon}
			_, err := reader.ReadPoint(context.Background(), rc, loc, []string{types.ZarrVarTemperatureC})
			if err == nil {
				t.Error("expected error for invalid location")
			}
			var appErr *types.AppError
			if !errors.As(err, &appErr) {
				t.Fatalf("expected *types.AppError, got %T", err)
			}
		})
	}
}

func TestReadPointNoVariables(t *testing.T) {
	s3 := newMockS3Client()
	reader := NewZarrReader(s3, "bucket", testLogger())
	rc := RunContext{Model: types.ForecastMediumRange, StoragePath: "path"}
	loc := types.Location{Lat: 45.0, Lon: 0.0}

	_, err := reader.ReadPoint(context.Background(), rc, loc, []string{})
	if err == nil {
		t.Error("expected error for empty variables")
	}
}

func TestReadTileEmptyLocations(t *testing.T) {
	s3 := newMockS3Client()
	reader := NewZarrReader(s3, "bucket", testLogger())
	rc := RunContext{Model: types.ForecastMediumRange, StoragePath: "path"}

	results, err := reader.ReadTile(context.Background(), rc, "0.0", []types.LocationIdent{}, []string{types.ZarrVarTemperatureC})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected empty results, got %d", len(results))
	}
}

// --- Test: Multiple variables ---

func TestReadPointMultipleVariables(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"

	chunkLatWithHalo := ChunkLatSize + 2*haloPixels
	chunkLonWithHalo := ChunkLonSize + 2*haloPixels
	timeSteps := 1

	vars := []string{types.ZarrVarTemperatureC, types.ZarrVarWindSpeedKmh, types.ZarrVarHumidityPercent}
	s3 := newMockS3Client()

	// For each variable, use a different constant value so we can verify they're distinct.
	varValues := map[string]float32{
		types.ZarrVarTemperatureC:    25.0,
		types.ZarrVarWindSpeedKmh:    15.0,
		types.ZarrVarHumidityPercent: 65.0,
	}

	for _, v := range vars {
		metaKey := zarrMetaKey(storagePath, v)
		s3.putObject(bucket, metaKey, makeZarrayMeta(timeSteps, chunkLatWithHalo, chunkLonWithHalo))

		val := varValues[v]
		raw := makeChunkData(timeSteps, chunkLatWithHalo, chunkLonWithHalo, func(t, lat, lon int) float32 { return val })
		compressed := compressZstd(raw)
		key := chunkKey(storagePath, v, 0, 0)
		s3.putObject(bucket, key, compressed)
	}

	reader := NewZarrReader(s3, bucket, testLogger())
	rc := RunContext{
		Model:       types.ForecastMediumRange,
		StoragePath: storagePath,
	}

	loc := types.Location{Lat: 90.0, Lon: -180.0}
	results, err := reader.ReadPoint(context.Background(), rc, loc, vars)
	if err != nil {
		t.Fatalf("ReadPoint failed: %v", err)
	}

	if len(results) != 1 {
		t.Fatalf("expected 1 data point, got %d", len(results))
	}

	dp := results[0]
	if dp.TemperatureC == nil || math.Abs(*dp.TemperatureC-25.0) > 0.01 {
		t.Errorf("TemperatureC = %v, want 25.0", dp.TemperatureC)
	}
	if dp.WindSpeedKmh == nil || math.Abs(*dp.WindSpeedKmh-15.0) > 0.01 {
		t.Errorf("WindSpeedKmh = %v, want 15.0", dp.WindSpeedKmh)
	}
	if dp.Humidity == nil || math.Abs(*dp.Humidity-65.0) > 0.01 {
		t.Errorf("Humidity = %v, want 65.0", dp.Humidity)
	}

	// Verify unset fields are nil.
	if dp.PrecipitationMM != nil {
		t.Errorf("PrecipitationMM should be nil, got %v", dp.PrecipitationMM)
	}
	if dp.PrecipitationProbability != nil {
		t.Errorf("PrecipitationProbability should be nil, got %v", dp.PrecipitationProbability)
	}
}

// --- Test: chunkKey format ---

func TestChunkKey(t *testing.T) {
	tests := []struct {
		path     string
		variable string
		latChunk int
		lonChunk int
		want     string
	}{
		{
			path:     "medium_range/2026-02-01T00:00:00Z",
			variable: "temperature_c",
			latChunk: 2,
			lonChunk: 3,
			want:     "medium_range/2026-02-01T00:00:00Z/temperature_c/0.2.3",
		},
		{
			path:     "nowcast/2026-02-01T12:00:00Z",
			variable: "precipitation_mm",
			latChunk: 0,
			lonChunk: 0,
			want:     "nowcast/2026-02-01T12:00:00Z/precipitation_mm/0.0.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.want, func(t *testing.T) {
			got := chunkKey(tt.path, tt.variable, tt.latChunk, tt.lonChunk)
			if got != tt.want {
				t.Errorf("chunkKey = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Test: validateLocation ---

func TestValidateLocation(t *testing.T) {
	tests := []struct {
		name    string
		lat     float64
		lon     float64
		wantErr bool
	}{
		{"valid center", 0.0, 0.0, false},
		{"valid bounds", 90.0, 180.0, false},
		{"valid negative bounds", -90.0, -180.0, false},
		{"lat too high", 90.1, 0.0, true},
		{"lat too low", -90.1, 0.0, true},
		{"lon too high", 0.0, 180.1, true},
		{"lon too low", 0.0, -180.1, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateLocation(tt.lat, tt.lon)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateLocation(%f, %f) error = %v, wantErr = %v", tt.lat, tt.lon, err, tt.wantErr)
			}
		})
	}
}

// --- Test: Corrupt/invalid .zarray metadata ---

func TestReadPointInvalidMetadata(t *testing.T) {
	bucket := "test-bucket"
	storagePath := "medium_range/2026-02-01T00:00:00Z"
	variable := types.ZarrVarTemperatureC

	t.Run("invalid JSON", func(t *testing.T) {
		s3 := newMockS3Client()
		metaKey := zarrMetaKey(storagePath, variable)
		s3.putObject(bucket, metaKey, []byte("not json"))

		reader := NewZarrReader(s3, bucket, testLogger())
		rc := RunContext{Model: types.ForecastMediumRange, StoragePath: storagePath}
		loc := types.Location{Lat: 45.0, Lon: 0.0}

		_, err := reader.ReadPoint(context.Background(), rc, loc, []string{variable})
		if err == nil {
			t.Error("expected error for invalid JSON metadata")
		}
	})

	t.Run("wrong dimensions", func(t *testing.T) {
		s3 := newMockS3Client()
		metaKey := zarrMetaKey(storagePath, variable)
		// Only 2 dimensions instead of 3.
		meta := `{"shape":[10,20],"chunks":[10,20],"dtype":"<f4","zarr_format":2}`
		s3.putObject(bucket, metaKey, []byte(meta))

		reader := NewZarrReader(s3, bucket, testLogger())
		rc := RunContext{Model: types.ForecastMediumRange, StoragePath: storagePath}
		loc := types.Location{Lat: 45.0, Lon: 0.0}

		_, err := reader.ReadPoint(context.Background(), rc, loc, []string{variable})
		if err == nil {
			t.Error("expected error for wrong dimensions")
		}
	})
}

// --- Test: setDataPointField ---

func TestSetDataPointField(t *testing.T) {
	dp := &ForecastDataPoint{}
	val := 42.5

	setDataPointField(dp, types.ZarrVarTemperatureC, val)
	if dp.TemperatureC == nil || *dp.TemperatureC != val {
		t.Errorf("TemperatureC = %v, want %f", dp.TemperatureC, val)
	}

	setDataPointField(dp, types.ZarrVarPrecipitationMM, val)
	if dp.PrecipitationMM == nil || *dp.PrecipitationMM != val {
		t.Errorf("PrecipitationMM = %v, want %f", dp.PrecipitationMM, val)
	}

	setDataPointField(dp, types.ZarrVarPrecipitationProb, val)
	if dp.PrecipitationProbability == nil || *dp.PrecipitationProbability != val {
		t.Errorf("PrecipitationProbability = %v, want %f", dp.PrecipitationProbability, val)
	}

	setDataPointField(dp, types.ZarrVarWindSpeedKmh, val)
	if dp.WindSpeedKmh == nil || *dp.WindSpeedKmh != val {
		t.Errorf("WindSpeedKmh = %v, want %f", dp.WindSpeedKmh, val)
	}

	setDataPointField(dp, types.ZarrVarHumidityPercent, val)
	if dp.Humidity == nil || *dp.Humidity != val {
		t.Errorf("Humidity = %v, want %f", dp.Humidity, val)
	}

	setDataPointField(dp, types.ZarrVarCloudCoverPercent, val)
	if dp.CloudCover == nil || *dp.CloudCover != val {
		t.Errorf("CloudCover = %v, want %f", dp.CloudCover, val)
	}

	// Unknown variable should be a no-op.
	dp2 := &ForecastDataPoint{}
	setDataPointField(dp2, "unknown_var", val)
	if dp2.TemperatureC != nil || dp2.PrecipitationMM != nil {
		t.Error("unknown variable should not set any field")
	}
}
