"""
Unit tests for MockEngine.

Validates that the mock inference engine generates datasets with correct
dimensions and structure for both Atlas (medium_range) and StormScope (nowcast).
"""

import numpy as np
import pytest

from mock_engine import (
    ATLAS_LAT_SIZE,
    ATLAS_LON_SIZE,
    CANONICAL_VARIABLES,
    MockEngine,
    STORMSCOPE_LAT_SIZE,
    STORMSCOPE_LON_SIZE,
    VARIABLE_BOUNDS,
    is_mock_mode,
)


class TestMockEngineAtlas:
    """Tests for Atlas (medium_range) model mock data generation."""

    def setup_method(self):
        self.engine = MockEngine(model="medium_range", num_time_steps=60, seed=42)

    def test_atlas_dimensions(self):
        """Verify Atlas dataset has correct lat=721, lon=1440, time=60."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        assert ds.sizes["lat"] == ATLAS_LAT_SIZE  # 721
        assert ds.sizes["lon"] == ATLAS_LON_SIZE  # 1440
        assert ds.sizes["time"] == 60

    def test_atlas_lat_coordinates(self):
        """Verify latitude is descending from 90.0 to -90.0."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")
        lat = ds["lat"].values

        assert lat[0] == pytest.approx(90.0, abs=0.01)
        assert lat[-1] == pytest.approx(-90.0, abs=0.01)
        # Confirm descending order
        assert np.all(np.diff(lat) < 0)

    def test_atlas_lon_coordinates(self):
        """Verify longitude is ascending from -180.0 to 179.75."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")
        lon = ds["lon"].values

        assert lon[0] == pytest.approx(-180.0, abs=0.01)
        assert lon[-1] == pytest.approx(179.75, abs=0.01)
        # Confirm ascending order
        assert np.all(np.diff(lon) > 0)

    def test_atlas_contains_all_canonical_variables(self):
        """All 5 canonical variables must be present."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            assert var_name in ds.data_vars, f"Missing variable: {var_name}"

    def test_atlas_variable_dtypes(self):
        """All variables must be float32."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            assert ds[var_name].dtype == np.float32

    def test_atlas_variable_bounds(self):
        """All variables must be within their physical bounds."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            lo, hi = VARIABLE_BOUNDS[var_name]
            data = ds[var_name].values
            assert np.all(data >= lo), f"{var_name} has values below {lo}"
            assert np.all(data <= hi), f"{var_name} has values above {hi}"

    def test_atlas_time_coordinates_are_int64(self):
        """Time coordinates must be int64 Unix timestamps."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")
        assert ds["time"].dtype == np.int64

    def test_atlas_no_nan_or_inf(self):
        """No NaN or Inf values in any variable."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            data = ds[var_name].values
            assert np.all(np.isfinite(data)), f"{var_name} contains non-finite values"


class TestMockEngineStormScope:
    """Tests for StormScope (nowcast) model mock data generation."""

    def setup_method(self):
        self.engine = MockEngine(model="nowcast", num_time_steps=24, seed=42)

    def test_stormscope_dimensions(self):
        """Verify StormScope dataset has correct lat=361, lon=240, time=24."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        assert ds.sizes["lat"] == STORMSCOPE_LAT_SIZE  # 361
        assert ds.sizes["lon"] == STORMSCOPE_LON_SIZE  # 240
        assert ds.sizes["time"] == 24

    def test_stormscope_lat_range(self):
        """Verify latitude covers CONUS range (65N to 20N, descending)."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")
        lat = ds["lat"].values

        assert lat[0] == pytest.approx(65.0, abs=0.01)
        assert lat[-1] == pytest.approx(20.0, abs=0.01)
        assert np.all(np.diff(lat) < 0)

    def test_stormscope_lon_range(self):
        """Verify longitude covers CONUS range (-130W to -70.25W, ascending)."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")
        lon = ds["lon"].values

        assert lon[0] == pytest.approx(-130.0, abs=0.01)
        assert lon[-1] == pytest.approx(-70.25, abs=0.01)
        assert np.all(np.diff(lon) > 0)

    def test_stormscope_contains_all_canonical_variables(self):
        """All 5 canonical variables must be present."""
        ds = self.engine.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            assert var_name in ds.data_vars


class TestMockEngineReproducibility:
    """Tests for deterministic behavior with seed."""

    def test_same_seed_produces_same_output(self):
        """Two engines with the same seed must produce identical results."""
        engine1 = MockEngine(model="medium_range", num_time_steps=5, seed=123)
        engine2 = MockEngine(model="medium_range", num_time_steps=5, seed=123)

        ds1 = engine1.predict(run_timestamp="2026-01-31T06:00:00Z")
        ds2 = engine2.predict(run_timestamp="2026-01-31T06:00:00Z")

        for var_name in CANONICAL_VARIABLES:
            np.testing.assert_array_equal(
                ds1[var_name].values,
                ds2[var_name].values,
                err_msg=f"Seed mismatch for {var_name}",
            )

    def test_different_seeds_produce_different_output(self):
        """Two engines with different seeds must produce different results."""
        engine1 = MockEngine(model="medium_range", num_time_steps=5, seed=1)
        engine2 = MockEngine(model="medium_range", num_time_steps=5, seed=2)

        ds1 = engine1.predict(run_timestamp="2026-01-31T06:00:00Z")
        ds2 = engine2.predict(run_timestamp="2026-01-31T06:00:00Z")

        # At least one variable should differ
        any_different = False
        for var_name in CANONICAL_VARIABLES:
            if not np.array_equal(ds1[var_name].values, ds2[var_name].values):
                any_different = True
                break
        assert any_different, "Different seeds produced identical output"


class TestMockEngineErrors:
    """Tests for error handling."""

    def test_unknown_model_raises_value_error(self):
        """Unknown model type must raise ValueError."""
        engine = MockEngine(model="unknown_model")
        with pytest.raises(ValueError, match="Unknown model type"):
            engine.predict(run_timestamp="2026-01-31T06:00:00Z")

    def test_load_weights_is_noop(self):
        """load_weights should not raise."""
        engine = MockEngine()
        engine.load_weights("/nonexistent/path")  # Should not raise


class TestIsMockMode:
    """Tests for the is_mock_mode helper function."""

    def test_mock_mode_from_options(self):
        """mock_inference=True in options enables mock mode."""
        assert is_mock_mode({"mock_inference": True}) is True

    def test_mock_mode_from_options_false(self):
        """mock_inference=False in options disables mock mode."""
        assert is_mock_mode({"mock_inference": False}) is False

    def test_mock_mode_from_env(self, monkeypatch):
        """MOCK_INFERENCE=true env var enables mock mode."""
        monkeypatch.setenv("MOCK_INFERENCE", "true")
        assert is_mock_mode(None) is True

    def test_mock_mode_env_case_insensitive(self, monkeypatch):
        """MOCK_INFERENCE env var is case insensitive."""
        monkeypatch.setenv("MOCK_INFERENCE", "TRUE")
        assert is_mock_mode(None) is True

    def test_mock_mode_env_one(self, monkeypatch):
        """MOCK_INFERENCE=1 enables mock mode."""
        monkeypatch.setenv("MOCK_INFERENCE", "1")
        assert is_mock_mode(None) is True

    def test_mock_mode_default_off(self, monkeypatch):
        """Without options or env var, mock mode is off."""
        monkeypatch.delenv("MOCK_INFERENCE", raising=False)
        assert is_mock_mode(None) is False

    def test_mock_mode_empty_options(self, monkeypatch):
        """Empty options dict does not enable mock mode."""
        monkeypatch.delenv("MOCK_INFERENCE", raising=False)
        assert is_mock_mode({}) is False
