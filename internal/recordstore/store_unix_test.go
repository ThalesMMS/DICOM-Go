//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd

package recordstore

import (
	"bufio"
	"context"
	"golang.org/x/sys/unix"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestStoreFIFORefusalDoesNotBlock(t *testing.T) {
	for _, name := range []string{lockName, key(0)} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			if err := unix.Mkfifo(filepath.Join(dir, name), 0600); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() {
				s, err := Open(context.Background(), dir, testOptions)
				if s != nil {
					s.Close()
				}
				done <- err
			}()
			select {
			case err := <-done:
				if err == nil {
					t.Fatal("FIFO admitted")
				}
			case <-time.After(time.Second):
				t.Fatal("FIFO open blocked")
			}
		})
	}
}

func TestStoreCrashReleasesKernelLock(t *testing.T) {
	if dir := os.Getenv("DICOM_RECORDSTORE_CHILD_DIR"); dir != "" {
		s, err := Open(context.Background(), dir, testOptions)
		if err != nil {
			os.Exit(3)
		}
		defer s.Close()
		if _, err := s.Create(context.Background(), key(0), []byte("durable")); err != nil {
			os.Exit(4)
		}
		io.WriteString(os.Stdout, "ready\n")
		io.Copy(io.Discard, os.Stdin)
		return
	}
	dir := filepath.Join(t.TempDir(), "records")
	cmd := exec.Command(os.Args[0], "-test.run=^TestStoreCrashReleasesKernelLock$")
	cmd.Env = append(os.Environ(), "DICOM_RECORDSTORE_CHILD_DIR="+dir)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	defer func() {
		stdin.Close()
		if !waited {
			cmd.Process.Kill()
			cmd.Wait()
		}
	}()
	ready := make(chan string, 1)
	go func() { line, _ := bufio.NewReader(stdout).ReadString('\n'); ready <- line }()
	select {
	case line := <-ready:
		if line != "ready\n" {
			t.Fatalf("child readiness %q", line)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("child timeout")
	}
	if s, err := Open(context.Background(), dir, testOptions); err == nil {
		s.Close()
		t.Fatal("second writer admitted")
	}
	cmd.Process.Kill()
	cmd.Wait()
	waited = true
	s := openTest(t, dir)
	data, err := s.Read(context.Background(), key(0))
	if err != nil || string(data) != "durable" {
		t.Fatalf("crash replay %q %v", data, err)
	}
}
