package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/notify"
	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestNotifyFromTheCommandLine(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	n := notify.New(st, nil, nil)
	run := func(o notifyOpts) (string, error) {
		var buf bytes.Buffer
		err := runNotify(ctx, n, o, &buf)
		return buf.String(), err
	}

	var hits atomic.Int32
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1) }))
	defer hook.Close()
	t.Setenv("KRYOSTATE_CLI_TEST_HOOK", hook.URL+"/hook")

	if _, err := run(notifyOpts{addWebhook: true, name: "ops", urlRef: "env:KRYOSTATE_CLI_TEST_HOOK", format: "slack"}); err != nil {
		t.Fatal(err)
	}
	if _, err := run(notifyOpts{addEmail: true, name: "team", to: "ops@example.com,dba@example.com", from: "kryostate@example.com",
		smtp: "smtp.example.com:587", user: "kryostate", passwordRef: "env:KRYOSTATE_SMTP_PASSWORD", starttls: true}); err != nil {
		t.Fatal(err)
	}
	out, err := run(notifyOpts{list: true})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ops", "slack webhook", "team", "ops@example.com,dba@example.com via smtp.example.com:587 (starttls)", "daily at 08:00"} {
		if !strings.Contains(out, want) {
			t.Errorf("list lacks %q:\n%s", want, out)
		}
	}

	for name, o := range map[string]notifyOpts{
		"url instead of a reference": {addWebhook: true, name: "x", urlRef: "https://hooks.example.com/T0/B0/x", format: "slack"},
		"duplicate name":             {addWebhook: true, name: "ops", urlRef: "env:X", format: "slack"},
		"unknown format":             {addWebhook: true, name: "y", urlRef: "env:X", format: "xml"},
		"no recipients":              {addEmail: true, name: "z", from: "a@example.com", smtp: "h:25"},
		"bad port":                   {addEmail: true, name: "z", to: "a@example.com", from: "a@example.com", smtp: "h"},
		"password in clear":          {addEmail: true, name: "z", to: "a@example.com", from: "a@example.com", smtp: "h:25", user: "u", passwordRef: "env:P"},
		"header in address":          {addEmail: true, name: "z", to: "a@example.com\nBcc: b@example.com", from: "a@example.com", smtp: "h:25"},
		"bad base url":               {baseURL: "javascript:alert(1)"},
		"bad time":                   {at: "25:00"},
		"remove unknown":             {remove: "nope"},
	} {
		if _, err := run(o); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}

	if _, err := run(notifyOpts{test: "ops"}); err != nil || hits.Load() != 1 {
		t.Errorf("test: %v, %d hits", err, hits.Load())
	}
	if _, err := run(notifyOpts{baseURL: "https://kryostate.example.com/", at: "7:30"}); err != nil {
		t.Fatal(err)
	}
	set, _ := st.GetNotifySettings(ctx)
	if set.BaseURL != "https://kryostate.example.com" || set.At != "07:30" {
		t.Errorf("settings: %+v", set)
	}
	out, err = run(notifyOpts{preview: true})
	if err != nil || !strings.Contains(out, "KryoState daily digest") || !strings.Contains(out, "https://kryostate.example.com/changes") {
		t.Errorf("preview: %v\n%s", err, out)
	}
	if _, err := run(notifyOpts{remove: "ops"}); err != nil {
		t.Fatal(err)
	}
	if ds, _ := st.NotifyDests(ctx); len(ds) != 1 || ds[0].Name != "team" {
		t.Errorf("after remove: %+v", ds)
	}
}
