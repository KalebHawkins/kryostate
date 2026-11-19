package main

import (
	"fmt"
	"strings"
)

// safeText makes text that came from a host safe to print to a terminal. A
// host controls what its commands print, so its output must not move the
// cursor, change the title or colours, or hide other lines. Control
// characters other than tab (and newline, when keepNewlines is set) are shown
// as escapes such as \x1b instead of being sent to the terminal.
func safeText(s string, keepNewlines bool) string {
	if keepNewlines {
		s = strings.ReplaceAll(s, "\r\n", "\n")
	}
	if !strings.ContainsFunc(s, func(r rune) bool { return unsafeRune(r, keepNewlines) }) {
		return s
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case !unsafeRune(r, keepNewlines):
			b.WriteRune(r)
		case r < 0x80:
			fmt.Fprintf(&b, `\x%02x`, r)
		default:
			fmt.Fprintf(&b, `\u%04x`, r)
		}
	}
	return b.String()
}

func unsafeRune(r rune, keepNewlines bool) bool {
	switch {
	case r == '\t', r == '\n' && keepNewlines:
		return false
	case r < 0x20, r == 0x7f, r >= 0x80 && r <= 0x9f:
		return true
	}
	return false
}
