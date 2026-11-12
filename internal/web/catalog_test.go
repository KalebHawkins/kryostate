package web

import (
	"context"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestSwitchCatalogCollectorsOnAndOff(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	seed(t, st)

	_, msg := followFlash(t, h, post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"all"}}))
	if !strings.Contains(msg, "CPU cores is on for every host") {
		t.Fatalf("message %q", msg)
	}
	// Limiting it to a stack that exists.
	post(t, h, "/stacks", url.Values{"name": {"web"}})
	post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"stacks"}, "stack": {"web"}})
	cs, _ := st.Choices(ctx)
	if len(cs) != 1 || len(cs[0].Stacks) != 1 || cs[0].Stacks[0] != "web" {
		t.Fatalf("choices %+v", cs)
	}
	if _, msg := followFlash(t, h, post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"stacks"}, "stack": {"nope"}})); !strings.Contains(msg, "no stack named nope") {
		t.Errorf("an unknown stack gave %q", msg)
	}

	// A template with a value, and one with a value that would be a command.
	post(t, h, "/collectors/choose", url.Values{"template": {"file.exists"}, "param": {"/etc/resolv.conf"}, "where": {"all"}})
	if _, msg := followFlash(t, h, post(t, h, "/collectors/choose", url.Values{"template": {"file.exists"}, "param": {"/etc/x'; reboot #"}, "where": {"all"}})); !strings.Contains(msg, "not allowed") {
		t.Errorf("an injected path gave %q", msg)
	}
	cs, _ = st.Choices(ctx)
	if len(cs) != 2 {
		t.Fatalf("choices %+v", cs)
	}
	body := get(t, h, "/collectors", nil).Body.String()
	if !strings.Contains(body, "file.resolv_conf.exists") {
		t.Error("the switched-on list does not show the new collector")
	}

	for _, c := range cs {
		post(t, h, "/collectors/remove", url.Values{"id": {strconv.FormatInt(c.ID, 10)}})
	}
	if cs, _ := st.Choices(ctx); len(cs) != 0 {
		t.Errorf("not switched off: %+v", cs)
	}
}

func TestRevealingCollectorsNeedPermission(t *testing.T) {
	s, h, st := testServer(t)
	form := url.Values{"template": {"file.contents"}, "param": {"/etc/resolv.conf"}, "where": {"all"}}
	if _, msg := followFlash(t, h, post(t, h, "/collectors/choose", form)); !strings.Contains(msg, "-allow-content-collectors") {
		t.Errorf("message %q", msg)
	}
	if cs, _ := st.Choices(context.Background()); len(cs) != 0 {
		t.Fatal("a file-contents collector was switched on without permission")
	}
	s.AllowRevealing = true
	post(t, h, "/collectors/choose", form)
	if cs, _ := st.Choices(context.Background()); len(cs) != 1 {
		t.Error("with permission it was still refused")
	}
}

func TestTryNeedsAConnection(t *testing.T) {
	_, h, st := testServer(t)
	id := seed(t, st)
	post(t, h, "/collectors/choose", url.Values{"template": {"cpu.cores"}, "where": {"all"}})
	cs, _ := st.Choices(context.Background())
	rec := post(t, h, "/collectors/try", url.Values{"choice": {strconv.FormatInt(cs[0].ID, 10)}, "host": {id}})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), "has no enabled connection") {
		t.Errorf("got %d: %s", rec.Code, rec.Body.String())
	}
	// Only switched-on catalog entries and loaded specs can be tried.
	rec = post(t, h, "/collectors/try", url.Values{"spec": {"made.up"}, "host": {id}})
	if rec.Code != 303 {
		t.Errorf("an unknown spec returned %d", rec.Code)
	}
}

func TestAddAStackOfHostsAtOnce(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	rec := post(t, h, "/stacks", url.Values{"name": {"payments"}, "user": {"svc-ro"},
		"hosts": {"web01 app\nweb02 app web02.example.invalid\ndb01 database"}})
	_, msg := followFlash(t, h, rec)
	if !strings.Contains(msg, "3 hosts added to payments (3 of them new, 3 given a connection)") {
		t.Fatalf("message %q", msg)
	}
	id, err := st.LookupCI(ctx, "web02.example.invalid")
	if err != nil {
		t.Fatal(err)
	}
	if ms, _ := st.MembershipsOf(ctx, id); len(ms) != 1 || ms[0].Role != "app" {
		t.Errorf("memberships %+v", ms)
	}
	_, msg = followFlash(t, h, post(t, h, "/stacks/hosts", url.Values{"stack": {"payments"}, "hosts": {"lb01 loadbalancer\nbad name with spaces ok"}}))
	if !strings.Contains(msg, "2 hosts added") {
		t.Errorf("second batch: %q", msg)
	}
	if _, msg := followFlash(t, h, post(t, h, "/stacks/hosts", url.Values{"stack": {"payments"}, "hosts": {"web03"}, "user": {"bad user"}})); !strings.Contains(msg, "spaces") {
		t.Errorf("a bad user gave %q", msg)
	}
}
