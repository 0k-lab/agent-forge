package gate

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"agent-forge/internal/store"
)

type fakeSweeper struct {
	times []time.Time
	errAt int
}

func TestStartRecoverySweepsBeforeReturnAndStopsOnCancellation(t *testing.T) {
	s, err := store.Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	job, _ := s.CreateJob("work")
	options := DefaultOptions()
	options.Policy = store.RecoveryPolicy{LeaseTTL: time.Second, BaseRetryBackoff: time.Second, MaxAttempts: 3}
	options.RecoveryInterval = time.Second
	options.Now = func() time.Time { return start.Add(time.Second) }
	if _, ok, err := s.LeaseNextAt("worker", start, options.Policy); err != nil || !ok {
		t.Fatalf("lease: %v %v", ok, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	errs, err := StartRecovery(ctx, s, options)
	if err != nil {
		t.Fatal(err)
	}
	if stored, err := s.Job(job.ID); err != nil || stored.Status != "retry_wait" {
		t.Fatalf("startup sweep = %#v, %v", stored, err)
	}
	cancel()
	select {
	case err := <-errs:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("stop error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("recovery goroutine leaked")
	}
}

func (s *fakeSweeper) SweepExpired(at time.Time, _ store.RecoveryPolicy) error {
	s.times = append(s.times, at)
	if s.errAt > 0 && len(s.times) == s.errAt {
		return errors.New("sweep failed")
	}
	return nil
}

func TestRecoveryRunnerSweepsImmediatelyAndOnTicks(t *testing.T) {
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	ticks := make(chan time.Time, 2)
	ticks <- start.Add(time.Second)
	ticks <- start.Add(2 * time.Second)
	close(ticks)
	s := &fakeSweeper{}
	err := runRecovery(context.Background(), s, store.DefaultRecoveryPolicy(), func() time.Time { return start }, ticks)
	if !errors.Is(err, errRecoveryTickerStopped) || len(s.times) != 3 || !s.times[0].Equal(start) || !s.times[2].Equal(start.Add(2*time.Second)) {
		t.Fatalf("sweeps=%v err=%v", s.times, err)
	}

	s = &fakeSweeper{errAt: 2}
	ticks = make(chan time.Time, 1)
	ticks <- start.Add(time.Second)
	if err := runRecovery(context.Background(), s, store.DefaultRecoveryPolicy(), func() time.Time { return start }, ticks); err == nil || err.Error() != "sweep failed" {
		t.Fatalf("store failure = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := runRecovery(ctx, &fakeSweeper{}, store.DefaultRecoveryPolicy(), func() time.Time { return start }, make(chan time.Time)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation = %v", err)
	}
}

func TestGateOptionsValidateBounds(t *testing.T) {
	options := DefaultOptions()
	if err := options.Validate(); err != nil || options.RecoveryInterval != time.Second || options.LeasePollInterval <= 0 {
		t.Fatalf("defaults=%#v err=%v", options, err)
	}
	options.RecoveryInterval = options.Policy.LeaseTTL + time.Nanosecond
	if err := options.Validate(); err == nil {
		t.Fatal("recovery interval longer than lease accepted")
	}
}
