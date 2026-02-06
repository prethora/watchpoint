// Package forecasts implements upstream data source availability checking.
//
// This file implements the UpstreamSource interface from architecture/09-scheduled-jobs.md
// Section 5.1. It uses AWS SDK v2 to list objects in NOAA S3 buckets (GFS, GOES, MRMS)
// and parse run timestamps from object keys.
//
// GFS key format: gfs.YYYYMMDD/HH/atmos/gfs.tHHz.pgrb2.0p25.f000
// GOES key format: ABI-L2-MCMIPC/YYYY/DDD/HH/...
// MRMS key format: CONUS/MergedReflectivityComposite_00.50/YYYYMMDD-HHmmss...
//
// The UpstreamSource supports mirror failover per FAIL-004: if the primary bucket
// is unavailable, it iterates through configured mirrors until one succeeds.
// Custom endpoints (AWS_ENDPOINT_URL) are supported for MinIO in local dev.
package forecasts

import (
	"context"
	"fmt"
	"log/slog"
	"regexp"
	"sort"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"watchpoint/internal/types"
)

// S3ListClient abstracts the S3 ListObjectsV2 operation for testability.
type S3ListClient interface {
	ListObjectsV2(ctx context.Context, params *s3.ListObjectsV2Input, optFns ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// UpstreamSource defines a remote data source (NOAA GFS/GOES/MRMS).
// Per architecture/09-scheduled-jobs.md Section 5.1.
type UpstreamSource interface {
	// Name returns the forecast model type this source provides data for.
	Name() types.ForecastType
	// CheckAvailability returns timestamps of runs available in S3 since the given time.
	// Mirror failover is handled internally: the implementation iterates through
	// configured mirrors sequentially. If a mirror fails, it logs a warning and
	// proceeds to the next. Only returns an error if ALL mirrors fail.
	CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error)
}

// gfsKeyPattern matches GFS directory prefixes of the form gfs.YYYYMMDD/HH/
// This extracts the date and hour from GFS S3 key prefixes.
var gfsKeyPattern = regexp.MustCompile(`^gfs\.(\d{4})(\d{2})(\d{2})/(\d{2})/`)

// goesKeyPattern matches GOES ABI L2 MCMIPC key prefixes of the form ABI-L2-MCMIPC/YYYY/DDD/HH/
var goesKeyPattern = regexp.MustCompile(`^ABI-L2-MCMIPC/(\d{4})/(\d{3})/(\d{2})/`)

// mrmsKeyPattern matches MRMS composite reflectivity keys with timestamps.
// Format: CONUS/MergedReflectivityComposite_00.50/YYYYMMDD-HHmmss...
var mrmsKeyPattern = regexp.MustCompile(`/(\d{4})(\d{2})(\d{2})-(\d{2})(\d{2})(\d{2})`)

// GFSSource implements UpstreamSource for the NOAA GFS medium-range model.
// It checks for new GFS model runs by listing objects in the NOAA GFS S3 bucket.
//
// GFS runs 4 times daily at 00z, 06z, 12z, and 18z.
// Keys follow the pattern: gfs.YYYYMMDD/HH/atmos/gfs.tHHz.pgrb2.0p25.f000
type GFSSource struct {
	client  S3ListClient
	mirrors []string // Ordered list of bucket names to try (failover)
	logger  *slog.Logger
}

// NewGFSSource creates a new GFS upstream source with mirror failover.
// mirrors is the ordered list of S3 bucket names from ForecastConfig.UpstreamMirrors.
func NewGFSSource(client S3ListClient, mirrors []string, logger *slog.Logger) *GFSSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &GFSSource{
		client:  client,
		mirrors: mirrors,
		logger:  logger,
	}
}

// Name returns the forecast type for this source.
func (s *GFSSource) Name() types.ForecastType {
	return types.ForecastMediumRange
}

// CheckAvailability lists GFS runs available since the given time.
// It implements mirror failover per architecture/09-scheduled-jobs.md Section 5.2:
// iterate through mirrors sequentially, return on first success, error only if all fail.
func (s *GFSSource) CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error) {
	var lastErr error

	for _, bucket := range s.mirrors {
		timestamps, err := s.checkBucket(ctx, bucket, since)
		if err != nil {
			s.logger.WarnContext(ctx, "mirror unavailable",
				"bucket", bucket,
				"model", "medium_range",
				"error", err,
			)
			lastErr = err
			continue
		}
		return timestamps, nil
	}

	if lastErr != nil {
		return nil, &types.AppError{
			Code:    types.ErrCodeUpstreamForecast,
			Message: fmt.Sprintf("all upstream mirrors failed for GFS: %v", lastErr),
			Err:     lastErr,
		}
	}

	// No mirrors configured.
	return nil, &types.AppError{
		Code:    types.ErrCodeUpstreamForecast,
		Message: "no upstream mirrors configured for GFS",
	}
}

// checkBucket lists GFS run directories in the given bucket since the provided time.
// It uses the ListObjectsV2 API with a date-based prefix to narrow the search.
func (s *GFSSource) checkBucket(ctx context.Context, bucket string, since time.Time) ([]time.Time, error) {
	// Build prefixes for dates from 'since' to now to catch recent runs.
	// GFS updates every 6 hours, so we look at the since date and today.
	prefixes := gfsPrefixesForRange(since)

	seen := make(map[time.Time]struct{})
	var timestamps []time.Time

	for _, prefix := range prefixes {
		input := &s3.ListObjectsV2Input{
			Bucket:    aws.String(bucket),
			Prefix:    aws.String(prefix),
			Delimiter: aws.String("/"),
		}

		output, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing %s/%s: %w", bucket, prefix, err)
		}

		// Parse timestamps from common prefixes (directories).
		for _, cp := range output.CommonPrefixes {
			if cp.Prefix == nil {
				continue
			}
			ts, ok := ParseGFSKey(*cp.Prefix)
			if !ok {
				continue
			}
			if ts.After(since) {
				if _, exists := seen[ts]; !exists {
					seen[ts] = struct{}{}
					timestamps = append(timestamps, ts)
				}
			}
		}

		// Also check direct object keys in case delimiter listing returns objects.
		for _, obj := range output.Contents {
			if obj.Key == nil {
				continue
			}
			ts, ok := ParseGFSKey(*obj.Key)
			if !ok {
				continue
			}
			if ts.After(since) {
				if _, exists := seen[ts]; !exists {
					seen[ts] = struct{}{}
					timestamps = append(timestamps, ts)
				}
			}
		}
	}

	// Sort chronologically.
	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i].Before(timestamps[j])
	})

	return timestamps, nil
}

// gfsPrefixesForRange generates the GFS S3 key prefixes to search.
// GFS keys start with "gfs.YYYYMMDD/", so we generate prefixes for each date
// from 'since' through the current UTC date.
func gfsPrefixesForRange(since time.Time) []string {
	now := time.Now().UTC()
	sinceDate := since.UTC().Truncate(24 * time.Hour)
	nowDate := now.Truncate(24 * time.Hour)

	var prefixes []string
	for d := sinceDate; !d.After(nowDate); d = d.Add(24 * time.Hour) {
		prefixes = append(prefixes, fmt.Sprintf("gfs.%s/", d.Format("20060102")))
	}

	return prefixes
}

// ParseGFSKey extracts a run timestamp from a GFS S3 key or prefix.
// GFS keys match the pattern: gfs.YYYYMMDD/HH/...
// Returns the parsed time and true if successful, or zero time and false otherwise.
func ParseGFSKey(key string) (time.Time, bool) {
	matches := gfsKeyPattern.FindStringSubmatch(key)
	if matches == nil {
		return time.Time{}, false
	}

	year := matches[1]
	month := matches[2]
	day := matches[3]
	hour := matches[4]

	ts, err := time.Parse("20060102 15", fmt.Sprintf("%s%s%s %s", year, month, day, hour))
	if err != nil {
		return time.Time{}, false
	}

	return ts.UTC(), true
}

// GOESSource implements UpstreamSource for GOES satellite imagery.
// Used by the Nowcast pipeline for satellite data availability checking.
type GOESSource struct {
	client S3ListClient
	bucket string // e.g., "noaa-goes16" or "noaa-goes18"
	logger *slog.Logger
}

// NewGOESSource creates a new GOES upstream source.
func NewGOESSource(client S3ListClient, bucket string, logger *slog.Logger) *GOESSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &GOESSource{
		client: client,
		bucket: bucket,
		logger: logger,
	}
}

// Name returns the forecast type for this source.
func (s *GOESSource) Name() types.ForecastType {
	return types.ForecastNowcast
}

// CheckAvailability lists GOES data timestamps available since the given time.
// GOES data is organized as ABI-L2-MCMIPC/YYYY/DDD/HH/ where DDD is day-of-year.
func (s *GOESSource) CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error) {
	prefixes := goesPrefixesForRange(since)

	seen := make(map[time.Time]struct{})
	var timestamps []time.Time

	for _, prefix := range prefixes {
		input := &s3.ListObjectsV2Input{
			Bucket:    aws.String(s.bucket),
			Prefix:    aws.String(prefix),
			Delimiter: aws.String("/"),
		}

		output, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing %s/%s: %w", s.bucket, prefix, err)
		}

		for _, cp := range output.CommonPrefixes {
			if cp.Prefix == nil {
				continue
			}
			ts, ok := ParseGOESKey(*cp.Prefix)
			if !ok {
				continue
			}
			if ts.After(since) {
				if _, exists := seen[ts]; !exists {
					seen[ts] = struct{}{}
					timestamps = append(timestamps, ts)
				}
			}
		}
	}

	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i].Before(timestamps[j])
	})

	return timestamps, nil
}

// goesPrefixesForRange generates GOES S3 key prefixes for the date range.
// GOES data is organized as ABI-L2-MCMIPC/YYYY/DDD/ where DDD is zero-padded day-of-year.
func goesPrefixesForRange(since time.Time) []string {
	now := time.Now().UTC()
	sinceDate := since.UTC().Truncate(24 * time.Hour)
	nowDate := now.Truncate(24 * time.Hour)

	var prefixes []string
	for d := sinceDate; !d.After(nowDate); d = d.Add(24 * time.Hour) {
		doy := d.YearDay()
		prefixes = append(prefixes, fmt.Sprintf("ABI-L2-MCMIPC/%d/%03d/", d.Year(), doy))
	}

	return prefixes
}

// ParseGOESKey extracts a timestamp from a GOES ABI-L2-MCMIPC key prefix.
// Keys match the pattern: ABI-L2-MCMIPC/YYYY/DDD/HH/
// Returns the parsed time (at the start of that hour) and true if successful.
func ParseGOESKey(key string) (time.Time, bool) {
	matches := goesKeyPattern.FindStringSubmatch(key)
	if matches == nil {
		return time.Time{}, false
	}

	year := matches[1]
	dayOfYear := matches[2]
	hour := matches[3]

	// Parse the base date from year and day-of-year.
	baseStr := fmt.Sprintf("%s %s", year, dayOfYear)
	base, err := time.Parse("2006 002", baseStr)
	if err != nil {
		return time.Time{}, false
	}

	// Parse hour and add to base date.
	var h int
	_, err = fmt.Sscanf(hour, "%d", &h)
	if err != nil || h < 0 || h > 23 {
		return time.Time{}, false
	}

	ts := base.Add(time.Duration(h) * time.Hour).UTC()
	return ts, true
}

// MRMSSource implements UpstreamSource for MRMS radar data.
// Used by the Nowcast pipeline for radar data availability checking.
type MRMSSource struct {
	client S3ListClient
	bucket string // e.g., "noaa-mrms-pds"
	logger *slog.Logger
}

// NewMRMSSource creates a new MRMS upstream source.
func NewMRMSSource(client S3ListClient, bucket string, logger *slog.Logger) *MRMSSource {
	if logger == nil {
		logger = slog.Default()
	}
	return &MRMSSource{
		client: client,
		bucket: bucket,
		logger: logger,
	}
}

// Name returns the forecast type for this source.
func (s *MRMSSource) Name() types.ForecastType {
	return types.ForecastNowcast
}

// CheckAvailability lists MRMS data timestamps available since the given time.
// MRMS data is organized under CONUS/MergedReflectivityComposite_00.50/.
func (s *MRMSSource) CheckAvailability(ctx context.Context, since time.Time) ([]time.Time, error) {
	// MRMS data is organized by date under the product prefix.
	prefix := "CONUS/MergedReflectivityComposite_00.50/"

	input := &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	}

	seen := make(map[time.Time]struct{})
	var timestamps []time.Time

	// Paginate through results.
	for {
		output, err := s.client.ListObjectsV2(ctx, input)
		if err != nil {
			return nil, fmt.Errorf("listing %s/%s: %w", s.bucket, prefix, err)
		}

		for _, obj := range output.Contents {
			if obj.Key == nil {
				continue
			}
			ts, ok := ParseMRMSKey(*obj.Key)
			if !ok {
				continue
			}
			// Truncate to the minute for deduplication (MRMS updates every ~2 minutes).
			truncated := ts.Truncate(time.Minute)
			if truncated.After(since) {
				if _, exists := seen[truncated]; !exists {
					seen[truncated] = struct{}{}
					timestamps = append(timestamps, truncated)
				}
			}
		}

		if output.IsTruncated == nil || !*output.IsTruncated {
			break
		}
		input.ContinuationToken = output.NextContinuationToken
	}

	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i].Before(timestamps[j])
	})

	return timestamps, nil
}

// ParseMRMSKey extracts a timestamp from an MRMS radar data key.
// Keys contain the pattern: YYYYMMDD-HHmmss in the filename.
// Returns the parsed time and true if successful.
func ParseMRMSKey(key string) (time.Time, bool) {
	matches := mrmsKeyPattern.FindStringSubmatch(key)
	if matches == nil {
		return time.Time{}, false
	}

	dateStr := fmt.Sprintf("%s%s%s %s%s%s", matches[1], matches[2], matches[3], matches[4], matches[5], matches[6])
	ts, err := time.Parse("20060102 150405", dateStr)
	if err != nil {
		return time.Time{}, false
	}

	return ts.UTC(), true
}
