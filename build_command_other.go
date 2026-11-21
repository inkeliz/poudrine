//go:build !freebsd

package poudrine

import (
	"context"
	"errors"
	"io"
	"os/exec"
)

// The production worker requires FreeBSD. Keep the unscheduled development
// command path available to platform-independent tests.
func (b *Builder) runWorkCommand(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	if b.currentWorkingHours() != (workingHours{}) {
		return errors.New("scheduled Poudriere commands require FreeBSD")
	}
	if err := b.startWork(ctx); err != nil {
		return err
	}
	defer b.endWork()
	cmd := exec.CommandContext(ctx, "poudriere", append([]string{"-N", "-e", b.cfg.ConfigDir}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

func runPoudriereCommand(context.Context, []string) error {
	return errors.New("Poudriere commands require FreeBSD")
}
