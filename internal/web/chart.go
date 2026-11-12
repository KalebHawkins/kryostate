package web

import (
	"fmt"
	"html/template"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/query"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// Chart data is rendered as inline SVG on the server.
//
// There is no charting library here. The charts are counts over a few dozen
// buckets, the marks have to be links into the filtered table, and every
// fragment the server sends is already HTML. A JavaScript chart library would
// add a build step, a supply chain and a re-initialisation bug on every swap,
// in exchange for animation nobody asked for.
//
// The form follows the shape of the data:
//
//   - a donut when there are a handful of answers (six slices or fewer), read
//     as a whole at a glance;
//   - ranked horizontal bars, one hue, when there are more, with the tail
//     folded into "Other";
//   - a histogram when every answer is a number with more than a handful of
//     distinct values, such as memory size.
//
// Attributes whose values are close to one per host (machine ids, serial
// numbers, addresses) are not charted at all: a chart of them groups nothing.

// ChartKind is the form a chart takes.
type ChartKind string

const (
	KindDonut ChartKind = "donut"
	KindBar   ChartKind = "bar"
	KindHist  ChartKind = "hist"
)

const (
	donutMax    = 6  // slices, including unknown buckets
	cardBars    = 8  // bars on a card before folding into Other
	overlayBars = 30 // bars in the overlay before folding
	histBins    = 8  // target number of histogram bins
)

// Slice is one mark: a donut segment, a bar, or a histogram bin.
type Slice struct {
	Label   string
	Count   int
	Href    string // inventory link; empty for folded or unlinkable marks
	Unknown bool   // an absence (never collected, failed, not understood)
	Other   bool   // the folded tail
	Slot    int    // categorical colour slot, 1-based; 0 for single-hue forms
}

// Share is the slice's percentage of the chart's total, rounded.
func (s Slice) Share(total int) string {
	if total == 0 {
		return "0%"
	}
	p := float64(s.Count) * 100 / float64(total)
	if p > 0 && p < 1 {
		return "<1%"
	}
	return fmt.Sprintf("%.0f%%", p)
}

// Chart is one attribute ready to render.
type Chart struct {
	Attr  string
	Title string
	Kind  ChartKind
	DomID string
	// Total is every live host, counted once: answers plus unknowns.
	Total int
	// Card is what the small chart shows; Rows is the full table in the
	// overlay. For a histogram, Unknowns are listed beside the bins.
	Card     []Slice
	Large    []Slice
	Rows     []Slice
	Unknowns []Slice
	Distinct int
}

// KindText describes the form for the overlay's caption.
func (c Chart) KindText() string {
	switch c.Kind {
	case KindDonut:
		return "Share of hosts by value"
	case KindHist:
		return "Hosts by range of value"
	}
	return "Hosts by value, most common first"
}

// identityAttr matches attribute names that identify a single machine. They
// are left off the charts page whatever their counts say, because on a small
// estate an id can look like a category.
var identityAttr = regexp.MustCompile(`(^|[._-])(hostname|fqdn|serial|serial_?number|uuid|guid|machine_?id|boot_?id|host_?id|product_?uuid|mac|mac_?address|macaddr|ip|ipv4|ipv6|ip_?address|address|addresses|addr|fingerprint|sid|hostkey)($|[._-])`)

// classify decides whether and how to chart an attribute from its
// distribution. It returns an empty kind and a reason when the attribute is
// not worth charting.
func classify(attr string, buckets []store.Bucket) (ChartKind, string) {
	if identityAttr.MatchString(attr) {
		return "", "identifies a single host"
	}
	known, hosts, numeric := 0, 0, true
	unknown := 0
	for _, b := range buckets {
		if b.Unknown || b.Key == varies {
			unknown++
			continue
		}
		known++
		hosts += b.Count
		if _, err := strconv.ParseFloat(strings.TrimSpace(b.Key), 64); err != nil {
			numeric = false
		}
	}
	switch {
	case known == 0:
		return "", "no host has a value yet"
	case numeric && known > donutMax:
		return KindHist, ""
	case hosts > 4 && float64(known) > 0.8*float64(hosts):
		return "", "close to one value per host"
	case known+unknown <= donutMax:
		return KindDonut, ""
	}
	return KindBar, ""
}

const varies = "\x00varies"

// NewChart prepares a distribution for display in the given form.
func NewChart(attr, title string, kind ChartKind, buckets []store.Bucket) Chart {
	c := Chart{Attr: attr, Title: title, Kind: kind}
	var known, unknown []Slice
	for _, b := range buckets {
		c.Total += b.Count
		label := b.Label
		if label == "" {
			label = b.Key
		}
		s := Slice{Label: label, Count: b.Count, Href: bucketURL(attr, b)}
		if b.Unknown || b.Key == varies {
			s.Unknown = true
			unknown = append(unknown, s)
			continue
		}
		s.Label = b.Key
		known = append(known, s)
	}
	c.Distinct = len(known)
	sort.SliceStable(known, func(i, j int) bool {
		if known[i].Count != known[j].Count {
			return known[i].Count > known[j].Count
		}
		return lessValue(known[i].Label, known[j].Label)
	})

	switch kind {
	case KindHist:
		c.Card = histogram(attr, buckets)
		c.Large = c.Card
		c.Rows = c.Card
		c.Unknowns = unknown
		return c
	case KindDonut:
		// A handful of numbers (2, 4, 8 cores) reads in numeric order, not
		// in order of how common each is.
		numeric := true
		for _, s := range known {
			if _, err := strconv.ParseFloat(s.Label, 64); err != nil {
				numeric = false
			}
		}
		if numeric {
			sort.SliceStable(known, func(i, j int) bool { return lessValue(known[i].Label, known[j].Label) })
		}
		for i := range known {
			known[i].Slot = i + 1
		}
		c.Card = append(append([]Slice(nil), known...), unknown...)
		c.Large = c.Card
	default:
		c.Card = append(fold(known, cardBars), unknown...)
		c.Large = append(fold(known, overlayBars), unknown...)
	}
	c.Rows = append(append([]Slice(nil), known...), unknown...)
	return c
}

// lessValue sorts numbers numerically and everything else as text.
func lessValue(a, b string) bool {
	fa, ea := strconv.ParseFloat(a, 64)
	fb, eb := strconv.ParseFloat(b, 64)
	if ea == nil && eb == nil {
		return fa < fb
	}
	return a < b
}

// fold keeps the first n-1 values and folds the rest into one Other bar, so a
// chart does not become four hundred bars of height one.
func fold(known []Slice, n int) []Slice {
	if len(known) <= n {
		return append([]Slice(nil), known...)
	}
	out := append([]Slice(nil), known[:n-1]...)
	rest := 0
	for _, s := range known[n-1:] {
		rest += s.Count
	}
	return append(out, Slice{Label: fmt.Sprintf("Other (%d values)", len(known)-n+1), Count: rest, Other: true})
}

// histogram bins numeric values into ranges with round edges.
func histogram(attr string, buckets []store.Bucket) []Slice {
	type pt struct {
		v float64
		n int
	}
	var pts []pt
	lo, hi := math.Inf(1), math.Inf(-1)
	integers := true
	for _, b := range buckets {
		if b.Unknown || b.Key == varies {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(b.Key), 64)
		if err != nil {
			continue
		}
		if v != math.Trunc(v) {
			integers = false
		}
		pts = append(pts, pt{v, b.Count})
		lo, hi = math.Min(lo, v), math.Max(hi, v)
	}
	if len(pts) == 0 {
		return nil
	}
	step := niceStep((hi - lo) / histBins)
	if integers && step < 1 {
		step = 1
	}
	start := math.Floor(lo/step) * step
	n := int(math.Floor((hi-start)/step)) + 1
	out := make([]Slice, n)
	for i := range out {
		a, b := start+float64(i)*step, start+float64(i+1)*step
		label := fmt.Sprintf("%s – %s", compact(a), compact(b))
		if integers && step == 1 {
			label = compact(a)
		}
		out[i] = Slice{Label: label}
		if isIdent(attr) {
			q := fmt.Sprintf("%s >= %s && %s < %s", attr, num(a), attr, num(b))
			out[i].Href = "/hosts?q=" + url.QueryEscape(q)
		}
	}
	for _, p := range pts {
		i := int(math.Floor((p.v - start) / step))
		if i >= n {
			i = n - 1
		}
		out[i].Count += p.n
	}
	return out
}

func niceStep(rough float64) float64 {
	if rough <= 0 || math.IsNaN(rough) {
		return 1
	}
	mag := math.Pow(10, math.Floor(math.Log10(rough)))
	switch r := rough / mag; {
	case r <= 1:
		return mag
	case r <= 2:
		return 2 * mag
	case r <= 5:
		return 5 * mag
	}
	return 10 * mag
}

func num(f float64) string { return strconv.FormatFloat(f, 'f', -1, 64) }

// compact prints an axis number: in full under 10,000, then 12K / 1.5M.
func compact(f float64) string {
	a := math.Abs(f)
	switch {
	case a >= 1e6:
		return strings.TrimSuffix(strconv.FormatFloat(f/1e6, 'f', 1, 64), ".0") + "M"
	case a >= 1e4:
		return strings.TrimSuffix(strconv.FormatFloat(f/1e3, 'f', 1, 64), ".0") + "K"
	}
	return num(f)
}

var reIdent = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_.-]*$`)

// isIdent reports whether an attribute can be written bare in a query.
func isIdent(attr string) bool {
	switch attr {
	case "true", "false", "has":
		return false
	}
	return reIdent.MatchString(attr)
}

// bucketURL builds the link a mark points at: a query for a value, and the
// structured filters for the three kinds of unknown.
//
// The three causes of unknown get three different links. Sending "never
// collected" and "collection failed" to the same filtered list would undo the
// reason they are counted separately: one needs a connection profile and the
// other needs somebody to find out why the connection is failing.
func bucketURL(attr string, b store.Bucket) string {
	switch {
	case b.Key == string(model.UnknownUnparsed):
		return "/hosts?unparsed=" + url.QueryEscape(attr)
	case b.Unknown:
		return "/hosts?unknown=" + url.QueryEscape(attr) + "&because=" + url.QueryEscape(b.Key)
	case b.Key == "" || b.Key == varies:
		return ""
	case isIdent(attr):
		return "/hosts?q=" + url.QueryEscape(attr+" == "+query.Quote(b.Key))
	default:
		return "/hosts?attr=" + url.QueryEscape(attr) + "&value=" + url.QueryEscape(b.Key)
	}
}

// trimLabel shortens a label to fit the gutter, counting characters rather
// than bytes so that a multi-byte one is not cut in half into a replacement
// glyph.
func trimLabel(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// ---- rendering ----

func esc(s string) string { return template.HTMLEscapeString(s) }

// markClass is the CSS class that colours a mark.
func markClass(s Slice) string {
	switch {
	case s.Unknown:
		return "viz__mark viz__mark--unknown"
	case s.Other:
		return "viz__mark viz__mark--other"
	case s.Slot > 0:
		return fmt.Sprintf("viz__mark viz-s%d", s.Slot)
	}
	return "viz__mark viz-s1"
}

// hatch defines the texture an unknown mark wears, so that an absence never
// reads as a measured value on colour alone. Pattern ids are page-global, so
// each SVG gets its own.
func hatch(id string) string {
	return `<defs><pattern id="` + id + `" width="6" height="6" patternUnits="userSpaceOnUse" patternTransform="rotate(45)">` +
		`<rect class="viz__hatch-bg" width="6" height="6"></rect>` +
		`<line class="viz__hatch-line" x1="0" y1="0" x2="0" y2="6"></line></pattern></defs>`
}

func fillAttr(s Slice, hatchID string) string {
	if s.Unknown {
		return ` style="fill:url(#` + hatchID + `)"`
	}
	return ""
}

// wrapMark opens and closes a link around a mark when linking is wanted. The
// links in a chart are kept out of the tab order: the table under the chart
// carries the same links for the keyboard and for screen readers.
func wrapMark(sb *strings.Builder, s Slice, total int, link bool, body string) {
	tip := fmt.Sprintf("%s: %d hosts (%s)", s.Label, s.Count, s.Share(total))
	if link && s.Href != "" {
		h := esc(s.Href)
		fmt.Fprintf(sb, `<a href="%s" hx-get="%s" hx-target="#main" hx-push-url="true" tabindex="-1"><title>%s</title>%s</a>`,
			h, h, esc(tip), body)
		return
	}
	fmt.Fprintf(sb, `<g><title>%s</title>%s</g>`, esc(tip), body)
}

// CardSVG is the chart as it appears on the page: no links of its own, since
// clicking it opens the overlay.
func (c Chart) CardSVG() template.HTML { return c.render(c.Card, false, c.DomID+"-c") }

// LargeSVG is the overlay's chart, whose marks link to the inventory.
func (c Chart) LargeSVG() template.HTML { return c.render(c.Large, true, c.DomID+"-l") }

func (c Chart) render(slices []Slice, link bool, id string) template.HTML {
	if len(slices) == 0 {
		return template.HTML(`<p class="muted">Nothing recorded for this attribute yet.</p>`)
	}
	switch c.Kind {
	case KindDonut:
		return c.donut(slices, link, id)
	case KindHist:
		return c.hist(slices, link, id)
	}
	return c.bars(slices, link, id)
}

func (c Chart) donut(slices []Slice, link bool, id string) template.HTML {
	const cx, cy, ro, ri = 80.0, 80.0, 76.0, 50.0
	var sb strings.Builder
	fmt.Fprintf(&sb, `<div class="viz__donut"><svg class="viz__svg" viewBox="0 0 160 160" aria-hidden="true" focusable="false">%s`, hatch(id+"-h"))
	total := 0
	for _, s := range slices {
		total += s.Count
	}
	a0 := -math.Pi / 2
	for _, s := range slices {
		if s.Count == 0 {
			continue
		}
		frac := float64(s.Count) / float64(total)
		a1 := a0 + frac*2*math.Pi
		var d string
		if frac >= 0.9999 {
			// A full ring cannot be drawn as one arc.
			d = fmt.Sprintf("M%.2f %.2fA%.0f %.0f 0 1 1 %.2f %.2fA%.0f %.0f 0 1 1 %.2f %.2fZM%.2f %.2fA%.0f %.0f 0 1 0 %.2f %.2fA%.0f %.0f 0 1 0 %.2f %.2fZ",
				cx, cy-ro, ro, ro, cx, cy+ro, ro, ro, cx, cy-ro,
				cx, cy-ri, ri, ri, cx, cy+ri, ri, ri, cx, cy-ri)
		} else {
			large := 0
			if a1-a0 > math.Pi {
				large = 1
			}
			d = fmt.Sprintf("M%.2f %.2fA%.0f %.0f 0 %d 1 %.2f %.2fL%.2f %.2fA%.0f %.0f 0 %d 0 %.2f %.2fZ",
				cx+ro*math.Cos(a0), cy+ro*math.Sin(a0), ro, ro, large, cx+ro*math.Cos(a1), cy+ro*math.Sin(a1),
				cx+ri*math.Cos(a1), cy+ri*math.Sin(a1), ri, ri, large, cx+ri*math.Cos(a0), cy+ri*math.Sin(a0))
		}
		wrapMark(&sb, s, c.Total, link, fmt.Sprintf(`<path class="%s viz__seg"%s fill-rule="evenodd" d="%s"></path>`, markClass(s), fillAttr(s, id+"-h"), d))
		a0 = a1
	}
	fmt.Fprintf(&sb, `<text class="viz__total" x="80" y="80" text-anchor="middle" dominant-baseline="central">%d</text>`, c.Total)
	fmt.Fprintf(&sb, `<text class="viz__axis" x="80" y="102" text-anchor="middle">hosts</text>`)
	sb.WriteString(`</svg><ul class="viz__legend">`)
	for _, s := range slices {
		sw := markClass(s)
		fmt.Fprintf(&sb, `<li><svg class="viz__swatch" viewBox="0 0 12 12" aria-hidden="true">%s<rect class="%s" width="12" height="12" rx="3"%s></rect></svg><span class="viz__name" title="%s">%s</span><span class="viz__num">%d</span></li>`,
			swatchHatch(s, id), sw, swatchFill(s, id), esc(s.Label), esc(trimLabel(s.Label, 28)), s.Count)
	}
	sb.WriteString(`</ul></div>`)
	return template.HTML(sb.String())
}

func swatchHatch(s Slice, id string) string {
	if s.Unknown {
		return hatch(id + "-sw")
	}
	return ""
}

func swatchFill(s Slice, id string) string {
	if s.Unknown {
		return ` style="fill:url(#` + id + `-sw)"`
	}
	return ""
}

// barPath is a horizontal bar, square at the baseline and rounded at the
// data end.
func barPath(x, y, w, h float64) string {
	r := math.Min(4, w/2)
	return fmt.Sprintf("M%.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fZ",
		x, y, x+w-r, x+w, y, x+w, y+r, y+h-r, x+w, y+h, x+w-r, y+h, x)
}

// colPath is a vertical column, square at the baseline and rounded on top.
func colPath(x, base, w, h float64) string {
	r := math.Min(4, math.Min(w/2, h))
	top := base - h
	return fmt.Sprintf("M%.1f %.1fV%.1fQ%.1f %.1f %.1f %.1fH%.1fQ%.1f %.1f %.1f %.1fV%.1fZ",
		x, base, top+r, x, top, x+r, top, x+w-r, x+w, top, x+w, top+r, base)
}

func (c Chart) bars(slices []Slice, link bool, id string) template.HTML {
	// The overlay is drawn on a wider canvas rather than scaled up, so text
	// and bar thickness stay the same size on screen.
	width, labelW, maxLabel := 360.0, 120.0, 16
	if link {
		width, labelW, maxLabel = 820.0, 240.0, 36
	}
	const countW, band, thick = 44.0, 26.0, 16.0
	height := float64(len(slices))*band + 4
	max := 0
	for _, s := range slices {
		if s.Count > max {
			max = s.Count
		}
	}
	if max == 0 {
		max = 1
	}
	area := width - labelW - countW
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="viz__svg" viewBox="0 0 %.0f %.0f" aria-hidden="true" focusable="false">%s`, width, height, hatch(id+"-h"))
	fmt.Fprintf(&sb, `<line class="viz__baseline" x1="%.1f" x2="%.1f" y1="0" y2="%.0f"></line>`, labelW, labelW, height)
	for i, s := range slices {
		y := float64(i)*band + (band-thick)/2 + 2
		w := float64(s.Count) * area / float64(max)
		if w < 2 && s.Count > 0 {
			w = 2
		}
		body := fmt.Sprintf(`<rect class="viz__hit" x="0" y="%.1f" width="%.0f" height="%.0f"></rect>`, float64(i)*band+2, width, band) +
			fmt.Sprintf(`<text class="viz__label" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="central">%s</text>`, labelW-8, y+thick/2, esc(trimLabel(s.Label, maxLabel)))
		if w > 0 {
			body += fmt.Sprintf(`<path class="%s"%s d="%s"></path>`, markClass(s), fillAttr(s, id+"-h"), barPath(labelW, y, w, thick))
		}
		body += fmt.Sprintf(`<text class="viz__value" x="%.1f" y="%.1f" dominant-baseline="central">%d</text>`, labelW+w+6, y+thick/2, s.Count)
		wrapMark(&sb, s, c.Total, link, body)
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

func (c Chart) hist(slices []Slice, link bool, id string) template.HTML {
	width, height := 360.0, 200.0
	if link {
		width, height = 820.0, 280.0
	}
	const left, right, top, bottom = 36.0, 8.0, 10.0, 34.0
	max := 0
	for _, s := range slices {
		if s.Count > max {
			max = s.Count
		}
	}
	ticks := axisTicks(max)
	ymax := float64(ticks[len(ticks)-1])
	plotH := height - top - bottom
	base := height - bottom
	band := (width - left - right) / float64(len(slices))
	var sb strings.Builder
	fmt.Fprintf(&sb, `<svg class="viz__svg" viewBox="0 0 %.0f %.0f" aria-hidden="true" focusable="false">`, width, height)
	for _, t := range ticks {
		y := base - float64(t)/ymax*plotH
		cls := "viz__grid"
		if t == 0 {
			cls = "viz__baseline"
		}
		fmt.Fprintf(&sb, `<line class="%s" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"></line>`, cls, left, width-right, y, y)
		fmt.Fprintf(&sb, `<text class="viz__axis" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="central">%s</text>`, left-6, y, compact(float64(t)))
	}
	// Labels: every bin when they fit, otherwise every other one.
	every := 1
	if band < 46 {
		every = 2
	}
	for i, s := range slices {
		x := left + float64(i)*band + 1
		h := float64(s.Count) / ymax * plotH
		body := fmt.Sprintf(`<rect class="viz__hit" x="%.1f" y="%.0f" width="%.1f" height="%.0f"></rect>`, x-1, top, band, plotH)
		if h > 0 {
			body += fmt.Sprintf(`<path class="viz__mark viz-s1" d="%s"></path>`, colPath(x, base, band-2, math.Max(h, 2)))
		}
		if i%every == 0 {
			body += fmt.Sprintf(`<text class="viz__axis" x="%.1f" y="%.0f" text-anchor="middle">%s</text>`, x+band/2-1, base+16, esc(trimLabel(s.Label, 16)))
		}
		wrapMark(&sb, s, c.Total, link, body)
	}
	sb.WriteString(`</svg>`)
	return template.HTML(sb.String())
}

// axisTicks returns round tick values from zero to at least max.
func axisTicks(max int) []int {
	if max <= 0 {
		return []int{0, 1}
	}
	step := int(niceStep(float64(max) / 4))
	if step < 1 {
		step = 1
	}
	var out []int
	for t := 0; ; t += step {
		out = append(out, t)
		if t >= max {
			break
		}
	}
	return out
}

// ---- page ----

type skippedAttr struct {
	Attr   string
	Title  string
	Reason string
	Hosts  int
}

type chartsData struct {
	Charts  []Chart
	Skipped []skippedAttr
	// Single is set when one attribute was asked for by name: the page then
	// shows the large chart and its table inline, which is also what a
	// browser without script gets from clicking a chart.
	Single bool
}

func (s *Server) handleCharts(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	attrs, err := s.Store.Attrs(ctx)
	if err != nil {
		s.fail(w, err)
		return
	}
	titles := s.attrTitles(ctx)
	var d chartsData
	domIDs := map[string]int{}
	requested := r.URL.Query()["attr"]
	d.Single = len(requested) > 0
	for _, a := range attrs {
		if d.Single && !contains(requested, a.Attr) {
			continue
		}
		buckets, err := s.Store.Distribution(ctx, a.Attr)
		if err != nil {
			s.fail(w, err)
			return
		}
		title := a.Label
		if title == "" {
			title = titles[a.Attr]
		}
		if title == "" {
			title = attrLabel(a.Attr)
		}
		kind, reason := classify(a.Attr, buckets)
		if kind == "" {
			if !d.Single {
				d.Skipped = append(d.Skipped, skippedAttr{Attr: a.Attr, Title: title, Reason: reason, Hosts: a.CICount})
				continue
			}
			// Asked for by name: draw it anyway, as bars.
			kind = KindBar
		}
		c := NewChart(a.Attr, title, kind, buckets)
		// Named for the attribute so /charts#chart-os-version keeps pointing at
		// the same chart however many others come before it.
		c.DomID = "chart-" + slug(a.Attr, domIDs)
		d.Charts = append(d.Charts, c)
	}
	s.render(w, r, "charts.html", "Charts", "charts", d)
}
