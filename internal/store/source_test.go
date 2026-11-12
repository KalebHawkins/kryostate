package store

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/KalebHawkins/hearsay/internal/model"
)

// TestSourceIDUnderConcurrency is the first collection on a new database:
// every worker asks for the same, not yet existing, source at once.
func TestSourceIDUnderConcurrency(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "race.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	ctx := context.Background()
	const n = 16
	ids := make([]int64, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			ids[i], errs[i] = st.SourceID(ctx, model.SourceCollector, "race")
		}(i)
	}
	close(start)
	wg.Wait()
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("worker %d: %v", i, errs[i])
		}
		if ids[i] == 0 || ids[i] != ids[0] {
			t.Fatalf("workers disagree on the id: %v", ids)
		}
	}
}
