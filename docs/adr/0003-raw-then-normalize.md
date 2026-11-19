# 3. Collectors return bytes; normalisation is a separate versioned layer

Status: accepted, partly superseded

> Since November 2026, the value from a host always wins, and kryostate has no
> disagreements, findings or decisions. Read the [user guide](../user-guide.md) for the current behaviour.

## Context

If a collector returns "RHEL 8.6", it has already made a decision. It read
`Red Hat Enterprise Linux 8.6 (Ootpa)` from `/etc/os-release` and made it
shorter. The original value is lost, and the rule is in Go source.

The rule can be incorrect. AIX `oslevel -s` returns `7200-05-04-2114`:
release 7.2, technology level 5, service pack 4, build `2114`. A rule can
easily read the build as a date. If only the result is stored, the repair is
a new collection from every host.

## Decision

**Collectors return raw bytes only.** A spec is a command, a parser name and
an attribute. The parsers (`first_line`, `colon_kv`, `kv`, `regex` and others)
extract a substring. They do not interpret it.

**Normalisation is a separate layer.** kryostate applies it at ingest, with
rules from a versioned table. Each rule has a pattern, a target attribute and
a template for the normalised value. kryostate stores the normalised result
in `assertion_norm`, next to the raw value. Each row records the
`normalizer_version` and the `rule_id` that matched. kryostate keeps the raw
value permanently.

**kryostate also keeps the command output.** It has a size limit, and each run
replaces it. Two commands repair the data without a connection to a host:

| Command | Runs again on |
|---|---|
| `Reparse()` | The stored command output, with the current parse rules |
| `Renormalize()` | The stored raw values, with the current normaliser rules |

Thus, you can correct the AIX rule in the database. No host is contacted.

Rules for a reparse:

- If it gets a different value, it retracts the earlier assertion. That value
  was a misreading by kryostate, not a claim by the host.
- It never overwrites a value that a person selected to resolve a
  disagreement.
- It replaces only the assertion that is linked to the stored output. It does
  not compare with the winning value, because that can remove a value from a
  document or a different collector.
- An output with no link made no value. The repair adds a value and retracts
  nothing.

If no rule understands a value, the normalised form is empty. The interface
shows this as "not understood": a bug in a rule, not a host that is down.

kryostate rejects an empty rule pattern at load. `regexp.Compile("")` matches
all input, so the rule would claim every value.

## Consequences

- An incorrect normaliser rule is not expensive, so rules can be incomplete.
- Rules are data. A new rule needs no Go, no build and no fork.
- kryostate stores two values for each fact. For a few hundred hosts, this is
  small.
- Stored output has a privacy cost. kryostate keeps only the latest 8 KB for
  each host and collector spec. Commands show, delete and refuse it. With
  `--no-evidence`, you must collect again after you correct a parse rule.
- Charts group on the normalised value, so a missing rule shows as an unknown
  bucket. If no value of an attribute normalises, the chart uses the raw
  values.
- kryostate keeps output for repair only from a command that ran and exited
  with zero. Thus, a rule never runs on an error message.
- Collectors must not interpret values. Reviewers must reject a collector that
  does.

## Alternatives considered

| Alternative | Reason for rejection |
|---|---|
| Normalise in the collector | Each parse bug needs a new collection from all hosts. |
| Normalise at read time only | Each query runs regular expressions on every row, and SQL cannot group the values. |
| Store normalised values and delete the raw value later | The raw value is lost, and a recoverable error becomes permanent. |
