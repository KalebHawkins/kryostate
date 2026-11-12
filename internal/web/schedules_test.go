package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// seedChanges gives appnode2 an os.version that moved from 9.4 to 9.5 two
// hours ago, and one that moved from 9.3 to 9.4 five days ago on appnode1.
// It returns the two host ids.
func seedChanges(t *testing.T, st *store.Store) (string, string) {
	t.Helper()
	ctx := context.Background()
	col, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	mk := func(label string) string {
		id, _, err := st.ResolveCI(ctx, model.KindHost, label, []model.Identity{{Scheme: model.IdentHostname, Value: label}})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	put := func(ci, raw string, ago time.Duration) {
		t.Helper()
		if _, err := st.Assert(ctx, model.Assertion{CIID: ci, Attr: "os.version", Raw: raw, SourceID: col,
			SpecID: "linux.os.version", ObservedAt: time.Now().Add(-ago)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	a, b := mk("chg-old"), mk("chg-new")
	put(a, "9.3-old", 20*24*time.Hour)
	put(a, "9.4-mid", 5*24*time.Hour)
	put(b, "9.4-before", 10*24*time.Hour)
	put(b, "9.5-after", 2*time.Hour)
	return a, b
}

// TestOverviewSaysWhetherTheEstateIsOK: failing hosts with their error,
// recent changes and upcoming runs, and no collect button or disagreements.
func TestOverviewSaysWhetherTheEstateIsOK(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	seed(t, st)
	_, newer := seedChanges(t, st)
	if err := st.RecordProbe(ctx, model.Probe{CIID: newer, Kind: model.ProbeCollectorRun,
		Outcome: model.OutcomeAuthFail, Detail: "ssh: unable to authenticate"}); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/", nil).Body.String()
	for _, want := range []string{
		"chg-new", "ssh: unable to authenticate", // failing host and why
		"9.4-before", "9.5-after", // what changed in the last day
		"Coming up", "Operating system (built in)", // what runs next
		"collected ok", "failing", "never collected", "no connection", "stale",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "9.3-old") {
		t.Error("a change older than a day is on the overview")
	}
	if strings.Contains(body, `hx-post="/collect"`) {
		t.Error("the overview still has the collect button")
	}
	if strings.Contains(body, "Open disagreements") || strings.Contains(body, "the document and the machine disagree") {
		t.Error("the overview still shows disagreements")
	}
}

func TestRunsPageCarriesManualCollection(t *testing.T) {
	_, h, st := testServer(t)
	if _, err := st.StartRunAs(context.Background(), "scheduled: catalog.cpu.cores", "schedule", 3); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/runs", nil).Body.String()
	for _, want := range []string{`hx-post="/collect"`, `sse-connect="/events"`, "catalog.cpu.cores", "chip--sched"} {
		if !strings.Contains(body, want) {
			t.Errorf("runs page lacks %q", want)
		}
	}
}

func TestSchedulesPageAndForm(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()

	body := get(t, h, "/schedules", nil).Body.String()
	for _, want := range []string{"builtin.os", "Operating system (built in)", "daily", `action="/schedules"`, "-no-schedule"} {
		if !strings.Contains(body, want) {
			t.Errorf("schedules page lacks %q", want)
		}
	}
	if !strings.Contains(get(t, h, "/", nil).Body.String(), `href="/schedules"`) {
		t.Error("no link to the schedules page")
	}

	rec := post(t, h, "/schedules", url.Values{"collector": {"builtin.os"}, "mode": {"every"}, "every": {"2d"}, "at": {"3:30"}})
	page, msg := followFlash(t, h, rec)
	if !strings.Contains(msg, "every 2d at 03:30") {
		t.Errorf("flash: %q", msg)
	}
	if !strings.Contains(page, "every 2d at 03:30") {
		t.Error("the page does not show the new schedule")
	}
	got, _ := st.Schedules(ctx)
	if got["builtin.os"].Every != 48*time.Hour || got["builtin.os"].At != "03:30" || got["builtin.os"].UpdatedBy != "operator@example.invalid" {
		t.Errorf("stored: %+v", got["builtin.os"])
	}

	post(t, h, "/schedules", url.Values{"collector": {"builtin.hw"}, "mode": {"manual"}})
	if got, _ := st.Schedules(ctx); !got["builtin.hw"].Manual() {
		t.Error("manual only not stored")
	}
	post(t, h, "/schedules", url.Values{"collector": {"builtin.os"}, "mode": {"default"}})
	if got, _ := st.Schedules(ctx); len(got) != 1 {
		t.Errorf("default did not clear the schedule: %+v", got)
	}

	// Refusals: too often, a time with an hourly interval, a collector that
	// does not exist. None is stored.
	for _, form := range []url.Values{
		{"collector": {"builtin.os"}, "mode": {"every"}, "every": {"1m"}},
		{"collector": {"builtin.os"}, "mode": {"every"}, "every": {"6h"}, "at": {"02:00"}},
		{"collector": {"spec.rm -rf /"}, "mode": {"every"}, "every": {"1h"}},
	} {
		_, msg := followFlash(t, h, post(t, h, "/schedules", form))
		if !strings.Contains(msg, "flash--error") {
			t.Errorf("%v accepted: %q", form, msg)
		}
	}
	if got, _ := st.Schedules(ctx); len(got) != 1 {
		t.Errorf("a refused form stored something: %+v", got)
	}

	// Cross-site posts are refused like every other form.
	form := url.Values{"collector": {"builtin.os"}, "mode": {"manual"}}
	req := httptest.NewRequest(http.MethodPost, "/schedules", strings.NewReader(form.Encode()))
	req.Host = "127.0.0.1:8099"
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	cross := httptest.NewRecorder()
	h.ServeHTTP(cross, req)
	if cross.Code != http.StatusForbidden {
		t.Errorf("cross-site schedule change returned %d", cross.Code)
	}
}

func TestChangesPageWindows(t *testing.T) {
	_, h, st := testServer(t)
	older, _ := seedChanges(t, st)

	day := tableBody(get(t, h, "/changes", nil).Body.String())
	if !strings.Contains(day, "9.5-after") || strings.Contains(day, "9.4-mid") {
		t.Errorf("24h window wrong: %s", day)
	}
	if !strings.Contains(day, "Operating system (built in)") {
		t.Error("change not attributed to its collector")
	}
	week := tableBody(get(t, h, "/changes?window=7d", nil).Body.String())
	if !strings.Contains(week, "9.5-after") || !strings.Contains(week, "9.4-mid") || !strings.Contains(week, "9.3-old") {
		t.Errorf("7d window wrong: %s", week)
	}
	one := tableBody(get(t, h, "/changes?window=30d&host="+older, nil).Body.String())
	if strings.Contains(one, "9.5-after") || !strings.Contains(one, "9.4-mid") {
		t.Errorf("host filter wrong: %s", one)
	}
	if rec := get(t, h, "/changes?host=ci_nope", nil); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host returned %d", rec.Code)
	}
}

func TestHostPageMarksChangedValues(t *testing.T) {
	_, h, st := testServer(t)
	_, newer := seedChanges(t, st)
	body := get(t, h, "/hosts/"+newer, nil).Body.String()
	row, ok := cell(body, "OS Version")
	if !ok {
		t.Fatal("no os.version row")
	}
	if !strings.Contains(row, "chip--changed") || !strings.Contains(row, "was 9.4-before") {
		t.Errorf("changed value not marked: %s", row)
	}

	quiet := seed(t, st)
	if strings.Contains(get(t, h, "/hosts/"+quiet, nil).Body.String(), "chip--changed") {
		t.Error("a host whose values never moved shows a change")
	}
}
