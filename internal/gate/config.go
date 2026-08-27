package gate

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/protocol"
	"agent-forge/internal/store"
)

const maxGateSlots = 256

var (
	configID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	envName   = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
	errConfig = errors.New("invalid config: validation")
)

type LifecycleConfig struct {
	LeaseTTL    time.Duration `json:"-"`
	RetryBase   time.Duration `json:"-"`
	MaxAttempts int           `json:"max_attempts"`
}

type ExecutionConfig struct {
	PluginID          string        `json:"plugin_id"`
	Environment       []string      `json:"environment"`
	PluginTimeout     time.Duration `json:"-"`
	CheckTimeout      time.Duration `json:"-"`
	GitTimeout        time.Duration `json:"-"`
	CleanupTimeout    time.Duration `json:"-"`
	PluginOutputBytes int64         `json:"plugin_output_bytes"`
	CheckOutputBytes  int64         `json:"check_output_bytes"`
	GitOutputBytes    int64         `json:"git_output_bytes"`
}

type WorkerRegistration struct {
	ID          string `json:"id"`
	Pool        string `json:"pool"`
	TokenEnv    string `json:"token_env"`
	Concurrency int    `json:"concurrency"`
}

type RepositoryRegistration struct {
	ID            string          `json:"id"`
	RepositoryURL string          `json:"repository_url,omitempty"`
	DefaultBranch string          `json:"default_branch"`
	WorkerPool    string          `json:"worker_pool"`
	Execution     ExecutionConfig `json:"execution"`
}

type Config struct {
	Version              int
	Listen               string
	Database             string
	OwnerTokenEnv        string
	RecoveryInterval     time.Duration
	LeasePollInterval    time.Duration
	DefaultPool          string
	Lifecycle            LifecycleConfig
	DefaultExecution     ExecutionConfig
	Workers              []WorkerRegistration
	Repositories         []RepositoryRegistration
	PublicRepositoryRoot string
	GitExecutable        string
	ownerToken           string
	ownerDigest          [sha256.Size]byte
	workerTokens         []workerCredential
}

type workerCredential struct {
	digest       [sha256.Size]byte
	registration WorkerRegistration
}

type durationLifecycle struct {
	LeaseTTL    string `json:"lease_ttl"`
	RetryBase   string `json:"retry_base"`
	MaxAttempts int    `json:"max_attempts"`
}

type durationExecution struct {
	PluginID          string   `json:"plugin_id"`
	Environment       []string `json:"environment"`
	PluginTimeout     string   `json:"plugin_timeout"`
	CheckTimeout      string   `json:"check_timeout"`
	GitTimeout        string   `json:"git_timeout"`
	CleanupTimeout    string   `json:"cleanup_timeout"`
	PluginOutputBytes int64    `json:"plugin_output_bytes"`
	CheckOutputBytes  int64    `json:"check_output_bytes"`
	GitOutputBytes    int64    `json:"git_output_bytes"`
}

type rawRepository struct {
	ID            string            `json:"id"`
	RepositoryURL string            `json:"repository_url"`
	DefaultBranch string            `json:"default_branch"`
	WorkerPool    string            `json:"worker_pool"`
	Execution     durationExecution `json:"execution"`
}

type rawGateConfig struct {
	Version              int                  `json:"version"`
	Listen               string               `json:"listen"`
	Database             string               `json:"database"`
	OwnerTokenEnv        string               `json:"owner_token_env"`
	RecoveryInterval     string               `json:"recovery_interval"`
	LeasePollInterval    string               `json:"lease_poll_interval"`
	DefaultPool          string               `json:"default_pool"`
	Lifecycle            durationLifecycle    `json:"lifecycle"`
	DefaultExecution     durationExecution    `json:"default_execution"`
	Workers              []WorkerRegistration `json:"workers"`
	Repositories         []rawRepository      `json:"repositories"`
	PublicRepositoryRoot string               `json:"public_repository_root"`
	GitExecutable        string               `json:"git_executable"`
}

func LoadConfig(path string) (Config, error) {
	data, err := configjson.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	return ParseConfig(data, os.Getenv)
}

func ParseConfig(data []byte, getenv func(string) string) (Config, error) {
	var raw rawGateConfig
	if err := configjson.Decode(data, &raw); err != nil {
		return Config{}, err
	}
	if getenv == nil {
		return Config{}, errConfig
	}
	c := Config{Version: raw.Version, Listen: raw.Listen, Database: raw.Database, OwnerTokenEnv: raw.OwnerTokenEnv, DefaultPool: raw.DefaultPool, Workers: raw.Workers}
	var err error
	if c.RecoveryInterval, err = boundedDuration(raw.RecoveryInterval, time.Millisecond, time.Hour); err != nil {
		return Config{}, errConfig
	}
	if c.LeasePollInterval, err = boundedDuration(raw.LeasePollInterval, time.Millisecond, time.Minute); err != nil {
		return Config{}, errConfig
	}
	if c.Lifecycle, err = parseLifecycle(raw.Lifecycle); err != nil {
		return Config{}, errConfig
	}
	if c.DefaultExecution, err = parseExecution(raw.DefaultExecution); err != nil {
		return Config{}, errConfig
	}
	if c.Version != 1 || c.Listen == "" || len(c.Listen) > 256 || c.Database == "" || len(c.Database) > 4096 || !envName.MatchString(c.OwnerTokenEnv) || !configID.MatchString(c.DefaultPool) || c.RecoveryInterval > c.Lifecycle.LeaseTTL || len(c.Workers) == 0 || len(c.Workers) > maxGateSlots || len(raw.Repositories) > 1024 || (raw.PublicRepositoryRoot == "") != (raw.GitExecutable == "") {
		return Config{}, errConfig
	}
	if raw.PublicRepositoryRoot != "" {
		if c.PublicRepositoryRoot, err = preparePublicRepositoryRoot(raw.PublicRepositoryRoot); err != nil {
			return Config{}, errConfig
		}
		if c.GitExecutable, err = canonicalGitExecutable(raw.GitExecutable); err != nil {
			return Config{}, errConfig
		}
	}
	c.ownerToken = getenv(c.OwnerTokenEnv)
	if c.ownerToken == "" {
		return Config{}, errConfig
	}
	c.ownerDigest = sha256.Sum256([]byte(c.ownerToken))
	pools, workerIDs, tokenSources := map[string]bool{}, map[string]bool{}, map[string]bool{}
	totalSlots := 0
	for _, worker := range c.Workers {
		if !configID.MatchString(worker.ID) || !configID.MatchString(worker.Pool) || !envName.MatchString(worker.TokenEnv) || worker.Concurrency < 1 || worker.Concurrency > 64 || workerIDs[worker.ID] || tokenSources[worker.TokenEnv] {
			return Config{}, errConfig
		}
		workerIDs[worker.ID], tokenSources[worker.TokenEnv], pools[worker.Pool] = true, true, true
		totalSlots += worker.Concurrency
		token := getenv(worker.TokenEnv)
		digest := sha256.Sum256([]byte(token))
		if token == "" || subtle.ConstantTimeCompare(digest[:], c.ownerDigest[:]) == 1 {
			return Config{}, errConfig
		}
		for _, credential := range c.workerTokens {
			if subtle.ConstantTimeCompare(digest[:], credential.digest[:]) == 1 {
				return Config{}, errConfig
			}
		}
		c.workerTokens = append(c.workerTokens, workerCredential{digest, worker})
	}
	if totalSlots > maxGateSlots || !pools[c.DefaultPool] {
		return Config{}, errConfig
	}
	repositories := map[string]bool{}
	publicRepositories := 0
	for _, repository := range raw.Repositories {
		execution, err := parseExecution(repository.Execution)
		if err != nil || !configID.MatchString(repository.ID) || repositories[repository.ID] || protocol.ValidateBranchName(repository.DefaultBranch) != nil || !pools[repository.WorkerPool] {
			return Config{}, errConfig
		}
		if repository.RepositoryURL != "" {
			if _, err := canonicalPublicGitHubURL(repository.RepositoryURL); err != nil {
				return Config{}, errConfig
			}
			publicRepositories++
		}
		repositories[repository.ID] = true
		c.Repositories = append(c.Repositories, RepositoryRegistration{repository.ID, repository.RepositoryURL, repository.DefaultBranch, repository.WorkerPool, execution})
	}
	if publicRepositories > 0 && c.PublicRepositoryRoot == "" {
		return Config{}, errConfig
	}
	return c, nil
}

func parseLifecycle(raw durationLifecycle) (LifecycleConfig, error) {
	lease, err := boundedDuration(raw.LeaseTTL, time.Millisecond, 24*time.Hour)
	if err != nil {
		return LifecycleConfig{}, err
	}
	retry, err := boundedDuration(raw.RetryBase, time.Millisecond, 24*time.Hour)
	if err != nil || raw.MaxAttempts < 1 || raw.MaxAttempts > 100 {
		return LifecycleConfig{}, errConfig
	}
	return LifecycleConfig{lease, retry, raw.MaxAttempts}, nil
}

func parseExecution(raw durationExecution) (ExecutionConfig, error) {
	result := ExecutionConfig{PluginID: raw.PluginID, Environment: raw.Environment, PluginOutputBytes: raw.PluginOutputBytes, CheckOutputBytes: raw.CheckOutputBytes, GitOutputBytes: raw.GitOutputBytes}
	var err error
	for _, item := range []struct {
		value  string
		target *time.Duration
	}{{raw.PluginTimeout, &result.PluginTimeout}, {raw.CheckTimeout, &result.CheckTimeout}, {raw.GitTimeout, &result.GitTimeout}, {raw.CleanupTimeout, &result.CleanupTimeout}} {
		if *item.target, err = boundedDuration(item.value, time.Millisecond, 24*time.Hour); err != nil {
			return ExecutionConfig{}, err
		}
	}
	if !configID.MatchString(result.PluginID) || result.PluginOutputBytes < 1 || result.PluginOutputBytes > 64<<20 || result.CheckOutputBytes < 1 || result.CheckOutputBytes > 1<<20 || result.GitOutputBytes < 1 || result.GitOutputBytes > 64<<20 || len(result.Environment) > 64 {
		return ExecutionConfig{}, errConfig
	}
	seen := map[string]bool{}
	for _, name := range result.Environment {
		if !envName.MatchString(name) || seen[name] {
			return ExecutionConfig{}, errConfig
		}
		seen[name] = true
	}
	return result, nil
}

func boundedDuration(value string, low, high time.Duration) (time.Duration, error) {
	duration, err := time.ParseDuration(value)
	if err != nil || duration < low || duration > high {
		return 0, errConfig
	}
	return duration, nil
}

func (c Config) resolvedPolicy(pool string, execution ExecutionConfig, repositoryID, defaultBranch string) store.ResolvedPolicy {
	return store.ResolvedPolicy{
		Version: protocol.ResolvedPolicyVersion, WorkerPool: pool,
		LeaseTTLNanos: int64(c.Lifecycle.LeaseTTL), RetryBaseNanos: int64(c.Lifecycle.RetryBase), MaxAttempts: c.Lifecycle.MaxAttempts,
		RetryAlgorithm: protocol.RetryAlgorithmV1, RetryMaxNanos: int64(24 * time.Hour),
		Execution: store.ExecutionPolicy{
			RepositoryID: repositoryID, DefaultBranch: defaultBranch, PluginID: execution.PluginID, Environment: append([]string(nil), execution.Environment...),
			PluginTimeoutNanos: int64(execution.PluginTimeout), CheckTimeoutNanos: int64(execution.CheckTimeout), GitTimeoutNanos: int64(execution.GitTimeout), CleanupTimeoutNanos: int64(execution.CleanupTimeout),
			PluginOutputBytes: execution.PluginOutputBytes, CheckOutputBytes: execution.CheckOutputBytes, GitOutputBytes: execution.GitOutputBytes,
		},
	}
}

func canonicalPublicGitHubURL(value string) (publicSource, error) {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.Host != "github.com" || u.User != nil || u.Port() != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" {
		return publicSource{}, errConfig
	}
	parts := strings.Split(u.Path, "/")
	if len(parts) != 3 || parts[0] != "" {
		return publicSource{}, errConfig
	}
	if !strings.HasSuffix(parts[2], ".git") {
		return publicSource{}, errConfig
	}
	source := publicSource{Owner: parts[1], Repository: strings.TrimSuffix(parts[2], ".git")}
	if source.Validate() != nil || source.URL() != value {
		return publicSource{}, errConfig
	}
	return source, nil
}
