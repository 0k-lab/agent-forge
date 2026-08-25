package store

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"agent-forge/internal/protocol"
)

type DebugJob struct {
	ID           string    `json:"id"`
	Kind         string    `json:"kind"`
	Status       string    `json:"status"`
	AttemptID    string    `json:"attempt_id,omitempty"`
	WorkerID     string    `json:"worker_id,omitempty"`
	BaseSHA      string    `json:"base_sha,omitempty"`
	CandidateSHA string    `json:"candidate_sha,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
}

type DebugWorker struct {
	ID        string    `json:"id"`
	Connected bool      `json:"connected"`
	LastSeen  time.Time `json:"last_seen"`
}

type DebugEvent struct {
	Type           string     `json:"type"`
	At             time.Time  `json:"at"`
	AttemptID      string     `json:"attempt_id,omitempty"`
	AttemptOrdinal int        `json:"attempt_ordinal,omitempty"`
	LeaseExpiresAt *time.Time `json:"lease_expires_at,omitempty"`
	RetryAt        *time.Time `json:"retry_at,omitempty"`
	Disposition    string     `json:"disposition,omitempty"`
	FailureCode    string     `json:"failure_code,omitempty"`
}

type DebugPosition struct {
	At time.Time
	ID string
}

type DebugJobPage struct {
	Items        []DebugJob     `json:"items"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	NextPosition *DebugPosition `json:"-"`
}

type DebugWorkerPage struct {
	Items        []DebugWorker  `json:"items"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	NextPosition *DebugPosition `json:"-"`
}

type DebugTimeline struct {
	Job          DebugJob       `json:"job"`
	Events       []DebugEvent   `json:"events"`
	NextCursor   string         `json:"next_cursor,omitempty"`
	NextPosition *DebugPosition `json:"-"`
}

var ErrInvalidDebugCursor = errors.New("invalid debug cursor")

func debugLimit(limit int) int {
	if limit <= 0 {
		return 25
	}
	if limit > 100 {
		return 100
	}
	return limit
}

const debugJobColumns = `id,CASE WHEN task_json='' THEN 'legacy' ELSE 'coding' END,status,attempt_id,worker_id,COALESCE(json_extract(NULLIF(task_json,''),'$.base_sha'),''),candidate_sha,created_at,updated_at`

func scanDebugJob(row scanner) (DebugJob, error) {
	var job DebugJob
	var created, updated string
	err := row.Scan(&job.ID, &job.Kind, &job.Status, &job.AttemptID, &job.WorkerID, &job.BaseSHA, &job.CandidateSHA, &created, &updated)
	if err != nil {
		return job, err
	}
	if job.CreatedAt, err = time.Parse(time.RFC3339Nano, created); err != nil {
		return job, err
	}
	job.UpdatedAt, err = time.Parse(time.RFC3339Nano, updated)
	return job, err
}

func (s *Store) RecentDebugJobs(ctx context.Context, limit int, position *DebugPosition) (DebugJobPage, error) {
	limit = debugLimit(limit)
	query := `SELECT ` + debugJobColumns + ` FROM jobs`
	args := []any{}
	if position != nil {
		query += ` WHERE updated_at < ? OR (updated_at = ? AND id < ?)`
		stamp := position.At.Format(time.RFC3339Nano)
		args = append(args, stamp, stamp, position.ID)
	}
	query += ` ORDER BY updated_at DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return DebugJobPage{}, err
	}
	defer rows.Close()
	page := DebugJobPage{Items: make([]DebugJob, 0, limit)}
	stamps := make([]string, 0, limit+1)
	for rows.Next() {
		job, err := scanDebugJob(rows)
		if err != nil {
			return DebugJobPage{}, err
		}
		page.Items = append(page.Items, job)
		stamps = append(stamps, job.UpdatedAt.Format(time.RFC3339Nano))
	}
	if err := rows.Err(); err != nil {
		return DebugJobPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		at, _ := time.Parse(time.RFC3339Nano, stamps[limit-1])
		page.NextPosition = &DebugPosition{At: at, ID: page.Items[limit-1].ID}
	}
	return page, nil
}

func (s *Store) RecentDebugWorkers(ctx context.Context, limit int, position *DebugPosition) (DebugWorkerPage, error) {
	limit = debugLimit(limit)
	query := `SELECT id,connected,last_seen FROM workers`
	args := []any{}
	if position != nil {
		query += ` WHERE last_seen < ? OR (last_seen = ? AND id < ?)`
		stamp := position.At.Format(time.RFC3339Nano)
		args = append(args, stamp, stamp, position.ID)
	}
	query += ` ORDER BY last_seen DESC,id DESC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return DebugWorkerPage{}, err
	}
	defer rows.Close()
	page := DebugWorkerPage{Items: make([]DebugWorker, 0, limit)}
	for rows.Next() {
		var worker DebugWorker
		var seen string
		if err := rows.Scan(&worker.ID, &worker.Connected, &seen); err != nil {
			return DebugWorkerPage{}, err
		}
		if worker.LastSeen, err = time.Parse(time.RFC3339Nano, seen); err != nil {
			return DebugWorkerPage{}, err
		}
		page.Items = append(page.Items, worker)
	}
	if err := rows.Err(); err != nil {
		return DebugWorkerPage{}, err
	}
	if len(page.Items) > limit {
		page.Items = page.Items[:limit]
		last := page.Items[limit-1]
		page.NextPosition = &DebugPosition{At: last.LastSeen, ID: last.ID}
	}
	return page, nil
}

func (s *Store) DebugJobTimeline(ctx context.Context, id string, limit int, position *DebugPosition) (DebugTimeline, error) {
	limit = debugLimit(limit)
	job, err := scanDebugJob(s.db.QueryRowContext(ctx, `SELECT `+debugJobColumns+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return DebugTimeline{}, err
	}
	var numericEventID int64
	if position != nil {
		numericEventID, err = strconv.ParseInt(position.ID, 10, 64)
		if err != nil || numericEventID < 1 {
			return DebugTimeline{}, ErrInvalidDebugCursor
		}
	}
	query := `SELECT id,kind,detail,at FROM events WHERE job_id=?`
	args := []any{id}
	if position != nil {
		query += ` AND (at > ? OR (at = ? AND id > ?))`
		stamp := position.At.Format(time.RFC3339Nano)
		args = append(args, stamp, stamp, numericEventID)
	}
	query += ` ORDER BY at,id LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return DebugTimeline{}, err
	}
	defer rows.Close()
	timeline := DebugTimeline{Job: job, Events: make([]DebugEvent, 0)}
	ids := make([]int64, 0, limit+1)
	for rows.Next() {
		var event DebugEvent
		var eventID int64
		var detail, at string
		if err := rows.Scan(&eventID, &event.Type, &detail, &at); err != nil {
			return DebugTimeline{}, err
		}
		if event.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return DebugTimeline{}, err
		}
		dispositionSeen := false
		for _, field := range strings.Fields(detail) {
			key, value, ok := strings.Cut(field, "=")
			if !ok {
				continue
			}
			switch key {
			case "attempt":
				event.AttemptID = value
			case "ordinal":
				event.AttemptOrdinal, _ = strconv.Atoi(value)
			case "lease_expires":
				if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
					event.LeaseExpiresAt = &parsed
				}
			case "retry_at":
				if parsed, parseErr := time.Parse(time.RFC3339Nano, value); parseErr == nil {
					event.RetryAt = &parsed
				}
			case "disposition":
				dispositionSeen = true
				if value == string(TerminalFailure) || value == string(RetryableFailure) {
					event.Disposition = value
				}
			case "failure_code":
				if value == protocol.FailureInvalidTask || value == protocol.FailureScopedTest || value == protocol.FailureExecution || value == "max_attempts_exceeded" {
					event.FailureCode = value
				}
			}
		}
		if event.Type == "failed" && !dispositionSeen {
			event.Disposition = "terminal"
		}
		timeline.Events = append(timeline.Events, event)
		ids = append(ids, eventID)
	}
	if err := rows.Err(); err != nil {
		return DebugTimeline{}, err
	}
	if len(timeline.Events) > limit {
		timeline.Events = timeline.Events[:limit]
		last := timeline.Events[limit-1]
		timeline.NextPosition = &DebugPosition{At: last.At, ID: strconv.FormatInt(ids[limit-1], 10)}
	}
	return timeline, nil
}
