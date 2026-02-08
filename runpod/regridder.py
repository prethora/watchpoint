"""
Regridder: Convert model-native grids to WatchPoint canonical grids.

Handles two regridding tasks:
  1. StormScope HRRR Lambert Conformal -> regular 361x240 lat/lon grid (CONUS)
  2. GFS 0.25-degree global -> CONUS 361x240 subset with interpolation

Uses scipy.interpolate.griddata with bilinear interpolation.

Target grid (CONUS nowcast):
  - lat: 65.0 to 20.0 (361 points, descending)
  - lon: -130.0 to -70.25 (240 points, ascending)

References:
  - 11-runpod.md Section 3.4 (Grid Specifications)
"""

from __future__ import annotations

import logging

import numpy as np
from scipy.interpolate import griddata

logger = logging.getLogger(__name__)

# Target CONUS grid specification
TARGET_LAT_SIZE = 361
TARGET_LON_SIZE = 240
TARGET_LAT_START = 65.0
TARGET_LAT_END = 20.0
TARGET_LON_START = -130.0
TARGET_LON_END = -70.25


class Regridder:
    """
    Regrids data from source grids to the WatchPoint CONUS target grid.

    The target grid is computed once at construction time and reused
    for all regridding operations.
    """

    def __init__(self) -> None:
        # Build target grid
        self.target_lat = np.linspace(
            TARGET_LAT_START, TARGET_LAT_END, TARGET_LAT_SIZE, dtype=np.float64,
        )
        self.target_lon = np.linspace(
            TARGET_LON_START, TARGET_LON_END, TARGET_LON_SIZE, dtype=np.float64,
        )
        # 2D meshgrid for interpolation targets
        self._target_lon_2d, self._target_lat_2d = np.meshgrid(
            self.target_lon, self.target_lat,
        )

    def regrid_hrrr(
        self,
        data: np.ndarray | list[np.ndarray],
        src_lat: np.ndarray,
        src_lon: np.ndarray,
    ) -> np.ndarray:
        """
        Regrid StormScope HRRR data (Lambert Conformal) to the target lat/lon grid.

        Parameters
        ----------
        data : np.ndarray or list of np.ndarray
            If a list, each element is a 2D array (lat, lon) for one time step.
            If an ndarray, shape is (time, lat, lon).
        src_lat : np.ndarray
            2D source latitude array from StormScope model, shape (ny, nx).
        src_lon : np.ndarray
            2D source longitude array from StormScope model, shape (ny, nx).

        Returns
        -------
        np.ndarray
            Regridded data, shape (n_times, TARGET_LAT_SIZE, TARGET_LON_SIZE).
        """
        if isinstance(data, list):
            arrays = data
        else:
            arrays = [data[t] for t in range(data.shape[0])]

        n_times = len(arrays)
        result = np.empty(
            (n_times, TARGET_LAT_SIZE, TARGET_LON_SIZE), dtype=np.float64,
        )

        # Flatten source grid coordinates
        src_points = np.column_stack([src_lat.ravel(), src_lon.ravel()])

        for t in range(n_times):
            src_values = arrays[t].ravel()
            result[t] = griddata(
                src_points,
                src_values,
                (self._target_lat_2d, self._target_lon_2d),
                method="linear",
                fill_value=0.0,
            )

        return result

    def regrid_gfs(
        self,
        gfs_data: dict[str, np.ndarray],
        gfs_lat: np.ndarray | None = None,
        gfs_lon: np.ndarray | None = None,
    ) -> dict[str, np.ndarray]:
        """
        Subset and interpolate GFS data (0.25-degree global) to the CONUS target grid.

        Parameters
        ----------
        gfs_data : dict
            Variable name -> numpy array mapping. Each array is either:
            - 2D (lat, lon) for a single-time GFS analysis
            - 3D (time, lat, lon)
        gfs_lat : np.ndarray, optional
            1D latitude array for GFS data. If None, assumes standard 0.25-degree
            grid from 90.0 to -90.0 (721 points).
        gfs_lon : np.ndarray, optional
            1D longitude array for GFS data. If None, assumes standard 0.25-degree
            grid from 0.0 to 359.75 (1440 points), and converts to -180..180.

        Returns
        -------
        dict
            Same variable names, each regridded to (TARGET_LAT_SIZE, TARGET_LON_SIZE).
        """
        if gfs_lat is None:
            gfs_lat = np.linspace(90.0, -90.0, 721, dtype=np.float64)
        if gfs_lon is None:
            # GFS uses 0-360, convert to -180..180
            gfs_lon = np.linspace(0.0, 359.75, 1440, dtype=np.float64)
            gfs_lon = np.where(gfs_lon > 180, gfs_lon - 360, gfs_lon)

        # Build source meshgrid
        src_lon_2d, src_lat_2d = np.meshgrid(gfs_lon, gfs_lat)

        # CONUS bounding box (with margin for interpolation)
        lat_mask = (gfs_lat >= TARGET_LAT_END - 1.0) & (gfs_lat <= TARGET_LAT_START + 1.0)
        lon_mask = (gfs_lon >= TARGET_LON_START - 1.0) & (gfs_lon <= TARGET_LON_END + 1.0)

        # Subset indices
        lat_idx = np.where(lat_mask)[0]
        lon_idx = np.where(lon_mask)[0]

        if len(lat_idx) == 0 or len(lon_idx) == 0:
            raise ValueError(
                "GFS data does not cover the CONUS target region. "
                f"lat range: [{gfs_lat.min()}, {gfs_lat.max()}], "
                f"lon range: [{gfs_lon.min()}, {gfs_lon.max()}]"
            )

        # Subset source grid
        sub_lat = src_lat_2d[np.ix_(lat_idx, lon_idx)]
        sub_lon = src_lon_2d[np.ix_(lat_idx, lon_idx)]
        src_points = np.column_stack([sub_lat.ravel(), sub_lon.ravel()])

        result = {}
        for var_name, arr in gfs_data.items():
            # Handle 3D arrays by taking first time step
            if arr.ndim == 3:
                arr = arr[0]

            sub_values = arr[np.ix_(lat_idx, lon_idx)].ravel()
            regridded = griddata(
                src_points,
                sub_values,
                (self._target_lat_2d, self._target_lon_2d),
                method="linear",
                fill_value=np.nan,
            )

            # Fill any remaining NaN with nearest neighbor
            if np.any(np.isnan(regridded)):
                nn = griddata(
                    src_points,
                    sub_values,
                    (self._target_lat_2d, self._target_lon_2d),
                    method="nearest",
                )
                regridded = np.where(np.isnan(regridded), nn, regridded)

            result[var_name] = regridded

        return result
