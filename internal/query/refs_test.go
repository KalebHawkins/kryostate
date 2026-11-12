package query

import (
	"slices"
	"testing"
)

func TestAttrsListsWhatAQueryReads(t *testing.T) {
	for src, want := range map[string][]string{
		`nginx.version >= "1.24"`:                                     {"nginx.version"},
		`stack == "web" && name =~ "^w" && role == "app"`:             {},
		`has(hw.serial) || !(os.family in ("Linux", "Windows"))`:      {"hw.serial", "os.family"},
		`app == "ledger" && os.family == "Linux" && os.family != "x"`: {"application", "os.family"},
		`file.exists["/etc/hosts"] == true`:                           {"file.hosts.exists"},
	} {
		e, err := Parse(src)
		if err != nil {
			t.Fatalf("%s: %v", src, err)
		}
		if got := e.Attrs(); !slices.Equal(got, want) {
			t.Errorf("%s: Attrs() = %v, want %v", src, got, want)
		}
	}
}

func TestDecideTellsMissingFromNo(t *testing.T) {
	old := Host{Name: "web01", Stacks: []string{"web"}, Values: map[string]string{"nginx.version": "1.20.1"}}
	cur := Host{Name: "web02", Stacks: []string{"web"}, Values: map[string]string{"nginx.version": "1.24.0", "os.family": "Linux"}}
	none := Host{Name: "web03", Stacks: []string{"web"}, Values: map[string]string{}}

	for _, c := range []struct {
		q         string
		h         Host
		want      Verdict
		wantMatch bool
	}{
		{`nginx.version >= "1.24"`, old, No, false},
		{`nginx.version >= "1.24"`, cur, Yes, true},
		{`nginx.version >= "1.24"`, none, Unknown, false},
		// A negated comparison is still unknown without a value, though
		// Match says yes.
		{`nginx.version != "1.20.1"`, none, Unknown, true},
		{`!(nginx.version == "1.20.1")`, none, Unknown, true},
		// Asking about absence always has an answer.
		{`has(nginx.version)`, none, No, false},
		{`!has(nginx.version)`, none, Yes, true},
		{`nginx.version == ""`, none, Yes, true},
		{`nginx.version != ""`, none, No, false},
		// && is settled by a No on either side, || by a Yes.
		{`nginx.version >= "1.24" && os.family == "Linux"`, old, No, false},
		{`nginx.version >= "1.24" && os.family == "Linux"`, cur, Yes, true},
		{`nginx.version >= "1.24" || os.family == "Linux"`, cur, Yes, true},
		{`os.family == "Linux" || nginx.version >= "1.24"`, old, Unknown, false},
		{`os.family == "Linux" && nginx.version >= "1.24"`, none, Unknown, false},
		{`nginx.version in ("1.24.0", "1.26.0")`, none, Unknown, false},
		{`nginx.version in ("1.24.0", "1.26.0")`, cur, Yes, true},
		{`stack == "web"`, none, Yes, true},
		{`file.exists["/etc/ssh/sshd_config.d/hardening.conf"] == true`, none, Unknown, false},
	} {
		e, err := Parse(c.q)
		if err != nil {
			t.Fatalf("%s: %v", c.q, err)
		}
		if got := e.Decide(c.h); got != c.want {
			t.Errorf("%s on %s: Decide = %s, want %s", c.q, c.h.Name, got, c.want)
		}
		if got := e.Match(c.h); got != c.wantMatch {
			t.Errorf("%s on %s: Match = %v, want %v", c.q, c.h.Name, got, c.wantMatch)
		}
	}

	e, _ := Parse(`nginx.version >= "1.24" && os.family == "Linux"`)
	if got := e.Missing(old); !slices.Equal(got, []string{"os.family"}) {
		t.Errorf("Missing = %v", got)
	}
}
