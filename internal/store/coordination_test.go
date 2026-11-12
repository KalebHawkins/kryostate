package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

// twoHandles opens one database file twice, as two processes would.
func twoHandles(t *testing.T) (*Store, *Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "lease.db")
	a, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { a.Close() })
	b, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { b.Close() })
	return a, b
}

// TestLeaseAcrossHandles: one holder at a time; the same holder renews; a
// lease left to expire goes to whoever asks next; releasing frees it.
func TestLeaseAcrossHandles(t *testing.T) {
	ctx := context.Background()
	a, b := twoHandles(t)
	now := time.Now()
	req := func(holder string, at time.Time) LeaseRequest {
		return LeaseRequest{Holder: holder, Purpose: "refresh by kim", Origin: "cli", Actor: "kim", TTL: 2 * time.Minute, Now: at}
	}

	got, l, err := a.AcquireLease(ctx, req("host-a/1/aa", now))
	if err != nil || !got || l.Holder != "host-a/1/aa" {
		t.Fatalf("first acquire: %v %+v %v", got, l, err)
	}
	got, l, err = b.AcquireLease(ctx, req("host-b/2/bb", now.Add(time.Second)))
	if err != nil || got {
		t.Fatalf("second holder took a held lease: %v %v", got, err)
	}
	if l.Holder != "host-a/1/aa" || l.Actor != "kim" || l.Host() != "host-a" || l.AcquiredAt.Sub(now).Abs() > time.Millisecond {
		t.Errorf("held lease reported as %+v", l)
	}
	if cur, ok, _ := b.CurrentLease(ctx, now.Add(time.Second)); !ok || cur.Holder != "host-a/1/aa" {
		t.Errorf("current lease %+v %v", cur, ok)
	}

	// The holder renews; acquired_at stays where it was.
	if ok, err := a.RenewLease(ctx, "host-a/1/aa", 2*time.Minute, now.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("renew: %v %v", ok, err)
	}
	if ok, _ := b.RenewLease(ctx, "host-b/2/bb", 2*time.Minute, now.Add(time.Minute)); ok {
		t.Error("a non-holder renewed the lease")
	}
	// Two minutes after the first acquire, the renewal still keeps it.
	if got, _, _ := b.AcquireLease(ctx, req("host-b/2/bb", now.Add(2*time.Minute+time.Second))); got {
		t.Error("a renewed lease was taken before it expired")
	}
	got, l, _ = a.AcquireLease(ctx, req("host-a/1/aa", now.Add(90*time.Second)))
	if !got || l.AcquiredAt.After(now.Add(time.Millisecond)) {
		t.Errorf("re-acquiring by the holder moved acquired_at: %+v", l)
	}

	// Left to expire (a crashed holder), it goes to the next to ask.
	late := now.Add(10 * time.Minute)
	if _, ok, _ := b.CurrentLease(ctx, late); ok {
		t.Error("an expired lease is reported as current")
	}
	if got, l, err = b.AcquireLease(ctx, req("host-b/2/bb", late)); err != nil || !got || l.Holder != "host-b/2/bb" {
		t.Fatalf("expired lease not taken: %v %+v %v", got, l, err)
	}
	if ok, _ := a.RenewLease(ctx, "host-a/1/aa", time.Minute, late); ok {
		t.Error("the old holder renewed a lease it lost")
	}

	// Releasing by a non-holder does nothing; by the holder frees it.
	if err := a.ReleaseLease(ctx, "host-a/1/aa"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.CurrentLease(ctx, late); !ok {
		t.Error("a non-holder released the lease")
	}
	if err := b.ReleaseLease(ctx, "host-b/2/bb"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := a.AcquireLease(ctx, req("host-a/1/aa", late)); !got {
		t.Error("a released lease could not be taken")
	}
}

// TestLeaseContention: of many handles asking at once, exactly one wins.
func TestLeaseContention(t *testing.T) {
	ctx := context.Background()
	a, b := twoHandles(t)
	now := time.Now()
	type res struct {
		got bool
		err error
	}
	out := make(chan res, 20)
	for i := 0; i < 20; i++ {
		st := a
		if i%2 == 1 {
			st = b
		}
		go func(i int) {
			got, _, err := st.AcquireLease(ctx, LeaseRequest{Holder: string(rune('a'+i)) + "/1/x", TTL: time.Minute, Now: now})
			out <- res{got, err}
		}(i)
	}
	wins := 0
	for i := 0; i < 20; i++ {
		r := <-out
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.got {
			wins++
		}
	}
	if wins != 1 {
		t.Errorf("%d holders won the lease", wins)
	}
}

func TestHostBackoffRoundTrip(t *testing.T) {
	ctx := context.Background()
	a, b := twoHandles(t)
	until := time.Now().Add(20 * time.Minute)
	if err := a.PutHostBackoff(ctx, HostBackoff{CIID: "h1", Fails: 2, Until: until, LastError: "refused"}); err != nil {
		t.Fatal(err)
	}
	if err := a.PutHostBackoff(ctx, HostBackoff{CIID: "h1", Fails: 3, Until: until, LastError: "timed out"}); err != nil {
		t.Fatal(err)
	}
	got, ok, err := b.HostBackoffFor(ctx, "h1")
	if err != nil || !ok || got.Fails != 3 || got.LastError != "timed out" || got.Until.Sub(until).Abs() > time.Microsecond {
		t.Fatalf("backoff read back as %+v %v %v", got, ok, err)
	}
	all, err := b.HostBackoffs(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("all backoffs %+v %v", all, err)
	}
	if err := b.ClearHostBackoff(ctx, "h1"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := a.HostBackoffFor(ctx, "h1"); ok {
		t.Error("cleared backoff still there")
	}
}
