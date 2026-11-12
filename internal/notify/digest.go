// Package notify builds the daily digest of what changed and delivers it to
// the destinations configured from the command line: incoming webhooks
// (Slack, Teams and anything that takes JSON) and email over SMTP.
//
// Destinations hold credential references, never URLs with tokens in them or
// passwords; those are resolved at the moment of sending and not kept,
// logged or shown. The web interface may change when and what is sent, but
// never where, so it cannot be used to make this server talk to an address
// of somebody's choosing.
package notify

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// Caps keep a digest readable after a large change, such as an upgrade
// across the estate. The full list is a link away.
const (
	MaxHosts       = 20
	MaxPerHost     = 10
	MaxHealthHosts = 20
	MaxNever       = 10
	maxChangeRead  = 2000
	maxValueLen    = 80
)

// Digest is what happened between Since and Until.
type Digest struct {
	Since, Until time.Time
	Loc          *time.Location
	BaseURL      string
	Settings     store.NotifySettings

	// Changes are grouped by host, the host with the most first.
	Changes []HostChanges
	// ChangeCount and HostsChanged count everything, including what the
	// caps left out; MoreHosts is how many hosts were left out.
	ChangeCount  int
	HostsChanged int
	MoreHosts    int
	// ChangesLimited says the store returned as many changes as were asked
	// for, so there may be more than ChangeCount.
	ChangesLimited bool

	StartedFailing []store.FailingHost
	Recovered      []store.FailingHost

	Never      []store.HostRef
	NeverCount int

	// Extra are sections contributed by SectionSources, such as rollout
	// rules once they exist.
	Extra []Section
}

// HostChanges are the changed values on one host.
type HostChanges struct {
	CIID  string
	Host  string
	Items []store.Change
	// More is how many changes on this host the cap left out.
	More int
}

// Section is a block of lines contributed from outside this package.
type Section struct {
	Key   string
	Title string
	Lines []string
	// Count is how many things the section reports; zero means nothing
	// happened as far as this section goes.
	Count int
}

// SectionSource contributes a section to the digest.
//
// RULES EXTENSION POINT: the rollout-tracking rules work exposes something
// like RuleStatus(ctx). Once it is merged, wire it up by appending a
// SectionSource to Notifier.Extra (see web.New, where the notifier is built)
// that returns Section{Key: store.SectionRules, Title: "Rules", ...}. The
// "rules" section checkbox on /notifications already exists and the digest
// skips a source whose key is a switched-off section.
type SectionSource func(ctx context.Context, since, until time.Time) (Section, error)

// Quiet reports whether nothing happened worth sending. Hosts never collected
// are a standing condition rather than news, so they alone do not make a
// digest worth sending.
func (d Digest) Quiet() bool {
	if d.ChangeCount > 0 || len(d.StartedFailing) > 0 || len(d.Recovered) > 0 {
		return false
	}
	for _, x := range d.Extra {
		if x.Count > 0 {
			return false
		}
	}
	return true
}

// Build gathers the digest for a window from the store. Sections switched off
// in the settings are left empty.
func Build(ctx context.Context, st *store.Store, set store.NotifySettings, since, until time.Time, loc *time.Location, extra []SectionSource) (Digest, error) {
	if loc == nil {
		loc = time.Local
	}
	d := Digest{Since: since, Until: until, Loc: loc, BaseURL: strings.TrimRight(set.BaseURL, "/"), Settings: set}
	if set.Has(store.SectionChanges) {
		cs, err := st.Changes(ctx, store.ChangeFilter{Since: since, Limit: maxChangeRead})
		if err != nil {
			return d, fmt.Errorf("changes: %w", err)
		}
		d.ChangesLimited = len(cs) >= maxChangeRead
		d.groupChanges(cs)
	}
	if set.Has(store.SectionHealth) {
		started, recovered, err := st.HealthTransitions(ctx, since, until)
		if err != nil {
			return d, fmt.Errorf("health: %w", err)
		}
		d.StartedFailing, d.Recovered = started, recovered
	}
	if set.Has(store.SectionNever) {
		never, n, err := st.NeverCollected(ctx, MaxNever)
		if err != nil {
			return d, fmt.Errorf("never collected: %w", err)
		}
		d.Never, d.NeverCount = never, n
	}
	for _, src := range extra {
		sec, err := src(ctx, since, until)
		if err != nil {
			return d, err
		}
		if sec.Key != "" && !set.Has(sec.Key) {
			continue
		}
		d.Extra = append(d.Extra, sec)
	}
	return d, nil
}

// groupChanges keeps only changes made before Until, groups them by host and
// applies the caps.
func (d *Digest) groupChanges(cs []store.Change) {
	by := map[string]*HostChanges{}
	var order []*HostChanges
	for _, c := range cs {
		if c.At.After(d.Until) {
			continue
		}
		d.ChangeCount++
		h := by[c.CIID]
		if h == nil {
			h = &HostChanges{CIID: c.CIID, Host: c.Host}
			by[c.CIID] = h
			order = append(order, h)
		}
		h.Items = append(h.Items, c)
	}
	d.HostsChanged = len(order)
	sort.SliceStable(order, func(i, j int) bool {
		if len(order[i].Items) != len(order[j].Items) {
			return len(order[i].Items) > len(order[j].Items)
		}
		return order[i].Host < order[j].Host
	})
	if len(order) > MaxHosts {
		d.MoreHosts = len(order) - MaxHosts
		order = order[:MaxHosts]
	}
	for _, h := range order {
		// Oldest first within a host reads as a story.
		sort.SliceStable(h.Items, func(i, j int) bool { return h.Items[i].At.Before(h.Items[j].At) })
		if len(h.Items) > MaxPerHost {
			h.More = len(h.Items) - MaxPerHost
			h.Items = h.Items[:MaxPerHost]
		}
		d.Changes = append(d.Changes, *h)
	}
}

// Link is the page the digest points at, or empty without a base URL.
func (d Digest) Link() string {
	if d.BaseURL == "" {
		return ""
	}
	return d.BaseURL + "/changes"
}

// HostLink is a host's page, or empty without a base URL.
func (d Digest) HostLink(ciID string) string {
	if d.BaseURL == "" {
		return ""
	}
	return d.BaseURL + "/hosts/" + ciID
}

// Subject is a one-line summary, for an email subject and a chat fallback.
func (d Digest) Subject() string {
	var parts []string
	if d.Settings.Has(store.SectionChanges) {
		parts = append(parts, plural(d.ChangeCount, "change", "changes"))
	}
	if d.Settings.Has(store.SectionHealth) {
		if n := len(d.StartedFailing); n > 0 {
			parts = append(parts, fmt.Sprintf("%d started failing", n))
		}
		if n := len(d.Recovered); n > 0 {
			parts = append(parts, fmt.Sprintf("%d recovered", n))
		}
	}
	for _, x := range d.Extra {
		if x.Count > 0 {
			parts = append(parts, fmt.Sprintf("%s: %d", strings.ToLower(x.Title), x.Count))
		}
	}
	if len(parts) == 0 {
		parts = append(parts, "nothing changed")
	}
	return "KryoState daily digest: " + strings.Join(parts, ", ")
}

// Window describes the period covered in local time.
func (d Digest) Window() string {
	return "since " + d.Since.In(d.Loc).Format("Mon 2 Jan 15:04") + " until " + d.Until.In(d.Loc).Format("Mon 2 Jan 15:04 MST")
}

func hostWord(n int) string {
	if n == 1 {
		return "host"
	}
	return "hosts"
}

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// short trims a value to one line of reasonable length.
func short(v string) string {
	v = strings.Join(strings.Fields(v), " ")
	if r := []rune(v); len(r) > maxValueLen {
		return string(r[:maxValueLen-1]) + "…"
	}
	if v == "" {
		return "(empty)"
	}
	return v
}

func attrName(c store.Change) string {
	if c.Scope != "" {
		return c.Attr + " [" + c.Scope + "]"
	}
	return c.Attr
}

// Text renders the digest as plain text, for email, the command line preview
// and the generic webhook body.
func (d Digest) Text() string {
	var b strings.Builder
	fmt.Fprintf(&b, "KryoState daily digest\n%s\n", d.Window())
	if d.Settings.Has(store.SectionChanges) {
		b.WriteString("\n")
		if d.ChangeCount == 0 {
			b.WriteString("No values changed.\n")
		} else {
			fmt.Fprintf(&b, "%s on %s%s\n", plural(d.ChangeCount, "value changed", "values changed"),
				plural(d.HostsChanged, "host", "hosts"), moreSuffix(d.ChangesLimited))
			for _, h := range d.Changes {
				fmt.Fprintf(&b, "  %s\n", h.Host)
				for _, c := range h.Items {
					fmt.Fprintf(&b, "    %s: %s → %s\n", attrName(c), short(c.Old), short(c.New))
				}
				if h.More > 0 {
					fmt.Fprintf(&b, "    … and %d more on this host\n", h.More)
				}
			}
			if d.MoreHosts > 0 {
				fmt.Fprintf(&b, "  … and %d more %s\n", d.MoreHosts, hostWord(d.MoreHosts))
			}
		}
	}
	if d.Settings.Has(store.SectionHealth) {
		b.WriteString("\n")
		if len(d.StartedFailing) == 0 && len(d.Recovered) == 0 {
			b.WriteString("No host started failing or recovered.\n")
		}
		if len(d.StartedFailing) > 0 {
			fmt.Fprintf(&b, "%s started failing\n", plural(len(d.StartedFailing), "host", "hosts"))
			writeHealth(&b, d.StartedFailing, true)
		}
		if len(d.Recovered) > 0 {
			fmt.Fprintf(&b, "%s recovered\n", plural(len(d.Recovered), "host", "hosts"))
			writeHealth(&b, d.Recovered, false)
		}
	}
	if d.Settings.Has(store.SectionNever) && d.NeverCount > 0 {
		b.WriteString("\n")
		fmt.Fprintf(&b, "%s never collected\n  %s\n", plural(d.NeverCount, "host", "hosts"), neverList(d))
	}
	for _, x := range d.Extra {
		b.WriteString("\n" + x.Title + "\n")
		for _, l := range x.Lines {
			b.WriteString("  " + l + "\n")
		}
	}
	if l := d.Link(); l != "" {
		b.WriteString("\nOpen KryoState: " + l + "\n")
	}
	return b.String()
}

func moreSuffix(limited bool) string {
	if limited {
		return " (at least)"
	}
	return ""
}

func writeHealth(b *strings.Builder, hs []store.FailingHost, detail bool) {
	for i, h := range hs {
		if i == MaxHealthHosts {
			fmt.Fprintf(b, "  … and %d more\n", len(hs)-MaxHealthHosts)
			break
		}
		if detail {
			fmt.Fprintf(b, "  %s: %s\n", h.Label, healthDetail(h))
		} else {
			fmt.Fprintf(b, "  %s\n", h.Label)
		}
	}
}

func healthDetail(h store.FailingHost) string {
	if h.Detail == "" {
		return h.Outcome
	}
	return h.Outcome + ", " + short(h.Detail)
}

func neverList(d Digest) string {
	names := make([]string, 0, len(d.Never))
	for _, h := range d.Never {
		names = append(names, h.Label)
	}
	s := strings.Join(names, ", ")
	if more := d.NeverCount - len(d.Never); more > 0 {
		s += fmt.Sprintf(" … and %d more", more)
	}
	return s
}

// Counts are the numbers recorded with each send.
func (d Digest) Counts() (changes, failing, recovered, never int) {
	return d.ChangeCount, len(d.StartedFailing), len(d.Recovered), d.NeverCount
}
