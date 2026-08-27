package gate

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-forge/internal/store"
	"agent-forge/internal/worker"
)

func publicGateConfig(t *testing.T) Config {
	t.Helper()
	root := filepath.Join(t.TempDir(), "public-repositories")
	gitPath, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	gitPath, err = filepath.Abs(gitPath)
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(validGateConfig, `"repositories": [`, `"public_repository_root":`+quoteJSON(root)+`,"git_executable":`+quoteJSON(gitPath)+`,"repositories": [`, 1)
	body = strings.Replace(body, `"id":"agent-forge"`, `"id":"agent-forge","repository_url":"https://github.com/0k-lab/agent-forge.git"`, 1)
	values := map[string]string{"FORGE_OWNER_TOKEN": "owner", "FORGE_WORKER_TOKEN": "worker"}
	config, err := ParseConfig([]byte(body), func(name string) string { return values[name] })
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func quoteJSON(value string) string {
	body, _ := json.Marshal(value)
	return string(body)
}

func TestPublicRepositoryConfigOwnsCanonicalURLRootAndGit(t *testing.T) {
	config := publicGateConfig(t)
	if len(config.Repositories) != 1 || config.Repositories[0].RepositoryURL != "https://github.com/0k-lab/agent-forge.git" || config.PublicRepositoryRoot == "" || config.GitExecutable == "" {
		t.Fatalf("config = %#v", config)
	}
	for _, invalid := range []string{
		"http://github.com/0k-lab/agent-forge.git",
		"https://user@github.com/0k-lab/agent-forge.git",
		"https://github.com:443/0k-lab/agent-forge.git",
		"https://github.com/0k-lab/../agent-forge.git",
		"https://GitHub.com/0k-lab/agent-forge.git",
		"https://github.com/0K-lab/agent-forge.git",
		"https://github.com/0k-lab/agent-forge",
	} {
		if _, err := canonicalPublicGitHubURL(invalid); err == nil {
			t.Fatalf("accepted %q", invalid)
		}
	}
}

func TestPublicSourceHTTPGitE2EConcurrentCloneAndReuse(t *testing.T) {
	config := publicGateConfig(t)
	fixture, first, second := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	repository := config.Repositories[0]

	var wg sync.WaitGroup
	paths := make(chan string, 2)
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			path, err := provisionPublicRepository(context.Background(), config, repository, first)
			paths <- path
			errs <- err
		}()
	}
	wg.Wait()
	close(paths)
	close(errs)
	var want string
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	for path := range paths {
		if want == "" {
			want = path
		} else if path != want {
			t.Fatalf("paths = %q, %q", want, path)
		}
	}
	if got := gitCommand(t, config.GitExecutable, want, "rev-parse", "refs/heads/main"); got != second {
		t.Fatalf("branch = %s, want %s", got, second)
	}
	gitCommand(t, config.GitExecutable, want, "update-ref", "refs/agent-forge/candidates/test", first)
	if _, err := provisionPublicRepository(context.Background(), config, repository, second); err != nil {
		t.Fatal(err)
	}
	if got := gitCommand(t, config.GitExecutable, want, "rev-parse", "refs/agent-forge/candidates/test"); got != first {
		t.Fatalf("candidate ref = %s, want %s", got, first)
	}
	gitCommand(t, config.GitExecutable, want, "remote", "set-url", "origin", "https://github.com/evil/repository.git")
	if _, err := provisionPublicRepository(context.Background(), config, repository, second); err == nil {
		t.Fatal("accepted origin drift")
	}
}

func TestPublicSubmissionPreparesBeforePendingAndHidesPath(t *testing.T) {
	config := publicGateConfig(t)
	fixture, first, _ := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	handler, err := NewConfiguredHandler(s, config, DefaultOptions())
	if err != nil {
		t.Fatal(err)
	}
	body := `{"repository_id":"agent-forge","base_sha":"` + first + `","instruction":"change"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/jobs", bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer owner")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, req)
	if response.Code != http.StatusCreated || bytes.Contains(response.Body.Bytes(), []byte(config.PublicRepositoryRoot)) || bytes.Contains(response.Body.Bytes(), []byte("repository_url")) {
		t.Fatalf("response = %d %s", response.Code, response.Body.String())
	}
	var public struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(response.Body).Decode(&public); err != nil {
		t.Fatal(err)
	}
	job, err := s.Job(public.ID)
	if err != nil || job.Status != "pending" || job.Task == nil || job.Task.Repository == "" || !strings.HasPrefix(job.Task.Repository, config.PublicRepositoryRoot+string(filepath.Separator)) {
		t.Fatalf("job = %#v, %v", job, err)
	}
}

func TestPublicSourceGateWorkerE2E(t *testing.T) {
	config := publicGateConfig(t)
	fixture, base, _ := gitHTTPFixture(t)
	original := publicCloneURL
	publicCloneURL = func(publicSource) string { return fixture }
	t.Cleanup(func() { publicCloneURL = original })
	s, err := store.Open(filepath.Join(secureTempDir(t), "forge.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	options := DefaultOptions()
	options.LeasePollInterval = time.Millisecond
	handler, err := NewConfiguredHandler(s, config, options)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)

	root := t.TempDir()
	repositories, worktrees, runtime := filepath.Join(root, "repositories"), filepath.Join(root, "worktrees"), filepath.Join(root, "runtime")
	for _, path := range []string{repositories, worktrees, runtime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Fatal(err)
	}
	plugin := `import json,pathlib,sys
i=json.loads(sys.stdin.readline())
print(json.dumps({"version":"v1","id":i["id"],"type":"initialized","capabilities":["workspace_edit","progress","cancel","commit_subject"]},separators=(",",":")),flush=True)
r=json.loads(sys.stdin.readline())
pathlib.Path(r["workspace"],"file").write_text("worker\n")
print(json.dumps({"version":"v1","id":i["id"],"type":"result","commit_subject":"test: worker candidate"},separators=(",",":")),flush=True)`
	workerBody, _ := json.Marshal(map[string]any{
		"version": 1, "gate_url": "ws" + strings.TrimPrefix(server.URL, "http"), "id": "worker-1", "token_env": "FORGE_WORKER_TOKEN", "heartbeat_interval": "10ms", "concurrency": 1,
		"repository_roots": []string{repositories}, "worktree_root": worktrees, "runtime_root": runtime, "repositories": []any{},
		"plugins":               []any{map[string]any{"id": "codex", "argv": []string{python, "-c", plugin}}},
		"environment_allowlist": []string{"PATH", "CODEX_HOME"}, "check_environment_allowlist": []string{"PATH"},
		"ceilings": map[string]any{"plugin_timeout": "15m", "check_timeout": "10m", "git_timeout": "2m", "cleanup_timeout": "10s", "plugin_output_bytes": 1048576, "check_output_bytes": 2048, "git_output_bytes": 1048576},
	})
	workerConfig, err := worker.ParseConfig(workerBody, func(name string) string {
		if name == "FORGE_WORKER_TOKEN" {
			return "worker"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	workerDone := make(chan error, 1)
	go func() { workerDone <- worker.RunConfigured(ctx, workerConfig) }()
	t.Cleanup(func() {
		cancel()
		<-workerDone
	})

	body := `{"repository_id":"agent-forge","base_sha":"` + base + `","instruction":"change","tests":[["true"]]}`
	req, _ := http.NewRequest(http.MethodPost, server.URL+"/v1/jobs", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer owner")
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var submitted struct {
		ID string `json:"id"`
	}
	if response.StatusCode != http.StatusCreated || json.NewDecoder(response.Body).Decode(&submitted) != nil {
		t.Fatalf("submit status = %d", response.StatusCode)
	}
	var job store.Job
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		job, err = s.Job(submitted.ID)
		if err == nil && (job.Status == "succeeded" || job.Status == "failed") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if job.Status != "succeeded" || job.CandidateSHA == "" || job.Task == nil || job.Task.Repository == "" {
		t.Fatalf("job = %#v, %v", job, err)
	}
	attempts, err := s.Attempts(job.ID)
	if err != nil || len(attempts) != 1 {
		t.Fatalf("attempts = %#v, %v", attempts, err)
	}
	ref := "refs/agent-forge/candidates/" + job.ID + "/" + attempts[0].ID
	if got := gitCommand(t, config.GitExecutable, job.Task.Repository, "rev-parse", ref); got != job.CandidateSHA {
		t.Fatalf("candidate ref = %s, want %s", got, job.CandidateSHA)
	}
	entries, err := os.ReadDir(worktrees)
	if err != nil || len(entries) != 0 {
		t.Fatalf("worktree cleanup = %#v, %v", entries, err)
	}
}

func gitHTTPFixture(t *testing.T) (string, string, string) {
	t.Helper()
	root := t.TempDir()
	source, bare := filepath.Join(root, "source"), filepath.Join(root, "repo.git")
	if err := os.Mkdir(source, 0o700); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, "git", source, "init", "-b", "main")
	gitCommand(t, "git", source, "config", "user.name", "Test")
	gitCommand(t, "git", source, "config", "user.email", "test@example.invalid")
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("one"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, "git", source, "add", "file")
	gitCommand(t, "git", source, "commit", "-m", "one")
	first := gitCommand(t, "git", source, "rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(source, "file"), []byte("two"), 0o600); err != nil {
		t.Fatal(err)
	}
	gitCommand(t, "git", source, "commit", "-am", "two")
	second := gitCommand(t, "git", source, "rev-parse", "HEAD")
	gitCommand(t, "git", root, "clone", "--bare", source, bare)
	gitCommand(t, "git", bare, "update-server-info")
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewUnstartedServer(http.FileServer(http.Dir(root)))
	server.Listener = listener
	server.Start()
	t.Cleanup(server.Close)
	return server.URL + "/repo.git", first, second
}

func gitCommand(t *testing.T, executable, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(executable, append([]string{"-C", dir}, args...)...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}
