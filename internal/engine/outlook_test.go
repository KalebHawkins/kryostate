package engine

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func hostsNamed(prefix string, n int) map[string]bool {
	out := map[string]bool{}
	for i := range n {
		out[fmt.Sprintf("%s%d", prefix, i)] = true
	}
	return out
}

// TestClusterRuns: runs starting within the window of a cluster's first are
// one cluster, wherever the collectors' schedules came from, and later runs
// start a new one.
func TestClusterRuns(t *testing.T) {
	base := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	occ := []Occurrence{
		{Key: "a", At: base, First: true},
		{Key: "b", At: base, First: true},
		{Key: "c", At: base.Add(10 * time.Minute), First: true},
		{Key: "d", At: base.Add(16 * time.Minute), First: true}, // past the window from 02:00
		{Key: "a", At: base.Add(3 * time.Hour)},
	}
	l := Load{MaxSessions: 4, SessionTime: 10 * time.Second, Reach: map[string]map[string]bool{
		"a": hostsNamed("h", 10), "b": hostsNamed("h", 10), "c": hostsNamed("h", 10), "d": hostsNamed("x", 3),
	}}
	cs := ClusterRuns(occ, ClusterWindow, l)
	if len(cs) != 3 {
		t.Fatalf("%d clusters, want 3: %+v", len(cs), cs)
	}
	first := cs[0]
	if len(first.Keys) != 3 || first.Firsts != 3 || !first.End.Equal(base.Add(10*time.Minute)) {
		t.Errorf("first cluster: %+v", first)
	}
	// a and b start in the same minute and share a session per host; c
	// starts ten minutes later and needs its own.
	if first.Hosts != 10 || first.Sessions != 20 {
		t.Errorf("hosts %d sessions %d, want 10 and 20", first.Hosts, first.Sessions)
	}
	if first.Duration != 5*10*time.Second {
		t.Errorf("duration %v, want 50s (20 sessions, 4 at a time, 10s each)", first.Duration)
	}
	if cs[2].Firsts != 0 || cs[2].Keys[0] != "a" {
		t.Errorf("a later run is not marked as such: %+v", cs[2])
	}
}

// TestStormFlag: a cluster is flagged when its sessions dwarf the estate's
// limit, or when many collectors pile into it and between them need more
// sessions than can be open at once. A handful of collectors on a tiny
// estate is not a storm.
func TestStormFlag(t *testing.T) {
	at := time.Date(2026, 11, 13, 2, 0, 0, 0, time.UTC)
	mk := func(keys int, hosts int, sameMinute bool) Cluster {
		var occ []Occurrence
		reach := map[string]map[string]bool{}
		for i := range keys {
			k := fmt.Sprintf("k%d", i)
			when := at
			if !sameMinute {
				when = at.Add(time.Duration(i) * time.Minute)
			}
			occ = append(occ, Occurrence{Key: k, At: when, First: true})
			reach[k] = hostsNamed("h", hosts)
		}
		cs := ClusterRuns(occ, ClusterWindow, Load{MaxSessions: 4, Reach: reach})
		if len(cs) != 1 {
			t.Fatalf("%d clusters", len(cs))
		}
		return cs[0]
	}
	for _, c := range []struct {
		name       string
		keys       int
		hosts      int
		sameMinute bool
		storm      bool
	}{
		{"one collector, small estate", 1, 3, false, false},
		{"one collector, 41 hosts", 1, 41, false, true},
		{"one collector, 40 hosts", 1, 40, false, false},
		{"four collectors on two hosts", 4, 1, false, false},
		{"four collectors apart on ten hosts", 4, 10, false, true},
		{"four collectors sharing a minute on two hosts", 4, 2, true, false},
		{"three collectors apart on ten hosts", 3, 10, false, false},
	} {
		got := mk(c.keys, c.hosts, c.sameMinute)
		if got.Storm != c.storm {
			t.Errorf("%s: storm %v (sessions %d), want %v", c.name, got.Storm, got.Sessions, c.storm)
		}
	}
}

// TestHoursAndQuietClock: the timeline counts runs per hour from now's hour,
// and the quiet time stays away from the busy part of the day.
func TestHoursAndQuietClock(t *testing.T) {
	loc := time.UTC
	now := time.Date(2026, 11, 12, 13, 20, 0, 0, loc)
	reach := map[string]map[string]bool{"a": hostsNamed("h", 5), "b": hostsNamed("h", 5), "me": hostsNamed("h", 5)}
	var occ []Occurrence
	for h := 0; h < 24; h++ {
		if h >= 22 || h < 6 { // the night is busy
			occ = append(occ, Occurrence{Key: "a", At: time.Date(2026, 11, 13, h, 0, 0, 0, loc)})
			occ = append(occ, Occurrence{Key: "b", At: time.Date(2026, 11, 13, h, 30, 0, 0, loc)})
		}
	}
	occ = append(occ, Occurrence{Key: "me", At: time.Date(2026, 11, 13, 12, 0, 0, 0, loc)})
	l := Load{MaxSessions: 4, Reach: reach}
	hours := Hours(occ, ClusterRuns(occ, ClusterWindow, l), now, l, loc)
	if len(hours) != OutlookHours || hours[0].Start.Hour() != 13 {
		t.Fatalf("timeline starts at %v with %d hours", hours[0].Start, len(hours))
	}
	if hours[(2+24-13)%24].Runs != 2 || hours[(2+24-13)%24].Sessions != 10 {
		t.Errorf("02:00 hour: %+v", hours[(2+24-13)%24])
	}
	if hours[0].Runs != 0 {
		t.Errorf("13:00 hour: %+v", hours[0])
	}

	q := QuietClock(occ, l, "me", loc)
	h, m, err := parseClock(q)
	if err != nil || m%15 != 0 {
		t.Fatalf("quiet clock %q: %v", q, err)
	}
	// The furthest from 06:00 and 21:30 (the edges of the busy night) is
	// around 13:45.
	if h < 12 || h > 15 {
		t.Errorf("quiet clock %s is not in the quiet afternoon", q)
	}
	// Its own run at 12:00 does not count against it.
	if q2 := QuietClock(occ, l, "", loc); q2 == "12:00" {
		t.Errorf("a slot with a run in it was suggested")
	}
}

func TestStaggerClocks(t *testing.T) {
	got, err := StaggerClocks("02:00", 3, 2*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"02:00", "02:40", "03:20"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	got, _ = StaggerClocks("23:30", 2, 2*time.Hour)
	if got[1] != "00:30" {
		t.Errorf("past midnight: %v", got)
	}
	if _, err := StaggerClocks("25:00", 2, time.Hour); err == nil {
		t.Error("a bad start was accepted")
	}
}

// TestOutlookFromTheScheduler puts it together against a real plan.
func TestOutlookFromTheScheduler(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"o1": "linux", "o2": "linux", "o3": "aix"})
	c := fx.choose(t, "cpu.cores")
	if _, err := fx.s.SetSchedule(ctx, c.Key, Schedule{Every: 24 * time.Hour, At: "02:00"}, "t"); err != nil {
		t.Fatal(err)
	}
	o, err := fx.s.Outlook(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(o.Load.Reach[c.Key]); n != 3 {
		t.Errorf("cpu.cores reaches %d hosts, want 3", n)
	}
	if len(o.Hours) != OutlookHours || len(o.Clusters) == 0 {
		t.Fatalf("outlook: %d hours, %d clusters", len(o.Hours), len(o.Clusters))
	}
	found := false
	for _, cl := range o.Clusters {
		for _, r := range cl.Runs {
			if r.Key == c.Key && r.First {
				found = r.At.In(fx.s.Loc()).Format("15:04") == "02:00"
			}
		}
	}
	if !found {
		t.Error("cpu.cores is not in a cluster at 02:00")
	}
	if o.Measured {
		t.Error("no scheduled run has finished, yet the session time is measured")
	}
}
