PRAGMA foreign_keys = ON;

-- A source is a named origin of claims. Precedence lives in the precedence
-- table keyed by attribute, not as a column here, because the same source is
-- authoritative for some attributes and barred from others.
CREATE TABLE IF NOT EXISTS source (
    id          INTEGER PRIMARY KEY,
    kind        TEXT NOT NULL,
    name        TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    UNIQUE (kind, name)
);

-- ci holds identity only. It deliberately carries no os_version, no site and
-- no last_seen: those are claims, and claims live in assertion.
CREATE TABLE IF NOT EXISTS ci (
    id          TEXT PRIMARY KEY,
    kind        TEXT NOT NULL,
    label       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    retired_at  TEXT
);
CREATE INDEX IF NOT EXISTS ci_label_idx ON ci(label);

-- Evidence that a CI is the one we think it is. Not unique: hostnames repeat
-- across sites and a cloned machine carries its source machine-id, so a
-- collision is a prompt for review rather than an automatic merge.
CREATE TABLE IF NOT EXISTS identity (
    id          INTEGER PRIMARY KEY,
    ci_id       TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    scheme      TEXT NOT NULL,
    value       TEXT NOT NULL,
    first_seen  TEXT NOT NULL,
    last_seen   TEXT NOT NULL,
    valid_to    TEXT
);
CREATE INDEX IF NOT EXISTS identity_lookup_idx ON identity(scheme, value);
CREATE INDEX IF NOT EXISTS identity_ci_idx ON identity(ci_id);

-- One claim, by one source, at one time. Append-only: superseding a value
-- means inserting a newer row, never updating this one. retracted_at marks a
-- claim withdrawn by its own source and still excludes it from resolution
-- without destroying the record that it was once made.
CREATE TABLE IF NOT EXISTS assertion (
    id            INTEGER PRIMARY KEY,
    ci_id         TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    attr          TEXT NOT NULL,
    raw_value     TEXT NOT NULL,
    value_type    TEXT NOT NULL DEFAULT 'text',
    source_id     INTEGER NOT NULL REFERENCES source(id),
    scope         TEXT NOT NULL DEFAULT '',
    run_id        TEXT NOT NULL DEFAULT '',
    observed_at   TEXT NOT NULL,
    received_at   TEXT NOT NULL,
    -- Updated every time a source repeats this same value. observed_at is
    -- when the claim was first made; confirmed_at is the last time anyone
    -- stood behind it, which is the question staleness actually asks.
    confirmed_at  TEXT NOT NULL DEFAULT '',
    retracted_at  TEXT
);
CREATE INDEX IF NOT EXISTS assertion_ci_attr_idx ON assertion(ci_id, attr, scope);
CREATE INDEX IF NOT EXISTS assertion_attr_idx ON assertion(attr);
CREATE INDEX IF NOT EXISTS assertion_run_idx ON assertion(run_id);

-- The normalised projection of an assertion. Separate table so a bad rule can
-- be corrected by re-running the normaliser over stored raw values, with no
-- need to reach a host that may no longer exist. normalizer_version is the
-- undo handle.
CREATE TABLE IF NOT EXISTS assertion_norm (
    assertion_id       INTEGER PRIMARY KEY REFERENCES assertion(id) ON DELETE CASCADE,
    norm_value         TEXT NOT NULL,
    family             TEXT NOT NULL DEFAULT '',
    major              TEXT NOT NULL DEFAULT '',
    minor              TEXT NOT NULL DEFAULT '',
    patch              TEXT NOT NULL DEFAULT '',
    rule_id            TEXT NOT NULL DEFAULT '',
    normalizer_version INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS assertion_norm_value_idx ON assertion_norm(norm_value);

-- Per-attribute precedence. attr '*' supplies the default. allowed = 0 bars a
-- source kind from an attribute entirely, which is how a hostname decoder is
-- prevented from ever establishing site or environment.
CREATE TABLE IF NOT EXISTS precedence (
    attr        TEXT NOT NULL,
    source_kind TEXT NOT NULL,
    rank        INTEGER NOT NULL,
    allowed     INTEGER NOT NULL DEFAULT 1,
    PRIMARY KEY (attr, source_kind)
);

-- Probes are discrete events, not a rolled-up timestamp. A failure row is as
-- valuable as a success row and is never overwritten by one.
CREATE TABLE IF NOT EXISTS probe (
    id       INTEGER PRIMARY KEY,
    ci_id    TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    kind     TEXT NOT NULL,
    outcome  TEXT NOT NULL,
    target   TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT '',
    at       TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS probe_ci_kind_idx ON probe(ci_id, kind, at DESC);

-- A liveness check an operator defined. Separate from conn_profile because
-- "can this answer on its service port" and "can this tool log in and read
-- files" are different questions with different answers, and a host that fails
-- one and passes the other is the case worth seeing.
--
-- kind is 'tcp' or 'http'. For tcp, target is host:port. For http, target is a
-- URL and expect is the status code, or a run of them as in '2xx'.
CREATE TABLE IF NOT EXISTS check_def (
    id         INTEGER PRIMARY KEY,
    ci_id      TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    kind       TEXT NOT NULL,
    target     TEXT NOT NULL,
    expect     TEXT NOT NULL DEFAULT '',
    timeout_ms INTEGER NOT NULL DEFAULT 5000,
    enabled    INTEGER NOT NULL DEFAULT 1,
    created_at TEXT NOT NULL,
    UNIQUE (ci_id, name)
);

-- What a collector command actually printed, kept so that a parse rule found
-- to be wrong can be corrected against stored text instead of by connecting to
-- every host again. Only the most recent output per host and spec is kept:
-- the point is repair, not an audit trail of identical outputs.
CREATE TABLE IF NOT EXISTS evidence (
    id        INTEGER PRIMARY KEY,
    ci_id     TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    spec_id   TEXT NOT NULL,
    attr      TEXT NOT NULL,
    scope     TEXT NOT NULL DEFAULT '',
    command   TEXT NOT NULL DEFAULT '',
    output    TEXT NOT NULL DEFAULT '',
    truncated INTEGER NOT NULL DEFAULT 0,
    -- The assertion this output produced, so a repair replaces its own
    -- derivation rather than whichever claim currently happens to win.
    assertion_id INTEGER,
    run_id    TEXT NOT NULL DEFAULT '',
    at        TEXT NOT NULL,
    UNIQUE (ci_id, spec_id, scope)
);
CREATE INDEX IF NOT EXISTS evidence_attr ON evidence (attr);
-- is a deliberate human decision about where to connect; it is never inferred
-- from sort order.
CREATE TABLE IF NOT EXISTS address (
    id          INTEGER PRIMARY KEY,
    ci_id       TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    addr        TEXT NOT NULL,
    family      TEXT NOT NULL DEFAULT 'ipv4',
    resolver    TEXT NOT NULL DEFAULT '',
    observed_at TEXT NOT NULL,
    selected    INTEGER NOT NULL DEFAULT 0,
    selected_by TEXT NOT NULL DEFAULT '',
    UNIQUE (ci_id, addr)
);

-- Where and how to reach a CI, and which credential to use. The credential is
-- a reference resolved at run time by a secret provider; no secret material is
-- ever stored in this database.
CREATE TABLE IF NOT EXISTS conn_profile (
    id              INTEGER PRIMARY KEY,
    ci_id           TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    transport       TEXT NOT NULL,
    address         TEXT NOT NULL,
    port            INTEGER NOT NULL,
    username        TEXT NOT NULL DEFAULT '',
    credential_ref  TEXT NOT NULL DEFAULT '',
    bastion_ref     TEXT NOT NULL DEFAULT '',
    host_key        TEXT NOT NULL DEFAULT '',
    bastion_host_key TEXT NOT NULL DEFAULT '',
    -- 'pinned' accepts an unrecorded key once and records it; 'strict'
    -- refuses to connect until a key has been recorded by other means.
    host_key_policy TEXT NOT NULL DEFAULT 'pinned',
    enabled         INTEGER NOT NULL DEFAULT 1,
    created_at      TEXT NOT NULL,
    UNIQUE (ci_id, transport)
);

CREATE TABLE IF NOT EXISTS stack (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL DEFAULT '',
    environment TEXT NOT NULL DEFAULT ''
);

-- Many-to-many on purpose: a shared database host serves more than one stack.
CREATE TABLE IF NOT EXISTS stack_member (
    stack_id INTEGER NOT NULL REFERENCES stack(id) ON DELETE CASCADE,
    ci_id    TEXT NOT NULL REFERENCES ci(id) ON DELETE CASCADE,
    role     TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (stack_id, ci_id)
);

-- A collection run. Recorded whether or not it succeeded, so that a partial
-- run cannot be mistaken for a complete enumeration.
CREATE TABLE IF NOT EXISTS run (
    id          TEXT PRIMARY KEY,
    collector   TEXT NOT NULL,
    started_at  TEXT NOT NULL,
    finished_at TEXT,
    status      TEXT NOT NULL DEFAULT 'running',
    targets     INTEGER NOT NULL DEFAULT 0,
    succeeded   INTEGER NOT NULL DEFAULT 0,
    failed      INTEGER NOT NULL DEFAULT 0,
    note        TEXT NOT NULL DEFAULT ''
);

-- Who did what. Written for credential use and for every change a person
-- makes, and never for secret material or unbounded raw output.
CREATE TABLE IF NOT EXISTS audit (
    id       INTEGER PRIMARY KEY,
    at       TEXT NOT NULL,
    actor    TEXT NOT NULL,
    action   TEXT NOT NULL,
    subject  TEXT NOT NULL DEFAULT '',
    detail   TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS audit_at_idx ON audit(at DESC);

-- How a fact key should be presented, learned from how values behave across
-- the estate and overridable in one row. Not a schema editor: the override is
-- offered beside the fact itself.
CREATE TABLE IF NOT EXISTS fact_meta (
    attr          TEXT PRIMARY KEY,
    label         TEXT NOT NULL DEFAULT '',
    value_type    TEXT NOT NULL DEFAULT 'text',
    chartable     INTEGER,          -- NULL means infer from cardinality
    distinct_vals INTEGER NOT NULL DEFAULT 0,
    ci_count      INTEGER NOT NULL DEFAULT 0,
    updated_at    TEXT NOT NULL DEFAULT ''
);

-- A reading that failed on a host that answered: the command ran and exited
-- non-zero, or printed nothing the parse rule could use. While an episode is
-- open (cleared_at NULL) that source's earlier value for the attribute is not
-- current: the host was asked again and could not say, so the honest answer
-- is unknown rather than an old value. The next good reading from the same
-- source closes the episode. Closed episodes are kept so that a host can be
-- shown as it was on a past day, unknowns included.
CREATE TABLE IF NOT EXISTS read_failure (
    id          INTEGER PRIMARY KEY,
    ci_id       TEXT NOT NULL REFERENCES ci(id),
    attr        TEXT NOT NULL,
    scope       TEXT NOT NULL DEFAULT '',
    source_id   INTEGER NOT NULL REFERENCES source(id),
    spec_id     TEXT NOT NULL DEFAULT '',
    error       TEXT NOT NULL DEFAULT '',
    since       TEXT NOT NULL,
    last_at     TEXT NOT NULL,
    cleared_at  TEXT
);
CREATE INDEX IF NOT EXISTS read_failure_key ON read_failure (ci_id, attr, scope, source_id, cleared_at);

-- Resolution. One row per (ci, attr, scope): the winning assertion under the
-- configured precedence, most recent first within a rank. Sources barred from
-- an attribute are filtered out before ranking rather than losing on rank, so
-- a barred source can never win by being the only claimant. What a host
-- reports sorts ahead of every other kind of source, whatever the precedence
-- table says: a collected value is the truth for that attribute.
DROP VIEW IF EXISTS current_fact;
DROP VIEW IF EXISTS ranked_assertion;
CREATE VIEW ranked_assertion AS
SELECT
    a.id            AS assertion_id,
    a.ci_id         AS ci_id,
    a.attr          AS attr,
    a.scope         AS scope,
    a.raw_value     AS raw_value,
    a.value_type    AS value_type,
    a.observed_at   AS observed_at,
    COALESCE(NULLIF(a.confirmed_at, ''), a.observed_at) AS confirmed_at,
    a.source_id     AS source_id,
    s.kind          AS source_kind,
    s.name          AS source_name,
    COALESCE(n.norm_value, '')             AS norm_value,
    COALESCE(n.family, '')                 AS family,
    COALESCE(n.normalizer_version, 0)      AS normalizer_version,
    COALESCE(ps.rank, pd.rank, 500)        AS rank,
    -- Equal-ranked sources are separated by when each last stood behind its
    -- value, not by when it first said it. A source that confirmed the same
    -- value this morning is more current than one that said something else
    -- last week, and ordering on observed_at alone would say otherwise.
    ROW_NUMBER() OVER (
        PARTITION BY a.ci_id, a.attr, a.scope
        ORDER BY CASE WHEN s.kind = 'collector' THEN 0 ELSE 1 END,
                 COALESCE(ps.rank, pd.rank, 500) ASC,
                 MAX(a.observed_at, COALESCE(NULLIF(a.confirmed_at, ''), a.observed_at)) DESC,
                 a.observed_at DESC, a.id DESC
    ) AS rn
FROM assertion a
JOIN source s ON s.id = a.source_id
LEFT JOIN assertion_norm n ON n.assertion_id = a.id
LEFT JOIN precedence ps ON ps.attr = a.attr AND ps.source_kind = s.kind
LEFT JOIN precedence pd ON pd.attr = '*'    AND pd.source_kind = s.kind
WHERE a.retracted_at IS NULL
  AND COALESCE(ps.allowed, pd.allowed, 1) = 1
  AND NOT EXISTS (SELECT 1 FROM read_failure rf
                  WHERE rf.ci_id = a.ci_id AND rf.attr = a.attr AND rf.scope = a.scope
                    AND rf.source_id = a.source_id AND rf.cleared_at IS NULL);

CREATE VIEW current_fact AS
SELECT assertion_id, ci_id, attr, scope, raw_value, value_type,
       norm_value, family, normalizer_version, source_kind, source_name,
       observed_at, confirmed_at
FROM ranked_assertion
WHERE rn = 1;

-- A catalog collector a person switched on. The command itself is not stored:
-- it is rendered from the catalog compiled into the binary, so a row here can
-- only ever select from commands that shipped with the tool. param is the one
-- value a template takes, checked again every time it is rendered. target is
-- a query in the inventory's language choosing the hosts it runs on; empty
-- means every host. stacks is the comma-separated list of stack names older
-- versions limited a collector to. It is turned into a target when the
-- database is opened and left empty from then on.
CREATE TABLE IF NOT EXISTS collector_choice (
    id         INTEGER PRIMARY KEY,
    template   TEXT NOT NULL,
    param      TEXT NOT NULL DEFAULT '',
    attr       TEXT NOT NULL,
    stacks     TEXT NOT NULL DEFAULT '',
    target     TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL DEFAULT '',
    created_at TEXT NOT NULL,
    UNIQUE (template, param)
);

-- When a collector runs on its own. A row exists only where somebody chose a
-- schedule; a collector without one runs on the default for its kind, which
-- lives in code so that a better default reaches every estate that never
-- chose. every_sec = 0 means manual only. at_time is HH:MM local time and is
-- only used with whole-day intervals.
CREATE TABLE IF NOT EXISTS collector_schedule (
    collector  TEXT PRIMARY KEY,
    every_sec  INTEGER NOT NULL,
    at_time    TEXT NOT NULL DEFAULT '',
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TEXT NOT NULL
);

-- What happened the last time each collector ran, scheduled or not. Kept
-- apart from collector_schedule so that a collector on its default schedule
-- still has a history.
CREATE TABLE IF NOT EXISTS collector_state (
    collector     TEXT PRIMARY KEY,
    last_started  TEXT NOT NULL DEFAULT '',
    last_finished TEXT NOT NULL DEFAULT '',
    last_outcome  TEXT NOT NULL DEFAULT '',
    last_detail   TEXT NOT NULL DEFAULT '',
    last_run_id   TEXT NOT NULL DEFAULT ''
);

-- A rule tracks a rollout: the hosts its scope query chooses should match its
-- expect query. Both are queries in the inventory's language, evaluated in Go
-- over current values; neither is ever turned into SQL. Blank scope means
-- every host.
CREATE TABLE IF NOT EXISTS rule (
    id          INTEGER PRIMARY KEY,
    name        TEXT NOT NULL UNIQUE,
    description TEXT NOT NULL DEFAULT '',
    owner       TEXT NOT NULL DEFAULT '',
    scope       TEXT NOT NULL DEFAULT '',
    expect      TEXT NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TEXT NOT NULL,
    updated_at  TEXT NOT NULL
);

-- How a rule's counts moved: a row whenever they change, and otherwise at
-- most one a day, taken when a collection run finishes. changed marks rows
-- whose counts differ from the row before.
CREATE TABLE IF NOT EXISTS rule_snapshot (
    id       INTEGER PRIMARY KEY,
    rule_id  INTEGER NOT NULL REFERENCES rule(id) ON DELETE CASCADE,
    at       TEXT NOT NULL,
    in_scope INTEGER NOT NULL,
    done     INTEGER NOT NULL,
    not_yet  INTEGER NOT NULL,
    unknown  INTEGER NOT NULL,
    changed  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS rule_snapshot_rule_at_idx ON rule_snapshot(rule_id, at);
