package worker

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"agent-forge/internal/configjson"
)

func validWorkerConfig(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	repositoryRoot := filepath.Join(root, "repositories")
	repository := filepath.Join(repositoryRoot, "repository")
	worktrees := filepath.Join(root, "worktrees")
	runtime := filepath.Join(root, "runtime")
	for _, path := range []string{repositoryRoot, repository, worktrees, runtime} {
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return `{"version":1,"gate_url":"ws://127.0.0.1:8080","id":"worker-1","token_env":"FORGE_WORKER_TOKEN","heartbeat_interval":"5s","concurrency":2,` +
		`"repository_roots":[` + quote(repositoryRoot) + `],"worktree_root":` + quote(worktrees) + `,"runtime_root":` + quote(runtime) + `,` +
		`"repositories":[{"id":"agent-forge","path":` + quote(repository) + `}],"plugins":[{"id":"codex","argv":["/bin/echo","ok"]}],` +
		`"environment_allowlist":["PATH","CODEX_HOME","CODEX_BIN"],"check_environment_allowlist":["PATH"],"ceilings":{"plugin_timeout":"20m","check_timeout":"10m","git_timeout":"2m","cleanup_timeout":"20s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}`
}

func TestLoadConfigRejectsOversizedFileWithoutLeakingValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private-config-path")
	private := "private-config-content"
	if err := os.WriteFile(path, []byte(strings.Repeat(private, configjson.MaxBytes/len(private)+2)), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil {
		t.Fatal("accepted oversized config")
	}
	for _, secret := range []string{path, private} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("error leaked config data: %q", err)
		}
	}
}

func quote(value string) string { return `"` + strings.ReplaceAll(value, `\`, `\\`) + `"` }

func rewriteWorkerConfig(t *testing.T, body string, rewrite func(*rawWorkerConfig)) string {
	t.Helper()
	var raw rawWorkerConfig
	if err := json.Unmarshal([]byte(body), &raw); err != nil {
		t.Fatal(err)
	}
	rewrite(&raw)
	data, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestParseWorkerConfigStrictAndCanonical(t *testing.T) {
	c, err := ParseConfig([]byte(validWorkerConfig(t)), func(name string) string {
		if name == "FORGE_WORKER_TOKEN" {
			return "worker-secret"
		}
		return ""
	})
	if err != nil {
		t.Fatal(err)
	}
	if c.Version != 1 || c.HeartbeatInterval != 5*time.Second || c.Concurrency != 2 || c.token != "worker-secret" {
		t.Fatalf("config = %#v", c)
	}
	if len(c.CheckEnvironmentAllowlist) != 1 || c.CheckEnvironmentAllowlist[0] != "PATH" {
		t.Fatalf("check environment allowlist = %#v", c.CheckEnvironmentAllowlist)
	}
	if len(c.Repositories) != 1 || !filepath.IsAbs(c.Repositories[0].Path) {
		t.Fatalf("repositories = %#v", c.Repositories)
	}
}

func TestParseWorkerConfigValidatesGateURL(t *testing.T) {
	valid := validWorkerConfig(t)
	for _, gateURL := range []string{"http://gate", "ftp://gate", "ws://user@gate", "ws://gate?secret=x", "ws://gate#fragment", "ws://"} {
		t.Run(gateURL, func(t *testing.T) {
			body := rewriteWorkerConfig(t, valid, func(raw *rawWorkerConfig) { raw.GateURL = gateURL })
			if _, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" }); err == nil {
				t.Fatal("accepted invalid gate URL")
			}
		})
	}
}

func TestParseWorkerConfigCanonicalizesExecutablePlugin(t *testing.T) {
	valid := validWorkerConfig(t)
	dir := t.TempDir()
	link := filepath.Join(dir, "plugin")
	if err := os.Symlink("/bin/echo", link); err != nil {
		t.Fatal(err)
	}
	body := rewriteWorkerConfig(t, valid, func(raw *rawWorkerConfig) { raw.Plugins[0].Argv[0] = link })
	c, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" })
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks("/bin/echo")
	if c.Plugins[0].Argv[0] != want {
		t.Fatalf("plugin executable = %q, want %q", c.Plugins[0].Argv[0], want)
	}

	nonExecutable := filepath.Join(dir, "not-executable")
	if err := os.WriteFile(nonExecutable, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(dir, "missing"), dir, nonExecutable} {
		body := rewriteWorkerConfig(t, valid, func(raw *rawWorkerConfig) { raw.Plugins[0].Argv[0] = path })
		if _, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" }); err == nil {
			t.Fatalf("accepted unsafe plugin executable %q", path)
		}
	}
}

func TestParseWorkerConfigRejectsInvalidPluginArguments(t *testing.T) {
	valid := validWorkerConfig(t)
	body := rewriteWorkerConfig(t, valid, func(raw *rawWorkerConfig) { raw.Plugins[0].Argv[1] = "bad\x00argument" })
	invalidUTF8 := bytes.Replace([]byte(valid), []byte(`"ok"`), []byte{'"', 'b', 'a', 'd', 0xff, '"'}, 1)
	for name, data := range map[string][]byte{"NUL": []byte(body), "invalid UTF-8": invalidUTF8} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseConfig(data, func(string) string { return "worker-secret" }); err == nil || strings.Contains(err.Error(), "bad") {
				t.Fatalf("ParseConfig error = %v", err)
			}
		})
	}
}

func TestParseWorkerConfigRejectsRootOverlaps(t *testing.T) {
	valid := validWorkerConfig(t)
	var base rawWorkerConfig
	if err := json.Unmarshal([]byte(valid), &base); err != nil {
		t.Fatal(err)
	}
	insideRepositoryRoot := filepath.Join(base.RepositoryRoots[0], "unsafe")
	if err := os.Mkdir(insideRepositoryRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	for name, rewrite := range map[string]func(*rawWorkerConfig){
		"worktree equals runtime":     func(raw *rawWorkerConfig) { raw.RuntimeRoot = raw.WorktreeRoot },
		"worktree in repository root": func(raw *rawWorkerConfig) { raw.WorktreeRoot = insideRepositoryRoot },
		"runtime contains repository": func(raw *rawWorkerConfig) { raw.RuntimeRoot = filepath.Dir(raw.RepositoryRoots[0]) },
	} {
		t.Run(name, func(t *testing.T) {
			body := rewriteWorkerConfig(t, valid, rewrite)
			if _, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" }); err == nil {
				t.Fatal("accepted overlapping roots")
			}
		})
	}
}

func TestParseWorkerConfigRejectsUnsafeOrAmbiguousDocuments(t *testing.T) {
	valid := validWorkerConfig(t)
	tests := map[string]string{
		"unknown":          strings.Replace(valid, `"version":1`, `"version":1,"unknown":"private-value"`, 1),
		"trailing":         valid + `{}`,
		"duplicate key":    strings.Replace(valid, `"version":1`, `"version":1,"version":1`, 1),
		"duplicate repo":   strings.Replace(valid, `"repositories":[`, `"repositories":[{"id":"agent-forge","path":"/private/path"},`, 1),
		"duplicate plugin": strings.Replace(valid, `"plugins":[`, `"plugins":[{"id":"codex","argv":["/bin/true"]},`, 1),
		"duplicate env":    strings.Replace(valid, `"PATH","CODEX_HOME","CODEX_BIN"`, `"PATH","PATH","CODEX_BIN"`, 1),
		"zero concurrency": strings.Replace(valid, `"concurrency":2`, `"concurrency":0`, 1),
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			_, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" })
			if err == nil {
				t.Fatal("accepted invalid config")
			}
			for _, private := range []string{"private-value", "/private/path", "worker-secret"} {
				if strings.Contains(err.Error(), private) {
					t.Fatalf("error leaked value: %q", err)
				}
			}
		})
	}
}

func TestParseWorkerConfigRejectsUnsafeCheckEnvironmentAllowlist(t *testing.T) {
	valid := validWorkerConfig(t)
	for name, rewrite := range map[string]func(*rawWorkerConfig){
		"invalid name": func(raw *rawWorkerConfig) { raw.CheckEnvironmentAllowlist = []string{"lowercase"} },
		"duplicate":    func(raw *rawWorkerConfig) { raw.CheckEnvironmentAllowlist = []string{"PATH", "PATH"} },
		"not subset":   func(raw *rawWorkerConfig) { raw.CheckEnvironmentAllowlist = []string{"UNLISTED"} },
		"token":        func(raw *rawWorkerConfig) { raw.CheckEnvironmentAllowlist = []string{raw.TokenEnv} },
		"over limit": func(raw *rawWorkerConfig) {
			raw.CheckEnvironmentAllowlist = make([]string, 65)
			for i := range raw.CheckEnvironmentAllowlist {
				raw.CheckEnvironmentAllowlist[i] = "CHECK_" + strconv.Itoa(i)
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			body := rewriteWorkerConfig(t, valid, rewrite)
			if _, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" }); err == nil {
				t.Fatal("accepted unsafe check environment allowlist")
			}
		})
	}
}

func TestParseWorkerConfigAllowsAbsentCheckEnvironmentAllowlist(t *testing.T) {
	body := strings.Replace(validWorkerConfig(t), `,"check_environment_allowlist":["PATH"]`, "", 1)
	config, err := ParseConfig([]byte(body), func(string) string { return "worker-secret" })
	if err != nil || len(config.CheckEnvironmentAllowlist) != 0 {
		t.Fatalf("check environment allowlist = %#v, %v", config.CheckEnvironmentAllowlist, err)
	}
}
