package web

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/url"
	"strings"
	"testing"
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

// TestCraftedColumnsAreDropped: a column name from a link reaches the command
// line shown to copy and the spreadsheet's header, so only attribute names
// are taken.
func TestCraftedColumnsAreDropped(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)
	bad := []string{`x;touch${IFS}pwned`, `$(id)`, "a`id`", `=HYPERLINK("http://x.invalid")`, `os.version'`, "@SUM(1)"}
	q := "?col=os.version"
	for _, b := range bad {
		q += "&col=" + url.QueryEscape(b)
	}
	body := get(t, h, "/hosts"+q, nil).Body.String()
	if !strings.Contains(body, "kryostate host list --cols os.version") {
		t.Error("the command line does not name the valid column")
	}
	for _, line := range strings.Split(body, "\n") {
		if !strings.Contains(line, "kryostate host list") {
			continue
		}
		for _, b := range []string{"pwned", "$(id)", "`id`", "HYPERLINK", "os.version&#39;", "SUM(1)"} {
			if strings.Contains(line, b) {
				t.Errorf("the command line carries %q: %s", b, line)
			}
		}
	}
	recs, err := csv.NewReader(get(t, h, "/hosts.csv"+q, nil).Body).ReadAll()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs[0]) != 7 || recs[0][2] != "os.version" {
		t.Errorf("CSV header %q", recs[0])
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
