package parametricmap

import (
	"errors"
	"math"
	"slices"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestReadMultipleMappingsSelectsPerFrameByStoredDomain(t *testing.T) {
	for _, location := range []string{"dataset", "shared", "per-frame"} {
		t.Run(location, func(t *testing.T) {
			file := testMapFile(t, PayloadFloat32, []float64{1, 25, 15, 30, 1, 25, 15, 30}, 2, false, core.Tag{})
			ranges := derivedio.Seq(tagRealWorldValueMappingSeq,
				testBoundedMapping("high", "mm", "millimetres", "method-A", 20, 30, 1, 1000),
				testBoundedMapping("low", "mm", "millimeters", "method-A", 0, 10, 1, 0),
			)
			setTestMappingLocation(file.Dataset, location, ranges, ranges)
			wire := rereadPart10(t, file)
			defer wire.Close()
			m, err := Read(wire)
			if err != nil {
				t.Fatal(err)
			}
			for frame := 0; frame < m.NumberOfFrames; frame++ {
				values := make([]float64, 4)
				padding := make([]bool, 4)
				counts, err := m.DecodeFrameInto(frame, values, padding)
				if err != nil || counts != (VoxelCounts{Valid: 3, OutsideMapping: 1}) || counts.Unmeasured() != 1 ||
					!slices.Equal(values[:2], []float64{1, 1025}) || !math.IsNaN(values[2]) || values[3] != 1030 ||
					!slices.Equal(padding, []bool{false, false, false, false}) {
					t.Fatalf("frame %d: %v counts %+v padding %v, err %v", frame, values, counts, padding, err)
				}
				domain, err := m.WindowDomain(frame)
				if err != nil || !domain.Known || domain.Min != 1 || domain.Max != 1030 || domain.Unit != "millimeters" || m.CachedBytes() != 0 {
					t.Fatalf("frame %d: domain %+v cache %d err %v", frame, domain, m.CachedBytes(), err)
				}
			}
			mask := roi.NewRasterMask(2, 2)
			for y := 0; y < 2; y++ {
				for x := 0; x < 2; x++ {
					mask.Set(x, y, true)
				}
			}
			stats, err := m.MaskStatistics(0, mask)
			if err != nil || stats.Count != 3 || stats.Unmeasured != 1 || stats.Min != 1 || stats.Max != 1030 {
				t.Fatalf("statistics %+v err %v", stats, err)
			}
			meta, err := ReadMetadata(wire)
			if err != nil || !meta.Frames[0].Mapping.HasRange || len(meta.Frames[0].AdditionalMappings) != 1 {
				t.Fatalf("metadata = %+v, %v", meta, err)
			}
			if _, err := meta.FrameValues(0); !errors.Is(err, ErrPayloadUnavailable) {
				t.Fatalf("metadata unexpectedly retained pixels: %v", err)
			}
		})
	}
}

func TestReadMultipleMappingsRejectsAmbiguityUnlessLabelSelected(t *testing.T) {
	for _, test := range []struct {
		name    string
		right   core.DataSet
		outcome error
	}{
		{"overlap not sampled", testBoundedMapping("right", "mm", "length", "method-A", 4.5, 9, 1, 1000), ErrAmbiguousMapping},
		{"ranges share an endpoint", testBoundedMapping("right", "mm", "length", "method-A", 5, 12, 1, 1000), ErrAmbiguousMapping},
		{"different unit", testBoundedMapping("right", "cm", "length", "method-A", 7, 12, 1, 1000), ErrAmbiguousMapping},
		{"different method", testBoundedMapping("right", "mm", "length", "method-B", 7, 12, 1, 1000), ErrAmbiguousMapping},
		{"same label", testBoundedMapping("left", "mm", "length", "method-A", 0, 11, 1, 1000), ErrAmbiguousMapping},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := testMapFile(t, PayloadFloat64, []float64{1, 2, 3, 4, 1, 2, 3, 4}, 2, false, core.Tag{})
			left := testBoundedMapping("left", "mm", "length", "method-A", 0, 5, 2, 1)
			leftNext := testBoundedMapping("left", "mm", "longitud", "method-A", 0, 5, 3, 1)
			first := derivedio.Seq(tagRealWorldValueMappingSeq, left, test.right)
			last := derivedio.Seq(tagRealWorldValueMappingSeq, test.right, leftNext)
			setTestMappingLocation(file.Dataset, "per-frame", first, last)
			wire := rereadPart10(t, file)
			defer wire.Close()
			if m, err := Read(wire); m != nil || !errors.Is(err, test.outcome) {
				t.Fatalf("implicit selection = %v, %v", m != nil, err)
			}
			opts := ReadOptions{MappingLabel: "left"}
			m, err := ReadWithOptions(wire, opts)
			if test.name == "same label" {
				if m != nil || !errors.Is(err, ErrAmbiguousMapping) {
					t.Fatalf("duplicate label selection = %v, %v", m != nil, err)
				}
				return
			}
			if err != nil || m.Units.Value != "mm" || m.Frames[1].Mapping.Units.Meaning != "length" {
				t.Fatalf("selected map = %v, %v", m, err)
			}
			for i, want := range [][]float64{{3, 5, 7, 9}, {4, 7, 10, 13}} {
				got, err := m.FrameValues(i)
				if err != nil || !slices.Equal(got, want) {
					t.Fatalf("selected frame %d got %v, want %v, err %v", i, got, want, err)
				}
			}
			if meta, err := ReadMetadataWithOptions(wire, opts); err != nil || meta.Frames[1].Mapping.Label != "left" {
				t.Fatalf("metadata selection = %v, %v", meta, err)
			}
			if selected, err := ReadDatasetWithOptions(wire.Dataset, ReadOptions{MappingLabel: "LEFT"}); selected != nil || !errors.Is(err, ErrMissingMapping) {
				t.Fatalf("unknown label selection = %v, %v", selected, err)
			}
		})
	}
}

func TestReadMultipleMappingsUsesSignedLUTAndPaddingInImplicitVR(t *testing.T) {
	file := integerPaddingMap(t, true, 16, []int64{-2, -1, 0, 3})
	file.Dataset.Put(integerPaddingElement(tagIntegerPixelPaddingValue, true, -2))
	lut := core.DataSet{Elements: []core.Element{
		integerPaddingElement(tagRWVFirstMapped, true, -2),
		integerPaddingElement(tagRWVLastMapped, true, -1),
		derivedio.FD(tagRWVLUTData, 19, 42),
		testMappingUnits("mm", "millimeters"),
	}}
	linear := core.DataSet{Elements: []core.Element{
		integerPaddingElement(tagRWVFirstMapped, true, 3),
		integerPaddingElement(tagRWVLastMapped, true, 5),
		derivedio.FD(tagRWVIntercept, 100),
		derivedio.FD(tagRWVSlope, 1),
		testMappingUnits("mm", "millimeters"),
	}}
	groups := sequence(file.Dataset, tagSharedFunctionalGroups)
	groups[0].Put(derivedio.Seq(tagRealWorldValueMappingSeq, linear, lut))
	file.Dataset.Put(derivedio.Seq(tagSharedFunctionalGroups, groups[0].ToDataSet()))
	file.TransferSyntax = transfer.ImplicitVRLittleEndian
	file.Meta.Put(derivedio.UI(derivedio.TagTransferSyntaxUID, transfer.ImplicitVRLittleEndian.UID))
	wire := rereadPart10(t, file)
	defer wire.Close()
	m, err := Read(wire)
	if err != nil {
		t.Fatal(err)
	}
	values := make([]float64, 4)
	padding := make([]bool, 4)
	counts, err := m.DecodeFrameInto(0, values, padding)
	if err != nil || !math.IsNaN(values[0]) || values[1] != 42 || !math.IsNaN(values[2]) || values[3] != 103 ||
		!slices.Equal(padding, []bool{true, false, false, false}) ||
		counts != (VoxelCounts{Valid: 2, Padding: 1, OutsideMapping: 1}) {
		t.Fatalf("values %v, padding %v, counts %+v, err %v", values, padding, counts, err)
	}
}

func TestReadMultipleMappingsRefusesMalformedFrameCalibration(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*object.Object)
		want   error
	}{
		{"one bound", func(obj *object.Object) { obj.Remove(tagDoubleRWVLastMapped) }, ErrMissingMapping},
		{"reversed bounds", func(obj *object.Object) { obj.Put(derivedio.FD(tagDoubleRWVLastMapped, -1)) }, ErrMissingMapping},
		{"nonfinite bound", func(obj *object.Object) { obj.Put(derivedio.FD(tagDoubleRWVLastMapped, math.Inf(1))) }, ErrMissingMapping},
		{"several first values", func(obj *object.Object) { obj.Put(derivedio.FD(tagDoubleRWVFirstMapped, 0, 1)) }, ErrMissingMapping},
		{"missing coefficients", func(obj *object.Object) { obj.Remove(tagRWVSlope) }, ErrMissingMapping},
		{"unbounded item", func(obj *object.Object) {
			obj.Remove(tagDoubleRWVFirstMapped)
			obj.Remove(tagDoubleRWVLastMapped)
		}, ErrAmbiguousMapping},
	} {
		t.Run(test.name, func(t *testing.T) {
			file := testMapFile(t, PayloadFloat32, []float64{1, 2, 1, 2}, 1, false, core.Tag{})
			item := object.FromDataSet(testBoundedMapping("a", "mm", "length", "", 0, 4, 1, 0), nil)
			test.mutate(item)
			ok := testBoundedMapping("b", "mm", "length", "", 5, 9, 1, 0)
			file.Dataset.Put(derivedio.Seq(tagRealWorldValueMappingSeq, item.ToDataSet(), ok))
			if result, err := ReadMetadata(file); result != nil || !errors.Is(err, test.want) {
				t.Fatalf("ReadMetadata = %v, %v, want %v", result != nil, err, test.want)
			}
		})
	}
}

func testBoundedMapping(label, unit, meaning, method string, first, last, slope, intercept float64) core.DataSet {
	elements := []core.Element{
		derivedio.SH(tagLUTLabel, label),
		derivedio.FD(tagDoubleRWVFirstMapped, first),
		derivedio.FD(tagDoubleRWVLastMapped, last),
		derivedio.FD(tagRWVSlope, slope),
		derivedio.FD(tagRWVIntercept, intercept),
		testMappingUnits(unit, meaning),
	}
	if method != "" {
		elements = append(elements, derivedio.Seq(tagQuantityDefinitionSeq,
			derivedio.DataSet(derivedio.Seq(tagConceptCodeSeq, codeItem(Code{Value: "total", Scheme: "99LOCAL", Meaning: "Total"}))),
			derivedio.DataSet(derivedio.Seq(tagConceptCodeSeq, codeItem(Code{Value: method, Scheme: "99LOCAL", Meaning: "Measurement method"}))),
		))
	}
	return derivedio.DataSet(elements...)
}

func testMappingUnits(unit, meaning string) core.Element {
	return derivedio.Seq(tagMeasurementUnitsCodeSeq, codeItem(Code{Value: unit, Scheme: "UCUM", Meaning: meaning}))
}

func setTestMappingLocation(obj *object.Object, location string, mappings ...core.Element) {
	switch location {
	case "dataset":
		obj.Put(mappings[0])
	case "shared":
		groups := sequence(obj, tagSharedFunctionalGroups)
		groups[0].Put(mappings[0])
		obj.Put(derivedio.Seq(tagSharedFunctionalGroups, groups[0].ToDataSet()))
	case "per-frame":
		groups := sequence(obj, tagPerFrameFunctionalGroups)
		datasets := make([]core.DataSet, len(groups))
		for i, group := range groups {
			group.Put(mappings[i])
			datasets[i] = group.ToDataSet()
		}
		obj.Put(derivedio.Seq(tagPerFrameFunctionalGroups, datasets...))
	}
}
