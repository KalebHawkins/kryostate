package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/KalebHawkins/kryostate/internal/collect"
)

func TestUserCollectorRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := testStore(t)
	sp := collect.Spec{ID: "mine.kernel", Attr: "custom.kernel", OS: []string{"linux"}, Command: "uname -r",
		Parse: collect.Parse{Kind: collect.ParseFirstLine}, Where: `name == "web01"`}

	if updated, err := st.PutUserCollector(ctx, sp, "ops"); err != nil || updated {
		t.Fatalf("first put: updated=%v err=%v", updated, err)
	}
	sp.Command = "uname -rv"
	if updated, err := st.PutUserCollector(ctx, sp, "svc-ro"); err != nil || !updated {
		t.Fatalf("second put: updated=%v err=%v", updated, err)
	}
	all, err := st.UserCollectors(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("got %d, %v", len(all), err)
	}
	u := all[0]
	if u.Spec.Command != "uname -rv" || u.Spec.Where != sp.Where || u.AddedBy != "ops" || u.UpdatedBy != "svc-ro" {
		t.Errorf("read back %+v", u)
	}
	if u.AddedAt.IsZero() || u.UpdatedAt.Before(u.AddedAt) {
		t.Errorf("times: %v %v", u.AddedAt, u.UpdatedAt)
	}

	if _, err := st.PutUserCollector(ctx, collect.Spec{ID: "bad", Attr: "x", Command: "true"}, "ops"); err == nil {
		t.Error("a spec without os was stored")
	}

	if err := st.DeleteUserCollector(ctx, "mine.kernel", "ops"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteUserCollector(ctx, "mine.kernel", "ops"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("deleting twice: %v", err)
	}
	if all, _ := st.UserCollectors(ctx); len(all) != 0 {
		t.Errorf("%d left after delete", len(all))
	}
	var n int
	st.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM audit WHERE subject='mine.kernel'
		AND action IN ('collector added', 'collector updated', 'collector removed')`).Scan(&n)
	if n != 3 {
		t.Errorf("%d audit rows, want 3", n)
	}
}
