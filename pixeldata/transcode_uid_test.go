package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const testTranscodeRoot = "1.2.826.0.1.3680043.10.1352"

func checkRootedSOP(t *testing.T, file *object.File, original string) string {
	t.Helper()
	uid, ok := file.Dataset.GetUID(core.NewTag(8, 0x18))
	if !ok || !core.IsValidUID(uid) || len(uid) > 64 || uid == original || !strings.HasPrefix(uid, testTranscodeRoot+".") {
		t.Fatalf("new SOP UID violates root contract: %q", uid)
	}
	meta, ok := file.Meta.GetUID(core.NewTag(2, 3))
	if !ok || meta != uid {
		t.Fatal("File Meta identity differs")
	}
	sources, ok := file.Dataset.GetSequence(core.NewTag(8, 0x2112))
	if !ok || len(sources) < 1 {
		t.Fatal("original source reference missing")
	}
	for _, source := range sources {
		ref, ok := source.GetUID(core.NewTag(8, 0x1155))
		if !ok || ref != original {
			t.Fatal("original predecessor reference remapped")
		}
	}
	return uid
}

func TestTranscodeUIDRootAcrossMaterializedAndStreaming(t *testing.T) {
	for _, tc := range []struct {
		name      string
		target    transfer.Syntax
		lossy, sc bool
	}{
		{"equivalent-copy", transfer.ExplicitVRLittleEndian, false, false},
		{"lossless", transfer.JPEGLSLossless, false, false},
		{"lossy", transfer.JPEGBaseline, true, false},
		{"secondary", transfer.ExplicitVRLittleEndian, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := outputOptionsFile(t, 1, 1, 8)
			original, _ := input.Dataset.GetUID(core.NewTag(8, 0x18))
			study, _ := input.Dataset.GetUID(core.NewTag(0x20, 0x0d))
			series, _ := input.Dataset.GetUID(core.NewTag(0x20, 0x0e))
			before := outputOptionsBytes(t, input)
			opts := streamTestRegistries(t)
			opts.UIDRoot = testTranscodeRoot
			opts.UIDPolicy = pixeldata.TranscodeUIDAlways
			opts.AllowLossy = tc.lossy
			opts.SecondaryCapture = tc.sc
			mat, report, err := pixeldata.TranscodeFile(context.Background(), input, tc.target, opts)
			if err != nil {
				t.Fatal(err)
			}
			if report.Lossy != tc.lossy {
				t.Fatalf("lossy provenance %+v", report)
			}
			first := checkRootedSOP(t, mat, original)
			if got, _ := mat.Dataset.GetUID(core.NewTag(0x20, 0x0d)); got != study {
				t.Fatal("Study UID changed")
			}
			if got, _ := mat.Dataset.GetUID(core.NewTag(0x20, 0x0e)); got != series {
				t.Fatal("Series UID changed")
			}
			dir := t.TempDir()
			source := writeStreamTestFile(t, dir, "source.dcm", input)
			seen := map[string]bool{first: true}
			for _, workers := range []int{1, 3} {
				opts.Pipeline.Workers = workers
				output := filepath.Join(dir, "out.dcm")
				if _, err := pixeldata.TranscodePath(context.Background(), source, output, tc.target, opts); err != nil {
					t.Fatal(err)
				}
				file, err := object.OpenFile(output)
				if err != nil {
					t.Fatal(err)
				}
				uid := checkRootedSOP(t, file, original)
				file.Close()
				if seen[uid] {
					t.Fatal("new calls reused UID")
				}
				seen[uid] = true
			}
			if !bytes.Equal(before, outputOptionsBytes(t, input)) {
				t.Fatal("source object mutated")
			}
			disk, err := os.ReadFile(source)
			if err != nil || !bytes.Equal(disk, before) {
				t.Fatal("original source file changed")
			}
		})
	}
}

func TestTranscodeUIDRootCopyDefaultAndAdmission(t *testing.T) {
	input := outputOptionsFile(t, 1, 1, 8)
	original, _ := input.Dataset.GetUID(core.NewTag(8, 0x18))
	for _, root := range []string{"", testTranscodeRoot} {
		result, _, err := pixeldata.TranscodeFile(nil, input, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{UIDRoot: root})
		if err != nil {
			t.Fatal(err)
		}
		got, _ := result.Dataset.GetUID(core.NewTag(8, 0x18))
		if got != original {
			t.Fatal("root unexpectedly forced new UID on default copy")
		}
	}
	for _, root := range []string{"bad", "1.02", "1.2.840.10008", "1.2.840.10008.999", "2.25", "2.25.123", strings.Repeat("1", 44)} {
		if _, _, err := pixeldata.TranscodeFile(nil, input, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{UIDRoot: root}); !errors.Is(err, core.ErrUIDRoot) {
			t.Fatalf("bad root accepted: %q %v", root, err)
		}
		output := filepath.Join(t.TempDir(), "absent.dcm")
		if _, err := pixeldata.TranscodePath(nil, "no-input", output, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{UIDRoot: root}); !errors.Is(err, core.ErrUIDRoot) {
			t.Fatalf("root not checked before input: %v", err)
		}
		if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("invalid root published")
		}
	}
	if _, _, err := pixeldata.TranscodeFile(nil, input, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{UIDRoot: testTranscodeRoot, UIDPolicy: pixeldata.TranscodeUIDPreserve}); !errors.Is(err, pixeldata.ErrTranscodeUnsupported) {
		t.Fatal("inert root+preserve accepted")
	}
}

func TestTranscodeUIDRootConcurrentCallsAreIndependent(t *testing.T) {
	const count = 12
	files := make([]*object.File, count)
	for i := range files {
		files[i] = outputOptionsFile(t, 1, 1, 8)
	}
	var wg sync.WaitGroup
	for i := range files {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			root := testTranscodeRoot + ".1"
			if i%2 != 0 {
				root = testTranscodeRoot + ".2"
			}
			result, _, err := pixeldata.TranscodeFile(nil, files[i], transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{UIDRoot: root, UIDPolicy: pixeldata.TranscodeUIDAlways})
			if err != nil {
				t.Error(err)
				return
			}
			uid, _ := result.Dataset.GetUID(core.NewTag(8, 0x18))
			if !strings.HasPrefix(uid, root+".") {
				t.Error("cross-call root changed")
			}
		}(i)
	}
	wg.Wait()
}
