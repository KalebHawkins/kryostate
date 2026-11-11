package normalize

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestNormalizeGolden checks the grouped value produced for output that real
// machines print. The AIX cases are the reason this layer exists: 7200-05-04
// and 7.2.5.0 describe the same release and no amount of string comparison
// will say so.
func TestNormalizeGolden(t *testing.T) {
	n, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		attr, raw, want, family string
	}{
		{"os.version", "7200-05-04-2114", "AIX 7.2 TL05-SP04", "AIX"},
		{"os.version", "7200-05", "AIX 7.2 TL05", "AIX"},
		{"os.version", "AIX 7.2", "AIX 7.2", "AIX"},
		{"os.version", "Red Hat Enterprise Linux 8.6 (Ootpa)", "RHEL 8.6", "RHEL"},
		{"os.version", "Red Hat Enterprise Linux Server release 7.9 (Maipo)", "RHEL 7.9", "RHEL"},
		{"os.version", "SUSE Linux Enterprise Server 15 SP4", "SLES 15 SP4", "SLES"},
		{"os.version", "Ubuntu 22.04.3 LTS", "Ubuntu 22.04", "Ubuntu"},
		{"os.version", "Debian GNU/Linux 12 (bookworm)", "Debian 12", "Debian"},
		{"os.version", "Microsoft Windows Server 2019 Standard", "Windows Server 2019", "Windows Server"},
		{"os.version", "Microsoft Windows Server 2012 R2 Standard", "Windows Server 2012 R2", "Windows Server"},
		{"os.build", "10.0.17763", "build 17763", "Windows Server"},
		{"os.kernel", "4.18.0-372.26.1.el8_6.x86_64", "4.18", ""},
		{"app.version", "9.0.5.12", "9.0", ""},
		{"app.version", "v2.11", "2.11", ""},
	}
	for _, c := range cases {
		t.Run(c.raw, func(t *testing.T) {
			got, ok := n.Normalize(c.attr, c.raw)
			if !ok {
				t.Fatalf("no rule matched %q", c.raw)
			}
			if got.Value != c.want {
				t.Errorf("value: got %q, want %q (rule %s)", got.Value, c.want, got.RuleID)
			}
			if got.Family != c.family {
				t.Errorf("family: got %q, want %q", got.Family, c.family)
			}
		})
	}
}

// TestAIXReleasesGroupTogether is the check that matters for an estate where
// a single release family is most of the machines: two spellings of the same
// release must land in the same bucket, and two different releases must not.
func TestAIXReleasesGroupTogether(t *testing.T) {
	n, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := n.Normalize("os.version", "7200-05-04-2114")
	b, _ := n.Normalize("os.version", "7200-05-04-2301")
	c, _ := n.Normalize("os.version", "7200-04-02-2016")

	if a.Value != b.Value {
		t.Errorf("same release with different build dates split: %q and %q", a.Value, b.Value)
	}
	if a.Value == c.Value {
		t.Errorf("different technology levels merged into %q", a.Value)
	}
}

// TestNoRuleIsNotAnError distinguishes "nothing understood this" from "this is
// empty". A value nobody wrote a rule for must stay visible as unparsed rather
// than being quietly dropped or guessed at.
func TestNoRuleIsNotAnError(t *testing.T) {
	n, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	if res, ok := n.Normalize("os.version", "some appliance firmware 4"); ok {
		t.Errorf("unexpected match: %+v", res)
	}
}

// TestLoadRejectsBadRuleSets makes a broken rule file fail at startup rather
// than partway through a run against several hundred hosts.
func TestLoadRejectsBadRuleSets(t *testing.T) {
	cases := map[string]string{
		"no version":    `{"rules":[]}`,
		"bad regex":     `{"version":1,"rules":[{"id":"x","attr":"os.version","pattern":"([","value":"y"}]}`,
		"no id":         `{"version":1,"rules":[{"attr":"os.version","pattern":"a","value":"y"}]}`,
		"duplicate id":  `{"version":1,"rules":[{"id":"x","pattern":"a","value":"y"},{"id":"x","pattern":"b","value":"z"}]}`,
		"not json":      `nonsense`,
		"empty pattern": `{"version":1,"rules":[{"id":"x","attr":"a","value":"y"}]}`,
	}
	for name, body := range cases {
		if _, err := Load([]byte(body)); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// TestMergedRulesWinOverBuiltins lets a site correct a shipped rule without
// forking the project.
func TestMergedRulesWinOverBuiltins(t *testing.T) {
	n, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	extra := `{"version":2,"rules":[{"id":"local-aix","attr":"os.version",
		"pattern":"^(?P<v>[0-9])(?P<r>[0-9])[0-9]{2}-(?P<tl>[0-9]{2}).*$",
		"family":"AIX","value":"local ${v}.${r}"}]}`
	if err := n.Merge([]byte(extra)); err != nil {
		t.Fatal(err)
	}
	got, ok := n.Normalize("os.version", "7200-05-04-2114")
	if !ok || got.RuleID != "local-aix" {
		t.Fatalf("local rule did not take precedence: %+v", got)
	}
	if got.Value != "local 7.2" {
		t.Errorf("got %q", got.Value)
	}
}

// TestNormalizeFixtureValues runs the normaliser over values pulled from real
// command output rather than values typed into this file.
func TestNormalizeFixtureValues(t *testing.T) {
	n, err := Builtin()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct{ file, attr, want string }{
		{"aix/oslevel-s.txt", "os.version", "AIX 7.2 TL05-SP04"},
		{"aix/oslevel-r.txt", "os.version", "AIX 7.2 TL05"},
		{"linux/uname-r.txt", "os.kernel", "4.18"},
	} {
		b, err := os.ReadFile(filepath.Join("../../testdata/fixtures", filepath.FromSlash(c.file)))
		if err != nil {
			t.Fatal(err)
		}
		raw := strings.TrimSpace(string(b))
		got, ok := n.Normalize(c.attr, raw)
		if !ok {
			t.Errorf("%s: no rule matched %q", c.file, raw)
			continue
		}
		if got.Value != c.want {
			t.Errorf("%s: got %q, want %q", c.file, got.Value, c.want)
		}
	}
}

// FuzzNormalize checks no value from a host can panic the normaliser.
func FuzzNormalize(f *testing.F) {
	n, err := Builtin()
	if err != nil {
		f.Fatal(err)
	}
	for _, s := range []string{"7200-05-04-2114", "", "10.0.17763", "Ubuntu 22.04.3 LTS", "\x00"} {
		f.Add("os.version", s)
	}
	f.Fuzz(func(t *testing.T, attr, raw string) {
		_, _ = n.Normalize(attr, raw)
	})
}
