package web

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestOverviewSaysWhetherTheEstateIsOK: a headline, the problems with links
// to the hosts behind them, failing hosts with their error, recent changes
// and upcoming runs, and no collect button or disagreements.
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
		"ov-banner--bad", "1 of 5 hosts failing", // the headline
		"Needs attention",
		`href="/hosts?health=failing"`, `href="/hosts?health=noconn"`, `href="/hosts?health=never"`,
		"chg-new", "ssh: unable to authenticate", // failing host and why
		"9.4-before", "9.5-after", // what changed in the last day
		"Next runs", "Operating system (built in)", // what runs next
		"Estate at a glance", "By operating system", "By stack", "By application",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	// Only buckets with hosts in them are listed: every host here was
	// collected recently or never, so none is out of date.
	if strings.Contains(body, "health=stale") {
		t.Error("an empty bucket is listed")
	}
	for _, gone := range []string{"9.3-old", `hx-post="/collect"`, "Open disagreements", "not from rows in a table", "stat__value"} {
		if strings.Contains(body, gone) {
			t.Errorf("overview still shows %q", gone)
		}
	}
}

func TestOverviewBanner(t *testing.T) {
	last := &store.RunSummary{Started: time.Now().Add(-38 * time.Minute), Finished: time.Now().Add(-30 * time.Minute), Succeeded: 4}
	running := &store.RunSummary{Started: time.Now().Add(-3 * time.Minute)}
	for _, c := range []struct {
		h         store.Health
		attention int
		last      *store.RunSummary
		tone      string
		headline  string
		detail    string
	}{
		{store.Health{}, 0, nil, "none", "No hosts yet", "add one from the inventory"},
		{store.Health{Hosts: 4, OK: 4}, 0, last, "ok", "All 4 hosts collected successfully", "last run 38m ago: 4 succeeded, 0 failed"},
		{store.Health{Hosts: 1, OK: 1}, 0, last, "ok", "The only host collected successfully", "last run 38m ago: 4 succeeded, 0 failed"},
		{store.Health{Hosts: 120, OK: 117, Failing: 3}, 3, last, "bad", "3 of 120 hosts failing", "last run 38m ago: 4 succeeded, 0 failed"},
		{store.Health{Hosts: 120, OK: 115, Failing: 3, Never: 2}, 5, last, "bad", "3 of 120 hosts failing · 2 more need attention", "last run 38m ago: 4 succeeded, 0 failed"},
		{store.Health{Hosts: 10, OK: 10, Stale: 2}, 2, last, "warn", "2 of 10 hosts need attention", "last run 38m ago: 4 succeeded, 0 failed"},
		{store.Health{Hosts: 3, Never: 3, NoConnection: 3}, 3, nil, "warn", "3 of 3 hosts need attention", "nothing collected yet"},
		{store.Health{Hosts: 4, OK: 4}, 0, running, "ok", "All 4 hosts collected successfully", "a run is in progress, started 3m ago"},
	} {
		b := overviewBanner(c.h, c.attention, c.last)
		if b.Tone != c.tone || b.Headline != c.headline || b.Detail != c.detail {
			t.Errorf("%+v/%d: got %+v, want %s %q %q", c.h, c.attention, b, c.tone, c.headline, c.detail)
		}
	}
}

// TestNeedsAttentionLinksListThoseHosts: each bucket on the overview links
// to an inventory holding exactly the hosts it counted.
func TestNeedsAttentionLinksListThoseHosts(t *testing.T) {
	s, h, st := testServer(t)
	ctx := context.Background()
	mk := func(label string, conn bool, outcome model.Outcome, ago time.Duration) {
		t.Helper()
		id, _, err := st.ResolveCI(ctx, model.KindHost, label, []model.Identity{{Scheme: model.IdentHostname, Value: label}})
		if err != nil {
			t.Fatal(err)
		}
		if conn {
			if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: label,
				Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
				t.Fatal(err)
			}
		}
		if outcome != "" {
			if err := st.RecordProbe(ctx, model.Probe{CIID: id, Kind: model.ProbeCollectorRun,
				Outcome: outcome, At: time.Now().Add(-ago)}); err != nil {
				t.Fatal(err)
			}
		}
	}
	mk("fine01", true, model.OutcomeOK, time.Hour)
	mk("broken01", true, model.OutcomeAuthFail, time.Hour)
	mk("fresh01", true, "", 0)
	mk("orphan01", false, model.OutcomeOK, 60*24*time.Hour)

	staleAfter := s.staleAfter(ctx)
	if staleAfter >= 60*24*time.Hour {
		t.Fatalf("stale window %v is too long for this test", staleAfter)
	}
	body := get(t, h, "/", nil).Body.String()
	for bucket, want := range map[string]string{
		store.HealthFailing: "broken01",
		store.HealthNever:   "fresh01",
		store.HealthNoConn:  "orphan01",
		store.HealthStale:   "orphan01",
	} {
		if !strings.Contains(body, `href="/hosts?health=`+bucket+`"`) {
			t.Errorf("overview has no link for %s", bucket)
		}
		inv := get(t, h, "/hosts?health="+bucket, nil).Body.String()
		for _, host := range []string{"fine01", "broken01", "fresh01", "orphan01"} {
			if got := strings.Contains(inv, ">"+host+"</a>"); got != (host == want) {
				t.Errorf("/hosts?health=%s lists %s: %v", bucket, host, got)
			}
		}
		if !strings.Contains(inv, healthLabels[bucket]) {
			t.Errorf("/hosts?health=%s does not say what it is filtered to", bucket)
		}
	}
	if !strings.Contains(body, "1 of 4 hosts failing · 2 more need attention") {
		t.Error("the headline does not count each host once")
	}
	if inv := get(t, h, "/hosts?health=bogus", nil).Body.String(); strings.Contains(inv, "fine01") {
		t.Error("an unrecognised health filter listed hosts")
	}
}

// TestOverviewAllWell: with nothing wrong the page says so and lists no
// problems.
func TestOverviewAllWell(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	id, _, err := st.ResolveCI(ctx, model.KindHost, "web01", []model.Identity{{Scheme: model.IdentHostname, Value: "web01"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: "web01",
		Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if err := st.RecordProbe(ctx, model.Probe{CIID: id, Kind: model.ProbeCollectorRun, Outcome: model.OutcomeOK, At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	sid, err := st.EnsureStack(ctx, "pay & co")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.AddToStack(ctx, sid, id, "app"); err != nil {
		t.Fatal(err)
	}
	body := get(t, h, "/", nil).Body.String()
	for _, want := range []string{"ov-banner--ok", "The only host collected successfully", "Nothing. Every host",
		`href="/hosts?stack=pay&#43;%26&#43;co"`} {
		if !strings.Contains(body, want) {
			t.Errorf("overview lacks %q", want)
		}
	}
	if strings.Contains(body, "health=") {
		t.Error("a problem is listed when there is none")
	}
}

func TestClockText(t *testing.T) {
	now := time.Date(2026, 11, 12, 9, 30, 0, 0, time.Local)
	for _, c := range []struct {
		t    time.Time
		want string
	}{
		{time.Time{}, "—"},
		{now.Add(-time.Minute), "due now"},
		{time.Date(2026, 11, 12, 14, 0, 0, 0, time.Local), "14:00 today"},
		{time.Date(2026, 11, 13, 2, 15, 0, 0, time.Local), "02:15 tomorrow"},
		{time.Date(2026, 11, 16, 8, 0, 0, 0, time.Local), "Mon 08:00"},
		{time.Date(2026, 12, 1, 8, 0, 0, 0, time.Local), "1 Dec 08:00"},
	} {
		if got := clockText(c.t, now); got != c.want {
			t.Errorf("clockText(%v) = %q, want %q", c.t, got, c.want)
		}
	}
}
