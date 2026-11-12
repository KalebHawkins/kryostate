package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// A rule's history is drawn on the server as SVG, with no script. Counts are
// held from one snapshot to the next, so the lines step rather than slope:
// a straight line between two snapshots would claim hosts were done at times
// nobody looked. The live counts are drawn as a short last step at the right,
// so a change since the last snapshot shows at once.

type trendPoint struct {
	At time.Time
	C  store.RuleCounts
}

func trendPoints(hist []store.RuleSnapshot, live store.RuleCounts, now time.Time) []trendPoint {
	pts := make([]trendPoint, 0, len(hist)+1)
	for _, h := range hist {
		pts = append(pts, trendPoint{h.At, h.RuleCounts})
	}
	return append(pts, trendPoint{now, live})
}

// trendXs places each point between left and right, keeping tail for the
// live point's step.
func trendXs(pts []trendPoint, left, right, tail float64) []float64 {
	xs := make([]float64, len(pts))
	span := pts[len(pts)-1].At.Sub(pts[0].At)
	w := right - tail - left
	for i, p := range pts {
		if span <= 0 {
			xs[i] = left + w*float64(i)/float64(max(len(pts)-1, 1))
			continue
		}
		xs[i] = left + w*float64(p.At.Sub(pts[0].At))/float64(span)
	}
	return xs
}

func frac(n, of int) float64 {
	if of <= 0 {
		return 0
	}
	return float64(n) / float64(of)
}

// stepBand is a closed path between two stepped edges, lo and hi, as shares
// of the height. Point i holds from xs[i] to xs[i+1], and the last to end.
func stepBand(xs []float64, end, top, height float64, lo, hi func(store.RuleCounts) float64, pts []trendPoint) string {
	y := func(f float64) float64 { return top + height*(1-f) }
	var b strings.Builder
	for i, p := range pts {
		x1 := end
		if i+1 < len(pts) {
			x1 = xs[i+1]
		}
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f %.1f L%.1f %.1f ", cmd, xs[i], y(hi(p.C)), x1, y(hi(p.C)))
	}
	for i := len(pts) - 1; i >= 0; i-- {
		x1 := end
		if i+1 < len(pts) {
			x1 = xs[i+1]
		}
		fmt.Fprintf(&b, "L%.1f %.1f L%.1f %.1f ", x1, y(lo(pts[i].C)), xs[i], y(lo(pts[i].C)))
	}
	b.WriteString("Z")
	return b.String()
}

// stepLine is the top edge of a band on its own.
func stepLine(xs []float64, end, top, height float64, at func(store.RuleCounts) float64, pts []trendPoint) string {
	y := func(f float64) float64 { return top + height*(1-f) }
	var b strings.Builder
	for i, p := range pts {
		x1 := end
		if i+1 < len(pts) {
			x1 = xs[i+1]
		}
		cmd := "L"
		if i == 0 {
			cmd = "M"
		}
		fmt.Fprintf(&b, "%s%.1f %.1f L%.1f %.1f ", cmd, xs[i], y(at(p.C)), x1, y(at(p.C)))
	}
	return strings.TrimSpace(b.String())
}

func doneShare(c store.RuleCounts) float64 { return frac(c.Done, c.InScope) }
func doneUnknownShare(c store.RuleCounts) float64 {
	return frac(c.Done+c.Unknown, c.InScope)
}
func zeroShare(store.RuleCounts) float64 { return 0 }
func fullShare(c store.RuleCounts) float64 {
	if c.InScope == 0 {
		return 0
	}
	return 1
}

// sparkline is the share done over time, small enough for a table cell.
func sparkline(hist []store.RuleSnapshot, live store.RuleCounts, now time.Time) template.HTML {
	const w, h, pad = 96.0, 22.0, 1.5
	pts := trendPoints(hist, live, now)
	xs := trendXs(pts, 0, w, 6)
	area := stepBand(xs, w, pad, h-2*pad, zeroShare, doneShare, pts)
	line := stepLine(xs, w, pad, h-2*pad, doneShare, pts)
	first := pts[0]
	label := fmt.Sprintf("%d%% done now", live.Pct())
	if len(pts) > 1 {
		label = fmt.Sprintf("%d%% done on %s, %d%% now", first.C.Pct(), first.At.Local().Format("2 Jan"), live.Pct())
	}
	return template.HTML(fmt.Sprintf(
		`<svg class="spark" viewBox="0 0 %.0f %.0f" width="%.0f" height="%.0f" role="img" aria-label="%s" preserveAspectRatio="none">`+
			`<title>%s</title><path class="spark__area" d="%s"/><path class="spark__line" d="%s"/></svg>`,
		w, h, w, h, template.HTMLEscapeString(label), template.HTMLEscapeString(label), area, line))
}

// trendChart is a rule's history as stacked bands of Done, Unknown and Not
// yet, as shares of the hosts in scope at each time.
func trendChart(hist []store.RuleSnapshot, live store.RuleCounts, now time.Time) template.HTML {
	const w, h = 640.0, 170.0
	const left, right, top, bottom = 38.0, 632.0, 8.0, 146.0
	pts := trendPoints(hist, live, now)
	xs := trendXs(pts, left, right, 14)
	height := bottom - top
	var b strings.Builder
	first := pts[0]
	desc := fmt.Sprintf("Now %d of %d hosts done (%d%%), %d not yet, %d unknown.",
		live.Done, live.InScope, live.Pct(), live.NotYet, live.Unknown)
	if len(pts) > 1 {
		desc = fmt.Sprintf("On %s, %d of %d hosts done (%d%%). ", first.At.Local().Format("2 Jan 2006"),
			first.C.Done, first.C.InScope, first.C.Pct()) + desc
	}
	fmt.Fprintf(&b, `<svg class="rtrend" viewBox="0 0 %.0f %.0f" role="img" aria-labelledby="rtrend-t rtrend-d">`, w, h)
	fmt.Fprintf(&b, `<title id="rtrend-t">Progress over time</title><desc id="rtrend-d">%s</desc>`, template.HTMLEscapeString(desc))
	for _, g := range []float64{0, 0.5, 1} {
		y := top + height*(1-g)
		fmt.Fprintf(&b, `<line class="rtrend__grid" x1="%.0f" x2="%.0f" y1="%.1f" y2="%.1f"/>`, left, right, y, y)
		fmt.Fprintf(&b, `<text class="rtrend__axis" x="%.0f" y="%.1f" text-anchor="end" dominant-baseline="middle">%d%%</text>`, left-6, y, int(g*100))
	}
	fmt.Fprintf(&b, `<path class="rtrend__notyet" d="%s"/>`, stepBand(xs, right, top, height, doneUnknownShare, fullShare, pts))
	fmt.Fprintf(&b, `<path class="rtrend__unknown" d="%s"/>`, stepBand(xs, right, top, height, doneShare, doneUnknownShare, pts))
	fmt.Fprintf(&b, `<path class="rtrend__done" d="%s"/>`, stepBand(xs, right, top, height, zeroShare, doneShare, pts))
	fmt.Fprintf(&b, `<path class="rtrend__line" d="%s"/>`, stepLine(xs, right, top, height, doneShare, pts))
	fmt.Fprintf(&b, `<text class="rtrend__axis" x="%.0f" y="%.0f">%s</text>`, left, h-6, first.At.Local().Format("2 Jan 15:04"))
	fmt.Fprintf(&b, `<text class="rtrend__axis" x="%.0f" y="%.0f" text-anchor="end">now</text>`, right, h-6)
	b.WriteString(`</svg>`)
	return template.HTML(b.String())
}
