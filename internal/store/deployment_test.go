package store

import (
	"reflect"
	"strings"
	"testing"
	"time"
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
