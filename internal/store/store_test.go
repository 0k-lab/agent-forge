package store

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func TestOpenPersistsOneValidDebugCursorSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	const opens = 8
	secrets := make(chan []byte, opens)
	errs := make(chan error, opens)
	var wg sync.WaitGroup
	for range opens {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s, err := Open(path)
			if err != nil {
				errs <- err
				return
			}
			defer s.Close()
			var secret []byte
			err = s.db.QueryRow(`SELECT value FROM metadata WHERE key='debug_cursor_secret'`).Scan(&secret)
			if err != nil {
				errs <- err
				return
			}
			secrets <- secret
		}()
	}
	wg.Wait()
	close(errs)
	close(secrets)
	for err := range errs {
		t.Fatal(err)
	}
	var first []byte
	for secret := range secrets {
		if len(secret) != 32 {
			t.Fatalf("secret length = %d, want 32", len(secret))
		}
		if first == nil {
			first = secret
		} else if !bytes.Equal(first, secret) {
			t.Fatal("concurrent opens loaded different secrets")
		}
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var reopened []byte
	if err := s.db.QueryRow(`SELECT value FROM metadata WHERE key='debug_cursor_secret'`).Scan(&reopened); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first, reopened) {
		t.Fatal("reopen changed debug cursor secret")
	}
}

func TestOpenRejectsMalformedDebugCursorSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`UPDATE metadata SET value=x'00' WHERE key='debug_cursor_secret'`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if s, err = Open(path); err == nil {
		s.Close()
		t.Fatal("Open accepted malformed debug cursor secret")
	}
}

func TestDebugReadModelsAreBoundedStableAndReadOnly(t *testing.T) {
	s := testStore(t)
	stamp := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC).Format(time.RFC3339Nano)
	ids := make([]string, 102)
	for i := range ids {
		ids[i] = fmt.Sprintf("job-%03d", i)
		if _, err := s.db.Exec(`INSERT INTO jobs(id,input,status,created_at,updated_at) VALUES(?,?,?,?,?)`, ids[i], "secret", "pending", stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	var before int
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&before); err != nil {
		t.Fatal(err)
	}
	first, err := s.RecentDebugJobs(context.Background(), 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 100 || first.NextPosition == nil {
		t.Fatalf("first page = %d items, position %#v", len(first.Items), first.NextPosition)
	}
	sort.Sort(sort.Reverse(sort.StringSlice(ids)))
	for i, job := range first.Items {
		if job.ID != ids[i] {
			t.Fatalf("item %d = %q, want %q", i, job.ID, ids[i])
		}
	}
	second, err := s.RecentDebugJobs(context.Background(), 100, first.NextPosition)
	if err != nil || len(second.Items) != 2 || second.Items[0].ID != ids[100] || second.Items[1].ID != ids[101] {
		t.Fatalf("second page = %#v, %v", second, err)
	}
	var after int
	if err := s.db.QueryRow(`SELECT total_changes()`).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatalf("debug reads mutated store: changes %d -> %d", before, after)
	}
	for i := 0; i < 102; i++ {
		if _, err := s.db.Exec(`INSERT INTO workers(id,connected,last_seen) VALUES(?,?,?)`, fmt.Sprintf("worker-%03d", i), i%2, stamp); err != nil {
			t.Fatal(err)
		}
	}
	workers, err := s.RecentDebugWorkers(context.Background(), 101, nil)
	if err != nil || len(workers.Items) != 100 || workers.NextPosition == nil || workers.Items[0].ID != "worker-101" {
		t.Fatalf("worker page = %#v, %v", workers, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.RecentDebugJobs(ctx, 1, nil); err == nil {
		t.Fatal("canceled context was ignored")
	}
}

func TestDebugTimelinePaginationIsBoundedAndStable(t *testing.T) {
	s := testStore(t)
	stamp := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC).Format(time.RFC3339Nano)
	if _, err := s.db.Exec(`INSERT INTO jobs(id,input,status,created_at,updated_at) VALUES('job','secret','pending',?,?)`, stamp, stamp); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 102; i++ {
		if _, err := s.db.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES('job',?,?,?)`, fmt.Sprintf("event-%03d", i), "secret detail", stamp); err != nil {
			t.Fatal(err)
		}
	}
	first, err := s.DebugJobTimeline(context.Background(), "job", 1000, nil)
	if err != nil || len(first.Events) != 100 || first.NextPosition == nil || first.Events[0].Type != "event-000" || first.Events[99].Type != "event-099" {
		t.Fatalf("first timeline page = %#v, %v", first, err)
	}
	second, err := s.DebugJobTimeline(context.Background(), "job", 100, first.NextPosition)
	if err != nil || len(second.Events) != 2 || second.Events[0].Type != "event-100" || second.Events[1].Type != "event-101" {
		t.Fatalf("second timeline page = %#v, %v", second, err)
	}
}

func TestDebugTimelineSanitizesSyntheticCompletedJob(t *testing.T) {
	s := testStore(t)
	base := strings.Repeat("a", 40)
	candidate := strings.Repeat("b", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{
		Repository: "/synthetic/private/repo", BaseSHA: base,
		Instruction: "do not expose this", Tests: [][]string{{"sh", "-c", "private output"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-secret-id")
	if err != nil || !ok {
		t.Fatalf("lease: %#v %v %v", lease, ok, err)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, candidate); err != nil {
		t.Fatal(err)
	}
	timeline, err := s.DebugJobTimeline(context.Background(), job.ID, 1000, nil)
	if err != nil {
		t.Fatal(err)
	}
	if timeline.Job.BaseSHA != base || timeline.Job.CandidateSHA != candidate || timeline.Job.Kind != "coding" {
		t.Fatalf("timeline job = %#v", timeline.Job)
	}
	want := []string{"submitted", "leased", "succeeded"}
	if len(timeline.Events) != len(want) {
		t.Fatalf("events = %#v", timeline.Events)
	}
	for i, event := range timeline.Events {
		if event.Type != want[i] || event.At.IsZero() {
			t.Fatalf("event %d = %#v", i, event)
		}
	}
}

func TestDebugTimelineOmitsUnknownRawEventDetail(t *testing.T) {
	s := testStore(t)
	stamp := time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC)
	if _, err := s.db.Exec(`INSERT INTO jobs(id,input,status,created_at,updated_at) VALUES('job','secret','failed',?,?)`, stamp.Format(time.RFC3339Nano), stamp.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	const detail = "attempt=attempt-1 ordinal=7 lease_expires=2026-02-03T04:06:06Z retry_at=2026-02-03T04:07:06Z failure_code=secret_failure_token disposition=secret-disposition repository=/private/alice/project api_key=unrelated-secret"
	if _, err := s.db.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES('job','failed',?,?)`, detail, stamp.Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}

	timeline, err := s.DebugJobTimeline(context.Background(), "job", 100, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(timeline.Events) != 1 {
		t.Fatalf("events = %#v", timeline.Events)
	}
	event := timeline.Events[0]
	if event.AttemptID != "attempt-1" || event.AttemptOrdinal != 7 || event.LeaseExpiresAt == nil || event.RetryAt == nil {
		t.Fatalf("known metadata lost: %#v", event)
	}
	if event.FailureCode != "" || event.Disposition != "" {
		t.Fatalf("unknown failure metadata exposed: %#v", event)
	}
	body, err := json.Marshal(timeline)
	if err != nil {
		t.Fatal(err)
	}
	got := fmt.Sprintf("%#v\n%s", timeline, body)
	for _, private := range []string{"secret_failure_token", "secret-disposition", "/private/alice/project", "unrelated-secret"} {
		if strings.Contains(got, private) {
			t.Fatalf("timeline exposed %q: %s", private, got)
		}
	}
}

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestResultIsBoundToLeaseAttemptAndIdempotent(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("hello")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if lease.JobID != job.ID {
		t.Fatalf("got job %q want %q", lease.JobID, job.ID)
	}

	if _, err := s.Complete(job.ID, "wrong-attempt", "HELLO"); err == nil {
		t.Fatal("wrong attempt accepted")
	}
	first, err := s.Complete(job.ID, lease.AttemptID, "HELLO")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Complete(job.ID, lease.AttemptID, "HELLO")
	if err != nil {
		t.Fatalf("identical retry must be idempotent: %v", err)
	}
	if first.Result != second.Result || second.Status != "succeeded" {
		t.Fatalf("unexpected results: %#v %#v", first, second)
	}
	if _, err := s.Complete(job.ID, lease.AttemptID, "DIFFERENT"); err == nil {
		t.Fatal("mutable duplicate accepted")
	}
	if _, err := s.db.Exec(`UPDATE attempts SET failure_code='corrupt' WHERE id=?`, lease.AttemptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(job.ID, lease.AttemptID, "HELLO"); err == nil {
		t.Fatal("duplicate accepted mismatched attempt row")
	}
}

func TestWorkerCannotLeaseSecondJobUntilFirstIsTerminal(t *testing.T) {
	s := testStore(t)
	first, err := s.CreateJob("first")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateJob("second")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok || lease.JobID != first.ID {
		t.Fatalf("first lease = %#v, %v, %v", lease, ok, err)
	}
	if lease, ok, err := s.LeaseNext("worker-1"); err != nil || ok {
		t.Fatalf("second lease while first nonterminal = %#v, %v, %v", lease, ok, err)
	}
	if _, err := s.Complete(first.ID, lease.AttemptID, "done"); err != nil {
		t.Fatal(err)
	}
	lease, ok, err = s.LeaseNext("worker-1")
	if err != nil || !ok || lease.JobID != second.ID {
		t.Fatalf("second lease after first terminal = %#v, %v, %v", lease, ok, err)
	}
}

func TestWorkerLivenessTracksConnectionState(t *testing.T) {
	s := testStore(t)
	if err := s.SetWorkerConnected("w1", true); err != nil {
		t.Fatal(err)
	}
	w, err := s.Worker("w1")
	if err != nil || !w.Connected || w.LastSeen.IsZero() {
		t.Fatalf("connected worker not live: %#v %v", w, err)
	}
	if err := s.SetWorkerConnected("w1", false); err != nil {
		t.Fatal(err)
	}
	w, err = s.Worker("w1")
	if err != nil || w.Connected {
		t.Fatalf("disconnected worker reported live: %#v %v", w, err)
	}
}

func TestCandidateResultIsExactAndImmutable(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	want := "1111111111111111111111111111111111111111"
	first, err := s.CompleteCandidate(job.ID, lease.AttemptID, want)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CompleteCandidate(job.ID, lease.AttemptID, want)
	if err != nil {
		t.Fatalf("identical retry must be idempotent: %v", err)
	}
	if first.CandidateSHA != want || second.CandidateSHA != want {
		t.Fatalf("candidate SHA not preserved: %#v %#v", first, second)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Detail; got != "candidate_sha="+want {
		t.Fatalf("terminal event detail = %q", got)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, "2222222222222222222222222222222222222222"); err == nil {
		t.Fatal("conflicting candidate accepted")
	}
}

func TestCodingTaskCommitAuthorRoundTripsThroughJobAndLease(t *testing.T) {
	s := testStore(t)
	task := protocol.CodingTask{
		Instruction:       "coding task",
		CommitAuthorName:  "kricha",
		CommitAuthorEmail: "4619899+kricha@users.noreply.github.com",
	}
	job, err := s.CreateCodingJob(task)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.Job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	for label, got := range map[string]*protocol.CodingTask{"job": stored.Task, "lease": lease.Task} {
		if got == nil || got.CommitAuthorName != task.CommitAuthorName || got.CommitAuthorEmail != task.CommitAuthorEmail {
			t.Fatalf("%s task = %#v, want author %q <%s>", label, got, task.CommitAuthorName, task.CommitAuthorEmail)
		}
	}
}

func TestCodingJobRejectsLegacyTextCompletion(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Complete(job.ID, lease.AttemptID, "legacy result"); err == nil {
		t.Fatal("coding job accepted legacy text completion")
	}
}

func TestLegacyJobRejectsCandidateCompletion(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("legacy job")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, "1111111111111111111111111111111111111111"); err == nil {
		t.Fatal("legacy job accepted candidate completion")
	}
}

func TestFailureEventNamesFailureCode(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJob("legacy job")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Fail(job.ID, lease.AttemptID, "scoped_test_failed"); err != nil {
		t.Fatal(err)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := events[len(events)-1].Detail; got != "failure_code=scoped_test_failed" {
		t.Fatalf("terminal failure event detail = %q", got)
	}
	coding, err := s.CreateCodingJob(protocol.CodingTask{Instruction: "coding task"})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err = s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("coding lease: ok=%v err=%v", ok, err)
	}
	if _, err := s.Fail(coding.ID, lease.AttemptID, "execution_failed"); err != nil {
		t.Fatalf("coding failure rejected: %v", err)
	}
}
