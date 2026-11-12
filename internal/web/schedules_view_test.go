package web

import (
	"context"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestParseScheduleForm(t *testing.T) {
	ok := []struct {
		f     scheduleForm
		mode  string
		every time.Duration
		at    string
	}{
		{scheduleForm{Preset: "default"}, "default", 0, ""},
		{scheduleForm{Preset: "manual", Hour: "02"}, "manual", 0, ""},
		{scheduleForm{Preset: "15m"}, "set", 15 * time.Minute, ""},
		{scheduleForm{Preset: "6h", Hour: "02", Minute: "30"}, "set", 6 * time.Hour, ""}, // hidden boxes are ignored
		{scheduleForm{Preset: "1d"}, "set", 24 * time.Hour, ""},
		{scheduleForm{Preset: "1d", Hour: "02", Minute: "15"}, "set", 24 * time.Hour, "02:15"},
		{scheduleForm{Preset: "7d", Hour: "23"}, "set", 7 * 24 * time.Hour, "23:00"},
		{scheduleForm{Preset: "custom", Every: "90m"}, "set", 90 * time.Minute, ""},
		{scheduleForm{Preset: "custom", Every: "3d", Hour: "4", Minute: "45"}, "set", 3 * 24 * time.Hour, "04:45"},
	}
	for _, c := range ok {
		mode, sc, err := parseScheduleForm(c.f)
		if err != nil || mode != c.mode || sc.Every != c.every || sc.At != c.at {
			t.Errorf("%+v: %s %+v %v; want %s every %v at %q", c.f, mode, sc, err, c.mode, c.every, c.at)
		}
	}
	for _, bad := range []scheduleForm{
		{Preset: "1m"},                          // not a preset
		{Preset: "custom", Every: "1m"},         // shorter than MinEvery
		{Preset: "custom", Every: "40d"},        // longer than MaxEvery
		{Preset: "custom", Every: "soon"},       // not an interval
		{Preset: "1d", Hour: "24"},              // not an hour
		{Preset: "1d", Hour: "02", Minute: "x"}, // not a minute
		{Preset: "rm -rf /"},
	} {
		if _, _, err := parseScheduleForm(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestWhenText(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 12, 10, 0, 0, 0, loc) // a Thursday
	for in, want := range map[time.Time]string{
		now.Add(-time.Minute):                      "Now",
		time.Date(2026, 11, 12, 14, 0, 0, 0, loc):  "Today 14:00",
		time.Date(2026, 11, 13, 2, 0, 0, 0, loc):   "Tomorrow 02:00",
		time.Date(2026, 11, 15, 2, 0, 0, 0, loc):   "Sun 02:00",
		time.Date(2026, 11, 25, 2, 0, 0, 0, loc):   "Wed 25 Nov 02:00",
		time.Date(2026, 11, 12, 23, 59, 0, 0, loc): "Today 23:59",
	} {
		if got := whenText(in, now, loc); got != want {
			t.Errorf("whenText(%v) = %q, want %q", in, got, want)
		}
	}
}

// TestSchedulesTimetable: the page leads with clock times grouped into
// clusters, has the timeline, the concurrency line and the editor, and no
// free-text interval box or native time picker on show.
func TestSchedulesTimetable(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	for _, k := range []string{"builtin.os", "builtin.hw"} {
		post(t, h, "/schedules", url.Values{"collector": {k}, "preset": {"1d"}, "hour": {"02"}, "minute": {"00"}})
	}
	got, _ := st.Schedules(ctx)
	if got["builtin.os"].At != "02:00" || got["builtin.hw"].At != "02:00" {
		t.Fatalf("presets not stored: %+v", got)
	}
	body := get(t, h, "/schedules", nil).Body.String()
	for _, want := range []string{
		"02:00", "2 collectors", // one cluster with both
		"Next 24 hours", "sched-hour", // the timeline
		"never more than 4 sessions across the estate and 1 per host",
		`name="preset"`, `name="hour"`, `name="minute"`, "Every 15 min", "Weekly", "Custom…",
		"Use a quiet time",
		`<details class="sched-editor">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("schedules page lacks %q", want)
		}
	}
	if strings.Contains(body, `type="time"`) {
		t.Error("the native time picker is still there")
	}
	// The two at 02:00 are listed together, in the same cluster.
	i := strings.Index(body, `id="sched-builtin-os"`)
	j := strings.Index(body, `id="sched-builtin-hw"`)
	section := body[min(i, j):max(i, j)]
	if i < 0 || j < 0 || strings.Contains(section, `class="sched-cluster`) {
		t.Error("collectors running at the same time are not listed together")
	}

	// The collector page uses the same editor.
	page := get(t, h, "/collectors/builtin.os.version", nil).Body.String()
	if !strings.Contains(page, `name="preset"`) || !strings.Contains(page, `name="back" value="/collectors/builtin.os.version"`) {
		t.Error("the collector page lacks the schedule editor")
	}

	// A quiet time moves a daily collector away from the others.
	rec := post(t, h, "/schedules", url.Values{"collector": {"builtin.host"}, "preset": {"1d"}, "mode": {"quiet"}})
	_, msg := followFlash(t, h, rec)
	got, _ = st.Schedules(ctx)
	q := got["builtin.host"].At
	if q == "" || q == "02:00" || !strings.Contains(msg, q) {
		t.Errorf("quiet time %q, flash %q", q, msg)
	}
	// It only goes with whole days.
	_, msg = followFlash(t, h, post(t, h, "/schedules", url.Values{"collector": {"builtin.host"}, "preset": {"6h"}, "mode": {"quiet"}}))
	if !strings.Contains(msg, "flash--error") {
		t.Errorf("a quiet time with 6h was accepted: %q", msg)
	}

	// Spreading a cluster gives each whole-day collector its own time.
	rec = post(t, h, "/schedules/spread", url.Values{"collector": {"builtin.os", "builtin.hw", "spec.nope"}, "start": {"02:00"}, "over": {"2h"}})
	_, msg = followFlash(t, h, rec)
	got, _ = st.Schedules(ctx)
	if got["builtin.os"].At != "02:00" || got["builtin.hw"].At != "03:00" {
		t.Errorf("spread: os %q hw %q (%s)", got["builtin.os"].At, got["builtin.hw"].At, msg)
	}
	if _, ok := got["spec.nope"]; ok {
		t.Error("an unknown collector was given a schedule")
	}
	_, msg = followFlash(t, h, post(t, h, "/schedules/spread", url.Values{"collector": {"builtin.os"}, "start": {"02:00"}, "over": {"2h"}}))
	if !strings.Contains(msg, "flash--error") {
		t.Errorf("spreading one collector was accepted: %q", msg)
	}
}
