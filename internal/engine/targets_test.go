package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// TestCollectFollowsTargets: a full collection runs a catalog collector only
// on the hosts its target names, and one whose target does not parse runs
// nowhere rather than everywhere.
func TestCollectFollowsTargets(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"web01": "linux", "web02": "linux", "db01": "linux"})
	if _, err := fx.st.PutChoice(ctx, collect.Choice{Template: "nginx.version",
		Target: `name in ("web01", "db01")`}, "test"); err != nil {
		t.Fatal(err)
	}
	// A broken target cannot be saved, so it is written as an older or
	// hand-edited database might hold it.
	if _, err := fx.st.PutChoice(ctx, collect.Choice{Template: "httpd.version"}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.DB().ExecContext(ctx,
		`UPDATE collector_choice SET target='name in ("web01"' WHERE template='httpd.version'`); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.e.Collect(ctx, Options{HostTimeout: 10 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	nginx := func(host string) bool {
		for _, c := range fx.f.commands(host) {
			if strings.Contains(c, "nginx") {
				return true
			}
		}
		return false
	}
	if !nginx("web01") || !nginx("db01") {
		t.Errorf("targeted hosts did not run nginx: web01 %v, db01 %v", fx.f.commands("web01"), fx.f.commands("db01"))
	}
	if nginx("web02") {
		t.Errorf("web02 is outside the target but ran %v", fx.f.commands("web02"))
	}
	for _, h := range []string{"web01", "web02", "db01"} {
		for _, c := range fx.f.commands(h) {
			if strings.Contains(c, "httpd") || strings.Contains(c, "apache") {
				t.Errorf("%s ran a collector whose target does not parse: %s", h, c)
			}
		}
	}
}

// TestScheduledRunsFollowTargets: the scheduler does not contact a host its
// due collector does not target.
func TestScheduledRunsFollowTargets(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"in1": "linux", "out1": "linux"})
	ch, err := fx.st.PutChoice(ctx, collect.Choice{Template: "nginx.version", Target: `name == "in1"`}, "test")
	if err != nil {
		t.Fatal(err)
	}
	fx.every(t, choiceKey(ch), time.Hour)
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	if len(fx.f.commands("in1")) != 1 {
		t.Errorf("target ran %v", fx.f.commands("in1"))
	}
	if fx.f.dials["out1"] != 0 {
		t.Errorf("a host outside the target was contacted %d times", fx.f.dials["out1"])
	}
}

// TestSpecWhere: a spec from collectors.json with where and stacks needs a
// host to satisfy both.
func TestSpecWhere(t *testing.T) {
	sp := collect.Spec{Stacks: []string{"web"}, Where: `name in ("a", "b") || name == "c"`}
	if got, want := SpecTarget(sp), `stack == "web" && (name in ("a", "b") || name == "c")`; got != want {
		t.Fatalf("SpecTarget = %s, want %s", got, want)
	}
	tg := NewTargets([]query.Host{
		{ID: "1", Name: "a", Stacks: []string{"web"}},
		{ID: "2", Name: "b"},
		{ID: "3", Name: "c", Stacks: []string{"web"}},
	}, nil)
	var got []string
	for _, id := range []string{"1", "2", "3", "unknown"} {
		if tg.specApplies(sp, id) {
			got = append(got, id)
		}
	}
	if strings.Join(got, ",") != "1,3" {
		t.Errorf("matched %v", got)
	}
	if !tg.specApplies(collect.Spec{}, "unknown") {
		t.Error("a spec with no target must run everywhere")
	}
	bad := collect.Spec{Where: `name ==`}
	if tg.specApplies(bad, "1") || tg.Err(SpecTarget(bad)) == nil {
		t.Error("a target that does not parse must match nothing and say why")
	}
	if n := len(tg.Hosts(`stack == "web"`)); n != 2 {
		t.Errorf("Hosts found %d", n)
	}
}
