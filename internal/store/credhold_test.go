package store

import (
	"context"
	"testing"
)

// TestCredentialHoldCountsThenHolds: failures in a row are counted per
// reference and user, the credential is held at the limit, and the hold is
// seen by another process sharing the file. Clearing it starts again.
func TestCredentialHoldCountsThenHolds(t *testing.T) {
	ctx := context.Background()
	a, b := twoHandles(t)
	const ref = "keyring:kryostate/ops"

	h, err := a.NoteCredentialFailure(ctx, ref, "ops", "node-1", "unable to authenticate", 2)
	if err != nil || h.Fails != 1 || h.Held {
		t.Fatalf("first failure: %+v %v", h, err)
	}
	if h, _ := a.NoteCredentialFailure(ctx, ref, "other", "node-1", "x", 2); h.Fails != 1 {
		t.Fatalf("another user shares the count: %+v", h)
	}
	h, err = a.NoteCredentialFailure(ctx, ref, "ops", "node-2", "unable to authenticate", 2)
	if err != nil || h.Fails != 2 || !h.Held || h.LastHost != "node-2" {
		t.Fatalf("second failure: %+v %v", h, err)
	}
	if got, ok, err := b.CredentialHoldFor(ctx, ref, "ops"); err != nil || !ok || !got.Held {
		t.Fatalf("other handle: %+v %v %v", got, ok, err)
	}

	if n, err := b.ClearCredentialHold(ctx, ref, ""); err != nil || n != 2 {
		t.Fatalf("clear for every user: %d %v", n, err)
	}
	if _, ok, _ := a.CredentialHoldFor(ctx, ref, "ops"); ok {
		t.Fatal("hold survived clearing")
	}
	if h, _ := a.NoteCredentialFailure(ctx, ref, "ops", "node-3", "x", 1); !h.Held {
		t.Fatal("a limit of one did not hold on the first failure")
	}
}
