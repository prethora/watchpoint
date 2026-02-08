"""
Unit tests for Regridder.

Tests regridding accuracy with analytical functions on small grids
to verify bilinear interpolation correctness.
"""

import numpy as np
import pytest

from regridder import (
    TARGET_LAT_END,
    TARGET_LAT_SIZE,
    TARGET_LAT_START,
    TARGET_LON_END,
    TARGET_LON_SIZE,
    TARGET_LON_START,
    Regridder,
)


class TestRegridderInit:
    """Tests for Regridder construction."""

    def test_target_grid_shape(self):
        """Target grid should have the specified dimensions."""
        r = Regridder()
        assert len(r.target_lat) == TARGET_LAT_SIZE
        assert len(r.target_lon) == TARGET_LON_SIZE

    def test_target_lat_range(self):
        """Target latitude should span 65.0 to 20.0 (descending)."""
        r = Regridder()
        assert r.target_lat[0] == pytest.approx(TARGET_LAT_START, abs=0.01)
        assert r.target_lat[-1] == pytest.approx(TARGET_LAT_END, abs=0.01)

    def test_target_lon_range(self):
        """Target longitude should span -130.0 to -70.25."""
        r = Regridder()
        assert r.target_lon[0] == pytest.approx(TARGET_LON_START, abs=0.01)
        assert r.target_lon[-1] == pytest.approx(TARGET_LON_END, abs=0.01)


class TestRegridHRRR:
    """Tests for HRRR Lambert Conformal -> regular lat/lon regridding."""

    def test_constant_field(self):
        """A constant field should remain constant after regridding."""
        r = Regridder()

        # Source grid: coarse regular grid covering the target domain
        src_lat_1d = np.linspace(66, 19, 50)
        src_lon_1d = np.linspace(-131, -69, 60)
        src_lon_2d, src_lat_2d = np.meshgrid(src_lon_1d, src_lat_1d)

        # Constant field
        data_2d = np.full(src_lat_2d.shape, 42.0)
        data = [data_2d]  # 1 time step

        result = r.regrid_hrrr(data, src_lat_2d, src_lon_2d)

        assert result.shape == (1, TARGET_LAT_SIZE, TARGET_LON_SIZE)
        np.testing.assert_allclose(result, 42.0, atol=0.01)

    def test_output_shape_multiple_timesteps(self):
        """Multiple time steps should produce correct output shape."""
        r = Regridder()

        src_lat_1d = np.linspace(66, 19, 50)
        src_lon_1d = np.linspace(-131, -69, 60)
        src_lon_2d, src_lat_2d = np.meshgrid(src_lon_1d, src_lat_1d)

        data = [np.ones(src_lat_2d.shape) * i for i in range(4)]
        result = r.regrid_hrrr(data, src_lat_2d, src_lon_2d)

        assert result.shape == (4, TARGET_LAT_SIZE, TARGET_LON_SIZE)

    def test_linear_gradient_lat(self):
        """A linear latitude gradient should be approximately preserved."""
        r = Regridder()

        src_lat_1d = np.linspace(66, 19, 100)
        src_lon_1d = np.linspace(-131, -69, 120)
        src_lon_2d, src_lat_2d = np.meshgrid(src_lon_1d, src_lat_1d)

        # Field = latitude value
        data = [src_lat_2d.copy()]
        result = r.regrid_hrrr(data, src_lat_2d, src_lon_2d)

        # Check that the result follows the target latitudes
        for i in range(0, TARGET_LAT_SIZE, 50):
            expected_lat = r.target_lat[i]
            # Allow some interpolation error
            row_mean = np.mean(result[0, i, :])
            assert abs(row_mean - expected_lat) < 1.0, (
                f"Row {i}: expected ~{expected_lat}, got {row_mean}"
            )

    def test_ndarray_input(self):
        """Should accept 3D ndarray in addition to list."""
        r = Regridder()

        src_lat_1d = np.linspace(66, 19, 50)
        src_lon_1d = np.linspace(-131, -69, 60)
        src_lon_2d, src_lat_2d = np.meshgrid(src_lon_1d, src_lat_1d)

        data = np.ones((2, 50, 60)) * 7.0
        result = r.regrid_hrrr(data, src_lat_2d, src_lon_2d)

        assert result.shape == (2, TARGET_LAT_SIZE, TARGET_LON_SIZE)
        np.testing.assert_allclose(result, 7.0, atol=0.01)


class TestRegridGFS:
    """Tests for GFS 0.25-degree global -> CONUS subset regridding."""

    def test_constant_field(self):
        """A constant GFS field should remain constant after subsetting."""
        r = Regridder()

        # Standard GFS grid
        gfs_lat = np.linspace(90.0, -90.0, 721)
        gfs_lon = np.linspace(-180.0, 179.75, 1440)

        data = {"t2m": np.full((721, 1440), 300.0)}
        result = r.regrid_gfs(data, gfs_lat=gfs_lat, gfs_lon=gfs_lon)

        assert result["t2m"].shape == (TARGET_LAT_SIZE, TARGET_LON_SIZE)
        np.testing.assert_allclose(result["t2m"], 300.0, atol=0.01)

    def test_multiple_variables(self):
        """Should handle multiple variables simultaneously."""
        r = Regridder()

        gfs_lat = np.linspace(90.0, -90.0, 721)
        gfs_lon = np.linspace(-180.0, 179.75, 1440)

        data = {
            "t2m": np.full((721, 1440), 280.0),
            "u10m": np.full((721, 1440), 5.0),
        }
        result = r.regrid_gfs(data, gfs_lat=gfs_lat, gfs_lon=gfs_lon)

        assert "t2m" in result
        assert "u10m" in result
        np.testing.assert_allclose(result["t2m"], 280.0, atol=0.01)
        np.testing.assert_allclose(result["u10m"], 5.0, atol=0.01)

    def test_3d_input_takes_first_timestep(self):
        """3D arrays should have the first time step extracted."""
        r = Regridder()

        gfs_lat = np.linspace(90.0, -90.0, 721)
        gfs_lon = np.linspace(-180.0, 179.75, 1440)

        data_3d = np.zeros((3, 721, 1440))
        data_3d[0] = 42.0
        data_3d[1] = 99.0
        data_3d[2] = 1.0

        data = {"t2m": data_3d}
        result = r.regrid_gfs(data, gfs_lat=gfs_lat, gfs_lon=gfs_lon)

        # Should use first timestep (42.0)
        np.testing.assert_allclose(result["t2m"], 42.0, atol=0.01)

    def test_0_360_longitude_conversion(self):
        """GFS with 0-360 longitude should be correctly handled."""
        r = Regridder()

        gfs_lat = np.linspace(90.0, -90.0, 721)
        # 0-360 convention (as returned by default when gfs_lon=None)
        gfs_lon = np.linspace(0.0, 359.75, 1440)

        data = {"t2m": np.full((721, 1440), 288.0)}
        result = r.regrid_gfs(data, gfs_lat=gfs_lat, gfs_lon=None)

        assert result["t2m"].shape == (TARGET_LAT_SIZE, TARGET_LON_SIZE)
        np.testing.assert_allclose(result["t2m"], 288.0, atol=0.5)

    def test_output_no_nan(self):
        """Output should not contain NaN after nearest-neighbor fill."""
        r = Regridder()

        gfs_lat = np.linspace(90.0, -90.0, 721)
        gfs_lon = np.linspace(-180.0, 179.75, 1440)

        data = {"t2m": np.full((721, 1440), 300.0)}
        result = r.regrid_gfs(data, gfs_lat=gfs_lat, gfs_lon=gfs_lon)

        assert not np.any(np.isnan(result["t2m"]))
