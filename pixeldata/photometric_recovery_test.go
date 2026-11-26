package pixeldata_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/pixeldata/frame"
	"github.com/ThalesMMS/dicom-go/transfer"
	"image/color"
	"reflect"
	"slices"
	"testing"
)

func recoveryUS(tag uint32, v uint16) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(uint16(tag>>16), uint16(tag)), VR: core.VRUS}, Value: core.Uint16Value{v}}
}
func recoveryImage(samples *uint16, name string, pc *uint16, raw []byte) *object.Object {
	es := []core.Element{recoveryUS(0x00280010, 1), recoveryUS(0x00280011, 2), recoveryUS(0x00280100, 8), recoveryUS(0x00280101, 8), recoveryUS(0x00280102, 7), recoveryUS(0x00280103, 0), core.NewRawElement(core.TagPixelData, core.VROB, raw)}
	if samples != nil {
		es = append(es, recoveryUS(0x00280002, *samples))
	}
	if name != "" {
		es = append(es, core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{name}})
	}
	if pc != nil {
		es = append(es, recoveryUS(0x00280006, *pc))
	}
	return object.FromElements(es, nil)
}
func recoveryAssertColors(t *testing.T, out *object.Object, syn transfer.Syntax, want []color.RGBA) {
	t.Helper()
	fs, err := frame.ExtractFrames(&object.File{Dataset: out, TransferSyntax: syn})
	if err != nil {
		t.Fatal(err)
	}
	img, err := fs[0].GetImage()
	if err != nil {
		t.Fatal(err)
	}
	for x, w := range want {
		if got := color.RGBAModel.Convert(img.At(x, 0)).(color.RGBA); got != w {
			t.Fatalf("pixel %d=%v, want %v", x, got, w)
		}
	}
}
func TestPhotometricRecoveryDecodeAndDisplay(t *testing.T) {
	one, three, four, zero := uint16(1), uint16(3), uint16(4), uint16(0)
	mono := []byte{0, 255}
	rgb := []byte{255, 0, 0, 0, 255, 0}
	argb := []byte{0, 255, 0, 0, 0, 0, 255, 0}
	mc := []color.RGBA{{0, 0, 0, 255}, {255, 255, 255, 255}}
	rc := []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}}
	for _, tc := range []struct {
		name    string
		spp     *uint16
		pi      string
		pc      *uint16
		raw     []byte
		wantPI  string
		wantSPP uint16
		diags   []string
		colors  []color.RGBA
	}{
		{"missing mono", &one, "", nil, mono, "MONOCHROME2", 1, []string{pixeldata.DiagnosticPhotometricFromSamples}, mc},
		{"invalid mono", &one, "BROKEN", nil, mono, "MONOCHROME2", 1, []string{pixeldata.DiagnosticInvalidPhotometricReplaced}, mc},
		{"missing RGB", &three, "", &zero, rgb, "RGB", 3, []string{pixeldata.DiagnosticPhotometricFromSamples}, rc},
		{"invalid RGB missing PC", &three, "BROKEN", nil, rgb, "RGB", 3, []string{pixeldata.DiagnosticInvalidPhotometricReplaced, pixeldata.DiagnosticDefaultPlanarConfiguration}, rc},
		{"RGB missing SPP", nil, "RGB", &zero, rgb, "RGB", 3, []string{pixeldata.DiagnosticSamplesFromPhotometric}, rc},
		{"RGB incorrect SPP", &one, "RGB", &zero, rgb, "RGB", 3, []string{pixeldata.DiagnosticSamplesFromPhotometric}, rc},
		{"RGB zero SPP", &zero, "RGB", &zero, rgb, "RGB", 3, []string{pixeldata.DiagnosticSamplesFromPhotometric}, rc},
		{"YBR derives SPP without replacing known PI", &one, "YBR_FULL", &zero, []byte{128, 128, 128, 255, 128, 128}, "YBR_FULL", 3, []string{pixeldata.DiagnosticSamplesFromPhotometric}, []color.RGBA{{128, 128, 128, 255}, {255, 255, 255, 255}}},
		{"missing ARGB", &four, "", &zero, argb, "ARGB", 4, []string{pixeldata.DiagnosticPhotometricFromSamples}, rc},
		{"invalid ARGB ignores PC1", &four, "BROKEN", &one, argb, "ARGB", 4, []string{pixeldata.DiagnosticInvalidPhotometricReplaced, pixeldata.DiagnosticIgnoredPlanarConfiguration}, rc},
		{"monochrome ignores PC1", &one, "MONOCHROME2", &one, mono, "MONOCHROME2", 1, []string{pixeldata.DiagnosticIgnoredPlanarConfiguration}, mc},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := recoveryImage(tc.spp, tc.pi, tc.pc, tc.raw)
			before := src.Elements()
			report := &pixeldata.DecompressReport{}
			opts := pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Report: report}
			out, syn, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(report.Diagnostics, tc.diags) {
				t.Fatalf("diagnostics %v, want %v", report.Diagnostics, tc.diags)
			}
			m, err := pixeldata.ExtractMetadata(out)
			if err != nil || m.PhotometricInterpretation != tc.wantPI || m.SamplesPerPixel != tc.wantSPP {
				t.Fatalf("metadata %+v: %v", m, err)
			}
			raw, _ := out.GetRaw(core.TagPixelData)
			if !bytes.Equal(raw, tc.raw) {
				t.Fatalf("native samples changed %v", raw)
			}
			recoveryAssertColors(t, out, syn, tc.colors)
			if !reflect.DeepEqual(src.Elements(), before) {
				t.Fatal("input mutated")
			}
			if _, _, err = pixeldata.DecompressDataSet(out, syn, opts); err != nil || len(report.Diagnostics) != 0 {
				t.Fatalf("repeat %v %v", err, report.Diagnostics)
			}
		})
	}
}
func TestPhotometricRecoveryDeclaredARGBPlanarPreserved(t *testing.T) {
	one := uint16(1)
	src := recoveryImage(&one, "ARGB", &one, []byte{0, 0, 255, 0, 0, 255, 0, 0})
	report := &pixeldata.DecompressReport{}
	out, syn, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Diagnostics, []string{pixeldata.DiagnosticSamplesFromPhotometric}) {
		t.Fatal(report.Diagnostics)
	}
	m, _ := pixeldata.ExtractMetadata(out)
	if m.PlanarConfiguration != 1 {
		t.Fatal("valid ARGB planar discarded")
	}
	recoveryAssertColors(t, out, syn, []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}})
}
func TestPhotometricRecoveryRefusesAmbiguityAndMalformedLayout(t *testing.T) {
	one, two, three, zero := uint16(1), uint16(2), uint16(3), uint16(0)
	for _, tc := range []struct {
		name string
		src  *object.Object
		syn  transfer.Syntax
	}{
		{"both absent", recoveryImage(nil, "", nil, []byte{1, 2}), transfer.ExplicitVRLittleEndian},
		{"unsupported SPP", recoveryImage(&two, "", nil, []byte{1, 2, 3, 4}), transfer.ExplicitVRLittleEndian},
		{"RGB wrong length", recoveryImage(&one, "RGB", &zero, []byte{1, 2}), transfer.ExplicitVRLittleEndian},
		{"extra bytes", recoveryImage(&three, "", &zero, make([]byte, 8)), transfer.ExplicitVRLittleEndian},
		{"invalid planar", recoveryImage(&three, "", &two, make([]byte, 6)), transfer.ExplicitVRLittleEndian},
		{"compressed", recoveryImage(&three, "", &zero, make([]byte, 6)), transfer.JPEGBaseline},
		{"native compressed color", recoveryImage(&one, "YBR_RCT", &zero, make([]byte, 6)), transfer.ExplicitVRLittleEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := tc.src.Elements()
			report := &pixeldata.DecompressReport{}
			out, _, err := pixeldata.DecompressDataSet(tc.src, tc.syn, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Report: report})
			if err == nil || out != nil {
				t.Fatalf("accepted ambiguous input %v", err)
			}
			if !reflect.DeepEqual(tc.src.Elements(), before) || len(report.Diagnostics) != 0 {
				t.Fatal("failed repair mutated input/report")
			}
		})
	}
	src := recoveryImage(&three, "", &zero, make([]byte, 6))
	src.Put(core.NewRawElement(core.NewTag(0x0028, 0x0002), core.VRUS, []byte{3, 0, 4, 0}))
	if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}}); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
		t.Fatalf("malformed US %v", err)
	}
}
func TestPhotometricRecoveryStrictAndLegacyControls(t *testing.T) {
	if pixeldata.DetectableDecodeRecovery().RepairPhotometricInterpretation {
		t.Fatal("producer-selected color assumption added to automatic detectable profile")
	}
	three, zero := uint16(3), uint16(0)
	src := recoveryImage(&three, "", &zero, []byte{255, 0, 0, 0, 255, 0})
	for _, r := range []pixeldata.DecodeRecovery{{}, {DefaultImagePixelModule: true}} {
		if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: r}); !errors.Is(err, pixeldata.ErrMissingMetadata) {
			t.Fatalf("existing profile silently inferred RGB %v", err)
		}
	}
	valid := recoveryImage(&three, "RGB", &zero, []byte{255, 0, 0, 0, 255, 0})
	report := &pixeldata.DecompressReport{}
	a, _, err := pixeldata.DecompressDataSet(valid, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := pixeldata.DecompressDataSet(valid, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Elements(), b.Elements()) || len(report.Diagnostics) != 0 {
		t.Fatal("valid RGB changed")
	}
}
func TestPhotometricRecoveryOddZeroPaddingAndBigEndian(t *testing.T) {
	three, zero := uint16(3), uint16(0)
	src := recoveryImage(&three, "", &zero, []byte{255, 0, 0, 0})
	src.Put(recoveryUS(0x00280011, 1))
	opts := pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}}
	if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, opts); err != nil {
		t.Fatal(err)
	}
	src.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{255, 0, 0, 9}))
	if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, opts); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
		t.Fatalf("nonzero pad accepted %v", err)
	}
	src = recoveryImage(&three, "", &zero, []byte{2, 0, 0, 0, 0, 0, 0, 0, 4, 0, 0, 0})
	for _, v := range []struct {
		tag uint32
		v   uint16
	}{{0x00280100, 16}, {0x00280101, 16}, {0x00280102, 15}} {
		src.Put(recoveryUS(v.tag, v.v))
	}
	src.SetValueByteOrder(binary.BigEndian)
	out, syn, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRBigEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	recoveryAssertColors(t, out, syn, []color.RGBA{{2, 0, 0, 255}, {0, 4, 0, 255}})
}

func TestPhotometricRecoveryMetadataBoundsAndDecompressLimits(t *testing.T) {
	three, zero := uint16(3), uint16(0)
	for _, bad := range []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRLO}, Value: core.StringValue{"BROKEN"}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"RGB", "MONOCHROME2"}},
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, bytes.Repeat([]byte{'X'}, 19)),
		core.NewRawElement(core.NewTag(0x0028, 0x0004), core.VRCS, []byte("RGB\\MONOCHROME2")),
	} {
		src := recoveryImage(&three, "", &zero, make([]byte, 6))
		src.Put(bad)
		if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}}); !errors.Is(err, pixeldata.ErrInvalidMetadata) {
			t.Fatalf("malformed photometric accepted %v", err)
		}
	}
	for _, limits := range []pixeldata.DecompressLimits{{MaxInputBytes: 5}, {MaxNativeBytes: 5}, {MaxPixels: 1}, {MaxFrames: 1}} {
		src := recoveryImage(&three, "", &zero, make([]byte, 6))
		if limits.MaxFrames > 0 {
			src.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0008), VR: core.VRIS}, Value: core.StringValue{"2"}})
			src.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 12)))
		}
		if _, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Limits: limits}); !errors.Is(err, pixeldata.ErrDecompressResourceLimit) {
			t.Fatalf("limit %+v: %v", limits, err)
		}
	}
}

func TestPhotometricRecoveryPaletteSampleAndPlanarMetadata(t *testing.T) {
	three, one := uint16(3), uint16(1)
	src := recoveryImage(&three, "PALETTE COLOR", &one, []byte{0, 1})
	report := &pixeldata.DecompressReport{}
	out, _, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(report.Diagnostics, []string{pixeldata.DiagnosticSamplesFromPhotometric, pixeldata.DiagnosticIgnoredPlanarConfiguration}) {
		t.Fatal(report.Diagnostics)
	}
	m, err := pixeldata.ExtractMetadata(out)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := out.GetRaw(core.TagPixelData)
	palette := &display.PaletteColorLUT{Red: &display.LUT{BitsPerEntry: 8, Entries: []uint16{255, 0}}, Green: &display.LUT{BitsPerEntry: 8, Entries: []uint16{0, 255}}, Blue: &display.LUT{BitsPerEntry: 8, Entries: []uint16{0, 0}}}
	img, err := display.RenderColor(display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: raw, Photometric: m.PhotometricInterpretation, SamplesPerPixel: int(m.SamplesPerPixel), PlanarConfiguration: int(m.PlanarConfiguration), Format: display.PixelFormat{BitsAllocated: 8, BitsStored: 8, HighBit: 7}, Palette: palette})
	if err != nil {
		t.Fatal(err)
	}
	if img.RGBAAt(0, 0) != (color.RGBA{255, 0, 0, 255}) || img.RGBAAt(1, 0) != (color.RGBA{0, 255, 0, 255}) {
		t.Fatal(img)
	}
}

func TestPhotometricRecoveryEmptyTypedPhotometric(t *testing.T) {
	three, zero := uint16(3), uint16(0)
	src := recoveryImage(&three, "", &zero, []byte{255, 0, 0, 0, 255, 0})
	src.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{}})
	out, syn, err := pixeldata.DecompressDataSet(src, transfer.ExplicitVRLittleEndian, pixeldata.DecompressOptions{Recovery: pixeldata.DecodeRecovery{RepairPhotometricInterpretation: true}})
	if err != nil {
		t.Fatal(err)
	}
	recoveryAssertColors(t, out, syn, []color.RGBA{{255, 0, 0, 255}, {0, 255, 0, 255}})
}
