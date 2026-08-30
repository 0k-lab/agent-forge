//go:build linux

package linuxinstall

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-forge/internal/configjson"
	"golang.org/x/sys/unix"
)

const (
	prefix                 = "/opt/agent-forge"
	storeSchemaVersion     = 5
	legacyInstallerVersion = "v0.1.3"
	legacyInstallerCommit  = "dfa09f5fd82a01b79c977cae20299db79ede9bdc"
)

var (
	versionRE = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	commitRE  = regexp.MustCompile(`^[0-9a-f]{40}$`)
	digestRE  = regexp.MustCompile(`^[0-9a-f]{64}$`)
)

type AccountManager interface {
	Ensure(name, home string) (AccountState, error)
	Rollback(name string, state AccountState) error
}
type AccountState struct {
	UID, GID                  int
	UserCreated, GroupCreated bool
}
type accountValidator interface {
	Validate(name, home string) (uid, gid int, err error)
}
type ServiceManager interface {
	Run(argv ...string) error
	GateReady(ownerToken, version, commit string) error
	WorkerReady(ownerToken string) error
}
type OwnershipManager interface {
	Chown(name string, uid, gid int) error
	Owner(name string) (uid, gid int, err error)
}

type Options struct {
	Version, Commit, AssetDir, SHA256SUMSSHA256 string
	InstallerVersion, InstallerCommit           string
	EnableNow, RunAsRoot, Upgrade               bool
	Root, Arch                                  string
	Account                                     AccountManager
	Services                                    ServiceManager
	Ownership                                   OwnershipManager
	Random                                      io.Reader
}

type installStageError struct {
	stage string
	err   error
}

func (e *installStageError) Error() string { return e.err.Error() }
func (e *installStageError) Unwrap() error { return e.err }

// FailureStage returns a coarse, secret-free installer stage suitable for diagnostics.
func FailureStage(err error) (string, bool) {
	var stageErr *installStageError
	if !errors.As(err, &stageErr) {
		return "", false
	}
	return stageErr.stage, true
}

type receipt struct {
	Version            string                    `json:"version"`
	Commit             string                    `json:"commit"`
	Architecture       string                    `json:"architecture"`
	ArchiveSHA256      map[string]string         `json:"archive_sha256"`
	OwnerTokenSHA256   string                    `json:"owner_token_sha256"`
	WorkerTokenSHA256  string                    `json:"worker_token_sha256"`
	RunAsRoot          bool                      `json:"run_as_root"`
	AccountUID         int                       `json:"account_uid"`
	AccountGID         int                       `json:"account_gid"`
	StoreSchemaVersion int                       `json:"store_schema_version"`
	Files              map[string]string         `json:"files"`
	Modes              map[string]uint32         `json:"modes"`
	Directories        map[string]directoryState `json:"directories"`
}

type directoryState struct {
	Mode uint32 `json:"mode"`
	UID  uint32 `json:"uid"`
	GID  uint32 `json:"gid"`
}

type verifiedArchive struct {
	role, name, file string
	digest           string
}

func Install(o Options) (retErr error) {
	failureStage := "validate"
	defer func() {
		if retErr != nil {
			if _, ok := FailureStage(retErr); !ok {
				retErr = &installStageError{stage: failureStage, err: retErr}
			}
		}
	}()
	if err := validate(&o); err != nil {
		return err
	}
	failureStage = "assets"
	archives, err := verifyAssets(o)
	if err != nil {
		return err
	}
	defer func() {
		for _, a := range archives {
			_ = os.Remove(a.file)
		}
	}()
	failureStage = "archives"
	if err := validateArchives(archives, o.Version, o.Commit); err != nil {
		return err
	}

	failureStage = "existing"
	installPath := rooted(o.Root, prefix)
	if o.Upgrade {
		if o.Root == "" {
			if err := requireTrustedAncestor("/opt"); err != nil {
				return err
			}
			if err := requireTrustedAncestor("/etc/systemd/system"); err != nil {
				return err
			}
		}
		lock, lockErr := acquireUpgradeLock(o.Root)
		if lockErr != nil {
			return &installStageError{stage: "host", err: lockErr}
		}
		defer lock.Close()
	}
	existing, err := inspectExistingInstall(installPath, o)
	if err != nil {
		return err
	}
	if existing != nil {
		if !o.RunAsRoot {
			if o.Account == nil {
				return errors.New("dedicated account manager is required")
			}
			validator, ok := o.Account.(accountValidator)
			if !ok {
				return errors.New("non-mutating dedicated account validation is required")
			}
			uid, gid, accountErr := validator.Validate("agent-forge", prefix)
			if accountErr != nil || uid != existing.AccountUID || gid != existing.AccountGID {
				return errors.New("dedicated account is not exact")
			}
		}
		if err := validateLinks(o.Root); err != nil {
			return err
		}
		if sameRelease(existing, o, archives) {
			if o.Upgrade {
				return errors.New("upgrade target is not newer than installed release")
			}
			return nil
		}
		if !o.Upgrade {
			return errors.New("different release requires explicit upgrade")
		}
		if o.Root == "" {
			if err := requireTrustedAncestor("/opt"); err != nil {
				return err
			}
			if err := requireTrustedAncestor("/etc/systemd/system"); err != nil {
				return err
			}
		}
		if compareVersions(o.Version, existing.Version) <= 0 {
			return errors.New("requested release is not newer than installed release")
		}
		parent := filepath.Dir(installPath)
		if err := rejectTransactionMaterial(parent); err != nil {
			return err
		}
		previousPath := filepath.Join(parent, previousSlotName)
		previousSlotExists := false
		if _, statErr := os.Lstat(previousPath); statErr == nil {
			if _, inspectErr := inspectPreviousSlot(previousPath, o, existing); inspectErr != nil {
				return errors.New("existing previous release slot mismatch")
			}
			if !sameReleaseFilesystem(installPath, previousPath) {
				return errors.New("existing previous release slot mismatch")
			}
			previousSlotExists = true
		} else if !os.IsNotExist(statErr) {
			return errors.New("existing previous release slot mismatch")
		}
		return upgradeInstall(o, installPath, existing, archives, previousSlotExists)
	}
	if o.Upgrade {
		return errors.New("upgrade requires an existing installation")
	}
	failureStage = "host"
	if o.Root == "" {
		if err := requireTrustedAncestor("/opt"); err != nil {
			return err
		}
		if err := requireTrustedAncestor("/etc/systemd/system"); err != nil {
			return err
		}
	}

	uid, gid := 0, 0
	var accountState AccountState
	accountPublished := false
	failureStage = "account"
	if !o.RunAsRoot {
		if o.Account == nil {
			return errors.New("dedicated account manager is required")
		}
		accountState, err = o.Account.Ensure("agent-forge", prefix)
		uid, gid = accountState.UID, accountState.GID
		if err != nil || uid <= 0 || gid <= 0 {
			return errors.New("dedicated account is not exact")
		}
		defer func() {
			if !accountPublished && (accountState.UserCreated || accountState.GroupCreated) {
				if rollbackErr := o.Account.Rollback("agent-forge", accountState); rollbackErr != nil {
					retErr = errors.Join(retErr, errors.New("created account rollback failed"))
				}
			}
		}()
	}
	failureStage = "staging"
	parent := filepath.Dir(installPath)
	if err := mkdirPathNoSymlinks(o.Root, filepath.Dir(prefix), 0o755); err != nil {
		return err
	}
	stage, err := os.MkdirTemp(parent, ".agent-forge.install-")
	if err != nil {
		return fmt.Errorf("create private staging: %w", err)
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}

	for _, dir := range []struct {
		name string
		mode fs.FileMode
	}{
		{"bin", 0o755}, {"etc", 0o750}, {"secrets", 0o700}, {"var", 0o755}, {"var/gate", 0o700}, {"var/gate/state", 0o700},
		{"var/repositories", 0o700}, {"var/worker", 0o700}, {"var/worker/worktrees", 0o700}, {"var/worker/runtime", 0o700}, {"systemd", 0o755},
	} {
		if err := os.Mkdir(filepath.Join(stage, dir.name), dir.mode); err != nil {
			return err
		}
	}
	for _, d := range []string{"var/gate", "var/gate/state", "var/repositories", "var/worker", "var/worker/worktrees", "var/worker/runtime"} {
		if err := o.Ownership.Chown(filepath.Join(stage, d), uid, gid); err != nil {
			return err
		}
	}
	if !o.RunAsRoot {
		if err := o.Ownership.Chown(filepath.Join(stage, "etc"), 0, gid); err != nil {
			return err
		}
	}

	for _, a := range archives {
		if err := extractArchive(a, filepath.Join(stage, "bin"), o.Version, o.Commit); err != nil {
			return err
		}
	}
	ownerToken, workerToken, err := tokens(o.Random)
	if err != nil {
		return err
	}
	paths := runtimePaths(o.Root)
	gateConfig := fmt.Sprintf(`{"version":1,"listen":"127.0.0.1:18080","database":%q,"owner_token_env":"FORGE_OWNER_TOKEN","recovery_interval":"1s","lease_poll_interval":"100ms","default_pool":"default","lifecycle":{"lease_ttl":"30s","retry_base":"1s","max_attempts":3},"default_execution":{"plugin_id":"reference","environment":[],"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576},"workers":[{"id":"worker-1","pool":"default","token_env":"FORGE_WORKER_TOKEN","concurrency":1}],"repositories":[]}`, paths.state+"/forge.db") + "\n"
	workerConfig := fmt.Sprintf(`{"version":1,"gate_url":"ws://127.0.0.1:18080","id":"worker-1","token_env":"FORGE_WORKER_TOKEN","heartbeat_interval":"10s","concurrency":1,"repository_roots":[%q],"worktree_root":%q,"runtime_root":%q,"repositories":[],"plugins":[{"id":"reference","argv":[%q]}],"environment_allowlist":[],"check_environment_allowlist":[],"ceilings":{"plugin_timeout":"15m","check_timeout":"10m","git_timeout":"1m","cleanup_timeout":"10s","plugin_output_bytes":1048576,"check_output_bytes":2048,"git_output_bytes":1048576}}`, paths.repositories, paths.worktrees, paths.runtime, paths.bin+"/forge-ref-plugin") + "\n"
	if err = writeExclusive(filepath.Join(stage, "etc/gate.json"), []byte(gateConfig), 0o440); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(stage, "etc/worker.json"), []byte(workerConfig), 0o440); err != nil {
		return err
	}
	if !o.RunAsRoot {
		for _, name := range []string{"etc/gate.json", "etc/worker.json"} {
			if err = o.Ownership.Chown(filepath.Join(stage, name), 0, gid); err != nil {
				return err
			}
		}
	}
	if err = writeExclusive(filepath.Join(stage, "secrets/gate.env"), []byte("FORGE_OWNER_TOKEN="+ownerToken+"\nFORGE_WORKER_TOKEN="+workerToken+"\n"), 0o600); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(stage, "secrets/worker.env"), []byte("FORGE_WORKER_TOKEN="+workerToken+"\n"), 0o600); err != nil {
		return err
	}
	user := "agent-forge"
	if o.RunAsRoot {
		user = "root"
	}
	gateUnit, workerUnit := units(user, paths)
	if err = writeExclusive(filepath.Join(stage, "systemd/agent-forge-gate.service"), []byte(gateUnit), 0o644); err != nil {
		return err
	}
	if err = writeExclusive(filepath.Join(stage, "systemd/agent-forge-worker.service"), []byte(workerUnit), 0o644); err != nil {
		return err
	}
	r := receipt{Version: o.Version, Commit: o.Commit, Architecture: o.Arch, ArchiveSHA256: map[string]string{}, OwnerTokenSHA256: hashString(ownerToken), WorkerTokenSHA256: hashString(workerToken), RunAsRoot: o.RunAsRoot, AccountUID: uid, AccountGID: gid, StoreSchemaVersion: storeSchemaVersion}
	for _, a := range archives {
		r.ArchiveSHA256[a.name] = a.digest
	}
	r.Files = map[string]string{}
	r.Modes = map[string]uint32{}
	r.Directories = directoryContracts(uid, gid)
	for _, name := range immutableFiles() {
		fileBody, readErr := os.ReadFile(filepath.Join(stage, name))
		if readErr != nil {
			return readErr
		}
		info, statErr := os.Lstat(filepath.Join(stage, name))
		if statErr != nil {
			return statErr
		}
		r.Files[name] = hashBytes(fileBody)
		r.Modes[name] = uint32(info.Mode().Perm())
	}
	body, _ := json.MarshalIndent(r, "", "  ")
	body = append(body, '\n')
	if err = writeExclusive(filepath.Join(stage, "install-receipt.json"), body, 0o400); err != nil {
		return err
	}
	if err = os.Chmod(stage, 0o755); err != nil {
		return err
	}
	failureStage = "publication"
	if err = unix.Renameat2(unix.AT_FDCWD, stage, unix.AT_FDCWD, installPath, unix.RENAME_NOREPLACE); err != nil {
		return fmt.Errorf("publish without replacement: %w", err)
	}
	published = true
	accountPublished = true
	if err = ensureLinks(o.Root); err != nil {
		return err
	}
	failureStage = "activation"
	return activate(o, ownerToken)
}

func validate(o *Options) error {
	if !versionRE.MatchString(o.Version) || !commitRE.MatchString(o.Commit) || !digestRE.MatchString(o.SHA256SUMSSHA256) || !filepath.IsAbs(o.AssetDir) {
		return errors.New("invalid installer arguments")
	}
	if o.Root != "" && !filepath.IsAbs(o.Root) {
		return errors.New("test root must be absolute")
	}
	if o.Upgrade && (o.InstallerVersion != o.Version || o.InstallerCommit != o.Commit) {
		return errors.New("upgrade requires the exact target forge CLI")
	}
	if o.Arch == "" {
		o.Arch = runtime.GOARCH
	}
	if o.Arch != "amd64" && o.Arch != "arm64" {
		return errors.New("unsupported Linux architecture")
	}
	if o.Random == nil {
		o.Random = rand.Reader
	}
	if o.Ownership == nil {
		o.Ownership = HostOwnershipManager{}
	}
	return nil
}

func verifyAssets(o Options) ([]verifiedArchive, error) {
	st, err := os.Lstat(o.AssetDir)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("asset directory must be a real directory")
	}
	manifestPath := filepath.Join(o.AssetDir, "SHA256SUMS")
	body, err := readNoFollow(manifestPath, 1<<20)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(body)
	if hex.EncodeToString(sum[:]) != o.SHA256SUMSSHA256 {
		return nil, errors.New("SHA256SUMS trust anchor mismatch")
	}
	entries, err := parseManifest(body, o.Version)
	if err != nil {
		return nil, err
	}
	var out []verifiedArchive
	complete := false
	defer func() {
		if !complete {
			for _, archive := range out {
				_ = os.Remove(archive.file)
			}
		}
	}()
	for _, role := range []string{"cli", "gate", "worker"} {
		name := fmt.Sprintf("agent-forge-%s_%s_linux_%s.tar.gz", role, o.Version, o.Arch)
		want := entries[name]
		src, err := openNoFollow(filepath.Join(o.AssetDir, name))
		if err != nil {
			return nil, err
		}
		tmp, err := os.CreateTemp("", "agent-forge-verified-*.tar.gz")
		if err != nil {
			src.Close()
			return nil, err
		}
		h := sha256.New()
		n, copyErr := io.Copy(io.MultiWriter(tmp, h), io.LimitReader(src, 256<<20+1))
		closeErr := src.Close()
		tmpErr := tmp.Close()
		if copyErr != nil || closeErr != nil || tmpErr != nil || n > 256<<20 {
			os.Remove(tmp.Name())
			return nil, errors.New("copy verified archive failed")
		}
		got := hex.EncodeToString(h.Sum(nil))
		if got != want {
			os.Remove(tmp.Name())
			return nil, errors.New("selected archive digest mismatch")
		}
		out = append(out, verifiedArchive{role: role, name: name, file: tmp.Name(), digest: got})
	}
	complete = true
	return out, nil
}

func parseManifest(body []byte, version string) (map[string]string, error) {
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	if len(lines) != 6 {
		return nil, errors.New("manifest must contain exact release matrix")
	}
	m := map[string]string{}
	last := ""
	for _, line := range lines {
		if len(line) < 67 || line[64:66] != "  " {
			return nil, errors.New("invalid manifest line")
		}
		d, n := line[:64], line[66:]
		if !digestRE.MatchString(d) || n <= last || path.Base(n) != n || !regexp.MustCompile(`^agent-forge-(cli|gate|worker)_`+regexp.QuoteMeta(version)+`_linux_(amd64|arm64)\.tar\.gz$`).MatchString(n) {
			return nil, errors.New("invalid manifest entry")
		}
		if _, ok := m[n]; ok {
			return nil, errors.New("duplicate manifest entry")
		}
		m[n] = d
		last = n
	}
	expected := []string{}
	for _, r := range []string{"cli", "gate", "worker"} {
		for _, a := range []string{"amd64", "arm64"} {
			expected = append(expected, fmt.Sprintf("agent-forge-%s_%s_linux_%s.tar.gz", r, version, a))
		}
	}
	sort.Strings(expected)
	for _, n := range expected {
		if _, ok := m[n]; !ok {
			return nil, errors.New("manifest release matrix mismatch")
		}
	}
	return m, nil
}

func validateArchives(archives []verifiedArchive, version, commit string) error {
	stage, err := os.MkdirTemp("", "agent-forge-archive-preflight-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	if err := os.Chmod(stage, 0o700); err != nil {
		return err
	}
	for _, archive := range archives {
		if err := extractArchive(archive, stage, version, commit); err != nil {
			return err
		}
	}
	return verifyBinaryIdentities(stage, version, commit)
}

func verifyBinaryIdentities(binDir, version, commit string) error {
	for _, name := range []string{"forge", "forge-gate", "forge-worker", "forge-codex-plugin", "forge-ref-plugin"} {
		want := name + " " + version + " " + commit + "\n"
		if err := verifyBinaryIdentity(filepath.Join(binDir, name), want, 2*time.Second); err != nil {
			return errors.New("binary identity mismatch")
		}
	}
	return nil
}

type identityOutput struct {
	mu       sync.Mutex
	b        bytes.Buffer
	limit    int
	overflow func()
}

func (w *identityOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.b.Len()+len(p) > w.limit {
		if remaining := w.limit - w.b.Len(); remaining > 0 {
			_, _ = w.b.Write(p[:remaining])
		}
		w.overflow()
		return 0, errors.New("identity output limit exceeded")
	}
	return w.b.Write(p)
}

func verifyBinaryIdentity(binary, want string, timeout time.Duration) error {
	cmd := exec.Command(binary, "--version")
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	var once sync.Once
	kill := func() {
		once.Do(func() {
			if cmd.Process != nil {
				_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			}
		})
	}
	output := &identityOutput{limit: 256, overflow: kill}
	cmd.Stdout, cmd.Stderr = output, output
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var err error
	select {
	case err = <-done:
	case <-timer.C:
		kill()
		select {
		case err = <-done:
		case <-time.After(time.Second):
			return errors.New("binary identity wait timeout")
		}
	}
	output.mu.Lock()
	got := output.b.String()
	output.mu.Unlock()
	if err != nil || got != want {
		return errors.New("binary identity mismatch")
	}
	return nil
}

func extractArchive(a verifiedArchive, binDir, version, commit string) error {
	f, err := os.Open(a.file)
	if err != nil {
		return err
	}
	defer f.Close()
	buffered := bufio.NewReader(f)
	gz, err := gzip.NewReader(buffered)
	if err != nil {
		return err
	}
	gz.Multistream(false)
	defer gz.Close()
	tr := tar.NewReader(gz)
	root := strings.TrimSuffix(a.name, ".tar.gz")
	allowed := map[string]fs.FileMode{"VERSION": 0o644}
	switch a.role {
	case "cli":
		allowed["forge"] = 0o755
	case "gate":
		allowed["forge-gate"] = 0o755
	case "worker":
		allowed["forge-worker"] = 0o755
		allowed["forge-ref-plugin"] = 0o755
		allowed["forge-codex-plugin"] = 0o755
	}
	seen := map[string]bool{}
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return e
		}
		if h.Format != tar.FormatUSTAR || h.PAXRecords != nil || h.Xattrs != nil || h.Linkname != "" || path.Clean(h.Name) != h.Name && h.Name != root+"/" || strings.HasPrefix(h.Name, "/") || strings.Contains(h.Name, "\\") {
			return errors.New("unsafe archive metadata")
		}
		if h.Name == root+"/" {
			if seen[""] || h.Typeflag != tar.TypeDir || h.Mode != 0o755 {
				return errors.New("invalid archive root")
			}
			seen[""] = true
			continue
		}
		prefixName := root + "/"
		if !strings.HasPrefix(h.Name, prefixName) {
			return errors.New("archive member outside root")
		}
		name := strings.TrimPrefix(h.Name, prefixName)
		mode, ok := allowed[name]
		if !ok || seen[name] || h.Typeflag != tar.TypeReg || h.Mode != int64(mode) || h.Size < 0 || h.Size > 128<<20 {
			return errors.New("unexpected archive member")
		}
		seen[name] = true
		data, e := io.ReadAll(io.LimitReader(tr, h.Size+1))
		if e != nil || int64(len(data)) != h.Size {
			return errors.New("invalid archive body")
		}
		if name == "VERSION" {
			if string(data) != "version="+version+"\ncommit="+commit+"\n" {
				return errors.New("archive VERSION mismatch")
			}
			continue
		}
		if e = writeExclusive(filepath.Join(binDir, name), data, mode); e != nil {
			return e
		}
	}
	trailing, err := io.ReadAll(io.LimitReader(gz, (1<<20)+1))
	if err != nil || len(trailing) > 1<<20 {
		return errors.New("invalid archive trailing data")
	}
	for _, value := range trailing {
		if value != 0 {
			return errors.New("invalid archive trailing data")
		}
	}
	if _, err := buffered.ReadByte(); err != io.EOF {
		return errors.New("archive must contain one gzip stream")
	}
	if !seen[""] || len(seen) != len(allowed)+1 {
		return errors.New("archive members missing")
	}
	return nil
}

func tokens(r io.Reader) (string, string, error) {
	b := make([]byte, 64)
	if _, e := io.ReadFull(r, b); e != nil {
		return "", "", e
	}
	a := base64.RawURLEncoding.EncodeToString(b[:32])
	c := base64.RawURLEncoding.EncodeToString(b[32:])
	if a == c {
		return "", "", errors.New("random token collision")
	}
	return a, c, nil
}
func writeExclusive(name string, b []byte, mode fs.FileMode) error {
	f, e := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if e != nil {
		return e
	}
	if _, e = f.Write(b); e == nil {
		e = f.Sync()
	}
	if c := f.Close(); e == nil {
		e = c
	}
	return e
}
func readNoFollow(name string, max int64) ([]byte, error) {
	f, e := openNoFollow(name)
	if e != nil {
		return nil, e
	}
	defer f.Close()
	b, e := io.ReadAll(io.LimitReader(f, max+1))
	if e != nil || int64(len(b)) > max {
		return nil, errors.New("file too large")
	}
	return b, nil
}
func openNoFollow(name string) (*os.File, error) {
	fd, e := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), name), nil
}
func rooted(root, p string) string {
	if root == "" {
		return p
	}
	return filepath.Join(root, p)
}

type paths struct{ bin, state, repositories, worktrees, runtime, etc, secrets string }

func runtimePaths(root string) paths {
	return paths{rooted(root, prefix+"/bin"), rooted(root, prefix+"/var/gate/state"), rooted(root, prefix+"/var/repositories"), rooted(root, prefix+"/var/worker/worktrees"), rooted(root, prefix+"/var/worker/runtime"), rooted(root, prefix+"/etc"), rooted(root, prefix+"/secrets")}
}
func hashString(s string) string { return hashBytes([]byte(s)) }
func hashBytes(b []byte) string {
	x := sha256.Sum256(b)
	return hex.EncodeToString(x[:])
}
func immutableFiles() []string {
	return []string{"bin/forge", "bin/forge-gate", "bin/forge-worker", "bin/forge-codex-plugin", "bin/forge-ref-plugin", "etc/gate.json", "etc/worker.json", "secrets/gate.env", "secrets/worker.env", "systemd/agent-forge-gate.service", "systemd/agent-forge-worker.service"}
}

func immutableModes() map[string]uint32 {
	return map[string]uint32{
		"bin/forge": 0o755, "bin/forge-gate": 0o755, "bin/forge-worker": 0o755,
		"bin/forge-codex-plugin": 0o755, "bin/forge-ref-plugin": 0o755,
		"etc/gate.json": 0o440, "etc/worker.json": 0o440,
		"secrets/gate.env": 0o600, "secrets/worker.env": 0o600,
		"systemd/agent-forge-gate.service": 0o644, "systemd/agent-forge-worker.service": 0o644,
	}
}

func directoryContracts(uid, gid int) map[string]directoryState {
	serviceUID, serviceGID := uint32(uid), uint32(gid)
	return map[string]directoryState{
		"":                     {Mode: 0o755},
		"bin":                  {Mode: 0o755},
		"etc":                  {Mode: 0o750, GID: serviceGID},
		"secrets":              {Mode: 0o700},
		"var":                  {Mode: 0o755},
		"var/gate":             {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"var/gate/state":       {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"var/repositories":     {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"var/worker":           {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"var/worker/worktrees": {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"var/worker/runtime":   {Mode: 0o700, UID: serviceUID, GID: serviceGID},
		"systemd":              {Mode: 0o755},
	}
}

func ownedBy(manager OwnershipManager, name string, uid, gid uint32) bool {
	gotUID, gotGID, err := manager.Owner(name)
	return err == nil && gotUID == int(uid) && gotGID == int(gid)
}

func singlyLinked(info fs.FileInfo) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	return ok && stat.Nlink == 1
}
func mkdirPathNoSymlinks(root, p string, mode fs.FileMode) error {
	cur := root
	if cur == "" {
		cur = "/"
	}
	for _, part := range strings.Split(strings.TrimPrefix(p, "/"), "/") {
		cur = filepath.Join(cur, part)
		st, e := os.Lstat(cur)
		if os.IsNotExist(e) {
			if e = os.Mkdir(cur, mode); e != nil {
				return e
			}
			continue
		}
		if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
			return errors.New("unsafe destination ancestor")
		}
	}
	return nil
}

func requireTrustedAncestor(name string) error {
	current := "/"
	for _, part := range strings.Split(strings.TrimPrefix(name, "/"), "/") {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
			return errors.New("unsafe privileged destination ancestor")
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || stat.Uid != 0 {
			return errors.New("untrusted privileged destination owner")
		}
	}
	return nil
}

func ensureLinks(root string) error {
	if e := mkdirPathNoSymlinks(root, "/etc/systemd/system", 0o755); e != nil {
		return e
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		link := rooted(root, "/etc/systemd/system/"+name)
		target := prefix + "/systemd/" + name
		if root != "" {
			target = rooted(root, target)
		}
		e := os.Symlink(target, link)
		if e == nil {
			continue
		}
		if !os.IsExist(e) {
			return e
		}
		got, x := os.Readlink(link)
		if x != nil || got != target {
			return errors.New("existing unit link mismatch")
		}
	}
	return nil
}
func validateLinks(root string) error {
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		link := rooted(root, "/etc/systemd/system/"+name)
		target := prefix + "/systemd/" + name
		if root != "" {
			target = rooted(root, target)
		}
		info, err := os.Lstat(link)
		if err != nil || info.Mode()&os.ModeSymlink == 0 || !singlyLinked(info) {
			return errors.New("existing unit link missing")
		}
		got, err := os.Readlink(link)
		if err != nil || got != target {
			return errors.New("existing unit link mismatch")
		}
	}
	return nil
}
func inspectExistingInstall(p string, o Options) (*receipt, error) {
	st, e := os.Lstat(p)
	if os.IsNotExist(e) {
		return nil, nil
	}
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return nil, errors.New("existing install is unsafe")
	}
	receiptPath := filepath.Join(p, "install-receipt.json")
	receiptInfo, receiptStatErr := os.Lstat(receiptPath)
	if receiptStatErr != nil || !receiptInfo.Mode().IsRegular() || !singlyLinked(receiptInfo) || receiptInfo.Mode().Perm() != 0o400 || !ownedBy(o.Ownership, receiptPath, 0, 0) {
		return nil, errors.New("existing install receipt metadata mismatch")
	}
	body, e := readNoFollow(receiptPath, 1<<20)
	if e != nil {
		return nil, errors.New("existing install has no valid receipt")
	}
	var r receipt
	if configjson.Decode(body, &r) != nil || !versionRE.MatchString(r.Version) || !commitRE.MatchString(r.Commit) || r.Architecture != o.Arch || r.RunAsRoot != o.RunAsRoot || !digestRE.MatchString(r.OwnerTokenSHA256) || !digestRE.MatchString(r.WorkerTokenSHA256) {
		return nil, errors.New("existing install receipt mismatch")
	}
	var rawReceipt map[string]json.RawMessage
	if json.Unmarshal(body, &rawReceipt) != nil {
		return nil, errors.New("existing install receipt mismatch")
	}
	_, schemaMarkerPresent := rawReceipt["store_schema_version"]
	if !schemaMarkerPresent && r.Version == legacyInstallerVersion && r.Commit == legacyInstallerCommit {
		r.StoreSchemaVersion = storeSchemaVersion
	}
	if !schemaMarkerPresent && (r.Version != legacyInstallerVersion || r.Commit != legacyInstallerCommit) || r.StoreSchemaVersion != storeSchemaVersion {
		return nil, errors.New("existing store schema is not upgrade-compatible")
	}
	if r.AccountUID < 0 || r.AccountGID < 0 || o.RunAsRoot && (r.AccountUID != 0 || r.AccountGID != 0) || !o.RunAsRoot && (r.AccountUID <= 0 || r.AccountGID <= 0) {
		return nil, errors.New("existing account receipt mismatch")
	}
	if len(r.ArchiveSHA256) != 3 || len(r.Files) != len(immutableFiles()) || len(r.Modes) != len(immutableFiles()) {
		return nil, errors.New("existing receipt key set mismatch")
	}
	for _, role := range []string{"cli", "gate", "worker"} {
		name := fmt.Sprintf("agent-forge-%s_%s_linux_%s.tar.gz", role, r.Version, r.Architecture)
		if !digestRE.MatchString(r.ArchiveSHA256[name]) {
			return nil, errors.New("existing archive receipt mismatch")
		}
	}
	wantDirectories := directoryContracts(r.AccountUID, r.AccountGID)
	if len(r.Directories) != len(wantDirectories) {
		return nil, errors.New("existing directory receipt mismatch")
	}
	for name, want := range wantDirectories {
		if r.Directories[name] != want {
			return nil, errors.New("existing directory receipt mismatch")
		}
		namePath := filepath.Join(p, name)
		info, statErr := os.Lstat(namePath)
		if statErr != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || uint32(info.Mode().Perm()) != want.Mode || !ownedBy(o.Ownership, namePath, want.UID, want.GID) {
			return nil, errors.New("existing directory mismatch")
		}
	}
	wantModes := immutableModes()
	for _, name := range immutableFiles() {
		wantMode := wantModes[name]
		if _, ok := r.Files[name]; !ok {
			return nil, errors.New("existing file receipt key missing")
		}
		if _, ok := r.Modes[name]; !ok {
			return nil, errors.New("existing mode receipt key missing")
		}
		info, statErr := os.Lstat(filepath.Join(p, name))
		uid, gid := uint32(0), uint32(0)
		if !r.RunAsRoot && (name == "etc/gate.json" || name == "etc/worker.json") {
			gid = uint32(r.AccountGID)
		}
		if statErr != nil || !info.Mode().IsRegular() || !singlyLinked(info) || info.Mode()&os.ModeSymlink != 0 || r.Modes[name] != wantMode || uint32(info.Mode().Perm()) != wantMode || !ownedBy(o.Ownership, filepath.Join(p, name), uid, gid) {
			return nil, errors.New("existing immutable object mismatch")
		}
		fileBody, readErr := os.ReadFile(filepath.Join(p, name))
		if readErr != nil || hashBytes(fileBody) != r.Files[name] {
			return nil, errors.New("existing immutable digest mismatch")
		}
	}
	gateSecrets, secretErr := readExactSecrets(filepath.Join(p, "secrets/gate.env"), true)
	if secretErr != nil {
		return nil, errors.New("existing Gate secrets mismatch")
	}
	workerSecrets, secretErr := readExactSecrets(filepath.Join(p, "secrets/worker.env"), false)
	if secretErr != nil || gateSecrets["FORGE_WORKER_TOKEN"] != workerSecrets["FORGE_WORKER_TOKEN"] || hashString(gateSecrets["FORGE_OWNER_TOKEN"]) != r.OwnerTokenSHA256 || hashString(gateSecrets["FORGE_WORKER_TOKEN"]) != r.WorkerTokenSHA256 {
		return nil, errors.New("existing secret receipt mismatch")
	}
	return &r, nil
}

func sameRelease(r *receipt, o Options, archives []verifiedArchive) bool {
	if r.Version != o.Version || r.Commit != o.Commit || len(r.ArchiveSHA256) != len(archives) {
		return false
	}
	for _, archive := range archives {
		if r.ArchiveSHA256[archive.name] != archive.digest {
			return false
		}
	}
	return true
}

func compareVersions(a, b string) int {
	aParts := strings.Split(strings.TrimPrefix(a, "v"), ".")
	bParts := strings.Split(strings.TrimPrefix(b, "v"), ".")
	for i := 0; i < 3; i++ {
		if len(aParts[i]) < len(bParts[i]) {
			return -1
		}
		if len(aParts[i]) > len(bParts[i]) {
			return 1
		}
		if aParts[i] < bParts[i] {
			return -1
		}
		if aParts[i] > bParts[i] {
			return 1
		}
	}
	return 0
}

func readExactSecrets(name string, gate bool) (map[string]string, error) {
	body, err := readNoFollow(name, 4096)
	if err != nil || len(body) == 0 || body[len(body)-1] != '\n' {
		return nil, errors.New("invalid secret file")
	}
	lines := strings.Split(strings.TrimSuffix(string(body), "\n"), "\n")
	want := []string{"FORGE_WORKER_TOKEN"}
	if gate {
		want = []string{"FORGE_OWNER_TOKEN", "FORGE_WORKER_TOKEN"}
	}
	if len(lines) != len(want) {
		return nil, errors.New("invalid secret file")
	}
	result := map[string]string{}
	for i, key := range want {
		prefix := key + "="
		if !strings.HasPrefix(lines[i], prefix) {
			return nil, errors.New("invalid secret file")
		}
		value := strings.TrimPrefix(lines[i], prefix)
		decoded, decodeErr := base64.RawURLEncoding.DecodeString(value)
		if decodeErr != nil || len(decoded) != 32 || base64.RawURLEncoding.EncodeToString(decoded) != value {
			return nil, errors.New("invalid secret token")
		}
		result[key] = value
	}
	return result, nil
}
func activate(o Options, ownerToken string) error {
	if o.Services == nil || !o.EnableNow {
		return nil
	}
	if e := o.Services.Run("daemon-reload"); e != nil {
		return e
	}
	if e := o.Services.Run("enable", "--now", "agent-forge-gate.service"); e != nil {
		return e
	}
	if e := o.Services.GateReady(ownerToken, o.Version, o.Commit); e != nil {
		return e
	}
	if e := o.Services.Run("enable", "--now", "agent-forge-worker.service"); e != nil {
		return e
	}
	return o.Services.WorkerReady(ownerToken)
}

func existingOwnerToken(installPath string) (string, error) {
	body, err := readNoFollow(filepath.Join(installPath, "secrets/gate.env"), 4096)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.HasPrefix(line, "FORGE_OWNER_TOKEN=") && len(line) > len("FORGE_OWNER_TOKEN=") {
			return strings.TrimPrefix(line, "FORGE_OWNER_TOKEN="), nil
		}
	}
	return "", errors.New("existing owner token missing")
}
func units(user string, p paths) (string, string) {
	common := `Type=simple
User=` + user + `
Group=` + user + `
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
UMask=0077
PrivateTmp=yes
PrivateDevices=yes
ProtectSystem=strict
ProtectHome=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectControlGroups=yes
ProtectClock=yes
ProtectHostname=yes
LockPersonality=yes
RestrictSUIDSGID=yes
RestrictRealtime=yes
RestrictAddressFamilies=AF_UNIX AF_INET AF_INET6
ProtectProc=invisible
ProcSubset=pid
Restart=on-failure
RestartSec=2s
`
	gate := "[Unit]\nDescription=Agent Forge Gate\nAfter=network.target\n\n[Service]\n" + common + "EnvironmentFile=" + p.secrets + "/gate.env\nExecStart=" + p.bin + "/forge-gate -config " + p.etc + "/gate.json\nReadWritePaths=" + p.state + " " + p.repositories + "\n\n[Install]\nWantedBy=multi-user.target\n"
	worker := "[Unit]\nDescription=Agent Forge Worker\nRequires=agent-forge-gate.service\nAfter=agent-forge-gate.service\n\n[Service]\n" + common + "EnvironmentFile=" + p.secrets + "/worker.env\nExecStart=" + p.bin + "/forge-worker -config " + p.etc + "/worker.json\nReadWritePaths=" + p.repositories + " " + p.worktrees + " " + p.runtime + "\n\n[Install]\nWantedBy=multi-user.target\n"
	return gate, worker
}
