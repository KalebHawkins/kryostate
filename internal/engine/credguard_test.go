package engine

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestStalePasswordFailsOnlyUpToTheLimit: twenty hosts share a password that
// no longer works, collected eight at a time. The password is tried against
// two of them, one after the other, and then put on hold; the rest are never
// sent it, and no later run sends it either until the hold is released.
func TestStalePasswordFailsOnlyUpToTheLimit(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	var attempts, inFlight, maxInFlight atomic.Int32
	e.Dial = func(context.Context, store.ConnProfile) (collect.Transport, string, error) {
		attempts.Add(1)
		n := inFlight.Add(1)
		for {
			m := maxInFlight.Load()
			if n <= m || maxInFlight.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(5 * time.Millisecond)
		inFlight.Add(-1)
		return nil, "", errors.New("ssh: handshake failed: ssh: unable to authenticate, attempted methods [none password]")
	}
	const ref = "keyring:kryostate/ops"
	for i := range 20 {
		id := host(t, st, fmt.Sprintf("node-%02d", i))
		if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh",
			Address: id, Username: "ops", CredentialRef: ref, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}

	sum, err := e.Collect(ctx, Options{Concurrency: 8}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != DefaultCredentialFailLimit {
		t.Fatalf("password tried %d times, want %d", got, DefaultCredentialFailLimit)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("%d attempts at once with an unproven password, want 1", maxInFlight.Load())
	}
	if sum.Failed != 20 {
		t.Fatalf("failed %d of 20", sum.Failed)
	}
	h, ok, err := st.CredentialHoldFor(ctx, ref, "ops")
	if err != nil || !ok || !h.Held {
		t.Fatalf("hold = %+v %v %v, want held", h, ok, err)
	}

	if _, err := e.Collect(ctx, Options{Concurrency: 8}, nil); err != nil {
		t.Fatal(err)
	}
	if got := attempts.Load(); got != DefaultCredentialFailLimit {
		t.Fatalf("a held password was tried again: %d attempts", got)
	}
}

// TestWorkingPasswordConnectsInParallel: once the password has logged in
// during a run, the other hosts stop taking turns, and the earlier failure
// count is forgotten.
func TestWorkingPasswordConnectsInParallel(t *testing.T) {
	ctx := context.Background()
	e, st := testEngine(t)
	const ref = "keyring:kryostate/ops"
	if _, err := st.NoteCredentialFailure(ctx, ref, "ops", "old", "unable to authenticate", 2); err != nil {
		t.Fatal(err)
	}
	var inFlight, maxInFlight atomic.Int32
	var first atomic.Bool
	dial := func(ok bool) func(context.Context, store.ConnProfile) (collect.Transport, string, error) {
		first.Store(true)
		inFlight.Store(0)
		maxInFlight.Store(0)
		return func(context.Context, store.ConnProfile) (collect.Transport, string, error) {
			n := inFlight.Add(1)
			defer inFlight.Add(-1)
			for {
				m := maxInFlight.Load()
				if n <= m || maxInFlight.CompareAndSwap(m, n) {
					break
				}
			}
			// The first connection returns at once; the rest linger, so
			// any that are allowed to run together are seen to.
			if !first.CompareAndSwap(true, false) {
				time.Sleep(30 * time.Millisecond)
			}
			if !ok {
				return nil, "", errors.New("connection refused")
			}
			return &answers{reply: map[string]collect.Result{}}, "linux", nil
		}
	}
	for i := range 4 {
		id := host(t, st, fmt.Sprintf("node-%d", i))
		if err := st.PutConnProfile(ctx, store.ConnProfile{CIID: id, Transport: "ssh",
			Address: id, Username: "ops", CredentialRef: ref, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}

	// A refused connection says nothing about the password, so it neither
	// proves nor counts against it: every host still takes its turn.
	e.Dial = dial(false)
	if _, err := e.Collect(ctx, Options{Concurrency: 4}, nil); err != nil {
		t.Fatal(err)
	}
	if maxInFlight.Load() != 1 {
		t.Fatalf("%d at once before the password was proven", maxInFlight.Load())
	}
	if h, _, _ := st.CredentialHoldFor(ctx, ref, "ops"); h.Fails != 1 {
		t.Fatalf("refusals changed the failure count to %d", h.Fails)
	}

	e.Dial = dial(true)
	if _, err := e.Collect(ctx, Options{Concurrency: 4}, nil); err != nil {
		t.Fatal(err)
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("connections after a proven password did not overlap (max %d)", maxInFlight.Load())
	}
	if _, ok, _ := st.CredentialHoldFor(ctx, ref, "ops"); ok {
		t.Fatal("a successful login did not forget the earlier failure")
	}
}
