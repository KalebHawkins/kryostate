package main

import (
	"context"
	"path/filepath"
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

	if err := execute(t, "host", "update", "--db", db, "web01", "env=prod", "--reason", "r"); err == nil ||
		!strings.Contains(err.Error(), "cannot be set by hand") {
		t.Errorf("env was accepted: %v", err)
	}
	if err := execute(t, "set", "--db", db, "web01", "application=ledger", "stack=pay", "role=app", "--reason", "r"); err != nil {
		t.Fatal(err)
	}
	// A role alone goes to the one stack the host is in. The reason is optional.
	if err := execute(t, "host", "update", "web01", "role=api", "--db", db); err != nil {
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

// TestStackAddsHostsByQuery: stack add -q adds what a query matches, once,
// in the --role role, and leaves members alone.
func TestStackAddsHostsByQuery(t *testing.T) {
	ctx := context.Background()
	db := filepath.Join(t.TempDir(), "k.db")
	st, err := openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]string{}
	for _, n := range []string{"web01", "web02", "db01"} {
		id, _, err := st.ResolveCI(ctx, model.KindHost, n, []model.Identity{{Scheme: model.IdentHostname, Value: n}})
		if err != nil {
			t.Fatal(err)
		}
		ids[n] = id
	}
	st.Close()

	if err := execute(t, "stack", "add", "--db", db, "pay", "web01:api"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "stack", "add", "--db", db, "pay", "-q", `name in ("web01", "web02")`, "--role", "app"); err != nil {
		t.Fatal(err)
	}
	if err := execute(t, "stack", "add", "--db", db, "pay", "--query", `name ==`); err == nil || !strings.Contains(err.Error(), "query") {
		t.Errorf("a bad query gave %v", err)
	}

	st, err = openStore(db)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	roles, err := st.StackRoles(ctx, "pay")
	if err != nil {
		t.Fatal(err)
	}
	if len(roles) != 2 || roles[ids["web01"]] != "api" || roles[ids["web02"]] != "app" {
		t.Errorf("roles %v", roles)
	}
}
