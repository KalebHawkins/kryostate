# 4. There is no "Last Seen" column

Status: accepted

## Context

Every inventory has one. A single timestamp, sometimes a green dot, meaning
"we heard from this recently".

It answers a question nobody has. These are all different facts:

- ICMP answered.
- TCP 22 accepted a connection.
- SSH authentication succeeded.
- WinRM authentication succeeded.
- A collection ran and returned usable output.
- An application health endpoint reported itself healthy.

A load balancer answering ICMP while every node behind it is down is the exact
situation an inventory should surface, and a single "Last Seen" field averages
it away into a green dot. Worse: the most reassuring signal — ping — is the one
that proves the least.

The failure modes are also diagnostic and get lost. "The port is open but
authentication failed" means a credential expired. "Authentication succeeded
but the collector failed" means a command is missing or a shell is restricted.
"Nothing answers" means a network or a decommissioned host. One field cannot
hold any of this, so it holds none of it.

## Decision

There is no `last_seen` column anywhere in the schema.

Probes are discrete, append-only events:

    (ci, kind, outcome, detail, latency_ms, at)

`Kind` is one of the six above. `Outcome` is `ok`, `refused`, `timeout`,
`auth_failed`, `nxdomain`, `error`. `Reachability()` returns the latest probe
*of each kind*, and the interface shows them as separate chips.

A heartbeat defined by an operator — a status endpoint, an application check —
is a probe kind like any other. It does not get its own special field, and it
does not overwrite anyone else's.

The same reasoning drives how unknowns are counted. A chart's buckets must add
up to the number of hosts, and a host with no value appears in one of three
buckets with distinguishable causes:

- **never collected** — nobody has tried
- **collection failed** — somebody tried and it did not work
- **not understood** — the host answered and no rule parsed the answer

These are disjoint by construction and each is clickable through to the hosts
in it. A chart that quietly drops the hosts it has no data for is flattering
the estate, which is the specific dishonesty this tool exists to remove.

## Consequences

- "Is this host up?" has no single answer, and the interface declines to invent
  one. This is a feature and it will occasionally annoy someone.
- Probe history is the raw material for questions nobody has asked yet: which
  hosts fail authentication intermittently, which site degraded last Tuesday.
- Row counts grow with probes rather than with hosts. Pruning by age is a
  `DELETE`; nothing in the resolution path depends on old probes.
- The hosts table has to show several freshness signals instead of one column,
  which costs horizontal space and is worth it.

## Alternatives considered

**One `last_seen` plus a separate probe log.** The column gets read and the log
does not. Within a year the column is the de facto answer and is wrong.

**A computed "status" field.** Every rule for computing it encodes an
assumption about which probe matters, and that assumption differs by host role.
A load balancer and a database have different answers to "is this healthy".
