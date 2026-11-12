package query

import (
	"strings"
	"testing"
)

var hosts = map[string]Host{
	"rhel7": {ID: "c1", Name: "web01", Stacks: []string{"splunk-prod"}, Roles: []string{"indexer"},
		Values: map[string]string{
			"os.family": "Linux", "os.version": "Red Hat 7.9 (Ootpa)", "kernel.version": "3.10.0-1160.el7.x86_64",
			"hw.cpu.cores": "8", "hw.memory.total_mb": "15884", "app.name": "splunk",
			"file.splunk.exists": "true", "pkg.openssl.version": "1.0.2k",
		}},
	"alma9": {ID: "c2", Name: "web02", Stacks: []string{"splunk-prod", "web"}, Roles: []string{"search-head", "frontend"},
		Values: map[string]string{
			"os.family": "Linux", "os.version": "AlmaLinux 9.4", "kernel.version": "5.14.0-427.el9",
			"hw.cpu.cores": "16", "file.splunk.exists": "no", "env": "prod",
		}},
	"win": {ID: "c3", Name: "db01", Values: map[string]string{
		"os.family": "Windows", "hw.cpu.cores": "4", "role": "database", "file.splunk.exists": "yes",
	}},
	"bare": {ID: "c4", Name: "new-host"},
}

func TestMatch(t *testing.T) {
	cases := []struct {
		q    string
		want string // space-separated host keys, sorted as in order below
	}{
		{`os.version == "Red Hat 7.9 (Ootpa)"`, "rhel7"},
		// The owner's second example, as written: the template shorthand.
		{`os.family == "Linux" && !file.exists == "/opt/splunkforwarder/bin/splunk"`, "alma9"},
		{`os.family == "Linux" && !file.exists["/opt/splunkforwarder/bin/splunk"]`, "alma9"},
		{`file.exists["/opt/splunkforwarder/bin/splunk"] == true`, "rhel7 win"},
		{`file.exists["/opt/splunkforwarder/bin/splunk"] == false`, "alma9"},
		{`file.exists["/opt/splunkforwarder/bin/splunk"] != true`, "alma9 bare"},
		{`file.exists != "/opt/splunkforwarder/bin/splunk"`, "alma9 bare"},
		{`file.splunk.exists`, "rhel7 win"},
		{`file.splunk.exists == "yes"`, "rhel7 win"},
		{`!file.splunk.exists`, "alma9 bare"},
		{`package.version == "openssl"`, "rhel7"},
		{`package.version["openssl"] >= "1.0.2"`, "rhel7"},
		{`package.version["openssl"] > "1.0.2k"`, ""},
		{`os.version != "Red Hat 7.9 (Ootpa)"`, "alma9 win bare"},
		{`os.version =~ "^Red Hat 7"`, "rhel7"},
		{`os.version =~ "Linux"`, "alma9"},
		{`os.version !~ "Linux"`, "rhel7 win bare"},
		{`os.version =~ "(?i)almalinux"`, "alma9"},
		{"name =~ `^web\\d+$`", "rhel7 alma9"},
		{`name =~ "^web"`, "rhel7 alma9"},
		{`name == "db01"`, "win"},
		{`id == "c4"`, "bare"},
		{`stack == "splunk-prod"`, "rhel7 alma9"},
		{`stack == "web"`, "alma9"},
		{`stack != "splunk-prod"`, "win bare"},
		{`role == "indexer"`, "rhel7"},
		{`role == "frontend"`, "alma9"},
		{`role == "database"`, "win"},
		{`role =~ "^search"`, "alma9"},
		{`application == "splunk"`, "rhel7"},
		{`app == "splunk"`, "rhel7"},
		{`has(stack)`, "rhel7 alma9"},
		{`!has(stack)`, "win bare"},
		{`has(os.version)`, "rhel7 alma9"},
		{`os.version != ""`, "rhel7 alma9"},
		{`os.version == ""`, "win bare"},
		{`hw.cpu.cores >= 8`, "rhel7 alma9"},
		{`hw.cpu.cores > 8`, "alma9"},
		{`hw.cpu.cores < 8`, "win"},
		{`hw.cpu.cores <= 8`, "rhel7 win"},
		{`hw.cpu.cores == 16`, "alma9"},
		{`hw.cpu.cores == "16"`, "alma9"},
		{`hw.cpu.cores > "10"`, "alma9"}, // numeric, not lexical: "8" > "10" as text
		{`hw.memory.total_mb > 8000.5`, "rhel7"},
		{`kernel.version >= "4"`, "alma9"},
		{`kernel.version < "3.10.1"`, "rhel7"},
		{`os.version > "7"`, ""}, // not a version on the host side
		{`hw.cpu.cores > -1`, "rhel7 alma9 win"},
		{`env == "prod" || os.family == "Windows"`, "alma9 win"},
		{`os.family == "Linux" && stack == "web" || name == "db01"`, "alma9 win"},
		{`os.family == "Linux" && (stack == "web" || name == "db01")`, "alma9"},
		{`!(os.family == "Linux")`, "win bare"},
		{`!!(os.family == "Linux")`, "rhel7 alma9"},
		{`name == 'nobody' || name == 'web01'`, "rhel7"},
	}
	order := []string{"rhel7", "alma9", "win", "bare"}
	for _, c := range cases {
		e, err := Parse(c.q)
		if err != nil {
			t.Errorf("%s: %v", c.q, err)
			continue
		}
		var got []string
		for _, k := range order {
			if e.Match(hosts[k]) {
				got = append(got, k)
			}
		}
		if g := strings.Join(got, " "); g != c.want {
			t.Errorf("%s\n got: %q\nwant: %q", c.q, g, c.want)
		}
	}
}

func TestEscapes(t *testing.T) {
	e, err := Parse(`name == "a \"quoted\" \\ name\t"`)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Match(Host{Name: "a \"quoted\" \\ name\t"}) {
		t.Error("escaped string did not match")
	}
	q := Quote("Red Hat \"7\" \\ x\n")
	e, err = Parse("name == " + q)
	if err != nil {
		t.Fatal(err)
	}
	if !e.Match(Host{Name: "Red Hat \"7\" \\ x\n"}) {
		t.Errorf("Quote did not round-trip: %s", q)
	}
}

func TestResolver(t *testing.T) {
	e, err := Compile(`file.exists["/opt/x"]`, func(tmpl, param string) string {
		if tmpl == "file.exists" && param == "/opt/x" {
			return "custom.x.present"
		}
		return "nope"
	})
	if err != nil {
		t.Fatal(err)
	}
	if !e.Match(Host{Values: map[string]string{"custom.x.present": "true"}}) {
		t.Error("resolver's attribute was not used")
	}
}

func TestErrors(t *testing.T) {
	cases := []struct {
		q    string
		col  int
		frag string
	}{
		{``, 1, "empty"},
		{`   `, 1, "empty"},
		{`os.version`, 0, ""}, // valid: a bare yes/no test
		{`os.version =`, 12, "use =="},
		{`os.version = "x"`, 12, "use =="},
		{`os.version ==`, 14, "ends here"},
		{`os.version == linux`, 15, "double quotes"},
		{`os.version == "x`, 15, "not closed"},
		{`os.version == 'x`, 15, "not closed"},
		{"os.version == `x", 15, "not closed"},
		{`os.version == "x\q"`, 17, "unknown escape"},
		{`"x" == os.version`, 1, "starts with a name"},
		{`8 == hw.cpu.cores`, 1, "starts with a name"},
		{`(os.family == "Linux"`, 22, "expected )"},
		{`os.family == "Linux")`, 21, "no matching ("},
		{`os.family == "Linux" os.version == "x"`, 22, "join conditions"},
		{`os.family == "Linux" &&`, 24, "ends too soon"},
		{`&& os.family == "Linux"`, 1, "expected a condition"},
		{`os.family == "Linux" & x`, 22, "&&"},
		{`os.family == "Linux" | x`, 22, "||"},
		{`name =~ "["`, 9, "not a valid pattern"},
		{`name =~ 5`, 9, "pattern in quotes"},
		{`hw.cpu.cores > "many"`, 16, "neither"},
		{`hw.cpu.cores > true`, 16, "not true or false"},
		{`os.version["x"] == "y"`, 11, "only a collector"},
		{`file.exists`, 1, "takes a path"},
		{`file.exists == true`, 13, "takes a path"},
		{`file.exists =~ "/opt"`, 13, "takes a path"},
		{`file.exists[/opt/x] == true`, 13, "double quotes"},
		{`file.exists[5] == true`, 13, "in quotes"},
		{`file.exists["/opt/x" == true`, 22, "expected ]"},
		{`has(file.exists)`, 5, "needs its parameter"},
		{`has(os.version`, 15, "expected )"},
		{`has()`, 5, "expected a name"},
		{`os.version == 1.2.3`, 15, "not a number"},
		{`os.version == 7x`, 15, "not a number"},
		{`name == "é" && $`, 16, "unexpected character"},
		{`os.version == "x" == "y"`, 19, "join conditions"},
		{`!`, 2, "ends too soon"},
		{`()`, 2, "expected a condition"},
	}
	for _, c := range cases {
		_, err := Parse(c.q)
		if c.frag == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", c.q, err)
			}
			continue
		}
		if err == nil {
			t.Errorf("%q: expected an error containing %q", c.q, c.frag)
			continue
		}
		qe, ok := err.(*Error)
		if !ok {
			t.Errorf("%q: error is %T, want *Error", c.q, err)
			continue
		}
		if qe.Col != c.col || !strings.Contains(qe.Msg, c.frag) {
			t.Errorf("%q: got col %d %q; want col %d containing %q", c.q, qe.Col, qe.Msg, c.col, c.frag)
		}
		if !strings.HasSuffix(qe.Caret(), "^") || len(qe.Caret()) != qe.Col {
			t.Errorf("%q: caret %q does not point at column %d", c.q, qe.Caret(), qe.Col)
		}
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		{"7.9", "7.10", -1},
		{"7.10", "7.9", 1},
		{"1.0", "1.0", 0},
		{"v1.2", "1.2", 0},
		{"7.9", "7.9.1", -1},
		{"1.0.2k", "1.0.2", 1},
		{"1.0rc1", "1.0rc2", -1},
		{"1.0", "1.0a", -1},
		{"3.10.0-1160.el7", "3.10.0-957.el7", 1},
		{"010", "9", 1},
		{"99999999999999999999999", "1", 1},
	}
	for _, c := range cases {
		if got := CompareVersions(c.a, c.b); got != c.want {
			t.Errorf("CompareVersions(%q, %q) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestLooksLikeQuery(t *testing.T) {
	for s, want := range map[string]bool{
		"web01": false, "web 01": false, "os.version": false,
		`name == "x"`: true, "has(x)": true, "!x": true, "a && b": true,
	} {
		if got := LooksLikeQuery(s); got != want {
			t.Errorf("LooksLikeQuery(%q) = %v", s, got)
		}
	}
}
