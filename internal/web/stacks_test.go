package web

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// stacksFixture makes two stacks: payments with three members in two roles,
// and an empty edge stack.
func stacksFixture(t *testing.T, h http.Handler) {
	t.Helper()
	post(t, h, "/stacks", url.Values{"name": {"payments"}, "environment": {"production"}, "owner": {"team-a"},
		"hosts": {"web01 app\nweb02 app\ndb01 database"}})
	post(t, h, "/stacks", url.Values{"name": {"edge"}, "owner": {"net-team"}})
}

func TestStacksListIsATableOfRows(t *testing.T) {
	_, h, _ := testServer(t)
	stacksFixture(t, h)
	page := get(t, h, "/stacks", nil).Body.String()
	for _, want := range []string{
		`href="/stacks/payments"`, `href="/stacks/edge"`,
		"production · team-a", "app ×2, database ×1",
		`href="/hosts?q=stack&#43;%3D%3D&#43;%22payments%22"`,
		"kryostate stack</span> lists them",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("list lacks %q", want)
		}
	}
	// No member lists, no per-stack forms and no list of every host.
	for _, bad := range []string{">web01<", "<datalist", "/stacks/members", "Also collect on"} {
		if strings.Contains(page, bad) {
			t.Errorf("list has %q", bad)
		}
	}
	if n := strings.Count(page, `<td class="slist__name">`); n != 2 {
		t.Errorf("%d rows, want 2", n)
	}
}

func TestStacksFilter(t *testing.T) {
	_, h, _ := testServer(t)
	stacksFixture(t, h)
	for q, want := range map[string]string{"team-a": "payments", "PROD": "payments", "net": "edge", "edg": "edge"} {
		page := get(t, h, "/stacks?q="+url.QueryEscape(q), nil).Body.String()
		other := "edge"
		if want == "edge" {
			other = "payments"
		}
		if !strings.Contains(page, `href="/stacks/`+want+`"`) || strings.Contains(page, `href="/stacks/`+other+`"`) {
			t.Errorf("q=%s: want only %s", q, want)
		}
		if !strings.Contains(page, "1 of 2 stacks match") {
			t.Errorf("q=%s: no count", q)
		}
	}
	page := get(t, h, "/stacks?q=nothing", nil).Body.String()
	if !strings.Contains(page, "No stack's name, environment or owner contains") {
		t.Error("an empty filter says nothing")
	}
	// The filter box swaps the list in place.
	frag := get(t, h, "/stacks?q=edge", map[string]string{"HX-Request": "true"}).Body.String()
	if strings.Contains(frag, "<html") || !strings.Contains(frag, `id="stack-list"`) {
		t.Error("an HTMX filter does not return the list fragment")
	}
}

func TestStackPage(t *testing.T) {
	_, h, st := testServer(t)
	stacksFixture(t, h)
	page := get(t, h, "/stacks/payments", nil).Body.String()
	for _, want := range []string{
		"← Stacks", "<h1>payments</h1>", "web01", "db01", `chip--role">database<`,
		`action="/stacks/members/remove"`, `name="back" value="/stacks/payments"`,
		`hx-get="/stacks/payments/hosts"`, "Add hosts by query", `href="/guide/queries"`,
		"Collected here", "Also collect on payments", "Edit details", "Delete payments",
		"kryostate hosts -stack payments", "kryostate stack -name payments -add web01:app",
		"kryostate stack -name payments -q",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("stack page lacks %q", want)
		}
	}
	if strings.Contains(page, "<datalist") {
		t.Error("the stack page lists every host")
	}
	if rec := get(t, h, "/stacks/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown stack gave %d", rec.Code)
	}

	// A name with a space and a slash is one path segment.
	post(t, h, "/stacks", url.Values{"name": {"a b/c"}})
	if rec := get(t, h, "/stacks/a%20b%2Fc", nil); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<h1>a b/c</h1>") {
		t.Errorf("an escaped name gave %d", rec.Code)
	}
	if !strings.Contains(get(t, h, "/stacks", nil).Body.String(), `href="/stacks/a%20b%2Fc"`) {
		t.Error("the list does not escape the name")
	}
	if !strings.Contains(get(t, h, "/stacks/a%20b%2Fc", nil).Body.String(), "-name &#39;a b/c&#39;") {
		t.Error("the command-line hint does not quote the name")
	}

	// The host page links to the stack's page, not an anchor on the list.
	id, err := st.LookupCI(context.Background(), "web01")
	if err != nil {
		t.Fatal(err)
	}
	if hp := get(t, h, "/hosts/"+id, nil).Body.String(); !strings.Contains(hp, `href="/stacks/payments"`) || strings.Contains(hp, "#stack-") {
		t.Error("the host page does not link to the stack's page")
	}
}

func TestStackPageCapsMembers(t *testing.T) {
	_, h, _ := testServer(t)
	var lines []string
	for i := range memberLimit + 5 {
		lines = append(lines, fmt.Sprintf("node%02d app", i))
	}
	post(t, h, "/stacks", url.Values{"name": {"big"}, "hosts": {strings.Join(lines, "\n")}})
	page := get(t, h, "/stacks/big", nil).Body.String()
	if n := strings.Count(page, `action="/stacks/members/remove"`); n != memberLimit {
		t.Errorf("%d members shown, want %d", n, memberLimit)
	}
	if !strings.Contains(page, "and 5 more") {
		t.Error("the cap does not say how many more")
	}
	if !strings.Contains(get(t, h, "/stacks", nil).Body.String(), "app ×55") {
		t.Error("the list does not count every member's role")
	}
}

func TestStackHostTypeAhead(t *testing.T) {
	_, h, _ := testServer(t)
	var lines []string
	for i := range 15 {
		lines = append(lines, fmt.Sprintf("node%02d app", i))
	}
	post(t, h, "/stacks", url.Values{"name": {"pool"}, "hosts": {strings.Join(lines, "\n")}})
	post(t, h, "/stacks", url.Values{"name": {"payments"}})

	body := get(t, h, "/stacks/payments/hosts?host=node", nil).Body.String()
	if n := strings.Count(body, "data-pick="); n != pickLimit {
		t.Errorf("%d picks, want %d", n, pickLimit)
	}
	if !strings.Contains(body, "and 5 more") || !strings.Contains(body, `data-into="#stack-host"`) {
		t.Errorf("picks: %s", body)
	}
	if body := get(t, h, "/stacks/payments/hosts?host=", nil).Body.String(); strings.Contains(body, "data-pick") {
		t.Error("an empty search lists hosts")
	}
	if body := get(t, h, "/stacks/payments/hosts?host=zzz", nil).Body.String(); !strings.Contains(body, "No host&#39;s name contains") {
		t.Errorf("no match: %s", body)
	}
	if rec := get(t, h, "/stacks/nope/hosts?host=node", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown stack gave %d", rec.Code)
	}
}

func TestStackFormsReturnToTheStackPage(t *testing.T) {
	_, h, st := testServer(t)
	stacksFixture(t, h)
	id, err := st.LookupCI(context.Background(), "web01")
	if err != nil {
		t.Fatal(err)
	}
	loc := func(rec *httptest.ResponseRecorder) string { return rec.Header().Get("Location") }
	page := "/stacks/payments"
	for name, c := range map[string]struct {
		path string
		form url.Values
		want string
	}{
		"add from the page":    {"/stacks/members", url.Values{"stack": {"payments"}, "host": {"web01"}, "back": {page}}, page},
		"add, no back":         {"/stacks/members", url.Values{"stack": {"payments"}, "host": {"web01"}}, page},
		"add from the host":    {"/stacks/members", url.Values{"stack": {"payments"}, "host": {"web01"}, "back": {"/hosts/" + id}}, "/hosts/" + id},
		"add from the list":    {"/stacks/members", url.Values{"stack": {"payments"}, "host": {"web01"}, "back": {"/stacks"}}, "/stacks"},
		"remove from the page": {"/stacks/members/remove", url.Values{"stack": {"payments"}, "ci": {id}, "back": {page}}, page},
		"edit":                 {"/stacks/edit", url.Values{"stack": {"payments"}, "owner": {"team-b"}, "back": {page}}, page},
		"collectors":           {"/stacks/collectors", url.Values{"stack": {"payments"}, "what": {"cpu.cores"}, "back": {page}}, page},
		"several hosts":        {"/stacks/hosts", url.Values{"stack": {"payments"}, "hosts": {"web03 app"}, "back": {page}}, page},
		"by query":             {"/stacks/query", url.Values{"stack": {"payments"}, "query": {`name == "web03"`}, "back": {page}}, page},
		"create":               {"/stacks", url.Values{"name": {"new one"}}, "/stacks/new%20one"},
		"delete":               {"/stacks/delete", url.Values{"stack": {"edge"}, "back": {"/stacks/edge"}}, "/stacks"},
	} {
		if got := loc(post(t, h, c.path, c.form)); got != c.want {
			t.Errorf("%s: redirected to %q, want %q", name, got, c.want)
		}
	}
	// Off-site and odd values fall back to the stack's page.
	for _, back := range []string{"https://evil.invalid/", "//evil.invalid", "/stacks//evil.invalid", `/stacks/\evil.invalid`,
		`/stacks/x\y`, "/stacks/x?y=1", "/stacks/x#y", "/hosts//evil.invalid", "/elsewhere", "javascript:alert(1)"} {
		rec := post(t, h, "/stacks/members", url.Values{"stack": {"payments"}, "host": {"web01"}, "back": {back}})
		if got := loc(rec); got != page {
			t.Errorf("back %q redirected to %q", back, got)
		}
	}
}

func TestStackBack(t *testing.T) {
	for in, want := range map[string]string{
		"/stacks":          "/stacks",
		"/stacks/a%20b":    "/stacks/a%20b",
		"/hosts/abc":       "/hosts/abc",
		"":                 "D",
		"/stacks/":         "/stacks/",
		"/stacks//x":       "D",
		"/stacks/a?b":      "D",
		"/stacks/a#b":      "D",
		`/stacks/a\b`:      "D",
		"https://x/stacks": "D",
		"/stacksx":         "D",
	} {
		if got := stackBack(in, "D"); got != want {
			t.Errorf("stackBack(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestAddHostsToAStackByQuery(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	stacksFixture(t, h)
	post(t, h, "/stacks", url.Values{"name": {"pool"}, "hosts": {"node01 x\nnode02 x\nnode03 x\nweb09 x"}})

	// The preview counts what it adds, without changing anything.
	body := get(t, h, "/stacks/payments/query?query="+url.QueryEscape(`stack == "pool" || name == "web01"`), nil).Body.String()
	if !strings.Contains(body, "adds 4 hosts") || !strings.Contains(body, "1 more is already in the stack") ||
		!strings.Contains(body, `hx-swap-oob="true"`) || !strings.Contains(body, "Add 4 hosts") {
		t.Errorf("preview: %s", body)
	}
	if ms, _ := st.StackMembers(ctx, "payments"); len(ms) != 3 {
		t.Fatalf("the preview changed the stack: %d members", len(ms))
	}
	body = get(t, h, "/stacks/payments/query?query="+url.QueryEscape(`name ==`), nil).Body.String()
	if !strings.Contains(body, "Column") || !strings.Contains(body, "disabled") {
		t.Errorf("a bad query's preview: %s", body)
	}
	if rec := get(t, h, "/stacks/nope/query?query=x", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown stack gave %d", rec.Code)
	}

	// Adding skips members, who keep their role.
	_, msg := followFlash(t, h, post(t, h, "/stacks/query", url.Values{"stack": {"payments"},
		"query": {`stack == "pool" || name == "web01"`}, "role": {"worker"}, "back": {"/stacks/payments"}}))
	if !strings.Contains(msg, "Added 4 hosts to payments (1 already members).") {
		t.Errorf("message %q", msg)
	}
	roles, _ := st.StackRoles(ctx, "payments")
	if len(roles) != 7 {
		t.Errorf("payments has %d members", len(roles))
	}
	web01, _ := st.LookupCI(ctx, "web01")
	node01, _ := st.LookupCI(ctx, "node01")
	if roles[web01] != "app" || roles[node01] != "worker" {
		t.Errorf("roles %v", roles)
	}

	for q, want := range map[string]string{
		"":                 "blank",
		"   ":              "blank",
		`name ==`:          "column",
		`name == "nobody"`: "matches no host",
	} {
		if _, msg := followFlash(t, h, post(t, h, "/stacks/query", url.Values{"stack": {"payments"}, "query": {q}})); !strings.Contains(msg, want) {
			t.Errorf("query %q gave %q", q, msg)
		}
	}
	if _, msg := followFlash(t, h, post(t, h, "/stacks/query", url.Values{"stack": {"nope"}, "query": {"node01"}})); !strings.Contains(msg, "no such stack") {
		t.Errorf("an unknown stack gave %q", msg)
	}

	// A cross-site form is refused like every other POST.
	form := url.Values{"stack": {"edge"}, "query": {`stack == "pool"`}}
	req := httptest.NewRequest(http.MethodPost, "/stacks/query", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("a cross-site add by query gave %d", rec.Code)
	}
	if ms, _ := st.StackMembers(ctx, "edge"); len(ms) != 0 {
		t.Error("a cross-site add by query changed the stack")
	}
}
