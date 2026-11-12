package web

import (
	"html/template"
	"regexp"
	"strconv"
	"strings"
)

// The user guide is Markdown so that it reads well on a code host and in a
// terminal. This renders the small subset the guide uses: headings, fenced
// code, tables, bullet and numbered lists, paragraphs, and inline code, bold
// and links. A full Markdown library would be the only dependency in the
// binary that exists to format one page.
//
// Every piece of text is HTML-escaped before any markup is added, so a
// careless edit to the guide produces odd formatting, never live HTML.

// tocEntry is one heading in the table of contents.
type tocEntry struct {
	Level int
	ID    string
	Text  string
}

var (
	reHeading  = regexp.MustCompile(`^(#{1,4})\s+(.*)$`)
	reBullet   = regexp.MustCompile(`^[-*]\s+(.*)$`)
	reOrdered  = regexp.MustCompile(`^\d+\.\s+(.*)$`)
	reTableSep = regexp.MustCompile(`^\|?\s*:?-{3,}:?\s*(\|\s*:?-{3,}:?\s*)*\|?$`)
	reBold     = regexp.MustCompile(`\*\*(.+?)\*\*`)
	reLink     = regexp.MustCompile(`\[([^\]]+)\]\(([^)\s]+)\)`)
	reSlugBad  = regexp.MustCompile(`[^a-z0-9]+`)
)

// renderMarkdown returns the HTML for a document and its h2/h3 headings.
func renderMarkdown(src string) (template.HTML, []tocEntry) {
	lines := strings.Split(strings.ReplaceAll(src, "\r\n", "\n"), "\n")
	var b strings.Builder
	var toc []tocEntry
	used := map[string]int{}

	for i := 0; i < len(lines); {
		line := lines[i]
		trimmed := strings.TrimSpace(line)

		switch {
		case trimmed == "":
			i++

		case strings.HasPrefix(trimmed, "```"):
			lang := strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
			i++
			var code []string
			for i < len(lines) && !strings.HasPrefix(strings.TrimSpace(lines[i]), "```") {
				code = append(code, lines[i])
				i++
			}
			i++ // closing fence
			b.WriteString(`<pre><code`)
			if lang != "" {
				b.WriteString(` data-lang="` + template.HTMLEscapeString(lang) + `"`)
			}
			b.WriteString(`>` + template.HTMLEscapeString(strings.Join(code, "\n")) + "</code></pre>\n")

		case reHeading.MatchString(trimmed):
			m := reHeading.FindStringSubmatch(trimmed)
			level := len(m[1])
			plain := strings.ReplaceAll(m[2], "`", "")
			id := slug(plain, used)
			if level == 2 || level == 3 {
				toc = append(toc, tocEntry{Level: level, ID: id, Text: plain})
			}
			tag := "h" + strconv.Itoa(level)
			b.WriteString("<" + tag + ` id="` + id + `">` + inline(m[2]) +
				` <a class="anchor" href="#` + id + `" aria-label="Link to this section">#</a></` + tag + ">\n")
			i++

		case strings.HasPrefix(trimmed, "|") && i+1 < len(lines) && reTableSep.MatchString(strings.TrimSpace(lines[i+1])):
			head := cells(trimmed)
			i += 2
			b.WriteString("<div class=\"table-wrap\"><table>\n")
			// A table whose header cells are all blank is a two-column list;
			// an empty header row would only draw an empty strip.
			if strings.Join(head, "") != "" {
				b.WriteString("<thead><tr>")
				for _, c := range head {
					b.WriteString(`<th scope="col">` + inline(c) + "</th>")
				}
				b.WriteString("</tr></thead>\n")
			}
			b.WriteString("<tbody>\n")
			for i < len(lines) && strings.HasPrefix(strings.TrimSpace(lines[i]), "|") {
				b.WriteString("<tr>")
				for _, c := range cells(strings.TrimSpace(lines[i])) {
					b.WriteString("<td>" + inline(c) + "</td>")
				}
				b.WriteString("</tr>\n")
				i++
			}
			b.WriteString("</tbody></table></div>\n")

		case reBullet.MatchString(trimmed) || reOrdered.MatchString(trimmed):
			re, tag := reBullet, "ul"
			if !reBullet.MatchString(trimmed) {
				re, tag = reOrdered, "ol"
			}
			b.WriteString("<" + tag + ">\n")
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				m := re.FindStringSubmatch(t)
				if m == nil {
					break
				}
				item := []string{m[1]}
				i++
				// Indented lines continue the item above them.
				for i < len(lines) && strings.HasPrefix(lines[i], "  ") && strings.TrimSpace(lines[i]) != "" &&
					!re.MatchString(strings.TrimSpace(lines[i])) {
					item = append(item, strings.TrimSpace(lines[i]))
					i++
				}
				b.WriteString("<li>" + inline(strings.Join(item, " ")) + "</li>\n")
			}
			b.WriteString("</" + tag + ">\n")

		default:
			para := []string{trimmed}
			i++
			for i < len(lines) {
				t := strings.TrimSpace(lines[i])
				if t == "" || strings.HasPrefix(t, "```") || strings.HasPrefix(t, "|") ||
					reHeading.MatchString(t) || reBullet.MatchString(t) || reOrdered.MatchString(t) {
					break
				}
				para = append(para, t)
				i++
			}
			b.WriteString("<p>" + inline(strings.Join(para, " ")) + "</p>\n")
		}
	}
	return template.HTML(b.String()), toc
}

// cells splits a table row, honouring "\|" as a literal pipe.
func cells(row string) []string {
	row = strings.TrimSpace(row)
	row = strings.TrimPrefix(row, "|")
	row = strings.TrimSuffix(row, "|")
	const pipe = "\x00"
	row = strings.ReplaceAll(row, `\|`, pipe)
	parts := strings.Split(row, "|")
	for i, p := range parts {
		parts[i] = strings.TrimSpace(strings.ReplaceAll(p, pipe, "|"))
	}
	return parts
}

// inline renders code spans, bold and links within one block of text. Code
// spans are cut out first so their contents are shown exactly.
func inline(s string) string {
	var b strings.Builder
	parts := strings.Split(s, "`")
	for i, p := range parts {
		if i%2 == 1 && i < len(parts)-1 {
			b.WriteString("<code>" + template.HTMLEscapeString(p) + "</code>")
			continue
		}
		if i%2 == 1 { // an unmatched backtick: show it as typed
			p = "`" + p
		}
		e := template.HTMLEscapeString(p)
		e = reBold.ReplaceAllString(e, "<strong>$1</strong>")
		e = reLink.ReplaceAllStringFunc(e, func(m string) string {
			sm := reLink.FindStringSubmatch(m)
			href := docRoute(sm[2])
			if !safeHref(href) {
				return sm[1]
			}
			return `<a href="` + href + `">` + sm[1] + "</a>"
		})
		b.WriteString(e)
	}
	return b.String()
}

// safeHref admits web links and links within the page. Relative file paths
// such as docs/adr/ mean nothing to a browser looking at /guide, so they are
// rendered as plain text.
func safeHref(h string) bool {
	return strings.HasPrefix(h, "/guide") || strings.HasPrefix(h, "https://") || strings.HasPrefix(h, "http://") || strings.HasPrefix(h, "#")
}

// docRoute turns a link between the served documents, written as a file name so
// that it also works when the Markdown is read on a code host, into the page
// that serves it here.
func docRoute(h string) string {
	file, frag, _ := strings.Cut(h, "#")
	routes := map[string]string{"user-guide.md": "/guide", "collectors.md": "/guide/collectors", "queries.md": "/guide/queries"}
	r, ok := routes[file]
	if !ok {
		return h
	}
	if frag != "" {
		r += "#" + frag
	}
	return r
}

func slug(text string, used map[string]int) string {
	s := strings.Trim(reSlugBad.ReplaceAllString(strings.ToLower(text), "-"), "-")
	if s == "" {
		s = "section"
	}
	used[s]++
	if n := used[s]; n > 1 {
		s += "-" + strconv.Itoa(n)
	}
	return s
}
