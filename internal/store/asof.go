package store

import (
	"context"
	"database/sql"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// factsAtSQL is the ranked_assertion view with a clock in it. The ordering is
// the same rule set (what a host reports first, then precedence rank, then
// the freshest claim) applied only to assertions already made at the time
// asked about. Only the latest confirmation of a claim is kept, so when that
// is later than the time asked about there is no telling when the claim was
// last confirmed before it; it then counts as fresh from when it was first
// made, the one moment it is known to have been said by then. Asked about
// the present, this is exactly current_fact.
//
// Retracted assertions stay out, whenever they were retracted: a retraction
// marks a reading this tool extracted wrongly, which was never what the
// source said, so it is not shown as having been true.
const factsAtSQL = `
WITH claim AS (
SELECT
    a.id AS assertion_id, a.ci_id, a.attr, a.scope, a.raw_value, a.value_type,
    COALESCE(n.norm_value, '') AS norm_value,
    s.kind AS source_kind, s.name AS source_name,
    a.observed_at,
    CASE WHEN COALESCE(NULLIF(a.confirmed_at, ''), a.observed_at) <= :at
         THEN MAX(a.observed_at, COALESCE(NULLIF(a.confirmed_at, ''), a.observed_at))
         ELSE a.observed_at END AS confirmed_at,
    COALESCE(n.normalizer_version, 0) AS normalizer_version,
    COALESCE(ps.rank, pd.rank, 500) AS rank
FROM assertion a
JOIN source s ON s.id = a.source_id
LEFT JOIN assertion_norm n ON n.assertion_id = a.id
LEFT JOIN precedence ps ON ps.attr = a.attr AND ps.source_kind = s.kind
LEFT JOIN precedence pd ON pd.attr = '*'    AND pd.source_kind = s.kind
WHERE a.ci_id = :ci
  AND a.observed_at <= :at
  AND a.retracted_at IS NULL
  AND COALESCE(ps.allowed, pd.allowed, 1) = 1
), ranked AS (
SELECT claim.*, ROW_NUMBER() OVER (
        PARTITION BY ci_id, attr, scope
        ORDER BY CASE WHEN source_kind = 'collector' THEN 0 ELSE 1 END,
                 rank ASC, confirmed_at DESC, observed_at DESC, assertion_id DESC
    ) AS rn
FROM claim
)
SELECT assertion_id, ci_id, attr, scope, raw_value, value_type, norm_value,
       source_kind, source_name, observed_at, confirmed_at, normalizer_version
FROM ranked WHERE rn = 1 ORDER BY attr, scope`

// FactsAt is Facts as it stood at a moment in the past: for each attribute,
// the value that won among the claims made by then. An attribute nobody had
// reported by that time is absent.
func (s *Store) FactsAt(ctx context.Context, ciID string, at time.Time) ([]model.Fact, error) {
	rows, err := s.db.QueryContext(ctx, factsAtSQL,
		sql.Named("ci", ciID), sql.Named("at", ts(at)))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Fact
	for rows.Next() {
		f, err := scanFact(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
