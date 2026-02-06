"""
Monitor Mode evaluation logic (EVAL-003).

Implements the ``MonitorEvaluator`` which handles rolling-window time-series
extraction, timezone conversion, active hours filtering, and violation period
identification for Monitor Mode WatchPoints.

Architecture References:
    - 07-eval-worker.md Section 5.2 (Monitor Mode)
    - flow-simulations.md EVAL-003 (Monitor Mode Evaluation)
    - flow-simulations.md EVAL-005 (First Evaluation Baseline)

Monitor Mode Pipeline:
    1. Extract time-series from TileData at the WatchPoint's location.
    2. Convert forecast time index from UTC to the WatchPoint's local timezone.
    3. Filter by Active Hours and Active Days.
    4. Evaluate conditions at each remaining timestep.
    5. Identify contiguous violation periods.
    6. Run temporal deduplication against ``seen_threats``.
    7. Generate ``MonitorSummary`` for digest data.
"""

from __future__ import annotations

import logging
from datetime import datetime, timedelta, timezone
from typing import Callable

import numpy as np

try:
    import zoneinfo
except ImportError:
    from backports import zoneinfo  # type: ignore[no-redef]

from worker.eval.models import (
    Condition,
    ConditionLogic,
    MonitorConfig,
    MonitorSummary,
    SeenThreat,
    TimeRange,
    WatchPoint,
)
from worker.eval.reader import TileData

logger = logging.getLogger(__name__)


# Type alias for the condition checker function
ConditionChecker = Callable[[float, Condition], bool]


class MonitorEvaluator:
    """Evaluates Monitor Mode WatchPoints over rolling time windows.

    This class extracts time-series data from loaded forecast tiles,
    applies timezone conversion and active hours filtering, identifies
    contiguous violation periods, and produces a ``MonitorSummary`` for
    downstream digest generation.

    Parameters
    ----------
    check_condition_fn : ConditionChecker
        The ``check_condition`` function from the evaluator module. Injected
        to avoid circular imports and enable testing with mocks.
    """

    def __init__(self, check_condition_fn: ConditionChecker) -> None:
        self._check_condition = check_condition_fn

    def evaluate(
        self,
        wp: WatchPoint,
        tile_data: TileData,
        now: datetime,
    ) -> tuple[MonitorSummary, list[SeenThreat]]:
        """Run the full Monitor Mode evaluation pipeline.

        Parameters
        ----------
        wp : WatchPoint
            The WatchPoint being evaluated. Must have ``monitor_config`` set.
        tile_data : TileData
            Loaded forecast data for the tile.
        now : datetime
            Current evaluation time (UTC).

        Returns
        -------
        tuple[MonitorSummary, list[SeenThreat]]
            A tuple of:
            - ``MonitorSummary`` containing window metadata and max values.
            - List of violation periods as ``SeenThreat`` entries.
        """
        if wp.monitor_config is None:
            raise ValueError(
                f"WatchPoint {wp.id} has no monitor_config; "
                f"cannot evaluate in Monitor Mode."
            )

        monitor_config = wp.monitor_config

        # Step 1: Extract time-series at the WatchPoint's location
        timeseries = tile_data.extract_timeseries(
            wp.location.lat, wp.location.lon
        )
        time_values = tile_data.times

        if len(time_values) == 0:
            logger.warning(
                "No time steps in tile data for wp=%s", wp.id
            )
            return self._empty_summary(now, monitor_config), []

        # Step 2: Calculate rolling window
        window_start = now
        window_end = now + timedelta(hours=monitor_config.window_hours)

        # Step 3: Timezone conversion (EVAL-003a)
        # Convert UTC time index to WatchPoint's local timezone
        try:
            local_tz = zoneinfo.ZoneInfo(wp.timezone)
        except (KeyError, Exception) as exc:
            logger.warning(
                "Invalid timezone '%s' for wp=%s, defaulting to UTC: %s",
                wp.timezone,
                wp.id,
                exc,
            )
            local_tz = timezone.utc

        # Convert numpy datetime64 to Python datetime objects in local time
        local_times = self._convert_times_to_local(time_values, local_tz)

        # Step 4: Filter to the rolling window
        window_mask = self._apply_window_filter(
            local_times, window_start, window_end, local_tz
        )

        # Step 5: Active Hours/Days filter (EVAL-003b)
        active_mask = self._apply_active_hours_filter(
            local_times, monitor_config
        )

        # Combined mask: within window AND active
        combined_mask = window_mask & active_mask

        if not np.any(combined_mask):
            logger.debug(
                "No active timesteps within window for wp=%s", wp.id
            )
            return (
                self._build_summary(
                    window_start, window_end, timeseries, combined_mask
                ),
                [],
            )

        # Step 6: Evaluate conditions at each active timestep
        violations = self._find_violations(
            wp, timeseries, local_times, combined_mask
        )

        # Step 7: Build MonitorSummary
        summary = self._build_summary(
            window_start, window_end, timeseries, combined_mask
        )

        return summary, violations

    def _convert_times_to_local(
        self,
        time_values: np.ndarray,
        local_tz: zoneinfo.ZoneInfo | timezone,
    ) -> list[datetime]:
        """Convert numpy datetime64 array to local-timezone Python datetimes.

        Parameters
        ----------
        time_values : np.ndarray
            Array of numpy datetime64 values (UTC).
        local_tz : ZoneInfo or timezone
            Target timezone for conversion.

        Returns
        -------
        list[datetime]
            Python datetime objects in the local timezone.
        """
        result: list[datetime] = []
        for t in time_values:
            # Convert numpy datetime64 -> Python datetime (UTC)
            ts = (t - np.datetime64("1970-01-01T00:00:00")) / np.timedelta64(
                1, "s"
            )
            utc_dt = datetime.fromtimestamp(float(ts), tz=timezone.utc)
            # Convert to local timezone
            local_dt = utc_dt.astimezone(local_tz)
            result.append(local_dt)
        return result

    def _apply_window_filter(
        self,
        local_times: list[datetime],
        window_start: datetime,
        window_end: datetime,
        local_tz: zoneinfo.ZoneInfo | timezone,
    ) -> np.ndarray:
        """Create a boolean mask for timesteps within the rolling window.

        Parameters
        ----------
        local_times : list[datetime]
            Local-timezone timestamps.
        window_start : datetime
            Start of the rolling window (UTC).
        window_end : datetime
            End of the rolling window (UTC).
        local_tz : ZoneInfo or timezone
            Local timezone for comparison.

        Returns
        -------
        np.ndarray
            Boolean mask array.
        """
        # Convert window bounds to local timezone for comparison
        if window_start.tzinfo is None:
            window_start = window_start.replace(tzinfo=timezone.utc)
        if window_end.tzinfo is None:
            window_end = window_end.replace(tzinfo=timezone.utc)

        ws_local = window_start.astimezone(local_tz)
        we_local = window_end.astimezone(local_tz)

        mask = np.array(
            [ws_local <= t <= we_local for t in local_times], dtype=bool
        )
        return mask

    def _apply_active_hours_filter(
        self,
        local_times: list[datetime],
        monitor_config: MonitorConfig,
    ) -> np.ndarray:
        """Create a boolean mask for timesteps within active hours/days.

        Active hours are defined as pairs ``[start_hour, end_hour]`` in the
        WatchPoint's local timezone. Active days are ISO weekdays (1=Monday,
        7=Sunday).

        If ``active_hours`` is empty, all hours are considered active.
        If ``active_days`` is empty, all days are considered active.

        Parameters
        ----------
        local_times : list[datetime]
            Timestamps in the WatchPoint's local timezone.
        monitor_config : MonitorConfig
            Monitor mode configuration with active_hours and active_days.

        Returns
        -------
        np.ndarray
            Boolean mask array.
        """
        mask = np.ones(len(local_times), dtype=bool)

        # Active days filter (ISO weekday: 1=Mon, 7=Sun)
        if monitor_config.active_days:
            active_days_set = set(monitor_config.active_days)
            for i, t in enumerate(local_times):
                if t.isoweekday() not in active_days_set:
                    mask[i] = False

        # Active hours filter
        if monitor_config.active_hours:
            for i, t in enumerate(local_times):
                if not mask[i]:
                    continue  # Already excluded by day filter
                hour = t.hour
                in_any_window = False
                for window in monitor_config.active_hours:
                    if len(window) != 2:
                        continue
                    start_hour, end_hour = window[0], window[1]
                    if start_hour <= end_hour:
                        # Normal range: e.g., [8, 18]
                        if start_hour <= hour < end_hour:
                            in_any_window = True
                            break
                    else:
                        # Wrapping range: e.g., [22, 6] means 22-23, 0-5
                        if hour >= start_hour or hour < end_hour:
                            in_any_window = True
                            break
                if not in_any_window:
                    mask[i] = False

        return mask

    def _find_violations(
        self,
        wp: WatchPoint,
        timeseries: dict[str, np.ndarray],
        local_times: list[datetime],
        active_mask: np.ndarray,
    ) -> list[SeenThreat]:
        """Identify contiguous periods where conditions are met.

        Scans through the active timesteps and checks the WatchPoint's
        conditions at each step. Groups contiguous violations into periods.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint being evaluated.
        timeseries : dict[str, np.ndarray]
            Time-series data keyed by canonical variable name.
        local_times : list[datetime]
            Timestamps in local timezone.
        active_mask : np.ndarray
            Boolean mask of active timesteps.

        Returns
        -------
        list[SeenThreat]
            List of contiguous violation periods.
        """
        n = len(local_times)
        violations: list[SeenThreat] = []
        in_violation = False
        period_start: datetime | None = None
        primary_variable = wp.conditions[0].variable if wp.conditions else ""

        for i in range(n):
            if not active_mask[i]:
                # Gap in active hours ends a violation period
                if in_violation and period_start is not None:
                    violations.append(
                        SeenThreat(
                            start=period_start,
                            end=local_times[i - 1],
                            type=primary_variable,
                        )
                    )
                    in_violation = False
                    period_start = None
                continue

            # Check conditions at this timestep
            triggered = self._check_conditions_at_timestep(
                wp, timeseries, i
            )

            if triggered:
                if not in_violation:
                    in_violation = True
                    period_start = local_times[i]
            else:
                if in_violation and period_start is not None:
                    violations.append(
                        SeenThreat(
                            start=period_start,
                            end=local_times[i - 1],
                            type=primary_variable,
                        )
                    )
                    in_violation = False
                    period_start = None

        # Close final violation if still in one at end of series
        if in_violation and period_start is not None:
            violations.append(
                SeenThreat(
                    start=period_start,
                    end=local_times[n - 1],
                    type=primary_variable,
                )
            )

        return violations

    def _check_conditions_at_timestep(
        self,
        wp: WatchPoint,
        timeseries: dict[str, np.ndarray],
        idx: int,
    ) -> bool:
        """Check all WatchPoint conditions at a specific timestep index.

        Applies condition_logic (ANY/ALL) to combine results.

        Parameters
        ----------
        wp : WatchPoint
            WatchPoint with conditions and condition_logic.
        timeseries : dict[str, np.ndarray]
            Time-series data.
        idx : int
            Index into the time-series arrays.

        Returns
        -------
        bool
            True if conditions are met at this timestep.
        """
        results: list[bool] = []

        for condition in wp.conditions:
            variable_data = timeseries.get(condition.variable)
            if variable_data is None:
                logger.warning(
                    "Variable '%s' not found in timeseries for wp=%s",
                    condition.variable,
                    wp.id,
                )
                results.append(False)
                continue

            # Handle both scalar and array data
            if variable_data.ndim == 0:
                actual = float(variable_data)
            elif idx < len(variable_data):
                actual = float(variable_data[idx])
            else:
                results.append(False)
                continue

            results.append(self._check_condition(actual, condition))

        if not results:
            return False

        if wp.condition_logic == ConditionLogic.ALL:
            return all(results)
        else:  # ANY
            return any(results)

    def _build_summary(
        self,
        window_start: datetime,
        window_end: datetime,
        timeseries: dict[str, np.ndarray],
        active_mask: np.ndarray,
    ) -> MonitorSummary:
        """Build a MonitorSummary with max values over the active window.

        Parameters
        ----------
        window_start : datetime
            Start of the rolling window.
        window_end : datetime
            End of the rolling window.
        timeseries : dict[str, np.ndarray]
            Full time-series data.
        active_mask : np.ndarray
            Boolean mask of active timesteps.

        Returns
        -------
        MonitorSummary
            Summary with max values and empty triggered_periods.
            (Triggered periods are populated by the caller after dedup.)
        """
        max_values: dict[str, float] = {}

        for var_name, data in timeseries.items():
            if data.ndim == 0:
                max_values[var_name] = float(data)
            elif np.any(active_mask) and len(data) == len(active_mask):
                active_data = data[active_mask]
                if len(active_data) > 0:
                    max_values[var_name] = float(np.nanmax(active_data))
            elif len(data) > 0:
                max_values[var_name] = float(np.nanmax(data))

        # Ensure timezone-aware datetimes
        if window_start.tzinfo is None:
            window_start = window_start.replace(tzinfo=timezone.utc)
        if window_end.tzinfo is None:
            window_end = window_end.replace(tzinfo=timezone.utc)

        return MonitorSummary(
            window_start=window_start,
            window_end=window_end,
            max_values=max_values,
            triggered_periods=[],
        )

    def _empty_summary(
        self,
        now: datetime,
        monitor_config: MonitorConfig,
    ) -> MonitorSummary:
        """Build an empty MonitorSummary when no data is available."""
        if now.tzinfo is None:
            now = now.replace(tzinfo=timezone.utc)

        return MonitorSummary(
            window_start=now,
            window_end=now + timedelta(hours=monitor_config.window_hours),
            max_values={},
            triggered_periods=[],
        )
