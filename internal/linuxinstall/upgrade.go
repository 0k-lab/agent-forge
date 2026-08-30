//go:build linux

package linuxinstall

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

var upgradeObjects = []string{
	"bin/forge",
	"bin/forge-gate",
	"bin/forge-worker",
	"bin/forge-codex-plugin",
	"bin/forge-ref-plugin",
	"systemd/agent-forge-gate.service",
	"systemd/agent-forge-worker.service",
	"install-receipt.json",
}

var renameUpgrade = os.Rename

func upgradeInstall(o Options, installPath string, previous *receipt, archives []verifiedArchive) (retErr error) {
	if !o.EnableNow || o.Services == nil {
		return &installStageError{stage: "activation", err: errors.New("upgrade requires readiness activation")}
	}
	ownerToken, err := existingOwnerToken(installPath)
	if err != nil {
		return &installStageError{stage: "existing", err: err}
	}
	parent := filepath.Dir(installPath)
	stage, err := os.MkdirTemp(parent, ".agent-forge.upgrade-")
	if err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	preserveRecovery := false
	defer func() {
		if !preserveRecovery {
			_ = os.RemoveAll(stage)
		}
	}()
	if err := os.Chmod(stage, 0o700); err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	for _, dir := range []struct {
		name string
		mode fs.FileMode
	}{{"bin", 0o755}, {"systemd", 0o755}} {
		if err := os.Mkdir(filepath.Join(stage, dir.name), dir.mode); err != nil {
			return &installStageError{stage: "staging", err: err}
		}
	}
	for _, archive := range archives {
		if err := extractArchive(archive, filepath.Join(stage, "bin"), o.Version, o.Commit); err != nil {
			return &installStageError{stage: "staging", err: err}
		}
	}
	user := "agent-forge"
	if o.RunAsRoot {
		user = "root"
	}
	gateUnit, workerUnit := units(user, runtimePaths(o.Root))
	if err := writeExclusive(filepath.Join(stage, "systemd/agent-forge-gate.service"), []byte(gateUnit), 0o644); err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	if err := writeExclusive(filepath.Join(stage, "systemd/agent-forge-worker.service"), []byte(workerUnit), 0o644); err != nil {
		return &installStageError{stage: "staging", err: err}
	}

	next := *previous
	next.Version, next.Commit = o.Version, o.Commit
	next.ArchiveSHA256 = make(map[string]string, len(archives))
	for _, archive := range archives {
		next.ArchiveSHA256[archive.name] = archive.digest
	}
	next.Files = cloneStringMap(previous.Files)
	next.Modes = cloneModeMap(previous.Modes)
	expectedModes := immutableModes()
	for _, name := range upgradeObjects[:len(upgradeObjects)-1] {
		if chmodErr := os.Chmod(filepath.Join(stage, name), fs.FileMode(expectedModes[name])); chmodErr != nil {
			return &installStageError{stage: "staging", err: chmodErr}
		}
		body, readErr := os.ReadFile(filepath.Join(stage, name))
		info, statErr := os.Lstat(filepath.Join(stage, name))
		if readErr != nil || statErr != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != fs.FileMode(expectedModes[name]) {
			return &installStageError{stage: "staging", err: errors.New("invalid staged upgrade object")}
		}
		next.Files[name] = hashBytes(body)
		next.Modes[name] = uint32(info.Mode().Perm())
	}
	receiptBody, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	receiptBody = append(receiptBody, '\n')
	if err := writeExclusive(filepath.Join(stage, "install-receipt.json"), receiptBody, 0o400); err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	if err := os.Chmod(filepath.Join(stage, "install-receipt.json"), 0o400); err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	if info, err := os.Lstat(filepath.Join(stage, "install-receipt.json")); err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0o400 {
		return &installStageError{stage: "staging", err: errors.New("invalid staged upgrade receipt")}
	}

	backup, err := os.MkdirTemp(parent, ".agent-forge.rollback-")
	if err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	defer func() {
		if !preserveRecovery {
			_ = os.RemoveAll(backup)
		}
	}()
	if err := os.Chmod(backup, 0o700); err != nil {
		return &installStageError{stage: "staging", err: err}
	}
	for _, dir := range []string{"bin", "systemd"} {
		if err := os.Mkdir(filepath.Join(backup, dir), 0o700); err != nil {
			return &installStageError{stage: "staging", err: err}
		}
	}

	stopped := false
	swapped := make([]string, 0, len(upgradeObjects))
	rollback := func(primary error) error {
		var rollbackErrors []error
		if stopped {
			if err := o.Services.Run("stop", "agent-forge-worker.service"); err != nil {
				rollbackErrors = append(rollbackErrors, errors.New("stop failed Worker during rollback"))
			}
			if err := o.Services.Run("stop", "agent-forge-gate.service"); err != nil {
				rollbackErrors = append(rollbackErrors, errors.New("stop failed Gate during rollback"))
			}
		}
		for i := len(swapped) - 1; i >= 0; i-- {
			name := swapped[i]
			current := filepath.Join(installPath, name)
			old := filepath.Join(backup, name)
			failed := filepath.Join(stage, name)
			_ = os.Remove(failed)
			if err := renameUpgrade(current, failed); err != nil && !os.IsNotExist(err) {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("remove failed upgrade object %s", name))
				continue
			}
			if err := renameUpgrade(old, current); err != nil {
				rollbackErrors = append(rollbackErrors, fmt.Errorf("restore old upgrade object %s", name))
			}
		}
		if stopped {
			previousOptions := o
			previousOptions.Version, previousOptions.Commit = previous.Version, previous.Commit
			if err := activate(previousOptions, ownerToken); err != nil {
				rollbackErrors = append(rollbackErrors, errors.New("old release readiness rollback failed"))
			}
		}
		if len(rollbackErrors) != 0 {
			preserveRecovery = true
		}
		return errors.Join(append([]error{primary}, rollbackErrors...)...)
	}

	stopped = true
	if err := o.Services.Run("stop", "agent-forge-worker.service"); err != nil {
		return &installStageError{stage: "activation", err: rollback(err)}
	}
	if err := o.Services.Run("stop", "agent-forge-gate.service"); err != nil {
		return &installStageError{stage: "activation", err: rollback(err)}
	}
	for _, name := range upgradeObjects {
		current := filepath.Join(installPath, name)
		old := filepath.Join(backup, name)
		candidate := filepath.Join(stage, name)
		if err := renameUpgrade(current, old); err != nil {
			return &installStageError{stage: "publication", err: rollback(err)}
		}
		swapped = append(swapped, name)
		if err := renameUpgrade(candidate, current); err != nil {
			return &installStageError{stage: "publication", err: rollback(err)}
		}
	}
	if err := activate(o, ownerToken); err != nil {
		return &installStageError{stage: "activation", err: rollback(err)}
	}
	return nil
}

func cloneStringMap(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneModeMap(source map[string]uint32) map[string]uint32 {
	result := make(map[string]uint32, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}
