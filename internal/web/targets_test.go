package web

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// estate makes n web hosts and a few others, every one with a connection and
// a reported system, and returns their ids by name.
func estate(t *testing.T, st *store.Store, n int) map[string]string {
	t.Helper()
	ctx := context.Background()
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	add := func(name, family string) {
		id, _, err := st.ResolveCI(ctx, model.KindHost, name, []model.Identity{{Scheme: model.IdentHostname, Value: name}})
		if err != nil {
			t.Fatal(err)
		}
		ids[name] = id
		if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: name, Port: 22,
			Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: "os.family", Raw: family, SourceID: src,
			ObservedAt: time.Now().Add(-time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= n; i++ {
		add(fmt.Sprintf("web%02d", i), "Linux")
	}
	add("db01", "Linux")
	add("win01", "Windows")
	return ids
}

var reItem = regexp.MustCompile(`class="hostpick__item"`)

func TestTargetPreview(t *testing.T) {
	_, h, st := testServer(t)
	estate(t, st, 15)
	preview := func(target string) string {
		t.Helper()
		rec := get(t, h, "/collectors/target/preview?id=nginx.version&target="+url.QueryEscape(target), nil)
		if rec.Code != 200 {
			t.Fatalf("%s: status %d", target, rec.Code)
		}
		return rec.Body.String()
	}

	// Every host: the sample is capped at ten, and says how many more.
	body := preview("")
	if !strings.Contains(body, "matches 17 hosts") || strings.Count(body, "<li>") != 10 || !strings.Contains(body, "and 6 more") {
		t.Errorf("blank target: %s", body)
	}
	// nginx.version has a command for Linux only, so win01 is counted
	// but said to be skipped.
	if !strings.Contains(body, "1 run a system it has no command for") {
		t.Errorf("the Windows host is not called out: %s", body)
	}

	body = preview(`name in ("web01", "db01")`)
	if !strings.Contains(body, "matches 2 hosts") || !strings.Contains(body, "<li>db01</li>") || !strings.Contains(body, "<li>web01</li>") {
		t.Errorf("name list: %s", body)
	}
	if !strings.Contains(body, `href="/hosts?q=name&#43;in&#43;`) && !strings.Contains(body, `href="/hosts?q=name+in+`) {
		t.Errorf("the count does not link to the inventory: %s", body)
	}

	// A target that does not parse says where, and matches nothing.
	body = preview(`name in ("web01"`)
	if !strings.Contains(body, "Column 17") || !strings.Contains(body, "not closed") || strings.Contains(body, "matches") {
		t.Errorf("broken target: %s", body)
	}
	// Whatever is typed comes back escaped.
	body = preview(`name == "<script>"`)
	if strings.Contains(body, "<script>") {
		t.Errorf("not escaped: %s", body)
	}
}

func TestTargetHostSearchIsCapped(t *testing.T) {
	_, h, st := testServer(t)
	estate(t, st, 15)
	body := get(t, h, "/collectors/target/hosts?into=target-new&find=web", nil).Body.String()
	if n := len(reItem.FindAllString(body, -1)); n != pickLimit {
		t.Errorf("%d suggestions, want %d: %s", n, pickLimit, body)
	}
	if !strings.Contains(body, "and 5 more") || !strings.Contains(body, `data-add-host="web01"`) || !strings.Contains(body, `data-into="#target-new"`) {
		t.Errorf("suggestions: %s", body)
	}
	body = get(t, h, "/collectors/target/hosts?into=target-new&find=DB", nil).Body.String()
	if n := len(reItem.FindAllString(body, -1)); n != 1 || !strings.Contains(body, "db01") {
		t.Errorf("case-insensitive search: %s", body)
	}
	body = get(t, h, "/collectors/target/hosts?into=target-new&find=%25", nil).Body.String()
	if reItem.MatchString(body) || !strings.Contains(body, "No host") {
		t.Errorf("%% must be literal: %s", body)
	}
	if rec := get(t, h, `/collectors/target/hosts?into="><b>&find=web`, nil); rec.Code != 400 {
		t.Errorf("a made-up target id was accepted: %d", rec.Code)
	}
}

func TestTryHostsFollowTheTarget(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	estate(t, st, 15)
	c, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores", Target: `name in ("web01", "web02", "db01")`}, "t")
	if err != nil {
		t.Fatal(err)
	}
	id := strconv.FormatInt(c.ID, 10)
	body := get(t, h, "/collectors/try/hosts?into=try-x&choice="+id+"&host=", nil).Body.String()
	if n := len(reItem.FindAllString(body, -1)); n != 3 || strings.Contains(body, "web03") || !strings.Contains(body, `data-pick="db01"`) {
		t.Errorf("targeted hosts: %s", body)
	}
	body = get(t, h, "/collectors/try/hosts?into=try-x&choice="+id+"&host=web", nil).Body.String()
	if n := len(reItem.FindAllString(body, -1)); n != 2 {
		t.Errorf("narrowed by name: %s", body)
	}

	// A target with no connected host falls back to every host, capped, and
	// says so.
	if _, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores", Target: `name == "nobody"`}, "t"); err != nil {
		t.Fatal(err)
	}
	body = get(t, h, "/collectors/try/hosts?into=try-x&choice="+id+"&host=", nil).Body.String()
	if n := len(reItem.FindAllString(body, -1)); n != pickLimit || !strings.Contains(body, "targets no host with a connection") || !strings.Contains(body, "and 7 more") {
		t.Errorf("fallback: %s", body)
	}
	// Built-in specs target every host.
	body = get(t, h, "/collectors/try/hosts?into=try-x&spec=linux.os.version&host=win", nil).Body.String()
	if !strings.Contains(body, `data-pick="win01"`) {
		t.Errorf("built-in: %s", body)
	}
	// The page offers a search box, not a list of every host.
	page := get(t, h, "/collectors/cpu.cores", nil).Body.String()
	if strings.Contains(page, "<select") || strings.Contains(page, "web09") {
		t.Error("the collector page lists hosts")
	}
	if !strings.Contains(page, `hx-get="/collectors/try/hosts"`) || !strings.Contains(page, "Only <span class=\"mono\">nobody</span>") {
		t.Errorf("collector page: %s", page)
	}
}

// TestTargetOnTheCollectorPage: each switched-on choice says where it runs in
// words and how many hosts that is, and a broken one says why.
func TestTargetOnTheCollectorPage(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	estate(t, st, 3)
	if _, err := st.EnsureStack(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	post(t, h, "/collectors/choose", url.Values{"template": {"file.exists"}, "param": {"/etc/a"}, "target": {`stack == "web"`}})
	post(t, h, "/collectors/choose", url.Values{"template": {"file.exists"}, "param": {"/etc/b"}, "target": {`os.family == "Linux" && name =~ "^web"`}})
	post(t, h, "/collectors/choose", url.Values{"template": {"file.exists"}, "param": {"/etc/c"}})
	if _, err := st.DB().ExecContext(ctx, `UPDATE collector_choice SET target='name ==' WHERE param='/etc/c'`); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/collectors/file.exists", nil).Body.String()
	for _, want := range []string{
		`Hosts in <a href="/stacks#stack-web">web</a>`, "matches 0 hosts",
		"Hosts matching", "matches 3 hosts",
		"does not parse", "Column 8",
		`action="/collectors/where"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
}

// TestSpecWhereIsShown: a hand-written spec's where is shown, read-only, with
// what it matches.
func TestSpecWhereIsShown(t *testing.T) {
	s, h, st := testServer(t)
	estate(t, st, 3)
	if err := s.Engine.Specs.Merge([]byte(`{"version": 1, "specs": [{"id": "my.thing", "attr": "my.thing",
		"os": ["linux"], "command": "true", "parse": {"kind": "first_line"},
		"where": "name in (\"web01\", \"web02\")"}]}`)); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/collectors/spec.my.thing", nil).Body.String()
	for _, want := range []string{"Only <span class=\"mono\">web01</span>, <span class=\"mono\">web02</span>", "matches 2 hosts", "<dt>where</dt>", "Read-only here"} {
		if !strings.Contains(page, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	if strings.Contains(page, `action="/collectors/where"`) {
		t.Error("a spec from a file is editable on the page")
	}
}
