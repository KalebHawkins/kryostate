// Package ingest reads written inventory and records what it claims.
//
// A document is imported as a source, not as truth, and it is imported again
// every time it changes. Loading a spreadsheet once and calling the result
// "the database" launders the errors in it: the wrong values arrive with no
// provenance and nothing can ever disagree with them again.
package ingest

import (
	"bufio"
	"encoding/csv"
	"io"
	"strings"
)

// Table is a parsed grid with the heading it appeared under. The heading is
// kept because written inventories carry meaning in their structure: rows
// grouped under a location heading are claiming that location.
type Table struct {
	Section string
	Headers []string
	Rows    [][]string
}

// Row pairs a table's headers with one line of values.
type Row struct {
	Section string
	Values  map[string]string
}

// Rows flattens a table into header-keyed maps.
func (t Table) RowMaps() []Row {
	out := make([]Row, 0, len(t.Rows))
	for _, r := range t.Rows {
		m := make(map[string]string, len(t.Headers))
		for i, h := range t.Headers {
			if i < len(r) {
				m[h] = r[i]
			}
		}
		out = append(out, Row{Section: t.Section, Values: m})
	}
	return out
}

// ParseMarkdown extracts every pipe table from a Markdown document, tracking
// the most recent heading so rows keep the grouping they were written under.
func ParseMarkdown(r io.Reader) ([]Table, error) {
	var (
		tables  []Table
		cur     *Table
		section string
	)
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	flush := func() {
		if cur != nil && len(cur.Rows) > 0 {
			tables = append(tables, *cur)
		}
		cur = nil
	}

	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(line, "#"):
			flush()
			section = strings.TrimSpace(strings.TrimLeft(line, "# "))
		case isTableRow(line):
			cells := splitRow(line)
			switch {
			case cur == nil:
				cur = &Table{Section: section, Headers: cells}
			case isDivider(cells):
				// The dashed line under the header carries no data.
			default:
				cur.Rows = append(cur.Rows, cells)
			}
		default:
			flush()
		}
	}
	flush()
	return tables, sc.Err()
}

func isTableRow(line string) bool {
	return strings.HasPrefix(line, "|") && strings.Count(line, "|") >= 2
}

func isDivider(cells []string) bool {
	for _, c := range cells {
		c = strings.TrimSpace(c)
		if c == "" {
			return false
		}
		for _, r := range c {
			if r != '-' && r != ':' {
				return false
			}
		}
	}
	return len(cells) > 0
}

// splitRow splits a pipe row and strips the backticks that written inventories
// use around identifiers.
func splitRow(line string) []string {
	line = strings.TrimPrefix(line, "|")
	line = strings.TrimSuffix(line, "|")
	parts := strings.Split(line, "|")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		out = append(out, strings.Trim(strings.TrimSpace(p), "`"))
	}
	return out
}

// ParseCSV reads a comma-separated export with a header line. Short rows are
// padded rather than rejected: a trailing empty column is a spreadsheet
// artefact, not a reason to drop a host.
func ParseCSV(r io.Reader, section string) (Table, error) {
	cr := csv.NewReader(r)
	cr.FieldsPerRecord = -1
	cr.TrimLeadingSpace = true
	records, err := cr.ReadAll()
	if err != nil {
		return Table{}, err
	}
	if len(records) == 0 {
		return Table{}, nil
	}
	t := Table{Section: section, Headers: records[0]}
	for _, rec := range records[1:] {
		for len(rec) < len(t.Headers) {
			rec = append(rec, "")
		}
		t.Rows = append(t.Rows, rec)
	}
	return t, nil
}
