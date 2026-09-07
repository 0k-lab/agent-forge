package store

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestControlTaskRunsIterationCancellationReleasesConnection(t *testing.T) {
	s := testStore(t)
	for _, source := range []string{"https://github.com/org/repo/issues/1", "https://github.com/org/repo/issues/2"} {
		if _, err := s.CreateJobWithSource("task", source); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	calls := 0
	refs, truncated, err := s.ControlTaskRuns(ctx, func(project, source string) bool { calls++; cancel(); return false })
	if !errors.Is(err, context.Canceled) || len(refs) != 0 || truncated || calls == 0 {
		t.Fatalf("refs=%v truncated=%v err=%v calls=%d", refs, truncated, err, calls)
	}
	// A cancelled query must release the sole connection, including its error path.
	check, done := context.WithTimeout(context.Background(), time.Second)
	defer done()
	refs, truncated, err = s.ControlTaskRuns(check, func(string, string) bool { return true })
	if err != nil || len(refs) != 2 || truncated {
		t.Fatalf("after cancellation: %v %v %v", refs, truncated, err)
	}
	_, _, err = s.ControlTaskRuns(ctx, func(string, string) bool { t.Fatal("cancelled query iterated"); return true })
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled query: %v", err)
	}
}
