//go:build darwin || linux

package main

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"
)

func storageHookPlatformReady() error { return nil }
func storageExecutablePlatform(_ string, info os.FileInfo) error {
	if info.Mode().Perm()&0o111 == 0 {
		return errStorageFeatureUsage
	}
	return nil
}
func runStorageHookPlatform(ctx context.Context, argv []string, directory string, output io.Writer) error {
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	cmd.Dir = directory
	cmd.Env = []string{"LANG=C", "LC_ALL=C", "TZ=UTC"}
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 2 * time.Second
	kill := func() error {
		if cmd.Process == nil {
			return nil
		}
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.Cancel = kill
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	defer func() { _ = kill() }()
	return cmd.Wait()
}

func storageHookArgumentProfile(_ *storageHookSpec, _ storageEventFields) error { return nil }
