package gate

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"path/filepath"
	"strconv"
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

//go:embed debug/index.html debug/app.css debug/app.js
var debugFiles embed.FS

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
	m.HandleFunc("/v1/debug/jobs", getOnly(x.ownerAuth(x.debugJobs)))
	m.HandleFunc("/v1/debug/jobs/{id}", getOnly(x.ownerAuth(x.debugTimeline)))
	m.HandleFunc("/v1/debug/workers", getOnly(x.ownerAuth(x.debugWorkers)))
	m.HandleFunc("GET /debug", x.debugAsset)
	m.HandleFunc("GET /debug/", x.debugAsset)
	return m
}

func getOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		next(w, r)
	}
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

func debugLimit(r *http.Request) (int, error) {
	if r.URL.Query().Get("limit") == "" {
		return 25, nil
	}
	return strconv.Atoi(r.URL.Query().Get("limit"))
}

func (x *server) debugJobs(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	page, err := x.store.RecentDebugJobs(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

func (x *server) debugWorkers(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	page, err := x.store.RecentDebugWorkers(r.Context(), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

func (x *server) debugTimeline(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	timeline, err := x.store.DebugJobTimeline(r.Context(), r.PathValue("id"), limit, r.URL.Query().Get("cursor"))
	if err != nil {
		status, message := http.StatusInternalServerError, "request failed"
		if errors.Is(err, sql.ErrNoRows) {
			status, message = http.StatusNotFound, "not found"
		} else if errors.Is(err, store.ErrInvalidDebugCursor) {
			status, message = http.StatusBadRequest, "invalid request"
		}
		writeJSON(w, status, map[string]string{"message": message})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, timeline)
}

func (x *server) debugAsset(w http.ResponseWriter, r *http.Request) {
	name, contentType := "debug/index.html", "text/html; charset=utf-8"
	if r.URL.Path == "/debug/app.css" {
		name, contentType = "debug/app.css", "text/css; charset=utf-8"
	} else if r.URL.Path == "/debug/app.js" {
		name, contentType = "debug/app.js", "text/javascript; charset=utf-8"
	} else if r.URL.Path != "/debug" && r.URL.Path != "/debug/" {
		http.NotFound(w, r)
		return
	}
	body, err := debugFiles.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	w.Header().Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
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
