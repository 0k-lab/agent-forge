package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
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

func (s *Store) Delivery(jobID string) (Delivery, error) {
	return scanDelivery(s.db.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE job_id=?`, jobID))
}

func (s *Store) ValidateDeliveries() error {
	rows, err := s.db.Query(`SELECT d.` + strings.ReplaceAll(deliveryColumns, ",", ",d.") + `,j.status,j.attempt_id,j.candidate_sha,j.error_text,a.status,a.candidate_sha
		FROM deliveries d JOIN jobs j ON j.id=d.job_id JOIN attempts a ON a.id=d.attempt_id AND a.job_id=d.job_id ORDER BY d.job_id`)
	if err != nil {
		return errors.New("delivery state validation failed")
	}
	defer rows.Close()
	for rows.Next() {
		var d Delivery
		var jobStatus, jobAttempt, jobCandidate, jobError, attemptStatus, attemptCandidate string
		values := []any{&d.JobID, &d.AttemptID, &d.CandidateSHA, &d.ExpectedTreeSHA, &d.ParentSHA, &d.CandidateRef, &d.RepositoryID, &d.RepositoryURL, &d.DefaultBranch, &d.Branch, &d.PRTitle, &d.PRBody, &d.Phase, &d.PRURL, &d.PRNumber, &d.CIState, &d.MergeSHA, &d.FailureCode, &d.Attempts, &d.MaxAttempts, &d.RetryAt, &d.UpdatedAt, &jobStatus, &jobAttempt, &jobCandidate, &jobError, &attemptStatus, &attemptCandidate}
		if rows.Scan(values...) != nil || !lowerHex(d.JobID, 32) || !lowerHex(d.AttemptID, 32) || !lowerHex(d.CandidateSHA, 40) || !lowerHex(d.ExpectedTreeSHA, 40) || !lowerHex(d.ParentSHA, 40) || d.AttemptID != jobAttempt || d.CandidateSHA != jobCandidate || d.CandidateSHA != attemptCandidate || attemptStatus != "succeeded" || d.Attempts < 0 || d.Attempts > d.MaxAttempts || d.MaxAttempts < 1 || d.UpdatedAt <= 0 {
			return errors.New("delivery state validation failed")
		}
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
	return nil
}

func (s *Store) RecoverDeliveries(at time.Time) error {
	stamp := at.UTC().UnixNano()
	_, err := s.db.Exec(`UPDATE deliveries SET phase='retry_wait',retry_at=?,updated_at=? WHERE phase IN ('publishing','ci','merging')`, stamp, stamp)
	return err
}

func (s *Store) ClaimDelivery(at time.Time) (Delivery, bool, error) {
	at = at.UTC()
	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Delivery{}, false, err
	}
	defer tx.Rollback()
	d, err := scanDelivery(tx.QueryRow(`SELECT `+deliveryColumns+` FROM deliveries WHERE (phase='pending' OR phase='retry_wait' AND retry_at<=?) AND attempts<max_attempts ORDER BY updated_at,job_id LIMIT 1`, at.UnixNano()))
	if errors.Is(err, sql.ErrNoRows) {
		return Delivery{}, false, tx.Commit()
	}
	if err != nil {
		return Delivery{}, false, err
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
