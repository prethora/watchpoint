"""
Temporal Deduplication for Monitor Mode (EVAL-003c).

Implements the ``ThreatDeduplicator`` which prevents re-alerting for the
same weather threat by comparing newly identified violation periods against
the ``seen_threats`` history stored in evaluation state.

Architecture References:
    - 07-eval-worker.md Section 5.2 (Temporal Overlap Detection)
    - flow-simulations.md EVAL-003 steps 5-6 (Deduplication & Pruning)

Overlap Logic:
    Two time periods overlap if: ``a.start <= b.end AND b.start <= a.end``.
    When overlap is detected, the existing threat is extended to cover both
    periods (merged). No notification is generated for the overlapping portion.

Pruning:
    Entries in ``seen_threats`` where ``end_time < now`` are removed to prevent
    unbounded growth of the JSONB array.
"""

from __future__ import annotations

import logging
from datetime import datetime, timezone

from worker.eval.models import SeenThreat

logger = logging.getLogger(__name__)


class ThreatDeduplicator:
    """Performs temporal overlap detection and pruning on seen_threats.

    Used by the Monitor Mode evaluator to determine which violation periods
    represent genuinely new threats versus continuations of known threats.
    """

    @staticmethod
    def overlaps(a: SeenThreat, b_start: datetime, b_end: datetime) -> bool:
        """Check if threat ``a`` overlaps with the period [b_start, b_end].

        Two periods overlap if: a.start <= b_end AND b_start <= a.end.

        Parameters
        ----------
        a : SeenThreat
            Existing threat from ``seen_threats``.
        b_start : datetime
            Start of the new violation period.
        b_end : datetime
            End of the new violation period.

        Returns
        -------
        bool
            True if the periods overlap.
        """
        return a.start <= b_end and b_start <= a.end

    @staticmethod
    def merge(existing: SeenThreat, new_start: datetime, new_end: datetime) -> SeenThreat:
        """Merge a new violation period into an existing threat.

        Extends the existing threat to cover both the original and new period.

        Parameters
        ----------
        existing : SeenThreat
            The existing threat entry to extend.
        new_start : datetime
            Start of the new violation period.
        new_end : datetime
            End of the new violation period.

        Returns
        -------
        SeenThreat
            A new SeenThreat with the merged time range.
        """
        return SeenThreat(
            start=min(existing.start, new_start),
            end=max(existing.end, new_end),
            type=existing.type,
        )

    @staticmethod
    def prune(
        threats: list[SeenThreat],
        now: datetime,
    ) -> list[SeenThreat]:
        """Remove expired threats from the seen_threats list.

        Per EVAL-003 step 6: Removes entries where ``end_time < now``.
        This prevents unbounded growth of the JSONB array.

        Parameters
        ----------
        threats : list[SeenThreat]
            Current seen_threats list.
        now : datetime
            Current evaluation time (UTC).

        Returns
        -------
        list[SeenThreat]
            Pruned list with expired threats removed.
        """
        # Ensure now is timezone-aware for comparison
        if now.tzinfo is None:
            now = now.replace(tzinfo=timezone.utc)

        pruned = []
        for t in threats:
            t_end = t.end
            if t_end.tzinfo is None:
                t_end = t_end.replace(tzinfo=timezone.utc)
            if t_end >= now:
                pruned.append(t)
            else:
                logger.debug(
                    "Pruning expired threat: type=%s, start=%s, end=%s",
                    t.type,
                    t.start.isoformat(),
                    t.end.isoformat(),
                )

        return pruned

    def deduplicate(
        self,
        seen_threats: list[SeenThreat],
        new_violations: list[SeenThreat],
        now: datetime,
    ) -> tuple[list[SeenThreat], list[SeenThreat]]:
        """Run full deduplication cycle: check overlaps, merge, and prune.

        For each new violation period, checks against all existing threats.
        If overlap is found, the existing threat is extended (merged) and no
        notification is generated for that violation. If no overlap is found,
        the violation is considered a new threat and should trigger a notification.

        After processing all new violations, expired threats are pruned.

        Parameters
        ----------
        seen_threats : list[SeenThreat]
            Current ``seen_threats`` from evaluation state.
        new_violations : list[SeenThreat]
            Newly identified violation periods from the current evaluation.
        now : datetime
            Current evaluation time (UTC). Used for pruning.

        Returns
        -------
        tuple[list[SeenThreat], list[SeenThreat]]
            A tuple of:
            - Updated ``seen_threats`` list (merged + pruned).
            - List of genuinely new threats that should trigger notifications.
        """
        # Work on a mutable copy of seen_threats
        current_threats = list(seen_threats)
        genuinely_new: list[SeenThreat] = []

        for violation in new_violations:
            merged = False
            for i, existing in enumerate(current_threats):
                if self.overlaps(existing, violation.start, violation.end):
                    # Overlap detected: merge into existing threat
                    current_threats[i] = self.merge(
                        existing, violation.start, violation.end
                    )
                    logger.debug(
                        "Merged overlapping violation into existing threat: "
                        "existing=[%s, %s], violation=[%s, %s], "
                        "merged=[%s, %s]",
                        existing.start.isoformat(),
                        existing.end.isoformat(),
                        violation.start.isoformat(),
                        violation.end.isoformat(),
                        current_threats[i].start.isoformat(),
                        current_threats[i].end.isoformat(),
                    )
                    merged = True
                    break

            if not merged:
                # New threat: add to list and mark for notification
                current_threats.append(violation)
                genuinely_new.append(violation)
                logger.debug(
                    "New threat detected: type=%s, start=%s, end=%s",
                    violation.type,
                    violation.start.isoformat(),
                    violation.end.isoformat(),
                )

        # Prune expired threats
        current_threats = self.prune(current_threats, now)

        return current_threats, genuinely_new
