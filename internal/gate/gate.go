package gate

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/githubdelivery"
	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

type server struct {
	store           *store.Store
	tokens          map[string]string
	ownerDigest     [sha256.Size]byte
	ownerConfigured bool
	cursor          debugCursorCodec
	options         Options
	config          *Config
	mu              sync.Mutex
	sessions        map[string]workerSession
}

type workerSession struct {
	generation string
}

type Options struct {
	Policy            store.RecoveryPolicy
	RecoveryInterval  time.Duration
	LeasePollInterval time.Duration
	Now               func() time.Time
	Logger            *slog.Logger
	Context           context.Context
	Delivery          githubdelivery.Options
}

func DefaultOptions() Options {
	return Options{Policy: store.DefaultRecoveryPolicy(), RecoveryInterval: time.Second, LeasePollInterval: 100 * time.Millisecond, Now: time.Now, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), Context: context.Background()}
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

func StartConfiguredRecovery(ctx context.Context, s *store.Store, interval time.Duration, now func() time.Time) (<-chan error, error) {
	if interval <= 0 || interval > time.Hour || now == nil {
		return nil, errors.New("invalid recovery configuration")
	}
	if err := s.SweepExpiredPolicies(now().UTC()); err != nil {
		return nil, err
	}
	ticker := time.NewTicker(interval)
	errs := make(chan error, 1)
	go func() {
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				errs <- ctx.Err()
				close(errs)
				return
			case at := <-ticker.C:
				if err := s.SweepExpiredPolicies(at.UTC()); err != nil {
					errs <- err
					close(errs)
					return
				}
			}
		}
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

func NewConfiguredHandler(s *store.Store, config Config, options Options) (http.Handler, error) {
	if err := options.Validate(); err != nil {
		return nil, err
	}
	if err := s.ValidateActivePolicies(); err != nil {
		return nil, err
	}
	if err := s.ValidateDeliveries(); err != nil {
		return nil, err
	}
	if err := s.MarkWorkersDisconnected(options.Now().UTC()); err != nil {
		return nil, errors.New("worker startup state failed")
	}
	if err := s.SweepExpiredPolicies(options.Now().UTC()); err != nil {
		return nil, errors.New("job recovery failed")
	}
	cursorKey := s.DebugCursorKey(config.ownerToken)
	config.ownerToken = ""
	x := newServerWithOwner(s, nil, config.ownerDigest, true, cursorKey, options)
	x.config = &config
	if config.Delivery != nil {
		if err := s.RecoverDeliveries(options.Now().UTC()); err != nil {
			return nil, errors.New("delivery recovery failed")
		}
		ctx := options.Context
		if ctx == nil {
			ctx = context.Background()
		}
		go x.deliveryLoop(ctx)
	}
	return x.routes(), nil
}

func newServer(s *store.Store, tokens map[string]string, ownerToken string, options Options) *server {
	return newServerWithOwner(s, tokens, sha256.Sum256([]byte(ownerToken)), ownerToken != "", s.DebugCursorKey(ownerToken), options)
}

func newServerWithOwner(s *store.Store, tokens map[string]string, ownerDigest [sha256.Size]byte, ownerConfigured bool, cursorKey [sha256.Size]byte, options Options) *server {
	if options.Logger == nil {
		options.Logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &server{store: s, tokens: tokens, ownerDigest: ownerDigest, ownerConfigured: ownerConfigured, cursor: newDebugCursorCodec(cursorKey), options: options, sessions: map[string]workerSession{}}
}

func (x *server) log(event string, fields ...any) {
	x.options.Logger.Info("gate lifecycle", append([]any{"component", "gate", "event", event}, fields...)...)
}

func (x *server) routes() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
	})
	m.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
	})
	m.HandleFunc("POST /v1/jobs", x.ownerAuth(x.submit))
	m.HandleFunc("GET /v1/jobs/{id}", x.ownerAuth(x.getJob))
	m.HandleFunc("GET /v1/jobs/{id}/status", x.ownerAuth(x.getJobStatus))
	m.HandleFunc("GET /v1/jobs/{id}/result", x.ownerAuth(x.getResult))
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
		w.Header().Set("Cache-Control", "no-store")
		digest := sha256.Sum256([]byte(bearerToken(r)))
		if subtle.ConstantTimeCompare(digest[:], x.ownerDigest[:]) != 1 || !x.ownerConfigured {
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
	if x.config != nil {
		x.submitConfigured(w, r)
		return
	}
	var in struct {
		Input     string            `json:"input"`
		SourceRef configjson.String `json:"source_ref"`
		protocol.CodingTask
	}
	body, decodeErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if decodeErr != nil || configjson.Decode(body, &in) != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid request"))
		return
	}
	if store.ValidateSourceRef(string(in.SourceRef)) != nil {
		writeErr(w, http.StatusBadRequest, errors.New("invalid request"))
		return
	}
	var j store.Job
	var err error
	coding := in.Repository != "" || in.BaseSHA != "" || in.Instruction != "" || in.Tests != nil
	if !coding {
		if in.SourceRef == "" {
			j, err = x.store.CreateJob(in.Input)
		} else {
			j, err = x.store.CreateJobWithSource(in.Input, string(in.SourceRef))
		}
	} else {
		if err = validateTask(in.CodingTask); err != nil {
			writeErr(w, 400, err)
			return
		}
		if in.SourceRef == "" {
			j, err = x.store.CreateCodingJob(in.CodingTask)
		} else {
			j, err = x.store.CreateCodingJobWithSource(in.CodingTask, string(in.SourceRef))
		}
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	x.log("job_submitted", "job_id", j.ID, "status", j.Status)
	writeJSON(w, 201, x.publicJob(j))
}

func (x *server) submitConfigured(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Input             string            `json:"input"`
		RepositoryID      string            `json:"repository_id"`
		BaseSHA           string            `json:"base_sha"`
		Instruction       string            `json:"instruction"`
		Tests             [][]string        `json:"tests"`
		CommitAuthorName  string            `json:"commit_author_name"`
		CommitAuthorEmail string            `json:"commit_author_email"`
		SourceRef         configjson.String `json:"source_ref"`
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil || configjson.Decode(body, &in) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if store.ValidateSourceRef(string(in.SourceRef)) != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if in.RepositoryID == "" {
		if in.Input == "" || in.BaseSHA != "" || in.Instruction != "" || in.Tests != nil || in.CommitAuthorName != "" || in.CommitAuthorEmail != "" {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
			return
		}
		policy := x.config.resolvedPolicy(x.config.DefaultPool, x.config.DefaultExecution, "", "")
		var job store.Job
		if in.SourceRef == "" {
			job, err = x.store.CreateJobWithPolicy(in.Input, policy)
		} else {
			job, err = x.store.CreateJobWithPolicyAndSource(in.Input, policy, string(in.SourceRef))
		}
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
			return
		}
		x.log("job_submitted", "job_id", job.ID, "status", job.Status)
		writeJSON(w, http.StatusCreated, x.publicJob(job))
		return
	}
	var repository *RepositoryRegistration
	for i := range x.config.Repositories {
		if x.config.Repositories[i].ID == in.RepositoryID {
			repository = &x.config.Repositories[i]
			break
		}
	}
	if repository == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	task := protocol.CodingTask{RepositoryID: in.RepositoryID, BaseSHA: in.BaseSHA, Instruction: in.Instruction, Tests: in.Tests, CommitAuthorName: in.CommitAuthorName, CommitAuthorEmail: in.CommitAuthorEmail}
	if err := validateTask(task); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid request"})
		return
	}
	if repository.RepositoryURL != "" {
		prepared, err := provisionPublicRepository(r.Context(), *x.config, *repository, task.BaseSHA)
		if err != nil {
			var preparation preparationError
			if errors.As(err, &preparation) {
				status := http.StatusUnprocessableEntity
				if preparation.retryable {
					status = http.StatusBadGateway
				}
				writeJSON(w, status, map[string]any{"error": "repository preparation failed", "phase": protocol.EvidencePhasePreparation, "reason": preparation.reason, "retryable": preparation.retryable})
				return
			}
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
			return
		}
		task.Repository = prepared
		x.log("repository_prepared", "repository_id", repository.ID, "base_sha", task.BaseSHA)
	}
	policy := x.config.resolvedPolicy(repository.WorkerPool, repository.Execution, repository.ID, repository.DefaultBranch)
	var job store.Job
	if in.SourceRef == "" {
		job, err = x.store.CreateCodingJobWithPolicy(task, policy)
	} else {
		job, err = x.store.CreateCodingJobWithPolicyAndSource(task, policy, string(in.SourceRef))
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "request failed"})
		return
	}
	x.log("job_submitted", "job_id", job.ID, "status", job.Status)
	writeJSON(w, http.StatusCreated, x.publicJob(job))
}

func validateTask(task protocol.CodingTask) error {
	if task.RepositoryID == "" {
		if !filepath.IsAbs(task.Repository) || len(task.Repository) > 4096 {
			return errors.New("repository must be an absolute path")
		}
	} else if task.Repository != "" && (!filepath.IsAbs(task.Repository) || len(task.Repository) > 4096) || !configID.MatchString(task.RepositoryID) {
		return errors.New("invalid repository ID")
	}
	if err := protocol.ValidateBaseSHA(task.BaseSHA); err != nil {
		return err
	}
	if task.Instruction == "" || len(task.Instruction) > 65536 || len(task.Tests) > 32 {
		return errors.New("invalid instruction or scoped tests")
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
	writeJSON(w, 200, x.publicJob(j))
}

func (x *server) getJobStatus(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		writeResultError(w, http.StatusNotFound, CodeJobNotFound, "not found")
		return
	}
	job, err := x.store.DebugJob(r.Context(), id)
	if errors.Is(err, sql.ErrNoRows) {
		writeResultError(w, http.StatusNotFound, CodeJobNotFound, "not found")
		return
	}
	if err != nil {
		writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
		return
	}
	writeJSON(w, http.StatusOK, struct {
		store.DebugJob
		Delivery *safeDelivery `json:"delivery,omitempty"`
	}{job, x.safeDelivery(id)})
}

type ErrorCode string

const (
	CodeJobNotFound    ErrorCode = "job_not_found"
	CodeJobNotTerminal ErrorCode = "job_not_terminal"
	CodeRequestFailed  ErrorCode = "request_failed"
	CodeResultTooLarge ErrorCode = "result_too_large"
)

type APIError struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
}

type safeEvidence struct {
	EvidenceID      string `json:"evidence_id"`
	Phase           string `json:"phase"`
	Reason          string `json:"reason"`
	CheckIndex      *int   `json:"check_index,omitempty"`
	ExitCode        *int   `json:"exit_code,omitempty"`
	DurationMS      int64  `json:"duration_ms"`
	OutputRedacted  bool   `json:"output_redacted,omitempty"`
	OutputTruncated bool   `json:"output_truncated,omitempty"`
	BaseSHA         string `json:"base_sha"`
	CandidateSHA    string `json:"candidate_sha,omitempty"`
	ArgvRedacted    bool   `json:"argv_redacted,omitempty"`
}

type safeAttempt struct {
	ID                 string         `json:"id"`
	Ordinal            int            `json:"ordinal"`
	Status             string         `json:"status"`
	FailureDisposition string         `json:"failure_disposition,omitempty"`
	FailureCode        string         `json:"failure_code,omitempty"`
	CandidateSHA       string         `json:"candidate_sha,omitempty"`
	LeasedAt           time.Time      `json:"leased_at"`
	DeadlineAt         time.Time      `json:"deadline_at"`
	CompletedAt        time.Time      `json:"completed_at,omitempty"`
	Evidence           []safeEvidence `json:"evidence"`
}

type safeResultJob struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	WorkerID     string    `json:"worker_id,omitempty"`
	BaseSHA      string    `json:"base_sha,omitempty"`
	CandidateSHA string    `json:"candidate_sha,omitempty"`
	FailureCode  string    `json:"failure_code,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type safeDelivery struct {
	Phase       string `json:"phase"`
	Branch      string `json:"branch,omitempty"`
	PRURL       string `json:"pr_url,omitempty"`
	CIState     string `json:"ci_state,omitempty"`
	MergeSHA    string `json:"merge_sha,omitempty"`
	FailureCode string `json:"failure_code,omitempty"`
}

type publicJobResponse struct {
	ID            string        `json:"id"`
	Status        string        `json:"status"`
	AttemptID     string        `json:"attempt_id,omitempty"`
	WorkerID      string        `json:"worker_id,omitempty"`
	RepositoryID  string        `json:"repository_id,omitempty"`
	BaseSHA       string        `json:"base_sha,omitempty"`
	WorkerPool    string        `json:"worker_pool,omitempty"`
	PolicyVersion int           `json:"policy_version,omitempty"`
	SourceRef     string        `json:"source_ref,omitempty"`
	CandidateSHA  string        `json:"candidate_sha,omitempty"`
	FailureCode   string        `json:"failure_code,omitempty"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	Delivery      *safeDelivery `json:"delivery,omitempty"`
}

func (x *server) publicJob(job store.Job) publicJobResponse {
	response := publicJobResponse{
		ID:            job.ID,
		Status:        job.Status,
		AttemptID:     job.AttemptID,
		WorkerID:      job.WorkerID,
		WorkerPool:    job.WorkerPool,
		PolicyVersion: job.PolicyVersion,
		SourceRef:     job.SourceRef,
		CandidateSHA:  job.CandidateSHA,
		FailureCode:   safeFailureCode(job.Error),
		CreatedAt:     job.CreatedAt,
		UpdatedAt:     job.UpdatedAt,
		Delivery:      x.safeDelivery(job.ID),
	}
	if job.Task != nil {
		response.RepositoryID = job.Task.RepositoryID
		response.BaseSHA = job.Task.BaseSHA
	}
	return response
}

type jobResult struct {
	Job      safeResultJob      `json:"job"`
	Attempts []safeAttempt      `json:"attempts"`
	Timeline []store.DebugEvent `json:"timeline"`
	Delivery *safeDelivery      `json:"delivery,omitempty"`
}

const (
	maxResultAttempts = 100
	maxResultEvents   = 100
)

func (x *server) getResult(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !validJobID(id) {
		writeResultError(w, http.StatusNotFound, CodeJobNotFound, "not found")
		return
	}
	job, err := x.store.Job(id)
	if errors.Is(err, sql.ErrNoRows) {
		writeResultError(w, http.StatusNotFound, CodeJobNotFound, "not found")
		return
	}
	if err != nil {
		writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
		return
	}
	if job.Status != "succeeded" && job.Status != "failed" {
		writeResultError(w, http.StatusConflict, CodeJobNotTerminal, "job is not terminal")
		return
	}
	attempts, err := x.store.Attempts(id)
	if err != nil || len(attempts) > maxResultAttempts {
		writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
		return
	}
	debugJob, err := x.store.DebugJob(r.Context(), id)
	if err != nil {
		writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
		return
	}
	result := jobResult{Job: safeResultJob{ID: debugJob.ID, Kind: debugJob.Kind, Status: debugJob.Status, AttemptID: debugJob.AttemptID, WorkerID: debugJob.WorkerID, BaseSHA: debugJob.BaseSHA, CandidateSHA: debugJob.CandidateSHA, FailureCode: safeFailureCode(job.Error), CreatedAt: debugJob.CreatedAt, UpdatedAt: debugJob.UpdatedAt}, Attempts: make([]safeAttempt, 0, len(attempts)), Delivery: x.safeDelivery(id)}
	for _, attempt := range attempts {
		evidence, err := x.store.AttemptEvidence(id, attempt.ID)
		if err != nil {
			writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
			return
		}
		safe := safeAttempt{ID: attempt.ID, Ordinal: attempt.Ordinal, Status: attempt.Status, FailureDisposition: attempt.FailureDisposition, FailureCode: safeFailureCode(attempt.FailureCode), CandidateSHA: attempt.CandidateSHA, LeasedAt: attempt.LeasedAt, DeadlineAt: attempt.DeadlineAt, CompletedAt: attempt.CompletedAt, Evidence: make([]safeEvidence, 0, len(evidence))}
		for _, record := range evidence {
			safe.Evidence = append(safe.Evidence, safeEvidence{EvidenceID: record.EvidenceID, Phase: record.Phase, Reason: record.Reason, CheckIndex: record.CheckIndex, ExitCode: record.ExitCode, DurationMS: record.DurationMS, OutputRedacted: record.OutputRedacted, OutputTruncated: record.OutputTruncated, BaseSHA: record.BaseSHA, CandidateSHA: record.CandidateSHA, ArgvRedacted: record.ArgvRedacted})
		}
		result.Attempts = append(result.Attempts, safe)
	}
	timeline, err := x.store.DebugJobTimeline(r.Context(), id, maxResultEvents, nil)
	if err != nil || timeline.NextPosition != nil {
		writeResultError(w, http.StatusInternalServerError, CodeRequestFailed, "request failed")
		return
	}
	result.Timeline = make([]store.DebugEvent, 0, len(timeline.Events))
	for _, event := range timeline.Events {
		switch event.Type {
		case "submitted", "leased", "lease_expired", "retryable_failed", "retry_scheduled", "failed", "succeeded", "delivery_pending", "delivery_phase", "delivery_retry", "delivery_merged", "delivery_failed":
		default:
			continue
		}
		if event.AttemptID != "" && !validJobID(event.AttemptID) {
			event.AttemptID = ""
		}
		result.Timeline = append(result.Timeline, event)
	}
	body, err := json.Marshal(result)
	if err != nil || len(body)+1 > protocol.MaxWorkerMessageBytes {
		writeResultError(w, http.StatusRequestEntityTooLarge, CodeResultTooLarge, "result exceeds limit")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(append(body, '\n'))
}

func safeFailureCode(code string) string {
	switch code {
	case protocol.FailureInvalidTask, protocol.FailureScopedTest, protocol.FailureExecution, "max_attempts_exceeded":
		return code
	}
	if deliveryFailureCodes[code] {
		return code
	}
	return ""
}

var deliveryFailureCodes = map[string]bool{
	"delivery_failed": true, "delivery_registration_changed": true, "delivery_automation_invalid": true,
	"delivery_transient_api": true, "delivery_push_failed": true, "delivery_credential_expired": true,
	"delivery_credential_invalid": true, "delivery_repository_not_found": true, "delivery_repository_not_selected": true,
	"delivery_installation_absent": true, "delivery_permission_missing": true, "delivery_base_drift": true,
	"delivery_branch_conflict": true, "delivery_push_conflict": true, "delivery_pull_request_conflict": true,
	"delivery_head_changed": true, "delivery_base_changed": true, "delivery_ci_no_runs": true,
	"delivery_ci_ambiguous": true, "delivery_ci_failed": true, "delivery_ci_timeout": true,
	"delivery_merge_failed": true, "delivery_publication_invalid": true, "delivery_candidate_ref_mismatch": true,
	"delivery_candidate_parent_mismatch": true, "delivery_candidate_tree_mismatch": true, "delivery_candidate_diff_invalid": true,
	"delivery_local_repository_unsafe": true, "delivery_git_executable_unsafe": true, "delivery_staging_failed": true,
	"delivery_command_output_exceeded": true, "delivery_api_rejected": true, "delivery_request_invalid": true,
	"delivery_state_failed": true,
}

func (x *server) safeDelivery(jobID string) *safeDelivery {
	delivery, err := x.store.Delivery(jobID)
	if err != nil {
		return nil
	}
	phase := delivery.Phase
	if phase != "pending" && phase != "publishing" && phase != "ci" && phase != "merging" && phase != "retry_wait" && phase != "merged" && phase != "failed" {
		return nil
	}
	return &safeDelivery{Phase: phase, Branch: delivery.Branch, PRURL: delivery.PRURL, CIState: delivery.CIState, MergeSHA: delivery.MergeSHA, FailureCode: safeFailureCode(delivery.FailureCode)}
}

func writeResultError(w http.ResponseWriter, status int, code ErrorCode, message string) {
	writeJSON(w, status, map[string]APIError{"error": {Code: code, Message: message}})
}

func validJobID(id string) bool {
	if len(id) != 32 {
		return false
	}
	for i := range len(id) {
		if id[i] < '0' || id[i] > '9' && (id[i] < 'a' || id[i] > 'f') {
			return false
		}
	}
	return true
}

type eventResponse struct {
	ID          int64     `json:"id"`
	JobID       string    `json:"job_id"`
	Kind        string    `json:"kind"`
	At          time.Time `json:"at"`
	Phase       string    `json:"phase,omitempty"`
	FailureCode string    `json:"failure_code,omitempty"`
}

func (x *server) getEvents(w http.ResponseWriter, r *http.Request) {
	events, err := x.store.Events(r.PathValue("id"))
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	response := make([]eventResponse, len(events))
	for i, event := range events {
		response[i] = eventResponse{ID: event.ID, JobID: event.JobID, Kind: event.Kind, At: event.At}
		if strings.HasPrefix(event.Kind, "delivery_") {
			for _, field := range strings.Fields(event.Detail) {
				key, value, ok := strings.Cut(field, "=")
				if key == "phase" && ok && (value == "pending" || value == "publishing" || value == "ci" || value == "merging" || value == "retry_wait" || value == "merged" || value == "failed") {
					response[i].Phase = value
				}
				if key == "failure_code" && ok {
					response[i].FailureCode = safeFailureCode(value)
				}
			}
		}
	}
	writeJSON(w, 200, response)
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
	pool, generation := "", ""
	slotIndex := 0
	effectiveID := workerID
	baseWorkerID := workerID
	if x.config != nil {
		slotText := r.URL.Query().Get("slot")
		parsed, err := strconv.Atoi(slotText)
		var registration WorkerRegistration
		found := false
		digest := sha256.Sum256([]byte(token))
		for _, credential := range x.config.workerTokens {
			if subtle.ConstantTimeCompare(digest[:], credential.digest[:]) == 1 {
				registration, found = credential.registration, true
			}
		}
		if err == nil && strconv.Itoa(parsed) == slotText && parsed >= 0 && found && registration.ID == workerID && parsed < registration.Concurrency && token != "" {
			authorized, pool, slotIndex = true, registration.Pool, parsed
			if parsed > 0 {
				effectiveID = workerID + "#" + slotText
			}
		}
	} else {
		for expected, id := range x.tokens {
			if id == workerID && expected != "" && token != "" && subtle.ConstantTimeCompare([]byte(token), []byte(expected)) == 1 {
				authorized = true
			}
		}
	}
	if workerID == "" || !authorized {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if x.config != nil {
		generation = newSessionGeneration()
		if err := x.store.ClaimWorkerSlot(baseWorkerID, slotIndex, effectiveID, pool, generation, x.options.Now().UTC()); err != nil {
			http.Error(w, "worker slot unavailable", http.StatusConflict)
			return
		}
	}
	c, err := websocket.Accept(w, r, nil)
	if err != nil {
		if x.config != nil {
			_ = x.store.ReleaseWorkerSlot(effectiveID, generation, x.options.Now().UTC())
		}
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(protocol.MaxWorkerMessageBytes)
	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	if x.config != nil {
		x.mu.Lock()
		x.sessions[effectiveID] = workerSession{generation: generation}
		x.mu.Unlock()
		defer func() {
			x.mu.Lock()
			if x.sessions[effectiveID].generation == generation {
				delete(x.sessions, effectiveID)
			}
			x.mu.Unlock()
			_ = x.store.ReleaseWorkerSlot(effectiveID, generation, x.options.Now().UTC())
			x.log("worker_disconnected", "worker_id", effectiveID)
		}()
	} else {
		if err = x.store.SetWorkerConnected(workerID, true); err != nil {
			return
		}
		defer func() {
			_ = x.store.SetWorkerConnected(workerID, false)
			x.log("worker_disconnected", "worker_id", workerID)
		}()
	}
	x.log("worker_connected", "worker_id", effectiveID)
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
				if m.WorkerID != baseWorkerID || m.Input != "" || m.Task != nil || m.Policy != nil || m.Result != "" || m.CandidateSHA != "" || m.Disposition != "" || m.Error != "" || len(m.Evidence) != 0 {
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
				var heartbeatErr error
				if x.config != nil {
					heartbeatErr = x.store.HeartbeatLease(m.JobID, m.AttemptID, effectiveID, generation, x.options.Now().UTC())
				} else {
					heartbeatErr = x.store.Heartbeat(m.JobID, m.AttemptID, workerID, x.options.Now().UTC(), x.options.Policy)
				}
				if m.WorkerID != baseWorkerID || m.Input != "" || m.Task != nil || m.Policy != nil || m.Result != "" || m.CandidateSHA != "" || m.Disposition != "" || m.Error != "" || len(m.Evidence) != 0 || heartbeatErr != nil {
					reject()
					return
				}
				continue
			}
			if m.Type == protocol.MessageEvidence {
				if m.WorkerID != "" || m.Input != "" || m.Task != nil || m.Policy != nil || m.Result != "" || m.CandidateSHA != "" || m.Error != "" || m.Disposition != "" ||
					len(m.Evidence) == 0 || len(m.Evidence) > protocol.MaxEvidenceRecordsPerBatch ||
					x.bindEvidence(m.JobID, m.AttemptID, effectiveID, generation, m.Evidence) != nil {
					reject()
					return
				}
				x.log("evidence_bound", "job_id", m.JobID, "attempt_id", m.AttemptID, "worker_id", effectiveID)
				if wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageAck, JobID: m.JobID, AttemptID: m.AttemptID}) != nil {
					return
				}
				continue
			}
			if m.Type != protocol.MessageResult || m.WorkerID != "" || m.Input != "" || m.Task != nil || m.Policy != nil || len(m.Evidence) != 0 {
				reject()
				return
			}
			at := x.options.Now().UTC()
			var terminalJob store.Job
			var resultErr error
			if m.Error != "" {
				disposition, ok := failureDisposition(m.Error)
				if !ok || m.Disposition != "" && m.Disposition != string(disposition) {
					reject()
					return
				}
				if x.config != nil {
					terminalJob, resultErr = x.store.FailLeaseAt(m.JobID, m.AttemptID, effectiveID, generation, m.Error, disposition, at)
				} else {
					terminalJob, resultErr = x.store.FailAt(m.JobID, m.AttemptID, m.Error, disposition, at, x.options.Policy)
				}
			} else if m.Disposition != "" {
				reject()
				return
			} else if m.CandidateSHA != "" {
				if x.config != nil {
					repository, deliver := RepositoryRegistration{}, false
					if x.config.Delivery != nil && active.Task != nil {
						repository, deliver = x.repository(active.Task.RepositoryID)
						deliver = deliver && repository.RepositoryURL != ""
					}
					if deliver {
						var delivery store.Delivery
						delivery, resultErr = x.deliveryForCandidate(ctx, *active, m.CandidateSHA)
						if resultErr == nil {
							terminalJob, resultErr = x.store.CompleteCandidateDeliveryLeaseAt(m.JobID, m.AttemptID, effectiveID, generation, delivery, at)
						}
					} else {
						terminalJob, resultErr = x.store.CompleteCandidateLeaseAt(m.JobID, m.AttemptID, effectiveID, generation, m.CandidateSHA, at)
					}
				} else {
					terminalJob, resultErr = x.store.CompleteCandidateAt(m.JobID, m.AttemptID, m.CandidateSHA, at)
				}
			} else {
				if x.config != nil {
					terminalJob, resultErr = x.store.CompleteLeaseAt(m.JobID, m.AttemptID, effectiveID, generation, m.Result, at)
				} else {
					terminalJob, resultErr = x.store.CompleteAt(m.JobID, m.AttemptID, m.Result, at)
				}
			}
			if resultErr != nil {
				reject()
				return
			}
			if m.Error != "" {
				event := "terminal_failure"
				if terminalJob.Status == "retry_wait" {
					event = "attempt_retry"
				}
				x.log(event, "job_id", m.JobID, "attempt_id", m.AttemptID, "worker_id", effectiveID, "status", terminalJob.Status, "failure_code", m.Error)
			} else if m.CandidateSHA != "" {
				x.log("terminal_candidate", "job_id", m.JobID, "attempt_id", m.AttemptID, "worker_id", effectiveID, "status", terminalJob.Status, "candidate_sha", m.CandidateSHA)
			} else {
				x.log("terminal_success", "job_id", m.JobID, "attempt_id", m.AttemptID, "worker_id", effectiveID, "status", terminalJob.Status)
			}
			if wsjson.Write(ctx, c, protocol.Message{Type: protocol.MessageAck, JobID: m.JobID, AttemptID: m.AttemptID}) != nil {
				return
			}
			completed, active = active, nil
		case <-ticker.C:
			if active != nil {
				continue
			}
			var lease store.Lease
			var ok bool
			var err error
			if x.config != nil {
				lease, ok, err = x.store.LeaseNextForPool(effectiveID, pool, generation, x.options.Now().UTC())
			} else {
				lease, ok, err = x.store.LeaseNextAt(workerID, x.options.Now().UTC(), x.options.Policy)
			}
			if err != nil {
				return
			}
			if !ok {
				continue
			}
			m := protocol.Message{Type: protocol.MessageLease, JobID: lease.JobID, AttemptID: lease.AttemptID, Input: lease.Input, Task: lease.Task}
			if x.config != nil {
				policy := protocol.ResolvedPolicy(lease.Policy)
				m.Policy = &policy
			}
			if err := wsjson.Write(ctx, c, m); err != nil {
				return
			}
			x.log("attempt_leased", "job_id", lease.JobID, "attempt_id", lease.AttemptID, "worker_id", effectiveID, "status", "leased")
			active = &lease
		}
	}
}

func newSessionGeneration() string {
	var value [16]byte
	if _, err := rand.Read(value[:]); err != nil {
		panic("crypto/rand failed")
	}
	return hex.EncodeToString(value[:])
}

func (x *server) bindEvidence(jobID, attemptID, slot, generation string, evidence []protocol.AttemptEvidence) error {
	if x.config != nil {
		return x.store.BindEvidenceLeaseAt(jobID, attemptID, slot, generation, evidence, x.options.Now().UTC())
	}
	return x.store.BindEvidenceAt(jobID, attemptID, slot, evidence, x.options.Now().UTC())
}

func readWorkerMessage(ctx context.Context, c *websocket.Conn, message *protocol.Message) error {
	typ, body, err := c.Read(ctx)
	if err != nil {
		return err
	}
	if typ != websocket.MessageText {
		return errors.New("worker message must be text")
	}
	return configjson.Decode(body, message)
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
