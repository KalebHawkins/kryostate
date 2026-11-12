// Package transfer moves the whole store in and out of a single JSON
// document.
//
// This exists for two reasons. One is that an operator needs a way to take the
// data somewhere else without learning the schema. The other is that a dump
// that can be restored and dumped again to the same bytes is a check on the
// store that neither the author nor a reviewer has to be trusted for: if
// resolution, precedence or timestamps were lossy, the second dump would
// differ.
package transfer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// Format is bumped when the shape changes, so a dump taken by an older build
// is rejected rather than half-read.
const Format = 1

// Dump is the whole store, table by table.
type Dump struct {
	Format int              `json:"format"`
	Order  []string         `json:"order"`
	Counts map[string]int   `json:"counts"`
	Tables map[string][]Row `json:"tables"`
}

// Row is one record as column/value pairs. Values keep the types SQLite gave
// them so a restore writes back what was read.
type Row map[string]any

// tables lists every table in the order a restore must write them, which is
// the order foreign keys allow.
var tables = []string{
	"source", "ci", "identity", "assertion", "assertion_norm",
	"precedence", "fact_meta",
	"address", "conn_profile", "check_def", "evidence",
	"stack", "stack_member",
	"run", "probe", "audit", "collector_choice",
	"collector_schedule", "collector_state",
	"notify_dest", "notify_config", "notify_send",
}

// obsoleteTables were written by earlier builds and hold nothing this build
// reads: disagreements and the staleness budgets that raised them. A dump that
// carries them still restores, without them.
var obsoleteTables = map[string]bool{"discrepancy": true, "freshness_budget": true}

// obsoleteColumns are columns an older database still has and this build no
// longer reads. They are left out of an export and skipped on restore.
var obsoleteColumns = map[string]map[string]bool{
	"assertion": {"decided": true},
}

// Export reads every table into a dump.
func Export(ctx context.Context, st *store.Store) (*Dump, error) {
	d := &Dump{Format: Format, Tables: map[string][]Row{}, Order: tables, Counts: map[string]int{}}
	for _, t := range tables {
		rows, err := readTable(ctx, st.DB(), t)
		if err != nil {
			return nil, fmt.Errorf("read %s: %w", t, err)
		}
		d.Tables[t] = rows
		d.Counts[t] = len(rows)
	}
	return d, nil
}

// Write encodes a dump. Indentation is on because the file is meant to be read
// by a person who wants to know whether their data is in there.
func (d *Dump) Write(w io.Writer) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	enc.SetEscapeHTML(false)
	return enc.Encode(d)
}

// Read decodes a dump and refuses a format it does not know.
func Read(r io.Reader) (*Dump, error) {
	var d Dump
	dec := json.NewDecoder(r)
	dec.UseNumber()
	if err := dec.Decode(&d); err != nil {
		return nil, err
	}
	if d.Format != Format {
		return nil, fmt.Errorf("dump format %d, this build reads %d", d.Format, Format)
	}
	return &d, nil
}

// Restore writes a dump into an empty store.
//
// It refuses a store that already holds configuration items. Merging two
// inventories is a decision with consequences for identity, and it is not one
// a restore command should make quietly.
func Restore(ctx context.Context, st *store.Store, d *Dump) error {
	var n int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM ci`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("store already holds %d configuration items; restore into an empty one", n)
	}

	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Policy rows are seeded when the store is opened and would collide with
	// the dump's own, so the seeded ones give way to what was exported.
	if _, err := tx.ExecContext(ctx, `DELETE FROM precedence`); err != nil {
		return err
	}

	// The dump's own "order" is not trusted to name tables: it would put a
	// string from the file into the SQL text. The write order is this
	// package's, and anything the file names that this build does not know is
	// an error rather than something to skip quietly.
	for t := range d.Tables {
		if !known(t) && !obsoleteTables[t] {
			return fmt.Errorf("dump holds unknown table %q", t)
		}
	}
	for _, t := range tables {
		allowed, err := columnSet(ctx, tx, t)
		if err != nil {
			return err
		}
		for _, row := range d.Tables[t] {
			cols := make([]string, 0, len(row))
			for c := range row {
				if obsoleteColumns[t][c] {
					continue
				}
				if !allowed[c] {
					return fmt.Errorf("restore %s: unknown column %q", t, c)
				}
				cols = append(cols, c)
			}
			if len(cols) == 0 {
				return fmt.Errorf("restore %s: row with no columns", t)
			}
			sort.Strings(cols)
			ph := make([]string, len(cols))
			args := make([]any, len(cols))
			quoted := make([]string, len(cols))
			for i, c := range cols {
				ph[i] = "?"
				quoted[i] = `"` + c + `"`
				args[i] = jsonValue(row[c])
			}
			q := fmt.Sprintf(`INSERT INTO %s (%s) VALUES (%s)`,
				t, strings.Join(quoted, ", "), strings.Join(ph, ", "))
			if _, err := tx.ExecContext(ctx, q, args...); err != nil {
				return fmt.Errorf("restore %s: %w", t, err)
			}
		}
	}
	return tx.Commit()
}

func known(table string) bool {
	for _, t := range tables {
		if t == table {
			return true
		}
	}
	return false
}

// columnSet reads the columns a table actually has, so a dump cannot name a
// column that only exists in the SQL text it would produce.
func columnSet(ctx context.Context, tx *sql.Tx, table string) (map[string]bool, error) {
	rows, err := tx.QueryContext(ctx, `SELECT name FROM pragma_table_info(?)`, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out[name] = true
	}
	return out, rows.Err()
}

func readTable(ctx context.Context, db *sql.DB, table string) ([]Row, error) {
	rows, err := db.QueryContext(ctx, `SELECT * FROM `+table+` ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return nil, err
	}
	out := []Row{}
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		r := Row{}
		for i, c := range cols {
			if obsoleteColumns[table][c] {
				continue
			}
			if b, ok := vals[i].([]byte); ok {
				r[c] = string(b)
				continue
			}
			r[c] = vals[i]
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// jsonValue converts decoded JSON back to something the driver accepts. The
// decoder runs in number mode so an integer primary key does not come back as
// a float and restore as "3.0".
func jsonValue(v any) any {
	n, ok := v.(json.Number)
	if !ok {
		return v
	}
	if i, err := n.Int64(); err == nil {
		return i
	}
	f, err := n.Float64()
	if err != nil {
		return n.String()
	}
	return f
}
