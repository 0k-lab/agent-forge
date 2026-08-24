package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"
)

type Store struct{ db *sql.DB }

type Job struct {
	ID        string    `json:"id"`
	Input     string    `json:"input"`
	Status    string    `json:"status"`
	AttemptID string    `json:"attempt_id,omitempty"`
	WorkerID  string    `json:"worker_id,omitempty"`
	Result    string    `json:"result,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Lease struct {
	JobID     string `json:"job_id"`
	AttemptID string `json:"attempt_id"`
	Input     string `json:"input"`
}
type Event struct {
	ID     int64     `json:"id"`
	JobID  string    `json:"job_id"`
	Kind   string    `json:"kind"`
	Detail string    `json:"detail"`
	At     time.Time `json:"at"`
}
type Worker struct {
	ID        string    `json:"id"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen"`
}

func Open(path string) (*Store, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	_, err = db.Exec(`PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, input TEXT NOT NULL, status TEXT NOT NULL, attempt_id TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL, at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workers (id TEXT PRIMARY KEY, connected INTEGER NOT NULL, last_seen TEXT NOT NULL);`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}
func (s *Store) Close() error { return s.db.Close() }
func newID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func now() string { return time.Now().UTC().Format(time.RFC3339Nano) }

func (s *Store) CreateJob(input string) (Job, error) {
	j := Job{ID: newID(), Input: input, Status: "pending", CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	stamp := j.CreatedAt.Format(time.RFC3339Nano)
	if _, err = tx.Exec(`INSERT INTO jobs(id,input,status,created_at,updated_at) VALUES(?,?,?,?,?)`, j.ID, j.Input, j.Status, stamp, stamp); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, j.ID, "submitted", "job accepted", stamp); err != nil {
		return Job{}, err
	}
	return j, tx.Commit()
}

func (s *Store) LeaseNext(workerID string) (Lease, bool, error) {
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Lease{}, false, err
	}
	defer tx.Rollback()
	var id, input string
	err = tx.QueryRow(`SELECT id,input FROM jobs WHERE status='pending' ORDER BY created_at,id LIMIT 1`).Scan(&id, &input)
	if errors.Is(err, sql.ErrNoRows) {
		return Lease{}, false, nil
	}
	if err != nil {
		return Lease{}, false, err
	}
	attempt := newID()
	stamp := now()
	res, err := tx.Exec(`UPDATE jobs SET status='leased',attempt_id=?,worker_id=?,updated_at=? WHERE id=? AND status='pending'`, attempt, workerID, stamp, id)
	if err != nil {
		return Lease{}, false, err
	}
	n, _ := res.RowsAffected()
	if n != 1 {
		return Lease{}, false, nil
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, id, "leased", fmt.Sprintf("worker=%s attempt=%s", workerID, attempt), stamp); err != nil {
		return Lease{}, false, err
	}
	if err = tx.Commit(); err != nil {
		return Lease{}, false, err
	}
	return Lease{JobID: id, AttemptID: attempt, Input: input}, true, nil
}

func (s *Store) Complete(jobID, attemptID, result string) (Job, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Job{}, err
	}
	defer tx.Rollback()
	j, err := scanJob(tx.QueryRow(`SELECT id,input,status,attempt_id,worker_id,result,created_at,updated_at FROM jobs WHERE id=?`, jobID))
	if err != nil {
		return Job{}, err
	}
	if j.AttemptID != attemptID {
		return Job{}, errors.New("attempt does not own job")
	}
	if j.Status == "succeeded" {
		if j.Result != result {
			return Job{}, errors.New("result is immutable")
		}
		return j, tx.Commit()
	}
	if j.Status != "leased" {
		return Job{}, fmt.Errorf("job status is %s", j.Status)
	}
	stamp := now()
	if _, err = tx.Exec(`UPDATE jobs SET status='succeeded',result=?,updated_at=? WHERE id=? AND status='leased' AND attempt_id=?`, result, stamp, jobID, attemptID); err != nil {
		return Job{}, err
	}
	if _, err = tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "succeeded", "result stored", stamp); err != nil {
		return Job{}, err
	}
	if err = tx.Commit(); err != nil {
		return Job{}, err
	}
	return s.Job(jobID)
}

type scanner interface{ Scan(...any) error }

func scanJob(r scanner) (Job, error) {
	var j Job
	var c, u string
	err := r.Scan(&j.ID, &j.Input, &j.Status, &j.AttemptID, &j.WorkerID, &j.Result, &c, &u)
	if err != nil {
		return j, err
	}
	j.CreatedAt, err = time.Parse(time.RFC3339Nano, c)
	if err != nil {
		return j, err
	}
	j.UpdatedAt, err = time.Parse(time.RFC3339Nano, u)
	return j, err
}
func (s *Store) Job(id string) (Job, error) {
	return scanJob(s.db.QueryRow(`SELECT id,input,status,attempt_id,worker_id,result,created_at,updated_at FROM jobs WHERE id=?`, id))
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
	err := s.db.QueryRow(`SELECT id,connected,last_seen FROM workers WHERE id=?`, id).Scan(&w.ID, &w.Connected, &at)
	if err != nil {
		return w, err
	}
	w.LastSeen, err = time.Parse(time.RFC3339Nano, at)
	return w, err
}
