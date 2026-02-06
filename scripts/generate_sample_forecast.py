#!/usr/bin/env python3
"""
generate_sample_forecast.py
===========================

Generates Zarr-format forecast data for local development and integration testing.

Two modes of operation:

1. **Random Mode** (--random):
   Generates a full forecast with random-but-physically-bounded data for all
   canonical variables. Useful for visual inspection and basic smoke tests.

2. **Scenario Mode** (--scenario config.json):
   Generates a forecast with deterministic data at specific coordinates. The
   baseline array is initialized with zeros (or a configurable value), then
   exact values are applied at the specified lat/lon positions. This is
   required for integration tests like INT-001 that need to guarantee a
   threshold violation (e.g., precip_prob=80 at a specific location).

S3 Path Convention (from 11-runpod.md / 06-batcher.md):
    s3://{bucket}/{forecast_type}/{run_timestamp_iso}/
    e.g., s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z/

The Zarr store is written WITHOUT Write-Side Halo padding for integration
test compatibility. The eval worker's ZarrForecastReader._select_tile_region
requires unique coordinate values (which halo padding would violate). In
production, the RunPod ZarrWriter applies halo padding, but the eval worker
reader tests already demonstrate correct operation on non-padded data (see
worker/eval/test_reader.py). A _SUCCESS commit marker is written to signal
the Batcher that the forecast is ready.

Usage:
    # Random mode (writes to MinIO)
    python scripts/generate_sample_forecast.py \\
        --random \\
        --model medium_range \\
        --timestamp 2026-01-31T06:00:00Z \\
        --output s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z \\
        --endpoint-url http://localhost:9000

    # Scenario mode
    python scripts/generate_sample_forecast.py \\
        --scenario scripts/scenarios/int001_precip_trigger.json \\
        --output s3://watchpoint-forecasts/medium_range/2026-01-31T06:00:00Z \\
        --endpoint-url http://localhost:9000

    # Local filesystem output (for quick inspection)
    python scripts/generate_sample_forecast.py \\
        --scenario scripts/scenarios/int001_precip_trigger.json \\
        --output /tmp/forecast-test

Architecture References:
    - 12-operations.md Section 2.3 (setup scripts)
    - flow-simulations.md INT-001 (happy path integration test)
    - 11-runpod.md Section 3 (Zarr output contract)
    - 07-eval-worker.md Section 4.1 (ForecastReader interface)
"""

from __future__ import annotations

import argparse
import json
import os
import sys
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

import numpy as np
import xarray as xr
from numcodecs import Zstd

# ---------------------------------------------------------------------------
# Ensure project root is on sys.path so we can import runpod/worker modules.
# ---------------------------------------------------------------------------
PROJECT_ROOT = Path(__file__).resolve().parent.parent
sys.path.insert(0, str(PROJECT_ROOT))

from runpod.zarr_writer import CANONICAL_VARIABLES
from runpod.mock_engine import (
    ATLAS_LAT_SIZE,
    ATLAS_LON_SIZE,
    STORMSCOPE_LAT_SIZE,
    STORMSCOPE_LON_SIZE,
    MockEngine,
)

# ---------------------------------------------------------------------------
# Constants
# ---------------------------------------------------------------------------

# Default baseline value for scenario mode (safe zero baseline)
DEFAULT_BASELINE = 0.0

# Grid resolution
GRID_RESOLUTION = 0.25

# Tile grid parameters
TILE_LAT_DEG = 22.5
TILE_LON_DEG = 45.0

# Zarr compression settings matching production (11-runpod.md Section 3.1)
ZSTD_COMPRESSION_LEVEL = 3

# Chunk sizes matching the tile grid (without halo for integration testing)
# These align with the core tile size from the chunking strategy.
TILE_LAT_CHUNK = 90   # 22.5 degrees / 0.25 degree resolution
TILE_LON_CHUNK = 180   # 45.0 degrees / 0.25 degree resolution

# Commit marker file name
SUCCESS_MARKER = "_SUCCESS"


# ---------------------------------------------------------------------------
# Scenario Config Schema
# ---------------------------------------------------------------------------


class PointOverride:
    """A single point override in a scenario configuration.

    Attributes:
        lat: Latitude (-90 to 90)
        lon: Longitude (-180 to 180)
        values: Dict of variable_name -> float value to inject
    """

    def __init__(self, lat: float, lon: float, values: dict[str, float]) -> None:
        self.lat = lat
        self.lon = lon
        self.values = values

    def validate(self) -> list[str]:
        """Validate this point override, returning a list of errors."""
        errors: list[str] = []
        if not (-90.0 <= self.lat <= 90.0):
            errors.append(
                f"Latitude {self.lat} out of range [-90, 90]"
            )
        if not (-180.0 <= self.lon <= 180.0):
            errors.append(
                f"Longitude {self.lon} out of range [-180, 180]"
            )
        for var_name in self.values:
            if var_name not in CANONICAL_VARIABLES:
                errors.append(
                    f"Unknown variable '{var_name}'. "
                    f"Must be one of: {CANONICAL_VARIABLES}"
                )
        return errors


class ScenarioConfig:
    """Parsed scenario configuration for deterministic forecast generation.

    JSON Schema::

        {
            "model": "medium_range",        // optional, default "medium_range"
            "num_time_steps": 6,            // optional, default 6
            "baseline": 0.0,               // optional, value for all non-override cells
            "baselines": {                  // optional, per-variable baselines
                "temperature_c": 15.0,
                "humidity_percent": 50.0
            },
            "points": [
                {
                    "lat": 40.0,
                    "lon": -74.0,
                    "values": {
                        "precipitation_probability": 80.0,
                        "precipitation_mm": 25.0,
                        "temperature_c": 22.0
                    }
                }
            ]
        }
    """

    def __init__(
        self,
        model: str = "medium_range",
        num_time_steps: int = 6,
        baseline: float = DEFAULT_BASELINE,
        baselines: dict[str, float] | None = None,
        points: list[PointOverride] | None = None,
    ) -> None:
        self.model = model
        self.num_time_steps = num_time_steps
        self.baseline = baseline
        self.baselines = baselines or {}
        self.points = points or []

    def validate(self) -> list[str]:
        """Validate the entire scenario config, returning a list of errors."""
        errors: list[str] = []

        if self.model not in ("medium_range", "nowcast"):
            errors.append(
                f"Unknown model '{self.model}'. Must be 'medium_range' or 'nowcast'."
            )

        if self.num_time_steps < 1:
            errors.append(
                f"num_time_steps must be >= 1, got {self.num_time_steps}"
            )

        if not self.points:
            errors.append("Scenario must define at least one point override.")

        for i, point in enumerate(self.points):
            point_errors = point.validate()
            for err in point_errors:
                errors.append(f"points[{i}]: {err}")

        for var_name in self.baselines:
            if var_name not in CANONICAL_VARIABLES:
                errors.append(
                    f"baselines: Unknown variable '{var_name}'. "
                    f"Must be one of: {CANONICAL_VARIABLES}"
                )

        return errors

    @classmethod
    def from_dict(cls, data: dict[str, Any]) -> ScenarioConfig:
        """Parse a ScenarioConfig from a dictionary."""
        points = []
        for p in data.get("points", []):
            points.append(PointOverride(
                lat=float(p["lat"]),
                lon=float(p["lon"]),
                values={k: float(v) for k, v in p.get("values", {}).items()},
            ))

        return cls(
            model=data.get("model", "medium_range"),
            num_time_steps=data.get("num_time_steps", 6),
            baseline=float(data.get("baseline", DEFAULT_BASELINE)),
            baselines={k: float(v) for k, v in data.get("baselines", {}).items()},
            points=points,
        )

    @classmethod
    def from_file(cls, path: str) -> ScenarioConfig:
        """Load and parse a ScenarioConfig from a JSON file."""
        with open(path, "r") as f:
            data = json.load(f)
        return cls.from_dict(data)


# ---------------------------------------------------------------------------
# Grid coordinate builders
# ---------------------------------------------------------------------------


def build_lat_coords(model: str) -> np.ndarray:
    """Build latitude coordinate array matching the model grid.

    Atlas (medium_range): 90.0 to -90.0 descending, 721 points.
    StormScope (nowcast): 65.0 to 20.0 descending, 361 points.
    """
    if model == "medium_range":
        return np.linspace(90.0, -90.0, ATLAS_LAT_SIZE, dtype=np.float32)
    elif model == "nowcast":
        return np.linspace(65.0, 20.0, STORMSCOPE_LAT_SIZE, dtype=np.float32)
    else:
        raise ValueError(f"Unknown model: {model}")


def build_lon_coords(model: str) -> np.ndarray:
    """Build longitude coordinate array matching the model grid.

    Atlas (medium_range): -180.0 to 179.75 ascending, 1440 points.
    StormScope (nowcast): -130.0 to -70.25 ascending, 240 points.
    """
    if model == "medium_range":
        return np.linspace(-180.0, 179.75, ATLAS_LON_SIZE, dtype=np.float32)
    elif model == "nowcast":
        return np.linspace(-130.0, -70.25, STORMSCOPE_LON_SIZE, dtype=np.float32)
    else:
        raise ValueError(f"Unknown model: {model}")


def build_time_coords(num_steps: int, run_timestamp: datetime | None = None) -> np.ndarray:
    """Build time coordinate array as int64 Unix timestamps.

    Parameters
    ----------
    num_steps : int
        Number of forecast time steps.
    run_timestamp : datetime or None
        Base timestamp. If None, uses current UTC hour.

    Returns
    -------
    np.ndarray
        Array of int64 Unix timestamps, 1 hour apart.
    """
    if run_timestamp is None:
        run_timestamp = datetime.now(timezone.utc).replace(
            minute=0, second=0, microsecond=0
        )
    base_ts = int(run_timestamp.timestamp())
    return np.arange(
        base_ts,
        base_ts + num_steps * 3600,
        3600,
        dtype=np.int64,
    )


# ---------------------------------------------------------------------------
# Coordinate-to-index mapping
# ---------------------------------------------------------------------------


def find_nearest_index(coord_array: np.ndarray, target: float) -> int:
    """Find the index of the nearest value in a coordinate array.

    Parameters
    ----------
    coord_array : np.ndarray
        Sorted coordinate array (ascending or descending).
    target : float
        Target value to find.

    Returns
    -------
    int
        Index of the nearest value.
    """
    return int(np.argmin(np.abs(coord_array - target)))


# ---------------------------------------------------------------------------
# Dataset builders
# ---------------------------------------------------------------------------


def build_scenario_dataset(
    config: ScenarioConfig,
    run_timestamp: datetime | None = None,
) -> xr.Dataset:
    """Build an xarray Dataset for a deterministic scenario.

    1. Initialize all variable arrays with the baseline value.
    2. Apply point overrides at the nearest grid indices.

    Parameters
    ----------
    config : ScenarioConfig
        The scenario configuration with point overrides.
    run_timestamp : datetime or None
        Base timestamp for the time coordinate.

    Returns
    -------
    xr.Dataset
        Dataset with deterministic values, ready for Zarr output.
    """
    lat_coords = build_lat_coords(config.model)
    lon_coords = build_lon_coords(config.model)
    time_coords = build_time_coords(config.num_time_steps, run_timestamp)

    num_time = len(time_coords)
    num_lat = len(lat_coords)
    num_lon = len(lon_coords)

    data_vars: dict[str, tuple] = {}

    for var_name in CANONICAL_VARIABLES:
        # Use per-variable baseline if defined, otherwise global baseline
        baseline_val = config.baselines.get(var_name, config.baseline)
        arr = np.full(
            (num_time, num_lat, num_lon),
            fill_value=baseline_val,
            dtype=np.float32,
        )
        data_vars[var_name] = (["time", "lat", "lon"], arr)

    ds = xr.Dataset(
        data_vars=data_vars,
        coords={
            "time": ("time", time_coords),
            "lat": ("lat", lat_coords),
            "lon": ("lon", lon_coords),
        },
    )

    # Apply point overrides
    for point in config.points:
        lat_idx = find_nearest_index(lat_coords, point.lat)
        lon_idx = find_nearest_index(lon_coords, point.lon)

        actual_lat = float(lat_coords[lat_idx])
        actual_lon = float(lon_coords[lon_idx])

        print(
            f"  Applying override at ({point.lat}, {point.lon}) -> "
            f"grid index ({lat_idx}, {lon_idx}) = ({actual_lat:.2f}, {actual_lon:.2f})"
        )

        for var_name, value in point.values.items():
            if var_name in ds.data_vars:
                # Set value across all time steps at this grid point
                ds[var_name].values[:, lat_idx, lon_idx] = value

    return ds


def build_random_dataset(
    model: str = "medium_range",
    num_time_steps: int = 60,
    run_timestamp: datetime | None = None,
    seed: int | None = None,
) -> xr.Dataset:
    """Build a random forecast dataset using MockEngine.

    Parameters
    ----------
    model : str
        Model type: "medium_range" or "nowcast".
    num_time_steps : int
        Number of forecast time steps.
    run_timestamp : datetime or None
        Base timestamp. If None, uses current UTC hour.
    seed : int or None
        Random seed for reproducibility.

    Returns
    -------
    xr.Dataset
        Dataset with random-but-physically-bounded values.
    """
    engine = MockEngine(model=model, num_time_steps=num_time_steps, seed=seed)
    ds = engine.predict(xr.Dataset())

    # If a specific run_timestamp was given, rebuild the time coordinate
    if run_timestamp is not None:
        time_coords = build_time_coords(num_time_steps, run_timestamp)
        ds = ds.assign_coords(time=time_coords)

    return ds


# ---------------------------------------------------------------------------
# Tile ID computation (for verification / logging)
# ---------------------------------------------------------------------------


def compute_tile_id(lat: float, lon: float) -> str:
    """Compute the tile ID for a given lat/lon.

    Matches the DB tile_id formula from the reader:
        lat_index = FLOOR((90.0 - lat) / 22.5)
        lon_index = FLOOR(adjusted_lon / 45.0)
        adjusted_lon = lon if lon >= 0 else 360 + lon
    """
    lat_idx = int((90.0 - lat) / TILE_LAT_DEG)
    adjusted_lon = lon if lon >= 0 else 360.0 + lon
    lon_idx = int(adjusted_lon / TILE_LON_DEG)
    return f"{lat_idx}.{lon_idx}"


# ---------------------------------------------------------------------------
# Output helpers
# ---------------------------------------------------------------------------


def write_forecast(
    ds: xr.Dataset,
    destination: str,
    endpoint_url: str | None = None,
) -> None:
    """Write a forecast dataset to the specified destination as a Zarr store.

    Writes a standard Zarr v2 store with Zstd compression, matching the
    production data layout (without halo padding -- see module docstring).
    Writes a _SUCCESS commit marker after all data is persisted.

    Parameters
    ----------
    ds : xr.Dataset
        The forecast dataset to write.
    destination : str
        Output path (local path or s3:// URI).
    endpoint_url : str or None
        MinIO endpoint URL for local development. Only used for S3 destinations.
    """
    compressor = Zstd(level=ZSTD_COMPRESSION_LEVEL)

    # Set encoding for each variable
    time_size = ds.sizes["time"]
    encoding: dict[str, dict[str, Any]] = {}
    for var_name in ds.data_vars:
        encoding[var_name] = {
            "compressor": compressor,
            "dtype": "float32",
            "chunks": (time_size, TILE_LAT_CHUNK, TILE_LON_CHUNK),
        }
    for coord_name in ["time", "lat", "lon"]:
        encoding[coord_name] = {
            "compressor": compressor,
        }

    if destination.startswith("s3://"):
        storage_options = _build_s3_storage_options(endpoint_url)
        ds.to_zarr(
            destination,
            mode="w",
            encoding=encoding,
            storage_options=storage_options if storage_options else None,
        )
        _write_success_marker_s3(destination, storage_options)
    else:
        # Local filesystem
        import shutil
        if os.path.exists(destination):
            shutil.rmtree(destination)
        ds.to_zarr(destination, mode="w", encoding=encoding)
        _write_success_marker_local(destination)

    print(f"  Forecast written to: {destination}")


def _build_s3_storage_options(endpoint_url: str | None) -> dict[str, Any]:
    """Build s3fs storage options for xarray.to_zarr().

    Parameters
    ----------
    endpoint_url : str or None
        MinIO endpoint URL. If None, uses default AWS credential chain.

    Returns
    -------
    dict
        Storage options for s3fs.
    """
    opts: dict[str, Any] = {}
    if endpoint_url:
        opts["client_kwargs"] = {"endpoint_url": endpoint_url}
        opts["key"] = os.environ.get("AWS_ACCESS_KEY_ID", "minioadmin")
        opts["secret"] = os.environ.get("AWS_SECRET_ACCESS_KEY", "minioadmin")
    return opts


def _write_success_marker_s3(
    destination: str,
    storage_options: dict[str, Any],
) -> None:
    """Write the _SUCCESS commit marker to S3/MinIO.

    Uses boto3 directly with the endpoint_url from storage_options.
    """
    import boto3

    parts = destination.replace("s3://", "").split("/", 1)
    bucket = parts[0]
    prefix = parts[1] if len(parts) > 1 else ""
    key = f"{prefix}/{SUCCESS_MARKER}" if prefix else SUCCESS_MARKER

    kwargs: dict[str, Any] = {"region_name": "us-east-1"}

    # Extract endpoint_url from storage_options
    if storage_options and "client_kwargs" in storage_options:
        ep = storage_options["client_kwargs"].get("endpoint_url")
        if ep:
            kwargs["endpoint_url"] = ep

    # Use credentials from storage_options if available
    access_key = storage_options.get("key") if storage_options else None
    secret_key = storage_options.get("secret") if storage_options else None

    if access_key and secret_key:
        kwargs["aws_access_key_id"] = access_key
        kwargs["aws_secret_access_key"] = secret_key

    s3 = boto3.client("s3", **kwargs)
    s3.put_object(Bucket=bucket, Key=key, Body=b"")


def _write_success_marker_local(destination: str) -> None:
    """Write the _SUCCESS commit marker to a local filesystem path."""
    success_path = os.path.join(destination, SUCCESS_MARKER)
    Path(success_path).touch()


# ---------------------------------------------------------------------------
# CLI Entry Point
# ---------------------------------------------------------------------------


def parse_timestamp(ts_str: str) -> datetime:
    """Parse an ISO 8601 timestamp string into a timezone-aware datetime."""
    # Handle the Z suffix (replace with +00:00 for fromisoformat)
    if ts_str.endswith("Z"):
        ts_str = ts_str[:-1] + "+00:00"
    return datetime.fromisoformat(ts_str)


def main() -> None:
    parser = argparse.ArgumentParser(
        description="Generate Zarr-format forecast data for local development and testing.",
        formatter_class=argparse.RawDescriptionHelpFormatter,
        epilog="""
Examples:
  # Random mode (full global forecast)
  python scripts/generate_sample_forecast.py \\
      --random \\
      --model medium_range --timestamp 2026-02-01T06:00:00Z \\
      --output /tmp/forecast-test

  # Scenario mode (deterministic values at specific locations)
  python scripts/generate_sample_forecast.py \\
      --scenario scripts/scenarios/int001_precip_trigger.json \\
      --output s3://watchpoint-forecasts/medium_range/2026-02-01T06:00:00Z \\
      --endpoint-url http://localhost:9000

Scenario JSON format:
  {
    "model": "medium_range",
    "num_time_steps": 6,
    "baseline": 0.0,
    "baselines": {"temperature_c": 15.0},
    "points": [
      {"lat": 40.0, "lon": -74.0, "values": {"precipitation_probability": 80.0}}
    ]
  }
        """,
    )

    # Mode selection (mutually exclusive)
    mode_group = parser.add_mutually_exclusive_group(required=True)
    mode_group.add_argument(
        "--scenario",
        type=str,
        metavar="CONFIG_JSON",
        help="Path to scenario JSON config file for deterministic data generation.",
    )
    mode_group.add_argument(
        "--random",
        action="store_true",
        help="Generate random-but-physically-bounded forecast data.",
    )

    # Common options
    parser.add_argument(
        "--output",
        type=str,
        required=True,
        help="Output path (local filesystem or s3:// URI).",
    )
    parser.add_argument(
        "--endpoint-url",
        type=str,
        default=os.environ.get("MINIO_ENDPOINT", None),
        help=(
            "S3-compatible endpoint URL (e.g., http://localhost:9000 for MinIO). "
            "Defaults to MINIO_ENDPOINT env var."
        ),
    )

    # Random mode options
    parser.add_argument(
        "--model",
        type=str,
        choices=["medium_range", "nowcast"],
        default="medium_range",
        help="Forecast model type (random mode only). Default: medium_range.",
    )
    parser.add_argument(
        "--timestamp",
        type=str,
        default=None,
        help="Forecast run timestamp in ISO 8601 format (e.g., 2026-02-01T06:00:00Z).",
    )
    parser.add_argument(
        "--time-steps",
        type=int,
        default=60,
        help="Number of forecast time steps (random mode only). Default: 60.",
    )
    parser.add_argument(
        "--seed",
        type=int,
        default=None,
        help="Random seed for reproducibility (random mode only).",
    )

    args = parser.parse_args()

    # Parse optional timestamp
    run_timestamp = None
    if args.timestamp:
        run_timestamp = parse_timestamp(args.timestamp)

    print("=" * 60)
    print(" WatchPoint Forecast Seeder")
    print("=" * 60)
    print()

    config = None  # Initialized here for use in verification hints below

    if args.scenario:
        # --- Scenario Mode ---
        print("Mode: SCENARIO (deterministic)")
        print(f"Config: {args.scenario}")

        config = ScenarioConfig.from_file(args.scenario)
        errors = config.validate()
        if errors:
            print("\nScenario config validation errors:")
            for err in errors:
                print(f"  - {err}")
            sys.exit(1)

        print(f"Model: {config.model}")
        print(f"Time steps: {config.num_time_steps}")
        print(f"Baseline: {config.baseline}")
        if config.baselines:
            print(f"Per-variable baselines: {config.baselines}")
        print(f"Point overrides: {len(config.points)}")
        print()

        # Log point details and tile IDs
        for i, point in enumerate(config.points):
            tile_id = compute_tile_id(point.lat, point.lon)
            print(f"  Point {i}: ({point.lat}, {point.lon}) -> tile_id={tile_id}")
            for var_name, value in point.values.items():
                print(f"    {var_name} = {value}")

        print()
        ds = build_scenario_dataset(config, run_timestamp)

    else:
        # --- Random Mode ---
        print("Mode: RANDOM")
        print(f"Model: {args.model}")
        print(f"Time steps: {args.time_steps}")
        if args.seed is not None:
            print(f"Seed: {args.seed}")
        print()

        ds = build_random_dataset(
            model=args.model,
            num_time_steps=args.time_steps,
            run_timestamp=run_timestamp,
            seed=args.seed,
        )

    # Write the dataset
    print(f"Output: {args.output}")
    if args.endpoint_url:
        print(f"Endpoint: {args.endpoint_url}")
    print()

    write_forecast(ds, args.output, endpoint_url=args.endpoint_url)

    print()
    print("=" * 60)
    print(" Forecast generation complete.")
    print("=" * 60)

    # Print verification hints (reuse the config already loaded above)
    if config is not None:
        print()
        print("Verification hints:")
        for point in config.points:
            tile_id = compute_tile_id(point.lat, point.lon)
            print(f"  tile_id={tile_id} lat={point.lat} lon={point.lon}")
            print(f"    Expected values: {point.values}")


if __name__ == "__main__":
    main()
