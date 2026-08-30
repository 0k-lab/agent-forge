//go:build linux

package linuxinstall

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"agent-forge/internal/configjson"
	"golang.org/x/sys/unix"
)

type RollbackOptions struct {
	Version, Commit, Root, Arch string
	Account                     AccountManager
	Services                    ServiceManager
	Ownership                   OwnershipManager
}

func Rollback(o RollbackOptions) (retErr error) {
	stage := "validate"
	defer func() {
		if retErr != nil {
			if _, ok := FailureStage(retErr); !ok {
				retErr = &installStageError{stage: stage, err: retErr}
			}
		}
	}()
	if !versionRE.MatchString(o.Version) || !commitRE.MatchString(o.Commit) || o.Arch == "" || o.Services == nil || o.Ownership == nil {
		return errors.New("rollback unavailable")
	}
	if o.Root == "" {
		if err := requireTrustedAncestor("/opt"); err != nil {
			return errors.New("rollback unavailable")
		}
		if err := requireTrustedAncestor("/etc/systemd/system"); err != nil {
			return errors.New("rollback unavailable")
		}
	}
	stage = "host"
	lock, err := acquireUpgradeLock(o.Root)
	if err != nil {
		return err
	}
	defer lock.Close()

	stage = "existing"
	installPath := rooted(o.Root, prefix)
	installInfo, err := os.Lstat(installPath)
	if err != nil || !installInfo.IsDir() || installInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("rollback unavailable")
	}
	var header receipt
	headerBody, headerErr := readNoFollow(filepath.Join(installPath, "install-receipt.json"), 1<<20)
	if headerErr != nil || configjson.Decode(headerBody, &header) != nil {
		return errors.New("rollback unavailable")
	}
	inspectOptions := Options{Arch: o.Arch, RunAsRoot: header.RunAsRoot, Account: o.Account, Ownership: o.Ownership}
	current, err := inspectExistingInstall(installPath, inspectOptions)
	if err != nil || current == nil || current.Version != o.Version || current.Commit != o.Commit {
		return errors.New("rollback unavailable")
	}
	if !current.RunAsRoot {
		validator, ok := o.Account.(accountValidator)
		if !ok {
			return errors.New("rollback unavailable")
		}
		uid, gid, validateErr := validator.Validate("agent-forge", prefix)
		if validateErr != nil || uid != current.AccountUID || gid != current.AccountGID {
			return errors.New("rollback unavailable")
		}
	}
	if err := validateLinks(o.Root); err != nil {
		return errors.New("rollback unavailable")
	}
	parent := filepath.Dir(installPath)
	if err := rejectTransactionMaterial(parent); err != nil {
		return errors.New("rollback unavailable")
	}
	previousPath := filepath.Join(parent, previousSlotName)
	previous, err := inspectPreviousSlot(previousPath, inspectOptions, current)
	if err != nil || compareVersions(previous.Version, current.Version) >= 0 {
		return errors.New("rollback unavailable")
	}
	if !sameReleaseFilesystem(installPath, previousPath) {
		return errors.New("rollback unavailable")
	}
	ownerToken, err := existingOwnerToken(installPath)
	if err != nil {
		return errors.New("rollback unavailable")
	}

	stage = "activation"
	stopped := true
	swapped := make([]string, 0, len(upgradeObjects))
	recover := func(primary error) error {
		var recovery []error
		var stillSwapped []string
		if stopped {
			if err := o.Services.Run("stop", "agent-forge-worker.service"); err != nil {
				recovery = append(recovery, errors.New("rollback recovery failed"))
			}
			if err := o.Services.Run("stop", "agent-forge-gate.service"); err != nil {
				recovery = append(recovery, errors.New("rollback recovery failed"))
			}
		}
		for i := len(swapped) - 1; i >= 0; i-- {
			name := swapped[i]
			if err := exchangeUpgrade(filepath.Join(installPath, name), filepath.Join(previousPath, name)); err != nil {
				recovery = append(recovery, errors.New("rollback recovery failed"))
				stillSwapped = append(stillSwapped, name)
			}
		}
		if len(stillSwapped) != 0 {
			record, _ := json.Marshal(struct {
				CurrentVersion, CurrentCommit, TargetVersion, TargetCommit string
				StillSwapped                                               []string
			}{current.Version, current.Commit, previous.Version, previous.Commit, stillSwapped})
			if err := writeExclusive(filepath.Join(parent, ".agent-forge.rollback-recovery"), append(record, '\n'), 0o600); err != nil {
				recovery = append(recovery, errors.New("rollback recovery material failed"))
			}
		}
		restore := Options{Version: current.Version, Commit: current.Commit, EnableNow: true, Services: o.Services}
		if err := activate(restore, ownerToken); err != nil {
			recovery = append(recovery, errors.New("rollback recovery failed"))
		}
		return errors.Join(append([]error{primary}, recovery...)...)
	}
	if err := o.Services.Run("stop", "agent-forge-worker.service"); err != nil {
		return recover(err)
	}
	if err := o.Services.Run("stop", "agent-forge-gate.service"); err != nil {
		return recover(err)
	}
	stage = "publication"
	for _, name := range upgradeObjects {
		if err := exchangeUpgrade(filepath.Join(installPath, name), filepath.Join(previousPath, name)); err != nil {
			return recover(err)
		}
		swapped = append(swapped, name)
	}
	stage = "activation"
	target := Options{Version: previous.Version, Commit: previous.Commit, EnableNow: true, Services: o.Services}
	if err := activate(target, ownerToken); err != nil {
		return recover(err)
	}
	return nil
}

func inspectPreviousSlot(path string, o Options, current *receipt) (*receipt, error) {
	for _, dir := range []struct {
		name string
		mode fs.FileMode
	}{{"", 0o700}, {"bin", 0o755}, {"systemd", 0o755}} {
		name := filepath.Join(path, dir.name)
		info, err := os.Lstat(name)
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != dir.mode || !ownedBy(o.Ownership, name, 0, 0) {
			return nil, errors.New("invalid previous slot")
		}
	}
	receiptPath := filepath.Join(path, "install-receipt.json")
	info, err := os.Lstat(receiptPath)
	if err != nil || !info.Mode().IsRegular() || !singlyLinked(info) || info.Mode().Perm() != 0o400 || !ownedBy(o.Ownership, receiptPath, 0, 0) {
		return nil, errors.New("invalid previous slot")
	}
	body, err := readNoFollow(receiptPath, 1<<20)
	var r receipt
	if err != nil || configjson.Decode(body, &r) != nil || !versionRE.MatchString(r.Version) || !commitRE.MatchString(r.Commit) || r.Architecture != o.Arch || r.RunAsRoot != o.RunAsRoot || r.AccountUID != current.AccountUID || r.AccountGID != current.AccountGID || !digestRE.MatchString(r.OwnerTokenSHA256) || !digestRE.MatchString(r.WorkerTokenSHA256) {
		return nil, errors.New("invalid previous slot")
	}
	if r.OwnerTokenSHA256 != current.OwnerTokenSHA256 || r.WorkerTokenSHA256 != current.WorkerTokenSHA256 {
		return nil, errors.New("invalid previous slot")
	}
	for _, name := range []string{"etc/gate.json", "etc/worker.json", "secrets/gate.env", "secrets/worker.env"} {
		if r.Files[name] != current.Files[name] || r.Modes[name] != current.Modes[name] {
			return nil, errors.New("invalid previous slot")
		}
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(body, &raw) != nil {
		return nil, errors.New("invalid previous slot")
	}
	_, marker := raw["store_schema_version"]
	if !marker && r.Version == legacyInstallerVersion && r.Commit == legacyInstallerCommit {
		r.StoreSchemaVersion = storeSchemaVersion
	}
	if !marker && (r.Version != legacyInstallerVersion || r.Commit != legacyInstallerCommit) || r.StoreSchemaVersion != storeSchemaVersion || len(r.Files) != len(immutableFiles()) || len(r.Modes) != len(immutableFiles()) || len(r.ArchiveSHA256) != 3 || len(r.Directories) != len(directoryContracts(r.AccountUID, r.AccountGID)) {
		return nil, errors.New("invalid previous slot")
	}
	wantModes := immutableModes()
	for _, role := range []string{"cli", "gate", "worker"} {
		name := "agent-forge-" + role + "_" + r.Version + "_linux_" + r.Architecture + ".tar.gz"
		if !digestRE.MatchString(r.ArchiveSHA256[name]) {
			return nil, errors.New("invalid previous slot")
		}
	}
	for _, name := range immutableFiles() {
		if !digestRE.MatchString(r.Files[name]) || r.Modes[name] != wantModes[name] {
			return nil, errors.New("invalid previous slot")
		}
	}
	for name, want := range directoryContracts(r.AccountUID, r.AccountGID) {
		if r.Directories[name] != want {
			return nil, errors.New("invalid previous slot")
		}
	}
	for _, name := range upgradeObjects[:len(upgradeObjects)-1] {
		object := filepath.Join(path, name)
		objectInfo, statErr := os.Lstat(object)
		body, readErr := readNoFollow(object, 1<<30)
		if statErr != nil || readErr != nil || !objectInfo.Mode().IsRegular() || !singlyLinked(objectInfo) || objectInfo.Mode().Perm() != fs.FileMode(wantModes[name]) || r.Modes[name] != wantModes[name] || !ownedBy(o.Ownership, object, 0, 0) || hashBytes(body) != r.Files[name] {
			return nil, errors.New("invalid previous slot")
		}
	}
	for _, exact := range []struct {
		name string
		want int
	}{{"", 3}, {"bin", 5}, {"systemd", 2}} {
		entries, readErr := os.ReadDir(filepath.Join(path, exact.name))
		if readErr != nil || len(entries) != exact.want {
			return nil, errors.New("invalid previous slot")
		}
	}
	return &r, nil
}

var statx = unix.Statx

var sameFilesystem = func(a, b string) bool {
	var aStat, bStat unix.Statx_t
	if statx(unix.AT_FDCWD, a, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &aStat) != nil ||
		statx(unix.AT_FDCWD, b, unix.AT_SYMLINK_NOFOLLOW, unix.STATX_MNT_ID, &bStat) != nil {
		return false
	}
	return aStat.Mask&unix.STATX_MNT_ID != 0 && bStat.Mask&unix.STATX_MNT_ID != 0 &&
		aStat.Mnt_id == bStat.Mnt_id && aStat.Dev_major == bStat.Dev_major && aStat.Dev_minor == bStat.Dev_minor
}

func sameReleaseFilesystem(current, slot string) bool {
	if !sameFilesystem(current, slot) {
		return false
	}
	for _, name := range upgradeObjects {
		if !sameFilesystem(filepath.Join(current, name), filepath.Join(slot, name)) {
			return false
		}
	}
	return true
}
