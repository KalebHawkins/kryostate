package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// otherProcess opens the database st uses a second time, as another
// kryostate process on the same file would.
func otherProcess(t *testing.T, st *store.Store) *store.Store {
	t.Helper()
	var seq int
	var name, file string
	if err := st.DB().QueryRow(`SELECT seq, name, file FROM pragma_database_list WHERE name = 'main'`).Scan(&seq, &name, &file); err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { other.Close() })
	return other
}

// TestCollectionLockHeartbeat: a held lock is renewed while it is held, so
// it outlives its TTL; another process cannot take it until it is released;
// and within one process it is shared and given up with its last user.
func TestCollectionLockHeartbeat(t *testing.T) {
	ctx := context.Background()
	_, st := testEngine(t)
	other := otherProcess(t, st)

	a := NewCollectionLock(st)
	a.TTL, a.Heartbeat = 400*time.Millisecond, 50*time.Millisecond
	b := NewCollectionLock(other)
	b.TTL, b.Heartbeat = 400*time.Millisecond, 50*time.Millisecond

	rel1, err := a.TryAcquire(ctx, LeaseInfo{Purpose: "refresh by kim", Origin: "cli", Actor: "kim"})
	if err != nil {
		t.Fatal(err)
	}
	rel2, err := a.TryAcquire(ctx, LeaseInfo{Purpose: "scheduled", Origin: "schedule"})
	if err != nil {
		t.Fatalf("the same process could not share its lock: %v", err)
	}

	// Well past the TTL, the heartbeat has kept it.
	time.Sleep(time.Second)
	_, err = b.TryAcquire(ctx, LeaseInfo{Origin: "cli"})
	var held *LeaseHeldError
	if !errors.As(err, &held) {
		t.Fatalf("another process took a held lock: %v", err)
	}
	if !strings.Contains(held.Error(), "command line by kim") {
		t.Errorf("held error says %q", held.Error())
	}
	if l, ok, _ := b.Elsewhere(ctx); !ok || l.Holder != a.Holder() {
		t.Errorf("Elsewhere = %+v, %v", l, ok)
	}
	if _, ok, _ := a.Elsewhere(ctx); ok {
		t.Error("the holder sees its own lease as somebody else's")
	}

	rel1()
	rel1() // twice is harmless
	if _, err := b.TryAcquire(ctx, LeaseInfo{}); err == nil {
		t.Fatal("released while still in use in its process")
	}
	rel2()
	relB, err := b.TryAcquire(ctx, LeaseInfo{Origin: "cli"})
	if err != nil {
		t.Fatalf("not free after release: %v", err)
	}
	relB()
}

// TestCollectionLockExpires: a holder that died without releasing (no
// heartbeat) loses the lease once it expires, and Acquire waits for that.
func TestCollectionLockExpires(t *testing.T) {
	ctx := context.Background()
	_, st := testEngine(t)
	other := otherProcess(t, st)
	if ok, _, err := other.AcquireLease(ctx, store.LeaseRequest{Holder: "gone/1/x", Origin: "cli", Actor: "kim",
		TTL: 600 * time.Millisecond}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	a := NewCollectionLock(st)
	if _, err := a.Acquire(ctx, LeaseInfo{}, 0, nil); err == nil {
		t.Fatal("took a live lease without waiting")
	}
	var told int
	start := time.Now()
	rel, err := a.Acquire(ctx, LeaseInfo{Origin: "cli"}, 5*time.Second, func(l store.Lease) {
		told++
		if l.Holder != "gone/1/x" {
			t.Errorf("told about %+v", l)
		}
	})
	if err != nil {
		t.Fatalf("did not get the lease once it expired: %v", err)
	}
	defer rel()
	if told != 1 {
		t.Errorf("told %d times about the same holder", told)
	}
	if time.Since(start) > 4*time.Second {
		t.Errorf("waited %v for a lease that expired in 0.6s", time.Since(start))
	}
}

// TestSchedulerSkipsWhileAnotherProcessCollects: while a kryostate refresh
// in another shell holds the lease, a due collector is not run; it stays due
// and runs at the first tick after the lease is released.
func TestSchedulerSkipsWhileAnotherProcessCollects(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"h1": "linux"})
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, 30*time.Minute)
	fx.clock.add(2 * time.Hour)

	other := otherProcess(t, fx.st)
	cli := NewCollectionLock(other)
	release, err := cli.TryAcquire(ctx, LeaseInfo{Purpose: "kryostate refresh by kim", Origin: "cli", Actor: "kim"})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		keys, err := fx.s.RunDue(ctx)
		if err != nil {
			t.Fatal(err)
		}
		fx.s.Wait()
		if len(keys) != 0 || fx.f.dials["h1"] != 0 {
			t.Fatalf("tick %d ran %v while another process collected (%d dials)", i, keys, fx.f.dials["h1"])
		}
	}
	runs, _ := fx.st.Runs(ctx, 5)
	for _, r := range runs {
		if r.Trigger == "schedule" {
			t.Errorf("a skipped tick recorded a run: %+v", r)
		}
	}

	release()
	keys, err := fx.s.RunDue(ctx)
	if err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	if len(keys) == 0 || fx.f.dials["h1"] != 1 {
		t.Errorf("after release: ran %v, %d dials", keys, fx.f.dials["h1"])
	}
	// And the scheduler gave the lease back when its run ended.
	if _, ok, _ := other.CurrentLease(ctx, time.Now()); ok {
		t.Error("the scheduler kept the lease after its run")
	}
}

// TestBackoffSurvivesRestart: a host the scheduler could not reach is still
// left alone by a new scheduler on the same database, and a success clears
// the record.
func TestBackoffSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"down": "linux"})
	fx.f.refuse["down"] = true
	fx.s.BackoffBase = 2 * time.Hour
	c := fx.choose(t, "cpu.cores")
	fx.every(t, c.Key, 30*time.Minute)
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	fx.s.Wait()
	fails, until := fx.s.Backoff(fx.ids["down"])
	if fails != 1 {
		t.Fatalf("not backed off: %d", fails)
	}
	saved, ok, err := fx.st.HostBackoffFor(ctx, fx.ids["down"])
	if err != nil || !ok || saved.Fails != 1 || !strings.Contains(saved.LastError, "refused") {
		t.Fatalf("backoff not saved: %+v %v %v", saved, ok, err)
	}
	if txt := BackoffText(saved, fx.clock.now(), time.Local); !strings.Contains(txt, "after 1 failed attempt: ") {
		t.Errorf("BackoffText = %q", txt)
	}

	// A restart: a new scheduler on the same database.
	s2 := NewScheduler(fx.e, nil)
	s2.Now, s2.Spread, s2.BackoffBase = fx.clock.now, 0, 2*time.Hour
	if f, u := s2.Backoff(fx.ids["down"]); f != 1 || u.Sub(until).Abs() > time.Microsecond {
		t.Fatalf("after restart: %d fails until %v, want 1 until %v", f, u, until)
	}
	fx.clock.add(46 * time.Minute)
	if _, err := s2.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	s2.Wait()
	if fx.f.dials["down"] != 1 {
		t.Errorf("restarted scheduler dialled a backed-off host (%d dials)", fx.f.dials["down"])
	}

	// Past it, the host answers and the record goes.
	fx.f.refuse["down"] = false
	fx.clock.add(2 * time.Hour)
	if _, err := s2.RunDue(ctx); err != nil {
		t.Fatal(err)
	}
	s2.Wait()
	if fx.f.dials["down"] != 2 {
		t.Fatalf("not retried after backoff: %d", fx.f.dials["down"])
	}
	if _, ok, _ := fx.st.HostBackoffFor(ctx, fx.ids["down"]); ok {
		t.Error("a host that answered still has a saved backoff")
	}
	if f, _ := NewScheduler(fx.e, nil).Backoff(fx.ids["down"]); f != 0 {
		t.Error("a new scheduler still backs off a host that answered")
	}
}
