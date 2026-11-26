package dicomdir_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/dicomdir"
)

func TestDICOMDIRExactOutputBudget(t *testing.T) {
	for _, test := range []struct {
		name           string
		shrinkImported bool
	}{
		{name: "fresh"},
		{name: "shrink existing"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			root, candidate, specs := filesetSyntheticFileSet(t)
			directory := filepath.Join(root, "DICOMDIR")
			var previous []byte
			expectedRecords := len(specs)
			if test.shrinkImported {
				if _, err := dicomdir.CommitDICOMDIR(ctx, candidate, dicomdir.WriteOptions{}); err != nil {
					t.Fatal(err)
				}
				previous = filesetReadBytes(t, directory)
				var err error
				candidate, _, err = dicomdir.PrepareFileSetUpdate(ctx, directory, nil, dicomdir.FileSetUpdateOptions{
					Mode: dicomdir.FileSetAppend, MaxBytes: int64(len(previous)),
				})
				if err != nil {
					t.Fatalf("prepare at the existing directory's exact size: %v", err)
				}
				for _, spec := range specs[1:] {
					if !candidate.Remove(spec.fileID) {
						t.Fatalf("remove %v from candidate", spec.fileID)
					}
				}
				expectedRecords = 1
			}
			sources := make(map[string][]byte, len(specs))
			for _, spec := range specs {
				path := filepath.Join(append([]string{root}, spec.fileID...)...)
				sources[path] = filesetReadBytes(t, path)
			}
			want := filesetWriteBytes(t, candidate)
			if test.shrinkImported && len(want) >= len(previous) {
				t.Fatalf("candidate size %d must be smaller than baseline %d", len(want), len(previous))
			}

			short := dicomdir.WriteOptions{MaxBytes: int64(len(want) - 1)}
			var streamed bytes.Buffer
			if report, err := dicomdir.WriteDICOMDIR(ctx, &streamed, candidate, short); !errors.Is(err, dicomdir.ErrResourceLimit) || report.BytesWritten != 0 || streamed.Len() != 0 {
				t.Fatalf("one-byte-short write wrote %d bytes with report %+v and error %v", streamed.Len(), report, err)
			}
			if report, err := dicomdir.CommitDICOMDIR(ctx, candidate, short); !errors.Is(err, dicomdir.ErrResourceLimit) || report.BytesWritten != 0 {
				t.Fatalf("one-byte-short commit returned report %+v and error %v", report, err)
			}
			if previous == nil {
				if _, err := os.Lstat(directory); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("rejected commit left a DICOMDIR: %v", err)
				}
			} else if !bytes.Equal(filesetReadBytes(t, directory), previous) {
				t.Fatal("rejected commit modified the previous directory")
			}

			exact := dicomdir.WriteOptions{MaxBytes: int64(len(want))}
			report, err := dicomdir.CommitDICOMDIR(ctx, candidate, exact)
			if err != nil || report.BytesWritten != int64(len(want)) || report.SHA256 != sha256.Sum256(want) {
				t.Fatalf("exact-size commit returned report %+v and error %v", report, err)
			}
			if !bytes.Equal(filesetReadBytes(t, directory), want) {
				t.Fatal("committed directory differs from streamed bytes")
			}
			parsed, err := dicomdir.ParseFileSet(directory)
			if err != nil {
				t.Fatal(err)
			}
			if got := len(parsed.Records()); got != expectedRecords {
				t.Fatalf("got %d indexed file references, want %d", got, expectedRecords)
			}
			for path, source := range sources {
				if !bytes.Equal(filesetReadBytes(t, path), source) {
					t.Fatalf("source changed: %s", path)
				}
			}
			if temps, err := filepath.Glob(filepath.Join(root, ".DICOMDIR.tmp-*")); err != nil || len(temps) != 0 {
				t.Fatalf("commit left staging entries %v: %v", temps, err)
			}
		})
	}
}

func TestPrepareDICOMDIRBudgetDoesNotAcceptIncompleteFraming(t *testing.T) {
	root, set, _ := filesetSyntheticFileSet(t)
	path := filepath.Join(root, "DICOMDIR")
	valid := filesetWriteBytes(t, set)
	ctx := context.Background()
	for _, test := range []struct {
		name string
		wire []byte
	}{
		{name: "truncated directory", wire: valid[:len(valid)-1]},
		{name: "incomplete trailing tag", wire: append(bytes.Clone(valid), 0)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(path, test.wire, 0o600); err != nil {
				t.Fatal(err)
			}
			candidate, _, err := dicomdir.PrepareFileSetUpdate(ctx, path, nil, dicomdir.FileSetUpdateOptions{
				Mode: dicomdir.FileSetAppend, MaxBytes: int64(len(test.wire)),
			})
			if candidate != nil || err == nil {
				t.Fatalf("malformed exact-size input returned candidate=%t, error %v", candidate != nil, err)
			}
			if !bytes.Equal(filesetReadBytes(t, path), test.wire) {
				t.Fatal("preparation modified the directory")
			}
		})
	}
	if err := os.WriteFile(path, valid, 0o600); err != nil {
		t.Fatal(err)
	}
	candidate, _, err := dicomdir.PrepareFileSetUpdate(ctx, path, nil, dicomdir.FileSetUpdateOptions{
		Mode: dicomdir.FileSetAppend, MaxBytes: int64(len(valid) - 1),
	})
	if candidate != nil || !errors.Is(err, dicomdir.ErrResourceLimit) {
		t.Fatalf("physical input exceeds the budget: candidate=%t, error %v", candidate != nil, err)
	}
}
