# 2. Identity is opaque, and a hostname is not a key

Status: accepted

## Context

Written inventories key on the hostname, because it is the thing people say out
loud. It is a poor key, and it fails in several ways at once:

- A name can resolve to more than one address. A document that carries one of
  them records whichever the resolver or a sort function put first, which is
  not evidence about where to connect.
- A documented name can resolve in no zone at all. It may be decommissioned,
  it may be in a zone nobody configured, it may be a typo.
- A naming convention can mislabel a host. A decoder that reads an environment
  letter out of a name will file a development rig as production, with
  confidence and no evidence.
- A name carries no site. A parser that infers one is inventing data and
  attributing it to the machine.

Each of these is a case where a name-keyed record is confidently wrong. That is
worse than a record that is absent, because nothing prompts anyone to check it.

## Decision

**Identity is an opaque internal ID.** Every other identifier — hostname,
FQDN, serial number, machine ID, UUID — is *evidence* in an `identity` table,
with a scheme and a last-seen timestamp.

`ResolveCI(kind, label, evidence)` tries evidence strongest-first:

- No match: create a new configuration item.
- One match: use it, and record the evidence seen.
- **More than one match: return `ErrAmbiguousIdentity` and stop.**

The third case is the important one. Two records matching the evidence means
either a rename, a rebuild, or two machines with the same name. Merging them
destroys one record's history; creating a third makes the problem permanent.
Both are decisions a person should make, so the import stops and says so.

Two further rules follow from the same reasoning:

**Hostname-derived values may suggest a role; they may never establish site,
environment or identity.** The `derived` source is marked `allowed=0` for
`env` and `site` in the precedence table, so a derived claim about either is
filtered out *before* ranking. It cannot win even when it is the only claimant.
This is the third bullet of the Context written into the schema rather than
into a comment.

**Addresses are observations, never instructions.** Every A record is recorded.
None is a management endpoint until a person chooses one with `SelectAddress`,
which records who chose it. A name with several addresses and no choice is
raised as a discrepancy, because that is a question for an operator, not an
ordering problem for a sort function.

## Consequences

- A rename is a new identity row against the same configuration item, not a
  new host.
- A rebuilt machine keeping its name but changing its machine ID produces an
  ambiguity a person resolves, instead of a silent merge of two histories.
- Imports can halt on a row. That is intended: a stopped import with a named
  problem is better than a finished import with a wrong merge in it.
- There is no way to ask "the host called X" without going through the identity
  table. Slightly more code; it is the whole point.

## Alternatives considered

**FQDN as the key.** Better than a bare name, still wrong. Machines are
renamed, moved between zones, and exist in no zone at all.

**Serial number or machine ID as the key.** Stable, but unknown until the host
has been collected from successfully — and the records that most need identity
are the ones nothing can reach.

**Automatic merge on strong-evidence match.** Tempting, and it is exactly the
operation whose errors are unrecoverable.
