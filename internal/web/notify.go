package web

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/notify"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// The notifications page shows where the daily digest goes and lets the
// browser change when and what is sent. Where it goes is set only from the
// command line: a form that accepted a URL or a mail server would let anyone
// who can reach this page make the server connect wherever they liked.

type notifySection struct {
	Key   string
	Label string
	Hint  string
	On    bool
}

type notifyDestRow struct {
	Name   string
	Kind   string
	Target string
	Secret string
	Last   *store.NotifySend
}

type notifyView struct {
	Settings     store.NotifySettings
	HourOpts     []option
	MinuteOpts   []option
	Sections     []notifySection
	Dests        []notifyDestRow
	Sends        []store.NotifySend
	Next         string
	SchedulerOff bool
	LastDigest   []store.NotifySend
	Preview      *notify.Digest
	PreviewErr   string
	Loc          *time.Location
}

func (s *Server) notifyLoc() *time.Location {
	if s.Scheduler != nil {
		return s.Scheduler.Loc()
	}
	return time.Local
}

// destTarget describes a destination without anything secret in it.
func destTarget(d store.NotifyDest) (target, secretRef string) {
	switch d.Kind {
	case store.DestWebhook:
		f := d.Format
		if f == "" {
			f = store.FormatSlack
		}
		return f + " webhook", "URL from " + notify.MaskRef(d.URLRef)
	case store.DestEmail:
		t := strings.Join(d.To, ", ") + " via " + d.SMTPAddr
		if d.Security != "" && d.Security != store.SecurityNone {
			t += " (" + d.Security + ")"
		}
		if d.PasswordRef != "" {
			return t, "password from " + notify.MaskRef(d.PasswordRef)
		}
		return t, ""
	}
	return d.Kind, ""
}

// hasRules says whether something contributes a Rules section yet. Until it
// does, the checkbox is not offered and the setting is kept as it was.
func (s *Server) hasRules() bool {
	return s.Notifier != nil && len(s.Notifier.Extra) > 0
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.Notifier == nil {
		http.NotFound(w, r)
		return
	}
	set, err := s.Store.GetNotifySettings(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	loc := s.notifyLoc()
	v := notifyView{Settings: set, Loc: loc, SchedulerOff: s.Scheduler == nil || !s.Scheduler.Running()}

	hour, minute := "08", "00"
	if h, m, err := notify.ParseClock(set.At); err == nil {
		hour, minute = fmt.Sprintf("%02d", h), fmt.Sprintf("%02d", m)
	}
	for h := 0; h < 24; h++ {
		x := fmt.Sprintf("%02d", h)
		v.HourOpts = append(v.HourOpts, option{x, x, x == hour})
	}
	mins := []string{"00", "15", "30", "45"}
	if !contains(mins, minute) {
		mins = append(mins, minute)
	}
	for _, m := range mins {
		v.MinuteOpts = append(v.MinuteOpts, option{m, m, m == minute})
	}
	v.Sections = []notifySection{
		{store.SectionChanges, "Values that changed", fmt.Sprintf("Old and new value, grouped by host; at most %d hosts and %d values each", notify.MaxHosts, notify.MaxPerHost), set.Has(store.SectionChanges)},
		{store.SectionHealth, "Hosts that started failing or recovered", "Compared with the previous digest", set.Has(store.SectionHealth)},
		{store.SectionNever, "Hosts never collected", "A standing list; on its own it does not make a quiet day worth sending", set.Has(store.SectionNever)},
	}
	if s.hasRules() {
		v.Sections = append(v.Sections, notifySection{store.SectionRules, "Rules", "Rollout rules and how far each has got", set.Has(store.SectionRules)})
	}

	now := time.Now()
	if slot, due, err := notify.Slot(now, set.At, loc); err == nil {
		done, _ := s.Store.DigestSlotDone(ctx, slot)
		switch {
		case !set.Enabled:
			v.Next = "off"
		case now.Before(due):
			v.Next = "today at " + due.Format("15:04")
		case len(done) > 0:
			v.Next = "tomorrow at " + due.Format("15:04")
		default:
			v.Next = "within a minute (today's is due)"
		}
	}

	dests, err := s.Store.NotifyDests(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	if v.Sends, err = s.Store.NotifySends(ctx, 200); err != nil {
		s.fail(w, err)
		return
	}
	for _, d := range dests {
		row := notifyDestRow{Name: d.Name, Kind: d.Kind}
		row.Target, row.Secret = destTarget(d)
		for i := range v.Sends {
			if v.Sends[i].Dest == d.Name {
				row.Last = &v.Sends[i]
				break
			}
		}
		v.Dests = append(v.Dests, row)
	}
	// The last digest: every row of the most recent slot.
	for _, x := range v.Sends {
		if x.Kind != store.SendDigest && x.Kind != store.SendSkipped {
			continue
		}
		if len(v.LastDigest) > 0 && x.Slot != v.LastDigest[0].Slot {
			break
		}
		v.LastDigest = append(v.LastDigest, x)
	}
	if len(v.Sends) > 15 {
		v.Sends = v.Sends[:15]
	}

	if r.URL.Query().Get("preview") != "" {
		d, err := s.Notifier.Preview(ctx, now, loc)
		if err != nil {
			v.PreviewErr = err.Error()
		} else {
			v.Preview = &d
		}
	}
	s.render(w, r, "notifications.html", "Notifications", "changes", v)
}

// handleNotifySettings saves the settings the browser may change. Anything
// else in the form, such as a URL, is ignored.
func (s *Server) handleNotifySettings(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	const back = "/notifications"
	cur, err := s.Store.GetNotifySettings(ctx)
	if err != nil {
		refuse(w, r, back, err)
		return
	}
	h, err1 := strconv.Atoi(field(r, "hour"))
	m, err2 := strconv.Atoi(field(r, "minute"))
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		refuse(w, r, back, errors.New("pick an hour and a minute"))
		return
	}
	next := store.NotifySettings{
		Enabled:   r.FormValue("enabled") != "",
		At:        fmt.Sprintf("%02d:%02d", h, m),
		SendQuiet: r.FormValue("quiet") != "",
		BaseURL:   cur.BaseURL,
	}
	if err := r.ParseForm(); err != nil {
		refuse(w, r, back, err)
		return
	}
	picked := r.PostForm["section"]
	for _, sec := range store.DigestSections {
		if sec == store.SectionRules && !s.hasRules() {
			if cur.Has(sec) {
				next.Sections = append(next.Sections, sec)
			}
			continue
		}
		if contains(picked, sec) {
			next.Sections = append(next.Sections, sec)
		}
	}
	if err := s.Store.PutNotifySettings(ctx, next, actorOf(r)); err != nil {
		refuse(w, r, back, err)
		return
	}
	msg := "The daily digest is off."
	if next.Enabled {
		msg = "The daily digest goes out at " + next.At + "."
	}
	done(w, r, back, msg)
}

// handleNotifyTest sends a test message to a destination named in the form.
// Only a name is taken; where it goes comes from the stored destination.
func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	const back = "/notifications"
	if s.Notifier == nil {
		http.NotFound(w, r)
		return
	}
	name := field(r, "name")
	ctx, cancel := context.WithTimeout(r.Context(), notify.SMTPTimeout+5*time.Second)
	defer cancel()
	if err := s.Notifier.SendTest(ctx, name, time.Now()); err != nil {
		refuse(w, r, back, fmt.Errorf("the test message to %s did not go through: %v", name, err))
		return
	}
	done(w, r, back, "Test message sent to "+name+".")
}
