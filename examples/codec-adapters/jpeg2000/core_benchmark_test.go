package jpeg2000

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/examples/codec-adapters/jpeg2000/internal/j2kcore"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
)

// benchmarkCohort is one input decoded by every backend that can produce the
// same verified output. Work per iteration is the whole object: frame
// planning, decode and native packing (plus process startup for OpenJPEG).
type benchmarkCohort struct {
	name    string
	fixture string // corpus fixture id
}

var jpeg2000BenchmarkCohorts = []benchmarkCohort{
	{"ct512-signed16-lossless", "jpeg2000-signed-ct"},
	{"mr64x64x10-u12-lossless", "jpeg2000-multiframe-mr"},
	{"us640x480-rgb8-irreversible", "jpeg2000-lossy-color-us"},
	{"u16-64x33-rlcp-lossless", "jpeg2000-core-u16-extremes-rlcp"},
	{"u8-64x64-irreversible", "jpeg2000-core-u8-irreversible"},
	// #1031: position progression with precincts, and every code-block style.
	{"rgb8-61x47-rpcl-precincts", "jpeg2000-core-rgb8-rpcl-precincts"},
	{"u16-52x45-block-styles", "jpeg2000-core-u16-block-styles"},
}

// BenchmarkDecodeCoreReduced times reduced-resolution decoding of the 512x512
// signed CT frame: each level dropped skips its tier-1 and synthesis, which
// is what makes a thumbnail cheap.
func BenchmarkDecodeCoreReduced(b *testing.B) {
	in := loadBenchmarkInput(b, "jpeg2000-signed-ct")
	for reduce := 0; reduce <= 3; reduce++ {
		b.Run(fmt.Sprintf("reduce-%d", reduce), func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := DecodeCoreReduced(context.Background(), in.payloads[0], reduce, CoreOptions{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

type benchmarkInput struct {
	dataset   *object.Object
	pixel     pixeldata.PixelData
	reference [][]byte
	layout    codecfixture.SampleLayout
	policy    codecfixture.SamplePolicy
	payloads  [][]byte
}

func loadBenchmarkInput(b *testing.B, id string) benchmarkInput {
	b.Helper()
	fixtures, err := codecfixture.CorpusFixtures(libraryRoot)
	if err != nil {
		b.Fatal(err)
	}
	for _, fixture := range fixtures {
		if fixture.ID != id {
			continue
		}
		in := benchmarkInput{layout: corpusLayout(fixture.Input)}
		in.reference, _ = referenceFrames(b, fixture)
		if fixture.Lossy {
			if fixture.SamplePolicy == nil {
				b.Fatalf("%s has no sample policy", id)
			}
			in.policy = *fixture.SamplePolicy
		}
		path, err := codecfixture.CorpusFilePath(libraryRoot, fixture.Path)
		if err != nil {
			b.Fatal(err)
		}
		if file, err := object.OpenFile(path); err == nil && file.Dataset.Has(core.TagPixelData) {
			in.dataset = file.Dataset
			b.Cleanup(func() { _ = file.Close() })
		} else {
			if file != nil {
				_ = file.Close()
			}
			codestream, err := os.ReadFile(path)
			if err != nil {
				b.Fatal(err)
			}
			if len(codestream)%2 != 0 {
				codestream = append(codestream, 0)
			}
			m := corpusMetadata(fixture.Input)
			in.dataset = object.FromElements([]core.Element{
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0002), VR: core.VRUS}, Value: core.Uint16Value{m.SamplesPerPixel}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{m.PhotometricInterpretation}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint16Value{m.Rows}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0011), VR: core.VRUS}, Value: core.Uint16Value{m.Columns}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0100), VR: core.VRUS}, Value: core.Uint16Value{m.BitsAllocated}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0101), VR: core.VRUS}, Value: core.Uint16Value{m.BitsStored}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0102), VR: core.VRUS}, Value: core.Uint16Value{m.HighBit}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0103), VR: core.VRUS}, Value: core.Uint16Value{m.PixelRepresentation}},
				{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{codestream}}},
			}, nil)
		}
		in.pixel, err = pixeldata.Extract(in.dataset)
		if err != nil {
			b.Fatal(err)
		}
		in.payloads, err = planJPEG2000Frames(context.Background(), in.pixel, in.dataset, len(in.reference), false)
		if err != nil {
			b.Fatal(err)
		}
		return in
	}
	b.Fatalf("unknown fixture %s", id)
	return benchmarkInput{}
}

// peakHeap samples HeapAlloc while fn runs and returns the largest increase.
func peakHeap(fn func()) uint64 {
	runtime.GC()
	var start runtime.MemStats
	runtime.ReadMemStats(&start)
	var peak atomic.Uint64
	done := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		var m runtime.MemStats
		for {
			runtime.ReadMemStats(&m)
			if m.HeapAlloc > start.HeapAlloc && m.HeapAlloc-start.HeapAlloc > peak.Load() {
				peak.Store(m.HeapAlloc - start.HeapAlloc)
			}
			select {
			case <-done:
				return
			case <-time.After(200 * time.Microsecond):
			}
		}
	}()
	fn()
	close(done)
	wg.Wait()
	return peak.Load()
}

// BenchmarkJPEG2000Backends times backends only on cohorts where their output
// was verified first. "core" is planning, decode and native packing;
// "core-decode-only" is the codestream-to-planes stage; "openjpeg" includes
// temporary files and one process per frame. Unsupported combinations are reported, never timed as
// zero. The OpenJPEG profile needs -tags jpeg2000_openjpeg and
// DICOM_GO_OPENJPEG_DECOMPRESS; with DICOM_GO_J2K_BENCH_REQUIRE_OPENJPEG=1 its
// absence fails instead of being reported.
func BenchmarkJPEG2000Backends(b *testing.B) {
	requireOpenJPEG := os.Getenv("DICOM_GO_J2K_BENCH_REQUIRE_OPENJPEG") == "1"
	// Process startup alone (opj_decompress -h and its version check), so the
	// subprocess share of each OpenJPEG decode can be separated from decoding.
	b.Run("openjpeg-process-startup", func(b *testing.B) {
		if err := ValidateOpenJPEGRuntime(); err != nil {
			if errors.Is(err, ErrOpenJPEGUnavailable) && !requireOpenJPEG {
				b.Skip(err)
			}
			b.Fatal(err)
		}
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			if err := ValidateOpenJPEGRuntime(); err != nil {
				b.Fatal(err)
			}
		}
	})
	for _, cohort := range jpeg2000BenchmarkCohorts {
		in := loadBenchmarkInput(b, cohort.fixture)
		backends := []struct {
			name  string
			codec *Codec
		}{
			{"core", NewCore(CoreOptions{})},
			{"purego-legacy", New()},
			{"openjpeg", NewOpenJPEGCodec()},
		}
		for _, backend := range backends {
			b.Run(cohort.name+"/"+backend.name, func(b *testing.B) {
				frames, err := backend.codec.DecodeContext(context.Background(), in.pixel, in.dataset)
				if err != nil {
					if backend.name == "openjpeg" && errors.Is(err, ErrOpenJPEGUnavailable) && !requireOpenJPEG {
						b.Skip("OpenJPEG profile not compiled in (-tags jpeg2000_openjpeg)")
					}
					if backend.name == "purego-legacy" && !requireOpenJPEG {
						b.Skipf("unsupported combination: %v", err)
					}
					b.Fatalf("verification decode: %v", err)
				}
				report, err := codecfixture.CompareSamples(frames.Data, in.layout, in.reference, in.layout, in.policy)
				if err != nil || !report.Qualified {
					if backend.name == "purego-legacy" {
						b.Skipf("unsupported combination: output not equivalent (%s)", report.Reason)
					}
					b.Fatalf("verification: %+v %v", report, err)
				}
				peak := peakHeap(func() {
					if _, err := backend.codec.DecodeContext(context.Background(), in.pixel, in.dataset); err != nil {
						b.Fatal(err)
					}
				})
				b.ReportAllocs()
				b.ResetTimer()
				for i := 0; i < b.N; i++ {
					out, err := backend.codec.DecodeContext(context.Background(), in.pixel, in.dataset)
					if err != nil || len(out.Data) != len(in.reference) {
						b.Fatal(err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(peak), "peak-heap-B")
			})
		}
		// Decode-only stage of the core: codestream to planes, no packing.
		b.Run(cohort.name+"/core-decode-only", func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				for _, payload := range in.payloads {
					stream, err := j2kcore.ParseCodestream(payload)
					if err != nil {
						b.Fatal(err)
					}
					if _, err := j2kcore.DecodeContext(context.Background(), stream, j2kcore.Limits{}); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}
