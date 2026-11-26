package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// This separate gate prevents an ordinary correctness run from invoking an
// external oracle. The operator must coordinate the finite campaign first.
func TestDcmodifyFiniteDCMTKOracle(t *testing.T) {
	if os.Getenv("DICOMGO_DCMODIFY_PARITY_ORACLE") != "1" {
		t.Skip("external oracle requires a separately coordinated campaign")
	}
	tool := os.Getenv("DICOMGO_DCMODIFY_REFERENCE")
	if tool == "" {
		t.Fatal("explicit reference binary required")
	}
	dir := t.TempDir()
	ours := dc1353Input(t, dir, "ours.dcm", transfer.ExplicitVRLittleEndian)
	theirs := dc1353Input(t, dir, "theirs.dcm", transfer.ExplicitVRLittleEndian)
	// Four deterministic jobs; no generated UID equality or release-specific
	// process-if assumption. Each subprocess is independently bounded.
	jobs := [][]string{{"-i", "ReferencedStudySequence[0].PatientName=ONE"}, {"-ma", "PatientName=ALL"}, {"-ea", "ReferencedStudySequence"}, {"-m", "PatientName=LAST"}}
	for _, job := range jobs {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		args := append([]string{"-nb"}, job...)
		args = append(args, theirs)
		cmd := exec.CommandContext(ctx, tool, args...)
		cmd.WaitDelay = time.Second
		out := &dc1353BoundedOutput{remaining: 1 << 20}
		cmd.Stdout = out
		cmd.Stderr = out
		err := cmd.Run()
		cancel()
		if err != nil {
			t.Fatal("reference execution rejected")
		}
		args = append(append([]string(nil), job...), "-in-place", ours)
		if dc1353Run(t, args...) != 0 {
			t.Fatal("local edit rejected")
		}
	}
	a := dc1353Read(t, ours)
	b := dc1353Read(t, theirs)
	for _, tag := range []core.Tag{core.NewTag(0x0010, 0x0010), core.NewTag(0x0008, 0x1110)} {
		av, aok := a.Dataset.Get(tag)
		bv, bok := b.Dataset.Get(tag)
		if aok != bok || aok && av.StringValue() != bv.StringValue() {
			t.Fatal("reference attributes differ")
		}
	}
}

type dc1353BoundedOutput struct {
	mu        sync.Mutex
	remaining int
}

func (w *dc1353BoundedOutput) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(p) > w.remaining {
		return 0, os.ErrInvalid
	}
	w.remaining -= len(p)
	return len(p), nil
}

// A real compiled CLI is supplied by the correctness campaign; ordinary
// package tests do not recursively invoke go build or use an external tool.
func TestDcmodifyExecutableE2E(t *testing.T) {
	binary := os.Getenv("DICOMGO_DCMODIFY_E2E_BINARY")
	if binary == "" {
		t.Skip("compiled CLI supplied by correctness campaign")
	}
	dir := t.TempDir()
	input := dc1353Input(t, dir, "in.dcm", transfer.ExplicitVRBigEndian)
	output := filepath.Join(dir, "out.dcm")
	value := filepath.Join(dir, "rows.raw")
	os.WriteFile(value, []byte{0x34, 0x12}, 0o600)
	before, _ := os.ReadFile(input)
	runBinary := func(args ...string) error {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, binary, args...)
		cmd.WaitDelay = time.Second
		out := &dc1353BoundedOutput{remaining: 1 << 20}
		cmd.Stdout = out
		cmd.Stderr = out
		return cmd.Run()
	}
	if e := runBinary("-gin", "-if", "Rows="+value, "-i", "ReferencedStudySequence[0].PatientName=ONE", "-ma", "PatientName=ALL", "-o", output, input); e != nil {
		t.Fatal(e)
	}
	f, e := object.OpenFile(output)
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	rows, e := f.Dataset.GetInts(core.NewTag(0x0028, 0x0010))
	if e != nil || len(rows) != 1 || rows[0] != 0x1234 {
		t.Fatal("real byte-order output incorrect")
	}
	uid, _ := f.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	meta, _ := f.Meta.GetString(core.NewTag(0x0002, 0x0003))
	if !core.IsValidUID(uid) || uid != meta {
		t.Fatal("real UID/meta mismatch")
	}
	after, _ := os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("real CLI modified source")
	}
	if e := runBinary("-if", "Rows="+value, "-max-edit-value-bytes", "1", "-in-place", input); e == nil {
		t.Fatal("real CLI accepted cap failure")
	}
	after, _ = os.ReadFile(input)
	if !bytes.Equal(before, after) {
		t.Fatal("real cap failure modified source")
	}
	dc1353NoTemps(t, dir)
	// Exercise actual operand batching, skip and absent-file creation through
	// the process entry point, rather than only the package's run adapter.
	second := dc1353Input(t, dir, "second.dcm", transfer.ExplicitVRLittleEndian)
	if err := runBinary("-gst", "-gin", "-in-place", input, second); err != nil {
		t.Fatal(err)
	}
	a := dc1353Read(t, input)
	b := dc1353Read(t, second)
	ua, _ := a.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	ub, _ := b.Dataset.GetString(core.NewTag(0x0008, 0x0018))
	if ua == ub {
		t.Fatal("real batch reused UID")
	}
	skipped := filepath.Join(dir, "skipped.dcm")
	if err := runBinary("-iff", "PatientID", "-gin", "-o", skipped, input); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(skipped); !os.IsNotExist(err) {
		t.Fatal("real false condition published")
	}
	created := filepath.Join(dir, "created.dcm")
	if err := runBinary("+fc", "-i", "SOPClassUID=1.2.840.10008.5.1.4.1.1.7", "-gin", "-in-place", created); err != nil {
		t.Fatal(err)
	}
	c := dc1353Read(t, created)
	if c.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
		t.Fatal("real creation syntax incorrect")
	}
	if err := runBinary("-ep", "-in-place", created); err != nil {
		t.Fatal("real private consumer unavailable")
	}
	dc1353NoTemps(t, dir)
}
