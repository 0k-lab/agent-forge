package store

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestJobSourceReferencePersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateJobWithSource("work", "board-item-ready-v3")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}

	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Job(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SourceRef != "board-item-ready-v3" {
		t.Fatalf("source ref = %q", got.SourceRef)
	}
	lease, ok, err := s.LeaseNext("worker-after-restart")
	if err != nil || !ok || lease.JobID != job.ID {
		t.Fatalf("lease after restart = %#v, %v, %v", lease, ok, err)
	}
}

func TestSameSourceReferenceCreatesDistinctJobs(t *testing.T) {
	s := testStore(t)
	first, err := s.CreateJobWithSource("first", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.CreateJobWithSource("second", "same-source")
	if err != nil {
		t.Fatal(err)
	}
	if first.ID == second.ID || first.SourceRef != second.SourceRef {
		t.Fatalf("jobs = %#v, %#v", first, second)
	}
}

func TestSourceReferenceValidationIsBoundedAndAtomic(t *testing.T) {
	s := testStore(t)
	for name, source := range map[string]string{
		"oversized":    strings.Repeat("x", 513),
		"control":      "item\nsecret",
		"invalid utf8": string([]byte{0xff}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.CreateJobWithSource("work", source); err == nil {
				t.Fatal("invalid source reference accepted")
			}
			var jobs, events int
			if err := s.db.QueryRow(`SELECT count(*) FROM jobs`).Scan(&jobs); err != nil {
				t.Fatal(err)
			}
			if err := s.db.QueryRow(`SELECT count(*) FROM events`).Scan(&events); err != nil {
				t.Fatal(err)
			}
			if jobs != 0 || events != 0 {
				t.Fatalf("mutation after rejection: jobs=%d events=%d", jobs, events)
			}
		})
	}
	for _, source := range []string{"", strings.Repeat("x", 512)} {
		if _, err := s.CreateJobWithSource("work", source); err != nil {
			t.Fatalf("valid boundary rejected: %v", err)
		}
	}
}

func TestSourceReferenceDoesNotEnterLeaseOrEventDetail(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJobWithSource("work", "recognizable-source-secret")
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNext("worker")
	if err != nil || !ok || lease.JobID != job.ID {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	body, err := json.Marshal(lease)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(body), "source_ref") || strings.Contains(string(body), "recognizable-source-secret") {
		t.Fatalf("lease exposed source: %s", body)
	}
	events, err := s.Events(job.ID)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if strings.Contains(event.Detail, "recognizable-source-secret") {
			t.Fatalf("event exposed source: %#v", event)
		}
	}
}

func TestSchemaV4MigrationPreservesJobsWithoutSourceReference(t *testing.T) {
	path := filepath.Join(secureTempDir(t), "forge-v4.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	for version := 1; version <= 4; version++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := runMigration(tx, version); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`PRAGMA user_version=` + string(rune('0'+version))); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`INSERT INTO jobs(id,input,status,created_at,updated_at) VALUES('0123456789abcdef0123456789abcdef','legacy','pending','2026-08-29T00:00:00Z','2026-08-29T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	legacy, err := s.Job("0123456789abcdef0123456789abcdef")
	if err != nil || legacy.SourceRef != "" {
		t.Fatalf("legacy job = %#v, %v", legacy, err)
	}
	created, err := s.CreateJobWithSource("new", "opaque-source")
	if err != nil || created.SourceRef != "opaque-source" {
		t.Fatalf("new job = %#v, %v", created, err)
	}
	var version int
	if err := s.db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 5 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
}

func TestJobRejectsCorruptSourceReference(t *testing.T) {
	s := testStore(t)
	job, err := s.CreateJobWithSource("work", "valid")
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{
		"oversized": strings.Repeat("x", 513),
		"control":   "item\nsecret",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := s.db.Exec(`UPDATE jobs SET source_ref=? WHERE id=?`, value, job.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Job(job.ID); err == nil {
				t.Fatal("corrupt source reference accepted")
			}
		})
	}
}
