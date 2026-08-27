package githubdelivery

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestConfigIsStrictAndProductionOnly(t *testing.T) {
	key := testKey(t)
	repo := t.TempDir()
	gitExecutable := testExecutable(t)
	if err := os.Chmod(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	if _, err := ownedDirectory(repo); err != nil {
		t.Fatalf("repo fixture: %v", err)
	}
	if _, err := protectedFile(key); err != nil {
		fileInfo, _ := os.Stat(key)
		parentInfo, _ := os.Stat(filepath.Dir(key))
		t.Fatalf("key fixture: %v file=%v owned=%v parent=%v owned=%v", err, fileInfo.Mode(), owned(fileInfo), parentInfo.Mode(), owned(parentInfo))
	}
	valid := fmt.Sprintf(`{"version":1,"api_base":"https://api.github.com","owner":"octo","repository":"repo","local_repository":%q,"git_executable":%q,"github_app_id_env":"APP_ID","github_app_private_key_path":%q}`, repo, gitExecutable, key)
	if _, err := ParseConfig([]byte(valid), func(string) string { return "123" }); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{
		strings.Replace(valid, `,"git_executable":`+fmt.Sprintf("%q", gitExecutable), "", 1),
		strings.Replace(valid, `"version":1`, `"version":2`, 1),
		strings.Replace(valid, `"api_base":"https://api.github.com"`, `"api_base":"http://localhost"`, 1),
		strings.TrimSuffix(valid, "}") + `,"token":"secret"}`,
		strings.Replace(valid, `"owner":"octo"`, `"owner":"octo","owner":"other"`, 1),
	} {
		if _, err := ParseConfig([]byte(body), func(string) string { return "123" }); err == nil {
			t.Fatalf("accepted invalid config: %s", body)
		}
	}
}

func testExecutable(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	realGit := testGitPath(t)
	path := filepath.Join(dir, "git")
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexec \""+realGit+"\" \"$@\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestRequiredPermissionsFollowExactDiff(t *testing.T) {
	if got := RequiredPermissions([]string{"README.md", "internal/x.go"}); fmt.Sprint(got) != "map[contents:write pull_requests:write]" {
		t.Fatalf("permissions = %v", got)
	}
	for _, path := range []string{".github/workflows/ci.yml", ".github/workflows"} {
		if got := RequiredPermissions([]string{path}); got["workflows"] != "write" {
			t.Fatalf("workflow permissions = %v", got)
		}
	}
}

func TestChangedPathsAreExactAndRequestWorkflowPermission(t *testing.T) {
	changedPath := ".github/workflows/ leading-é\nci.yml"
	repo, base, candidate, tree, ref := testRepository(t, changedPath)
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver"}
	var diffArgs []string
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "diff") {
			diffArgs = append([]string(nil), args...)
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	changed, err := Preflight(context.Background(), Config{LocalRepository: repo, GitExecutable: testExecutable(t)}, input, Options{Run: run})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(changed, []string{changedPath}) {
		t.Fatalf("changed paths = %q, want %q", changed, []string{changedPath})
	}
	if RequiredPermissions(changed)["workflows"] != "write" {
		t.Fatalf("workflow permission missing for %q", changed)
	}
	if !slices.Contains(diffArgs, "-z") {
		t.Fatalf("diff argv missing -z: %q", diffArgs)
	}
}

func TestPreflightIgnoresHostileWorktreeGitConfig(t *testing.T) {
	repo, base, candidate, _, ref := testRepository(t, "payload")
	git(t, repo, "checkout", "--detach", candidate)
	if err := os.WriteFile(filepath.Join(repo, ".gitattributes"), []byte("payload filter=hostile\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "add", ".gitattributes")
	git(t, repo, "commit", "--amend", "--no-edit")
	candidate = git(t, repo, "rev-parse", "HEAD")
	tree := git(t, repo, "rev-parse", "HEAD^{tree}")
	git(t, repo, "update-ref", ref, candidate)

	markers := t.TempDir()
	command := func(name, body string) string {
		path := filepath.Join(markers, name)
		if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch \""+filepath.Join(markers, name+"-ran")+"\"\n"+body), 0o700); err != nil {
			t.Fatal(err)
		}
		return path
	}
	filter := command("filter", "cat\n")
	fsmonitor := command("fsmonitor", "exit 0\n")
	hooks := filepath.Join(markers, "hooks")
	if err := os.Mkdir(hooks, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hooks, "pre-commit"), []byte("#!/bin/sh\ntouch \""+filepath.Join(markers, "hook-ran")+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "filter.hostile.clean", filter)
	git(t, repo, "config", "core.fsmonitor", fsmonitor)
	git(t, repo, "config", "core.hooksPath", hooks)
	git(t, repo, "config", "uploadpack.packObjectsHook", command("pack-objects-hook", "exit 1\n"))
	git(t, repo, "config", "core.pager", command("pager", "exit 1\n"))
	git(t, repo, "config", "credential.helper", "!"+command("credential-helper", "exit 1\n"))
	git(t, repo, "config", "alias.rev-parse", "!"+command("alias", "exit 1\n"))
	if err := os.WriteFile(filepath.Join(repo, "payload"), []byte("different\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver"}
	_, preflightErr := Preflight(context.Background(), Config{LocalRepository: repo, GitExecutable: testExecutable(t)}, input, Options{})
	for _, name := range []string{"filter-ran", "fsmonitor-ran", "hook-ran", "pack-objects-hook-ran", "pager-ran", "credential-helper-ran", "alias-ran"} {
		if _, err := os.Stat(filepath.Join(markers, name)); !os.IsNotExist(err) {
			t.Fatalf("hostile Git config executed %s: %v", name, err)
		}
	}
	if preflightErr != nil {
		t.Fatal(preflightErr)
	}
}

func TestParseChangedPathsRejectsMalformedOutput(t *testing.T) {
	for _, output := range [][]byte{
		nil,
		[]byte("README.md"),
		[]byte("README.md\x00\x00"),
		append(bytes.Repeat([]byte("x"), maxOutput), 0),
	} {
		if paths, err := parseChangedPaths(output); err == nil {
			t.Fatalf("accepted %d-byte output as %q", len(output), paths)
		}
	}
}

func TestParseChangedPathsPreservesExactBytes(t *testing.T) {
	output := []byte(" leading\x00.github/workflows/é\nci.yml\x00")
	want := []string{" leading", ".github/workflows/é\nci.yml"}
	if paths, err := parseChangedPaths(output); err != nil || !slices.Equal(paths, want) {
		t.Fatalf("paths = %q, %v; want %q", paths, err, want)
	}
}

func TestProtectedReadRejectsSymlinksAndStaysOnOpenedDescriptor(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	original := []byte(`{"version":1}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := openProtected(path)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	backup := filepath.Join(dir, "opened")
	malicious := filepath.Join(dir, "replacement")
	if err := os.Rename(path, backup); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(malicious, []byte(`{"token":"synthetic-leak"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(malicious, path); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(file)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("descriptor read = %q, %v", got, err)
	}
	if _, err := readProtectedFile(path, 0o600, maxOutput); err == nil {
		t.Fatal("accepted swapped symlink")
	}
}

func TestLoadConfigAndPEMRejectUnsafeFiles(t *testing.T) {
	repo := t.TempDir()
	if err := os.Chmod(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	key := testKey(t)
	gitExecutable := testExecutable(t)
	t.Setenv("APP_ID", "123")
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "github.json")
	body := fmt.Sprintf(`{"version":1,"api_base":"https://api.github.com","owner":"octo","repository":"repo","local_repository":%q,"git_executable":%q,"github_app_id_env":"APP_ID","github_app_private_key_path":%q}`, repo, gitExecutable, key)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("accepted insecure config mode")
	}
	if err := os.Chmod(path, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o770); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig(path); err == nil {
		t.Fatal("accepted insecure config parent")
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "key.pem")
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	if _, err := readPrivateKey(link); err == nil {
		t.Fatal("accepted symlink PEM")
	}
}

func TestCredentialedPushIsolatedFromHostileGitState(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	gitExecutable := testExecutable(t)
	markerDir := t.TempDir()
	hookMarker := filepath.Join(markerDir, "hook-ran")
	helperStore := filepath.Join(markerDir, "credentials")
	traceFile := filepath.Join(markerDir, "trace")
	hook := filepath.Join(repo, ".git", "hooks", "pre-push")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\ntouch \""+hookMarker+"\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "config", "credential.helper", "store --file="+helperStore)
	git(t, repo, "config", "url.https://attacker.invalid/.insteadOf", "https://github.com/")

	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	global := filepath.Join(home, ".gitconfig")
	globalBody := "[credential]\n\thelper = store --file=" + helperStore + "\n[url \"https://attacker.invalid/\"]\n\tinsteadOf = https://github.com/\n"
	if err := os.WriteFile(global, []byte(globalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	xdg := filepath.Join(home, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "git"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "git", "config"), []byte(globalBody), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv("XDG_CONFIG_HOME", xdg)
	t.Setenv("GIT_CONFIG_GLOBAL", global)
	t.Setenv("GIT_TRACE", traceFile)
	t.Setenv("GIT_ASKPASS", filepath.Join(markerDir, "hostile-askpass"))

	secret := "synthetic-installation-token"
	var pushed, sandboxRoot string
	var sourceCommands int
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if name != gitExecutable {
			t.Fatalf("unpinned executable: %q", name)
		}
		fetch, maintenance := slices.Index(args, "fetch"), slices.Index(args, "maintenance.auto=false")
		if dir == repo {
			sourceCommands++
			argv := "\n" + strings.Join(args, "\n") + "\n"
			for _, required := range []string{"core.hooksPath=/dev/null", "core.fsmonitor=false", "maintenance.auto=false", "credential.helper=", "uploadpack.packObjectsHook=", "--no-pager", "--no-optional-locks"} {
				if !strings.Contains(argv, "\n"+required+"\n") {
					t.Fatalf("source Git command lacks %s: %q", required, args)
				}
			}
			for _, forbidden := range []string{"status", "--is-inside-work-tree", "--show-toplevel"} {
				if strings.Contains(argv, "\n"+forbidden+"\n") {
					t.Fatalf("source Git command inspects worktree: %q", args)
				}
			}
		} else if fetch >= 0 {
			if maintenance <= 0 || maintenance != fetch-1 || args[maintenance-1] != "-c" {
				t.Fatalf("staging fetch can launch maintenance: %q", args)
			}
		}
		if slices.Contains(args, "push") {
			pushed = strings.Join(args, "\n")
			sandboxRoot = filepath.Dir(dir)
			joined := strings.Join(env, "\n")
			allowed := map[string]bool{"HOME": true, "XDG_CONFIG_HOME": true, "GIT_CONFIG_NOSYSTEM": true, "GIT_CONFIG_GLOBAL": true, "GIT_TERMINAL_PROMPT": true, "GIT_TRACE": true, "GIT_TRACE_PACKET": true, "GIT_CURL_VERBOSE": true, "LC_ALL": true, "GIT_ASKPASS": true, "FORGE_GITHUB_TOKEN": true}
			if len(env) != len(allowed) {
				t.Fatalf("environment is not minimal: %q", env)
			}
			for _, value := range env {
				name, _, _ := strings.Cut(value, "=")
				if !allowed[name] {
					t.Fatalf("unexpected environment variable: %q", name)
				}
			}
			for _, inherited := range []string{home, xdg, global, traceFile, "https://attacker.invalid/"} {
				if strings.Contains(joined, inherited) {
					t.Fatalf("inherited hostile environment: %q", inherited)
				}
			}
			if dir == repo || !strings.Contains(joined, "GIT_CONFIG_NOSYSTEM=1") || !strings.Contains(joined, "GIT_CONFIG_GLOBAL=/dev/null") || !strings.Contains(joined, "GIT_TERMINAL_PROMPT=0") || !strings.Contains(pushed, "credential.helper=") || !strings.Contains(pushed, "core.hooksPath=/dev/null") || !strings.Contains(pushed, "http.followRedirects=false") {
				t.Fatalf("unsafe push dir=%q env=%q args=%q", dir, env, args)
			}
			if strings.Contains(pushed, secret) || !strings.Contains(pushed, "https://github.com/octo/repo.git") {
				t.Fatalf("credential or remote in argv: %q", args)
			}
			config, err := os.ReadFile(filepath.Join(dir, "config"))
			if err != nil || bytes.Contains(config, []byte("credential")) || bytes.Contains(config, []byte("attacker")) || bytes.Contains(config, []byte("hooks")) {
				t.Fatalf("source config reached staging: %v %q", err, config)
			}
			refCommand := exec.CommandContext(ctx, name, "-C", dir, "for-each-ref", "--format=%(refname)")
			refCommand.Env = env
			refs, err := refCommand.Output()
			if err != nil || strings.TrimSpace(string(refs)) != ref+"\nrefs/heads/main" {
				t.Fatalf("staging refs = %q, %v", refs, err)
			}
			askpass := envValue(env, "GIT_ASKPASS")
			script, err := os.ReadFile(askpass)
			if err != nil || strings.Contains(string(script), secret) {
				t.Fatalf("askpass leaked token: %v %q", err, script)
			}
			err = filepath.Walk(sandboxRoot, func(path string, info os.FileInfo, walkErr error) error {
				if walkErr != nil || info.IsDir() {
					return walkErr
				}
				body, readErr := os.ReadFile(path)
				if readErr == nil && bytes.Contains(body, []byte(secret)) {
					t.Fatalf("token persisted in %s", path)
				}
				return readErr
			})
			return nil, err
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	cfg := Config{LocalRepository: repo, GitExecutable: gitExecutable, Owner: "octo", Repository: "repo"}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver"}
	state, err := localPreflight(context.Background(), cfg, input, normalizeOptions(Options{Run: run}))
	if err != nil {
		t.Fatal(err)
	}
	if err := push(context.Background(), cfg, input, secret, state.sandbox, normalizeOptions(Options{Run: run})); err != nil {
		t.Fatal(err)
	}
	state.sandbox.remove()
	if pushed == "" || sandboxRoot == "" || sourceCommands == 0 {
		t.Fatalf("source commands=%d push=%t sandbox=%t", sourceCommands, pushed != "", sandboxRoot != "")
	}
	for _, path := range []string{hookMarker, helperStore, traceFile, sandboxRoot} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("hostile side effect or temp remains at %s: %v", path, err)
		}
	}
}

func envValue(env []string, name string) string {
	prefix := name + "="
	for _, value := range env {
		if strings.HasPrefix(value, prefix) {
			return strings.TrimPrefix(value, prefix)
		}
	}
	return ""
}

func TestDeliverPushesExactCandidateAndIsIdempotent(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	var mu sync.Mutex
	requests := map[string]int{}
	token := "installation-token-never-print"
	branchExists := false
	prExists := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests[r.Method+" "+r.URL.Path]++
		mu.Unlock()
		switch {
		case r.Method == "GET" && r.URL.Path == "/repos/octo/repo":
			io.WriteString(w, `{"full_name":"octo/repo","private":false,"owner":{"type":"Organization"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/octo/repo/installation":
			io.WriteString(w, `{"id":7,"permissions":{"contents":"write","pull_requests":"write","workflows":"write"}}`)
		case r.Method == "POST" && r.URL.Path == "/app/installations/7/access_tokens":
			io.WriteString(w, `{"token":"`+token+`","expires_at":"2099-01-01T00:00:00Z"}`)
		case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/repos/octo/repo/git/ref/heads/"):
			if strings.HasSuffix(r.URL.Path, "/main") {
				io.WriteString(w, `{"object":{"sha":"`+base+`"}}`)
				return
			}
			if !branchExists {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			io.WriteString(w, `{"object":{"sha":"`+candidate+`"}}`)
		case r.Method == "GET" && r.URL.Path == "/repos/octo/repo/pulls":
			if prExists {
				io.WriteString(w, `[{"number":42,"html_url":"https://github.com/octo/repo/pull/42","head":{"ref":"forge/job","label":"octo:forge/job"},"base":{"ref":"main"},"title":"Deliver","body":"Reviewed"}]`)
			} else {
				io.WriteString(w, `[]`)
			}
		case r.Method == "POST" && r.URL.Path == "/repos/octo/repo/pulls":
			prExists = true
			io.WriteString(w, `{"number":42,"html_url":"https://github.com/octo/repo/pull/42"}`)
		default:
			t.Errorf("unexpected API request %s %s", r.Method, r.URL.String())
			w.WriteHeader(http.StatusNotFound)
		}
	})
	client := testHTTPClient(handler)

	var pushes int
	var askpassPath string
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "push") {
			pushes++
			joined := strings.Join(append(env, args...), "\n")
			argv := strings.Join(args, "\n")
			if strings.Contains(argv, token) || !strings.Contains(joined, "GIT_ASKPASS=") || !strings.Contains(joined, "FORGE_GITHUB_TOKEN="+token) || !strings.Contains(argv, "--atomic") || !strings.Contains(argv, "--force-with-lease=refs/heads/main:"+base) || !strings.Contains(argv, "--force-with-lease=refs/heads/forge/job:") || !strings.Contains(argv, ref+":refs/heads/forge/job") || !strings.Contains(argv, "refs/heads/main:refs/heads/main") || dir == repo {
				t.Fatalf("unsafe or incorrect push: env=%q args=%q", env, args)
			}
			for _, value := range env {
				if strings.HasPrefix(value, "GIT_ASKPASS=") {
					askpassPath = strings.TrimPrefix(value, "GIT_ASKPASS=")
				}
			}
			branchExists = true
			return nil, nil
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	cfg := Config{Version: 1, APIBase: "https://api.github.com", Owner: "octo", Repository: "repo", LocalRepository: repo, GitExecutable: testExecutable(t), AppID: "123", PrivateKeyPath: testKey(t)}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	options := Options{BaseURL: "http://github.test", HTTPClient: client, Run: run, Timeout: 2 * time.Minute, RetryDelay: func(context.Context, time.Duration) error { return nil }}
	for i := 0; i < 2; i++ {
		result, err := Deliver(context.Background(), cfg, input, options)
		if err != nil {
			t.Fatal(err)
		}
		if result.CandidateSHA != candidate || result.Branch != "forge/job" || result.PRURL != "https://github.com/octo/repo/pull/42" || result.Status != "open" {
			t.Fatalf("result = %#v", result)
		}
	}
	if pushes != 1 || requests["POST /repos/octo/repo/pulls"] != 1 {
		t.Fatalf("pushes=%d requests=%v", pushes, requests)
	}
	if _, err := os.Stat(askpassPath); !os.IsNotExist(err) {
		t.Fatalf("temporary askpass remains: %v", err)
	}
}

func TestPullRequestCreateReconcilesCommittedLostResponse(t *testing.T) {
	var gets, posts int
	exists := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch r.Method {
		case http.MethodGet:
			gets++
			if r.URL.Query().Get("state") != "open" || r.URL.Query().Get("head") != "octo:forge/job" || r.URL.Query().Get("base") != "main" {
				t.Fatalf("imprecise reconciliation query: %s", r.URL.RawQuery)
			}
			if exists {
				return testResponse(r, http.StatusOK, `[{
                    "number":42,"html_url":"https://github.com/octo/repo/pull/42",
                    "head":{"ref":"forge/job","label":"octo:forge/job"},"base":{"ref":"main"},
                    "title":"Deliver","body":"Reviewed"}]`), nil
			}
			return testResponse(r, http.StatusOK, `[]`), nil
		case http.MethodPost:
			posts++
			exists = true
			return nil, io.ErrUnexpectedEOF
		default:
			t.Fatalf("unexpected request %s", r.Method)
			return nil, errors.New("unexpected request")
		}
	})}
	api := newAPI(Config{Owner: "octo", Repository: "repo"}, normalizeOptions(Options{BaseURL: "http://github.test", HTTPClient: client, RetryDelay: func(context.Context, time.Duration) error { return nil }}), nil)
	input := Publication{BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	url, err := api.reconcilePR(context.Background(), "token", input, func() (string, error) { return "", errors.New("unexpected refresh") })
	if err != nil || url != "https://github.com/octo/repo/pull/42" || posts != 1 || gets != 2 {
		t.Fatalf("url=%q err=%v posts=%d gets=%d", url, err, posts, gets)
	}
}

func TestPullRequestCreateDoesNotBlindRetryRetryableResponse(t *testing.T) {
	var gets, posts int
	exists := false
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			posts++
			exists = true
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		gets++
		if exists {
			io.WriteString(w, `[{"number":42,"html_url":"https://github.com/octo/repo/pull/42","head":{"ref":"forge/job","label":"octo:forge/job"},"base":{"ref":"main"},"title":"Deliver","body":"Reviewed"}]`)
			return
		}
		io.WriteString(w, `[]`)
	})
	api := newAPI(Config{Owner: "octo", Repository: "repo"}, normalizeOptions(Options{BaseURL: "http://github.test", HTTPClient: testHTTPClient(handler), RetryDelay: func(context.Context, time.Duration) error { return nil }}), nil)
	input := Publication{BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	url, err := api.reconcilePR(context.Background(), "token", input, func() (string, error) { return "", errors.New("unexpected refresh") })
	if err != nil || url == "" || posts != 1 || gets != 2 {
		t.Fatalf("url=%q err=%v posts=%d gets=%d", url, err, posts, gets)
	}
}

func TestPullRequestCreate401RefreshesThenReconciles(t *testing.T) {
	var gets, posts, refreshes int
	exists := false
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method == http.MethodPost {
			posts++
			exists = true
			return testResponse(r, http.StatusUnauthorized, ``), nil
		}
		gets++
		if exists {
			if r.Header.Get("Authorization") != "Bearer fresh-token" {
				t.Fatalf("reconciliation used stale credential: %q", r.Header.Get("Authorization"))
			}
			return testResponse(r, http.StatusOK, `[{"number":42,"html_url":"https://github.com/octo/repo/pull/42","head":{"ref":"forge/job","label":"octo:forge/job"},"base":{"ref":"main"},"title":"Deliver","body":"Reviewed"}]`), nil
		}
		return testResponse(r, http.StatusOK, `[]`), nil
	})}
	api := newAPI(Config{Owner: "octo", Repository: "repo"}, normalizeOptions(Options{BaseURL: "http://github.test", HTTPClient: client}), nil)
	input := Publication{BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	url, err := api.reconcilePR(context.Background(), "expired-token", input, func() (string, error) {
		refreshes++
		return "fresh-token", nil
	})
	if err != nil || url == "" || posts != 1 || gets != 2 || refreshes != 1 {
		t.Fatalf("url=%q err=%v posts=%d gets=%d refreshes=%d", url, err, posts, gets, refreshes)
	}
}

func TestAtomicPushRejectsBaseDriftWithoutCreatingBranch(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	gitExecutable := testExecutable(t)
	remote := t.TempDir()
	git(t, remote, "init", "--bare", "--quiet")
	git(t, repo, "push", "--quiet", remote, "refs/heads/main:refs/heads/main")
	cfg := Config{LocalRepository: repo, GitExecutable: gitExecutable, Owner: "octo", Repository: "repo"}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver"}
	state, err := localPreflight(context.Background(), cfg, input, normalizeOptions(Options{Timeout: 2 * time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	defer state.sandbox.remove()
	git(t, repo, "push", "--quiet", remote, ref+":refs/heads/main")
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		for i := range args {
			if args[i] == "https://github.com/octo/repo.git" {
				args[i] = remote
			}
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	err = push(context.Background(), cfg, input, "token", state.sandbox, normalizeOptions(Options{Run: run}))
	var deliveryErr *Error
	if !AsError(err, &deliveryErr) || deliveryErr.Code != "base_drift" {
		t.Fatalf("push error = %#v", err)
	}
	if got := git(t, remote, "rev-parse", "refs/heads/main"); got != candidate {
		t.Fatalf("remote base = %s, want drifted %s", got, candidate)
	}
	cmd := testGitCommand(t, context.Background(), remote, "show-ref", "--verify", "--quiet", "refs/heads/forge/job")
	if err := cmd.Run(); err == nil {
		t.Fatal("atomic rejection created candidate branch")
	}
}

func TestAtomicPushRejectsConcurrentBranchWithoutChangingRefs(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	gitExecutable := testExecutable(t)
	remote := t.TempDir()
	git(t, remote, "init", "--bare", "--quiet")
	git(t, repo, "push", "--quiet", remote, "refs/heads/main:refs/heads/main")
	cfg := Config{LocalRepository: repo, GitExecutable: gitExecutable, Owner: "octo", Repository: "repo"}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver"}
	state, err := localPreflight(context.Background(), cfg, input, normalizeOptions(Options{Timeout: 2 * time.Minute}))
	if err != nil {
		t.Fatal(err)
	}
	defer state.sandbox.remove()
	var pushArgs []string
	run := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		pushArgs = append([]string(nil), args...)
		git(t, repo, "push", "--quiet", remote, "refs/heads/main:refs/heads/forge/job")
		for i := range args {
			if args[i] == "https://github.com/octo/repo.git" {
				args[i] = remote
			}
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	err = push(context.Background(), cfg, input, "token", state.sandbox, normalizeOptions(Options{Run: run}))
	var deliveryErr *Error
	if !AsError(err, &deliveryErr) || deliveryErr.Code != "push_conflict" {
		t.Fatalf("push error = %#v", err)
	}
	if !slices.Contains(pushArgs, "--atomic") || !slices.Contains(pushArgs, "--force-with-lease=refs/heads/main:"+base) || !slices.Contains(pushArgs, "--force-with-lease=refs/heads/forge/job:") {
		t.Fatalf("push argv missing atomic leases: %q", pushArgs)
	}
	if got := git(t, remote, "rev-parse", "refs/heads/main"); got != base {
		t.Fatalf("remote base = %s, want %s", got, base)
	}
	if got := git(t, remote, "rev-parse", "refs/heads/forge/job"); got != base {
		t.Fatalf("remote branch = %s, want raced base %s", got, base)
	}
}

func TestDeliverRefreshesExpiredCredentialOnceAndBoundsTransientRetries(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, ".github/workflows/ci.yml")
	var mints, refs int
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/repos/octo/repo":
			io.WriteString(w, `{"full_name":"octo/repo","private":false,"owner":{"type":"Organization"}}`)
		case r.URL.Path == "/repos/octo/repo/installation":
			io.WriteString(w, `{"id":7,"permissions":{"contents":"write","pull_requests":"write","workflows":"write"}}`)
		case r.URL.Path == "/app/installations/7/access_tokens":
			mints++
			io.WriteString(w, fmt.Sprintf(`{"token":"token-%d","expires_at":"2099-01-01T00:00:00Z"}`, mints))
		case strings.Contains(r.URL.Path, "/git/ref/heads/"):
			refs++
			if refs == 1 {
				w.WriteHeader(http.StatusUnauthorized)
			} else if refs < 4 {
				w.Header().Set("Retry-After", "1")
				w.WriteHeader(http.StatusServiceUnavailable)
			} else if refs == 4 {
				io.WriteString(w, `{"object":{"sha":"`+base+`"}}`)
			} else {
				w.WriteHeader(http.StatusNotFound)
			}
		default:
			io.WriteString(w, `[{"number":1,"html_url":"https://github.com/octo/repo/pull/1","head":{"ref":"forge/job","label":"octo:forge/job"},"base":{"ref":"main"},"title":"Deliver","body":"Reviewed"}]`)
		}
	})
	client := testHTTPClient(handler)
	push := func(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
		if slices.Contains(args, "push") {
			return nil, nil
		}
		cmd := exec.CommandContext(ctx, name, args...)
		cmd.Dir, cmd.Env = dir, env
		return cmd.CombinedOutput()
	}
	cfg := Config{Version: 1, APIBase: "https://api.github.com", Owner: "octo", Repository: "repo", LocalRepository: repo, GitExecutable: testExecutable(t), AppID: "123", PrivateKeyPath: testKey(t)}
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	_, err := Deliver(context.Background(), cfg, input, Options{BaseURL: "http://github.test", HTTPClient: client, Run: push, Timeout: 2 * time.Minute, RetryDelay: func(context.Context, time.Duration) error { return nil }})
	if err != nil || mints != 2 || refs != 5 {
		t.Fatalf("err=%v mints=%d refs=%d", err, mints, refs)
	}
}

func TestTypedAccessFailuresAreValueSafe(t *testing.T) {
	type response struct {
		status int
		body   string
	}
	for _, tt := range []struct {
		name      string
		code      string
		requests  []string
		responses []response
	}{
		{"repository missing", "repository_not_found", []string{"GET /repos/octo/repo"}, []response{{404, ""}}},
		{"installation absent", "installation_absent", []string{"GET /repos/octo/repo", "GET /repos/octo/repo/installation", "GET /users/octo/installation"}, []response{{200, `{"full_name":"octo/repo","private":false,"owner":{"type":"User"}}`}, {404, ""}, {404, ""}}},
		{"repository not selected", "repository_not_selected", []string{"GET /repos/octo/repo", "GET /repos/octo/repo/installation", "GET /orgs/octo/installation"}, []response{{200, `{"full_name":"octo/repo","private":false,"owner":{"type":"Organization"}}`}, {404, ""}, {200, `{}`}}},
		{"permission missing", "permission_missing", []string{"GET /repos/octo/repo", "GET /repos/octo/repo/installation"}, []response{{200, `{"full_name":"octo/repo","private":false,"owner":{"type":"Organization"}}`}, {200, `{"id":7,"permissions":{"contents":"read","pull_requests":"write"}}`}}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			repo, base, candidate, tree, ref := testRepository(t, "README.md")
			var requests []string
			client := testHTTPClient(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests = append(requests, r.Method+" "+r.URL.Path)
				if len(requests) > len(tt.responses) {
					t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
				}
				if len(requests) == 1 && r.Header.Get("Authorization") != "" {
					t.Fatal("public repository lookup used installation credentials")
				}
				response := tt.responses[len(requests)-1]
				w.WriteHeader(response.status)
				io.WriteString(w, response.body)
			}))
			cfg := Config{Version: 1, APIBase: "https://api.github.com", Owner: "octo", Repository: "repo", LocalRepository: repo, GitExecutable: testExecutable(t), AppID: "123", PrivateKeyPath: testKey(t)}
			input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
			_, err := Deliver(context.Background(), cfg, input, Options{BaseURL: "http://github.test", HTTPClient: client, Timeout: 2 * time.Minute})
			var deliveryErr *Error
			if !AsError(err, &deliveryErr) || deliveryErr.Code != tt.code || strings.Contains(err.Error(), repo) {
				t.Fatalf("error = %#v", err)
			}
			if !slices.Equal(requests, tt.requests) {
				t.Fatalf("requests = %q, want %q", requests, tt.requests)
			}
		})
	}
}

func TestLocalPreflightRejectsMergeWithExpectedFirstParent(t *testing.T) {
	repo, base, _, _, ref := testRepository(t, "README.md")
	git(t, repo, "checkout", "-b", "side", base)
	os.WriteFile(filepath.Join(repo, "side.txt"), []byte("side\n"), 0o600)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "side")
	side := git(t, repo, "rev-parse", "HEAD")
	git(t, repo, "checkout", "main")
	git(t, repo, "merge", "--no-ff", "--no-edit", side)
	merge := git(t, repo, "rev-parse", "HEAD")
	tree := git(t, repo, "rev-parse", "HEAD^{tree}")
	git(t, repo, "update-ref", ref, merge)
	git(t, repo, "reset", "--hard", base)
	input := Publication{Version: 1, CandidateSHA: merge, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	_, err := Preflight(context.Background(), Config{LocalRepository: repo, GitExecutable: testExecutable(t)}, input, Options{})
	var deliveryErr *Error
	if !AsError(err, &deliveryErr) || deliveryErr.Code != "candidate_parent_mismatch" {
		t.Fatalf("merge preflight error = %#v", err)
	}
}

func TestLocalPreflightIgnoresDirtyWorktreeAndRejectsIdentityMismatches(t *testing.T) {
	repo, base, candidate, tree, ref := testRepository(t, "README.md")
	input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
	if err := os.WriteFile(filepath.Join(repo, "dirty"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Preflight(context.Background(), Config{LocalRepository: repo, GitExecutable: testExecutable(t)}, input, Options{}); err != nil {
		t.Fatalf("immutable candidate rejected for dirty worktree: %v", err)
	}

	for _, tt := range []struct {
		mutation string
		code     string
	}{
		{"base", "base_drift"},
		{"ref", "candidate_ref_mismatch"},
		{"tree", "candidate_tree_mismatch"},
		{"branch", "publication_invalid"},
	} {
		t.Run(tt.mutation, func(t *testing.T) {
			repo, base, candidate, tree, ref := testRepository(t, "README.md")
			input := Publication{Version: 1, CandidateSHA: candidate, ExpectedParentSHA: base, ExpectedTreeSHA: tree, CandidateRef: ref, BaseBranch: "main", NewBranch: "forge/job", PRTitle: "Deliver", PRBody: "Reviewed"}
			switch tt.mutation {
			case "base":
				git(t, repo, "update-ref", "refs/heads/main", candidate)
			case "ref":
				input.CandidateRef = "refs/agent-forge/candidates/" + strings.Repeat("f", 32) + "/" + strings.Repeat("e", 32)
			case "tree":
				input.ExpectedTreeSHA = strings.Repeat("a", 40)
			case "branch":
				input.NewBranch = "../unsafe"
			}
			_, err := Preflight(context.Background(), Config{LocalRepository: repo, GitExecutable: testExecutable(t)}, input, Options{})
			var deliveryErr *Error
			if !AsError(err, &deliveryErr) || deliveryErr.Code != tt.code {
				t.Fatalf("preflight error = %#v, want %s", err, tt.code)
			}
		})
	}
}

func TestNoTokenLeakageInResultOrErrors(t *testing.T) {
	secret := "token-never-leak"
	err := &Error{Code: "credential_expired"}
	result := Result{Version: 1, CandidateSHA: strings.Repeat("a", 40), Branch: "forge/job", PRURL: "https://github.com/octo/repo/pull/1", Status: "open"}
	body, marshalErr := json.Marshal(result)
	configBody, configErr := json.Marshal(Config{AppID: secret})
	if marshalErr != nil || configErr != nil || strings.Contains(string(body), secret) || strings.Contains(string(configBody), secret) || strings.Contains(err.Error(), secret) {
		t.Fatalf("leak: body=%s err=%v", body, err)
	}
}

func TestPushFailureRedactsCredential(t *testing.T) {
	secret := "synthetic-token-in-command-output"
	sandbox, err := newGitSandbox()
	if err != nil {
		t.Fatal(err)
	}
	defer sandbox.remove()
	op := normalizeOptions(Options{Run: func(context.Context, string, []string, string, ...string) ([]byte, error) {
		return []byte(secret), errors.New(secret)
	}})
	err = push(context.Background(), Config{GitExecutable: testExecutable(t), Owner: "octo", Repository: "repo"}, Publication{CandidateRef: "refs/agent-forge/candidates/" + strings.Repeat("1", 32) + "/" + strings.Repeat("2", 32), NewBranch: "forge/job"}, secret, sandbox, op)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("push error = %v", err)
	}
}

func testRepository(t *testing.T, changedPath string) (repo, base, candidate, tree, ref string) {
	t.Helper()
	repo = t.TempDir()
	if err := os.Chmod(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-b", "main")
	git(t, repo, "config", "user.name", "Test")
	git(t, repo, "config", "user.email", "test@example.invalid")
	os.WriteFile(filepath.Join(repo, "base.txt"), []byte("base\n"), 0o600)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "base")
	base = git(t, repo, "rev-parse", "HEAD")
	path := filepath.Join(repo, filepath.FromSlash(changedPath))
	os.MkdirAll(filepath.Dir(path), 0o700)
	os.WriteFile(path, []byte("candidate\n"), 0o600)
	git(t, repo, "add", ".")
	git(t, repo, "commit", "-m", "candidate")
	candidate = git(t, repo, "rev-parse", "HEAD")
	tree = git(t, repo, "rev-parse", "HEAD^{tree}")
	ref = "refs/agent-forge/candidates/" + strings.Repeat("1", 32) + "/" + strings.Repeat("2", 32)
	git(t, repo, "update-ref", ref, candidate)
	git(t, repo, "reset", "--hard", base)
	return
}

func testKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "app.pem")
	body := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := testGitCommand(t, context.Background(), dir, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s", args, err, stderr.String())
	}
	return strings.TrimSpace(string(out))
}

func testGitPath(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Fatal(err)
	}
	path, err = filepath.EvalSymlinks(path)
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func testGitCommand(t *testing.T, ctx context.Context, dir string, args ...string) *exec.Cmd {
	t.Helper()
	root := t.TempDir()
	home, xdg := filepath.Join(root, "home"), filepath.Join(root, "xdg")
	if err := os.Mkdir(home, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(xdg, 0o700); err != nil {
		t.Fatal(err)
	}
	cmd := exec.CommandContext(ctx, testGitPath(t), args...)
	cmd.Dir = dir
	cmd.Env = []string{"HOME=" + home, "XDG_CONFIG_HOME=" + xdg, "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	return cmd
}

type handlerTransport struct{ handler http.Handler }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) { return f(request) }

func testResponse(request *http.Request, status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: request}
}

func (t handlerTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	recorder := &responseRecorder{header: make(http.Header)}
	t.handler.ServeHTTP(recorder, request)
	if recorder.status == 0 {
		recorder.status = http.StatusOK
	}
	return &http.Response{StatusCode: recorder.status, Header: recorder.header, Body: io.NopCloser(&recorder.body), Request: request}, nil
}

type responseRecorder struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (r *responseRecorder) Header() http.Header    { return r.header }
func (r *responseRecorder) WriteHeader(status int) { r.status = status }
func (r *responseRecorder) Write(body []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.body.Write(body)
}

func testHTTPClient(handler http.Handler) *http.Client {
	return &http.Client{Transport: handlerTransport{handler: handler}}
}
