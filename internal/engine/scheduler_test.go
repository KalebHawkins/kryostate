package engine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestParseEvery(t *testing.T) {
	for in, want := range map[string]time.Duration{
		"30m": 30 * time.Minute, "6h": 6 * time.Hour, "1d": 24 * time.Hour,
		"2d12h": 60 * time.Hour, "1h30m": 90 * time.Minute, " 7D ": 7 * 24 * time.Hour,
	} {
		got, err := ParseEvery(in)
		if err != nil || got != want {
			t.Errorf("ParseEvery(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "soon", "xd", "-1d", "1w"} {
		if _, err := ParseEvery(bad); err == nil {
			t.Errorf("ParseEvery(%q) accepted", bad)
		}
	}
	if FormatEvery(24*time.Hour) != "1d" || FormatEvery(6*time.Hour) != "6h" || FormatEvery(90*time.Minute) != "90m" {
		t.Error("FormatEvery does not round-trip whole units")
	}
}

func TestScheduleCheck(t *testing.T) {
	ok := []Schedule{
		{}, {Every: MinEvery}, {Every: 24 * time.Hour, At: "02:00"}, {Every: 7 * 24 * time.Hour, At: "23:59"},
	}
	for _, s := range ok {
		if err := s.Check(); err != nil {
			t.Errorf("%+v refused: %v", s, err)
		}
	}
	bad := []Schedule{
		{Every: time.Minute},                   // more often than anyone should
		{Every: 6 * time.Hour, At: "02:00"},    // a time of day needs whole days
		{Every: 24 * time.Hour, At: "25:00"},   // not a time
		{At: "02:00"},                          // manual has no time
		{Every: 40 * 24 * time.Hour},           // rarer than "manual" says better
		{Every: 24 * time.Hour, At: "2:0"},     // minutes need two digits
		{Every: 24 * time.Hour, At: "noon:00"}, // not a number
	}
	for _, s := range bad {
		if err := s.Check(); err == nil {
			t.Errorf("%+v accepted", s)
		}
	}
}

// TestNextDue covers the arithmetic the whole scheduler rests on.
func TestNextDue(t *testing.T) {
	loc := time.UTC
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, loc)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	now := at("2026-11-12 10:00")
	cases := []struct {
		name string
		s    Schedule
		last time.Time
		want time.Time
	}{
		{"manual is never due", Schedule{}, at("2026-11-01 00:00"), time.Time{}},
		{"never run is due now", Schedule{Every: time.Hour}, time.Time{}, now},
		{"an interval after the last start", Schedule{Every: 6 * time.Hour}, at("2026-11-12 09:00"), at("2026-11-12 15:00")},
		{"overdue stays in the past, run once", Schedule{Every: time.Hour}, at("2026-11-01 00:00"), at("2026-11-01 01:00")},
		{"never run with a time waits for it", Schedule{Every: 24 * time.Hour, At: "02:00"}, time.Time{}, at("2026-11-13 02:00")},
		{"never run, time later today", Schedule{Every: 24 * time.Hour, At: "23:30"}, time.Time{}, at("2026-11-12 23:30")},
		{"a late start does not drift", Schedule{Every: 24 * time.Hour, At: "02:00"}, at("2026-11-12 02:03"), at("2026-11-13 02:00")},
		{"evening manual run keeps tonight", Schedule{Every: 24 * time.Hour, At: "02:00"}, at("2026-11-11 19:00"), at("2026-11-12 02:00")},
		{"every two days", Schedule{Every: 48 * time.Hour, At: "02:00"}, at("2026-11-12 02:00"), at("2026-11-14 02:00")},
	}
	for _, c := range cases {
		got := NextDue(c.s, c.last, now, loc)
		if !got.Equal(c.want) {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

// fakeEstate stands in for SSH. It records what each host was asked and how
// many sessions were open at once.
type fakeEstate struct {
	mu        sync.Mutex
	family    map[string]string // address -> family
	refuse    map[string]bool
	ran       map[string][]string
	dials     map[string]int
	open      int
	maxOpen   int
	perHost   map[string]int
	maxPer    int
	block     chan struct{}
	commandAt time.Duration
}

func newFakeEstate() *fakeEstate {
	return &fakeEstate{family: map[string]string{}, refuse: map[string]bool{},
		ran: map[string][]string{}, dials: map[string]int{}, perHost: map[string]int{}}
}

func (f *fakeEstate) dial(ctx context.Context, p store.ConnProfile) (collect.Transport, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dials[p.Address]++
	if f.refuse[p.Address] {
		return nil, "", errors.New("dial tcp: connection refused")
	}
	f.open++
	f.perHost[p.Address]++
	f.maxOpen = max(f.maxOpen, f.open)
	f.maxPer = max(f.maxPer, f.perHost[p.Address])
	fam := f.family[p.Address]
	if fam == "" {
		fam = "linux"
	}
	return &fakeConn{f: f, addr: p.Address}, fam, nil
}

func (f *fakeEstate) commands(addr string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.ran[addr]...)
}

type fakeConn struct {
	f    *fakeEstate
	addr string
}

func (c *fakeConn) Run(ctx context.Context, cmd string) (collect.Result, error) {
	c.f.mu.Lock()
	c.f.ran[c.addr] = append(c.f.ran[c.addr], cmd)
	block, wait := c.f.block, c.f.commandAt
	c.f.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return collect.Result{}, ctx.Err()
		}
	}
	if wait > 0 {
		time.Sleep(wait)
	}
	out := "value-from-" + c.addr
	if strings.Contains(cmd, "NPROCESSORS") || strings.Contains(cmd, "processor") {
		out = "4"
	}
	return collect.Result{Stdout: out + "\n"}, nil
}

func (c *fakeConn) HostKey() string { return "" }
func (c *fakeConn) Close() error {
	c.f.mu.Lock()
	c.f.open--
	c.f.perHost[c.addr]--
	c.f.mu.Unlock()
	return nil
}

// clock is a settable time for the scheduler.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *clock) add(d time.Duration) { c.mu.Lock(); c.t = c.t.Add(d); c.mu.Unlock() }

type schedFixture struct {
	e     *Engine
	st    *store.Store
	s     *Scheduler
	f     *fakeEstate
	clock *clock
	ids   map[string]string
}

// newSchedFixture builds an estate of linux and aix hosts, each with an SSH
// profile whose address is its name, every collector freshly run so that
// nothing is due until a test makes it so.
func newSchedFixture(t *testing.T, hosts map[string]string) *schedFixture {
	t.Helper()
	ctx := context.Background()
	e, st := testEngine(t)
	f := newFakeEstate()
	e.Dial = f.dial
	fx := &schedFixture{e: e, st: st, f: f, clock: &clock{t: time.Now()}, ids: map[string]string{}}
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	for name, fam := range hosts {
		id := host(t, st, name)
		fx.ids[name] = id
		f.family[name] = fam
		if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh", Address: name,
			Port: 22, Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
			t.Fatal(err)
		}
		if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: "os.family", Raw: strings.ToUpper(fam),
			SourceID: src, ObservedAt: time.Now().Add(-time.Hour)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	s := NewScheduler(e, nil)
	s.Now = fx.clock.now
	s.Spread = 0
	fx.s = s
	fx.markAllRun(t)
	return fx
}

func (fx *schedFixture) markAllRun(t *testing.T) {
	t.Helper()
	cs, err := fx.e.Collectors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, c := range cs {
		keys = append(keys, c.Key)
	}
	now := fx.clock.now()
	if err := fx.st.MarkCollectorsRan(context.Background(), keys, now, now, "", "ok", ""); err != nil {
		t.Fatal(err)
	}
}

func (fx *schedFixture) choose(t *testing.T, template string, stacks ...string) Collector {
	t.Helper()
	ctx := context.Background()
	ch, err := fx.st.PutChoice(ctx, collect.Choice{Template: template, Target: store.StacksTarget(stacks)}, "test")
	if err != nil {
		t.Fatal(err)
	}
	c, err := fx.s.Find(ctx, choiceKey(ch))
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func (fx *schedFixture) every(t *testing.T, key string, d time.Duration) {
	t.Helper()
	if _, err := fx.s.SetSchedule(context.Background(), key, Schedule{Every: d}, "test"); err != nil {
		t.Fatal(err)
	}
}

// TestScheduledRunIsOnlyThatCollector is the point of a per-collector
// schedule: the hourly CPU check logs in and asks for the CPU count, and
// nothing else, on the hosts it applies to, and no others.
func TestScheduledRunIsOnlyThatCollector(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"lin1": "linux", "aix1": "aix"})
	other := host(t, fx.st, "no-profile") // no way in: never contacted for specs
	_ = other
	cpu := fx.choose(t, "cpu.cores")
	fx.every(t, cpu.Key, time.Hour)

	keys, err := fx.s.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	if len(keys) != 1 || keys[0] != cpu.Key {
		t.Fatalf("started %v, want only %s", keys, cpu.Key)
	}
	if got := fx.f.commands("lin1"); len(got) != 1 || got[0] != "getconf _NPROCESSORS_ONLN" {
		t.Errorf("linux host ran %q, want only the linux cpu command", got)
	}
	if got := fx.f.commands("aix1"); len(got) != 1 || !strings.Contains(got[0], "lsdev -Cc processor") {
		t.Errorf("aix host ran %q, want only the aix cpu command", got)
	}

	facts, err := fx.st.Facts(ctx, fx.ids["lin1"])
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range facts {
		if f.Attr == "hw.cpu.cores" && f.Raw == "4" {
			found = true
		}
	}
	if !found {
		t.Error("the scheduled run did not record the value")
	}

	runs, err := fx.st.Runs(ctx, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) == 0 || runs[0].Trigger != "schedule" || !strings.Contains(runs[0].Collector, cpu.Key) {
		t.Fatalf("the run was not recorded as scheduled: %+v", runs)
	}
	if runs[0].Succeeded != 2 || runs[0].Failed != 0 || runs[0].Status != "complete" {
		t.Errorf("run counts: %+v", runs[0])
	}

	// Not due again until an hour has passed.
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 0 {
		t.Errorf("ran again immediately: %v", keys)
	}
	fx.clock.add(61 * time.Minute)
	keys, err = fx.s.RunDue(ctx)
	fx.s.Wait()
	if err != nil || len(keys) != 1 {
		t.Errorf("not due after its interval: %v %v", keys, err)
	}
}

// TestStackLimitedCollectorSkipsOtherHosts: a collector limited to a stack is
// not even a login on hosts outside it.
func TestStackLimitedCollectorSkipsOtherHosts(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"in1": "linux", "out1": "linux"})
	sid, err := fx.st.EnsureStack(ctx, "web")
	if err != nil {
		t.Fatal(err)
	}
	if err := fx.st.AddToStack(ctx, sid, fx.ids["in1"], ""); err != nil {
		t.Fatal(err)
	}
	c := fx.choose(t, "nginx.version", "web")
	fx.every(t, c.Key, time.Hour)
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	if len(fx.f.commands("in1")) != 1 {
		t.Errorf("stack member ran %v", fx.f.commands("in1"))
	}
	if fx.f.dials["out1"] != 0 {
		t.Errorf("a host outside the stack was contacted %d times", fx.f.dials["out1"])
	}
}

// TestNoOverlapWithItself: a collector still running when it falls due again
// is left to finish, not started a second time.
func TestNoOverlapWithItself(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"slow1": "linux"})
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, 10*time.Minute)
	fx.f.block = make(chan struct{})

	if keys, err := fx.s.RunDue(ctx); err != nil || len(keys) != 1 {
		t.Fatalf("first run: %v %v", keys, err)
	}
	fx.clock.add(time.Hour)
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 0 {
		t.Errorf("started %v again while the first run was still going", keys)
	}
	plan, _ := fx.s.Plan(ctx)
	for _, p := range plan {
		if p.Key == c.Key && !p.Running {
			t.Error("the plan does not show the collector running")
		}
	}
	close(fx.f.block)
	fx.s.Wait()
	if n := fx.f.dials["slow1"]; n != 1 {
		t.Errorf("host contacted %d times, want 1", n)
	}
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 1 {
		t.Errorf("not due again once the first run finished: %v", keys)
	}
	fx.s.Wait()
}

// TestConcurrencyCaps: no more sessions than the cap across the estate, and
// never two at once to one host even when two collectors overlap there.
func TestConcurrencyCaps(t *testing.T) {
	ctx := context.Background()
	hosts := map[string]string{}
	for _, n := range []string{"h1", "h2", "h3", "h4", "h5", "h6"} {
		hosts[n] = "linux"
	}
	fx := newSchedFixture(t, hosts)
	fx.s.MaxSessions = 2
	fx.f.commandAt = 20 * time.Millisecond
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, time.Hour)
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	if fx.f.maxOpen > 2 {
		t.Errorf("%d sessions open at once, cap is 2", fx.f.maxOpen)
	}
	for n := range hosts {
		if len(fx.f.commands(n)) != 1 {
			t.Errorf("%s ran %v", n, fx.f.commands(n))
		}
	}

	// Two collectors on one host, falling due a moment apart, take turns.
	fx2 := newSchedFixture(t, map[string]string{"one": "linux"})
	fx2.f.block = make(chan struct{})
	a := fx2.choose(t, "cpu.cores")
	fx2.every(t, a.Key, time.Hour)
	if _, err := fx2.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	b := fx2.choose(t, "timezone")
	fx2.every(t, b.Key, time.Hour)
	if keys, err := fx2.s.RunDue(ctx); err != nil || len(keys) != 1 || keys[0] != b.Key {
		t.Fatalf("second collector: %v %v", keys, err)
	}
	time.Sleep(50 * time.Millisecond)
	fx2.f.mu.Lock()
	open := fx2.f.open
	fx2.f.mu.Unlock()
	if open != 1 {
		t.Errorf("%d sessions open to one host", open)
	}
	close(fx2.f.block)
	fx2.s.Wait()
	if fx2.f.maxPer != 1 {
		t.Errorf("host saw %d sessions at once", fx2.f.maxPer)
	}
	if len(fx2.f.commands("one")) != 2 {
		t.Errorf("both collectors should have run in turn: %v", fx2.f.commands("one"))
	}
}

// TestUnreachableHostIsBackedOff: a host that refuses connections is left
// alone for longer after each failure, and tried again once that has passed.
func TestUnreachableHostIsBackedOff(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"down": "linux", "up": "linux"})
	fx.f.refuse["down"] = true
	fx.s.BackoffBase = 2 * time.Hour
	fx.s.BackoffMax = 5 * time.Hour
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, 30*time.Minute)

	run := func() {
		t.Helper()
		if _, err := fx.s.RunDue(ctx); err != nil {
			t.Fatal(err)
		}
		fx.s.Wait()
	}
	run()
	fails, until := fx.s.Backoff(fx.ids["down"])
	if fails != 1 || !until.Equal(fx.clock.now().Add(2*time.Hour)) {
		t.Fatalf("after one failure: %d fails, until %v", fails, until)
	}
	if f, _ := fx.s.Backoff(fx.ids["up"]); f != 0 {
		t.Error("a healthy host was put in backoff")
	}

	// Due again in 31 minutes, but the down host is still being left alone.
	fx.clock.add(31 * time.Minute)
	run()
	if fx.f.dials["down"] != 1 {
		t.Errorf("down host dialled %d times while backing off", fx.f.dials["down"])
	}
	if fx.f.dials["up"] != 2 {
		t.Errorf("up host dialled %d times, want 2", fx.f.dials["up"])
	}
	runs, _ := fx.st.Runs(ctx, 1)
	if len(runs) == 0 || !strings.Contains(runs[0].Note, "backing off") {
		t.Errorf("the run does not say a host was skipped: %+v", runs)
	}

	// Past the backoff it is tried, fails again, and waits twice as long.
	fx.clock.add(2 * time.Hour)
	run()
	if fx.f.dials["down"] != 2 {
		t.Errorf("down host not retried after its backoff: %d", fx.f.dials["down"])
	}
	fails, until = fx.s.Backoff(fx.ids["down"])
	if fails != 2 || !until.Equal(fx.clock.now().Add(4*time.Hour)) {
		t.Errorf("after two failures: %d fails, until %v", fails, until)
	}
	if got := fx.s.backoffFor(5); got != 5*time.Hour {
		t.Errorf("backoff is not capped: %v", got)
	}

	// A success forgives the failures.
	fx.f.refuse["down"] = false
	fx.clock.add(5 * time.Hour)
	run()
	if f, _ := fx.s.Backoff(fx.ids["down"]); f != 0 {
		t.Errorf("a host that answered is still backed off (%d)", f)
	}
}

// TestManualAndScheduledDoNotOverlap: a manual run waits for scheduled
// sessions to close, and the scheduler starts nothing while it runs.
func TestManualAndScheduledDoNotOverlap(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"m1": "linux"})
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, time.Hour)
	fx.f.block = make(chan struct{})
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	time.Sleep(30 * time.Millisecond) // let the job open its session

	gate := fx.s.Gate()
	began := make(chan bool)
	go func() { began <- gate.BeginManual() }()
	select {
	case <-began:
		t.Fatal("the manual run started while a scheduled session was open")
	case <-time.After(50 * time.Millisecond):
	}
	if gate.BeginManual() {
		t.Error("a second manual run was allowed")
	}
	close(fx.f.block)
	if !<-began {
		t.Fatal("the manual run never started")
	}
	fx.s.Wait()

	// While it runs, nothing scheduled starts.
	fx.clock.add(2 * time.Hour)
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 0 {
		t.Errorf("scheduler started %v during a manual run", keys)
	}
	gate.EndManual()
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 1 {
		t.Errorf("scheduler did not resume after the manual run: %v", keys)
	}
	fx.s.Wait()
}

// TestFullCollectionResetsTheSchedule: after a full collection nothing is
// due, so the scheduler does not repeat it straight away. The refresh path
// itself is otherwise unchanged.
func TestFullCollectionResetsTheSchedule(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"f1": "linux"})
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, time.Hour)
	fx.clock.add(2 * time.Hour)

	sum, err := fx.e.Collect(ctx, Options{Trigger: "web"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Succeeded != 1 {
		t.Fatalf("full run: %+v", sum)
	}
	runs, _ := fx.st.Runs(ctx, 1)
	if runs[0].Trigger != "web" || runs[0].Collector != "builtin" {
		t.Errorf("full run recorded as %+v", runs[0])
	}
	// The scheduler's clock is behind real time here; move it to now.
	fx.clock.mu.Lock()
	fx.clock.t = time.Now()
	fx.clock.mu.Unlock()
	if keys, _ := fx.s.RunDue(ctx); len(keys) != 0 {
		t.Errorf("due straight after a full collection: %v", keys)
	}
	// The full run ran every linux command, not only the scheduled one.
	if len(fx.f.commands("f1")) < 3 {
		t.Errorf("full collection ran only %v", fx.f.commands("f1"))
	}
}

func TestCollectorKeys(t *testing.T) {
	for spec, want := range map[string]string{
		"linux.os.version":            "builtin.os",
		"aix.hw.serial":               "builtin.hw",
		"linux.host.machine_id":       "builtin.host",
		"catalog.cpu.cores.linux":     "catalog.cpu.cores",
		"catalog.file.exists.12.unix": "catalog.file.exists.12",
		"example.nodename":            "spec.example.nodename",
	} {
		if got := CollectorKey(spec); got != want {
			t.Errorf("CollectorKey(%q) = %q, want %q", spec, got, want)
		}
	}
	if CollectorKeyFor("whatever", "external:cmdb") != "external.cmdb" {
		t.Error("external source not named")
	}
	if catalogTemplate("catalog.file.exists.12") != "file.exists" || catalogTemplate("catalog.cpu.cores") != "cpu.cores" {
		t.Error("catalog template not recovered from key")
	}
	if DefaultSchedule(Collector{Kind: KindCatalog, Key: "catalog.cpu.cores"}).Every != 24*time.Hour {
		t.Error("hardware should default to daily")
	}
	if DefaultSchedule(Collector{Kind: KindCatalog, Key: "catalog.file.exists.3"}).Every != 6*time.Hour {
		t.Error("file checks should default to 6h")
	}
	if DefaultSchedule(Collector{Kind: KindBuiltin, Key: "builtin.os"}).Every != 24*time.Hour {
		t.Error("built-in should default to daily")
	}
}
