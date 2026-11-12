package web

import (
	"context"
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
	runs := map[string][]string{}
	cs, _ := st.Choices(ctx)
	for _, c := range cs {
		runs[c.Template] = c.Stacks
	}
	if len(runs) != 3 || len(runs["python.version"]) != 0 || strings.Join(runs["java.version"], ",") != "data" {
		t.Fatalf("after the group: %v", runs)
	}

	// From the edge stack's card, the same group adds edge beside data.
	_, msg = followFlash(t, h, post(t, h, "/stacks/collectors", url.Values{"stack": {"edge"}, "what": {"group.runtimes"}}))
	if !strings.Contains(msg, "Language runtimes") {
		t.Errorf("message %q", msg)
	}
	cs, _ = st.Choices(ctx)
	for _, c := range cs {
		if c.Template == "java.version" && strings.Join(c.Stacks, ",") != "data,edge" {
			t.Errorf("java runs on %v", c.Stacks)
		}
	}
	// Doing it again changes nothing, and says so.
	if _, msg := followFlash(t, h, post(t, h, "/stacks/collectors", url.Values{"stack": {"edge"}, "what": {"group.runtimes"}})); !strings.Contains(msg, "nothing changed") {
		t.Errorf("repeat gave %q", msg)
	}
	// A single entry from the card, and things the card may not switch on.
	post(t, h, "/stacks/collectors", url.Values{"stack": {"edge"}, "what": {"cpu.cores"}})
	for _, what := range []string{"file.contents", "file.exists", "made.up"} {
		if _, msg := followFlash(t, h, post(t, h, "/stacks/collectors", url.Values{"stack": {"edge"}, "what": {what}})); !strings.Contains(msg, "choose a group") {
			t.Errorf("%s gave %q", what, msg)
		}
	}
	if _, msg := followFlash(t, h, post(t, h, "/stacks/collectors", url.Values{"stack": {"gone"}, "what": {"cpu.cores"}})); !strings.Contains(msg, "no such stack") {
		t.Errorf("unknown stack gave %q", msg)
	}

	page := get(t, h, "/stacks", nil).Body.String()
	for _, want := range []string{"Collected here", "CPU cores", "Python version", "Language runtimes"} {
		if !strings.Contains(page, want) {
			t.Errorf("stacks page lacks %q", want)
		}
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

func TestStackCardShowsRoles(t *testing.T) {
	_, h, _ := testServer(t)
	post(t, h, "/stacks", url.Values{"name": {"payments"}, "hosts": {"web01 app\ndb01 database"}})
	page := get(t, h, "/stacks", nil).Body.String()
	if !strings.Contains(page, `chip--role">database<`) || !strings.Contains(page, `chip--role">app<`) {
		t.Error("roles are not shown on the stack card")
	}
}
