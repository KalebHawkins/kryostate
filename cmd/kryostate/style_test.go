package main

import (
	"bytes"
	"fmt"
	"os"
	"strings"
	"testing"
)

// TestPlainTableWhenNotATerminal: output going anywhere but a terminal is the
// tab-aligned text scripts already parse, with no borders or escape codes.
func TestPlainTableWhenNotATerminal(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "")
	var b bytes.Buffer
	tw := newTable(&b)
	fmt.Fprintln(tw, "HOST\tSTATUS")
	fmt.Fprintln(tw, "web01\tenabled")
	fmt.Fprintln(tw, "db-primary\tHOST DISABLED")
	if err := tw.Flush(); err != nil {
		t.Fatal(err)
	}
	want := "HOST        STATUS\nweb01       enabled\ndb-primary  HOST DISABLED\n"
	if b.String() != want {
		t.Fatalf("got %q, want %q", b.String(), want)
	}
}

// TestNoColorWins: NO_COLOR turns styling off even when it is forced.
func TestNoColorWins(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	if !styled(&bytes.Buffer{}) {
		t.Fatal("CLICOLOR_FORCE did not force styling")
	}
	t.Setenv("NO_COLOR", "1")
	if styled(os.Stdout) {
		t.Fatal("NO_COLOR did not turn styling off")
	}
	var b bytes.Buffer
	tw := newTable(&b)
	fmt.Fprintln(tw, "A\tB")
	tw.Flush()
	if strings.Contains(b.String(), "\x1b[") || strings.Contains(b.String(), "╭") {
		t.Fatalf("styled output with NO_COLOR: %q", b.String())
	}
}
