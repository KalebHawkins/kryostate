package store

import (
	"context"
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
				return "[" + strings.Join(c.Stacks, ",") + "]"
			}
		}
		return "off"
	}
	steps := []struct {
		stacks  []string
		changed bool
		want    string
	}{
		{[]string{"a"}, true, "[a]"},
		{[]string{"a"}, false, "[a]"},
		{[]string{"b"}, true, "[a,b]"},
		{nil, true, "[]"},            // every host
		{[]string{"a"}, false, "[]"}, // still every host
	}
	for i, s := range steps {
		_, changed, err := st.EnableChoice(ctx, collect.Choice{Template: "cpu.cores", Stacks: s.stacks}, "t")
		if err != nil {
			t.Fatal(err)
		}
		if changed != s.changed || runs("cpu.cores") != s.want {
			t.Errorf("step %d: changed=%v runs=%s, want %v %s", i, changed, runs("cpu.cores"), s.changed, s.want)
		}
	}
}
