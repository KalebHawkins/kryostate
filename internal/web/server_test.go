package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/hearsay/internal/collect"
	"github.com/KalebHawkins/hearsay/internal/engine"
	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/normalize"
	"github.com/KalebHawkins/hearsay/internal/store"
)

func testServer(t *testing.T) (*Server, http.Handler, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })

	n, err := normalize.Builtin()
	if err != nil {
		t.Fatal(err)
	}
	specs, err := collect.BuiltinSpecs()
	if err != nil {
		t.Fatal(err)
	}
	eng := &engine.Engine{Store: st, Norm: n, Specs: specs}
	s, err := New(st, eng, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	return s, s.Routes(), st
}

// seed puts enough in the store that the pages have something to be wrong
// about: two hosts that agree, one the document and the collector disagree
// over, and one nobody has ever reached.
func seed(t *testing.T, st *store.Store) string {
	t.Helper()
	ctx := context.Background()
	at := time.Now().Add(-time.Hour)

	doc, err := st.SourceID(ctx, model.SourceDocument, "inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	col, err := st.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}

	disputed, _, err := st.ResolveCI(ctx, model.KindHost, "appnode1", []model.Identity{
		{Scheme: model.IdentHostname, Value: "appnode1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	agreed, _, err := st.ResolveCI(ctx, model.KindHost, "appnode2", []model.Identity{
		{Scheme: model.IdentHostname, Value: "appnode2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	silent, _, err := st.ResolveCI(ctx, model.KindHost, "appnode3", []model.Identity{
		{Scheme: model.IdentHostname, Value: "appnode3"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = silent

	assert := func(ci, raw string, src int64, norm string) {
		t.Helper()
		var n *store.Normalized
		if norm != "" {
			n = &store.Normalized{Value: norm, Family: "RHEL", Version: 1, RuleID: "rhel"}
		}
		if _, err := st.Assert(ctx, model.Assertion{
			CIID: ci, Attr: "os.version", Raw: raw, SourceID: src, ObservedAt: at,
		}, n); err != nil {
			t.Fatal(err)
		}
	}
	assert(disputed, "RHEL 7.9", doc, "RHEL 7.9")
	assert(disputed, "Red Hat Enterprise Linux 8.6 (Ootpa)", col, "RHEL 8.6")
	assert(agreed, "Red Hat Enterprise Linux 7.9 (Maipo)", col, "RHEL 7.9")

	if err := st.RaiseDiscrepancy(ctx, model.Discrepancy{
		CIID: disputed, Attr: "os.version", Kind: model.DiscValueConflict,
		Summary: "the document and the machine disagree",
		Claimed: "RHEL 7.9", ClaimedBy: "inventory.md",
		Observed: "RHEL 8.6", ObservedBy: "ssh", DetectedAt: at,
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, model.Probe{
		CIID: disputed, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeOK, At: at,
	}); err != nil {
		t.Fatal(err)
	}
	return disputed
}

func get(t *testing.T, h http.Handler, target string, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestOneHandlerServesBothAPageAndAFragment is the HTMX failure this design
// exists to avoid: the browser caches whatever came back for a URL, and
// pressing back later paints a bare table where a page should be.
func TestOneHandlerServesBothAPageAndAFragment(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)

	page := get(t, h, "/hosts", nil)
	if page.Code != http.StatusOK {
		t.Fatalf("GET /hosts returned %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, "<!doctype html") && !strings.Contains(body, "<!DOCTYPE html") {
		t.Error("a direct request did not get a whole document")
	}
	if !strings.Contains(body, "appnode1") {
		t.Error("the host table has no hosts in it")
	}

	frag := get(t, h, "/hosts", map[string]string{"HX-Request": "true"})
	fb := frag.Body.String()
	if strings.Contains(strings.ToLower(fb), "<html") {
		t.Error("an HTMX request got a whole document, which would nest one page inside another")
	}
	if !strings.Contains(fb, "appnode1") {
		t.Error("the fragment is missing the content it was asked for")
	}

	for _, rec := range []*httptest.ResponseRecorder{page, frag} {
		if got := rec.Header().Get("Vary"); !strings.Contains(got, "HX-Request") {
			t.Errorf("Vary is %q; a cache cannot tell the page and the fragment apart", got)
		}
	}

	// A history restore asks with HX-Request set but needs the whole page
	// back, because it is repainting the document, not swapping into it.
	restore := get(t, h, "/hosts", map[string]string{
		"HX-Request": "true", "HX-History-Restore-Request": "true",
	})
	if !strings.Contains(strings.ToLower(restore.Body.String()), "<html") {
		t.Error("a history restore got a fragment, so the back button paints a bare table")
	}
}

// TestFilterStateLivesInTheURL. A filtered view that cannot be pasted into a
// message is a view two people cannot discuss.
func TestFilterStateLivesInTheURL(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)

	cases := []struct {
		query    string
		contains []string
		excludes []string
	}{
		{"?attr=os.version&value=RHEL+8.6", []string{"appnode1"}, []string{"appnode2", "appnode3"}},
		{"?attr=os.version&value=RHEL+7.9", []string{"appnode2"}, []string{"appnode1", "appnode3"}},
		{"?unknown=os.version", []string{"appnode3"}, []string{"appnode2"}},
		{"?contested=1", []string{"appnode1"}, []string{"appnode2", "appnode3"}},
		{"?q=appnode2", []string{"appnode2"}, []string{"appnode1", "appnode3"}},
	}
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			rec := get(t, h, "/hosts"+tc.query, nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("returned %d", rec.Code)
			}
			body := rec.Body.String()
			rows := tableBody(body)
			for _, want := range tc.contains {
				if !strings.Contains(rows, want) {
					t.Errorf("filtered table is missing %s", want)
				}
			}
			for _, no := range tc.excludes {
				if strings.Contains(rows, no) {
					t.Errorf("filtered table still lists %s", no)
				}
			}
		})
	}
}

// TestChartBarsLinkToTheHostsTheyCount. A chart that cannot be clicked through
// is a picture; the number has to lead to the list behind it.
func TestChartBarsLinkToTheHostsTheyCount(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)

	rec := get(t, h, "/charts", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /charts returned %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") {
		t.Fatal("no chart was rendered")
	}
	if strings.Contains(body, "<script src=\"http") || strings.Contains(body, "cdn.") {
		t.Error("the page reaches out to the internet to draw a bar chart")
	}

	wantLinks := []string{
		"/hosts?attr=os.version&amp;value=RHEL+8.6",
		"/hosts?unknown=os.version",
	}
	for _, want := range wantLinks {
		if !strings.Contains(body, want) {
			t.Errorf("no bar links to %s", want)
		}
	}

	// Follow one of the links the page actually produced and check it lands
	// on the hosts it claimed to count.
	target := strings.ReplaceAll("/hosts?attr=os.version&amp;value=RHEL+8.6", "&amp;", "&")
	if _, err := url.Parse(target); err != nil {
		t.Fatal(err)
	}
	follow := get(t, h, target, map[string]string{"HX-Request": "true"})
	rows := tableBody(follow.Body.String())
	if !strings.Contains(rows, "appnode1") {
		t.Error("the bar counted a host the link does not show")
	}
	if strings.Contains(rows, "appnode2") {
		t.Error("the link shows a host the bar did not count")
	}
}

// TestEveryHostIsAccountedForOnTheChartsPage. A chart whose bars do not add up
// to the host count reports better coverage than it has.
func TestEveryHostIsAccountedForOnTheChartsPage(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)

	buckets, err := st.Distribution(context.Background(), "os.version")
	if err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, b := range buckets {
		total += b.Count
	}
	if total != 3 {
		t.Fatalf("buckets cover %d of 3 hosts", total)
	}

	body := get(t, h, "/charts", nil).Body.String()
	if !strings.Contains(body, "never collected") && !strings.Contains(body, "never") {
		t.Error("the chart does not show the host nobody has tried")
	}
}

// TestDisposingADiscrepancyRecordsWhoDidIt. A decision with no name on it is
// not an audit trail.
func TestDisposingADiscrepancyRecordsWhoDidIt(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)
	ctx := context.Background()

	open, err := st.Discrepancies(ctx, store.DiscrepancyFilter{Disposition: model.DispOpen}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(open) != 1 {
		t.Fatalf("expected one discrepancy to act on, got %d", len(open))
	}

	form := url.Values{"disposition": {string(model.DispConfirmMachine)}, "note": {"rebuilt last month"}}
	req := httptest.NewRequest(http.MethodPost,
		"/discrepancies/"+itoa(open[0].ID)+"/dispose", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("X-Forwarded-User", "operator@example.invalid")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("dispose returned %d: %s", rec.Code, rec.Body.String())
	}
	after, err := st.Discrepancies(ctx, store.DiscrepancyFilter{Disposition: model.DispOpen}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != 0 {
		t.Errorf("the disposed discrepancy is still shown as open")
	}

	all, err := st.Discrepancies(ctx, store.DiscrepancyFilter{Disposition: model.DispConfirmMachine}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 {
		t.Fatalf("the decision was not recorded")
	}
	if all[0].DisposedBy != "operator@example.invalid" {
		t.Errorf("recorded the decision as %q, not the person who made it", all[0].DisposedBy)
	}
	if all[0].Note == "" {
		t.Error("the reason given was dropped")
	}
}

// TestUnknownHostIsNotA500 covers the path a stale bookmark takes.
func TestUnknownHostIsNotA500(t *testing.T) {
	_, h, _ := testServer(t)
	rec := get(t, h, "/hosts/ci_doesnotexist", nil)
	if rec.Code != http.StatusNotFound {
		t.Errorf("a missing host returned %d, want 404", rec.Code)
	}
}

// TestStaticAssetsAreServedLocally. An estate with no route to the internet
// must still get a working page.
func TestStaticAssetsAreServedLocally(t *testing.T) {
	_, h, _ := testServer(t)
	for _, path := range []string{"/static/app.css", "/static/htmx.min.js", "/static/sse.js"} {
		rec := get(t, h, path, nil)
		if rec.Code != http.StatusOK {
			t.Errorf("%s returned %d; the page depends on something it does not ship", path, rec.Code)
		}
		if rec.Body.Len() == 0 {
			t.Errorf("%s is empty", path)
		}
	}
	body := get(t, h, "/", nil).Body.String()
	if strings.Contains(body, "//unpkg.com") || strings.Contains(body, "//cdn.") {
		t.Error("the layout loads an asset from the internet")
	}
}

// TestHomeIsTheDisagreements, not the host table. What a person needs on
// opening this is what is wrong, not what exists.
func TestHomeIsTheDisagreements(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)
	body := get(t, h, "/", nil).Body.String()
	if !strings.Contains(body, "the document and the machine disagree") {
		t.Error("the landing page does not lead with the disagreements")
	}
}

// tableBody narrows a page to the rows of its table, so a filter test is not
// satisfied by a hostname appearing in a filter chip or a heading.
func tableBody(html string) string {
	i := strings.Index(html, "<tbody")
	if i < 0 {
		return ""
	}
	j := strings.Index(html[i:], "</tbody>")
	if j < 0 {
		return html[i:]
	}
	return html[i : i+j]
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestFreeTextIsShownNotCalledUnparsed keeps the host page consistent with the
// chart of the same attribute. Nobody writes a normaliser rule for a role, so
// the value column must show what the source said. Labelling it "not
// understood" blames the estate for a rule this tool never wanted.
func TestFreeTextIsShownNotCalledUnparsed(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)

	ctx := context.Background()
	doc, err := st.SourceID(ctx, model.SourceDocument, "inventory.md")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.Assert(ctx, model.Assertion{
		CIID: id, Attr: "role", Raw: "loadbalancer", SourceID: doc,
		ObservedAt: time.Now().Add(-time.Hour),
	}, nil); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/hosts/"+id, nil).Body.String()
	row, ok := cell(body, "Role")
	if !ok {
		t.Fatal("the host page has no row for role")
	}
	if !strings.Contains(row, "loadbalancer") {
		t.Errorf("the role row does not show the value: %s", row)
	}
	if strings.Contains(row, "not understood") {
		t.Errorf("free text was reported as unparsed: %s", row)
	}

	// os.version does have rules, so a value that escapes them must still be
	// reported rather than quietly shown as though it were understood.
	if _, err := st.Assert(ctx, model.Assertion{
		CIID: id, Attr: "os.version", Raw: "something no rule matches",
		SourceID: doc, Decided: true, ObservedAt: time.Now(),
	}, nil); err != nil {
		t.Fatal(err)
	}
	body = get(t, h, "/hosts/"+id, nil).Body.String()
	if row, ok := cell(body, "OS Version"); !ok || !strings.Contains(row, "not understood") {
		t.Errorf("an unparsed value on a rule-bearing attribute was not flagged: %s", row)
	}
}

// cell returns the table row whose heading is label.
func cell(body, label string) (string, bool) {
	for _, row := range strings.Split(body, "<tr>") {
		if strings.Contains(row, ">"+label+"<") {
			return row, true
		}
	}
	return "", false
}
