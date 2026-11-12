package web

import (
	"html"
	"net/url"
	"strings"
	"testing"
)

func TestShellQuote(t *testing.T) {
	for in, want := range map[string]string{
		"web01":                "web01",
		`os.family == "Linux"`: `'os.family == "Linux"'`,
		`name =~ 'x'`:          `'name =~ '\''x'\'''`,
		"":                     "''",
		"/etc/app/app.conf":    "/etc/app/app.conf",
		"a b":                  "'a b'",
	} {
		if got := shellQuote(in); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", in, got, want)
		}
	}
}

func TestKeyPagesShowTheCommandLine(t *testing.T) {
	_, h, st := testServer(t)
	seed(t, st)
	for _, path := range []string{"/", "/hosts", "/runs", "/collectors", "/schedules"} {
		if !strings.Contains(get(t, h, path, nil).Body.String(), `class="cli"`) {
			t.Errorf("%s has no command-line hint", path)
		}
	}
	q := `os.family == "Linux"`
	body := html.UnescapeString(get(t, h, "/hosts?q="+url.QueryEscape(q), nil).Body.String())
	if !strings.Contains(body, `kryostate hosts -q 'os.family == "Linux"' -cols`) {
		t.Error("the inventory hint does not carry the query")
	}
}
