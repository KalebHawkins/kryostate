package engine

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// The collection lease is held for LeaseTTL at a time and renewed every
// LeaseHeartbeat, so a process that dies holding it frees it within LeaseTTL.
const (
	LeaseTTL       = 2 * time.Minute
	LeaseHeartbeat = 30 * time.Second
)

// LeaseHeldError says another process is collecting.
type LeaseHeldError struct{ Lease store.Lease }

func (e *LeaseHeldError) Error() string {
	return fmt.Sprintf("%s has been running since %s", e.Lease.Describe(), e.Lease.AcquiredAt.Local().Format("15:04:05"))
}

// CollectionLock is one process's handle on the collection lease in the
// database. The Gate keeps a manual run and the scheduler apart inside one
// process; this keeps processes apart: a kryostate refresh in one shell and
// the scheduler in a kryostate serve do not both open sessions into the
// estate.
//
// Within a process it is shared: the scheduler's waves and a manual run in
// the same server take it together (the Gate already orders them), and it is
// given up when the last of them finishes.
type CollectionLock struct {
	st     *store.Store
	holder string
	// TTL and Heartbeat default to LeaseTTL and LeaseHeartbeat.
	TTL       time.Duration
	Heartbeat time.Duration
	// Now is the wall clock; leases are compared between processes, so it
	// is not the scheduler's clock.
	Now func() time.Time
	Log *slog.Logger

	mu    sync.Mutex
	count int
	stop  chan struct{}
	done  chan struct{}
}

// NewCollectionLock returns a handle with a holder id unique to this process
// and call: host name, process id and a random part.
func NewCollectionLock(st *store.Store) *CollectionLock {
	return &CollectionLock{st: st, holder: newHolderID()}
}

func newHolderID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%s/%d/%s", host, os.Getpid(), hex.EncodeToString(b[:]))
}

// Holder is this handle's id in the lease table.
func (l *CollectionLock) Holder() string { return l.holder }

func (l *CollectionLock) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now()
}

func (l *CollectionLock) ttl() time.Duration {
	if l.TTL > 0 {
		return l.TTL
	}
	return LeaseTTL
}

func (l *CollectionLock) heartbeat() time.Duration {
	if l.Heartbeat > 0 {
		return l.Heartbeat
	}
	return LeaseHeartbeat
}

// LeaseInfo says what the lease is for: origin is "cli", "web" or
// "schedule".
type LeaseInfo struct {
	Purpose string
	Origin  string
	Actor   string
}

// TryAcquire takes the lease if no other process holds it, and returns the
// function that gives it back. When another process holds it, the error is a
// *LeaseHeldError.
func (l *CollectionLock) TryAcquire(ctx context.Context, info LeaseInfo) (func(), error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count > 0 {
		l.count++
		return l.releaser(), nil
	}
	ok, cur, err := l.st.AcquireLease(ctx, store.LeaseRequest{Holder: l.holder, Purpose: info.Purpose,
		Origin: info.Origin, Actor: info.Actor, TTL: l.ttl(), Now: l.now()})
	if err != nil {
		return nil, fmt.Errorf("collection lease: %w", err)
	}
	if !ok {
		return nil, &LeaseHeldError{Lease: cur}
	}
	l.count = 1
	l.stop, l.done = make(chan struct{}), make(chan struct{})
	go l.beat(l.stop, l.done, info)
	return l.releaser(), nil
}

// Acquire is TryAcquire, waiting up to wait for another process to finish.
// onHeld is called when the lease is found held, and again whenever the
// holder changes. A wait of zero does not wait.
func (l *CollectionLock) Acquire(ctx context.Context, info LeaseInfo, wait time.Duration, onHeld func(store.Lease)) (func(), error) {
	deadline := time.Now().Add(wait)
	delay := 500 * time.Millisecond
	var seen string
	for {
		release, err := l.TryAcquire(ctx, info)
		var held *LeaseHeldError
		if !errors.As(err, &held) {
			return release, err
		}
		if held.Lease.Holder != seen {
			seen = held.Lease.Holder
			if onHeld != nil {
				onHeld(held.Lease)
			}
		}
		left := time.Until(deadline)
		if left <= 0 {
			return nil, err
		}
		if delay > left {
			delay = left
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(delay):
		}
		if delay *= 2; delay > 10*time.Second {
			delay = 10 * time.Second
		}
	}
}

// Elsewhere returns the lease when a process other than this one holds it.
func (l *CollectionLock) Elsewhere(ctx context.Context) (store.Lease, bool, error) {
	cur, ok, err := l.st.CurrentLease(ctx, l.now())
	if err != nil || !ok || cur.Holder == l.holder {
		return store.Lease{}, false, err
	}
	return cur, true, nil
}

func (l *CollectionLock) releaser() func() {
	var once sync.Once
	return func() { once.Do(l.release) }
}

func (l *CollectionLock) release() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.count--; l.count > 0 {
		return
	}
	close(l.stop)
	<-l.done
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := l.st.ReleaseLease(ctx, l.holder); err != nil && l.Log != nil {
		l.Log.Warn("release collection lease", "err", err)
	}
}

// beat renews the lease until stopped. Should it have expired meanwhile (a
// machine asleep, a database locked for minutes) and been taken, it says so;
// the run in progress carries on, since stopping halfway helps nobody.
func (l *CollectionLock) beat(stop, done chan struct{}, info LeaseInfo) {
	defer close(done)
	t := time.NewTicker(l.heartbeat())
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		ok, err := l.st.RenewLease(ctx, l.holder, l.ttl(), l.now())
		if err == nil && !ok {
			// Lost it; take it back if nobody else has.
			var got bool
			var cur store.Lease
			got, cur, err = l.st.AcquireLease(ctx, store.LeaseRequest{Holder: l.holder, Purpose: info.Purpose,
				Origin: info.Origin, Actor: info.Actor, TTL: l.ttl(), Now: l.now()})
			if err == nil && !got && l.Log != nil {
				l.Log.Warn("collection lease expired and was taken by another process", "holder", cur.Holder, "purpose", cur.Purpose)
			}
		}
		cancel()
		if err != nil && l.Log != nil {
			l.Log.Warn("renew collection lease", "err", err)
		}
	}
}
