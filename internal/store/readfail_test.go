package store

import (
	"context"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// TestFailedReadMakesTheValueUnknown: a host that answers but cannot read a
// value no longer shows the old one; the next good reading brings it back;
// a day in the past shows what was known that day, unknowns included.
func TestFailedReadMakesTheValueUnknown(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ci := mustHost(t, s, "node-unread")
	col := mustSource(t, s, model.SourceCollector, "builtin")
	other := mustSource(t, s, model.SourceCollector, "external:cmdb")

	assertValue(t, s, ci, "file.exists", "false", col, day("2026-03-01"))
	assertValue(t, s, ci, "os.version", "9.4", col, day("2026-03-01"))

	fail := ReadFailure{CIID: ci, Attr: "file.exists", SourceID: col, SpecID: "x",
		Error: "cannot look inside /etc/ssh/sshd_config.d as this user", LastAt: day("2026-03-05")}
	if err := s.RecordReadFailure(ctx, fail); err != nil {
		t.Fatal(err)
	}
	fail.LastAt = day("2026-03-06")
	if err := s.RecordReadFailure(ctx, fail); err != nil {
		t.Fatal(err)
	}

	now := factsAt(t, s, ci, time.Now())
	if _, ok := now["file.exists"]; ok {
		t.Error("a value the host could not read is still current")
	}
	if now["os.version"].Raw != "9.4" {
		t.Error("an unrelated value was hidden")
	}
	facts, _ := s.Facts(ctx, ci)
	for _, f := range facts {
		if f.Attr == "file.exists" {
			t.Error("Facts still shows the unreadable value")
		}
	}
	open, err := s.ReadFailures(ctx, ci)
	if err != nil || len(open) != 1 {
		t.Fatalf("open failures: %v %+v", err, open)
	}
	if !open[0].Since.Equal(day("2026-03-05")) || !open[0].LastAt.Equal(day("2026-03-06")) {
		t.Errorf("one episode, since the first failure: %+v", open[0])
	}

	// Another source's value is not hidden by this one's failure.
	assertValue(t, s, ci, "file.exists", "maybe", other, day("2026-03-07"))
	if got := factFor(t, s, ci, "file.exists").Raw; got != "maybe" {
		t.Errorf("other source: %q", got)
	}

	// Output replayed from before the failure does not end it.
	assertValue(t, s, ci, "file.exists", "false", col, day("2026-03-02"))
	if open, _ := s.ReadFailures(ctx, ci); len(open) != 1 {
		t.Error("an older reading closed a newer failure")
	}

	// A good reading, the same value as before, ends it.
	assertValue(t, s, ci, "file.exists", "true", col, day("2026-03-10"))
	if open, _ := s.ReadFailures(ctx, ci); len(open) != 0 {
		t.Errorf("still open: %+v", open)
	}
	if got := factFor(t, s, ci, "file.exists").Raw; got != "true" {
		t.Errorf("after recovery: %q", got)
	}

	// The past: known before, unknown during, known after.
	if got := factsAt(t, s, ci, day("2026-03-04"))["file.exists"].Raw; got != "false" {
		t.Errorf("4 Mar: %q", got)
	}
	if _, ok := factsAt(t, s, ci, day("2026-03-06"))["file.exists"]; ok {
		t.Error("6 Mar: the value was unknown then")
	}
	if got := factsAt(t, s, ci, day("2026-03-11"))["file.exists"].Raw; got != "true" {
		t.Errorf("11 Mar: %q", got)
	}
}
