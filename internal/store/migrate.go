package store

import (
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"
)

const schemaVersion = 3

var migrationMu sync.Mutex

func migrate(db *sql.DB) error {
	// ponytail: process-local lock; use a cross-process migration lock if multiple Gates ever share one database.
	migrationMu.Lock()
	defer migrationMu.Unlock()
	var version int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version < 0 || version > schemaVersion {
		return errors.New("unsupported database schema")
	}
	for next := version + 1; next <= schemaVersion; next++ {
		tx, err := db.Begin()
		if err != nil {
			return err
		}
		if err = runMigration(tx, next); err == nil {
			_, err = tx.Exec(fmt.Sprintf(`PRAGMA user_version=%d`, next))
		}
		if err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	for current := 1; current <= schemaVersion; current++ {
		if err := runMigration(tx, current); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

func runMigration(tx *sql.Tx, version int) error {
	switch version {
	case 1:
		if _, err := tx.Exec(`
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, input TEXT NOT NULL, status TEXT NOT NULL, attempt_id TEXT NOT NULL DEFAULT '', worker_id TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', created_at TEXT NOT NULL, updated_at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS events (id INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, kind TEXT NOT NULL, detail TEXT NOT NULL, at TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS workers (id TEXT PRIMARY KEY, connected INTEGER NOT NULL, last_seen TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value BLOB NOT NULL);`); err != nil {
			return err
		}
		for _, column := range []struct{ table, name, definition string }{
			{"jobs", "candidate_sha", `TEXT NOT NULL DEFAULT ''`}, {"jobs", "task_json", `TEXT NOT NULL DEFAULT ''`}, {"jobs", "error_text", `TEXT NOT NULL DEFAULT ''`}, {"jobs", "retry_at", `INTEGER NOT NULL DEFAULT 0`},
		} {
			if err := addColumn(tx, column.table, column.name, column.definition); err != nil {
				return err
			}
		}
	case 2:
		if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS attempts (
			id TEXT PRIMARY KEY, job_id TEXT NOT NULL, ordinal INTEGER NOT NULL CHECK(ordinal > 0), worker_id TEXT NOT NULL,
			status TEXT NOT NULL CHECK(status IN ('leased','succeeded','terminal_failed','retryable_failed','expired')),
			leased_at INTEGER NOT NULL, deadline_at INTEGER NOT NULL, completed_at INTEGER NOT NULL DEFAULT 0,
			failure_disposition TEXT NOT NULL DEFAULT '', failure_code TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '', candidate_sha TEXT NOT NULL DEFAULT '', UNIQUE(job_id, ordinal));
		CREATE UNIQUE INDEX IF NOT EXISTS attempts_one_active_worker ON attempts(worker_id) WHERE status='leased';
		CREATE UNIQUE INDEX IF NOT EXISTS attempts_one_active_job ON attempts(job_id) WHERE status='leased';
		CREATE TABLE IF NOT EXISTS attempt_evidence (
			sequence INTEGER PRIMARY KEY AUTOINCREMENT, job_id TEXT NOT NULL, attempt_id TEXT NOT NULL, evidence_id TEXT NOT NULL,
			phase TEXT NOT NULL, reason TEXT NOT NULL, check_index INTEGER, exit_code INTEGER, duration_ms INTEGER NOT NULL, output TEXT NOT NULL,
			output_redacted INTEGER NOT NULL DEFAULT 0, output_truncated INTEGER NOT NULL, base_sha TEXT NOT NULL, candidate_sha TEXT NOT NULL,
			argv_json TEXT NOT NULL, argv_redacted INTEGER NOT NULL, payload_hash BLOB NOT NULL, bound_at INTEGER NOT NULL, UNIQUE(attempt_id,evidence_id));
		CREATE INDEX IF NOT EXISTS attempt_evidence_attempt ON attempt_evidence(job_id,attempt_id,sequence);`); err != nil {
			return err
		}
		if err := addColumn(tx, "attempt_evidence", "output_redacted", `INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
		now := time.Now().UTC()
		_, err := tx.Exec(`INSERT OR IGNORE INTO attempts(id,job_id,ordinal,worker_id,status,leased_at,deadline_at,completed_at,failure_disposition,failure_code,result,candidate_sha)
			SELECT attempt_id,id,1,worker_id,CASE status WHEN 'leased' THEN 'leased' WHEN 'succeeded' THEN 'succeeded' ELSE 'terminal_failed' END,
			?,?,CASE WHEN status='leased' THEN 0 ELSE ? END,CASE WHEN status='failed' THEN 'terminal' ELSE '' END,error_text,result,candidate_sha FROM jobs WHERE attempt_id<>''`, now.UnixNano(), now.Add(defaultLeaseTTL).UnixNano(), now.UnixNano())
		return err
	case 3:
		for _, column := range []struct{ table, name, definition string }{
			{"jobs", "worker_pool", `TEXT`}, {"jobs", "policy_version", `INTEGER`}, {"jobs", "resolved_policy", `BLOB`},
			{"attempts", "worker_pool", `TEXT`}, {"attempts", "slot", `TEXT`}, {"attempts", "session_generation", `TEXT`}, {"attempts", "policy_version", `INTEGER`}, {"attempts", "resolved_policy", `BLOB`},
			{"workers", "base_worker_id", `TEXT`}, {"workers", "slot", `INTEGER`}, {"workers", "worker_pool", `TEXT`}, {"workers", "generation", `TEXT`},
		} {
			if err := addColumn(tx, column.table, column.name, column.definition); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`CREATE INDEX IF NOT EXISTS jobs_worker_pool_ready ON jobs(worker_pool,status,retry_at,created_at,id)`)
		return err
	default:
		return errors.New("unsupported database schema")
	}
	return nil
}

func addColumn(tx *sql.Tx, table, name, definition string) error {
	rows, err := tx.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notnull, pk int
		var columnName, columnType string
		var defaultValue any
		if err := rows.Scan(&cid, &columnName, &columnType, &notnull, &defaultValue, &pk); err != nil {
			rows.Close()
			return err
		}
		found = found || columnName == name
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	_, err = tx.Exec(`ALTER TABLE ` + table + ` ADD COLUMN ` + name + ` ` + definition)
	return err
}
