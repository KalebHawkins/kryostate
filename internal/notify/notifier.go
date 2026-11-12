package notify

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Notifier builds and sends the digest.
type Notifier struct {
	Store   *store.Store
	Secrets *secret.Registry
	Log     *slog.Logger
	// Extra contributes sections beyond the built-in ones. See
	// SectionSource for the rules extension point.
	Extra []SectionSource

	// HTTP posts webhooks; nil uses a client with timeouts and the redirect
	// policy above.
	HTTP *http.Client
	// Dial opens SMTP connections and TLS configures them; both exist for
	// tests.
	Dial Dialer
	TLS  *tls.Config
	// Attempts is how many times a digest is tried on one destination
	// before the failure is recorded, and Backoff the wait before the
	// second try, doubled each time after.
	Attempts int
	Backoff  time.Duration
	// Sleep waits between tries; tests replace it.
	Sleep func(ctx context.Context, d time.Duration) error

	busy   atomic.Bool
	wg     sync.WaitGroup
	mu     sync.Mutex
	client *http.Client
}

// New returns a notifier. A nil registry gets the default one.
func New(st *store.Store, reg *secret.Registry, log *slog.Logger) *Notifier {
	if reg == nil {
		reg = secret.NewRegistry(nil)
	}
	return &Notifier{Store: st, Secrets: reg, Log: log, Attempts: 3, Backoff: 30 * time.Second}
}

func (n *Notifier) httpClient() *http.Client {
	if n.HTTP != nil {
		return n.HTTP
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.client == nil {
		n.client = newHTTPClient()
	}
	return n.client
}

func (n *Notifier) sleep(ctx context.Context, d time.Duration) error {
	if n.Sleep != nil {
		return n.Sleep(ctx, d)
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// ParseClock reads HH:MM.
func ParseClock(at string) (h, m int, err error) {
	hs, ms, ok := strings.Cut(strings.TrimSpace(at), ":")
	if !ok {
		return 0, 0, fmt.Errorf("time of day %q: want HH:MM", at)
	}
	h, err1 := strconv.Atoi(hs)
	m, err2 := strconv.Atoi(ms)
	if err1 != nil || err2 != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, 0, fmt.Errorf("time of day %q: want HH:MM between 00:00 and 23:59", at)
	}
	return h, m, nil
}

// Slot is the day a digest at now belongs to, and when that day's digest is
// due, in loc.
func Slot(now time.Time, at string, loc *time.Location) (slot string, due time.Time, err error) {
	if loc == nil {
		loc = time.Local
	}
	h, m, err := ParseClock(at)
	if err != nil {
		return "", time.Time{}, err
	}
	l := now.In(loc)
	return l.Format("2006-01-02"), time.Date(l.Year(), l.Month(), l.Day(), h, m, 0, 0, loc), nil
}

// windowStart is where the next digest starts: the end of the last one, or a
// day before until when there was none.
func (n *Notifier) windowStart(ctx context.Context, until time.Time) (time.Time, error) {
	last, err := n.Store.LastDigestEnd(ctx)
	if err != nil {
		return time.Time{}, err
	}
	if last.IsZero() || !last.Before(until) {
		return until.Add(-24 * time.Hour), nil
	}
	return last, nil
}

// Preview builds the digest that would be sent at now.
func (n *Notifier) Preview(ctx context.Context, now time.Time, loc *time.Location) (Digest, error) {
	set, err := n.Store.GetNotifySettings(ctx)
	if err != nil {
		return Digest{}, err
	}
	since, err := n.windowStart(ctx, now)
	if err != nil {
		return Digest{}, err
	}
	return Build(ctx, n.Store, set, since, now, loc, n.Extra)
}

// TickAsync runs Tick in the background unless a previous one is still
// going, so a slow destination never holds up the collection scheduler.
func (n *Notifier) TickAsync(ctx context.Context, now time.Time, loc *time.Location) {
	if !n.busy.CompareAndSwap(false, true) {
		return
	}
	n.wg.Add(1)
	go func() {
		defer n.wg.Done()
		defer n.busy.Store(false)
		if err := n.Tick(ctx, now, loc); err != nil && n.Log != nil {
			n.Log.Error("daily digest", "err", err)
		}
	}()
}

// Wait blocks until a digest started by TickAsync has finished, so the
// store is not closed under it.
func (n *Notifier) Wait() { n.wg.Wait() }

// Tick sends today's digest to each destination that has not had it, once
// the configured time has passed. Whether a destination had it is read from
// the send history, so a restart does not send it again.
func (n *Notifier) Tick(ctx context.Context, now time.Time, loc *time.Location) error {
	set, err := n.Store.GetNotifySettings(ctx)
	if err != nil || !set.Enabled {
		return err
	}
	slot, due, err := Slot(now, set.At, loc)
	if err != nil || now.Before(due) {
		return err
	}
	dests, err := n.Store.NotifyDests(ctx)
	if err != nil || len(dests) == 0 {
		return err
	}
	done, err := n.Store.DigestSlotDone(ctx, slot)
	if err != nil || done[""] {
		return err
	}
	var pending []store.NotifyDest
	for _, d := range dests {
		if !done[d.Name] {
			pending = append(pending, d)
		}
	}
	if len(pending) == 0 {
		return nil
	}
	since, err := n.windowStart(ctx, now)
	if err != nil {
		return err
	}
	dg, err := Build(ctx, n.Store, set, since, now, loc, n.Extra)
	if err != nil {
		return err
	}
	c, f, r, nv := dg.Counts()
	rec := store.NotifySend{At: now, Slot: slot, WindowStart: since, WindowEnd: now,
		Changes: c, Failing: f, Recovered: r, Never: nv}
	if dg.Quiet() && !set.SendQuiet {
		rec.Kind, rec.OK = store.SendSkipped, true
		if n.Log != nil {
			n.Log.Info("daily digest: nothing happened, not sent")
		}
		return n.Store.RecordNotifySend(ctx, rec)
	}
	msg := dg.Message()
	var errs []error
	for _, d := range pending {
		attempts, err := n.deliverWithRetry(ctx, d, msg, now)
		r := rec
		r.Kind, r.Dest, r.Attempts, r.OK = store.SendDigest, d.Name, attempts, err == nil
		if err != nil {
			r.Error = err.Error()
			errs = append(errs, fmt.Errorf("%s: %w", d.Name, err))
		}
		if n.Log != nil {
			if err != nil {
				n.Log.Error("daily digest not delivered", "dest", d.Name, "attempts", attempts, "err", r.Error)
			} else {
				n.Log.Info("daily digest sent", "dest", d.Name, "changes", c)
			}
		}
		if rerr := n.Store.RecordNotifySend(ctx, r); rerr != nil {
			return rerr
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) deliverWithRetry(ctx context.Context, d store.NotifyDest, m Message, now time.Time) (int, error) {
	tries := n.Attempts
	if tries <= 0 {
		tries = 1
	}
	wait := n.Backoff
	var err error
	for i := 1; i <= tries; i++ {
		if err = n.Deliver(ctx, d, m, now); err == nil {
			return i, nil
		}
		if i == tries || ctx.Err() != nil {
			return i, err
		}
		if serr := n.sleep(ctx, wait); serr != nil {
			return i, err
		}
		wait *= 2
	}
	return tries, err
}

// SendTest sends a short test message to one destination, once, and records
// how it went.
func (n *Notifier) SendTest(ctx context.Context, name string, now time.Time) error {
	d, err := n.Store.FindNotifyDest(ctx, name)
	if err != nil {
		return err
	}
	err = n.Deliver(ctx, d, TestMessage(now), now)
	rec := store.NotifySend{At: now, Kind: store.SendTest, Dest: d.Name, OK: err == nil, Attempts: 1}
	if err != nil {
		rec.Error = err.Error()
	}
	if rerr := n.Store.RecordNotifySend(ctx, rec); rerr != nil && err == nil {
		return rerr
	}
	return err
}

// Deliver sends one message to one destination, once. Secrets are resolved
// here and dropped afterwards, and scrubbed from any error.
func (n *Notifier) Deliver(ctx context.Context, d store.NotifyDest, m Message, now time.Time) error {
	switch d.Kind {
	case store.DestWebhook:
		u, err := n.resolve(ctx, d.URLRef)
		if err != nil {
			return err
		}
		return scrub(postWebhook(ctx, n.httpClient(), u, d.Format, m), u)
	case store.DestEmail:
		pw := ""
		if d.PasswordRef != "" {
			var err error
			if pw, err = n.resolve(ctx, d.PasswordRef); err != nil {
				return err
			}
		}
		msg, err := BuildEmail(d.From, d.To, m, now)
		if err != nil {
			return err
		}
		return scrub(sendMail(ctx, n.Dial, n.TLS, d, pw, msg), pw)
	default:
		return fmt.Errorf("unknown destination kind %q", d.Kind)
	}
}

func (n *Notifier) resolve(ctx context.Context, ref string) (string, error) {
	s, err := n.Secrets.Resolve(ctx, ref)
	if err != nil {
		// The registry's error names the reference, which is not secret,
		// and never the material.
		return "", fmt.Errorf("credential reference %s: %w", MaskRef(ref), err)
	}
	defer s.Zero()
	v := strings.TrimSpace(string(s.Bytes))
	if v == "" {
		return "", fmt.Errorf("credential reference %s is empty", MaskRef(ref))
	}
	return v, nil
}

// scrub removes a secret from an error's text, in case a library quoted it.
func scrub(err error, secrets ...string) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	changed := false
	for _, s := range secrets {
		if len(s) >= 4 && strings.Contains(msg, s) {
			msg = strings.ReplaceAll(msg, s, "[redacted]")
			changed = true
		}
	}
	if !changed {
		return err
	}
	return errors.New(msg)
}

// MaskRef shows which kind of reference a destination holds without the
// whole of it: enough to recognise, not enough to go and read.
func MaskRef(ref string) string {
	scheme, value, ok := strings.Cut(ref, ":")
	if !ok {
		return "(not a reference)"
	}
	if r := []rune(value); len(r) > 6 {
		return scheme + ":" + string(r[:6]) + "…"
	}
	return scheme + ":" + value
}

// CheckRef refuses anything that is not a reference this build resolves,
// and in particular a URL pasted where its reference should be.
func CheckRef(reg *secret.Registry, ref, what string) error {
	scheme, value, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" || value == "" && scheme != "agent" {
		return fmt.Errorf("%s must be a credential reference such as env:NAME or file:/path", what)
	}
	if scheme == "http" || scheme == "https" || strings.HasPrefix(value, "//") {
		return fmt.Errorf("%s is a URL, not a reference: put the URL in an environment variable or a file and pass env:NAME or file:/path, so the database never holds it", what)
	}
	if reg == nil {
		reg = secret.NewRegistry(nil)
	}
	for _, s := range reg.Schemes() {
		if s == scheme && s != "agent" {
			return nil
		}
	}
	return fmt.Errorf("%s: %q is not a reference scheme this build understands (env, file, exec, keyring)", what, scheme)
}
