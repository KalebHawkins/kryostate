package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// rolloutEstate is three web hosts, one on the new nginx, one on the old,
// one never collected, and a database host outside the web stack.
func rolloutEstate(t *testing.T, s *Store) map[string]string {
	t.Helper()
	ctx := context.Background()
	src := mustSource(t, s, model.SourceCollector, "ssh")
	at := time.Now().Add(-time.Hour)
	web, err := s.EnsureStack(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, n := range []string{"web01", "web02", "web03", "db01"} {
		ids[n] = mustHost(t, s, n)
		if strings.HasPrefix(n, "web") {
			if err := s.AddToStack(ctx, web, ids[n], "web"); err != nil {
				t.Fatal(err)
			}
		}
	}
	assertValue(t, s, ids["web01"], "nginx.version", "1.24.0", src, at)
	assertValue(t, s, ids["web02"], "nginx.version", "1.20.1", src, at)
	assertValue(t, s, ids["db01"], "nginx.version", "1.18.0", src, at)
	return ids
}

func TestRuleCRUD(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()

	id, err := s.PutRule(ctx, Rule{Name: " nginx 1.24 ", Scope: `stack == "web"`, Expect: `nginx.version >= "1.24"`, Owner: "web-team"}, "kaleb")
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.Rule(ctx, id)
	if err != nil || r.Name != "nginx 1.24" || r.Owner != "web-team" || r.CreatedBy != "kaleb" || r.Created.IsZero() {
		t.Fatalf("Rule = %+v, %v", r, err)
	}
	if got, err := s.RuleByName(ctx, "NGINX 1.24"); err != nil || got.ID != id {
		t.Errorf("RuleByName ignores case: %+v, %v", got, err)
	}

	// A duplicate name, a missing expect and an unparsable query are refused.
	if _, err := s.PutRule(ctx, Rule{Name: "nginx 1.24", Expect: `has(x)`}, "k"); err == nil {
		t.Error("duplicate name accepted")
	}
	if _, err := s.PutRule(ctx, Rule{Name: "x"}, "k"); err == nil {
		t.Error("no expect accepted")
	}
	_, err = s.PutRule(ctx, Rule{Name: "x", Scope: `stack ==`, Expect: `has(x)`}, "k")
	var qe *RuleQueryError
	if !errors.As(err, &qe) || qe.Field != "scope" || qe.Err.Col != 9 {
		t.Errorf("bad scope: %v", err)
	}
	_, err = s.PutRule(ctx, Rule{Name: "x", Expect: `nginx.version >= `}, "k")
	if !errors.As(err, &qe) || qe.Field != "expect" || !strings.Contains(err.Error(), "column") {
		t.Errorf("bad expect: %v", err)
	}

	r.Description = "Move the web tier to nginx 1.24"
	if _, err := s.PutRule(ctx, r, "kaleb"); err != nil {
		t.Fatal(err)
	}
	rules, err := s.Rules(ctx)
	if err != nil || len(rules) != 1 || rules[0].Description != r.Description {
		t.Fatalf("Rules = %+v, %v", rules, err)
	}

	if err := s.DeleteRule(ctx, id, "kaleb"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Rule(ctx, id); !errors.Is(err, ErrRuleNotFound) {
		t.Errorf("after delete: %v", err)
	}
	if h, _ := s.RuleHistory(ctx, id); len(h) != 0 {
		t.Errorf("history outlived the rule: %+v", h)
	}
	if err := s.DeleteRule(ctx, id, "kaleb"); !errors.Is(err, ErrRuleNotFound) {
		t.Errorf("second delete: %v", err)
	}
}

func TestRuleEvaluationBuckets(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	rolloutEstate(t, s)

	r := Rule{Name: "nginx", Scope: `stack == "web"`, Expect: `nginx.version >= "1.24"`}
	ev, err := s.EvaluateRuleNow(ctx, r)
	if err != nil || ev.Err != nil {
		t.Fatal(err, ev.Err)
	}
	names := func(hs []RuleHost) string {
		var out []string
		for _, h := range hs {
			out = append(out, h.Name)
		}
		return strings.Join(out, ",")
	}
	if ev.InScope != 3 || names(ev.Done) != "web01" || names(ev.NotYet) != "web02" || names(ev.Unknown) != "web03" {
		t.Errorf("buckets: in %d, done %s, not yet %s, unknown %s", ev.InScope, names(ev.Done), names(ev.NotYet), names(ev.Unknown))
	}
	if ev.NotYet[0].Values["nginx.version"] != "1.20.1" {
		t.Errorf("not yet says what it has: %+v", ev.NotYet[0])
	}
	if got := ev.Unknown[0].Missing; len(got) != 1 || got[0] != "nginx.version" {
		t.Errorf("unknown says what is missing: %+v", got)
	}
	c := ev.Counts()
	if c.AllDone() || c.Pct() != 33 {
		t.Errorf("counts %+v pct %d", c, c.Pct())
	}

	// Blank scope is every host; db01 is not yet.
	ev, _ = s.EvaluateRuleNow(ctx, Rule{Name: "all", Expect: `nginx.version >= "1.24"`})
	if ev.InScope != 4 || len(ev.NotYet) != 2 {
		t.Errorf("blank scope: %+v", ev.Counts())
	}

	// A scope that matches nothing is not complete.
	ev, _ = s.EvaluateRuleNow(ctx, Rule{Name: "none", Scope: `stack == "nope"`, Expect: `has(x)`})
	if ev.Counts().AllDone() {
		t.Error("an empty rule is complete")
	}

	// A stored rule whose query no longer parses counts nothing and says why.
	ev = EvaluateRule(Rule{Name: "broken", Expect: `x ==`}, nil, nil)
	if ev.Err == nil || !strings.Contains(ev.Err.Error(), "expect, column") {
		t.Errorf("broken rule: %v", ev.Err)
	}
}

func TestRuleSnapshotsAreThrottled(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ids := rolloutEstate(t, s)
	id, err := s.PutRule(ctx, Rule{Name: "nginx", Scope: `stack == "web"`, Expect: `nginx.version >= "1.24"`}, "k")
	if err != nil {
		t.Fatal(err)
	}
	// Creating the rule took the first snapshot.
	hist, _ := s.RuleHistory(ctx, id)
	if len(hist) != 1 || !hist[0].Changed || hist[0].Done != 1 {
		t.Fatalf("first snapshot: %+v", hist)
	}

	now := time.Now()
	// Nothing changed and less than a day passed: no snapshot.
	if n, err := s.SnapshotRules(ctx, now.Add(time.Hour)); err != nil || n != 0 {
		t.Errorf("unchanged within a day: %d, %v", n, err)
	}
	// A finished run takes a snapshot when something moved.
	src := mustSource(t, s, model.SourceCollector, "ssh")
	assertValue(t, s, ids["web02"], "nginx.version", "1.24.0", src, now)
	run, _ := s.StartRun(ctx, "builtin", 3)
	if err := s.FinishRun(ctx, run, "complete", 3, 0); err != nil {
		t.Fatal(err)
	}
	if hist, _ = s.RuleHistory(ctx, id); len(hist) != 2 || hist[1].Done != 2 {
		t.Errorf("after a run: %+v", hist)
	}
	// Counts changed: a snapshot at once, however soon.
	assertValue(t, s, ids["web03"], "nginx.version", "1.24.1", src, now)
	if n, _ := s.SnapshotRules(ctx, now.Add(2*time.Hour)); n != 1 {
		t.Errorf("changed: %d snapshots", n)
	}
	// Unchanged for a day: one more, so the line has a point a day.
	if n, _ := s.SnapshotRules(ctx, now.Add(30*time.Hour)); n != 1 {
		t.Errorf("a day later: %d snapshots", n)
	}
	hist, _ = s.RuleHistory(ctx, id)
	if len(hist) != 4 {
		t.Fatalf("history: %+v", hist)
	}
	if !hist[2].Changed || hist[2].Done != 3 || hist[3].Changed {
		t.Errorf("history marks changes: %+v", hist)
	}

	st, err := s.RuleStatus(ctx)
	if err != nil || len(st) != 1 {
		t.Fatal(st, err)
	}
	if !st[0].Complete || st[0].Done != 3 || !st[0].CompleteSince.Equal(hist[2].At) || !st[0].LastChange.Equal(hist[2].At) {
		t.Errorf("status: %+v, history %+v", st[0], hist)
	}

	// Changing the expect query starts the history again.
	r, _ := s.Rule(ctx, id)
	r.Expect = `nginx.version >= "1.26"`
	if _, err := s.PutRule(ctx, r, "k"); err != nil {
		t.Fatal(err)
	}
	if hist, _ = s.RuleHistory(ctx, id); len(hist) != 1 || hist[0].Done != 0 {
		t.Errorf("after editing the query: %+v", hist)
	}
}
