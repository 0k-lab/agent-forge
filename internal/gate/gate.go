package gate

import (
	"context"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type server struct {
	store  *store.Store
	tokens map[string]string
	owner  string
}

func NewHandler(s *store.Store, tokens map[string]string, ownerToken string) http.Handler {
	for workerToken := range tokens {
		if subtle.ConstantTimeCompare([]byte(ownerToken), []byte(workerToken)) == 1 {
			ownerToken = ""
		}
	}
	x := &server{store: s, tokens: tokens, owner: ownerToken}
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/jobs", x.ownerAuth(x.submit))
	m.HandleFunc("GET /v1/jobs/{id}", x.ownerAuth(x.getJob))
	m.HandleFunc("GET /v1/jobs/{id}/events", x.ownerAuth(x.getEvents))
	m.HandleFunc("GET /v1/workers/{id}", x.ownerAuth(x.getWorker))
	m.HandleFunc("GET /v1/workers/connect", x.connect)
	return m
}

func (x *server) ownerAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token := bearerToken(r)
		if x.owner == "" || token == "" || subtle.ConstantTimeCompare([]byte(token), []byte(x.owner)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, prefix) {
		return ""
	}
	return strings.TrimPrefix(header, prefix)
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}
func (x *server) submit(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input string `json:"input"`
		protocol.CodingTask
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&in); err != nil {
		writeErr(w, 400, err)
		return
	}
	var j store.Job
	var err error
	coding := in.Repository != "" || in.BaseSHA != "" || in.Instruction != "" || in.Tests != nil
	if !coding {
		j, err = x.store.CreateJob(in.Input)
	} else {
		if err = validateTask(in.CodingTask); err != nil {
			writeErr(w, 400, err)
			return
		}
		j, err = x.store.CreateCodingJob(in.CodingTask)
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 201, j)
}

func validateTask(task protocol.CodingTask) error {
	if !filepath.IsAbs(task.Repository) || len(task.Repository) > 4096 {
		return errors.New("repository must be an absolute path")
	}
	if len(task.BaseSHA) != 40 {
		return errors.New("base_sha must be a full SHA")
	}
	if _, err := hex.DecodeString(task.BaseSHA); err != nil {
		return errors.New("base_sha must be hexadecimal")
	}
	if task.Instruction == "" || len(task.Instruction) > 65536 || len(task.Tests) == 0 || len(task.Tests) > 32 {
		return errors.New("instruction and scoped tests are required")
	}
	for _, argv := range task.Tests {
		if len(argv) == 0 || len(argv) > 64 {
			return errors.New("invalid scoped test argv")
		}
	}
	if err := protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail); err != nil {
		return err
	}
	return nil
}
func (x *server) getJob(w http.ResponseWriter, r *http.Request) {
	j, err := x.store.Job(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, j)
}
func (x *server) getEvents(w http.ResponseWriter, r *http.Request) {
	e, err := x.store.Events(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, e)
}
func (x *server) getWorker(w http.ResponseWriter, r *http.Request) {
	v, err := x.store.Worker(r.PathValue("id"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	writeJSON(w, 200, v)
}
func (x *server) connect(w http.ResponseWriter, r *http.Request) {
	workerID := r.URL.Query().Get("worker_id")
	token := bearerToken(r)
	authorized := false
	for expected, id := range x.tokens {
		if id == workerID && expected != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
			authorized = true
		}
	}
	if workerID == "" || !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	defer c.CloseNow()
	if err = x.store.SetWorkerConnected(workerID, true); err != nil {
		return
	}
	defer x.store.SetWorkerConnected(workerID, false)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	incoming := make(chan protocol.Message)
	readErr := make(chan error, 1)
	go func() {
		for {
			var m protocol.Message
			if err := wsjson.Read(ctx, c, &m); err != nil {
				readErr <- err
				return
			}
			incoming <- m
		}
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-readErr:
			return
		case m := <-incoming:
			if m.Type != "result" {
				continue
			}
			var err error
			if m.Error != "" {
				_, err = x.store.Fail(m.JobID, m.AttemptID, m.Error)
			} else if m.CandidateSHA != "" {
				_, err = x.store.CompleteCandidate(m.JobID, m.AttemptID, m.CandidateSHA)
			} else {
				_, err = x.store.Complete(m.JobID, m.AttemptID, m.Result)
			}
			ack := protocol.Message{Type: "ack", JobID: m.JobID, AttemptID: m.AttemptID}
			if err != nil {
				ack.Type = "error"
				ack.Error = err.Error()
			}
			if wsjson.Write(ctx, c, ack) != nil {
				return
			}
		case <-ticker.C:
			lease, ok, err := x.store.LeaseNext(workerID)
			if err != nil {
				return
			}
			if !ok {
				continue
			}
			m := protocol.Message{Type: "lease", JobID: lease.JobID, AttemptID: lease.AttemptID, Input: lease.Input, Task: lease.Task}
			if err := wsjson.Write(ctx, c, m); err != nil {
				return
			}
		}
	}
}
