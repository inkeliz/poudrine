package config

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"syscall"
	"time"
)

func ServiceCredential(u *user.User) (*syscall.Credential, error) {
	uid, err := strconv.ParseUint(u.Uid, 10, 32)
	if err != nil || uid == 0 {
		return nil, errors.New("service user must have a nonzero uid")
	}
	gid, err := strconv.ParseUint(u.Gid, 10, 32)
	if err != nil || gid == 0 {
		return nil, errors.New("service user must have a nonzero primary gid")
	}
	return &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid)}, nil
}

func ChildProcess(ctx context.Context, exe string, cred *syscall.Credential, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, exe, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: cred}
	cmd.Env = []string{"PATH=/sbin:/bin:/usr/sbin:/usr/bin:/usr/local/sbin:/usr/local/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	cmd.Dir = "/"
	cmd.Stderr = os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	return cmd
}
