package web

import (
	"context"
	"html"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

const ownCollectors = `{
  "version": 1,
  "specs": [
    {
      "id": "acme.agent.version",
      "attr": "acme.agent.version",
      "os": ["linux"],
      "comment": "The ACME agent's version.",
      "command": "cat /opt/acme/VERSION",
      "parse": { "kind": "regex", "pattern": "([0-9.]+)" },
      "stacks": ["payments"]
    }
  ]
}
`

// withOwnCollectors gives the test server a collectors.json and an external
// program, as cmdServe would after loading them.
func withOwnCollectors(t *testing.T, s *Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "collectors.json")
	if err := os.WriteFile(path, []byte(ownCollectors), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.Engine.Specs.Merge([]byte(ownCollectors)); err != nil {
		t.Fatal(err)
	}
	s.Engine.Externals = []collect.External{{Name: "vcenter", Path: "/opt/kryostate/vcenter.sh",
		Args: []string{"--quick"}, Env: []string{"VC_TOKEN=s3cret"}, Timeout: time.Minute, Comment: "Asks vCenter."}}
	s.Config.Collectors = path
	return path
}

var rowLink = regexp.MustCompile(`<tr data-row[^>]*>\s*<td class="clist__name">\s*<a href="/collectors/([^"]+)"`)

// TestEveryCollectorHasAPage: whatever its source, each collector in the list
// opens on a page of its own, with its command and a definition that is what
// the loader reads.
func TestEveryCollectorHasAPage(t *testing.T) {
	s, h, _ := testServer(t)
	withOwnCollectors(t, s)
	list := get(t, h, "/collectors", nil).Body.String()
	ids := rowLink.FindAllStringSubmatch(list, -1)
	if len(ids) < len(collect.Catalog)+10 {
		t.Fatalf("only %d collectors listed", len(ids))
	}
	seen := map[string]bool{}
	for _, m := range ids {
		id := m[1]
		seen[id] = true
		rec := get(t, h, "/collectors/"+id, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s: %d", id, rec.Code)
			continue
		}
		body := rec.Body.String()
		for _, want := range []string{"What it records", "View definition", `id="schedule"`, `data-copy="#def-json"`} {
			if !strings.Contains(body, want) {
				t.Errorf("%s: the page lacks %q", id, want)
			}
		}
		file, def, err := s.Engine.Definition(id, "")
		if err != nil {
			t.Errorf("%s: %v", id, err)
			continue
		}
		shown := regexp.MustCompile(`(?s)<pre class="cdef__code" id="def-json"><code>(.*?)</code></pre>`).FindStringSubmatch(body)
		if shown == nil || html.UnescapeString(shown[1]) != string(def) {
			t.Errorf("%s: the page does not show the %s definition", id, file)
		}
		if file == "collectors.json" {
			if _, err := collect.LoadSpecs([]byte(html.UnescapeString(shown[1]))); err != nil {
				t.Errorf("%s: the definition shown does not load: %v", id, err)
			}
		}
		if strings.Contains(body, "s3cret") {
			t.Errorf("%s: an external program's environment value is shown", id)
		}
	}
	for _, id := range []string{"cpu.cores", "file.exists", "builtin.os.version", "spec.acme.agent.version", "external.vcenter"} {
		if !seen[id] {
			t.Errorf("%s is not in the list", id)
		}
	}

	own := html.UnescapeString(get(t, h, "/collectors/spec.acme.agent.version", nil).Body.String())
	for _, want := range []string{"cat /opt/acme/VERSION", "what the pattern ([0-9.]+) captures", "acme.agent.version", "payments", "your own"} {
		if !strings.Contains(own, want) {
			t.Errorf("your own collector's page lacks %q", want)
		}
	}
	ext := get(t, h, "/collectors/external.vcenter", nil).Body.String()
	for _, want := range []string{"/opt/kryostate/vcenter.sh", "--quick", "VC_TOKEN=(hidden)", "externals.json", "Asks vCenter."} {
		if !strings.Contains(ext, want) {
			t.Errorf("the external program's page lacks %q", want)
		}
	}
	tmpl := get(t, h, "/collectors/file.exists", nil).Body.String()
	if !strings.Contains(tmpl, "&#39;/etc/resolv.conf&#39;") || !strings.Contains(tmpl, "-show file.exists -param") {
		t.Error("a template's definition is not written out for its example value")
	}
	if get(t, h, "/collectors/made.up", nil).Code != http.StatusNotFound {
		t.Error("an unknown collector did not answer 404")
	}
}

func TestCollectorListFilters(t *testing.T) {
	s, h, _ := testServer(t)
	withOwnCollectors(t, s)
	post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"all"}})

	visible := func(query string) []string {
		body := get(t, h, "/collectors?"+query, nil).Body.String()
		var out []string
		for _, m := range regexp.MustCompile(`<tr data-row([^>]*)>\s*<td class="clist__name">\s*<a href="/collectors/([^"]+)"`).FindAllStringSubmatch(body, -1) {
			if !strings.Contains(m[1], "hidden") {
				out = append(out, m[2])
			}
		}
		return out
	}
	if got := visible("source=own"); strings.Join(got, ",") != "spec.acme.agent.version" {
		t.Errorf("source=own shows %v", got)
	}
	if got := visible("source=catalog&status=on"); strings.Join(got, ",") != "cpu.cores" {
		t.Errorf("catalog entries switched on: %v", got)
	}
	if got := visible("q=apache+httpd"); len(got) != 1 || got[0] != "httpd.version" {
		t.Errorf("searching for apache httpd shows %v", got)
	}
	if got := visible("cat=web"); len(got) != 2 {
		t.Errorf("web servers: %v", got)
	}
	if got := visible("cat=custom"); len(got) != 2 {
		t.Errorf("custom: %v", got)
	}
	if got := visible("status=off&source=builtin"); len(got) != 0 {
		t.Errorf("built-in collectors shown as off: %v", got)
	}
	if got := visible("q=hw.cpu.cores"); strings.Join(got, ",") != "cpu.cores" {
		t.Errorf("searching by attribute shows %v", got)
	}
}

func TestSwitchingOnReturnsToTheCollector(t *testing.T) {
	_, h, _ := testServer(t)
	rec := post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"all"}})
	if loc := rec.Header().Get("Location"); loc != "/collectors/cpu.cores#where" {
		t.Errorf("redirected to %q", loc)
	}
	page := get(t, h, "/collectors/cpu.cores", nil).Body.String()
	if !strings.Contains(page, `action="/collectors/remove"`) || !strings.Contains(page, "Every host it has a command for") {
		t.Error("the switched-on collector's page does not offer turning it off")
	}
}

func TestTryABuiltinCollector(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	rec := post(t, h, "/collectors/try", url.Values{"spec": {"linux.os.version", "aix.os.version"}, "host": {id}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "has no enabled connection") ||
		!strings.Contains(rec.Body.String(), `href="/collectors/builtin.os.version"`) {
		t.Errorf("got %d: %s", rec.Code, rec.Body.String())
	}
}

func TestABrokenCollectorsFileIsShown(t *testing.T) {
	s, h, _ := testServer(t)
	path := withOwnCollectors(t, s)
	if err := os.WriteFile(path, []byte("{\n  \"version\": 1,\n  \"specs\": [ { \"id\": \"x\", \"optinal\": true } ]\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/collectors", nil).Body.String()
	if !strings.Contains(body, "would not load now") || !strings.Contains(body, "optinal") {
		t.Error("an edited, broken collectors.json is not reported")
	}
	s.Config.CollectorsErr = "parse collectors: line 3, column 40: unknown field"
	body = get(t, h, "/collectors/spec.acme.agent.version", nil).Body.String()
	if !strings.Contains(body, "was not loaded when the server started") || !strings.Contains(body, "line 3, column 40") {
		t.Error("a collectors.json that failed at start-up is not reported")
	}
}

// TestCollectorPageShowsItsSchedule: the schedule card on a collector's page
// edits the scheduler's own key for it, which is not always the page's id.
func TestCollectorPageShowsItsSchedule(t *testing.T) {
	s, h, _ := testServer(t)
	withOwnCollectors(t, s)
	for id, key := range map[string]string{
		"builtin.os.version":      "builtin.os",
		"spec.acme.agent.version": "spec.acme.agent.version",
		"external.vcenter":        "external.vcenter",
	} {
		body := get(t, h, "/collectors/"+id, nil).Body.String()
		if !strings.Contains(body, `name="collector" value="`+key+`"`) {
			t.Errorf("%s: no schedule form for %s", id, key)
		}
		if !strings.Contains(body, `name="back" value="/collectors/`+id+`"`) {
			t.Errorf("%s: the form does not return to the page", id)
		}
	}
}

func TestSchedulesFor(t *testing.T) {
	rows := []scheduleRow{{Key: "catalog.file.exists.3"}, {Key: "catalog.file.exists.12"}, {Key: "catalog.file.existsx"}, {Key: "catalog.cpu.cores"}, {Key: "builtin.hw"}}
	if got := schedulesFor(rows, "file.exists", srcCatalog); len(got) != 2 {
		t.Errorf("file.exists matched %v", got)
	}
	if got := schedulesFor(rows, "cpu.cores", srcCatalog); len(got) != 1 {
		t.Errorf("cpu.cores matched %v", got)
	}
	if got := schedulesFor(rows, "builtin.hw.cpu.cores", "builtin"); len(got) != 1 {
		t.Errorf("builtin matched %v", got)
	}
}

// TestScheduleFormReturnsOnlyToACollectorPage: the "back" field picks where a
// schedule change lands, so it may only name a collector page here.
func TestScheduleFormReturnsOnlyToACollectorPage(t *testing.T) {
	_, h, _ := testServer(t)
	for back, want := range map[string]string{
		"/collectors/builtin.os.version": "/collectors/builtin.os.version#schedule",
		"//evil.example/collectors/":     "/schedules#",
		"/collectors/x?y=1":              "/schedules#",
		"https://evil.example/":          "/schedules#",
	} {
		rec := post(t, h, "/schedules", url.Values{"collector": {"builtin.os"}, "mode": {"every"}, "every": {"1d"}, "back": {back}})
		loc := rec.Header().Get("Location") + rec.Header().Get("HX-Location") + rec.Header().Get("HX-Redirect")
		if !strings.HasPrefix(loc, want) {
			t.Errorf("back=%q went to %q, want %s…", back, loc, want)
		}
	}
}

// TestHeadingsUseTheChoiceTitle: an attribute recorded by a switched-on
// catalog choice is named after it on every page, not spelled out from its
// dotted name.
func TestHeadingsUseTheChoiceTitle(t *testing.T) {
	_, h, st := testServer(t)
	c := collect.Choice{Template: "file.exists", Param: "/etc/ssh/sshd_config.d/hardening.conf"}
	tpl, _ := collect.Lookup(c.Template)
	c.Attr = collect.DefaultAttr(tpl, c.Param)
	if _, _, err := st.EnableChoice(context.Background(), c, "test"); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/hosts?col="+url.QueryEscape(c.Attr), nil).Body.String()
	want := template.HTMLEscapeString(c.ShortLabel())
	if !strings.Contains(body, "<th scope=\"col\">"+want+"</th>") {
		t.Errorf("column heading is not %q", want)
	}
}
