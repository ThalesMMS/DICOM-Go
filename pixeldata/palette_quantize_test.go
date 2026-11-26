package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/validation"
)

func paletteQuantizeRaw(element core.Element) []byte {
	raw, _ := element.RawBytes()
	return raw
}

func paletteQuantizeSource(rows, columns, frames, planar int, rgb []byte) *object.Object {
	data := append([]byte(nil), rgb...)
	if planar == 1 {
		n := rows * columns
		for f := 0; f < frames; f++ {
			for i := 0; i < n; i++ {
				for c := 0; c < 3; c++ {
					data[f*n*3+c*n+i] = rgb[f*n*3+3*i+c]
				}
			}
		}
	}
	if len(data)%2 != 0 {
		data = append(data, 0)
	}
	return derivedio.Object(
		derivedio.US(derivedio.TagRows, uint16(rows)), derivedio.US(derivedio.TagColumns, uint16(columns)),
		derivedio.US(derivedio.TagSamplesPerPixel, 3), derivedio.CS(derivedio.TagPhotometricInterpretation, "RGB"),
		derivedio.US(core.NewTag(0x0028, 0x0006), uint16(planar)), derivedio.IS(derivedio.TagNumberOfFrames, frames),
		derivedio.US(derivedio.TagBitsAllocated, 8), derivedio.US(derivedio.TagBitsStored, 8),
		derivedio.US(derivedio.TagHighBit, 7), derivedio.US(derivedio.TagPixelRepresentation, 0),
		core.NewRawElement(core.TagPixelData, core.VROB, data),
		derivedio.UI(derivedio.TagSOPInstanceUID, "1.2.826.0.1.3680043.10.543.1325.99"),
	)
}

func paletteQuantizeRendered(t *testing.T, obj *object.Object) []*image.RGBA {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		t.Fatal(err)
	}
	lut, err := display.PaletteFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	m := frames.Metadata
	images := make([]*image.RGBA, len(frames.Data))
	for i, data := range frames.Data {
		images[i], err = display.RenderColor(display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: data,
			Photometric: m.PhotometricInterpretation, SamplesPerPixel: int(m.SamplesPerPixel), Palette: lut,
			Format: display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit)}})
		if err != nil {
			t.Fatal(err)
		}
	}
	return images
}

func paletteQuantizeError(rgb []byte, images []*image.RGBA) (float64, uint8) {
	var sum float64
	var largest uint8
	pixel := 0
	for _, img := range images {
		for y := 0; y < img.Bounds().Dy(); y++ {
			for x := 0; x < img.Bounds().Dx(); x++ {
				p := img.PixOffset(x, y)
				for c := 0; c < 3; c++ {
					d := int(rgb[pixel*3+c]) - int(img.Pix[p+c])
					sum += float64(d * d)
					if d < 0 {
						d = -d
					}
					if uint8(d) > largest {
						largest = uint8(d)
					}
				}
				pixel++
			}
		}
	}
	return math.Sqrt(sum / float64(len(rgb))), largest
}

func paletteQuantizeGradient(width, height int) []byte {
	rgb := make([]byte, width*height*3)
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			p := (y*width + x) * 3
			rgb[p], rgb[p+1], rgb[p+2] = byte(x*255/(width-1)), byte(y*255/(height-1)), byte((x+y)*255/(width+height-2))
		}
	}
	return rgb
}

func TestPaletteQuantizeExactGlobalPlanarAndPadding(t *testing.T) {
	// Three odd-length frames share nine exact colors, including two colors
	// in the same 5-bit histogram bin. No padding may separate the frames.
	rgb := []byte{0, 0, 0, 1, 2, 3, 255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 255, 11, 22, 33, 33, 22, 11, 123, 45, 67}
	for _, planar := range []int{0, 1} {
		for _, dither := range []bool{false, true} {
			t.Run(fmt.Sprintf("planar%d/dither%v", planar, dither), func(t *testing.T) {
				source := paletteQuantizeSource(1, 3, 3, planar, rgb)
				before, _ := source.Get(core.TagPixelData)
				original := append([]byte(nil), paletteQuantizeRaw(before)...)
				result, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{Dither: dither})
				if err != nil {
					t.Fatal(err)
				}
				if result.Colors != 9 || result.Frames != 3 || result.RMSE != 0 || result.MaxAbsoluteError != 0 {
					t.Fatalf("report: %+v", result)
				}
				if result.ImagePixel.Has(core.NewTag(0x0028, 0x0006)) || result.ImagePixel.Has(derivedio.TagSOPInstanceUID) {
					t.Fatal("output copied planar configuration or source identity")
				}
				pixels, _ := result.ImagePixel.Get(core.TagPixelData)
				if pixels.VR() != core.VROB || len(paletteQuantizeRaw(pixels)) != 10 || paletteQuantizeRaw(pixels)[9] != 0 {
					t.Fatalf("pixel value has wrong VR/padding: %+v", pixels.Header)
				}
				images := paletteQuantizeRendered(t, result.ImagePixel)
				if rmse, _ := paletteQuantizeError(rgb, images); rmse != 0 {
					t.Fatalf("exact-color RMSE %v", rmse)
				}
				for c := 0; c < 3; c++ {
					descriptor, _ := result.ImagePixel.Get(core.NewTag(0x0028, uint16(0x1101+c)))
					data, _ := result.ImagePixel.Get(core.NewTag(0x0028, uint16(0x1201+c)))
					if descriptor.VR() != core.VRUS || !bytes.Equal(paletteQuantizeRaw(descriptor), []byte{9, 0, 0, 0, 16, 0}) || data.VR() != core.VROW || len(paletteQuantizeRaw(data)) != 18 {
						t.Fatal("invalid palette descriptor/data")
					}
					for i := 0; i < len(paletteQuantizeRaw(data)); i += 2 {
						if binary.LittleEndian.Uint16(paletteQuantizeRaw(data)[i:])%257 != 0 {
							t.Fatal("RGB8 palette was not expanded to 16 bits")
						}
					}
				}
				// Output is independently owned, even when modified by the caller.
				paletteQuantizeRaw(pixels)[0] ^= 1
				palette, _ := result.ImagePixel.Get(core.NewTag(0x0028, 0x1201))
				paletteQuantizeRaw(palette)[0] ^= 1
				if !bytes.Equal(paletteQuantizeRaw(before), original) {
					t.Fatal("quantization or output mutation changed source")
				}
			})
		}
	}
}

func TestPaletteQuantizeQualityDeterminismAndFrameReset(t *testing.T) {
	gradient := paletteQuantizeGradient(64, 64)
	rgb := append(append([]byte(nil), gradient...), gradient...)
	source := paletteQuantizeSource(64, 64, 2, 0, rgb)
	for _, dither := range []bool{false, true} {
		opts := pixeldata.PaletteQuantizeOptions{MaxColors: 64, Dither: dither, MaxRMSE: 16}
		a, err := pixeldata.QuantizePaletteColor(context.Background(), source, opts)
		if err != nil {
			t.Fatal(err)
		}
		b, err := pixeldata.QuantizePaletteColor(context.Background(), source, opts)
		if err != nil {
			t.Fatal(err)
		}
		for _, element := range a.ImagePixel.Elements() {
			other, ok := b.ImagePixel.Get(element.Tag())
			if !ok || !bytes.Equal(paletteQuantizeRaw(element), paletteQuantizeRaw(other)) {
				t.Fatalf("nondeterministic element %v", element.Tag())
			}
		}
		frames, err := pixeldata.ExtractNativeFrames(a.ImagePixel)
		if err != nil || !bytes.Equal(frames.Data[0], frames.Data[1]) {
			t.Fatalf("dither crossed frames: %v", err)
		}
		rmse, largest := paletteQuantizeError(rgb, paletteQuantizeRendered(t, a.ImagePixel))
		if math.Abs(a.RMSE-rmse) > 1e-12 || a.MaxAbsoluteError != largest || a.Colors > 64 {
			t.Fatalf("independent error report differs: %+v vs %.12f/%d", a, rmse, largest)
		}
		t.Logf("dither=%v RGB8 RMSE=%.6f max=%d colors=%d", dither, rmse, largest, a.Colors)
	}
	result, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{MaxColors: 2, MaxRMSE: 0.01})
	if result != nil || !errors.Is(err, pixeldata.ErrPaletteQuantizeQuality) {
		t.Fatalf("quality rejection: result=%v err=%v", result, err)
	}
}

func TestPaletteQuantizeBudgetAndDomain(t *testing.T) {
	source := paletteQuantizeSource(1, 1, 1, 0, []byte{20, 30, 40})
	for _, opts := range []pixeldata.PaletteQuantizeOptions{{MaxColors: -1}, {MaxColors: 257}, {MaxPixels: -1}, {MaxRMSE: -1}, {MaxRMSE: math.NaN()}, {MaxRMSE: math.Inf(1)}} {
		if _, err := pixeldata.QuantizePaletteColor(nil, source, opts); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
			t.Fatalf("invalid options %+v: %v", opts, err)
		}
	}
	if _, err := pixeldata.QuantizePaletteColor(nil, nil, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pixeldata.QuantizePaletteColor(ctx, source, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	for _, change := range []core.Element{derivedio.CS(derivedio.TagPhotometricInterpretation, "YBR_FULL"), derivedio.US(derivedio.TagBitsAllocated, 16), derivedio.US(derivedio.TagBitsStored, 7), derivedio.US(derivedio.TagHighBit, 6), derivedio.US(derivedio.TagPixelRepresentation, 1), derivedio.US(derivedio.TagSamplesPerPixel, 4), derivedio.US(core.NewTag(0x0028, 0x0006), 2), derivedio.US(derivedio.TagRows, 0)} {
		input := paletteQuantizeSource(1, 1, 1, 0, []byte{20, 30, 40})
		input.Put(change)
		if _, err := pixeldata.QuantizePaletteColor(nil, input, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
			t.Fatalf("unsupported %v: %v", change.Tag(), err)
		}
	}
	source.Remove(core.NewTag(0x0028, 0x0006))
	if _, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
		t.Fatal(err)
	}
	source = paletteQuantizeSource(1, 2, 1, 0, []byte{20, 30, 40, 50, 60, 70})
	if _, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{MaxPixels: 1}); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
		t.Fatal(err)
	}
	source.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{20, 30}))
	if _, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrPixelDataSizeMismatch) {
		t.Fatal(err)
	}
	source = paletteQuantizeSource(1, 1, 1, 0, []byte{20, 30, 40})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength}, Value: core.FragmentSequence{Fragments: [][]byte{{1, 2}}}})
	if _, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrEncapsulatedPixelData) {
		t.Fatal(err)
	}
}

type paletteQuantizeCancelContext struct {
	context.Context
	cancel context.CancelFunc
	after  int
	calls  int
}

func (ctx *paletteQuantizeCancelContext) Err() error {
	ctx.calls++
	if ctx.calls == ctx.after {
		ctx.cancel()
	}
	return ctx.Context.Err()
}

func TestPaletteQuantizeCancellationDuringWork(t *testing.T) {
	// A single-color image takes the exact-palette path: call 3 cancels
	// histogram construction; call 67 cancels the second mapping row.
	source := paletteQuantizeSource(64, 64, 1, 0, make([]byte, 64*64*3))
	for _, after := range []int{3, 67} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &paletteQuantizeCancelContext{Context: base, cancel: cancel, after: after}
		result, err := pixeldata.QuantizePaletteColor(ctx, source, pixeldata.PaletteQuantizeOptions{})
		cancel()
		if result != nil || !errors.Is(err, context.Canceled) || ctx.calls != after {
			t.Fatalf("cancel at %d: result=%v err=%v calls=%d", after, result, err, ctx.calls)
		}
	}
}

func TestPaletteQuantizeColorBudgetExtremes(t *testing.T) {
	for _, count := range []int{1, 256} {
		rgb := make([]byte, count*3)
		for i := 0; i < count; i++ {
			rgb[i*3], rgb[i*3+1], rgb[i*3+2] = byte(i), byte(255-i), byte(i^127)
		}
		result, err := pixeldata.QuantizePaletteColor(nil, paletteQuantizeSource(1, count, 1, 0, rgb), pixeldata.PaletteQuantizeOptions{MaxColors: count})
		if err != nil || result.Colors != count || result.RMSE != 0 {
			t.Fatalf("%d exact colors: result=%v err=%v", count, result, err)
		}
	}
}

func TestPaletteQuantizeBigEndianByteDomain(t *testing.T) {
	rgb := []byte{10, 20, 30, 40, 50, 60}
	source := paletteQuantizeSource(1, 2, 1, 0, rgb)
	// OB bytes do not depend on byte order; the numeric attributes do.
	for _, element := range source.Elements() {
		if element.VR() == core.VRUS {
			raw := append([]byte(nil), paletteQuantizeRaw(element)...)
			for i := 0; i < len(raw); i += 2 {
				raw[i], raw[i+1] = raw[i+1], raw[i]
			}
			source.Put(core.NewRawElement(element.Tag(), core.VRUS, raw))
		}
	}
	source.SetValueByteOrder(binary.BigEndian)
	result, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{})
	if err != nil || result.RMSE != 0 || result.ImagePixel.ValueByteOrder() != binary.LittleEndian {
		t.Fatalf("big-endian attributes with OB: result=%v err=%v", result, err)
	}
	if rmse, _ := paletteQuantizeError(rgb, paletteQuantizeRendered(t, result.ImagePixel)); rmse != 0 {
		t.Fatal("big-endian OB samples changed")
	}
	source.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{20, 10, 40, 30, 60, 50}))
	if _, err := pixeldata.QuantizePaletteColor(nil, source, pixeldata.PaletteQuantizeOptions{}); !errors.Is(err, pixeldata.ErrPaletteQuantize) {
		t.Fatalf("big-endian OW was silently interpreted as OB: %v", err)
	}
}

func paletteQuantizeSC(t *testing.T, obj *object.Object, suffix string) *object.File {
	t.Helper()
	dataset := object.FromElements(obj.Elements(), nil)
	for _, element := range []core.Element{
		derivedio.UI(derivedio.TagSOPClassUID, "1.2.840.10008.5.1.4.1.1.7"),
		derivedio.UI(derivedio.TagSOPInstanceUID, "1.2.826.0.1.3680043.10.543.1325."+suffix),
		derivedio.Strings(core.NewTag(0x0008, 0x0008), core.VRCS, []string{"DERIVED", "SECONDARY"}),
		derivedio.CS(core.NewTag(0x0008, 0x0064), "SYN"), derivedio.CS(derivedio.TagModality, "OT"),
		derivedio.Str(core.NewTag(0x0010, 0x0010), core.VRPN, "SYNTHETIC^PALETTE"), derivedio.LO(core.NewTag(0x0010, 0x0020), "SYNTHETIC1325"),
		derivedio.Str(core.NewTag(0x0010, 0x0030), core.VRDA, ""), derivedio.CS(core.NewTag(0x0010, 0x0040), ""),
		derivedio.Str(core.NewTag(0x0008, 0x0020), core.VRDA, "20261009"), derivedio.Str(core.NewTag(0x0008, 0x0030), core.VRTM, "120000"),
		derivedio.Str(core.NewTag(0x0008, 0x0090), core.VRPN, ""), derivedio.SH(core.NewTag(0x0008, 0x0050), ""),
		derivedio.UI(derivedio.TagStudyInstanceUID, "1.2.826.0.1.3680043.10.543.1325.1"),
		derivedio.UI(derivedio.TagSeriesInstanceUID, "1.2.826.0.1.3680043.10.543.1325.2"),
		derivedio.SH(core.NewTag(0x0020, 0x0010), "SYNTHETIC"), derivedio.IS(core.NewTag(0x0020, 0x0011), 1), derivedio.IS(core.NewTag(0x0020, 0x0013), 1),
		derivedio.Strings(core.NewTag(0x0020, 0x0020), core.VRCS, []string{"", ""}), derivedio.LO(core.NewTag(0x0008, 0x0070), "dicom-go synthetic fixture"),
		derivedio.Str(core.NewTag(0x0008, 0x2111), core.VRST, "RGB8 palette quantization; synthetic fixture"),
	} {
		dataset.Put(element)
	}
	file, err := derivedio.File("1.2.840.10008.5.1.4.1.1.7", "1.2.826.0.1.3680043.10.543.1325."+suffix, dataset)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

func TestPaletteQuantizePart10RoundTrip(t *testing.T) {
	rgb := paletteQuantizeGradient(128, 128)
	for _, dither := range []bool{false, true} {
		result, err := pixeldata.QuantizePaletteColor(nil, paletteQuantizeSource(128, 128, 1, 0, rgb), pixeldata.PaletteQuantizeOptions{Dither: dither, MaxRMSE: 16})
		if err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, paletteQuantizeSC(t, result.ImagePixel, "3")); err != nil {
			t.Fatal(err)
		}
		file, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		if report, err := validation.ValidateFile(context.Background(), file.Meta.ToDataSet(), file.Dataset.ToDataSet(), file.TransferSyntax, validation.Options{Mode: validation.ModeStrict, Dictionary: std.Dictionary, ByteOrder: binary.LittleEndian, RequiredUIDs: []core.Tag{derivedio.TagSOPClassUID, derivedio.TagSOPInstanceUID, derivedio.TagStudyInstanceUID, derivedio.TagSeriesInstanceUID}}); err != nil {
			t.Fatalf("Part10 VR/VM/meta validation: %v %+v", err, report)
		}
		rmse, largest := paletteQuantizeError(rgb, paletteQuantizeRendered(t, file.Dataset))
		if rmse > 16 || rmse != result.RMSE || largest != result.MaxAbsoluteError {
			t.Fatalf("round-trip RGB8 error: %.6f/%d vs %+v", rmse, largest, result)
		}
		t.Logf("Part10 dither=%v colors=%d RGB8 RMSE=%.6f max=%d", dither, result.Colors, rmse, largest)
	}
}
