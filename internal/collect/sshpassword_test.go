package collect

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"strconv"
	"sync/atomic"
	"testing"

	"golang.org/x/crypto/ssh"

	"github.com/KalebHawkins/kryostate/internal/secret"
)

// passwordServer accepts the password "right" by the methods it is given and
// counts every password it is offered, by either method.
func passwordServer(t *testing.T, password, interactive bool) (string, int, *atomic.Int32) {
	t.Helper()
	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(key)
	if err != nil {
		t.Fatal(err)
	}
	var offered atomic.Int32
	check := func(pw string) error {
		offered.Add(1)
		if pw != "right" {
			return errors.New("wrong password")
		}
		return nil
	}
	cfg := &ssh.ServerConfig{}
	if password {
		cfg.PasswordCallback = func(_ ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			return nil, check(string(pw))
		}
	}
	if interactive {
		cfg.KeyboardInteractiveCallback = func(_ ssh.ConnMetadata, ask ssh.KeyboardInteractiveChallenge) (*ssh.Permissions, error) {
			ans, err := ask("", "", []string{"Password: "}, []bool{false})
			if err != nil {
				return nil, err
			}
			return nil, check(ans[0])
		}
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for ch := range chans {
					ch.Reject(ssh.Prohibited, "test server")
				}
			}()
		}
	}()
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p, &offered
}

func dialWithPassword(t *testing.T, host string, port int, pw string) error {
	t.Helper()
	t.Setenv("KRYOSTATE_TEST_PW", pw)
	tr, err := DialSSH(context.Background(), SSHConfig{Address: host, Port: port, User: "ops",
		CredentialRef: "env:KRYOSTATE_TEST_PW"}, secret.NewRegistry(nil))
	if err == nil {
		tr.Close()
	}
	return err
}

// TestWrongPasswordOfferedOnce: a server that takes a password by both
// methods is sent a wrong one once, not once per method, because each
// refusal counts towards locking the account.
func TestWrongPasswordOfferedOnce(t *testing.T) {
	host, port, offered := passwordServer(t, true, true)
	if err := dialWithPassword(t, host, port, "wrong"); err == nil {
		t.Fatal("a wrong password logged in")
	}
	if n := offered.Load(); n != 1 {
		t.Fatalf("password offered %d times, want 1", n)
	}
}

// TestPasswordByKeyboardInteractive: a host that accepts passwords only at
// a keyboard-interactive prompt, as PAM-configured hosts often do, is logged
// in to.
func TestPasswordByKeyboardInteractive(t *testing.T) {
	host, port, offered := passwordServer(t, false, true)
	if err := dialWithPassword(t, host, port, "right"); err != nil {
		t.Fatal(err)
	}
	if n := offered.Load(); n != 1 {
		t.Fatalf("password offered %d times, want 1", n)
	}
}
