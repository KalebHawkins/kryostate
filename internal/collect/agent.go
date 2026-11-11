package collect

import (
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"strings"
)

// dialAgent connects to a running SSH agent.
//
// Using an agent is the preferred arrangement: the private key stays in the
// agent, this process asks it to sign, and a copy of the inventory database is
// worth nothing to an attacker who cannot also reach the agent.
//
// On Windows the agent listens on a named pipe, which the net package will not
// dial; a named pipe does open as a file, and the agent protocol only needs a
// reader and a writer.
func dialAgent(sock string) (io.ReadWriteCloser, error) {
	if sock == "" {
		return nil, fmt.Errorf("no agent address")
	}
	if runtime.GOOS == "windows" || strings.HasPrefix(sock, `\\.\pipe\`) {
		f, err := os.OpenFile(sock, os.O_RDWR, 0)
		if err != nil {
			return nil, fmt.Errorf("open agent pipe %s: %w", sock, err)
		}
		return f, nil
	}
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, fmt.Errorf("dial agent %s: %w", sock, err)
	}
	return c, nil
}
