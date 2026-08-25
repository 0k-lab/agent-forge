package protocol

import (
	"errors"
	"regexp"
	"time"
)

const (
	ResolvedPolicyVersion = 1
	RetryAlgorithmV1      = "exponential-v1"
)

var (
	policyID  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
	policyEnv = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)
)

type ExecutionPolicy struct {
	RepositoryID        string   `json:"repository_id,omitempty"`
	DefaultBranch       string   `json:"default_branch,omitempty"`
	PluginID            string   `json:"plugin_id"`
	Environment         []string `json:"environment"`
	PluginTimeoutNanos  int64    `json:"plugin_timeout_nanos"`
	CheckTimeoutNanos   int64    `json:"check_timeout_nanos"`
	GitTimeoutNanos     int64    `json:"git_timeout_nanos"`
	CleanupTimeoutNanos int64    `json:"cleanup_timeout_nanos"`
	PluginOutputBytes   int64    `json:"plugin_output_bytes"`
	CheckOutputBytes    int64    `json:"check_output_bytes"`
	GitOutputBytes      int64    `json:"git_output_bytes"`
}

type ResolvedPolicy struct {
	Version        int             `json:"version"`
	WorkerPool     string          `json:"worker_pool"`
	LeaseTTLNanos  int64           `json:"lease_ttl_nanos"`
	RetryBaseNanos int64           `json:"retry_base_nanos"`
	MaxAttempts    int             `json:"max_attempts"`
	RetryAlgorithm string          `json:"retry_algorithm"`
	RetryMaxNanos  int64           `json:"retry_max_nanos"`
	Execution      ExecutionPolicy `json:"execution"`
}

func (p ResolvedPolicy) Validate() error {
	if p.Version != ResolvedPolicyVersion || !policyID.MatchString(p.WorkerPool) || p.LeaseTTLNanos <= 0 || p.LeaseTTLNanos > int64(24*time.Hour) || p.RetryBaseNanos <= 0 || p.RetryBaseNanos > int64(24*time.Hour) || p.MaxAttempts < 1 || p.MaxAttempts > 100 || p.RetryAlgorithm != RetryAlgorithmV1 || p.RetryMaxNanos != int64(24*time.Hour) {
		return errors.New("invalid resolved policy")
	}
	e := p.Execution
	if !policyID.MatchString(e.PluginID) || e.RepositoryID != "" && !policyID.MatchString(e.RepositoryID) || (e.RepositoryID == "") != (e.DefaultBranch == "") || e.RepositoryID != "" && ValidateBranchName(e.DefaultBranch) != nil || e.PluginTimeoutNanos <= 0 || e.PluginTimeoutNanos > int64(24*time.Hour) || e.CheckTimeoutNanos <= 0 || e.CheckTimeoutNanos > int64(24*time.Hour) || e.GitTimeoutNanos <= 0 || e.GitTimeoutNanos > int64(24*time.Hour) || e.CleanupTimeoutNanos <= 0 || e.CleanupTimeoutNanos > int64(24*time.Hour) || e.PluginOutputBytes <= 0 || e.PluginOutputBytes > 64<<20 || e.CheckOutputBytes <= 0 || e.CheckOutputBytes > 1<<20 || e.GitOutputBytes <= 0 || e.GitOutputBytes > 64<<20 || len(e.Environment) > 64 {
		return errors.New("invalid resolved policy")
	}
	seen := map[string]bool{}
	for _, name := range e.Environment {
		if !policyEnv.MatchString(name) || seen[name] {
			return errors.New("invalid resolved policy")
		}
		seen[name] = true
	}
	return nil
}

func ValidateBranchName(branch string) error {
	if branch == "" || branch == "@" || len(branch) > 255 || branch[0] == '-' || branch[0] == '/' || branch[len(branch)-1] == '/' || branch[len(branch)-1] == '.' {
		return errors.New("invalid branch")
	}
	componentStart := true
	for i := 0; i < len(branch); i++ {
		b := branch[i]
		if b <= ' ' || b == 0x7f || b == '~' || b == '^' || b == ':' || b == '?' || b == '*' || b == '[' || b == '\\' {
			return errors.New("invalid branch")
		}
		if componentStart && b == '.' || i > 0 && branch[i-1] == '.' && b == '.' || i > 0 && branch[i-1] == '@' && b == '{' {
			return errors.New("invalid branch")
		}
		componentStart = b == '/'
		if componentStart && (i+1 == len(branch) || branch[i+1] == '/') {
			return errors.New("invalid branch")
		}
	}
	for start := 0; start < len(branch); {
		end := start
		for end < len(branch) && branch[end] != '/' {
			end++
		}
		if end-start >= 5 && branch[end-5:end] == ".lock" {
			return errors.New("invalid branch")
		}
		start = end + 1
	}
	return nil
}
