"""
Core evaluation logic for the Eval Worker.

This package contains the condition evaluation engine, monitor mode
time-series analysis, and temporal deduplication logic.

Public API:
    - ``StandardEvaluator`` -- Main evaluator implementing ``evaluate_batch``.
    - ``ConfigPolicy`` -- Resolves config version mismatches (EVAL-004).
    - ``check_condition`` -- Single condition evaluation against a value.
    - ``MonitorEvaluator`` -- Monitor Mode rolling window analysis (EVAL-003).
    - ``ThreatDeduplicator`` -- Temporal overlap detection (EVAL-003c).
"""

from worker.eval.logic.dedup import ThreatDeduplicator
from worker.eval.logic.evaluator import (
    ConfigPolicy,
    StandardEvaluator,
    check_condition,
    compute_tile_id,
)
from worker.eval.logic.monitor import MonitorEvaluator

__all__ = [
    "StandardEvaluator",
    "ConfigPolicy",
    "check_condition",
    "compute_tile_id",
    "MonitorEvaluator",
    "ThreatDeduplicator",
]
