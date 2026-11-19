# 1. Store assertions, not current values

Status: accepted, partly superseded

> Since November 2026, the value from a host always wins, and kryostate has no
> disagreements, findings or decisions. Read the [user guide](../user-guide.md) for the current behaviour.

## Context

A usual inventory is a table with one row for each host and one cell for each
value. The table does not record the source of a value or when it was true.
If a cell is incorrect, you cannot find the cause: a person, a generator or a
change on the host. The only repair is to overwrite the cell, and this deletes
the evidence.

A table of current values also cannot show that two sources disagree. It has
one cell, so the last writer deletes the other value.

## Decision

kryostate stores no current value. Each value is an append-only assertion:

    (ci, attr, scope, raw_value, source, observed_at)

A source has a kind: `document`, `collector`, `human`, `dns` or `derived`.
Each kind is a different type of evidence.

The current value is a view. kryostate ranks the assertions for each
attribute:

    ranked_assertion → current_fact

kryostate keeps the assertions that lose, and you can query them.
`current_fact` is a view, not a table, so no code can write to it.

## Consequences

- Each value on the screen can show its source and its time.
- kryostate can show and find a disagreement between sources.
- To correct a bad normaliser rule, kryostate runs the rule again on the
  stored raw values. It does not connect to a host, and no data is lost.
- The store grows with each observation. For a few hundred hosts collected
  each night, this is a few million rows each year. SQLite can hold this, and
  you can delete old rows by age.
- Each read of a current value uses a window function. For a few hundred
  hosts, the cost is too small to measure. If the cost increases, use a
  materialised view, not a mutable column.
- `Assert` does not add a row if the same source sends the same value again.
  It records a confirmation on the existing row. Thus, `observed_at` stays at
  the time when the value was first seen.
- For this reason, if two sources have the same rank, the source that
  confirmed its value last wins.

## Alternatives considered

| Alternative | Reason for rejection |
|---|---|
| Current-value columns and a separate history table | The two can drift apart. Code reads only the column, and the history becomes incorrect. |
| Event sourcing with a rebuilt projection | A SQL view on an append-only table gives the same result without a rebuild step that someone can skip. |
