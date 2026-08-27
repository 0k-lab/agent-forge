package githubdelivery

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"agent-forge/internal/configjson"
	"agent-forge/internal/processtree"
)

const maxOutput = 1 << 20

var (
	candidateRefPattern = regexp.MustCompile(`^refs/agent-forge/candidates/[0-9a-f]{32}/[0-9a-f]{32}$`)
	branchPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
)

type Publication struct {
	Version           int    `json:"version"`
	CandidateSHA      string `json:"candidate_sha"`
	ExpectedParentSHA string `json:"expected_parent_sha"`
	ExpectedTreeSHA   string `json:"expected_tree_sha"`
	CandidateRef      string `json:"candidate_ref"`
	BaseBranch        string `json:"base_branch"`
	NewBranch         string `json:"new_branch"`
	PRTitle           string `json:"pr_title"`
	PRBody            string `json:"pr_body"`
}

type Result struct {
	Version      int    `json:"version"`
	CandidateSHA string `json:"candidate_sha"`
	Branch       string `json:"branch"`
	PRURL        string `json:"pr_url"`
	Status       string `json:"status"`
}

type Error struct {
	Code string `json:"code"`
}

func (e *Error) Error() string               { return e.Code }
func AsError(err error, target **Error) bool { return errors.As(err, target) }
func failure(code string) error              { return &Error{Code: code} }

type RunFunc func(context.Context, string, []string, string, ...string) ([]byte, error)

type Options struct {
	BaseURL    string
	HTTPClient *http.Client
	Run        RunFunc
	Timeout    time.Duration
	RetryDelay func(context.Context, time.Duration) error
	Now        func() time.Time
}

type localState struct {
	changed []string
	sandbox *gitSandbox
}

type gitSandbox struct {
	root string
	repo string
	env  []string
}

func ParsePublication(data []byte) (Publication, error) {
	var publication Publication
	if !utf8.Valid(data) || configjson.Decode(data, &publication) != nil {
		return Publication{}, failure("publication_invalid")
	}
	return publication, nil
}

func ReadPublication(reader io.Reader) (Publication, error) {
	data, err := io.ReadAll(io.LimitReader(reader, configjson.MaxBytes+1))
	if err != nil || len(data) > configjson.MaxBytes {
		return Publication{}, failure("publication_invalid")
	}
	return ParsePublication(data)
}

func RequiredPermissions(paths []string) map[string]string {
	permissions := map[string]string{"contents": "write", "pull_requests": "write"}
	for _, path := range paths {
		if path == ".github/workflows" || strings.HasPrefix(path, ".github/workflows/") {
			permissions["workflows"] = "write"
			break
		}
	}
	return permissions
}

func Preflight(ctx context.Context, cfg Config, input Publication, options Options) ([]string, error) {
	state, err := localPreflight(ctx, cfg, input, normalizeOptions(options))
	if state.sandbox != nil {
		defer state.sandbox.remove()
	}
	return state.changed, err
}

func Deliver(parent context.Context, cfg Config, input Publication, options Options) (Result, error) {
	op := normalizeOptions(options)
	ctx, cancel := context.WithTimeout(parent, op.Timeout)
	defer cancel()
	state, err := localPreflight(ctx, cfg, input, op)
	if err != nil {
		return Result{}, err
	}
	defer state.sandbox.remove()
	key, err := readPrivateKey(cfg.PrivateKeyPath)
	if err != nil {
		return Result{}, failure("credential_invalid")
	}
	api := newAPI(cfg, op, key)
	permissions := RequiredPermissions(state.changed)
	installationID, err := api.preflight(ctx, permissions)
	if err != nil {
		return Result{}, err
	}
	token, err := api.mint(ctx, installationID, permissions)
	if err != nil {
		return Result{}, err
	}
	refreshed := false
	refresh := func() error {
		if refreshed {
			return failure("credential_expired")
		}
		refreshed = true
		token, err = api.mint(ctx, installationID, permissions)
		return err
	}
	baseSHA, baseFound, err := api.branch(ctx, token, input.BaseBranch)
	if isCode(err, "credential_expired") {
		if err = refresh(); err == nil {
			baseSHA, baseFound, err = api.branch(ctx, token, input.BaseBranch)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if !baseFound || baseSHA != input.ExpectedParentSHA {
		return Result{}, failure("base_drift")
	}
	remoteSHA, found, err := api.branch(ctx, token, input.NewBranch)
	if isCode(err, "credential_expired") {
		if err = refresh(); err == nil {
			remoteSHA, found, err = api.branch(ctx, token, input.NewBranch)
		}
	}
	if err != nil {
		return Result{}, err
	}
	if found && remoteSHA != input.CandidateSHA {
		return Result{}, failure("branch_conflict")
	}
	if !found {
		if err := push(ctx, cfg, input, token, state.sandbox, op); err != nil {
			return Result{}, err
		}
	}
	prURL, err := api.reconcilePR(ctx, token, input, func() (string, error) {
		if err := refresh(); err != nil {
			return "", err
		}
		return token, nil
	})
	if err != nil {
		return Result{}, err
	}
	return Result{Version: 1, CandidateSHA: input.CandidateSHA, Branch: input.NewBranch, PRURL: prURL, Status: "open"}, nil
}

func normalizeOptions(options Options) Options {
	if options.Run == nil {
		options.Run = runCommand
	}
	if options.HTTPClient == nil {
		options.HTTPClient = http.DefaultClient
	}
	if options.Timeout <= 0 || options.Timeout > 2*time.Minute {
		options.Timeout = 30 * time.Second
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.RetryDelay == nil {
		options.RetryDelay = func(ctx context.Context, delay time.Duration) error {
			timer := time.NewTimer(delay)
			defer timer.Stop()
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-timer.C:
				return nil
			}
		}
	}
	return options
}

func localPreflight(ctx context.Context, cfg Config, input Publication, op Options) (localState, error) {
	if input.Version != 1 || !hash(input.CandidateSHA) || !hash(input.ExpectedParentSHA) || !hash(input.ExpectedTreeSHA) || !candidateRefPattern.MatchString(input.CandidateRef) || !safeBranch(input.BaseBranch) || !safeBranch(input.NewBranch) || input.BaseBranch == input.NewBranch || input.PRTitle == "" || len(input.PRTitle) > 256 || len(input.PRBody) > 65536 {
		return localState{}, failure("publication_invalid")
	}
	repo, err := ownedDirectory(cfg.LocalRepository)
	if err != nil {
		return localState{}, failure("local_repository_unsafe")
	}
	if _, err := protectedExecutable(cfg.GitExecutable); err != nil {
		return localState{}, failure("git_executable_unsafe")
	}
	sandbox, err := newGitSandbox()
	if err != nil {
		return localState{}, failure("staging_failed")
	}
	state := localState{sandbox: sandbox}
	fail := func(code string) (localState, error) {
		sandbox.remove()
		return localState{}, failure(code)
	}
	run := func(args ...string) (string, error) {
		args = append([]string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "maintenance.auto=false", "-c", "credential.helper=", "-c", "uploadpack.packObjectsHook=", "--no-pager", "--no-optional-locks"}, args...)
		out, err := op.Run(ctx, repo, sandbox.env, cfg.GitExecutable, args...)
		if len(out) > maxOutput {
			return "", failure("command_output_exceeded")
		}
		return strings.TrimSpace(string(out)), err
	}
	if base, err := run("rev-parse", "--verify", "refs/heads/"+input.BaseBranch+"^{commit}"); err != nil || base != input.ExpectedParentSHA {
		return fail("base_drift")
	}
	if candidate, err := run("rev-parse", "--verify", input.CandidateRef+"^{commit}"); err != nil || candidate != input.CandidateSHA {
		return fail("candidate_ref_mismatch")
	}
	if parents, err := run("rev-list", "--parents", "-n", "1", input.CandidateSHA); err != nil || !exactParent(parents, input.CandidateSHA, input.ExpectedParentSHA) {
		return fail("candidate_parent_mismatch")
	}
	if tree, err := run("rev-parse", input.CandidateSHA+"^{tree}"); err != nil || tree != input.ExpectedTreeSHA {
		return fail("candidate_tree_mismatch")
	}
	if branch, err := run("rev-parse", "--verify", "refs/heads/"+input.NewBranch+"^{commit}"); err == nil && branch != input.CandidateSHA {
		return fail("branch_conflict")
	}
	bundle := filepath.Join(sandbox.root, "candidate.bundle")
	if _, err := run("bundle", "create", bundle, input.CandidateRef, "refs/heads/"+input.BaseBranch); err != nil {
		return fail("staging_failed")
	}
	stageRun := func(args ...string) (string, error) {
		out, err := op.Run(ctx, sandbox.repo, sandbox.env, cfg.GitExecutable, args...)
		return strings.TrimSpace(string(out)), err
	}
	if _, err := stageRun("init", "--bare", "--quiet", "."); err != nil {
		return fail("staging_failed")
	}
	if _, err := stageRun("-c", "maintenance.auto=false", "fetch", "--quiet", "--no-tags", "--no-write-fetch-head", bundle, input.CandidateRef+":"+input.CandidateRef, "refs/heads/"+input.BaseBranch+":refs/heads/"+input.BaseBranch); err != nil {
		return fail("staging_failed")
	}
	_ = os.Remove(bundle)
	verify := func(args ...string) (string, error) {
		return stageRun(append([]string{"--no-optional-locks"}, args...)...)
	}
	if candidate, err := verify("rev-parse", "--verify", input.CandidateRef+"^{commit}"); err != nil || candidate != input.CandidateSHA {
		return fail("candidate_ref_mismatch")
	}
	if base, err := verify("rev-parse", "--verify", "refs/heads/"+input.BaseBranch+"^{commit}"); err != nil || base != input.ExpectedParentSHA {
		return fail("base_drift")
	}
	if parents, err := verify("rev-list", "--parents", "-n", "1", input.CandidateSHA); err != nil || !exactParent(parents, input.CandidateSHA, input.ExpectedParentSHA) {
		return fail("candidate_parent_mismatch")
	}
	if tree, err := verify("rev-parse", input.CandidateSHA+"^{tree}"); err != nil || tree != input.ExpectedTreeSHA {
		return fail("candidate_tree_mismatch")
	}
	changedOutput, err := op.Run(ctx, sandbox.repo, sandbox.env, cfg.GitExecutable, "--no-optional-locks", "diff", "--name-only", "-z", "--no-renames", input.ExpectedParentSHA+".."+input.CandidateSHA)
	changed, parseErr := parseChangedPaths(changedOutput)
	if err != nil || parseErr != nil {
		return fail("candidate_diff_invalid")
	}
	state.changed = changed
	return state, nil
}

func parseChangedPaths(output []byte) ([]string, error) {
	if len(output) == 0 || len(output) > maxOutput || output[len(output)-1] != 0 {
		return nil, failure("candidate_diff_invalid")
	}
	entries := bytes.Split(output[:len(output)-1], []byte{0})
	paths := make([]string, len(entries))
	for i, entry := range entries {
		if len(entry) == 0 {
			return nil, failure("candidate_diff_invalid")
		}
		paths[i] = string(entry)
	}
	return paths, nil
}

func safeBranch(value string) bool {
	return branchPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, "//") && !strings.HasSuffix(value, "/") && !strings.HasSuffix(value, ".") && !strings.HasSuffix(value, ".lock") && !strings.Contains(value, "@{")
}

func hash(value string) bool {
	if len(value) != 40 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && hex.EncodeToString(decoded) == value
}

func exactParent(output, candidate, parent string) bool {
	fields := strings.Fields(output)
	return len(fields) == 2 && fields[0] == candidate && fields[1] == parent
}

func push(ctx context.Context, cfg Config, input Publication, token string, sandbox *gitSandbox, op Options) error {
	askpass := filepath.Join(sandbox.root, "askpass")
	script := "#!/bin/sh\ncase \"$1\" in *Username*) printf '%s\\n' x-access-token;; *) printf '%s\\n' \"$FORGE_GITHUB_TOKEN\";; esac\n"
	if err := os.WriteFile(askpass, []byte(script), 0o700); err != nil {
		return failure("push_failed")
	}
	env := append([]string{}, sandbox.env...)
	env = append(env, "GIT_ASKPASS="+askpass, "FORGE_GITHUB_TOKEN="+token)
	remote := "https://github.com/" + cfg.Owner + "/" + cfg.Repository + ".git"
	out, err := op.Run(ctx, sandbox.repo, env, cfg.GitExecutable, "-c", "credential.helper=", "-c", "core.hooksPath=/dev/null", "-c", "http.followRedirects=false", "push", "--no-recurse-submodules", "--porcelain", "--atomic", "--force-with-lease=refs/heads/"+input.BaseBranch+":"+input.ExpectedParentSHA, "--force-with-lease=refs/heads/"+input.NewBranch+":", remote, input.CandidateRef+":refs/heads/"+input.NewBranch, "refs/heads/"+input.BaseBranch+":refs/heads/"+input.BaseBranch)
	if err != nil || len(out) > maxOutput {
		message := string(out)
		if strings.Contains(message, ":refs/heads/"+input.BaseBranch+"\t[rejected] (stale info)") {
			return failure("base_drift")
		}
		if strings.Contains(message, ":refs/heads/"+input.NewBranch+"\t[rejected] (stale info)") {
			return failure("push_conflict")
		}
		return failure("push_failed")
	}
	return nil
}

func readPrivateKey(path string) (*rsa.PrivateKey, error) {
	body, err := readProtectedFile(path, 0o600, maxOutput)
	if err != nil {
		return nil, errConfig
	}
	block, _ := pem.Decode(body)
	if block == nil {
		return nil, errConfig
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key, nil
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	key, ok := parsed.(*rsa.PrivateKey)
	if err != nil || !ok {
		return nil, errConfig
	}
	return key, nil
}

func appJWT(appID string, key *rsa.PrivateKey, now time.Time) (string, error) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{"iat": now.Add(-time.Minute).Unix(), "exp": now.Add(9 * time.Minute).Unix(), "iss": appID})
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}

func runCommand(ctx context.Context, dir string, env []string, name string, args ...string) ([]byte, error) {
	cmd := exec.Command(name, args...)
	cmd.Dir = dir
	cmd.Env = env
	var output limitedBuffer
	cmd.Stdout, cmd.Stderr = &output, &output
	err := processtree.RunInvocation(ctx, cmd)
	if output.exceeded {
		return nil, failure("command_output_exceeded")
	}
	return output.Bytes(), err
}

func newGitSandbox() (*gitSandbox, error) {
	root, err := os.MkdirTemp("", "forge-github-")
	if err != nil {
		return nil, errConfig
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, errConfig
	}
	sandbox := &gitSandbox{root: root, repo: filepath.Join(root, "repo.git")}
	for _, dir := range []string{sandbox.repo, filepath.Join(root, "home"), filepath.Join(root, "xdg")} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			sandbox.remove()
			return nil, errConfig
		}
	}
	sandbox.env = []string{
		"HOME=" + filepath.Join(root, "home"),
		"XDG_CONFIG_HOME=" + filepath.Join(root, "xdg"),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_TRACE=0",
		"GIT_TRACE_PACKET=0",
		"GIT_CURL_VERBOSE=0",
		"LC_ALL=C",
	}
	return sandbox, nil
}

func (s *gitSandbox) remove() { _ = os.RemoveAll(s.root) }

type limitedBuffer struct {
	bytes.Buffer
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	n := len(p)
	remaining := maxOutput - b.Len()
	if b.Len() < maxOutput {
		keep := min(len(p), remaining)
		_, _ = b.Buffer.Write(p[:keep])
	}
	if len(p) > remaining {
		b.exceeded = true
	}
	return n, nil
}

func isCode(err error, code string) bool {
	var typed *Error
	return errors.As(err, &typed) && typed.Code == code
}

func escaped(value string) string { return url.PathEscape(value) }
func number(value int64) string   { return strconv.FormatInt(value, 10) }
