package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestScheduleFromTheCommandLine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	old := dataDir
	dataDir = ""
	t.Cleanup(func() { dataDir = old })
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.PutChoice(ctx, collect.Choice{Template: "cpu.cores"}, "t"); err != nil {
		t.Fatal(err)
	}

	if err := setSchedule(ctx, eng, "os", "1d", "2:00", false, false); err != nil {
		t.Fatal(err)
	}
	// The short catalog name is accepted.
	if err := setSchedule(ctx, eng, "cpu.cores", "", "", true, false); err != nil {
		t.Fatal(err)
	}
	got, _ := st.Schedules(ctx)
	if got["builtin.os"].Every != 24*time.Hour || got["builtin.os"].At != "02:00" {
		t.Errorf("builtin.os: %+v", got["builtin.os"])
	}
	if !got["catalog.cpu.cores"].Manual() {
		t.Errorf("cpu.cores: %+v", got["catalog.cpu.cores"])
	}
	if err := setSchedule(ctx, eng, "os", "", "", false, true); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.Schedules(ctx); len(got) != 1 {
		t.Errorf("-default did not reset: %+v", got)
	}

	for name, call := range map[string]func() error{
		"no collector":   func() error { return setSchedule(ctx, eng, "", "1d", "", false, false) },
		"nothing to do":  func() error { return setSchedule(ctx, eng, "os", "", "", false, false) },
		"two at once":    func() error { return setSchedule(ctx, eng, "os", "1d", "", true, false) },
		"at without day": func() error { return setSchedule(ctx, eng, "os", "6h", "02:00", false, false) },
		"too often":      func() error { return setSchedule(ctx, eng, "os", "1m", "", false, false) },
		"unknown":        func() error { return setSchedule(ctx, eng, "nope", "1d", "", false, false) },
	} {
		if err := call(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := listSchedules(ctx, eng); err != nil {
		t.Fatal(err)
	}

	// The listing leads with clock times and groups runs that start
	// together.
	for _, ref := range []string{"os", "hw"} {
		if err := setSchedule(ctx, eng, ref, "1d", "02:00", false, false); err != nil {
			t.Fatal(err)
		}
	}
	s := engine.NewScheduler(eng, nil)
	o, err := s.Outlook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := writeSchedules(&buf, o, s.Loc(), s.MaxSessions, s.PerHost); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{"02:00 · 2 collectors", "Manual only", "catalog.cpu.cores", "never more than\n4 sessions"} {
		if !strings.Contains(out, want) {
			t.Errorf("listing lacks %q:\n%s", want, out)
		}
	}
}
