package pixeldata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func acrNemaImage(bits uint16, pixel []byte, extra ...core.Element) *object.Object {
	elements := []core.Element{
		uint16RawElement(tagRows, 2),
		uint16RawElement(tagColumns, 2),
		uint16RawElement(tagBitsAllocated, bits),
		uint16RawElement(tagBitsStored, min(bits, 12)),
		uint16RawElement(tagHighBit, min(bits, 12)-1),
		uint16RawElement(tagPixelRepresentation, 0),
		core.NewRawElement(core.TagPixelData, core.VROW, pixel),
	}
	obj := object.FromElements(append(elements, extra...), nil)
	obj.SetValueByteOrder(binary.LittleEndian)
	return obj
}

func TestDefaultImagePixelModule(t *testing.T) {
	pixel := []byte{1, 0, 2, 0, 3, 0, 4, 0}
	if _, _, err := DecompressDataSet(acrNemaImage(16, pixel), transfer.ImplicitVRLittleEndian, DecompressOptions{}); !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("strict error = %v", err)
	}
	report := &DecompressReport{}
	out, _, err := DecompressDataSet(acrNemaImage(16, pixel), transfer.ImplicitVRLittleEndian,
		DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{DiagnosticDefaultSamplesPerPixel, DiagnosticDefaultPhotometricInterpretation}; !slices.Equal(report.Diagnostics, want) {
		t.Fatalf("diagnostics %v", report.Diagnostics)
	}
	// The output carries the defaults explicitly.
	if samples, ok := getUint16(out, tagSamplesPerPixel); !ok || samples != 1 {
		t.Fatalf("samples per pixel %d %v", samples, ok)
	}
	if photometric, _ := out.GetString(tagPhotometricInterpretation); normalizedPhotometric(photometric) != "MONOCHROME2" {
		t.Fatalf("photometric %q", photometric)
	}
	if raw, _ := out.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixel) {
		t.Fatalf("pixel data % X", raw)
	}
	// Pixel Data that could hold three samples per pixel is left to the strict error.
	rgbSized := bytes.Repeat([]byte{1, 0}, 12)
	if _, _, err := DecompressDataSet(acrNemaImage(16, rgbSized), transfer.ImplicitVRLittleEndian,
		DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}}); !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("three-sample sized pixel data: %v", err)
	}
	// A multi-sample Photometric Interpretation is not overridden.
	rgb := acrNemaImage(16, pixel, photometricInterpretationElement("RGB"))
	if _, _, err := DecompressDataSet(rgb, transfer.ImplicitVRLittleEndian,
		DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}}); !errors.Is(err, ErrMissingMetadata) {
		t.Fatalf("RGB without samples per pixel: %v", err)
	}
}

func TestNumberOfFramesFromPlanes(t *testing.T) {
	pixel := bytes.Repeat([]byte{7}, 2*2*5)
	volume := acrNemaImage(8, pixel, uint16RawElement(tagImageDimensions, 3), uint16RawElement(tagPlanes, 5))
	report := &DecompressReport{}
	out, _, err := DecompressDataSet(volume, transfer.ImplicitVRLittleEndian,
		DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	if n, _ := numberOfFrames(out); n != 5 || report.Diagnostics[0] != DiagnosticNumberOfFramesFromPlanes {
		t.Fatalf("frames %d diagnostics %v", n, report.Diagnostics)
	}
}

func TestUnpack12BitPacked(t *testing.T) {
	packed := []byte{0x12, 0x34, 0x56, 0xab, 0xcd, 0xef}
	image := acrNemaImage(12, packed, uint16RawElement(tagSamplesPerPixel, 1), photometricInterpretationElement("MONOCHROME2"))
	if _, _, err := DecompressDataSet(image, transfer.ImplicitVRLittleEndian, DecompressOptions{}); err == nil {
		t.Fatal("strict decode accepted packed 12-bit pixel data")
	}
	report := &DecompressReport{}
	out, _, err := DecompressDataSet(image, transfer.ImplicitVRLittleEndian,
		DecompressOptions{Recovery: DecodeRecovery{Unpack12BitPacked: true}, Report: report})
	if err != nil {
		t.Fatal(err)
	}
	want := binary.LittleEndian.AppendUint16(nil, 0x412)
	want = binary.LittleEndian.AppendUint16(want, 0x563)
	want = binary.LittleEndian.AppendUint16(want, 0xdab)
	want = binary.LittleEndian.AppendUint16(want, 0xefc)
	if raw, _ := out.GetRaw(core.TagPixelData); !bytes.Equal(raw, want) {
		t.Fatalf("pixel data % X, want % X", raw, want)
	}
	if bits, _ := getUint16(out, tagBitsAllocated); bits != 16 || !slices.Equal(report.Diagnostics, []string{DiagnosticUnpacked12Bit}) {
		t.Fatalf("bits allocated %d diagnostics %v", bits, report.Diagnostics)
	}
}

// bits12Image is a native MONOCHROME2 Bits Allocated 12, Bits Stored 12
// image of rows x columns in the given byte order.
func bits12Image(order binary.ByteOrder, rows, columns uint16, pixel []byte) *object.Object {
	obj := object.FromElements([]core.Element{
		photometricInterpretationElement("MONOCHROME2"),
		uint16Element(tagRows, rows, order), uint16Element(tagColumns, columns, order),
		uint16Element(tagSamplesPerPixel, 1, order),
		uint16Element(tagBitsAllocated, 12, order), uint16Element(tagBitsStored, 12, order),
		uint16Element(tagHighBit, 11, order), uint16Element(tagPixelRepresentation, 0, order),
		core.NewRawElement(core.TagPixelData, core.VROW, pixel),
	}, nil)
	obj.SetValueByteOrder(order)
	return obj
}

func TestBitsAllocated12ReadAs16(t *testing.T) {
	want := []int32{0x000, 0x123, 0xfff, 0x800, 0xa5a, 0x001}
	// ACR-NEMA packing of want, two samples per three bytes: the low byte of
	// the first sample, then its high nibble in the low nibble of the middle
	// byte, the low nibble of the second sample in its high nibble, then the
	// high byte of the second sample. Nine bytes are padded to an even ten.
	packed := []byte{0x00, 0x30, 0x12, 0xff, 0x0f, 0x80, 0x5a, 0x1a, 0x00, 0x00}
	words := func(order binary.ByteOrder) []byte {
		out := make([]byte, 2*len(want))
		for i, v := range want {
			// Bits above Bits Stored are not part of the sample.
			order.PutUint16(out[2*i:], uint16(v)|uint16(i%2)<<15)
		}
		return out
	}
	for _, tc := range []struct {
		name       string
		order      binary.ByteOrder
		syntax     transfer.Syntax
		pixel      []byte
		diagnostic string
	}{
		{"packed", binary.LittleEndian, transfer.ExplicitVRLittleEndian, packed, DiagnosticUnpacked12Bit},
		{"packed unpadded", binary.LittleEndian, transfer.ImplicitVRLittleEndian, packed[:9], DiagnosticUnpacked12Bit},
		{"words", binary.LittleEndian, transfer.ExplicitVRLittleEndian, words(binary.LittleEndian), DiagnosticBitsAllocated12As16},
		{"words big endian", binary.BigEndian, transfer.ExplicitVRBigEndian, words(binary.BigEndian), DiagnosticBitsAllocated12As16},
	} {
		t.Run(tc.name, func(t *testing.T) {
			image := bits12Image(tc.order, 2, 3, tc.pixel)
			// Without the recovery Bits Allocated 12 stays refused.
			if _, _, err := decodedSamples(t, image, tc.syntax, DecodeRecovery{}); err == nil {
				t.Fatal("strict decode read Bits Allocated 12")
			}
			got, report, err := decodedSamples(t, image, tc.syntax, DecodeRecovery{Unpack12BitPacked: true})
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(got, want) {
				t.Fatalf("samples %#x, want %#x", got, want)
			}
			if !slices.Equal(report.Diagnostics, []string{tc.diagnostic}) {
				t.Fatalf("diagnostics %v", report.Diagnostics)
			}
		})
	}

	// A Bits Allocated 12 value of any other length is refused, including
	// an odd sample count, which cannot be packed two to three bytes.
	for _, tc := range []struct {
		rows, size int
	}{{2, 8}, {2, 11}, {2, 13}, {2, 14}, {2, 18}, {1, 4}, {1, 5}} {
		image := bits12Image(binary.LittleEndian, uint16(tc.rows), 3, make([]byte, tc.size))
		_, _, err := DecompressDataSet(image, transfer.ExplicitVRLittleEndian, DecompressOptions{Recovery: DecodeRecovery{Unpack12BitPacked: true}})
		if !errors.Is(err, ErrPixelDataSizeMismatch) || !strings.Contains(err.Error(), "Bits Allocated 12") {
			t.Errorf("%dx3 samples in %d bytes: %v", tc.rows, tc.size, err)
		}
	}
	// An odd sample count held as 16-bit words is read.
	image := bits12Image(binary.LittleEndian, 1, 3, words(binary.LittleEndian)[:6])
	if got, _, err := decodedSamples(t, image, transfer.ExplicitVRLittleEndian, DecodeRecovery{Unpack12BitPacked: true}); err != nil || !slices.Equal(got, want[:3]) {
		t.Fatalf("odd count words: %#x %v", got, err)
	}
}

func TestLIBIDODimensionsRecoveryIsOptInAndIdempotent(t *testing.T) {
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		for _, code := range []string{"ACRNEMA_LIBIDO_1.0", "ACRNEMA_LIBIDO_1.1"} {
			t.Run(order.String()+"/"+code, func(t *testing.T) {
				pixel := []byte{0, 31, 63, 127, 191, 255}
				source := libidoImage(order, code, 8, pixel)
				source.Put(uint16Element(tagSamplesPerPixel, 1, order))
				source.Put(photometricInterpretationElement("MONOCHROME2"))
				syntax := transfer.ImplicitVRLittleEndian
				if order == binary.BigEndian {
					syntax = transfer.ExplicitVRBigEndian
				}
				strict, _, err := DecompressDataSet(source, syntax, DecompressOptions{})
				if err != nil {
					t.Fatal(err)
				}
				assertLIBIDODimensions(t, strict, 3, 2)
				report := &DecompressReport{}
				opts := DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report}
				out, target, err := DecompressDataSet(source, syntax, opts)
				if err != nil {
					t.Fatal(err)
				}
				assertLIBIDODimensions(t, out, 2, 3)
				assertLIBIDODimensions(t, source, 3, 2)
				if !source.Has(tagRecognitionCode) || out.Has(tagRecognitionCode) {
					t.Fatal("recognition marker was not consumed only in the recovered copy")
				}
				if !slices.Equal(report.Diagnostics, []string{DiagnosticLIBIDODimensions}) {
					t.Fatalf("diagnostics %v", report.Diagnostics)
				}
				if raw, _ := out.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixel) {
					t.Fatalf("pixels changed: %x", raw)
				}
				again, _, err := DecompressDataSet(out, target, opts)
				if err != nil {
					t.Fatal(err)
				}
				assertLIBIDODimensions(t, again, 2, 3)
				if len(report.Diagnostics) != 0 {
					t.Fatalf("repeat recovery diagnostics %v", report.Diagnostics)
				}
				upgraded, upgradeReport, err := object.UpgradeLegacy(&object.File{Dataset: out, TransferSyntax: target})
				if err != nil || len(upgradeReport.Corrected) != 0 {
					t.Fatalf("upgrade of recovered copy: %v %+v", err, upgradeReport)
				}
				assertLIBIDODimensions(t, upgraded.Dataset, 2, 3)
			})
		}
	}
}

func TestLIBIDORecoveryKeepsOtherDeclarations(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*object.Object)
	}{
		{"other recognition", func(o *object.Object) {
			o.Put(core.NewRawElement(tagRecognitionCode, core.VRLO, []byte("ACR-NEMA 2.0")))
		}},
		{"unrecognized version", func(o *object.Object) {
			o.Put(core.NewRawElement(tagRecognitionCode, core.VRLO, []byte("ACRNEMA_LIBIDO_1.2")))
		}},
		{"declared SOP Class", func(o *object.Object) {
			o.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte("1.2.840.10008.5.1.4.1.1.7")))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := libidoImage(binary.LittleEndian, "ACRNEMA_LIBIDO_1.1", 8, []byte{0, 1, 2, 3, 4, 5})
			tc.edit(source)
			report := &DecompressReport{}
			out, _, err := DecompressDataSet(source, transfer.ImplicitVRLittleEndian, DecompressOptions{
				Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report,
			})
			if err != nil {
				t.Fatal(err)
			}
			assertLIBIDODimensions(t, out, 3, 2)
			if slices.Contains(report.Diagnostics, DiagnosticLIBIDODimensions) || slices.Contains(report.Diagnostics, DiagnosticLIBIDORGB24) {
				t.Fatalf("unexpected LIBIDO repair: %v", report.Diagnostics)
			}
		})
	}
}

func TestLIBIDORGB24Recovery(t *testing.T) {
	pixel := []byte{255, 0, 0, 0, 255, 0, 0, 0, 255, 255, 255, 0, 0, 255, 255, 255, 0, 255}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		t.Run(order.String(), func(t *testing.T) {
			source := libidoImage(order, "ACRNEMA_LIBIDO_1.1", 24, pixel)
			syntax := transfer.ImplicitVRLittleEndian
			if order == binary.BigEndian {
				syntax = transfer.ExplicitVRBigEndian
			}
			report := &DecompressReport{}
			out, target, err := DecompressDataSet(source, syntax, DecompressOptions{
				Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report,
			})
			if err != nil {
				t.Fatal(err)
			}
			metadata, err := ExtractMetadata(out)
			if err != nil || metadata.Rows != 2 || metadata.Columns != 3 || metadata.SamplesPerPixel != 3 || metadata.BitsAllocated != 8 || metadata.BitsStored != 8 || metadata.HighBit != 7 || metadata.PlanarConfiguration != 0 || !metadata.PlanarConfigurationPresent || metadata.PhotometricInterpretation != "RGB" {
				t.Fatalf("RGB metadata %+v: %v", metadata, err)
			}
			if !slices.Equal(report.Diagnostics, []string{DiagnosticLIBIDODimensions, DiagnosticLIBIDORGB24}) {
				t.Fatalf("diagnostics %v", report.Diagnostics)
			}
			if raw, _ := out.GetRaw(core.TagPixelData); !bytes.Equal(raw, pixel) {
				t.Fatalf("RGB pixel order changed: %x", raw)
			}
			// Both entry points must yield identical metadata and unchanged bytes.
			upgraded, upgradeReport, err := object.UpgradeLegacy(&object.File{Dataset: source, TransferSyntax: syntax})
			if err != nil || !slices.Equal(upgradeReport.Diagnostics, report.Diagnostics) {
				t.Fatalf("upgrade diagnostics %+v: %v", upgradeReport, err)
			}
			upgradedNative, _, err := DecompressDataSet(upgraded.Dataset, upgraded.TransferSyntax, DecompressOptions{})
			if err != nil {
				t.Fatal(err)
			}
			upgradeMetadata, _ := ExtractMetadata(upgradedNative)
			if upgradeMetadata != metadata {
				t.Fatalf("upgrade metadata %+v differs from recovered %+v", upgradeMetadata, metadata)
			}
			again, _, err := DecompressDataSet(out, target, DecompressOptions{Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report})
			if err != nil || len(report.Diagnostics) != 0 {
				t.Fatalf("repeat RGB recovery: %v %v", err, report.Diagnostics)
			}
			assertLIBIDODimensions(t, again, 2, 3)
		})
	}
}

func TestLIBIDORGB24RequiresExactLayout(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*object.Object)
	}{
		{"stored precision", func(o *object.Object) { o.Put(uint16RawElement(tagBitsStored, 16)) }},
		{"high bit", func(o *object.Object) { o.Put(uint16RawElement(tagHighBit, 15)) }},
		{"signed", func(o *object.Object) { o.Put(uint16RawElement(tagPixelRepresentation, 1)) }},
		{"three declared samples", func(o *object.Object) { o.Put(uint16RawElement(tagSamplesPerPixel, 3)) }},
		{"declared photometric", func(o *object.Object) { o.Put(photometricInterpretationElement("MONOCHROME2")) }},
		{"planar", func(o *object.Object) { o.Put(uint16RawElement(tagPlanarConfiguration, 1)) }},
		{"short pixels", func(o *object.Object) { o.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 16))) }},
		{"excess pixels", func(o *object.Object) { o.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 20))) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := libidoImage(binary.LittleEndian, "ACRNEMA_LIBIDO_1.1", 24, make([]byte, 18))
			tc.edit(source)
			out, diagnostics, err := withLegacyImageRecovery(source, transfer.ImplicitVRLittleEndian, DecodeRecovery{DefaultImagePixelModule: true})
			if err != nil {
				t.Fatal(err)
			}
			if slices.Contains(diagnostics, DiagnosticLIBIDORGB24) {
				t.Fatalf("unexpected RGB repair %v", diagnostics)
			}
			if bits, _ := getUint16(out, tagBitsAllocated); bits != 24 {
				t.Fatalf("Bits Allocated changed to %d", bits)
			}
			upgraded, report, err := object.UpgradeLegacy(&object.File{Dataset: source, TransferSyntax: transfer.ImplicitVRLittleEndian})
			if err != nil || slices.Contains(report.Diagnostics, DiagnosticLIBIDORGB24) {
				t.Fatalf("unexpected upgrade RGB repair: %+v %v", report, err)
			}
			if bits, _ := getUint16(upgraded.Dataset, tagBitsAllocated); bits != 24 {
				t.Fatalf("upgraded Bits Allocated changed to %d", bits)
			}
		})
	}
}

func TestLIBIDORGB24PlanesUpgradeMatchesRecovery(t *testing.T) {
	source := libidoImage(binary.LittleEndian, "ACRNEMA_LIBIDO_1.0", 24, make([]byte, 36))
	source.Put(uint16RawElement(tagImageDimensions, 3))
	source.Put(uint16RawElement(tagPlanes, 2))
	report := &DecompressReport{}
	recovered, _, err := DecompressDataSet(source, transfer.ImplicitVRLittleEndian, DecompressOptions{
		Recovery: DecodeRecovery{DefaultImagePixelModule: true}, Report: report,
	})
	if err != nil {
		t.Fatal(err)
	}
	upgraded, upgradeReport, err := object.UpgradeLegacy(&object.File{Dataset: source, TransferSyntax: transfer.ImplicitVRLittleEndian})
	if err != nil || !slices.Equal(upgradeReport.Diagnostics, report.Diagnostics) {
		t.Fatalf("upgrade/recovery diagnostics: %+v / %v: %v", upgradeReport, report.Diagnostics, err)
	}
	native, _, err := DecompressDataSet(upgraded.Dataset, upgraded.TransferSyntax, DecompressOptions{})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := ExtractMetadata(native)
	want, _ := ExtractMetadata(recovered)
	if got != want || got.NumberOfFrames != 2 || got.SamplesPerPixel != 3 || source.Has(tagNumberOfFrames) {
		t.Fatalf("upgrade/recovery metadata %+v / %+v", got, want)
	}
}

func libidoImage(order binary.ByteOrder, code string, bits uint16, pixel []byte) *object.Object {
	obj := object.FromElements([]core.Element{
		core.NewRawElement(tagRecognitionCode, core.VRLO, []byte(code+" ")),
		uint16Element(tagRows, 3, order), uint16Element(tagColumns, 2, order),
		uint16Element(tagBitsAllocated, bits, order), uint16Element(tagBitsStored, bits, order),
		uint16Element(tagHighBit, bits-1, order), uint16Element(tagPixelRepresentation, 0, order),
		core.NewRawElement(core.TagPixelData, core.VROB, pixel),
	}, nil)
	obj.SetValueByteOrder(order)
	return obj
}

func assertLIBIDODimensions(t *testing.T, obj *object.Object, rows, columns uint16) {
	t.Helper()
	r, okR := getUint16(obj, tagRows)
	c, okC := getUint16(obj, tagColumns)
	if !okR || !okC || r != rows || c != columns {
		t.Fatalf("dimensions rows=%d columns=%d, want rows=%d columns=%d", r, c, rows, columns)
	}
}
