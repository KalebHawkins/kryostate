package web

import (
	"fmt"
	"html/template"
	"sort"
	"strings"

	"github.com/KalebHawkins/hearsay/internal/model"
	"github.com/KalebHawkins/hearsay/internal/store"
)

// Chart data is rendered as inline SVG on the server.
//
// There is no charting library here. The charts are counts over a few dozen
// buckets, the bars have to be clickable links into the filtered table, and
// every fragment the server sends is already HTML. A JavaScript chart library
// would add a build step, a supply chain and a re-initialisation bug on every
// swap, in exchange for animation nobody asked for.

const (
	chartWidth  = 560
	barHeight   = 22
	barGap      = 6
	labelWidth  = 190
	countWidth  = 56
	maxBarCount = 14
)

// Chart is a horizontal bar chart ready to render.
type Chart struct {
	Attr    string
	Title   string
	Buckets []store.Bucket
	Total   int
	Height  int
}

// NewChart prepares a distribution for display, folding the long tail into a
// single bucket so that one chart does not become four hundred bars of height
// one. Unknown buckets are never folded away: they are the hosts worth
// chasing.
func NewChart(attr, title string, buckets []store.Bucket) Chart {
	known := make([]store.Bucket, 0, len(buckets))
	unknown := make([]store.Bucket, 0, 3)
	total := 0
	for _, b := range buckets {
		total += b.Count
		if b.Unknown {
			unknown = append(unknown, b)
			continue
		}
		known = append(known, b)
	}
	sort.SliceStable(known, func(i, j int) bool { return known[i].Count > known[j].Count })

	room := maxBarCount - len(unknown)
	if room < 1 {
		room = 1
	}
	if len(known) > room {
		rest := 0
		for _, b := range known[room-1:] {
			rest += b.Count
		}
		folded := len(known) - room + 1
		known = append(known[:room-1], store.Bucket{
			Key:   "",
			Label: fmt.Sprintf("%d more values", folded),
			Count: rest,
		})
	}

	c := Chart{Attr: attr, Title: title, Buckets: append(known, unknown...), Total: total}
	c.Height = len(c.Buckets)*(barHeight+barGap) + barGap
	return c
}

// SVG renders the chart. Each bar links to the inventory filtered to that
// value, which is the interaction that makes a chart useful: seeing that
// eleven hosts run an old release is only half of the question.
func (c Chart) SVG() template.HTML {
	if len(c.Buckets) == 0 {
		return template.HTML(`<p class="muted">Nothing recorded for this attribute yet.</p>`)
	}
	max := 0
	for _, b := range c.Buckets {
		if b.Count > max {
			max = b.Count
		}
	}
	if max == 0 {
		max = 1
	}
	barArea := chartWidth - labelWidth - countWidth

	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="chart" viewBox="0 0 %d %d" role="img" aria-label="%s by count" preserveAspectRatio="xMinYMin meet">`,
		chartWidth, c.Height, template.HTMLEscapeString(c.Title))
	// The hatch is how an unknown bucket is told apart from a measured one
	// without relying on colour.
	sb.WriteString(`<defs><pattern id="hatch" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">` +
		`<rect width="6" height="6" fill="#20252b"></rect>` +
		`<line x1="0" y1="0" x2="0" y2="6" stroke="#5d6670" stroke-width="3"></line>` +
		`</pattern></defs>`)

	y := barGap
	for _, b := range c.Buckets {
		w := b.Count * barArea / max
		if w < 2 && b.Count > 0 {
			w = 2
		}
		class := "chart__bar"
		if b.Unknown {
			class += " chart__bar--unknown"
		}
		label := b.Label
		if label == "" {
			label = b.Key
		}

		href := c.filterURL(b)
		if href != "" {
			esc := template.HTMLEscapeString(href)
			fmt.Fprintf(&sb, `<a href="%s" hx-get="%s" hx-target="#main" hx-push-url="true">`, esc, esc)
		} else {
			sb.WriteString(`<g>`)
		}

		fmt.Fprintf(&sb, `<title>%s: %d</title>`, template.HTMLEscapeString(label), b.Count)
		fmt.Fprintf(&sb, `<text class="chart__label" x="%d" y="%d" text-anchor="end">%s</text>`,
			labelWidth-10, y+barHeight-6, template.HTMLEscapeString(trimLabel(label)))
		fmt.Fprintf(&sb, `<rect class="%s" x="%d" y="%d" width="%d" height="%d" rx="3"></rect>`,
			class, labelWidth, y, w, barHeight)
		fmt.Fprintf(&sb, `<text class="chart__count" x="%d" y="%d">%d</text>`,
			labelWidth+w+8, y+barHeight-6, b.Count)

		if href != "" {
			sb.WriteString(`</a>`)
		} else {
			sb.WriteString(`</g>`)
		}
		y += barHeight + barGap
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// filterURL builds the link a bar points at. The folded tail links nowhere,
// because "nine other values" is not a filter anyone can act on.
//
// The three causes of unknown get three different links. Sending "never
// collected" and "collection failed" to the same filtered list would undo the
// reason they are counted separately: one needs a connection profile and the
// other needs somebody to find out why the connection is failing.
func (c Chart) filterURL(b store.Bucket) string {
	switch {
	case b.Key == string(model.UnknownUnparsed):
		return "/hosts?unparsed=" + template.URLQueryEscaper(c.Attr)
	case b.Unknown:
		return "/hosts?unknown=" + template.URLQueryEscaper(c.Attr) +
			"&because=" + template.URLQueryEscaper(b.Key)
	case b.Key == "":
		return ""
	default:
		return "/hosts?attr=" + template.URLQueryEscaper(c.Attr) + "&value=" + template.URLQueryEscaper(b.Key)
	}
}

// trimLabel shortens a label to fit the gutter, counting characters rather
// than bytes so that a multi-byte one is not cut in half into a replacement
// glyph.
func trimLabel(s string) string {
	const max = 28
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
