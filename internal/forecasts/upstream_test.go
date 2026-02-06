package forecasts

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"

	"watchpoint/internal/types"
)

// --- Mock S3 List Client ---

// mockS3ListClient implements S3ListClient for testing.
type mockS3ListClient struct {
	// responses maps "bucket/prefix" to the ListObjectsV2 result.
	responses map[string]*s3.ListObjectsV2Output
	// errors maps "bucket/prefix" to an error to return.
	errors map[string]error
	// callLog records the bucket/prefix pairs that were called.
	callLog []string
}

func newMockS3ListClient() *mockS3ListClient {
	return &mockS3ListClient{
		responses: make(map[string]*s3.ListObjectsV2Output),
		errors:    make(map[string]error),
	}
}

func (m *mockS3ListClient) setResponse(bucket, prefix string, output *s3.ListObjectsV2Output) {
	m.responses[bucket+"/"+prefix] = output
}

func (m *mockS3ListClient) setError(bucket, prefix string, err error) {
	m.errors[bucket+"/"+prefix] = err
}

func (m *mockS3ListClient) ListObjectsV2(_ context.Context, params *s3.ListObjectsV2Input, _ ...func(*s3.Options)) (*s3.ListObjectsV2Output, error) {
	bucket := aws.ToString(params.Bucket)
	prefix := aws.ToString(params.Prefix)
	key := bucket + "/" + prefix
	m.callLog = append(m.callLog, key)

	if err, ok := m.errors[key]; ok {
		return nil, err
	}
	if resp, ok := m.responses[key]; ok {
		return resp, nil
	}

	// Default: empty response.
	return &s3.ListObjectsV2Output{
		Contents: []s3types.Object{},
	}, nil
}

// --- Test: ParseGFSKey ---

func TestParseGFSKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantTS  time.Time
		wantOK  bool
	}{
		{
			name:   "valid GFS directory prefix",
			key:    "gfs.20231027/06/",
			wantTS: time.Date(2023, 10, 27, 6, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GFS key with atmos path",
			key:    "gfs.20231027/06/atmos/gfs.t06z.pgrb2.0p25.f000",
			wantTS: time.Date(2023, 10, 27, 6, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GFS midnight run",
			key:    "gfs.20260201/00/",
			wantTS: time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GFS 12z run",
			key:    "gfs.20260201/12/atmos/",
			wantTS: time.Date(2026, 2, 1, 12, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GFS 18z run",
			key:    "gfs.20260201/18/",
			wantTS: time.Date(2026, 2, 1, 18, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "invalid - not GFS format",
			key:    "hrrr.20231027/06/",
			wantOK: false,
		},
		{
			name:   "invalid - no trailing content after hour",
			key:    "gfs.20231027/",
			wantOK: false,
		},
		{
			name:   "invalid - bad date",
			key:    "gfs.20231327/06/",
			wantOK: false,
		},
		{
			name:   "invalid - empty string",
			key:    "",
			wantOK: false,
		},
		{
			name:   "invalid - just prefix",
			key:    "gfs.",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTS, gotOK := ParseGFSKey(tt.key)
			if gotOK != tt.wantOK {
				t.Errorf("ParseGFSKey(%q) ok = %v, want %v", tt.key, gotOK, tt.wantOK)
				return
			}
			if gotOK && !gotTS.Equal(tt.wantTS) {
				t.Errorf("ParseGFSKey(%q) = %v, want %v", tt.key, gotTS, tt.wantTS)
			}
		})
	}
}

// --- Test: ParseGOESKey ---

func TestParseGOESKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantTS  time.Time
		wantOK  bool
	}{
		{
			name:   "valid GOES hourly prefix",
			key:    "ABI-L2-MCMIPC/2026/037/14/",
			wantTS: time.Date(2026, 2, 6, 14, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GOES day 1 (Jan 1)",
			key:    "ABI-L2-MCMIPC/2026/001/00/",
			wantTS: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid GOES day 365 (Dec 31)",
			key:    "ABI-L2-MCMIPC/2026/365/23/",
			wantTS: time.Date(2026, 12, 31, 23, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "invalid - not GOES format",
			key:    "some-other-product/2026/037/14/",
			wantOK: false,
		},
		{
			name:   "invalid - empty string",
			key:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTS, gotOK := ParseGOESKey(tt.key)
			if gotOK != tt.wantOK {
				t.Errorf("ParseGOESKey(%q) ok = %v, want %v", tt.key, gotOK, tt.wantOK)
				return
			}
			if gotOK && !gotTS.Equal(tt.wantTS) {
				t.Errorf("ParseGOESKey(%q) = %v, want %v", tt.key, gotTS, tt.wantTS)
			}
		})
	}
}

// --- Test: ParseMRMSKey ---

func TestParseMRMSKey(t *testing.T) {
	tests := []struct {
		name    string
		key     string
		wantTS  time.Time
		wantOK  bool
	}{
		{
			name:   "valid MRMS key",
			key:    "CONUS/MergedReflectivityComposite_00.50/20260206-143000.grib2",
			wantTS: time.Date(2026, 2, 6, 14, 30, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid MRMS key midnight",
			key:    "CONUS/MergedReflectivityComposite_00.50/20260206-000000.grib2",
			wantTS: time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "valid MRMS key end of day",
			key:    "CONUS/MergedReflectivityComposite_00.50/20260206-235959.grib2",
			wantTS: time.Date(2026, 2, 6, 23, 59, 59, 0, time.UTC),
			wantOK: true,
		},
		{
			name:   "invalid - no timestamp in key",
			key:    "CONUS/MergedReflectivityComposite_00.50/readme.txt",
			wantOK: false,
		},
		{
			name:   "invalid - empty string",
			key:    "",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotTS, gotOK := ParseMRMSKey(tt.key)
			if gotOK != tt.wantOK {
				t.Errorf("ParseMRMSKey(%q) ok = %v, want %v", tt.key, gotOK, tt.wantOK)
				return
			}
			if gotOK && !gotTS.Equal(tt.wantTS) {
				t.Errorf("ParseMRMSKey(%q) = %v, want %v", tt.key, gotTS, tt.wantTS)
			}
		})
	}
}

// --- Test: GFSSource.Name ---

func TestGFSSourceName(t *testing.T) {
	src := NewGFSSource(nil, nil, testLogger())
	if src.Name() != types.ForecastMediumRange {
		t.Errorf("Name() = %q, want %q", src.Name(), types.ForecastMediumRange)
	}
}

// --- Test: GOESSource.Name ---

func TestGOESSourceName(t *testing.T) {
	src := NewGOESSource(nil, "noaa-goes16", testLogger())
	if src.Name() != types.ForecastNowcast {
		t.Errorf("Name() = %q, want %q", src.Name(), types.ForecastNowcast)
	}
}

// --- Test: MRMSSource.Name ---

func TestMRMSSourceName(t *testing.T) {
	src := NewMRMSSource(nil, "noaa-mrms-pds", testLogger())
	if src.Name() != types.ForecastNowcast {
		t.Errorf("Name() = %q, want %q", src.Name(), types.ForecastNowcast)
	}
}

// --- Test: GFSSource.CheckAvailability ---

func TestGFSCheckAvailability(t *testing.T) {
	t.Run("finds new runs from common prefixes", func(t *testing.T) {
		client := newMockS3ListClient()

		// Simulate GFS directories for 2026-02-06.
		client.setResponse("noaa-gfs-bdp-pds", "gfs.20260206/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("gfs.20260206/00/")},
				{Prefix: aws.String("gfs.20260206/06/")},
				{Prefix: aws.String("gfs.20260206/12/")},
			},
		})

		src := NewGFSSource(client, []string{"noaa-gfs-bdp-pds"}, testLogger())

		// Ask for runs since 06z - should only return 12z (after since time).
		since := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(timestamps) != 1 {
			t.Fatalf("expected 1 timestamp, got %d: %v", len(timestamps), timestamps)
		}

		expected := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
		if !timestamps[0].Equal(expected) {
			t.Errorf("got %v, want %v", timestamps[0], expected)
		}
	})

	t.Run("returns empty when no new data", func(t *testing.T) {
		client := newMockS3ListClient()

		client.setResponse("noaa-gfs-bdp-pds", "gfs.20260206/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("gfs.20260206/00/")},
				{Prefix: aws.String("gfs.20260206/06/")},
			},
		})

		src := NewGFSSource(client, []string{"noaa-gfs-bdp-pds"}, testLogger())

		// Since is after the latest available - should return empty.
		since := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(timestamps) != 0 {
			t.Errorf("expected 0 timestamps, got %d: %v", len(timestamps), timestamps)
		}
	})

	t.Run("timestamps sorted chronologically", func(t *testing.T) {
		client := newMockS3ListClient()

		// Return them out of order to verify sorting.
		client.setResponse("noaa-gfs-bdp-pds", "gfs.20260206/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("gfs.20260206/18/")},
				{Prefix: aws.String("gfs.20260206/06/")},
				{Prefix: aws.String("gfs.20260206/12/")},
			},
		})

		src := NewGFSSource(client, []string{"noaa-gfs-bdp-pds"}, testLogger())

		since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(timestamps) != 3 {
			t.Fatalf("expected 3 timestamps, got %d", len(timestamps))
		}

		for i := 1; i < len(timestamps); i++ {
			if timestamps[i].Before(timestamps[i-1]) {
				t.Errorf("timestamps not sorted: [%d]=%v before [%d]=%v",
					i, timestamps[i], i-1, timestamps[i-1])
			}
		}
	})
}

// --- Test: GFSSource Mirror Failover ---

func TestGFSMirrorFailover(t *testing.T) {
	t.Run("falls back to secondary mirror on primary failure", func(t *testing.T) {
		client := newMockS3ListClient()

		// Primary mirror fails.
		client.setError("noaa-gfs-bdp-pds", "gfs.20260206/", fmt.Errorf("connection refused"))

		// Secondary mirror succeeds.
		client.setResponse("aws-noaa-gfs", "gfs.20260206/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("gfs.20260206/12/")},
			},
		})

		src := NewGFSSource(client, []string{"noaa-gfs-bdp-pds", "aws-noaa-gfs"}, testLogger())

		since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("expected fallback to succeed, got error: %v", err)
		}

		if len(timestamps) != 1 {
			t.Fatalf("expected 1 timestamp from secondary, got %d", len(timestamps))
		}

		expected := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
		if !timestamps[0].Equal(expected) {
			t.Errorf("got %v, want %v", timestamps[0], expected)
		}
	})

	t.Run("returns error when all mirrors fail", func(t *testing.T) {
		client := newMockS3ListClient()

		client.setError("mirror1", "gfs.20260206/", fmt.Errorf("timeout"))
		client.setError("mirror2", "gfs.20260206/", fmt.Errorf("access denied"))

		src := NewGFSSource(client, []string{"mirror1", "mirror2"}, testLogger())

		since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
		_, err := src.CheckAvailability(context.Background(), since)
		if err == nil {
			t.Fatal("expected error when all mirrors fail")
		}

		// Verify it wraps as AppError.
		appErr, ok := err.(*types.AppError)
		if !ok {
			t.Fatalf("expected *types.AppError, got %T: %v", err, err)
		}
		if appErr.Code != types.ErrCodeUpstreamForecast {
			t.Errorf("error code = %q, want %q", appErr.Code, types.ErrCodeUpstreamForecast)
		}
	})

	t.Run("returns error when no mirrors configured", func(t *testing.T) {
		client := newMockS3ListClient()
		src := NewGFSSource(client, []string{}, testLogger())

		since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
		_, err := src.CheckAvailability(context.Background(), since)
		if err == nil {
			t.Fatal("expected error when no mirrors configured")
		}
	})

	t.Run("does not try secondary if primary succeeds", func(t *testing.T) {
		client := newMockS3ListClient()

		// Primary succeeds.
		client.setResponse("primary", "gfs.20260206/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("gfs.20260206/06/")},
			},
		})

		// Secondary should not be called.
		client.setError("secondary", "gfs.20260206/", fmt.Errorf("should not be called"))

		src := NewGFSSource(client, []string{"primary", "secondary"}, testLogger())

		since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
		_, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify secondary was never called.
		for _, call := range client.callLog {
			if call == "secondary/gfs.20260206/" {
				t.Error("secondary mirror was called despite primary succeeding")
			}
		}
	})
}

// --- Test: GOESSource.CheckAvailability ---

func TestGOESCheckAvailability(t *testing.T) {
	t.Run("finds hourly data after since", func(t *testing.T) {
		client := newMockS3ListClient()

		// Day 37 of 2026 = Feb 6.
		client.setResponse("noaa-goes16", "ABI-L2-MCMIPC/2026/037/", &s3.ListObjectsV2Output{
			CommonPrefixes: []s3types.CommonPrefix{
				{Prefix: aws.String("ABI-L2-MCMIPC/2026/037/12/")},
				{Prefix: aws.String("ABI-L2-MCMIPC/2026/037/13/")},
				{Prefix: aws.String("ABI-L2-MCMIPC/2026/037/14/")},
			},
		})

		src := NewGOESSource(client, "noaa-goes16", testLogger())

		// Since is 12z - should only return 13z and 14z (strictly after since).
		since := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if len(timestamps) != 2 {
			t.Fatalf("expected 2 timestamps (strictly after since), got %d: %v", len(timestamps), timestamps)
		}

		expected13 := time.Date(2026, 2, 6, 13, 0, 0, 0, time.UTC)
		expected14 := time.Date(2026, 2, 6, 14, 0, 0, 0, time.UTC)
		if !timestamps[0].Equal(expected13) {
			t.Errorf("timestamps[0] = %v, want %v", timestamps[0], expected13)
		}
		if !timestamps[1].Equal(expected14) {
			t.Errorf("timestamps[1] = %v, want %v", timestamps[1], expected14)
		}
	})
}

// --- Test: MRMSSource.CheckAvailability ---

func TestMRMSCheckAvailability(t *testing.T) {
	t.Run("finds radar data timestamps", func(t *testing.T) {
		client := newMockS3ListClient()

		falseVal := false
		client.setResponse("noaa-mrms-pds", "CONUS/MergedReflectivityComposite_00.50/", &s3.ListObjectsV2Output{
			Contents: []s3types.Object{
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143000.grib2")},
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143200.grib2")},
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143500.grib2")},
			},
			IsTruncated: &falseVal,
		})

		src := NewMRMSSource(client, "noaa-mrms-pds", testLogger())

		since := time.Date(2026, 2, 6, 14, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// 14:30, 14:32, 14:35 - all 3 distinct minutes after since time.
		if len(timestamps) != 3 {
			t.Fatalf("expected 3 timestamps, got %d: %v", len(timestamps), timestamps)
		}
	})

	t.Run("deduplicates same-minute entries", func(t *testing.T) {
		client := newMockS3ListClient()

		falseVal := false
		client.setResponse("noaa-mrms-pds", "CONUS/MergedReflectivityComposite_00.50/", &s3.ListObjectsV2Output{
			Contents: []s3types.Object{
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143000.grib2")},
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143001.grib2")}, // Same minute as above.
				{Key: aws.String("CONUS/MergedReflectivityComposite_00.50/20260206-143100.grib2")},
			},
			IsTruncated: &falseVal,
		})

		src := NewMRMSSource(client, "noaa-mrms-pds", testLogger())

		since := time.Date(2026, 2, 6, 14, 0, 0, 0, time.UTC)
		timestamps, err := src.CheckAvailability(context.Background(), since)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Only 2 distinct minutes: 14:30 and 14:31.
		if len(timestamps) != 2 {
			t.Fatalf("expected 2 timestamps (deduplicated), got %d: %v", len(timestamps), timestamps)
		}
	})
}

// --- Test: gfsPrefixesForRange ---

func TestGFSPrefixesForRange(t *testing.T) {
	t.Run("single day", func(t *testing.T) {
		since := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)
		prefixes := gfsPrefixesForRange(since)

		if len(prefixes) < 1 {
			t.Fatalf("expected at least 1 prefix, got %d", len(prefixes))
		}

		// Should contain the prefix for the since date.
		found := false
		for _, p := range prefixes {
			if p == "gfs.20260206/" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected prefix gfs.20260206/ in %v", prefixes)
		}
	})

	t.Run("multi-day range", func(t *testing.T) {
		// Since is 2 days ago from a fixed "now". We test the function generates
		// correct prefixes by using a since time we know the range for.
		since := time.Date(2026, 2, 4, 0, 0, 0, 0, time.UTC)
		prefixes := gfsPrefixesForRange(since)

		// Should have at least 3 prefixes (Feb 4, 5, 6 assuming today is Feb 6).
		if len(prefixes) < 2 {
			t.Fatalf("expected at least 2 prefixes for multi-day range, got %d", len(prefixes))
		}

		// Verify first prefix.
		if prefixes[0] != "gfs.20260204/" {
			t.Errorf("first prefix = %q, want %q", prefixes[0], "gfs.20260204/")
		}
	})
}

// --- Test: GFS finds runs from object keys (not just common prefixes) ---

func TestGFSCheckAvailabilityFromObjectKeys(t *testing.T) {
	client := newMockS3ListClient()

	// Some S3 implementations return objects directly instead of common prefixes.
	client.setResponse("test-bucket", "gfs.20260206/", &s3.ListObjectsV2Output{
		Contents: []s3types.Object{
			{Key: aws.String("gfs.20260206/06/atmos/gfs.t06z.pgrb2.0p25.f000")},
			{Key: aws.String("gfs.20260206/12/atmos/gfs.t12z.pgrb2.0p25.f000")},
		},
	})

	src := NewGFSSource(client, []string{"test-bucket"}, testLogger())

	since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	timestamps, err := src.CheckAvailability(context.Background(), since)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(timestamps) != 2 {
		t.Fatalf("expected 2 timestamps from object keys, got %d: %v", len(timestamps), timestamps)
	}

	expected06 := time.Date(2026, 2, 6, 6, 0, 0, 0, time.UTC)
	expected12 := time.Date(2026, 2, 6, 12, 0, 0, 0, time.UTC)

	if !timestamps[0].Equal(expected06) {
		t.Errorf("timestamps[0] = %v, want %v", timestamps[0], expected06)
	}
	if !timestamps[1].Equal(expected12) {
		t.Errorf("timestamps[1] = %v, want %v", timestamps[1], expected12)
	}
}

// --- Test: GFS deduplicates keys ---

func TestGFSDeduplicatesTimestamps(t *testing.T) {
	client := newMockS3ListClient()

	// Both common prefix and object key report the same run.
	client.setResponse("test-bucket", "gfs.20260206/", &s3.ListObjectsV2Output{
		CommonPrefixes: []s3types.CommonPrefix{
			{Prefix: aws.String("gfs.20260206/06/")},
		},
		Contents: []s3types.Object{
			{Key: aws.String("gfs.20260206/06/atmos/gfs.t06z.pgrb2.0p25.f000")},
		},
	})

	src := NewGFSSource(client, []string{"test-bucket"}, testLogger())

	since := time.Date(2026, 2, 6, 0, 0, 0, 0, time.UTC)
	timestamps, err := src.CheckAvailability(context.Background(), since)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Should only have one entry despite appearing in both prefixes and objects.
	if len(timestamps) != 1 {
		t.Fatalf("expected 1 deduplicated timestamp, got %d: %v", len(timestamps), timestamps)
	}
}

// --- Test: interface compliance ---

func TestUpstreamSourceInterfaceCompliance(t *testing.T) {
	// Compile-time verification that all source types implement UpstreamSource.
	var _ UpstreamSource = (*GFSSource)(nil)
	var _ UpstreamSource = (*GOESSource)(nil)
	var _ UpstreamSource = (*MRMSSource)(nil)
}
