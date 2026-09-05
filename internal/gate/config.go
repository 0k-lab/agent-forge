package gate

import (
	"bytes"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"agent-forge/internal/configjson"
	"agent-forge/internal/githubdelivery"
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
	ID                string          `json:"id"`
	RepositoryURL     string          `json:"repository_url,omitempty"`
	DefaultBranch     string          `json:"default_branch"`
	WorkerPool        string          `json:"worker_pool"`
	Execution         ExecutionConfig `json:"execution"`
	DeploymentProfile string          `json:"deployment_profile,omitempty"`
}

type DeploymentCommand struct {
	Argv    []string      `json:"argv"`
	Timeout time.Duration `json:"-"`
}

type DeploymentProfile struct {
	Version       int               `json:"version"`
	ID            string            `json:"id"`
	Target        string            `json:"target"`
	Prepare       DeploymentCommand `json:"prepare"`
	Activate      DeploymentCommand `json:"activate"`
	Healthcheck   DeploymentCommand `json:"healthcheck"`
	CleanupPolicy string            `json:"cleanup_policy"`
}

type DeliveryConfig struct {
	APIBase        string
	AppID          string `json:"-"`
	PrivateKeyPath string
	MaxAttempts    int
	RetryBase      time.Duration
	PollInterval   time.Duration
	NoRunsGrace    time.Duration
	Timeout        time.Duration
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
	DeploymentProfiles   []DeploymentProfile `json:"deployment_profiles,omitempty"`
	PublicRepositoryRoot string
	GitExecutable        string
	Delivery             *DeliveryConfig
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

type rawDeploymentProfileReference struct {
	value *string
}

func (reference *rawDeploymentProfileReference) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errConfig
	}
	var value string
	if err := json.Unmarshal(data, &value); err != nil {
		return errConfig
	}
	reference.value = &value
	return nil
}

type rawRepository struct {
	ID                string                        `json:"id"`
	RepositoryURL     string                        `json:"repository_url"`
	DefaultBranch     string                        `json:"default_branch"`
	WorkerPool        string                        `json:"worker_pool"`
	Execution         durationExecution             `json:"execution"`
	DeploymentProfile rawDeploymentProfileReference `json:"deployment_profile"`
}

type rawDeploymentCommand struct {
	Argv    []string `json:"argv"`
	Timeout string   `json:"timeout"`
}

type rawDeploymentProfile struct {
	Version       int                  `json:"version"`
	ID            string               `json:"id"`
	Target        string               `json:"target"`
	Prepare       rawDeploymentCommand `json:"prepare"`
	Activate      rawDeploymentCommand `json:"activate"`
	Healthcheck   rawDeploymentCommand `json:"healthcheck"`
	CleanupPolicy string               `json:"cleanup_policy"`
}

type rawDeploymentProfiles []rawDeploymentProfile

func (profiles *rawDeploymentProfiles) UnmarshalJSON(data []byte) error {
	if bytes.Equal(data, []byte("null")) {
		return errConfig
	}
	type plainDeploymentProfiles rawDeploymentProfiles
	var decoded plainDeploymentProfiles
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return errConfig
	}
	*profiles = rawDeploymentProfiles(decoded)
	return nil
}

type rawGateConfig struct {
	Version              int                   `json:"version"`
	Listen               string                `json:"listen"`
	Database             string                `json:"database"`
	OwnerTokenEnv        string                `json:"owner_token_env"`
	RecoveryInterval     string                `json:"recovery_interval"`
	LeasePollInterval    string                `json:"lease_poll_interval"`
	DefaultPool          string                `json:"default_pool"`
	Lifecycle            durationLifecycle     `json:"lifecycle"`
	DefaultExecution     durationExecution     `json:"default_execution"`
	Workers              []WorkerRegistration  `json:"workers"`
	Repositories         []rawRepository       `json:"repositories"`
	DeploymentProfiles   rawDeploymentProfiles `json:"deployment_profiles"`
	PublicRepositoryRoot string                `json:"public_repository_root"`
	GitExecutable        string                `json:"git_executable"`
	Delivery             *rawDeliveryConfig    `json:"delivery"`
}

type rawDeliveryConfig struct {
	APIBase        string `json:"api_base"`
	AppIDEnv       string `json:"github_app_id_env"`
	PrivateKeyPath string `json:"github_app_private_key_path"`
	MaxAttempts    int    `json:"max_attempts"`
	RetryBase      string `json:"retry_base"`
	PollInterval   string `json:"poll_interval"`
	NoRunsGrace    string `json:"no_runs_grace"`
	Timeout        string `json:"timeout"`
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
	if c.Version != 1 || c.Listen == "" || len(c.Listen) > 256 || c.Database == "" || len(c.Database) > 4096 || !envName.MatchString(c.OwnerTokenEnv) || !configID.MatchString(c.DefaultPool) || c.RecoveryInterval > c.Lifecycle.LeaseTTL || len(c.Workers) == 0 || len(c.Workers) > maxGateSlots || len(raw.Repositories) > 1024 || len(raw.DeploymentProfiles) > 128 || (raw.PublicRepositoryRoot == "") != (raw.GitExecutable == "") {
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
	profiles := map[string]bool{}
	for _, rawProfile := range raw.DeploymentProfiles {
		profile, err := parseDeploymentProfile(rawProfile)
		if err != nil || profiles[profile.ID] {
			return Config{}, errConfig
		}
		profiles[profile.ID] = true
		c.DeploymentProfiles = append(c.DeploymentProfiles, profile)
	}
	repositories := map[string]bool{}
	publicRepositories := 0
	for _, repository := range raw.Repositories {
		execution, err := parseExecution(repository.Execution)
		profileID := ""
		if repository.DeploymentProfile.value != nil {
			profileID = *repository.DeploymentProfile.value
		}
		if err != nil || !configID.MatchString(repository.ID) || repositories[repository.ID] || protocol.ValidateBranchName(repository.DefaultBranch) != nil || !pools[repository.WorkerPool] || repository.DeploymentProfile.value != nil && (!configID.MatchString(profileID) || !profiles[profileID]) {
			return Config{}, errConfig
		}
		if repository.RepositoryURL != "" {
			if _, err := canonicalPublicGitHubURL(repository.RepositoryURL); err != nil {
				return Config{}, errConfig
			}
			publicRepositories++
		}
		repositories[repository.ID] = true
		c.Repositories = append(c.Repositories, RepositoryRegistration{ID: repository.ID, RepositoryURL: repository.RepositoryURL, DefaultBranch: repository.DefaultBranch, WorkerPool: repository.WorkerPool, Execution: execution, DeploymentProfile: profileID})
	}
	if publicRepositories > 0 && c.PublicRepositoryRoot == "" {
		return Config{}, errConfig
	}
	if raw.Delivery != nil {
		d := &DeliveryConfig{APIBase: raw.Delivery.APIBase, AppID: getenv(raw.Delivery.AppIDEnv), PrivateKeyPath: raw.Delivery.PrivateKeyPath, MaxAttempts: raw.Delivery.MaxAttempts}
		if publicRepositories == 0 || !envName.MatchString(raw.Delivery.AppIDEnv) || d.MaxAttempts < 1 || d.MaxAttempts > 100 {
			return Config{}, errConfig
		}
		for _, item := range []struct {
			value  string
			target *time.Duration
		}{{raw.Delivery.RetryBase, &d.RetryBase}, {raw.Delivery.PollInterval, &d.PollInterval}, {raw.Delivery.NoRunsGrace, &d.NoRunsGrace}, {raw.Delivery.Timeout, &d.Timeout}} {
			if *item.target, err = boundedDuration(item.value, time.Millisecond, 24*time.Hour); err != nil {
				return Config{}, errConfig
			}
		}
		var source publicSource
		for _, repository := range c.Repositories {
			if repository.RepositoryURL != "" {
				source, _ = canonicalPublicGitHubURL(repository.RepositoryURL)
				break
			}
		}
		if githubdelivery.ValidateConfig(githubdelivery.Config{Version: 1, APIBase: d.APIBase, Owner: source.Owner, Repository: source.Repository, LocalRepository: c.PublicRepositoryRoot, GitExecutable: c.GitExecutable, AppID: d.AppID, PrivateKeyPath: d.PrivateKeyPath}) != nil {
			return Config{}, errConfig
		}
		c.Delivery = d
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

func parseDeploymentProfile(raw rawDeploymentProfile) (DeploymentProfile, error) {
	if raw.Version != 1 || !configID.MatchString(raw.ID) || !configID.MatchString(raw.Target) || raw.CleanupPolicy != "restore_previous" && raw.CleanupPolicy != "retain" && raw.CleanupPolicy != "cleanup" {
		return DeploymentProfile{}, errConfig
	}
	profile := DeploymentProfile{Version: raw.Version, ID: raw.ID, Target: raw.Target, CleanupPolicy: raw.CleanupPolicy}
	commands := []struct {
		raw    rawDeploymentCommand
		target *DeploymentCommand
	}{{raw.Prepare, &profile.Prepare}, {raw.Activate, &profile.Activate}, {raw.Healthcheck, &profile.Healthcheck}}
	for _, item := range commands {
		if len(item.raw.Argv) == 0 || len(item.raw.Argv) > 64 || !filepath.IsAbs(item.raw.Argv[0]) {
			return DeploymentProfile{}, errConfig
		}
		for _, arg := range item.raw.Argv {
			if arg == "" || len(arg) > 4096 || !utf8.ValidString(arg) || strings.IndexByte(arg, 0) >= 0 {
				return DeploymentProfile{}, errConfig
			}
		}
		timeout, err := boundedDuration(item.raw.Timeout, time.Millisecond, 24*time.Hour)
		if err != nil {
			return DeploymentProfile{}, errConfig
		}
		*item.target = DeploymentCommand{Argv: append([]string(nil), item.raw.Argv...), Timeout: timeout}
	}
	return profile, nil
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
