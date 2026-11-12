package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// post submits a form the way a browser on the same origin does.
func post(t *testing.T, h http.Handler, target string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	req.Header.Set("X-Forwarded-User", "operator@example.invalid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// followFlash returns the page a redirect points at, carrying the flash
// cookie the way a browser would, and the text of the message shown.
func followFlash(t *testing.T, h http.Handler, rec *httptest.ResponseRecorder) (string, string) {
	t.Helper()
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("expected a redirect, got %d: %s", rec.Code, rec.Body.String())
	}
	// A browser keeps the fragment to itself.
	loc, _, _ := strings.Cut(rec.Header().Get("Location"), "#")
	req := httptest.NewRequest(http.MethodGet, loc, nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	page := httptest.NewRecorder()
	h.ServeHTTP(page, req)
	body := page.Body.String()
	i := strings.Index(body, `class="flash`)
	if i < 0 {
		return body, ""
	}
	end := strings.Index(body[i:], "</div>")
	return body, body[i : i+end]
}

func TestCrossSiteFormsAreRefused(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	for name, hdr := range map[string][2]string{
		"fetch metadata": {"Sec-Fetch-Site", "cross-site"},
		"origin":         {"Origin", "https://attacker.invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			form := url.Values{"attr": {"env"}, "value": {"pwned"}, "reason": {"x"}}
			req := httptest.NewRequest(http.MethodPost, "/hosts/"+id+"/claim", strings.NewReader(form.Encode()))
			req.Host = "127.0.0.1:8099"
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			req.Header.Set(hdr[0], hdr[1])
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if rec.Code != http.StatusForbidden {
				t.Fatalf("a cross-site form returned %d", rec.Code)
			}
		})
	}
	facts, _ := st.Facts(context.Background(), id)
	for _, f := range facts {
		if f.Attr == "env" {
			t.Fatal("a cross-site submission changed the inventory")
		}
	}
}

func TestAddHostFromTheBrowser(t *testing.T) {
	_, h, st := testServer(t)
	rec := post(t, h, "/hosts", url.Values{"name": {"web09"}, "fqdn": {"web09.example.invalid"}})
	body, msg := followFlash(t, h, rec)
	if !strings.Contains(msg, "web09 added") || !strings.Contains(body, "<h1 class=\"mono\">web09</h1>") {
		t.Fatalf("message %q", msg)
	}
	id, err := st.LookupCI(context.Background(), "web09.example.invalid")
	if err != nil || !strings.HasSuffix(rec.Header().Get("Location"), id) {
		t.Fatalf("host not recorded: %v", err)
	}
	// The flash is shown once, not on every later visit.
	if again := get(t, h, "/hosts/"+id, nil).Body.String(); strings.Contains(again, `class="flash`) {
		t.Error("the message was shown again")
	}
	if _, msg := followFlash(t, h, post(t, h, "/hosts", url.Values{"name": {"has space"}})); !strings.Contains(msg, "spaces") {
		t.Errorf("a bad name gave %q", msg)
	}
}

func TestConnectionEditableOnlyForAgent(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id := seed(t, st)

	rec := post(t, h, "/hosts/"+id+"/connection", url.Values{
		"address": {"appnode1.example.invalid"}, "user": {"svc-ro"}, "port": {"2222"}, "enabled": {"1"}})
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "Connection saved") {
		t.Fatalf("message %q", msg)
	}
	ps, _ := st.ConnProfiles(ctx, id)
	if len(ps) != 1 || ps[0].CredentialRef != "agent:" || ps[0].Port != 2222 || !ps[0].Enabled {
		t.Fatalf("profile %+v", ps)
	}

	// A profile holding a password reference must not be redirectable from
	// the browser.
	other, _, _ := st.ResolveCI(ctx, model.KindHost, "locked", []model.Identity{{Scheme: model.IdentHostname, Value: "locked"}})
	if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: other, Transport: "ssh", Address: "real.invalid",
		Username: "svc", CredentialRef: "keyring:kryostate/svc", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{"/hosts/" + other + "/connection", "/hosts/" + other + "/connection/delete"} {
		rec := post(t, h, target, url.Values{"address": {"attacker.invalid"}, "user": {"svc"}, "transport": {"ssh"}})
		if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "command line") {
			t.Errorf("%s: message %q", target, msg)
		}
	}
	ps, _ = st.ConnProfiles(ctx, other)
	if len(ps) != 1 || ps[0].Address != "real.invalid" {
		t.Fatalf("the locked profile changed: %+v", ps)
	}
	if body := get(t, h, "/hosts/"+other, nil).Body.String(); strings.Contains(body, `action="/hosts/`+other+`/connection"`) {
		t.Error("the page offers a form for a connection it will refuse")
	}

	if _, msg := followFlash(t, h, post(t, h, "/hosts/"+id+"/connection/delete", url.Values{"transport": {"ssh"}})); !strings.Contains(msg, "removed") {
		t.Errorf("delete: %q", msg)
	}
}

func TestChecksFromTheBrowser(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	rec := post(t, h, "/hosts/"+id+"/checks", url.Values{"name": {"https"}, "kind": {"http"},
		"target": {"https://appnode1.example.invalid/health"}, "expect": {"2xx"}})
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "saved") {
		t.Fatalf("message %q", msg)
	}
	if cs, _ := st.Checks(context.Background(), id); len(cs) != 1 {
		t.Fatalf("checks %+v", cs)
	}
	if _, msg := followFlash(t, h, post(t, h, "/hosts/"+id+"/checks", url.Values{"name": {"bad"}, "kind": {"tcp"}, "target": {"no-port"}})); !strings.HasPrefix(strings.TrimSpace(msg), `class="flash flash--error"`) {
		t.Errorf("an invalid check gave %q", msg)
	}
	post(t, h, "/hosts/"+id+"/checks/delete", url.Values{"name": {"https"}})
	if cs, _ := st.Checks(context.Background(), id); len(cs) != 0 {
		t.Errorf("check not removed")
	}
}

func TestStacksFromTheBrowser(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id := seed(t, st)

	post(t, h, "/stacks", url.Values{"name": {"payments"}, "owner": {"team-a"}, "environment": {"prod"}})
	if _, msg := followFlash(t, h, post(t, h, "/stacks", url.Values{"name": {"payments"}})); !strings.Contains(msg, "already") {
		t.Errorf("a duplicate stack gave %q", msg)
	}
	post(t, h, "/stacks/members", url.Values{"stack": {"payments"}, "host": {"appnode1"}, "role": {"app"}, "back": {"/stacks"}})
	ms, _ := st.MembershipsOf(ctx, id)
	if len(ms) != 1 || ms[0].Stack != "payments" || ms[0].Role != "app" {
		t.Fatalf("memberships %+v", ms)
	}

	post(t, h, "/stacks/edit", url.Values{"stack": {"payments"}, "owner": {"team-b"}, "environment": {"prod"}})
	views, _ := st.Stacks(ctx)
	if len(views) != 1 || views[0].Stack.Owner != "team-b" {
		t.Fatalf("stack %+v", views)
	}
	if body := get(t, h, "/stacks/payments", nil).Body.String(); !strings.Contains(body, "appnode1") || !strings.Contains(body, "team-b") {
		t.Error("the stacks page does not show the member")
	}

	post(t, h, "/stacks/members/remove", url.Values{"stack": {"payments"}, "ci": {id}, "back": {"/hosts/" + id}})
	if ms, _ := st.MembershipsOf(ctx, id); len(ms) != 0 {
		t.Error("member not removed")
	}
	// An off-site "back" must not become an open redirect.
	rec := post(t, h, "/stacks/members", url.Values{"stack": {"payments"}, "host": {"appnode1"}, "back": {"https://evil.invalid/"}})
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/stacks") {
		t.Errorf("redirected to %q", loc)
	}

	post(t, h, "/stacks/delete", url.Values{"stack": {"payments"}})
	if views, _ := st.Stacks(ctx); len(views) != 0 {
		t.Error("stack not deleted")
	}
	if _, err := st.CI(ctx, id); err != nil {
		t.Error("deleting a stack deleted its host")
	}
}

func TestRetireAndRestoreFromTheBrowser(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	if _, msg := followFlash(t, h, post(t, h, "/hosts/"+id+"/retire", url.Values{})); !strings.Contains(msg, "why") {
		t.Errorf("retire without a reason gave %q", msg)
	}
	post(t, h, "/hosts/"+id+"/retire", url.Values{"reason": {"gone"}})
	if c, _ := st.CI(context.Background(), id); c.RetiredAt == nil {
		t.Fatal("not retired")
	}
	if body := get(t, h, "/hosts?retired=1", nil).Body.String(); !strings.Contains(tableBody(body), "appnode1") {
		t.Error("the retired list does not show it")
	}
	post(t, h, "/hosts/"+id+"/unretire", url.Values{})
	if c, _ := st.CI(context.Background(), id); c.RetiredAt != nil {
		t.Error("not restored")
	}
}

// TestCollectorsPageOffersOnlyTheCatalog: the page may switch catalog entries
// on, but must never take a command.
func TestCollectorsPageOffersOnlyTheCatalog(t *testing.T) {
	_, h, _ := testServer(t)
	body := get(t, h, "/collectors", nil).Body.String()
	for _, want := range []string{"builtin.os.version", "kryostate collectors -init", "CPU cores", "Apache httpd version", `href="/collectors/file.exists"`} {
		if !strings.Contains(body, want) {
			t.Errorf("the collectors page is missing %q", want)
		}
	}
	pages := []string{body}
	for _, id := range []string{"file.exists", "config.value", "builtin.os.version", "cpu.cores"} {
		pages = append(pages, get(t, h, "/collectors/"+id, nil).Body.String())
	}
	if !strings.Contains(pages[3], "linux.os.version") {
		t.Error("the built-in OS version page does not name its specs")
	}
	for _, p := range pages {
		if strings.Contains(p, `name="command"`) || strings.Contains(p, `name="pattern"`) {
			t.Error("a collectors page has a field for a command or a parse rule")
		}
	}
	// Entries that reveal content are not offered as forms by default.
	page := get(t, h, "/collectors/file.contents", nil).Body.String()
	if strings.Contains(page, `action="/collectors/choose"`) || !strings.Contains(page, "kryostate collectors -enable file.contents") {
		t.Error("the file contents collector is offered from the page without permission")
	}
	guide := get(t, h, "/guide/collectors", nil)
	if guide.Code != http.StatusOK || !strings.Contains(guide.Body.String(), "Step 5: test it") {
		t.Errorf("collector guide returned %d", guide.Code)
	}
	// The user guide links to it by file name, which must become the route.
	if !strings.Contains(get(t, h, "/guide", nil).Body.String(), `href="/guide/collectors"`) {
		t.Error("the user guide does not link to the collector guide")
	}
}
