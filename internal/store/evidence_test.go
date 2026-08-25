package store

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/protocol"
)

func TestAttemptEvidenceSurvivesStoreReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "forge.db")
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	records := []protocol.AttemptEvidence{{
		EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin,
		Reason: protocol.EvidenceReasonPluginFailed, DurationMS: 25,
		Output: protocol.EvidenceRedactedMarker, OutputRedacted: true, BaseSHA: base,
	}}

	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	job, err := s.CreateCodingJob(protocol.CodingTask{
		Repository: "/private/repository", BaseSHA: base, Instruction: "private prompt", Tests: [][]string{{"go", "test", "./..."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`DROP TABLE attempt_evidence`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", records, start.Add(time.Second)); err != nil {
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
	got, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, records) {
		t.Fatalf("evidence after reopen = %#v, want %#v", got, records)
	}
}

func TestBindEvidenceReplayIsIdempotentAndConflictsFailClosed(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{
		Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"go", "test"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	record := protocol.AttemptEvidence{
		EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin,
		Reason: protocol.EvidenceReasonPluginFailed, DurationMS: 25, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true, BaseSHA: base,
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(2*time.Second)); err != nil {
		t.Fatalf("exact replay = %v", err)
	}
	conflict := record
	conflict.DurationMS++
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{conflict}, start.Add(3*time.Second)); err == nil {
		t.Fatal("conflicting replay accepted")
	}
	got, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0], record) {
		t.Fatalf("evidence = %#v, %v", got, err)
	}
}

func TestBindEvidenceAcceptsOnlyEmptyOrFixedMarkerOutput(t *testing.T) {
	for _, output := range []string{
		`["Authorization","Bearer reviewer-json-array-secret"]`,
		`file:///private/reviewer/home/config`,
		`arbitrary-unlabeled-reviewer-secret`,
		protocol.EvidenceRedactedMarker + "\nsafe suffix",
	} {
		t.Run(output, func(t *testing.T) {
			s, job, lease, start, base := evidenceLease(t, [][]string{{"true"}})
			record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base, Output: output, OutputRedacted: true}
			if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
				t.Fatal("arbitrary non-empty evidence accepted")
			}
		})
	}

	s, job, lease, start, base := evidenceLease(t, [][]string{{"true"}})
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("2", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatalf("fixed marker rejected: %v", err)
	}
	got, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil || len(got) != 1 || got[0].Output != protocol.EvidenceRedactedMarker {
		t.Fatalf("stored evidence=%#v err=%v", got, err)
	}
}

func TestCheckEvidenceCanonicalArgvIsOnlyFixedPlaceholders(t *testing.T) {
	tests := [][]string{{
		"/private/bin/reviewer-check",
		`["Authorization","Bearer argv-json-secret"]`,
		`file:///private/reviewer/config`,
		"arbitrary-unlabeled-argv-secret",
	}}
	s, job, lease, start, base := evidenceLease(t, tests)
	index := 0
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &index, BaseSHA: base}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	want := []string{protocol.EvidenceRedactedMarker, protocol.EvidenceRedactedMarker, protocol.EvidenceRedactedMarker, protocol.EvidenceRedactedMarker}
	got, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil || len(got) != 1 || !reflect.DeepEqual(got[0].Argv, want) || !got[0].ArgvRedacted {
		t.Fatalf("canonical argv=%#v err=%v", got, err)
	}
	canonical, err := prepareEvidence(record, protocol.CodingTask{Repository: "/repo", BaseSHA: base, Tests: tests})
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(canonical)
	for _, raw := range tests[0] {
		if bytes.Contains(payload, []byte(raw)) {
			t.Fatalf("canonical payload contains raw argv %q: %s", raw, payload)
		}
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(2*time.Second)); err != nil {
		t.Fatalf("canonical replay changed: %v", err)
	}
}

func evidenceLease(t *testing.T, tests [][]string) (*Store, Job, Lease, time.Time, string) {
	t.Helper()
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: tests})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease=%#v ok=%v err=%v", lease, ok, err)
	}
	return s, job, lease, start, base
}

func TestBindEvidenceRejectsEmptyUnsafeAndInvalidExitCode(t *testing.T) {
	newLease := func(t *testing.T) (*Store, Job, Lease, time.Time, string) {
		t.Helper()
		s := testStore(t)
		start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
		base := strings.Repeat("a", 40)
		job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
		if err != nil || !ok {
			t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
		}
		return s, job, lease, start, base
	}

	t.Run("empty bind", func(t *testing.T) {
		s, job, lease, start, _ := newLease(t)
		if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", nil, start.Add(time.Second)); err == nil {
			t.Fatal("empty evidence accepted")
		}
	})

	for _, output := range []string{
		"Authorization: Bearer secret", "authorization: Basic secret",
		"ghp_abcdefghijklmnopqrstuvwxyz0123456789", "github_pat_abcdefghijklmnopqrstuvwxyz_0123456789",
		"eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.signature", "AKIAIOSFODNN7EXAMPLE",
		"-----BEGIN PRIVATE KEY-----", "PASSWORD=hunter2", "/repo/private", "/tmp/private", string([]byte{'b', 'a', 'd', 0xff}),
		"https://synthetic-user:synthetic-pass@example.invalid/api", "CrEdEnTiAl:synthetic-colon-secret",
		"https://example.invalid/?password=synthetic-query-secret", "AUTHORIZATION = synthetic-equals-auth",
		"OPENAI_API_KEY: synthetic-reviewer-key", "DbPaSsWoRd: synthetic-db-password",
		"https://example.invalid/?client_secret=synthetic-client-secret&x-api-key=synthetic-prefixed-key&safe=kept",
		"workspace: /synthetic/private/alice/file", `config: C:\synthetic\private\alice\file`,
		`{"Authorization":"Bearer synthetic JSON secret","safe":"kept"}`,
		`{'api_key': 'synthetic single quoted secret', 'safe': 'kept'}`,
		"check --token synthetic CLI token --later tail; safe",
		"[credential]: synthetic bracket credential\nsafe",
		"Access Token: synthetic-access-token; safe", "Client Secret = synthetic-client-secret&safe=kept",
		"Authorization Header: Basic synthetic-review-value; safe", "Database Password Value: synthetic-db-value; safe",
		`"Access Token": "synthetic-quoted-token", "safe":"kept"`, "[Client Secret]: synthetic-bracketed-secret\nsafe",
		"check --access token synthetic-cli-token; safe", "check --client secret synthetic-cli-secret && safe",
		`workspace: \\synthetic-server\private\output`, `\\synthetic-server\private\output`,
	} {
		t.Run("unsafe output", func(t *testing.T) {
			s, job, lease, start, base := newLease(t)
			record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base, Output: output}
			if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
				t.Fatal("unsafe evidence accepted")
			}
			var count int
			if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempt_evidence`).Scan(&count); err != nil || count != 0 {
				t.Fatalf("unsafe evidence persisted: count=%d err=%v", count, err)
			}
		})
	}

	for _, exitCode := range []int{-1, 256} {
		t.Run(fmt.Sprintf("exit %d", exitCode), func(t *testing.T) {
			s, job, lease, start, base := newLease(t)
			index := 0
			record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckFailed, CheckIndex: &index, ExitCode: &exitCode, BaseSHA: base}
			if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
				t.Fatal("invalid exit code accepted")
			}
		})
	}
}

func TestBindEvidenceRejectsTruncatedPartialFinalRecord(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	partial := "synthetic-user:synthetic-password"
	record := protocol.AttemptEvidence{
		EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin,
		Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base,
		Output: "[REDACTED]\nhttps://" + partial, OutputRedacted: true, OutputTruncated: true,
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
		t.Fatal("truncated partial final record accepted")
	}
	var count int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM attempt_evidence`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial userinfo reached canonical hash input: count=%d err=%v", count, err)
	}
	record.EvidenceID = strings.Repeat("2", 32)
	record.Output = protocol.EvidenceRedactedMarker
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	canonical, err := prepareEvidence(record, protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(canonical)
	if err != nil || bytes.Contains(payload, []byte(partial)) {
		t.Fatalf("canonical payload contains partial userinfo: %s, %v", payload, err)
	}
	want := sha256.Sum256(payload)
	var got []byte
	if err := s.db.QueryRow(`SELECT payload_hash FROM attempt_evidence WHERE attempt_id=?`, lease.AttemptID).Scan(&got); err != nil || !bytes.Equal(got, want[:]) {
		t.Fatalf("stored hash is not canonical: got=%x want=%x err=%v", got, want, err)
	}
}

func TestBindEvidenceHashesOnlyCanonicalSafePayload(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	task := protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"check", "https://synthetic-hash-user:synthetic-hash-pass@example.invalid", "credential:synthetic-hash-secret", "workspace:/synthetic/private/hash", `{"token":"synthetic-hash-json","safe":"kept"}`, "--authorization synthetic-hash CLI secret; safe", `[credential]: synthetic-hash-bracket`, "Access Token: synthetic-hash-access-token", "Authorization Header: Basic synthetic-hash-review-value", "Database Password Value: synthetic-hash-db-value", "--client secret synthetic-hash-client-secret; safe", `workspace: \\synthetic-hash-server\private\file`}}}
	job, err := s.CreateCodingJob(task)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	index := 0
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &index, BaseSHA: base, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	canonical, err := prepareEvidence(record, task)
	if err != nil {
		t.Fatal(err)
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{"synthetic-hash-user", "synthetic-hash-pass", "synthetic-hash-secret", "/synthetic/private/hash", "synthetic-hash-json", "synthetic-hash CLI secret", "synthetic-hash-bracket", "synthetic-hash-access-token", "synthetic-hash-review-value", "synthetic-hash-db-value", "synthetic-hash-client-secret", `\\synthetic-hash-server\private\file`} {
		if bytes.Contains(canonicalJSON, []byte(private)) {
			t.Fatalf("canonical payload retained %q: %s", private, canonicalJSON)
		}
	}
	payload, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	want := sha256.Sum256(payload)
	var got []byte
	if err := s.db.QueryRow(`SELECT payload_hash FROM attempt_evidence WHERE attempt_id=?`, lease.AttemptID).Scan(&got); err != nil || !bytes.Equal(got, want[:]) {
		t.Fatalf("payload hash is not canonical: got=%x want=%x err=%v", got, want, err)
	}
}

func TestBindEvidenceValidatesOutputRedactionMarker(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base, repository := strings.Repeat("a", 40), "/private/repository"
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: repository, BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	valid := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base, Output: protocol.EvidenceRedactedMarker, OutputRedacted: true}
	for name, mutate := range map[string]func(*protocol.AttemptEvidence){
		"missing marker": func(r *protocol.AttemptEvidence) { r.EvidenceID = strings.Repeat("2", 32); r.OutputRedacted = false },
		"false marker":   func(r *protocol.AttemptEvidence) { r.EvidenceID = strings.Repeat("3", 32); r.Output = "safe output" },
	} {
		t.Run(name, func(t *testing.T) {
			record := valid
			mutate(&record)
			if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
				t.Fatal("inconsistent redaction marker accepted")
			}
		})
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{valid}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestBindEvidenceRejectsUnknownAndOversizedPayloads(t *testing.T) {
	newLease := func(t *testing.T) (*Store, Job, Lease, time.Time, string) {
		t.Helper()
		s := testStore(t)
		start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
		base := strings.Repeat("a", 40)
		job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Hour, BaseRetryBackoff: time.Second, MaxAttempts: 3})
		if err != nil || !ok {
			t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
		}
		return s, job, lease, start, base
	}
	valid := func(id, base string) protocol.AttemptEvidence {
		return protocol.AttemptEvidence{EvidenceID: id, Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, DurationMS: 1, BaseSHA: base}
	}

	for name, mutate := range map[string]func(*protocol.AttemptEvidence){
		"unknown phase":    func(r *protocol.AttemptEvidence) { r.Phase = "invented" },
		"unknown reason":   func(r *protocol.AttemptEvidence) { r.Reason = "invented" },
		"uppercase ID":     func(r *protocol.AttemptEvidence) { r.EvidenceID = strings.Repeat("A", 32) },
		"wrong base":       func(r *protocol.AttemptEvidence) { r.BaseSHA = strings.Repeat("b", 40) },
		"uppercase SHA":    func(r *protocol.AttemptEvidence) { r.CandidateSHA = strings.Repeat("C", 40) },
		"negative time":    func(r *protocol.AttemptEvidence) { r.DurationMS = -1 },
		"excessive time":   func(r *protocol.AttemptEvidence) { r.DurationMS = int64((15*time.Minute)/time.Millisecond) + 1 },
		"oversized output": func(r *protocol.AttemptEvidence) { r.Output = strings.Repeat("x", 2049) },
		"worker argv":      func(r *protocol.AttemptEvidence) { r.Argv = []string{"untrusted"} },
	} {
		t.Run(name, func(t *testing.T) {
			s, job, lease, start, base := newLease(t)
			record := valid(strings.Repeat("1", 32), base)
			mutate(&record)
			if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}

	t.Run("record count", func(t *testing.T) {
		s, job, lease, start, base := newLease(t)
		records := make([]protocol.AttemptEvidence, 35)
		for i := range records {
			records[i] = valid(strings.Repeat("0", 30)+fmt.Sprintf("%02x", i), base)
		}
		if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", records, start.Add(time.Second)); err == nil {
			t.Fatal("oversized record batch accepted")
		}
	})

	t.Run("total output", func(t *testing.T) {
		s, job, lease, start, base := newLease(t)
		records := make([]protocol.AttemptEvidence, 33)
		for i := range records {
			records[i] = valid(strings.Repeat("0", 30)+fmt.Sprintf("%02x", i), base)
			records[i].Output = strings.Repeat("x", 2048)
		}
		if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", records, start.Add(time.Second)); err == nil {
			t.Fatal("oversized total output accepted")
		}
	})
}

func TestCheckEvidenceUsesAuthoritativeArgvAndRedactsPrivateValues(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	repository := "/private/alice/repository"
	task := protocol.CodingTask{
		Repository: repository, BaseSHA: base, Instruction: "private prompt",
		Tests: [][]string{
			{"go", "test", "./internal/store"},
			{"private-check", repository + "/secret", "--token=argv-secret", "PRIVATE_ENV=env-secret", "/tmp/worktree/secret", "--endpoint=https://synthetic-user:synthetic-pass@example.invalid/?client_secret=url-secret&x-api-key=prefixed-key&safe=kept", `config:C:\private\secret`, "credential:colon-secret", "OPENAI_API_KEY:reviewer-key", "DbPaSsWoRd:mixed-password", "Authorization: Bearer argv secret; safe=kept", "--endpoint=https://example.invalid/?credential=argv value with spaces&safe=kept#fragment", `{"Authorization":"Bearer argv JSON secret","safe":"kept"}`, `{'api_key': 'argv single quoted secret', 'safe': 'kept'}`, "-credential argv single-hyphen secret --later tail; safe", "--credential argv CLI secret --later tail; safe", "[token]: argv bracket secret\nsafe", `workspace: \\argv-server\private\file`, `\\standalone-argv-server\private\file`},
		},
	}
	job, err := s.CreateCodingJob(task)
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	zero, one, exit := 0, 1, 1
	records := []protocol.AttemptEvidence{
		{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &zero, DurationMS: 10, BaseSHA: base},
		{EvidenceID: strings.Repeat("2", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckFailed, CheckIndex: &one, ExitCode: &exit, DurationMS: 20,
			Output: protocol.EvidenceRedactedMarker, OutputRedacted: true, BaseSHA: base},
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", records, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	got, err := s.AttemptEvidence(job.ID, lease.AttemptID)
	if err != nil || len(got) != 2 {
		t.Fatalf("evidence = %#v, %v", got, err)
	}
	for i, record := range got {
		if len(record.Argv) != len(task.Tests[i]) || !record.ArgvRedacted {
			t.Fatalf("argv %d = %#v redacted=%v", i, record.Argv, record.ArgvRedacted)
		}
		for _, arg := range record.Argv {
			if arg != protocol.EvidenceRedactedMarker {
				t.Fatalf("raw argv retained: %#v", record.Argv)
			}
		}
	}
	body, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	for _, private := range []string{repository, "argv-secret", "env-secret", "output-secret", "output-env", "/tmp/worktree", "url-secret", "url-output-secret", "synthetic-user", "synthetic-pass", `C:\private\secret`, "colon-secret", "reviewer-key", "mixed-password", "prefixed-key", "Bearer argv secret", "argv value with spaces", "argv JSON secret", "argv single quoted secret", "argv single-hyphen secret", "argv CLI secret", "later tail", "argv bracket secret", `\\argv-server\private\file`, `\\standalone-argv-server\private\file`} {
		if strings.Contains(string(body), private) {
			t.Fatalf("evidence exposed %q: %s", private, body)
		}
	}
	if strings.Count(string(body), protocol.EvidenceRedactedMarker) != len(task.Tests[0])+len(task.Tests[1])+1 {
		t.Fatalf("unexpected marker count: %s", body)
	}
	bad := records[0]
	bad.EvidenceID = strings.Repeat("3", 32)
	bad.CheckIndex = func() *int { value := len(task.Tests); return &value }()
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{bad}, start.Add(2*time.Second)); err == nil {
		t.Fatal("out-of-range check index accepted")
	}
}

func TestBindEvidenceRequiresExactCurrentLiveCodingLease(t *testing.T) {
	policy := RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3}
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhasePlugin, Reason: protocol.EvidenceReasonPluginFailed, BaseSHA: base}

	for name, bind := range map[string]func(*testing.T, *Store, Job, Lease) error{
		"other worker": func(_ *testing.T, s *Store, job Job, lease Lease) error {
			return s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-2", []protocol.AttemptEvidence{record}, start.Add(time.Second))
		},
		"wrong attempt": func(_ *testing.T, s *Store, job Job, _ Lease) error {
			return s.BindEvidenceAt(job.ID, strings.Repeat("f", 32), "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second))
		},
		"expiry boundary": func(_ *testing.T, s *Store, job Job, lease Lease) error {
			return s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(policy.LeaseTTL))
		},
		"terminal": func(t *testing.T, s *Store, job Job, lease Lease) error {
			if _, err := s.CompleteCandidateAt(job.ID, lease.AttemptID, strings.Repeat("b", 40), start.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			return s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(2*time.Second))
		},
		"superseded": func(t *testing.T, s *Store, job Job, lease Lease) error {
			if _, err := s.FailAt(job.ID, lease.AttemptID, "retry", RetryableFailure, start.Add(time.Second), policy); err != nil {
				t.Fatal(err)
			}
			if _, ok, err := s.LeaseNextAt("worker-2", start.Add(2*time.Second), policy); err != nil || !ok {
				t.Fatalf("retry lease: ok=%v err=%v", ok, err)
			}
			return s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(3*time.Second))
		},
	} {
		t.Run(name, func(t *testing.T) {
			s := testStore(t)
			job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
			if err != nil {
				t.Fatal(err)
			}
			lease, ok, err := s.LeaseNextAt("worker-1", start, policy)
			if err != nil || !ok {
				t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
			}
			if err := bind(t, s, job, lease); err == nil {
				t.Fatal("unauthorized evidence accepted")
			}
		})
	}

	t.Run("legacy task", func(t *testing.T) {
		s := testStore(t)
		job, err := s.CreateJob("legacy")
		if err != nil {
			t.Fatal(err)
		}
		lease, ok, err := s.LeaseNextAt("worker-1", start, policy)
		if err != nil || !ok {
			t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
		}
		if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", nil, start.Add(time.Second)); err == nil {
			t.Fatal("legacy task accepted evidence")
		}
	})
}

func TestCandidateCompletionMatchesBoundEvidence(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	record := protocol.AttemptEvidence{
		EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseCleanup, Reason: protocol.EvidenceReasonCleanupFailed,
		DurationMS: 1, BaseSHA: base, CandidateSHA: candidate,
	}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteCandidateAt(job.ID, lease.AttemptID, strings.Repeat("c", 40), start.Add(2*time.Second)); err == nil {
		t.Fatal("candidate conflicting with evidence accepted")
	}
	if _, err := s.CompleteCandidateAt(job.ID, lease.AttemptID, candidate, start.Add(2*time.Second)); err != nil {
		t.Fatalf("matching candidate rejected: %v", err)
	}
}

func TestCandidateCompletionRequiresEveryBoundCheckToNameCandidate(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base, candidate := strings.Repeat("a", 40), strings.Repeat("b", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"true"}}})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	index, exit := 0, 0
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckPassed, CheckIndex: &index, ExitCode: &exit, BaseSHA: base}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.CompleteCandidateAt(job.ID, lease.AttemptID, candidate, start.Add(2*time.Second)); err == nil {
		t.Fatal("candidate completion accepted base-only check evidence")
	}
}

func TestFailedCheckEvidenceRemainsBaseBound(t *testing.T) {
	s := testStore(t)
	start := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	base := strings.Repeat("a", 40)
	job, err := s.CreateCodingJob(protocol.CodingTask{Repository: "/repo", BaseSHA: base, Instruction: "edit", Tests: [][]string{{"false"}}})
	if err != nil {
		t.Fatal(err)
	}
	lease, ok, err := s.LeaseNextAt("worker-1", start, RecoveryPolicy{LeaseTTL: time.Minute, BaseRetryBackoff: time.Second, MaxAttempts: 3})
	if err != nil || !ok {
		t.Fatalf("lease = %#v, %v, %v", lease, ok, err)
	}
	index, exit := 0, 1
	record := protocol.AttemptEvidence{EvidenceID: strings.Repeat("1", 32), Phase: protocol.EvidencePhaseScopedCheck, Reason: protocol.EvidenceReasonScopedCheckFailed, CheckIndex: &index, ExitCode: &exit, BaseSHA: base, CandidateSHA: strings.Repeat("b", 40)}
	if err := s.BindEvidenceAt(job.ID, lease.AttemptID, "worker-1", []protocol.AttemptEvidence{record}, start.Add(time.Second)); err == nil {
		t.Fatal("failed pre-commit check accepted candidate binding")
	}
}
