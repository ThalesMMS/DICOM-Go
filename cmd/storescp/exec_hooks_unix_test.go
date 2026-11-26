//go:build darwin || linux

package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func init() { storageHookExtraHelper = storageHookUnixExtraHelper }
func storageHookUnixExtraHelper(mode string, args []string) bool {
	if mode != "descendant" && mode != "wait-child" {
		return false
	}
	if len(args) < 1 {
		os.Exit(87)
	}
	if mode == "wait-child" {
		_ = os.WriteFile(args[0], []byte(strconv.Itoa(os.Getpid())), 0600)
		time.Sleep(time.Minute)
		return true
	}
	cmd := exec.Command(os.Args[0], "-test.run=^TestStorageHookProcessHelper$", "--", "--storescp-hook-helper", "wait-child", args[0])
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = os.Environ()
	if err := cmd.Start(); err != nil {
		os.Exit(88)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if waitStorageFile(ctx, args[0]) != nil {
		os.Exit(89)
	}
	// A normal child inherits the private group. The parent exits without waiting;
	// the runner must kill the group and bound pipes held by that child.
	return true
}
func TestStorageHookUnixCleansDescendantAfterDirectExit(t *testing.T) {
	root := t.TempDir()
	pidPath := filepath.Join(root, "child.pid")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	_ = runStorageHook(ctx, storageHelperArgv(t, "descendant", pidPath), root)
	if time.Since(started) > 4*time.Second {
		t.Fatal("descendant pipes exceeded bounded cleanup")
	}
	b, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(b))
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		err = syscall.Kill(pid, 0)
		if err == syscall.ESRCH {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("descendant not reaped/absent pid=%d: %v", pid, err)
		case <-ticker.C:
		}
	}
}
