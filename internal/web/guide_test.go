package web

import (
	"html"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/query"
)

// TestGuideSwitcherMarksTheCurrentGuide: the three guides are a switcher
// above the article, and exactly one of them is marked as the page shown.
func TestGuideSwitcherMarksTheCurrentGuide(t *testing.T) {
	_, h, _ := testServer(t)
	for _, g := range guides {
		// The fragment, so the nav bar's own aria-current does not count.
		body := get(t, h, g.Path, map[string]string{"HX-Request": "true"}).Body.String()
		if !strings.Contains(body, `<nav class="guide__switch" aria-label="Guides">`) {
			t.Fatalf("%s: no guide switcher", g.Path)
		}
		want := `href="` + g.Path + `" hx-get="` + g.Path + `" hx-target="#main" hx-push-url="true" aria-current="page">` + g.Title + `</a>`
		if !strings.Contains(body, want) {
			t.Errorf("%s: the switcher does not mark %q as current", g.Path, g.Title)
		}
		if n := strings.Count(body, `aria-current="page"`); n != 1 {
			t.Errorf("%s: %d guides marked current, want 1", g.Path, n)
		}
		for _, other := range guides {
			if !strings.Contains(body, `href="`+other.Path+`"`) {
				t.Errorf("%s: the switcher lacks %s", g.Path, other.Path)
			}
		}
	}
}

// TestGuideContentsAreTitledWithTheGuide: the contents say which guide they
// belong to, in the sidebar and in the disclosure used at phone width.
func TestGuideContentsAreTitledWithTheGuide(t *testing.T) {
	_, h, _ := testServer(t)
	for _, g := range guides {
		body := get(t, h, g.Path, nil).Body.String()
		for _, want := range []string{
			`<span class="guide__toc-kicker">On this page</span> <span class="guide__toc-name">` + g.Title + `</span>`,
			`<summary>On this page: ` + g.Title + `</summary>`,
			`<title>` + g.Title + ` — KryoState</title>`,
		} {
			if !strings.Contains(body, want) {
				t.Errorf("%s lacks %q", g.Path, want)
			}
		}
	}
}

// TestQueryHelpIsTables: the inventory's query help is an operator table and
// a table of examples, each a link that runs the query.
func TestQueryHelpIsTables(t *testing.T) {
	_, h, _ := testServer(t)
	body := get(t, h, "/hosts", nil).Body.String()
	for _, want := range []string{
		`<table class="qhelp__table qhelp__ops">`,
		`<table class="qhelp__table qhelp__ex">`,
		`<p class="qhelp__names">`,
		`<code>application</code>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the query help lacks %q", want)
		}
	}
	if n := strings.Count(body, `<tr><td><a class="mono" href="/hosts?q=`); n != len(examples) {
		t.Errorf("%d example links, want %d", n, len(examples))
	}
	for _, ex := range examples {
		if !strings.Contains(html.UnescapeString(body), ">"+ex.Q+"</a>") {
			t.Errorf("example %q is not shown as a link", ex.Q)
		}
	}
	if n := strings.Count(body, `<td class="qhelp__op">`); n != len(queryOperators) {
		t.Errorf("%d operator rows, want %d", n, len(queryOperators))
	}
}

// TestQueryHelpExamplesParse: every query the help shows is a valid query, so
// clicking one never lands on a parse error.
func TestQueryHelpExamplesParse(t *testing.T) {
	var qs []string
	for _, ex := range examples {
		qs = append(qs, ex.Q)
	}
	for _, op := range queryOperators {
		qs = append(qs, op.Example)
	}
	for _, q := range qs {
		if _, err := query.Parse(q); err != nil {
			t.Errorf("%q does not parse: %v", q, err)
		}
		if strings.Contains(strings.ToLower(q), "splunk") {
			t.Errorf("%q names a product; examples use generic data", q)
		}
	}
}

// TestExternalProgramsCardExplainsItself: the card says what an external
// program is, where it runs, how it gets the host and where it is listed, and
// links to the section of the collector guide that exists.
func TestExternalProgramsCardExplainsItself(t *testing.T) {
	s, h, _ := testServer(t)
	s.Config.Dir = t.TempDir()
	body := get(t, h, "/collectors", nil).Body.String()
	for _, want := range []string{
		"<h2>External programs</h2>",
		"on the KryoState server, not on the host",
		"KRYOSTATE_TARGET", "KRYOSTATE_ADDRESS",
		"JSON envelope of facts",
		"externals.json",
		"None are configured.",
		`href="/guide/collectors#external-programs"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the external programs card lacks %q", want)
		}
	}
	if !strings.Contains(get(t, h, "/guide/collectors", nil).Body.String(), `id="external-programs"`) {
		t.Error("the collector guide has no external-programs section to link to")
	}

	s.Engine.Externals = []collect.External{{Name: "vmware", Path: "/opt/vmware-facts"}}
	s.Config.Externals = "/data/externals.json"
	body = get(t, h, "/collectors", nil).Body.String()
	if !strings.Contains(body, `1 configured, read from <span class="mono">/data/externals.json</span>`) {
		t.Error("the card does not say how many external programs are configured")
	}
	if strings.Contains(body, "None are configured.") {
		t.Error("the card says none are configured when one is")
	}
}
