package pixeldata_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// routerTestObject is a synthetic multi-frame RLE data set (with a Patient's
// Name, to show reports do not carry it), its native frames and the
// built-in RLE codec.
func routerTestObject(t testing.TB, rows, columns, frames int) (*object.Object, pixeldata.PixelData, [][]byte, pixeldata.Codec) {
	t.Helper()
	options := streamTestRegistries(t)
	source := streamTestSource(rows, columns, frames, 1, 16)
	source.Put(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("Doe^Jane")))
	native := &object.File{Dataset: source, TransferSyntax: transfer.ExplicitVRLittleEndian}
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), native, transfer.RLELossless, options)
	if err != nil {
		t.Fatal(err)
	}
	pixel, err := pixeldata.ExtractView(encoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	codec, ok := options.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	if !ok {
		t.Fatal("no RLE codec")
	}
	reference, err := codec.Decode(pixel, encoded.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return encoded.Dataset, pixel, reference.Data, codec
}

// faultyCodec wraps a codec: it fails on the frames fail selects, with an
// error that quotes the Patient's Name, and adds delta to sample corrupt of
// its output.
type faultyCodec struct {
	pixeldata.Codec
	fail    func(codestream []byte) bool
	corrupt int
	delta   uint16
	calls   atomic.Int64
}

func (c *faultyCodec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	c.calls.Add(1)
	if c.fail != nil && c.fail(pixel.Sequence.Fragments[0]) {
		return pixeldata.Frames{}, errors.New("cannot decode the image of Doe^Jane")
	}
	frames, err := c.Codec.Decode(pixel, obj)
	if err == nil && c.delta != 0 {
		data := append([]byte(nil), frames.Data[0]...)
		data[2*c.corrupt] += byte(c.delta)
		frames.Data[0] = data
	}
	return frames, err
}

func newRouter(t testing.TB, options pixeldata.CodecRouterOptions) *pixeldata.CodecRouter {
	t.Helper()
	if options.TransferSyntaxUID == "" {
		options.TransferSyntaxUID = transfer.RLELossless.UID
	}
	router, err := pixeldata.NewCodecRouter(options)
	if err != nil {
		t.Fatal(err)
	}
	return router
}

func TestCodecRouterChoosesABackendPerFrame(t *testing.T) {
	obj, pixel, reference, rle := routerTestObject(t, 9, 11, 6)
	odd := map[string]bool{}
	for i, fragment := range pixel.Sequence.Fragments {
		odd[string(fragment)] = i%2 == 1
	}
	special := &faultyCodec{Codec: rle}
	var decisions []pixeldata.FrameCodecDecision
	router := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{
			{Name: "odd-frames", Codec: special, Accept: func(codestream []byte, m pixeldata.Metadata) bool {
				if m.NumberOfFrames != 1 || m.Rows != 9 {
					t.Errorf("Accept saw metadata %+v", m)
				}
				return odd[string(codestream)]
			}},
			{Name: "rle", Codec: rle},
		},
		Observe: func(d pixeldata.FrameCodecDecision) { decisions = append(decisions, d) },
	})
	frames, err := router.DecodeContext(context.Background(), pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	if len(frames.Data) != 6 || frames.Rows != 9 || frames.Columns != 11 {
		t.Fatalf("%d frames of %dx%d", len(frames.Data), frames.Columns, frames.Rows)
	}
	for i := range reference {
		if !bytes.Equal(frames.Data[i], reference[i]) {
			t.Fatalf("frame %d differs from the RLE codec's", i)
		}
	}
	for i, d := range decisions {
		want := "rle"
		if i%2 == 1 {
			want = "odd-frames"
		}
		if d.Frame != i || d.Backend != want || d.Fallback || d.TransferSyntaxUID != transfer.RLELossless.UID {
			t.Fatalf("decision %d: %+v", i, d)
		}
		if declined := len(d.Declined); want == "rle" && (declined != 1 || d.Declined[0] != (pixeldata.BackendDecline{Backend: "odd-frames", Reason: pixeldata.DeclineNotAccepted})) || want != "rle" && declined != 0 {
			t.Fatalf("decision %d declined %+v", i, d.Declined)
		}
	}
	if special.calls.Load() != 3 {
		t.Fatalf("the odd-frame backend decoded %d frames", special.calls.Load())
	}
	stats := router.Stats()
	if stats.Frames != 6 || stats.Fallbacks != 0 || stats.Backends["rle"] != 3 || stats.Backends["odd-frames"] != 3 {
		t.Fatalf("stats %+v", stats)
	}

	// No backend for a frame.
	none := newRouter(t, pixeldata.CodecRouterOptions{Backends: []pixeldata.CodecBackend{
		{Name: "odd-frames", Codec: rle, Accept: func(c []byte, _ pixeldata.Metadata) bool { return odd[string(c)] }},
	}})
	if _, err := none.Decode(pixel, obj); !errors.Is(err, pixeldata.ErrNoCodecBackend) {
		t.Fatalf("no backend: %v", err)
	}
}

func TestCodecRouterFallsBackPerFrame(t *testing.T) {
	obj, pixel, reference, rle := routerTestObject(t, 9, 11, 4)
	third := pixel.Sequence.Fragments[2]
	failing := &faultyCodec{Codec: rle, fail: func(c []byte) bool { return bytes.Equal(c, third) }}
	var decisions []pixeldata.FrameCodecDecision
	router := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{{Name: "preferred", Codec: failing}, {Name: "fallback", Codec: rle}},
		Observe:  func(d pixeldata.FrameCodecDecision) { decisions = append(decisions, d) },
	})
	frames, err := router.Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reference {
		if !bytes.Equal(frames.Data[i], reference[i]) {
			t.Fatalf("frame %d differs", i)
		}
	}
	if d := decisions[2]; d.Backend != "fallback" || !d.Fallback || len(d.Declined) != 1 || d.Declined[0].Reason != pixeldata.DeclineDecodeFailed {
		t.Fatalf("decision for frame 2: %+v", d)
	}
	if !contains(frames.Diagnostics, pixeldata.DiagnosticCodecBackendFallback) {
		t.Fatalf("diagnostics %v", frames.Diagnostics)
	}
	if stats := router.Stats(); stats.Fallbacks != 1 || stats.Backends["preferred"] != 3 || stats.Backends["fallback"] != 1 {
		t.Fatalf("stats %+v", stats)
	}

	// A forced backend does not fall back; the registry wraps its error as
	// a codec failure.
	forced := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{{Name: "preferred", Codec: failing}, {Name: "fallback", Codec: rle}},
		Fallback: func(backend string, err error) bool { return false },
	})
	registry := pixeldata.NewMemoryRegistry()
	if err := forced.Register(registry); err != nil {
		t.Fatal(err)
	}
	if _, err := registry.DecodeFrames(transfer.RLELossless.UID, pixel, obj); !errors.Is(err, pixeldata.ErrCodecDecodeFailed) {
		t.Fatalf("forced backend: %v", err)
	}
}

func TestCodecRouterShadowReportsDivergenceWithoutPHI(t *testing.T) {
	obj, pixel, reference, rle := routerTestObject(t, 9, 11, 4)
	second := pixel.Sequence.Fragments[1]
	oracle := &faultyCodec{Codec: rle, corrupt: 17, delta: 3,
		fail: func(c []byte) bool { return bytes.Equal(c, second) }}
	var reports []pixeldata.ShadowReport
	router := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{{Name: "candidate", Codec: rle}},
		Shadow: &pixeldata.ShadowComparison{Backend: pixeldata.CodecBackend{Name: "oracle", Codec: oracle},
			Report: func(r pixeldata.ShadowReport) { reports = append(reports, r) }},
	})
	frames, err := router.Decode(pixel, obj)
	if err != nil {
		t.Fatal(err)
	}
	for i := range reference {
		if !bytes.Equal(frames.Data[i], reference[i]) {
			t.Fatalf("frame %d: the shadow changed the output", i)
		}
	}
	if len(reports) != 4 {
		t.Fatalf("%d reports", len(reports))
	}
	for i, r := range reports {
		if r.Frame != i || r.Primary != "candidate" || r.Shadow != "oracle" || r.Rows != 9 || r.Columns != 11 {
			t.Fatalf("report %d: %+v", i, r)
		}
		if i == 1 {
			if r.Outcome != pixeldata.ShadowFailed || r.Reason != pixeldata.ShadowReasonDecodeError {
				t.Fatalf("failed shadow: %+v", r)
			}
			continue
		}
		if r.Outcome != pixeldata.ShadowMismatched || r.Reason != pixeldata.ShadowReasonSamples || r.Samples != 99 ||
			r.DifferingSamples != 1 || r.MaxAbsoluteDifference != 3 || r.FirstDifference != 17 {
			t.Fatalf("mismatch: %+v", r)
		}
		if text := fmt.Sprintf("%+v %v", r, r.Outcome); strings.Contains(text, "Doe") || strings.Contains(text, "Jane") {
			t.Fatalf("the report carries the Patient's Name: %s", text)
		}
	}
	if stats := router.Stats(); stats.Compared != 4 || stats.Mismatched != 3 || stats.Failed != 1 || stats.Matched != 0 {
		t.Fatalf("stats %+v", stats)
	}

	// Within the tolerance, sampled one frame in three.
	reports = nil
	tolerant := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{{Name: "candidate", Codec: rle}},
		Shadow: &pixeldata.ShadowComparison{Backend: pixeldata.CodecBackend{Name: "oracle", Codec: &faultyCodec{Codec: rle, corrupt: 5, delta: 1}},
			SampleEvery: 3, MaxAbsoluteError: 1, Report: func(r pixeldata.ShadowReport) { reports = append(reports, r) }},
	})
	if _, err := tolerant.Decode(pixel, obj); err != nil {
		t.Fatal(err)
	}
	if len(reports) != 2 || reports[0].Frame != 0 || reports[1].Frame != 3 || reports[0].Outcome != pixeldata.ShadowMatched || reports[0].DifferingSamples != 1 {
		t.Fatalf("sampled reports %+v", reports)
	}
	if stats := tolerant.Stats(); stats.Compared != 2 || stats.SampledOut != 2 || stats.Matched != 2 {
		t.Fatalf("sampled stats %+v", stats)
	}
}

// liveCodec decodes with a codec while counting the bytes of the frames
// decoding at once, holding each for a moment so that workers overlap.
type liveCodec struct {
	pixeldata.Codec
	frameBytes int64
	live, peak atomic.Int64
}

func (c *liveCodec) Decode(pixel pixeldata.PixelData, obj *object.Object) (pixeldata.Frames, error) {
	live := c.live.Add(c.frameBytes)
	defer c.live.Add(-c.frameBytes)
	for {
		peak := c.peak.Load()
		if live <= peak || c.peak.CompareAndSwap(peak, live) {
			break
		}
	}
	time.Sleep(2 * time.Millisecond)
	return c.Codec.Decode(pixel, obj)
}

func TestCodecRouterAdmissionBoundsConcurrentFrames(t *testing.T) {
	obj, pixel, reference, rle := routerTestObject(t, 16, 16, 24)
	const frameBytes = 1000
	estimate := func(int, pixeldata.Metadata) int64 { return frameBytes }
	run := func(admission *pixeldata.MemoryAdmission) int64 {
		codec := &liveCodec{Codec: rle, frameBytes: frameBytes}
		router := newRouter(t, pixeldata.CodecRouterOptions{
			Backends: []pixeldata.CodecBackend{{Name: "rle", Codec: codec}},
			Workers:  8, Admission: admission, FrameBytes: estimate,
		})
		frames, err := router.Decode(pixel, obj)
		if err != nil {
			t.Fatal(err)
		}
		for i := range reference {
			if !bytes.Equal(frames.Data[i], reference[i]) {
				t.Fatalf("frame %d differs", i)
			}
		}
		return codec.peak.Load()
	}
	admission, err := pixeldata.NewMemoryAdmission(2 * frameBytes)
	if err != nil {
		t.Fatal(err)
	}
	if peak := run(admission); peak > 2*frameBytes {
		t.Fatalf("%d bytes decoding at once, admission %d", peak, 2*frameBytes)
	}
	if stats := admission.Stats(); stats.Peak != 2*frameBytes || stats.Granted != 24 || stats.Held != 0 || stats.Waited == 0 {
		t.Fatalf("admission %+v", stats)
	}
	if peak := run(nil); peak <= 2*frameBytes {
		t.Logf("without admission only %d bytes decoded at once", peak)
	}

	// A canceled decode waiting for admission returns the context's error.
	held, err := admission.Acquire(context.Background(), 2*frameBytes)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	router := newRouter(t, pixeldata.CodecRouterOptions{Backends: []pixeldata.CodecBackend{{Name: "rle", Codec: rle}}, Admission: admission, Workers: 3})
	if _, err := router.DecodeContext(ctx, pixel, obj); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("waiting decode: %v", err)
	}
	held()
	if stats := admission.Stats(); stats.Held != 0 || stats.Queued != 0 {
		t.Fatalf("after cancellation %+v", stats)
	}
}

// The router serves FrameReader: single frames, and partial decodes with a
// backend that implements them.
func TestFrameReaderRoutesThroughCodecRouter(t *testing.T) {
	paths, options := partialTestFiles(t)
	rleCodec, _ := options.DecoderRegistry.GetCodec(transfer.RLELossless.UID)
	partialBackend := &partialCodec{Codec: rleCodec}
	var mu sync.Mutex
	var decisions []pixeldata.FrameCodecDecision
	router := newRouter(t, pixeldata.CodecRouterOptions{
		Backends: []pixeldata.CodecBackend{{Name: "partial", Codec: partialBackend}, {Name: "rle", Codec: rleCodec}},
		Observe: func(d pixeldata.FrameCodecDecision) {
			mu.Lock()
			decisions = append(decisions, d)
			mu.Unlock()
		},
	})
	registry := pixeldata.NewMemoryRegistry()
	if err := router.Register(registry); err != nil {
		t.Fatal(err)
	}
	frames, metadata := fullDecode(t, paths["rle16"], options.DecoderRegistry)
	reader, err := pixeldata.OpenFrameReader(context.Background(), paths["rle16"], pixeldata.FrameReaderOptions{Registry: registry})
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	got, _, err := reader.Frame(context.Background(), 1)
	if err != nil || !bytes.Equal(got, frames[1]) {
		t.Fatalf("frame 1: %v", err)
	}
	if capabilities, err := reader.PartialDecodeCapabilities(context.Background(), 0); err != nil || capabilities.ReductionLevels != 3 {
		t.Fatalf("capabilities %+v, %v", capabilities, err)
	}
	request := pixeldata.PartialDecodeRequest{Frame: 1, Region: image.Rect(3, 4, 20, 9), ReductionLevel: 2, NoFallback: true}
	part, err := reader.DecodePartial(context.Background(), request)
	if err != nil || part.Path != pixeldata.PartialDecodeCodec || !bytes.Equal(part.Data, referenceReduce(frames[1], metadata, request.Region, 2)) {
		t.Fatalf("partial: %+v, %v", part.Path, err)
	}
	// A failing partial backend falls back to the next, which decodes
	// whole frames only: FrameReader then decodes the frame through the
	// router (whose first backend decodes whole frames) and crops it.
	partialBackend.err = errors.New("broken")
	region := image.Rect(1, 1, 3, 3)
	part, err = reader.DecodePartial(context.Background(), pixeldata.PartialDecodeRequest{Frame: 1, Region: region})
	if err != nil || part.Path != pixeldata.PartialDecodeFullFrame || !bytes.Equal(part.Data, crop(frames[1], metadata, region)) {
		t.Fatalf("a failing partial backend: path %v, %v", part.Path, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(decisions) < 2 || decisions[0].Frame != 0 || decisions[0].Backend != "partial" || !decisions[1].Partial || decisions[1].Frame != -1 {
		t.Fatalf("decisions %+v", decisions)
	}
}

func TestNewCodecRouterRejectsInvalidOptions(t *testing.T) {
	_, _, _, rle := routerTestObject(t, 2, 2, 1)
	backend := pixeldata.CodecBackend{Name: "rle", Codec: rle}
	for name, options := range map[string]pixeldata.CodecRouterOptions{
		"no syntax":         {Backends: []pixeldata.CodecBackend{backend}},
		"no backends":       {TransferSyntaxUID: transfer.RLELossless.UID},
		"unnamed":           {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{{Codec: rle}}},
		"no codec":          {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{{Name: "x"}}},
		"duplicate":         {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{backend, backend}},
		"negative workers":  {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{backend}, Workers: -1},
		"shadow sampling":   {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{backend}, Shadow: &pixeldata.ShadowComparison{Backend: backend, SampleEvery: -1}},
		"shadow without it": {TransferSyntaxUID: transfer.RLELossless.UID, Backends: []pixeldata.CodecBackend{backend}, Shadow: &pixeldata.ShadowComparison{}},
	} {
		if _, err := pixeldata.NewCodecRouter(options); !errors.Is(err, pixeldata.ErrInvalidCodecRouter) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func contains(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// BenchmarkCodecRouterMultiFrameAdmission decodes a 64-frame object on eight
// workers and reports the bytes of frames decoding at once (peak-live-B):
// unbounded, and under an admission of two and four frames.
func BenchmarkCodecRouterMultiFrameAdmission(b *testing.B) {
	obj, pixel, _, rle := routerTestObject(b, 128, 128, 64)
	frameBytes := int64(128 * 128 * 2)
	for _, frames := range []int64{0, 2, 4} {
		name := "unbounded"
		if frames > 0 {
			name = fmt.Sprintf("admission-%d-frames", frames)
		}
		b.Run(name, func(b *testing.B) {
			var admission *pixeldata.MemoryAdmission
			if frames > 0 {
				var err error
				if admission, err = pixeldata.NewMemoryAdmission(frames * frameBytes); err != nil {
					b.Fatal(err)
				}
			}
			codec := &liveCodec{Codec: rle, frameBytes: frameBytes}
			router := newRouter(b, pixeldata.CodecRouterOptions{
				Backends: []pixeldata.CodecBackend{{Name: "rle", Codec: codec}}, Workers: 8, Admission: admission,
				FrameBytes: func(int, pixeldata.Metadata) int64 { return frameBytes },
			})
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := router.Decode(pixel, obj); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(codec.peak.Load()), "peak-live-B")
			if admission != nil && codec.peak.Load() > frames*frameBytes {
				b.Fatalf("%d bytes live, admission %d", codec.peak.Load(), frames*frameBytes)
			}
		})
	}
}
