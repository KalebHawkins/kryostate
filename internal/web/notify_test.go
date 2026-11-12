package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestNotificationsPage(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	seedChanges(t, st)

	var hits atomic.Int32
	var status atomic.Int32
	status.Store(200)
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		w.WriteHeader(int(status.Load()))
	}))
	defer hook.Close()
	secretURL := hook.URL + "/services/T0/B0/very-secret-token"
	t.Setenv("KRYOSTATE_WEB_TEST_HOOK", secretURL)
	t.Setenv("KRYOSTATE_WEB_TEST_PW", "smtp-password-value")
	if err := st.AddNotifyDest(ctx, store.NotifyDest{Name: "ops", Kind: store.DestWebhook, Format: store.FormatSlack,
		URLRef: "env:KRYOSTATE_WEB_TEST_HOOK"}, "t"); err != nil {
		t.Fatal(err)
	}
	if err := st.AddNotifyDest(ctx, store.NotifyDest{Name: "team", Kind: store.DestEmail, To: []string{"ops@example.com"},
		From: "kryostate@example.com", SMTPAddr: "smtp.example.com:587", SMTPUser: "u",
		PasswordRef: "env:KRYOSTATE_WEB_TEST_PW", Security: store.SecurityStartTLS}, "t"); err != nil {
		t.Fatal(err)
	}
	set := store.DefaultNotifySettings()
	set.BaseURL = "https://kryostate.example.com"
	if err := st.PutNotifySettings(ctx, set, "t"); err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/notifications", nil).Body.String()
	for _, want := range []string{"ops", "slack webhook", "env:KRYOST…", "ops@example.com via smtp.example.com:587",
		"Send a test now", `action="/notifications/test"`, `name="hour"`, `name="section"`, "https://kryostate.example.com"} {
		if !strings.Contains(body, want) {
			t.Errorf("page lacks %q", want)
		}
	}
	for _, leak := range []string{"very-secret-token", hook.URL, "smtp-password-value", "KRYOSTATE_WEB_TEST_HOOK"} {
		if strings.Contains(body, leak) {
			t.Errorf("page shows %q", leak)
		}
	}
	// There is nowhere to type an address.
	for _, field := range []string{`name="url`, `name="smtp`, `name="base_url"`, `name="to"`} {
		if strings.Contains(body, field) {
			t.Errorf("page has a %s field", field)
		}
	}
	if !strings.Contains(get(t, h, "/changes", nil).Body.String(), `href="/notifications"`) {
		t.Error("the changes page does not link to the digest")
	}

	// Settings: the safe ones are saved, anything else is ignored.
	rec := post(t, h, "/notifications", url.Values{"enabled": {"on"}, "hour": {"07"}, "minute": {"30"},
		"section": {"changes", "never"}, "quiet": {"on"}, "base_url": {"https://evil.example.com"}, "url": {"http://169.254.169.254/"}})
	_, msg := followFlash(t, h, rec)
	if !strings.Contains(msg, "07:30") {
		t.Errorf("flash %q", msg)
	}
	got, _ := st.GetNotifySettings(ctx)
	if !got.Enabled || got.At != "07:30" || !got.SendQuiet || strings.Join(got.Sections, ",") != "changes,never" ||
		got.BaseURL != "https://kryostate.example.com" || got.UpdatedBy != "operator@example.invalid" {
		t.Errorf("settings: %+v", got)
	}
	rec = post(t, h, "/notifications", url.Values{"hour": {"25"}, "minute": {"00"}})
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "hour") {
		t.Errorf("bad hour: %q", msg)
	}
	rec = post(t, h, "/notifications", url.Values{"hour": {"06"}, "minute": {"00"}})
	followFlash(t, h, rec)
	if got, _ := st.GetNotifySettings(ctx); got.Enabled || got.Has(store.SectionChanges) {
		t.Errorf("unticked boxes kept: %+v", got)
	}

	// A form from another site is refused before it reaches the handler.
	req := httptest.NewRequest(http.MethodPost, "/notifications/test", strings.NewReader("name=ops"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	cross := httptest.NewRecorder()
	h.ServeHTTP(cross, req)
	if cross.Code != http.StatusForbidden || hits.Load() != 0 {
		t.Errorf("cross-site test send: %d, %d hits", cross.Code, hits.Load())
	}

	// Send a test now.
	rec = post(t, h, "/notifications/test", url.Values{"name": {"ops"}})
	if _, msg := followFlash(t, h, rec); !strings.Contains(msg, "sent to ops") || hits.Load() != 1 {
		t.Errorf("test send: %q, %d hits", msg, hits.Load())
	}
	status.Store(500)
	rec = post(t, h, "/notifications/test", url.Values{"name": {"ops"}})
	page, msg := followFlash(t, h, rec)
	if !strings.Contains(msg, "HTTP 500") || strings.Contains(msg, "very-secret-token") || strings.Contains(page, "very-secret-token") {
		t.Errorf("failed test: %q", msg)
	}
	if !strings.Contains(page, "failed") {
		t.Error("the failed send is not on the page")
	}

	// Preview renders the digest in the page.
	set.Sections = store.DigestSections
	st.PutNotifySettings(ctx, set, "t")
	body = get(t, h, "/notifications?preview=1", nil).Body.String()
	for _, want := range []string{"Preview", "chg-new", "9.5-after", "KryoState daily digest"} {
		if !strings.Contains(body, want) {
			t.Errorf("preview lacks %q", want)
		}
	}
}
