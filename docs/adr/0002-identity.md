# 2. Identity is opaque, and a hostname is not a key

Status: accepted, partly superseded

> Since November 2026, the value from a host always wins, and kryostate has no
> disagreements, findings or decisions. Read the [user guide](../user-guide.md) for the current behaviour.

## Context

Written inventories use the hostname as the key. A hostname is a bad key:

- A name can resolve to more than one address. A document that keeps one
  address keeps the first address from the resolver or a sort. This is not
  evidence of where to connect.
- A documented name can resolve in no zone. The host can be decommissioned,
  in a zone that is not configured, or the name can have a typing error.
- A naming convention can give an incorrect label. A decoder that reads an
  environment letter from a name can record a development host as
  production.
- A name has no site. A parser that finds a site in a name makes up data.

In each case, the record is incorrect but looks correct. This is worse than
no record, because nothing tells a person to examine it.

## Decision

**Identity is an opaque internal ID.** All other identifiers (hostname, FQDN,
serial number, machine ID, UUID) are evidence in the `identity` table. Each
has a scheme and a last-seen time.

`ResolveCI(kind, label, evidence)` tries the strongest evidence first:

| Result | Action |
|---|---|
| No match | Create a new configuration item. |
| One match | Use it, and record the evidence. |
| More than one match | Return `ErrAmbiguousIdentity` and stop. |

More than one match means a rename, a rebuild, or two hosts with the same
name. A merge deletes the history of one record. A third record makes the
problem permanent. Thus, a person must decide, and the import stops.

**A value from a hostname can suggest a role. It cannot set site,
environment or identity.** The `precedence` table has `allowed=0` for the
`derived` source on `env` and `site`. kryostate removes these claims before it
ranks. They cannot win, even if they are the only claim.

**Addresses are observations, not instructions.** kryostate records every A
record. An address becomes the management address only when a person selects
it with `SelectAddress`, which records the person. A name with more than one
address and no selection is a question for an operator.

## Consequences

- A rename adds an identity row to the same configuration item. It does not
  make a new host.
- If a rebuilt host keeps its name but has a new machine ID, a person resolves
  the ambiguity. kryostate does not merge the two histories.
- An import can stop on a row. This is intentional: an import that stops with
  a named problem is better than a finished import with an incorrect merge.
- To find a host by name, code must use the identity table.

## Alternatives considered

| Alternative | Reason for rejection |
|---|---|
| FQDN as the key | Hosts are renamed, move between zones, or are in no zone. |
| Serial number or machine ID as the key | It is unknown until a collection succeeds. The hosts that most need an identity are the hosts that kryostate cannot reach. |
| Automatic merge on a strong-evidence match | An incorrect merge cannot be reversed. |
