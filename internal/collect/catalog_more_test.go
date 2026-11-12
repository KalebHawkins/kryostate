package collect

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/model"
)

func TestConfigParamRoundTrip(t *testing.T) {
	cases := []struct{ path, section, key, joined, text, attr string }{
		{"/opt/splunk/etc/system/local/server.conf", "general", "serverName",
			"/opt/splunk/etc/system/local/server.conf#[general]serverName",
			"/opt/splunk/etc/system/local/server.conf [general] serverName", "conf.server_conf.general.servername"},
		{"/etc/app.properties", "", "db.port", "/etc/app.properties#db.port",
			"/etc/app.properties db.port", "conf.app_properties.db_port"},
		// Brackets typed around the section are tolerated.
		{" /etc/x.ini ", "[main]", " port ", "/etc/x.ini#[main]port", "/etc/x.ini [main] port", "conf.x_ini.main.port"},
	}
	tpl, _ := Lookup("config.value")
	for _, c := range cases {
		got := ConfigParam(c.path, c.section, c.key)
		if got != c.joined {
			t.Errorf("ConfigParam = %q, want %q", got, c.joined)
		}
		if err := CheckParam(ParamConfig, got); err != nil {
			t.Errorf("%q refused: %v", got, err)
		}
		if txt := ParamText(tpl, got); txt != c.text {
			t.Errorf("ParamText = %q, want %q", txt, c.text)
		}
		if a := DefaultAttr(tpl, got); a != c.attr {
			t.Errorf("DefaultAttr = %q, want %q", a, c.attr)
		}
		if err := CheckAttr(DefaultAttr(tpl, got)); err != nil {
			t.Error(err)
		}
	}
}

func TestConfigValueCommandQuotesEveryPart(t *testing.T) {
	specs, err := Choice{ID: 3, Template: "config.value",
		Param: "/opt/splunk/etc/system/local/server.conf#[general]serverName"}.Specs()
	if err != nil {
		t.Fatal(err)
	}
	unix := specs[0].Command
	for _, want := range []string{"-v s='general'", "-v k='serverName'", "' '/opt/splunk/etc/system/local/server.conf'"} {
		if !strings.Contains(unix, want) {
			t.Errorf("unix command lacks %q:\n%s", want, unix)
		}
	}
	win := specs[1].Command
	for _, want := range []string{"$s = 'general'", "$k = 'serverName'", "-LiteralPath '/opt/splunk/etc/system/local/server.conf'"} {
		if !strings.Contains(win, want) {
			t.Errorf("windows command lacks %q:\n%s", want, win)
		}
	}
	// No section reads the key anywhere in the file.
	specs, _ = Choice{ID: 4, Template: "config.value", Param: "/etc/app.properties#db.port"}.Specs()
	if !strings.Contains(specs[0].Command, "-v s=''") {
		t.Errorf("empty section not passed as '': %s", specs[0].Command)
	}
	if specs[0].Attr != "conf.app_properties.db_port" {
		t.Errorf("attr %s", specs[0].Attr)
	}
}

func TestGroupsNameFixedEntries(t *testing.T) {
	seen := map[string]bool{}
	for _, g := range Groups {
		if seen[g.ID] || !strings.HasPrefix(g.ID, "group.") {
			t.Errorf("group id %q is duplicated or lacks the group. prefix", g.ID)
		}
		seen[g.ID] = true
		if _, clash := Lookup(g.ID); clash {
			t.Errorf("group %s has the same id as a catalog entry", g.ID)
		}
		if len(g.Entries) == 0 {
			t.Errorf("group %s is empty", g.ID)
		}
		for _, id := range g.Entries {
			tpl, ok := Lookup(id)
			if !ok {
				t.Errorf("group %s names unknown entry %s", g.ID, id)
			} else if !tpl.Fixed() || tpl.Reveals {
				t.Errorf("group %s holds %s, which needs a value or reveals content", g.ID, id)
			}
		}
		if len(g.Titles()) != len(g.Entries) {
			t.Errorf("group %s titles do not match entries", g.ID)
		}
	}
}

func TestSbinCommandsReachSystemPrograms(t *testing.T) {
	for _, id := range []string{"selinux", "nginx.version", "httpd.version"} {
		specs, _ := Choice{Template: id}.Specs()
		if !strings.Contains(specs[0].Command, "/usr/sbin") {
			t.Errorf("%s does not add /usr/sbin to the PATH: %s", id, specs[0].Command)
		}
	}
}

func TestExplain(t *testing.T) {
	cases := []struct {
		r    Reading
		want string
	}{
		{Reading{Err: errors.New("nginx exited 127: bash: nginx: command not found")}, "not installed"},
		{Reading{Err: errors.New("cat exited 1: cat: /opt/splunk/etc/splunk.version: No such file or directory")}, "not there"},
		{Reading{Err: errors.New("cat exited 1: cat: /etc/shadow: Permission denied")}, "not allowed"},
		{Reading{Err: errors.New("ps exited 1: ")}, "found nothing"},
		{Reading{Ran: true, Output: "hello", Err: errors.New("no match")}, "rule found nothing"},
		{Reading{Ran: true, Value: "x"}, ""},
	}
	for _, c := range cases {
		got := Explain(c.r)
		if c.want == "" && got != "" || !strings.Contains(got, c.want) {
			t.Errorf("Explain(%v) = %q, want it to mention %q", c.r.Err, got, c.want)
		}
	}
}

func TestFirstWordSkipsSetup(t *testing.T) {
	for in, want := range map[string]string{
		"getenforce": "getenforce",
		`PATH="$PATH:/usr/sbin:/sbin"; getenforce`:    "getenforce",
		`PATH="$PATH:/usr/sbin:/sbin"; nginx -v 2>&1`: "nginx",
		"LANG=C rpm -q openssl":                       "rpm",
		"cat /opt/splunkforwarder/etc/splunk.version": "cat",
	} {
		if got := firstWord(in); got != want {
			t.Errorf("firstWord(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestShortLabels(t *testing.T) {
	cases := []struct {
		c    Choice
		want string
	}{
		{Choice{Template: "cpu.cores"}, "CPU cores"},
		{Choice{Template: "file.exists", Param: "/etc/resolv.conf"}, "/etc/resolv.conf exists"},
		{Choice{Template: "env.var", Param: "SPLUNK_HOME"}, "Variable SPLUNK_HOME"},
		{Choice{Template: "package.version", Param: "openssl"}, "openssl package version"},
		{Choice{Template: "config.value", Param: "/a/server.conf#[general]serverName"}, "serverName in server.conf [general]"},
		{Choice{Template: "config.value", Param: "/a/app.properties#port"}, "port in app.properties"},
	}
	for _, c := range cases {
		if got := c.c.ShortLabel(); got != c.want {
			t.Errorf("%+v: %q, want %q", c.c, got, c.want)
		}
	}
	if FixedTitles()["hw.memory.total_mb"] != "Memory total (MB)" {
		t.Error("fixed titles")
	}
}

// TestYesNoAnswersAreTrueFalse: an entry that answers a yes or no question
// records true or false on every operating system, and says it is boolean.
func TestYesNoAnswersAreTrueFalse(t *testing.T) {
	yesNo := regexp.MustCompile(`(?i)(echo|')\s*(yes|no|present|absent)\b`)
	for _, tpl := range Catalog {
		for _, v := range tpl.Variants {
			if yesNo.MatchString(v.Command) {
				t.Errorf("%s (%s) prints yes or no: %s", tpl.ID, v.Name, v.Command)
			}
		}
	}
	tpl, _ := Lookup("file.exists")
	if tpl.Type != model.TypeBool {
		t.Errorf("file.exists records a %q, want bool", tpl.Type)
	}
	for _, v := range tpl.Variants {
		if !strings.Contains(v.Command, "true") || !strings.Contains(v.Command, "false") {
			t.Errorf("file.exists (%s) does not print true and false: %s", v.Name, v.Command)
		}
	}
}

// TestNoSplunkGroups: the Splunk collectors are switched on one at a time.
func TestNoSplunkGroups(t *testing.T) {
	for _, g := range Groups {
		if strings.Contains(g.ID, "splunk") {
			t.Errorf("group %s is still offered", g.ID)
		}
	}
	for _, id := range []string{"splunk.uf.version", "splunk.enterprise.version", "splunk.home"} {
		if _, ok := Lookup(id); !ok {
			t.Errorf("catalog entry %s is gone", id)
		}
	}
}
