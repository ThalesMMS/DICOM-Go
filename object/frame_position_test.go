package object

import (
	"encoding/binary"
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

// Synthetic NM and legacy multi-frame geometry; no patient data.

func isValue(tag core.Tag, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRIS}, Value: core.StringValue{value}}
}

func detectorItem(ipp, iop []string) core.DataSet {
	var elements []core.Element
	if ipp != nil {
		elements = append(elements, dsMulti(tagImagePositionPatient, ipp...))
	}
	if iop != nil {
		elements = append(elements, dsMulti(tagImageOrientationPatient, iop...))
	}
	return core.DataSet{Elements: elements}
}

var (
	axialIOP   = []string{"1", "0", "0", "0", "1", "0"}
	coronalIOP = []string{"1", "0", "0", "0", "0", "-1"}
)

// spectElements is a RECON TOMO NM object of len(sliceVector) frames. A nil
// sliceVector omits Slice Vector; an empty spacing omits Spacing Between
// Slices.
func spectElements(sliceVector []uint16, spacing string, detectors ...core.DataSet) []core.Element {
	le := binary.LittleEndian
	frames := len(sliceVector)
	if frames == 0 {
		frames = 3
	}
	elements := []core.Element{
		{Header: core.ElementHeader{Tag: tagSOPClassUID, VR: core.VRUI}, Value: core.StringValue{"1.2.840.10008.5.1.4.1.1.20"}},
		csMulti(tagImageType, "ORIGINAL", "PRIMARY", "RECON TOMO", "EMISSION"),
		isValue(tagNumberOfFrames, itoa(frames)),
		ATElement(tagFrameIncrementPointer, le, tagSliceVector),
		USElement(tagNumberOfSlices, le, uint16(frames)),
		dsMulti(tagSliceThickness, "6"),
		dsMulti(tagPixelSpacing, "4", "4"),
	}
	if sliceVector != nil {
		elements = append(elements, USElement(tagSliceVector, le, sliceVector...))
	}
	if spacing != "" {
		elements = append(elements, dsMulti(tagSpacingBetweenSlices, spacing))
	}
	if len(detectors) > 0 {
		elements = append(elements, geometrySequence(tagDetectorInformationSequence, detectors...))
	}
	return elements
}

func itoa(value int) string {
	return string(rune('0' + value))
}

func framePositions(obj *Object, frames int, options FrameGeometryOptions) [][]float64 {
	out := make([][]float64, frames)
	for frame := range out {
		out[frame] = obj.FrameGeometryAtWithOptions(frame, options).ImagePositionPatient
	}
	return out
}

func positionsClose(got, want [][]float64) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if len(got[i]) != len(want[i]) {
			return false
		}
		for axis := range got[i] {
			if math.Abs(got[i][axis]-want[i][axis]) > 1e-9 {
				return false
			}
		}
	}
	return true
}

func TestFrameGeometryAtNMReconstruction(t *testing.T) {
	origin := []string{"-100", "-100", "50"}
	for _, tc := range []struct {
		name      string
		elements  []core.Element
		options   FrameGeometryOptions
		want      [][]float64
		findings  []FrameGeometryFinding
		recovered bool
	}{
		{
			name:     "positive spacing steps along the normal",
			elements: spectElements([]uint16{1, 2, 3}, "4", detectorItem(origin, axialIOP)),
			want:     [][]float64{{-100, -100, 50}, {-100, -100, 54}, {-100, -100, 58}},
		},
		{
			name:     "negative spacing steps against the normal",
			elements: spectElements([]uint16{1, 2, 3}, "-4", detectorItem(origin, axialIOP)),
			want:     [][]float64{{-100, -100, 50}, {-100, -100, 46}, {-100, -100, 42}},
			findings: []FrameGeometryFinding{FindingNegativeSpacingBetweenSlices},
		},
		{
			name:     "slice vector order, relative to the first frame",
			elements: spectElements([]uint16{3, 1, 2}, "4", detectorItem(origin, axialIOP)),
			want:     [][]float64{{-100, -100, 50}, {-100, -100, 42}, {-100, -100, 46}},
		},
		{
			name:     "coronal detector orientation",
			elements: spectElements([]uint16{1, 2, 3}, "2", detectorItem([]string{"0", "0", "0"}, coronalIOP)),
			want:     [][]float64{{0, 0, 0}, {0, 2, 0}, {0, 4, 0}},
		},
		{
			name:     "missing slice vector is reported",
			elements: spectElements(nil, "4", detectorItem(origin, axialIOP)),
			want:     [][]float64{nil, nil, nil},
			findings: []FrameGeometryFinding{FindingSliceVectorMissing},
		},
		{
			name:      "missing slice vector with the opt-in recovery uses frame order",
			elements:  spectElements(nil, "4", detectorItem(origin, axialIOP)),
			options:   FrameGeometryOptions{DeriveLegacyFramePositions: true},
			want:      [][]float64{{-100, -100, 50}, {-100, -100, 54}, {-100, -100, 58}},
			findings:  []FrameGeometryFinding{FindingFrameOrderAsSliceIndex},
			recovered: true,
		},
		{
			name:     "missing spacing between slices is reported, thickness is not used",
			elements: spectElements([]uint16{1, 2, 3}, "", detectorItem(origin, axialIOP)),
			want:     [][]float64{nil, nil, nil},
			findings: []FrameGeometryFinding{FindingSpacingBetweenSlicesMissing},
		},
		{
			name:      "slice thickness as spacing is opt-in",
			elements:  spectElements([]uint16{1, 2, 3}, "", detectorItem(origin, axialIOP)),
			options:   FrameGeometryOptions{SliceThicknessAsSpacing: true},
			want:      [][]float64{{-100, -100, 50}, {-100, -100, 56}, {-100, -100, 62}},
			findings:  []FrameGeometryFinding{FindingSliceThicknessAsSpacing},
			recovered: true,
		},
		{
			name:     "slice number outside Number of Slices",
			elements: spectElements([]uint16{1, 2, 9}, "4", detectorItem(origin, axialIOP)),
			want:     [][]float64{{-100, -100, 50}, {-100, -100, 54}, nil},
			findings: []FrameGeometryFinding{FindingSliceVectorOutOfRange},
		},
		{
			name:     "detector without geometry",
			elements: spectElements([]uint16{1, 2, 3}, "4", detectorItem(nil, nil)),
			want:     [][]float64{nil, nil, nil},
			findings: []FrameGeometryFinding{FindingNMDetectorGeometryMissing},
		},
		{
			name: "two detectors without a detector vector are ambiguous",
			elements: spectElements([]uint16{1, 2, 3}, "4",
				detectorItem(origin, axialIOP), detectorItem([]string{"0", "0", "0"}, coronalIOP)),
			want:     [][]float64{nil, nil, nil},
			findings: []FrameGeometryFinding{FindingNMDetectorAmbiguous},
		},
		{
			name: "detector vector selects the item",
			elements: append(spectElements([]uint16{1, 2, 3}, "2",
				detectorItem(origin, axialIOP), detectorItem([]string{"0", "0", "0"}, coronalIOP)),
				USElement(tagDetectorVector, binary.LittleEndian, 2, 2, 2)),
			want: [][]float64{{0, 0, 0}, {0, 2, 0}, {0, 4, 0}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := FromElements(tc.elements, std.Dictionary)
			if got := framePositions(obj, 3, tc.options); !positionsClose(got, tc.want) {
				t.Fatalf("positions = %v, want %v", got, tc.want)
			}
			last := obj.FrameGeometryAtWithOptions(2, tc.options)
			first := obj.FrameGeometryAtWithOptions(0, tc.options)
			findings := append(append([]FrameGeometryFinding(nil), first.Findings...), last.Findings...)
			for _, want := range tc.findings {
				if !containsFinding(findings, want) {
					t.Errorf("findings = %v, want %s", findings, want)
				}
			}
			if len(tc.findings) == 0 && len(findings) != 0 {
				t.Errorf("findings = %v, want none", findings)
			}
			if tc.want[0] == nil {
				return
			}
			if first.PositionSource != GeometrySourceNMSliceVector || first.PositionDerivation == nil {
				t.Fatalf("position source = %q derivation %+v, want nm-slice-vector", first.PositionSource, first.PositionDerivation)
			}
			if first.OrientationSource != GeometrySourceNMDetector || len(first.ImageOrientationPatient) != 6 {
				t.Fatalf("orientation %v from %q, want the detector item", first.ImageOrientationPatient, first.OrientationSource)
			}
			if first.PositionDerivation.Recovered != tc.recovered {
				t.Fatalf("Recovered = %v, want %v", first.PositionDerivation.Recovered, tc.recovered)
			}
		})
	}
}

func TestFrameGeometryAtNMDerivationReportsStep(t *testing.T) {
	obj := FromElements(spectElements([]uint16{1, 2, 3}, "-4.5", detectorItem([]string{"1", "2", "3"}, axialIOP)), std.Dictionary)
	got := obj.FrameGeometryAt(2).PositionDerivation
	want := &FramePositionDerivation{
		Origin: []float64{1, 2, 3}, Normal: []float64{0, 0, 1}, Offset: -9,
		SliceIndex: 2, Step: -4.5, StepSource: SliceSpacingFromSpacingBetweenSlices,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("PositionDerivation = %+v, want %+v", got, want)
	}
}

// TestFrameGeometryAtNMPlanarIsUntouched: only reconstructed slices are
// placed; a planar (STATIC) frame keeps no invented geometry.
func TestFrameGeometryAtNMPlanarIsUntouched(t *testing.T) {
	elements := []core.Element{
		csMulti(tagImageType, "ORIGINAL", "PRIMARY", "STATIC", "EMISSION"),
		isValue(tagNumberOfFrames, "2"),
		ATElement(tagFrameIncrementPointer, binary.LittleEndian, tagDetectorVector),
		USElement(tagDetectorVector, binary.LittleEndian, 1, 2),
		geometrySequence(tagDetectorInformationSequence,
			detectorItem([]string{"0", "0", "0"}, axialIOP), detectorItem([]string{"0", "0", "0"}, coronalIOP)),
	}
	g := FromElements(elements, std.Dictionary).FrameGeometryAt(1)
	if g.ImagePositionPatient != nil || g.ImageOrientationPatient != nil || g.PositionSource != GeometrySourceAbsent || len(g.Findings) != 0 {
		t.Fatalf("planar NM frame geometry = %+v, want none", g)
	}
}

func legacyElements(extra ...core.Element) []core.Element {
	return append([]core.Element{
		isValue(tagNumberOfFrames, "3"),
		dsMulti(tagImagePositionPatient, "0", "0", "10"),
		dsMulti(tagImageOrientationPatient, axialIOP...),
		dsMulti(tagPixelSpacing, "1", "1"),
	}, extra...)
}

func TestFrameGeometryAtLegacyMultiframe(t *testing.T) {
	le := binary.LittleEndian
	derive := FrameGeometryOptions{DeriveLegacyFramePositions: true}
	for _, tc := range []struct {
		name     string
		elements []core.Element
		options  FrameGeometryOptions
		want     [][]float64
		source   GeometrySource
		step     SliceSpacingSource
		finding  FrameGeometryFinding
	}{
		{
			name:     "default keeps the top-level position",
			elements: legacyElements(dsMulti(tagSpacingBetweenSlices, "2")),
			want:     [][]float64{{0, 0, 10}, {0, 0, 10}, {0, 0, 10}},
			source:   GeometrySourceImagePlane,
		},
		{
			name:     "opt-in steps by spacing between slices",
			elements: legacyElements(dsMulti(tagSpacingBetweenSlices, "2"), dsMulti(tagSliceThickness, "5")),
			options:  derive,
			want:     [][]float64{{0, 0, 10}, {0, 0, 12}, {0, 0, 14}},
			source:   GeometrySourceLegacyFrameIncrement,
			step:     SliceSpacingFromSpacingBetweenSlices,
		},
		{
			name:     "opt-in keeps a negative spacing between slices",
			elements: legacyElements(dsMulti(tagSpacingBetweenSlices, "-2")),
			options:  derive,
			want:     [][]float64{{0, 0, 10}, {0, 0, 8}, {0, 0, 6}},
			source:   GeometrySourceLegacyFrameIncrement,
			step:     SliceSpacingFromSpacingBetweenSlices,
			finding:  FindingNegativeSpacingBetweenSlices,
		},
		{
			name:     "opt-in falls back to slice thickness",
			elements: legacyElements(dsMulti(tagSliceThickness, "5")),
			options:  derive,
			want:     [][]float64{{0, 0, 10}, {0, 0, 15}, {0, 0, 20}},
			source:   GeometrySourceLegacyFrameIncrement,
			step:     SliceSpacingFromSliceThickness,
		},
		{
			name:     "opt-in without any spacing",
			elements: legacyElements(),
			options:  derive,
			want:     [][]float64{{0, 0, 10}, {0, 0, 10}, {0, 0, 10}},
			source:   GeometrySourceImagePlane,
			finding:  FindingSpacingBetweenSlicesMissing,
		},
		{
			name: "opt-in follows a slice location vector",
			elements: legacyElements(ATElement(tagFrameIncrementPointer, le, tagSliceLocationVector),
				dsMulti(tagSliceLocationVector, "-5", "-2.5", "0")),
			options: derive,
			want:    [][]float64{{0, 0, 10}, {0, 0, 12.5}, {0, 0, 15}},
			source:  GeometrySourceLegacyFrameIncrement,
		},
		{
			name: "opt-in rejects a short slice location vector",
			elements: legacyElements(ATElement(tagFrameIncrementPointer, le, tagSliceLocationVector),
				dsMulti(tagSliceLocationVector, "-5", "-2.5")),
			options: derive,
			want:    [][]float64{{0, 0, 10}, {0, 0, 10}, {0, 0, 10}},
			source:  GeometrySourceImagePlane,
			finding: FindingSliceLocationVectorInvalid,
		},
		{
			name: "opt-in leaves temporal frames alone",
			elements: legacyElements(ATElement(tagFrameIncrementPointer, le, core.NewTag(0x0018, 0x1063)),
				dsMulti(tagSpacingBetweenSlices, "2")),
			options: derive,
			want:    [][]float64{{0, 0, 10}, {0, 0, 10}, {0, 0, 10}},
			source:  GeometrySourceImagePlane,
			finding: FindingFrameIncrementNotSpatial,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := FromElements(tc.elements, std.Dictionary)
			if got := framePositions(obj, 3, tc.options); !positionsClose(got, tc.want) {
				t.Fatalf("positions = %v, want %v", got, tc.want)
			}
			g := obj.FrameGeometryAtWithOptions(1, tc.options)
			if g.PositionSource != tc.source {
				t.Fatalf("PositionSource = %q, want %q", g.PositionSource, tc.source)
			}
			if tc.finding != "" && !containsFinding(g.Findings, tc.finding) {
				t.Fatalf("Findings = %v, want %s", g.Findings, tc.finding)
			}
			if tc.source != GeometrySourceLegacyFrameIncrement {
				if g.PositionDerivation != nil {
					t.Fatalf("PositionDerivation = %+v, want nil", g.PositionDerivation)
				}
				return
			}
			if g.PositionDerivation == nil || !g.PositionDerivation.Recovered || g.PositionDerivation.StepSource != tc.step {
				t.Fatalf("PositionDerivation = %+v, want recovered step from %q", g.PositionDerivation, tc.step)
			}
		})
	}
}

// TestFrameGeometryAtFunctionalGroupsWinOverDerivation: a Plane Position
// functional group is never replaced, and its provenance is reported.
func TestFrameGeometryAtFunctionalGroupsWinOverDerivation(t *testing.T) {
	elements := append(spectElements([]uint16{1, 2, 3}, "4", detectorItem([]string{"0", "0", "0"}, axialIOP)),
		geometrySequence(tagSharedFunctionalGroups, core.DataSet{Elements: []core.Element{
			geometrySequence(tagPlaneOrientationSequence, core.DataSet{Elements: []core.Element{
				dsMulti(tagImageOrientationPatient, axialIOP...),
			}}),
		}}),
		geometrySequence(tagPerFrameFunctionalGroups,
			planePositionItem("7"), planePositionItem("8"), planePositionItem("9")))
	g := FromElements(elements, std.Dictionary).FrameGeometryAtWithOptions(1, FrameGeometryOptions{DeriveLegacyFramePositions: true})
	if len(g.ImagePositionPatient) != 3 || g.ImagePositionPatient[2] != 8 || g.PositionSource != GeometrySourcePerFrameFunctionalGroups ||
		g.OrientationSource != GeometrySourceSharedFunctionalGroups || g.PositionDerivation != nil {
		t.Fatalf("geometry = %+v, want per-frame position 8 and shared orientation", g)
	}
}

func planePositionItem(z string) core.DataSet {
	return core.DataSet{Elements: []core.Element{
		geometrySequence(tagPlanePositionSequence, core.DataSet{Elements: []core.Element{
			dsMulti(tagImagePositionPatient, "0", "0", z),
		}}),
	}}
}

func TestGetTags(t *testing.T) {
	frameTime := core.NewTag(0x0018, 0x1063)
	for _, tc := range []struct {
		name    string
		element core.Element
		want    []core.Tag
	}{
		{"raw little endian", ATElement(tagFrameIncrementPointer, binary.LittleEndian, frameTime, tagSliceVector), []core.Tag{frameTime, tagSliceVector}},
		{"typed value", core.Element{Header: core.ElementHeader{Tag: tagFrameIncrementPointer, VR: core.VRAT}, Value: core.TagValue{tagSliceVector}}, []core.Tag{tagSliceVector}},
		{"truncated", core.NewRawElement(tagFrameIncrementPointer, core.VRAT, []byte{0x54, 0x00}), nil},
		{"wrong VR", core.NewRawElement(tagFrameIncrementPointer, core.VRUL, []byte{0x54, 0x00, 0x80, 0x00}), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FromElements([]core.Element{tc.element}, nil).GetTags(tagFrameIncrementPointer)
			if !reflect.DeepEqual(got, tc.want) || ok != (tc.want != nil) {
				t.Fatalf("GetTags = %v, %v; want %v", got, ok, tc.want)
			}
		})
	}
}

func containsFinding(findings []FrameGeometryFinding, want FrameGeometryFinding) bool {
	for _, finding := range findings {
		if finding == want {
			return true
		}
	}
	return false
}
