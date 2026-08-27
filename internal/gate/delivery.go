package gate

import (
	"context"
	"errors"
	"time"

	"agent-forge/internal/githubdelivery"
	"agent-forge/internal/store"
)

func (x *server) deliveryForCandidate(ctx context.Context, lease store.Lease, candidate string) (store.Delivery, error) {
	if x.config == nil || x.config.Delivery == nil || lease.Task == nil || lease.Task.RepositoryID == "" {
		return store.Delivery{}, errors.New("delivery is not configured")
	}
	repository, ok := x.repository(lease.Task.RepositoryID)
	if !ok || repository.RepositoryURL == "" {
		return store.Delivery{}, errors.New("repository is not deliverable")
	}
	ref := "refs/agent-forge/candidates/" + lease.JobID + "/" + lease.AttemptID
	git := func(args ...string) (string, error) {
		return publicGitRunner(ctx, x.config.GitExecutable, lease.Task.Repository, repository.Execution.GitTimeout, repository.Execution.GitOutputBytes, args...)
	}
	if got, err := git("rev-parse", "--verify", ref+"^{commit}"); err != nil || got != candidate {
		return store.Delivery{}, errors.New("candidate ref mismatch")
	}
	parents, err := git("rev-list", "--parents", "-n", "1", candidate)
	if err != nil || parents != candidate+" "+lease.Task.BaseSHA {
		return store.Delivery{}, errors.New("candidate parent mismatch")
	}
	tree, err := git("rev-parse", candidate+"^{tree}")
	if err != nil || len(tree) != 40 {
		return store.Delivery{}, errors.New("candidate tree invalid")
	}
	return store.Delivery{
		JobID: lease.JobID, AttemptID: lease.AttemptID, CandidateSHA: candidate, ExpectedTreeSHA: tree,
		ParentSHA: lease.Task.BaseSHA, CandidateRef: ref, RepositoryID: repository.ID, RepositoryURL: repository.RepositoryURL,
		DefaultBranch: repository.DefaultBranch, Branch: "forge/" + lease.JobID, PRTitle: "Agent Forge job " + lease.JobID,
		PRBody: "Automated delivery for candidate `" + candidate + "`.", Phase: "pending", MaxAttempts: x.config.Delivery.MaxAttempts,
	}, nil
}

func (x *server) repository(id string) (RepositoryRegistration, bool) {
	for _, repository := range x.config.Repositories {
		if repository.ID == id {
			return repository, true
		}
	}
	return RepositoryRegistration{}, false
}

func (x *server) deliveryLoop(ctx context.Context) {
	ticker := time.NewTicker(x.options.LeasePollInterval)
	defer ticker.Stop()
	// ponytail: deliveries are serialized; add a bounded worker pool if delivery throughput becomes measurable.
	for {
		x.runOneDelivery(ctx)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (x *server) runOneDelivery(ctx context.Context) {
	delivery, ok, err := x.store.ClaimDelivery(x.options.Now().UTC())
	if err != nil || !ok {
		return
	}
	repository, found := x.repository(delivery.RepositoryID)
	source, sourceErr := canonicalPublicGitHubURL(delivery.RepositoryURL)
	if !found || sourceErr != nil || repository.RepositoryURL != delivery.RepositoryURL || repository.DefaultBranch != delivery.DefaultBranch {
		_ = x.store.FailDelivery(delivery.JobID, "delivery_registration_changed", x.options.Now().UTC())
		return
	}
	cfg := githubdelivery.Config{Version: 1, APIBase: x.config.Delivery.APIBase, Owner: source.Owner, Repository: source.Repository,
		LocalRepository: publicRepositoryPath(x.config.PublicRepositoryRoot, source), GitExecutable: x.config.GitExecutable,
		AppID: x.config.Delivery.AppID, PrivateKeyPath: x.config.Delivery.PrivateKeyPath}
	publication := githubdelivery.Publication{Version: 1, CandidateSHA: delivery.CandidateSHA, ExpectedParentSHA: delivery.ParentSHA,
		ExpectedTreeSHA: delivery.ExpectedTreeSHA, CandidateRef: delivery.CandidateRef, BaseBranch: delivery.DefaultBranch,
		NewBranch: delivery.Branch, PRTitle: delivery.PRTitle, PRBody: delivery.PRBody}
	op := githubdelivery.AutomationOptions{Options: x.options.Delivery, PollInterval: x.config.Delivery.PollInterval,
		NoRunsGrace: x.config.Delivery.NoRunsGrace, Timeout: x.config.Delivery.Timeout,
		OnState: func(result githubdelivery.Result, number int, ci string) error {
			phase := "ci"
			if ci == "success" {
				phase = "merging"
			}
			return x.store.UpdateDelivery(delivery.JobID, phase, result.PRURL, number, ci, x.options.Now().UTC())
		}}
	result, err := githubdelivery.DeliverAndMerge(ctx, cfg, publication, op)
	if err == nil {
		_ = x.store.CompleteDelivery(delivery.JobID, result.MergeSHA, x.options.Now().UTC())
		x.log("delivery_merged", "job_id", delivery.JobID, "phase", "merged", "candidate_sha", delivery.CandidateSHA, "merge_sha", result.MergeSHA)
		return
	}
	code, transient := deliveryFailure(err)
	if transient {
		_ = x.store.RetryDelivery(delivery.JobID, code, x.options.Now().UTC(), x.config.Delivery.RetryBase)
	} else {
		_ = x.store.FailDelivery(delivery.JobID, code, x.options.Now().UTC())
	}
	x.log("delivery_failed", "job_id", delivery.JobID, "phase", "failed", "failure_code", code)
}

func deliveryFailure(err error) (string, bool) {
	var failure *githubdelivery.Error
	code := "delivery_failed"
	if githubdelivery.AsError(err, &failure) {
		code = "delivery_" + failure.Code
	}
	transient := code == "delivery_transient_api" || code == "delivery_push_failed" || code == "delivery_credential_expired"
	return code, transient
}
