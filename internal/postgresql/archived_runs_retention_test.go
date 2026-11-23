package postgresql

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestRunArchivedRunsRetention_SweepsNowAndOnEachTickAndCounts(t *testing.T) {
	var mu sync.Mutex
	var cutoffs []time.Time
	reduce := func(_ context.Context, cutoff time.Time) (int64, error) {
		mu.Lock()
		defer mu.Unlock()
		cutoffs = append(cutoffs, cutoff)
		if len(cutoffs) == 1 {
			return 0, errors.New("transient") // a failed sweep must not stop the loop
		}
		return 3, nil
	}
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	before := testutil.ToFloat64(archivedRunsSwept)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runArchivedRunsRetention(ctx, reduce, func() time.Time { return now }, 30*24*time.Hour, 10*time.Millisecond)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(cutoffs)
		mu.Unlock()
		if n >= 3 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(cutoffs) < 3 {
		t.Fatalf("swept %d times, want an immediate run plus ticks", len(cutoffs))
	}
	if want := now.Add(-30 * 24 * time.Hour); !cutoffs[0].Equal(want) {
		t.Errorf("cutoff = %v, want %v", cutoffs[0], want)
	}
	// Each successful sweep after the failed first one counted its 3 rows.
	if got, want := testutil.ToFloat64(archivedRunsSwept)-before, float64(3*(len(cutoffs)-1)); got != want {
		t.Errorf("swept counter rose by %v, want %v", got, want)
	}
}

func TestRunArchivedRunsRetention_ZeroKeepsEverything(t *testing.T) {
	reduce := func(context.Context, time.Time) (int64, error) {
		t.Error("swept although retention is 0")
		return 0, nil
	}
	if err := runArchivedRunsRetention(context.Background(), reduce, time.Now, 0, time.Millisecond); err != nil {
		t.Fatal(err)
	}
}
