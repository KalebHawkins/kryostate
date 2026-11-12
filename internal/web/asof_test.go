package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

func TestHostPageAsOf(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id, _, err := st.ResolveCI(ctx, model.KindHost, "node-past",
		[]model.Identity{{Scheme: model.IdentHostname, Value: "node-past"}})
	if err != nil {
		t.Fatal(err)
	}
	col, err := st.SourceID(ctx, model.SourceCollector, "ssh")
	if err != nil {
		t.Fatal(err)
	}
	for _, a := range []struct{ attr, raw, day string }{
		{"kernel", "5.14.0-old", "2026-01-10"},
		{"kernel", "5.14.0-new", "2026-04-02"},
		{"timezone", "Europe/Paris", "2026-05-01"},
	} {
		at, _ := time.ParseInLocation(time.DateOnly, a.day, time.Local)
		if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: a.attr, Raw: a.raw, SourceID: col, ObservedAt: at.Add(12 * time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}

	now := get(t, h, "/hosts/"+id, nil).Body.String()
	if !strings.Contains(now, "5.14.0-new") || strings.Contains(now, "as they were") {
		t.Error("the page without ?at= should show today's values and no banner")
	}

	past := get(t, h, "/hosts/"+id+"?at=2026-03-01", nil).Body.String()
	if !strings.Contains(past, "Showing values as they were on 1 Mar 2026") || !strings.Contains(past, "Back to now") {
		t.Error("no as-of banner")
	}
	if !strings.Contains(past, "5.14.0-old") || strings.Contains(past, "5.14.0-new") {
		t.Error("kernel on 1 Mar should be the old value only")
	}
	if strings.Contains(past, "Europe/Paris") {
		t.Error("timezone was first reported in May and should be absent on 1 Mar")
	}
	if !strings.Contains(past, `value="2026-03-01"`) {
		t.Error("the date box should keep the date asked about")
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts/"+id+"?at=yesterday", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("a date that is not one: status %d", rec.Code)
	}
}

func TestParseAt(t *testing.T) {
	d, err := ParseAt("2026-03-01")
	if err != nil {
		t.Fatal(err)
	}
	if d.Format("2006-01-02 15:04") != "2026-03-01 23:59" {
		t.Errorf("a date alone should mean the end of that day, got %v", d)
	}
	m, err := ParseAt("2026-03-01T15:04")
	if err != nil || m.Format("15:04") != "15:04" {
		t.Errorf("got %v %v", m, err)
	}
	if _, err := ParseAt("1 March"); err == nil {
		t.Error("accepted a date in an unknown form")
	}
}
