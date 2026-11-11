package web

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// TestCSVExportMatchesTheView: the export takes the same query string as the
// page, including the chosen columns, so what is downloaded is what was seen.
func TestCSVExportMatchesTheView(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)

	rec := get(t, h, "/hosts.csv?attr=os.version&value=RHEL+8.6&col=os.version", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("returned %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/csv") {
		t.Errorf("content type %q", ct)
	}
	recs, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 2 {
		t.Fatalf("got %d records, want a header and appnode1", len(recs))
	}
	if recs[0][2] != "os.version" || len(recs[0]) != 7 {
		t.Errorf("header %v does not carry exactly the chosen column", recs[0])
	}
	if recs[1][0] != "appnode1" || recs[1][2] != "RHEL 8.6" {
		t.Errorf("row %v", recs[1])
	}
}

// TestCSVExportDefusesFormulas: values come from documents and remote hosts,
// and a spreadsheet would execute one that starts with "=".
func TestCSVExportDefusesFormulas(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	if err := st.Claim(context.Background(), id, "app.name", "", `=HYPERLINK("http://x.invalid")`, "op", "test"); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/hosts.csv?col=app.name", nil)
	if strings.Contains(rec.Body.String(), "\n=") || strings.Contains(rec.Body.String(), ",=") {
		t.Errorf("a formula reached the export unescaped:\n%s", rec.Body.String())
	}
}

func TestColumnsAreChosenInTheURL(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)
	body := get(t, h, "/hosts?col=os.version", nil).Body.String()
	if !strings.Contains(body, "<th scope=\"col\">"+attrLabel("os.version")+"</th>") {
		t.Error("the chosen column is not in the table")
	}
	if strings.Contains(body, "<th scope=\"col\">"+attrLabel("site")+"</th>") {
		t.Error("a default column is still shown after choosing others")
	}
}

// TestDisagreementsFilterToOneHost: the inventory's open count links here, and
// the link is useless if it shows every host's findings.
func TestDisagreementsFilterToOneHost(t *testing.T) {
	_, h, st := testServer(t)
	disputed := seed(t, st)
	other, _, err := st.ResolveCI(context.Background(), model.KindHost, "otherhost",
		[]model.Identity{{Scheme: model.IdentHostname, Value: "otherhost"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.RaiseDiscrepancy(context.Background(), model.Discrepancy{
		CIID: other, Kind: model.DiscUndocumented, Summary: "nobody wrote this one down",
	}); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/discrepancies?ci="+disputed, nil).Body.String()
	if !strings.Contains(body, "appnode1") || strings.Contains(body, "nobody wrote this one down") {
		t.Error("the per-host view does not show exactly that host's findings")
	}
	if !strings.Contains(get(t, h, "/hosts", nil).Body.String(), "/discrepancies?ci="+disputed) {
		t.Error("the inventory's open count does not link to that host's findings")
	}
	if rec := get(t, h, "/discrepancies?ci=nonexistent", nil); rec.Code != http.StatusNotFound {
		t.Errorf("an unknown host returned %d", rec.Code)
	}
}

// TestHostPageListsOnlyOpenFindings: the heading says "open".
func TestHostPageListsOnlyOpenFindings(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	ctx := context.Background()
	open, err := st.Discrepancies(ctx, store.DiscrepancyFilter{CIID: id}, 0)
	if err != nil || len(open) != 1 {
		t.Fatalf("seed: %v, %d", err, len(open))
	}
	if err := st.Dispose(ctx, open[0].ID, model.DispAccepted, "op", "fine"); err != nil {
		t.Fatal(err)
	}
	if body := get(t, h, "/hosts/"+id, nil).Body.String(); strings.Contains(body, "Open against this host") {
		t.Error("a settled disagreement is listed as open on the host page")
	}
}

func TestClaimFromTheHostPage(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)

	claim := func(form url.Values) *httptest.ResponseRecorder {
		return post(t, h, "/hosts/"+id+"/claim", form)
	}
	if _, msg := followFlash(t, h, claim(url.Values{"attr": {"env"}, "value": {"production"}})); !strings.Contains(msg, "reason") {
		t.Errorf("a claim with no reason gave %q", msg)
	}
	if _, msg := followFlash(t, h, claim(url.Values{"attr": {"env"}, "value": {"production"}, "reason": {"ticket 7"}})); !strings.Contains(msg, "Recorded env = production") {
		t.Fatalf("claim gave %q", msg)
	}
	facts, err := st.Facts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range facts {
		if f.Attr == "env" {
			if f.Raw != "production" || f.SourceName != "operator@example.invalid" {
				t.Errorf("env is %q from %q", f.Raw, f.SourceName)
			}
			return
		}
	}
	t.Error("the claim was not recorded")
}
