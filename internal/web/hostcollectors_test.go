package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// linuxHost records a host that reported Linux and has a connection.
func linuxHost(t *testing.T, st *store.Store, name string) string {
	t.Helper()
	ctx := context.Background()
	id, _, err := st.ResolveCI(ctx, model.KindHost, name, []model.Identity{{Scheme: model.IdentHostname, Value: name}})
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	for attr, v := range map[string]string{"os.family": "Linux", "hw.cpu.cores": "8"} {
		if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: attr, Raw: v, SourceID: src,
			ObservedAt: time.Now().Add(-time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: name,
		Port: 22, Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestHostPageSaysWhichCollectorsRunAndWhy: the host page lists what runs on
// the host with the reason and schedule, and what is on but skips it, with
// the reason.
func TestHostPageSaysWhichCollectorsRunAndWhy(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id := linuxHost(t, st, "web01")
	linuxHost(t, st, "db01")
	post(t, h, "/stacks", url.Values{"name": {"web"}, "hosts": {"web01 app"}})
	for _, c := range []collect.Choice{
		{Template: "cpu.cores"},
		{Template: "nginx.version", Target: `stack == "web"`},
		{Template: "httpd.version", Target: `name == "db01"`},
	} {
		if _, err := st.PutChoice(ctx, c, "test"); err != nil {
			t.Fatal(err)
		}
	}

	page := get(t, h, "/hosts/"+id, nil).Body.String()
	at := strings.Index(page, "Collectors on this host")
	if at < 0 {
		t.Fatal("the host page has no Collectors on this host section")
	}
	section := page[at:]
	skipped := strings.Index(section, "Not running here")
	if skipped < 0 {
		t.Fatalf("no Not running here list:\n%s", section)
	}
	running, notRunning := section[:skipped], section[skipped:]
	for _, want := range []string{
		"Built in · every host",
		`href="/collectors/cpu.cores"`, "every host", "daily at ",
		`href="/collectors/nginx.version"`, "matches", `<code class="mono">stack == &#34;web&#34;</code>`,
		">8<", // the last value cpu.cores recorded
	} {
		if !strings.Contains(running, want) {
			t.Errorf("running collectors lack %q", want)
		}
	}
	if strings.Contains(running, "httpd.version") {
		t.Error("httpd is listed as running on web01")
	}
	for _, want := range []string{`href="/collectors/httpd.version"`, "target excludes this host", `name == &#34;db01&#34;`} {
		if !strings.Contains(notRunning, want) {
			t.Errorf("not-running list lacks %q", want)
		}
	}
	if strings.Contains(notRunning, "nginx.version") {
		t.Error("nginx is listed as not running on a web host")
	}
}

// TestRemovedCatalogChoiceIsShownAndRemovable: a choice whose catalog entry a
// later build removed does not break the collectors page, the host page or
// the schedules page; the collectors page labels it and turns it off.
func TestRemovedCatalogChoiceIsShownAndRemovable(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id := linuxHost(t, st, "web01")
	if _, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores"}, "test"); err != nil {
		t.Fatal(err)
	}
	res, err := st.DB().ExecContext(ctx,
		`INSERT INTO collector_choice (template, param, attr, target, created_at)
		 VALUES ('vendor.gone.version', '', 'vendor.gone.version', 'stack == "web"', '2026-01-01T00:00:00Z')`)
	if err != nil {
		t.Fatal(err)
	}
	gone, _ := res.LastInsertId()

	for _, path := range []string{"/collectors", "/hosts/" + id, "/schedules", "/stacks", "/"} {
		if rec := get(t, h, path, nil); rec.Code != http.StatusOK {
			t.Fatalf("%s gave %d with a removed catalog entry switched on", path, rec.Code)
		}
	}
	page := get(t, h, "/collectors", nil).Body.String()
	for _, want := range []string{"No longer in the catalog", "vendor.gone.version", "not in catalog",
		`action="/collectors/remove"`, `name="id" value="` + itoa(gone) + `"`, "Turn off", "kryostate collectors -disable"} {
		if !strings.Contains(page, want) {
			t.Errorf("the collectors page lacks %q", want)
		}
	}
	if host := get(t, h, "/hosts/"+id, nil).Body.String(); strings.Contains(host, "vendor.gone") {
		t.Error("the host page offers or lists the removed entry")
	}

	rec := post(t, h, "/collectors/remove", url.Values{"id": {itoa(gone)}})
	if loc := rec.Header().Get("Location"); loc != "/collectors" {
		t.Errorf("turning it off went to %q", loc)
	}
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "is off") {
		t.Errorf("turning it off said %q", msg)
	}
	cs, _ := st.Choices(ctx)
	if len(cs) != 1 || cs[0].Template != "cpu.cores" {
		t.Errorf("after turning it off: %+v", cs)
	}
	if strings.Contains(get(t, h, "/collectors", nil).Body.String(), "No longer in the catalog") {
		t.Error("the list stays after the last one is turned off")
	}
}

// TestHostPageShowsUnreadableValuesAsUnknown: a value the host could not read
// last time is shown as unknown with what the host said, not as the old value.
func TestHostPageShowsUnreadableValuesAsUnknown(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id := linuxHost(t, st, "web01")
	c, err := st.PutChoice(ctx, collect.Choice{Template: "file.exists", Param: "/etc/ssh/sshd_config.d/hardening.conf"}, "test")
	if err != nil {
		t.Fatal(err)
	}
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: c.Attr, Raw: "false", SourceID: src,
		ObservedAt: time.Now().Add(-2 * time.Hour)}, nil); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordReadFailure(ctx, store.ReadFailure{CIID: id, Attr: c.Attr, SourceID: src,
		Error: "cannot look inside /etc/ssh/sshd_config.d as this user", LastAt: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/hosts/"+id, nil).Body.String()
	for _, want := range []string{">unknown<", "could not read since", "cannot look inside /etc/ssh/sshd_config.d"} {
		if !strings.Contains(page, want) {
			t.Errorf("host page lacks %q", want)
		}
	}
	// The query language sees it as missing, so a rule counts it as unknown.
	if hosts := get(t, h, "/hosts?q="+url.QueryEscape(c.Attr+` == "false"`), nil).Body.String(); strings.Contains(hosts, ">web01<") {
		t.Error("an unreadable value still matches a query")
	}
}
