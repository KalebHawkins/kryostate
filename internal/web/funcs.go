package web

import (
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/KalebHawkins/hearsay/internal/model"
)

func funcs() template.FuncMap {
	return template.FuncMap{
		"chip":      chipClass,
		"ago":       ago,
		"stamp":     stamp,
		"pct":       pct,
		"add":       func(a, b int) int { return a + b },
		"sub":       func(a, b int) int { return a - b },
		"seg":       seg,
		"label":     attrLabel,
		"qesc":      template.URLQueryEscaper,
		"join":      strings.Join,
		"hasPrefix": strings.HasPrefix,
		"short":     short,
		"kindText":  kindText,
		"dict":      dict,
	}
}

// chipClass maps a source kind to its badge. Provenance is shown on every
// value in the interface, because a value without a source is the thing this
// tool exists to stop producing.
func chipClass(k model.SourceKind) string {
	switch k {
	case model.SourceCollector:
		return "chip chip--collector"
	case model.SourceDocument:
		return "chip chip--document"
	case model.SourceHuman:
		return "chip chip--human"
	case model.SourceDerived:
		return "chip chip--derived"
	case model.SourceDNS:
		return "chip chip--dns"
	default:
		return "chip chip--unknown"
	}
}

// ago prints an age at the precision that matters for this data. Minutes are
// noise for a nightly collection; "4 days" is the number that makes someone
// act.
func ago(t time.Time) string {
	if t.IsZero() {
		return "never"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func pct(n, total int) int {
	if total <= 0 {
		return 0
	}
	return n * 100 / total
}

// seg sizes one segment of a proportional bar. Any non-zero count gets at
// least a sliver, so a single failing host in a stack of ninety is still
// visible.
func seg(n, total int) template.CSS {
	if total <= 0 || n <= 0 {
		return template.CSS("flex:0 0 0")
	}
	return template.CSS(fmt.Sprintf("flex:%d 0 0", n))
}

// attrLabel turns a namespaced attribute into something readable without a
// lookup table, so an attribute invented by an operator's own collector gets a
// sensible heading the first time it appears.
func attrLabel(attr string) string {
	s := strings.NewReplacer(".", " ", "_", " ", "-", " ").Replace(attr)
	parts := strings.Fields(s)
	for i, p := range parts {
		switch p {
		case "os", "id", "ip", "cpu", "ram":
			parts[i] = strings.ToUpper(p)
		default:
			parts[i] = strings.ToUpper(p[:1]) + p[1:]
		}
	}
	return strings.Join(parts, " ")
}

// short truncates by characters, not bytes. Cutting a multi-byte character in
// half turns an error message into a replacement glyph at exactly the moment
// somebody is trying to read it.
func short(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
}

func kindText(k model.DiscrepancyKind) string {
	switch k {
	case model.DiscValueConflict:
		return "sources disagree"
	case model.DiscUnresolved:
		return "name does not resolve"
	case model.DiscMultiAddress:
		return "several addresses, none chosen"
	case model.DiscUndocumented:
		return "answers but undocumented"
	case model.DiscStale:
		return "value past its freshness budget"
	case model.DiscUnparsed:
		return "collected but not understood"
	default:
		return string(k)
	}
}

func dict(kv ...any) map[string]any {
	m := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		k, ok := kv[i].(string)
		if !ok {
			continue
		}
		m[k] = kv[i+1]
	}
	return m
}
