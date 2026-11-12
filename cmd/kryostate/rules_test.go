package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestRulesFromTheCommandLine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	src, _ := st.SourceID(ctx, model.SourceCollector, "ssh")
	web, _ := st.EnsureStack(ctx, "web")
	for name, v := range map[string]string{"web01": "1.24.0", "web02": "1.20.1", "web03": ""} {
		id, _, err := st.ResolveCI(ctx, model.KindHost, name, []model.Identity{{Scheme: model.IdentHostname, Value: name}})
		if err != nil {
			t.Fatal(err)
		}
		if err := st.AddToStack(ctx, web, id, ""); err != nil {
			t.Fatal(err)
		}
		if v != "" {
			if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: "nginx.version", Raw: v, SourceID: src, ObservedAt: time.Now()}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	run := func(o rulesOpts) (string, error) {
		var b bytes.Buffer
		err := runRules(ctx, st, &b, o)
		return b.String(), err
	}

	out, err := run(rulesOpts{})
	if err != nil || !strings.Contains(out, "no rules") {
		t.Errorf("empty list: %q %v", out, err)
	}

	add := rulesOpts{add: true, name: "nginx 1.24", scope: `stack == "web"`, expect: `nginx.version >= "1.24"`}
	out, err = run(add)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`rule "nginx 1.24" created`, "33% done: 1 done, 1 not yet, 1 unknown, of 3 hosts",
		"NOT YET (1)", "web02", "nginx.version = 1.20.1", "UNKNOWN (1)", "web03", "no value for nginx.version yet"} {
		if !strings.Contains(out, want) {
			t.Errorf("-add output lacks %q:\n%s", want, out)
		}
	}
	if _, err := run(add); err == nil || !strings.Contains(err.Error(), "already") {
		t.Errorf("duplicate: %v", err)
	}
	_, err = run(rulesOpts{add: true, name: "bad", expect: `nginx.version >=`})
	if err == nil || !strings.Contains(err.Error(), "-expect, column 17") || !strings.Contains(err.Error(), "^") {
		t.Errorf("bad expect: %v", err)
	}
	if _, err := run(rulesOpts{name: "x"}); err == nil {
		t.Error("-name without -add accepted")
	}
	if _, err := run(rulesOpts{add: true, show: "x"}); err == nil {
		t.Error("-add with -show accepted")
	}

	out, err = run(rulesOpts{})
	if err != nil || !strings.Contains(out, "nginx 1.24") || !strings.Contains(out, "33%") || !strings.Contains(out, "open") {
		t.Errorf("list: %q %v", out, err)
	}
	out, err = run(rulesOpts{json: true})
	var list []RuleJSON
	if err != nil || json.Unmarshal([]byte(out), &list) != nil || len(list) != 1 || list[0].NotYet != 1 || list[0].Unknown != 1 {
		t.Errorf("list -json: %s %v", out, err)
	}
	out, err = run(rulesOpts{show: "NGINX 1.24", json: true})
	var d RuleDetailJSON
	if err != nil || json.Unmarshal([]byte(out), &d) != nil || len(d.DoneList) != 1 || d.DoneList[0].Name != "web01" ||
		len(d.Unknowns) != 1 || d.Unknowns[0].Missing[0] != "nginx.version" || d.Reads[0] != "nginx.version" {
		t.Errorf("show -json: %s %v", out, err)
	}
	if _, err := run(rulesOpts{show: "nope"}); err == nil || !strings.Contains(err.Error(), "no rule named") {
		t.Errorf("unknown rule: %v", err)
	}

	out, err = run(rulesOpts{del: "nginx 1.24"})
	if err != nil || !strings.Contains(out, "deleted") {
		t.Errorf("delete: %q %v", out, err)
	}
	if rs, _ := st.Rules(ctx); len(rs) != 0 {
		t.Errorf("not deleted: %+v", rs)
	}
}
