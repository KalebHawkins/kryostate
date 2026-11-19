package collect

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

// scripted answers each command from a table, so a spec run can be given a
// transport error, a non-zero exit and a clean run in one test.
type scripted struct {
	out  string
	code int
	err  error
}

func (s scripted) Run(context.Context, string) (Result, error) {
	if s.err != nil {
		return Result{}, s.err
	}
	return Result{Stdout: s.out, ExitCode: s.code}, nil
}

func (s scripted) Close() error { return nil }

func (s scripted) HostKey() string { return "" }

// TestOnlyACommandThatRanIsReplayable guards the gate on stored output. The
// repair path re-runs parse rules over whatever was kept, so keeping the
// output of a command that failed would let a rule match an error message and
// publish it as a value — a value live collection had already rejected.
//
// A command that ran and whose rule then found nothing is the opposite case:
// that output is exactly what the repair path exists for, so it is kept.
func TestOnlyACommandThatRanIsReplayable(t *testing.T) {
	sp := Spec{
		ID: "example", Attr: "os.version",
		Command: "cat /etc/os-release",
		Parse:   Parse{Kind: ParseKV, Key: "VERSION_ID"},
	}

	cases := []struct {
		name string
		tr   scripted
		ran  bool
	}{
		{"host unreachable", scripted{err: errors.New("dial tcp: connection refused")}, false},
		{"command not found", scripted{out: "not found", code: 127}, false},
		{"ran, rule matched nothing", scripted{out: "NAME=\"Example\"\n"}, true},
		{"ran, rule matched", scripted{out: "VERSION_ID=\"9.4\"\n"}, true},
	}
	for _, c := range cases {
		r := Run(context.Background(), c.tr, []Spec{sp})
		if len(r) != 1 {
			t.Fatalf("%s: %d readings for one spec", c.name, len(r))
		}
		if r[0].Ran != c.ran {
			t.Errorf("%s: Ran = %v, want %v", c.name, r[0].Ran, c.ran)
		}
	}
}

// TestSpecFileMistakesAreExplained covers the two mistakes a person editing
// the file by hand makes most: a misspelled key, and broken JSON.
func TestSpecFileMistakesAreExplained(t *testing.T) {
	_, err := LoadSpecs([]byte(`{"version": 1, "specs": [{"id": "a", "attr": "x.y", "os": ["linux"],
  "command": "true", "optinal": true}]}`))
	if err == nil || !strings.Contains(err.Error(), `"optinal"`) {
		t.Errorf("a misspelled key gave %v", err)
	}
	_, err = LoadSpecs([]byte("{\n  \"version\": 1,\n  \"specs\": [\n    {\"id\": \"a\"}\n    {\"id\": \"b\"}\n  ]\n}"))
	if err == nil || !strings.Contains(err.Error(), "line 5") {
		t.Errorf("a missing comma gave %v", err)
	}
}

// TestParseSpecList: "collectors add" takes one spec, a list, or a whole
// collectors file, and refuses the mistakes a collectors file would.
func TestParseSpecList(t *testing.T) {
	one := `{"id": "a.b", "attr": "a.b", "os": ["linux"], "command": "true", "parse": {"kind": "first_line"}}`
	two := `{"id": "a.c", "attr": "a.c", "os": ["linux"], "command": "true", "parse": {"kind": "first_line"}}`
	for _, c := range []struct {
		in   string
		n    int
		want string
	}{
		{one, 1, ""},
		{"[" + one + ", " + two + "]", 2, ""},
		{`{"version": 1, "specs": [` + one + `]}`, 1, ""},
		{`{"id": "a.b", "attr": "a.b", "os": ["linux"], "command": "true", "optinal": true}`, 0, "optinal"},
		{`{"id": "a.b", "attr": "a.b", "command": "true"}`, 0, "which systems"},
		{`{"id": "a.b", "attr": "a.b", "os": ["linux"], "command": "true", "parse": {"kind": "regex", "pattern": "("}}`, 0, "a.b"},
		{"[" + one + ", " + one + "]", 0, "duplicate"},
		{"[]", 0, "no collectors"},
		{one + one, 0, "after top-level value"},
		{"[" + one + "][" + two + "]", 0, "more than one"},
		{"{\n  \"id\": 3\n}", 0, "line 2"},
	} {
		specs, err := ParseSpecList([]byte(c.in))
		if c.want != "" {
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("%s: got %v, want an error with %q", c.in, err, c.want)
			}
			continue
		}
		if err != nil || len(specs) != c.n {
			t.Errorf("%s: got %d specs, %v", c.in, len(specs), err)
		}
	}
}

// gauge is a transport that answers each command with its own text after a
// delay, counts how many commands run at once, and refuses a session past
// limit when limit is set.
type gauge struct {
	mu       sync.Mutex
	open     int
	most     int
	limit    int
	refused  int
	serial   bool
	delay    func(cmd string) time.Duration
	blockCtx bool
}

func (g *gauge) Run(ctx context.Context, cmd string) (Result, error) {
	g.mu.Lock()
	if g.limit > 0 && g.open >= g.limit {
		g.refused++
		g.mu.Unlock()
		return Result{}, fmt.Errorf("%w: ssh: rejected: administratively prohibited (open failed)", ErrSessionRefused)
	}
	g.open++
	g.most = max(g.most, g.open)
	g.mu.Unlock()
	defer func() {
		g.mu.Lock()
		g.open--
		g.mu.Unlock()
	}()
	if g.blockCtx {
		<-ctx.Done()
		return Result{}, ctx.Err()
	}
	select {
	case <-time.After(g.delay(cmd)):
	case <-ctx.Done():
		return Result{}, ctx.Err()
	}
	return Result{Stdout: "out " + cmd + "\n"}, nil
}
func (g *gauge) HostKey() string { return "" }
func (g *gauge) Close() error    { return nil }
func (g *gauge) Serial() bool    { return g.serial }

func countedSpecs(n int) []Spec {
	specs := make([]Spec, n)
	for i := range specs {
		specs[i] = Spec{ID: fmt.Sprintf("s%02d", i), Attr: "a", Command: fmt.Sprintf("c%02d", i),
			Parse: Parse{Kind: ParseFirstLine}}
	}
	return specs
}

// laterFirst makes later commands finish first, so readings in spec order
// show that order is kept rather than the order of completion.
func laterFirst(cmd string) time.Duration {
	var n int
	fmt.Sscanf(cmd, "c%d", &n)
	return time.Duration(30-n) * 2 * time.Millisecond
}

func checkReadings(t *testing.T, specs []Spec, rs []Reading) {
	t.Helper()
	if len(rs) != len(specs) {
		t.Fatalf("%d readings for %d specs", len(rs), len(specs))
	}
	for i, r := range rs {
		if r.SpecID != specs[i].ID || r.Err != nil || r.Value != "out "+specs[i].Command || !r.Completed {
			t.Errorf("reading %d = %s %q %v, want %s %q", i, r.SpecID, r.Value, r.Err, specs[i].ID, "out "+specs[i].Command)
		}
	}
}

// TestRunParallelKeepsOrderAndBound: a host's commands run several at once,
// never more than asked, and the readings come back in spec order. A
// transport that must run serially is kept to one.
func TestRunParallelKeepsOrderAndBound(t *testing.T) {
	specs := countedSpecs(12)
	g := &gauge{delay: laterFirst}
	checkReadings(t, specs, RunParallel(context.Background(), g, specs, 4))
	if g.most != 4 {
		t.Errorf("at most %d commands at once, want 4", g.most)
	}

	s := &gauge{delay: laterFirst, serial: true}
	checkReadings(t, specs, RunParallel(context.Background(), s, specs, 4))
	if s.most != 1 {
		t.Errorf("a serial transport ran %d commands at once", s.most)
	}
}

// TestRunParallelFallsBackWhenSessionsRefused: a host that allows fewer
// sessions than asked refuses the rest; those commands are sent again one at
// a time, and every value still comes back with no refusal recorded.
func TestRunParallelFallsBackWhenSessionsRefused(t *testing.T) {
	specs := countedSpecs(12)
	g := &gauge{delay: laterFirst, limit: 2}
	checkReadings(t, specs, RunParallel(context.Background(), g, specs, 4))
	if g.refused == 0 {
		t.Fatal("the transport refused nothing, so the test proves nothing")
	}
	// After the first refusals the run is serial: at most the two refused
	// commands plus the ones already open were turned away.
	if g.refused > 2 {
		t.Errorf("%d refusals; after the first the run should be serial", g.refused)
	}
}

// TestRunParallelDeadlineCancels: commands still open when the deadline
// passes are cancelled, Run returns only after every one of them has, and no
// goroutine is left behind.
func TestRunParallelDeadlineCancels(t *testing.T) {
	before := runtime.NumGoroutine()
	specs := countedSpecs(10)
	g := &gauge{blockCtx: true}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	start := time.Now()
	rs := RunParallel(ctx, g, specs, 4)
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("Run took %v after a 50ms deadline", d)
	}
	g.mu.Lock()
	open := g.open
	g.mu.Unlock()
	if open != 0 {
		t.Errorf("%d commands still running after Run returned", open)
	}
	for i, r := range rs {
		if r.SpecID != specs[i].ID || !errors.Is(r.Err, context.DeadlineExceeded) || r.Completed {
			t.Errorf("reading %d = %s %v completed=%v", i, r.SpecID, r.Err, r.Completed)
		}
	}
	for i := 0; runtime.NumGoroutine() > before && i < 100; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Errorf("%d goroutines before, %d after", before, n)
	}
}
