package store

import (
	"errors"
	"path/filepath"

	"agent-forge/internal/protocol"
)

func validateStoredTask(task protocol.CodingTask, policy ResolvedPolicy) error {
	e := policy.Execution
	validSource := task.RepositoryID != "" && task.RepositoryID == e.RepositoryID && (task.Repository == "" || filepath.IsAbs(task.Repository) && filepath.Clean(task.Repository) == task.Repository && len(task.Repository) <= 4096)
	if !validSource || protocol.ValidateBaseSHA(task.BaseSHA) != nil || task.Instruction == "" || len(task.Instruction) > 65536 || len(task.Tests) > 32 || protocol.ValidateCommitAuthor(task.CommitAuthorName, task.CommitAuthorEmail) != nil {
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
