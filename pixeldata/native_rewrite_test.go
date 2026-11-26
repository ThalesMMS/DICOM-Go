package pixeldata_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpeglossless"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func us(group, element uint16, v uint16) core.Element {
	return core.NewRawElement(core.NewTag(group, element), core.VRUS, binary.LittleEndian.AppendUint16(nil, v))
}

func text(group, element uint16, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(group, element), VR: vr}, Value: core.StringValue(values)}
}

// image builds a native two-frame image of rows x columns.
func nativeImage(photometric string, samples, bitsAllocated, bitsStored, representation int, pixels []byte, extra ...core.Element) *object.Object {
	vr := core.VROB
	if bitsAllocated > 8 {
		vr = core.VROW
	}
	elements := []core.Element{
		text(0x0008, 0x0016, core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		text(0x0008, 0x0018, core.VRUI, "1.2.826.0.1.3680043.10.1.1036.1"),
		text(0x0028, 0x0004, core.VRCS, photometric),
		text(0x0028, 0x0008, core.VRIS, "2"),
		us(0x0028, 0x0002, uint16(samples)), us(0x0028, 0x0010, 3), us(0x0028, 0x0011, 4),
		us(0x0028, 0x0100, uint16(bitsAllocated)), us(0x0028, 0x0101, uint16(bitsStored)), us(0x0028, 0x0102, uint16(bitsStored-1)),
		us(0x0028, 0x0103, uint16(representation)),
		core.NewRawElement(core.TagPixelData, vr, pixels),
	}
	if samples == 3 {
		elements = append(elements, us(0x0028, 0x0006, 0))
	}
	obj := object.FromElements(append(elements, extra...), nil)
	return obj
}

const pixelsPerFrame = 12 // 3 rows x 4 columns

func colorFrame(t *testing.T, obj *object.Object, frame int) display.ColorFrame {
	t.Helper()
	native, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		t.Fatal(err)
	}
	m := native.Metadata
	palette, err := display.PaletteFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	return display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: native.Data[frame], Photometric: m.PhotometricInterpretation,
		SamplesPerPixel: int(m.SamplesPerPixel), PlanarConfiguration: int(m.PlanarConfiguration), Palette: palette,
		Format: display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit), Signed: m.PixelRepresentation == 1}}
}

// sameColor renders both objects with the display pipeline at 16 bits and
// compares every frame within tolerance (in 16-bit steps).
func sameColor(t *testing.T, a, b *object.Object, tolerance int) {
	t.Helper()
	for f := 0; f < 2; f++ {
		x, err := display.RenderColor16(colorFrame(t, a, f))
		if err != nil {
			t.Fatal(err)
		}
		y, err := display.RenderColor16(colorFrame(t, b, f))
		if err != nil {
			t.Fatal(err)
		}
		for i := 0; i+1 < len(x.Pix); i += 2 {
			a, b := int(x.Pix[i])<<8|int(x.Pix[i+1]), int(y.Pix[i])<<8|int(y.Pix[i+1])
			if d := a - b; d > tolerance || -d > tolerance {
				t.Fatalf("frame %d sample %d: %d vs %d", f, i/2, a, b)
			}
		}
	}
}

func rewrite(t *testing.T, obj *object.Object, r pixeldata.NativeRewrite) (*object.Object, pixeldata.NativeRewriteReport) {
	t.Helper()
	out, report, err := pixeldata.RewriteNative(context.Background(), obj, r)
	if err != nil {
		t.Fatal(err)
	}
	return out, report
}

func TestRewritePlanarConfigurationRoundTrip(t *testing.T) {
	for _, bits := range []int{8, 16} {
		width := bits / 8
		pixels := make([]byte, 2*pixelsPerFrame*3*width)
		for i := range pixels {
			pixels[i] = byte(i*7 + 3)
		}
		source := nativeImage("RGB", 3, bits, bits, 0, pixels)
		one := uint16(1)
		planar, report := rewrite(t, source, pixeldata.NativeRewrite{PlanarConfiguration: &one})
		if len(report.Steps) != 1 || report.Frames != 2 {
			t.Fatalf("report %+v", report)
		}
		m, _ := pixeldata.ExtractMetadata(planar)
		if m.PlanarConfiguration != 1 {
			t.Fatal("Planar Configuration not 1")
		}
		native, _ := pixeldata.ExtractNativeFrames(planar)
		// The red plane of frame 1 holds the first sample of every pixel.
		for i := 0; i < pixelsPerFrame; i++ {
			if !bytes.Equal(native.Data[0][i*width:(i+1)*width], pixels[3*i*width:(3*i+1)*width]) {
				t.Fatalf("%d-bit red plane sample %d", bits, i)
			}
		}
		sameColor(t, source, planar, 0)
		zero := uint16(0)
		back, _ := rewrite(t, planar, pixeldata.NativeRewrite{PlanarConfiguration: &zero})
		raw, _ := back.GetRaw(core.TagPixelData)
		if !bytes.Equal(raw, pixels) {
			t.Fatalf("%d-bit planar round trip changed the pixels", bits)
		}
		if planar.Has(core.NewTag(0x0008, 0x2111)) {
			t.Fatal("a layout change was recorded as a derivation")
		}
	}
}

func paletteImage(extra ...core.Element) *object.Object {
	indices := make([]byte, 2*pixelsPerFrame)
	for i := range indices {
		indices[i] = byte(i * 11 % 256)
	}
	return nativeImage("PALETTE COLOR", 1, 8, 8, 0, indices, extra...)
}

func linearPalette(bits int) []core.Element {
	var elements []core.Element
	for c := 0; c < 3; c++ {
		descriptor := binary.LittleEndian.AppendUint16(nil, 256)
		descriptor = binary.LittleEndian.AppendUint16(descriptor, 0)
		descriptor = binary.LittleEndian.AppendUint16(descriptor, uint16(bits))
		entries := make([]uint16, 256)
		for i := range entries {
			entries[i] = uint16((i*(c+1)*37 + 11*c) % (1 << bits))
		}
		elements = append(elements,
			core.NewRawElement(core.NewTag(0x0028, 0x1101+uint16(c)), core.VRUS, descriptor),
			core.NewRawElement(core.NewTag(0x0028, 0x1201+uint16(c)), core.VROW, display.EncodeLUTData(entries, bits)))
	}
	return elements
}

// segmentedPalette encodes each channel as a discrete segment of 16 entries
// then a linear segment to the end (PS3.3 C.7.9.2).
func segmentedPalette() []core.Element {
	var elements []core.Element
	for c := 0; c < 3; c++ {
		descriptor := binary.LittleEndian.AppendUint16(nil, 256)
		descriptor = binary.LittleEndian.AppendUint16(descriptor, 0)
		descriptor = binary.LittleEndian.AppendUint16(descriptor, 16)
		words := []uint16{0, 16}
		for i := 0; i < 16; i++ {
			words = append(words, uint16(i*1000+c*5000))
		}
		words = append(words, 1, 240, uint16(60000-c*9000))
		var data []byte
		for _, w := range words {
			data = binary.LittleEndian.AppendUint16(data, w)
		}
		elements = append(elements,
			core.NewRawElement(core.NewTag(0x0028, 0x1101+uint16(c)), core.VRUS, descriptor),
			core.NewRawElement(core.NewTag(0x0028, 0x1221+uint16(c)), core.VROW, data))
	}
	return elements
}

func TestRewritePaletteToRGB(t *testing.T) {
	for name, palette := range map[string][]core.Element{"8-bit": linearPalette(8), "16-bit": linearPalette(16), "segmented": segmentedPalette()} {
		t.Run(name, func(t *testing.T) {
			source := paletteImage(append(palette, us(0x0028, 0x0106, 0), text(0x0028, 0x1199, core.VRUI, "1.2.3"))...)
			rgb, report := rewrite(t, source, pixeldata.NativeRewrite{PaletteToRGB: true})
			if len(report.Steps) != 1 {
				t.Fatalf("report %+v", report)
			}
			m, err := pixeldata.ExtractMetadata(rgb)
			if err != nil || m.PhotometricInterpretation != "RGB" || m.SamplesPerPixel != 3 || m.PixelRepresentation != 0 {
				t.Fatalf("metadata %+v %v", m, err)
			}
			for _, tag := range []core.Tag{core.NewTag(0x0028, 0x1101), core.NewTag(0x0028, 0x1201), core.NewTag(0x0028, 0x1221), core.NewTag(0x0028, 0x0106), core.NewTag(0x0028, 0x1199)} {
				if rgb.Has(tag) {
					t.Fatalf("%s kept", tag)
				}
			}
			if d, _ := rgb.GetString(core.NewTag(0x0008, 0x2111)); d != "PALETTE COLOR to RGB" {
				t.Fatalf("Derivation Description %q", d)
			}
			sameColor(t, source, rgb, 0)
		})
	}
}

func TestLinearizeSegmentedPaletteDisplaysTheSame(t *testing.T) {
	source := paletteImage(segmentedPalette()...)
	linear, report := rewrite(t, source, pixeldata.NativeRewrite{LinearizeSegmentedPalette: true})
	if len(report.Steps) != 1 || linear.Has(core.NewTag(0x0028, 0x1221)) || !linear.Has(core.NewTag(0x0028, 0x1201)) {
		t.Fatalf("report %+v", report)
	}
	if m, _ := pixeldata.ExtractMetadata(linear); m.PhotometricInterpretation != "PALETTE COLOR" {
		t.Fatal("photometric changed")
	}
	sameColor(t, source, linear, 0)
}

func TestLinearizeBigEndianSegmentedPalette(t *testing.T) {
	for _, bits := range []uint16{8, 16} {
		source := paletteImage()
		// Convert every numeric metadata value to the declared byte order.
		for _, element := range source.Elements() {
			if element.VR() == core.VRUS {
				raw, _ := element.RawBytes()
				for i := 0; i+1 < len(raw); i += 2 {
					raw[i], raw[i+1] = raw[i+1], raw[i]
				}
			}
		}
		source.SetValueByteOrder(binary.BigEndian)
		for channel := uint16(0); channel < 3; channel++ {
			var descriptor, data []byte
			for _, value := range []uint16{4, 0, bits} {
				descriptor = binary.BigEndian.AppendUint16(descriptor, value)
			}
			for _, value := range []uint16{0, 4, 3, 21, 78, 219} {
				data = binary.BigEndian.AppendUint16(data, value)
			}
			source.Put(core.NewRawElement(core.NewTag(0x0028, 0x1101+channel), core.VRUS, descriptor))
			source.Put(core.NewRawElement(core.NewTag(0x0028, 0x1221+channel), core.VROW, data))
		}
		want, err := display.PaletteFromObject(source)
		if err != nil {
			t.Fatal(err)
		}
		linear, _ := rewrite(t, source, pixeldata.NativeRewrite{LinearizeSegmentedPalette: true})
		got, err := display.PaletteFromObject(linear)
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("%d-bit palette: got %+v, want %+v, error %v", bits, got, want, err)
		}
	}
}

func TestTranscodeRewritePreservesDecodeDiagnostics(t *testing.T) {
	ctx := context.Background()
	options := streamTestRegistries(t)
	source := streamTestSource(4, 6, 2, 3, 8)
	compressed, _, err := pixeldata.TranscodeDataSet(ctx, source, transfer.ExplicitVRLittleEndian, transfer.JPEGLosslessSV1, options)
	if err != nil {
		t.Fatal(err)
	}
	compressed.Put(us(0x0028, 0x0010, 5)) // The codestream still has four rows.
	one := uint16(1)
	options.Rewrite = &pixeldata.NativeRewrite{PlanarConfiguration: &one}
	options.Recovery.ReconcileWithCodestream = true
	output, report, err := pixeldata.TranscodeDataSet(ctx, compressed, transfer.JPEGLosslessSV1, transfer.ExplicitVRLittleEndian, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewrite == nil || !slices.Contains(report.DecodeDiagnostics, pixeldata.DiagnosticReconciledWithCodestream) {
		t.Fatalf("report %+v", report)
	}
	metadata, err := pixeldata.ExtractMetadata(output)
	if err != nil || metadata.Rows != 4 || metadata.PlanarConfiguration != 1 {
		t.Fatalf("metadata %+v, error %v", metadata, err)
	}
	sameColor(t, source, output, 0)
}

func TestRewriteYBRFullToRGB(t *testing.T) {
	pixels := make([]byte, 2*pixelsPerFrame*3)
	for i := range pixels {
		pixels[i] = byte(40 + i*13%180)
	}
	for _, planar := range []uint16{0, 1} {
		source := nativeImage("YBR_FULL", 3, 8, 8, 0, pixels)
		source.Put(us(0x0028, 0x0006, planar))
		rgb, _ := rewrite(t, source, pixeldata.NativeRewrite{YBRFullToRGB: true})
		if m, _ := pixeldata.ExtractMetadata(rgb); m.PhotometricInterpretation != "RGB" || m.PlanarConfiguration != planar {
			t.Fatalf("metadata %+v", m)
		}
		sameColor(t, source, rgb, 257) // one 8-bit step
	}
}

func grayFrame(t *testing.T, obj *object.Object, frame int) *image.Gray {
	t.Helper()
	native, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		t.Fatal(err)
	}
	m := native.Metadata
	modality, err := display.ModalityLUTFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	voi, err := display.VOIFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	img, err := display.RenderGray(display.Frame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: native.Data[frame], Modality: modality, VOI: voi,
		Format: display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit), Signed: m.PixelRepresentation == 1}})
	if err != nil {
		t.Fatal(err)
	}
	return img
}

func TestRewriteMonochrome1ToMonochrome2DisplaysTheSame(t *testing.T) {
	for _, tc := range []struct {
		name           string
		bits, signed   int
		center, width  string
		function       string
		slope          string
		wantPad, wantS int
	}{
		{"12-bit unsigned", 12, 0, "1800", "1200", "", "1", 4095 - 7, 4095 - 4000},
		{"16-bit signed with rescale", 16, 1, "-100", "900", "", "2", -1 - 7, -1 - 4000},
		{"LINEAR_EXACT", 12, 0, "1800", "1200", "LINEAR_EXACT", "1", 4095 - 7, 4095 - 4000},
		// A narrow LINEAR window needs its thresholds at c-0.5 mirrored exactly.
		{"narrow LINEAR window", 12, 0, "1989", "4", "", "1", 4095 - 7, 4095 - 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pixels := make([]byte, 2*pixelsPerFrame*2)
			for i := 0; i < 2*pixelsPerFrame; i++ {
				v := (i*331 + 5) % (1 << tc.bits)
				if tc.signed == 1 {
					v -= 1 << (tc.bits - 1)
				}
				binary.LittleEndian.PutUint16(pixels[2*i:], uint16(v))
			}
			extra := []core.Element{text(0x0028, 0x1050, core.VRDS, tc.center), text(0x0028, 0x1051, core.VRDS, tc.width),
				text(0x0028, 0x1052, core.VRDS, "-3"), text(0x0028, 0x1053, core.VRDS, tc.slope),
				us(0x0028, 0x0120, 7), us(0x0028, 0x0107, 4000), text(0x2050, 0x0020, core.VRCS, "INVERSE")}
			if tc.function != "" {
				extra = append(extra, text(0x0028, 0x1056, core.VRCS, tc.function))
			}
			if tc.signed == 1 {
				extra[4] = core.NewRawElement(core.NewTag(0x0028, 0x0120), core.VRSS, binary.LittleEndian.AppendUint16(nil, 7))
				extra[5] = core.NewRawElement(core.NewTag(0x0028, 0x0107), core.VRSS, binary.LittleEndian.AppendUint16(nil, 4000))
			}
			source := nativeImage("MONOCHROME1", 1, 16, tc.bits, tc.signed, pixels, extra...)
			mono2, _ := rewrite(t, source, pixeldata.NativeRewrite{Monochrome1ToMonochrome2: true})
			for f := 0; f < 2; f++ {
				a, b := grayFrame(t, source, f), grayFrame(t, mono2, f)
				for i := range a.Pix {
					if d := int(255-a.Pix[i]) - int(b.Pix[i]); d > 1 || d < -1 {
						t.Fatalf("frame %d pixel %d: inverted MONOCHROME1 %d, MONOCHROME2 %d", f, i, 255-a.Pix[i], b.Pix[i])
					}
				}
			}
			pad, _ := mono2.GetRaw(core.NewTag(0x0028, 0x0120))
			smallest, _ := mono2.GetRaw(core.NewTag(0x0028, 0x0106))
			if int(int16(binary.LittleEndian.Uint16(pad))) != tc.wantPad && int(binary.LittleEndian.Uint16(pad)) != tc.wantPad ||
				int(int16(binary.LittleEndian.Uint16(smallest))) != tc.wantS && int(binary.LittleEndian.Uint16(smallest)) != tc.wantS || mono2.Has(core.NewTag(0x0028, 0x0107)) {
				t.Fatalf("padding %v, smallest %v", pad, smallest)
			}
			if shape, _ := mono2.GetString(core.NewTag(0x2050, 0x0020)); shape != "IDENTITY" {
				t.Fatalf("Presentation LUT Shape %q", shape)
			}
		})
	}
}

func TestRewriteRefusals(t *testing.T) {
	voiLUT := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x3010), VR: core.VRSQ}, Value: core.SequenceValue{}}
	mono1 := nativeImage("MONOCHROME1", 1, 8, 8, 0, make([]byte, 2*pixelsPerFrame), voiLUT)
	if _, _, err := pixeldata.RewriteNative(context.Background(), mono1, pixeldata.NativeRewrite{Monochrome1ToMonochrome2: true}); !errors.Is(err, pixeldata.ErrNativeRewrite) {
		t.Fatalf("MONOCHROME1 with a VOI LUT Sequence: %v", err)
	}
	signedYBR := nativeImage("YBR_FULL", 3, 8, 8, 1, make([]byte, 2*pixelsPerFrame*3))
	if _, _, err := pixeldata.RewriteNative(context.Background(), signedYBR, pixeldata.NativeRewrite{YBRFullToRGB: true}); !errors.Is(err, pixeldata.ErrNativeRewrite) {
		t.Fatalf("signed YBR_FULL: %v", err)
	}
	// A request that does not apply leaves the object unchanged.
	rgb := nativeImage("RGB", 3, 8, 8, 0, make([]byte, 2*pixelsPerFrame*3))
	out, report, err := pixeldata.RewriteNative(context.Background(), rgb, pixeldata.NativeRewrite{PaletteToRGB: true, YBRFullToRGB: true, Monochrome1ToMonochrome2: true})
	if err != nil || len(report.Steps) != 0 || out.Has(core.NewTag(0x0008, 0x2111)) {
		t.Fatalf("no-op rewrite: %+v %v", report, err)
	}
}

// TestTranscodeRewriteReachesRestrictedEncoder transcodes PALETTE COLOR to
// JPEG Lossless, whose encoder refuses palettes, through Rewrite.
func TestTranscodeRewriteReachesRestrictedEncoder(t *testing.T) {
	source := paletteImage(linearPalette(8)...)
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := jpeglossless.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	decoders := pixeldata.NewMemoryRegistry()
	if err := jpeglossless.Register(decoders); err != nil {
		t.Fatal(err)
	}
	options := pixeldata.TranscodeOptions{EncoderRegistry: encoders, DecoderRegistry: decoders}
	if _, _, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGLosslessSV1, options); err == nil {
		t.Fatal("JPEG Lossless accepted PALETTE COLOR")
	}
	options.Rewrite = &pixeldata.NativeRewrite{PaletteToRGB: true}
	encoded, report, err := pixeldata.TranscodeDataSet(context.Background(), source, transfer.ExplicitVRLittleEndian, transfer.JPEGLosslessSV1, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Rewrite == nil || strings.Join(report.Rewrite.Steps, ",") != "PALETTE COLOR to RGB" || report.Frames != 2 {
		t.Fatalf("report %+v", report)
	}
	back, _, err := pixeldata.TranscodeDataSet(context.Background(), encoded, transfer.JPEGLosslessSV1, transfer.ExplicitVRLittleEndian, pixeldata.TranscodeOptions{DecoderRegistry: decoders})
	if err != nil {
		t.Fatal(err)
	}
	sameColor(t, source, back, 0)
}
