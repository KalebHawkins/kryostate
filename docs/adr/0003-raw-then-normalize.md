# 3. Collectors return bytes; normalisation is a separate versioned layer

Status: accepted

## Context

A collector that returns "RHEL 8.6" has already made a decision. It read
`/etc/os-release`, found `Red Hat Enterprise Linux 8.6 (Ootpa)`, and chose a
shorter form. The original is gone, the decision is invisible, and the rule
that made it lives in Go source.

That matters when the rule is wrong. AIX reports `7200-05-04-2114` from
`oslevel -s`: release 7.2, technology level 5, service pack 4, and a build
identifier of `2114`. The last field reads as a year and a week — 2021, week
14 — and the obvious misreading is to take the first two digits as a century,
or the whole thing as a date, or to assume a build identifier appears at all.
A rule that gets this wrong does so quietly, and every host running that
release carries the mistake. Fixing a normaliser that bakes its output into
the stored value means re-collecting from every host, which means sessions
into production to repair a parsing bug on the machine doing the parsing.

## Decision

**Collectors return raw bytes and nothing else.** A spec is a command, a
parser name, and an attribute to file the result under. The parsers — `first_line`,
`colon_kv`, `kv`, `regex` and so on — extract a substring. None of them
interpret it.

**Normalisation is a separate layer, applied at ingest, driven by rules in a
versioned table.** Each rule is a pattern, a target attribute, and a template
for the normalised value. A normalised result is stored *alongside* the raw
value in `assertion_norm`, keyed by assertion, recording `normalizer_version`
and the `rule_id` that matched.

The raw value is kept forever.

**The command's own output is kept too**, bounded and replaced each run. A
parser that took the wrong substring is a different bug from a normaliser that
read the substring wrongly, and storing only the extracted value would leave
the first one repairable solely by going back to the hosts. `Reparse()` re-runs
the current parse rules over stored output; `Renormalize()` re-runs the current
normaliser rules over stored raw values. Neither contacts anything.

When a reparse derives a different value, the earlier assertion is retracted
rather than outranked. It was this tool's misreading, not something the host
ever claimed, and leaving it in place would show the collector disagreeing with
itself — a disagreement the tool manufactured, which is worse than none. A
value a person chose while settling a disagreement is never overwritten this
way.

Each stored output is tied to the assertion it produced, and a repair replaces
only that one. The alternative — comparing against whichever claim currently
wins — looks equivalent and is not: it lets a reparse withdraw a document's
value or another collector's, and it skips the repair entirely whenever some
other source already happens to say the right thing, leaving the misreading in
the database.

An output with no such link has never produced a value: either its rule found
nothing, or it was stored before the link existed. Either way a repair treats
it as a recovery and asserts without retracting anything, which is the safe
reading — it adds a claim rather than removing one.

Fixing the AIX rule is a local operation over the database. No host is
contacted.

A value that no rule understood is stored with an empty normalised form, and
the interface shows it as a distinguishable kind of unknown: the host answered,
and this tool failed to understand the answer. That is a bug report, not a dead
host, and conflating the two means the bug is never fixed.

An empty rule pattern is rejected at load. `regexp.Compile("")` succeeds and
matches every input, so a rule missing its pattern would claim every value and
attribute them all to itself.

## Consequences

- The normaliser can be wrong without that being expensive. This is the
  property that makes it safe to ship rules that are merely good enough.
- Rules are data. Adding one for a product this project has never heard of
  needs no Go, no build, and no fork.
- Two values are stored per fact. The raw text of a few hundred hosts is
  nothing next to the ability to recompute.
- Command output is stored as well, which is a decision with a privacy cost.
  It is capped at 8 KB per host and collector spec, only the latest copy is
  kept, and there are commands to inspect, delete and refuse it. An estate that
  cannot accept it runs with `-no-evidence` and accepts a second pass instead.
- Charts group on the normalised value, so a missing rule shows up as an
  unknown bucket rather than as forty bars of height one. The exception is an
  attribute where *no* value normalises: that is indistinguishable from free
  text, so it is charted on raw values instead of becoming one bucket
  containing the whole estate.
- Output kept for repair is only ever output from a command that ran and
  exited zero. A rule replayed over an error message would publish a value
  that live collection had already rejected.
- Collectors cannot be "clever", and reviewers should reject it when they try.

## Alternatives considered

**Normalise in the collector.** Simpler, one value to store, and every parsing
bug becomes a re-collection of the estate.

**Normalise at read time only.** Keeps the raw as the only stored form, but
then every query runs regular expressions over every row, and nothing can be
grouped in SQL.

**Store normalised values and discard the raw once confident.** "Once
confident" never arrives, and this would quietly convert the recoverable case
into the unrecoverable one.
