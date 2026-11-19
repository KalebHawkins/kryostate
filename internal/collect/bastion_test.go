package collect

import "testing"

func TestParseBastion(t *testing.T) {
	for _, c := range []struct {
		spec, user, host string
		port             int
		ref              string
	}{
		{"ops@jump.example.com:22/keyring:kryostate/ops", "ops", "jump.example.com", 22, "keyring:kryostate/ops"},
		{"ops@jump.example.com:2222/file:/etc/kryostate/jump.key", "ops", "jump.example.com", 2222, "file:/etc/kryostate/jump.key"},
		{"ops@jump.example.com/agent:", "ops", "jump.example.com", 22, "agent:"},
		{"ops@[2001:db8::1]:22/exec:/usr/local/bin/key", "ops", "2001:db8::1", 22, "exec:/usr/local/bin/key"},
	} {
		user, host, port, ref, err := ParseBastion(c.spec)
		if err != nil || user != c.user || host != c.host || port != c.port || ref != c.ref {
			t.Errorf("%q: got %q %q %d %q %v", c.spec, user, host, port, ref, err)
		}
	}
	for _, bad := range []string{"ops@jump.example.com", "jump.example.com/agent:", "@jump.example.com/agent:", "ops@/agent:"} {
		if _, _, _, _, err := ParseBastion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
