package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// External is a program supplied by an operator that reports facts about a
// target. It runs on the machine hosting this tool, not on the target.
//
// That boundary is the whole point. The program is told which host to describe
// and is given nothing else: no key, no password, no agent socket. If it needs
// to log in somewhere it brings its own credential, which keeps a contributed
// script from becoming a way to borrow the keys to the estate. Installing one
// is still a privileged act, because it runs with this process's privileges.
type External struct {
	Name    string
	Path    string
	Args    []string
	Timeout time.Duration
	// Env lists additional variables to pass. The program otherwise runs with
	// an empty environment so that it cannot read this process's secrets.
	// Windows is the one exception: Go supplies SystemRoot when it is absent,
	// because a process without it cannot load a DLL and so cannot start.
	Env []string
	// Comment is the note its author wrote beside it, shown on the
	// collectors page.
	Comment string
}

// RunExternal executes the program for one target and returns its envelope.
func RunExternal(ctx context.Context, ex External, targetLabel, targetAddress string) (Envelope, error) {
	if ex.Timeout <= 0 {
		ex.Timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, ex.Timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, ex.Path, ex.Args...)
	cmd.Env = append([]string{
		"KRYOSTATE_TARGET=" + targetLabel,
		"KRYOSTATE_ADDRESS=" + targetAddress,
		"KRYOSTATE_SCHEMA=" + EnvelopeSchema,
		"PATH=" + os.Getenv("PATH"),
	}, ex.Env...)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &limitWriter{w: &stdout, n: MaxOutput}
	cmd.Stderr = &limitWriter{w: &stderr, n: 8 << 10}

	err := cmd.Run()
	if ctx.Err() != nil {
		return Envelope{}, fmt.Errorf("collector %s did not finish within %s", ex.Name, ex.Timeout)
	}
	if err != nil {
		return Envelope{}, fmt.Errorf("collector %s failed: %v: %s", ex.Name, err, truncate(stderr.String(), 300))
	}

	var e Envelope
	dec := json.NewDecoder(strings.NewReader(stdout.String()))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&e); err != nil {
		return Envelope{}, fmt.Errorf("collector %s did not print a valid envelope: %w", ex.Name, err)
	}
	if err := e.Validate(); err != nil {
		return Envelope{}, fmt.Errorf("collector %s: %w", ex.Name, err)
	}
	if e.Collector == "" {
		e.Collector = ex.Name
	}
	return e, nil
}

// limitWriter stops after n bytes. A collector cannot be allowed to decide how
// much memory this process uses.
type limitWriter struct {
	w *bytes.Buffer
	n int
}

// Write keeps the first n bytes and accepts the rest without storing it. It
// reports the full length every time: io.Copy turns a short write into
// io.ErrShortWrite, which would turn a chatty collector into a failed one.
func (l *limitWriter) Write(p []byte) (int, error) {
	n := len(p)
	if l.n <= 0 {
		return n, nil
	}
	if len(p) > l.n {
		p = p[:l.n]
	}
	l.n -= len(p)
	l.w.Write(p)
	return n, nil
}
