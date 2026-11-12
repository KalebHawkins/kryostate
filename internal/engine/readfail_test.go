package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// answers is a transport whose replies a test sets per command.
type answers struct {
	reply map[string]collect.Result
	broke map[string]bool
}

func (a *answers) Run(ctx context.Context, cmd string) (collect.Result, error) {
	if a.broke[cmd] {
		return collect.Result{}, errors.New("ssh: session closed")
	}
	return a.reply[cmd], nil
}
func (a *answers) HostKey() string { return "" }
func (a *answers) Close() error    { return nil }

// TestFailedReadingMakesTheValueUnknown: a host that runs a command and
// cannot answer loses the value it reported before, and says why; a session
// that broke says nothing about the host and changes nothing.
func TestFailedReadingMakesTheValueUnknown(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	first := collect.Parse{Kind: collect.ParseFirstLine}
	e.Specs = &collect.SpecSet{Specs: []collect.Spec{
		{ID: "t.file", Attr: "t.file", OS: []string{"linux"}, Command: "check-file", Parse: first},
		{ID: "t.other", Attr: "t.other", OS: []string{"linux"}, Command: "check-other", Parse: first},
	}}
	tr := &answers{reply: map[string]collect.Result{
		"check-file":  {Stdout: "false\n"},
		"check-other": {Stdout: "1\n"},
	}, broke: map[string]bool{}}
	e.Dial = func(context.Context, store.ConnProfile) (collect.Transport, string, error) { return tr, "linux", nil }
	id := host(t, st, "node-a")
	p := store.ConnProfile{CIID: id, Transport: "ssh", Address: "node-a", Port: 22, Username: "u", CredentialRef: "agent:", Enabled: true}

	value := func(attr string) (string, bool) {
		facts, err := st.Facts(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range facts {
			if f.Attr == attr {
				return f.Raw, true
			}
		}
		return "", false
	}

	if _, err := e.collectSome(ctx, "r1", p, nil, nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := value("t.file"); v != "false" {
		t.Fatalf("first reading: %q", v)
	}

	// The session breaks during one command: our failure, not the host's.
	tr.broke["check-file"] = true
	e.collectSome(ctx, "r2", p, nil, nil)
	if v, _ := value("t.file"); v != "false" {
		t.Errorf("a broken session cleared the value: %q", v)
	}

	// The host runs it and cannot tell.
	tr.broke["check-file"] = false
	tr.reply["check-file"] = collect.Result{ExitCode: 1, Stderr: "cannot look inside /etc/x as this user\n"}
	e.collectSome(ctx, "r3", p, nil, nil)
	if v, ok := value("t.file"); ok {
		t.Errorf("the old value is still current: %q", v)
	}
	if v, _ := value("t.other"); v != "1" {
		t.Errorf("an unrelated value went: %q", v)
	}
	open, err := st.ReadFailures(ctx, id)
	if err != nil || len(open) != 1 || open[0].Attr != "t.file" || !strings.Contains(open[0].Error, "cannot look inside /etc/x") {
		t.Fatalf("recorded: %v %+v", err, open)
	}

	// It can read again.
	tr.reply["check-file"] = collect.Result{Stdout: "true\n"}
	e.collectSome(ctx, "r4", p, nil, nil)
	if v, _ := value("t.file"); v != "true" {
		t.Errorf("after recovery: %q", v)
	}
	if open, _ := st.ReadFailures(ctx, id); len(open) != 0 {
		t.Errorf("still unknown: %+v", open)
	}
}

// TestExplainUsesWhatTheCommandSaid: a command that fails with a message is
// not described as having printed nothing.
func TestExplainUsesWhatTheCommandSaid(t *testing.T) {
	r := collect.Reading{Err: errors.New("if exited 1: oops"), Completed: true, Stderr: "disk is on fire\nmore"}
	if got := collect.Explain(r); !strings.Contains(got, "disk is on fire") || strings.Contains(got, "printed nothing") {
		t.Errorf("explain: %q", got)
	}
	r.Stderr = "cannot look inside /etc/ssh/sshd_config.d as this user"
	if got := collect.Explain(r); !strings.Contains(got, "not allowed to read") {
		t.Errorf("permission: %q", got)
	}
}
