package web

import (
	"context"
	"fmt"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// rolloutWeb makes n web hosts in the web stack: the first done on nginx
// 1.24, the last never reporting nginx, the rest on 1.20. db01 and win01 are
// outside the stack.
func rolloutWeb(t *testing.T, st *store.Store, n int) map[string]string {
	t.Helper()
	ctx := context.Background()
	ids := estate(t, st, n)
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	web, err := st.EnsureStack(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= n; i++ {
		name := fmt.Sprintf("web%02d", i)
		if err := st.AddToStack(ctx, web, ids[name], "web"); err != nil {
			t.Fatal(err)
		}
		v := "1.20.1"
		switch i {
		case 1:
			v = "1.24.0"
		case n:
			continue
		}
		if _, err := st.Assert(ctx, model.Assertion{CIID: ids[name], Attr: "nginx.version", Raw: v, SourceID: src,
			ObservedAt: time.Now().Add(-time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	return ids
}

func ruleValues(name, scope, expect string) url.Values {
	return url.Values{"name": {name}, "scope": {scope}, "expect": {expect}, "owner": {"web-team"}}
}

func TestRulePages(t *testing.T) {
	_, h, st := testServer(t)
	rolloutWeb(t, st, 4)

	// No rules yet: the form is open.
	body := get(t, h, "/rules", nil).Body.String()
	if !strings.Contains(body, "No rules yet") || !strings.Contains(body, `<details class="card disclosure" open>`) {
		t.Errorf("empty list: %s", body)
	}

	rec := post(t, h, "/rules", ruleValues("nginx 1.24", `stack == "web"`, `nginx.version >= "1.24"`))
	page, msg := followFlash(t, h, rec)
	if rec.Header().Get("Location") != "/rules/1" || !strings.Contains(msg, "Created the rule nginx 1.24") {
		t.Fatalf("create: %s %q", rec.Header().Get("Location"), msg)
	}
	for _, want := range []string{
		"<h1>nginx 1.24</h1>", "web-team",
		"25% · 1 done", "2 not yet", "1 unknown", "of 4",
		`stack == &#34;web&#34;`, `nginx.version &gt;= &#34;1.24&#34;`,
		"Not yet <span", "Unknown <span", "Done <span",
		"no value for <span class=\"mono\">nginx.version</span> yet",
		`<span class="mono">nginx.version</span> is <span class="mono">1.20.1</span>`,
		`class="rtrend"`, "From the command line", "kryostate rules -show &#39;nginx 1.24&#39;",
		"Edit rule", "Delete rule",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("rule page lacks %q", want)
		}
	}
	if strings.Contains(page, "rchip--done") {
		t.Error("an open rule is marked complete")
	}

	// The list: progress bar, counts, sparkline and the scope in words.
	body = get(t, h, "/rules", nil).Body.String()
	for _, want := range []string{`href="/rules/1"`, "Stack web", `class="segbar rbar"`, `class="spark"`, "1 open", "kryostate rules -add"} {
		if !strings.Contains(body, want) {
			t.Errorf("list lacks %q", want)
		}
	}

	// The overview lists it under Rollouts.
	body = get(t, h, "/", nil).Body.String()
	if !strings.Contains(body, "Rollouts") || !strings.Contains(body, `href="/rules/1"`) {
		t.Error("overview has no rollouts card")
	}
	// The nav names it.
	if !strings.Contains(body, `<a href="/rules"`) {
		t.Error("no Rules in the nav")
	}

	// A query that does not parse is refused, with its column, and what was
	// typed is kept.
	rec = post(t, h, "/rules", ruleValues("broken", `stack == "web"`, `nginx.version >= `))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Expect, column 17:") ||
		!strings.Contains(rec.Body.String(), `value="broken"`) {
		t.Errorf("bad expect: %d %s", rec.Code, rec.Body.String())
	}
	if rs, _ := st.Rules(context.Background()); len(rs) != 1 {
		t.Errorf("a refused rule was saved: %+v", rs)
	}

	// Editing: a bad scope is refused on the rule's page; a good change
	// saves and says the history restarts.
	rec = post(t, h, "/rules/1", ruleValues("nginx 1.24", `stack ==`, `nginx.version >= "1.24"`))
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "Scope, column") {
		t.Errorf("bad edit: %d", rec.Code)
	}
	rec = post(t, h, "/rules/1", ruleValues("nginx 1.20", `stack == "web"`, `nginx.version >= "1.20"`))
	page, msg = followFlash(t, h, rec)
	if !strings.Contains(msg, "history starts again") || !strings.Contains(page, "75% · 3 done") {
		t.Errorf("edit: %q", msg)
	}

	// Deleting.
	rec = post(t, h, "/rules/1/delete", nil)
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "Deleted the rule nginx 1.20") {
		t.Errorf("delete: %q", msg)
	}
	if rec := get(t, h, "/rules/1", nil); rec.Code != http.StatusNotFound {
		t.Errorf("deleted rule: %d", rec.Code)
	}
	if rec := get(t, h, "/rules/nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("bad id: %d", rec.Code)
	}
}

func TestRuleListsAreCappedAndLinkToTheInventory(t *testing.T) {
	_, h, st := testServer(t)
	rolloutWeb(t, st, 60)
	if _, err := st.PutRule(context.Background(), store.Rule{Name: "nginx", Scope: `stack == "web"`, Expect: `nginx.version >= "1.24"`}, "t"); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/rules/1", nil).Body.String()
	// 58 not yet: 50 shown and 8 more.
	if !strings.Contains(page, "and 8 more — <a href=") {
		t.Errorf("not yet is not capped")
	}
	// Each list's link opens the inventory on exactly its hosts.
	for _, c := range []struct {
		list string
		want int
	}{{"notyet", 58}, {"unknown", 1}, {"done", 1}} {
		i := strings.Index(page, `rlistcard--`+c.list)
		if i < 0 {
			t.Fatalf("no %s list", c.list)
		}
		sec := page[i:]
		sec = sec[:strings.Index(sec, "</section>")]
		j := strings.LastIndex(sec, `href="/hosts?q=`)
		href := sec[j+len(`href="`):]
		href = html.UnescapeString(href[:strings.Index(href, `"`)])
		inv := get(t, h, href, nil).Body.String()
		if got := strings.Count(tableBody(inv), "<tr"); got != c.want {
			t.Errorf("%s: %s lists %d hosts, want %d", c.list, href, got, c.want)
		}
	}
}

func TestRuleCompleteBadge(t *testing.T) {
	_, h, st := testServer(t)
	rolloutWeb(t, st, 3)
	if _, err := st.PutRule(context.Background(), store.Rule{Name: "first", Scope: `name == "web01"`, Expect: `nginx.version >= "1.24"`}, "t"); err != nil {
		t.Fatal(err)
	}
	page := get(t, h, "/rules/1", nil).Body.String()
	if !strings.Contains(page, "rchip--done") || !strings.Contains(page, "complete since") {
		t.Errorf("complete rule has no badge")
	}
	// A complete rule is not listed among open rollouts.
	body := get(t, h, "/", nil).Body.String()
	if !strings.Contains(body, "Every rollout is complete") {
		t.Errorf("overview: complete rule listed as open")
	}
}

func TestRulePreview(t *testing.T) {
	_, h, st := testServer(t)
	rolloutWeb(t, st, 4)
	preview := func(q url.Values) string {
		t.Helper()
		rec := get(t, h, "/rules/preview?"+q.Encode(), nil)
		if rec.Code != 200 {
			t.Fatalf("%v: status %d", q, rec.Code)
		}
		return rec.Body.String()
	}
	body := preview(url.Values{"which": {"expect"}, "scope": {`stack == "web"`}, "expect": {`nginx.version >= "1.24"`}})
	if !strings.Contains(body, "25% · 1 done") || !strings.Contains(body, "2 not yet") || !strings.Contains(body, "1 unknown") {
		t.Errorf("expect preview: %s", body)
	}
	body = preview(url.Values{"which": {"expect"}, "expect": {`nginx.version >=`}})
	if !strings.Contains(body, "Column 17:") || !strings.Contains(body, "target__caret") {
		t.Errorf("bad expect preview: %s", body)
	}
	body = preview(url.Values{"which": {"expect"}, "scope": {`stack ==`}, "expect": {`has(x)`}})
	if !strings.Contains(body, "Fix the scope first") {
		t.Errorf("bad scope under expect: %s", body)
	}
	body = preview(url.Values{"which": {"scope"}, "scope": {`stack == "web"`}})
	if !strings.Contains(body, "matches 4 hosts") {
		t.Errorf("scope preview: %s", body)
	}
	body = preview(url.Values{"which": {"scope"}, "scope": {`stack = "web"`}})
	if !strings.Contains(body, "Column 7:") {
		t.Errorf("bad scope preview: %s", body)
	}
	if _, err := st.PutRule(context.Background(), store.Rule{Name: "Taken", Expect: `has(x)`}, "t"); err != nil {
		t.Fatal(err)
	}
	if body = preview(url.Values{"which": {"name"}, "name": {"taken"}}); !strings.Contains(body, "already a rule named Taken") {
		t.Errorf("name preview: %s", body)
	}
	// The rule's own name is not taken when editing it.
	if body = preview(url.Values{"which": {"name"}, "name": {"taken"}, "id": {"1"}}); strings.Contains(body, "already") {
		t.Errorf("own name taken: %s", body)
	}
}

func TestRulePostsAreProtected(t *testing.T) {
	_, h, st := testServer(t)
	for _, target := range []string{"/rules", "/rules/1", "/rules/1/delete"} {
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(ruleValues("x", "", "has(x)").Encode()))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Sec-Fetch-Site", "cross-site")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s: cross-site POST got %d", target, rec.Code)
		}
	}
	if rs, _ := st.Rules(context.Background()); len(rs) != 0 {
		t.Errorf("a cross-site POST saved a rule")
	}
}

func TestSparklineAndTrend(t *testing.T) {
	now := time.Now()
	hist := []store.RuleSnapshot{
		{At: now.Add(-48 * time.Hour), RuleCounts: store.RuleCounts{InScope: 4, Done: 0, NotYet: 4}},
		{At: now.Add(-24 * time.Hour), RuleCounts: store.RuleCounts{InScope: 4, Done: 2, NotYet: 1, Unknown: 1}},
	}
	live := store.RuleCounts{InScope: 4, Done: 4}
	s := string(sparkline(hist, live, now))
	if !strings.HasPrefix(s, `<svg class="spark"`) || !strings.Contains(s, "0% done on") || !strings.Contains(s, "100% now") {
		t.Errorf("sparkline: %s", s)
	}
	tr := string(trendChart(hist, live, now))
	for _, want := range []string{"rtrend__done", "rtrend__unknown", "rtrend__notyet", "Now 4 of 4 hosts done (100%)", ">now</text>"} {
		if !strings.Contains(tr, want) {
			t.Errorf("trend lacks %q", want)
		}
	}
	// No history: one point, still drawn.
	if s := string(sparkline(nil, live, now)); !strings.Contains(s, "100% done now") || strings.Contains(s, "NaN") {
		t.Errorf("single point: %s", s)
	}
	if s := string(trendChart(nil, store.RuleCounts{}, now)); strings.Contains(s, "NaN") {
		t.Errorf("empty trend: %s", s)
	}
}
