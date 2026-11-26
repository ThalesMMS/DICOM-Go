package pixeldata

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func lossyMetadata(rows, columns, components int) Metadata {
	pi := "MONOCHROME2"
	if components == 3 {
		pi = "RGB"
	}
	return Metadata{Rows: uint16(rows), Columns: uint16(columns), SamplesPerPixel: uint16(components), BitsAllocated: 8, BitsStored: 8, HighBit: 7,
		NumberOfFrames: 1, PhotometricInterpretation: pi, PlanarConfigurationPresent: components == 3}
}

func TestLossyStoredSampleComparison(t *testing.T) {
	word := func(values ...uint16) []byte {
		out := make([]byte, len(values)*2)
		for i, v := range values {
			binary.LittleEndian.PutUint16(out[i*2:], v)
		}
		return out
	}
	for _, tc := range []struct {
		name            string
		a, b            Metadata
		source, decoded []byte
		block           int
		maximum         float64
		raw             int64
		blocks          int64
	}{
		{"unsigned8-boundary", lossyMetadata(1, 2, 1), lossyMetadata(1, 2, 1), []byte{127, 255}, []byte{128, 254}, 1, 1, 1, 2},
		{"all-complete-blocks", lossyMetadata(2, 2, 1), lossyMetadata(2, 2, 1), []byte{0, 0, 0, 0}, []byte{0, 0, 0, 4}, 2, 1, 4, 1},
		{"fractional-no-integer-truncation", lossyMetadata(2, 2, 1), lossyMetadata(2, 2, 1), []byte{0, 0, 0, 0}, []byte{0, 0, 0, 1}, 2, .25, 1, 1},
		{"signed-errors-cancel", lossyMetadata(2, 2, 1), lossyMetadata(2, 2, 1), []byte{10, 10, 10, 10}, []byte{11, 9, 10, 10}, 2, 0, 1, 1},
		{"partial-right-bottom", lossyMetadata(3, 3, 1), lossyMetadata(3, 3, 1), make([]byte, 9), []byte{0, 0, 0, 0, 0, 0, 0, 0, 7}, 2, 7, 7, 4},
		{"large-side-covers-small-image", lossyMetadata(1, 2, 1), lossyMetadata(1, 2, 1), []byte{0, 0}, []byte{1, 3}, math.MaxInt, 2, 3, 1},
		{"components-separate", lossyMetadata(1, 2, 3), lossyMetadata(1, 2, 3), []byte{0, 0, 0, 0, 0, 0}, []byte{0, 2, 0, 0, 4, 0}, 2, 3, 4, 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := compareLossyFrame(context.Background(), tc.source, tc.a, tc.decoded, tc.b, tc.block)
			if err != nil || got.MaximumError != tc.maximum || got.MaximumSampleError != tc.raw || got.Blocks != tc.blocks {
				t.Fatalf("comparison %+v: %v", got, err)
			}
		})
	}
	m := lossyMetadata(1, 2, 1)
	m.BitsAllocated = 16
	m.BitsStored = 16
	m.HighBit = 15
	got, err := compareLossyFrame(context.Background(), word(32767, 65535), m, word(32768, 65534), m, 1)
	if err != nil || got.MaximumError != 1 {
		t.Fatalf("unsigned16 boundary: %+v %v", got, err)
	}
	m.BitsStored = 12
	m.HighBit = 14
	m.PixelRepresentation = 1
	// Sign and mask are applied to Bits Stored after shifting off low unused bits.
	got, err = compareLossyFrame(context.Background(), word(0xfff8, 0x3ff8), m, word(0x0000, 0x3ff0), m, 1)
	if err != nil || got.MaximumError != 1 {
		t.Fatalf("signed shifted12: %+v %v", got, err)
	}
	m.PixelRepresentation = 0
	m.HighBit = 11
	got, err = compareLossyFrame(context.Background(), word(0xf123, 0xa456), m, word(0x0123, 0x0456), m, 1)
	if err != nil || got.MaximumError != 0 {
		t.Fatalf("unused bits: %+v %v", got, err)
	}
	a := lossyMetadata(1, 2, 3)
	b := a
	b.PlanarConfiguration = 1
	got, err = compareLossyFrame(context.Background(), []byte{1, 2, 3, 4, 5, 6}, a, []byte{1, 4, 2, 5, 3, 6}, b, 1)
	if err != nil || got.MaximumError != 0 || got.Blocks != 6 {
		t.Fatalf("planar mapped: %+v %v", got, err)
	}
	// Different container width/high bit can express the same stored values.
	a = lossyMetadata(1, 2, 1)
	b = a
	b.BitsAllocated = 16
	b.HighBit = 11
	got, err = compareLossyFrame(context.Background(), []byte{127, 128}, a, word(127<<4, 128<<4), b, 1)
	if err != nil || got.MaximumError != 0 {
		t.Fatalf("container normalized: %+v %v", got, err)
	}
}

func TestLossyComparisonRejectsIncomparable(t *testing.T) {
	a := lossyMetadata(1, 2, 1)
	for _, change := range []func(*Metadata){
		func(m *Metadata) { m.Rows = 2 }, func(m *Metadata) { m.SamplesPerPixel = 3 }, func(m *Metadata) { m.BitsStored = 7 }, func(m *Metadata) { m.PixelRepresentation = 1 },
		func(m *Metadata) { m.PhotometricInterpretation = "MONOCHROME1" }, func(m *Metadata) { m.BitsAllocated = 1 }, func(m *Metadata) { m.PlanarConfiguration = 2 },
	} {
		b := a
		change(&b)
		if _, err := compareLossyFrame(context.Background(), []byte{1, 2}, a, []byte{1, 2}, b, 1); !errors.Is(err, ErrTranscodeComparisonUnsupported) {
			t.Fatalf("incompatible metadata accepted: %v", err)
		}
	}
	for _, pi := range []string{"PALETTE COLOR", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_ICT"} {
		m := a
		m.PhotometricInterpretation = pi
		if _, err := compareLossyFrame(context.Background(), []byte{1, 2}, m, []byte{1, 2}, m, 1); !errors.Is(err, ErrTranscodeComparisonUnsupported) {
			t.Fatal(pi, err)
		}
	}
	if _, err := compareLossyFrame(context.Background(), []byte{1}, a, []byte{1, 2}, a, 1); !errors.Is(err, ErrPixelDataSizeMismatch) {
		t.Fatal(err)
	}
	m := lossyMetadata(65535, 65535, 1)
	m.BitsAllocated = 32
	m.BitsStored = 32
	m.HighBit = 31
	// Impossible slices are rejected by length before any indexing or allocation.
	if _, err := compareLossyFrame(context.Background(), nil, m, nil, m, math.MaxInt); !errors.Is(err, ErrPixelDataSizeMismatch) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := compareLossyFrame(ctx, []byte{1, 2}, a, []byte{1, 2}, a, 1); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

type lossyCancelDuringComparison struct {
	context.Context
	checks int
}

func (c *lossyCancelDuringComparison) Err() error {
	c.checks++
	if c.checks > 10 {
		return context.Canceled
	}
	return nil
}

func TestLossyComparisonCancellationDuringScan(t *testing.T) {
	m := lossyMetadata(64, 2048, 1)
	ctx := &lossyCancelDuringComparison{Context: context.Background()}
	if _, err := compareLossyFrame(ctx, make([]byte, m.FrameSize()), m, make([]byte, m.FrameSize()), m, 2048); !errors.Is(err, context.Canceled) || ctx.checks <= 10 {
		t.Fatal("scan cancellation lost", err)
	}
}

type lossyControlledCodec struct {
	calls atomic.Int64
	cause error
	pi    string
}

func (c *lossyControlledCodec) Decode(pixel PixelData, obj *object.Object) (Frames, error) {
	c.calls.Add(1)
	if c.cause != nil {
		return Frames{}, c.cause
	}
	m, err := ExtractMetadata(obj)
	if err != nil {
		return Frames{}, err
	}
	return Frames{Rows: int(m.Rows), Columns: int(m.Columns), Data: cloneFrames(pixel.Sequence.Fragments), PhotometricInterpretation: c.pi}, nil
}

type lossyControlledEncoder struct {
	calls  atomic.Int64
	mutate func()
	cancel context.CancelFunc
	delta  byte
}

func (*lossyControlledEncoder) Capabilities() EncoderCapabilities {
	return EncoderCapabilities{TransferSyntaxUID: transfer.JPEGLSNearLossless.UID, BitsAllocated: []uint16{8}, PixelRepresentations: []uint16{0}, SamplesPerPixel: []uint16{1}, PhotometricInterpretations: []string{"MONOCHROME2"}, LossyMethod: "ISO_14495_1", SupportsMultiFrame: true}
}
func (e *lossyControlledEncoder) EncodeFrame(ctx context.Context, raw []byte, m Metadata) (EncodedFrame, error) {
	e.calls.Add(1)
	out := core.CloneBytes(raw)
	out[len(out)-1] += e.delta
	if e.mutate != nil {
		e.mutate()
	}
	if e.cancel != nil {
		e.cancel()
	}
	return EncodedFrame{Data: out}, nil
}
func lossyControlledOptions(t testing.TB, limit *float64) (TranscodeOptions, *lossyControlledEncoder, *lossyControlledCodec) {
	t.Helper()
	encoder := &lossyControlledEncoder{delta: 2}
	decoder := &lossyControlledCodec{}
	enc := NewMemoryEncoderRegistry()
	dec := NewMemoryRegistry()
	if err := enc.RegisterEncoder(transfer.JPEGLSNearLossless.UID, encoder); err != nil {
		t.Fatal(err)
	}
	if err := dec.RegisterCodec(transfer.JPEGLSNearLossless.UID, decoder); err != nil {
		t.Fatal(err)
	}
	return TranscodeOptions{EncoderRegistry: enc, DecoderRegistry: dec, AllowLossy: true, MaxPixelError: limit}, encoder, decoder
}
func lossyControlledObject(frames int) *object.Object {
	var elements []core.Element
	for _, v := range []struct {
		tag   core.Tag
		value uint16
	}{{tagRows, 2}, {tagColumns, 3}, {tagSamplesPerPixel, 1}, {tagBitsAllocated, 8}, {tagBitsStored, 8}, {tagHighBit, 7}, {tagPixelRepresentation, 0}} {
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: v.tag, VR: core.VRUS}, Value: core.Uint16Value{v.value}})
	}
	for _, v := range []struct {
		tag   core.Tag
		vr    core.VR
		value string
	}{
		{tagNumberOfFrames, core.VRIS, strconv.Itoa(frames)}, {tagPhotometricInterpretation, core.VRCS, "MONOCHROME2"},
		{core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"}, {tagSOPInstanceUID, core.VRUI, "1.2.826.0.1.3680043.10.543.1322.1"},
	} {
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: v.tag, VR: v.vr}, Value: core.StringValue{v.value}})
	}
	data := bytes.Repeat([]byte{127}, frames*6)
	elements = append(elements, core.NewRawElement(core.TagPixelData, core.VROB, data))
	return object.FromElements(elements, nil)
}

func TestLossyTranscodeControlledPolicyAndReports(t *testing.T) {
	for _, route := range []string{"dataset", "stream", "fallback"} {
		for _, limit := range []float64{2, 1, 0} {
			t.Run(route+"-"+strconv.FormatFloat(limit, 'g', -1, 64), func(t *testing.T) {
				input := lossyControlledObject(3)
				defer input.Close()
				opts, _, dec := lossyControlledOptions(t, &limit)
				opts.Pipeline.Workers = 2
				opts.Pipeline.MaxInFlightFrames = 2
				var report TranscodeReport
				var err error
				if route == "dataset" {
					var output *object.Object
					output, report, err = TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts)
					if output != nil {
						defer output.Close()
					}
				} else {
					dir := t.TempDir()
					src := filepath.Join(dir, "in.dcm")
					dst := filepath.Join(dir, "out.dcm")
					var serialized bytes.Buffer
					if e := object.WriteFile(&serialized, &object.File{Dataset: input, TransferSyntax: transfer.ExplicitVRLittleEndian}); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(src, serialized.Bytes(), 0600); e != nil {
						t.Fatal(e)
					}
					if e := os.WriteFile(dst, []byte("existing destination"), 0600); e != nil {
						t.Fatal(e)
					}
					if route == "fallback" {
						opts.Rewrite = &NativeRewrite{}
					}
					report, err = TranscodePath(context.Background(), src, dst, transfer.JPEGLSNearLossless, opts)
					if limit < 2 {
						got, _ := os.ReadFile(dst)
						if string(got) != "existing destination" {
							t.Fatal("failed check published")
						}
					}
					entries, _ := os.ReadDir(dir)
					if len(entries) != 2 {
						t.Fatal("temporaries retained")
					}
				}
				if limit < 2 {
					var diagnostic *LossyVerificationError
					if !errors.As(err, &diagnostic) || !errors.Is(err, ErrTranscodePixelError) || diagnostic.Measured != 2 || diagnostic.Limit != limit || diagnostic.Frame < 1 || strings.Contains(err.Error(), "127") {
						t.Fatalf("diagnostic %v", err)
					}
					return
				}
				if err != nil || report.LossyVerification == nil || report.Verification != TranscodeVerificationPixelError {
					t.Fatalf("report %+v err %v", report, err)
				}
				got := report.LossyVerification
				if got.MaximumError != 2 || got.MaximumSampleError != 2 || got.Frames != 3 || got.Blocks != 18 || got.Limit != 2 || got.AverageBlockSize != 1 {
					t.Fatalf("metrics %+v", got)
				}
				if dec.calls.Load() != 3 {
					t.Fatalf("redundant decoder calls %d", dec.calls.Load())
				}
				if route == "stream" && (!report.Streaming || report.PeakInFlightFrames > 2) {
					t.Fatal("pipeline bounds lost")
				}
			})
		}
	}
}

func TestLossyVerificationOptionsFrozenAndValidated(t *testing.T) {
	input := lossyControlledObject(2)
	defer input.Close()
	for _, limit := range []float64{-1, math.NaN(), math.Inf(1), math.Inf(-1)} {
		opts, enc, _ := lossyControlledOptions(t, &limit)
		if output, _, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts); output != nil || !errors.Is(err, ErrTranscodeVerificationOptions) || enc.calls.Load() != 0 {
			t.Fatal("invalid limit accepted", err)
		}
	}
	for _, options := range []TranscodeOptions{{AverageBlockSize: -1}, {AverageBlockSize: 2}, {MaxPixelError: new(float64), SkipVerification: true}} {
		if _, err := ResolveTranscodeVerificationOptions(options); !errors.Is(err, ErrTranscodeVerificationOptions) {
			t.Fatal(err)
		}
	}
	limit := 1.
	opts, enc, _ := lossyControlledOptions(t, &limit)
	enc.mutate = func() { limit = 100 }
	if output, _, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts); output != nil || !errors.Is(err, ErrTranscodePixelError) {
		t.Fatal("caller changed frozen limit", err)
	}
	limit = 2
	opts, _, decoder := lossyControlledOptions(t, &limit)
	opts.DecoderRegistry = nil
	if output, _, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts); output != nil || !errors.Is(err, ErrCodecRegistryNil) || decoder.calls.Load() != 0 {
		t.Fatal("missing decoder accepted", err)
	}
	opts, enc, _ = lossyControlledOptions(t, &limit)
	ctx, cancel := context.WithCancel(context.Background())
	enc.cancel = cancel
	if output, _, err := TranscodeDataSet(ctx, input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts); output != nil || !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation lost", err)
	}
	opts, _, decoder = lossyControlledOptions(t, &limit)
	decoder.cause = errors.New("SECRET backend")
	if output, _, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts); output != nil || !errors.Is(err, decoder.cause) || strings.Contains(err.Error(), "SECRET") {
		t.Fatal("decoder error lost/redaction", err)
	}
	// Nil retains the in-memory adapter's existing policy: no extra lossy decode.
	opts, _, decoder = lossyControlledOptions(t, nil)
	output, report, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts)
	if err != nil || report.LossyVerification != nil || decoder.calls.Load() != 0 {
		t.Fatal("default changed", err)
	}
	defer output.Close()
}

func TestLossyTranscodeFractionalBlockPolicy(t *testing.T) {
	input := lossyControlledObject(1)
	defer input.Close()
	for _, limit := range []float64{.5, .49} {
		opts, enc, _ := lossyControlledOptions(t, &limit)
		enc.delta = 1
		opts.AverageBlockSize = 2
		out, report, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, opts)
		if limit < .5 {
			var diagnostic *LossyVerificationError
			if out != nil || !errors.As(err, &diagnostic) || diagnostic.Measured != .5 {
				t.Fatal("fractional error truncated", err)
			}
		} else {
			if err != nil || report.LossyVerification.MaximumError != .5 || report.LossyVerification.Blocks != 2 {
				t.Fatal("partial block policy", err)
			}
			_ = out.Close()
		}
	}
}

func TestLossyVerificationResourceLimitBeforeCodec(t *testing.T) {
	input := lossyControlledObject(2)
	defer input.Close()
	limit := 2.
	options, encoder, decoder := lossyControlledOptions(t, &limit)
	options.Limits.MaxOutputBytes = 5
	if output, _, err := TranscodeDataSet(context.Background(), input, transfer.ExplicitVRLittleEndian, transfer.JPEGLSNearLossless, options); output != nil || !errors.Is(err, ErrDecompressResourceLimit) || encoder.calls.Load() != 0 || decoder.calls.Load() != 0 {
		t.Fatal("whole-object resource limit late", err)
	}
	options, encoder, decoder = lossyControlledOptions(t, &limit)
	options.Pipeline.MaxInFlightBytes = 1
	dir := t.TempDir()
	src := filepath.Join(dir, "in.dcm")
	dst := filepath.Join(dir, "out.dcm")
	var serialized bytes.Buffer
	if err := object.WriteFile(&serialized, &object.File{Dataset: input, TransferSyntax: transfer.ExplicitVRLittleEndian}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, serialized.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := TranscodePath(context.Background(), src, dst, transfer.JPEGLSNearLossless, options); !errors.Is(err, ErrTranscodeResourceLimit) || encoder.calls.Load() != 0 || decoder.calls.Load() != 0 {
		t.Fatal("stream verification charge late", err)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatal("limit failure retained temporaries")
	}
}
