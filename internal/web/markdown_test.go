package web

import (
	"html/template"
	"net/http"
	"strings"
	"testing"

	"github.com/KalebHawkins/hearsay/docs"
)

func TestMarkdownRendersTheSubsetTheGuideUses(t *testing.T) {
	src := "# Title\n\nSome **bold** and `a <b>` code, and [a link](https://example.invalid).\n" +
		"continued line.\n\n" +
		"## Section `one`\n\n- first\n  continued\n- second\n\n1. one\n2. two\n\n" +
		"| A | B |\n|---|---|\n| `x\\|y` | 2 |\n\n" +
		"```sh\necho <hi> && true\n```\n\n## Section one\n"
	got, toc := renderMarkdown(src)
	html := string(got)

	for _, want := range []string{
		`<h1 id="title">Title`,
		`<strong>bold</strong>`,
		`<code>a &lt;b&gt;</code>`,
		`<a href="https://example.invalid">a link</a>`,
		`and <a href="https://example.invalid">a link</a>. continued line.</p>`,
		`<h2 id="section-one">Section <code>one</code>`,
		"<li>first continued</li>",
		"<ol>\n<li>one</li>",
		`<th scope="col">A</th>`,
		`<td><code>x|y</code></td>`,
		`<pre><code data-lang="sh">echo &lt;hi&gt; &amp;&amp; true</code></pre>`,
		`id="section-one-2"`,
	} {
		if !strings.Contains(html, want) {
			t.Errorf("missing %q in:\n%s", want, html)
		}
	}
	if len(toc) != 2 || toc[0].ID != "section-one" || toc[0].Text != "Section one" {
		t.Errorf("toc: %+v", toc)
	}
}

// TestMarkdownNeverEmitsLiveHTML: the guide is ours, but an edit to it must
// not be able to put script or a javascript: link on the page.
func TestMarkdownNeverEmitsLiveHTML(t *testing.T) {
	got, _ := renderMarkdown("<script>alert(1)</script>\n\n[x](javascript:alert(1))\n\n| <img src=x> |\n|---|\n| a |\n")
	html := string(got)
	for _, bad := range []string{"<script", "<img", "javascript:alert(1)\""} {
		if strings.Contains(html, bad) {
			t.Errorf("rendered %q:\n%s", bad, html)
		}
	}
	if strings.Contains(html, `href="javascript`) {
		t.Error("a javascript: link became a link")
	}
}

func TestGuideIsServed(t *testing.T) {
	_, h, _ := testServer(t)
	rec := get(t, h, "/guide", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /guide returned %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		"Tutorial: add your first host",
		"Tutorial: build a stack",
		"Tutorial: choose what to collect",
		`href="/guide"`, // the nav link
	} {
		if !strings.Contains(body, want) {
			t.Errorf("the guide page is missing %q", want)
		}
	}
}

// TestGuideHasNoUnrenderedMarkdown catches an edit to the guide that uses
// syntax the renderer does not know, which would show up as stray symbols.
func TestGuideHasNoUnrenderedMarkdown(t *testing.T) {
	html, _ := renderMarkdown(docs.UserGuide)
	// Code blocks may legitimately contain anything; look only outside them.
	outside := html
	for {
		start := strings.Index(string(outside), "<pre>")
		if start < 0 {
			break
		}
		end := strings.Index(string(outside[start:]), "</pre>")
		if end < 0 {
			t.Fatal("unclosed code block")
		}
		outside = outside[:start] + outside[start+end+len("</pre>"):]
	}
	for _, line := range strings.Split(string(outside), "\n") {
		for _, marker := range []string{"```", "**", "|---", "](", "## "} {
			if strings.Contains(line, marker) {
				t.Errorf("unrendered %q in: %s", marker, line)
			}
		}
	}
}

func TestCollectorGuideHasNoUnrenderedMarkdown(t *testing.T) {
	html, toc := renderMarkdown(docs.Collectors)
	if len(toc) < 8 {
		t.Errorf("the collector guide has only %d sections", len(toc))
	}
	for _, line := range strings.Split(string(stripPre(html)), "\n") {
		for _, marker := range []string{"```", "**", "|---", "](", "## "} {
			if strings.Contains(line, marker) {
				t.Errorf("unrendered %q in: %s", marker, line)
			}
		}
	}
}

func stripPre(html template.HTML) string {
	s := string(html)
	for {
		start := strings.Index(s, "<pre>")
		if start < 0 {
			return s
		}
		end := strings.Index(s[start:], "</pre>")
		if end < 0 {
			return s
		}
		s = s[:start] + s[start+end+len("</pre>"):]
	}
}

func TestDocLinksBecomeRoutes(t *testing.T) {
	for in, want := range map[string]string{
		"collectors.md":           "/guide/collectors",
		"user-guide.md#4-x":       "/guide#4-x",
		"docs/adr/0001.md":        "docs/adr/0001.md",
		"https://example.invalid": "https://example.invalid",
	} {
		if got := docRoute(in); got != want {
			t.Errorf("%s: got %s, want %s", in, got, want)
		}
	}
}
