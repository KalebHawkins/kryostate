package collect

import (
	"context"
	"errors"
	"io"
	"time"
)

// MaxOutput bounds what a single command may return. A host that answers a
// version query with a gigabyte of text is either broken or hostile, and
// either way it must not decide how much memory this process allocates.
const MaxOutput = 256 << 10

// DefaultCommandTimeout bounds one command. Note what it does not do: killing
// the local side of a connection does not stop the command on the remote host.
// Collectors are written to be short and side-effect-free so that an abandoned
// command is harmless, and runs are not retried automatically after a timeout
// because the first attempt may still be running.
const DefaultCommandTimeout = 30 * time.Second

// Result is the outcome of one command.
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// Transport runs commands on a host. Implementations hold credentials;
// everything downstream of them handles only bytes.
type Transport interface {
	// Run executes cmd and returns its output. An error means the command
	// could not be run; a non-zero ExitCode means it ran and failed. The
	// distinction matters: one is an unreachable host, the other is a host
	// without that command.
	Run(ctx context.Context, cmd string) (Result, error)
	// HostKey returns the key the host presented, if the transport has one.
	HostKey() string
	Close() error
}

// ErrHostKeyChanged is returned when a host presents a key other than the one
// recorded. Collection stops rather than continuing against an unverified
// host: a configuration database is not worth handing credentials to an
// impostor for.
var ErrHostKeyChanged = errors.New("host key does not match the recorded key")

// ErrHostKeyUnknown is returned under the strict host key policy when no key
// has been recorded for a host. Under that policy the first connection is as
// dangerous as any other, so there is nothing to accept it on faith with.
var ErrHostKeyUnknown = errors.New("no host key recorded and the policy is strict")

// readLimited keeps at most MaxOutput bytes and reports whether it stopped
// early. It goes on reading past the limit and throws the rest away, because
// an SSH pipe that nobody drains stops the remote command mid-write, and a
// command stopped mid-write never exits.
func readLimited(r io.Reader) (string, bool, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxOutput+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > MaxOutput {
		_, _ = io.Copy(io.Discard, r)
		return string(b[:MaxOutput]), true, nil
	}
	return string(b), false, nil
}
