"""
ZarrWriter: Handles chunking, compression, halo padding, and Zarr store writes.

Implements the Write-Side Halo strategy defined in 11-runpod.md Section 3.3.
Each spatial chunk is padded with 1 extra pixel on each edge where neighboring
data exists, so that downstream readers (eval-worker) can perform bilinear
interpolation at tile boundaries using a single chunk fetch.

Zarr v2 format with Zstd(level=3) compression.
"""

from __future__ import annotations

import os
import pathlib
from datetime import datetime, timezone

import numpy as np
import xarray as xr
import zarr
from numcodecs import Zstd

# ---------------------------------------------------------------------------
# Constants from 11-runpod.md Section 3
# ---------------------------------------------------------------------------
ZARR_FORMAT_VERSION = 2  # Documenting the target format; zarr v2 is the default
ZSTD_COMPRESSION_LEVEL = 3

# Tile-aligned chunk sizes (before halo padding)
TILE_LAT_CHUNK = 90   # 22.5 degrees / 0.25 degree resolution
TILE_LON_CHUNK = 180   # 45.0 degrees / 0.25 degree resolution

# Halo-padded chunk sizes (what actually gets stored)
HALO_LAT_CHUNK = TILE_LAT_CHUNK + 2   # 92
HALO_LON_CHUNK = TILE_LON_CHUNK + 2   # 182

HALO_SIZE = 1  # 1-pixel overlap on each side

# Canonical variables from 11-runpod.md Section 3.4
CANONICAL_VARIABLES: list[str] = [
    "precipitation_mm",
    "precipitation_probability",
    "temperature_c",
    "wind_speed_kmh",
    "humidity_percent",
]

# Schema version for .zattrs metadata
SCHEMA_VERSION = "1.0"

# Commit marker file name
SUCCESS_MARKER = "_SUCCESS"


class ZarrWriter:
    """
    Writes xarray Datasets as Zarr v2 stores with Write-Side Halo padding.

    The halo strategy duplicates 1 pixel of data at each chunk boundary so
    that the eval-worker can fetch exactly one chunk per tile and still have
    the neighbor pixels needed for bilinear interpolation at the edges.

    Usage::

        writer = ZarrWriter(model_name="atlas", model_version="v1.2")
        writer.write(dataset, "/path/to/output")
    """

    def __init__(
        self,
        model_name: str = "atlas",
        model_version: str = "v1.0",
    ) -> None:
        self.model_name = model_name
        self.model_version = model_version
        self._compressor = Zstd(level=ZSTD_COMPRESSION_LEVEL)

    # ------------------------------------------------------------------
    # Public API
    # ------------------------------------------------------------------

    def write(self, dataset: xr.Dataset, destination: str) -> None:
        """
        Write an xarray Dataset to a Zarr store with halo-padded chunks.

        Parameters
        ----------
        dataset : xr.Dataset
            Must have dimensions (time, lat, lon) and contain variables
            from CANONICAL_VARIABLES.
        destination : str
            Output path. Can be a local filesystem path or an S3 URI.
            For S3, the s3fs library handles transparent writes.
        """
        self._validate_dataset(dataset)

        # Build the halo-padded arrays
        padded_ds = self._apply_halo_padding(dataset)

        # Determine chunk sizes for the padded dataset
        time_size = padded_ds.sizes["time"]
        chunks = {
            "time": time_size,       # Full horizon in one chunk
            "lat": HALO_LAT_CHUNK,   # 92
            "lon": HALO_LON_CHUNK,   # 182
        }

        # Set encoding for each variable: zstd compression, float32
        encoding = {}
        for var_name in padded_ds.data_vars:
            encoding[var_name] = {
                "compressor": self._compressor,
                "dtype": "float32",
                "chunks": (chunks["time"], chunks["lat"], chunks["lon"]),
            }

        # Write coordinate arrays with compression too
        for coord_name in ["time", "lat", "lon"]:
            encoding[coord_name] = {
                "compressor": self._compressor,
            }

        # Write the Zarr store in v2 format (required for numcodecs compressors).
        # zarr v3 library defaults to zarr_format=3 which uses a different codec
        # system; explicitly requesting v2 keeps numcodecs.Zstd compatible.
        padded_ds.to_zarr(
            destination,
            mode="w",
            encoding=encoding,
            zarr_format=2,
        )

        # Write metadata attributes
        self._write_metadata(destination)

        # Write the _SUCCESS commit marker
        self._write_success_marker(destination)

    def clean_slate(self, destination: str) -> None:
        """
        Remove any existing data at the destination before writing.

        Implements the Clean Slate Policy from 11-runpod.md Section 7.1:
        Purge any potentially corrupt chunks from previous failed runs.

        Parameters
        ----------
        destination : str
            The output path to clean (local or S3).
        """
        if destination.startswith("s3://"):
            self._clean_slate_s3(destination)
        else:
            self._clean_slate_local(destination)

    # ------------------------------------------------------------------
    # Halo padding logic
    # ------------------------------------------------------------------

    def _apply_halo_padding(self, ds: xr.Dataset) -> xr.Dataset:
        """
        Apply Write-Side Halo padding to the dataset.

        For each chunk boundary in lat and lon, duplicate the boundary pixels
        to create 1-pixel overlap. Every padded chunk is exactly
        (chunk_size + 2) elements: 1 left halo + chunk_size core + 1 right halo.

        At domain edges (poles, date line), halos use clamped (edge-repeated)
        values since no neighboring data exists beyond the boundary.

        Partial trailing chunks (when dim_size % chunk_size != 0) are padded
        with the last valid pixel to maintain uniform chunk size.
        """
        lat_vals = ds["lat"].values
        lon_vals = ds["lon"].values

        # Build index maps: for each chunk, determine the slice of original
        # data it covers, plus the halo indices from neighbors.
        padded_lat_indices = self._compute_padded_indices(len(lat_vals), TILE_LAT_CHUNK)
        padded_lon_indices = self._compute_padded_indices(len(lon_vals), TILE_LON_CHUNK)

        # Build padded coordinate arrays
        padded_lat = lat_vals[padded_lat_indices]
        padded_lon = lon_vals[padded_lon_indices]

        # Build padded data variables
        data_vars = {}
        for var_name in ds.data_vars:
            orig_data = ds[var_name].values  # shape: (time, lat, lon)
            # Apply lat padding: index along axis 1
            padded_data = orig_data[:, padded_lat_indices, :]
            # Apply lon padding: index along axis 2
            padded_data = padded_data[:, :, padded_lon_indices]
            data_vars[var_name] = (["time", "lat", "lon"], padded_data)

        # Build the padded time coordinates (unchanged)
        time_vals = ds["time"].values

        padded_ds = xr.Dataset(
            data_vars=data_vars,
            coords={
                "time": ("time", time_vals),
                "lat": ("lat", padded_lat),
                "lon": ("lon", padded_lon),
            },
        )

        return padded_ds

    @staticmethod
    def _compute_padded_indices(dim_size: int, chunk_size: int) -> np.ndarray:
        """
        Compute the array of indices into the original dimension that
        produces a halo-padded version suitable for Zarr chunking.

        **Critical invariant**: Every padded chunk MUST be exactly
        ``chunk_size + 2`` elements so that Zarr's fixed-size chunk
        boundaries align perfectly with tile+halo boundaries. This allows
        the eval-worker to fetch exactly one Zarr chunk per tile.

        For each tile of ``chunk_size`` elements:
          - 1 left halo pixel (from left neighbor, or edge-clamped)
          - ``chunk_size`` core pixels (the tile data)
          - 1 right halo pixel (from right neighbor, or edge-clamped)

        At domain edges where no neighbor exists, the edge pixel is
        repeated (clamped indexing). For partial trailing chunks (when
        dim_size is not evenly divisible by chunk_size), the remaining
        core pixels are padded with the last valid pixel to fill the
        chunk to exactly ``chunk_size + 2`` elements.

        Parameters
        ----------
        dim_size : int
            Total size of the original dimension (e.g., 721 for Atlas lat).
        chunk_size : int
            Core tile size before halo (e.g., 90 for lat).

        Returns
        -------
        np.ndarray
            Array of indices into the original dimension. Length is always
            ``num_chunks * (chunk_size + 2)``.

        Example
        -------
        For dim_size=10, chunk_size=5:
          Chunk 0: [0] + [0,1,2,3,4] + [5]  = [0,0,1,2,3,4,5]  (7 elements)
          Chunk 1: [4] + [5,6,7,8,9] + [9]  = [4,5,6,7,8,9,9]  (7 elements)
          Total padded: 14 elements
        """
        num_full_chunks = dim_size // chunk_size
        remainder = dim_size % chunk_size

        # Total number of chunks (including partial trailing chunk)
        num_chunks = num_full_chunks + (1 if remainder > 0 else 0)

        indices = []

        for c in range(num_chunks):
            start = c * chunk_size
            end = min(start + chunk_size, dim_size)
            core_size = end - start

            # Left halo: clamp to 0 at domain start
            left_halo_idx = max(start - HALO_SIZE, 0)
            indices.append(left_halo_idx)

            # Core indices
            for i in range(start, end):
                indices.append(i)

            # If this is a partial trailing chunk, pad the core with
            # the last valid pixel to fill to chunk_size
            padding_needed = chunk_size - core_size
            if padding_needed > 0:
                last_valid = end - 1
                for _ in range(padding_needed):
                    indices.append(last_valid)

            # Right halo: clamp to dim_size-1 at domain end
            right_halo_idx = min(end, dim_size - 1)
            indices.append(right_halo_idx)

        return np.array(indices, dtype=np.intp)

    # ------------------------------------------------------------------
    # Validation
    # ------------------------------------------------------------------

    @staticmethod
    def _validate_dataset(ds: xr.Dataset) -> None:
        """
        Validate that the dataset has the required structure.

        Raises ValueError if:
          - Missing required dimensions (time, lat, lon)
          - Missing any canonical variable
          - Data contains non-finite values (NaN, Inf)
        """
        required_dims = {"time", "lat", "lon"}
        actual_dims = set(ds.dims)
        missing_dims = required_dims - actual_dims
        if missing_dims:
            raise ValueError(
                f"Dataset is missing required dimensions: {missing_dims}. "
                f"Found: {actual_dims}"
            )

        missing_vars = []
        for var_name in CANONICAL_VARIABLES:
            if var_name not in ds.data_vars:
                missing_vars.append(var_name)
        if missing_vars:
            raise ValueError(
                f"Dataset is missing canonical variables: {missing_vars}"
            )

        # Output validation (11-runpod.md Section 7.3):
        # Scan for NaN/Inf to prevent cache poisoning
        for var_name in CANONICAL_VARIABLES:
            data = ds[var_name].values
            if not np.all(np.isfinite(data)):
                non_finite_count = int(np.sum(~np.isfinite(data)))
                raise ValueError(
                    f"Variable '{var_name}' contains {non_finite_count} "
                    f"non-finite values (NaN or Inf). "
                    f"Output validation failed per Section 7.3."
                )

    # ------------------------------------------------------------------
    # Metadata and commit marker
    # ------------------------------------------------------------------

    def _write_metadata(self, destination: str) -> None:
        """
        Write .zattrs metadata to the Zarr store root.

        Metadata schema from 11-runpod.md Section 3.5.
        """
        store = zarr.open(destination, mode="r+", zarr_format=2)
        store.attrs["model_name"] = self.model_name
        store.attrs["model_version"] = self.model_version
        store.attrs["schema_version"] = SCHEMA_VERSION
        store.attrs["generated_at"] = datetime.now(timezone.utc).isoformat()
        # Record halo configuration for readers
        store.attrs["halo_size"] = HALO_SIZE
        store.attrs["tile_lat_chunk"] = TILE_LAT_CHUNK
        store.attrs["tile_lon_chunk"] = TILE_LON_CHUNK

    def _write_success_marker(self, destination: str) -> None:
        """
        Write the _SUCCESS commit marker (0-byte file) to the store root.

        Per 11-runpod.md Section 3.1: Written only after all chunks persist.
        The Batcher triggers on this S3 event.
        """
        if destination.startswith("s3://"):
            self._write_success_marker_s3(destination)
        else:
            success_path = os.path.join(destination, SUCCESS_MARKER)
            # Create empty file
            pathlib.Path(success_path).touch()

    # ------------------------------------------------------------------
    # S3 operations (stubs for local testing, real impl uses boto3/s3fs)
    # ------------------------------------------------------------------

    @staticmethod
    def _write_success_marker_s3(destination: str) -> None:
        """Write _SUCCESS marker to S3."""
        import boto3

        # Parse s3://bucket/key
        parts = destination.replace("s3://", "").split("/", 1)
        bucket = parts[0]
        prefix = parts[1] if len(parts) > 1 else ""
        key = f"{prefix}/{SUCCESS_MARKER}" if prefix else SUCCESS_MARKER

        s3 = boto3.client("s3")
        s3.put_object(Bucket=bucket, Key=key, Body=b"")

    @staticmethod
    def _clean_slate_s3(destination: str) -> None:
        """Purge existing S3 objects at the destination prefix."""
        import boto3

        parts = destination.replace("s3://", "").split("/", 1)
        bucket = parts[0]
        prefix = parts[1] if len(parts) > 1 else ""

        s3 = boto3.client("s3")
        paginator = s3.get_paginator("list_objects_v2")

        objects_to_delete = []
        for page in paginator.paginate(Bucket=bucket, Prefix=prefix):
            for obj in page.get("Contents", []):
                objects_to_delete.append({"Key": obj["Key"]})

        # DeleteObjects supports up to 1000 keys per request
        while objects_to_delete:
            batch = objects_to_delete[:1000]
            objects_to_delete = objects_to_delete[1000:]
            s3.delete_objects(
                Bucket=bucket,
                Delete={"Objects": batch},
            )

    @staticmethod
    def _clean_slate_local(destination: str) -> None:
        """Remove local directory if it exists."""
        import shutil

        if os.path.exists(destination):
            shutil.rmtree(destination)
