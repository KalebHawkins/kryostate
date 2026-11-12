package store

import (
	"context"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

func factsAt(t *testing.T, s *Store, ci string, at time.Time) map[string]model.Fact {
	t.Helper()
	facts, err := s.FactsAt(context.Background(), ci, at)
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]model.Fact{}
	for _, f := range facts {
		out[f.Attr] = f
	}
	return out
}

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

// TestFactsAtShowsTheValueOfTheDay: a version that moved after the date is
// shown as it was, and an attribute nobody reported until later is absent.
func TestFactsAtShowsTheValueOfTheDay(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-asof")
	col := mustSource(t, s, model.SourceCollector, "ssh")

	assertValue(t, s, ci, "os.version", "9.3", col, day("2026-01-10"))
	assertValue(t, s, ci, "os.version", "9.4", col, day("2026-04-02"))
	assertValue(t, s, ci, "kernel", "5.14", col, day("2026-05-01"))

	then := factsAt(t, s, ci, day("2026-03-01"))
	if got := then["os.version"].Raw; got != "9.3" {
		t.Errorf("os.version on 1 Mar is %q, want 9.3", got)
	}
	if _, ok := then["kernel"]; ok {
		t.Error("kernel was first reported in May and should be absent on 1 Mar")
	}

	now := factsAt(t, s, ci, time.Now())
	if now["os.version"].Raw != "9.4" || now["kernel"].Raw != "5.14" {
		t.Errorf("as of now: %+v", now)
	}
	if got := factFor(t, s, ci, "os.version"); got.AssertionID != now["os.version"].AssertionID {
		t.Errorf("as of now disagrees with Facts: %d vs %d", now["os.version"].AssertionID, got.AssertionID)
	}

	if len(factsAt(t, s, ci, day("2025-12-31"))) != 0 {
		t.Error("nothing was known before the first report")
	}
}

// TestFactsAtKeepsTheResolutionRules: the host's report beats a document on
// the date as it does today, and a later confirmation of one source's value
// does not let it beat a source that was fresher at the time.
func TestFactsAtKeepsTheResolutionRules(t *testing.T) {
	s := testStore(t)
	ci := mustHost(t, s, "node-asof-rules")
	doc := mustSource(t, s, model.SourceDocument, "inventory.md")
	ssh := mustSource(t, s, model.SourceCollector, "ssh")
	agent := mustSource(t, s, model.SourceCollector, "agent")

	assertValue(t, s, ci, "site", "dc1", doc, day("2026-02-20"))
	assertValue(t, s, ci, "site", "dc2", ssh, day("2026-01-05"))
	if got := factsAt(t, s, ci, day("2026-03-01"))["site"].Raw; got != "dc2" {
		t.Errorf("site on 1 Mar is %q; the document beat what the host reported", got)
	}

	// ssh says A in January and keeps confirming it into summer; agent says
	// B in February. On 1 Mar agent's claim was the fresher of the two.
	assertValue(t, s, ci, "tz", "UTC", ssh, day("2026-01-05"))
	assertValue(t, s, ci, "tz", "CET", agent, day("2026-02-10"))
	assertValue(t, s, ci, "tz", "UTC", ssh, day("2026-07-01")) // a confirmation
	if got := factsAt(t, s, ci, day("2026-03-01"))["tz"].Raw; got != "CET" {
		t.Errorf("tz on 1 Mar is %q; a confirmation made later counted early", got)
	}
	if got := factsAt(t, s, ci, day("2026-08-01"))["tz"].Raw; got != "UTC" {
		t.Errorf("tz on 1 Aug is %q, want UTC", got)
	}
}
