//go:build linux

package linuxinstall

import (
	"os"
	"path/filepath"
	"runtime"
	"time"

	"agent-forge/internal/configjson"
	"agent-forge/internal/store"
)

type DoctorCheck struct {
	ID string
	OK bool
}

type DoctorReport struct {
	Checks []DoctorCheck
}

func (r DoctorReport) Healthy() bool {
	for _, check := range r.Checks {
		if !check.OK {
			return false
		}
	}
	return len(r.Checks) > 0
}

type DoctorOptions struct {
	Root      string
	Services  ServiceManager
	Ownership OwnershipManager
	Account   AccountManager
}

func Doctor(o DoctorOptions) DoctorReport {
	if o.Ownership == nil {
		o.Ownership = HostOwnershipManager{}
	}
	installPath := rooted(o.Root, prefix)
	var header receipt
	body, headerErr := readNoFollow(filepath.Join(installPath, "install-receipt.json"), 1<<20)
	if headerErr == nil {
		headerErr = configjson.Decode(body, &header)
	}
	inspectOptions := Options{Root: o.Root, Arch: runtime.GOARCH, RunAsRoot: header.RunAsRoot, Ownership: o.Ownership}
	installed, inspectErr := inspectExistingInstall(installPath, inspectOptions)
	receiptOK := headerErr == nil && inspectErr == nil && installed != nil
	if receiptOK && !installed.RunAsRoot {
		validator, ok := o.Account.(accountValidator)
		uid, gid, err := 0, 0, error(nil)
		if ok {
			uid, gid, err = validator.Validate("agent-forge", prefix)
		}
		receiptOK = ok && err == nil && uid == installed.AccountUID && gid == installed.AccountGID
	}

	trusted := validateLinks(o.Root) == nil
	if o.Root == "" {
		trusted = trusted && requireTrustedAncestor("/opt") == nil && requireTrustedAncestor("/etc/systemd/system") == nil
	}
	identity := receiptOK && verifyBinaryIdentity(filepath.Join(installPath, "bin/forge"), "forge "+installed.Version+" "+installed.Commit+"\n", 2*time.Second) == nil

	database := filepath.Join(installPath, "var/gate/state/forge.db")
	mutable := receiptOK && safeMutableFile(o.Ownership, database, installed.AccountUID, installed.AccountGID)
	checks := []DoctorCheck{
		{ID: "receipt", OK: receiptOK},
		{ID: "cli-identity", OK: identity},
		{ID: "trusted-paths", OK: trusted},
		{ID: "immutable-paths", OK: receiptOK},
		{ID: "mutable-paths", OK: mutable},
	}

	gateEnabled := observedService(o.Services, "is-enabled", "agent-forge-gate.service")
	gateActive := observedService(o.Services, "is-active", "agent-forge-gate.service")
	workerEnabled := observedService(o.Services, "is-enabled", "agent-forge-worker.service")
	workerActive := observedService(o.Services, "is-active", "agent-forge-worker.service")
	checks = append(checks,
		DoctorCheck{ID: "gate-enabled", OK: gateEnabled}, DoctorCheck{ID: "gate-active", OK: gateActive},
		DoctorCheck{ID: "worker-enabled", OK: workerEnabled}, DoctorCheck{ID: "worker-active", OK: workerActive},
	)
	ownerToken := ""
	if receiptOK {
		ownerToken, _ = existingOwnerToken(installPath)
	}
	gateReady, workerReady := false, false
	if ownerToken != "" && o.Services != nil {
		gateReady = o.Services.GateReady(ownerToken, installed.Version, installed.Commit) == nil
		workerReady = o.Services.WorkerReady(ownerToken) == nil
	}
	checks = append(checks,
		DoctorCheck{ID: "gate-readiness", OK: gateReady},
		DoctorCheck{ID: "worker-readiness", OK: workerReady},
		DoctorCheck{ID: "store-schema", OK: mutable && store.CheckSchemaReadOnly(database) == nil},
	)
	return DoctorReport{Checks: checks}
}

func observedService(services ServiceManager, operation, name string) bool {
	return services != nil && services.Run(operation, name) == nil
}

func safeMutableFile(ownership OwnershipManager, name string, uid, gid int) bool {
	info, err := os.Lstat(name)
	return err == nil && info.Mode().IsRegular() && singlyLinked(info) && info.Mode().Perm() == 0o600 && ownedBy(ownership, name, uint32(uid), uint32(gid))
}
