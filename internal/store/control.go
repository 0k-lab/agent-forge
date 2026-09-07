package store

import (
	"context"
	"time"
)

// ControlWorker describes a recorded runtime slot; only leased jobs occupy it.
type ControlWorker struct {
	ID          string     `json:"id"`
	BaseID      string     `json:"base_id"`
	Slot        int        `json:"slot"`
	Pool        string     `json:"pool"`
	Connected   bool       `json:"connected"`
	LastSeen    *time.Time `json:"last_seen"`
	ActiveJobID string     `json:"active_job_id,omitempty"`
	Agent       string     `json:"agent,omitempty"`
	Occupied    bool       `json:"occupied"`
}

func (s *Store) ControlWorkers(ctx context.Context) ([]ControlWorker, bool, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT w.id,COALESCE(w.base_worker_id,w.id),COALESCE(w.slot,0),COALESCE(w.worker_pool,''),w.connected,w.last_seen,COALESCE(j.id,''),COALESCE(json_extract(NULLIF(j.resolved_policy,''),'$.execution.plugin_id'),'') FROM workers w LEFT JOIN jobs j ON j.worker_id=w.id AND j.status='leased' ORDER BY w.connected DESC,w.last_seen DESC,w.id LIMIT 257`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	out := []ControlWorker{}
	for rows.Next() {
		var w ControlWorker
		var stamp string
		if err := rows.Scan(&w.ID, &w.BaseID, &w.Slot, &w.Pool, &w.Connected, &stamp, &w.ActiveJobID, &w.Agent); err != nil {
			return nil, false, err
		}
		at, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return nil, false, err
		}
		w.LastSeen = &at
		w.Occupied = w.ActiveJobID != ""
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(out) > 256
	if truncated {
		out = out[:256]
	}
	return out, truncated, nil
}

// ControlAgent reads the pinned policy without returning its environment or paths.
func (s *Store) ControlAgent(ctx context.Context, jobID string) (string, int64, error) {
	var agent string
	var timeout int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(json_extract(NULLIF(resolved_policy,''),'$.execution.plugin_id'),''),COALESCE(json_extract(NULLIF(resolved_policy,''),'$.execution.plugin_timeout_nanos'),0)/1000000 FROM jobs WHERE id=?`, jobID).Scan(&agent, &timeout)
	return agent, timeout, err
}

func (s *Store) ControlAttempts(jobID string) ([]Attempt, bool, error) {
	attempts, err := s.attempts(jobID, 101)
	truncated := len(attempts) > 100
	if truncated {
		attempts = attempts[:100]
	}
	return attempts, truncated, err
}

// ControlRunRef is a bounded projection; ordinals describe runs, not attempts.
type ControlRunRef struct {
	ID      string
	Count   int
	Ordinal int
}

// accept must only inspect the projection: the Store connection is held during iteration.
// The bound applies after acceptance so non-task groups cannot starve valid tasks.
func (s *Store) ControlTaskRuns(ctx context.Context, accept func(project, source string) bool) ([]ControlRunRef, bool, error) {
	rows, err := s.db.QueryContext(ctx, `WITH linked AS (
 SELECT id,created_at,source_ref,COALESCE(json_extract(NULLIF(task_json,''),'$.repository_id'),'') AS project,COUNT(*) OVER (PARTITION BY COALESCE(json_extract(NULLIF(task_json,''),'$.repository_id'),''),source_ref) AS total,
 ROW_NUMBER() OVER (PARTITION BY COALESCE(json_extract(NULLIF(task_json,''),'$.repository_id'),''),source_ref ORDER BY created_at DESC,id DESC) AS rank
 FROM jobs WHERE source_ref<>''
 ) SELECT id,total,project,source_ref FROM linked WHERE rank=1 ORDER BY created_at DESC,id DESC`)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	refs := []ControlRunRef{}
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return nil, false, err
		}
		var ref ControlRunRef
		var project, source string
		if err := rows.Scan(&ref.ID, &ref.Count, &project, &source); err != nil {
			return nil, false, err
		}
		if !accept(project, source) {
			continue
		}
		ref.Ordinal = ref.Count
		refs = append(refs, ref)
		if len(refs) == 101 {
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	truncated := len(refs) > 100
	if truncated {
		refs = refs[:100]
	}
	return refs, truncated, nil
}

func (s *Store) ControlSourceRuns(ctx context.Context, project, source string) ([]ControlRunRef, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,COUNT(*) OVER (),ROW_NUMBER() OVER (ORDER BY created_at,id) FROM jobs
 WHERE COALESCE(json_extract(NULLIF(task_json,''),'$.repository_id'),'')=? AND source_ref=? ORDER BY created_at DESC,id DESC LIMIT 20`, project, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	refs := []ControlRunRef{}
	for rows.Next() {
		var ref ControlRunRef
		if err := rows.Scan(&ref.ID, &ref.Count, &ref.Ordinal); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	for i, j := 0, len(refs)-1; i < j; i, j = i+1, j-1 {
		refs[i], refs[j] = refs[j], refs[i]
	}
	return refs, rows.Err()
}
