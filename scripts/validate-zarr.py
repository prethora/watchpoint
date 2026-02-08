#!/usr/bin/env python3
"""
Validate a Zarr store produced by the WatchPoint RunPod inference worker.

Checks the output against the canonical schema defined in 11-runpod.md
Sections 3 and 7.3:
  - All 5 canonical variables present with correct dims and dtype
  - Chunk sizes match halo-padded tile grid
  - Metadata attributes present
  - _SUCCESS marker exists
  - No NaN or Inf values
  - Values within physical bounds

Usage:
    python3 scripts/validate-zarr.py s3://bucket/medium_range/2026-02-07T06:00:00Z/
    python3 scripts/validate-zarr.py /tmp/local_zarr_output

For S3 URIs, uses s3fs. Respects AWS_ENDPOINT_URL for local MinIO.
"""

from __future__ import annotations

import argparse
import os
import sys

import numpy as np

# ---------------------------------------------------------------------------
# Constants (must match zarr_writer.py and canonical_translator.py)
# ---------------------------------------------------------------------------
CANONICAL_VARIABLES = [
    "temperature_c",
    "wind_speed_kmh",
    "precipitation_mm",
    "precipitation_probability",
    "humidity_percent",
]

EXPECTED_DIMS = ("time", "lat", "lon")

VARIABLE_BOUNDS: dict[str, tuple[float, float]] = {
    "temperature_c": (-90.0, 65.0),
    "wind_speed_kmh": (0.0, 500.0),
    "precipitation_mm": (0.0, 1000.0),
    "precipitation_probability": (0.0, 100.0),
    "humidity_percent": (0.0, 100.0),
}

# Halo-padded chunk sizes from zarr_writer.py
HALO_LAT_CHUNK = 92   # 90 + 2 halo pixels
HALO_LON_CHUNK = 182  # 180 + 2 halo pixels

SUCCESS_MARKER = "_SUCCESS"

REQUIRED_ATTRS = ["model_name", "model_version", "schema_version"]


def open_store(path: str) -> "zarr.Group":
    """Open a Zarr store from a local path or S3 URI."""
    import zarr

    if path.startswith("s3://"):
        import s3fs

        endpoint_url = os.environ.get("AWS_ENDPOINT_URL")
        fs_kwargs: dict = {}
        if endpoint_url:
            fs_kwargs["endpoint_url"] = endpoint_url
            fs_kwargs["anon"] = False
        fs = s3fs.S3FileSystem(**fs_kwargs)
        store = s3fs.S3Map(root=path.rstrip("/"), s3=fs, check=False)
        return zarr.open_group(store, mode="r")
    else:
        return zarr.open_group(path, mode="r")


def check_success_marker(path: str) -> bool:
    """Check if _SUCCESS marker exists."""
    if path.startswith("s3://"):
        import s3fs

        endpoint_url = os.environ.get("AWS_ENDPOINT_URL")
        fs_kwargs: dict = {}
        if endpoint_url:
            fs_kwargs["endpoint_url"] = endpoint_url
            fs_kwargs["anon"] = False
        fs = s3fs.S3FileSystem(**fs_kwargs)
        marker_path = path.rstrip("/") + "/" + SUCCESS_MARKER
        return fs.exists(marker_path)
    else:
        return os.path.exists(os.path.join(path, SUCCESS_MARKER))


def validate(path: str) -> list[str]:
    """
    Validate a Zarr store. Returns a list of error messages (empty = valid).
    """
    errors: list[str] = []

    # 1. Check _SUCCESS marker
    if not check_success_marker(path):
        errors.append(f"Missing {SUCCESS_MARKER} marker at root")

    # 2. Open as Zarr v2 store
    try:
        root = open_store(path)
    except Exception as exc:
        errors.append(f"Failed to open Zarr store: {exc}")
        return errors

    # 3. Check .zattrs metadata
    attrs = dict(root.attrs)
    for attr in REQUIRED_ATTRS:
        if attr not in attrs:
            errors.append(f"Missing required attribute: {attr}")

    # 4. Check all canonical variables
    for var_name in CANONICAL_VARIABLES:
        if var_name not in root:
            errors.append(f"Missing canonical variable: {var_name}")
            continue

        arr = root[var_name]

        # Check dimensions
        if hasattr(arr, "attrs") and "_ARRAY_DIMENSIONS" in arr.attrs:
            dims = tuple(arr.attrs["_ARRAY_DIMENSIONS"])
            if dims != EXPECTED_DIMS:
                errors.append(
                    f"Variable '{var_name}': expected dims {EXPECTED_DIMS}, got {dims}"
                )

        # Check dtype
        if arr.dtype != np.float32:
            errors.append(
                f"Variable '{var_name}': expected dtype float32, got {arr.dtype}"
            )

        # Check chunk sizes (lat and lon dimensions)
        if len(arr.chunks) >= 3:
            lat_chunk = arr.chunks[1]
            lon_chunk = arr.chunks[2]
            if lat_chunk != HALO_LAT_CHUNK:
                errors.append(
                    f"Variable '{var_name}': expected lat chunk {HALO_LAT_CHUNK}, got {lat_chunk}"
                )
            if lon_chunk != HALO_LON_CHUNK:
                errors.append(
                    f"Variable '{var_name}': expected lon chunk {HALO_LON_CHUNK}, got {lon_chunk}"
                )

        # Check data quality
        data = arr[:]
        if not np.all(np.isfinite(data)):
            non_finite = int(np.sum(~np.isfinite(data)))
            errors.append(
                f"Variable '{var_name}': {non_finite} non-finite values (NaN or Inf)"
            )
        else:
            lo, hi = VARIABLE_BOUNDS[var_name]
            below = int(np.sum(data < lo))
            above = int(np.sum(data > hi))
            if below > 0 or above > 0:
                errors.append(
                    f"Variable '{var_name}': {below} below {lo}, {above} above {hi}"
                )

    return errors


def main() -> int:
    parser = argparse.ArgumentParser(
        description="Validate a WatchPoint Zarr store against the canonical schema.",
    )
    parser.add_argument(
        "path",
        help="Zarr store path (local directory or s3://bucket/prefix/)",
    )
    parser.add_argument(
        "--quiet", "-q",
        action="store_true",
        help="Only print errors, no success messages",
    )
    args = parser.parse_args()

    if not args.quiet:
        print(f"Validating Zarr store: {args.path}")
        print()

    errors = validate(args.path)

    if errors:
        print(f"FAILED — {len(errors)} error(s):\n")
        for i, err in enumerate(errors, 1):
            print(f"  {i}. {err}")
        return 1
    else:
        if not args.quiet:
            print("PASSED — All checks passed.")
        return 0


if __name__ == "__main__":
    sys.exit(main())
