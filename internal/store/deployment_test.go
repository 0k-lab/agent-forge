package store

import (
	"bytes"
	"database/sql"
	"reflect"
	"strings"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

func TestCanonicalDeploymentProfileContract(t *testing.T) {
	profile := ResolvedDeploymentProfile{
		Version:       1,
		ID:            "staging",
		Target:        "staging-app",
		Prepare:       ResolvedDeploymentCommand{Argv: []string{"/opt/forge/prepare", "staging"}, TimeoutNanos: int64(2 * time.Minute)},
		Activate:      ResolvedDeploymentCommand{Argv: []string{"/opt/forge/activate", "staging"}, TimeoutNanos: int64(30 * time.Second)},
		Healthcheck:   ResolvedDeploymentCommand{Argv: []string{"/opt/forge/healthcheck", "staging"}, TimeoutNanos: int64(10 * time.Second)},
		CleanupPolicy: "restore_previous",
	}
	body, err := CanonicalDeploymentProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"version":1,"id":"staging","target":"staging-app","prepare":{"argv":["/opt/forge/prepare","staging"],"timeout_nanos":120000000000},"activate":{"argv":["/opt/forge/activate","staging"],"timeout_nanos":30000000000},"healthcheck":{"argv":["/opt/forge/healthcheck","staging"],"timeout_nanos":10000000000},"cleanup_policy":"restore_previous"}`
	if string(body) != want {
		t.Fatalf("canonical profile = %s", body)
	}
	decoded, err := DecodeCanonicalDeploymentProfile(body)
	if err != nil || !reflect.DeepEqual(decoded, profile) {
		t.Fatalf("decoded profile = %#v, %v", decoded, err)
	}

	invalid := map[string][]byte{
		"noncanonical whitespace": append(append([]byte(nil), body...), ' '),
		"trailing data":           append(append([]byte(nil), body...), []byte(`{}`)...),
		"duplicate field":         []byte(strings.Replace(want, `"version":1`, `"version":1,"version":1`, 1)),
		"unknown field":           []byte(strings.Replace(want, `"target":"staging-app"`, `"target":"staging-app","unknown":true`, 1)),
		"null field":              []byte(strings.Replace(want, `"id":"staging"`, `"id":null`, 1)),
		"unsafe id":               []byte(strings.Replace(want, `"id":"staging"`, `"id":"../staging"`, 1)),
		"unsafe target":           []byte(strings.Replace(want, `"target":"staging-app"`, `"target":"../staging"`, 1)),
		"relative executable":     []byte(strings.Replace(want, `"/opt/forge/prepare"`, `"opt/forge/prepare"`, 1)),
		"empty executable":        []byte(strings.Replace(want, `"/opt/forge/prepare"`, `""`, 1)),
		"empty argv element":      []byte(strings.Replace(want, `"/opt/forge/prepare","staging"`, `"/opt/forge/prepare",""`, 1)),
		"empty argv":              []byte(strings.Replace(want, `"argv":["/opt/forge/prepare","staging"]`, `"argv":[]`, 1)),
		"zero timeout":            []byte(strings.Replace(want, `120000000000`, `0`, 1)),
		"timeout over bound":      []byte(strings.Replace(want, `120000000000`, `86400000000001`, 1)),
		"unknown policy":          []byte(strings.Replace(want, `"restore_previous"`, `"restore"`, 1)),
		"credential field":        []byte(strings.Replace(want, `"target":"staging-app"`, `"target":"staging-app","credential":"secret"`, 1)),
		"environment field":       []byte(strings.Replace(want, `"timeout_nanos":120000000000`, `"timeout_nanos":120000000000,"environment":["SECRET"]`, 1)),
		"token field":             []byte(strings.Replace(want, `"cleanup_policy":"restore_previous"`, `"cleanup_policy":"restore_previous","token":"secret"`, 1)),
	}
	for name, candidate := range invalid {
		t.Run(name, func(t *testing.T) {
			if _, err := DecodeCanonicalDeploymentProfile(candidate); err == nil {
				t.Fatal("accepted invalid deployment profile")
			}
		})
	}
	for _, policy := range []string{"retain", "cleanup"} {
		candidate := profile
		candidate.CleanupPolicy = policy
		if _, err := CanonicalDeploymentProfile(candidate); err != nil {
			t.Fatalf("rejected cleanup policy %q: %v", policy, err)
		}
	}
}

func TestMigrationSixDeploymentAttempts(t *testing.T) {
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for version := 1; version <= 5; version++ {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := runMigration(tx, version); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := db.Exec(`PRAGMA user_version=5; INSERT INTO metadata(key,value) VALUES('preserved',x'01')`); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatal(err)
	}
	if err := migrate(db); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
	var version, preserved int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 6 {
		t.Fatalf("schema version = %d, %v", version, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM metadata WHERE key='preserved' AND value=x'01'`).Scan(&preserved); err != nil || preserved != 1 {
		t.Fatalf("schema-5 data was not preserved: %d, %v", preserved, err)
	}

	profile, err := CanonicalDeploymentProfile(ResolvedDeploymentProfile{
		Version: 1, ID: "staging", Target: "staging-app",
		Prepare:       ResolvedDeploymentCommand{Argv: []string{"/bin/true"}, TimeoutNanos: int64(time.Second)},
		Activate:      ResolvedDeploymentCommand{Argv: []string{"/bin/true"}, TimeoutNanos: int64(time.Second)},
		Healthcheck:   ResolvedDeploymentCommand{Argv: []string{"/bin/true"}, TimeoutNanos: int64(time.Second)},
		CleanupPolicy: "retain",
	})
	if err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO deployment_attempts(id,job_id,attempt_id,kind,repository_id,base_sha,candidate_sha,expected_tree_sha,profile_id,target,profile_version,profile_snapshot,phase,failure_code,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`
	valid := []any{strings.Repeat("e", 32), strings.Repeat("a", 32), strings.Repeat("b", 32), "preview", "agent-forge", strings.Repeat("c", 40), strings.Repeat("d", 40), strings.Repeat("e", 40), "staging", "staging-app", 1, profile, "awaiting_acceptance", "", int64(1), int64(1)}
	if _, err := db.Exec(insert, valid...); err != nil {
		t.Fatalf("valid deployment attempt rejected: %v", err)
	}

	invalid := map[string]func([]any){
		"short id":                    func(v []any) { v[0] = strings.Repeat("e", 31) },
		"uppercase id":                func(v []any) { v[0] = strings.Repeat("E", 32) },
		"nonhex id":                   func(v []any) { v[0] = strings.Repeat("g", 32) },
		"short job id":                func(v []any) { v[1] = strings.Repeat("a", 31) },
		"uppercase job id":            func(v []any) { v[1] = strings.Repeat("A", 32) },
		"nonhex job id":               func(v []any) { v[1] = strings.Repeat("g", 32) },
		"short attempt id":            func(v []any) { v[2] = strings.Repeat("b", 31) },
		"uppercase attempt id":        func(v []any) { v[2] = strings.Repeat("B", 32) },
		"nonhex attempt id":           func(v []any) { v[2] = strings.Repeat("g", 32) },
		"unknown kind":                func(v []any) { v[3] = "production" },
		"uppercase base sha":          func(v []any) { v[5] = strings.Repeat("A", 40) },
		"short candidate sha":         func(v []any) { v[6] = strings.Repeat("d", 39) },
		"null expected tree sha":      func(v []any) { v[7] = nil },
		"short expected tree sha":     func(v []any) { v[7] = strings.Repeat("e", 39) },
		"uppercase expected tree sha": func(v []any) { v[7] = strings.Repeat("E", 40) },
		"succeeded phase":             func(v []any) { v[12] = "succeeded" },
		"unknown phase":               func(v []any) { v[12] = "unknown" },
		"overlong failure":            func(v []any) { v[13] = strings.Repeat("x", 65) },
		"text snapshot":               func(v []any) { v[11] = string(profile) },
		"zero timestamp":              func(v []any) { v[14] = int64(0) },
		"backwards timestamp":         func(v []any) { v[14], v[15] = int64(2), int64(1) },
	}
	for name, mutate := range invalid {
		t.Run(name, func(t *testing.T) {
			candidate := append([]any(nil), valid...)
			candidate[0] = strings.Repeat("f", 32)
			mutate(candidate)
			if _, err := db.Exec(insert, candidate...); err == nil {
				t.Fatal("constraint accepted invalid deployment attempt")
			}
		})
	}
	var stored []byte
	if err := db.QueryRow(`SELECT profile_snapshot FROM deployment_attempts WHERE id=?`, valid[0]).Scan(&stored); err != nil || !bytes.Equal(stored, profile) {
		t.Fatalf("profile snapshot changed: %x, %v", stored, err)
	}
}
