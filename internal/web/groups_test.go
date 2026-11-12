package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestGroupsOnlyWidenWhereCollectorsRun(t *testing.T) {
	_, h, st := testServer(t)
	ctx := context.Background()
	post(t, h, "/stacks", url.Values{"name": {"data"}})
	post(t, h, "/stacks", url.Values{"name": {"edge"}})

	// Python on for every host, then the runtimes group for one stack:
	// Python must stay on everywhere.
	post(t, h, "/collectors/choose", url.Values{"template": {"python.version"}, "where": {"all"}})
	_, msg := followFlash(t, h, post(t, h, "/collectors/group",
		url.Values{"group": {"group.runtimes"}, "where": {"stacks"}, "stack": {"data"}}))
	if !strings.Contains(msg, "already covered") {
		t.Errorf("message %q", msg)
	}
	runs := map[string]string{}
	cs, _ := st.Choices(ctx)
	for _, c := range cs {
		runs[c.Template] = c.Target
	}
	if len(runs) != 3 || runs["python.version"] != "" || runs["java.version"] != `stack == "data"` {
		t.Fatalf("after the group: %v", runs)
	}

	// The same group for edge adds edge beside data.
	edge := url.Values{"group": {"group.runtimes"}, "where": {"stacks"}, "stack": {"edge"}}
	_, msg = followFlash(t, h, post(t, h, "/collectors/group", edge))
	if !strings.Contains(msg, "Language runtimes") {
		t.Errorf("message %q", msg)
	}
	cs, _ = st.Choices(ctx)
	for _, c := range cs {
		if c.Template == "java.version" && c.Target != `stack in ("data", "edge")` {
			t.Errorf("java runs on %v", c.Target)
		}
	}
	// Doing it again changes nothing, and says so.
	if _, msg := followFlash(t, h, post(t, h, "/collectors/group", edge)); !strings.Contains(msg, "nothing changed") {
		t.Errorf("repeat gave %q", msg)
	}
	// Things a group button may not switch on.
	for _, what := range []string{"file.contents", "file.exists", "made.up"} {
		if _, msg := followFlash(t, h, post(t, h, "/collectors/group", url.Values{"group": {what}, "where": {"all"}})); !strings.Contains(msg, "choose a group") {
			t.Errorf("%s gave %q", what, msg)
		}
	}

	if page := get(t, h, "/stacks/edge", nil).Body.String(); strings.Contains(page, "Collected here") || strings.Contains(page, "/stacks/collectors") {
		t.Error("the stack page still has its Collected here card")
	}
	if rec := post(t, h, "/stacks/collectors", url.Values{"stack": {"edge"}, "what": {"cpu.cores"}}); rec.Code != http.StatusMethodNotAllowed && rec.Code != http.StatusNotFound {
		t.Errorf("POST /stacks/collectors gave %d; the route is gone", rec.Code)
	}
}

func TestConfigValueFromTheWeb(t *testing.T) {
	s, h, st := testServer(t)
	s.AllowRevealing = true
	form := url.Values{"template": {"config.value"}, "param": {"/etc/app/app.conf"},
		"section": {"database"}, "key": {"host"}, "where": {"all"}}
	_, msg := followFlash(t, h, post(t, h, "/collectors/choose", form))
	if !strings.Contains(msg, "app.conf [database] host") || !strings.Contains(msg, "conf.app_conf.database.host") {
		t.Errorf("message %q", msg)
	}
	form.Set("key", "")
	if _, msg := followFlash(t, h, post(t, h, "/collectors/choose", form)); !strings.Contains(msg, "name the key") {
		t.Errorf("no key gave %q", msg)
	}
	form.Set("key", "x';id;'")
	if _, msg := followFlash(t, h, post(t, h, "/collectors/choose", form)); !strings.Contains(msg, "not allowed") {
		t.Errorf("an injected key gave %q", msg)
	}
	if cs, _ := st.Choices(context.Background()); len(cs) != 1 {
		t.Errorf("choices %+v", cs)
	}
	if !strings.Contains(get(t, h, "/collectors", nil).Body.String(), "Host basics") {
		t.Error("the collectors page lacks the groups")
	}
	page := get(t, h, "/collectors/config.value", nil).Body.String()
	if !strings.Contains(page, `name="section"`) || !strings.Contains(page, "app.conf [database] host") {
		t.Error("the config value page lacks the config fields or the switched-on setting")
	}
}

func TestStackPageShowsRoles(t *testing.T) {
	_, h, _ := testServer(t)
	post(t, h, "/stacks", url.Values{"name": {"payments"}, "hosts": {"web01 app\ndb01 database"}})
	page := get(t, h, "/stacks/payments", nil).Body.String()
	if !strings.Contains(page, `chip--role">database<`) || !strings.Contains(page, `chip--role">app<`) {
		t.Error("roles are not shown on the stack page")
	}
}
