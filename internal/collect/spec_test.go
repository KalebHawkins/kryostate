package collect

import (
	"context"
	"errors"
	"testing"
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
