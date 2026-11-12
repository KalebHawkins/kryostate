package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// apiGet fetches an API URL, checks it answered JSON with the status
// expected, and decodes it.
func apiGet(t *testing.T, h http.Handler, target string, status int) map[string]any {
	t.Helper()
	rec := get(t, h, target, nil)
	if rec.Code != status {
		t.Fatalf("GET %s: %d, want %d: %s", target, rec.Code, status, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("GET %s: content type %q", target, ct)
	}
	var out map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("GET %s: %v: %s", target, err, rec.Body.String())
	}
	return out
}

func items(t *testing.T, m map[string]any) []map[string]any {
	t.Helper()
	raw, ok := m["items"].([]any)
	if !ok {
		t.Fatalf("no items: %v", m)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		out = append(out, r.(map[string]any))
	}
	return out
}

func apiEstate(t *testing.T) (http.Handler, *store.Store, string) {
	t.Helper()
	_, h, st := testServer(t)
	ctx := context.Background()
	web01 := linuxHost(t, st, "web01")
	linuxHost(t, st, "web02")
	linuxHost(t, st, "db01")
	post(t, h, "/stacks", url.Values{"name": {"web"}, "hosts": {"web01 app\nweb02 app"}})
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range []string{"1.22.1", "1.24.0"} {
		if _, err := st.Assert(ctx, model.Assertion{CIID: web01, Attr: "nginx.version", Raw: v, SourceID: src,
			ObservedAt: time.Now().Add(time.Duration(i-2) * time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.RecordReadFailure(ctx, store.ReadFailure{CIID: web01, Attr: "file.hardening", SourceID: src,
		Error: "cannot look inside /etc/ssh as this user", LastAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutRule(ctx, store.Rule{Name: "nginx 1.24", Scope: `stack == "web"`,
		Expect: `nginx.version >= "1.24"`}, "t"); err != nil {
		t.Fatal(err)
	}
	return h, st, web01
}

// TestAPIDescribesEveryRoute: the index and the OpenAPI document list the
// same endpoints the server answers, so a generated client is complete.
func TestAPIDescribesEveryRoute(t *testing.T) {
	_, h, _ := testServer(t)
	var spec struct {
		OpenAPI string                    `json:"openapi"`
		Paths   map[string]map[string]any `json:"paths"`
	}
	if err := json.Unmarshal(openAPISpec, &spec); err != nil {
		t.Fatalf("openapi.json: %v", err)
	}
	if !strings.HasPrefix(spec.OpenAPI, "3.") {
		t.Errorf("openapi %q", spec.OpenAPI)
	}
	for _, rt := range apiRoutes {
		if _, ok := spec.Paths[rt.Path]["get"]; !ok {
			t.Errorf("%s is served but not described", rt.Path)
		}
	}
	for p := range spec.Paths {
		found := false
		for _, rt := range apiRoutes {
			found = found || rt.Path == p
		}
		if !found {
			t.Errorf("%s is described but not served", p)
		}
	}
	idx := apiGet(t, h, "/api/v1", http.StatusOK)
	if eps, _ := idx["endpoints"].([]any); len(eps) != len(apiRoutes) || idx["read_only"] != true {
		t.Errorf("index: %v", idx)
	}
	apiGet(t, h, "/api/v1/", http.StatusOK)
	if rec := get(t, h, "/api/v1/openapi.json", nil); rec.Code != 200 || !json.Valid(rec.Body.Bytes()) {
		t.Errorf("openapi.json: %d", rec.Code)
	}
}

func TestAPIHosts(t *testing.T) {
	h, _, web01 := apiEstate(t)

	all := apiGet(t, h, "/api/v1/hosts", http.StatusOK)
	if all["total"].(float64) != 3 || len(items(t, all)) != 3 {
		t.Fatalf("all hosts: %v", all)
	}

	q := url.QueryEscape(`stack == "web" && nginx.version >= "1.24"`)
	got := items(t, apiGet(t, h, "/api/v1/hosts?q="+q+"&fields=nginx.version,os.version", http.StatusOK))
	if len(got) != 1 || got[0]["name"] != "web01" {
		t.Fatalf("query: %v", got)
	}
	vals := got[0]["values"].(map[string]any)
	if vals["nginx.version"] != "1.24.0" || vals["os.version"] != nil || len(vals) != 2 {
		t.Errorf("fields: %v", vals)
	}

	// Paging walks every host once.
	p1 := apiGet(t, h, "/api/v1/hosts?limit=2", http.StatusOK)
	next, _ := p1["next"].(string)
	if len(items(t, p1)) != 2 || !strings.Contains(next, "offset=2") {
		t.Fatalf("page 1: %v", p1)
	}
	p2 := apiGet(t, h, next, http.StatusOK)
	if len(items(t, p2)) != 1 || p2["next"] != nil {
		t.Errorf("page 2: %v", p2)
	}

	bad := apiGet(t, h, "/api/v1/hosts?q="+url.QueryEscape(`os.family ==`), http.StatusBadRequest)
	if e := bad["error"].(map[string]any); e["status"].(float64) != 400 || !strings.Contains(e["message"].(string), "query") {
		t.Errorf("bad query: %v", bad)
	}
	apiGet(t, h, "/api/v1/hosts?limit=0", http.StatusBadRequest)

	// One host, by name or by id.
	one := apiGet(t, h, "/api/v1/hosts/web01", http.StatusOK)
	if one["id"] != web01 {
		t.Errorf("by name: %v", one["id"])
	}
	apiGet(t, h, "/api/v1/hosts/"+web01, http.StatusOK)
	facts := one["facts"].([]any)
	if len(facts) == 0 {
		t.Fatal("no facts")
	}
	unk := one["unknown"].([]any)
	if len(unk) != 1 || !strings.Contains(unk[0].(map[string]any)["error"].(string), "cannot look inside") {
		t.Errorf("unknown: %v", unk)
	}
	if st := one["stacks"].([]any); len(st) != 1 {
		t.Errorf("stacks: %v", st)
	}
	conns := one["connections"].([]any)
	if len(conns) != 1 || conns[0].(map[string]any)["user"] != "u" {
		t.Errorf("connections: %v", conns)
	}
	if body := get(t, h, "/api/v1/hosts/web01", nil).Body.String(); strings.Contains(body, "agent:") || strings.Contains(body, "credential") {
		t.Error("the host shows its credential reference")
	}
	past := apiGet(t, h, "/api/v1/hosts/web01?at="+time.Now().Add(-90*time.Minute).Format("2006-01-02T15:04"), http.StatusOK)
	if past["as_of"] == nil || past["unknown"] != nil {
		t.Errorf("as of: %v", past)
	}
	apiGet(t, h, "/api/v1/hosts/nosuch", http.StatusNotFound)

	ch := items(t, apiGet(t, h, "/api/v1/hosts/web01/changes", http.StatusOK))
	if len(ch) != 1 || ch[0]["old"] != "1.22.1" || ch[0]["new"] != "1.24.0" {
		t.Errorf("host changes: %v", ch)
	}
}

func TestAPIEverythingElse(t *testing.T) {
	h, _, _ := apiEstate(t)

	st := apiGet(t, h, "/api/v1/status", http.StatusOK)
	if st["hosts"].(float64) != 3 {
		t.Errorf("status: %v", st)
	}

	stacks := items(t, apiGet(t, h, "/api/v1/stacks", http.StatusOK))
	if len(stacks) != 1 || stacks[0]["name"] != "web" || stacks[0]["members"].(float64) != 2 {
		t.Errorf("stacks: %v", stacks)
	}
	web := apiGet(t, h, "/api/v1/stacks/web", http.StatusOK)
	if ms := web["members"].([]any); len(ms) != 2 || ms[0].(map[string]any)["role"] != "app" {
		t.Errorf("stack: %v", web)
	}
	apiGet(t, h, "/api/v1/stacks/nosuch", http.StatusNotFound)

	attrs := items(t, apiGet(t, h, "/api/v1/attributes", http.StatusOK))
	if len(attrs) == 0 {
		t.Error("no attributes")
	}
	dist := apiGet(t, h, "/api/v1/attributes/nginx.version", http.StatusOK)
	if dist["hosts"].(float64) != 3 {
		t.Errorf("distribution counts %v hosts", dist["hosts"])
	}
	// A bucket's hosts_url lists exactly its hosts.
	for _, b := range dist["values"].([]any) {
		bk := b.(map[string]any)
		if bk["hosts"].(float64) == 0 {
			continue
		}
		listed := apiGet(t, h, bk["hosts_url"].(string), http.StatusOK)
		if listed["total"] != bk["hosts"] {
			t.Errorf("%v lists %v hosts, counted %v", bk["hosts_url"], listed["total"], bk["hosts"])
		}
	}

	ch := items(t, apiGet(t, h, "/api/v1/changes?since=24h", http.StatusOK))
	if len(ch) != 1 || ch[0]["host"] != "web01" {
		t.Errorf("changes: %v", ch)
	}
	if ch := items(t, apiGet(t, h, "/api/v1/changes?since=24h&host=db01", http.StatusOK)); len(ch) != 0 {
		t.Errorf("db01 changes: %v", ch)
	}
	apiGet(t, h, "/api/v1/changes?since=yesterday", http.StatusBadRequest)

	rules := items(t, apiGet(t, h, "/api/v1/rules", http.StatusOK))
	if len(rules) != 1 || rules[0]["done"].(float64) != 1 || rules[0]["unknown"].(float64) != 1 || rules[0]["percent_done"].(float64) != 50 {
		t.Errorf("rules: %v", rules)
	}
	rule := apiGet(t, h, "/api/v1/rules/"+url.PathEscape("nginx 1.24"), http.StatusOK)
	done := rule["done"].([]any)
	if len(done) != 1 || done[0].(map[string]any)["name"] != "web01" {
		t.Errorf("rule done: %v", rule)
	}
	apiGet(t, h, "/api/v1/rules/999", http.StatusNotFound)

	if cs := items(t, apiGet(t, h, "/api/v1/collectors", http.StatusOK)); len(cs) == 0 || cs[0]["schedule"] == "" {
		t.Errorf("collectors: %v", cs)
	}
	apiGet(t, h, "/api/v1/runs", http.StatusOK)

	// Unknown paths and writes answer JSON, not the HTML pages.
	apiGet(t, h, "/api/v1/nosuch", http.StatusNotFound)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/hosts", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed || !strings.Contains(rec.Body.String(), "read-only") {
		t.Errorf("POST: %d %s", rec.Code, rec.Body.String())
	}
}
