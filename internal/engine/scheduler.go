package engine

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// DialError is a failure to connect to a host at all, as opposed to a host
// that answered and then could not say something. The scheduler backs off
// from the first kind and not the second.
type DialError struct{ Err error }

func (e *DialError) Error() string { return e.Err.Error() }
func (e *DialError) Unwrap() error { return e.Err }

func (e *Engine) connect(ctx context.Context, p store.ConnProfile) (collect.Transport, string, error) {
	if e.Dial != nil {
		return e.Dial(ctx, p)
	}
	return e.dial(ctx, p)
}

// markAllRan records a full collection against every collector, so that the
// scheduler does not run them all again straight after somebody else did.
func (e *Engine) markAllRan(ctx context.Context, runID string, started time.Time, status string, ok, failed int) {
	cs, err := e.Collectors(ctx)
	if err != nil {
		e.logf("collector state", err)
		return
	}
	keys := make([]string, 0, len(cs))
	for _, c := range cs {
		keys = append(keys, c.Key)
	}
	outcome := waveOutcome(status, ok, failed, 0)
	detail := fmt.Sprintf("full collection: %d ok, %d failed", ok, failed)
	if err := e.Store.MarkCollectorsRan(ctx, keys, started, time.Now(), runID, outcome, detail); err != nil {
		e.logf("collector state", err)
	}
}

func waveOutcome(status string, ok, failed, skipped int) string {
	switch {
	case status != "" && status != "complete":
		return status
	case failed == 0 && ok > 0:
		return "ok"
	case failed > 0 && ok > 0:
		return "partial"
	case failed > 0:
		return "failed"
	case skipped > 0:
		return "skipped"
	default:
		return "ok"
	}
}

// Gate keeps a manual collection and scheduled runs from becoming two waves
// of sessions into the same estate. A manual run waits for scheduled
// sessions already open to finish, and while it runs the scheduler opens no
// new ones. Scheduled runs never wait on each other here; their limits are
// the scheduler's own.
type Gate struct {
	mu        sync.Mutex
	cond      *sync.Cond
	manual    bool
	scheduled int
}

// NewGate returns an open gate.
func NewGate() *Gate {
	g := &Gate{}
	g.cond = sync.NewCond(&g.mu)
	return g
}

// BeginManual claims the gate for a manual run, waiting until no scheduled
// session is open. It reports false, without waiting, when another manual
// run already holds it.
func (g *Gate) BeginManual() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.manual {
		return false
	}
	g.manual = true
	for g.scheduled > 0 {
		g.cond.Wait()
	}
	return true
}

// EndManual releases the gate after a manual run.
func (g *Gate) EndManual() {
	g.mu.Lock()
	g.manual = false
	g.mu.Unlock()
	g.cond.Broadcast()
}

// ManualRunning reports whether a manual run holds the gate, or is waiting
// for it.
func (g *Gate) ManualRunning() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.manual
}

func (g *Gate) beginScheduled() bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.manual {
		return false
	}
	g.scheduled++
	return true
}

func (g *Gate) endScheduled() {
	g.mu.Lock()
	g.scheduled--
	g.mu.Unlock()
	g.cond.Broadcast()
}

// Scheduler runs collectors when they are due, on the hosts they apply to,
// and only their own commands.
//
// The limits are deliberately below what a manual run uses. A person who
// clicks "Collect now" is watching; a schedule fires at three in the morning
// with nobody watching, against hosts other people are responsible for.
type Scheduler struct {
	Engine *Engine
	// MaxSessions caps sessions open at once across the estate.
	MaxSessions int
	// PerHost caps sessions open at once to any one host.
	PerHost int
	// Tick is how often due collectors are looked for.
	Tick time.Duration
	// Spread is the longest a host's turn is delayed at random, so a run
	// does not reach every host in the same second. It is never more than a
	// quarter of the shortest interval in the run.
	Spread time.Duration
	// BackoffBase is how long a host that could not be reached is left
	// alone after its first failure; each further failure doubles it, up to
	// BackoffMax.
	BackoffBase time.Duration
	BackoffMax  time.Duration
	// HostTimeout bounds everything done against one host in one run.
	HostTimeout time.Duration
	// Location is where a time of day is read. Local by default.
	Location *time.Location
	// Now and Rand exist for tests.
	Now  func() time.Time
	Rand func(n int64) int64
	// OnProgress receives each host's result, for a live view.
	OnProgress func(Progress)
	// OnTick, when set, is called on every tick with the scheduler's clock
	// and location, for work that runs at a time of day, such as the daily
	// digest. It must not block for long.
	OnTick func(ctx context.Context, now time.Time, loc *time.Location)

	gate    *Gate
	started atomic.Bool

	mu        sync.Mutex
	running   map[string]bool
	hosts     map[string]*backoff
	hostSlots map[string]chan struct{}
	sessions  chan struct{}
	wg        sync.WaitGroup
}

type backoff struct {
	fails int
	until time.Time
	last  string
}

// Default limits. Four sessions at once is half what a manual run uses; one
// per host means two collectors due at once on one host take turns rather
// than logging in twice.
const (
	DefaultMaxSessions = 4
	DefaultPerHost     = 1
	DefaultTick        = 30 * time.Second
	DefaultSpread      = 2 * time.Minute
	DefaultBackoffBase = 10 * time.Minute
	DefaultBackoffMax  = 6 * time.Hour
)

// NewScheduler returns a scheduler with the default limits, sharing gate
// with whoever starts manual runs. A nil gate gets a new one.
func NewScheduler(e *Engine, gate *Gate) *Scheduler {
	if gate == nil {
		gate = NewGate()
	}
	return &Scheduler{
		Engine: e, MaxSessions: DefaultMaxSessions, PerHost: DefaultPerHost,
		Tick: DefaultTick, Spread: DefaultSpread,
		BackoffBase: DefaultBackoffBase, BackoffMax: DefaultBackoffMax,
		HostTimeout: 2 * time.Minute, gate: gate,
	}
}

// Gate is the gate manual runs must pass through.
func (s *Scheduler) Gate() *Gate { return s.gate }

func (s *Scheduler) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Loc is where times of day are read and shown.
func (s *Scheduler) Loc() *time.Location { return s.loc() }

func (s *Scheduler) loc() *time.Location {
	if s.Location != nil {
		return s.Location
	}
	return time.Local
}

func (s *Scheduler) init() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.running == nil {
		s.running = map[string]bool{}
		s.hosts = map[string]*backoff{}
		s.hostSlots = map[string]chan struct{}{}
	}
	if s.sessions == nil {
		n := s.MaxSessions
		if n <= 0 {
			n = DefaultMaxSessions
		}
		s.sessions = make(chan struct{}, n)
	}
}

// Planned is one collector with its effective schedule and where it stands.
type Planned struct {
	Collector
	Schedule Schedule
	// Custom says somebody chose the schedule; otherwise it is the default.
	Custom bool
	State  store.CollectorState
	// Last is when it last started, from its own state or, failing that,
	// the last full collection.
	Last    time.Time
	Next    time.Time
	Running bool
}

// Plan lists every collector with its schedule and next due time, in the
// order Collectors returns them.
func (s *Scheduler) Plan(ctx context.Context) ([]Planned, error) {
	s.init()
	e := s.Engine
	cs, err := e.Collectors(ctx)
	if err != nil {
		return nil, err
	}
	chosen, err := e.Store.Schedules(ctx)
	if err != nil {
		return nil, err
	}
	states, err := e.Store.CollectorStates(ctx)
	if err != nil {
		return nil, err
	}
	full, err := e.Store.LastFullRun(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	out := make([]Planned, 0, len(cs))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range cs {
		p := Planned{Collector: c, Schedule: c.Default, State: states[c.Key], Running: s.running[c.Key]}
		if sc, ok := chosen[c.Key]; ok {
			p.Schedule = Schedule{Every: sc.Every, At: sc.At}
			p.Custom = true
		}
		p.Last = p.State.LastStarted
		if p.Last.IsZero() {
			p.Last = full
		}
		p.Next = NextDue(c.Key, p.Schedule, p.Last, now, s.loc())
		out = append(out, p)
	}
	return out, nil
}

// Upcoming is Plan narrowed to scheduled collectors, soonest first.
func (s *Scheduler) Upcoming(ctx context.Context, n int) ([]Planned, error) {
	all, err := s.Plan(ctx)
	if err != nil {
		return nil, err
	}
	var out []Planned
	for _, p := range all {
		if !p.Schedule.Manual() {
			out = append(out, p)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Next.Before(out[j].Next) })
	if n > 0 && len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// Find looks a collector up by its key, or by the shorter name a person is
// likely to type: a catalog entry, a spec id, an external's name, or a
// built-in group such as "os".
func (s *Scheduler) Find(ctx context.Context, ref string) (Collector, error) {
	cs, err := s.Engine.Collectors(ctx)
	if err != nil {
		return Collector{}, err
	}
	ref = strings.TrimSpace(ref)
	for _, c := range cs {
		if c.Key == ref {
			return c, nil
		}
	}
	var match []Collector
	for _, c := range cs {
		for _, prefix := range []string{"builtin.", "catalog.", "spec.", "external."} {
			if c.Key == prefix+ref {
				match = append(match, c)
			}
		}
		if c.Kind == KindCatalog && catalogTemplate(c.Key) == ref && c.Key != "catalog."+ref {
			match = append(match, c)
		}
	}
	switch len(match) {
	case 1:
		return match[0], nil
	case 0:
		return Collector{}, fmt.Errorf("no collector %q; list them with: kryostate collectors -schedules", ref)
	default:
		var keys []string
		for _, c := range match {
			keys = append(keys, c.Key)
		}
		return Collector{}, fmt.Errorf("%q could be any of %s; give the full name", ref, strings.Join(keys, ", "))
	}
}

// SetSchedule checks and stores a schedule for one collector.
func (s *Scheduler) SetSchedule(ctx context.Context, key string, sc Schedule, by string) (Collector, error) {
	c, err := s.Find(ctx, key)
	if err != nil {
		return c, err
	}
	at, err := NormalizeClock(sc.At)
	if err != nil {
		return c, err
	}
	sc.At = at
	if err := sc.Check(); err != nil {
		return c, err
	}
	return c, s.Engine.Store.PutSchedule(ctx, store.Schedule{Collector: c.Key, Every: sc.Every, At: sc.At}, by)
}

// ResetSchedule returns a collector to its default schedule.
func (s *Scheduler) ResetSchedule(ctx context.Context, key, by string) (Collector, error) {
	c, err := s.Find(ctx, key)
	if err != nil {
		return c, err
	}
	return c, s.Engine.Store.ResetSchedule(ctx, c.Key, by)
}

// StaleAfter is how old a host's last good collection may be before the host
// is called stale: twice the shortest schedule of the built-in collectors,
// which run on every host. When those only run by hand, fallback is used.
func (s *Scheduler) StaleAfter(ctx context.Context, fallback time.Duration) time.Duration {
	plan, err := s.Plan(ctx)
	if err != nil {
		return fallback
	}
	var shortest time.Duration
	for _, p := range plan {
		if p.Kind != KindBuiltin || p.Schedule.Manual() {
			continue
		}
		if shortest == 0 || p.Schedule.Every < shortest {
			shortest = p.Schedule.Every
		}
	}
	if shortest == 0 {
		return fallback
	}
	return 2 * shortest
}

// Backoff reports how a host stands with the scheduler: consecutive failures
// to connect, and when it will next be tried.
func (s *Scheduler) Backoff(ciID string) (fails int, until time.Time) {
	s.init()
	s.mu.Lock()
	defer s.mu.Unlock()
	if b := s.hosts[ciID]; b != nil {
		return b.fails, b.until
	}
	return 0, time.Time{}
}

// Run looks for due collectors every Tick until ctx ends, then waits for
// runs in flight to finish.
func (s *Scheduler) Run(ctx context.Context) {
	s.started.Store(true)
	defer s.started.Store(false)
	tick := s.Tick
	if tick <= 0 {
		tick = DefaultTick
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		if _, err := s.RunDue(ctx); err != nil {
			s.Engine.logf("scheduler", err)
		}
		if s.OnTick != nil {
			s.OnTick(ctx, s.now(), s.loc())
		}
		select {
		case <-ctx.Done():
			s.Wait()
			return
		case <-t.C:
		}
	}
}

// Running reports whether Run is looking for due collectors, which it is not
// when the server was started with -no-schedule.
func (s *Scheduler) Running() bool { return s.started.Load() }

// Wait blocks until every run the scheduler started has finished.
func (s *Scheduler) Wait() { s.wg.Wait() }

type job struct {
	ciID      string
	profiles  []store.ConnProfile
	externals []collect.External
	keys      map[string]bool
	// targets is the run's view of where collectors run, shared by every
	// job in it; nil when no collector is limited to some hosts.
	targets *Targets
}

// RunDue starts a run of every collector that is due and not already
// running, and returns their keys. The run proceeds in the background; Wait
// waits for it.
//
// Collectors due at the same moment share one run, and on each host one
// session runs all of them, so three collectors due at midnight log in once,
// not three times.
func (s *Scheduler) RunDue(ctx context.Context) ([]string, error) {
	s.init()
	if s.gate.ManualRunning() {
		return nil, nil
	}
	plan, err := s.Plan(ctx)
	if err != nil {
		return nil, err
	}
	now := s.now()
	var due []Planned
	for _, p := range plan {
		if p.Schedule.Manual() || p.Running || p.Next.After(now) {
			continue
		}
		due = append(due, p)
	}
	if len(due) == 0 {
		return nil, nil
	}

	jobs, err := s.jobsFor(ctx, due)
	if err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(due))
	shortest := time.Duration(0)
	for _, p := range due {
		keys = append(keys, p.Key)
		if shortest == 0 || p.Schedule.Every < shortest {
			shortest = p.Schedule.Every
		}
	}
	st := s.Engine.Store
	if len(jobs) == 0 {
		// Nothing to do anywhere is still a run of the collector as far as
		// its schedule goes; otherwise it would be due again every tick.
		return keys, st.MarkCollectorsRan(ctx, keys, now, now, "", "ok", "no host it applies to")
	}

	label := "scheduled: " + strings.Join(keys, ", ")
	if len(label) > 200 {
		label = label[:197] + "…"
	}
	runID, err := st.StartRunAs(ctx, label, "schedule", len(jobs))
	if err != nil {
		return nil, err
	}
	if err := st.MarkCollectorsStarted(ctx, keys, now, runID); err != nil {
		return nil, err
	}
	s.mu.Lock()
	for _, k := range keys {
		s.running[k] = true
	}
	s.mu.Unlock()

	spread := s.Spread
	if spread > shortest/4 {
		spread = shortest / 4
	}
	s.wg.Add(1)
	go s.runWave(ctx, runID, keys, jobs, spread)
	return keys, nil
}

// jobsFor works out, for each host, which of the due collectors apply to it.
// A host none of them applies to is not contacted at all.
func (s *Scheduler) jobsFor(ctx context.Context, due []Planned) ([]*job, error) {
	e := s.Engine
	byHost := map[string]*job{}
	var order []string
	get := func(id string) *job {
		j, ok := byHost[id]
		if !ok {
			j = &job{ciID: id, keys: map[string]bool{}}
			byHost[id] = j
			order = append(order, id)
		}
		return j
	}

	var withSpecs, externals []Planned
	for _, p := range due {
		if p.External != nil {
			externals = append(externals, p)
		} else if len(p.Specs) > 0 {
			withSpecs = append(withSpecs, p)
		}
	}

	if len(withSpecs) > 0 {
		targets, err := e.Store.CollectTargets(ctx, "")
		if err != nil {
			return nil, err
		}
		families, err := e.Store.HostFamilies(ctx)
		if err != nil {
			return nil, err
		}
		// Where each collector runs is decided once for the whole run.
		var tg *Targets
		for _, p := range withSpecs {
			if anyTarget(p.Specs) {
				if tg, err = e.LoadTargets(ctx); err != nil {
					return nil, err
				}
				break
			}
		}
		for _, t := range targets {
			fam := families[t.CIID]
			if fam == "" && t.Transport == "winrm" {
				fam = "windows"
			}
			added := false
			for _, p := range withSpecs {
				if p.appliesTo(fam, t.CIID, tg) {
					get(t.CIID).keys[p.Key] = true
					added = true
				}
			}
			if added {
				j := byHost[t.CIID]
				j.profiles = append(j.profiles, t)
				j.targets = tg
			}
		}
	}

	if len(externals) > 0 {
		hosts, err := e.externalTargets(ctx, Options{})
		if err != nil {
			return nil, err
		}
		for _, h := range hosts {
			j := get(h.ID)
			for _, p := range externals {
				j.keys[p.Key] = true
				j.externals = append(j.externals, *p.External)
			}
		}
	}

	out := make([]*job, 0, len(order))
	for _, id := range order {
		out = append(out, byHost[id])
	}
	return out, nil
}

type jobResult int

const (
	jobOK jobResult = iota
	jobFailed
	jobBackingOff
	jobSkipped
)

func (s *Scheduler) runWave(ctx context.Context, runID string, keys []string, jobs []*job, spread time.Duration) {
	defer s.wg.Done()
	var (
		mu                              sync.Mutex
		ok, failed, backingOff, skipped int
		done                            int
		wg                              sync.WaitGroup
	)
	total := len(jobs)
	for _, j := range jobs {
		var delay time.Duration
		if spread > 0 {
			delay = time.Duration(s.rand(int64(spread)))
		}
		wg.Add(1)
		go func(j *job, delay time.Duration) {
			defer wg.Done()
			res := jobSkipped
			if delay > 0 {
				select {
				case <-time.After(delay):
				case <-ctx.Done():
				}
			}
			if ctx.Err() == nil {
				res = s.runJob(ctx, runID, j, func(p Progress) {
					mu.Lock()
					done++
					p.Done, p.Total = done, total
					mu.Unlock()
					if s.OnProgress != nil {
						s.OnProgress(p)
					}
				})
			}
			mu.Lock()
			switch res {
			case jobOK:
				ok++
			case jobFailed:
				failed++
			case jobBackingOff:
				backingOff++
			default:
				skipped++
			}
			mu.Unlock()
		}(j, delay)
	}
	wg.Wait()

	status := "complete"
	if ctx.Err() != nil {
		status = "interrupted"
	}
	var notes []string
	if backingOff > 0 {
		notes = append(notes, fmt.Sprintf("%d skipped while backing off", backingOff))
	}
	if skipped > 0 {
		notes = append(notes, fmt.Sprintf("%d skipped for a manual run or shutdown", skipped))
	}
	note := strings.Join(notes, "; ")
	closing := context.WithoutCancel(ctx)
	st := s.Engine.Store
	if err := st.FinishRunNote(closing, runID, status, ok, failed, note); err != nil {
		s.Engine.logf("finish scheduled run", err)
	}
	outcome := waveOutcome(status, ok, failed, backingOff+skipped)
	detail := fmt.Sprintf("%d ok, %d failed", ok, failed)
	if note != "" {
		detail += "; " + note
	}
	finished := s.now()
	for _, k := range keys {
		if err := st.MarkCollectorFinished(closing, k, finished, outcome, detail); err != nil {
			s.Engine.logf("collector state", err)
		}
	}
	_ = st.Audit(closing, "scheduler", "collect", runID, strings.Join(keys, ", ")+": "+detail)

	s.mu.Lock()
	for _, k := range keys {
		delete(s.running, k)
	}
	s.mu.Unlock()
	if s.OnProgress != nil {
		s.OnProgress(Progress{RunID: runID, Label: "scheduled run finished", Done: total, Total: total,
			Outcome: model.OutcomeOK, Detail: detail})
	}
}

func (s *Scheduler) rand(n int64) int64 {
	if n <= 0 {
		return 0
	}
	if s.Rand != nil {
		return s.Rand(n)
	}
	return rand.Int64N(n)
}

// runJob runs one host's share of a wave.
func (s *Scheduler) runJob(ctx context.Context, runID string, j *job, report func(Progress)) jobResult {
	e := s.Engine
	if _, until := s.Backoff(j.ciID); s.now().Before(until) {
		return jobBackingOff
	}
	release, ok := s.acquire(ctx, j.ciID)
	if !ok {
		return jobSkipped
	}
	defer release()
	if !s.gate.beginScheduled() {
		return jobSkipped
	}
	defer s.gate.endScheduled()

	timeout := s.HostTimeout
	if timeout <= 0 {
		timeout = 2 * time.Minute
	}
	hctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	label := j.ciID
	if ci, err := e.Store.CI(hctx, j.ciID); err == nil && ci.Label != "" {
		label = ci.Label
	}
	keep := func(sp collect.Spec) bool { return j.keys[CollectorKey(sp.ID)] }

	var (
		facts   int
		firstEr error
		dialErr error
	)
	for _, p := range j.profiles {
		n, err := e.collectSome(hctx, runID, p, keep, j.targets)
		facts += n
		if err != nil {
			if firstEr == nil {
				firstEr = err
			}
			var de *DialError
			if errors.As(err, &de) {
				dialErr = err
			}
		}
	}
	if len(j.externals) > 0 {
		target := externalTarget{ID: j.ciID, Label: label, Address: e.addressFor(hctx, j.ciID)}
		for _, ex := range j.externals {
			n, err := e.runExternalOne(hctx, runID, target, ex)
			facts += n
			if err != nil && firstEr == nil {
				firstEr = err
			}
		}
	}

	if len(j.profiles) > 0 {
		s.noteHost(j.ciID, dialErr)
	}
	p := Progress{RunID: runID, CIID: j.ciID, Label: label + " (scheduled)", Facts: facts, Outcome: model.OutcomeOK}
	if firstEr != nil {
		p.Outcome = outcomeFor(firstEr)
		p.Detail = firstEr.Error()
	}
	report(p)
	if firstEr != nil {
		return jobFailed
	}
	return jobOK
}

// noteHost records whether a host could be reached. Each failure to connect
// doubles how long the host is left alone; one success forgives them all.
func (s *Scheduler) noteHost(ciID string, dialErr error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if dialErr == nil {
		delete(s.hosts, ciID)
		return
	}
	b := s.hosts[ciID]
	if b == nil {
		b = &backoff{}
		s.hosts[ciID] = b
	}
	b.fails++
	b.last = dialErr.Error()
	b.until = s.now().Add(s.backoffFor(b.fails))
}

func (s *Scheduler) backoffFor(fails int) time.Duration {
	base, hi := s.BackoffBase, s.BackoffMax
	if base <= 0 {
		base = DefaultBackoffBase
	}
	if hi <= 0 {
		hi = DefaultBackoffMax
	}
	d := base
	for i := 1; i < fails && d < hi; i++ {
		d *= 2
	}
	if d > hi {
		d = hi
	}
	return d
}

// acquire takes a session slot for one host, then one for the estate. The
// host's slot is taken first so that a run waiting on a busy host does not
// sit on one of the estate's slots meanwhile.
func (s *Scheduler) acquire(ctx context.Context, ciID string) (func(), bool) {
	s.mu.Lock()
	hs, ok := s.hostSlots[ciID]
	if !ok {
		n := s.PerHost
		if n <= 0 {
			n = DefaultPerHost
		}
		hs = make(chan struct{}, n)
		s.hostSlots[ciID] = hs
	}
	s.mu.Unlock()
	select {
	case hs <- struct{}{}:
	case <-ctx.Done():
		return nil, false
	}
	select {
	case s.sessions <- struct{}{}:
	case <-ctx.Done():
		<-hs
		return nil, false
	}
	return func() {
		<-s.sessions
		<-hs
	}, true
}
