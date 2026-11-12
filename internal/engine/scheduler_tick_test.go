package engine

import (
	"context"
	"testing"
	"time"
)

// TestOnTickGetsTheSchedulerClock: work done at a time of day, such as the
// daily digest, sees the same clock and location as the collectors do, so a
// test can fake both.
func TestOnTickGetsTheSchedulerClock(t *testing.T) {
	fx := newSchedFixture(t, nil)
	loc := time.FixedZone("Test", 5*3600)
	fake := time.Date(2026, 11, 12, 8, 0, 0, 0, time.UTC)
	fx.s.Now = func() time.Time { return fake }
	fx.s.Location = loc
	fx.s.Tick = 5 * time.Millisecond
	type call struct {
		now time.Time
		loc *time.Location
	}
	got := make(chan call, 10)
	fx.s.OnTick = func(_ context.Context, now time.Time, l *time.Location) {
		select {
		case got <- call{now, l}:
		default:
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); fx.s.Run(ctx) }()
	select {
	case c := <-got:
		if !c.now.Equal(fake) || c.loc != loc {
			t.Errorf("OnTick got %v in %v", c.now, c.loc)
		}
	case <-time.After(5 * time.Second):
		t.Error("OnTick was never called")
	}
	cancel()
	<-done
}
