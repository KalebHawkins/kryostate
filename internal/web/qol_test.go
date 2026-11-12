package web

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/model"
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
	if recs[0][2] != "os.version" || len(recs[0]) != 6 {
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
	if err := st.SetApplication(context.Background(), id, `=HYPERLINK("http://x.invalid")`, "op", "test"); err != nil {
		t.Fatal(err)
	}
	rec := get(t, h, "/hosts.csv?col=application", nil)
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
	if strings.Contains(body, "<th scope=\"col\">"+attrLabel("application")+"</th>") {
		t.Error("a default column is still shown after choosing others")
	}
}

// TestApplicationFromTheHostPage: the application is the one attribute the
// host page lets a person state. It carries their name and needs no reason;
// the audit log notes where it was set.
func TestApplicationFromTheHostPage(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)

	page := get(t, h, "/hosts/"+id, nil).Body.String()
	if i := strings.Index(page, `id="application"`); i < 0 {
		t.Fatal("no application form on the host page")
	} else if form := page[i:]; strings.Contains(form[:strings.Index(form, "</form>")], `name="reason"`) {
		t.Error("the application form still asks for a reason")
	}

	set := func(form url.Values) *httptest.ResponseRecorder {
		return post(t, h, "/hosts/"+id+"/application", form)
	}
	if _, msg := followFlash(t, h, set(url.Values{"application": {"  "}})); !strings.Contains(msg, "name the application") {
		t.Errorf("an empty application gave %q", msg)
	}
	if _, msg := followFlash(t, h, set(url.Values{"application": {"ledger"}})); !strings.Contains(msg, "ledger") {
		t.Fatalf("setting the application gave %q", msg)
	}
	var detail string
	if err := st.DB().QueryRow(`SELECT detail FROM audit WHERE action='set application' AND subject=?`, id).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	if detail != "application=ledger: set on the host page" {
		t.Errorf("audit detail is %q", detail)
	}
	facts, err := st.Facts(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range facts {
		if f.Attr == model.AttrApplication {
			found = true
			if f.Raw != "ledger" || f.SourceName != "operator@example.invalid" {
				t.Errorf("application is %q from %q", f.Raw, f.SourceName)
			}
		}
	}
	if !found {
		t.Error("the application was not recorded")
	}
}

// TestNoArbitraryAttributesFromTheWeb: the form that let a person state any
// attribute is gone, and nothing answers in its place.
func TestNoArbitraryAttributesFromTheWeb(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	rec := post(t, h, "/hosts/"+id+"/claim", url.Values{"attr": {"env"}, "value": {"production"}, "reason": {"r"}})
	if rec.Code == http.StatusSeeOther || rec.Code == http.StatusOK {
		t.Errorf("POST /hosts/{id}/claim answered %d", rec.Code)
	}
	for _, path := range []string{"/discrepancies"} {
		if rec := get(t, h, path, nil); rec.Code != http.StatusNotFound {
			t.Errorf("%s answered %d, want 404", path, rec.Code)
		}
	}
}
