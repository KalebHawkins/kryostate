// Package secret resolves a credential reference into credential material at
// the moment it is needed.
//
// The database stores references such as "agent:" or "keyring:hearsay/svc-ro",
// never keys or passwords. That is not a convenience: this tool holds the
// means to log in to every host it manages, which makes it more valuable to an
// attacker than any single host it manages. A reference that resolves through
// ssh-agent or an external broker means a stolen database file is an inventory
// leak rather than an estate compromise.
//
// Two things this package deliberately does not do. It does not implement
// cryptography of its own; where material must rest locally it is held in an
// age-encrypted file using an audited implementation. And it does not promise
// that material is unrecoverable from process memory: Go moves and copies
// memory, and anyone with privileges on this host has already won.
package secret

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Kind describes what a resolved secret is, so a caller cannot accidentally
// hand a password to a public-key authenticator.
type Kind string

const (
	KindPrivateKey Kind = "private_key"
	KindPassword   Kind = "password"
	KindPassphrase Kind = "passphrase"
	// KindAgent carries no material. The private key stays in an agent and
	// this process only ever asks it to sign.
	KindAgent Kind = "agent"
)

// Secret is resolved credential material with a known kind.
type Secret struct {
	Kind  Kind
	Bytes []byte
	// Socket is set for KindAgent: the address of the agent to sign with.
	Socket string
}

// Zero overwrites the material. It reduces the window in which a heap dump is
// useful; it does not make one safe.
func (s *Secret) Zero() {
	for i := range s.Bytes {
		s.Bytes[i] = 0
	}
	s.Bytes = nil
}

// Resolver turns one scheme of reference into material.
type Resolver interface {
	Scheme() string
	Resolve(ctx context.Context, value string) (Secret, error)
}

// ErrNotFound means the reference names nothing.
var ErrNotFound = errors.New("credential reference did not resolve")

// Auditor records credential use. The reference is recorded; the material
// never is.
type Auditor func(ctx context.Context, action, ref, detail string)

// Registry dispatches references to resolvers by scheme.
type Registry struct {
	mu        sync.RWMutex
	resolvers map[string]Resolver
	audit     Auditor
}

// NewRegistry builds a registry with the resolvers that need no configuration.
func NewRegistry(audit Auditor) *Registry {
	r := &Registry{resolvers: map[string]Resolver{}, audit: audit}
	r.Register(agentResolver{})
	r.Register(envResolver{})
	r.Register(fileResolver{})
	r.Register(execResolver{})
	r.Register(keyringResolver{})
	return r
}

// Register adds a resolver, replacing any resolver for the same scheme.
func (r *Registry) Register(res Resolver) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.resolvers[res.Scheme()] = res
}

// Schemes lists the reference schemes this registry understands.
func (r *Registry) Schemes() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.resolvers))
	for s := range r.resolvers {
		out = append(out, s)
	}
	return out
}

// Resolve turns "scheme:value" into material. A reference with no scheme is
// rejected rather than guessed at: guessing is how a password ends up being
// tried as a file path and logged.
func (r *Registry) Resolve(ctx context.Context, ref string) (Secret, error) {
	scheme, value, ok := strings.Cut(ref, ":")
	if !ok || scheme == "" {
		return Secret{}, fmt.Errorf("%q is not a credential reference: expected scheme:value", ref)
	}
	r.mu.RLock()
	res, found := r.resolvers[scheme]
	r.mu.RUnlock()
	if !found {
		return Secret{}, fmt.Errorf("%w: no resolver for scheme %q", ErrNotFound, scheme)
	}
	s, err := res.Resolve(ctx, value)
	if r.audit != nil {
		detail := "ok"
		if err != nil {
			detail = "failed"
		}
		r.audit(ctx, "resolve_credential", redact(ref), detail)
	}
	return s, err
}

// redact keeps the scheme and drops the rest, so an audit trail says which
// credential was used without saying where to find it.
func redact(ref string) string {
	scheme, value, ok := strings.Cut(ref, ":")
	if !ok {
		return "malformed"
	}
	if len(value) <= 4 {
		return scheme + ":…"
	}
	return scheme + ":" + value[:4] + "…"
}

// agentResolver returns a handle to an SSH agent. No key material enters this
// process: the agent signs on request. This is the reference kind to prefer.
type agentResolver struct{}

func (agentResolver) Scheme() string { return "agent" }

func (agentResolver) Resolve(_ context.Context, value string) (Secret, error) {
	sock := value
	if sock == "" {
		sock = os.Getenv("SSH_AUTH_SOCK")
	}
	if sock == "" {
		return Secret{}, fmt.Errorf("%w: no agent address and SSH_AUTH_SOCK is unset", ErrNotFound)
	}
	return Secret{Kind: KindAgent, Socket: sock}, nil
}

// envResolver reads an environment variable. Convenient in a container, and
// visible to anything that can read the process environment, so it is the
// weakest option offered.
type envResolver struct{}

func (envResolver) Scheme() string { return "env" }

func (envResolver) Resolve(_ context.Context, value string) (Secret, error) {
	v, ok := os.LookupEnv(value)
	if !ok || v == "" {
		return Secret{}, fmt.Errorf("%w: %s is not set", ErrNotFound, value)
	}
	return Secret{Kind: KindPassword, Bytes: []byte(v)}, nil
}

// fileResolver reads a key from disk, decrypting it first when the path ends
// in .age.
type fileResolver struct{}

func (fileResolver) Scheme() string { return "file" }

func (fileResolver) Resolve(ctx context.Context, value string) (Secret, error) {
	if strings.HasSuffix(value, ".age") {
		return ageFile(ctx, value)
	}
	b, err := os.ReadFile(value)
	if err != nil {
		return Secret{}, fmt.Errorf("%w: %v", ErrNotFound, err)
	}
	return Secret{Kind: KindPrivateKey, Bytes: b}, nil
}

// execResolver runs a command and takes its standard output as the secret.
// This is the escape hatch for an existing broker; the command is configuration
// and runs with this process's privileges, so adding one is a privileged act.
type execResolver struct{}

func (execResolver) Scheme() string { return "exec" }

func (execResolver) Resolve(ctx context.Context, value string) (Secret, error) {
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	parts := strings.Fields(value)
	if len(parts) == 0 {
		return Secret{}, errors.New("exec reference needs a command")
	}
	cmd := exec.CommandContext(ctx, parts[0], parts[1:]...)
	// The command inherits no environment: a helper that needs configuration
	// should be given it as arguments, where it is visible in the reference.
	cmd.Env = []string{}
	out, err := cmd.Output()
	if err != nil {
		return Secret{}, fmt.Errorf("%w: credential helper failed: %v", ErrNotFound, err)
	}
	return Secret{Kind: KindPassword, Bytes: trimNewline(out)}, nil
}

func trimNewline(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
