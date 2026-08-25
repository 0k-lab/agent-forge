package gate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func secureTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestWorkerConnectionHeartbeatFailureAndLaterLease(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	first, _ := s.CreateJob("first")
	second, _ := s.CreateJob("second")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	options := DefaultOptions()
	options.Policy = store.RecoveryPolicy{LeaseTTL: 10 * time.Second, BaseRetryBackoff: time.Second, MaxAttempts: 3}
	options.LeasePollInterval = time.Millisecond
	options.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	h, err := NewHandlerWithOptions(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret", options)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	c := dialWorker(t, ts.URL, "worker-1", "worker-secret")
	lease := readMessage(t, c)
	if lease.Type != protocol.MessageLease || lease.JobID != first.ID {
		t.Fatalf("first lease = %#v", lease)
	}
	clock.Store(start.Add(5 * time.Second).UnixNano())
	writeMessage(t, c, protocol.Message{Type: protocol.MessageHeartbeat, JobID: lease.JobID, AttemptID: lease.AttemptID, WorkerID: "worker-1"})
	deadline := time.Now().Add(time.Second)
	for {
		attempts, err := s.Attempts(first.ID)
		if err == nil && len(attempts) == 1 && attempts[0].DeadlineAt.Equal(start.Add(15*time.Second)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("heartbeat did not extend lease: %#v, %v", attempts, err)
		}
		time.Sleep(time.Millisecond)
	}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: lease.JobID, AttemptID: lease.AttemptID, Error: protocol.FailureExecution, Disposition: protocol.DispositionRetryable})
	writeMessage(t, c, protocol.Message{Type: protocol.MessageHeartbeat, JobID: lease.JobID, AttemptID: lease.AttemptID, WorkerID: "worker-1"})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck {
		t.Fatalf("failure ack = %#v", ack)
	}
	later := readMessage(t, c)
	if later.Type != protocol.MessageLease || later.JobID != second.ID || later.AttemptID == lease.AttemptID {
		t.Fatalf("later lease = %#v", later)
	}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: later.JobID, AttemptID: later.AttemptID, Result: "done"})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck {
		t.Fatalf("success ack = %#v", ack)
	}
	if job, err := s.Job(first.ID); err != nil || job.Status != "retry_wait" {
		t.Fatalf("retry job = %#v, %v", job, err)
	}
	if job, err := s.Job(second.ID); err != nil || job.Status != "succeeded" {
		t.Fatalf("later job = %#v, %v", job, err)
	}
	c.CloseNow()
	deadline = time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker, err := s.Worker("worker-1"); err == nil && !worker.Connected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker connection did not close")
}

func TestWorkerConnectionBindsEvidenceWithoutClearingLease(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base := strings.Repeat("a", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	options := DefaultOptions()
	options.LeasePollInterval = time.Millisecond
	options.Now = func() time.Time { return start }
	h, err := NewHandlerWithOptions(s, map[string]string{"token": "worker-1"}, "owner", options)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(h)
	defer ts.Close()
	c := dialWorker(t, ts.URL, "worker-1", "token")
	defer c.CloseNow()
	lease := readMessage(t, c)
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageEvidence, JobID: lease.JobID, AttemptID: lease.AttemptID, Evidence: []protocol.AttemptEvidence{record}})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck || ack.JobID != lease.JobID || ack.AttemptID != lease.AttemptID {
		t.Fatalf("evidence ACK = %#v", ack)
	}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: lease.JobID, AttemptID: lease.AttemptID, Error: protocol.FailureExecution, Disposition: protocol.DispositionRetryable})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck || ack.JobID != lease.JobID || ack.AttemptID != lease.AttemptID {
		t.Fatalf("result ACK = %#v", ack)
	}
	records, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil || len(records) != 1 || records[0].EvidenceID != record.EvidenceID {
		t.Fatalf("stored evidence = %#v, %v", records, err)
	}
	c.CloseNow()
	waitWorkerDisconnected(t, s, "worker-1")
}

func TestWorkerConnectionRejectsMismatchedLeaseMessagesGenerically(t *testing.T) {
	for name, bad := range map[string]func(protocol.Message) protocol.Message{
		"job":     func(m protocol.Message) protocol.Message { m.JobID = "wrong"; return m },
		"attempt": func(m protocol.Message) protocol.Message { m.AttemptID = "wrong"; return m },
		"worker":  func(m protocol.Message) protocol.Message { m.WorkerID = "worker-2"; return m },
		"mixed heartbeat": func(m protocol.Message) protocol.Message {
			m.Evidence = []protocol.AttemptEvidence{{EvidenceID: strings.Repeat("1", 32)}}
			return m
		},
		"disposition": func(m protocol.Message) protocol.Message {
			m.Type, m.Error, m.Disposition = protocol.MessageResult, protocol.FailureExecution, protocol.DispositionTerminal
			return m
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if _, err := s.CreateJob("work"); err != nil {
				t.Fatal(err)
			}
			options := DefaultOptions()
			options.LeasePollInterval = time.Millisecond
			h, err := NewHandlerWithOptions(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret", options)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := dialWorker(t, ts.URL, "worker-1", "worker-secret")
			lease := readMessage(t, c)
			message := protocol.Message{Type: protocol.MessageHeartbeat, JobID: lease.JobID, AttemptID: lease.AttemptID, WorkerID: "worker-1"}
			writeMessage(t, c, bad(message))
			got := readMessage(t, c)
			if got.Type != protocol.MessageError || got.Error != "request failed" || got.JobID != "" || got.AttemptID != "" {
				t.Fatalf("error = %#v", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if err := wsjson.Read(ctx, c, &got); err == nil {
				t.Fatal("stale connection remained open")
			}
			waitWorkerDisconnected(t, s, "worker-1")
		})
	}
}

func TestWorkerConnectionRejectsUnknownMessageFields(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.CreateJob("work"); err != nil {
		t.Fatal(err)
	}
	options := DefaultOptions()
	options.LeasePollInterval = time.Millisecond
	h, _ := NewHandlerWithOptions(s, map[string]string{"token": "worker"}, "owner", options)
	ts := httptest.NewServer(h)
	defer ts.Close()
	c := dialWorker(t, ts.URL, "worker", "token")
	lease := readMessage(t, c)
	body := `{"type":"heartbeat","job_id":"` + lease.JobID + `","attempt_id":"` + lease.AttemptID + `","worker_id":"worker","unknown":true}`
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := c.Write(ctx, websocket.MessageText, []byte(body)); err != nil {
		t.Fatal(err)
	}
	if got := readMessage(t, c); got.Type != protocol.MessageError {
		t.Fatalf("unknown field response = %#v", got)
	}
	c.CloseNow()
	waitWorkerDisconnected(t, s, "worker")
}

func TestWorkerConnectionRejectsInvalidEvidenceWithoutMutation(t *testing.T) {
	for name, mutate := range map[string]func(*protocol.Message){
		"empty":         func(m *protocol.Message) { m.Evidence = nil },
		"mixed":         func(m *protocol.Message) { m.Result = "not evidence" },
		"payload owner": func(m *protocol.Message) { m.WorkerID = "worker-1" },
		"malformed":     func(m *protocol.Message) { m.Evidence[0].Reason = "invented" },
		"truncated partial record": func(m *protocol.Message) {
			m.Evidence[0].Output = "[REDACTED]\nhttps://synthetic-user:synthetic-password"
			m.Evidence[0].OutputRedacted = true
			m.Evidence[0].OutputTruncated = true
		},
		"too many": func(m *protocol.Message) {
			m.Evidence = make([]protocol.AttemptEvidence, protocol.MaxEvidenceRecordsPerBatch+1)
		},
	} {
		t.Run(name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			base := strings.Repeat("a", 40)
			job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultOptions()
			options.LeasePollInterval = time.Millisecond
			h, _ := NewHandlerWithOptions(s, map[string]string{"token": "worker-1"}, "owner", options)
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := dialWorker(t, ts.URL, "worker-1", "token")
			lease := readMessage(t, c)
			message := protocol.Message{Type: protocol.MessageEvidence, JobID: lease.JobID, AttemptID: lease.AttemptID, Evidence: []protocol.AttemptEvidence{{
				EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base,
			}}}
			mutate(&message)
			writeMessage(t, c, message)
			if got := readMessage(t, c); got.Type != protocol.MessageError || got.Error != "request failed" || got.JobID != "" || got.AttemptID != "" {
				t.Fatalf("rejection = %#v", got)
			}
			records, err := s.AttemptEvidence(job.ID, lease.AttemptID)
			if err != nil || len(records) != 0 {
				t.Fatalf("rejected evidence persisted: %#v, %v", records, err)
			}
			c.CloseNow()
			waitWorkerDisconnected(t, s, "worker-1")
		})
	}
}

func TestEvidenceSurvivesGateRestartBeforeTerminalResult(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	policy := store.RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3}
	base := strings.Repeat("a", 40)
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultOptions()
	options.Policy = policy
	options.LeasePollInterval = time.Millisecond
	options.Now = func() time.Time { return start }
	h, _ := NewHandlerWithOptions(s, map[string]string{"token": "worker-1"}, "owner", options)
	ts := httptest.NewServer(h)
	c := dialWorker(t, ts.URL, "worker-1", "token")
	lease := readMessage(t, c)
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageEvidence, JobID: lease.JobID, AttemptID: lease.AttemptID, Evidence: []protocol.AttemptEvidence{record}})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck {
		t.Fatalf("evidence ACK = %#v", ack)
	}
	c.CloseNow()
	waitWorkerDisconnected(t, s, "worker-1")
	ts.Close()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	restarted := NewHandler(s, map[string]string{"token": "worker-1"}, "owner")
	res := debugRequest(t, restarted, "/v1/jobs/"+job.ID+"/attempts/"+lease.AttemptID+"/evidence", "owner")
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), record.EvidenceID) {
		t.Fatalf("restarted evidence = %d %s", res.Code, res.Body.String())
	}
	stored, err := s.Job(job.ID)
	if err != nil || stored.Status != "leased" {
		t.Fatalf("job after evidence-only restart = %#v, %v", stored, err)
	}
	if err := s.SweepExpired(start.Add(policy.LeaseTTL), policy); err != nil {
		t.Fatal(err)
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{{EvidenceID: strings.Repeat("2", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base}}, start.Add(policy.LeaseTTL+time.Second)); err == nil {
		t.Fatal("expired prior attempt appended evidence")
	}
}

func TestGateOwnsBoundedFailurePolicy(t *testing.T) {
	for _, tc := range []struct {
		name, code, disposition, status, attemptStatus string
		maxAttempts                                    int
	}{
		{"invalid", protocol.FailureInvalidTask, protocol.DispositionTerminal, "failed", "terminal_failed", 3},
		{"scoped legacy", protocol.FailureScopedTest, "", "failed", "terminal_failed", 3},
		{"execution", protocol.FailureExecution, protocol.DispositionRetryable, "retry_wait", "retryable_failed", 3},
		{"max attempts", protocol.FailureExecution, protocol.DispositionRetryable, "failed", "retryable_failed", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			job, err := s.CreateJob("work")
			if err != nil {
				t.Fatal(err)
			}
			options := DefaultOptions()
			options.Policy.MaxAttempts = tc.maxAttempts
			options.LeasePollInterval = time.Millisecond
			h, err := NewHandlerWithOptions(s, map[string]string{"token": "worker"}, "owner", options)
			if err != nil {
				t.Fatal(err)
			}
			ts := httptest.NewServer(h)
			defer ts.Close()
			c := dialWorker(t, ts.URL, "worker", "token")
			defer func() {
				c.CloseNow()
				waitWorkerDisconnected(t, s, "worker")
			}()
			lease := readMessage(t, c)
			writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: lease.JobID, AttemptID: lease.AttemptID, Error: tc.code, Disposition: tc.disposition})
			if ack := readMessage(t, c); ack.Type != protocol.MessageAck {
				t.Fatalf("ack = %#v", ack)
			}
			stored, err := s.Job(job.ID)
			if err != nil || stored.Status != tc.status {
				t.Fatalf("job = %#v, %v", stored, err)
			}
			attempts, err := s.Attempts(job.ID)
			if err != nil || len(attempts) != 1 || attempts[0].Status != tc.attemptStatus {
				t.Fatalf("attempts = %#v, %v", attempts, err)
			}
		})
	}

	t.Run("unknown code", func(t *testing.T) {
		s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
		if err != nil {
			t.Fatal(err)
		}
		defer s.Close()
		job, _ := s.CreateJob("work")
		options := DefaultOptions()
		options.LeasePollInterval = time.Millisecond
		h, _ := NewHandlerWithOptions(s, map[string]string{"token": "worker"}, "owner", options)
		ts := httptest.NewServer(h)
		defer ts.Close()
		c := dialWorker(t, ts.URL, "worker", "token")
		defer func() {
			c.CloseNow()
			waitWorkerDisconnected(t, s, "worker")
		}()
		lease := readMessage(t, c)
		writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: lease.JobID, AttemptID: lease.AttemptID, Error: "invented", Disposition: protocol.DispositionRetryable})
		if got := readMessage(t, c); got.Type != protocol.MessageError || got.Error != "request failed" {
			t.Fatalf("error = %#v", got)
		}
		stored, _ := s.Job(job.ID)
		attempts, _ := s.Attempts(job.ID)
		if stored.Status != "leased" || len(attempts) != 1 || attempts[0].Status != "leased" {
			t.Fatalf("unknown consumed attempt: %#v %#v", stored, attempts)
		}
	})
}

func TestGateRestartRecoversLeaseAndRejectsLateOldResult(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	var clock atomic.Int64
	clock.Store(start.UnixNano())
	options := DefaultOptions()
	options.Policy = store.RecoveryPolicy{LeaseTTL: 20 * time.Millisecond, BaseRetryBackoff: 10 * time.Millisecond, MaxAttempts: 3}
	options.RecoveryInterval = 10 * time.Millisecond
	options.LeasePollInterval = time.Millisecond
	options.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }

	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, _ := s.CreateJob("work")
	h, _ := NewHandlerWithOptions(s, map[string]string{"token": "worker"}, "owner", options)
	ts := httptest.NewServer(h)
	c := dialWorker(t, ts.URL, "worker", "token")
	old := readMessage(t, c)
	c.CloseNow()
	ts.Close()
	waitWorkerDisconnected(t, s, "worker")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	clock.Store(start.Add(options.Policy.LeaseTTL).UnixNano())
	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	recoveryErr, err := StartRecovery(ctx, s, options)
	if err != nil {
		t.Fatal(err)
	}
	recoveryStopped := false
	defer func() {
		if !recoveryStopped {
			cancel()
			<-recoveryErr
		}
	}()
	if attempts, err := s.Attempts(job.ID); err != nil || len(attempts) != 1 || attempts[0].ID != old.AttemptID || attempts[0].Status != "expired" {
		t.Fatalf("recovered attempts = %#v, %v", attempts, err)
	}
	cancel()
	recoveryRunErr := <-recoveryErr
	recoveryStopped = true
	if !errors.Is(recoveryRunErr, context.Canceled) {
		t.Fatalf("stop recovery: %v", recoveryRunErr)
	}

	h, _ = NewHandlerWithOptions(s, map[string]string{"token": "worker"}, "owner", options)
	ts = httptest.NewServer(h)
	defer ts.Close()
	early := dialWorker(t, ts.URL, "worker", "token")
	readCtx, readCancel := context.WithTimeout(context.Background(), 15*time.Millisecond)
	var message protocol.Message
	if err := wsjson.Read(readCtx, early, &message); err == nil {
		t.Fatalf("retry leased early: %#v", message)
	}
	readCancel()
	early.CloseNow()
	waitWorkerDisconnected(t, s, "worker")
	clock.Store(start.Add(options.Policy.LeaseTTL + options.Policy.BaseRetryBackoff).UnixNano())
	c = dialWorker(t, ts.URL, "worker", "token")
	defer c.CloseNow()
	current := readMessage(t, c)
	if current.AttemptID == old.AttemptID {
		t.Fatalf("attempt reused: %#v", current)
	}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: current.JobID, AttemptID: current.AttemptID, Result: "done"})
	if ack := readMessage(t, c); ack.Type != protocol.MessageAck {
		t.Fatalf("new result ack = %#v", ack)
	}
	writeMessage(t, c, protocol.Message{Type: protocol.MessageResult, JobID: old.JobID, AttemptID: old.AttemptID, Result: "late"})
	if rejected := readMessage(t, c); rejected.Type != protocol.MessageError || rejected.Error != "request failed" {
		t.Fatalf("late result = %#v", rejected)
	}
	waitWorkerDisconnected(t, s, "worker")
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 2 || attempts[0].Ordinal != 1 || attempts[0].Status != "expired" || attempts[1].Ordinal != 2 || attempts[1].Status != "succeeded" {
		t.Fatalf("attempt history = %#v, %v", attempts, err)
	}
}

func dialWorker(t *testing.T, serverURL, workerID, token string) *websocket.Conn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	h := http.Header{"Authorization": []string{"Bearer " + token}}
	c, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(serverURL, "http")+"/v1/workers/connect?worker_id="+workerID, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func readMessage(t *testing.T, c *websocket.Conn) protocol.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	var m protocol.Message
	if err := wsjson.Read(ctx, c, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func writeMessage(t *testing.T, c *websocket.Conn, m protocol.Message) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := wsjson.Write(ctx, c, m); err != nil {
		t.Fatal(err)
	}
}

func waitWorkerDisconnected(t *testing.T, s *store.Store, workerID string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker, err := s.Worker(workerID); err == nil && !worker.Connected {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("worker connection did not close")
}

func TestDebugCursorsAreAuthenticatedScopedAndStableAcrossRestart(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	jobA, err := s.CreateJob("secret-a")
	if err != nil {
		t.Fatal(err)
	}
	jobB, err := s.CreateJob("secret-b")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := s.LeaseNext("worker-a"); err != nil || !ok {
		t.Fatalf("lease: ok=%v err=%v", ok, err)
	}
	if err := s.SetWorkerConnected("worker-a", true); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerConnected("worker-b", true); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, nil, "owner-secret")
	jobsCursor := debugResponseCursor(t, h, "/v1/debug/jobs?limit=1", "owner-secret")
	workersCursor := debugResponseCursor(t, h, "/v1/debug/workers?limit=1", "owner-secret")
	timelineCursor := debugResponseCursor(t, h, "/v1/debug/jobs/"+jobA.ID+"?limit=1", "owner-secret")

	for name, tc := range map[string]struct {
		h     http.Handler
		path  string
		token string
	}{
		"same token after restart": {NewHandler(s, nil, "owner-secret"), "/v1/debug/jobs?cursor=" + jobsCursor, "owner-secret"},
		"owner token changed":      {NewHandler(s, nil, "new-owner-secret"), "/v1/debug/jobs?cursor=" + jobsCursor, "new-owner-secret"},
		"jobs on workers":          {h, "/v1/debug/workers?cursor=" + jobsCursor, "owner-secret"},
		"workers on jobs":          {h, "/v1/debug/jobs?cursor=" + workersCursor, "owner-secret"},
		"timeline on jobs":         {h, "/v1/debug/jobs?cursor=" + timelineCursor, "owner-secret"},
		"timeline on another job":  {h, "/v1/debug/jobs/" + jobB.ID + "?cursor=" + timelineCursor, "owner-secret"},
	} {
		t.Run(name, func(t *testing.T) {
			want := http.StatusBadRequest
			if name == "same token after restart" {
				want = http.StatusOK
			}
			assertDebugStatus(t, tc.h, tc.path, tc.token, want)
		})
	}

	codec := newDebugCursorCodec(s.DebugCursorKey("owner-secret"))
	wrongVersion := signedDebugCursor(t, codec, debugCursorPayload{Version: 2, Purpose: debugJobsPurpose, Stamp: time.Now().UTC().Format(time.RFC3339Nano), ID: "job"})
	unsigned := base64.RawURLEncoding.EncodeToString([]byte(time.Now().UTC().Format(time.RFC3339Nano) + "\x00job"))
	replacement := "A"
	if jobsCursor[0] == 'A' {
		replacement = "B"
	}
	modified := replacement + jobsCursor[1:]
	for name, cursor := range map[string]string{
		"modified":         modified,
		"truncated":        jobsCursor[:len(jobsCursor)-1],
		"unsigned":         unsigned,
		"malformed":        "not-valid",
		"version mismatch": wrongVersion,
		"too long":         strings.Repeat("A", maxDebugCursorLength+1),
	} {
		t.Run(name, func(t *testing.T) {
			assertDebugStatus(t, h, "/v1/debug/jobs?cursor="+cursor, "owner-secret", http.StatusBadRequest)
		})
	}
}

func TestDebugCursorKeyBindsDatabaseAndOwnerToken(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	s, err := store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := s.CreateJob("secret"); err != nil {
			t.Fatal(err)
		}
	}
	cursor := debugResponseCursor(t, NewHandler(s, nil, "owner-secret"), "/v1/debug/jobs?limit=1", "owner-secret")
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = store.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	assertDebugStatus(t, NewHandler(s, nil, "owner-secret"), "/v1/debug/jobs?cursor="+cursor, "owner-secret", http.StatusOK)
	assertDebugStatus(t, NewHandler(s, nil, "changed-owner"), "/v1/debug/jobs?cursor="+cursor, "changed-owner", http.StatusBadRequest)

	other, err := store.Open(filepath.Join(secureTempDir(t), "other.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	assertDebugStatus(t, NewHandler(other, nil, "owner-secret"), "/v1/debug/jobs?cursor="+cursor, "owner-secret", http.StatusBadRequest)

	payload := debugCursorPayload{Version: debugCursorVersion, Purpose: debugJobsPurpose, Stamp: time.Now().UTC().Format(time.RFC3339Nano), ID: "job"}
	assertDebugStatus(t, NewHandler(s, nil, "owner-secret"), "/v1/debug/jobs?cursor="+ownerTokenOnlyCursor(t, "owner-secret", payload), "owner-secret", http.StatusBadRequest)
}

func TestDebugLimitsRejectNonPositiveAndCapLargeValues(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, "owner-secret")
	for _, path := range []string{"/v1/debug/jobs", "/v1/debug/workers", "/v1/debug/jobs/missing"} {
		assertDebugStatus(t, h, path+"?limit=0", "owner-secret", http.StatusBadRequest)
		assertDebugStatus(t, h, path+"?limit=-1", "owner-secret", http.StatusBadRequest)
	}
	for range 101 {
		if _, err := s.CreateJob("secret"); err != nil {
			t.Fatal(err)
		}
	}
	res := debugRequest(t, h, "/v1/debug/jobs?limit=1000000", "owner-secret")
	var page struct {
		Items []json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &page); res.Code != http.StatusOK || err != nil || len(page.Items) != 100 {
		t.Fatalf("large limit = %d, %d items, err=%v; want 200 and 100 items", res.Code, len(page.Items), err)
	}
}

func TestDebugJobsAndWorkersHideStoreFailures(t *testing.T) {
	for _, path := range []string{"/v1/debug/jobs", "/v1/debug/workers"} {
		t.Run(path+" database", func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			h := NewHandler(s, nil, "owner-secret")
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
			res := debugRequest(t, h, path, "owner-secret")
			if res.Code != http.StatusInternalServerError || res.Body.String() != "{\"message\":\"request failed\"}\n" {
				t.Fatalf("store failure = %d %q, want generic 500", res.Code, res.Body.String())
			}
		})
		t.Run(path+" context", func(t *testing.T) {
			s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			h := NewHandler(s, nil, "owner-secret")
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			req := httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx)
			req.Header.Set("Authorization", "Bearer owner-secret")
			res := httptest.NewRecorder()
			h.ServeHTTP(res, req)
			if res.Code != http.StatusInternalServerError || res.Body.String() != "{\"message\":\"request failed\"}\n" {
				t.Fatalf("context failure = %d %q, want generic 500", res.Code, res.Body.String())
			}
		})
		t.Run(path+" scan", func(t *testing.T) {
			dbPath := filepath.Join(secureTempDir(t), "forge.db")
			s, err := store.Open(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if path == "/v1/debug/jobs" {
				if _, err := s.CreateJob("secret"); err != nil {
					t.Fatal(err)
				}
			} else if err := s.SetWorkerConnected("worker", true); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", dbPath)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			table, column := "jobs", "updated_at"
			if path == "/v1/debug/workers" {
				table, column = "workers", "last_seen"
			}
			if _, err := db.Exec("UPDATE " + table + " SET " + column + "='not-a-time'"); err != nil {
				t.Fatal(err)
			}
			h := NewHandler(s, nil, "owner-secret")
			res := debugRequest(t, h, path, "owner-secret")
			if res.Code != http.StatusInternalServerError || res.Body.String() != "{\"message\":\"request failed\"}\n" {
				t.Fatalf("scan failure = %d %q, want generic 500", res.Code, res.Body.String())
			}
		})
	}
}

func debugResponseCursor(t *testing.T, h http.Handler, path, token string) string {
	t.Helper()
	res := debugRequest(t, h, path, token)
	if res.Code != http.StatusOK {
		t.Fatalf("GET %s = %d: %s", path, res.Code, res.Body.String())
	}
	var body struct {
		NextCursor string `json:"next_cursor"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil || body.NextCursor == "" {
		t.Fatalf("GET %s cursor = %q, err=%v", path, body.NextCursor, err)
	}
	return body.NextCursor
}

func assertDebugStatus(t *testing.T, h http.Handler, path, token string, want int) {
	t.Helper()
	res := debugRequest(t, h, path, token)
	if res.Code != want {
		t.Fatalf("GET %s = %d: %s; want %d", path, res.Code, res.Body.String(), want)
	}
}

func debugRequest(t *testing.T, h http.Handler, path, token string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+token)
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	return res
}

func signedDebugCursor(t *testing.T, codec debugCursorCodec, payload debugCursorPayload) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	mac := hmac.New(sha256.New, codec.key[:])
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(append(body, mac.Sum(nil)...))
}

func ownerTokenOnlyCursor(t *testing.T, ownerToken string, payload debugCursorPayload) string {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	key := sha256.Sum256([]byte("agent-forge/debug-cursor/v1\x00" + ownerToken))
	mac := hmac.New(sha256.New, key[:])
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(append(body, mac.Sum(nil)...))
}

func TestDebugAPIRequiresOwnerAndReturnsOnlySanitizedData(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	base, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{
		Repository: "/synthetic/private/repository", BaseSHA: base,
		Instruction: "private instruction", Tests: [][]string{{"private-test", "private-output"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker-1")
	if err != nil || !ok {
		t.Fatalf("lease: %#v %v %v", lease, ok, err)
	}
	if _, err := s.CompleteCandidate(job.ID, lease.AttemptID, candidate); err != nil {
		t.Fatal(err)
	}
	if err := s.SetWorkerConnected("worker-1", true); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret")

	for _, path := range []string{"/v1/debug/jobs", "/v1/debug/workers", "/v1/debug/jobs/" + job.ID} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		res := httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s = %d, want 401", path, res.Code)
		}
		req.Header.Set("Authorization", "Bearer owner-secret")
		res = httptest.NewRecorder()
		h.ServeHTTP(res, req)
		if res.Code != http.StatusOK {
			t.Fatalf("authenticated %s = %d: %s", path, res.Code, res.Body.String())
		}
		body := res.Body.String()
		for _, secret := range []string{"private instruction", "/synthetic/private/repository", "private-test", "private-output", "worker-secret", "job accepted", "candidate_sha="} {
			if strings.Contains(body, secret) {
				t.Fatalf("%s exposed %q in %s", path, secret, body)
			}
		}
		var value any
		if err := json.Unmarshal([]byte(body), &value); err != nil {
			t.Fatal(err)
		}
		assertNoDebugSensitiveKeys(t, value)
		if strings.HasSuffix(path, job.ID) && (!strings.Contains(body, base) || !strings.Contains(body, candidate)) {
			t.Fatalf("timeline omitted exact SHAs: %s", body)
		}
		if path == "/v1/debug/workers" && !strings.Contains(body, `"connected":true`) {
			t.Fatalf("workers omitted connection state: %s", body)
		}
	}
}

func TestAttemptEvidenceAPIRequiresOwnerAndReturnsOnlyStructuredEvidence(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base, repository := strings.Repeat("a", 40), "/private/alice/repository"
	job, err := s.CreateCodingJob(protocol.CodingTask{
		Repository: repository, BaseSHA: base, Instruction: "private prompt",
		Tests: [][]string{{"check", repository + "/secret", "--token=argv-secret", "Access Token: synthetic-gate-access-token", "--client secret synthetic-gate-client-secret; safe"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, store.RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	checkIndex := 0
	safeOutput := protocol.EvidenceRedactedMarker
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{{
		EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckFailed,
		CheckIndex: &checkIndex, DurationMS: 5, Output: safeOutput, OutputRedacted: true, OutputTruncated: true, BaseSHA: base,
	}}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	h := NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret")
	path := "/v1/jobs/" + job.ID + "/attempts/" + lease.AttemptID + "/evidence"

	for name, tc := range map[string]struct {
		token string
		want  int
	}{
		"missing token": {want: http.StatusUnauthorized},
		"worker token":  {token: "worker-secret", want: http.StatusUnauthorized},
		"owner":         {token: "owner-secret", want: http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			res := debugRequest(t, h, path, tc.token)
			if res.Code != tc.want {
				t.Fatalf("GET evidence = %d: %s, want %d", res.Code, res.Body.String(), tc.want)
			}
			if tc.want != http.StatusOK {
				return
			}
			body := res.Body.String()
			for _, private := range []string{repository, "private prompt", "argv-secret", "output-secret", "worker-secret", "PRIVATE_ENV=output-secret", "synthetic-user:synthetic-password", "synthetic-gate-access-token", "synthetic-gate-client-secret"} {
				if strings.Contains(body, private) {
					t.Fatalf("evidence API exposed %q: %s", private, body)
				}
			}
			var value any
			if err := json.Unmarshal([]byte(body), &value); err != nil {
				t.Fatal(err)
			}
			assertNoDebugSensitiveKeys(t, value)
			if !strings.Contains(body, base) || !strings.Contains(body, `"argv_redacted":true`) || !strings.Contains(body, `"output_redacted":true`) || !strings.Contains(body, `"check_index":0`) {
				t.Fatalf("evidence API omitted structured binding: %s", body)
			}
		})
	}

	for _, missing := range []string{
		"/v1/jobs/missing/attempts/" + lease.AttemptID + "/evidence",
		"/v1/jobs/" + job.ID + "/attempts/missing/evidence",
	} {
		res := debugRequest(t, h, missing, "owner-secret")
		if res.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d: %s, want 404", missing, res.Code, res.Body.String())
		}
	}
}

func assertNoDebugSensitiveKeys(t *testing.T, value any) {
	t.Helper()
	forbidden := map[string]bool{"input": true, "task": true, "instruction": true, "repository": true, "tests": true, "result": true, "detail": true, "error": true, "authorization": true}
	switch value := value.(type) {
	case map[string]any:
		for key, child := range value {
			if forbidden[strings.ToLower(key)] {
				t.Fatalf("sensitive debug JSON key %q", key)
			}
			assertNoDebugSensitiveKeys(t, child)
		}
	case []any:
		for _, child := range value {
			assertNoDebugSensitiveKeys(t, child)
		}
	}
}

func TestDebugRoutesAreGETOnlyAndViewerIsLockedDown(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, "owner-secret")
	for _, method := range []string{http.MethodHead, http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete} {
		for _, path := range []string{"/v1/debug/jobs", "/v1/debug/workers", "/v1/debug/jobs/missing"} {
			res := httptest.NewRecorder()
			h.ServeHTTP(res, httptest.NewRequest(method, path, nil))
			if res.Code != http.StatusMethodNotAllowed {
				t.Fatalf("%s %s = %d, want 405", method, path, res.Code)
			}
		}
	}
	badCursor := httptest.NewRequest(http.MethodGet, "/v1/debug/jobs?cursor=not-valid", nil)
	badCursor.Header.Set("Authorization", "Bearer owner-secret")
	badCursorResult := httptest.NewRecorder()
	h.ServeHTTP(badCursorResult, badCursor)
	if badCursorResult.Code != http.StatusBadRequest {
		t.Fatalf("invalid cursor = %d, want 400", badCursorResult.Code)
	}

	var assets strings.Builder
	for _, path := range []string{"/debug/", "/debug/app.css", "/debug/app.js"} {
		res := httptest.NewRecorder()
		h.ServeHTTP(res, httptest.NewRequest(http.MethodGet, path, nil))
		if res.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, res.Code)
		}
		if res.Header().Get("Content-Security-Policy") == "" || res.Header().Get("X-Content-Type-Options") != "nosniff" || res.Header().Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("missing security headers on %s: %v", path, res.Header())
		}
		assets.WriteString(res.Body.String())
	}
	body := strings.ToLower(assets.String())
	for _, forbidden := range []string{"https://", "http://", "innerhtml", "localstorage", "sessionstorage", "document.cookie", "type=\"submit\"", ">retry<", ">cancel<", ">approve<", "method: 'post'", "method: \"post\""} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("viewer contains forbidden content %q", forbidden)
		}
	}
	if !strings.Contains(body, `type="password"`) || !strings.Contains(body, "textcontent") || !strings.Contains(body, "/v1/debug/jobs") {
		t.Fatalf("viewer lacks password-only auth or safe rendering: %s", body)
	}
}

func TestValidateTaskRejectsInvalidCommitAuthorsWithoutEchoingValues(t *testing.T) {
	valid := protocol.CodingTask{
		Repository:  "/repo",
		BaseSHA:     strings.Repeat("a", 40),
		Instruction: "edit",
		Tests:       [][]string{{"true"}},
	}
	tests := []struct {
		name  string
		value string
		set   func(*protocol.CodingTask, string)
	}{
		{"name only", "kricha", func(task *protocol.CodingTask, value string) { task.CommitAuthorName = value }},
		{"email only", "kricha@example.com", func(task *protocol.CodingTask, value string) { task.CommitAuthorEmail = value }},
		{"oversized name", strings.Repeat("n", 257), func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"oversized email", strings.Repeat("e", 255), func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name newline", "kricha\nInjected", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email carriage return", "kricha@example.com\rInjected", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name control", "k\x00richa", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"name angle bracket", "kricha <admin>", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email header form", "kricha <kricha@example.com>", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name header style", "Author: kricha", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"malformed email", "not-an-email", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"name leading whitespace", " kricha", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"name trailing unicode whitespace", "kricha\u2003", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = value, "kricha@example.com"
		}},
		{"email leading unicode whitespace", "\u2003kricha@example.com", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
		{"email trailing whitespace", "kricha@example.com ", func(task *protocol.CodingTask, value string) {
			task.CommitAuthorName, task.CommitAuthorEmail = "kricha", value
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := valid
			tt.set(&task, tt.value)
			err := validateTask(task)
			if err == nil {
				t.Fatal("invalid commit author accepted")
			}
			if len(err.Error()) > 64 || strings.Contains(err.Error(), tt.value) {
				t.Fatalf("unsafe validation error %q", err)
			}
		})
	}
}

func TestValidateTaskAcceptsAbsentOrPairedCommitAuthor(t *testing.T) {
	for _, author := range [][2]string{{}, {"kricha", "4619899+kricha@users.noreply.github.com"}} {
		task := protocol.CodingTask{
			Repository:        "/repo",
			BaseSHA:           strings.Repeat("a", 40),
			Instruction:       "edit",
			Tests:             [][]string{{"true"}},
			CommitAuthorName:  author[0],
			CommitAuthorEmail: author[1],
		}
		if err := validateTask(task); err != nil {
			t.Fatalf("author %q <%s> rejected: %v", author[0], author[1], err)
		}
	}
}

func TestSubmitRejectsUppercaseBaseSHAWithoutPersistingJob(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	h := NewHandler(s, nil, "owner-secret")
	body := `{"repository":"/repo","base_sha":"` + strings.Repeat("A", 40) + `","instruction":"edit","tests":[["true"]]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer owner-secret")
	res := httptest.NewRecorder()
	h.ServeHTTP(res, req)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("POST uppercase base_sha = %d, want 400", res.Code)
	}
	if _, ok, err := s.LeaseNext("worker-1"); err != nil || ok {
		t.Fatalf("rejected job persisted: ok=%v err=%v", ok, err)
	}
}

func TestWorkerWebSocketRequiresMatchingBearerToken(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret"))
	defer ts.Close()
	url := "ws" + strings.TrimPrefix(ts.URL, "http") + "/v1/workers/connect?worker_id=worker-1"

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if c, _, err := websocket.Dial(ctx, url, nil); err == nil {
		c.CloseNow()
		t.Fatal("missing token accepted")
	}
	h := http.Header{"Authorization": []string{"Bearer wrong"}}
	if c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h}); err == nil {
		c.CloseNow()
		t.Fatal("wrong token accepted")
	}
	h.Set("Authorization", "Bearer worker-secret")
	c, _, err := websocket.Dial(ctx, url, &websocket.DialOptions{HTTPHeader: h})
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	c.Close(websocket.StatusNormalClosure, "done")
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker, err := s.Worker("worker-1"); err == nil && !worker.Connected {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker still connected after WebSocket close")
}

func TestOwnerHTTPAPIRequiresDistinctBearerToken(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, "owner-secret"))
	defer ts.Close()

	for _, token := range []string{"", "wrong", "worker-secret"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/jobs", bytes.NewBufferString(`{"input":"hello"}`))
		if err != nil {
			t.Fatal(err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		if res.StatusCode != http.StatusUnauthorized {
			t.Fatalf("token %q status = %d, want 401", token, res.StatusCode)
		}
	}

	req, err := http.NewRequest(http.MethodPost, ts.URL+"/v1/jobs", bytes.NewBufferString(`{"input":"hello"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer owner-secret")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusCreated {
		t.Fatalf("owner token status = %d, want 201", res.StatusCode)
	}
}

func TestOwnerAuthHashesEveryPresentedToken(t *testing.T) {
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	x := newServer(s, nil, "owner-secret", DefaultOptions())
	if x.ownerDigest != sha256.Sum256([]byte("owner-secret")) {
		t.Fatal("server did not store the owner token as a SHA-256 digest")
	}
	for _, token := range []string{"x", strings.Repeat("x", 4096), "wrong-secret"} {
		req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(`{"input":"hello"}`))
		req.Header.Set("Authorization", "Bearer "+token)
		res := httptest.NewRecorder()
		x.routes().ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("token length %d status = %d, want 401", len(token), res.Code)
		}
	}
}

func TestOwnerHTTPRoutesFailClosedWithoutConfiguredToken(t *testing.T) {
	for _, ownerToken := range []string{"", "worker-secret"} {
		s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
		if err != nil {
			t.Fatal(err)
		}
		ts := httptest.NewServer(NewHandler(s, map[string]string{"worker-secret": "worker-1"}, ownerToken))
		for _, path := range []string{"/v1/jobs/missing", "/v1/jobs/missing/events", "/v1/workers/missing"} {
			req, err := http.NewRequest(http.MethodGet, ts.URL+path, nil)
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Authorization", "Bearer worker-secret")
			res, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			res.Body.Close()
			if res.StatusCode != http.StatusUnauthorized {
				t.Fatalf("owner token %q, %s status = %d, want 401", ownerToken, path, res.StatusCode)
			}
		}
		ts.Close()
		s.Close()
	}
}
