package gate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
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
	store   *store.Store
	tokens  map[string]string
	owner   string
	cursor  debugCursorCodec
	options Options
}

type Options struct {
	Policy            store.RecoveryPolicy
	RecoveryInterval  time.Duration
	LeasePollInterval time.Duration
	Now               func() time.Time
}

func DefaultOptions() Options {
	return Options{Policy: store.DefaultRecoveryPolicy(), RecoveryInterval: time.Second, LeasePollInterval: 100 * time.Millisecond, Now: time.Now}
}

func (o Options) Validate() error {
	if err := o.Policy.Validate(); err != nil {
		return err
	}
	if o.RecoveryInterval <= 0 || o.RecoveryInterval > o.Policy.LeaseTTL {
		return errors.New("recovery interval must be positive and not exceed lease TTL")
	}
	if o.LeasePollInterval <= 0 || o.LeasePollInterval > time.Minute {
		return errors.New("lease poll interval must be positive and at most 1m")
	}
	if o.Now == nil {
		return errors.New("clock is required")
	}
	return nil
}

type recoverySweeper interface {
	SweepExpired(time.Time, store.RecoveryPolicy) error
}

var errRecoveryTickerStopped = errors.New("recovery ticker stopped")

func RunRecovery(ctx context.Context, s *store.Store, options Options) error {
	if err := options.Validate(); err != nil {
		return err
	}
	ticker := time.NewTicker(options.RecoveryInterval)
	defer ticker.Stop()
	return runRecovery(ctx, s, options.Policy, options.Now, ticker.C)
}

func StartRecovery(ctx context.Context, s *store.Store, options Options) (<-chan error, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := s.SweepExpired(options.Now().UTC(), options.Policy); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(options.RecoveryInterval)
	errs := make(chan error, 1)
	go func() {
		defer ticker.Stop()
		errs <- recoveryLoop(ctx, s, options.Policy, ticker.C)
		close(errs)
	}()
	return errs, nil
}

func runRecovery(ctx context.Context, s recoverySweeper, policy store.RecoveryPolicy, now func() time.Time, ticks <-chan time.Time) error {
	if err := s.SweepExpired(now().UTC(), policy); err != nil {
		return err
	}
	return recoveryLoop(ctx, s, policy, ticks)
}

func recoveryLoop(ctx context.Context, s recoverySweeper, policy store.RecoveryPolicy, ticks <-chan time.Time) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case at, ok := <-ticks:
			if !ok {
				return errRecoveryTickerStopped
			}
			if err := s.SweepExpired(at.UTC(), policy); err != nil {
				return err
			}
		}
	}
}

const (
	debugCursorVersion   = 1
	maxDebugCursorLength = 512
	debugJobsPurpose     = "jobs"
	debugWorkersPurpose  = "workers"
)

type debugCursorCodec struct{ key [sha256.Size]byte }

type debugCursorPayload struct {
	Version int    `json:"v"`
	Purpose string `json:"purpose"`
	Stamp   string `json:"stamp"`
	ID      string `json:"id"`
}

func newDebugCursorCodec(key [sha256.Size]byte) debugCursorCodec {
	return debugCursorCodec{key: key}
}

func (c debugCursorCodec) encode(purpose string, position *store.DebugPosition) string {
	if position == nil {
		return ""
	}
	body, _ := json.Marshal(debugCursorPayload{
		Version: debugCursorVersion,
		Purpose: purpose,
		Stamp:   position.At.Format(time.RFC3339Nano),
		ID:      position.ID,
	})
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write(body)
	return base64.RawURLEncoding.EncodeToString(append(body, mac.Sum(nil)...))
}

func (c debugCursorCodec) decode(cursor, purpose string) (*store.DebugPosition, error) {
	if cursor == "" {
		return nil, nil
	}
	if len(cursor) > maxDebugCursorLength {
		return nil, store.ErrInvalidDebugCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(raw) <= sha256.Size {
		return nil, store.ErrInvalidDebugCursor
	}
	body, suppliedMAC := raw[:len(raw)-sha256.Size], raw[len(raw)-sha256.Size:]
	mac := hmac.New(sha256.New, c.key[:])
	_, _ = mac.Write(body)
	if subtle.ConstantTimeCompare(suppliedMAC, mac.Sum(nil)) != 1 {
		return nil, store.ErrInvalidDebugCursor
	}
	var payload debugCursorPayload
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&payload); err != nil || decoder.Decode(&struct{}{}) != io.EOF || payload.Version != debugCursorVersion || payload.Purpose != purpose || payload.ID == "" {
		return nil, store.ErrInvalidDebugCursor
	}
	at, err := time.Parse(time.RFC3339Nano, payload.Stamp)
	if err != nil {
		return nil, store.ErrInvalidDebugCursor
	}
	return &store.DebugPosition{At: at, ID: payload.ID}, nil
}

//go:embed debug/index.html debug/app.css debug/app.js
var debugFiles embed.FS

func NewHandler(s *store.Store, tokens map[string]string, ownerToken string) http.Handler {
	return newHandler(s, tokens, ownerToken, DefaultOptions())
}

func NewHandlerWithOptions(s *store.Store, tokens map[string]string, ownerToken string, options Options) (http.Handler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	return newHandler(s, tokens, ownerToken, options), nil
}

func newHandler(s *store.Store, tokens map[string]string, ownerToken string, options Options) http.Handler {
	for workerToken := range tokens {
		if subtle.ConstantTimeCompare([]byte(ownerToken), []byte(workerToken)) == 1 {
			ownerToken = ""
		}
	}
	x := &server{store: s, tokens: tokens, owner: ownerToken, cursor: newDebugCursorCodec(s.DebugCursorKey(ownerToken)), options: options}
	m := http.NewServeMux()
	m.HandleFunc("POST /v1/jobs", x.ownerAuth(x.submit))
	m.HandleFunc("GET /v1/jobs/{id}", x.ownerAuth(x.getJob))
	m.HandleFunc("GET /v1/jobs/{id}/events", x.ownerAuth(x.getEvents))
	m.HandleFunc("GET /v1/jobs/{id}/attempts/{attempt_id}/evidence", x.ownerAuth(x.getAttemptEvidence))
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
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 {
		return 0, errors.New("invalid limit")
	}
	if limit > 100 {
		limit = 100
	}
	return limit, nil
}

func (x *server) debugJobs(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	position, err := x.cursor.decode(r.URL.Query().Get("cursor"), debugJobsPurpose)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	page, err := x.store.RecentDebugJobs(r.Context(), limit, position)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	page.NextCursor = x.cursor.encode(debugJobsPurpose, page.NextPosition)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

func (x *server) debugWorkers(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	position, err := x.cursor.decode(r.URL.Query().Get("cursor"), debugWorkersPurpose)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	page, err := x.store.RecentDebugWorkers(r.Context(), limit, position)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	page.NextCursor = x.cursor.encode(debugWorkersPurpose, page.NextPosition)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, page)
}

func (x *server) debugTimeline(w http.ResponseWriter, r *http.Request) {
	limit, err := debugLimit(r)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": "invalid request"})
		return
	}
	jobID := r.PathValue("id")
	purpose := "timeline:" + jobID
	position, err := x.cursor.decode(r.URL.Query().Get("cursor"), purpose)
	if err != nil {
		writeDebugError(w, err)
		return
	}
	timeline, err := x.store.DebugJobTimeline(r.Context(), jobID, limit, position)
	if err != nil {
		status, message := http.StatusInternalServerError, "request failed"
		if errors.Is(err, sql.ErrNoRows) {
			status, message = http.StatusNotFound, "not found"
		}
		if errors.Is(err, store.ErrInvalidDebugCursor) {
			writeDebugError(w, err)
		} else {
			writeJSON(w, status, map[string]string{"message": message})
		}
		return
	}
	timeline.NextCursor = x.cursor.encode(purpose, timeline.NextPosition)
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, timeline)
}

func writeDebugError(w http.ResponseWriter, err error) {
	status, message := http.StatusInternalServerError, "request failed"
	if errors.Is(err, store.ErrInvalidDebugCursor) {
		status, message = http.StatusBadRequest, "invalid request"
	}
	writeJSON(w, status, map[string]string{"message": message})
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
	if err := protocol.ValidateBaseSHA(task.BaseSHA); err != nil {
		return err
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
func (x *server) getAttemptEvidence(w http.ResponseWriter, r *http.Request) {
	records, err := x.store.AttemptEvidence(r.PathValue("id"), r.PathValue("attempt_id"))
	if err != nil {
		status, message := http.StatusInternalServerError, "request failed"
		if errors.Is(err, sql.ErrNoRows) {
			status, message = http.StatusNotFound, "not found"
		}
		writeJSON(w, status, map[string]string{"message": message})
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, records)
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
	c.SetReadLimit(protocol.MaxWorkerMessageBytes)
	if err = x.store.SetWorkerConnected(workerID, true); err != nil {
		return
	}
	defer x.store.SetWorkerConnected(workerID, false)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	incoming := make(chan protocol.Message, 1)
	readErr := make(chan error, 1)
	go func() {
		for {
			var m protocol.Message
			if err := readWorkerMessage(ctx, c, &m); err != nil {
				select {
				case readErr <- err:
				case <-ctx.Done():
				}
				return
			}
			select {
			case incoming <- m:
			case <-ctx.Done():
				return
			}
		}
	}()
	ticker := time.NewTicker(x.options.LeasePollInterval)
	defer ticker.Stop()
	var active *store.Lease
	var completed *store.Lease
	reject := func() {
		_ = wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageError, Error: "request failed"})
	}
	for {
		select {
		case <-ctx.Done():
			return
		case err := <-readErr:
			if websocket.CloseStatus(err) == -1 {
				reject()
			}
			return
		case m := <-incoming:
			if completed != nil && m.Type == protocol.MessageHeartbeat && m.JobID == completed.JobID && m.AttemptID == completed.AttemptID {
				if m.WorkerID != workerID || m.Input != "" || m.Task != nil || m.Result != "" || m.CandidateSHA != "" || m.Disposition != "" || m.Error != "" || len(m.Evidence) != 0 {
					reject()
					return
				}
				continue
			}
			if active == nil || m.JobID != active.JobID || m.AttemptID != active.AttemptID {
				reject()
				return
			}
			if m.Type == protocol.MessageHeartbeat {
				if m.WorkerID != workerID || m.Input != "" || m.Task != nil || m.Result != "" || m.CandidateSHA != "" || m.Disposition != "" || m.Error != "" || len(m.Evidence) != 0 || x.store.Heartbeat(m.JobID, m.AttemptID, workerID, x.options.Now().UTC(), x.options.Policy) != nil {
					reject()
					return
				}
				continue
			}
			if m.Type == protocol.MessageEvidence {
				if m.WorkerID != "" || m.Input != "" || m.Task != nil || m.Result != "" || m.CandidateSHA != "" || m.Error != "" || m.Disposition != "" ||
					len(m.Evidence) == 0 || len(m.Evidence) > protocol.MaxEvidenceRecordsPerBatch ||
					x.store.BindEvidenceAt(m.JobID, m.AttemptID, workerID, m.Evidence, x.options.Now().UTC()) != nil {
					reject()
					return
				}
				if wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageAck, JobID: m.JobID, AttemptID: m.AttemptID}) != nil {
					return
				}
				continue
			}
			if m.Type != protocol.MessageResult || m.WorkerID != "" || m.Input != "" || m.Task != nil || len(m.Evidence) != 0 {
				reject()
				return
			}
			at := x.options.Now().UTC()
			var resultErr error
			if m.Error != "" {
				disposition, ok := failureDisposition(m.Error)
				if !ok || m.Disposition != "" && m.Disposition != string(disposition) {
					reject()
					return
				}
				_, resultErr = x.store.FailAt(m.JobID, m.AttemptID, m.Error, disposition, at, x.options.Policy)
			} else if m.Disposition != "" {
				reject()
				return
			} else if m.CandidateSHA != "" {
				_, resultErr = x.store.CompleteCandidateAt(m.JobID, m.AttemptID, m.CandidateSHA, at)
			} else {
				_, resultErr = x.store.CompleteAt(m.JobID, m.AttemptID, m.Result, at)
			}
			if resultErr != nil {
				reject()
				return
			}
			if wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageAck, JobID: m.JobID, AttemptID: m.AttemptID}) != nil {
				return
			}
			completed, active = active, nil
		case <-ticker.C:
			if active != nil {
				continue
			}
			lease, ok, err := x.store.LeaseNextAt(workerID, x.options.Now().UTC(), x.options.Policy)
			if err != nil {
				return
			}
			if !ok {
				continue
			}
			m := protocol.Message{Type: protocol.MessageLease, JobID: lease.JobID, AttemptID: lease.AttemptID, Input: lease.Input, Task: lease.Task}
			if err := wsjson.Write(ctx, c, m); err != nil {
				return
			}
			active = &lease
		}
	}
}

func readWorkerMessage(ctx context.Context, c *websocket.Conn, message *protocol.Message) error {
	typ, body, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("worker message must be text")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(message); err != nil {
		return err
	}
	if decoder.Decode(&struct{}{}) != io.EOF {
		return errors.New("worker message has trailing data")
	}
	return nil
}

func failureDisposition(code string) (store.FailureDisposition, bool) {
	switch code {
	case protocol.FailureInvalidTask, protocol.FailureScopedTest:
		return store.TerminalFailure, true
	case protocol.FailureExecution:
		return store.RetryableFailure, true
	default:
		return "", false
	}
}
