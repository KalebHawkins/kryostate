package main

// Output styling. At a terminal, tables are drawn with lipgloss and states are
// coloured; anywhere else (a pipe, a file, a script, NO_COLOR set) the output
// is the plain tab-aligned text it has always been, byte for byte, so nothing
// that parses it changes.

import (
	"bytes"
	"io"
	"os"
	"strings"
	"text/tabwriter"

	"charm.land/lipgloss/v2"
	"charm.land/lipgloss/v2/table"
	"golang.org/x/term"
)

// The terminal's own basic colours, so the palette follows whatever theme,
// light or dark, the terminal has.
var (
	styleHeader = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("6"))
	styleBorder = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleCell   = lipgloss.NewStyle().Padding(0, 1)
	styleFaint  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
	styleGood   = lipgloss.NewStyle().Foreground(lipgloss.Color("2"))
	styleWarn   = lipgloss.NewStyle().Foreground(lipgloss.Color("3"))
	styleBad    = lipgloss.NewStyle().Foreground(lipgloss.Color("1")).Bold(true)
	styleTitle  = lipgloss.NewStyle().Bold(true)
	styleLabel  = lipgloss.NewStyle().Foreground(lipgloss.Color("8"))
)

// styled reports whether w is a terminal that wants colour. CLICOLOR_FORCE
// asks for it anyway, for a pager such as less -R.
func styled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	if v := os.Getenv("CLICOLOR_FORCE"); v != "" && v != "0" {
		return true
	}
	f, ok := w.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

// tableWriter collects tab-separated lines, the first being the header, and
// draws them when flushed.
type tableWriter struct {
	w   io.Writer
	buf bytes.Buffer
}

func newTable(w io.Writer) *tableWriter { return &tableWriter{w: w} }

func (t *tableWriter) Write(p []byte) (int, error) { return t.buf.Write(p) }

func (t *tableWriter) Flush() error {
	defer t.buf.Reset()
	if !styled(t.w) {
		tw := tabwriter.NewWriter(t.w, 0, 0, 2, ' ', 0)
		if _, err := tw.Write(t.buf.Bytes()); err != nil {
			return err
		}
		return tw.Flush()
	}
	lines := strings.Split(strings.TrimRight(t.buf.String(), "\n"), "\n")
	if len(lines) == 0 || lines[0] == "" {
		return nil
	}
	header := strings.Split(lines[0], "\t")
	rows := make([][]string, 0, len(lines)-1)
	for _, l := range lines[1:] {
		cells := strings.Split(l, "\t")
		for len(cells) < len(header) {
			cells = append(cells, "")
		}
		rows = append(rows, cells)
	}
	faintCol := map[int]bool{}
	for i, h := range header {
		if h == "ID" {
			faintCol[i] = true
		}
	}
	tb := table.New().
		Border(lipgloss.RoundedBorder()).
		BorderStyle(styleBorder).
		BorderColumn(false).
		Headers(header...).
		Rows(rows...).
		StyleFunc(func(row, col int) lipgloss.Style {
			if row == table.HeaderRow {
				return styleHeader.Padding(0, 1)
			}
			if row < 0 || row >= len(rows) || col >= len(rows[row]) {
				return styleCell
			}
			if faintCol[col] {
				return styleFaint.Padding(0, 1)
			}
			return stateStyle(rows[row][col]).Padding(0, 1)
		})
	_, err := lipgloss.Fprintln(t.w, tb.Render())
	return err
}

// stateStyle colours a cell by the state it names, so that the one failure
// in a column of "ok" is the first thing seen.
func stateStyle(cell string) lipgloss.Style {
	v := strings.ToLower(strings.TrimSpace(cell))
	switch {
	case v == "":
		return styleCell
	case strings.Contains(v, "on hold"), strings.HasSuffix(v, " failed"),
		v == "failed", v == "error", v == "auth_failed", v == "refused",
		v == "timeout", v == "nxdomain", v == "no_answer", v == "interrupted":
		return styleBad
	case v == "host disabled", v == "disabled", v == "connection disabled",
		v == "partial", v == "counting", v == "never", v == "unknown", v == "off":
		return styleWarn
	case v == "ok", v == "enabled", v == "complete", v == "on", v == "success":
		return styleGood
	case v == "-":
		return styleFaint
	}
	return styleCell
}

// render applies st when w is a styled terminal.
func render(w io.Writer, st lipgloss.Style, s string) string {
	if !styled(w) {
		return s
	}
	return st.Render(s)
}

// field prints "label: value" with the label dimmed.
func field(label, value string) {
	lipgloss.Printf("%s %s\n", render(os.Stdout, styleLabel, label+":"), value)
}
