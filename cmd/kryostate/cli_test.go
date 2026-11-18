package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestMain keeps the tests away from a config file in the user's own
// config folder.
func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "kryostate-cmd-test")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cfg := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(cfg, nil, 0o600); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	os.Setenv("KRYOSTATE_CONFIG", cfg)
	os.Unsetenv("KRYOSTATE_DB")
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

// execute runs one command line as main would.
func execute(_ *testing.T, args ...string) error {
	return executeIn(nil, args...)
}

func executeIn(in io.Reader, args ...string) error {
	root := newRootCmd(nil)
	root.SetArgs(args)
	if in != nil {
		root.SetIn(in)
	}
	return root.ExecuteContext(context.Background())
}

func TestHostDisableEnableAndRm(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	for _, args := range [][]string{
		{"host", "add", "web01", "--fqdn", "web01.example.com"},
		{"host", "add", "web02"},
		{"target", "add", "web01", "web02", "--user", "ops"},
		{"stack", "add", "pay", "web01:app", "web02:app"},
		{"check", "add", "web01", "ssh", "--tcp", "web01:22"},
		{"host", "disable", "web01.example.com", "--reason", "powered off"},
	} {
		if err := execute(t, append(args, "--db", db)...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
	}

	// Disabling keeps everything, and a refresh limited to the host
	// contacts nothing: no run is started at all.
	if err := execute(t, "refresh", "--only", "web01", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "check", "run", "--only", "web01", "--db", db); err != nil {
		t.Fatal(err)
	}
	st, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	id, err := st.LookupCI(ctx, "web01")
	if err != nil {
		t.Fatal(err)
	}
	ps, _ := st.ConnProfiles(ctx, id)
	ms, _ := st.MembershipsOf(ctx, id)
	cs, _ := st.Checks(ctx, id)
	c, _ := st.CI(ctx, id)
	if len(ps) != 1 || !ps[0].Enabled || len(ms) != 1 || len(cs) != 1 || c.DisabledAt == nil || c.DisabledReason != "powered off" {
		t.Errorf("after disable: profiles %v, memberships %v, checks %v, ci %+v", ps, ms, cs, c)
	}
	if runs, _ := st.Runs(ctx, 10); len(runs) != 0 {
		t.Errorf("a refresh of a disabled host started %d runs", len(runs))
	}
	if r, _ := st.Reachability(ctx, id); len(r) != 0 {
		t.Errorf("a disabled host was probed: %v", r)
	}
	st.Close()
	if err := execute(t, "host", "enable", "web01", "--db", db); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	if c, _ := st.CI(ctx, id); c.DisabledAt != nil {
		t.Error("still disabled after enable")
	}
	st.Close()

	// Deleting asks first, and anything but yes leaves the host alone.
	err = executeIn(strings.NewReader("no\n"), "host", "rm", "web01", "--db", db)
	if err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Errorf("unconfirmed rm: %v", err)
	}
	if err := executeIn(strings.NewReader("yes\n"), "host", "rm", "web01", "--db", db, "--reason", "gone"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "host", "rm", "-y", "web02", "--db", db); err != nil {
		t.Fatal(err)
	}
	st, err = openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, h := range []string{"web01", "web02"} {
		if _, err := st.LookupCI(ctx, h); err == nil {
			t.Errorf("%s still there after rm", h)
		}
	}
	var n int
	st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE subject=? AND action IN ('disable host','enable host','delete host')`, id).Scan(&n)
	if n != 3 {
		t.Errorf("%d audit rows for web01's disable, enable and delete, want 3", n)
	}
	if err := execute(t, "host", "rm", "-y", "nope", "--db", db); err == nil {
		t.Error("removing an unknown host succeeded")
	}
}

// TestSettingsPrecedence: a flag beats KRYOSTATE_*, which beats the config
// file, where a setting under the command's name beats one at the top.
func TestSettingsPrecedence(t *testing.T) {
	dir := t.TempDir()
	cfg := filepath.Join(dir, "config.yaml")
	p := func(n string) string { return filepath.Join(dir, n) }
	write := func(s string) {
		if err := os.WriteFile(cfg, []byte(s), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	exists := func(n string) bool { _, err := os.Stat(p(n)); return err == nil }

	write(fmt.Sprintf("db: %q\n", p("top.db")))
	if err := execute(t, "host", "add", "a", "--config", cfg); err != nil || !exists("top.db") {
		t.Fatalf("top-level db: %v", err)
	}
	write(fmt.Sprintf("db: %q\nhost:\n  add:\n    db: %q\n", p("top.db"), p("section.db")))
	if err := execute(t, "host", "add", "a", "--config", cfg); err != nil || !exists("section.db") {
		t.Fatalf("db under the command: %v", err)
	}
	t.Setenv("KRYOSTATE_DB", p("env.db"))
	if err := execute(t, "host", "add", "a", "--config", cfg); err != nil || !exists("env.db") {
		t.Fatalf("env: %v", err)
	}
	if err := execute(t, "host", "add", "a", "--config", cfg, "--db", p("flag.db")); err != nil || !exists("flag.db") {
		t.Fatalf("flag: %v", err)
	}
	t.Setenv("KRYOSTATE_CONCURRENCY", "many")
	if err := execute(t, "refresh", "--config", cfg); err == nil || !strings.Contains(err.Error(), "KRYOSTATE_CONCURRENCY") {
		t.Errorf("a bad value from the environment: %v", err)
	}
	if err := execute(t, "host", "list", "--config", p("missing.yaml")); err == nil {
		t.Error("a missing --config file was ignored")
	}
}

// TestTargetUpdateChangesOnlyWhatIsGiven: target update keeps the rest of
// the profile, the pinned host key included.
func TestTargetUpdateChangesOnlyWhatIsGiven(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	if err := execute(t, "host", "add", "web01", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "target", "add", "web01", "-u", "ops", "-c", "keyring:kryostate/ops", "--db", db); err != nil {
		t.Fatal(err)
	}
	st, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	id, _ := st.LookupCI(ctx, "web01")
	if err := st.PinHostKey(ctx, id, "ssh", "ssh-ed25519 AAAA"); err != nil {
		t.Fatal(err)
	}
	st.Close()
	if err := execute(t, "target", "update", "web01", "--port", "2222", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "target", "update", "web01", "-t", "winrm", "--port", "1", "--db", db); err == nil {
		t.Error("updating a connection the host does not have succeeded")
	}
	st, err = openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ps, _ := st.ConnProfiles(ctx, id)
	want := store.ConnProfile{Port: 2222, Username: "ops", CredentialRef: "keyring:kryostate/ops", HostKey: "ssh-ed25519 AAAA", Address: "web01"}
	if len(ps) != 1 || ps[0].Port != want.Port || ps[0].Username != want.Username ||
		ps[0].CredentialRef != want.CredentialRef || ps[0].HostKey != want.HostKey || ps[0].Address != want.Address {
		t.Errorf("profile %+v", ps)
	}
}

// TestTargetAddReusesWhatTheOthersUse: adding one more host like the others
// needs only its name; naming a kryostate keyring credential names the user;
// and with users that differ, the command asks rather than guesses.
func TestTargetAddReusesWhatTheOthersUse(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	for _, h := range []string{"was1", "was2", "was3", "was4"} {
		if err := execute(t, "host", "add", h, "--db", db); err != nil {
			t.Fatal(err)
		}
	}
	if err := execute(t, "target", "add", "was1", "-u", "ops", "-c", "keyring:kryostate/ops", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "target", "add", "was2", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "target", "add", "was3", "-c", "keyring:kryostate/svc", "--db", db); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "target", "add", "was4", "--db", db); err == nil {
		t.Error("with two users in use, a target was added without --user")
	}
	st, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for h, want := range map[string][2]string{
		"was2": {"ops", "keyring:kryostate/ops"},
		"was3": {"svc", "keyring:kryostate/svc"},
	} {
		id, _ := st.LookupCI(ctx, h)
		ps, _ := st.ConnProfiles(ctx, id)
		if len(ps) != 1 || ps[0].Username != want[0] || ps[0].CredentialRef != want[1] {
			t.Errorf("%s: %+v, want %s with %s", h, ps, want[0], want[1])
		}
	}
}
