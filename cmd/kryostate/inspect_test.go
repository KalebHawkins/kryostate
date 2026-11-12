package main

import (
	"context"
	"flag"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/model"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestSetAcceptsOnlyApplicationStackAndRole: everything else about a host is
// what the host reports, so set refuses it before touching the store.
func TestSetAcceptsOnlyApplicationStackAndRole(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	st, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	id, _, err := st.ResolveCI(ctx, model.KindHost, "web01",
		[]model.Identity{{Scheme: model.IdentHostname, Value: "web01"}})
	if err != nil {
		t.Fatal(err)
	}
	st.Close()

	if err := cmdSet(ctx, []string{"-db", db, "web01", "env=prod", "-reason", "r"}); err == nil ||
		!strings.Contains(err.Error(), "cannot be set by hand") {
		t.Errorf("env was accepted: %v", err)
	}
	if err := cmdSet(ctx, []string{"-db", db, "web01", "application=ledger", "stack=pay", "role=app", "-reason", "r"}); err != nil {
		t.Fatal(err)
	}
	// A role alone goes to the one stack the host is in. The reason is optional.
	if err := cmdSet(ctx, []string{"-db", db, "web01", "role=api"}); err != nil {
		t.Fatal(err)
	}

	st, err = openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ms, err := st.MembershipsOf(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) != 1 || ms[0].Stack != "pay" || ms[0].Role != "api" {
		t.Errorf("memberships %+v", ms)
	}
	hosts, err := st.Hosts(ctx, store.HostFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(hosts) != 1 || hosts[0].Values[model.AttrApplication] != "ledger" {
		t.Errorf("hosts %+v", hosts)
	}
}

func TestFlagsMayFollowTheHost(t *testing.T) {
	for _, args := range [][]string{
		{"web01", "-json", "-reason", "why", "env=prod"},
		{"-json", "web01", "-reason", "why", "env=prod"},
		{"-json", "-reason", "why", "web01", "env=prod"},
	} {
		fs := flag.NewFlagSet("t", flag.ContinueOnError)
		asJSON := fs.Bool("json", false, "")
		reason := fs.String("reason", "", "")
		pos := parseInterspersed(fs, args)
		if !*asJSON || *reason != "why" || !slices.Equal(pos, []string{"web01", "env=prod"}) {
			t.Errorf("%v: json=%v reason=%q pos=%v", args, *asJSON, *reason, pos)
		}
	}
}
