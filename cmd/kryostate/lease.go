package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// leaseFlags are -wait and -no-wait, on every command that opens sessions
// into the estate.
type leaseFlags struct {
	wait   *time.Duration
	noWait *bool
}

func addLeaseFlags(fs *flag.FlagSet) leaseFlags {
	return leaseFlags{
		wait: fs.Duration("wait", 10*time.Minute,
			"when another collection on this database is running (a kryostate serve's schedule, or another shell), how long to wait for it"),
		noWait: fs.Bool("no-wait", false, "fail at once instead of waiting when another collection is running"),
	}
}

// leaseOut is where waiting is reported; tests replace it.
var leaseOut io.Writer = os.Stderr

// takeLease takes the collection lease for a command-line collection,
// waiting for another process's run as the flags say. The returned function
// gives it back.
func takeLease(ctx context.Context, st *store.Store, command string, f leaseFlags) (func(), error) {
	who := actor()
	lock := engine.NewCollectionLock(st)
	info := engine.LeaseInfo{Purpose: "kryostate " + command + " by " + who, Origin: "cli", Actor: who}
	wait := *f.wait
	if *f.noWait {
		wait = 0
	}
	release, err := lock.Acquire(ctx, info, wait, func(l store.Lease) {
		if wait > 0 {
			fmt.Fprintf(leaseOut, "%s has been running since %s; waiting up to %s for it to finish (-no-wait gives up at once)\n",
				l.Describe(), l.AcquiredAt.Local().Format(time.TimeOnly), wait)
		}
	})
	var held *engine.LeaseHeldError
	switch {
	case errors.As(err, &held) && wait == 0:
		return nil, fmt.Errorf("not collecting: %w (wait for it by leaving out -no-wait)", held)
	case errors.As(err, &held):
		return nil, fmt.Errorf("gave up after waiting %s: %w (try again later, or raise -wait)", wait, held)
	case err != nil:
		return nil, err
	}
	return release, nil
}
