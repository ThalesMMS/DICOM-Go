package pixeldata

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestTranscodePipelineWindowChargesAndBackpressure(t *testing.T) {
	options := TranscodeOptions{Pipeline: TranscodePipelineOptions{Workers: 4, MaxInFlightFrames: 3, MaxInFlightBytes: 100, MaxEncodedFrameBytes: 2}}
	p, err := newTranscodeFramePipeline(options, 7, 1, 100, true, func(int) (int64, error) { return 1, nil })
	if err != nil {
		t.Fatal(err)
	}
	charge, err := p.charge(0)
	if err != nil {
		t.Fatal(err)
	}
	p.options.MaxInFlightBytes = 2 * charge
	var produced, consumed atomic.Int64
	var report TranscodeReport
	err = p.run(context.Background(), 7, &report, func(_ context.Context, i int) (transcodeFrameResult, error) {
		if produced.Add(1)-consumed.Load() > 2 {
			t.Error("admitted next window before consuming current output")
		}
		return transcodeFrameResult{data: []byte{byte(i)}}, nil
	}, func(i int, r transcodeFrameResult) error {
		if len(r.data) != 1 || int(r.data[0]) != i {
			t.Error("completion reordered output")
		}
		consumed.Add(1)
		return nil
	})
	if err != nil || report.PeakInFlightFrames != 2 || report.PeakInFlightBytes != 2*charge || consumed.Load() != 7 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
	for _, raw := range []int64{0, -1, math.MaxInt64} {
		p.frameRawSize = func(int) (int64, error) { return raw, nil }
		if _, err := p.charge(0); !errors.Is(err, ErrTranscodeResourceLimit) {
			t.Fatalf("raw=%d: %v", raw, err)
		}
	}
}

func TestTranscodePipelineDiskFullShortWriteAndWriteCancel(t *testing.T) {
	if !transcodePathSupported() {
		t.Skip("atomic publication unavailable")
	}
	for _, kind := range []string{"disk-full", "short-write", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			input, out := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
			var source bytes.Buffer
			if err := object.WriteFile(&source, stageTestPart10File()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, source.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			canary := []byte("destination must survive")
			if err := os.WriteFile(out, canary, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			previous := transcodeSpoolWriteAt
			defer func() { transcodeSpoolWriteAt = previous }()
			transcodeSpoolWriteAt = func(_ *os.File, data []byte, _ int64) (int, error) {
				switch kind {
				case "disk-full":
					return 0, syscall.ENOSPC
				case "short-write":
					return len(data) - 1, nil
				default:
					cancel()
					return 0, context.Canceled
				}
			}
			// Encapsulated Uncompressed needs no external codec but exercises
			// ordered native-frame spool and its full failure cleanup path.
			_, err := TranscodePath(ctx, input, out, transfer.EncapsulatedUncompressedExplicitVRLittleEndian,
				TranscodeOptions{Pipeline: TranscodePipelineOptions{Workers: 2}})
			if kind == "cancel" && !errors.Is(err, context.Canceled) || kind != "cancel" && !errors.Is(err, ErrTranscodeTransaction) {
				t.Fatalf("error=%v", err)
			}
			got, _ := os.ReadFile(out)
			if !bytes.Equal(got, canary) {
				t.Fatal("failed spool published output")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Fatalf("retained temporary: %v", entries)
			}
		})
	}
}

func TestTranscodeSerialNativeTargetDoesNotUseSpool(t *testing.T) {
	if !transcodePathSupported() {
		t.Skip("atomic publication unavailable")
	}
	dir := t.TempDir()
	input := filepath.Join(dir, "in.dcm")
	var source bytes.Buffer
	if err := object.WriteFile(&source, stageTestPart10File()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	previous := transcodeSpoolWriteAt
	defer func() { transcodeSpoolWriteAt = previous }()
	transcodeSpoolWriteAt = func(*os.File, []byte, int64) (int, error) {
		t.Error("serial decode attempted a spool write")
		return 0, syscall.ENOSPC
	}
	for _, workers := range []int{0, 1} {
		report, err := TranscodePath(context.Background(), input, filepath.Join(t.TempDir(), "out.dcm"),
			transfer.EncapsulatedUncompressedExplicitVRLittleEndian, TranscodeOptions{Pipeline: TranscodePipelineOptions{Workers: workers}})
		if err != nil || report.WorkerLimit != 1 || report.PeakInFlightFrames != 1 || report.Timings.SpoolWriteTime != 0 || !report.FileSynced {
			t.Fatalf("serial report=%+v err=%v", report, err)
		}
	}
}

func TestTranscodeDurabilitySyncFailureAndExplicitAtomicOptOut(t *testing.T) {
	if !transcodePathSupported() {
		t.Skip("atomic publication unavailable")
	}
	for _, policy := range []TranscodeDurability{TranscodeDurable, TranscodeAtomic} {
		t.Run(policy.String(), func(t *testing.T) {
			dir := t.TempDir()
			input, out := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
			var source bytes.Buffer
			if err := object.WriteFile(&source, stageTestPart10File()); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(input, source.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			canary := []byte("original")
			if err := os.WriteFile(out, canary, 0600); err != nil {
				t.Fatal(err)
			}
			var calls int
			previous := syncTranscodeStagedFile
			defer func() { syncTranscodeStagedFile = previous }()
			syncTranscodeStagedFile = func(*os.File) error { calls++; return syscall.ENOSPC }
			report, err := TranscodePath(context.Background(), input, out, transfer.ImplicitVRLittleEndian, TranscodeOptions{Durability: policy})
			got, _ := os.ReadFile(out)
			if policy == TranscodeDurable {
				if !errors.Is(err, ErrTranscodeTransaction) || calls != 1 || !bytes.Equal(got, canary) {
					t.Fatalf("default sync failure: report=%+v err=%v calls=%d", report, err, calls)
				}
			} else if err != nil || calls != 0 || bytes.Equal(got, canary) || report.FileSynced || report.DirectorySynced {
				t.Fatalf("atomic: report=%+v err=%v calls=%d", report, err, calls)
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Fatalf("temporary retained: %v", entries)
			}
		})
	}
}

func TestTranscodeDurabilityPostPublishFailureCarriesPolicyReport(t *testing.T) {
	if !transcodePathSupported() {
		t.Skip("atomic publication unavailable")
	}
	dir := t.TempDir()
	input, output := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
	var source bytes.Buffer
	if err := object.WriteFile(&source, stageTestPart10File()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	previous := syncTranscodeDestinationDirectory
	defer func() { syncTranscodeDestinationDirectory = previous }()
	syncTranscodeDestinationDirectory = func(*os.File) error { return syscall.ENOSPC }
	report, err := TranscodePath(context.Background(), input, output, transfer.ImplicitVRLittleEndian, TranscodeOptions{})
	var failure *TranscodeError
	if !errors.As(err, &failure) || failure.Stage != "durability" || !report.FileSynced || report.DirectorySynced || report.Durability != TranscodeDurable || report.Frames != 1 {
		t.Fatalf("post-publish report=%+v err=%v", report, err)
	}
	if _, err := os.Stat(output); err != nil {
		t.Fatal("durability failure rolled back completed publication")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 2 {
		t.Fatalf("retained temporary: %v", entries)
	}
}

type pipelineVerifyCodec struct{ corrupt bool }

func (c pipelineVerifyCodec) Decode(_ PixelData, _ *object.Object) (Frames, error) {
	data := []byte{1, 2, 3, 4}
	if c.corrupt {
		data[3] = 5
	}
	return Frames{Data: [][]byte{data}, Rows: 2, Columns: 2}, nil
}

func TestTranscodePipelineVerificationFailurePreventsPublication(t *testing.T) {
	if !transcodePathSupported() {
		t.Skip("atomic publication unavailable")
	}
	dir := t.TempDir()
	input, out := filepath.Join(dir, "in.dcm"), filepath.Join(dir, "out.dcm")
	var source bytes.Buffer
	if err := object.WriteFile(&source, stageTestPart10File()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(input, source.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	registry := NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, pipelineVerifyCodec{corrupt: true}); err != nil {
		t.Fatal(err)
	}
	encoders := NewMemoryEncoderRegistry()
	encoder := &stageFrameEncoder{capabilities: EncoderCapabilities{TransferSyntaxUID: transfer.RLELossless.UID, BitsAllocated: []uint16{8}, PixelRepresentations: []uint16{0}, SamplesPerPixel: []uint16{1}, PhotometricInterpretations: []string{"MONOCHROME2"}, Lossless: true, SupportsMultiFrame: true}, encode: func(_ context.Context, _ []byte, _ Metadata) (EncodedFrame, error) {
		return EncodedFrame{Data: []byte{42, 43}}, nil
	}}
	if err := encoders.RegisterEncoder(transfer.RLELossless.UID, encoder); err != nil {
		t.Fatal(err)
	}
	_, err := TranscodePath(context.Background(), input, out, transfer.RLELossless, TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: registry})
	if !errors.Is(err, ErrIncompatiblePixelData) {
		t.Fatalf("verification error=%v", err)
	}
	if _, err := os.Stat(out); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("verification failure published output")
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary retained: %v", entries)
	}
}

func TestTranscodeBoundedWriterCancelDoesNotWrite(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "staged")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	w := &transcodeBoundedWriter{ctx: ctx, writer: f, remaining: 10}
	if n, err := w.Write([]byte{1}); n != 0 || !errors.Is(err, context.Canceled) {
		t.Fatalf("Write=(%d,%v)", n, err)
	}
	info, _ := f.Stat()
	if info.Size() != 0 {
		t.Fatal("canceled write modified staged data")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
}

func TestTranscodePipelineRejectsInvalidPolicies(t *testing.T) {
	for _, options := range []TranscodeOptions{
		{Durability: TranscodeDurability(255)},
		{Pipeline: TranscodePipelineOptions{Workers: -1}},
		{Pipeline: TranscodePipelineOptions{MaxInFlightFrames: -1}},
		{Pipeline: TranscodePipelineOptions{MaxInFlightBytes: -1}},
		{Pipeline: TranscodePipelineOptions{MaxEncodedFrameBytes: -1}},
	} {
		if err := validateTranscodePolicy(options); err == nil {
			t.Fatal("accepted invalid policy")
		}
	}
}
