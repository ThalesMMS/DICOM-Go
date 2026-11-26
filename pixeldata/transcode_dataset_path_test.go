package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func rawTranscodeFixture(t testing.TB, dir string, source transfer.Syntax) (string, *object.Object) {
	t.Helper()
	dataset := streamTestSource(16, 16, 5, 1, 16)
	var raw bytes.Buffer
	if err := object.WriteDataSet(&raw, dataset, source); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "source.dataset")
	if err := os.WriteFile(path, raw.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	return path, dataset
}

func TestTranscodeDataSetPathLosslessFrameRoute(t *testing.T) {
	for _, target := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.RLELossless, transfer.JPEGLSLossless} {
		t.Run(target.UID, func(t *testing.T) {
			dir := t.TempDir()
			input, want := rawTranscodeFixture(t, dir, transfer.ExplicitVRLittleEndian)
			output := filepath.Join(dir, "output.dcm")
			opts := streamTestRegistries(t)
			report, err := pixeldata.TranscodeDataSetPath(context.Background(), input, output, transfer.ExplicitVRLittleEndian, target, opts, pixeldata.DataSetPathOptions{MaxTemporaryBytes: 1 << 20})
			if err != nil || !report.Streaming || report.PeakTemporaryBytes <= 0 || report.PeakTemporaryBytes > 1<<20 {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			file, err := object.OpenFile(output)
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			decoded := file.Dataset
			if target.Encapsulated {
				decoded, _, err = pixeldata.TranscodeDataSet(context.Background(), decoded, target, transfer.ExplicitVRLittleEndian, opts)
				if err != nil {
					t.Fatal(err)
				}
				defer decoded.Close()
			}
			p, _ := decoded.Get(core.TagPixelData)
			got, _ := p.RawBytes()
			p, _ = want.Get(core.TagPixelData)
			expected, _ := p.RawBytes()
			if !bytes.Equal(got, expected) {
				t.Fatal("decoded frame samples differ")
			}
			uid, _ := decoded.GetUID(core.NewTag(0x0008, 0x0018))
			wantedUID, _ := want.GetUID(core.NewTag(0x0008, 0x0018))
			if uid != wantedUID {
				t.Fatal("lossless identity changed")
			}
		})
	}
}

func TestTranscodeDataSetPathTemporaryPeakFailureKeepsDestination(t *testing.T) {
	dir := t.TempDir()
	input, _ := rawTranscodeFixture(t, dir, transfer.ExplicitVRLittleEndian)
	output := filepath.Join(dir, "output.dcm")
	opts := streamTestRegistries(t)
	qualified, err := pixeldata.TranscodeDataSetPath(context.Background(), input, output, transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts, pixeldata.DataSetPathOptions{MaxTemporaryBytes: 1 << 20})
	if err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(output)
	if err != nil {
		t.Fatal(err)
	}
	_, err = pixeldata.TranscodeDataSetPath(context.Background(), input, output, transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts, pixeldata.DataSetPathOptions{MaxTemporaryBytes: qualified.PeakTemporaryBytes - 1})
	if !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
		t.Fatalf("expected aggregate frame/output spool limit: %v", err)
	}
	after, err := os.ReadFile(output)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("failed staging changed destination")
	}
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 2 {
		t.Fatalf("temporary spool retained: %v %v", entries, err)
	}
}

func TestTranscodeDataSetPathIneligibleAndCanceledDoNotPublish(t *testing.T) {
	dir := t.TempDir()
	input, _ := rawTranscodeFixture(t, dir, transfer.ExplicitVRLittleEndian)
	output := filepath.Join(dir, "output.dcm")
	opts := streamTestRegistries(t)
	opts.AllowLossy = true
	_, err := pixeldata.TranscodeDataSetPath(context.Background(), input, output, transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts, pixeldata.DataSetPathOptions{})
	if !errors.Is(err, pixeldata.ErrTranscodeStreamingIneligible) {
		t.Fatalf("expected no-work resident policy: %v", err)
	}
	opts.AllowLossy = false
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = pixeldata.TranscodeDataSetPath(ctx, input, output, transfer.ExplicitVRLittleEndian, transfer.RLELossless, opts, pixeldata.DataSetPathOptions{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation classified as ineligibility: %v", err)
	}
	if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("unqualified/canceled operation published")
	}
}

func TestTranscodeDataSetPathSectionOffset(t *testing.T) {
	dir := t.TempDir()
	input, want := rawTranscodeFixture(t, dir, transfer.ExplicitVRLittleEndian)
	raw, err := os.ReadFile(input)
	if err != nil {
		t.Fatal(err)
	}
	prefix := []byte("private snapshot prefix")
	if err := os.WriteFile(input, append(prefix, raw...), 0600); err != nil {
		t.Fatal(err)
	}
	output := filepath.Join(dir, "output.dcm")
	_, err = pixeldata.TranscodeDataSetPath(context.Background(), input, output, transfer.ExplicitVRLittleEndian, transfer.RLELossless, streamTestRegistries(t), pixeldata.DataSetPathOptions{SourceOffset: int64(len(prefix))})
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.OpenFile(output)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	uid, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	expected, _ := want.GetUID(core.NewTag(0x0008, 0x0018))
	if uid != expected {
		t.Fatal("section framing changed identity")
	}
}
