package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
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

// headerAfter returns the header row of the first styled table after marker:
// the line after the table's top border.
func headerAfter(out, marker string) string {
	_, rest, ok := strings.Cut(out, marker)
	if !ok {
		return ""
	}
	lines := strings.Split(rest, "\n")
	for i, l := range lines {
		if strings.Contains(l, "╭") && i+1 < len(lines) {
			return lines[i+1]
		}
	}
	return ""
}

// TestEveryTableHasAHeader: at a terminal the first row of a table is drawn
// as its header, so a table written without one shows a value as a heading.
func TestEveryTableHasAHeader(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "1")
	t.Setenv("NO_COLOR", "")
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	eng, err := buildEngine(st, engineFiles{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	read := make(chan []byte)
	go func() { b, _ := io.ReadAll(r); read <- b }()
	stdout := os.Stdout
	os.Stdout = w
	lerr := listCollectors(ctx, eng, st, "")
	os.Stdout = stdout
	w.Close()
	out := string(<-read)
	if lerr != nil {
		t.Fatal(lerr)
	}
	if h := headerAfter(out, "Always collected:"); !strings.Contains(h, "ATTRIBUTE") || !strings.Contains(h, "SYSTEMS") {
		t.Errorf("Always collected header is %q", h)
	}

	src, _ := st.SourceID(ctx, model.SourceCollector, "ssh")
	for name, v := range map[string]string{"web01": "1.20.1", "web02": ""} {
		id, _, err := st.ResolveCI(ctx, model.KindHost, name, []model.Identity{{Scheme: model.IdentHostname, Value: name}})
		if err != nil {
			t.Fatal(err)
		}
		if v != "" {
			if _, err := st.Assert(ctx, model.Assertion{CIID: id, Attr: "nginx.version", Raw: v, SourceID: src, ObservedAt: time.Now()}, nil); err != nil {
				t.Fatal(err)
			}
		}
	}
	var b bytes.Buffer
	if err := runRules(ctx, st, &b, rulesOpts{add: true, name: "nginx", expect: `nginx.version >= "1.24"`}); err != nil {
		t.Fatal(err)
	}
	if h := headerAfter(b.String(), "NOT YET (1)"); !strings.Contains(h, "HOST") || strings.Contains(h, "web01") {
		t.Errorf("NOT YET header is %q", h)
	}
	if h := headerAfter(b.String(), "UNKNOWN (1)"); !strings.Contains(h, "HOST") || strings.Contains(h, "web02") {
		t.Errorf("UNKNOWN header is %q", h)
	}
}
