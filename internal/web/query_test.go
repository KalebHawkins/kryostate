package web

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// seedQuery adds a stack with roles and a few attributes to the usual seed.
func seedQuery(t *testing.T, st *store.Store) {
	t.Helper()
	ctx := context.Background()
	seed(t, st)
	rows, err := st.Hosts(ctx, store.HostFilter{})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, r := range rows {
		ids[r.CI.Label] = r.CI.ID
	}
	col, err := st.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	set := func(host, attr, v string) {
		t.Helper()
		if _, err := st.Assert(ctx, model.Assertion{CIID: ids[host], Attr: attr, Raw: v, SourceID: col,
			ObservedAt: time.Now().Add(-time.Minute)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	set("appnode1", "file.nginx.exists", "true")
	set("appnode2", "file.nginx.exists", "false")
	set("appnode1", "hw.cpu.cores", "16")
	set("appnode2", "hw.cpu.cores", "4")
	set("appnode1", "application", "nginx")
	sid, err := st.EnsureStack(ctx, "orders-prod")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddToStack(ctx, sid, ids["appnode1"], "backend"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddToStack(ctx, sid, ids["appnode2"], "api-gateway"); err != nil {
		t.Fatal(err)
	}
}

func TestInventoryQuery(t *testing.T) {
	_, h, st := testServer(t)
	seedQuery(t, st)
	cases := []struct {
		q        string
		has, not []string
	}{
		{`os.version == "RHEL 8.6"`, []string{"appnode1"}, []string{"appnode2", "appnode3"}},
		{`!file.exists == "/usr/sbin/nginx"`, []string{"appnode2", "appnode3"}, []string{"appnode1"}},
		{`file.exists["/usr/sbin/nginx"] == true`, []string{"appnode1"}, []string{"appnode2", "appnode3"}},
		{`stack == "orders-prod" && role == "api-gateway"`, []string{"appnode2"}, []string{"appnode1", "appnode3"}},
		{`hw.cpu.cores >= 8 || name =~ "3$"`, []string{"appnode1", "appnode3"}, []string{"appnode2"}},
		{`application == "nginx"`, []string{"appnode1"}, []string{"appnode2", "appnode3"}},
		{`node2`, []string{"appnode2"}, []string{"appnode1", "appnode3"}}, // plain text: a name search
	}
	for _, c := range cases {
		rec := get(t, h, "/hosts?q="+url.QueryEscape(c.q), map[string]string{"HX-Request": "true"})
		if rec.Code != http.StatusOK {
			t.Fatalf("%s: status %d", c.q, rec.Code)
		}
		body := rec.Body.String()
		rows := tableBody(body)
		for _, want := range c.has {
			if !strings.Contains(rows, want) {
				t.Errorf("%s: %s missing", c.q, want)
			}
		}
		for _, no := range c.not {
			if strings.Contains(rows, no) {
				t.Errorf("%s: %s listed", c.q, no)
			}
		}
		// The query is echoed back into the box.
		if !strings.Contains(body, `value="`+htmlAttr(c.q)+`"`) {
			t.Errorf("%s: the query box does not show the query", c.q)
		}
	}
}

func htmlAttr(s string) string {
	return strings.NewReplacer(`&`, "&amp;", `"`, "&#34;", `<`, "&lt;", `>`, "&gt;", `'`, "&#39;", `+`, "&#43;").Replace(s)
}

func TestInventoryQueryErrorIsShownInline(t *testing.T) {
	_, h, st := testServer(t)
	seedQuery(t, st)
	rec := get(t, h, "/hosts?q="+url.QueryEscape(`os.version = "x"`), map[string]string{"HX-Request": "true"})
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, `id="q-err"`) || !strings.Contains(body, "use ==") || !strings.Contains(body, "column 12") {
		t.Error("the parse error is not shown with its position")
	}
	if strings.Contains(tableBody(body), "appnode1") {
		t.Error("a query that does not parse still listed hosts")
	}

	for _, path := range []string{"/hosts.csv", "/hosts.json"} {
		rec := get(t, h, path+"?q="+url.QueryEscape(`(name == "x"`), nil)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "column") {
			t.Errorf("%s with a bad query: %d %q", path, rec.Code, rec.Body.String())
		}
	}
}

func TestJSONExport(t *testing.T) {
	_, h, st := testServer(t)
	seedQuery(t, st)
	rec := get(t, h, "/hosts.json?q="+url.QueryEscape(`stack == "orders-prod"`)+"&col=os.version,hw.cpu.cores,env", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content type %q", ct)
	}
	var got []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d hosts, want 2", len(got))
	}
	a := got[0]
	if a["name"] != "appnode1" || a["application"] != "nginx" {
		t.Errorf("first host: %v", a)
	}
	stacks, _ := a["stacks"].([]any)
	if len(stacks) != 1 || fmt.Sprint(stacks[0]) != "map[role:backend stack:orders-prod]" {
		t.Errorf("stacks: %v", a["stacks"])
	}
	vals, _ := a["values"].(map[string]any)
	if vals["os.version"] != "RHEL 8.6" || vals["hw.cpu.cores"] != "16" {
		t.Errorf("values: %v", vals)
	}
	if v, ok := vals["env"]; !ok || v != nil {
		t.Errorf("a column with no value should be null, got %v (present %v)", v, ok)
	}
	if _, ok := vals["file.nginx.exists"]; ok {
		t.Error("values holds an attribute that is not a chosen column")
	}

	// col=* chooses every attribute.
	rec = get(t, h, "/hosts.json?q=name+%3D%3D+%22appnode1%22&col=*", nil)
	var all []HostJSON
	if err := json.Unmarshal(rec.Body.Bytes(), &all); err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Values["file.nginx.exists"] != "true" {
		t.Errorf("col=*: %+v", all)
	}
}

func TestCSVHonoursQuery(t *testing.T) {
	_, h, st := testServer(t)
	seedQuery(t, st)
	rec := get(t, h, "/hosts.csv?q="+url.QueryEscape(`hw.cpu.cores < 8`)+"&col=hw.cpu.cores", nil)
	recs, err := csv.NewReader(strings.NewReader(rec.Body.String())).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 || recs[1][0] != "appnode2" || recs[0][2] != "hw.cpu.cores" || recs[1][2] != "4" {
		t.Errorf("csv: %v", recs)
	}
}

func TestClassify(t *testing.T) {
	b := func(pairs ...any) []store.Bucket {
		var out []store.Bucket
		for i := 0; i < len(pairs); i += 2 {
			out = append(out, store.Bucket{Key: pairs[i].(string), Label: pairs[i].(string), Count: pairs[i+1].(int)})
		}
		return out
	}
	unique := func(n int, numeric bool) []store.Bucket {
		var out []store.Bucket
		for i := 0; i < n; i++ {
			k := fmt.Sprintf("id-%04d", i)
			if numeric {
				k = fmt.Sprint(1000 + i*37)
			}
			out = append(out, store.Bucket{Key: k, Count: 1})
		}
		return out
	}
	cases := []struct {
		attr    string
		buckets []store.Bucket
		want    ChartKind
	}{
		{"os.family", b("Linux", 40, "Windows", 9), KindDonut},
		{"os.version", b("a", 5, "b", 5, "c", 5, "d", 5, "e", 5, "f", 5, "g", 5), KindBar},
		{"os.version", append(b("a", 5, "b", 5, "c", 5, "d", 5, "e", 5, "f", 5), store.Bucket{Key: "never_collected", Unknown: true, Count: 3}), KindBar},
		{"hw.cpu.cores", b("2", 3, "4", 9, "8", 9), KindDonut},
		{"hw.cpu.cores", b("1", 1, "2", 3, "4", 9, "8", 9, "12", 2, "16", 4, "32", 1), KindHist},
		{"hw.memory.total_mb", unique(30, true), KindHist},
		{"sys.machine_id", b("x", 3, "y", 4), ""},
		{"net.fqdn", b("x", 3), ""},
		{"hw.serial_number", b("x", 3), ""},
		{"net.ip", b("x", 3), ""},
		{"app.install_path", unique(30, false), ""},
		{"thing.only_unknown", []store.Bucket{{Key: "never_collected", Unknown: true, Count: 4}}, ""},
		{"site", unique(3, false), KindDonut}, // too few hosts to call it an identifier
	}
	for _, c := range cases {
		if got, _ := classify(c.attr, c.buckets); got != c.want {
			t.Errorf("classify(%s) = %q, want %q", c.attr, got, c.want)
		}
	}
}

func TestHistogramBinsAndLinks(t *testing.T) {
	var bs []store.Bucket
	for _, v := range []string{"1000", "2000", "3900", "7900", "15884", "16000", "31000"} {
		bs = append(bs, store.Bucket{Key: v, Count: 2})
	}
	c := NewChart("hw.memory.total_mb", "Memory", KindHist, bs)
	total := 0
	for _, s := range c.Card {
		total += s.Count
		if s.Href == "" || !strings.HasPrefix(s.Href, "/hosts?q=hw.memory.total_mb+%3E%3D+") {
			t.Errorf("bin %q links to %q", s.Label, s.Href)
		}
	}
	if total != 14 {
		t.Errorf("bins hold %d hosts, want 14", total)
	}
	if len(c.Card) < 4 || len(c.Card) > 12 {
		t.Errorf("%d bins", len(c.Card))
	}
}

func TestChartsPageOverlayAndSkips(t *testing.T) {
	_, h, st := testServer(t)
	seedQuery(t, st)
	ctx := context.Background()
	col, _ := st.SourceID(ctx, model.SourceCollector, "ssh")
	rows, _ := st.Hosts(ctx, store.HostFilter{})
	for i, r := range rows {
		if _, err := st.Assert(ctx, model.Assertion{CIID: r.CI.ID, Attr: "sys.machine_id", Raw: fmt.Sprintf("m%d", i),
			SourceID: col, ObservedAt: time.Now()}, nil); err != nil {
			t.Fatal(err)
		}
	}
	body := get(t, h, "/charts", nil).Body.String()
	for _, want := range []string{`<dialog class="viz-dialog"`, `data-dialog="chart-os-version"`, `<table>`, "/static/charts.js"} {
		if !strings.Contains(body, want) {
			t.Errorf("charts page lacks %s", want)
		}
	}
	notCharted := body[strings.Index(body, "Not charted"):]
	if !strings.Contains(notCharted, "sys.machine_id") {
		t.Error("an identifier was charted rather than listed as not charted")
	}
	// Asking for it by name draws it anyway.
	one := get(t, h, "/charts?attr=sys.machine_id", nil).Body.String()
	if !strings.Contains(one, "<svg") {
		t.Error("a chart asked for by name was not drawn")
	}
}
