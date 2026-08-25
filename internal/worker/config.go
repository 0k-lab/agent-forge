package worker

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"agent-forge/internal/configjson"
)

func lookupEnv(name string) (string, bool) { return os.LookupEnv(name) }

var (
	workerConfigID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	workerEnvName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	errWorkerConfig = errors.New("invalid config: validation")
)

type RepositoryRegistration struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

type PluginRegistration struct {
	ID   string   `json:"id"`
	Argv []string `json:"argv"`
}

type LocalCeilings struct {
	PluginTimeout     time.Duration
	CheckTimeout      time.Duration
	GitTimeout        time.Duration
	CleanupTimeout    time.Duration
	PluginOutputBytes int64
	CheckOutputBytes  int64
	GitOutputBytes    int64
}

type Config struct {
	Version                   int
	GateURL                   string
	ID                        string
	TokenEnv                  string
	HeartbeatInterval         time.Duration
	Concurrency               int
	RepositoryRoots           []string
	WorktreeRoot              string
	RuntimeRoot               string
	Repositories              []RepositoryRegistration
	Plugins                   []PluginRegistration
	EnvironmentAllowlist      []string
	CheckEnvironmentAllowlist []string
	Ceilings                  LocalCeilings
	token                     string
}

type rawCeilings struct {
	PluginTimeout     string `json:"plugin_timeout"`
	CheckTimeout      string `json:"check_timeout"`
	GitTimeout        string `json:"git_timeout"`
	CleanupTimeout    string `json:"cleanup_timeout"`
	PluginOutputBytes int64  `json:"plugin_output_bytes"`
	CheckOutputBytes  int64  `json:"check_output_bytes"`
	GitOutputBytes    int64  `json:"git_output_bytes"`
}

type rawWorkerConfig struct {
	Version                   int                      `json:"version"`
	GateURL                   string                   `json:"gate_url"`
	ID                        string                   `json:"id"`
	TokenEnv                  string                   `json:"token_env"`
	HeartbeatInterval         string                   `json:"heartbeat_interval"`
	Concurrency               int                      `json:"concurrency"`
	RepositoryRoots           []string                 `json:"repository_roots"`
	WorktreeRoot              string                   `json:"worktree_root"`
	RuntimeRoot               string                   `json:"runtime_root"`
	Repositories              []RepositoryRegistration `json:"repositories"`
	Plugins                   []PluginRegistration     `json:"plugins"`
	EnvironmentAllowlist      []string                 `json:"environment_allowlist"`
	CheckEnvironmentAllowlist []string                 `json:"check_environment_allowlist"`
	Ceilings                  rawCeilings              `json:"ceilings"`
}

func LoadConfig(path string) (Config, error) {
	data, err := configjson.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data, os.Getenv)
}

func ParseConfig(data []byte, getenv func(string) string) (Config, error) {
	var raw rawWorkerConfig
	if err := configjson.Decode(data, &raw); err != nil {
		return Config{}, err
	}
	heartbeat, err := time.ParseDuration(raw.HeartbeatInterval)
	gateURL, urlErr := url.Parse(raw.GateURL)
	if err != nil || heartbeat <= 0 || heartbeat > time.Hour || raw.Version != 1 || raw.GateURL == "" || len(raw.GateURL) > 2048 || urlErr != nil || gateURL.Host == "" || gateURL.User != nil || gateURL.RawQuery != "" || gateURL.ForceQuery || gateURL.Fragment != "" || gateURL.Scheme != "ws" && gateURL.Scheme != "wss" || !workerConfigID.MatchString(raw.ID) || !workerEnvName.MatchString(raw.TokenEnv) || raw.Concurrency < 1 || raw.Concurrency > 64 || len(raw.RepositoryRoots) == 0 || len(raw.RepositoryRoots) > 64 || len(raw.Repositories) > 1024 || len(raw.Plugins) == 0 || len(raw.Plugins) > 128 || len(raw.EnvironmentAllowlist) > 64 || len(raw.CheckEnvironmentAllowlist) > 64 || getenv == nil {
		return Config{}, errWorkerConfig
	}
	c := Config{Version: raw.Version, GateURL: raw.GateURL, ID: raw.ID, TokenEnv: raw.TokenEnv, HeartbeatInterval: heartbeat, Concurrency: raw.Concurrency, WorktreeRoot: raw.WorktreeRoot, RuntimeRoot: raw.RuntimeRoot, Repositories: raw.Repositories, Plugins: raw.Plugins, EnvironmentAllowlist: raw.EnvironmentAllowlist, CheckEnvironmentAllowlist: raw.CheckEnvironmentAllowlist}
	c.token = getenv(c.TokenEnv)
	if c.token == "" {
		return Config{}, errWorkerConfig
	}
	seenPaths := map[string]bool{}
	for _, root := range raw.RepositoryRoots {
		canonical, err := canonicalDirectory(root)
		if err != nil || seenPaths[canonical] {
			return Config{}, errWorkerConfig
		}
		seenPaths[canonical] = true
		c.RepositoryRoots = append(c.RepositoryRoots, canonical)
	}
	if c.WorktreeRoot, err = canonicalDirectory(raw.WorktreeRoot); err != nil {
		return Config{}, errWorkerConfig
	}
	if c.RuntimeRoot, err = canonicalDirectory(raw.RuntimeRoot); err != nil || c.RuntimeRoot == c.WorktreeRoot {
		return Config{}, errWorkerConfig
	}
	if pathsOverlap(c.WorktreeRoot, c.RuntimeRoot) {
		return Config{}, errWorkerConfig
	}
	for _, root := range c.RepositoryRoots {
		if pathsOverlap(c.WorktreeRoot, root) || pathsOverlap(c.RuntimeRoot, root) {
			return Config{}, errWorkerConfig
		}
	}
	repositoryIDs := map[string]bool{}
	for i := range c.Repositories {
		repository := &c.Repositories[i]
		canonical, err := allowedRepository(repository.Path, c.RepositoryRoots)
		if err != nil || !workerConfigID.MatchString(repository.ID) || repositoryIDs[repository.ID] {
			return Config{}, errWorkerConfig
		}
		repositoryIDs[repository.ID], repository.Path = true, canonical
		if pathsOverlap(c.WorktreeRoot, canonical) || pathsOverlap(c.RuntimeRoot, canonical) {
			return Config{}, errWorkerConfig
		}
	}
	pluginIDs := map[string]bool{}
	for i := range c.Plugins {
		plugin := &c.Plugins[i]
		if !workerConfigID.MatchString(plugin.ID) || pluginIDs[plugin.ID] || len(plugin.Argv) == 0 || len(plugin.Argv) > 64 || !filepath.IsAbs(plugin.Argv[0]) {
			return Config{}, errWorkerConfig
		}
		pluginIDs[plugin.ID] = true
		if plugin.Argv[0], err = canonicalExecutable(plugin.Argv[0]); err != nil {
			return Config{}, errWorkerConfig
		}
		for _, arg := range plugin.Argv {
			if arg == "" || len(arg) > 4096 {
				return Config{}, errWorkerConfig
			}
		}
	}
	environment := map[string]bool{}
	for _, name := range c.EnvironmentAllowlist {
		if !workerEnvName.MatchString(name) || name == c.TokenEnv || environment[name] {
			return Config{}, errWorkerConfig
		}
		environment[name] = true
	}
	checks := map[string]bool{}
	for _, name := range c.CheckEnvironmentAllowlist {
		if !workerEnvName.MatchString(name) || !environment[name] || checks[name] {
			return Config{}, errWorkerConfig
		}
		checks[name] = true
	}
	if c.Ceilings, err = parseCeilings(raw.Ceilings); err != nil {
		return Config{}, errWorkerConfig
	}
	return c, nil
}

func canonicalExecutable(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errWorkerConfig
	}
	return filepath.Clean(resolved), nil
}

func pathsOverlap(a, b string) bool {
	contains := func(parent, child string) bool {
		rel, err := filepath.Rel(parent, child)
		return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	}
	return contains(a, b) || contains(b, a)
}

func parseCeilings(raw rawCeilings) (LocalCeilings, error) {
	c := LocalCeilings{PluginOutputBytes: raw.PluginOutputBytes, CheckOutputBytes: raw.CheckOutputBytes, GitOutputBytes: raw.GitOutputBytes}
	var err error
	for _, item := range []struct {
		value  string
		target *time.Duration
	}{{raw.PluginTimeout, &c.PluginTimeout}, {raw.CheckTimeout, &c.CheckTimeout}, {raw.GitTimeout, &c.GitTimeout}, {raw.CleanupTimeout, &c.CleanupTimeout}} {
		*item.target, err = time.ParseDuration(item.value)
		if err != nil || *item.target <= 0 || *item.target > 24*time.Hour {
			return LocalCeilings{}, errWorkerConfig
		}
	}
	if c.PluginOutputBytes < 1 || c.PluginOutputBytes > 64<<20 || c.CheckOutputBytes < 1 || c.CheckOutputBytes > 1<<20 || c.GitOutputBytes < 1 || c.GitOutputBytes > 64<<20 {
		return LocalCeilings{}, errWorkerConfig
	}
	return c, nil
}
