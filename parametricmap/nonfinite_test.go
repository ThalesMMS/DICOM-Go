package parametricmap

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// paddingElements returns the padding pair of the pixel module matching
// payload, with value and limit deliberately in descending order.
func paddingElements(payload PayloadKind, value, limit float64) []core.Element {
	if payload == PayloadFloat64 {
		return []core.Element{
			derivedio.FD(tagDoubleFloatPixelPaddingValue, value),
			derivedio.FD(tagDoubleFloatPixelPaddingRangeLimit, limit),
		}
	}
	return []core.Element{
		derivedio.FL(tagFloatPixelPaddingValue, value),
		derivedio.FL(tagFloatPixelPaddingRangeLimit, limit),
	}
}

// rereadPart10 encodes file as Part 10 and parses it back. The fixture's short
// placeholder UIDs are replaced by syntactically valid synthetic ones first.
func rereadPart10(t *testing.T, file *object.File) *object.File {
	t.Helper()
	const root = "2.25.1118"
	file.Meta.Put(derivedio.UI(derivedio.TagMediaStorageSOPInstanceUID, root+".1"))
	file.Dataset.Put(derivedio.UI(derivedio.TagSOPInstanceUID, root+".1"))
	file.Dataset.Put(derivedio.UI(derivedio.TagStudyInstanceUID, root+".2"))
	file.Dataset.Put(derivedio.UI(derivedio.TagSeriesInstanceUID, root+".3"))
	file.Dataset.Put(derivedio.UI(derivedio.TagFrameOfReferenceUID, root+".4"))
	file.Dataset.Put(derivedio.Seq(core.NewTag(0x0008, 0x2112), derivedio.DataSet(
		derivedio.UI(tagReferencedSOPClassUID, "1.2.840.10008.5.1.4.1.1.2"),
		derivedio.UI(tagReferencedSOPInstanceUID, root+".5"),
	)))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	reread, err := object.ReadFile(&encoded)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	return reread
}

func TestNonFiniteAndPaddingVoxelsSurviveWriteReadAndAreMasked(t *testing.T) {
	for _, payload := range []PayloadKind{PayloadFloat32, PayloadFloat64} {
		t.Run(string(payload), func(t *testing.T) {
			// Frame 0: measured, NaN, +Inf, padding. Frame 1: measured
			// voxels around one padding voxel at the far end of the range.
			file := testMapFile(t, payload, []float64{
				1, math.NaN(), math.Inf(1), -1000,
				3, -2000, 5, 7,
			}, 2, true, core.Tag{})
			for _, element := range paddingElements(payload, -1000, -2000) {
				file.Dataset.Put(element)
			}
			m, err := Read(rereadPart10(t, file))
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if m.Padding == nil || m.Padding.Value != -1000 || m.Padding.RangeLimit != -2000 {
				t.Fatalf("Padding = %+v, want [-1000, -2000]", m.Padding)
			}

			values := make([]float64, 4)
			mask := make([]bool, 4)
			counts, err := m.DecodeFrameInto(0, values, mask)
			if err != nil {
				t.Fatalf("DecodeFrameInto(0) error = %v", err)
			}
			if counts != (VoxelCounts{Valid: 1, Padding: 1, NaN: 1, Infinite: 1}) || counts.Unmeasured() != 3 {
				t.Fatalf("frame 0 counts = %+v", counts)
			}
			if values[0] != 3 || !math.IsNaN(values[1]) || !math.IsInf(values[2], 1) || !math.IsNaN(values[3]) {
				t.Fatalf("frame 0 values = %v, want [3 NaN +Inf NaN]", values)
			}
			if mask[0] || mask[1] || mask[2] || !mask[3] {
				t.Fatalf("frame 0 padding mask = %v, want only voxel 3", mask)
			}
			counts, err = m.DecodeFrameInto(1, values, nil)
			if err != nil || counts != (VoxelCounts{Valid: 3, Padding: 1}) {
				t.Fatalf("frame 1 counts = %+v, err %v", counts, err)
			}
			if values[0] != 7 || !math.IsNaN(values[1]) || values[2] != 11 || values[3] != 15 {
				t.Fatalf("frame 1 values = %v, want [7 NaN 11 15]", values)
			}

			cached, err := m.FrameValues(0)
			if err != nil || cached[0] != 3 || !math.IsNaN(cached[1]) || !Measured(cached[0]) || Measured(cached[2]) {
				t.Fatalf("FrameValues(0) = %v, %v", cached, err)
			}

			full := roi.NewRasterMask(2, 2)
			full.SetRun(0, 0, 2)
			full.SetRun(1, 0, 2)
			stats, err := m.MaskStatistics(0, full)
			if err != nil || stats.Count != 1 || stats.Min != 3 || stats.Max != 3 || stats.Mean != 3 || stats.Unmeasured != 3 {
				t.Fatalf("MaskStatistics(0) = %+v, %v; want one measured voxel and three unmeasured", stats, err)
			}
			stats, err = m.MaskStatistics(1, full)
			if err != nil || stats.Count != 3 || stats.Min != 7 || stats.Max != 15 || stats.Unmeasured != 1 {
				t.Fatalf("MaskStatistics(1) = %+v, %v", stats, err)
			}

			target := m.Frames[0].Geometry
			resampled, valid, err := m.Resample(target)
			if err != nil {
				t.Fatalf("Resample() error = %v", err)
			}
			if !valid[0] || valid[1] || valid[2] || valid[3] || resampled[0] != 3 {
				t.Fatalf("Resample() = %v valid %v, want only the measured voxel", resampled, valid)
			}

			if value, ok, err := m.SamplePatient(render.Vec3{}); err != nil || !ok || value != 3 {
				t.Fatalf("SamplePatient(measured) = %v, %v, %v", value, ok, err)
			}
			// The outer half pixel repeats the edge pixel (#1115).
			if value, ok, err := m.SamplePatient(render.Vec3{X: -0.4}); err != nil || !ok || value != 3 {
				t.Fatalf("SamplePatient(outer half pixel) = %v, %v, %v", value, ok, err)
			}
			if _, ok, err := m.SamplePatient(render.Vec3{X: -0.6}); err != nil || ok {
				t.Fatalf("SamplePatient(beyond the half pixel) = ok %v err %v", ok, err)
			}
			if _, ok, err := m.SamplePatient(render.Vec3{X: 0.5}); err != nil || ok {
				t.Fatalf("SamplePatient(next to NaN) = ok %v err %v, want unsampled", ok, err)
			}
			if _, ok, err := m.SamplePatient(render.Vec3{X: 1, Y: 1}); err != nil || ok {
				t.Fatalf("SamplePatient(padding) = ok %v err %v, want unsampled", ok, err)
			}
			if value, ok, err := m.SamplePatient(render.Vec3{X: 0.5, Y: 1, Z: 1}); err != nil || !ok || value != 13 {
				t.Fatalf("SamplePatient(frame 1 measured row) = %v, %v, %v, want 13", value, ok, err)
			}
		})
	}
}

func TestNaNPaddingPairClassifiesStoredNaNAsPadding(t *testing.T) {
	file := testMapFile(t, PayloadFloat32, []float64{1, math.NaN(), 3, 4}, 1, true, core.Tag{})
	for _, element := range paddingElements(PayloadFloat32, math.NaN(), math.NaN()) {
		file.Dataset.Put(element)
	}
	m, err := Read(rereadPart10(t, file))
	if err != nil {
		t.Fatal(err)
	}
	mask := make([]bool, 4)
	counts, err := m.DecodeFrameInto(0, make([]float64, 4), mask)
	if err != nil || counts != (VoxelCounts{Valid: 3, Padding: 1}) || !mask[1] {
		t.Fatalf("NaN padding counts = %+v mask %v err %v", counts, mask, err)
	}
}

func TestReadRejectsMalformedFloatPadding(t *testing.T) {
	tests := []struct {
		name     string
		payload  PayloadKind
		elements []core.Element
		want     error
	}{
		{name: "value without Type 1C range limit", payload: PayloadFloat32,
			elements: []core.Element{derivedio.FL(tagFloatPixelPaddingValue, -1)}, want: ErrInvalidPadding},
		{name: "range limit without value", payload: PayloadFloat64,
			elements: []core.Element{derivedio.FD(tagDoubleFloatPixelPaddingRangeLimit, -1)}, want: ErrInvalidPadding},
		{name: "FD value in the FL module", payload: PayloadFloat32,
			elements: []core.Element{derivedio.FD(tagFloatPixelPaddingValue, -1), derivedio.FL(tagFloatPixelPaddingRangeLimit, -1)}, want: ErrInvalidPadding},
		{name: "two values", payload: PayloadFloat32,
			elements: []core.Element{derivedio.FL(tagFloatPixelPaddingValue, -1, -2), derivedio.FL(tagFloatPixelPaddingRangeLimit, -1)}, want: ErrInvalidPadding},
		{name: "NaN paired with a number", payload: PayloadFloat64,
			elements: paddingElements(PayloadFloat64, math.NaN(), -1), want: ErrInvalidPadding},
		{name: "double pair on float payload is outside its module", payload: PayloadFloat32,
			elements: paddingElements(PayloadFloat64, -1, -1)},
		{name: "float pair on integer payload is outside its module", payload: PayloadInteger,
			elements: paddingElements(PayloadFloat32, -1, -1)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			file := testMapFile(t, test.payload, []float64{1, 2, 3, 4}, 1, true, core.Tag{})
			for _, element := range test.elements {
				file.Dataset.Put(element)
			}
			m, err := Read(file)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("Read() error = %v, want %v", err, test.want)
				}
				return
			}
			if err != nil || m.Padding != nil {
				t.Fatalf("Read() = padding %+v err %v, want ignored pair", m.Padding, err)
			}
		})
	}
}

func TestMappingApplySeparatesMissingValuesFromInvalidMappings(t *testing.T) {
	linear := Mapping{Slope: -2, Intercept: 1}
	lut := Mapping{FirstValue: 0, LastValue: 2, LUT: []float64{10, math.NaN(), 30}}
	tests := []struct {
		name    string
		mapping Mapping
		stored  float64
		check   func(float64) bool
		want    error
	}{
		{name: "NaN stays NaN", mapping: linear, stored: math.NaN(), check: math.IsNaN},
		{name: "NaN through LUT stays NaN", mapping: lut, stored: math.NaN(), check: math.IsNaN},
		{name: "+Inf maps through negative slope", mapping: linear, stored: math.Inf(1), check: func(v float64) bool { return math.IsInf(v, -1) }},
		{name: "-Inf maps through negative slope", mapping: linear, stored: math.Inf(-1), check: func(v float64) bool { return math.IsInf(v, 1) }},
		{name: "finite value", mapping: linear, stored: 2, check: func(v float64) bool { return v == -3 }},
		{name: "LUT entry", mapping: lut, stored: 2.2, check: func(v float64) bool { return v == 30 }},
		{name: "Inf has no LUT entry", mapping: lut, stored: math.Inf(1), want: ErrMissingMapping},
		{name: "outside LUT", mapping: lut, stored: 2.6, want: ErrMissingMapping},
		{name: "half below the first entry (#1115)", mapping: lut, stored: -0.5, check: func(v float64) bool { return v == 10 }},
		{name: "just below the half", mapping: lut, stored: -0.51, want: ErrMissingMapping},
		{name: "non-finite LUT entry", mapping: lut, stored: 1, want: ErrNonFinite},
		{name: "overflowing linear mapping", mapping: Mapping{Slope: math.MaxFloat64}, stored: 4, want: ErrNonFinite},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := test.mapping.Apply(test.stored)
			if test.want != nil {
				if !errors.Is(err, test.want) {
					t.Fatalf("Apply(%v) error = %v, want %v", test.stored, err, test.want)
				}
				return
			}
			if err != nil || !test.check(got) {
				t.Fatalf("Apply(%v) = %v, %v", test.stored, got, err)
			}
		})
	}
}
