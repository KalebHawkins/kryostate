package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"text/tabwriter"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
)

// setSchedule handles "kryostate collectors -schedule <collector> ...".
func setSchedule(ctx context.Context, eng *engine.Engine, ref, every, at string, manual, reset bool) error {
	if ref == "" {
		return errors.New("-every, -at, -manual and -default go with -schedule <collector>; list them with: kryostate collectors -schedules")
	}
	chosen := 0
	for _, b := range []bool{every != "", manual, reset} {
		if b {
			chosen++
		}
	}
	switch {
	case chosen == 0:
		return errors.New("say how often with -every (such as 6h or 1d), or use -manual or -default")
	case chosen > 1:
		return errors.New("use only one of -every, -manual and -default")
	case at != "" && every == "":
		return errors.New("-at goes with a whole-day -every, such as -every 1d -at 02:00")
	}

	s := engine.NewScheduler(eng, nil)
	var (
		c   engine.Collector
		err error
		say string
	)
	switch {
	case reset:
		c, err = s.ResetSchedule(ctx, ref, actor())
		say = "its default, " + c.Default.Describe()
	case manual:
		c, err = s.SetSchedule(ctx, ref, engine.Schedule{}, actor())
		say = "manual only"
	default:
		var d time.Duration
		if d, err = engine.ParseEvery(every); err != nil {
			return err
		}
		sc := engine.Schedule{Every: d, At: at}
		c, err = s.SetSchedule(ctx, ref, sc, actor())
		if norm, nerr := engine.NormalizeClock(at); nerr == nil {
			sc.At = norm
		}
		say = sc.Describe()
	}
	if err != nil {
		return err
	}
	fmt.Printf("%s (%s) now runs %s.\n", c.Title, c.Key, say)
	fmt.Println("a running kryostate serve picks this up at its next check, within a minute;")
	fmt.Println("see them all with: kryostate collectors -schedules")
	return nil
}

// listSchedules prints every collector with its schedule, last run and next
// due time.
func listSchedules(ctx context.Context, eng *engine.Engine) error {
	s := engine.NewScheduler(eng, nil)
	plan, err := s.Plan(ctx)
	if err != nil {
		return err
	}
	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "COLLECTOR\tSCHEDULE\tLAST RUN\tOUTCOME\tNEXT DUE\tWHAT")
	for _, p := range plan {
		sched := p.Schedule.Describe()
		if !p.Custom {
			sched += " (default)"
		}
		last := "never"
		if !p.Last.IsZero() {
			last = p.Last.Local().Format("2006-01-02 15:04")
		}
		outcome := p.State.Outcome
		if outcome == "" {
			outcome = "-"
		}
		next := "-"
		switch {
		case p.Schedule.Manual():
			next = "when asked"
		case !p.Next.After(now):
			next = "due now"
		default:
			next = p.Next.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", p.Key, sched, last, outcome, next, p.Title)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("change one:  kryostate collectors -schedule builtin.os -every 1d -at 02:00")
	fmt.Println("             kryostate collectors -schedule cpu.cores -manual   (or -default)")
	fmt.Println("schedules run while kryostate serve is running, unless it was started with -no-schedule.")
	return nil
}
