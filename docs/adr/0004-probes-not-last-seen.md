# 4. There is no "Last Seen" column

Status: accepted

## Context

Most inventories have one "Last Seen" time for each host. It combines
different facts: ICMP answered, TCP 22 accepted, SSH or WinRM authentication
succeeded, a collection returned usable output, or a health endpoint was
healthy. A load balancer can answer ICMP while all nodes behind it are down,
and one value hides this. ICMP is also the signal that proves the least.

One value also loses the cause of a failure:

| Symptom | Possible cause |
|---|---|
| Port open, authentication failed | An expired credential |
| Authentication succeeded, collector failed | A missing command or a restricted shell |
| Nothing answers | A network problem or a decommissioned host |

## Decision

No host has a `last_seen` column. (The `identity` table records when kryostate
last saw each identifier. That is identity evidence, not reachability.)

Probes are discrete, append-only events:

    (ci, kind, outcome, detail, latency_ms, at)

| Field | Values |
|---|---|
| `kind` | `icmp`, `tcp`, `ssh_auth`, `winrm_auth`, `collector_run`, `resolve`, `health` |
| `outcome` | `ok`, `refused`, `timeout`, `auth_failed`, `nxdomain`, `no_answer`, `partial`, `error` |

`Reachability()` returns the latest probe of each kind, and the interface
shows each kind as a separate chip. A heartbeat that an operator defines is a
`health` probe. It has no special field and overwrites no other probe.

The buckets of a chart must add up to the number of hosts. A host with no
value goes into one of three buckets:

| Bucket | Meaning |
|---|---|
| never collected | No collection was tried. |
| collection failed | A collection was tried and did not work. |
| not understood | The host answered, and no rule parsed the answer. |

The buckets do not overlap. You can click each bucket to see its hosts.

## Consequences

- "Is this host up?" has no single answer, and the interface does not make
  one.
- Probe history can answer new questions, for example which hosts fail
  authentication intermittently.
- Rows grow with each probe. A `DELETE` by age removes old probes, because
  resolution does not use them.
- The hosts table shows more than one freshness signal, in more space.

## Alternatives considered

| Alternative | Reason for rejection |
|---|---|
| One `last_seen` column and a separate probe log | Code reads the column and ignores the log. The column becomes the answer, and it is incorrect. |
| A computed "status" field | Each rule assumes which probe is important. This is different for each host role. |
