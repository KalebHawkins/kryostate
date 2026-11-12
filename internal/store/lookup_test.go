package store

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/model"
)

// TestLookupFindsAHostByWhatAPersonTypes covers each way a person names a
// host on the command line.
func TestLookupFindsAHostByWhatAPersonTypes(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	id, _, err := s.ResolveCI(ctx, model.KindHost, "web01", []model.Identity{
		{Scheme: model.IdentFQDN, Value: "web01.example.invalid"},
		{Scheme: model.IdentHostname, Value: "web01"},
	})
	if err != nil {
		t.Fatal(err)
	}
	mustHost(t, s, "db01")

	for _, ref := range []string{id, id[:6], strings.ToUpper(id[:8]), "web01", "WEB01", "web01.example.invalid"} {
		got, err := s.LookupCI(ctx, ref)
		if err != nil {
			t.Errorf("%q: %v", ref, err)
			continue
		}
		if got != id {
			t.Errorf("%q found %s, want %s", ref, got, id)
		}
	}
}

// TestLookupNeverGuessesBetweenTwoHosts: a hostname reused at two sites must
// produce an error naming both, never one of them chosen silently.
func TestLookupNeverGuessesBetweenTwoHosts(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	a, _, err := s.ResolveCI(ctx, model.KindHost, "app", []model.Identity{
		{Scheme: model.IdentFQDN, Value: "app.site-a.invalid"}})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.ResolveCI(ctx, model.KindHost, "app", []model.Identity{
		{Scheme: model.IdentFQDN, Value: "app.site-b.invalid"}})
	if err != nil {
		t.Fatal(err)
	}

	_, err = s.LookupCI(ctx, "app")
	if !errors.Is(err, ErrAmbiguousIdentity) {
		t.Fatalf("got %v, want an ambiguity error", err)
	}
	for _, id := range []string{a, b} {
		if !strings.Contains(err.Error(), id) {
			t.Errorf("the error does not list %s, so the person cannot pick", id)
		}
	}
	// The stronger evidence still singles each one out.
	if got, err := s.LookupCI(ctx, "app.site-b.invalid"); err != nil || got != b {
		t.Errorf("fqdn lookup: got %s, %v", got, err)
	}
}

func TestLookupOfNothingSuggestsSomething(t *testing.T) {
	s := testStore(t)
	mustHost(t, s, "payments-db01")

	_, err := s.LookupCI(context.Background(), "payments")
	if !errors.Is(err, ErrNoSuchHost) {
		t.Fatalf("got %v, want not found", err)
	}
	if !strings.Contains(err.Error(), "payments-db01") {
		t.Errorf("the error %q does not suggest the near match", err)
	}
	// A short prefix must not be read as an id prefix.
	if _, err := s.LookupCI(context.Background(), "ab"); !errors.Is(err, ErrNoSuchHost) {
		t.Errorf("a two-character reference matched something: %v", err)
	}
}

// TestClaimCompetesUnderPrecedence: a value stated with kryostate set is an
// ordinary human assertion, so it wins env over a collector but not
// os.version, where the machine ranks first.
func TestClaimCompetesUnderPrecedence(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ci := mustHost(t, s, "node-claim")
	col := mustSource(t, s, model.SourceCollector, "ssh")
	assertValue(t, s, ci, "env", "staging", col, time.Now())
	assertValue(t, s, ci, "os.version", "RHEL 9.4", col, time.Now())

	if err := s.Claim(ctx, ci, "env", "", "production", "operator", "change ticket 42"); err != nil {
		t.Fatal(err)
	}
	if err := s.Claim(ctx, ci, "os.version", "", "RHEL 8.6", "operator", "the wiki says so"); err != nil {
		t.Fatal(err)
	}
	if got := factFor(t, s, ci, "env"); got.Raw != "production" || got.SourceKind != model.SourceHuman {
		t.Errorf("env: got %q from %s, want the person's claim", got.Raw, got.SourceKind)
	}
	if got := factFor(t, s, ci, "os.version"); got.Raw != "RHEL 9.4" {
		t.Errorf("os.version: got %q, the machine should still win", got.Raw)
	}
	if err := s.Claim(ctx, ci, " ", "", "x", "operator", "r"); err == nil {
		t.Error("a claim with no attribute was accepted")
	}
}

func TestRemoveFromStackReportsWhetherThereWasAMembership(t *testing.T) {
	s := testStore(t)
	ctx := context.Background()
	ci := mustHost(t, s, "member")
	id, err := s.PutStack(ctx, model.Stack{Name: "payments"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.AddToStack(ctx, id, ci, "db"); err != nil {
		t.Fatal(err)
	}
	if ok, err := s.RemoveFromStack(ctx, id, ci); err != nil || !ok {
		t.Fatalf("first removal: %v, %v", ok, err)
	}
	if ok, err := s.RemoveFromStack(ctx, id, ci); err != nil || ok {
		t.Errorf("second removal reported a membership that was already gone: %v, %v", ok, err)
	}
}
