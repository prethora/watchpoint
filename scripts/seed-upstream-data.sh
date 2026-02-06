#!/usr/bin/env bash
# seed-upstream-data.sh
#
# Populates the local MinIO instance with placeholder files that simulate
# upstream NOAA data availability.  The Data Poller (FCST-003) discovers new
# forecast runs by listing objects in these buckets; this script ensures there
# is something to find.
#
# Buckets seeded:
#   1. noaa-gfs-bdp-pds       -- GFS model data (GRIB2, every 6 hours)
#   2. noaa-goes16            -- GOES-16 ABI-L2 imagery (NetCDF, every 5 min)
#   3. noaa-mrms-pds          -- MRMS radar composites (GRIB2, every 2 min)
#
# Key formats mirror the real NOAA Open Data buckets so the UpstreamSource
# implementation can parse timestamps without special-casing local dev.
#
# Usage:
#   ./scripts/seed-upstream-data.sh              # default: MinIO at localhost:9000
#   MINIO_ENDPOINT=http://minio:9000 ./scripts/seed-upstream-data.sh  # inside Docker
#
# The script is idempotent -- running it multiple times is safe.

set -euo pipefail

# ---------------------------------------------------------------------------
# Preflight checks
# ---------------------------------------------------------------------------
if ! command -v aws >/dev/null 2>&1; then
    echo "ERROR: aws CLI is required but not found in PATH." >&2
    echo "Install it: https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html" >&2
    exit 1
fi

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
MINIO_ENDPOINT="${MINIO_ENDPOINT:-http://localhost:9000}"
AWS_REGION="${AWS_REGION:-us-east-1}"

# Credentials -- must match the MinIO root user in docker-compose.yml.
export AWS_ACCESS_KEY_ID="${AWS_ACCESS_KEY_ID:-minioadmin}"
export AWS_SECRET_ACCESS_KEY="${AWS_SECRET_ACCESS_KEY:-minioadmin}"
export AWS_DEFAULT_REGION="${AWS_REGION}"

BUCKETS=(
    "noaa-gfs-bdp-pds"
    "noaa-goes16"
    "noaa-mrms-pds"
)

# Number of recent GFS cycles to seed (each cycle is 6 hours apart).
GFS_CYCLES=4

# Number of recent GOES scans to seed (each scan is ~10 minutes apart,
# seeding a handful is enough for intersection testing with MRMS).
GOES_SCANS=6

# Number of recent MRMS files to seed (each ~10 minutes apart).
MRMS_FILES=6

# GFS forecast hours to include per cycle.
GFS_FHOURS=(000 003 006 012 024 048)

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

# aws_s3: wrapper that injects the MinIO endpoint.
aws_s3() {
    aws --endpoint-url "$MINIO_ENDPOINT" s3 "$@" --region "$AWS_REGION" 2>/dev/null
}

aws_s3api() {
    aws --endpoint-url "$MINIO_ENDPOINT" s3api "$@" --region "$AWS_REGION" 2>/dev/null
}

# create_bucket: idempotent bucket creation.
create_bucket() {
    local bucket="$1"
    if aws_s3api head-bucket --bucket "$bucket" 2>/dev/null; then
        echo "  OK: $bucket already exists."
    else
        if aws --endpoint-url "$MINIO_ENDPOINT" s3 mb "s3://$bucket" --region "$AWS_REGION"; then
            echo "  OK: $bucket created."
        else
            echo "  FAIL: Could not create $bucket. Is MinIO running at $MINIO_ENDPOINT?" >&2
            return 1
        fi
    fi
}

# upload_placeholder: uploads a small placeholder file to the given S3 key.
# Content is a brief text marker -- the Data Poller only needs the key to
# exist; it never reads file contents.  Uses --quiet to suppress per-file
# progress output.  Errors still surface via set -e / pipefail.
upload_placeholder() {
    local bucket="$1"
    local key="$2"
    local marker="placeholder: $(date -u +%Y-%m-%dT%H:%M:%SZ)"

    printf '%s' "$marker" | aws --endpoint-url "$MINIO_ENDPOINT" s3 cp - \
        "s3://${bucket}/${key}" --region "$AWS_REGION" --quiet
}

# ---------------------------------------------------------------------------
# Platform-safe date arithmetic.
# macOS `date` does not support -d; GNU `date` does not support -v.
# We detect which flavour is available and define a helper accordingly.
# ---------------------------------------------------------------------------
if date -v-1H >/dev/null 2>&1; then
    # BSD / macOS date
    date_offset_hours() {
        local offset_hours="$1"
        local fmt="$2"
        date -u -v"${offset_hours}H" +"$fmt"
    }
    date_offset_minutes() {
        local offset_minutes="$1"
        local fmt="$2"
        date -u -v"${offset_minutes}M" +"$fmt"
    }
else
    # GNU date
    date_offset_hours() {
        local offset_hours="$1"
        local fmt="$2"
        date -u -d "${offset_hours} hours" +"$fmt"
    }
    date_offset_minutes() {
        local offset_minutes="$1"
        local fmt="$2"
        date -u -d "${offset_minutes} minutes" +"$fmt"
    }
fi

echo "============================================"
echo " WatchPoint Upstream Data Seeder"
echo "============================================"
echo ""
echo " MinIO endpoint: $MINIO_ENDPOINT"
echo " Timestamp base: $(date -u +%Y-%m-%dT%H:%M:%SZ)"
echo ""

# ===========================================================================
# 1. Create Buckets
# ===========================================================================
echo "--- [1/4] Creating upstream buckets ---"
echo ""

for bucket in "${BUCKETS[@]}"; do
    create_bucket "$bucket"
done

echo ""

# ===========================================================================
# 2. Seed GFS Data
#
# Real key format (NOAA Open Data):
#   gfs.YYYYMMDD/HH/atmos/gfs.tHHz.pgrb2.0p25.fHHH
#
# GFS cycles run at 00, 06, 12, 18 UTC.  We seed the most recent N cycles
# relative to now so the Data Poller always finds fresh data.
# ===========================================================================
echo "--- [2/4] Seeding GFS data (noaa-gfs-bdp-pds) ---"
echo ""

# Determine the current UTC hour and snap to the most recent GFS cycle.
CURRENT_HOUR=$(date -u +%H)
CYCLE_HOUR=$(( (CURRENT_HOUR / 6) * 6 ))

gfs_count=0
for (( i=0; i<GFS_CYCLES; i++ )); do
    # Hours offset from now to reach this cycle.
    # i=0 is the most recent cycle, i=1 is 6 hours before, etc.
    hours_back=$(( i * 6 + (CURRENT_HOUR - CYCLE_HOUR) ))
    offset_sign="-"

    cycle_date=$(date_offset_hours "${offset_sign}${hours_back}" "%Y%m%d")
    cycle_hh=$(date_offset_hours "${offset_sign}${hours_back}" "%H")

    # Snap cycle_hh to the nearest 6-hour boundary (should already be, but be safe).
    cycle_hh_int=$((10#$cycle_hh))
    cycle_hh_snapped=$(printf "%02d" $(( (cycle_hh_int / 6) * 6 )))

    for fhour in "${GFS_FHOURS[@]}"; do
        key="gfs.${cycle_date}/${cycle_hh_snapped}/atmos/gfs.t${cycle_hh_snapped}z.pgrb2.0p25.f${fhour}"
        upload_placeholder "noaa-gfs-bdp-pds" "$key"
        gfs_count=$((gfs_count + 1))
    done

    echo "  Cycle: gfs.${cycle_date}/${cycle_hh_snapped} (${#GFS_FHOURS[@]} forecast hours)"
done

echo "  Total GFS files: $gfs_count"
echo ""

# ===========================================================================
# 3. Seed GOES-16 Data
#
# Real key format (NOAA Open Data):
#   ABI-L2-MCMIPF/YYYY/DDD/HH/OR_ABI-L2-MCMIPF-M6_G16_sYYYYDDDHHMMSSS_...nc
#
# DDD is the three-digit day of year.
# The Data Poller lists objects under the prefix ABI-L2-MCMIPF/YYYY/DDD/HH/
# and parses timestamps from the filenames.
# ===========================================================================
echo "--- [3/4] Seeding GOES-16 data (noaa-goes16) ---"
echo ""

goes_count=0
for (( i=0; i<GOES_SCANS; i++ )); do
    minutes_back=$(( i * 10 ))
    offset_sign="-"

    scan_year=$(date_offset_minutes "${offset_sign}${minutes_back}" "%Y")
    scan_doy=$(date_offset_minutes "${offset_sign}${minutes_back}" "%j")
    scan_hh=$(date_offset_minutes "${offset_sign}${minutes_back}" "%H")
    scan_mm=$(date_offset_minutes "${offset_sign}${minutes_back}" "%M")
    scan_ss="000"  # Tenths of second -- placeholder

    # Build the filename to resemble the real GOES naming convention.
    # Real format: OR_ABI-L2-MCMIPF-M6_G16_sYYYYDDDHHMMSSS_eYYYYDDDHHMMSSS_cYYYYDDDHHMMSSS.nc
    start_ts="${scan_year}${scan_doy}${scan_hh}${scan_mm}${scan_ss}"
    filename="OR_ABI-L2-MCMIPF-M6_G16_s${start_ts}_e${start_ts}_c${start_ts}.nc"

    key="ABI-L2-MCMIPF/${scan_year}/${scan_doy}/${scan_hh}/${filename}"
    upload_placeholder "noaa-goes16" "$key"
    goes_count=$((goes_count + 1))

    echo "  Scan: ${scan_year}/${scan_doy}/${scan_hh}/${scan_hh}:${scan_mm} UTC"
done

echo "  Total GOES files: $goes_count"
echo ""

# ===========================================================================
# 4. Seed MRMS Data
#
# Real key format (NOAA Open Data):
#   CONUS/MergedReflectivityQC_00.50/YYYYMMDD/MRMS_MergedReflectivityQC_00.50_YYYYMMDD-HHmmss.grib2.gz
#
# The Data Poller parses the YYYYMMDD-HHmmss timestamp from the filename.
# We align MRMS timestamps with GOES timestamps so the Nowcast intersection
# logic (5-minute tolerance window) can find matching pairs.
# ===========================================================================
echo "--- [4/4] Seeding MRMS data (noaa-mrms-pds) ---"
echo ""

mrms_count=0
for (( i=0; i<MRMS_FILES; i++ )); do
    minutes_back=$(( i * 10 ))
    offset_sign="-"

    mrms_date=$(date_offset_minutes "${offset_sign}${minutes_back}" "%Y%m%d")
    mrms_hh=$(date_offset_minutes "${offset_sign}${minutes_back}" "%H")
    mrms_mm=$(date_offset_minutes "${offset_sign}${minutes_back}" "%M")
    mrms_ss="00"

    filename="MRMS_MergedReflectivityQC_00.50_${mrms_date}-${mrms_hh}${mrms_mm}${mrms_ss}.grib2.gz"
    key="CONUS/MergedReflectivityQC_00.50/${mrms_date}/${filename}"

    upload_placeholder "noaa-mrms-pds" "$key"
    mrms_count=$((mrms_count + 1))

    echo "  File: ${mrms_date} ${mrms_hh}:${mrms_mm}:${mrms_ss} UTC"
done

echo "  Total MRMS files: $mrms_count"
echo ""

# ===========================================================================
# Summary
# ===========================================================================
echo "============================================"
echo " Upstream Data Seeder: COMPLETE"
echo "============================================"
echo ""
echo " noaa-gfs-bdp-pds:  $gfs_count files  ($GFS_CYCLES cycles x ${#GFS_FHOURS[@]} forecast hours)"
echo " noaa-goes16:        $goes_count files  ($GOES_SCANS scans, ~10 min apart)"
echo " noaa-mrms-pds:      $mrms_count files  ($MRMS_FILES files, ~10 min apart)"
echo ""
echo " GOES and MRMS timestamps are aligned for Nowcast intersection testing."
echo ""
echo " Verify with:"
echo "   aws --endpoint-url $MINIO_ENDPOINT s3 ls s3://noaa-gfs-bdp-pds/ --recursive"
echo "   aws --endpoint-url $MINIO_ENDPOINT s3 ls s3://noaa-goes16/ --recursive"
echo "   aws --endpoint-url $MINIO_ENDPOINT s3 ls s3://noaa-mrms-pds/ --recursive"
echo ""
echo "============================================"
