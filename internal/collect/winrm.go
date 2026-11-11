package collect

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/masterzen/winrm"

	"github.com/KalebHawkins/hearsay/internal/secret"
)

// WinRMConfig describes a Windows connection. Commands are PowerShell, so the
// collector specs for Windows are written as PowerShell rather than as cmd.
type WinRMConfig struct {
	Address       string
	Port          int
	User          string
	CredentialRef string
	// HTTPS should stay on. Turning it off sends the credential over the
	// network in a form a listener can use; this tool offers the switch for
	// isolated lab work and does not pretend it is a configuration choice.
	HTTPS bool
	// Insecure skips certificate verification. It is accepted only together
	// with an explicit acknowledgement, because an unverified peer is exactly
	// the condition in which handing over a credential is a mistake.
	Insecure bool
	Timeout  time.Duration
}

// WinRMTransport runs PowerShell over WinRM.
type WinRMTransport struct {
	client *winrm.Client
}

// DialWinRM prepares a WinRM client. WinRM has no persistent connection to
// establish here; a failure to authenticate surfaces on the first command.
func DialWinRM(ctx context.Context, cfg WinRMConfig, reg *secret.Registry) (*WinRMTransport, error) {
	if cfg.Port == 0 {
		if cfg.HTTPS {
			cfg.Port = 5986
		} else {
			cfg.Port = 5985
		}
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.Insecure && !cfg.HTTPS {
		return nil, errors.New("refusing to skip verification on a plaintext connection")
	}
	if cfg.CredentialRef == "" {
		return nil, errors.New("no credential reference configured for this host")
	}

	s, err := reg.Resolve(ctx, cfg.CredentialRef)
	if err != nil {
		return nil, err
	}
	if s.Kind != secret.KindPassword {
		s.Zero()
		return nil, fmt.Errorf("winrm needs a password credential, got %s", s.Kind)
	}
	password := string(s.Bytes)
	s.Zero()

	endpoint := winrm.NewEndpoint(cfg.Address, cfg.Port, cfg.HTTPS, cfg.Insecure, nil, nil, nil, cfg.Timeout)
	c, err := winrm.NewClient(endpoint, cfg.User, password)
	if err != nil {
		return nil, err
	}
	return &WinRMTransport{client: c}, nil
}

// Run executes a PowerShell command.
func (t *WinRMTransport) Run(ctx context.Context, cmd string) (Result, error) {
	var stdout, stderr boundedBuffer
	out, errOut, code, err := t.client.RunWithContextWithString(ctx, winrm.Powershell(cmd), "")
	if err != nil {
		return Result{}, err
	}
	stdout.WriteString(out)
	stderr.WriteString(errOut)
	return Result{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		ExitCode:  code,
		Truncated: stdout.truncated,
	}, nil
}

// HostKey has no meaning for WinRM; peer identity comes from TLS.
func (t *WinRMTransport) HostKey() string { return "" }

// Close releases the client. WinRM holds no long-lived connection.
func (t *WinRMTransport) Close() error { return nil }

// boundedBuffer discards anything past MaxOutput and remembers that it did.
type boundedBuffer struct {
	b         []byte
	truncated bool
}

func (w *boundedBuffer) WriteString(s string) {
	room := MaxOutput - len(w.b)
	if room <= 0 {
		w.truncated = true
		return
	}
	if len(s) > room {
		s = s[:room]
		w.truncated = true
	}
	w.b = append(w.b, s...)
}

func (w *boundedBuffer) String() string { return string(w.b) }
