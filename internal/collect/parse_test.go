package collect

import (
	"os"
	"path/filepath"
	"testing"
)

// fixturesDir holds the output of each supplied command, one file per command
// per platform. The hostnames and serial numbers in them are not real. Keep
// the spacing, the field order and the line endings exactly as a real command
// prints them: those are what the parsers depend on, and test input written
// from memory only proves the parser agrees with the author's recollection.
const fixturesDir = "../../testdata/fixtures"

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(fixturesDir, filepath.FromSlash(name)))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	return string(b)
}

// TestSpecsAgainstFixtures runs the shipped collector specs over real output
// and checks the value each one pulls out.
func TestSpecsAgainstFixtures(t *testing.T) {
	set, err := BuiltinSpecs()
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]Spec{}
	for _, s := range set.Specs {
		byID[s.ID] = s
	}

	cases := []struct {
		spec    string
		fixture string
		want    string
	}{
		{"aix.os.version", "aix/oslevel-s.txt", "7200-05-04-2114"},
		{"aix.os.bos", "aix/lslpp-bos-rte.txt", "7.2.5.0"},
		{"aix.host.wpar", "aix/uname-W.txt", "0"},
		{"aix.hw.model", "aix/uname-M.txt", "IBM,9009-22A"},
		{"aix.hw.serial", "aix/prtconf.txt", "21A1B2C"},
		{"linux.os.version", "linux/os-release.txt", "Red Hat Enterprise Linux 8.6 (Ootpa)"},
		{"linux.os.kernel", "linux/uname-r.txt", "4.18.0-372.26.1.el8_6.x86_64"},
		{"windows.os.version", "windows/win32-operatingsystem.txt", "Microsoft Windows Server 2019 Standard"},
		{"windows.os.build", "windows/win32-operatingsystem.txt", "10.0.17763"},
	}

	for _, c := range cases {
		t.Run(c.spec, func(t *testing.T) {
			sp, ok := byID[c.spec]
			if !ok {
				t.Fatalf("no collector named %q ships with this build", c.spec)
			}
			got, err := Extract(sp.Parse, readFixture(t, c.fixture))
			if err != nil {
				t.Fatalf("extract: %v", err)
			}
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestExtractDistinguishesNoMatch checks the parser reports "the command
// answered and the value was not in it" rather than returning an empty string
// that would be stored as a fact.
func TestExtractDistinguishesNoMatch(t *testing.T) {
	cases := []Parse{
		{Kind: ParseKV, Key: "NOT_PRESENT"},
		{Kind: ParseColonKV, Key: "Not Present"},
		{Kind: ParseField, Marker: "nothing", Field: 2},
		{Kind: ParseRegex, Pattern: `\bnothing-(\d+)\b`},
	}
	out := readFixture(t, "linux/os-release.txt")
	for _, p := range cases {
		if v, err := Extract(p, out); err == nil {
			t.Errorf("%s returned %q where it should have reported no match", p.Kind, v)
		}
	}
}

// TestExtractRejectsUnusableRules makes a misconfigured collector fail loudly
// instead of silently recording nothing for every host in the estate.
func TestExtractRejectsUnusableRules(t *testing.T) {
	cases := []Parse{
		{Kind: ParseKV},
		{Kind: ParseColonKV},
		{Kind: ParseField, Marker: "x", Field: 0},
		{Kind: ParseRegex},
		{Kind: ParseJSONPath},
		{Kind: "invented"},
	}
	for _, p := range cases {
		if _, err := Extract(p, "anything"); err == nil {
			t.Errorf("%+v was accepted", p)
		}
	}
}

// FuzzExtract checks that no input makes a parser panic. Collector output
// arrives from hosts nobody in this project controls; a panic there takes down
// a run partway through and leaves the estate half-collected.
func FuzzExtract(f *testing.F) {
	for _, name := range []string{
		"aix/oslevel-s.txt", "aix/prtconf.txt", "aix/lslpp-bos-rte.txt",
		"linux/os-release.txt", "windows/win32-operatingsystem.txt",
	} {
		b, err := os.ReadFile(filepath.Join(fixturesDir, filepath.FromSlash(name)))
		if err == nil {
			f.Add(string(b))
		}
	}
	f.Add("")
	f.Add("\x00\x00\x00")
	f.Add("a:b\nc=d\n")

	parses := []Parse{
		{Kind: ParseFirstLine},
		{Kind: ParseKV, Key: "PRETTY_NAME"},
		{Kind: ParseColonKV, Key: "Version"},
		{Kind: ParseField, Marker: "bos.rte", Field: 2},
		{Kind: ParseRegex, Pattern: `(?m)^(\d{4}-\d{2})`},
		{Kind: ParseJSONPath, Path: "a.b"},
	}
	f.Fuzz(func(t *testing.T, out string) {
		for _, p := range parses {
			_, _ = Extract(p, out)
		}
	})
}
