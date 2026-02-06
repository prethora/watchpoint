"""
Tests for the Forecast Reader (Zarr I/O).

Tests cover:
  - Tile ID parsing and coordinate geometry
  - Zarr path construction
  - Successful tile loading (using local filesystem Zarr stores)
  - ForecastExpiredError on missing data
  - ForecastCorruptError on corrupt/invalid data
  - Schema validation
  - Bilinear interpolation via TileData.extract_point
  - Time series extraction via TileData.extract_timeseries
  - Date line crossing tiles
  - Factory function

All tests use local filesystem Zarr stores (no MinIO/S3 required).

Architecture References:
  - 07-eval-worker.md Section 4.1
  - Flow EVAL-001 step 9
  - Flow FAIL-008
"""

from __future__ import annotations

import os
import shutil
import tempfile
from datetime import datetime, timezone
from pathlib import Path
from unittest.mock import MagicMock, patch

import numpy as np
import pytest
import xarray as xr
import zarr

from worker.eval.models import ForecastType
from worker.eval.reader import (
    CANONICAL_VARIABLES,
    GRID_RESOLUTION,
    HALO_SIZE,
    NUM_LAT_TILES,
    NUM_LON_TILES,
    TILE_LAT_CHUNK,
    TILE_LAT_DEG,
    TILE_LON_CHUNK,
    TILE_LON_DEG,
    DefaultSchemaValidator,
    ForecastCorruptError,
    ForecastExpiredError,
    MetricEmitter,
    TileData,
    ValidationResult,
    ZarrForecastReader,
    _build_zarr_path,
    create_forecast_reader,
    parse_tile_id,
    tile_lat_bounds,
    tile_lon_bounds,
)


# ---------------------------------------------------------------------------
# Helpers: Create synthetic forecast Zarr stores
# ---------------------------------------------------------------------------


def _create_synthetic_dataset(
    lat_start: float = 90.0,
    lat_end: float = -90.0,
    lon_start: float = -180.0,
    lon_end: float = 180.0,
    lat_step: float = -GRID_RESOLUTION,
    lon_step: float = GRID_RESOLUTION,
    n_times: int = 6,
    seed: int = 42,
) -> xr.Dataset:
    """Create a synthetic global forecast dataset with canonical variables.

    The dataset mimics the structure produced by the ZarrWriter:
    - Dimensions: (time, lat, lon)
    - Coordinates: lat descending, lon ascending
    - Variables: all CANONICAL_VARIABLES with realistic-ish values
    """
    rng = np.random.RandomState(seed)

    # Build coordinate arrays
    lats = np.arange(lat_start, lat_end + lat_step / 2, lat_step)
    lons = np.arange(lon_start, lon_end - lon_step / 2, lon_step)
    times = np.arange(n_times)

    # Create data arrays with physically plausible values
    shape = (n_times, len(lats), len(lons))
    data_vars = {
        "precipitation_mm": (
            ["time", "lat", "lon"],
            rng.uniform(0, 50, shape).astype(np.float32),
        ),
        "precipitation_probability": (
            ["time", "lat", "lon"],
            rng.uniform(0, 100, shape).astype(np.float32),
        ),
        "temperature_c": (
            ["time", "lat", "lon"],
            rng.uniform(-30, 45, shape).astype(np.float32),
        ),
        "wind_speed_kmh": (
            ["time", "lat", "lon"],
            rng.uniform(0, 150, shape).astype(np.float32),
        ),
        "humidity_percent": (
            ["time", "lat", "lon"],
            rng.uniform(0, 100, shape).astype(np.float32),
        ),
    }

    ds = xr.Dataset(
        data_vars,
        coords={
            "time": ("time", times),
            "lat": ("lat", lats),
            "lon": ("lon", lons),
        },
    )

    return ds


def _write_zarr_store(ds: xr.Dataset, path: str) -> None:
    """Write an xarray Dataset as a Zarr store to a local directory."""
    ds.to_zarr(path, mode="w")


def _create_zarr_store_at_path(
    base_dir: str,
    forecast_type: str = "medium_range",
    timestamp_str: str = "2026-01-31T06:00:00Z",
    **kwargs,
) -> str:
    """Create a Zarr store in the expected directory structure.

    Returns the base_dir (used as the 'bucket' root for local testing).
    """
    zarr_path = os.path.join(base_dir, forecast_type, timestamp_str)
    os.makedirs(os.path.dirname(zarr_path), exist_ok=True)

    ds = _create_synthetic_dataset(**kwargs)
    _write_zarr_store(ds, zarr_path)

    return base_dir


# ---------------------------------------------------------------------------
# Test: parse_tile_id
# ---------------------------------------------------------------------------


class TestParseTileId:
    """Tests for tile_id string parsing."""

    def test_valid_tile_ids(self):
        """Valid tile IDs should parse correctly."""
        assert parse_tile_id("0.0") == (0, 0)
        assert parse_tile_id("3.4") == (3, 4)
        assert parse_tile_id("7.7") == (7, 7)
        assert parse_tile_id("0.7") == (0, 7)
        assert parse_tile_id("7.0") == (7, 0)

    def test_invalid_format_no_dot(self):
        """Tile IDs without a dot separator should raise ValueError."""
        with pytest.raises(ValueError, match="Invalid tile_id format"):
            parse_tile_id("34")

    def test_invalid_format_multiple_dots(self):
        """Tile IDs with multiple dots should raise ValueError."""
        with pytest.raises(ValueError, match="Invalid tile_id format"):
            parse_tile_id("3.4.5")

    def test_invalid_non_integer(self):
        """Non-integer indices should raise ValueError."""
        with pytest.raises(ValueError, match="Indices must be integers"):
            parse_tile_id("a.b")

    def test_out_of_range_lat(self):
        """Lat index >= NUM_LAT_TILES should raise ValueError."""
        with pytest.raises(ValueError, match="lat_index.*out of range"):
            parse_tile_id("8.0")

    def test_out_of_range_lon(self):
        """Lon index >= NUM_LON_TILES should raise ValueError."""
        with pytest.raises(ValueError, match="lon_index.*out of range"):
            parse_tile_id("0.8")

    def test_negative_index(self):
        """Negative indices should raise ValueError."""
        with pytest.raises(ValueError, match="lat_index.*out of range"):
            parse_tile_id("-1.0")


# ---------------------------------------------------------------------------
# Test: tile_lat_bounds
# ---------------------------------------------------------------------------


class TestTileLatBounds:
    """Tests for latitude bound calculation."""

    def test_tile_0_north_pole(self):
        """Tile 0 should cover the northernmost band [90, 67.5]."""
        lat_max, lat_min = tile_lat_bounds(0)
        assert lat_max == 90.0
        assert lat_min == 67.5

    def test_tile_7_south_pole(self):
        """Tile 7 should cover the southernmost band [-67.5, -90]."""
        lat_max, lat_min = tile_lat_bounds(7)
        assert lat_max == -67.5
        assert lat_min == -90.0

    def test_equator_tile(self):
        """Tile 3 should include the northern edge of the equatorial band."""
        lat_max, lat_min = tile_lat_bounds(3)
        assert lat_max == 22.5
        assert lat_min == 0.0

    def test_all_tiles_cover_full_range(self):
        """All 8 tiles should collectively cover [-90, 90]."""
        total_min = min(tile_lat_bounds(i)[1] for i in range(NUM_LAT_TILES))
        total_max = max(tile_lat_bounds(i)[0] for i in range(NUM_LAT_TILES))
        assert total_min == -90.0
        assert total_max == 90.0


# ---------------------------------------------------------------------------
# Test: tile_lon_bounds
# ---------------------------------------------------------------------------


class TestTileLonBounds:
    """Tests for longitude bound calculation."""

    def test_tile_0_prime_meridian(self):
        """Tile 0 should cover [0, 45) longitude."""
        lon_min, lon_max = tile_lon_bounds(0)
        assert lon_min == 0.0
        assert lon_max == 45.0

    def test_tile_4_date_line_west(self):
        """Tile 4 should cover [-180, -135) longitude."""
        lon_min, lon_max = tile_lon_bounds(4)
        assert lon_min == -180.0
        assert lon_max == -135.0

    def test_tile_7_negative_to_zero(self):
        """Tile 7 should cover [-45, 0) longitude."""
        lon_min, lon_max = tile_lon_bounds(7)
        assert lon_min == -45.0
        assert lon_max == 0.0

    def test_all_tiles_cover_full_range(self):
        """All 8 tiles should collectively span 360 degrees."""
        total_span = 0.0
        for i in range(NUM_LON_TILES):
            lon_min, lon_max = tile_lon_bounds(i)
            if lon_min <= lon_max:
                total_span += lon_max - lon_min
            else:
                # Date line wrap
                total_span += (180.0 - lon_min) + (lon_max + 180.0)
        assert abs(total_span - 360.0) < 0.01


# ---------------------------------------------------------------------------
# Test: _build_zarr_path
# ---------------------------------------------------------------------------


class TestBuildZarrPath:
    """Tests for S3 path construction."""

    def test_medium_range_path(self):
        """Medium range forecast should produce correct S3 path."""
        ts = datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc)
        path = _build_zarr_path("watchpoint-forecasts", ForecastType.MEDIUM_RANGE, ts)
        assert path == "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z"

    def test_nowcast_path(self):
        """Nowcast forecast should produce correct S3 path."""
        ts = datetime(2026, 2, 1, 12, 30, 0, tzinfo=timezone.utc)
        path = _build_zarr_path("my-bucket", ForecastType.NOWCAST, ts)
        assert path == "s3://my-bucket/nowcast/2026-02-01T12:30:00Z"


# ---------------------------------------------------------------------------
# Test: DefaultSchemaValidator
# ---------------------------------------------------------------------------


class TestDefaultSchemaValidator:
    """Tests for the dataset schema validator."""

    def test_valid_dataset(self):
        """A well-formed dataset should pass validation."""
        ds = _create_synthetic_dataset()
        validator = DefaultSchemaValidator()
        result = validator.validate(ds)
        assert result.valid
        assert result.errors == []

    def test_missing_dimension(self):
        """Dataset missing 'time' dimension should fail validation."""
        # Create a 2D dataset without time
        ds = xr.Dataset(
            {
                "temperature_c": (["lat", "lon"], np.zeros((10, 10))),
                "precipitation_mm": (["lat", "lon"], np.zeros((10, 10))),
                "precipitation_probability": (["lat", "lon"], np.zeros((10, 10))),
                "wind_speed_kmh": (["lat", "lon"], np.zeros((10, 10))),
                "humidity_percent": (["lat", "lon"], np.zeros((10, 10))),
            },
            coords={"lat": np.arange(10), "lon": np.arange(10)},
        )
        validator = DefaultSchemaValidator()
        result = validator.validate(ds)
        assert not result.valid
        assert any("time" in e for e in result.errors)

    def test_missing_variable(self):
        """Dataset missing a canonical variable should fail validation."""
        ds = _create_synthetic_dataset()
        # Drop one variable
        ds = ds.drop_vars("humidity_percent")
        validator = DefaultSchemaValidator()
        result = validator.validate(ds)
        assert not result.valid
        assert any("humidity_percent" in e for e in result.errors)

    def test_nan_values(self):
        """Dataset with NaN values should fail validation."""
        ds = _create_synthetic_dataset()
        # Inject NaN
        temp_data = ds["temperature_c"].values.copy()
        temp_data[0, 0, 0] = np.nan
        ds["temperature_c"] = (["time", "lat", "lon"], temp_data)

        validator = DefaultSchemaValidator()
        result = validator.validate(ds)
        assert not result.valid
        assert any("non-finite" in e for e in result.errors)

    def test_inf_values(self):
        """Dataset with Inf values should fail validation."""
        ds = _create_synthetic_dataset()
        wind_data = ds["wind_speed_kmh"].values.copy()
        wind_data[0, 5, 5] = np.inf
        ds["wind_speed_kmh"] = (["time", "lat", "lon"], wind_data)

        validator = DefaultSchemaValidator()
        result = validator.validate(ds)
        assert not result.valid
        assert any("non-finite" in e for e in result.errors)


# ---------------------------------------------------------------------------
# Test: TileData
# ---------------------------------------------------------------------------


class TestTileData:
    """Tests for the TileData wrapper around xarray.Dataset."""

    @pytest.fixture
    def sample_tile_ds(self) -> xr.Dataset:
        """Create a small tile-sized dataset for testing."""
        lats = np.arange(45.0, 22.0, -GRID_RESOLUTION)  # ~92 points
        lons = np.arange(0.0, 46.0, GRID_RESOLUTION)  # ~184 points
        times = np.arange(6)

        shape = (len(times), len(lats), len(lons))
        rng = np.random.RandomState(42)

        data_vars = {}
        for var in CANONICAL_VARIABLES:
            data_vars[var] = (
                ["time", "lat", "lon"],
                rng.uniform(0, 100, shape).astype(np.float32),
            )

        return xr.Dataset(
            data_vars,
            coords={
                "time": ("time", times),
                "lat": ("lat", lats),
                "lon": ("lon", lons),
            },
        )

    def test_extract_point_returns_all_variables(self, sample_tile_ds):
        """extract_point should return all canonical variables."""
        tile = TileData(sample_tile_ds)
        result = tile.extract_point(lat=35.0, lon=20.0)

        for var in CANONICAL_VARIABLES:
            assert var in result
            assert isinstance(result[var], float)
            assert np.isfinite(result[var])

    def test_extract_point_interpolation(self, sample_tile_ds):
        """extract_point should interpolate between grid points."""
        tile = TileData(sample_tile_ds)
        # Pick a point between two grid cells
        result = tile.extract_point(lat=34.125, lon=20.125)

        # The result should be a weighted average of surrounding grid cells
        for var in CANONICAL_VARIABLES:
            assert var in result
            assert isinstance(result[var], float)

    def test_extract_point_at_grid_point(self, sample_tile_ds):
        """extract_point at an exact grid point should return that value."""
        tile = TileData(sample_tile_ds)
        # Use an exact grid point
        lat = sample_tile_ds["lat"].values[10]
        lon = sample_tile_ds["lon"].values[20]
        result = tile.extract_point(lat=float(lat), lon=float(lon))

        # Value should match the dataset exactly (first timestep)
        expected_temp = float(
            sample_tile_ds["temperature_c"].values[0, 10, 20]
        )
        assert abs(result["temperature_c"] - expected_temp) < 0.01

    def test_extract_timeseries(self, sample_tile_ds):
        """extract_timeseries should return arrays with the time dimension."""
        tile = TileData(sample_tile_ds)
        result = tile.extract_timeseries(lat=35.0, lon=20.0)

        for var in CANONICAL_VARIABLES:
            assert var in result
            assert isinstance(result[var], np.ndarray)
            assert len(result[var]) == 6  # n_times

    def test_times_property(self, sample_tile_ds):
        """times property should return the time coordinate."""
        tile = TileData(sample_tile_ds)
        assert len(tile.times) == 6


# ---------------------------------------------------------------------------
# Test: ZarrForecastReader (with local filesystem)
# ---------------------------------------------------------------------------


class TestZarrForecastReader:
    """Tests for the ZarrForecastReader using local Zarr stores.

    These tests create Zarr stores on the local filesystem to test the
    reader logic without requiring MinIO or S3.
    """

    @pytest.fixture
    def tmp_dir(self, tmp_path):
        """Create a temporary directory for test Zarr stores."""
        return str(tmp_path)

    @pytest.fixture
    def zarr_store_path(self, tmp_dir):
        """Create a Zarr store in the expected directory structure.

        Returns a tuple of (base_path, forecast_type, timestamp).
        """
        forecast_type = "medium_range"
        timestamp_str = "2026-01-31T06:00:00Z"
        zarr_path = os.path.join(tmp_dir, forecast_type, timestamp_str)

        ds = _create_synthetic_dataset()
        _write_zarr_store(ds, zarr_path)

        return tmp_dir, forecast_type, timestamp_str

    def test_load_tile_success_with_local_zarr(self, zarr_store_path):
        """Loading a valid tile should return TileData with correct structure."""
        base_path, ft, ts = zarr_store_path
        zarr_uri = os.path.join(base_path, ft, ts)

        # Read directly from local Zarr
        reader = ZarrForecastReader(bucket="unused")

        # Patch _open_zarr to open from local filesystem directly
        ds = xr.open_zarr(zarr_uri)
        with patch.object(reader, "_open_zarr", return_value=ds):
            tile_data = reader.load_tile(
                model=ForecastType.MEDIUM_RANGE,
                timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                tile_id="2.0",  # lat [45, 22.5], lon [0, 45]
            )

        assert isinstance(tile_data, TileData)
        assert tile_data.ds is not None
        # Verify data was extracted for the tile region
        assert len(tile_data.ds["lat"]) > 0
        assert len(tile_data.ds["lon"]) > 0

    def test_load_tile_expired_raises_forecast_expired(self, tmp_dir):
        """Missing Zarr store should raise ForecastExpiredError."""
        reader = ZarrForecastReader(bucket="nonexistent-bucket")

        # Patch xr.open_zarr to raise FileNotFoundError
        with patch("xarray.open_zarr", side_effect=FileNotFoundError("Not found")):
            with pytest.raises(ForecastExpiredError, match="not found"):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

    def test_load_tile_corrupt_raises_forecast_corrupt(self, tmp_dir):
        """Corrupt Zarr data should raise ForecastCorruptError."""
        reader = ZarrForecastReader(bucket="test-bucket")

        # Simulate a checksum/format error via OSError
        with patch("xarray.open_zarr", side_effect=OSError("Checksum mismatch")):
            with pytest.raises(ForecastCorruptError, match="corrupt"):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

    def test_load_tile_key_error_raises_corrupt(self, tmp_dir):
        """KeyError during Zarr read should raise ForecastCorruptError."""
        reader = ZarrForecastReader(bucket="test-bucket")

        with patch("xarray.open_zarr", side_effect=KeyError("missing_array")):
            with pytest.raises(ForecastCorruptError):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

    def test_load_tile_value_error_raises_corrupt(self, tmp_dir):
        """ValueError during Zarr read should raise ForecastCorruptError."""
        reader = ZarrForecastReader(bucket="test-bucket")

        with patch("xarray.open_zarr", side_effect=ValueError("bad metadata")):
            with pytest.raises(ForecastCorruptError):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

    def test_load_tile_schema_validation_failure(self, zarr_store_path):
        """Tile with missing variables should raise ForecastCorruptError."""
        base_path, ft, ts = zarr_store_path
        zarr_uri = os.path.join(base_path, ft, ts)

        reader = ZarrForecastReader(bucket="unused")

        # Create a dataset missing some variables
        bad_ds = xr.open_zarr(zarr_uri)
        bad_ds = bad_ds.drop_vars("humidity_percent")

        with patch.object(reader, "_open_zarr", return_value=bad_ds):
            with patch.object(reader, "_select_tile_region", return_value=bad_ds):
                with pytest.raises(ForecastCorruptError, match="humidity_percent"):
                    reader.load_tile(
                        model=ForecastType.MEDIUM_RANGE,
                        timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                        tile_id="2.0",
                    )

    def test_load_tile_invalid_tile_id(self, tmp_dir):
        """Invalid tile_id should raise ValueError."""
        reader = ZarrForecastReader(bucket="test-bucket")

        with pytest.raises(ValueError, match="Invalid tile_id"):
            reader.load_tile(
                model=ForecastType.MEDIUM_RANGE,
                timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                tile_id="invalid",
            )

    def test_corrupt_data_emits_metric(self, tmp_dir):
        """CorruptForecast metric should be emitted when data is corrupt (FAIL-008)."""
        emitter = MagicMock()
        reader = ZarrForecastReader(
            bucket="test-bucket",
            metric_emitter=emitter,
        )

        with patch("xarray.open_zarr", side_effect=OSError("Checksum mismatch")):
            with pytest.raises(ForecastCorruptError):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

        # Verify the metric was emitted
        emitter.assert_called_once_with(
            "CorruptForecast",
            1.0,
            "Count",
            {"TileId": "0.0"},
        )

    def test_schema_validation_failure_emits_metric(self, zarr_store_path):
        """CorruptForecast metric should be emitted on schema validation failure."""
        base_path, ft, ts = zarr_store_path
        zarr_uri = os.path.join(base_path, ft, ts)

        emitter = MagicMock()
        reader = ZarrForecastReader(
            bucket="unused",
            metric_emitter=emitter,
        )

        # Create a dataset missing some variables
        bad_ds = xr.open_zarr(zarr_uri)
        bad_ds = bad_ds.drop_vars("humidity_percent")

        with patch.object(reader, "_open_zarr", return_value=bad_ds):
            with patch.object(reader, "_select_tile_region", return_value=bad_ds):
                with pytest.raises(ForecastCorruptError):
                    reader.load_tile(
                        model=ForecastType.MEDIUM_RANGE,
                        timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                        tile_id="2.0",
                    )

        emitter.assert_called_once_with(
            "CorruptForecast",
            1.0,
            "Count",
            {"TileId": "2.0"},
        )

    def test_metric_emitter_failure_does_not_mask_error(self, tmp_dir):
        """If the metric emitter itself fails, the original error should still propagate."""
        emitter = MagicMock(side_effect=RuntimeError("Metrics service down"))
        reader = ZarrForecastReader(
            bucket="test-bucket",
            metric_emitter=emitter,
        )

        with patch("xarray.open_zarr", side_effect=OSError("Checksum mismatch")):
            with pytest.raises(ForecastCorruptError, match="corrupt"):
                reader.load_tile(
                    model=ForecastType.MEDIUM_RANGE,
                    timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                    tile_id="0.0",
                )

    def test_s3_storage_options_with_endpoint(self):
        """Storage options should include endpoint_url for MinIO."""
        reader = ZarrForecastReader(
            bucket="test",
            endpoint_url="http://localhost:9000",
            aws_access_key_id="minioadmin",
            aws_secret_access_key="minioadmin",
        )
        opts = reader._get_s3_storage_options()
        assert opts["endpoint_url"] == "http://localhost:9000"
        assert opts["key"] == "minioadmin"
        assert opts["secret"] == "minioadmin"
        assert opts["anon"] is False

    def test_s3_storage_options_production(self):
        """Storage options for production (no endpoint) should be minimal."""
        reader = ZarrForecastReader(bucket="prod-bucket")
        opts = reader._get_s3_storage_options()
        assert "endpoint_url" not in opts
        assert "key" not in opts
        assert "anon" not in opts


# ---------------------------------------------------------------------------
# Test: Full integration with local Zarr store
# ---------------------------------------------------------------------------


class TestForecastReaderIntegration:
    """Integration tests using actual local Zarr stores.

    These tests create a full synthetic dataset, write it to disk as a
    Zarr store, and then read it back through the ZarrForecastReader.
    """

    @pytest.fixture
    def local_zarr_dataset(self, tmp_path):
        """Create a synthetic dataset and write it as a Zarr store.

        Returns (zarr_path, original_dataset).
        """
        zarr_path = str(tmp_path / "forecast.zarr")
        ds = _create_synthetic_dataset(seed=123)
        _write_zarr_store(ds, zarr_path)
        return zarr_path, ds

    def test_roundtrip_read(self, local_zarr_dataset):
        """Data read from Zarr should match the original dataset."""
        zarr_path, original_ds = local_zarr_dataset
        read_ds = xr.open_zarr(zarr_path)

        # Verify all variables present
        for var in CANONICAL_VARIABLES:
            assert var in read_ds.data_vars
            np.testing.assert_array_almost_equal(
                read_ds[var].values, original_ds[var].values, decimal=5
            )

    def test_tile_region_selection(self, local_zarr_dataset):
        """Selecting a tile region should return data within the expected bounds."""
        zarr_path, original_ds = local_zarr_dataset

        reader = ZarrForecastReader(bucket="unused")

        # Test tile 2.0: lat [45, 22.5], lon [0, 45]
        lat_idx, lon_idx = 2, 0

        tile_ds = reader._select_tile_region(
            original_ds, lat_idx, lon_idx, "2.0"
        )

        # Verify lat bounds (with halo)
        lat_max_bound, lat_min_bound = tile_lat_bounds(lat_idx)
        assert float(tile_ds["lat"].max()) <= lat_max_bound + GRID_RESOLUTION * HALO_SIZE + 0.01
        assert float(tile_ds["lat"].min()) >= lat_min_bound - GRID_RESOLUTION * HALO_SIZE - 0.01

        # Verify lon bounds (with halo)
        lon_min_bound, lon_max_bound = tile_lon_bounds(lon_idx)
        assert float(tile_ds["lon"].max()) <= lon_max_bound + GRID_RESOLUTION * HALO_SIZE + 0.01
        assert float(tile_ds["lon"].min()) >= lon_min_bound - GRID_RESOLUTION * HALO_SIZE - 0.01

    def test_extract_point_from_loaded_tile(self, local_zarr_dataset):
        """Extracting a point from a loaded tile should return valid data."""
        zarr_path, original_ds = local_zarr_dataset

        reader = ZarrForecastReader(bucket="unused")

        tile_ds = reader._select_tile_region(original_ds, 2, 0, "2.0")
        tile_data = TileData(tile_ds)

        # Point in the middle of tile 2.0
        result = tile_data.extract_point(lat=35.0, lon=20.0)

        assert len(result) == len(CANONICAL_VARIABLES)
        for var in CANONICAL_VARIABLES:
            assert var in result
            assert np.isfinite(result[var])

    def test_full_load_tile_with_mocked_open(self, local_zarr_dataset):
        """Full load_tile flow with mocked _open_zarr returning local data."""
        zarr_path, original_ds = local_zarr_dataset

        reader = ZarrForecastReader(bucket="unused")

        with patch.object(reader, "_open_zarr", return_value=original_ds):
            tile_data = reader.load_tile(
                model=ForecastType.MEDIUM_RANGE,
                timestamp=datetime(2026, 1, 31, 6, 0, 0, tzinfo=timezone.utc),
                tile_id="3.1",  # lat [22.5, 0], lon [45, 90]
            )

        assert isinstance(tile_data, TileData)

        # Extract a point within the tile
        result = tile_data.extract_point(lat=10.0, lon=60.0)
        for var in CANONICAL_VARIABLES:
            assert var in result


# ---------------------------------------------------------------------------
# Test: create_forecast_reader factory
# ---------------------------------------------------------------------------


class TestCreateForecastReader:
    """Tests for the factory function."""

    def test_creates_zarr_reader(self):
        """Factory should return a ZarrForecastReader instance."""
        reader = create_forecast_reader(bucket="test-bucket")
        assert isinstance(reader, ZarrForecastReader)

    def test_passes_endpoint_url(self):
        """Factory should pass endpoint_url for MinIO."""
        reader = create_forecast_reader(
            bucket="test",
            endpoint_url="http://localhost:9000",
            aws_access_key_id="key",
            aws_secret_access_key="secret",
        )
        assert isinstance(reader, ZarrForecastReader)
        assert reader._endpoint_url == "http://localhost:9000"
        assert reader._aws_access_key_id == "key"
        assert reader._aws_secret_access_key == "secret"


# ---------------------------------------------------------------------------
# Test: ValidationResult
# ---------------------------------------------------------------------------


class TestValidationResult:
    """Tests for the ValidationResult data class."""

    def test_valid_result(self):
        """Valid result should have no errors."""
        result = ValidationResult(valid=True)
        assert result.valid
        assert result.errors == []

    def test_invalid_result_with_errors(self):
        """Invalid result should contain error messages."""
        result = ValidationResult(valid=False, errors=["missing time"])
        assert not result.valid
        assert "missing time" in result.errors

    def test_repr(self):
        """repr should be readable."""
        result = ValidationResult(valid=True, errors=[])
        assert "valid=True" in repr(result)
