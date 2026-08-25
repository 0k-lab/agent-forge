package worker

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func TestWorkerHeartbeatsStopBeforeResult(t *testing.T) {
	release := make(chan struct{})
	serverErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.CloseNow()
		ctx := r.Context()
		if err := wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageLease, JobID: "job", AttemptID: "attempt", Input: "work"}); err != nil {
			serverErr <- err
			return
		}
		heartbeats := 0
		for {
			var m protocol.Message
			if err := wsjson.Read(ctx, c, &m); err != nil {
				serverErr <- err
				return
			}
			if m.Type == protocol.MessageHeartbeat {
				heartbeats++
				if heartbeats == 2 {
					close(release)
				}
				continue
			}
			if m.Type != protocol.MessageResult || heartbeats < 2 {
				serverErr <- errors.New("result preceded heartbeats")
				return
			}
			if err := wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageAck, JobID: m.JobID, AttemptID: m.AttemptID}); err != nil {
				serverErr <- err
				return
			}
			serverErr <- nil
			return
		}
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runWithExecutor(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), "worker-1", "token", WorkerOptions{HeartbeatInterval: time.Millisecond}, func(ctx context.Context, _ protocol.Message) (string, string, error) {
		select {
		case <-release:
			return "done", "", nil
		case <-ctx.Done():
			return "", "", ctx.Err()
		}
	})
	if err == nil {
		t.Fatal("closed Gate connection reported success")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerBindsEvidenceBeforeCleanupAndResultWhileHeartbeating(t *testing.T) {
	cleaned := make(chan struct{})
	serverErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.CloseNow()
		lease := protocol.Message{Type: protocol.MessageLease, JobID: "job", AttemptID: "attempt", Task: &protocol.CodingTask{}}
		if err := wsjson.Write(r.Context(), c, lease); err != nil {
			serverErr <- err
			return
		}
		readNonHeartbeat := func() (protocol.Message, int, error) {
			heartbeats := 0
			for {
				var m protocol.Message
				if err := wsjson.Read(r.Context(), c, &m); err != nil {
					return m, heartbeats, err
				}
				if m.Type != protocol.MessageHeartbeat {
					return m, heartbeats, nil
				}
				heartbeats++
			}
		}
		primary, _, err := readNonHeartbeat()
		if err != nil || primary.Type != protocol.MessageEvidence || len(primary.Evidence) != 1 {
			serverErr <- errors.New("primary evidence missing")
			return
		}
		select {
		case <-cleaned:
			serverErr <- errors.New("cleanup ran before primary evidence ACK")
			return
		default:
		}
		time.Sleep(5 * time.Millisecond)
		if err := wsjson.Write(r.Context(), c, protocol.Message{Type: protocol.MessageAck, JobID: lease.JobID, AttemptID: lease.AttemptID}); err != nil {
			serverErr <- err
			return
		}
		cleanupEvidence, heartbeats, err := readNonHeartbeat()
		if err != nil || heartbeats == 0 || cleanupEvidence.Type != protocol.MessageEvidence || len(cleanupEvidence.Evidence) != 1 || cleanupEvidence.Evidence[0].Phase != protocol.EvidencePhaseCleanup {
			serverErr <- errors.New("cleanup evidence ordering or heartbeat wrong")
			return
		}
		if err := wsjson.Write(r.Context(), c, protocol.Message{Type: protocol.MessageAck, JobID: lease.JobID, AttemptID: lease.AttemptID}); err != nil {
			serverErr <- err
			return
		}
		result, _, err := readNonHeartbeat()
		if err != nil || result.Type != protocol.MessageResult || result.Error != protocol.FailureExecution {
			serverErr <- errors.New("terminal result ordering wrong")
			return
		}
		if err := wsjson.Write(r.Context(), c, protocol.Message{Type: protocol.MessageAck, JobID: lease.JobID, AttemptID: lease.AttemptID}); err != nil {
			serverErr <- err
			return
		}
		serverErr <- nil
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runWithOutcomeExecutor(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), "worker-1", "token", WorkerOptions{HeartbeatInterval: time.Millisecond}, func(context.Context, protocol.Message) leaseOutcome {
		return leaseOutcome{err: errors.New("failed"), evidence: []protocol.AttemptEvidence{{EvidenceID: strings.Repeat("1", 32)}}, cleanup: func() *protocol.AttemptEvidence {
			close(cleaned)
			time.Sleep(5 * time.Millisecond)
			return &protocol.AttemptEvidence{EvidenceID: strings.Repeat("2", 32), Phase: protocol.EvidencePhaseCleanup}
		}}
	})
	if err == nil {
		t.Fatal("closed Gate connection reported success")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerHeartbeatWriteFailureCancelsTask(t *testing.T) {
	cancelled := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		ctx := r.Context()
		_ = wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageLease, JobID: "job", AttemptID: "attempt"})
		var heartbeat protocol.Message
		_ = wsjson.Read(ctx, c, &heartbeat)
		c.CloseNow()
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runWithExecutor(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), "worker-1", "token", WorkerOptions{HeartbeatInterval: time.Millisecond}, func(ctx context.Context, _ protocol.Message) (string, string, error) {
		<-ctx.Done()
		close(cancelled)
		return "", "", ctx.Err()
	})
	if err == nil {
		t.Fatal("heartbeat failure reported success")
	}
	select {
	case <-cancelled:
	case <-time.After(time.Second):
		t.Fatal("task context was not cancelled")
	}
}

func TestWorkerRejectsACKWithUnknownFieldsBeforeNextLease(t *testing.T) {
	serverErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.CloseNow()
		lease := protocol.Message{Type: protocol.MessageLease, JobID: "job-1", AttemptID: "attempt-1"}
		if err := wsjson.Write(r.Context(), c, lease); err != nil {
			serverErr <- err
			return
		}
		var result protocol.Message
		if err := wsjson.Read(r.Context(), c, &result); err != nil {
			serverErr <- err
			return
		}
		body := `{"type":"ack","job_id":"job-1","attempt_id":"attempt-1","unknown":true}`
		if err := c.Write(r.Context(), websocket.MessageText, []byte(body)); err != nil {
			serverErr <- err
			return
		}
		_ = wsjson.Write(r.Context(), c, protocol.Message{Type: protocol.MessageLease, JobID: "job-2", AttemptID: "attempt-2"})
		var unexpected protocol.Message
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
		defer cancel()
		if err := wsjson.Read(ctx, c, &unexpected); err == nil {
			serverErr <- errors.New("Worker accepted unknown ACK field")
			return
		}
		serverErr <- nil
	}))
	defer ts.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	executions := 0
	err := runWithExecutor(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), "worker-1", "token", DefaultWorkerOptions(), func(context.Context, protocol.Message) (string, string, error) {
		executions++
		return "done", "", nil
	})
	if err == nil || executions != 1 {
		t.Fatalf("run error=%v executions=%d", err, executions)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestWorkerEmitsTypedFailureMappingsAndContinues(t *testing.T) {
	want := []protocol.Message{
		{Error: protocol.FailureInvalidTask, Disposition: protocol.DispositionTerminal},
		{Error: protocol.FailureScopedTest, Disposition: protocol.DispositionTerminal},
		{Error: protocol.FailureExecution, Disposition: protocol.DispositionRetryable},
	}
	serverErr := make(chan error, 1)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			serverErr <- err
			return
		}
		defer c.CloseNow()
		for i := range want {
			lease := protocol.Message{Type: protocol.MessageLease, JobID: "job" + string(rune('1'+i)), AttemptID: "attempt" + string(rune('1'+i))}
			if err := wsjson.Write(r.Context(), c, lease); err != nil {
				serverErr <- err
				return
			}
			var got protocol.Message
			if err := wsjson.Read(r.Context(), c, &got); err != nil {
				serverErr <- err
				return
			}
			if got.Type != protocol.MessageResult || got.Error != want[i].Error || got.Disposition != want[i].Disposition {
				serverErr <- errors.New("wrong failure mapping")
				return
			}
			if err := wsjson.Write(r.Context(), c, protocol.Message{Type: protocol.MessageAck, JobID: got.JobID, AttemptID: got.AttemptID}); err != nil {
				serverErr <- err
				return
			}
		}
		serverErr <- nil
	}))
	defer ts.Close()
	errs := []error{invalidTask(errors.New("bad task")), errScopedTest, errors.New("plugin failed")}
	var mu sync.Mutex
	next := 0
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := runWithExecutor(ctx, "ws"+strings.TrimPrefix(ts.URL, "http"), "worker-1", "token", DefaultWorkerOptions(), func(context.Context, protocol.Message) (string, string, error) {
		mu.Lock()
		defer mu.Unlock()
		err := errs[next]
		next++
		return "", "", err
	})
	if err == nil {
		t.Fatal("closed Gate connection reported success")
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
	if next != 3 {
		t.Fatalf("executed %d leases", next)
	}
}
