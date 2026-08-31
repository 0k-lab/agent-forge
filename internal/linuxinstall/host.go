//go:build linux

package linuxinstall

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// HostOwnershipManager applies and verifies Unix ownership.
type HostOwnershipManager struct{}

func (HostOwnershipManager) Chown(name string, uid, gid int) error {
	return os.Chown(name, uid, gid)
}

func (HostOwnershipManager) Owner(name string) (int, int, error) {
	info, err := os.Lstat(name)
	if err != nil {
		return 0, 0, err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, 0, errors.New("ownership unavailable")
	}
	return int(st.Uid), int(st.Gid), nil
}

// HostAccountManager creates or validates the one supported dedicated system account.
type HostAccountManager struct{}

const (
	groupaddPath            = "/usr/sbin/groupadd"
	groupdelPath            = "/usr/sbin/groupdel"
	useraddPath             = "/usr/sbin/useradd"
	userdelPath             = "/usr/sbin/userdel"
	getentPath              = "/usr/bin/getent"
	systemctlPath           = "/usr/bin/systemctl"
	serviceStateOutputLimit = 1024
)

var (
	gateBaseURL     = "http://127.0.0.1:18080"
	readinessWindow = 30 * time.Second
)

func privilegedCommand(name string, argv ...string) *exec.Cmd {
	cmd := exec.Command(name, argv...)
	cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin", "LANG=C", "LC_ALL=C"}
	return cmd
}

func runPrivileged(name string, argv ...string) error {
	cmd := privilegedCommand(name, argv...)
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	return cmd.Run()
}

func (HostAccountManager) Ensure(name, home string) (AccountState, error) {
	if name != "agent-forge" || home != prefix {
		return AccountState{}, errors.New("unsupported account contract")
	}
	state := AccountState{}
	if _, err := user.LookupGroup(name); err != nil {
		var unknown user.UnknownGroupError
		if !errors.As(err, &unknown) {
			return AccountState{}, errors.New("service group lookup failed")
		}
		if runErr := runPrivileged(groupaddPath, "--system", name); runErr != nil {
			return AccountState{}, errors.New("create service group failed")
		}
		state.GroupCreated = true
	}
	if _, err := user.Lookup(name); err != nil {
		var unknown user.UnknownUserError
		if !errors.As(err, &unknown) {
			cleanupErr := (HostAccountManager{}).Rollback(name, state)
			return AccountState{}, joinCleanup(errors.New("service account lookup failed"), cleanupErr)
		}
		if runErr := runPrivileged(useraddPath, "--system", "--gid", name, "--home-dir", home, "--shell", "/usr/sbin/nologin", "--no-create-home", name); runErr != nil {
			var cleanupErr error
			if state.GroupCreated {
				cleanupErr = runPrivileged(groupdelPath, name)
			}
			return AccountState{}, joinCleanup(errors.New("create service account failed"), cleanupErr)
		}
		state.UserCreated = true
	}
	uid, gid, err := (HostAccountManager{}).Validate(name, home)
	state.UID, state.GID = uid, gid
	if err != nil {
		cleanupErr := (HostAccountManager{}).Rollback(name, state)
		return AccountState{}, joinCleanup(err, cleanupErr)
	}
	return state, nil
}

func joinCleanup(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return errors.Join(primary, errors.New("created identity cleanup failed"))
}

func (HostAccountManager) Rollback(name string, state AccountState) error {
	var first error
	if state.UserCreated {
		first = runPrivileged(userdelPath, name)
	}
	if state.GroupCreated {
		if err := runPrivileged(groupdelPath, name); first == nil {
			first = err
		}
	}
	return first
}

// Validate checks the account contract without creating or modifying identity state.
func (HostAccountManager) Validate(name, home string) (int, int, error) {
	if name != "agent-forge" || home != prefix {
		return 0, 0, errors.New("unsupported account contract")
	}
	u, err := user.Lookup(name)
	if err != nil || u.HomeDir != home {
		return 0, 0, errors.New("service account mismatch")
	}
	group, groupErr := user.LookupGroup(name)
	uid, uidErr := strconv.Atoi(u.Uid)
	gid, gidErr := strconv.Atoi(u.Gid)
	groups, groupsErr := u.GroupIds()
	if uidErr != nil || gidErr != nil || groupErr != nil || group.Gid != u.Gid || groupsErr != nil || uid <= 0 || gid <= 0 || len(groups) != 1 || groups[0] != u.Gid {
		return 0, 0, errors.New("service account groups mismatch")
	}
	cmd := privilegedCommand(getentPath, "passwd", name)
	record, recordErr := cmd.Output()
	if recordErr != nil || validateServiceAccountRecord(record, name, home, u.Gid) != nil {
		return 0, 0, errors.New("service account record mismatch")
	}
	return uid, gid, nil
}

func validateServiceAccountRecord(body []byte, name, home, gid string) error {
	line := strings.TrimSuffix(string(body), "\n")
	if line == string(body) || strings.Contains(line, "\n") {
		return errors.New("invalid service account record")
	}
	fields := strings.Split(line, ":")
	if len(fields) != 7 || fields[0] != name || fields[2] == "" || fields[3] != gid || fields[5] != home || fields[6] != "/usr/sbin/nologin" {
		return errors.New("invalid service account record")
	}
	uid, err := strconv.Atoi(fields[2])
	if err != nil || uid <= 0 {
		return errors.New("invalid service account record")
	}
	return nil
}

// HostServiceManager invokes systemctl with an argument vector and no shell.
type HostServiceManager struct{}

type boundedOutput struct {
	bytes.Buffer
	overflow bool
}

func (b *boundedOutput) Write(p []byte) (int, error) {
	written := len(p)
	remaining := serviceStateOutputLimit - b.Len()
	if len(p) > remaining {
		p = p[:max(remaining, 0)]
		b.overflow = true
	}
	_, err := b.Buffer.Write(p)
	return written, err
}

var systemctlStateOutput = func(argv ...string) ([]byte, error) {
	cmd := privilegedCommand(systemctlPath, argv...)
	var output boundedOutput
	cmd.Stdout, cmd.Stderr = &output, io.Discard
	if err := cmd.Run(); err != nil || output.overflow {
		return nil, errors.New("systemctl state query failed")
	}
	return output.Bytes(), nil
}

func (HostServiceManager) Run(argv ...string) error {
	if len(argv) == 0 {
		return errors.New("empty systemctl invocation")
	}
	if err := runPrivileged(systemctlPath, argv...); err != nil {
		return errors.New("systemctl operation failed")
	}
	return nil
}

func (HostServiceManager) State(unit string) (ServiceState, error) {
	if unit != "agent-forge-gate.service" && unit != "agent-forge-worker.service" {
		return ServiceState{}, errors.New("unsupported service state query")
	}
	body, err := systemctlStateOutput("show", "--no-pager", "--property=LoadState", "--property=ActiveState", "--property=UnitFileState", unit)
	if err != nil || len(body) == 0 || len(body) > serviceStateOutputLimit {
		return ServiceState{}, errors.New("systemctl state query failed")
	}
	values := map[string]string{}
	for _, line := range strings.Split(strings.TrimSuffix(string(body), "\n"), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok || key != "LoadState" && key != "ActiveState" && key != "UnitFileState" {
			return ServiceState{}, errors.New("invalid systemctl state")
		}
		if _, duplicate := values[key]; duplicate {
			return ServiceState{}, errors.New("invalid systemctl state")
		}
		values[key] = value
	}
	if len(values) != 3 {
		return ServiceState{}, errors.New("invalid systemctl state")
	}
	if values["LoadState"] == "not-found" && values["ActiveState"] == "inactive" && values["UnitFileState"] == "" {
		return ServiceState{}, nil
	}
	if values["LoadState"] != "loaded" {
		return ServiceState{}, errors.New("ambiguous systemctl state")
	}
	state := ServiceState{}
	switch values["ActiveState"] {
	case "active":
		state.Active = true
	case "inactive":
	default:
		return ServiceState{}, errors.New("ambiguous systemctl state")
	}
	switch values["UnitFileState"] {
	case "enabled":
		state.Enabled = true
	case "disabled", "linked":
	default:
		return ServiceState{}, errors.New("ambiguous systemctl state")
	}
	return state, nil
}

func readinessClient() *http.Client {
	return &http.Client{
		Timeout: 2 * time.Second,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func (HostServiceManager) GateReady(ownerToken, version, commit string) error {
	challengeBytes := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, challengeBytes); err != nil || ownerToken == "" || !versionRE.MatchString(version) || !commitRE.MatchString(commit) {
		return errors.New("create Gate proof challenge failed")
	}
	challenge := base64.RawURLEncoding.EncodeToString(challengeBytes)
	ownerDigest := sha256.Sum256([]byte(ownerToken))
	mac := hmac.New(sha256.New, ownerDigest[:])
	_, _ = mac.Write([]byte("agent-forge/install-ready/v2\x00" + challenge + "\x00" + version + "\x00" + commit))
	wantProof := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	legacyRelease := version == legacyInstallerVersion && commit == legacyInstallerCommit
	wantLegacyProof := ""
	if legacyRelease {
		mac.Reset()
		_, _ = mac.Write([]byte("agent-forge/install-ready/v1\x00" + challenge))
		wantLegacyProof = base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	ctx, cancel := context.WithTimeout(context.Background(), readinessWindow)
	defer cancel()
	client := readinessClient()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gateBaseURL+"/install-proof?challenge="+challenge, nil)
		resp, err := client.Do(req)
		if err == nil {
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 257))
			_ = resp.Body.Close()
			var proof struct {
				Commit  string `json:"commit"`
				Proof   string `json:"proof"`
				Status  string `json:"status"`
				Version string `json:"version"`
			}
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			decodeErr := decoder.Decode(&proof)
			var extra any
			trailingErr := decoder.Decode(&extra)
			v2 := proof.Version == version && proof.Commit == commit && hmac.Equal([]byte(proof.Proof), []byte(wantProof))
			v1 := legacyRelease && proof.Version == "" && proof.Commit == "" && hmac.Equal([]byte(proof.Proof), []byte(wantLegacyProof))
			if resp.StatusCode == http.StatusOK && readErr == nil && len(body) <= 256 && decodeErr == nil && trailingErr == io.EOF && proof.Status == "ready" && (v2 || v1) {
				return nil
			}
		}
		if ctx.Err() != nil {
			return errors.New("Gate readiness timeout")
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func (HostServiceManager) WorkerReady(ownerToken string) error {
	ctx, cancel := context.WithTimeout(context.Background(), readinessWindow)
	defer cancel()
	client := readinessClient()
	for {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, gateBaseURL+"/v1/workers/worker-1", nil)
		req.Header.Set("Authorization", "Bearer "+ownerToken)
		resp, err := client.Do(req)
		if err == nil {
			var status struct {
				ID        string    `json:"id"`
				Connected bool      `json:"connected"`
				LastSeen  time.Time `json:"last_seen"`
				BaseID    string    `json:"base_id,omitempty"`
				Slot      int       `json:"slot,omitempty"`
				Pool      string    `json:"pool,omitempty"`
			}
			body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
			_ = resp.Body.Close()
			decoder := json.NewDecoder(bytes.NewReader(body))
			decoder.DisallowUnknownFields()
			decodeErr := decoder.Decode(&status)
			var extra any
			trailingErr := decoder.Decode(&extra)
			if resp.StatusCode == http.StatusOK && readErr == nil && len(body) <= 4096 && decodeErr == nil && trailingErr == io.EOF && status.ID == "worker-1" && status.Connected {
				return nil
			}
		}
		if ctx.Err() != nil {
			return errors.New("Worker connection readiness timeout")
		}
		time.Sleep(100 * time.Millisecond)
	}
}
