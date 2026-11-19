package main

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/store"
)

func TestSafeText(t *testing.T) {
	for _, c := range []struct {
		in   string
		keep bool
		want string
	}{
		{"plain\ttext", false, "plain\ttext"},
		{"\x1b]0;owned\x07title", false, `\x1b]0;owned\x07title`},
		{"a\x1b[2Jb\x7f", false, `a\x1b[2Jb\x7f`},
		{"one\r\ntwo\rthree\n", true, "one\ntwo\\x0dthree\n"},
		{"one\ntwo", false, `one\x0atwo`},
		{"c1\u009bmé", false, `c1\u009bmé`},
	} {
		if got := safeText(c.in, c.keep); got != c.want {
			t.Errorf("safeText(%q, %v) = %q, want %q", c.in, c.keep, got, c.want)
		}
	}
}

// TestTryDoesNotPassEscapesToTheTerminal: what a command printed is shown
// with its control characters made visible.
func TestTryDoesNotPassEscapesToTheTerminal(t *testing.T) {
	t.Setenv("CLICOLOR_FORCE", "")
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
	specs, err := specsToTry(ctx, eng, "linux.os.family", "")
	if err != nil {
		t.Fatal(err)
	}
	sample := filepath.Join(t.TempDir(), "out.txt")
	if err := os.WriteFile(sample, []byte("Linux\x1b]0;owned\x07\n\x1b[2Jsecond\n"), 0o600); err != nil {
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
	tryCollector(ctx, eng, st, specs, "", sample, "")
	os.Stdout = stdout
	w.Close()
	out := string(<-read)
	if strings.ContainsAny(out, "\x1b\x07") {
		t.Fatalf("raw control characters reached the terminal: %q", out)
	}
	if !strings.Contains(out, `\x1b[2Jsecond`) {
		t.Errorf("output not shown escaped:\n%s", out)
	}
}
