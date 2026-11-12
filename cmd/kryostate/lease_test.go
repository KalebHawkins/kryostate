package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
)

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestRefreshWaitsForAnotherProcess: while another process (here a second
// handle on the same file, standing in for kryostate serve) holds the
// collection lease, refresh -no-wait fails at once saying who holds it, and
// refresh without it waits and then runs once the lease is released.
func TestRefreshWaitsForAnotherProcess(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	t.Setenv("KRYOSTATE_ACTOR", "kim")
	out := &syncBuffer{}
	prev := leaseOut
	leaseOut = out
	t.Cleanup(func() { leaseOut = prev })

	other, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	srv := engine.NewCollectionLock(other)
	release, err := srv.TryAcquire(ctx, engine.LeaseInfo{Purpose: "scheduled: builtin.os", Origin: "schedule"})
	if err != nil {
		t.Fatal(err)
	}

	err = cmdRefresh(ctx, nil, []string{"-db", db, "-no-checks", "-quiet", "-no-wait"})
	if err == nil || !strings.Contains(err.Error(), "scheduled run in another kryostate serve") ||
		!strings.Contains(err.Error(), "since") {
		t.Fatalf("-no-wait: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- cmdRefresh(ctx, nil, []string{"-db", db, "-no-checks", "-quiet", "-wait", "30s"})
	}()
	select {
	case err := <-done:
		t.Fatalf("refresh did not wait for the other collection: %v", err)
	case <-time.After(700 * time.Millisecond):
	}
	if !strings.Contains(out.String(), "waiting up to 30s") {
		t.Errorf("refresh did not say what it waits for: %q", out.String())
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("refresh after the lease was released: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("refresh still waiting after the lease was released")
	}
	// It gave the lease back.
	if l, ok, _ := other.CurrentLease(ctx, time.Now()); ok {
		t.Errorf("refresh kept the lease: %+v", l)
	}

	// A short -wait gives up with a clear error.
	release, err = srv.TryAcquire(ctx, engine.LeaseInfo{Origin: "schedule"})
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	err = cmdCollect(ctx, nil, []string{"-db", db, "-wait", "300ms"})
	if err == nil || !strings.Contains(err.Error(), "gave up after waiting 300ms") {
		t.Errorf("collect -wait 300ms: %v", err)
	}
}
