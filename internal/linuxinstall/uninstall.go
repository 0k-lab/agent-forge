//go:build linux

package linuxinstall

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"agent-forge/internal/configjson"
)

type UninstallOptions struct {
	Version, Commit, Root, Arch string
	Account                     AccountManager
	Services                    ServiceManager
	Ownership                   OwnershipManager
}

var renameUninstall = os.Rename
var unlinkUninstall = os.Remove
var removeUninstallCleanup = os.Remove

func cleanupUninstallQuarantine(quarantine string, previousExists bool) {
	for _, name := range upgradeObjects {
		if removeUninstallCleanup(filepath.Join(quarantine, name)) != nil {
			return
		}
	}
	if previousExists {
		previous := filepath.Join(quarantine, previousSlotName)
		for _, name := range upgradeObjects {
			if removeUninstallCleanup(filepath.Join(previous, name)) != nil {
				return
			}
		}
		for _, name := range []string{"bin", "systemd", ""} {
			if removeUninstallCleanup(filepath.Join(previous, name)) != nil {
				return
			}
		}
	}
	for _, name := range []string{"bin", "systemd", ""} {
		if removeUninstallCleanup(filepath.Join(quarantine, name)) != nil {
			return
		}
	}
}

func Uninstall(o UninstallOptions) (retErr error) {
	stage := "validate"
	defer func() {
		if retErr != nil {
			if _, ok := FailureStage(retErr); !ok {
				retErr = &installStageError{stage: stage, err: retErr}
			}
		}
	}()
	if !versionRE.MatchString(o.Version) || !commitRE.MatchString(o.Commit) || o.Arch == "" || o.Services == nil || o.Ownership == nil {
		return errors.New("uninstall unavailable")
	}
	if o.Root == "" {
		if requireTrustedAncestor("/opt") != nil || requireTrustedAncestor("/etc/systemd/system") != nil {
			return errors.New("uninstall unavailable")
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
	var header receipt
	headerBody, err := readNoFollow(filepath.Join(installPath, "install-receipt.json"), 1<<20)
	if err != nil || configjson.Decode(headerBody, &header) != nil {
		return errors.New("uninstall unavailable")
	}
	inspectOptions := Options{Arch: o.Arch, RunAsRoot: header.RunAsRoot, Account: o.Account, Ownership: o.Ownership}
	current, err := inspectExistingInstall(installPath, inspectOptions)
	if err != nil || current == nil || current.Version != o.Version || current.Commit != o.Commit {
		return errors.New("uninstall unavailable")
	}
	if !current.RunAsRoot {
		validator, ok := o.Account.(accountValidator)
		if !ok {
			return errors.New("uninstall unavailable")
		}
		uid, gid, validateErr := validator.Validate("agent-forge", prefix)
		if validateErr != nil || uid != current.AccountUID || gid != current.AccountGID {
			return errors.New("uninstall unavailable")
		}
	}
	if validateLinks(o.Root) != nil {
		return errors.New("uninstall unavailable")
	}
	parent := filepath.Dir(installPath)
	if rejectTransactionMaterial(parent) != nil {
		return errors.New("uninstall unavailable")
	}
	previousPath := filepath.Join(parent, previousSlotName)
	previousExists := false
	if _, statErr := os.Lstat(previousPath); statErr == nil {
		if _, inspectErr := inspectPreviousSlot(previousPath, inspectOptions, current); inspectErr != nil {
			return errors.New("uninstall unavailable")
		}
		previousExists = true
	} else if !os.IsNotExist(statErr) {
		return errors.New("uninstall unavailable")
	}
	ownerToken, err := existingOwnerToken(installPath)
	if err != nil {
		return errors.New("uninstall unavailable")
	}
	stateManager, ok := o.Services.(ServiceStateManager)
	if !ok {
		return errors.New("uninstall unavailable")
	}
	gateState, err := stateManager.State("agent-forge-gate.service")
	if err != nil {
		return errors.New("uninstall unavailable")
	}
	workerState, err := stateManager.State("agent-forge-worker.service")
	if err != nil || workerState.Active && !gateState.Active {
		return errors.New("uninstall unavailable")
	}

	stage = "staging"
	quarantine, err := os.MkdirTemp(parent, ".agent-forge.uninstall-")
	if err != nil {
		return err
	}
	preserve := false
	defer func() {
		if !preserve {
			for _, name := range []string{"bin", "systemd", ""} {
				_ = os.Remove(filepath.Join(quarantine, name))
			}
		}
	}()
	if err := os.Chmod(quarantine, 0o700); err != nil {
		return err
	}
	for _, dir := range []string{"bin", "systemd"} {
		if err := os.Mkdir(filepath.Join(quarantine, dir), 0o700); err != nil {
			return err
		}
	}
	for _, name := range upgradeObjects {
		if !sameFilesystem(filepath.Join(installPath, name), filepath.Dir(filepath.Join(quarantine, name))) {
			return errors.New("uninstall rename topology crosses filesystems")
		}
	}
	if previousExists && !sameFilesystem(previousPath, quarantine) {
		return errors.New("uninstall rename topology crosses filesystems")
	}

	stage = "activation"
	moved := make([]string, 0, len(upgradeObjects))
	previousMoved := false
	recover := func(primary error) error {
		var recovery []error
		for i := len(moved) - 1; i >= 0; i-- {
			name := moved[i]
			if err := renameUninstall(filepath.Join(quarantine, name), filepath.Join(installPath, name)); err != nil {
				recovery = append(recovery, fmt.Errorf("uninstall recovery failed: %s", name))
			}
		}
		if previousMoved {
			if err := renameUninstall(filepath.Join(quarantine, previousSlotName), previousPath); err != nil {
				recovery = append(recovery, errors.New("uninstall recovery failed: previous slot"))
			}
		}
		for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
			link := rooted(o.Root, "/etc/systemd/system/"+name)
			target := prefix + "/systemd/" + name
			if o.Root != "" {
				target = rooted(o.Root, target)
			}
			got, readErr := os.Readlink(link)
			if readErr == nil && got == target {
				continue
			}
			info, statErr := os.Lstat(link)
			if statErr == nil && info.Mode()&os.ModeSymlink != 0 && singlyLinked(info) {
				statErr = os.Remove(link)
			}
			if os.IsNotExist(statErr) || statErr == nil {
				statErr = os.Symlink(target, link)
			}
			if statErr != nil {
				recovery = append(recovery, errors.New("uninstall recovery failed: unit link"))
			}
		}
		if err := o.Services.Run("daemon-reload"); err != nil {
			recovery = append(recovery, errors.New("uninstall recovery failed: daemon reload"))
		}
		restoreService := func(unit string, want ServiceState, ready func() error) {
			got, probeErr := stateManager.State(unit)
			if probeErr != nil || got.Enabled != want.Enabled {
				operation := "disable"
				if want.Enabled {
					operation = "enable"
				}
				if err := o.Services.Run(operation, unit); err != nil {
					recovery = append(recovery, errors.New("uninstall recovery failed: service enablement"))
				}
			}
			if probeErr != nil || got.Active != want.Active {
				operation := "stop"
				if want.Active {
					operation = "start"
				}
				if err := o.Services.Run(operation, unit); err != nil {
					recovery = append(recovery, errors.New("uninstall recovery failed: service activity"))
				}
			}
			if want.Active {
				if err := ready(); err != nil {
					recovery = append(recovery, errors.New("uninstall recovery failed: readiness"))
				}
			}
			if got, err := stateManager.State(unit); err != nil || got != want {
				recovery = append(recovery, errors.New("uninstall recovery failed: service state"))
			}
		}
		restoreService("agent-forge-gate.service", gateState, func() error {
			return o.Services.GateReady(ownerToken, current.Version, current.Commit)
		})
		restoreService("agent-forge-worker.service", workerState, func() error {
			return o.Services.WorkerReady(ownerToken)
		})
		if len(recovery) != 0 {
			preserve = true
			record := filepath.Join(parent, ".agent-forge.uninstall-recovery")
			if err := writeExclusive(record, []byte("manual recovery required\n"), 0o600); err != nil && !os.IsExist(err) {
				recovery = append(recovery, errors.New("uninstall recovery material failed"))
			}
		}
		return errors.Join(append([]error{primary}, recovery...)...)
	}
	var shutdown [][]string
	if workerState.Active {
		shutdown = append(shutdown, []string{"stop", "agent-forge-worker.service"})
	}
	if workerState.Enabled {
		shutdown = append(shutdown, []string{"disable", "agent-forge-worker.service"})
	}
	if gateState.Active {
		shutdown = append(shutdown, []string{"stop", "agent-forge-gate.service"})
	}
	if gateState.Enabled {
		shutdown = append(shutdown, []string{"disable", "agent-forge-gate.service"})
	}
	successfullyDisabled := map[string]bool{}
	for _, call := range shutdown {
		if err := o.Services.Run(call...); err != nil {
			return recover(err)
		}
		if call[0] == "disable" {
			successfullyDisabled[call[1]] = true
		}
	}

	stage = "publication"
	for _, name := range upgradeObjects {
		if err := renameUninstall(filepath.Join(installPath, name), filepath.Join(quarantine, name)); err != nil {
			return recover(err)
		}
		moved = append(moved, name)
	}
	if previousExists {
		if err := renameUninstall(previousPath, filepath.Join(quarantine, previousSlotName)); err != nil {
			return recover(err)
		}
		previousMoved = true
	}
	for _, name := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		link := rooted(o.Root, "/etc/systemd/system/"+name)
		target := prefix + "/systemd/" + name
		if o.Root != "" {
			target = rooted(o.Root, target)
		}
		info, statErr := os.Lstat(link)
		if os.IsNotExist(statErr) && successfullyDisabled[name] {
			continue
		}
		if statErr != nil || info.Mode()&os.ModeSymlink == 0 || !singlyLinked(info) {
			return recover(errors.New("uninstall unit link changed"))
		}
		got, readErr := os.Readlink(link)
		if readErr != nil || got != target {
			return recover(errors.New("uninstall unit link changed"))
		}
		if err := unlinkUninstall(link); err != nil {
			return recover(err)
		}
	}
	if err := o.Services.Run("daemon-reload"); err != nil {
		return recover(err)
	}
	for _, unit := range []string{"agent-forge-gate.service", "agent-forge-worker.service"} {
		state, err := stateManager.State(unit)
		if err != nil || state != (ServiceState{}) {
			return recover(errors.New("uninstall service shutdown not verified"))
		}
	}
	preserve = true
	cleanupUninstallQuarantine(quarantine, previousExists)
	return nil
}
