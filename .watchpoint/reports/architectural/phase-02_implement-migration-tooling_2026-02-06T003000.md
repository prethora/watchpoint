# Architectural Suggestion: tile_id Generated Column Uses Non-Immutable CONCAT Function

## Context
While implementing the "Implement Migration tooling in Makefile" task in Phase 2, running `make migrate-up` failed on migration `003_watchpoints.up.sql` with the error: "generation expression is not immutable".

## The Issue
The `tile_id` generated column in the `watchpoints` table specification uses the `CONCAT()` function, which PostgreSQL classifies as `STABLE` (not `IMMUTABLE`). PostgreSQL requires `GENERATED ALWAYS AS ... STORED` expressions to use only `IMMUTABLE` functions and operators.

The spec in `02-foundation-db.md` Section 3.2 defines:
```sql
tile_id TEXT GENERATED ALWAYS AS (
    CONCAT(
        FLOOR((90.0 - location_lat) / 22.5)::INT,
        '.',
        FLOOR(...)::INT
    )
) STORED,
```

This will not execute on PostgreSQL 15 (or any version). The `CONCAT()` function is marked `STABLE` in PostgreSQL's catalog because it handles NULL arguments by converting them to empty strings, and this NULL-handling behavior is considered locale-dependent.

## Why It Matters
This is a hard blocker -- the migration cannot be applied at all. No workaround exists; the expression itself must be changed.

## Which Architecture Files Are Affected
- `02-foundation-db.md` (Section 3.2, the `watchpoints` table definition, lines ~145-156)

## Suggested Resolution
Replace `CONCAT(a, '.', b)` with `a::text || '.' || b::text`. The `||` operator for text concatenation is `IMMUTABLE` in PostgreSQL. The fix preserves identical semantics:

```sql
tile_id TEXT GENERATED ALWAYS AS (
    FLOOR((90.0 - location_lat) / 22.5)::int::text
    || '.'
    || FLOOR(
        CASE
            WHEN location_lon >= 0 THEN location_lon
            ELSE 360.0 + location_lon
        END / 45.0
    )::int::text
) STORED,
```

## Impact on Current Task
This was a blocker for the definition of done (`make migrate-up` must apply all migrations). The fix was applied directly to `migrations/003_watchpoints.up.sql` and verified to produce correct tile_id values.
