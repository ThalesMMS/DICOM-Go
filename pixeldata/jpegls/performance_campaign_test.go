package jpegls

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type performanceSample struct {
	Tool         string                     `json:"tool"`
	Operation    string                     `json:"operation"`
	Workers      int                        `json:"workers"`
	Repetition   int                        `json:"repetition"`
	ElapsedNS    int64                      `json:"elapsed_ns"`
	VerifyNS     int64                      `json:"external_pixel_verification_ns"`
	OutputSHA256 string                     `json:"output_sha256"`
	Report       *pixeldata.TranscodeReport `json:"transcode_report,omitempty"`
}

type performanceClock struct {
	name      string
	frequency int64
	now       func() int64
}

func (c performanceClock) elapsedNS(start, end int64) int64 {
	return (end - start) * 1e9 / c.frequency
}

// TestJPEGLSPerformanceCampaign is a local, opt-in, sequential CPU campaign.
// Go uses TranscodeAtomic with its extra pixel verification disabled. DCMTK
// writes a private candidate and the harness renames it in the same directory.
// Neither side requests fsync. The same frame reader and exact comparison
// verify every output outside the encode/decode timer, in a separate column.
// Go's mandatory structural validation remains inside TranscodePath's timer;
// DCMTK's process startup remains inside its timer. Performance has no assertion.
func TestJPEGLSPerformanceCampaign(t *testing.T) {
	artifact := os.Getenv("DICOMGO_JPEGLS_PERFORMANCE_DIR")
	if artifact == "" {
		t.Skip("explicit exclusive CPU campaign only")
	}
	clock := newPerformanceClock(t)
	if !filepath.IsAbs(artifact) {
		t.Fatal("artifact directory must be absolute")
	}
	if err := os.MkdirAll(artifact, 0700); err != nil {
		t.Fatal(err)
	}
	encodeTool, err := exec.LookPath("dcmcjpls")
	if err != nil {
		t.Fatal(err)
	}
	decodeTool, err := exec.LookPath("dcmdjpls")
	if err != nil {
		t.Fatal(err)
	}
	version, err := exec.Command(encodeTool, "--version").CombinedOutput()
	if err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := Register(decoders); err != nil {
		t.Fatal(err)
	}
	if err := RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	options := pixeldata.TranscodeOptions{DecoderRegistry: decoders, EncoderRegistry: encoders,
		SkipVerification: true, Durability: pixeldata.TranscodeAtomic}
	frame := ctBenchFrame()
	pixels := bytes.Repeat(frame, 48)
	metadata := encoderMetadata(512, 512, 1, 16, "MONOCHROME2")
	metadata.BitsStored, metadata.HighBit = 12, 11
	elements := []core.Element{
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0016), "1.2.840.10008.5.1.4.1.1.7.3"),
		dicomtest.NewUIElement(core.NewTag(0x0008, 0x0018), "1.2.826.0.1.3680043.10.1.1296"),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, "48"),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		core.NewRawElement(core.TagPixelData, core.VROW, pixels),
	}
	for _, field := range []struct {
		tag   uint16
		value uint16
	}{
		{0x0010, 512}, {0x0011, 512}, {0x0002, 1}, {0x0100, 16}, {0x0101, 12}, {0x0102, 11}, {0x0103, 0},
	} {
		elements = append(elements, dicomtest.NewUShortElement(core.NewTag(0x0028, field.tag), field.value))
	}
	native := filepath.Join(artifact, "native.dcm")
	f, err := os.Create(native)
	if err != nil {
		t.Fatal(err)
	}
	err = object.WriteFile(f, &object.File{Dataset: object.FromElements(elements, nil), TransferSyntax: transfer.ExplicitVRLittleEndian})
	closeErr := f.Close()
	if err != nil || closeErr != nil {
		t.Fatalf("write fixture: %v / %v", err, closeErr)
	}
	compressed := filepath.Join(artifact, "fixture.dcm")
	if _, err := pixeldata.TranscodePath(context.Background(), native, compressed, transfer.JPEGLSLossless, options); err != nil {
		t.Fatal(err)
	}
	verify := func(path string) int64 {
		start := clock.now()
		r, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{Registry: decoders})
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		if r.Metadata().NumberOfFrames != 48 {
			t.Fatal("frame count")
		}
		for i := 0; i < 48; i++ {
			got, _, err := r.Frame(context.Background(), i)
			if err != nil || !bytes.Equal(got, frame) {
				t.Fatalf("frame %d: %v or pixel mismatch", i, err)
			}
		}
		return clock.elapsedNS(start, clock.now())
	}
	var samples []performanceSample
	for rep := -1; rep < 5; rep++ {
		order := []string{"go-1", "dcmtk", "go-4"}
		if rep&1 == 1 {
			order = []string{"go-4", "dcmtk", "go-1"}
		}
		for _, tool := range order {
			for _, operation := range []string{"encode", "decode"} {
				input, target := native, transfer.JPEGLSLossless
				if operation == "decode" {
					input, target = compressed, transfer.ExplicitVRLittleEndian
				}
				output := filepath.Join(artifact, fmt.Sprintf("%s-%s-%d.dcm", tool, operation, rep))
				sample := performanceSample{Tool: tool, Operation: operation, Repetition: rep}
				start := clock.now()
				if tool == "dcmtk" {
					candidate := output + ".candidate"
					command := exec.Command(encodeTool, "+el", "+pr", "+un", input, candidate)
					if operation == "decode" {
						command = exec.Command(decodeTool, input, candidate)
					}
					if out, err := command.CombinedOutput(); err != nil {
						t.Fatalf("DCMTK: %v: %s", err, out)
					}
					if err := os.Rename(candidate, output); err != nil {
						t.Fatal(err)
					}
				} else {
					sample.Workers = 1
					if tool == "go-4" {
						sample.Workers = 4
					}
					options.Pipeline.Workers = sample.Workers
					report, err := pixeldata.TranscodePath(context.Background(), input, output, target, options)
					if err != nil {
						t.Fatal(err)
					}
					sample.Report = &report
				}
				sample.ElapsedNS = clock.elapsedNS(start, clock.now())
				sample.VerifyNS = verify(output)
				data, err := os.ReadFile(output)
				if err != nil {
					t.Fatal(err)
				}
				sample.OutputSHA256 = fmt.Sprintf("%x", sha256.Sum256(data))
				if rep >= 0 {
					samples = append(samples, sample)
				}
				if err := os.Remove(output); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	// Measure cancellation after a timer fires during a frame, rather than
	// the already-cancelled fast path. Retain actual timer-to-return latency.
	stream, err := NewEncoder().EncodeFrame(context.Background(), frame, metadata)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, "frame.jls"), stream.Data, 0600); err != nil {
		t.Fatal(err)
	}
	cancellation := map[string][]int64{}
	for _, operation := range []string{"encode", "decode"} {
		for i := 0; i < 20; i++ {
			ctx, cancel := context.WithCancel(context.Background())
			fired := make(chan int64, 1)
			timer := time.AfterFunc(time.Millisecond, func() { fired <- clock.now(); cancel() })
			if operation == "encode" {
				_, err = NewEncoder().EncodeFrame(ctx, frame, metadata)
			} else {
				_, err = decodeFrameContext(ctx, stream.Data, metadata)
			}
			returned := clock.now()
			if timer.Stop() {
				cancel()
				t.Fatal("operation finished before cancellation")
			}
			onset := <-fired
			cancel()
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancellation: %v", err)
			}
			cancellation[operation] = append(cancellation[operation], clock.elapsedNS(onset, returned))
		}
	}
	var clockTick int64
	previous := clock.now()
	for i := 0; i < 50000; i++ {
		now := clock.now()
		if delta := clock.elapsedNS(previous, now); delta > 0 && (clockTick == 0 || delta < clockTick) {
			clockTick = delta
		}
		previous = now
	}
	data, err := json.MarshalIndent(struct {
		Go, OS, Arch, DCMTK string
		GOMAXPROCS          int
		FrameSHA256         string
		Samples             []performanceSample
		Cancellation        map[string][]int64
		ObservedClockTickNS int64
		Clock               string
		ClockFrequency      int64
	}{runtime.Version(), runtime.GOOS, runtime.GOARCH, string(version), runtime.GOMAXPROCS(0), fmt.Sprintf("%x", sha256.Sum256(frame)), samples, cancellation, clockTick, clock.name, clock.frequency}, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(artifact, "campaign.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// A fresh-process decoder-only RSS sample must not prepare its input with the
// encoder, whose unrelated full-frame pool would confound peak working set.
func TestJPEGLSDecoderMemorySample(t *testing.T) {
	path := os.Getenv("DICOMGO_JPEGLS_MEMORY_INPUT")
	if path == "" {
		t.Skip("fresh-process decoder memory sample only")
	}
	stream, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	metadata := encoderMetadata(512, 512, 1, 16, "MONOCHROME2")
	metadata.BitsStored, metadata.HighBit = 12, 11
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak uint64
	var digest [32]byte
	for i := 0; i < 20; i++ {
		frame, err := decodeFrame(stream, metadata)
		if err != nil {
			t.Fatal(err)
		}
		digest = sha256.Sum256(frame)
		runtime.ReadMemStats(&after)
		if after.HeapAlloc > peak {
			peak = after.HeapAlloc
		}
	}
	t.Logf("frame_sha256=%x heap_peak=%d allocated_bytes=%d", digest, peak, after.TotalAlloc-before.TotalAlloc)
}
