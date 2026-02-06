"""
Forecast Reader: Zarr I/O abstraction for the Eval Worker.

Implements the ``ForecastReader`` interface from ``07-eval-worker.md`` Section 4.1.
Reads Zarr-formatted forecast data from S3 (or MinIO in local dev) using xarray
and s3fs. Each tile corresponds to exactly one Zarr chunk thanks to the Write-Side
Halo strategy defined in ``11-runpod.md`` Section 3.3.

Tile Coordinate System:
    tile_id format: "{lat_index}.{lon_index}"
    lat_index = FLOOR((90.0 - lat) / 22.5)   -- 0..7 for 8 latitude bands
    lon_index = FLOOR(adjusted_lon / 45.0)    -- 0..7 for 8 longitude bands
        where adjusted_lon = lon if lon >= 0 else 360 + lon

    Each tile covers a 22.5 deg lat x 45.0 deg lon region.
    At 0.25 deg resolution: 90 lat points x 180 lon points per tile core.
    With 1px halo on each side: 92 x 182 per Zarr chunk.

Architecture References:
    - 07-eval-worker.md Section 4.1 (ForecastReader interface)
    - 11-runpod.md Section 3 (Zarr output contract)
    - runpod/zarr_writer.py (Write-Side Halo implementation)
    - Flow EVAL-001 step 9 (single chunk read)
    - Flow FAIL-008 (corruption handling)
"""

from __future__ import annotations

import logging
from abc import ABC, abstractmethod
from datetime import datetime
from typing import Any, Callable

import numpy as np
import xarray as xr

from worker.eval.models import ForecastType

# ---------------------------------------------------------------------------
# Logging
# ---------------------------------------------------------------------------
logger = logging.getLogger(__name__)

# ---------------------------------------------------------------------------
# Constants (mirror runpod/zarr_writer.py)
# ---------------------------------------------------------------------------

# Tile-aligned chunk sizes (before halo padding)
TILE_LAT_CHUNK = 90   # 22.5 degrees / 0.25 degree resolution
TILE_LON_CHUNK = 180   # 45.0 degrees / 0.25 degree resolution

# Halo padding
HALO_SIZE = 1

# Tile grid parameters
TILE_LAT_DEG = 22.5
TILE_LON_DEG = 45.0
GRID_RESOLUTION = 0.25

# Number of tiles
NUM_LAT_TILES = 8  # 180 / 22.5
NUM_LON_TILES = 8  # 360 / 45.0

# Canonical variables expected in the Zarr store
# From 11-runpod.md Section 3.4
CANONICAL_VARIABLES: list[str] = [
    "precipitation_mm",
    "precipitation_probability",
    "temperature_c",
    "wind_speed_kmh",
    "humidity_percent",
]

# Type alias for a metric emission callback.
# The Reader calls this when it detects corruption (FAIL-008).
# Signature: emit_metric(metric_name: str, value: float, unit: str, dimensions: dict)
# In production, this is backed by aws-lambda-powertools Metrics.
MetricEmitter = Callable[[str, float, str, dict[str, str]], None]

# Variable name mapping from raw Zarr names to canonical names.
# If the Zarr store already uses canonical names (which it does per spec),
# this serves as an identity mapping and validation list.
VARIABLE_NAME_MAP: dict[str, str] = {
    "precipitation_mm": "precipitation_mm",
    "precipitation_probability": "precipitation_probability",
    "temperature_c": "temperature_c",
    "wind_speed_kmh": "wind_speed_kmh",
    "humidity_percent": "humidity_percent",
    # Common raw model names that might appear in non-canonical stores:
    "t2m": "temperature_c",
    "tp": "precipitation_mm",
    "r2": "humidity_percent",
    "ws10": "wind_speed_kmh",
}


# ---------------------------------------------------------------------------
# Custom Exceptions (07-eval-worker.md Section 4.1)
# ---------------------------------------------------------------------------


class ForecastExpiredError(Exception):
    """Raised when the forecast data has been deleted or is unavailable.

    Per EVAL-001 step 9: If ``FileNotFound`` (expired), log warning,
    ACK message, exit. The Worker handler MUST catch this, log a warning
    (not error), and successfully acknowledge the SQS message to prevent
    DLQ loops.
    """

    pass


class ForecastCorruptError(Exception):
    """Raised when the forecast data is corrupt (checksum or format error).

    Per FAIL-008: The Reader MUST:
      1. Log a critical error with full context (tile_id, timestamp, error details)
      2. Emit a ``CorruptForecast`` metric to CloudWatch for alerting
      3. Raise a terminal ``ForecastCorruptError`` exception
    The Worker handler MUST catch this and ACK the SQS message to prevent
    infinite retry loops.
    """

    pass


# ---------------------------------------------------------------------------
# Schema Validation (07-eval-worker.md Section 4.1)
# ---------------------------------------------------------------------------


class ValidationResult:
    """Result of schema validation on a forecast dataset."""

    def __init__(self, valid: bool, errors: list[str] | None = None) -> None:
        self.valid = valid
        self.errors = errors or []

    def __repr__(self) -> str:
        return f"ValidationResult(valid={self.valid}, errors={self.errors})"


class SchemaValidator(ABC):
    """Abstract validator for forecast datasets."""

    @abstractmethod
    def validate(self, dataset: xr.Dataset) -> ValidationResult:
        """Checks for required variables, dimensions, and NaN values."""
        ...


class DefaultSchemaValidator(SchemaValidator):
    """Validates that a forecast dataset conforms to the Zarr output contract.

    Checks:
      - Required dimensions: time, lat, lon
      - Required canonical variables present
      - No NaN/Inf values in data variables (data corruption indicator)
    """

    def validate(self, dataset: xr.Dataset) -> ValidationResult:
        errors: list[str] = []

        # Check dimensions
        required_dims = {"time", "lat", "lon"}
        actual_dims = set(dataset.dims)
        missing_dims = required_dims - actual_dims
        if missing_dims:
            errors.append(
                f"Missing required dimensions: {sorted(missing_dims)}. "
                f"Found: {sorted(actual_dims)}"
            )

        # Check canonical variables
        missing_vars = [
            v for v in CANONICAL_VARIABLES if v not in dataset.data_vars
        ]
        if missing_vars:
            errors.append(f"Missing canonical variables: {missing_vars}")

        # Check for NaN/Inf values (corruption indicator)
        for var_name in CANONICAL_VARIABLES:
            if var_name in dataset.data_vars:
                data = dataset[var_name].values
                if not np.all(np.isfinite(data)):
                    non_finite_count = int(np.sum(~np.isfinite(data)))
                    errors.append(
                        f"Variable '{var_name}' contains {non_finite_count} "
                        f"non-finite values (NaN or Inf)"
                    )

        return ValidationResult(valid=len(errors) == 0, errors=errors)


# ---------------------------------------------------------------------------
# TileData (07-eval-worker.md Section 4.1)
# ---------------------------------------------------------------------------


class TileData:
    """Wrapper around xarray Dataset for a specific geospatial tile.

    Provides point extraction with bilinear interpolation and variable
    name mapping from raw model names to canonical variable names.

    Per spec: Data assumes Write-Side Halo (1px padding). Readers should
    fetch exactly one chunk per tile.
    """

    def __init__(self, dataset: xr.Dataset) -> None:
        self.ds = dataset

    def extract_point(self, lat: float, lon: float) -> dict[str, float]:
        """Extract weather values at a specific lat/lon using bilinear interpolation.

        Uses ``xarray.Dataset.interp()`` with linear interpolation to perform
        bilinear interpolation. The Write-Side Halo ensures that even points
        at tile edges have sufficient neighboring data for interpolation.

        Parameters
        ----------
        lat : float
            Latitude in degrees (-90 to 90).
        lon : float
            Longitude in degrees (-180 to 180).

        Returns
        -------
        dict[str, float]
            Dictionary of canonical variable names to interpolated values.
            Example: ``{"temperature_c": 24.5, "precipitation_mm": 0.3, ...}``
        """
        # Use xarray's interp for bilinear interpolation.
        # Select the first time step for point-in-time extraction.
        point = self.ds.interp(lat=lat, lon=lon, method="linear")

        result: dict[str, float] = {}
        for var_name in self.ds.data_vars:
            canonical_name = VARIABLE_NAME_MAP.get(var_name, var_name)
            values = point[var_name].values
            # If there is a time dimension, return the full time series as
            # the first time step value for simple point extraction.
            # The evaluator will handle time series for monitor mode.
            if values.ndim == 0:
                result[canonical_name] = float(values)
            else:
                # Return first timestep for simple point extraction
                result[canonical_name] = float(values.flat[0])

        return result

    def extract_timeseries(
        self, lat: float, lon: float
    ) -> dict[str, np.ndarray]:
        """Extract full time series at a specific lat/lon for Monitor Mode.

        Parameters
        ----------
        lat : float
            Latitude in degrees.
        lon : float
            Longitude in degrees.

        Returns
        -------
        dict[str, np.ndarray]
            Dictionary of canonical variable names to 1-D numpy arrays
            indexed by the time dimension.
        """
        point = self.ds.interp(lat=lat, lon=lon, method="linear")

        result: dict[str, np.ndarray] = {}
        for var_name in self.ds.data_vars:
            canonical_name = VARIABLE_NAME_MAP.get(var_name, var_name)
            result[canonical_name] = np.asarray(point[var_name].values)

        return result

    @property
    def times(self) -> np.ndarray:
        """Return the time coordinate values."""
        return self.ds["time"].values


# ---------------------------------------------------------------------------
# Tile Geometry Helpers
# ---------------------------------------------------------------------------


def parse_tile_id(tile_id: str) -> tuple[int, int]:
    """Parse a tile_id string into (lat_index, lon_index).

    Parameters
    ----------
    tile_id : str
        Tile identifier in format "lat_index.lon_index" (e.g., "3.4").

    Returns
    -------
    tuple[int, int]
        (lat_index, lon_index) both in range [0, 7].

    Raises
    ------
    ValueError
        If the tile_id format is invalid or indices are out of range.
    """
    parts = tile_id.split(".")
    if len(parts) != 2:
        raise ValueError(
            f"Invalid tile_id format: '{tile_id}'. "
            f"Expected 'lat_index.lon_index' (e.g., '3.4')."
        )

    try:
        lat_idx = int(parts[0])
        lon_idx = int(parts[1])
    except ValueError:
        raise ValueError(
            f"Invalid tile_id format: '{tile_id}'. "
            f"Indices must be integers."
        )

    if not (0 <= lat_idx < NUM_LAT_TILES):
        raise ValueError(
            f"Tile lat_index {lat_idx} out of range [0, {NUM_LAT_TILES - 1}]."
        )
    if not (0 <= lon_idx < NUM_LON_TILES):
        raise ValueError(
            f"Tile lon_index {lon_idx} out of range [0, {NUM_LON_TILES - 1}]."
        )

    return lat_idx, lon_idx


def tile_lat_bounds(lat_idx: int) -> tuple[float, float]:
    """Calculate the latitude bounds for a tile (core region, no halo).

    Latitude runs from 90.0 (north) to -90.0 (south).
    lat_index 0 covers [90.0, 67.5], lat_index 7 covers [-67.5, -90.0].

    Returns
    -------
    tuple[float, float]
        (lat_max, lat_min) -- note descending order matching the coordinate system.
    """
    lat_max = 90.0 - lat_idx * TILE_LAT_DEG
    lat_min = lat_max - TILE_LAT_DEG
    return lat_max, lat_min


def tile_lon_bounds(lon_idx: int) -> tuple[float, float]:
    """Calculate the longitude bounds for a tile (core region, no halo).

    The tile grid uses adjusted longitude [0, 360) internally, but the
    Zarr store uses [-180, 180). This function returns bounds in the
    [-180, 180) system.

    The DB formula is::

        lon_idx = FLOOR(adjusted_lon / 45.0)
        where adjusted_lon = lon if lon >= 0 else 360 + lon

    Adjusted longitude 0 corresponds to actual longitude 0.
    The mapping from lon_idx to actual longitude ranges::

        lon_idx 0: adjusted [0, 45)    -> actual [0, 45)
        lon_idx 1: adjusted [45, 90)   -> actual [45, 90)
        lon_idx 2: adjusted [90, 135)  -> actual [90, 135)
        lon_idx 3: adjusted [135, 180) -> actual [135, 180)
        lon_idx 4: adjusted [180, 225) -> actual [-180, -135)
        lon_idx 5: adjusted [225, 270) -> actual [-135, -90)
        lon_idx 6: adjusted [270, 315) -> actual [-90, -45)
        lon_idx 7: adjusted [315, 360) -> actual [-45, 0)

    Returns
    -------
    tuple[float, float]
        (lon_min, lon_max) in the [-180, 180) system.
    """
    adj_lon_min = lon_idx * TILE_LON_DEG
    adj_lon_max = adj_lon_min + TILE_LON_DEG

    def adjusted_to_actual(adj: float) -> float:
        if adj >= 180.0:
            return adj - 360.0
        return adj

    lon_min = adjusted_to_actual(adj_lon_min)
    lon_max = adjusted_to_actual(adj_lon_max)

    return lon_min, lon_max


def _build_zarr_path(
    bucket: str, forecast_type: ForecastType, timestamp: datetime
) -> str:
    """Build the S3 path to the Zarr store.

    The path format follows the convention established in 11-runpod.md:
        s3://{bucket}/{forecast_type}/{run_timestamp_iso}/

    Parameters
    ----------
    bucket : str
        S3 bucket name (e.g., "watchpoint-forecasts").
    forecast_type : ForecastType
        The forecast model type.
    timestamp : datetime
        The forecast run timestamp.

    Returns
    -------
    str
        S3 URI to the Zarr store root.
    """
    ts_str = timestamp.strftime("%Y-%m-%dT%H:%M:%SZ")
    return f"s3://{bucket}/{forecast_type.value}/{ts_str}"


# ---------------------------------------------------------------------------
# ForecastReader Interface (07-eval-worker.md Section 4.1)
# ---------------------------------------------------------------------------


class ForecastReader(ABC):
    """Abstract base class for reading forecast Zarr data."""

    @abstractmethod
    def load_tile(
        self,
        model: ForecastType,
        timestamp: datetime,
        tile_id: str,
    ) -> TileData:
        """Fetch a Zarr chunk from S3 for the specified tile.

        MUST run SchemaValidator before returning.

        Error Handling:
            - FileNotFoundError -> ForecastExpiredError
            - Checksum/format errors -> ForecastCorruptError
        """
        ...


# ---------------------------------------------------------------------------
# Concrete Implementation: ZarrForecastReader
# ---------------------------------------------------------------------------


class ZarrForecastReader(ForecastReader):
    """Reads forecast data from Zarr stores on S3/MinIO.

    Uses xarray with s3fs for transparent S3 access. Configures s3fs
    with an explicit endpoint_url for MinIO in local development.

    Parameters
    ----------
    bucket : str
        S3 bucket name containing forecast Zarr stores.
    aws_region : str
        AWS region for S3 access.
    endpoint_url : str or None
        Custom S3 endpoint URL for MinIO in local dev. When set, s3fs
        is configured to route requests to this endpoint.
    aws_access_key_id : str or None
        AWS access key (for MinIO). If None, uses default credential chain.
    aws_secret_access_key : str or None
        AWS secret key (for MinIO). If None, uses default credential chain.
    validator : SchemaValidator or None
        Custom schema validator. Defaults to DefaultSchemaValidator.
    metric_emitter : MetricEmitter or None
        Callback for emitting CloudWatch metrics. Per FAIL-008, the reader
        MUST emit a ``CorruptForecast`` metric when corruption is detected.
        If None, metrics are logged but not emitted to CloudWatch.
    """

    def __init__(
        self,
        bucket: str,
        aws_region: str = "us-east-1",
        endpoint_url: str | None = None,
        aws_access_key_id: str | None = None,
        aws_secret_access_key: str | None = None,
        validator: SchemaValidator | None = None,
        metric_emitter: MetricEmitter | None = None,
    ) -> None:
        self._bucket = bucket
        self._aws_region = aws_region
        self._endpoint_url = endpoint_url
        self._aws_access_key_id = aws_access_key_id
        self._aws_secret_access_key = aws_secret_access_key
        self._validator = validator or DefaultSchemaValidator()
        self._metric_emitter = metric_emitter

    def _emit_corrupt_forecast_metric(self, tile_id: str) -> None:
        """Emit the CorruptForecast metric to CloudWatch.

        Per FAIL-008: The Reader MUST emit a CorruptForecast metric when
        data corruption is detected. This triggers the CorruptForecastAlarm
        which pages on-call engineers via PagerDuty.
        """
        if self._metric_emitter:
            try:
                self._metric_emitter(
                    "CorruptForecast",
                    1.0,
                    "Count",
                    {"TileId": tile_id},
                )
            except Exception as metric_exc:
                # Never let metric emission failure mask the actual error
                logger.warning(
                    "Failed to emit CorruptForecast metric: %s",
                    str(metric_exc),
                )
        else:
            logger.warning(
                "No metric emitter configured; CorruptForecast metric "
                "for tile=%s would have been emitted here.",
                tile_id,
            )

    def _get_s3_storage_options(self) -> dict[str, Any]:
        """Build s3fs storage options for xarray.open_zarr().

        Returns the ``storage_options`` dict that xarray passes through
        to s3fs.S3FileSystem for S3 access configuration.
        """
        opts: dict[str, Any] = {}

        if self._endpoint_url:
            opts["endpoint_url"] = self._endpoint_url
        if self._aws_access_key_id:
            opts["key"] = self._aws_access_key_id
        if self._aws_secret_access_key:
            opts["secret"] = self._aws_secret_access_key

        # Required for MinIO
        if self._endpoint_url:
            opts["anon"] = False

        return opts

    def load_tile(
        self,
        model: ForecastType,
        timestamp: datetime,
        tile_id: str,
    ) -> TileData:
        """Load a single tile's forecast data from the Zarr store.

        Opens the Zarr store from S3 and selects the geographic subset
        corresponding to the tile_id. Thanks to the Write-Side Halo
        strategy, this results in exactly one Zarr chunk fetch.

        Parameters
        ----------
        model : ForecastType
            Forecast model type (medium_range or nowcast).
        timestamp : datetime
            Forecast run timestamp.
        tile_id : str
            Tile identifier in "lat_index.lon_index" format.

        Returns
        -------
        TileData
            Wrapper around the tile's xarray.Dataset subset.

        Raises
        ------
        ForecastExpiredError
            If the Zarr store does not exist (data expired/deleted).
        ForecastCorruptError
            If the Zarr data is corrupt (checksum or format error).
        ValueError
            If the tile_id is invalid.
        """
        lat_idx, lon_idx = parse_tile_id(tile_id)
        zarr_path = _build_zarr_path(self._bucket, model, timestamp)

        logger.info(
            "Loading tile %s from %s (lat_idx=%d, lon_idx=%d)",
            tile_id,
            zarr_path,
            lat_idx,
            lon_idx,
        )

        # Open the Zarr store
        ds = self._open_zarr(zarr_path, tile_id, timestamp)

        # Select the tile's geographic region
        tile_ds = self._select_tile_region(ds, lat_idx, lon_idx, tile_id)

        # Validate the extracted data
        self._validate_tile(tile_ds, tile_id, timestamp)

        return TileData(tile_ds)

    def _open_zarr(
        self, zarr_path: str, tile_id: str, timestamp: datetime
    ) -> xr.Dataset:
        """Open the Zarr store, handling file-not-found and corruption.

        Uses xarray.open_zarr with chunks=None to force eager loading
        of the tile data into memory (since we need exactly one chunk).
        """
        storage_options = self._get_s3_storage_options()

        try:
            ds = xr.open_zarr(
                zarr_path,
                storage_options=storage_options if storage_options else None,
                consolidated=False,
            )
            return ds
        except FileNotFoundError as exc:
            logger.warning(
                "Forecast data not found (expired): tile=%s, path=%s, "
                "timestamp=%s. Acknowledging message to prevent DLQ loop.",
                tile_id,
                zarr_path,
                timestamp.isoformat(),
            )
            raise ForecastExpiredError(
                f"Forecast data not found at {zarr_path} for tile {tile_id}. "
                f"Data may have expired or been deleted."
            ) from exc
        except (KeyError, ValueError, OSError) as exc:
            # Zarr checksum failures, truncated headers, and format errors
            # manifest as various exceptions during deserialization.
            # KeyError: missing array/group in store
            # ValueError: bad metadata, incompatible data
            # OSError: I/O errors, checksum failures
            error_type = type(exc).__name__
            logger.critical(
                "Zarr corruption detected: tile=%s, path=%s, "
                "timestamp=%s, error_type=%s, details=%s",
                tile_id,
                zarr_path,
                timestamp.isoformat(),
                error_type,
                str(exc),
            )
            self._emit_corrupt_forecast_metric(tile_id)
            raise ForecastCorruptError(
                f"Forecast data corrupt at {zarr_path} for tile {tile_id}: "
                f"{error_type}: {exc}"
            ) from exc

    def _select_tile_region(
        self,
        ds: xr.Dataset,
        lat_idx: int,
        lon_idx: int,
        tile_id: str,
    ) -> xr.Dataset:
        """Select the geographic region for a tile from the full dataset.

        The Zarr store contains halo-padded data. We use xarray.sel with
        method='nearest' to select the tile's core region plus halo data.
        This relies on the Write-Side Halo: each chunk already contains
        1px of neighbor data on each side.

        Per EVAL-001 step 9: Reads **single** Zarr chunk for tile_id
        (relies on Write-Side Halo).
        """
        lat_max, lat_min = tile_lat_bounds(lat_idx)
        lon_min, lon_max = tile_lon_bounds(lon_idx)

        # Include halo: extend bounds by one grid cell in each direction
        halo_lat = GRID_RESOLUTION * HALO_SIZE
        halo_lon = GRID_RESOLUTION * HALO_SIZE

        sel_lat_max = lat_max + halo_lat
        sel_lat_min = lat_min - halo_lat
        sel_lon_min_ext = lon_min - halo_lon
        sel_lon_max_ext = lon_max + halo_lon

        # Clamp to global bounds
        sel_lat_max = min(sel_lat_max, 90.0)
        sel_lat_min = max(sel_lat_min, -90.0)

        try:
            # For tiles that don't wrap the date line, use simple sel.
            # Latitude is descending in the Zarr store, so we need to
            # handle the slice direction correctly.
            lat_vals = ds["lat"].values

            if len(lat_vals) > 1 and lat_vals[0] > lat_vals[-1]:
                # Descending latitude (standard: 90 to -90)
                lat_slice = slice(sel_lat_max, sel_lat_min)
            else:
                # Ascending latitude
                lat_slice = slice(sel_lat_min, sel_lat_max)

            # Handle longitude wrapping for tiles that cross -180/180
            lon_vals = ds["lon"].values
            lon_ascending = len(lon_vals) > 1 and lon_vals[0] < lon_vals[-1]

            if lon_min <= lon_max:
                # Normal case: no date line crossing
                if lon_ascending:
                    lon_slice = slice(sel_lon_min_ext, sel_lon_max_ext)
                else:
                    lon_slice = slice(sel_lon_max_ext, sel_lon_min_ext)
                tile_ds = ds.sel(lat=lat_slice, lon=lon_slice)
            else:
                # Date line crossing: lon_min > lon_max
                # e.g., lon_min=135, lon_max=-135
                # Need to select [lon_min, 180] and [-180, lon_max]
                ds_west = ds.sel(
                    lat=lat_slice,
                    lon=slice(sel_lon_min_ext, None) if lon_ascending else slice(None, sel_lon_min_ext),
                )
                ds_east = ds.sel(
                    lat=lat_slice,
                    lon=slice(None, sel_lon_max_ext) if lon_ascending else slice(sel_lon_max_ext, None),
                )
                tile_ds = xr.concat([ds_west, ds_east], dim="lon")

            # Load into memory to detach from the Zarr store
            tile_ds = tile_ds.load()

            return tile_ds

        except Exception as exc:
            logger.error(
                "Failed to select tile region: tile=%s, lat=[%f, %f], "
                "lon=[%f, %f], error=%s",
                tile_id,
                sel_lat_min,
                sel_lat_max,
                sel_lon_min_ext,
                sel_lon_max_ext,
                str(exc),
            )
            raise

    def _validate_tile(
        self, tile_ds: xr.Dataset, tile_id: str, timestamp: datetime
    ) -> None:
        """Run schema validation on the extracted tile data.

        Per spec: ForecastReader MUST run SchemaValidator before returning.
        If validation fails, raise ForecastCorruptError.
        """
        result = self._validator.validate(tile_ds)

        if not result.valid:
            error_details = "; ".join(result.errors)
            logger.critical(
                "Schema validation failed for tile: tile=%s, "
                "timestamp=%s, errors=%s",
                tile_id,
                timestamp.isoformat(),
                error_details,
            )
            self._emit_corrupt_forecast_metric(tile_id)
            raise ForecastCorruptError(
                f"Schema validation failed for tile {tile_id}: {error_details}"
            )


# ---------------------------------------------------------------------------
# Factory function
# ---------------------------------------------------------------------------


def create_forecast_reader(
    bucket: str,
    aws_region: str = "us-east-1",
    endpoint_url: str | None = None,
    aws_access_key_id: str | None = None,
    aws_secret_access_key: str | None = None,
    metric_emitter: MetricEmitter | None = None,
) -> ForecastReader:
    """Create a ForecastReader instance with the given configuration.

    For local development with MinIO, pass the endpoint_url and credentials.
    For production (AWS), leave endpoint_url as None to use the default
    AWS credential chain.

    Parameters
    ----------
    bucket : str
        S3 bucket name.
    aws_region : str
        AWS region.
    endpoint_url : str or None
        MinIO endpoint URL for local dev.
    aws_access_key_id : str or None
        AWS access key for MinIO.
    aws_secret_access_key : str or None
        AWS secret key for MinIO.
    metric_emitter : MetricEmitter or None
        Callback for emitting CloudWatch metrics (e.g., CorruptForecast).

    Returns
    -------
    ForecastReader
        Configured reader instance.
    """
    return ZarrForecastReader(
        bucket=bucket,
        aws_region=aws_region,
        endpoint_url=endpoint_url,
        aws_access_key_id=aws_access_key_id,
        aws_secret_access_key=aws_secret_access_key,
        metric_emitter=metric_emitter,
    )
