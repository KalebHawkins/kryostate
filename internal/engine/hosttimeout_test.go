package engine

import (
	"context"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// stalls answers one command and blocks on every other until the host's
// time is up.
type stalls struct{ answers }

func (s *stalls) Run(ctx context.Context, cmd string) (collect.Result, error) {
	if r, ok := s.reply[cmd]; ok {
		return r, nil
	}
	<-ctx.Done()
	return collect.Result{}, ctx.Err()
}

// TestHostTimeoutKeepsWhatWasCollected: a host that runs out of time after
// some commands answered keeps those values, and the run says it was partial.
func TestHostTimeoutKeepsWhatWasCollected(t *testing.T) {
	e, st := testEngine(t)
	first := collect.Parse{Kind: collect.ParseFirstLine}
	e.Specs = &collect.SpecSet{Specs: []collect.Spec{
		{ID: "t.fast", Attr: "t.fast", OS: []string{"linux"}, Command: "fast", Parse: first},
		{ID: "t.slow", Attr: "t.slow", OS: []string{"linux"}, Command: "slow", Parse: first},
	}}
	tr := &stalls{answers{reply: map[string]collect.Result{"fast": {Stdout: "yes\n"}}}}
	e.Dial = func(context.Context, store.ConnProfile) (collect.Transport, string, error) { return tr, "linux", nil }
	id := host(t, st, "node-a")
	p := store.ConnProfile{CIID: id, Transport: "ssh", Address: "node-a", Port: 22, Username: "u", CredentialRef: "agent:", Enabled: true}

	hctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	e.collectSome(hctx, "r1", p, nil, nil)

	ctx := context.Background()
	facts, err := st.Facts(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, f := range facts {
		found = found || (f.Attr == "t.fast" && f.Raw == "yes")
	}
	if !found {
		t.Errorf("the value collected before the timeout was dropped: %+v", facts)
	}
	probes, err := st.Reachability(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	pr, ok := probes[model.ProbeCollectorRun]
	if !ok {
		t.Fatal("no collector_run probe recorded")
	}
	if pr.Outcome != model.OutcomePartial {
		t.Errorf("outcome %q, want partial", pr.Outcome)
	}
}
