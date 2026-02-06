"""
Integration tests for the RunPod Local Simulator (FastAPI).

Tests the POST /run endpoint, request/response formats, and
the RunPod response envelope structure.

Uses FastAPI's TestClient (backed by httpx) for in-process testing
without starting a real server.
"""

from __future__ import annotations

import os

import pytest
from fastapi.testclient import TestClient

from sim import app


@pytest.fixture
def client():
    """Create a FastAPI TestClient."""
    return TestClient(app)


@pytest.fixture
def output_dir(tmp_path):
    """Create a temporary output directory for Zarr stores."""
    return str(tmp_path / "sim_output")


def _make_request_body(
    output_destination: str,
    model: str = "medium_range",
    mock_inference: bool = True,
    task_type: str = "inference",
) -> dict:
    """Build a valid RunPod request body."""
    return {
        "input": {
            "task_type": task_type,
            "model": model,
            "run_timestamp": "2026-01-31T06:00:00Z",
            "output_destination": output_destination,
            "options": {
                "force_rebuild": False,
                "mock_inference": mock_inference,
            },
        }
    }


# ---------------------------------------------------------------------------
# Health check
# ---------------------------------------------------------------------------

class TestHealthEndpoint:
    """Tests for GET /health."""

    def test_health_returns_ok(self, client):
        """Health endpoint should return 200 with status ok."""
        resp = client.get("/health")
        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "ok"
        assert data["service"] == "runpod-simulator"


# ---------------------------------------------------------------------------
# POST /run
# ---------------------------------------------------------------------------

class TestRunEndpoint:
    """Tests for the POST /run inference endpoint."""

    def test_successful_inference(self, client, output_dir):
        """
        A valid request should return 200 with COMPLETED status
        and create a Zarr store at the output destination.
        """
        body = _make_request_body(output_dir, mock_inference=True)
        resp = client.post("/run", json=body)

        assert resp.status_code == 200
        data = resp.json()

        # RunPod envelope
        assert "id" in data
        assert data["status"] == "COMPLETED"

        # Output payload
        output = data["output"]
        assert output["status"] == "success"
        assert output["job_id"].startswith("sim_")
        assert output["inference_duration_ms"] >= 0
        assert output["chunks_written"] > 0
        assert output["output_path"] == output_dir
        assert output["error"] is None

        # Verify Zarr store was actually written
        assert os.path.exists(output_dir)
        assert os.path.exists(os.path.join(output_dir, "_SUCCESS"))

    def test_nowcast_model(self, client, output_dir):
        """Nowcast model should also work."""
        body = _make_request_body(output_dir, model="nowcast", mock_inference=True)
        resp = client.post("/run", json=body)

        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "COMPLETED"
        assert data["output"]["status"] == "success"

    def test_response_envelope_format(self, client, output_dir):
        """
        Verify the RunPod response envelope has the correct structure:
        {"id": "...", "status": "COMPLETED", "output": {...}}
        """
        body = _make_request_body(output_dir, mock_inference=True)
        resp = client.post("/run", json=body)
        data = resp.json()

        # Top-level keys
        assert set(data.keys()) == {"id", "status", "output"}

        # Output keys
        expected_output_keys = {
            "status", "job_id", "inference_duration_ms",
            "chunks_written", "output_path", "error",
        }
        assert set(data["output"].keys()) == expected_output_keys

    def test_unsupported_task_type_returns_failed(self, client, output_dir):
        """
        Non-inference task types should return FAILED status.
        """
        body = _make_request_body(
            output_dir, task_type="verification", mock_inference=True,
        )
        resp = client.post("/run", json=body)

        # The handler returns an error result, simulator wraps as FAILED
        assert resp.status_code == 500
        data = resp.json()
        assert data["status"] == "FAILED"
        assert data["output"]["status"] == "error"
        assert "Unsupported task_type" in data["output"]["error"]

    def test_missing_model_returns_422(self, client, output_dir):
        """
        Missing required field 'model' should return 422
        (Pydantic validation error from FastAPI).
        """
        body = {
            "input": {
                "run_timestamp": "2026-01-31T06:00:00Z",
                "output_destination": output_dir,
            }
        }
        resp = client.post("/run", json=body)
        assert resp.status_code == 422

    def test_missing_input_returns_422(self, client):
        """Missing 'input' key should return 422."""
        resp = client.post("/run", json={})
        assert resp.status_code == 422

    def test_empty_body_returns_422(self, client):
        """Empty request body should return 422."""
        resp = client.post("/run", json=None)
        # FastAPI returns 422 for invalid JSON
        assert resp.status_code == 422


# ---------------------------------------------------------------------------
# POST /runsync
# ---------------------------------------------------------------------------

class TestRunSyncEndpoint:
    """Tests for the POST /runsync endpoint."""

    def test_runsync_works_like_run(self, client, output_dir):
        """
        /runsync should behave identically to /run in the local simulator.
        """
        body = _make_request_body(output_dir, mock_inference=True)
        resp = client.post("/runsync", json=body)

        assert resp.status_code == 200
        data = resp.json()
        assert data["status"] == "COMPLETED"
        assert data["output"]["status"] == "success"


# ---------------------------------------------------------------------------
# Idempotency (Clean Slate Policy)
# ---------------------------------------------------------------------------

class TestIdempotency:
    """Tests for the idempotency behavior via the API."""

    def test_second_run_is_idempotent(self, client, output_dir):
        """
        Second request to the same destination should return cached
        result (chunks_written=0) when force_rebuild is false.
        """
        body = _make_request_body(output_dir, mock_inference=True)

        # First run
        resp1 = client.post("/run", json=body)
        assert resp1.status_code == 200
        data1 = resp1.json()
        assert data1["output"]["chunks_written"] > 0

        # Second run (same destination, no force_rebuild)
        resp2 = client.post("/run", json=body)
        assert resp2.status_code == 200
        data2 = resp2.json()
        assert data2["output"]["chunks_written"] == 0  # Cached

    def test_force_rebuild_rewrites(self, client, output_dir):
        """
        With force_rebuild=true, data should be regenerated even if
        _SUCCESS exists.
        """
        body_initial = _make_request_body(output_dir, mock_inference=True)
        resp1 = client.post("/run", json=body_initial)
        assert resp1.status_code == 200

        # Force rebuild
        body_rebuild = _make_request_body(output_dir, mock_inference=True)
        body_rebuild["input"]["options"]["force_rebuild"] = True
        resp2 = client.post("/run", json=body_rebuild)
        assert resp2.status_code == 200
        data2 = resp2.json()
        assert data2["output"]["chunks_written"] > 0  # Re-written
