package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"reflect"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type legacyPixelCase struct {
	name       string
	source     *object.Object
	recovery   pixeldata.DecodeRecovery
	want       *object.Object
	diagnostic string
}

func legacyImage(bits, samples, pc uint16, name string, values []uint16) *object.Object {
	raw := make([]byte, len(values)*int(bits/8))
	for i, value := range values {
		if bits == 8 {
			raw[i] = byte(value)
		} else {
			binary.LittleEndian.PutUint16(raw[2*i:], value)
		}
	}
	out := recoveryImage(&samples, name, &pc, raw)
	if bits == 16 {
		out.Put(core.NewRawElement(core.TagPixelData, core.VROW, raw))
	}
	out.Put(recoveryUS(0x00280011, uint16(len(values)/int(samples))))
	for tag, value := range map[uint32]uint16{0x00280100: bits, 0x00280101: bits, 0x00280102: bits - 1} {
		out.Put(recoveryUS(tag, value))
	}
	return out
}

func legacyPalette(out *object.Object, wrong bool) {
	for c := uint16(0); c < 3; c++ {
		desc, data := uint16(0x1101)+c, uint16(0x1201)+c
		if wrong {
			desc += 0x10
			data += 0x10
		}
		rawDesc := make([]byte, 6)
		for i, value := range []uint16{2, 1, 8} {
			binary.LittleEndian.PutUint16(rawDesc[2*i:], value)
		}
		out.Put(core.NewRawElement(core.NewTag(0x0028, desc), core.VRUS, rawDesc))
		values := [3][]byte{{240, 30}, {20, 220}, {10, 40}}
		out.Put(core.NewRawElement(core.NewTag(0x0028, data), core.VROW, values[c]))
	}
}

func legacyCases() []legacyPixelCase {
	var cases []legacyPixelCase
	for _, bits := range []uint16{8, 16} {
		depth := fmt.Sprint(bits)
		scale := uint16(1)
		if bits == 16 {
			scale = 257
		}
		values := func(v ...uint16) []uint16 {
			for i := range v {
				v[i] *= scale
			}
			return v
		}
		cases = append(cases,
			legacyPixelCase{name: "planar-by-frame-" + depth, source: legacyImage(bits, 3, 0, "RGB", values(10, 20, 30, 40, 50, 60)), recovery: pixeldata.DecodeRecovery{NativePlanarLayout: pixeldata.NativePlanarByFrame}, want: legacyImage(bits, 3, 1, "RGB", values(10, 20, 30, 40, 50, 60)), diagnostic: pixeldata.DiagnosticNativePlanarOverride},
			legacyPixelCase{name: "interleaved-" + depth, source: legacyImage(bits, 3, 1, "RGB", values(10, 30, 50, 20, 40, 60)), recovery: pixeldata.DecodeRecovery{NativePlanarLayout: pixeldata.NativePlanarInterleaved}, want: legacyImage(bits, 3, 0, "RGB", values(10, 30, 50, 20, 40, 60)), diagnostic: pixeldata.DiagnosticNativePlanarOverride})
		whole := legacyImage(bits, 3, 0, "RGB", values(10, 20, 11, 21, 30, 40, 31, 41, 50, 60, 51, 61))
		whole.Put(recoveryUS(0x00280011, 2))
		whole.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}})
		wanted := legacyImage(bits, 3, 1, "RGB", values(10, 20, 30, 40, 50, 60, 11, 21, 31, 41, 51, 61))
		wanted.Put(recoveryUS(0x00280011, 2))
		wanted.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}})
		cases = append(cases, legacyPixelCase{name: "whole-image-" + depth, source: whole, recovery: pixeldata.DecodeRecovery{NativePlanarLayout: pixeldata.NativePlanarWholeImage}, want: wanted, diagnostic: pixeldata.DiagnosticNativeWholeImagePlanar})
		mono := legacyImage(bits, 1, 0, "MONOCHROME2", values(10, 220))
		if bits == 16 {
			mono = legacyImage(bits, 1, 0, "MONOCHROME2", []uint16{0x1234, 0xc567})
		}
		mono.Remove(core.NewTag(0x0028, 0x0006))
		raw, _ := mono.GetRaw(core.TagPixelData)
		swapped := bytes.Clone(raw)
		for i := 0; i < len(swapped); i += 2 {
			swapped[i], swapped[i+1] = swapped[i+1], swapped[i]
		}
		bad := object.FromElements(mono.Elements(), nil)
		bad.Put(core.NewRawElement(core.TagPixelData, core.VROW, swapped))
		cases = append(cases, legacyPixelCase{name: "ow-swap-" + depth, source: bad, recovery: pixeldata.DecodeRecovery{NativeOWByteSwap: true}, want: mono, diagnostic: pixeldata.DiagnosticNativeOWByteSwap})
		for _, planar := range []uint16{0, 1} {
			for _, model := range []struct {
				name, diagnostic string
				samples          uint16
				raw, want        []uint16
			}{
				{"ARGB", pixeldata.DiagnosticRetiredARGBToRGB, 4, values(0, 255, 0, 0, 0, 0, 255, 0), values(255, 0, 0, 0, 255, 0)},
				{"CMYK", pixeldata.DiagnosticRetiredCMYKToRGB, 4, values(0, 255, 255, 0, 100, 20, 50, 30), values(255, 0, 0, 125, 205, 175)},
				{"HSV", pixeldata.DiagnosticRetiredHSVToRGB, 3, values(85, 255, 255, 170, 255, 255), values(0, 255, 0, 0, 0, 255)},
			} {
				raw := slices.Clone(model.raw)
				if planar == 1 {
					for i := 0; i < 2; i++ {
						for c := 0; c < int(model.samples); c++ {
							raw[c*2+i] = model.raw[i*int(model.samples)+c]
						}
					}
				}
				src := legacyImage(bits, model.samples, planar, model.name, raw)
				if model.name == "HSV" {
					model.want[0], model.want[4] = 1, 3
				}
				if model.name == "ARGB" {
					legacyPalette(src, false)
				}
				cases = append(cases, legacyPixelCase{name: model.name + "-" + depth + "-" + string(rune('0'+planar)), source: src, recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}, want: legacyImage(bits, 3, 0, "RGB", model.want), diagnostic: model.diagnostic})
			}
			alpha := values(1, 0, 0, 0, 2, 0, 0, 0)
			// Alpha is an unscaled palette index, even in 16-bit containers.
			alpha[0], alpha[4] = 1, 2
			if planar == 1 {
				alpha = []uint16{1, 2, 0, 0, 0, 0, 0, 0}
			}
			argb := legacyImage(bits, 4, planar, "ARGB", alpha)
			legacyPalette(argb, false)
			if bits == 16 {
				for c := uint16(0); c < 3; c++ {
					rawDesc := []byte{2, 0, 1, 0, 16, 0}
					argb.Put(core.NewRawElement(core.NewTag(0x0028, 0x1101+c), core.VRUS, rawDesc))
					entries := [3][2]uint16{{240 * 257, 30 * 257}, {20 * 257, 220 * 257}, {10 * 257, 40 * 257}}
					rawData := make([]byte, 4)
					binary.LittleEndian.PutUint16(rawData, entries[c][0])
					binary.LittleEndian.PutUint16(rawData[2:], entries[c][1])
					argb.Put(core.NewRawElement(core.NewTag(0x0028, 0x1201+c), core.VROW, rawData))
				}
			}
			cases = append(cases, legacyPixelCase{name: fmt.Sprintf("ARGB-palette-%d-%d", bits, planar), source: argb, recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}, want: legacyImage(bits, 3, 0, "RGB", values(240, 20, 10, 30, 220, 40)), diagnostic: pixeldata.DiagnosticRetiredARGBToRGB})
		}
	}
	palette := legacyImage(8, 1, 0, "PALETTE COLOR", []uint16{1, 2})
	palette.Remove(core.NewTag(0x0028, 0x0006))
	legacyPalette(palette, true)
	canonical := object.FromElements(palette.Elements(), nil)
	for c := uint16(0); c < 3; c++ {
		canonical.Remove(core.NewTag(0x0028, 0x1111+c))
		canonical.Remove(core.NewTag(0x0028, 0x1211+c))
	}
	legacyPalette(canonical, false)
	oddOW := legacyImage(8, 1, 0, "MONOCHROME2", []uint16{1, 2, 3})
	oddOW.Remove(core.NewTag(0x0028, 0x0006))
	badOW := object.FromElements(oddOW.Elements(), nil)
	badOW.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{2, 1, 0, 3}))
	cases = append(cases, legacyPixelCase{name: "ow-swap-pad", source: badOW, recovery: pixeldata.DecodeRecovery{NativeOWByteSwap: true}, want: oddOW, diagnostic: pixeldata.DiagnosticNativeOWByteSwap})
	return append(cases, legacyPixelCase{name: "wrong-palette", source: palette, recovery: pixeldata.DecodeRecovery{WrongPaletteAttributeTags: true}, want: canonical, diagnostic: pixeldata.DiagnosticWrongPaletteAttributeTags})
}

// The altered stream stores each component's bytes least significant first;
// the RLE header's segment offsets are rebuilt and remain conforming.
func reversedRLEFixture(t *testing.T, bits, samples uint16) (*object.Object, []byte) {
	t.Helper()
	name := "MONOCHROME2"
	if samples == 3 {
		name = "RGB"
	}
	values := []uint16{0x1234, 0xabcd}
	if samples == 3 {
		values = []uint16{0x1234, 0x5678, 0x9abc, 0xabcd, 0x2468, 0x1357}
	}
	src := legacyImage(16, samples, 0, name, values)
	if bits != 16 {
		raw := []byte{0x78, 0x56, 0x34, 0x12, 0xef, 0xcd, 0xab, 0x89}
		if bits == 64 {
			raw = []byte{8, 7, 6, 5, 4, 3, 2, 1, 16, 15, 14, 13, 12, 11, 10, 9}
		}
		src.Put(core.NewRawElement(core.TagPixelData, core.VROW, raw))
		for tag, value := range map[uint32]uint16{0x00280100: bits, 0x00280101: bits, 0x00280102: bits - 1} {
			src.Put(recoveryUS(tag, value))
		}
	}
	m, err := pixeldata.ExtractMetadata(src)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := src.GetRaw(core.TagPixelData)
	encoded, err := rle.NewEncoder().EncodeFrame(context.Background(), raw, m)
	if err != nil {
		t.Fatal(err)
	}
	count := int(binary.LittleEndian.Uint32(encoded.Data))
	stream := make([]byte, 64)
	binary.LittleEndian.PutUint32(stream, uint32(count))
	width := int(bits / 8)
	for s := 0; s < count; s++ {
		original := (s/width)*width + width - 1 - s%width
		start := int(binary.LittleEndian.Uint32(encoded.Data[4+4*original:]))
		end := len(encoded.Data)
		if original+1 < count {
			end = int(binary.LittleEndian.Uint32(encoded.Data[8+4*original:]))
		}
		binary.LittleEndian.PutUint32(stream[4+4*s:], uint32(len(stream)))
		stream = append(stream, encoded.Data[start:end]...)
	}
	src.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}, Value: core.FragmentSequence{Fragments: [][]byte{stream}}})
	return src, raw
}

type refusingLegacyCodestream struct{ called bool }

func (*refusingLegacyCodestream) CanTranscodeCodestream(string, string) bool { return true }
func (s *refusingLegacyCodestream) TranscodeCodestreams(context.Context, string, string, pixeldata.PixelData, *object.Object) ([][]byte, error) {
	s.called = true
	return nil, errors.New("legacy repair must decode pixels")
}

func TestLegacyPixelRepairsRLEEveryDecodePathAndSameSyntax(t *testing.T) {
	registry := pixeldata.NewMemoryRegistry()
	if err := rle.Register(registry); err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []struct{ bits, samples uint16 }{{16, 1}, {16, 3}, {32, 1}, {64, 1}} {
		t.Run(fmt.Sprintf("%dx%d", profile.bits, profile.samples), func(t *testing.T) {
			src, want := reversedRLEFixture(t, profile.bits, profile.samples)
			options := pixeldata.DecodeRecovery{RLEReversedByteSegments: true}
			ctx := pixeldata.DecodeContextWithRecovery(context.Background(), options)
			var report pixeldata.DecompressReport
			out, _, err := pixeldata.DecompressDataSetContext(ctx, src, transfer.RLELossless, pixeldata.DecompressOptions{Registry: registry, Recovery: options, Report: &report})
			if err != nil {
				t.Fatal(err)
			}
			raw, _ := out.GetRaw(core.TagPixelData)
			if !bytes.Equal(raw, want) || !slices.Equal(report.Diagnostics, []string{pixeldata.DiagnosticRLEReversedByteSegments}) {
				t.Fatalf("RLE recovery %v %+v, want %v", raw, report, want)
			}
			strict, _, err := pixeldata.DecompressDataSet(src, transfer.RLELossless, pixeldata.DecompressOptions{Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			wrong, _ := strict.GetRaw(core.TagPixelData)
			if bytes.Equal(wrong, want) {
				t.Fatal("strict decoder silently reversed nonstandard segments")
			}
			pixel, err := pixeldata.Extract(src)
			if err != nil {
				t.Fatal(err)
			}
			m, err := pixeldata.ExtractMetadata(src)
			if err != nil {
				t.Fatal(err)
			}
			for _, region := range []image.Rectangle{image.Rect(0, 0, 2, 1), image.Rect(1, 0, 2, 1)} {
				step := int(profile.samples * profile.bits / 8)
				expected := want[region.Min.X*step : region.Max.X*step]
				cropped, err := rle.New().DecodeFrameRegion(ctx, pixel.Sequence.Fragments[0], m, region)
				if err != nil || !bytes.Equal(cropped.Data[0], expected) || !slices.Equal(cropped.Diagnostics, []string{pixeldata.DiagnosticRLEReversedByteSegments}) {
					t.Fatalf("region %v %+v %v", region, cropped, err)
				}
				dst := bytes.Repeat([]byte{0xcc}, len(expected)+3)
				n, diagnostics, err := rle.New().DecodeFrameInto(ctx, pixel.Sequence.Fragments[0], m, pixeldata.FrameBufferRequest{Region: region, WholeFrame: region.Dx() == 2}, dst)
				if err != nil || n != len(expected) || !bytes.Equal(dst[:n], expected) || !bytes.Equal(dst[n:], []byte{0xcc, 0xcc, 0xcc}) || !slices.Equal(diagnostics, []string{pixeldata.DiagnosticRLEReversedByteSegments}) {
					t.Fatalf("buffer path %v %v %v", dst, diagnostics, err)
				}
			}
			spy := &refusingLegacyCodestream{}
			encoded, tr, err := pixeldata.TranscodeDataSet(context.Background(), src, transfer.RLELossless, transfer.RLELossless, pixeldata.TranscodeOptions{Recovery: options, DecoderRegistry: registry, EncoderRegistry: encoders, CodestreamTranscoder: spy})
			if err != nil || spy.called || tr.PixelDataPreserved || !slices.Equal(tr.DecodeDiagnostics, []string{pixeldata.DiagnosticRLEReversedByteSegments}) {
				t.Fatalf("same-syntax repair bypassed: %+v %v", tr, err)
			}
			normalized, _, err := pixeldata.DecompressDataSet(encoded, transfer.RLELossless, pixeldata.DecompressOptions{Registry: registry})
			if err != nil {
				t.Fatal(err)
			}
			normalRaw, _ := normalized.GetRaw(core.TagPixelData)
			if !bytes.Equal(normalRaw, want) {
				t.Fatal("repair not standardized", normalRaw, want)
			}
			if profile.bits == 16 {
				gc, ge := legacyDisplay(out)
				wc, we := legacyDisplay(normalized)
				if ge != nil || we != nil || !slices.Equal(gc, wc) {
					t.Fatal("RLE display mismatch", ge, we)
				}
			}
		})
	}
}

func legacyDisplay(obj *object.Object) ([]color.RGBA, error) {
	native, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		return nil, err
	}
	m := native.Metadata
	format := display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit), Signed: m.PixelRepresentation == 1, ByteOrder: obj.ValueByteOrder()}
	var result []color.RGBA
	for _, raw := range native.Data {
		var img image.Image
		if m.SamplesPerPixel == 1 && m.PhotometricInterpretation != "PALETTE COLOR" {
			img, err = display.RenderGray(display.Frame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: raw, Format: format})
		} else {
			var palette *display.PaletteColorLUT
			palette, err = display.PaletteFromObject(obj)
			if err != nil {
				return nil, err
			}
			img, err = display.RenderColor(display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: raw, Format: format, Photometric: m.PhotometricInterpretation, SamplesPerPixel: int(m.SamplesPerPixel), PlanarConfiguration: int(m.PlanarConfiguration), Palette: palette})
		}
		if err != nil {
			return nil, err
		}
		for y := 0; y < img.Bounds().Dy(); y++ {
			for x := 0; x < img.Bounds().Dx(); x++ {
				result = append(result, color.RGBAModel.Convert(img.At(x, y)).(color.RGBA))
			}
		}
	}
	return result, nil
}

func assertLegacyEquivalent(t *testing.T, got, want *object.Object) {
	t.Helper()
	gm, ge := pixeldata.ExtractMetadata(got)
	wm, we := pixeldata.ExtractMetadata(want)
	if ge != nil || we != nil || gm != wm {
		t.Fatalf("metadata got %+v / %v, want %+v / %v", gm, ge, wm, we)
	}
	gr, _ := got.GetRaw(core.TagPixelData)
	wr, _ := want.GetRaw(core.TagPixelData)
	if !bytes.Equal(gr, wr) {
		t.Fatalf("stored samples %v, want %v", gr, wr)
	}
	gc, ge := legacyDisplay(got)
	wc, we := legacyDisplay(want)
	if ge != nil || we != nil || !slices.Equal(gc, wc) {
		t.Fatalf("display %v / %v, want %v / %v", gc, ge, wc, we)
	}
}

func TestLegacyPixelRepairsNativeDisplayAndTranscode(t *testing.T) {
	decoders := pixeldata.NewMemoryRegistry()
	if err := rle.Register(decoders); err != nil {
		t.Fatal(err)
	}
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	for _, tc := range legacyCases() {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.source.Elements()
			var report pixeldata.DecompressReport
			out, _, err := pixeldata.DecompressDataSet(tc.source, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: tc.recovery, Report: &report})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(report.Diagnostics, []string{tc.diagnostic}) {
				t.Fatal(report.Diagnostics)
			}
			assertLegacyEquivalent(t, out, tc.want)
			if !reflect.DeepEqual(before, tc.source.Elements()) {
				t.Fatal("source mutated")
			}
			strict, _, strictErr := pixeldata.DecompressDataSet(tc.source, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{})
			if strictErr == nil {
				sc, se := legacyDisplay(strict)
				wc, we := legacyDisplay(tc.want)
				if tc.diagnostic == pixeldata.DiagnosticRetiredARGBToRGB {
					if se != nil || we != nil || !slices.Equal(sc, wc) {
						t.Fatal("existing strict ARGB display regressed")
					}
				} else if se == nil && we == nil && slices.Equal(sc, wc) {
					t.Fatal("strict path silently repaired legacy input")
				}
			}
			for _, target := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.RLELossless} {
				options := pixeldata.TranscodeOptions{Recovery: tc.recovery, DecoderRegistry: decoders, EncoderRegistry: encoders}
				if target.Encapsulated && (tc.recovery.NativePlanarLayout == pixeldata.NativePlanarByFrame || tc.recovery.NativePlanarLayout == pixeldata.NativePlanarWholeImage) {
					interleaved := uint16(0)
					options.Rewrite = &pixeldata.NativeRewrite{PlanarConfiguration: &interleaved}
				}
				transcoded, tr, err := pixeldata.TranscodeDataSet(context.Background(), tc.source, transfer.ExplicitVRLittleEndian, target, options)
				if err != nil {
					t.Fatal(target.UID, err)
				}
				if !slices.Equal(tr.DecodeDiagnostics, []string{tc.diagnostic}) || tr.PixelDataPreserved {
					t.Fatalf("repair bypassed: %+v", tr)
				}
				decoded, _, err := pixeldata.DecompressDataSet(transcoded, target, pixeldata.DecompressOptions{Registry: decoders})
				if err != nil {
					t.Fatal(err)
				}
				if target.Encapsulated {
					gc, ge := legacyDisplay(decoded)
					wc, we := legacyDisplay(tc.want)
					if ge != nil || we != nil || !slices.Equal(gc, wc) {
						t.Fatalf("encoded display %v / %v, want %v / %v", gc, ge, wc, we)
					}
				} else {
					assertLegacyEquivalent(t, decoded, tc.want)
				}
			}
		})
	}
}

func TestLegacyPixelRepairsRejectInvalidProfilesAndRespectLimits(t *testing.T) {
	for _, mutate := range []func(*object.Object){
		func(o *object.Object) { o.Put(recoveryUS(0x00280103, 1)) },
		func(o *object.Object) { o.Put(recoveryUS(0x00280101, 7)) },
		func(o *object.Object) { o.Put(recoveryUS(0x00280102, 6)) },
		func(o *object.Object) { o.Put(recoveryUS(0x00280006, 2)) },
		func(o *object.Object) { o.Put(recoveryUS(0x00280002, 3)) },
	} {
		src := legacyImage(8, 4, 0, "CMYK", []uint16{0, 255, 255, 0, 100, 20, 50, 30})
		mutate(src)
		var report pixeldata.DecompressReport
		out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}, Report: &report})
		if err == nil || out != nil || len(report.Diagnostics) != 0 {
			t.Fatalf("invalid profile accepted: %v", err)
		}
	}
	for _, tc := range legacyCases() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		out, _, err := pixeldata.DecompressDataSetContext(ctx, tc.source, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: tc.recovery})
		if !errors.Is(err, context.Canceled) || out != nil {
			t.Fatal("cancel not honored", err)
		}
		out, _, err = pixeldata.DecompressDataSet(tc.source, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: tc.recovery, Limits: pixeldata.DecompressLimits{MaxNativeBytes: 1}})
		if !errors.Is(err, pixeldata.ErrDecompressResourceLimit) || out != nil {
			t.Fatal("native allocation admission bypassed", err)
		}
	}
	profile := pixeldata.DetectableDecodeRecovery()
	if profile.NativePlanarLayout != pixeldata.NativePlanarDeclared || profile.NativeOWByteSwap || profile.RLEReversedByteSegments || profile.WrongPaletteAttributeTags || profile.ConvertRetiredColorModels {
		t.Fatal("producer assumptions enabled by detectable retry")
	}
	src := legacyImage(8, 3, 0, "RGB", []uint16{1, 2, 3, 4, 5, 6})
	if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{NativePlanarLayout: 99}}); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
		t.Fatal("invalid planar option accepted", err)
	}
	if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{NativeOWByteSwap: true}}); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
		t.Fatal("OB byte swapping accepted", err)
	}
}

func TestLegacyPixelRepairsBigEndianSourceAndMetadataOnlyIcon(t *testing.T) {
	for _, tc := range legacyCases() {
		metadata, err := pixeldata.ExtractMetadata(tc.source)
		if err != nil || metadata.BitsAllocated != 16 {
			continue
		}
		t.Run(tc.name, func(t *testing.T) {
			bigEndian, _, err := pixeldata.TranscodeDataSet(context.Background(), tc.source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{})
			if err != nil {
				t.Fatal(err)
			}
			out, _, err := pixeldata.DecompressDataSet(bigEndian, transfer.ExplicitVRBigEndian, pixeldata.DecompressOptions{Recovery: tc.recovery})
			if err != nil {
				t.Fatal(err)
			}
			assertLegacyEquivalent(t, out, tc.want)
			// Native output in the other byte order must also run the repair
			// instead of taking the ordinary native-clone shortcut.
			encoded, _, err := pixeldata.TranscodeDataSet(context.Background(), tc.source, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, pixeldata.TranscodeOptions{Recovery: tc.recovery})
			if err != nil {
				t.Fatal(err)
			}
			roundtrip, _, err := pixeldata.DecompressDataSet(encoded, transfer.ExplicitVRBigEndian, pixeldata.DecompressOptions{})
			if err != nil {
				t.Fatal(err)
			}
			assertLegacyEquivalent(t, roundtrip, tc.want)
		})
	}
	icon := legacyImage(8, 4, 0, "CMYK", []uint16{0, 255, 255, 0, 100, 20, 50, 30})
	// A metadata-only native parent is still valid; its separate icon is the
	// only pixel module requiring repair and verification.
	parent := withIcon(object.FromElements(nil, nil), icon)
	before := parent.Elements()
	out, report, err := pixeldata.TranscodeDataSet(context.Background(), parent, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{Recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Icons) != 1 || !slices.Equal(report.Icons[0].DecodeDiagnostics, []string{pixeldata.DiagnosticRetiredCMYKToRGB}) || !reflect.DeepEqual(before, parent.Elements()) {
		t.Fatalf("icon recovery not diagnosed/atomic: %+v", report)
	}
	assertLegacyEquivalent(t, firstIcon(t, out), legacyImage(8, 3, 0, "RGB", []uint16{255, 0, 0, 125, 205, 175}))
}

func TestLegacyPixelRepairsPaletteConflictsAreAtomic(t *testing.T) {
	for _, mutate := range []func(*object.Object){
		func(o *object.Object) { o.Remove(core.NewTag(0x0028, 0x1213)) },
		func(o *object.Object) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x1201), core.VROW, []byte{1, 2}))
		},
		func(o *object.Object) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x1221), core.VROW, []byte{1, 2}))
		},
		func(o *object.Object) { o.Put(core.NewRawElement(core.NewTag(0x0028, 0x1211), core.VROW, nil)) },
	} {
		src := legacyCases()[len(legacyCases())-1].source
		mutate(src)
		before := src.Elements()
		report := pixeldata.DecompressReport{Diagnostics: []string{"previous"}}
		out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{WrongPaletteAttributeTags: true}, Report: &report})
		if err == nil || out != nil || !reflect.DeepEqual(before, src.Elements()) || !slices.Equal(report.Diagnostics, []string{"previous"}) {
			t.Fatalf("palette refusal not atomic: %v %+v", err, report)
		}
	}
}

func mixedDepthARGB16(planar uint16) *object.Object {
	raw := []uint16{1, 0, 0, 0, 2, 0, 0, 0}
	if planar == 1 {
		raw = []uint16{1, 2, 0, 0, 0, 0, 0, 0}
	}
	src := legacyImage(16, 4, planar, "ARGB", raw)
	legacyPalette(src, false)
	return src
}

func TestLegacyPixelRepairsARGBMixedDepthAndCombinedPalette(t *testing.T) {
	for _, planar := range []uint16{0, 1} {
		src := mixedDepthARGB16(planar)
		src.Put(core.NewRawElement(core.NewTag(0x0028, 0x2000), core.VROB, []byte{1, 2}))
		src.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x2002), VR: core.VRCS}, Value: core.StringValue{"ARGB"}})
		var report pixeldata.DecompressReport
		out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}, Report: &report})
		if err != nil {
			t.Fatal(err)
		}
		assertLegacyEquivalent(t, out, legacyImage(16, 3, 0, "RGB", []uint16{240 * 257, 20 * 257, 10 * 257, 30 * 257, 220 * 257, 40 * 257}))
		original, e1 := legacyDisplay(src)
		normalized, e2 := legacyDisplay(out)
		if e1 != nil || e2 != nil || !slices.Equal(original, normalized) {
			t.Fatal("existing mixed-depth ARGB semantics changed", e1, e2)
		}
		if out.Has(core.NewTag(0x0028, 0x2000)) || out.Has(core.NewTag(0x0028, 0x2002)) || out.Has(core.NewTag(0x0028, 0x1101)) {
			t.Fatal("obsolete color profile or palette retained after RGB conversion")
		}
		if !src.Has(core.NewTag(0x0028, 0x2000)) {
			t.Fatal("source profile mutated")
		}
		for c := uint16(0); c < 3; c++ {
			src.Remove(core.NewTag(0x0028, 0x1101+c))
			src.Remove(core.NewTag(0x0028, 0x1201+c))
		}
		legacyPalette(src, true)
		out, _, err = pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{WrongPaletteAttributeTags: true, ConvertRetiredColorModels: true}, Report: &report})
		if err != nil || !slices.Equal(report.Diagnostics, []string{pixeldata.DiagnosticWrongPaletteAttributeTags, pixeldata.DiagnosticRetiredARGBToRGB}) {
			t.Fatalf("combined palette/ARGB recovery: %+v %v", report, err)
		}
		if out.Has(core.NewTag(0x0028, 0x1111)) {
			t.Fatal("consumed incorrect tags retained")
		}
	}
	src := mixedDepthARGB16(0)
	src.Put(core.NewRawElement(core.NewTag(0x0028, 0x1201), core.VROW, make([]byte, 2*65536+2)))
	if out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}}); !errors.Is(err, pixeldata.ErrUnsupportedDecompression) || out != nil {
		t.Fatal("unbounded palette admitted", err)
	}
}

type legacyCancelAfterChecks struct {
	context.Context
	remaining atomic.Int32
}

func (c *legacyCancelAfterChecks) Err() error {
	if c.remaining.Add(-1) <= 0 {
		return context.Canceled
	}
	return nil
}

func TestLegacyPixelRepairsCancellationDuringSampleWork(t *testing.T) {
	for _, model := range []string{"RGB", "CMYK"} {
		samples := uint16(3)
		if model == "CMYK" {
			samples = 4
		}
		src := legacyImage(8, samples, 0, model, make([]uint16, 10000*int(samples)))
		recovery := pixeldata.DecodeRecovery{NativePlanarLayout: pixeldata.NativePlanarWholeImage}
		if model == "CMYK" {
			recovery = pixeldata.DecodeRecovery{ConvertRetiredColorModels: true}
		}
		ctx := &legacyCancelAfterChecks{Context: context.Background()}
		ctx.remaining.Store(5)
		report := pixeldata.DecompressReport{Diagnostics: []string{"previous"}}
		before := src.Elements()
		out, _, err := pixeldata.DecompressDataSetContext(ctx, src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: recovery, Report: &report})
		if !errors.Is(err, context.Canceled) || out != nil || !reflect.DeepEqual(before, src.Elements()) || !slices.Equal(report.Diagnostics, []string{"previous"}) {
			t.Fatal("mid-work cancellation not atomic", err)
		}
	}
}
