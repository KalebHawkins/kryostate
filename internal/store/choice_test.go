package store

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

func TestEnableChoiceNeverNarrows(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	for _, n := range []string{"a", "b"} {
		if _, err := st.EnsureStack(ctx, n); err != nil {
			t.Fatal(err)
		}
	}
	runs := func(tpl string) string {
		cs, _ := st.Choices(ctx)
		for _, c := range cs {
			if c.Template == tpl {
				return "[" + c.Target + "]"
			}
		}
		return "off"
	}
	steps := []struct {
		target  string
		changed bool
		want    string
	}{
		{`stack == "a"`, true, `[stack == "a"]`},
		{`stack == "a"`, false, `[stack == "a"]`},
		{`stack == "b"`, true, `[stack in ("a", "b")]`},
		{`stack in ("b", "a")`, false, `[stack in ("a", "b")]`},
		{`name == "web01"`, true, `[stack in ("a", "b") || name == "web01"]`},
		{`name == "web01"`, false, `[stack in ("a", "b") || name == "web01"]`},
		{"", true, "[]"},              // every host
		{`stack == "a"`, false, "[]"}, // still every host
	}
	for i, s := range steps {
		_, changed, err := st.EnableChoice(ctx, collect.Choice{Template: "cpu.cores", Target: s.target}, "t")
		if err != nil {
			t.Fatal(err)
		}
		if changed != s.changed || runs("cpu.cores") != s.want {
			t.Errorf("step %d: changed=%v runs=%s, want %v %s", i, changed, runs("cpu.cores"), s.changed, s.want)
		}
	}
}

func TestPutChoiceChecksTheTarget(t *testing.T) {
	st := testStore(t)
	ctx := context.Background()
	if _, err := st.EnsureStack(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ target, frag string }{
		{`name in ("web01"`, "column 17"},
		{`stack == "nope"`, "nope"},
		{`stack in ("a", "nope")`, "nope"},
		{`stack == "a"`, ""},
		{`stack == "nope" || name == "x"`, ""}, // not only stacks: allowed, it may simply match nothing yet
		{`  `, ""},
	} {
		_, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores", Target: c.target}, "t")
		if c.frag == "" && err != nil {
			t.Errorf("%q: %v", c.target, err)
		}
		if c.frag != "" && (err == nil || !strings.Contains(err.Error(), c.frag)) {
			t.Errorf("%q: got %v, want an error with %q", c.target, err, c.frag)
		}
	}
}

// TestStacksBecomeTargets: a database from before targets, with collectors
// limited by a comma list of stacks, opens with the same limits written as
// targets, and the old column emptied so there is one source of truth.
func TestStacksBecomeTargets(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "old.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, q := range []string{
		`INSERT INTO collector_choice (template, param, attr, stacks, created_at) VALUES ('cpu.cores', '', 'hw.cpu.cores', 'web, db,web', '')`,
		`INSERT INTO collector_choice (template, param, attr, stacks, created_at) VALUES ('timezone', '', 'timezone', 'only', '')`,
		`INSERT INTO collector_choice (template, param, attr, stacks, created_at) VALUES ('memory.total', '', 'hw.memory.total_mb', '', '')`,
	} {
		if _, err := st.DB().ExecContext(ctx, q); err != nil {
			t.Fatal(err)
		}
	}
	st.Close()

	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	got := map[string]string{}
	cs, err := st.Choices(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		got[c.Template] = c.Target
	}
	want := map[string]string{
		"cpu.cores":    `stack in ("web", "db")`,
		"timezone":     `stack == "only"`,
		"memory.total": ``,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: target %q, want %q", k, got[k], v)
		}
	}
	var left int
	if err := st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM collector_choice WHERE stacks<>''`).Scan(&left); err != nil || left != 0 {
		t.Errorf("%d rows still have stacks (%v)", left, err)
	}
}
