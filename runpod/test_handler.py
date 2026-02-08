"""
Unit tests for InferenceHandler.

Validates the orchestration pipeline: payload parsing, engine selection,
mock inference, Zarr writing, Clean Slate Policy, and error handling.
"""

from __future__ import annotations

import math
import os

import numpy as np
import pytest
import xarray as xr

from handler import (
    InferenceHandler,
    InferencePayload,
    InferenceResult,
    InputConfig,
    InferenceOptions,
    parse_payload,
)
from mock_engine import CANONICAL_VARIABLES, MockEngine
from zarr_writer import SUCCESS_MARKER, TILE_LAT_CHUNK, TILE_LON_CHUNK, ZarrWriter


# ---------------------------------------------------------------------------
# Payload parsing tests
# ---------------------------------------------------------------------------

class TestParsePayload:
    """Tests for the parse_payload function."""

    def test_parse_full_payload(self):
        """Parse a complete payload with all fields."""
        raw = {
            "input": {
                "task_type": "inference",
                "model": "medium_range",
                "run_timestamp": "2026-01-31T06:00:00Z",
                "output_destination": "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z",
                "input_config": {
                    "gfs_source_paths": ["s3://noaa-gfs/f000", "s3://noaa-gfs/f006"],
                    "calibration": {"high_threshold": 45.0},
                },
                "options": {
                    "force_rebuild": True,
                    "mock_inference": True,
                },
            }
        }

        payload = parse_payload(raw)

        assert payload.task_type == "inference"
        assert payload.model == "medium_range"
        assert payload.run_timestamp == "2026-01-31T06:00:00Z"
        assert payload.output_destination == "s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z"
        assert payload.input_config.gfs_source_paths == ["s3://noaa-gfs/f000", "s3://noaa-gfs/f006"]
        assert payload.input_config.calibration == {"high_threshold": 45.0}
        assert payload.options.force_rebuild is True
        assert payload.options.mock_inference is True

    def test_parse_minimal_payload(self):
        """Parse a payload with only required fields."""
        raw = {
            "input": {
                "model": "nowcast",
                "run_timestamp": "2026-01-31T12:00:00Z",
                "output_destination": "/tmp/test_output",
            }
        }

        payload = parse_payload(raw)

        assert payload.task_type == "inference"  # default
        assert payload.model == "nowcast"
        assert payload.options.force_rebuild is False  # default
        assert payload.options.mock_inference is False  # default

    def test_parse_without_envelope(self):
        """Parse a payload that is not wrapped in an 'input' key."""
        raw = {
            "model": "medium_range",
            "run_timestamp": "2026-01-31T06:00:00Z",
            "output_destination": "/tmp/output",
        }

        payload = parse_payload(raw)

        assert payload.model == "medium_range"

    def test_parse_missing_model_raises(self):
        """Missing 'model' field should raise ValueError."""
        raw = {
            "input": {
                "run_timestamp": "2026-01-31T06:00:00Z",
                "output_destination": "/tmp/output",
            }
        }
        with pytest.raises(ValueError, match="Missing required field: 'model'"):
            parse_payload(raw)

    def test_parse_missing_run_timestamp_raises(self):
        """Missing 'run_timestamp' field should raise ValueError."""
        raw = {
            "input": {
                "model": "medium_range",
                "output_destination": "/tmp/output",
            }
        }
        with pytest.raises(ValueError, match="Missing required field: 'run_timestamp'"):
            parse_payload(raw)

    def test_parse_missing_output_destination_raises(self):
        """Missing 'output_destination' field should raise ValueError."""
        raw = {
            "input": {
                "model": "medium_range",
                "run_timestamp": "2026-01-31T06:00:00Z",
            }
        }
        with pytest.raises(ValueError, match="Missing required field: 'output_destination'"):
            parse_payload(raw)


# ---------------------------------------------------------------------------
# InferenceResult tests
# ---------------------------------------------------------------------------

class TestInferenceResult:
    """Tests for InferenceResult serialization."""

    def test_to_dict_success(self):
        """Successful result serializes correctly."""
        result = InferenceResult(
            status="success",
            job_id="sim_abc123",
            inference_duration_ms=1500,
            chunks_written=40,
            output_path="/tmp/output",
        )
        d = result.to_dict()
        assert d["status"] == "success"
        assert d["job_id"] == "sim_abc123"
        assert d["inference_duration_ms"] == 1500
        assert d["chunks_written"] == 40
        assert d["output_path"] == "/tmp/output"
        assert d["error"] is None

    def test_to_dict_error(self):
        """Error result serializes with error message."""
        result = InferenceResult(
            status="error",
            job_id="sim_xyz",
            inference_duration_ms=50,
            chunks_written=0,
            output_path="",
            error="Something went wrong",
        )
        d = result.to_dict()
        assert d["status"] == "error"
        assert d["error"] == "Something went wrong"


# ---------------------------------------------------------------------------
# InferenceHandler tests
# ---------------------------------------------------------------------------

def _make_mock_payload(
    tmp_path: str,
    model: str = "medium_range",
    mock_inference: bool = True,
    force_rebuild: bool = False,
) -> dict:
    """Create a valid mock inference payload."""
    return {
        "input": {
            "task_type": "inference",
            "model": model,
            "run_timestamp": "2026-01-31T06:00:00Z",
            "output_destination": tmp_path,
            "options": {
                "force_rebuild": force_rebuild,
                "mock_inference": mock_inference,
            },
        }
    }


class TestInferenceHandlerSuccess:
    """Tests for successful inference runs."""

    def test_mock_inference_atlas(self, tmp_path):
        """
        Full mock inference for Atlas (medium_range) model.
        Verifies Zarr output is created with _SUCCESS marker.
        """
        dest = str(tmp_path / "atlas_run")
        payload = _make_mock_payload(dest, model="medium_range", mock_inference=True)

        handler = InferenceHandler()
        result = handler.run(payload)

        assert result.status == "success"
        assert result.error is None
        assert result.inference_duration_ms >= 0
        assert result.chunks_written > 0
        assert result.output_path == dest

        # Verify Zarr store was written
        assert os.path.exists(dest)
        assert os.path.exists(os.path.join(dest, SUCCESS_MARKER))

        # Verify readable by xarray
        ds = xr.open_zarr(dest)
        for var_name in CANONICAL_VARIABLES:
            assert var_name in ds.data_vars
        ds.close()

    def test_mock_inference_nowcast(self, tmp_path):
        """Full mock inference for StormScope (nowcast) model."""
        dest = str(tmp_path / "nowcast_run")
        payload = _make_mock_payload(dest, model="nowcast", mock_inference=True)

        handler = InferenceHandler()
        result = handler.run(payload)

        assert result.status == "success"
        assert result.error is None
        assert os.path.exists(os.path.join(dest, SUCCESS_MARKER))

    def test_custom_engine_injection(self, tmp_path):
        """Handler should use injected engine instead of default."""
        dest = str(tmp_path / "custom_engine_run")
        payload = _make_mock_payload(dest, mock_inference=True)

        custom_engine = MockEngine(model="medium_range", num_time_steps=4, seed=99)
        handler = InferenceHandler(engine=custom_engine)
        result = handler.run(payload)

        assert result.status == "success"

        # Verify the output has 4 time steps (from our custom engine)
        ds = xr.open_zarr(dest)
        assert ds.sizes["time"] == 4
        ds.close()

    def test_custom_writer_injection(self, tmp_path):
        """Handler should use injected writer."""
        dest = str(tmp_path / "custom_writer_run")
        payload = _make_mock_payload(dest, mock_inference=True)

        custom_writer = ZarrWriter(model_name="test_model", model_version="v9.9")
        handler = InferenceHandler(writer=custom_writer)
        result = handler.run(payload)

        assert result.status == "success"

        # Verify metadata from custom writer
        import zarr
        store = zarr.open(dest, mode="r")
        assert store.attrs["model_name"] == "test_model"
        assert store.attrs["model_version"] == "v9.9"

    def test_result_has_job_id(self, tmp_path):
        """Every result should have a unique job ID starting with 'sim_'."""
        dest = str(tmp_path / "jobid_test")
        payload = _make_mock_payload(dest, mock_inference=True)

        handler = InferenceHandler()
        result = handler.run(payload)

        assert result.job_id.startswith("sim_")
        assert len(result.job_id) > 4  # sim_ + hex chars


class TestInferenceHandlerCleanSlate:
    """Tests for the Clean Slate Policy (Section 7.1)."""

    def test_idempotency_with_existing_success(self, tmp_path):
        """
        If _SUCCESS exists and force_rebuild=false, return immediately
        without re-running inference (idempotency).
        """
        dest = str(tmp_path / "idempotent_run")

        # First run: creates the Zarr store
        payload = _make_mock_payload(dest, mock_inference=True)
        handler = InferenceHandler()
        result1 = handler.run(payload)
        assert result1.status == "success"

        # Second run: should detect _SUCCESS and return cached
        result2 = handler.run(payload)
        assert result2.status == "success"
        assert result2.chunks_written == 0  # No chunks written (cached)

    def test_force_rebuild_overwrites(self, tmp_path):
        """
        With force_rebuild=true, existing data should be cleaned
        and re-created.
        """
        dest = str(tmp_path / "force_rebuild_run")

        # First run
        payload_initial = _make_mock_payload(dest, mock_inference=True)
        handler = InferenceHandler()
        result1 = handler.run(payload_initial)
        assert result1.status == "success"

        # Second run with force_rebuild
        payload_rebuild = _make_mock_payload(
            dest, mock_inference=True, force_rebuild=True,
        )
        result2 = handler.run(payload_rebuild)
        assert result2.status == "success"
        assert result2.chunks_written > 0  # Chunks were re-written


class TestInferenceHandlerErrors:
    """Tests for error handling."""

    def test_missing_model_returns_error(self):
        """Payload without 'model' should return error result (not raise)."""
        raw = {
            "input": {
                "run_timestamp": "2026-01-31T06:00:00Z",
                "output_destination": "/tmp/test",
            }
        }
        handler = InferenceHandler()
        result = handler.run(raw)

        assert result.status == "error"
        assert "Missing required field" in result.error

    def test_unsupported_task_type_returns_error(self, tmp_path):
        """Non-inference task_type should return error."""
        dest = str(tmp_path / "bad_task")
        raw = {
            "input": {
                "task_type": "verification",
                "model": "medium_range",
                "run_timestamp": "2026-01-31T06:00:00Z",
                "output_destination": dest,
                "options": {"mock_inference": True},
            }
        }
        handler = InferenceHandler()
        result = handler.run(raw)

        assert result.status == "error"
        assert "Unsupported task_type" in result.error

    def test_no_mock_no_real_engine_returns_error(self, tmp_path):
        """
        When mock mode is off and real engine dependencies (E2S) are not
        installed, the handler should return an error from the import failure.
        """
        dest = str(tmp_path / "no_engine")
        payload = _make_mock_payload(dest, mock_inference=False)

        # Clear MOCK_INFERENCE env var to prevent fallback
        old_val = os.environ.pop("MOCK_INFERENCE", None)
        try:
            handler = InferenceHandler()
            result = handler.run(payload)
            assert result.status == "error"
            # Will be either an ImportError (E2S not installed) or
            # RuntimeError (unknown model)
            assert result.error is not None
        finally:
            if old_val is not None:
                os.environ["MOCK_INFERENCE"] = old_val

    def test_error_result_has_job_id(self):
        """Even error results should have a valid job ID."""
        raw = {"input": {"run_timestamp": "x", "output_destination": "x"}}
        handler = InferenceHandler()
        result = handler.run(raw)

        assert result.status == "error"
        assert result.job_id.startswith("sim_")

    def test_error_result_has_duration(self):
        """Error results should report the time taken before failure."""
        raw = {"input": {"run_timestamp": "x", "output_destination": "x"}}
        handler = InferenceHandler()
        result = handler.run(raw)

        assert result.inference_duration_ms >= 0


class TestInferenceHandlerChunkCount:
    """Tests for chunk count calculation."""

    def test_atlas_chunk_count(self, tmp_path):
        """
        Verify chunk count matches expected Atlas grid.
        Atlas: lat=721, lon=1440 -> ceil(721/90)*ceil(1440/180)*5_vars = 9*8*5 = 360
        """
        dest = str(tmp_path / "chunk_count")
        payload = _make_mock_payload(dest, model="medium_range", mock_inference=True)

        handler = InferenceHandler()
        result = handler.run(payload)

        # MockEngine produces 721 lat, 1440 lon, 5 variables
        expected_lat_chunks = math.ceil(721 / TILE_LAT_CHUNK)  # 9
        expected_lon_chunks = math.ceil(1440 / TILE_LON_CHUNK)  # 8
        expected_vars = 5
        expected_total = expected_lat_chunks * expected_lon_chunks * expected_vars

        assert result.chunks_written == expected_total
