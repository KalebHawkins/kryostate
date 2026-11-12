package web

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// colourLiteral matches a colour written out: hex, or one of the CSS colour
// functions. Named colours are not caught; none are used.
var colourLiteral = regexp.MustCompile(`#[0-9a-fA-F]{3,8}\b|\b(?:rgba?|hsla?|hwb|lab|lch|oklab|oklch)\(`)

// literalAllowed lists declarations that may keep a colour literal outside a
// custom property, as "selector-free property: value" text. Keep it empty
// unless a rule truly cannot take a token.
var literalAllowed = map[string]bool{}

func appCSS(t *testing.T) string {
	t.Helper()
	b, err := staticFS.ReadFile("static/app.css")
	if err != nil {
		t.Fatal(err)
	}
	return regexp.MustCompile(`(?s)/\*.*?\*/`).ReplaceAllString(string(b), "")
}

// declarations walks a stylesheet and returns every declaration with the
// selector (or at-rule) it sits under. A segment ended by "{" is a selector;
// one ended by ";" or "}" is a declaration.
type decl struct{ selector, prop, value string }

func declarations(css string) []decl {
	var out []decl
	var stack []string
	var buf strings.Builder
	for _, r := range css {
		switch r {
		case '{':
			stack = append(stack, strings.Join(strings.Fields(buf.String()), " "))
			buf.Reset()
		case ';', '}':
			if d := strings.TrimSpace(buf.String()); d != "" {
				if i := strings.Index(d, ":"); i > 0 {
					sel := ""
					if len(stack) > 0 {
						sel = stack[len(stack)-1]
					}
					out = append(out, decl{sel, strings.TrimSpace(d[:i]), strings.TrimSpace(d[i+1:])})
				}
			}
			buf.Reset()
			if r == '}' && len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
		default:
			buf.WriteRune(r)
		}
	}
	return out
}

// TestStylesheetUsesTokensForColour keeps colours in the theme blocks. A rule
// with a literal colour looks right in one theme and wrong in the other, so
// rules use tokens and only custom properties hold literals.
func TestStylesheetUsesTokensForColour(t *testing.T) {
	for _, d := range declarations(appCSS(t)) {
		if strings.HasPrefix(d.prop, "--") {
			continue
		}
		if colourLiteral.MatchString(d.value) && !literalAllowed[d.prop+": "+d.value] {
			t.Errorf("%s { %s: %s } uses a colour literal; add a token to the theme blocks at the top of app.css", d.selector, d.prop, d.value)
		}
	}
}

// TestColourScanFindsLiterals makes sure the scan above is not vacuous.
func TestColourScanFindsLiterals(t *testing.T) {
	sample := ":root { --a: #fff; }\n#main a:hover { color: #1a1e24; background: rgb(0 0 0 / 0.5); fill: url(#hatch); }"
	var found []string
	for _, d := range declarations(sample) {
		if !strings.HasPrefix(d.prop, "--") && colourLiteral.MatchString(d.value) {
			found = append(found, d.prop)
		}
	}
	if strings.Join(found, ",") != "color,background" {
		t.Errorf("scan found %v, want color and background", found)
	}
}

// tokensIn returns the custom properties set directly under the first rule
// whose selector is exactly sel.
func tokensIn(t *testing.T, css, sel string) map[string]string {
	t.Helper()
	got := map[string]string{}
	for _, d := range declarations(css) {
		if d.selector == sel && strings.HasPrefix(d.prop, "--") {
			got[d.prop] = d.value
		}
	}
	if len(got) == 0 {
		t.Fatalf("app.css has no tokens under %q", sel)
	}
	return got
}

// darkRoot is the selector of the default (dark) token block.
const darkRoot = `:root, :root[data-theme="dark"]`

// TestStylesheetDefinesBothThemes checks the theme blocks: dark by default and
// on request, light by system preference or on request, the two light blocks
// identical, and every colour the dark theme sets also set by the light one.
func TestStylesheetDefinesBothThemes(t *testing.T) {
	css := appCSS(t)
	for _, want := range []string{
		`@media (prefers-color-scheme: light)`,
		`:root:not([data-theme="dark"])`,
		`:root[data-theme="light"]`,
		`:root[data-theme="dark"]`,
	} {
		if !strings.Contains(css, want) {
			t.Errorf("app.css lacks %s", want)
		}
	}

	dark := tokensIn(t, css, darkRoot)
	system := tokensIn(t, css, `:root:not([data-theme="dark"])`)
	light := tokensIn(t, css, `:root[data-theme="light"]`)

	for k, v := range system {
		if light[k] != v {
			t.Errorf("light tokens differ: %s is %q under the system preference and %q when chosen", k, v, light[k])
		}
	}
	for k := range light {
		if _, ok := system[k]; !ok {
			t.Errorf("light token %s is set when chosen but not under the system preference", k)
		}
	}
	var missing []string
	for k, v := range dark {
		if colourLiteral.MatchString(v) {
			if _, ok := light[k]; !ok {
				missing = append(missing, k)
			}
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("the light theme does not set these dark colours: %s", strings.Join(missing, ", "))
	}

	schemes := map[string]string{}
	for _, d := range declarations(css) {
		if d.prop == "color-scheme" {
			schemes[d.selector] = d.value
		}
	}
	if schemes[darkRoot] != "dark" || schemes[`:root:not([data-theme="dark"])`] != "light" || schemes[`:root[data-theme="light"]`] != "light" {
		t.Errorf("color-scheme does not follow the theme: %v", schemes)
	}
}

// TestStylesheetTokensAreDefined catches a var(--name) that nothing sets,
// which would silently fall back to the property's initial value.
func TestStylesheetTokensAreDefined(t *testing.T) {
	css := appCSS(t)
	defined := map[string]bool{}
	for _, d := range declarations(css) {
		if strings.HasPrefix(d.prop, "--") {
			defined[d.prop] = true
		}
	}
	for _, m := range regexp.MustCompile(`var\(\s*(--[\w-]+)`).FindAllStringSubmatch(css, -1) {
		if !defined[m[1]] {
			t.Errorf("app.css uses %s but never sets it", m[1])
		}
	}
}

// TestNoColourLiteralsInMarkup keeps colours out of templates and the SVG the
// server draws, so both recolour with the theme.
func TestNoColourLiteralsInMarkup(t *testing.T) {
	attr := regexp.MustCompile(`(?:style|fill|stroke|color)\s*=\s*"([^"]*)"`)
	inline := regexp.MustCompile(`(?:fill|stroke|color|background)\s*:\s*(#[0-9a-fA-F]{3,8}\b|(?:rgba?|hsla?)\()`)
	files, _ := filepath.Glob("templates/*.html")
	gofiles, _ := filepath.Glob("*.go")
	for _, f := range append(files, gofiles...) {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		for _, m := range attr.FindAllStringSubmatch(src, -1) {
			if colourLiteral.MatchString(m[1]) {
				t.Errorf("%s: %q sets a colour; use a class and a token", f, m[0])
			}
		}
		for _, m := range inline.FindAllString(src, -1) {
			t.Errorf("%s: %q sets a colour; use a class and a token", f, m)
		}
	}
}

// TestLayoutAppliesThemeBeforePaint: the stored choice is applied by a script
// in <head> ahead of the stylesheet, and the nav carries the toggle.
func TestLayoutAppliesThemeBeforePaint(t *testing.T) {
	_, h, _ := testServer(t)
	body := get(t, h, "/", nil).Body.String()

	head, _, ok := strings.Cut(body, "</head>")
	if !ok {
		t.Fatal("no </head> in the page")
	}
	script := strings.Index(head, `localStorage.getItem("kryostate-theme")`)
	sheet := strings.Index(head, `/static/app.css`)
	if script < 0 {
		t.Fatal("the head has no script applying the stored theme")
	}
	if sheet < 0 || script > sheet {
		t.Error("the theme script must run before the stylesheet loads")
	}
	if !strings.Contains(head, `try{`) {
		t.Error("the theme script must survive blocked storage (try/catch)")
	}
	if !strings.Contains(head, `<meta name="color-scheme" content="dark light">`) {
		t.Error("the head does not declare both colour schemes")
	}

	nav, _, _ := strings.Cut(body, "</nav>")
	if !strings.Contains(nav, "data-theme-toggle") {
		t.Error("the nav has no theme toggle")
	}
	// Without script the toggle could do nothing, so it starts hidden.
	if !regexp.MustCompile(`<button[^>]*data-theme-toggle[^>]*\bhidden\b`).MatchString(nav) {
		t.Error("the theme toggle should start hidden until script shows it")
	}

	// A fragment swapped into #main by HTMX leaves the nav, and the toggle, alone.
	frag := get(t, h, "/", map[string]string{"HX-Request": "true"}).Body.String()
	if strings.Contains(frag, "data-theme-toggle") {
		t.Error("an HTMX fragment should not carry the nav's theme toggle")
	}

	js, err := staticFS.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"kryostate-theme"`, "data-theme-toggle", "localStorage.setItem", "htmx:historyRestore"} {
		if !strings.Contains(string(js), want) {
			t.Errorf("app.js lacks %s", want)
		}
	}
}
