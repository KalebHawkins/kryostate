package notify

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/secret"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "n.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// seedDigest gives web1 two changed values and a failure in the last hour
// after a good collection yesterday, db1 one change and a recovery, and app1
// nothing at all.
func seedDigest(t *testing.T, st *store.Store, now time.Time) map[string]string {
	t.Helper()
	ctx := context.Background()
	src, err := st.SourceID(ctx, model.SourceCollector, "builtin")
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, h := range []string{"web1", "db1", "app1"} {
		id, _, err := st.ResolveCI(ctx, model.KindHost, h, []model.Identity{{Scheme: model.IdentHostname, Value: h}})
		if err != nil {
			t.Fatal(err)
		}
		ids[h] = id
	}
	put := func(h, attr, raw string, ago time.Duration) {
		t.Helper()
		if _, err := st.Assert(ctx, model.Assertion{CIID: ids[h], Attr: attr, Raw: raw, SourceID: src,
			ObservedAt: now.Add(-ago)}, nil); err != nil {
			t.Fatal(err)
		}
	}
	put("web1", "os.version", "9.4", 30*time.Hour)
	put("web1", "kernel.release", "5.14", 30*time.Hour)
	put("db1", "os.version", "8", 30*time.Hour)
	put("web1", "os.version", "9.5", 2*time.Hour)
	put("web1", "kernel.release", "5.15", time.Hour)
	put("db1", "os.version", "9", 3*time.Hour)
	probe := func(h string, o model.Outcome, detail string, ago time.Duration) {
		t.Helper()
		if err := st.RecordProbe(ctx, model.Probe{CIID: ids[h], Kind: model.ProbeCollectorRun, Outcome: o,
			Detail: detail, At: now.Add(-ago)}); err != nil {
			t.Fatal(err)
		}
	}
	probe("web1", model.OutcomeOK, "", 30*time.Hour)
	probe("web1", model.OutcomeRefused, "connection refused", time.Hour)
	probe("db1", model.OutcomeRefused, "connection refused", 30*time.Hour)
	probe("db1", model.OutcomeOK, "", 2*time.Hour)
	return ids
}

func TestDigestText(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Now()
	seedDigest(t, st, now)
	set := store.DefaultNotifySettings()
	set.BaseURL = "https://kryostate.example.com/"
	d, err := Build(ctx, st, set, now.Add(-24*time.Hour), now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := "KryoState daily digest\n" + d.Window() + `

3 values changed on 2 hosts
  web1
    os.version: 9.4 → 9.5
    kernel.release: 5.14 → 5.15
  db1
    os.version: 8 → 9

1 host started failing
  web1: refused, connection refused
1 host recovered
  db1

1 host never collected
  app1

Open KryoState: https://kryostate.example.com/changes
`
	if got := d.Text(); got != want {
		t.Errorf("digest text:\n%s\nwant:\n%s", got, want)
	}
	if d.Quiet() {
		t.Error("a digest with changes is quiet")
	}
	if s := d.Subject(); s != "KryoState daily digest: 3 changes, 1 started failing, 1 recovered" {
		t.Errorf("subject %q", s)
	}

	// Sections switched off are left out, and a window with nothing in it
	// is quiet even though a host has never been collected.
	set.Sections = []string{store.SectionNever}
	d, err = Build(ctx, st, set, now.Add(-time.Minute), now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Quiet() || strings.Contains(d.Text(), "changed") || !strings.Contains(d.Text(), "app1") {
		t.Errorf("quiet digest:\n%s", d.Text())
	}

	// Extra sections arrive only when their section is switched on.
	src := func(context.Context, time.Time, time.Time) (Section, error) {
		return Section{Key: store.SectionRules, Title: "Rules", Lines: []string{"openssl 3.2: 4 of 9 hosts"}, Count: 1}, nil
	}
	d, _ = Build(ctx, st, set, now.Add(-time.Minute), now, time.UTC, []SectionSource{src})
	if len(d.Extra) != 0 {
		t.Error("a switched-off extra section was included")
	}
	set.Sections = append(set.Sections, store.SectionRules)
	d, _ = Build(ctx, st, set, now.Add(-time.Minute), now, time.UTC, []SectionSource{src})
	if d.Quiet() || !strings.Contains(d.Text(), "openssl 3.2: 4 of 9 hosts") {
		t.Errorf("extra section missing:\n%s", d.Text())
	}
}

func TestDigestCaps(t *testing.T) {
	var cs []store.Change
	now := time.Now()
	for h := 0; h < MaxHosts+3; h++ {
		for i := 0; i < 2; i++ {
			cs = append(cs, store.Change{CIID: string(rune('a' + h)), Host: string(rune('a' + h)), Attr: "x", Old: "1", New: "2", At: now.Add(-time.Minute)})
		}
	}
	for i := 0; i < MaxPerHost+5; i++ {
		cs = append(cs, store.Change{CIID: "big", Host: "big", Attr: "y", Old: "1", New: "2", At: now.Add(-time.Minute)})
	}
	d := Digest{Until: now, Loc: time.UTC, Settings: store.DefaultNotifySettings()}
	d.groupChanges(cs)
	if d.Changes[0].Host != "big" || d.Changes[0].More != 5 || len(d.Changes[0].Items) != MaxPerHost {
		t.Errorf("busiest host first and capped: %+v", d.Changes[0])
	}
	if len(d.Changes) != MaxHosts || d.MoreHosts != 4 || d.HostsChanged != MaxHosts+4 {
		t.Errorf("host cap: %d shown, %d more, %d total", len(d.Changes), d.MoreHosts, d.HostsChanged)
	}
	if !strings.Contains(d.Text(), "… and 4 more hosts") || !strings.Contains(d.Text(), "… and 5 more on this host") {
		t.Errorf("caps not mentioned:\n%s", d.Text())
	}
}

// hook is a webhook receiver that records what it was sent.
type hook struct {
	mu     sync.Mutex
	bodies [][]byte
	agents []string
	fail   atomic.Int32 // answer 500 this many more times
	srv    *httptest.Server
}

func newHook(t *testing.T) *hook {
	h := &hook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		h.mu.Lock()
		h.bodies = append(h.bodies, b)
		h.agents = append(h.agents, r.Header.Get("User-Agent"))
		h.mu.Unlock()
		if h.fail.Load() > 0 {
			h.fail.Add(-1)
			http.Error(w, "nope", http.StatusInternalServerError)
			return
		}
		w.Write([]byte("ok"))
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *hook) count() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies)
}

func (h *hook) last(t *testing.T) map[string]any {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	var m map[string]any
	if err := json.Unmarshal(h.bodies[len(h.bodies)-1], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func TestWebhookFormats(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	now := time.Now()
	seedDigest(t, st, now)
	h := newHook(t)
	secretURL := h.srv.URL + "/services/T000/B000/sekrit-token"
	t.Setenv("KRYOSTATE_TEST_HOOK", secretURL)
	n := New(st, secret.NewRegistry(nil), nil)
	set := store.DefaultNotifySettings()
	d, err := Build(ctx, st, set, now.Add(-24*time.Hour), now, time.UTC, nil)
	if err != nil {
		t.Fatal(err)
	}
	msg := d.Message()

	slack := store.NotifyDest{Name: "ops", Kind: store.DestWebhook, Format: store.FormatSlack, URLRef: "env:KRYOSTATE_TEST_HOOK"}
	if err := n.Deliver(ctx, slack, msg, now); err != nil {
		t.Fatal(err)
	}
	body := h.last(t)
	if !strings.Contains(body["text"].(string), "web1") {
		t.Errorf("slack text: %v", body["text"])
	}
	blocks := body["blocks"].([]any)
	if len(blocks) < 3 {
		t.Fatalf("slack blocks: %v", blocks)
	}
	raw, _ := json.Marshal(blocks)
	if !strings.Contains(string(raw), "9.4 → 9.5") || !strings.Contains(string(raw), "mrkdwn") {
		t.Errorf("slack blocks lack the change: %s", raw)
	}
	if h.agents[0] != "KryoState" {
		t.Errorf("user agent %q", h.agents[0])
	}

	generic := slack
	generic.Format = store.FormatGeneric
	if err := n.Deliver(ctx, generic, msg, now); err != nil {
		t.Fatal(err)
	}
	body = h.last(t)
	counts := body["counts"].(map[string]any)
	if body["source"] != "KryoState" || counts["changes"].(float64) != 3 || counts["started_failing"].(float64) != 1 {
		t.Errorf("generic body: %v", body)
	}
	if _, ok := body["blocks"]; ok {
		t.Error("generic body carries slack blocks")
	}

	// A failure says what happened without the URL, which is the secret.
	h.fail.Store(1)
	err = n.Deliver(ctx, slack, msg, now)
	if err == nil || !strings.Contains(err.Error(), "HTTP 500") || strings.Contains(err.Error(), "sekrit") {
		t.Errorf("failure: %v", err)
	}
	h.srv.Close()
	err = n.Deliver(ctx, slack, msg, now)
	if err == nil || strings.Contains(err.Error(), "sekrit") || strings.Contains(err.Error(), "/services/") {
		t.Errorf("connection failure leaks the URL: %v", err)
	}
	// An unset reference fails without guessing.
	t.Setenv("KRYOSTATE_TEST_HOOK", "")
	if err := n.Deliver(ctx, slack, msg, now); err == nil {
		t.Error("an empty reference was used")
	}
}

func TestWebhookRefusesRedirectElsewhere(t *testing.T) {
	var hit atomic.Bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit.Store(true) }))
	defer other.Close()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/x", http.StatusTemporaryRedirect)
	}))
	defer first.Close()
	err := postWebhook(context.Background(), newHTTPClient(), first.URL+"/hook", store.FormatSlack, TestMessage(time.Now()))
	if err == nil || hit.Load() {
		t.Errorf("redirect to another host followed: %v", err)
	}
	for _, bad := range []string{"ftp://example.com/x", "file:///etc/passwd", "https://user:pw@example.com/x", "not a url"} {
		if err := postWebhook(context.Background(), newHTTPClient(), bad, store.FormatSlack, Message{}); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// fakeSMTP is just enough of an SMTP server to take one message. It offers
// STARTTLS only when told to, and never completes it.
type fakeSMTP struct {
	ln       net.Listener
	offerTLS bool
	mu       sync.Mutex
	from     string
	rcpt     []string
	data     string
}

func newFakeSMTP(t *testing.T, offerTLS bool) *fakeSMTP {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSMTP{ln: ln, offerTLS: offerTLS}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSMTP) serve(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(s string) { io.WriteString(c, s+"\r\n") }
	say("220 fake ESMTP")
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.ToUpper(strings.TrimSpace(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			if f.offerTLS {
				say("250-fake")
				say("250 STARTTLS")
			} else {
				say("250 fake")
			}
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			f.mu.Lock()
			f.from = strings.TrimSpace(line[len("MAIL FROM:"):])
			f.mu.Unlock()
			say("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			f.mu.Lock()
			f.rcpt = append(f.rcpt, strings.TrimSpace(line[len("RCPT TO:"):]))
			f.mu.Unlock()
			say("250 ok")
		case cmd == "DATA":
			say("354 go ahead")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			f.mu.Lock()
			f.data = b.String()
			f.mu.Unlock()
			say("250 queued")
		case cmd == "QUIT":
			say("221 bye")
			return
		default:
			say("502 not here")
		}
	}
}

func TestEmail(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	f := newFakeSMTP(t, false)
	n := New(st, secret.NewRegistry(nil), nil)
	d := store.NotifyDest{Name: "team", Kind: store.DestEmail, To: []string{"ops@example.com", "Dba Team <dba@example.com>"},
		From: "kryostate@example.com", SMTPAddr: f.ln.Addr().String(), Security: store.SecurityNone}
	m := Message{Subject: "KryoState daily digest: 3 changes", Text: "web1\n  os.version: 9.4 → 9.5\n"}
	if err := n.Deliver(ctx, d, m, time.Now()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	from, rcpt, data := f.from, f.rcpt, f.data
	f.mu.Unlock()
	if from != "<kryostate@example.com>" || len(rcpt) != 2 || rcpt[1] != "<dba@example.com>" {
		t.Errorf("envelope: %q %q", from, rcpt)
	}
	head, body, _ := strings.Cut(data, "\r\n\r\n")
	for _, want := range []string{"Subject: KryoState daily digest: 3 changes", "Content-Transfer-Encoding: quoted-printable", "Auto-Submitted: auto-generated", "To: <ops@example.com>"} {
		if !strings.Contains(head, want) {
			t.Errorf("headers lack %q:\n%s", want, head)
		}
	}
	dec, _ := io.ReadAll(quotedprintable.NewReader(strings.NewReader(body)))
	if !strings.Contains(string(dec), "os.version: 9.4 → 9.5") {
		t.Errorf("body: %q", dec)
	}

	// STARTTLS asked for and not offered is refused, not quietly skipped.
	plain := newFakeSMTP(t, false)
	d.SMTPAddr, d.Security = plain.ln.Addr().String(), store.SecurityStartTLS
	if err := n.Deliver(ctx, d, m, time.Now()); err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("missing STARTTLS: %v", err)
	}
	// So is a password over a connection that is not encrypted, and the
	// password never appears in the error.
	t.Setenv("KRYOSTATE_TEST_SMTP", "hunter2-password")
	d.Security, d.SMTPUser, d.PasswordRef = store.SecurityNone, "u", "env:KRYOSTATE_TEST_SMTP"
	if err := n.Deliver(ctx, d, m, time.Now()); err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("password over plain smtp: %v", err)
	}

	// Addresses that would smuggle a header are refused.
	if _, err := BuildEmail("kryostate@example.com", []string{"a@example.com\r\nBcc: evil@example.com"}, m, time.Now()); err == nil {
		t.Error("header injection accepted")
	}
	msg, err := BuildEmail("kryostate@example.com", []string{"a@example.com"}, Message{Subject: "line\r\nBcc: x@example.com", Text: "x"}, time.Now())
	if err != nil || strings.Contains(string(msg), "\r\nBcc:") {
		t.Errorf("subject injection: %v\n%s", err, msg)
	}
}

// TestScheduledOnce is the scheduling contract: nothing before the time,
// one digest at it, none again the same day even after a restart, and the
// next one the following day.
func TestScheduledOnce(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	h := newHook(t)
	t.Setenv("KRYOSTATE_TEST_HOOK", h.srv.URL+"/hook")
	if err := st.AddNotifyDest(ctx, store.NotifyDest{Name: "ops", Kind: store.DestWebhook, Format: store.FormatSlack,
		URLRef: "env:KRYOSTATE_TEST_HOOK"}, "t"); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultNotifySettings()
	set.SendQuiet = true
	if err := st.PutNotifySettings(ctx, set, "t"); err != nil {
		t.Fatal(err)
	}
	loc := time.FixedZone("Test", 2*3600)
	at := func(day, hh, mm int) time.Time { return time.Date(2026, 11, day, hh, mm, 0, 0, loc) }

	n := New(st, nil, nil)
	for _, tick := range []time.Time{at(12, 7, 59), at(12, 8, 0), at(12, 8, 30)} {
		if err := n.Tick(ctx, tick, loc); err != nil {
			t.Fatal(err)
		}
	}
	if h.count() != 1 {
		t.Fatalf("sent %d times on the first day, want 1", h.count())
	}
	restarted := New(st, nil, nil)
	if err := restarted.Tick(ctx, at(12, 9, 0), loc); err != nil {
		t.Fatal(err)
	}
	if h.count() != 1 {
		t.Fatalf("sent again after a restart")
	}
	if err := restarted.Tick(ctx, at(13, 8, 1), loc); err != nil {
		t.Fatal(err)
	}
	if h.count() != 2 {
		t.Fatalf("next day: %d sends", h.count())
	}
	sends, _ := st.NotifySends(ctx, 10)
	if len(sends) != 2 || !sends[0].OK || sends[0].Slot != "2026-11-13" || sends[0].Dest != "ops" {
		t.Errorf("history: %+v", sends)
	}
	// The second digest starts where the first ended.
	if !sends[0].WindowStart.Equal(at(12, 8, 0)) {
		t.Errorf("window start %v", sends[0].WindowStart)
	}

	// A quiet day is not sent when send-when-quiet is off, and is recorded
	// so it is not reconsidered every tick.
	set.SendQuiet = false
	st.PutNotifySettings(ctx, set, "t")
	if err := restarted.Tick(ctx, at(14, 8, 0), loc); err != nil {
		t.Fatal(err)
	}
	if h.count() != 2 {
		t.Error("a quiet day was sent")
	}
	if done, _ := st.DigestSlotDone(ctx, "2026-11-14"); !done[""] {
		t.Error("the quiet day was not recorded")
	}

	// Turned off, nothing goes.
	set.Enabled, set.SendQuiet = false, true
	st.PutNotifySettings(ctx, set, "t")
	restarted.Tick(ctx, at(15, 9, 0), loc)
	if h.count() != 2 {
		t.Error("sent while off")
	}
}

func TestRetryThenRecord(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	h := newHook(t)
	t.Setenv("KRYOSTATE_TEST_HOOK", h.srv.URL+"/hook")
	st.AddNotifyDest(ctx, store.NotifyDest{Name: "ops", Kind: store.DestWebhook, URLRef: "env:KRYOSTATE_TEST_HOOK"}, "t")
	set := store.DefaultNotifySettings()
	set.SendQuiet = true
	st.PutNotifySettings(ctx, set, "t")
	loc := time.UTC
	n := New(st, nil, nil)
	var waits []time.Duration
	n.Backoff = time.Second
	n.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }

	h.fail.Store(2)
	if err := n.Tick(ctx, time.Date(2026, 11, 12, 9, 0, 0, 0, loc), loc); err != nil {
		t.Fatal(err)
	}
	sends, _ := st.NotifySends(ctx, 1)
	if !sends[0].OK || sends[0].Attempts != 3 || len(waits) != 2 || waits[1] != 2*time.Second {
		t.Errorf("retry: %+v waits %v", sends[0], waits)
	}

	h.fail.Store(10)
	err := n.Tick(ctx, time.Date(2026, 11, 13, 9, 0, 0, 0, loc), loc)
	if err == nil {
		t.Error("a failed digest reported no error")
	}
	sends, _ = st.NotifySends(ctx, 1)
	if sends[0].OK || !strings.Contains(sends[0].Error, "HTTP 500") || sends[0].Attempts != 3 {
		t.Errorf("failure record: %+v", sends[0])
	}
	before := h.count()
	n.Tick(ctx, time.Date(2026, 11, 13, 9, 1, 0, 0, loc), loc)
	if h.count() != before {
		t.Error("a failed day was retried on the next tick")
	}
}

func TestCheckRef(t *testing.T) {
	reg := secret.NewRegistry(nil)
	for _, ok := range []string{"env:SLACK_URL", "file:/etc/kryostate/hook", "keyring:kryostate/slack"} {
		if err := CheckRef(reg, ok, "ref"); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"https://hooks.example.com/x", "http://example.com", "SLACK_URL", "env:", "agent:", "nope:x"} {
		if err := CheckRef(reg, bad, "ref"); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	if MaskRef("env:KRYOSTATE_SLACK_URL") != "env:KRYOST…" || MaskRef("env:A") != "env:A" {
		t.Errorf("mask: %s", MaskRef("env:KRYOSTATE_SLACK_URL"))
	}
	if got := scrub(errors.New("dial sekrit-url: refused"), "sekrit-url").Error(); got != "dial [redacted]: refused" {
		t.Errorf("scrub: %q", got)
	}
	if scrub(nil, "x") != nil {
		t.Error("scrub invented an error")
	}
}
