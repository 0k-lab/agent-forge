package gate

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"agent-forge/internal/processtree"
	"agent-forge/internal/protocol"
)

var (
	githubOwner       = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,37}[a-z0-9])?$`)
	githubRepository  = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,99}$`)
	publicProvisionMu sync.Mutex
	publicCloneURL    = func(source publicSource) string { return source.URL() }
	publicGitRunner   = runPublicGit
)

type publicSource struct {
	Owner      string
	Repository string
}

func (s publicSource) Validate() error {
	if !githubOwner.MatchString(s.Owner) || !githubRepository.MatchString(s.Repository) || s.Repository == "." || s.Repository == ".." || strings.HasSuffix(s.Repository, ".git") {
		return errors.New("invalid public source")
	}
	return nil
}

func (s publicSource) URL() string {
	return "https://github.com/" + s.Owner + "/" + s.Repository + ".git"
}

type preparationError struct {
	reason    string
	retryable bool
}

func (preparationError) Error() string { return "repository preparation failed" }

func preparationFailure(reason string, retryable bool) error {
	return preparationError{reason: reason, retryable: retryable}
}

func preparePublicRepositoryRoot(path string) (string, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return "", errConfig
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return "", errConfig
	}
	return path, nil
}

func canonicalGitExecutable(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errConfig
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", errConfig
	}
	return resolved, nil
}

func publicRepositoryPath(root string, source publicSource) string {
	digest := sha256.Sum256([]byte(source.URL()))
	return filepath.Join(root, hex.EncodeToString(digest[:]))
}

func provisionPublicRepository(parent context.Context, config Config, repository RepositoryRegistration, base string) (string, error) {
	source, err := canonicalPublicGitHubURL(repository.RepositoryURL)
	if err != nil || protocol.ValidateBranchName(repository.DefaultBranch) != nil || protocol.ValidateBaseSHA(base) != nil || config.PublicRepositoryRoot == "" || config.GitExecutable == "" {
		return "", preparationFailure(protocol.EvidenceReasonSourcePolicyInvalid, false)
	}
	publicProvisionMu.Lock()
	defer publicProvisionMu.Unlock()
	target := publicRepositoryPath(config.PublicRepositoryRoot, source)
	remote := publicCloneURL(source)
	timeout, outputBytes := repository.Execution.GitTimeout, repository.Execution.GitOutputBytes
	if _, err := os.Lstat(target); errors.Is(err, os.ErrNotExist) {
		temp, err := os.MkdirTemp(config.PublicRepositoryRoot, ".forge-clone-")
		if err != nil {
			return "", preparationFailure(protocol.EvidenceReasonRepositoryStateUnsafe, false)
		}
		if err := os.Remove(temp); err != nil {
			return "", preparationFailure(protocol.EvidenceReasonRepositoryStateUnsafe, false)
		}
		keep := false
		defer func() {
			if !keep {
				_ = os.RemoveAll(temp)
			}
		}()
		if _, err := publicGitRunner(parent, config.GitExecutable, "", timeout, outputBytes, "clone", "--bare", "--template=", "--no-tags", "--single-branch", "--branch", repository.DefaultBranch, "--", remote, temp); err != nil {
			return "", preparationFailure(protocol.EvidenceReasonCloneFailed, true)
		}
		if _, err := publicGitRunner(parent, config.GitExecutable, temp, timeout, outputBytes, "config", "--local", "core.hooksPath", "/dev/null"); err != nil || validatePublicRepository(parent, config.GitExecutable, temp, remote, timeout, outputBytes) != nil || os.Rename(temp, target) != nil {
			return "", preparationFailure(protocol.EvidenceReasonRepositoryStateUnsafe, false)
		}
		keep = true
	} else if err != nil {
		return "", preparationFailure(protocol.EvidenceReasonRepositoryStateUnsafe, false)
	}
	if validatePublicRepository(parent, config.GitExecutable, target, remote, timeout, outputBytes) != nil {
		return "", preparationFailure(protocol.EvidenceReasonRepositoryStateUnsafe, false)
	}
	refspec := "+refs/heads/" + repository.DefaultBranch + ":refs/heads/" + repository.DefaultBranch
	if _, err := publicGitRunner(parent, config.GitExecutable, target, timeout, outputBytes, "fetch", "--no-tags", "--prune", "origin", refspec); err != nil {
		return "", preparationFailure(protocol.EvidenceReasonFetchFailed, true)
	}
	if _, err := publicGitRunner(parent, config.GitExecutable, target, timeout, outputBytes, "cat-file", "-e", base+"^{commit}"); err != nil {
		return "", preparationFailure(protocol.EvidenceReasonBaseUnavailable, false)
	}
	if _, err := publicGitRunner(parent, config.GitExecutable, target, timeout, outputBytes, "merge-base", "--is-ancestor", base, "refs/heads/"+repository.DefaultBranch); err != nil {
		return "", preparationFailure(protocol.EvidenceReasonBaseUnavailable, false)
	}
	return target, nil
}

func validatePublicRepository(parent context.Context, executable, path, remote string, timeout time.Duration, outputBytes int64) error {
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("unsafe repository")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil || resolved != path {
		return errors.New("unsafe repository")
	}
	bare, err := publicGitRunner(parent, executable, path, timeout, outputBytes, "rev-parse", "--is-bare-repository")
	if err != nil || bare != "true" {
		return errors.New("unsafe repository")
	}
	origin, err := publicGitRunner(parent, executable, path, timeout, outputBytes, "remote", "get-url", "--all", "origin")
	if err != nil || origin != remote {
		return errors.New("unsafe repository")
	}
	hooks, err := publicGitRunner(parent, executable, path, timeout, outputBytes, "config", "--local", "--get", "core.hooksPath")
	if err != nil || hooks != "/dev/null" {
		return errors.New("unsafe repository")
	}
	return nil
}

type boundedGitWriter struct {
	w io.Writer
	n int64
}

func (w *boundedGitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.n {
		return 0, errors.New("git output limit exceeded")
	}
	w.n -= int64(len(p))
	return w.w.Write(p)
}

func runPublicGit(parent context.Context, executable, dir string, timeout time.Duration, outputBytes int64, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(parent, timeout)
	defer cancel()
	hardened := []string{"-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "http.followRedirects=false", "-c", "protocol.file.allow=never"}
	if dir != "" {
		hardened = append(hardened, "-C", dir)
	}
	gitArgs := append([]string{executable}, append(hardened, args...)...)
	cmd := exec.Command("/bin/sh", append([]string{"-c", `umask 077 && exec "$@"`, "git"}, gitArgs...)...)
	cmd.Env = []string{"HOME=/dev", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0", "GCM_INTERACTIVE=never", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_PROTOCOL_FROM_USER=0"}
	var out bytes.Buffer
	budget := &boundedGitWriter{w: &out, n: outputBytes}
	cmd.Stdout = budget
	cmd.Stderr = &boundedGitWriter{w: io.Discard, n: outputBytes}
	err := processtree.Run(ctx, cmd)
	return strings.TrimSpace(out.String()), err
}
