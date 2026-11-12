package engine

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/collect"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestRemovedCatalogEntryIsSkipped: a database switched a collector on that a
// later build no longer has in its catalog. Collection, the collector list
// and the plan carry on without it, and the warning is logged once.
func TestRemovedCatalogEntryIsSkipped(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"web01": "linux"})
	var logged bytes.Buffer
	fx.e.Log = slog.New(slog.NewTextHandler(&logged, nil))
	if _, err := fx.st.PutChoice(ctx, collect.Choice{Template: "cpu.cores"}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.DB().ExecContext(ctx,
		`INSERT INTO collector_choice (template, param, attr, target, created_at)
		 VALUES ('vendor.gone.version', '', 'vendor.gone.version', '', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	// So is a schedule someone set for it.
	if err := fx.st.PutSchedule(ctx, store.Schedule{Collector: "catalog.vendor.gone.version", Every: time.Hour}, "test"); err != nil {
		t.Fatal(err)
	}

	if _, err := fx.e.Collect(ctx, Options{HostTimeout: 10 * time.Second}, nil); err != nil {
		t.Fatal(err)
	}
	if !hasCommand(fx.f.commands("web01"), "_NPROCESSORS_ONLN") {
		t.Errorf("cpu.cores did not run beside the removed entry: %v", fx.f.commands("web01"))
	}
	cs, err := fx.e.Collectors(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cs {
		if strings.Contains(c.Key, "vendor.gone") {
			t.Errorf("the removed entry is listed as a collector: %+v", c)
		}
	}
	if _, err := fx.s.Plan(ctx); err != nil {
		t.Fatalf("plan: %v", err)
	}
	if _, err := fx.s.RunDue(ctx); err != nil {
		t.Fatalf("scheduled run: %v", err)
	}
	if n := strings.Count(logged.String(), "vendor.gone.version"); n != 1 {
		t.Errorf("warned %d times, want once:\n%s", n, logged.String())
	}
	if !strings.Contains(logged.String(), "no longer exists") || !strings.Contains(logged.String(), "-disable") {
		t.Errorf("the warning does not say what happened or how to clear it:\n%s", logged.String())
	}
}

func hasCommand(cmds []string, want string) bool {
	for _, c := range cmds {
		if strings.Contains(c, want) {
			return true
		}
	}
	return false
}

// TestForHostSaysWhy: the host view makes the scheduler's decision and keeps
// the reason, for a target that admits the host, one that excludes it, and a
// collector with no command for the host's system.
func TestForHostSaysWhy(t *testing.T) {
	ctx := context.Background()
	fx := newSchedFixture(t, map[string]string{"web01": "linux", "db01": "linux"})
	if _, err := fx.st.PutChoice(ctx, collect.Choice{Template: "nginx.version", Target: `name == "web01"`}, "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.PutChoice(ctx, collect.Choice{Template: "selinux"}, "test"); err != nil {
		t.Fatal(err)
	}
	plan, err := fx.s.Plan(ctx)
	if err != nil {
		t.Fatal(err)
	}
	find := func(hs []OnHost, key string) OnHost {
		for _, h := range hs {
			if h.Key == key {
				return h
			}
		}
		t.Fatalf("no %s in %d collectors", key, len(hs))
		return OnHost{}
	}

	fam, web, err := fx.e.ForHost(ctx, fx.ids["web01"], plan)
	if err != nil || fam != "linux" {
		t.Fatalf("family %q, %v", fam, err)
	}
	if h := find(web, "catalog.nginx.version"); !h.Runs || h.Target != `name == "web01"` {
		t.Errorf("nginx on web01: %+v", h.Fit)
	}
	if h := find(web, "builtin.os"); !h.Runs || h.Target != "" {
		t.Errorf("built in on web01: %+v", h.Fit)
	}
	_, db, _ := fx.e.ForHost(ctx, fx.ids["db01"], plan)
	if h := find(db, "catalog.nginx.version"); h.Runs || h.NoSystem || h.Target != `name == "web01"` {
		t.Errorf("nginx on db01: %+v", h.Fit)
	}
	// Agrees with the rule a scheduled run uses.
	for _, h := range db {
		if h.Runs != h.appliesTo("linux", fx.ids["db01"], nil) && h.Target == "" {
			t.Errorf("%s: host view and run disagree", h.Key)
		}
	}

	// selinux has a command for Linux only. A host reached over WinRM that
	// has not reported its system is taken to be Windows.
	win := host(t, fx.st, "win01")
	if err := fx.st.PutConnProfile(ctx, store.ConnProfile{CIID: win, Transport: "winrm", Address: "win01",
		Port: 5986, Username: "u", CredentialRef: "agent:", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	fam, db, _ = fx.e.ForHost(ctx, win, plan)
	if fam != "windows" {
		t.Fatalf("a WinRM host with no system reported is taken as %q", fam)
	}
	if h := find(db, "catalog.selinux"); h.Runs || !h.NoSystem {
		t.Errorf("selinux on a Windows host: %+v", h.Fit)
	}
}
