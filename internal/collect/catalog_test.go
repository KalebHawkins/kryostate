package collect

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

// TestCatalogEntriesAreWellFormed runs every entry through the same checks a
// hand-written spec file gets, so a typo in the catalog fails here rather
// than on someone's estate.
func TestCatalogEntriesAreWellFormed(t *testing.T) {
	seen := map[string]bool{}
	params := map[ParamKind]string{ParamPath: "/etc/resolv.conf", ParamEnvVar: "SPLUNK_HOME", ParamPackage: "openssl",
		ParamConfig: "/opt/splunk/etc/system/local/server.conf#[general]serverName"}
	for _, tpl := range Catalog {
		if seen[tpl.ID] {
			t.Errorf("duplicate catalog id %s", tpl.ID)
		}
		seen[tpl.ID] = true
		c := Choice{ID: 7, Template: tpl.ID}
		if !tpl.Fixed() {
			c.Param = params[tpl.Param.Kind]
		}
		specs, err := c.Specs()
		if err != nil {
			t.Errorf("%s: %v", tpl.ID, err)
			continue
		}
		file := specFile{Version: 1, Specs: specs}
		b, _ := json.Marshal(file)
		if _, err := LoadSpecs(b); err != nil {
			t.Errorf("%s does not pass the spec checks: %v", tpl.ID, err)
		}
		for _, s := range specs {
			if strings.Contains(s.Command, "{{") {
				t.Errorf("%s: placeholder left in %q", s.ID, s.Command)
			}
			if !s.Optional {
				t.Errorf("%s is not optional; a host without the program would count as failed", s.ID)
			}
		}
		b2, _ := BuiltinSpecs()
		if err := b2.Merge(b); err != nil {
			t.Errorf("%s clashes with a built-in collector: %v", tpl.ID, err)
		}
	}
}

// TestTemplateValuesCannotBecomeCommands is the reason the catalog may be
// used from the web interface at all.
func TestTemplateValuesCannotBecomeCommands(t *testing.T) {
	bad := map[ParamKind][]string{
		ParamPath: {
			"/etc/passwd'; rm -rf / #", "/tmp/$(id)", "/tmp/`id`", "/etc/x;id", "/etc/x|id",
			"/etc/x\nid", "relative/path", "/etc/../shadow", "/etc/x&id", "/etc/x>y", `/etc/"x"`, "/etc/$HOME",
		},
		ParamEnvVar:  {"HOME;id", "A B", "$(id)", "1ABC", "X}", ""},
		ParamPackage: {"openssl;id", "a'b", "-rf", "x$(id)", "*", "a/b"},
		ParamConfig: {"/etc/x.conf", "/etc/x.conf#", "#key", "/etc/x.conf#k'ey", "/etc/x.conf#[a'b]key", "/etc/x.conf#[sec]$(id)",
			"/etc/x.conf#[sec", "/etc/x;id#key", "/etc/x.conf#key;id", "/etc/x.conf#[a]b]c", "/etc/x.conf#k`id`", `/etc/x.conf#[\]k`},
	}
	for kind, vs := range bad {
		for _, v := range vs {
			if err := CheckParam(kind, v); err == nil {
				t.Errorf("%s %q was accepted", kind, v)
			}
		}
	}
	good := map[ParamKind][]string{
		ParamPath:    {"/etc/resolv.conf", `C:\Program Files (x86)\App\conf.xml`, "/opt/splunkforwarder/etc/system/local/outputs.conf"},
		ParamEnvVar:  {"SPLUNK_HOME", "JAVA_HOME", "_X1"},
		ParamPackage: {"openssl", "python3-libs", "Splunk Universal Forwarder", "bos.rte"},
		ParamConfig: {"/opt/splunk/etc/system/local/server.conf#[general]serverName", "/etc/app.properties#db.port",
			`C:\Program Files\SplunkUniversalForwarder\etc\system\local\inputs.conf#[default]host`, "/opt/splunk/etc/system/local/inputs.conf#[monitor:///var/log]index"},
	}
	for kind, vs := range good {
		for _, v := range vs {
			if err := CheckParam(kind, v); err != nil {
				t.Errorf("%s %q was refused: %v", kind, v, err)
			}
		}
	}
	// A rendered command carries the value inside single quotes.
	specs, err := Choice{ID: 1, Template: "file.exists", Param: "/etc/resolv.conf"}.Specs()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(specs[0].Command, "[ -e '/etc/resolv.conf' ]") {
		t.Errorf("unquoted: %s", specs[0].Command)
	}
	if _, err := (Choice{ID: 1, Template: "file.exists", Param: "/x'; id"}).Specs(); err == nil {
		t.Error("Specs rendered a value CheckParam refuses")
	}
}

func TestDefaultAttrIsUsable(t *testing.T) {
	for _, tc := range []struct{ tpl, param, want string }{
		{"file.exists", "/etc/resolv.conf", "file.resolv_conf.exists"},
		{"file.contents", `C:\App\Config.XML`, "file.config_xml.contents"},
		{"env.var", "SPLUNK_HOME", "env.splunk_home"},
		{"package.version", "Splunk Universal Forwarder", "pkg.splunk_universal_forwarder.version"},
		{"cpu.cores", "", "hw.cpu.cores"},
	} {
		tpl, _ := Lookup(tc.tpl)
		got := DefaultAttr(tpl, tc.param)
		if got != tc.want {
			t.Errorf("%s %q: got %s, want %s", tc.tpl, tc.param, got, tc.want)
		}
		if err := CheckAttr(got); err != nil {
			t.Error(err)
		}
	}
}

// TestCatalogParsesRealOutput checks the parse rules against what the
// commands really print.
func TestCatalogParsesRealOutput(t *testing.T) {
	for _, tc := range []struct{ id, variant, out, want string }{
		{"memory.total", "aix", "Memory Size: 16384 MB\n", "16384"},
		{"splunk.uf.version", "unix", "VERSION=9.1.2\nBUILD=b6b9c8185839\nPRODUCT=splunk\n", "9.1.2"},
		{"splunk.home", "unix", "splunkd -p 8089 start\n[splunkd pid=1234] /opt/splunkforwarder/bin/splunkd -p 8089 start [process-runner]\n", "/opt/splunkforwarder"},
		{"splunk.home", "windows", `"C:\Program Files\SplunkUniversalForwarder\bin\splunkd.exe" service`, `C:\Program Files\SplunkUniversalForwarder`},
		{"java.version", "unix", "openjdk version \"17.0.9\" 2023-10-17\nOpenJDK Runtime Environment\n", "17.0.9"},
		{"python.version", "unix", "Python 3.11.5\n", "3.11.5"},
		{"openssl.version", "unix", "OpenSSL 3.0.7 1 Nov 2022 (Library: OpenSSL 3.0.7 1 Nov 2022)\n", "3.0.7"},
		{"nginx.version", "linux", "nginx version: nginx/1.24.0\n", "1.24.0"},
		{"httpd.version", "linux", "Server version: Apache/2.4.57 (Red Hat Enterprise Linux)\n", "2.4.57"},
		{"docker.version", "linux", "Docker version 24.0.7, build afdd53b\n", "24.0.7"},
		{"package.version", "aix", "bos.rte:bos.rte:7.2.5.0: : :C:F:Base Operating System Runtime: : : : : : :0:0:/:\n", "7.2.5.0"},
		{"file.contents", "unix", "search example.com\nnameserver 10.0.0.2\n\n", "search example.com\nnameserver 10.0.0.2"},
	} {
		tpl, _ := Lookup(tc.id)
		var p *Parse
		for _, v := range tpl.Variants {
			if v.Name == tc.variant {
				p = &v.Parse
			}
		}
		if p == nil {
			t.Errorf("%s has no %s variant", tc.id, tc.variant)
			continue
		}
		got, err := Extract(*p, tc.out)
		if err != nil || got != tc.want {
			t.Errorf("%s/%s: got %q, %v; want %q", tc.id, tc.variant, got, err, tc.want)
		}
	}
	if _, err := Extract(Parse{Kind: ParseAll}, " \n "); !errors.Is(err, ErrNoMatch) {
		t.Error("empty output was read as a value")
	}
	long := strings.Repeat("x", MaxValue+100)
	if v, _ := Extract(Parse{Kind: ParseAll}, long); len(v) != MaxValue {
		t.Errorf("a large file was kept at %d bytes", len(v))
	}
}
