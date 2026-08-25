//go:build !linux

package processtree

import (
	"context"
	"os/exec"
)

func RunInvocation(ctx context.Context, cmd *exec.Cmd) error { return Run(ctx, cmd) }
