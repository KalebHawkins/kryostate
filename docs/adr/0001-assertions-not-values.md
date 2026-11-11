# 1. Store assertions, not current values

Status: accepted

## Context

The inventory this replaces is a set of Markdown tables: one row per host, one
column per attribute, each cell holding the current value. The tables were
generated once by a program that has since been deleted, then hand-edited for
two years. Nothing in them records where a value came from or when it was last
true.

That shape has a specific failure. When a cell is wrong there is no way to tell
whether a person typed it wrong, the generator misread something, or the host
changed. The only repair is to overwrite the cell, which destroys the evidence
that would have explained the error.

A table of current values cannot represent the most common real state, which is
*two sources disagree*. It has one cell, so one of them has to be discarded at
write time, by whoever wrote last.

## Decision

Nothing stores a current value. Every attribute is an append-only assertion:

    (ci, attr, scope, raw_value, source, observed_at)

A source has a kind — `document`, `collector`, `human`, `dns`, `derived` — and
these are different kinds of evidence, not different spellings of the same
thing. "The spreadsheet says RHEL 7" and "the machine answered RHEL 8" are both
true statements about different things.

The current value is a view, computed by ranking assertions per attribute:

    ranked_assertion → current_fact

Losing assertions are kept and remain queryable. `current_fact` is a view, not
a table, so no code path can write to it.

## Consequences

- Every value on screen can say who said so and when.
- Disagreement is representable. It is also detectable, which is the feature
  the whole tool is built around.
- Fixing a bad normaliser rule means re-running it over stored raw values. No
  host is contacted and nothing is lost.
- The store grows with observations rather than with hosts. An append-only
  table of a few hundred hosts collected nightly is a few million rows a year,
  which SQLite is comfortable with and which is cheap to prune by age if it
  ever stops being.
- Every read of a current value goes through a window function. This costs more
  than reading a column. On an estate of this size it is not close to
  mattering, and when it does the answer is a materialised view, not a mutable
  column.
- `Assert` does not insert a new row for a repeat of the same value from the
  same source, so nightly collection of an unchanged value does not bury real
  history. The repeat is recorded as a confirmation on the existing row, which
  keeps `observed_at` pointing at when the value was first seen.
- Because of that, two sources of equal rank are separated by whichever last
  stood behind its value, not by whichever first said it. Ranking on
  `observed_at` alone let a source that confirmed its value this morning lose
  to one that said something else a week ago.

## Alternatives considered

**Current-value columns with a separate history table.** The usual shape.
Rejected because the two can drift, and because the history table becomes
optional: code reads the column, nothing reads the history, and within a year
the history is wrong and nobody notices.

**Event sourcing with a rebuilt projection.** The same idea with more
machinery. A SQL view over an append-only table gets the same guarantees
without a rebuild step that can be skipped.
