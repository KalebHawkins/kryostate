package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
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

// listSchedules prints when collectors run, soonest first, grouped into
// clusters of runs that start close together, with what each cluster costs.
func listSchedules(ctx context.Context, eng *engine.Engine) error {
	s := engine.NewScheduler(eng, nil)
	o, err := s.Outlook(ctx)
	if err != nil {
		return err
	}
	if err := writeSchedules(os.Stdout, o, s.Loc(), s.MaxSessions, s.PerHost); err != nil {
		return err
	}
	fmt.Println()
	fmt.Println("change one:  kryostate collectors -schedule builtin.os -every 1d -at 02:00")
	fmt.Println("             kryostate collectors -schedule cpu.cores -manual   (or -default)")
	fmt.Println("schedules run while kryostate serve is running, unless it was started with -no-schedule.")
	return nil
}

func writeSchedules(out io.Writer, o engine.Outlook, loc *time.Location, maxSessions, perHost int) error {
	now := o.Now
	plan := map[string]engine.Planned{}
	for _, p := range o.Plan {
		plan[p.Key] = p
	}
	fmt.Fprintf(out, "Times are the server's (%s). Collectors run on many hosts at once, but never more than\n", now.In(loc).Format("MST"))
	fmt.Fprintf(out, "%d sessions across the estate and %d per host; runs starting within %s are grouped.\n",
		maxSessions, perHost, engine.FormatEvery(engine.ClusterWindow))

	var hours []string
	for _, h := range o.Hours {
		if h.Runs == 0 {
			continue
		}
		mark := ""
		if h.Storm {
			mark = " (busy)"
		}
		hours = append(hours, fmt.Sprintf("%s %d%s", h.Start.In(loc).Format("15h"), h.Runs, mark))
	}
	if len(hours) > 0 {
		fmt.Fprintf(out, "Runs per hour, next 24h: %s\n", strings.Join(hours, ", "))
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	row := func(p engine.Planned) {
		sched := p.Schedule.Describe()
		var notes []string
		if t := engine.SpreadText(p.Key, p.Schedule); t != "" {
			sched += ", " + t
			notes = append(notes, "picked for it")
		}
		if !p.Custom {
			notes = append(notes, "default")
		}
		if len(notes) > 0 {
			sched += " (" + strings.Join(notes, ", ") + ")"
		}
		last := "never"
		if !p.Last.IsZero() {
			last = p.Last.In(loc).Format("2006-01-02 15:04")
		}
		if p.State.Outcome != "" {
			last += " " + p.State.Outcome
		}
		hosts := "-"
		if !p.Schedule.Manual() {
			hosts = fmt.Sprintf("%d hosts", len(o.Load.Reach[p.Key]))
		}
		fmt.Fprintf(tw, "  %s\t%s\t%s\tlast %s\t%s\n", p.Key, sched, hosts, last, p.Title)
	}
	for _, c := range o.Clusters {
		if c.Firsts == 0 {
			continue
		}
		fmt.Fprintln(tw)
		head := engine.ClockText(c.Start, now, loc)
		if c.End.Sub(c.Start) >= time.Minute {
			head += "-" + c.End.In(loc).Format("15:04")
		}
		head += fmt.Sprintf(" · %d collector%s", len(c.Keys), pluralS(len(c.Keys)))
		if c.Hosts > 0 {
			head += fmt.Sprintf(" · ~%d host%s · ~%d sessions, %s", c.Hosts, pluralS(c.Hosts), c.Sessions, engine.AboutText(c.Duration))
		}
		fmt.Fprintln(tw, head)
		if c.Storm {
			fmt.Fprintln(tw, "  BUSY: these queue behind the session limit; spread them out on the Schedules page, or give each its own -at.")
		}
		var also []string
		listed := map[string]bool{}
		for _, r := range c.Runs {
			if listed[r.Key] {
				continue
			}
			listed[r.Key] = true
			if r.First {
				row(plan[r.Key])
			} else {
				also = append(also, r.Key)
			}
		}
		if len(also) > 0 {
			fmt.Fprintf(tw, "  also running then: %s\n", strings.Join(also, ", "))
		}
	}
	first := true
	for _, p := range o.Plan {
		if !p.Schedule.Manual() {
			continue
		}
		if first {
			fmt.Fprintln(tw)
			fmt.Fprintln(tw, "Manual only")
			first = false
		}
		row(p)
	}
	return tw.Flush()
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
