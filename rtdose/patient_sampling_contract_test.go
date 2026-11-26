package rtdose

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/rtstruct"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// PS3.3 C.8.8.3.4.6 uses two's complement for ERROR. Keep the two bytes
// of each sample identical in the physical dose controls.
func TestReferenceRTDoseSignedErrorPreservesNegativeDose(t *testing.T) {
	for _, tc := range []struct {
		name        string
		doseType    string
		repr        uint16
		want        []float64
		displayUnit string
	}{
		{"unsigned physical", DoseTypePhysical, 0, []float64{8191.75, 0.25}, "Gy (physical)"},
		{"signed physical", DoseTypePhysical, 1, nil, ""},
		{"signed error", "ERROR ", 1, []float64{-0.25, 0.25}, "Gy (error)"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := testDoseFile(t, testDoseOptions{
				rows: 1, columns: 2, frames: 1, spacing: []float64{1, 1},
				position: []float64{0, 0, 0}, orientation: []float64{1, 0, 0, 0, 1, 0},
				scaling: 0.125, values: []uint16{0xFFFE, 0x0002},
				units: DoseUnitsGray, doseType: tc.doseType, summation: "PLAN",
			})
			file.Dataset.Put(derivedio.US(derivedio.TagPixelRepresentation, tc.repr))
			dose, err := Read(referenceRTDoseWire(t, file, transfer.ExplicitVRLittleEndian))
			if tc.want == nil {
				if dose != nil || !errors.Is(err, ErrInvalidObject) {
					t.Fatalf("Read accepted a signed physical dose: %+v, %v", dose, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read rejected %s dose: %v", tc.name, err)
			}
			if !slices.Equal(dose.Values, tc.want) || dose.DisplayUnit() != tc.displayUnit {
				t.Fatalf("Read returned %v %q; want %v %q", dose.Values, dose.DisplayUnit(), tc.want, tc.displayUnit)
			}
			wantMidpoint := (tc.want[0] + tc.want[1]) / 2
			if got, ok := dose.SamplePatient(testVec(0.5, 0, 0)); !ok || got != wantMidpoint {
				t.Fatalf("dose at midpoint = %g, %t; want %g", got, ok, wantMidpoint)
			}
		})
	}
}

// Use a descending, nonuniform stack with anisotropic in-plane spacing. The
// affine dose field has a distinct gradient on each patient-space axis.
func TestReferenceRTDosePatientSampling(t *testing.T) {
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
		bits   uint16
	}{
		{"implicit 16", transfer.ImplicitVRLittleEndian, 16},
		{"big-endian 16", transfer.ExplicitVRBigEndian, 16},
		{"explicit 32", transfer.ExplicitVRLittleEndian, 32},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The high bit is set, so a mistaken signed or narrowed decode is visible.
			base := uint32(1) << (tc.bits - 1)
			width := int(tc.bits / 8)
			pixels := make([]byte, 3*2*2*width)
			order := tc.syntax.ByteOrder
			offset := 0
			for _, z := range []uint32{6, 2, 0} {
				for y := uint32(0); y < 2; y++ {
					for x := uint32(0); x < 2; x++ {
						value := base + 64 + 8*x + 16*y + 32*z
						if tc.bits == 16 {
							order.PutUint16(pixels[offset:], uint16(value))
						} else {
							order.PutUint32(pixels[offset:], value)
						}
						offset += width
					}
				}
			}
			file := testDoseFile(t, testDoseOptions{
				rows: 2, columns: 2, frames: 3, spacing: []float64{4, 2},
				position: []float64{10, -10, 9}, orientation: []float64{1, 0, 0, 0, 1, 0},
				offsets: []float64{0, -4, -6}, scaling: 0.125,
				units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
			})
			file.Dataset.SetValueByteOrder(order)
			for _, item := range []struct {
				tag   core.Tag
				value uint16
			}{
				{derivedio.TagRows, 2}, {derivedio.TagColumns, 2}, {derivedio.TagSamplesPerPixel, 1},
				{derivedio.TagBitsAllocated, tc.bits}, {derivedio.TagBitsStored, tc.bits},
				{derivedio.TagHighBit, tc.bits - 1}, {derivedio.TagPixelRepresentation, 0},
			} {
				file.Dataset.Put(object.USElement(item.tag, order, item.value))
			}
			file.Dataset.Put(derivedio.Raw(derivedio.TagPixelData, core.VROW, pixels))
			wire := referenceRTDoseWire(t, file, tc.syntax)
			dose, err := Read(wire)
			if err != nil {
				t.Fatal(err)
			}
			wantDose := func(x, y, z float64) float64 {
				return float64(base)/8 + 8 + x + 2*y + 4*z
			}
			if dose.Maximum != wantDose(1, 1, 6) || dose.DisplayUnit() != "Gy (physical)" {
				t.Fatalf("maximum %g, units %q", dose.Maximum, dose.DisplayUnit())
			}
			for _, z := range []float64{-1.25, -1, -0.5, 0, 0.75, 2, 3.75, 6, 7.75, 8, 8.25} {
				for _, y := range []float64{-0.75, -0.5, 0, 0.25, 1, 1.25, 1.5} {
					for _, x := range []float64{-0.75, -0.5, 0, 0.75, 1, 1.25, 1.5} {
						point := testVec(10+2*x, -10+4*y, 3+z)
						got, ok := dose.SamplePatient(point)
						inside := x >= -0.5 && x < 1.5 && y >= -0.5 && y < 1.5 && z >= -1 && z < 8
						want := wantDose(max(0, min(1, x)), max(0, min(1, y)), max(0, min(6, z)))
						if ok != inside || inside && got != want {
							t.Fatalf("SamplePatient(%+v) = %g, %t; want %g, %t", point, got, ok, want, inside)
						}
					}
				}
			}
		})
	}
}

// A signed physical grid must not be interpreted as unsigned dose or silently
// replaced by an otherwise usable embedded DVH.
func TestReferenceRTDoseSeparatesCorruptGridAndEmbeddedDVH(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*object.Object)
	}{
		{"signed physical dose", func(obj *object.Object) { obj.Put(derivedio.US(derivedio.TagPixelRepresentation, 1)) }},
		{"missing declared grid", func(obj *object.Object) { obj.Remove(derivedio.TagPixelData) }},
		{"truncated declared grid", func(obj *object.Object) {
			obj.Put(derivedio.Raw(derivedio.TagPixelData, core.VROW, []byte{0xFF, 0xFF}))
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			file := testDoseFile(t, testDoseOptions{
				rows: 1, columns: 2, frames: 1, spacing: []float64{2, 4},
				position: []float64{10, -10, 9}, orientation: []float64{1, 0, 0, 0, 1, 0},
				scaling: 0.125, values: []uint16{0x8001, 0xFFFF},
				units: DoseUnitsGray, doseType: DoseTypePhysical, summation: "PLAN",
				extra: []core.Element{
					testDVHStructureSet(rtstruct.RTStructureSetStorage, "2.25.481.9"),
					derivedio.Seq(tagDVHSequence, testEmbeddedDVHItem(DoseUnitsGray, DoseTypePhysical, 1, []float64{12, 8})),
				},
			})
			tc.mutate(file.Dataset)
			wire := referenceRTDoseWire(t, file, transfer.ExplicitVRLittleEndian)
			if dose, err := Read(wire); dose != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("invalid dose grid published: %v, %v", dose, err)
			}
			report, err := ReadEmbeddedDVHs(wire.Dataset)
			if err != nil || len(report.Histograms) != 1 || len(report.Diagnostics) != 0 {
				t.Fatalf("separate histogram read = %+v, %v", report, err)
			}
			dvh := report.Histograms[0]
			if report.StructureSet.SOPInstanceUID != "2.25.481.9" || dvh.NumberOfBins != 1 || !slices.Equal(dvh.Data, []float64{12, 8}) {
				t.Fatalf("histogram lost identity or values: %+v", dvh)
			}
		})
	}
}

func referenceRTDoseWire(t *testing.T, file *object.File, syntax transfer.Syntax) *object.File {
	t.Helper()
	const uid = "2.25.481.1"
	file.Meta.Put(derivedio.UI(derivedio.TagMediaStorageSOPInstanceUID, uid))
	file.Meta.Put(derivedio.UI(derivedio.TagTransferSyntaxUID, syntax.UID))
	file.TransferSyntax = syntax
	file.Dataset.Put(derivedio.UI(derivedio.TagSOPInstanceUID, uid))
	file.Dataset.Put(derivedio.UI(derivedio.TagStudyInstanceUID, "2.25.481.2"))
	file.Dataset.Put(derivedio.UI(derivedio.TagSeriesInstanceUID, "2.25.481.3"))
	file.Dataset.Put(derivedio.UI(derivedio.TagFrameOfReferenceUID, "2.25.481.4"))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = parsed.Close() })
	return parsed
}
