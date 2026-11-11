package secret

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
)

// The agent address can come from three places, and Windows OpenSSH supplies
// none of the usual ones.
func TestAgentResolverAddress(t *testing.T) {
	ctx := context.Background()

	t.Run("explicit value wins", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "/tmp/from-env")
		got, err := agentResolver{}.Resolve(ctx, "/tmp/explicit")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Kind != KindAgent {
			t.Errorf("kind = %q, want %q", got.Kind, KindAgent)
		}
		if got.Socket != "/tmp/explicit" {
			t.Errorf("socket = %q, want the explicit value", got.Socket)
		}
	})

	t.Run("environment supplies the address", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "/tmp/from-env")
		got, err := agentResolver{}.Resolve(ctx, "")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if got.Socket != "/tmp/from-env" {
			t.Errorf("socket = %q, want the environment value", got.Socket)
		}
	})

	// Windows OpenSSH sets no variable, so a bare "agent:" must still name
	// the pipe that dialAgent knows how to open, and must say so plainly
	// when no agent is listening there. Elsewhere, an empty variable means
	// there is no agent to talk to.
	t.Run("no environment", func(t *testing.T) {
		t.Setenv("SSH_AUTH_SOCK", "")
		got, err := agentResolver{}.Resolve(ctx, "")
		if runtime.GOOS == "windows" && agentPipePresent(windowsAgentPipe) {
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Socket != windowsAgentPipe {
				t.Errorf("socket = %q, want %q", got.Socket, windowsAgentPipe)
			}
			return
		}
		if !errors.Is(err, ErrNotFound) {
			t.Fatalf("err = %v, want ErrNotFound", err)
		}
		if runtime.GOOS == "windows" && !strings.Contains(err.Error(), windowsAgentPipe) {
			t.Errorf("err = %v, want it to name %s", err, windowsAgentPipe)
		}
	})
}
