package store

import (
	"errors"

	"agent-forge/internal/protocol"
)

func validateStoredTask(task protocol.CodingTask, policy ResolvedPolicy) error {
	if task.Repository != "" || task.RepositoryID == "" || task.RepositoryID != policy.Execution.RepositoryID || protocol.ValidateBaseSHA(task.BaseSHA) != nil || task.Instruction == "" || len(task.Instruction) > 65536 || len(task.Tests) > 32 || protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail) != nil {
		return errors.New("invalid coding task")
	}
	for _, argv := range task.Tests {
		if len(argv) == 0 || len(argv) > 64 {
			return errors.New("invalid coding task")
		}
		for _, argument := range argv {
			if len(argument) > 4096 {
				return errors.New("invalid coding task")
			}
		}
	}
	return nil
}
