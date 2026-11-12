package web

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/KalebHawkins/kryostate/internal/engine"
	"github.com/KalebHawkins/kryostate/internal/store"
)

// TestCollectNowRespectsAnotherProcess: while a kryostate refresh from the
// command line holds the collection lease, the Runs page says so and Collect
// now does not start a second wave.
func TestCollectNowRespectsAnotherProcess(t *testing.T) {
	ctx := context.Background()
	_, h, st := testServer(t)
	var file string
	if err := st.DB().QueryRow(`SELECT file FROM pragma_database_list WHERE name = 'main'`).Scan(&file); err != nil {
		t.Fatal(err)
	}
	other, err := store.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	release, err := engine.NewCollectionLock(other).TryAcquire(ctx,
		engine.LeaseInfo{Purpose: "kryostate refresh by kim", Origin: "cli", Actor: "kim"})
	if err != nil {
		t.Fatal(err)
	}

	body := get(t, h, "/runs", nil).Body.String()
	if !strings.Contains(body, "A collection started from the command line by kim") {
		t.Errorf("the Runs page does not say a command-line collection is running")
	}
	rec := post(t, h, "/collect", nil)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "command line by kim") {
		t.Errorf("Collect now: %d %s", rec.Code, rec.Body.String())
	}
	runs, _ := st.Runs(ctx, 5)
	if len(runs) != 0 {
		t.Errorf("a run was started: %+v", runs)
	}

	release()
	if body := get(t, h, "/runs", nil).Body.String(); strings.Contains(body, "command line by kim") {
		t.Error("the Runs page still reports the finished collection")
	}
	rec = post(t, h, "/collect", nil)
	if rec.Code != http.StatusAccepted {
		t.Errorf("Collect now after release: %d %s", rec.Code, rec.Body.String())
	}
	// Let the background run finish before the store closes.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, ok, _ := other.CurrentLease(ctx, time.Now()); !ok {
			if runs, _ := st.Runs(ctx, 1); len(runs) == 1 && runs[0].Status != "running" {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Error("the web run did not finish and release the lease")
}
