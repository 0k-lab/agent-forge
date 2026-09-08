package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/protocol"
)

type Delivery struct {
	JobID           string `json:"-"`
	AttemptID       string `json:"-"`
	CandidateSHA    string `json:"-"`
	ExpectedTreeSHA string `json:"-"`
	ParentSHA       string `json:"-"`
	CandidateRef    string `json:"-"`
	RepositoryID    string `json:"-"`
	RepositoryURL   string `json:"-"`
	DefaultBranch   string `json:"-"`
	Branch          string `json:"branch,omitempty"`
	PRTitle         string `json:"-"`
	PRBody          string `json:"-"`
	Phase           string `json:"phase,omitempty"`
	PRURL           string `json:"pr_url,omitempty"`
	PRNumber        int    `json:"-"`
	CIState         string `json:"ci_state,omitempty"`
	MergeSHA        string `json:"merge_sha,omitempty"`
	FailureCode     string `json:"failure_code,omitempty"`
	Attempts        int    `json:"-"`
	MaxAttempts     int    `json:"-"`
	RetryAt         int64  `json:"-"`
	UpdatedAt       int64  `json:"-"`
}

const deliveryColumns = `job_id,attempt_id,candidate_sha,expected_tree_sha,parent_sha,candidate_ref,repository_id,repository_url,default_branch,branch,pr_title,pr_body,phase,pr_url,pr_number,ci_state,merge_sha,failure_code,attempts,max_attempts,retry_at,updated_at`

func scanDelivery(row scanner) (Delivery, error) {
	var d Delivery
	err := row.Scan(&d.JobID, &d.AttemptID, &d.CandidateSHA, &d.ExpectedTreeSHA, &d.ParentSHA, &d.CandidateRef, &d.RepositoryID, &d.RepositoryURL, &d.DefaultBranch, &d.Branch, &d.PRTitle, &d.PRBody, &d.Phase, &d.PRURL, &d.PRNumber, &d.CIState, &d.MergeSHA, &d.FailureCode, &d.Attempts, &d.MaxAttempts, &d.RetryAt, &d.UpdatedAt)
	return d, err
}

// Review phases live in the existing event journal because the delivery table's
// phase constraint predates review gating. A hold and its candidate commit atomically.
const heldDelivery = `EXISTS (SELECT 1 FROM events WHERE job_id=deliveries.job_id AND kind='delivery_review') AND NOT EXISTS (SELECT 1 FROM events WHERE job_id=deliveries.job_id AND kind='delivery_resumed')`

func (s *Store) Delivery(jobID string) (Delivery, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return Delivery{}, err
	}
	defer tx.Rollback()
	d, held, _, err := reviewDelivery(tx, jobID)
	if err != nil {
		return Delivery{}, err
	}
	if held {
		d.Phase = "awaiting_review"
	}
	return d, tx.Commit()
}

// Read the candidate and its closed review journal in one consistent transaction.
func reviewDelivery(tx *sql.Tx, jobID string) (Delivery, bool, bool, error) {
	invalid := errors.New("invalid delivery review state")
	d, err := scanDelivery(tx.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE job_id=?`, jobID))
	if err != nil {
		return d, false, false, err
	}
	var body []byte
	var status, attempt, candidate, taskJSON string
	if err := tx.QueryRow(`SELECT resolved_policy,status,attempt_id,candidate_sha,task_json FROM jobs WHERE id=?`, jobID).Scan(&body, &status, &attempt, &candidate, &taskJSON); err != nil {
		return d, false, false, err
	}
	p, err := DecodeCanonicalPolicy(body)
	if err != nil {
		return d, false, false, invalid
	}
	var task protocol.CodingTask
	var attemptStatus, attemptCandidate string
	var attemptPolicy []byte
	if err := tx.QueryRow(`SELECT status,candidate_sha,resolved_policy FROM attempts WHERE id=? AND job_id=?`, d.AttemptID, jobID).Scan(&attemptStatus, &attemptCandidate, &attemptPolicy); err != nil {
		return d, false, false, invalid
	}
	if configjson.Decode([]byte(taskJSON), &task) != nil || validateStoredTask(task, p) != nil ||
		!bytes.Equal(body, attemptPolicy) || attemptStatus != "succeeded" || attemptCandidate != d.CandidateSHA ||
		!lowerHex(jobID, 32) || !lowerHex(d.AttemptID, 32) || !lowerHex(d.CandidateSHA, 40) || !lowerHex(d.ExpectedTreeSHA, 40) ||
		d.CandidateRef != "refs/agent-forge/candidates/"+jobID+"/"+d.AttemptID || d.ParentSHA != task.BaseSHA ||
		d.RepositoryID != task.RepositoryID || d.RepositoryID != p.Execution.RepositoryID || d.DefaultBranch != p.Execution.DefaultBranch ||
		d.Branch != "forge/"+jobID {
		return d, false, false, invalid
	}
	var holds, resumes, bad, holdID, resumeID int64
	if err := tx.QueryRow(`SELECT COALESCE(SUM(kind='delivery_review'),0),COALESCE(SUM(kind='delivery_resumed'),0),COALESCE(SUM((kind='delivery_review' AND detail<>'phase=awaiting_review') OR (kind='delivery_resumed' AND detail<>'phase=pending')),0),COALESCE(MIN(CASE WHEN kind='delivery_review' THEN id END),0),COALESCE(MIN(CASE WHEN kind='delivery_resumed' THEN id END),0) FROM events WHERE job_id=? AND kind IN ('delivery_review','delivery_resumed')`, jobID).Scan(&holds, &resumes, &bad, &holdID, &resumeID); err != nil {
		return d, false, false, err
	}
	review := p.DeliveryPolicy == protocol.DeliveryReview
	if resumes != 0 && (holds != 1 || resumeID <= holdID) || bad != 0 || holds > 1 || resumes > 1 || review && holds != 1 || !review && (holds != 0 || resumes != 0) || attempt != d.AttemptID || candidate != d.CandidateSHA {
		return d, false, false, invalid
	}
	if review {
		var contradictory int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM events WHERE job_id=? AND kind LIKE 'delivery_%' AND kind NOT IN ('delivery_review','delivery_resumed') AND (kind='delivery_pending' OR ?=0 OR id<?)`, jobID, resumeID, resumeID).Scan(&contradictory); err != nil {
			return d, false, false, err
		}
		if contradictory != 0 {
			return d, false, false, invalid
		}
	}
	switch d.Phase {
	case "pending", "publishing", "ci", "merging", "retry_wait":
		if status != "delivering" {
			return d, false, false, invalid
		}
	case "merged":
		if status != "succeeded" {
			return d, false, false, invalid
		}
	case "failed":
		if status != "failed" {
			return d, false, false, invalid
		}
	default:
		return d, false, false, invalid
	}
	held := review && resumes == 0
	if held && (d.Phase != "pending" || d.Attempts != 0 || d.PRURL != "" || d.PRNumber != 0 || d.CIState != "" || d.MergeSHA != "" || d.FailureCode != "" || d.RetryAt != 0) {
		return d, false, false, invalid
	}
	return d, held, resumes == 1, nil
}

func (s *Store) ValidateDeliveries() error {
	rows, err := s.db.Query(`SELECT d.` + strings.ReplaceAll(deliveryColumns, ",", ",d.") + `,j.status,j.attempt_id,j.candidate_sha,j.error_text,a.status,a.candidate_sha
		FROM deliveries d LEFT JOIN jobs j ON j.id=d.job_id LEFT JOIN attempts a ON a.id=d.attempt_id AND a.job_id=d.job_id ORDER BY d.job_id`)
	if err != nil {
		return errors.New("delivery state validation failed")
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var d Delivery
		var jobStatus, jobAttempt, jobCandidate, jobError, attemptStatus, attemptCandidate string
		values := []any{&d.JobID, &d.AttemptID, &d.CandidateSHA, &d.ExpectedTreeSHA, &d.ParentSHA, &d.CandidateRef, &d.RepositoryID, &d.RepositoryURL, &d.DefaultBranch, &d.Branch, &d.PRTitle, &d.PRBody, &d.Phase, &d.PRURL, &d.PRNumber, &d.CIState, &d.MergeSHA, &d.FailureCode, &d.Attempts, &d.MaxAttempts, &d.RetryAt, &d.UpdatedAt, &jobStatus, &jobAttempt, &jobCandidate, &jobError, &attemptStatus, &attemptCandidate}
		if rows.Scan(values...) != nil || !lowerHex(d.JobID, 32) || !lowerHex(d.AttemptID, 32) || !lowerHex(d.CandidateSHA, 40) || !lowerHex(d.ExpectedTreeSHA, 40) || !lowerHex(d.ParentSHA, 40) || d.AttemptID != jobAttempt || d.CandidateSHA != jobCandidate || d.CandidateSHA != attemptCandidate || attemptStatus != "succeeded" || d.Attempts < 0 || d.Attempts > d.MaxAttempts || d.MaxAttempts < 1 || d.UpdatedAt <= 0 {
			return errors.New("delivery state validation failed")
		}
		ids = append(ids, d.JobID)
		switch d.Phase {
		case "pending", "publishing", "ci", "merging", "retry_wait":
			if jobStatus != "delivering" || jobError != "" || d.MergeSHA != "" {
				return errors.New("delivery state validation failed")
			}
		case "merged":
			if jobStatus != "succeeded" || jobError != "" || d.CIState != "success" || !lowerHex(d.MergeSHA, 40) || d.FailureCode != "" {
				return errors.New("delivery state validation failed")
			}
		case "failed":
			if jobStatus != "failed" || d.FailureCode == "" || jobError != d.FailureCode || d.MergeSHA != "" {
				return errors.New("delivery state validation failed")
			}
		default:
			return errors.New("delivery state validation failed")
		}
	}
	if rows.Err() != nil {
		return errors.New("delivery state validation failed")
	}
	if err := rows.Close(); err != nil {
		return err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range ids {
		if _, _, _, err := reviewDelivery(tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// An interrupted final attempt still needs one reconciliation pass; it has not
// durably finished and must not be stranded by the attempt budget.
func (s *Store) RecoverDeliveries(at time.Time) error {
	stamp := at.UTC().UnixNano()
	_, err := s.db.Exec(`UPDATE deliveries SET phase='retry_wait',attempts=CASE WHEN attempts=max_attempts THEN attempts-1 ELSE attempts END,retry_at=?,updated_at=? WHERE phase IN ('publishing','ci','merging')`, stamp, stamp)
	return err
}

func (s *Store) ClaimDelivery(at time.Time) (Delivery, bool, error) {
	at = at.UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Delivery{}, false, err
	}
	defer tx.Rollback()
	d, err := scanDelivery(tx.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE (phase='pending' OR phase='retry_wait' AND retry_at<=?) AND attempts<max_attempts AND NOT (`+heldDelivery+`) ORDER BY updated_at,job_id LIMIT 1`, at.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, tx.Commit()
	}
	if err != nil {
		return Delivery{}, false, err
	}
	if _, held, _, err := reviewDelivery(tx, d.JobID); err != nil || held {
		return Delivery{}, false, errors.New("invalid delivery claim")
	}
	result, err := tx.Exec(`UPDATE deliveries SET phase='publishing',attempts=attempts+1,retry_at=0,failure_code='',updated_at=? WHERE job_id=? AND phase=? AND attempts=?`, at.UnixNano(), d.JobID, d.Phase, d.Attempts)
	if err != nil {
		return Delivery{}, false, err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		if err != nil {
			return Delivery{}, false, err
		}
		return Delivery{}, false, errors.New("delivery claim failed")
	}
	d.Phase, d.Attempts, d.FailureCode, d.RetryAt, d.UpdatedAt = "publishing", d.Attempts+1, "", 0, at.UnixNano()
	if _, err := tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, d.JobID, "delivery_phase", "phase=publishing", at.Format(time.RFC3339Nano)); err != nil {
		return Delivery{}, false, err
	}
	return d, true, tx.Commit()
}

func (s *Store) UpdateDelivery(jobID, phase, prURL string, prNumber int, ciState string, at time.Time) error {
	if phase != "ci" && phase != "merging" {
		return errors.New("invalid delivery phase")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	result, err := tx.Exec(`UPDATE deliveries SET phase=?,pr_url=CASE WHEN ?<>'' THEN ? ELSE pr_url END,pr_number=CASE WHEN ?>0 THEN ? ELSE pr_number END,ci_state=?,updated_at=? WHERE job_id=? AND phase IN ('publishing','ci','merging')`, phase, prURL, prURL, prNumber, prNumber, ciState, at.UTC().UnixNano(), jobID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("delivery is not active")
	}
	if _, err := tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "delivery_phase", "phase="+phase, at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) RetryDelivery(jobID, code string, at time.Time, base time.Duration) error {
	if code == "" || base <= 0 {
		return errors.New("invalid delivery retry")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var attempts, maximum int
	if err := tx.QueryRow(`SELECT attempts,max_attempts FROM deliveries WHERE job_id=? AND phase IN ('publishing','ci','merging')`, jobID).Scan(&attempts, &maximum); err != nil {
		return err
	}
	if attempts >= maximum {
		return finishDelivery(tx, jobID, "failed", "", code, at)
	}
	delay := base
	for i := 1; i < attempts; i++ {
		if delay >= 24*time.Hour/2 {
			delay = 24 * time.Hour
			break
		}
		delay *= 2
	}
	retryAt := at.UTC().Add(delay)
	if _, err := tx.Exec(`UPDATE deliveries SET phase='retry_wait',failure_code=?,retry_at=?,updated_at=? WHERE job_id=?`, code, retryAt.UnixNano(), at.UTC().UnixNano(), jobID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "delivery_retry", fmt.Sprintf("phase=retry_wait failure_code=%s retry_at=%s", code, retryAt.Format(time.RFC3339Nano)), at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) FailDelivery(jobID, code string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return finishDelivery(tx, jobID, "failed", "", code, at)
}

func (s *Store) CompleteDelivery(jobID, mergeSHA string, at time.Time) error {
	if !lowerHex(mergeSHA, 40) {
		return errors.New("invalid merge SHA")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	return finishDelivery(tx, jobID, "merged", mergeSHA, "", at)
}

func finishDelivery(tx *sql.Tx, jobID, phase, mergeSHA, code string, at time.Time) error {
	if phase == "failed" && code == "" {
		return errors.New("invalid delivery failure")
	}
	jobStatus := "succeeded"
	if phase == "failed" {
		jobStatus = "failed"
	}
	result, err := tx.Exec(`UPDATE deliveries SET phase=?,ci_state=CASE WHEN ?='merged' THEN 'success' ELSE ci_state END,merge_sha=?,failure_code=?,retry_at=0,updated_at=? WHERE job_id=? AND phase IN ('publishing','ci','merging')`, phase, phase, mergeSHA, code, at.UTC().UnixNano(), jobID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("delivery is not active")
	}
	result, err = tx.Exec(`UPDATE jobs SET status=?,error_text=?,worker_id='',updated_at=? WHERE id=? AND status='delivering'`, jobStatus, code, at.UTC().Format(time.RFC3339Nano), jobID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("delivery does not own job")
	}
	kind := "delivery_merged"
	if phase == "failed" {
		kind = "delivery_failed"
	}
	if _, err := tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, kind, fmt.Sprintf("phase=%s failure_code=%s", phase, code), at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) ResumeDelivery(jobID string, at time.Time) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, held, resumed, err := reviewDelivery(tx, jobID)
	if err != nil {
		return err
	}
	if resumed {
		return tx.Commit()
	}
	if !held {
		return errors.New("delivery is not awaiting review")
	}
	result, err := tx.Exec(`UPDATE deliveries SET updated_at=? WHERE job_id=? AND phase='pending' AND attempts=0 AND EXISTS(SELECT 1 FROM jobs WHERE id=deliveries.job_id AND status='delivering' AND attempt_id=deliveries.attempt_id AND candidate_sha=deliveries.candidate_sha)`, at.UTC().UnixNano(), jobID)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err != nil || n != 1 {
		return errors.New("delivery is not awaiting review")
	}
	if _, err := tx.Exec(`INSERT INTO events(job_id,kind,detail,at) VALUES(?,?,?,?)`, jobID, "delivery_resumed", "phase=pending", at.UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return tx.Commit()
}
