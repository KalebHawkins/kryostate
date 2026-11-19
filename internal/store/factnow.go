package store

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
)

// fact_now holds what the current_fact view would say, one row per CI,
// attribute and scope, so that reading the current value of something does
// not mean ranking every claim ever made about it.
//
// It is kept by triggers rather than by the Go code that writes claims.
// Whatever changes which claim wins, whether a new or confirmed claim, a
// retraction, a corrected normalisation, a failed or recovered reading, a
// precedence or source change, or a deleted host, changes a table these
// triggers watch, and the triggers recompute exactly the keys it can affect.
// That holds for every writer: this package, a restore, an older build of
// the tool opening the same file, and a person at the sqlite3 prompt.
//
// Ranking is the current_fact view's, restricted to the keys being
// recomputed. The view stays in the schema as the definition the projection
// is tested against.

// factNowVersion is bumped whenever the triggers or the ranking change, so
// that the next Open rebuilds the projection from the claims rather than
// trusting rows an older rule produced.
const factNowVersion = 1

// factNowCols are fact_now's columns, in the order factNowSelect yields them.
const factNowCols = `assertion_id, ci_id, attr, scope, raw_value, value_type, norm_value, family,
    normalizer_version, source_kind, source_name, observed_at, confirmed_at`

// factNowSelect ranks the live claims matching filter, an expression over
// the assertion aliased a, and yields the winner of each key. It is the
// current_fact view with a WHERE clause inside the window, which the view
// cannot take.
func factNowSelect(filter string) string {
	return `SELECT ` + factNowCols + ` FROM (
SELECT
    a.id AS assertion_id, a.ci_id AS ci_id, a.attr AS attr, a.scope AS scope,
    a.raw_value AS raw_value, a.value_type AS value_type,
    COALESCE(n.norm_value, '') AS norm_value,
    COALESCE(n.family, '') AS family,
    COALESCE(n.normalizer_version, 0) AS normalizer_version,
    s.kind AS source_kind, s.name AS source_name,
    a.observed_at AS observed_at,
    COALESCE(NULLIF(a.confirmed_at, ''), a.observed_at) AS confirmed_at,
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
WHERE (` + filter + `)
  AND a.retracted_at IS NULL
  AND COALESCE(ps.allowed, pd.allowed, 1) = 1
  AND NOT EXISTS (SELECT 1 FROM read_failure rf
                  WHERE rf.ci_id = a.ci_id AND rf.attr = a.attr AND rf.scope = a.scope
                    AND rf.source_id = a.source_id AND rf.cleared_at IS NULL)
) WHERE rn = 1`
}

// refreshKey recomputes one key. ref is OLD or NEW.
func refreshKey(ci, attr, scope string) string {
	return fmt.Sprintf(`DELETE FROM fact_now WHERE ci_id = %[1]s AND attr = %[2]s AND scope = %[3]s;
INSERT INTO fact_now (%[4]s) %[5]s;`, ci, attr, scope, factNowCols,
		factNowSelect(fmt.Sprintf("a.ci_id = %s AND a.attr = %s AND a.scope = %s", ci, attr, scope)))
}

func refreshAttr(attr string) string {
	return fmt.Sprintf(`DELETE FROM fact_now WHERE attr = %[1]s;
INSERT INTO fact_now (%[2]s) %[3]s;`, attr, factNowCols, factNowSelect("a.attr = "+attr))
}

func refreshAll() string {
	return `DELETE FROM fact_now;
INSERT INTO fact_now (` + factNowCols + `) ` + factNowSelect("1") + `;`
}

// factBump marks the projection changed, for caches of what is derived from
// every current value. A confirmation that leaves the value as it was does
// not bump it.
const factBump = `UPDATE projection SET gen = gen + 1 WHERE name = 'fact_now';`

// The columns of a claim that bear on which claim wins or on what fact_now
// holds for it. run_id, spec_id and received_at do not.
const assertionRankCols = `ci_id, attr, scope, raw_value, value_type, source_id, observed_at, confirmed_at, retracted_at`

// sameClaimExceptConfirmed is true when an update to a claim changed nothing
// but moved confirmed_at forward. The claim's freshness can then only have
// risen, so a claim that was winning still wins and only its row's
// confirmed_at needs writing; one that was not winning must be re-ranked.
const sameClaimExceptConfirmed = `NEW.ci_id IS OLD.ci_id AND NEW.attr IS OLD.attr AND NEW.scope IS OLD.scope
    AND NEW.raw_value IS OLD.raw_value AND NEW.value_type IS OLD.value_type
    AND NEW.source_id IS OLD.source_id AND NEW.observed_at IS OLD.observed_at
    AND NEW.retracted_at IS OLD.retracted_at
    AND COALESCE(NULLIF(NEW.confirmed_at, ''), NEW.observed_at) >= COALESCE(NULLIF(OLD.confirmed_at, ''), OLD.observed_at)
    AND EXISTS (SELECT 1 FROM fact_now WHERE assertion_id = NEW.id)`

const sameKey = `NEW.ci_id IS OLD.ci_id AND NEW.attr IS OLD.attr AND NEW.scope IS OLD.scope`

type trigger struct{ name, sql string }

// factNowTriggers are the triggers that keep fact_now, by name.
func factNowTriggers() []trigger {
	keyNew := refreshKey("NEW.ci_id", "NEW.attr", "NEW.scope")
	keyOld := refreshKey("OLD.ci_id", "OLD.attr", "OLD.scope")
	normSet := `UPDATE fact_now SET norm_value = NEW.norm_value, family = NEW.family,
    normalizer_version = NEW.normalizer_version WHERE assertion_id = NEW.assertion_id;`
	normClear := `UPDATE fact_now SET norm_value = '', family = '', normalizer_version = 0
    WHERE assertion_id = OLD.assertion_id;`
	t := func(name, when, body string) trigger {
		if body != "" {
			body += "\n"
		}
		return trigger{name, "CREATE TRIGGER " + name + " " + when + "\nBEGIN\n" + body + factBump + "\nEND"}
	}
	return []trigger{
		t("fact_now_assertion_insert", "AFTER INSERT ON assertion", keyNew),
		t("fact_now_assertion_delete", "AFTER DELETE ON assertion", keyOld),
		{"fact_now_assertion_confirm", `CREATE TRIGGER fact_now_assertion_confirm
AFTER UPDATE OF ` + assertionRankCols + ` ON assertion
WHEN ` + sameClaimExceptConfirmed + `
BEGIN
UPDATE fact_now SET confirmed_at = COALESCE(NULLIF(NEW.confirmed_at, ''), NEW.observed_at)
    WHERE assertion_id = NEW.id;
END`},
		t("fact_now_assertion_update", "AFTER UPDATE OF "+assertionRankCols+" ON assertion\nWHEN NOT ("+sameClaimExceptConfirmed+")", keyOld),
		t("fact_now_assertion_rekey", "AFTER UPDATE OF ci_id, attr, scope ON assertion\nWHEN NOT ("+sameKey+")", keyNew),

		t("fact_now_norm_insert", "AFTER INSERT ON assertion_norm", normSet),
		t("fact_now_norm_update", "AFTER UPDATE ON assertion_norm", normClear+"\n"+normSet),
		t("fact_now_norm_delete", "AFTER DELETE ON assertion_norm", normClear),

		t("fact_now_read_failure_insert", "AFTER INSERT ON read_failure", keyNew),
		t("fact_now_read_failure_update", "AFTER UPDATE OF ci_id, attr, scope, source_id, cleared_at ON read_failure", keyOld),
		t("fact_now_read_failure_rekey", "AFTER UPDATE OF ci_id, attr, scope ON read_failure\nWHEN NOT ("+sameKey+")", keyNew),
		t("fact_now_read_failure_delete", "AFTER DELETE ON read_failure", keyOld),

		// '*' is the default for every attribute, so changing it can move
		// any key; a named attribute moves only its own.
		t("fact_now_precedence_insert_all", "AFTER INSERT ON precedence WHEN NEW.attr = '*'", refreshAll()),
		t("fact_now_precedence_insert", "AFTER INSERT ON precedence WHEN NEW.attr <> '*'", refreshAttr("NEW.attr")),
		t("fact_now_precedence_update_all", "AFTER UPDATE ON precedence WHEN NEW.attr = '*' OR OLD.attr = '*'", refreshAll()),
		t("fact_now_precedence_update", "AFTER UPDATE ON precedence WHEN NEW.attr <> '*' AND OLD.attr <> '*'",
			refreshAttr("OLD.attr")+"\n"+refreshAttr("NEW.attr")),
		t("fact_now_precedence_delete_all", "AFTER DELETE ON precedence WHEN OLD.attr = '*'", refreshAll()),
		t("fact_now_precedence_delete", "AFTER DELETE ON precedence WHEN OLD.attr <> '*'", refreshAttr("OLD.attr")),

		// Sources are not renamed or removed in normal use; when one is, the
		// whole projection is rebuilt rather than worked out piecemeal.
		t("fact_now_source_update", "AFTER UPDATE OF id, kind, name ON source", refreshAll()),
		t("fact_now_source_delete", "AFTER DELETE ON source", refreshAll()),

		t("fact_now_ci_delete", "AFTER DELETE ON ci", "DELETE FROM fact_now WHERE ci_id = OLD.id;"),
		// Which CIs are hosts decides what is counted over fact_now, so a
		// new CI or a changed kind moves the generation too.
		t("fact_now_ci_insert", "AFTER INSERT ON ci", ""),
		t("fact_now_ci_kind", "AFTER UPDATE OF kind ON ci", ""),
	}
}

// migrateFactNow installs the triggers that keep fact_now and, when the
// projection is new or was built by different triggers, rebuilds it from the
// claims. It runs in one transaction, so another process writing at the same
// time either sees the old triggers or the new ones and never neither.
func (s *Store) migrateFactNow(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	existing := map[string]string{}
	rows, err := tx.QueryContext(ctx, `SELECT name, sql FROM sqlite_master WHERE type='trigger' AND name LIKE 'fact\_now\_%' ESCAPE '\'`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var name, text string
		if err := rows.Scan(&name, &text); err != nil {
			rows.Close()
			return err
		}
		existing[name] = text
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	want := map[string]bool{}
	changed := false
	for _, t := range factNowTriggers() {
		want[t.name] = true
		if existing[t.name] == t.sql {
			continue
		}
		changed = true
		if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+t.name); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, t.sql); err != nil {
			return fmt.Errorf("create trigger %s: %w", t.name, err)
		}
	}
	for name := range existing {
		if !want[name] {
			changed = true
			if _, err := tx.ExecContext(ctx, `DROP TRIGGER IF EXISTS `+name); err != nil {
				return err
			}
		}
	}

	var version int
	err = tx.QueryRowContext(ctx, `SELECT version FROM projection WHERE name='fact_now'`).Scan(&version)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if changed || version != factNowVersion {
		if err := rebuildFactNow(ctx, tx); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO projection (name, version, gen) VALUES ('fact_now', ?, 1)
			 ON CONFLICT(name) DO UPDATE SET version=excluded.version, gen=gen+1`, factNowVersion); err != nil {
			return err
		}
	}
	return tx.Commit()
}

type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func rebuildFactNow(ctx context.Context, x execer) error {
	for _, q := range strings.Split(strings.TrimSuffix(refreshAll(), ";"), ";\n") {
		if _, err := x.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("rebuild fact_now: %w", err)
		}
	}
	return nil
}

// RebuildFacts recomputes every current value from the claims. The triggers
// keep the projection correct as claims are written, so this is for after a
// bulk load, such as a restore, where rebuilding once is the cheaper and the
// more obviously complete way to be sure.
func (s *Store) RebuildFacts(ctx context.Context) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := rebuildFactNow(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, factBump); err != nil {
		return err
	}
	return tx.Commit()
}

// factGen is a number that changes whenever any current value does, in this
// process or another one, and whenever a CI is added or changes kind, for
// caching what is derived from all of them. Moving a confirmation stamp
// forward does not change it: nothing cached reads the stamp.
func (s *Store) factGen(ctx context.Context) (int64, error) {
	var gen int64
	err := s.db.QueryRowContext(ctx, `SELECT gen FROM projection WHERE name='fact_now'`).Scan(&gen)
	return gen, err
}
