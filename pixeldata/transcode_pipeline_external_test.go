package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/deflateframe"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func pipelineTestOptions(t testing.TB) pixeldata.TranscodeOptions {
	t.Helper()
	options := streamTestRegistries(t)
	if err := deflateframe.RegisterEncoder(options.EncoderRegistry); err != nil {
		t.Fatal(err)
	}
	return options
}

// These adapters let exactly the same campaign source run against the pinned
// pre-pipeline library using a task-local legacy adapter. The old adapter only
// advertises semantics the old API actually supports; unsupported cells are
// omitted rather than relabelled as equivalent work.
const transcode1294Implementation = "candidate: bounded pipeline and explicit policy API"

func transcode1294Options(t testing.TB) pixeldata.TranscodeOptions { return pipelineTestOptions(t) }
func transcode1294Supported(_ int, _ bool, _ string) bool          { return true }
func transcode1294Run(ctx context.Context, input, output string, target transfer.Syntax, options pixeldata.TranscodeOptions, workers int, verify bool, durability string) (pixeldata.TranscodeReport, error) {
	options.Pipeline.Workers = workers
	options.SkipVerification = !verify
	if durability == "atomic" || durability == "setup" {
		options.Durability = pixeldata.TranscodeAtomic
	}
	return pixeldata.TranscodePath(ctx, input, output, target, options)
}

func TestTranscodePipelineFourCodecsByteExactAndOrdered(t *testing.T) {
	options := pipelineTestOptions(t)
	dir := t.TempDir()
	native := writeStreamTestFile(t, dir, "native.dcm", &object.File{
		Dataset: streamTestSource(32, 40, 9, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	for _, syntax := range []transfer.Syntax{transfer.RLELossless, transfer.JPEGLSLossless, transfer.JPEGLosslessSV1, transfer.DeflatedImageFrameCompression} {
		t.Run(syntax.Name, func(t *testing.T) {
			serial := options
			serial.Pipeline.Workers = 1
			parallel := options
			parallel.Pipeline = pixeldata.TranscodePipelineOptions{Workers: 4, MaxInFlightFrames: 3, MaxInFlightBytes: 2 << 20}
			serialPath, parallelPath := filepath.Join(t.TempDir(), "serial.dcm"), filepath.Join(t.TempDir(), "parallel.dcm")
			for _, run := range []struct {
				path string
				opts pixeldata.TranscodeOptions
			}{{serialPath, serial}, {parallelPath, parallel}} {
				report, err := pixeldata.TranscodePath(context.Background(), native, run.path, syntax, run.opts)
				if err != nil {
					t.Fatal(err)
				}
				if report.Frames != 9 || report.Verification != pixeldata.TranscodeVerificationPixelExact ||
					report.Durability != pixeldata.TranscodeDurable || !report.FileSynced ||
					(runtime.GOOS != "windows" && !report.DirectorySynced) {
					t.Fatalf("unexpected report: %+v", report)
				}
				if report.PeakInFlightFrames > 3 || report.PeakInFlightBytes > 2<<20 {
					t.Fatalf("window exceeded bounds: %+v", report)
				}
			}
			want, err := os.ReadFile(serialPath)
			if err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(parallelPath)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("parallel encoded bytes differ: %v", err)
			}
			// Decode the same encapsulated source in both modes, proving frame
			// order independently of the source encoder's determinism.
			for _, run := range []struct {
				path string
				opts pixeldata.TranscodeOptions
			}{{serialPath + ".native", serial}, {parallelPath + ".native", parallel}} {
				if _, err := pixeldata.TranscodePath(context.Background(), parallelPath, run.path, transfer.ExplicitVRLittleEndian, run.opts); err != nil {
					t.Fatal(err)
				}
			}
			want, _ = os.ReadFile(serialPath + ".native")
			got, _ = os.ReadFile(parallelPath + ".native")
			if !bytes.Equal(got, want) {
				t.Fatal("parallel decoded bytes differ")
			}
			original, _ := os.ReadFile(native)
			if !bytes.Equal(got, original) {
				t.Fatal("decode did not recover the independent native fixture")
			}
		})
	}
}

// A legacy decoder is allowed to keep mutable scratch state without locking.
// Rejecting overlap models that contract without introducing a race in the
// test itself. Only the explicit parallel case permits concurrent calls.
type pipelineLegacyDecoder struct {
	inner               pixeldata.Codec
	active, peak, calls atomic.Int64
	concurrentAllowed   bool
	afterDecode         func(int64) error
}

func (d *pipelineLegacyDecoder) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	active := d.active.Add(1)
	defer d.active.Add(-1)
	for peak := d.peak.Load(); active > peak; peak = d.peak.Load() {
		if d.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if active > 1 && !d.concurrentAllowed {
		return pixeldata.Frames{}, errors.New("legacy decoder called concurrently")
	}
	// Make independent calls overlap reliably if an unintended automatic
	// worker pool is introduced, including during default verification.
	time.Sleep(3 * time.Millisecond)
	frames, err := d.inner.Decode(pixel, obj)
	if err == nil && d.afterDecode != nil {
		err = d.afterDecode(d.calls.Add(1))
	} else {
		d.calls.Add(1)
	}
	return frames, err
}

func TestTranscodePipelineDefaultPreservesLegacyDecoderSerialCalls(t *testing.T) {
	old := parallel.SetBudget(3)
	defer parallel.SetBudget(old)
	options := pipelineTestOptions(t)
	inner, ok := options.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	if !ok {
		t.Fatal("RLE decoder absent")
	}
	legacy := &pipelineLegacyDecoder{inner: inner}
	registry := pixeldata.NewMemoryRegistry()
	if err := registry.RegisterCodec(transfer.RLELossless.UID, legacy); err != nil {
		t.Fatal(err)
	}
	options.DecoderRegistry = registry
	dir := t.TempDir()
	native := writeStreamTestFile(t, dir, "native.dcm", &object.File{
		Dataset: streamTestSource(16, 20, 8, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	encoded := filepath.Join(dir, "encoded.dcm")
	report, err := pixeldata.TranscodePath(context.Background(), native, encoded, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.WorkerLimit != 1 || report.Verification != pixeldata.TranscodeVerificationPixelExact || legacy.peak.Load() != 1 {
		t.Fatalf("default verification overlapped decoder: report=%+v peak=%d", report, legacy.peak.Load())
	}
	decoded := filepath.Join(dir, "decoded.dcm")
	report, err = pixeldata.TranscodePath(context.Background(), encoded, decoded, transfer.ExplicitVRLittleEndian, options)
	if err != nil || report.WorkerLimit != 1 || legacy.peak.Load() != 1 {
		t.Fatalf("default source decode overlapped: report=%+v peak=%d err=%v", report, legacy.peak.Load(), err)
	}
	want, _ := os.ReadFile(native)
	got, err := os.ReadFile(decoded)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("default roundtrip changed bytes: %v", err)
	}
	legacy.concurrentAllowed = true
	legacy.peak.Store(0)
	options.Pipeline = pixeldata.TranscodePipelineOptions{Workers: 4, MaxInFlightFrames: 4}
	report, err = pixeldata.TranscodePath(context.Background(), native, filepath.Join(dir, "parallel.dcm"), transfer.RLELossless, options)
	if err != nil || report.WorkerLimit != 4 || legacy.peak.Load() < 2 {
		t.Fatalf("explicit opt-in did not overlap: report=%+v peak=%d err=%v", report, legacy.peak.Load(), err)
	}
	if legacy.active.Load() != 0 || parallel.InUse() != 0 {
		t.Fatal("decoder work survived return")
	}
}

func TestTranscodePipelineSerialDecodeLateFailureAndCancelDoNotPublish(t *testing.T) {
	setup := pipelineTestOptions(t)
	dir := t.TempDir()
	native := writeStreamTestFile(t, dir, "native.dcm", &object.File{
		Dataset: streamTestSource(16, 20, 8, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	encoded := filepath.Join(dir, "source.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), native, encoded, transfer.RLELossless, setup); err != nil {
		t.Fatal(err)
	}
	inner, _ := setup.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	for _, workers := range []int{0, 1} {
		for _, cancelWork := range []bool{false, true} {
			t.Run(map[bool]string{false: "late failure", true: "cancel"}[cancelWork]+string(rune('0'+workers)), func(t *testing.T) {
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				outputDir := t.TempDir()
				out := filepath.Join(outputDir, "destination.dcm")
				canary := []byte("previous destination")
				if err := os.WriteFile(out, canary, 0600); err != nil {
					t.Fatal(err)
				}
				decoder := &pipelineLegacyDecoder{inner: inner, afterDecode: func(call int64) error {
					got, err := os.ReadFile(out)
					if err != nil || !bytes.Equal(got, canary) {
						t.Error("decoded output published before completion")
					}
					if call == 6 {
						if cancelWork {
							cancel()
						} else {
							return errors.New("deliberate late decode failure")
						}
					}
					return nil
				}}
				registry := pixeldata.NewMemoryRegistry()
				if err := registry.RegisterCodec(transfer.RLELossless.UID, decoder); err != nil {
					t.Fatal(err)
				}
				options := setup
				options.DecoderRegistry = registry
				options.Pipeline.Workers = workers
				_, err := pixeldata.TranscodePath(ctx, encoded, out, transfer.ExplicitVRLittleEndian, options)
				if cancelWork && !errors.Is(err, context.Canceled) || !cancelWork && !errors.Is(err, pixeldata.ErrTranscodeTransaction) {
					t.Fatalf("error=%v", err)
				}
				got, _ := os.ReadFile(out)
				entries, _ := os.ReadDir(outputDir)
				if !bytes.Equal(got, canary) || len(entries) != 1 || decoder.calls.Load() != 6 || decoder.active.Load() != 0 || decoder.peak.Load() != 1 || parallel.InUse() != 0 {
					t.Fatalf("serial failure leaked/published: entries=%v calls=%d active=%d peak=%d", entries, decoder.calls.Load(), decoder.active.Load(), decoder.peak.Load())
				}
			})
		}
	}
}

type pipelineEncoder struct {
	inner               pixeldata.FrameEncoder
	active, peak, calls atomic.Int64
	failAt              int
	wait                bool
	started             chan struct{}
}

func (e *pipelineEncoder) Capabilities() pixeldata.EncoderCapabilities { return e.inner.Capabilities() }
func (e *pipelineEncoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	e.calls.Add(1)
	active := e.active.Add(1)
	defer e.active.Add(-1)
	for peak := e.peak.Load(); active > peak; peak = e.peak.Load() {
		if e.peak.CompareAndSwap(peak, active) {
			break
		}
	}
	if e.started != nil {
		select {
		case e.started <- struct{}{}:
		default:
		}
	}
	if e.wait {
		<-ctx.Done()
		return pixeldata.EncodedFrame{}, ctx.Err()
	}
	if e.failAt >= 0 && int(binary.LittleEndian.Uint16(frame))/131 == e.failAt {
		return pixeldata.EncodedFrame{}, errors.New("deliberate frame failure")
	}
	// The first frame is slower, so completion order differs from write order.
	delay := time.Millisecond
	if binary.LittleEndian.Uint16(frame) == 0 {
		delay = 5 * time.Millisecond
	}
	select {
	case <-ctx.Done():
		return pixeldata.EncodedFrame{}, ctx.Err()
	case <-time.After(delay):
	}
	return e.inner.EncodeFrame(ctx, frame, metadata)
}

func installPipelineEncoder(t testing.TB, options *pixeldata.TranscodeOptions, failAt int, wait bool) *pipelineEncoder {
	t.Helper()
	inner, ok := options.EncoderRegistry.GetEncoder(transfer.RLELossless.UID)
	if !ok {
		t.Fatal("RLE encoder absent")
	}
	encoder := &pipelineEncoder{inner: inner, failAt: failAt, wait: wait, started: make(chan struct{}, 16)}
	registry := pixeldata.NewMemoryEncoderRegistry()
	if err := registry.RegisterEncoder(transfer.RLELossless.UID, encoder); err != nil {
		t.Fatal(err)
	}
	options.EncoderRegistry = registry
	return encoder
}

func TestTranscodePipelineLateFailureAndCancelJoinBeforeCleanup(t *testing.T) {
	old := parallel.SetBudget(3)
	defer parallel.SetBudget(old)
	for _, cancelWork := range []bool{false, true} {
		t.Run(map[bool]string{false: "late failure", true: "cancel"}[cancelWork], func(t *testing.T) {
			options := pipelineTestOptions(t)
			options.Pipeline = pixeldata.TranscodePipelineOptions{Workers: 4, MaxInFlightFrames: 3}
			encoder := installPipelineEncoder(t, &options, 6, cancelWork)
			dir := t.TempDir()
			native := writeStreamTestFile(t, dir, "source.dcm", &object.File{Dataset: streamTestSource(32, 40, 12, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian})
			out := filepath.Join(dir, "out.dcm")
			canary := []byte("original destination")
			if err := os.WriteFile(out, canary, 0600); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() {
				_, err := pixeldata.TranscodePath(ctx, native, out, transfer.RLELossless, options)
				done <- err
			}()
			if cancelWork {
				select {
				case <-encoder.started:
					cancel()
				case <-time.After(5 * time.Second):
					t.Fatal("encoder did not start")
				}
			}
			select {
			case err := <-done:
				if cancelWork && !errors.Is(err, context.Canceled) || !cancelWork && !errors.Is(err, pixeldata.ErrEncoderFailed) {
					t.Fatalf("error: %v", err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("workers did not join")
			}
			if encoder.active.Load() != 0 || parallel.InUse() != 0 {
				t.Fatalf("retained work: active=%d helpers=%d", encoder.active.Load(), parallel.InUse())
			}
			if !cancelWork && encoder.peak.Load() < 2 {
				t.Fatal("independent frame encoding never overlapped")
			}
			got, _ := os.ReadFile(out)
			if !bytes.Equal(got, canary) {
				t.Fatal("failure changed destination")
			}
			entries, _ := os.ReadDir(dir)
			if len(entries) != 2 {
				t.Fatalf("temporaries retained: %v", entries)
			}
		})
	}
}

func TestTranscodePipelineWholeObjectFallbackReportsScope(t *testing.T) {
	dir := t.TempDir()
	input := writeStreamTestFile(t, dir, "native.dcm", &object.File{
		Dataset: streamTestSource(16, 20, 3, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian,
	})
	options := pipelineTestOptions(t)
	options.RecoverUN = &object.UNRecoveryOptions{}
	options.Pipeline.MaxInFlightBytes = 1
	report, err := pixeldata.TranscodePath(context.Background(), input, filepath.Join(dir, "whole.dcm"), transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Streaming || report.WorkerLimit != 0 || report.PeakInFlightBytes != 0 || report.Verification != pixeldata.TranscodeVerificationPixelExact || !report.FileSynced {
		t.Fatalf("fallback claimed frame-window execution: %+v", report)
	}
}

func TestTranscodePipelineLimitsAndExplicitPolicies(t *testing.T) {
	dir := t.TempDir()
	native := writeStreamTestFile(t, dir, "source.dcm", &object.File{Dataset: streamTestSource(16, 20, 7, 1, 16), TransferSyntax: transfer.ExplicitVRLittleEndian})
	for _, bound := range []string{"bytes", "encoded"} {
		t.Run(bound, func(t *testing.T) {
			options := pipelineTestOptions(t)
			encoder := installPipelineEncoder(t, &options, -1, false)
			if bound == "bytes" {
				options.Pipeline.MaxInFlightBytes = 1
			} else {
				options.Pipeline.MaxEncodedFrameBytes = 1
			}
			out := filepath.Join(t.TempDir(), "out.dcm")
			_, err := pixeldata.TranscodePath(context.Background(), native, out, transfer.RLELossless, options)
			if !errors.Is(err, pixeldata.ErrTranscodeResourceLimit) {
				t.Fatalf("error: %v", err)
			}
			if bound == "bytes" && encoder.calls.Load() != 0 {
				t.Fatal("inadmissible frame invoked encoder")
			}
			entries, _ := os.ReadDir(filepath.Dir(out))
			if len(entries) != 0 {
				t.Fatal("limit retained temporary/output")
			}
		})
	}
	options := pipelineTestOptions(t)
	options.SkipVerification = true
	options.DecoderRegistry = nil // Native input needs no decoder; opted-out output check must not request one.
	options.Durability = pixeldata.TranscodeAtomic
	options.Pipeline.MaxInFlightFrames = 2
	out := filepath.Join(t.TempDir(), "out.dcm")
	report, err := pixeldata.TranscodePath(context.Background(), native, out, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Verification != pixeldata.TranscodeVerificationSkipped || report.Durability != pixeldata.TranscodeAtomic || report.FileSynced || report.DirectorySynced || report.Timings.VerifyWorkerTime != 0 || report.Timings.FileSyncTime != 0 || report.Timings.DirectorySyncTime != 0 {
		t.Fatalf("policies not applied: %+v", report)
	}
}
