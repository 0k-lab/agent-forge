package store

import (
	"context"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"time"
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
	Type string    `json:"type"`
	At   time.Time `json:"at"`
}

type DebugJobPage struct {
	Items      []DebugJob `json:"items"`
	NextCursor string     `json:"next_cursor,omitempty"`
}

type DebugWorkerPage struct {
	Items      []DebugWorker `json:"items"`
	NextCursor string        `json:"next_cursor,omitempty"`
}

type DebugTimeline struct {
	Job        DebugJob     `json:"job"`
	Events     []DebugEvent `json:"events"`
	NextCursor string       `json:"next_cursor,omitempty"`
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

func debugCursor(stamp, id string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(stamp + "\x00" + id))
}

func parseDebugCursor(cursor string) (string, string, error) {
	if cursor == "" {
		return "", "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	parts := strings.Split(string(b), "\x00")
	if err != nil || len(parts) != 2 || parts[1] == "" {
		return "", "", ErrInvalidDebugCursor
	}
	if _, err = time.Parse(time.RFC3339Nano, parts[0]); err != nil {
		return "", "", ErrInvalidDebugCursor
	}
	return parts[0], parts[1], nil
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

func (s *Store) RecentDebugJobs(ctx context.Context, limit int, cursor string) (DebugJobPage, error) {
	limit = debugLimit(limit)
	stamp, id, err := parseDebugCursor(cursor)
	if err != nil {
		return DebugJobPage{}, err
	}
	query := `SELECT ` + debugJobColumns + ` FROM jobs`
	args := []any{}
	if cursor != "" {
		query += ` WHERE updated_at < ? OR (updated_at = ? AND id < ?)`
		args = append(args, stamp, stamp, id)
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
		page.NextCursor = debugCursor(stamps[limit-1], page.Items[limit-1].ID)
	}
	return page, nil
}

func (s *Store) RecentDebugWorkers(ctx context.Context, limit int, cursor string) (DebugWorkerPage, error) {
	limit = debugLimit(limit)
	stamp, id, err := parseDebugCursor(cursor)
	if err != nil {
		return DebugWorkerPage{}, err
	}
	query := `SELECT id,connected,last_seen FROM workers`
	args := []any{}
	if cursor != "" {
		query += ` WHERE last_seen < ? OR (last_seen = ? AND id < ?)`
		args = append(args, stamp, stamp, id)
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
		page.NextCursor = debugCursor(last.LastSeen.Format(time.RFC3339Nano), last.ID)
	}
	return page, nil
}

func (s *Store) DebugJobTimeline(ctx context.Context, id string, limit int, cursor string) (DebugTimeline, error) {
	limit = debugLimit(limit)
	job, err := scanDebugJob(s.db.QueryRowContext(ctx, `SELECT `+debugJobColumns+` FROM jobs WHERE id=?`, id))
	if err != nil {
		return DebugTimeline{}, err
	}
	stamp, eventID, err := parseDebugCursor(cursor)
	if err != nil {
		return DebugTimeline{}, err
	}
	var numericEventID int64
	if cursor != "" {
		numericEventID, err = strconv.ParseInt(eventID, 10, 64)
		if err != nil || numericEventID < 1 {
			return DebugTimeline{}, ErrInvalidDebugCursor
		}
	}
	query := `SELECT id,kind,at FROM events WHERE job_id=?`
	args := []any{id}
	if cursor != "" {
		query += ` AND (at > ? OR (at = ? AND id > ?))`
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
		var at string
		if err := rows.Scan(&eventID, &event.Type, &at); err != nil {
			return DebugTimeline{}, err
		}
		if event.At, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return DebugTimeline{}, err
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
		timeline.NextCursor = debugCursor(last.At.Format(time.RFC3339Nano), strconv.FormatInt(ids[limit-1], 10))
	}
	return timeline, nil
}
