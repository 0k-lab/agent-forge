//go:build !linux

package processtree

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
)

func RunInvocation(ctx context.Context, cmd *exec.Cmd) error {
	err := Run(ctx, cmd)
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var execErr *exec.Error
	var pathErr *os.PathError
	if errors.As(err, &execErr) || errors.As(err, &pathErr) {
		return fmt.Errorf("%w: %v", ErrStart, err)
	}
	return err
}
