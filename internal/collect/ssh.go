package collect

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"

	"github.com/KalebHawkins/hearsay/internal/secret"
)

// SSHConfig describes one connection. The credential is a reference, resolved
// at dial time; nothing here holds material between connections.
type SSHConfig struct {
	Address string
	Port    int
	User    string
	// CredentialRef is resolved by the secret registry. Prefer "agent:", which
	// keeps the private key in an agent and out of this process entirely.
	CredentialRef string
	// BastionRef describes a jump host as "user@host:port/credential-ref".
	// Empty means connect directly.
	Bastion string
	// KnownHostKey is the authorized_keys-format key previously recorded. When
	// set, a different key aborts the connection. When empty, the key
	// presented is captured for pinning and the connection proceeds once.
	KnownHostKey string
	// KnownBastionKey is the same thing for the jump host. A bastion sits on
	// the routed path to everything behind it, so an unverified bastion key
	// exposes the credential for every host reached through it, not just its
	// own.
	KnownBastionKey string
	// RequireKnownHostKey refuses a host whose key has not been recorded,
	// rather than accepting it once and pinning it. This is the difference
	// between the 'strict' and 'pinned' host key policies.
	RequireKnownHostKey bool
	Timeout             time.Duration
}

// SSHTransport runs commands over SSH.
type SSHTransport struct {
	client     *ssh.Client
	bastion    *ssh.Client
	hostKey    string
	bastionKey string
}

// DialSSH opens a connection, pinning the host key.
func DialSSH(ctx context.Context, cfg SSHConfig, reg *secret.Registry) (*SSHTransport, error) {
	if cfg.Timeout <= 0 {
		cfg.Timeout = 15 * time.Second
	}
	if cfg.Port == 0 {
		cfg.Port = 22
	}

	auth, cleanup, err := sshAuth(ctx, cfg.CredentialRef, reg)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	t := &SSHTransport{}
	clientCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auth,
		Timeout:         cfg.Timeout,
		HostKeyCallback: pinningCallback(cfg.KnownHostKey, &t.hostKey, cfg.RequireKnownHostKey),
	}

	addr := net.JoinHostPort(cfg.Address, strconv.Itoa(cfg.Port))

	var conn net.Conn
	if cfg.Bastion != "" {
		b, err := dialBastion(ctx, cfg.Bastion, reg, cfg.KnownBastionKey, &t.bastionKey, cfg.RequireKnownHostKey)
		if err != nil {
			return nil, fmt.Errorf("bastion: %w", err)
		}
		t.bastion = b
		conn, err = b.DialContext(ctx, "tcp", addr)
		if err != nil {
			b.Close()
			return nil, fmt.Errorf("dial %s through bastion: %w", addr, err)
		}
	} else {
		d := net.Dialer{Timeout: cfg.Timeout}
		conn, err = d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, err
		}
	}

	c, chans, reqs, err := ssh.NewClientConn(conn, addr, clientCfg)
	if err != nil {
		conn.Close()
		if t.bastion != nil {
			t.bastion.Close()
		}
		return nil, err
	}
	t.client = ssh.NewClient(c, chans, reqs)
	return t, nil
}

// pinningCallback records the key a host presented and refuses a key that
// differs from the one already recorded.
func pinningCallback(known string, captured *string, require bool) ssh.HostKeyCallback {
	return func(hostname string, remote net.Addr, key ssh.PublicKey) error {
		presented := string(bytes.TrimSpace(ssh.MarshalAuthorizedKey(key)))
		*captured = presented
		if known == "" {
			if require {
				return fmt.Errorf("%w: %s presented %s", ErrHostKeyUnknown, hostname, key.Type())
			}
			return nil
		}
		if presented != known {
			return fmt.Errorf("%w: %s presented %s %s. If the host was rebuilt, compare that with its own key (ssh-keygen -lf /etc/ssh/ssh_host_*_key.pub on the host), then clear the old one with: hearsay target -host <name> -forget-host-key",
				ErrHostKeyChanged, hostname, key.Type(), ssh.FingerprintSHA256(key))
		}
		return nil
	}
}

func sshAuth(ctx context.Context, ref string, reg *secret.Registry) ([]ssh.AuthMethod, func(), error) {
	noop := func() {}
	if ref == "" {
		return nil, noop, errors.New("no credential reference configured for this host")
	}
	s, err := reg.Resolve(ctx, ref)
	if err != nil {
		return nil, noop, err
	}
	switch s.Kind {
	case secret.KindAgent:
		conn, err := dialAgent(s.Socket)
		if err != nil {
			return nil, noop, err
		}
		ag := agent.NewClient(conn)
		return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}, func() { conn.Close() }, nil
	case secret.KindPrivateKey:
		signer, err := ssh.ParsePrivateKey(s.Bytes)
		s.Zero()
		if err != nil {
			return nil, noop, fmt.Errorf("parse private key: %w", err)
		}
		return []ssh.AuthMethod{ssh.PublicKeys(signer)}, noop, nil
	case secret.KindPassword:
		pw := string(s.Bytes)
		s.Zero()
		return []ssh.AuthMethod{ssh.Password(pw)}, noop, nil
	default:
		return nil, noop, fmt.Errorf("credential %s cannot be used for ssh", ref)
	}
}

// dialBastion parses "user@host:port/credential-ref" and connects, pinning the
// jump host's key under the same rules as a target.
func dialBastion(ctx context.Context, spec string, reg *secret.Registry, known string, captured *string, require bool) (*ssh.Client, error) {
	userHost, credRef, ok := cutLast(spec, "/")
	if !ok {
		return nil, errors.New("bastion should be user@host:port/credential-ref")
	}
	user, hostPort, ok := cutLast(userHost, "@")
	if !ok {
		return nil, errors.New("bastion needs a user")
	}
	host, portStr, err := net.SplitHostPort(hostPort)
	if err != nil {
		host, portStr = hostPort, "22"
	}
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		port = 22
	}
	auth, cleanup, err := sshAuth(ctx, credRef, reg)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", net.JoinHostPort(host, portStr))
	if err != nil {
		return nil, err
	}
	cc, chans, reqs, err := ssh.NewClientConn(conn, net.JoinHostPort(host, portStr), &ssh.ClientConfig{
		User:            user,
		Auth:            auth,
		Timeout:         15 * time.Second,
		HostKeyCallback: pinningCallback(known, captured, require),
	})
	if err != nil {
		conn.Close()
		return nil, err
	}
	return ssh.NewClient(cc, chans, reqs), nil
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}
	return s, "", false
}

// Run executes one command.
func (t *SSHTransport) Run(ctx context.Context, cmd string) (Result, error) {
	sess, err := t.client.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer sess.Close()

	stdout, err := sess.StdoutPipe()
	if err != nil {
		return Result{}, err
	}
	stderr, err := sess.StderrPipe()
	if err != nil {
		return Result{}, err
	}
	if err := sess.Start(cmd); err != nil {
		return Result{}, err
	}

	type readOut struct {
		out, errOut string
		truncated   bool
	}
	done := make(chan readOut, 1)
	go func() {
		// Both streams are drained at once. Reading stdout to the end before
		// touching stderr is the documented way to wedge a remote command
		// that writes enough to fill the stderr window.
		var (
			wg   sync.WaitGroup
			o, e string
			tr   bool
		)
		wg.Add(2)
		go func() { defer wg.Done(); o, tr, _ = readLimited(stdout) }()
		go func() { defer wg.Done(); e, _, _ = readLimited(stderr) }()
		wg.Wait()
		done <- readOut{o, e, tr}
	}()

	var res Result
	select {
	case <-ctx.Done():
		// Signal the remote side, but do not claim the command stopped: the
		// host may still be running it, which is why nothing retries here.
		_ = sess.Signal(ssh.SIGKILL)
		return Result{}, ctx.Err()
	case r := <-done:
		res.Stdout, res.Stderr, res.Truncated = r.out, r.errOut, r.truncated
	}

	// Wait blocks until the far end reports an exit status, which a wedged
	// host may never do. The deadline has to reach it as well, or one host
	// holds a slot in the run for ever.
	waited := make(chan error, 1)
	go func() { waited <- sess.Wait() }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		return Result{}, ctx.Err()
	case err := <-waited:
		if err != nil {
			var ee *ssh.ExitError
			if errors.As(err, &ee) {
				res.ExitCode = ee.ExitStatus()
				return res, nil
			}
			return res, err
		}
	}
	return res, nil
}

// HostKey returns the key the host presented on this connection.
func (t *SSHTransport) HostKey() string { return t.hostKey }

// BastionHostKey returns the key the jump host presented, empty when the
// connection was direct.
func (t *SSHTransport) BastionHostKey() string { return t.bastionKey }

// Close shuts the connection and the bastion behind it.
func (t *SSHTransport) Close() error {
	var err error
	if t.client != nil {
		err = t.client.Close()
	}
	if t.bastion != nil {
		t.bastion.Close()
	}
	return err
}
