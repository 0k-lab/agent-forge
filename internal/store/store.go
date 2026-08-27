package store

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"agent-forge/internal/protocol"
	_ "modernc.org/sqlite"
)

type Store struct {
	db                *sql.DB
	debugCursorSecret [32]byte
	lock              io.Closer
	closeOnce         sync.Once
	closeErr          error
}

var initializationMu sync.Mutex

type RecoveryPolicy struct {
	LeaseTTL         time.Duration
	BaseRetryBackoff time.Duration
	MaxAttempts      int
}

type FailureDisposition string

const (
	TerminalFailure  FailureDisposition = "terminal"
	RetryableFailure FailureDisposition = "retryable"
)

const (
	defaultLeaseTTL         = 30 * time.Second
	defaultBaseRetryBackoff = time.Second
	defaultMaxAttempts      = 3
)

func DefaultRecoveryPolicy() RecoveryPolicy {
	return RecoveryPolicy{defaultLeaseTTL, defaultBaseRetryBackoff, defaultMaxAttempts}
}

func (p RecoveryPolicy) Validate() error {
	if p.LeaseTTL <= 0 || p.LeaseTTL > 24*time.Hour {
		return errors.New("lease TTL must be greater than 0 and at most 24h")
	}
	if p.BaseRetryBackoff <= 0 || p.BaseRetryBackoff > 24*time.Hour {
		return errors.New("retry backoff must be greater than 0 and at most 24h")
	}
	if p.MaxAttempts < 1 || p.MaxAttempts > 100 {
		return errors.New("max attempts must be between 1 and 100")
	}
	return nil
}

type Job struct {
	ID            string               `json:"id"`
	Input         string               `json:"input,omitempty"`
	Task          *protocol.CodingTask `json:"task,omitempty"`
	Status        string               `json:"status"`
	AttemptID     string               `json:"attempt_id,omitempty"`
	WorkerID      string               `json:"worker_id,omitempty"`
	Result        string               `json:"result,omitempty"`
	CandidateSHA  string               `json:"candidate_sha,omitempty"`
	Error         string               `json:"error,omitempty"`
	WorkerPool    string               `json:"worker_pool,omitempty"`
	PolicyVersion int                  `json:"policy_version,omitempty"`
	CreatedAt     time.Time            `json:"created_at"`
	UpdatedAt     time.Time            `json:"updated_at"`
}

type Lease struct {
	JobID      string               `json:"job_id"`
	AttemptID  string               `json:"attempt_id"`
	Input      string               `json:"input,omitempty"`
	Task       *protocol.CodingTask `json:"task,omitempty"`
	WorkerPool string               `json:"worker_pool,omitempty"`
	Slot       string               `json:"slot,omitempty"`
	Policy     ResolvedPolicy       `json:"policy"`
}
type Attempt struct {
	ID                 string    `json:"id"`
	JobID              string    `json:"job_id"`
	Ordinal            int       `json:"ordinal"`
	WorkerID           string    `json:"worker_id"`
	WorkerPool         string    `json:"worker_pool,omitempty"`
	Slot               string    `json:"slot,omitempty"`
	PolicyVersion      int       `json:"policy_version,omitempty"`
	Status             string    `json:"status"`
	LeasedAt           time.Time `json:"leased_at"`
	DeadlineAt         time.Time `json:"deadline_at"`
	CompletedAt        time.Time `json:"completed_at,omitempty"`
	FailureDisposition string    `json:"failure_disposition,omitempty"`
	FailureCode        string    `json:"failure_code,omitempty"`
	Result             string    `json:"result,omitempty"`
	CandidateSHA       string    `json:"candidate_sha,omitempty"`
}
type Event struct {
	ID     int64     `json:"id"`
	JobID  string    `json:"job_id"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}
type Worker struct {
	ID         string    `json:"id"`
	Connected  bool      `json:"connected"`
	LastSeen   time.Time `json:"last_seen"`
	BaseID     string    `json:"base_id,omitempty"`
	Slot       int       `json:"slot,omitempty"`
	Pool       string    `json:"pool,omitempty"`
	Generation string    `json:"-"`
}

func Open(path string) (_ *Store, retErr error) {
	initializationMu.Lock()
	defer initializationMu.Unlock()
	dsn, err := parseSQLiteDSN(path)
	if err != nil {
		return nil, err
	}
	defer func() {
		if retErr != nil && !errors.Is(retErr, ErrInsecureDatabase) && !errors.Is(retErr, ErrAlreadyOwned) {
			retErr = ErrDatabaseOpen
		}
	}()
	var lock io.Closer
	if !dsn.memory {
		lock, err = acquireSQLiteLock(dsn.path)
		if err != nil {
			return nil, err
		}
		defer func() {
			if retErr != nil {
				_ = lock.Close()
			}
		}()
	}
	postOpen := func() error { return nil }
	if !dsn.memory {
		postOpen, err = prepareSQLiteFile(dsn)
		if err != nil {
			return nil, err
		}
	}
	db, err := sql.Open("sqlite", dsn.sqliteDSN)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db, lock: lock}
	if _, err = db.Exec(`PRAGMA busy_timeout=5000`); err != nil {
		db.Close()
		return nil, err
	}
	if err = migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	candidate := make([]byte, len(s.debugCursorSecret))
	if _, err = rand.Read(candidate); err != nil {
		db.Close()
		return nil, err
	}
	if _, err = db.Exec(`INSERT OR IGNORE INTO metadata(key,value) VALUES('debug_cursor_secret',?)`, candidate); err != nil {
		db.Close()
		return nil, err
	}
	var secret []byte
	if err = db.QueryRow(`SELECT value FROM metadata WHERE key='debug_cursor_secret'`).Scan(&secret); err != nil {
		db.Close()
		return nil, err
	}
	if len(secret) != len(s.debugCursorSecret) {
		db.Close()
		return nil, errors.New("invalid debug cursor secret")
	}
	copy(s.debugCursorSecret[:], secret)
	if err = postOpen(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error {
	s.closeOnce.Do(func() {
		s.closeErr = s.db.Close()
		if s.lock != nil {
			s.closeErr = errors.Join(s.closeErr, s.lock.Close())
		}
	})
	return s.closeErr
}
func (s *Store) DebugCursorKey(ownerToken string) [sha256.Size]byte {
	mac := hmac.New(sha256.New, s.debugCursorSecret[:])
	_, _ = mac.Write([]byte("agent-forge/debug-cursor/key/v1\x00" + ownerToken))
	var key [sha256.Size]byte
	copy(key[:], mac.Sum(nil))
	return key
}
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) CreateJob(input string) (Job, error) {
	return s.createJob(input, nil)
}

func (s *Store) CreateCodingJob(task protocol.CodingTask) (Job, error) {
	if err := protocol.ValidateBaseSHA(task.BaseSHA); err != nil {
		return Job{}, err
	}
	return s.createJob("", &task)
}

func (s *Store) CreateJobWithPolicy(input string, policy ResolvedPolicy) (Job, error) {
	if policy.Execution.RepositoryID != "" {
		return Job{}, errors.New("invalid non-coding policy")
	}
	return s.createJobWithPolicy(input, nil, &policy)
}

func (s *Store) CreateCodingJobWithPolicy(task protocol.CodingTask, policy ResolvedPolicy) (Job, error) {
	if err := validateStoredTask(task, policy); err != nil {
		return Job{}, err
	}
	return s.createJobWithPolicy("", &task, &policy)
}

func (s *Store) createJob(input string, task *protocol.CodingTask) (Job, error) {
	return s.createJobWithPolicy(input, task, nil)
}

func (s *Store) createJobWithPolicy(input string, task *protocol.CodingTask, policy *ResolvedPolicy) (Job, error) {
	j := Job{ID: newID(), Input: input, Task: task, Status: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	taskJSON := ""
	if task != nil {
		body, err := json.Marshal(task)
		if err != nil {
			return Job{}, err
		}
		taskJSON = string(body)
	}
	var workerPool any
	var policyVersion any
	var policyBytes any
	if policy != nil {
		body, err := CanonicalPolicy(*policy)
		if err != nil {
			return Job{}, err
		}
		workerPool, policyVersion, policyBytes = policy.WorkerPool, policy.Version, body
		j.WorkerPool, j.PolicyVersion = policy.WorkerPool, policy.Version
	}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	stamp := j.CreatedAt.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO jobs(id,input,task_json,status,created_at,updated_at,worker_pool,policy_version,resolved_policy) VALUES(?,?,?,?,?,?,?,?,?)`, j.ID, j.Input, taskJSON, j.Status, stamp, stamp, workerPool, policyVersion, policyBytes); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, j.ID, "submitted", "job accepted", stamp); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

func (s *Store) ClaimWorkerSlot(baseID string, slotIndex int, effectiveID, pool, generation string, at time.Time) error {
	if !policyID.MatchString(baseID) || slotIndex < 0 || slotIndex > 63 || effectiveID == "" || len(effectiveID) > 80 || !policyID.MatchString(pool) || !validSessionGeneration(generation) {
		return errors.New("invalid worker session")
	}
	result, err := s.db.Exec(`INSERT INTO workers(id,connected,last_seen,base_worker_id,slot,worker_pool,generation) VALUES(?,1,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET connected=1,last_seen=excluded.last_seen,base_worker_id=excluded.base_worker_id,slot=excluded.slot,worker_pool=excluded.worker_pool,generation=excluded.generation WHERE workers.connected=0`, effectiveID, at.UTC().Format(time.RFC3339Nano), baseID, slotIndex, pool, generation)
	if err != nil {
		return err
	}
	changed, err := result.RowsAffected()
	if err != nil || changed != 1 {
		return errors.New("worker slot unavailable")
	}
	return nil
}

func (s *Store) ReleaseWorkerSlot(effectiveID, generation string, at time.Time) error {
	if !validSessionGeneration(generation) {
		return errors.New("invalid worker session")
	}
	_, err := s.db.Exec(`UPDATE workers SET connected=0,last_seen=? WHERE id=? AND generation=?`, at.UTC().Format(time.RFC3339Nano), effectiveID, generation)
	return err
}

func (s *Store) MarkWorkersDisconnected(at time.Time) error {
	_, err := s.db.Exec(`UPDATE workers SET connected=0,last_seen=? WHERE connected=1`, at.UTC().Format(time.RFC3339Nano))
	return err
}

func (s *Store) LeaseNextForPool(slot, pool, generation string, at time.Time) (Lease, bool, error) {
	if slot == "" || !policyID.MatchString(pool) || !validSessionGeneration(generation) {
		return Lease{}, false, errors.New("invalid worker session")
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Lease{}, false, err
	}
	defer tx.Rollback()
	var live int
	if err := tx.QueryRow(`SELECT 1 FROM workers WHERE id=? AND connected=1 AND worker_pool=? AND generation=?`, slot, pool, generation).Scan(&live); err != nil {
		return Lease{}, false, errors.New("worker session is not active")
	}
	var id, input, taskJSON string
	var policyVersion int
	var policyBytes []byte
	err = tx.QueryRow(`SELECT id,input,task_json,policy_version,resolved_policy FROM jobs
		WHERE worker_pool=? AND (status='pending' OR status='retry_wait' AND retry_at<=?)
		AND NOT EXISTS (SELECT 1 FROM attempts WHERE status='leased' AND slot=?)
		ORDER BY created_at,id LIMIT 1`, pool, at.UnixNano(), slot).Scan(&id, &input, &taskJSON, &policyVersion, &policyBytes)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, tx.Commit()
	}
	if err != nil {
		return Lease{}, false, err
	}
	policy, err := DecodeCanonicalPolicy(policyBytes)
	if err != nil || policy.Version != policyVersion || policy.WorkerPool != pool {
		return Lease{}, false, errors.New("corrupt resolved policy")
	}
	lease := Lease{JobID: id, Input: input, WorkerPool: pool, Slot: slot, Policy: policy}
	if taskJSON != "" {
		lease.Task = new(protocol.CodingTask)
		decoder := json.NewDecoder(strings.NewReader(taskJSON))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(lease.Task); err != nil || decoder.Decode(&struct{}{}) != io.EOF || validateStoredTask(*lease.Task, policy) != nil {
			return Lease{}, false, errors.New("corrupt task")
		}
	}
	attempt := newID()
	lease.AttemptID = attempt
	stamp := at.Format(time.RFC3339Nano)
	var ordinal int
	if err = tx.QueryRow(`SELECT COALESCE(MAX(ordinal),0)+1 FROM attempts WHERE job_id=?`, id).Scan(&ordinal); err != nil {
		return Lease{}, false, err
	}
	if ordinal > policy.MaxAttempts {
		return Lease{}, false, errors.New("corrupt retry budget")
	}
	result, err := tx.Exec(`INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at,worker_pool,slot,session_generation,policy_version,resolved_policy)
		SELECT ?,id,?,?,'leased',?,?,worker_pool,?,?,policy_version,resolved_policy FROM jobs WHERE id=? AND worker_pool=? AND policy_version=? AND resolved_policy=?`, attempt, ordinal, slot, at.UnixNano(), at.Add(time.Duration(policy.LeaseTTLNanos)).UnixNano(), slot, generation, id, pool, policyVersion, policyBytes)
	if err != nil {
		return Lease{}, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Lease{}, false, err
		}
		return Lease{}, false, errors.New("policy copy failed")
	}
	result, err = tx.Exec(`UPDATE jobs SET status='leased',attempt_id=?,worker_id=?,retry_at=0,updated_at=? WHERE id=? AND worker_pool=? AND (status='pending' OR status='retry_wait' AND retry_at<=?)`, attempt, slot, stamp, id, pool, at.UnixNano())
	if err != nil {
		return Lease{}, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Lease{}, false, err
		}
		return Lease{}, false, errors.New("job lease failed")
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, id, "leased", fmt.Sprintf("attempt=%s ordinal=%d lease_expires=%s", attempt, ordinal, at.Add(time.Duration(policy.LeaseTTLNanos)).Format(time.RFC3339Nano)), stamp); err != nil {
		return Lease{}, false, err
	}
	return lease, true, tx.Commit()
}

func (s *Store) LeaseNext(workerID string) (Lease, bool, error) {
	return s.LeaseNextAt(workerID, time.Now().UTC(), DefaultRecoveryPolicy())
}

func (s *Store) LeaseNextAt(workerID string, at time.Time, policy RecoveryPolicy) (Lease, bool, error) {
	if workerID == "" {
		return Lease{}, false, errors.New("worker ID is required")
	}
	if err := policy.Validate(); err != nil {
		return Lease{}, false, err
	}
	at = at.UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Lease{}, false, err
	}
	defer tx.Rollback()
	if err := reconcileRetryBudget(tx, at, policy); err != nil {
		return Lease{}, false, err
	}
	var id, input, taskJSON string
	err = tx.QueryRow(`SELECT id,input,task_json FROM jobs
		WHERE (status='pending' OR (status='retry_wait' AND retry_at<=?
			AND EXISTS (SELECT 1 FROM attempts current WHERE current.id=jobs.attempt_id AND current.job_id=jobs.id AND current.ordinal<?)))
		AND (SELECT COALESCE(MAX(ordinal),0)+1 FROM attempts WHERE job_id=jobs.id)<=?
		AND NOT EXISTS (SELECT 1 FROM attempts WHERE status='leased' AND worker_id=?)
		ORDER BY created_at,id LIMIT 1`, at.UnixNano(), policy.MaxAttempts, policy.MaxAttempts, workerID).Scan(&id, &input, &taskJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, tx.Commit()
	}
	if err != nil {
		return Lease{}, false, err
	}
	attempt := newID()
	stamp := at.Format(time.RFC3339Nano)
	var ordinal int
	if err = tx.QueryRow(`SELECT COALESCE(MAX(ordinal),0)+1 FROM attempts WHERE job_id=?`, id).Scan(&ordinal); err != nil {
		return Lease{}, false, err
	}
	if ordinal > policy.MaxAttempts {
		return Lease{}, false, errors.New("job exceeds current retry budget")
	}
	if _, err = tx.Exec(`INSERT INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at) VALUES(?,?,?,?, 'leased',?,?)`, attempt, id, ordinal, workerID, at.UnixNano(), at.Add(policy.LeaseTTL).UnixNano()); err != nil {
		return Lease{}, false, err
	}
	res, err := tx.Exec(`UPDATE jobs SET status='leased',attempt_id=?,worker_id=?,retry_at=0,updated_at=? WHERE id=? AND (status='pending' OR (status='retry_wait' AND retry_at<=?))`, attempt, workerID, stamp, id, at.UnixNano())
	if err != nil {
		return Lease{}, false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Lease{}, false, err
	}
	if n != 1 {
		return Lease{}, false, fmt.Errorf("job lease update affected %d rows", n)
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, id, "leased", fmt.Sprintf("attempt=%s ordinal=%d lease_expires=%s", attempt, ordinal, at.Add(policy.LeaseTTL).Format(time.RFC3339Nano)), stamp); err != nil {
		return Lease{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Lease{}, false, err
	}
	lease := Lease{JobID: id, AttemptID: attempt, Input: input}
	if taskJSON != "" {
		lease.Task = new(protocol.CodingTask)
		if err := json.Unmarshal([]byte(taskJSON), lease.Task); err != nil {
			return Lease{}, false, err
		}
	}
	return lease, true, nil
}

func (s *Store) SweepExpired(at time.Time, policy RecoveryPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	at = at.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	if err = reconcileRetryBudget(tx, at, policy); err != nil {
		tx.Rollback()
		return err
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	rows, err := s.db.Query(`SELECT id,job_id,ordinal,deadline_at FROM attempts WHERE status='leased' AND deadline_at<=? ORDER BY deadline_at,id`, at.UnixNano())
	if err != nil {
		return err
	}
	type dueAttempt struct {
		id, jobID string
		ordinal   int
		deadline  int64
	}
	var due []dueAttempt
	for rows.Next() {
		var item dueAttempt
		if err := rows.Scan(&item.id, &item.jobID, &item.ordinal, &item.deadline); err != nil {
			rows.Close()
			return err
		}
		due = append(due, item)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, item := range due {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		res, err := tx.Exec(`UPDATE attempts SET status='expired',completed_at=? WHERE id=? AND job_id=? AND status='leased' AND deadline_at<=?`, at.UnixNano(), item.id, item.jobID, at.UnixNano())
		if err != nil {
			tx.Rollback()
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			tx.Rollback()
			return err
		}
		if n == 0 {
			tx.Rollback()
			continue
		}
		if n != 1 {
			tx.Rollback()
			return fmt.Errorf("attempt expiry update affected %d rows", n)
		}
		detail := fmt.Sprintf("attempt=%s ordinal=%d lease_expires=%s", item.id, item.ordinal, time.Unix(0, item.deadline).UTC().Format(time.RFC3339Nano))
		if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, item.jobID, "lease_expired", detail, at.Format(time.RFC3339Nano)); err == nil {
			err = scheduleRetry(tx, item.jobID, item.id, at, policy)
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func reconcileRetryBudget(tx *sql.Tx, at time.Time, policy RecoveryPolicy) error {
	rows, err := tx.Query(`SELECT j.id,j.attempt_id,a.ordinal,a.status,
		(SELECT COALESCE(MAX(ordinal),0) FROM attempts WHERE job_id=j.id)
		FROM jobs j LEFT JOIN attempts a ON a.id=j.attempt_id AND a.job_id=j.id
		WHERE j.status='retry_wait' ORDER BY j.created_at,j.id`)
	if err != nil {
		return err
	}
	type waitingJob struct {
		id, attemptID, attemptStatus string
		ordinal, maxOrdinal          int
	}
	var waiting []waitingJob
	for rows.Next() {
		var job waitingJob
		var ordinal sql.NullInt64
		var status sql.NullString
		if err := rows.Scan(&job.id, &job.attemptID, &ordinal, &status, &job.maxOrdinal); err != nil {
			rows.Close()
			return err
		}
		if !ordinal.Valid || !status.Valid {
			rows.Close()
			return fmt.Errorf("retry_wait job %s has no current attempt", job.id)
		}
		job.ordinal = int(ordinal.Int64)
		job.attemptStatus = status.String
		waiting = append(waiting, job)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	stamp := at.Format(time.RFC3339Nano)
	for _, job := range waiting {
		if job.ordinal != job.maxOrdinal || (job.attemptStatus != "retryable_failed" && job.attemptStatus != "expired") {
			return fmt.Errorf("retry_wait job %s has inconsistent current attempt", job.id)
		}
		if job.ordinal < policy.MaxAttempts {
			continue
		}
		res, err := tx.Exec(`UPDATE jobs SET status='failed',worker_id='',retry_at=0,error_text='max_attempts_exceeded',updated_at=?
			WHERE id=? AND status='retry_wait' AND attempt_id=?
			AND EXISTS (SELECT 1 FROM attempts WHERE id=? AND job_id=? AND ordinal=? AND status=?)`,
			stamp, job.id, job.attemptID, job.attemptID, job.id, job.ordinal, job.attemptStatus)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n != 1 {
			return fmt.Errorf("job retry reconciliation affected %d rows", n)
		}
		detail := fmt.Sprintf("attempt=%s ordinal=%d disposition=terminal failure_code=max_attempts_exceeded", job.attemptID, job.ordinal)
		res, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, job.id, "failed", detail, stamp)
		if err != nil {
			return err
		}
		if n, err = res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("job retry reconciliation event affected %d rows", n)
		}
	}
	return nil
}

func (s *Store) Heartbeat(jobID, attemptID, workerID string, at time.Time, policy RecoveryPolicy) error {
	if err := policy.Validate(); err != nil {
		return err
	}
	at = at.UTC()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.Exec(`UPDATE attempts SET deadline_at=?
		WHERE id=? AND job_id=? AND worker_id=? AND status='leased' AND deadline_at>?
		AND EXISTS (SELECT 1 FROM jobs WHERE id=? AND status='leased' AND attempt_id=? AND worker_id=?)`,
		at.Add(policy.LeaseTTL).UnixNano(), attemptID, jobID, workerID, at.UnixNano(), jobID, attemptID, workerID)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n != 1 {
		return errors.New("attempt is not active")
	}
	res, err = tx.Exec(`UPDATE jobs SET updated_at=? WHERE id=? AND status='leased' AND attempt_id=? AND worker_id=?`, at.Format(time.RFC3339Nano), jobID, attemptID, workerID)
	if err != nil {
		return err
	}
	if n, err = res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("job heartbeat update affected %d rows", n)
	}
	return tx.Commit()
}

func scheduleRetry(tx *sql.Tx, jobID, attemptID string, at time.Time, policy RecoveryPolicy) error {
	var ordinal int
	if err := tx.QueryRow(`SELECT ordinal FROM attempts WHERE id=? AND job_id=?`, attemptID, jobID).Scan(&ordinal); err != nil {
		return err
	}
	stamp := at.Format(time.RFC3339Nano)
	if ordinal >= policy.MaxAttempts {
		res, err := tx.Exec(`UPDATE jobs SET status='failed',worker_id='',error_text='max_attempts_exceeded',updated_at=? WHERE id=? AND attempt_id=? AND status='leased'`, stamp, jobID, attemptID)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil || n != 1 {
			if err != nil {
				return err
			}
			return fmt.Errorf("job retry update affected %d rows", n)
		}
		_, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "failed", fmt.Sprintf("attempt=%s ordinal=%d disposition=terminal failure_code=max_attempts_exceeded", attemptID, ordinal), stamp)
		return err
	}
	backoff := policy.BaseRetryBackoff
	for i := 1; i < ordinal; i++ {
		if backoff > 24*time.Hour/2 {
			backoff = 24 * time.Hour
			break
		}
		backoff *= 2
	}
	retryAt := at.Add(backoff)
	res, err := tx.Exec(`UPDATE jobs SET status='retry_wait',worker_id='',retry_at=?,updated_at=? WHERE id=? AND attempt_id=? AND status='leased'`, retryAt.UnixNano(), stamp, jobID, attemptID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf("job retry update affected %d rows", n)
	}
	_, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "retry_scheduled", fmt.Sprintf("attempt=%s ordinal=%d retry_at=%s", attemptID, ordinal, retryAt.Format(time.RFC3339Nano)), stamp)
	return err
}

func (s *Store) Complete(jobID, attemptID, result string) (Job, error) {
	return s.CompleteAt(jobID, attemptID, result, time.Now().UTC())
}

func (s *Store) CompleteAt(jobID, attemptID, result string, at time.Time) (Job, error) {
	return s.terminal(jobID, attemptID, "succeeded", result, "", "", at.UTC())
}

func (s *Store) CompleteCandidate(jobID, attemptID, candidateSHA string) (Job, error) {
	return s.CompleteCandidateAt(jobID, attemptID, candidateSHA, time.Now().UTC())
}

func (s *Store) CompleteCandidateAt(jobID, attemptID, candidateSHA string, at time.Time) (Job, error) {
	if len(candidateSHA) != 40 {
		return Job{}, errors.New("candidate SHA must be full length")
	}
	if _, err := hex.DecodeString(candidateSHA); err != nil {
		return Job{}, errors.New("candidate SHA is not hexadecimal")
	}
	return s.terminal(jobID, attemptID, "succeeded", "", candidateSHA, "", at.UTC())
}

func (s *Store) Fail(jobID, attemptID, code string) (Job, error) {
	return s.FailAt(jobID, attemptID, code, TerminalFailure, time.Now().UTC(), DefaultRecoveryPolicy())
}

func (s *Store) FailAt(jobID, attemptID, code string, disposition FailureDisposition, at time.Time, policy RecoveryPolicy) (Job, error) {
	if code == "" || len(code) > 64 {
		return Job{}, errors.New("invalid failure code")
	}
	if err := policy.Validate(); err != nil {
		return Job{}, err
	}
	if disposition != TerminalFailure && disposition != RetryableFailure {
		return Job{}, errors.New("invalid failure disposition")
	}
	if disposition == RetryableFailure {
		return s.retryableFailure(jobID, attemptID, code, at.UTC(), policy)
	}
	return s.terminal(jobID, attemptID, "terminal_failed", "", "", code, at.UTC())
}

func (s *Store) retryableFailure(jobID, attemptID, code string, at time.Time, policy RecoveryPolicy) (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	var deadline int64
	err = tx.QueryRow(`SELECT a.deadline_at FROM attempts a JOIN jobs j ON j.id=a.job_id
		WHERE a.id=? AND a.job_id=? AND a.status='leased' AND j.status='leased' AND j.attempt_id=a.id`, attemptID, jobID).Scan(&deadline)
	if err != nil {
		return Job{}, errors.New("attempt is not active")
	}
	if at.UnixNano() >= deadline {
		return Job{}, errors.New("attempt lease expired")
	}
	res, err := tx.Exec(`UPDATE attempts SET status='retryable_failed',completed_at=?,failure_disposition='retryable',failure_code=? WHERE id=? AND job_id=? AND status='leased'`, at.UnixNano(), code, attemptID, jobID)
	if err != nil {
		return Job{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return Job{}, err
	}
	if n != 1 {
		return Job{}, errors.New("attempt is not active")
	}
	stamp := at.Format(time.RFC3339Nano)
	var ordinal int
	if err = tx.QueryRow(`SELECT ordinal FROM attempts WHERE id=? AND job_id=?`, attemptID, jobID).Scan(&ordinal); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "retryable_failed", fmt.Sprintf("attempt=%s ordinal=%d disposition=retryable failure_code=%s", attemptID, ordinal, code), stamp); err != nil {
		return Job{}, err
	}
	if err = scheduleRetry(tx, jobID, attemptID, at, policy); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return s.Job(jobID)
}

func (s *Store) terminal(jobID, attemptID, attemptStatus, result, candidateSHA, failure string, at time.Time) (Job, error) {
	return s.terminalOwned(jobID, attemptID, attemptStatus, result, candidateSHA, failure, at, "", "", false)
}

func (s *Store) terminalOwned(jobID, attemptID, attemptStatus, result, candidateSHA, failure string, at time.Time, slot, generation string, enforceOwnership bool) (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	if enforceOwnership {
		if _, err := ownedPolicy(tx, jobID, attemptID, slot, generation); err != nil {
			return Job{}, err
		}
	}
	j, err := scanJob(tx.QueryRow(`SELECT id,input,task_json,status,attempt_id,worker_id,result,candidate_sha,error_text,created_at,updated_at,worker_pool,policy_version FROM jobs WHERE id=?`, jobID))
	if err != nil {
		return Job{}, err
	}
	if j.AttemptID != attemptID {
		return Job{}, errors.New("attempt does not own job")
	}
	jobStatus := "failed"
	if attemptStatus == "succeeded" {
		jobStatus = "succeeded"
	}
	disposition := ""
	if attemptStatus == "terminal_failed" {
		disposition = "terminal"
	}
	if attemptStatus == "succeeded" && (j.Task != nil) != (candidateSHA != "") {
		return Job{}, errors.New("result kind does not match job kind")
	}
	if candidateSHA != "" {
		var conflicts int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM attempt_evidence WHERE job_id=? AND attempt_id=? AND (candidate_sha<>'' AND candidate_sha<>? OR phase='scoped_check' AND candidate_sha<>?)`, jobID, attemptID, candidateSHA, candidateSHA).Scan(&conflicts); err != nil {
			return Job{}, err
		}
		if conflicts != 0 {
			return Job{}, errors.New("candidate SHA conflicts with evidence")
		}
	}
	if j.Status == "succeeded" || j.Status == "failed" {
		if j.Status != jobStatus || j.Result != result || j.CandidateSHA != candidateSHA || j.Error != failure {
			return Job{}, errors.New("result is immutable")
		}
		var storedStatus, storedDisposition, storedFailure, storedResult, storedCandidate string
		if err := tx.QueryRow(`SELECT status,failure_disposition,failure_code,result,candidate_sha FROM attempts WHERE id=? AND job_id=?`, attemptID, jobID).
			Scan(&storedStatus, &storedDisposition, &storedFailure, &storedResult, &storedCandidate); err != nil ||
			storedStatus != attemptStatus || storedDisposition != disposition || storedFailure != failure || storedResult != result || storedCandidate != candidateSHA {
			return Job{}, errors.New("attempt does not own result")
		}
		return j, tx.Commit()
	}
	if j.Status != "leased" {
		return Job{}, fmt.Errorf("job status is %s", j.Status)
	}
	var deadline int64
	if err = tx.QueryRow(`SELECT deadline_at FROM attempts WHERE id=? AND job_id=? AND status='leased'`, attemptID, jobID).Scan(&deadline); err != nil {
		return Job{}, errors.New("attempt is not active")
	}
	if at.UnixNano() >= deadline {
		return Job{}, errors.New("attempt lease expired")
	}
	stamp := at.Format(time.RFC3339Nano)
	res, err := tx.Exec(`UPDATE attempts SET status=?,completed_at=?,failure_disposition=?,failure_code=?,result=?,candidate_sha=? WHERE id=? AND job_id=? AND status='leased'`, attemptStatus, at.UnixNano(), disposition, failure, result, candidateSHA, attemptID, jobID)
	if err != nil {
		return Job{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("attempt terminal update affected %d rows", n)
	}
	res, err = tx.Exec(`UPDATE jobs SET status=?,result=?,candidate_sha=?,error_text=?,updated_at=? WHERE id=? AND status='leased' AND attempt_id=?`, jobStatus, result, candidateSHA, failure, stamp, jobID, attemptID)
	if err != nil {
		return Job{}, err
	}
	if n, err := res.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Job{}, err
		}
		return Job{}, fmt.Errorf("job terminal update affected %d rows", n)
	}
	detail := "result stored"
	if failure != "" {
		detail = "failure_code=" + failure
	} else if candidateSHA != "" {
		detail = "candidate_sha=" + candidateSHA
	}
	eventKind := jobStatus
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, eventKind, detail, stamp); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return s.Job(jobID)
}

func (s *Store) Attempts(jobID string) ([]Attempt, error) {
	rows, err := s.db.Query(`SELECT id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at,failure_disposition,failure_code,result,candidate_sha,worker_pool,slot,policy_version FROM attempts WHERE job_id=? ORDER BY ordinal`, jobID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Attempt
	for rows.Next() {
		var a Attempt
		var leased, deadline, completed int64
		var pool, slot sql.NullString
		var policyVersion sql.NullInt64
		if err := rows.Scan(&a.ID, &a.JobID, &a.Ordinal, &a.WorkerID, &a.Status, &leased, &deadline, &completed, &a.FailureDisposition, &a.FailureCode, &a.Result, &a.CandidateSHA, &pool, &slot, &policyVersion); err != nil {
			return nil, err
		}
		a.WorkerPool, a.Slot, a.PolicyVersion = pool.String, slot.String, int(policyVersion.Int64)
		a.LeasedAt = time.Unix(0, leased).UTC()
		a.DeadlineAt = time.Unix(0, deadline).UTC()
		if completed != 0 {
			a.CompletedAt = time.Unix(0, completed).UTC()
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (Job, error) {
	var j Job
	var taskJSON, c, u string
	var pool sql.NullString
	var policyVersion sql.NullInt64
	err := r.Scan(&j.ID, &j.Input, &taskJSON, &j.Status, &j.AttemptID, &j.WorkerID, &j.Result, &j.CandidateSHA, &j.Error, &c, &u, &pool, &policyVersion)
	if err != nil {
		return j, err
	}
	j.WorkerPool, j.PolicyVersion = pool.String, int(policyVersion.Int64)
	if taskJSON != "" {
		j.Task = new(protocol.CodingTask)
		if err = json.Unmarshal([]byte(taskJSON), j.Task); err != nil {
			return j, err
		}
	}
	j.CreatedAt, err = time.Parse(time.RFC3339Nano, c)
	if err != nil {
		return j, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, u)
	return j, err
}
func (s *Store) Job(id string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT id,input,task_json,status,attempt_id,worker_id,result,candidate_sha,error_text,created_at,updated_at,worker_pool,policy_version FROM jobs WHERE id=?`, id))
}
func (s *Store) Events(id string) ([]Event, error) {
	rows, err := s.db.Query(`SELECT id,job_id,kind,detail,at FROM events WHERE job_id=? ORDER BY id`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Event
	for rows.Next() {
		var e Event
		var at string
		if err := rows.Scan(&e.ID, &e.JobID, &e.Kind, &e.Detail, &at); err != nil {
			return nil, err
		}
		e.At, err = time.Parse(time.RFC3339Nano, at)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}
func (s *Store) SetWorkerConnected(id string, connected bool) error {
	_, err := s.db.Exec(`INSERT INTO workers(id,connected,last_seen) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET connected=excluded.connected,last_seen=excluded.last_seen`, id, connected, now())
	return err
}
func (s *Store) Worker(id string) (Worker, error) {
	var w Worker
	var at string
	var base, pool, generation sql.NullString
	var slot sql.NullInt64
	err := s.db.QueryRow(`SELECT id,connected,last_seen,base_worker_id,slot,worker_pool,generation FROM workers WHERE id=?`, id).Scan(&w.ID, &w.Connected, &at, &base, &slot, &pool, &generation)
	if err != nil {
		return w, err
	}
	w.BaseID, w.Slot, w.Pool, w.Generation = base.String, int(slot.Int64), pool.String, generation.String
	w.LastSeen, err = time.Parse(time.RFC3339Nano, at)
	return w, err
}
