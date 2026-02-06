"""
Unit tests for ZarrWriter.

Validates that the ZarrWriter produces correct Zarr v2 stores with:
  - Write-Side Halo padding (1-pixel overlap at chunk boundaries)
  - Zstd(level=3) compression
  - Correct dimensions readable via xarray.open_zarr
  - Proper _SUCCESS marker
  - Metadata attributes
"""

import math
import os

import numpy as np
import pytest
import xarray as xr
import zarr
from numcodecs import Zstd

from mock_engine import CANONICAL_VARIABLES, MockEngine
from zarr_writer import (
    HALO_LAT_CHUNK,
    HALO_LON_CHUNK,
    HALO_SIZE,
    SCHEMA_VERSION,
    SUCCESS_MARKER,
    TILE_LAT_CHUNK,
    TILE_LON_CHUNK,
    ZSTD_COMPRESSION_LEVEL,
    ZarrWriter,
)


def _make_test_dataset(
    lat_size: int = 721,
    lon_size: int = 1440,
    time_steps: int = 4,
    seed: int = 42,
) -> xr.Dataset:
    """Create a minimal test dataset with correct structure."""
    rng = np.random.default_rng(seed)
    lat = np.linspace(90.0, -90.0, lat_size, dtype=np.float32)
    lon = np.linspace(-180.0, 179.75, lon_size, dtype=np.float32)
    time = np.arange(0, time_steps * 3600, 3600, dtype=np.int64)

    data_vars = {}
    for var_name in CANONICAL_VARIABLES:
        arr = rng.uniform(0, 50, size=(time_steps, lat_size, lon_size)).astype(
            np.float32
        )
        data_vars[var_name] = (["time", "lat", "lon"], arr)

    return xr.Dataset(
        data_vars=data_vars,
        coords={
            "time": ("time", time),
            "lat": ("lat", lat),
            "lon": ("lon", lon),
        },
    )


class TestZarrWriterBasic:
    """Basic write and read tests."""

    def test_write_and_read_atlas(self, tmp_path):
        """
        Write a mock Atlas dataset and confirm xarray.open_zarr can read it.

        This is the primary definition-of-done test.
        """
        engine = MockEngine(model="medium_range", num_time_steps=4, seed=42)
        ds = engine.predict(input_xr=xr.Dataset())

        writer = ZarrWriter(model_name="atlas", model_version="v1.2")
        dest = str(tmp_path / "atlas_test.zarr")
        writer.write(ds, dest)

        # Read back with xarray
        read_ds = xr.open_zarr(dest)

        # Confirm all canonical variables are present
        for var_name in CANONICAL_VARIABLES:
            assert var_name in read_ds.data_vars, f"Missing variable: {var_name}"

        # Confirm dimensions exist
        assert "time" in read_ds.dims
        assert "lat" in read_ds.dims
        assert "lon" in read_ds.dims

        # Time dimension should match original
        assert read_ds.sizes["time"] == 4

        read_ds.close()

    def test_write_and_read_stormscope(self, tmp_path):
        """Write a mock StormScope dataset and confirm readable."""
        engine = MockEngine(model="nowcast", num_time_steps=4, seed=42)
        ds = engine.predict(input_xr=xr.Dataset())

        writer = ZarrWriter(model_name="stormscope", model_version="v1.0")
        dest = str(tmp_path / "stormscope_test.zarr")
        writer.write(ds, dest)

        read_ds = xr.open_zarr(dest)

        for var_name in CANONICAL_VARIABLES:
            assert var_name in read_ds.data_vars

        assert read_ds.sizes["time"] == 4
        read_ds.close()


class TestZarrWriterHaloPadding:
    """Tests that verify the Write-Side Halo logic."""

    def test_atlas_padded_dimensions_are_larger(self, tmp_path):
        """
        After halo padding, the stored lat/lon dimensions should be
        larger than the original 721/1440 due to duplicated boundary pixels
        and edge clamping.

        Atlas: 9 lat chunks * 92 = 828 padded lat,
               8 lon chunks * 182 = 1456 padded lon
        """
        ds = _make_test_dataset(lat_size=721, lon_size=1440, time_steps=2)
        writer = ZarrWriter()
        dest = str(tmp_path / "halo_test.zarr")
        writer.write(ds, dest)

        read_ds = xr.open_zarr(dest)

        # Padded lat = 9 chunks * 92 = 828
        assert read_ds.sizes["lat"] == 9 * (TILE_LAT_CHUNK + 2)  # 828
        # Padded lon = 8 chunks * 182 = 1456
        assert read_ds.sizes["lon"] == 8 * (TILE_LON_CHUNK + 2)  # 1456

        read_ds.close()

    def test_halo_padding_small_grid(self, tmp_path):
        """
        Test halo padding with a small grid to verify exact behavior.

        A 10-element dimension with chunk_size=5 should produce uniform
        chunks of size 7 (5 core + 1 left halo + 1 right halo):
          Chunk 0: left_clamp[0] + [0,1,2,3,4] + right_halo[5]  = 7 elements
          Chunk 1: left_halo[4] + [5,6,7,8,9] + right_clamp[9]  = 7 elements
        Total padded: 14 elements = 2 chunks * 7
        """
        from zarr_writer import ZarrWriter as ZW

        indices = ZW._compute_padded_indices(dim_size=10, chunk_size=5)

        # Chunk 0: edge-clamped left + 5 core + right halo
        # Chunk 1: left halo + 5 core + edge-clamped right
        expected = [0, 0, 1, 2, 3, 4, 5, 4, 5, 6, 7, 8, 9, 9]
        np.testing.assert_array_equal(indices, expected)
        # Verify uniform chunk size = 7
        assert len(indices) == 2 * 7

    def test_halo_padding_with_remainder(self, tmp_path):
        """
        Test halo padding when dimension doesn't divide evenly by chunk_size.

        A 12-element dimension with chunk_size=5 should produce:
          Chunk 0: [0] + [0,1,2,3,4] + [5]           = 7 elements
          Chunk 1: [4] + [5,6,7,8,9] + [10]          = 7 elements
          Chunk 2: [9] + [10,11,11,11,11] + [11]     = 7 elements (padded)
        Total padded: 21 elements = 3 chunks * 7
        """
        from zarr_writer import ZarrWriter as ZW

        indices = ZW._compute_padded_indices(dim_size=12, chunk_size=5)

        expected = [
            0, 0, 1, 2, 3, 4, 5,           # chunk 0: clamped left + core + right halo
            4, 5, 6, 7, 8, 9, 10,          # chunk 1: left halo + core + right halo
            9, 10, 11, 11, 11, 11, 11,     # chunk 2: left halo + 2 core + 3 pad + clamped right
        ]
        np.testing.assert_array_equal(indices, expected)
        # Verify uniform chunk size = 7
        assert len(indices) == 3 * 7

    def test_halo_all_chunks_uniform_size(self, tmp_path):
        """
        Every padded chunk MUST be exactly chunk_size + 2 elements.
        This is the critical invariant for Zarr chunk alignment.
        """
        from zarr_writer import ZarrWriter as ZW

        # Test several dimension sizes
        for dim_size in [10, 12, 90, 180, 361, 721, 1440]:
            for chunk_size in [5, 90, 180]:
                if chunk_size >= dim_size:
                    continue  # Skip trivial cases
                indices = ZW._compute_padded_indices(dim_size, chunk_size)
                padded_chunk = chunk_size + 2
                assert len(indices) % padded_chunk == 0, (
                    f"Padded array length {len(indices)} is not a multiple of "
                    f"padded chunk size {padded_chunk} for dim_size={dim_size}, "
                    f"chunk_size={chunk_size}"
                )

    def test_halo_boundary_data_duplicated(self, tmp_path):
        """
        Verify that boundary pixels are actually duplicated in the written data.
        """
        from zarr_writer import ZarrWriter as ZW

        # For a 10-element dim with 5-element chunks, padded chunk = 7:
        indices = ZW._compute_padded_indices(10, 5)
        chunk_0 = indices[:7]   # First padded chunk
        chunk_1 = indices[7:]   # Second padded chunk

        # Boundary: chunk 0 must have index 5 (right halo from chunk 1)
        assert 5 in chunk_0, "Right halo pixel missing from chunk 0"
        # Boundary: chunk 1 must have index 4 (left halo from chunk 0)
        assert 4 in chunk_1, "Left halo pixel missing from chunk 1"

    def test_atlas_chunk_count(self, tmp_path):
        """
        Verify the expected padded dimensions for Atlas.

        Original: 721 lat, 1440 lon
        Lat: ceil(721/90) = 9 chunks, each padded to 92 -> total = 9 * 92 = 828
        Lon: ceil(1440/180) = 8 chunks, each padded to 182 -> total = 8 * 182 = 1456
        """
        from zarr_writer import ZarrWriter as ZW

        lat_indices = ZW._compute_padded_indices(721, TILE_LAT_CHUNK)
        lon_indices = ZW._compute_padded_indices(1440, TILE_LON_CHUNK)

        # 9 lat chunks * 92 elements each = 828
        assert len(lat_indices) == 9 * (TILE_LAT_CHUNK + 2)  # 9 * 92 = 828

        # 8 lon chunks * 182 elements each = 1456
        assert len(lon_indices) == 8 * (TILE_LON_CHUNK + 2)  # 8 * 182 = 1456


class TestZarrWriterCompression:
    """Tests for compression configuration."""

    def test_zstd_compression(self, tmp_path):
        """Verify Zstd compression at level 3 is used."""
        ds = _make_test_dataset(lat_size=90, lon_size=180, time_steps=2)
        writer = ZarrWriter()
        dest = str(tmp_path / "compression_test.zarr")
        writer.write(ds, dest)

        # Open with zarr to inspect compression settings
        store = zarr.open(dest, mode="r")
        for var_name in CANONICAL_VARIABLES:
            arr = store[var_name]
            compressor = arr.compressor
            assert isinstance(compressor, Zstd), (
                f"Expected Zstd compressor for {var_name}, got {type(compressor)}"
            )
            assert compressor.level == ZSTD_COMPRESSION_LEVEL


class TestZarrWriterMetadata:
    """Tests for metadata attributes."""

    def test_metadata_written(self, tmp_path):
        """Verify .zattrs metadata is present and correct."""
        ds = _make_test_dataset(lat_size=90, lon_size=180, time_steps=2)
        writer = ZarrWriter(model_name="atlas", model_version="v1.2")
        dest = str(tmp_path / "metadata_test.zarr")
        writer.write(ds, dest)

        store = zarr.open(dest, mode="r")
        assert store.attrs["model_name"] == "atlas"
        assert store.attrs["model_version"] == "v1.2"
        assert store.attrs["schema_version"] == SCHEMA_VERSION
        assert "generated_at" in store.attrs
        assert store.attrs["halo_size"] == HALO_SIZE
        assert store.attrs["tile_lat_chunk"] == TILE_LAT_CHUNK
        assert store.attrs["tile_lon_chunk"] == TILE_LON_CHUNK


class TestZarrWriterSuccessMarker:
    """Tests for the _SUCCESS commit marker."""

    def test_success_marker_exists(self, tmp_path):
        """The _SUCCESS file must exist after a successful write."""
        ds = _make_test_dataset(lat_size=90, lon_size=180, time_steps=2)
        writer = ZarrWriter()
        dest = str(tmp_path / "marker_test.zarr")
        writer.write(ds, dest)

        success_path = os.path.join(dest, SUCCESS_MARKER)
        assert os.path.exists(success_path), "_SUCCESS marker not found"
        assert os.path.getsize(success_path) == 0, "_SUCCESS marker should be 0 bytes"


class TestZarrWriterValidation:
    """Tests for input validation."""

    def test_missing_dimension_raises(self, tmp_path):
        """Writing a dataset missing required dimensions should raise ValueError."""
        # Create a dataset without 'time' dimension
        ds = xr.Dataset(
            data_vars={
                "precipitation_mm": (["lat", "lon"], np.zeros((10, 10), dtype=np.float32)),
                "precipitation_probability": (["lat", "lon"], np.zeros((10, 10), dtype=np.float32)),
                "temperature_c": (["lat", "lon"], np.zeros((10, 10), dtype=np.float32)),
                "wind_speed_kmh": (["lat", "lon"], np.zeros((10, 10), dtype=np.float32)),
                "humidity_percent": (["lat", "lon"], np.zeros((10, 10), dtype=np.float32)),
            },
            coords={
                "lat": np.linspace(90, -90, 10, dtype=np.float32),
                "lon": np.linspace(-180, 180, 10, dtype=np.float32),
            },
        )
        writer = ZarrWriter()
        with pytest.raises(ValueError, match="missing required dimensions"):
            writer.write(ds, str(tmp_path / "bad.zarr"))

    def test_missing_variable_raises(self, tmp_path):
        """Writing a dataset missing canonical variables should raise ValueError."""
        ds = xr.Dataset(
            data_vars={
                "temperature_c": (["time", "lat", "lon"],
                                  np.zeros((2, 10, 10), dtype=np.float32)),
            },
            coords={
                "time": np.arange(0, 7200, 3600, dtype=np.int64),
                "lat": np.linspace(90, -90, 10, dtype=np.float32),
                "lon": np.linspace(-180, 180, 10, dtype=np.float32),
            },
        )
        writer = ZarrWriter()
        with pytest.raises(ValueError, match="missing canonical variables"):
            writer.write(ds, str(tmp_path / "bad.zarr"))

    def test_nan_values_raise(self, tmp_path):
        """Writing data with NaN values should raise ValueError."""
        ds = _make_test_dataset(lat_size=10, lon_size=10, time_steps=2)
        # Inject a NaN
        ds["temperature_c"].values[0, 0, 0] = np.nan

        writer = ZarrWriter()
        with pytest.raises(ValueError, match="non-finite values"):
            writer.write(ds, str(tmp_path / "nan.zarr"))

    def test_inf_values_raise(self, tmp_path):
        """Writing data with Inf values should raise ValueError."""
        ds = _make_test_dataset(lat_size=10, lon_size=10, time_steps=2)
        ds["wind_speed_kmh"].values[0, 0, 0] = np.inf

        writer = ZarrWriter()
        with pytest.raises(ValueError, match="non-finite values"):
            writer.write(ds, str(tmp_path / "inf.zarr"))


class TestZarrWriterCleanSlate:
    """Tests for the clean_slate method."""

    def test_clean_slate_removes_local_directory(self, tmp_path):
        """clean_slate should remove an existing local Zarr store."""
        dest = str(tmp_path / "to_clean.zarr")
        os.makedirs(dest)
        # Write a dummy file
        with open(os.path.join(dest, "dummy"), "w") as f:
            f.write("test")

        writer = ZarrWriter()
        writer.clean_slate(dest)

        assert not os.path.exists(dest)

    def test_clean_slate_nonexistent_path_no_error(self, tmp_path):
        """clean_slate should not raise if path doesn't exist."""
        dest = str(tmp_path / "nonexistent.zarr")
        writer = ZarrWriter()
        writer.clean_slate(dest)  # Should not raise


class TestZarrWriterEndToEnd:
    """End-to-end tests combining MockEngine and ZarrWriter."""

    def test_full_pipeline_atlas(self, tmp_path):
        """
        Full pipeline: MockEngine -> ZarrWriter -> xarray.open_zarr.

        This is the primary integration test matching the definition of done:
        "Unit test writes a mock Zarr to a temporary directory.
         xarray.open_zarr confirms successful read and correct dimensions."
        """
        # Generate mock data
        engine = MockEngine(model="medium_range", num_time_steps=4, seed=42)
        ds = engine.predict(input_xr=xr.Dataset())

        # Write to Zarr
        writer = ZarrWriter(model_name="atlas", model_version="v1.2")
        dest = str(tmp_path / "e2e_atlas.zarr")
        writer.write(ds, dest)

        # Read back and verify
        read_ds = xr.open_zarr(dest)

        # All 5 canonical variables present
        assert len([v for v in CANONICAL_VARIABLES if v in read_ds.data_vars]) == 5

        # Dimensions exist with correct time
        assert read_ds.sizes["time"] == 4

        # Padded spatial dimensions: 9 lat chunks * 92, 8 lon chunks * 182
        assert read_ds.sizes["lat"] == 9 * (TILE_LAT_CHUNK + 2)  # 828
        assert read_ds.sizes["lon"] == 8 * (TILE_LON_CHUNK + 2)  # 1456

        # _SUCCESS marker exists
        assert os.path.exists(os.path.join(dest, "_SUCCESS"))

        # Data is readable (no corruption)
        sample = read_ds["temperature_c"].values
        assert np.all(np.isfinite(sample))

        read_ds.close()

    def test_full_pipeline_stormscope(self, tmp_path):
        """Full pipeline for StormScope (nowcast) model."""
        engine = MockEngine(model="nowcast", num_time_steps=4, seed=42)
        ds = engine.predict(input_xr=xr.Dataset())

        writer = ZarrWriter(model_name="stormscope", model_version="v1.0")
        dest = str(tmp_path / "e2e_stormscope.zarr")
        writer.write(ds, dest)

        read_ds = xr.open_zarr(dest)

        assert len([v for v in CANONICAL_VARIABLES if v in read_ds.data_vars]) == 5
        assert read_ds.sizes["time"] == 4

        # StormScope padded dimensions should be larger than original
        # lat: ceil(361/90)=5 chunks * 92 = 460
        # lon: ceil(240/180)=2 chunks * 182 = 364
        expected_lat_chunks = math.ceil(361 / TILE_LAT_CHUNK)
        expected_lon_chunks = math.ceil(240 / TILE_LON_CHUNK)
        assert read_ds.sizes["lat"] == expected_lat_chunks * (TILE_LAT_CHUNK + 2)
        assert read_ds.sizes["lon"] == expected_lon_chunks * (TILE_LON_CHUNK + 2)

        assert os.path.exists(os.path.join(dest, "_SUCCESS"))

        read_ds.close()
