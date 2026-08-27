package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"github.com/coder/websocket"
)

func configuredGate(t *testing.T) (*store.Store, Config, *httptest.Server) {
	t.Helper()
	body := strings.Replace(validGateConfig,
		`"workers": [{"id":"worker-1","pool":"general","token_env":"FORGE_WORKER_TOKEN","concurrency":2}]`,
		`"workers": [{"id":"worker-1","pool":"general","token_env":"FORGE_WORKER_TOKEN","concurrency":2},{"id":"worker-2","pool":"coding","token_env":"FORGE_CODING_TOKEN","concurrency":1}]`, 1)
	body = strings.Replace(body, `"worker_pool":"general"`, `"worker_pool":"coding"`, 1)
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner", "FORGE_WORKER_TOKEN": "general-token", "FORGE_CODING_TOKEN": "coding-token"}
	config, err := ParseConfig([]byte(body), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	options := DefaultOptions()
	options.LeasePollInterval = time.Millisecond
	handler, err := NewConfiguredHandler(s, config, options)
	if err != nil {
		s.Close()
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	t.Cleanup(func() { s.Close() })
	return s, config, server
}

func TestConfiguredSubmissionResolvesRepositoryPolicyAndPool(t *testing.T) {
	_, _, server := configuredGate(t)
	request := `{"repository_id":"agent-forge","base_sha":"` + strings.Repeat("a", 40) + `","instruction":"change","tests":[["go","test","./..."]]}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", bytes.NewBufferString(request))
	req.Header.Set("Authorization", "Bearer owner")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d", response.StatusCode)
	}
	var job struct {
		RepositoryID  string `json:"repository_id"`
		WorkerPool    string `json:"worker_pool"`
		PolicyVersion int    `json:"policy_version"`
	}
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if job.WorkerPool != "coding" || job.PolicyVersion != 1 || job.RepositoryID != "agent-forge" {
		t.Fatalf("resolved job = %#v", job)
	}

	header := http.Header{"Authorization": []string{"Bearer coding-token"}}
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/workers/connect?worker_id=worker-2&slot=0", &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	defer c.CloseNow()
	lease := readMessage(t, c)
	if lease.Type != protocol.MessageLease || lease.Policy == nil || lease.Policy.WorkerPool != "coding" || lease.Task == nil || lease.Task.Repository != "" {
		t.Fatalf("lease = %#v", lease)
	}
}

func TestConfiguredSubmissionAcceptsOmittedOrEmptyScopedChecks(t *testing.T) {
	_, _, server := configuredGate(t)
	for name, tests := range map[string]string{"omitted": "", "empty": `,"tests":[]`} {
		t.Run(name, func(t *testing.T) {
			request := `{"repository_id":"agent-forge","base_sha":"` + strings.Repeat("a", 40) + `","instruction":"change"` + tests + `}`
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", strings.NewReader(request))
			req.Header.Set("Authorization", "Bearer owner")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusCreated {
				t.Fatalf("submit status = %d", response.StatusCode)
			}
		})
	}
}

func TestConfiguredStartupRejectsMalformedRowsBeforeDisconnectOrLease(t *testing.T) {
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner", "FORGE_WORKER_TOKEN": "worker"}
	config, err := ParseConfig([]byte(validGateConfig), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	job, err := s.CreateJob("malformed active row")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.ClaimWorkerSlot("worker-1", 0, "worker-1", "general", "00000000000000000000000000000001", time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := NewConfiguredHandler(s, config, DefaultOptions()); err == nil {
		t.Fatal("configured startup accepted malformed active row")
	}
	worker, err := s.Worker("worker-1")
	if err != nil || !worker.Connected {
		t.Fatalf("startup reached worker disconnect: %#v, %v", worker, err)
	}
	if attempts, err := s.Attempts(job.ID); err != nil || len(attempts) != 0 {
		t.Fatalf("malformed row reached lease path: %#v, %v", attempts, err)
	}
}

func TestConfiguredSubmissionRejectsPathUnknownRepositoryAndOverrides(t *testing.T) {
	_, _, server := configuredGate(t)
	for name, body := range map[string]string{
		"path":     `{"repository":"/private/repository","base_sha":"` + strings.Repeat("a", 40) + `","instruction":"x","tests":[["true"]]}`,
		"unknown":  `{"repository_id":"missing-private-id","base_sha":"` + strings.Repeat("a", 40) + `","instruction":"x","tests":[["true"]]}`,
		"override": `{"repository_id":"agent-forge","worker_pool":"general","base_sha":"` + strings.Repeat("a", 40) + `","instruction":"x","tests":[["true"]]}`,
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", bytes.NewBufferString(body))
			req.Header.Set("Authorization", "Bearer owner")
			response, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d", response.StatusCode)
			}
		})
	}
}

func TestConfiguredWorkerAuthRejectsNoncanonicalSlot(t *testing.T) {
	_, _, server := configuredGate(t)
	header := http.Header{"Authorization": []string{"Bearer general-token"}}
	for _, slot := range []string{"", "00", "2", "-1"} {
		_, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/workers/connect?worker_id=worker-1&slot="+slot, &websocket.DialOptions{HTTPHeader: header})
		if err == nil || response == nil || response.StatusCode != http.StatusUnauthorized {
			t.Fatalf("slot %q accepted: response=%v err=%v", slot, response, err)
		}
	}
}

func submitConfiguredInput(t *testing.T, serverURL, input string) store.Job {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"input": input})
	req, _ := http.NewRequest(http.MethodPost, serverURL+"/v1/jobs", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer owner")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusCreated {
		t.Fatalf("submit status = %d", response.StatusCode)
	}
	var job store.Job
	if err := json.NewDecoder(response.Body).Decode(&job); err != nil {
		t.Fatal(err)
	}
	return job
}

func dialConfiguredSlot(t *testing.T, serverURL string, slot int) *websocket.Conn {
	t.Helper()
	header := http.Header{"Authorization": []string{"Bearer general-token"}}
	c, _, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(serverURL, "http")+"/v1/workers/connect?worker_id=worker-1&slot="+strconv.Itoa(slot), &websocket.DialOptions{HTTPHeader: header})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConfiguredConcurrencyUsesIndependentAuthenticatedSlots(t *testing.T) {
	s, _, server := configuredGate(t)
	first := submitConfiguredInput(t, server.URL, "first")
	second := submitConfiguredInput(t, server.URL, "second")
	third := submitConfiguredInput(t, server.URL, "third")
	slot0, slot1 := dialConfiguredSlot(t, server.URL, 0), dialConfiguredSlot(t, server.URL, 1)
	defer slot0.CloseNow()
	defer slot1.CloseNow()
	lease0, lease1 := readMessage(t, slot0), readMessage(t, slot1)
	if lease0.JobID == lease1.JobID || lease0.Policy == nil || lease1.Policy == nil {
		t.Fatalf("slot leases = %#v %#v", lease0, lease1)
	}
	attempts0, err := s.Attempts(lease0.JobID)
	if err != nil || len(attempts0) != 1 || attempts0[0].Slot != "worker-1" {
		t.Fatalf("slot 0 attempt = %#v, %v", attempts0, err)
	}
	attempts1, err := s.Attempts(lease1.JobID)
	if err != nil || len(attempts1) != 1 || attempts1[0].Slot != "worker-1#1" {
		t.Fatalf("slot 1 attempt = %#v, %v", attempts1, err)
	}
	if job, err := s.Job(third.ID); err != nil || job.Status != "pending" {
		t.Fatalf("third job entered busy slot: %#v, %v", job, err)
	}
	writeMessage(t, slot0, protocol.Message{Type: protocol.MessageResult, JobID: lease0.JobID, AttemptID: lease0.AttemptID, Result: "done"})
	if ack := readMessage(t, slot0); ack.Type != protocol.MessageAck {
		t.Fatalf("ack = %#v", ack)
	}
	if next := readMessage(t, slot0); next.JobID != third.ID {
		t.Fatalf("next lease = %#v; first=%s second=%s", next, first.ID, second.ID)
	}
}

func TestConfiguredDuplicateLiveSlotIsRejected(t *testing.T) {
	s, _, server := configuredGate(t)
	submitConfiguredInput(t, server.URL, "work")
	old := dialConfiguredSlot(t, server.URL, 0)
	defer old.CloseNow()
	lease := readMessage(t, old)
	before, err := s.Worker("worker-1")
	if err != nil {
		t.Fatal(err)
	}
	header := http.Header{"Authorization": []string{"Bearer general-token"}}
	replacement, response, err := websocket.Dial(context.Background(), "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/workers/connect?worker_id=worker-1&slot=0", &websocket.DialOptions{HTTPHeader: header})
	if err == nil || replacement != nil || response == nil || response.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate response = connection %v, response %v, error %v", replacement, response, err)
	}
	writeMessage(t, old, protocol.Message{Type: protocol.MessageHeartbeat, WorkerID: "worker-1", JobID: lease.JobID, AttemptID: lease.AttemptID})
	worker, err := s.Worker("worker-1")
	if err != nil || !worker.Connected || worker.Generation != before.Generation {
		t.Fatalf("duplicate disconnected live owner: %#v, %v", worker, err)
	}
}
