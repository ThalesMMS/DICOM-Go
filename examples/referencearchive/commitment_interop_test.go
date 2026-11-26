package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestReferenceArchiveIndependentStorageCommitment has pynetdicom 3.0.4 act
// as the requestor: C-STORE, N-ACTION, release, N-EVENT-REPORT on a separate
// association (Event Type 2 with a 0112H failure, then Event Type 1), and a
// C-GET whose content matches what was committed. Opt-in with
// DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON (#903 peer profile).
func TestReferenceArchiveIndependentStorageCommitment(t *testing.T) {
	if os.Getenv("DICOMGO_PYNETDICOM_INTEGRATION") == "" {
		t.Skip("opt-in: DICOMGO_PYNETDICOM_INTEGRATION=1 and DICOMGO_PYTHON; uses the #903 pinned peer profile")
	}
	python := os.Getenv("DICOMGO_PYTHON")
	if python == "" {
		python = "python3"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, python, "-u", "testdata/commit_peer.py")
	input, err := command.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	command.Stderr = &stderr
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = command.Process.Kill(); _ = command.Wait() }()
	scanner := bufio.NewScanner(output)
	if !scanner.Scan() {
		t.Fatalf("peer start: %s", stderr.String())
	}
	fields := strings.Fields(scanner.Text())
	if len(fields) != 2 || fields[0] != "PORT" {
		t.Fatal("invalid peer readiness")
	}
	running, _ := startCommitArchive(t, ctx, t.TempDir(), t.TempDir(), map[string]string{commitRequestorAE: "127.0.0.1:" + fields[1]}, true)
	port := running.address[strings.LastIndex(running.address, ":")+1:]
	if _, err := fmt.Fprintln(input, port); err != nil {
		t.Fatal(err)
	}
	var last string
	for scanner.Scan() {
		last = scanner.Text()
	}
	if err := command.Wait(); err != nil || last != "OK" {
		t.Fatalf("pynetdicom requestor: %v (%q)\n%s", err, last, stderr.String())
	}
}
