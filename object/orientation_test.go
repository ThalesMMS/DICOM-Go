package object

import (
	"math"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
)

func csMulti(tag core.Tag, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VRCS}, Value: core.StringValue(values)}
}

func TestPatientDirectionLabel(t *testing.T) {
	cos30, sin30 := math.Cos(math.Pi/6), math.Sin(math.Pi/6)
	tests := []struct {
		name string
		dir  [3]float64
		opts OrientationLabelOptions
		want string
	}{
		{name: "left", dir: [3]float64{1, 0, 0}, want: "L"},
		{name: "right", dir: [3]float64{-1, 0, 0}, want: "R"},
		{name: "posterior", dir: [3]float64{0, 1, 0}, want: "P"},
		{name: "anterior", dir: [3]float64{0, -1, 0}, want: "A"},
		{name: "head", dir: [3]float64{0, 0, 1}, want: "H"},
		{name: "feet", dir: [3]float64{0, 0, -1}, want: "F"},
		{name: "superior display letters", dir: [3]float64{0, 0, 2}, opts: OrientationLabelOptions{SuperiorInferior: true}, want: "S"},
		{name: "inferior display letters", dir: [3]float64{0, 0, -2}, opts: OrientationLabelOptions{SuperiorInferior: true}, want: "I"},
		{name: "unnormalized input", dir: [3]float64{0, -250, 0}, want: "A"},
		{name: "30 degree oblique", dir: [3]float64{cos30, -sin30, 0}, want: "LA"},
		{name: "three axes by weight", dir: [3]float64{0.3, -0.9, 0.2}, want: "ALH"},
		{name: "three axes feet", dir: [3]float64{0.8, 0.5, -0.33}, want: "LPF"},
		{name: "max two letters", dir: [3]float64{0.8, 0.5, -0.33}, opts: OrientationLabelOptions{MaxLetters: 2}, want: "LP"},
		{name: "max one letter", dir: [3]float64{0.8, 0.5, -0.33}, opts: OrientationLabelOptions{MaxLetters: 1}, want: "L"},
		// Greedy selection keeps exact ties (Clunie's strict comparison would
		// return no label for a 45 degree direction); ties resolve x, y, z.
		{name: "45 degree tie", dir: [3]float64{math.Sqrt2 / 2, math.Sqrt2 / 2, 0}, want: "LP"},
		{name: "45 degree tie negative", dir: [3]float64{0, -math.Sqrt2 / 2, -math.Sqrt2 / 2}, want: "AF"},
		{name: "equal three axes", dir: [3]float64{-1, -1, -1}, want: "RAF"},
		{name: "floating noise ignored", dir: [3]float64{1, 1e-6, -1e-7}, want: "L"},
		{name: "small tilt kept", dir: [3]float64{0, 0.9998, -0.0175}, want: "PF"},
		{name: "custom threshold drops tilt", dir: [3]float64{0, 0.9998, -0.0175}, opts: OrientationLabelOptions{MinComponent: 0.1}, want: "P"},
		{name: "zero vector", dir: [3]float64{}, want: ""},
		{name: "nan vector", dir: [3]float64{math.NaN(), 1, 0}, want: ""},
		{name: "inf vector", dir: [3]float64{math.Inf(1), 1, 0}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := PatientDirectionLabel(tt.dir, tt.opts); got != tt.want {
				t.Fatalf("PatientDirectionLabel(%v) = %q, want %q", tt.dir, got, tt.want)
			}
		})
	}
}

func TestOppositeOrientationLabel(t *testing.T) {
	tests := map[string]string{
		"L": "R", "R": "L", "A": "P", "P": "A", "H": "F", "F": "H", "S": "I", "I": "S",
		"LA": "RP", "FL": "HR", "RAF": "LPH", "": "", "X": "", "LX": "",
	}
	for in, want := range tests {
		if got := OppositeOrientationLabel(in); got != want {
			t.Errorf("OppositeOrientationLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestMajorAxisLabel(t *testing.T) {
	tests := []struct {
		dir       [3]float64
		threshold float64
		want      string
	}{
		{dir: [3]float64{1, 0, 0}, want: "L"},
		{dir: [3]float64{0, -0.9, 0.1}, want: "A"},
		{dir: [3]float64{0, 0.1, -0.95}, want: "F"},
		{dir: [3]float64{math.Sqrt2 / 2, math.Sqrt2 / 2, 0}, want: ""},
		{dir: [3]float64{0.766, 0.643, 0}, want: ""},
		{dir: [3]float64{0.766, 0.643, 0}, threshold: 0.7, want: "L"},
		{dir: [3]float64{}, want: ""},
	}
	for _, tt := range tests {
		if got := MajorAxisLabel(tt.dir, tt.threshold, OrientationLabelOptions{}); got != tt.want {
			t.Errorf("MajorAxisLabel(%v, %v) = %q, want %q", tt.dir, tt.threshold, got, tt.want)
		}
	}
}

// TestClassifyImagePlane uses the conventional 0.8 obliquity threshold
// classification (axial/coronal/sagittal/oblique) as the oracle.
func TestClassifyImagePlane(t *testing.T) {
	c10, s10 := math.Cos(10*math.Pi/180), math.Sin(10*math.Pi/180)
	c40, s40 := math.Cos(40*math.Pi/180), math.Sin(40*math.Pi/180)
	tests := []struct {
		name string
		iop  []float64
		want ImagePlane
	}{
		{name: "axial", iop: []float64{1, 0, 0, 0, 1, 0}, want: ImagePlaneAxial},
		{name: "axial rotated in plane", iop: []float64{0, 1, 0, -1, 0, 0}, want: ImagePlaneAxial},
		{name: "axial prone", iop: []float64{-1, 0, 0, 0, -1, 0}, want: ImagePlaneAxial},
		{name: "axial 10 degree gantry tilt", iop: []float64{1, 0, 0, 0, c10, -s10}, want: ImagePlaneAxial},
		{name: "coronal", iop: []float64{1, 0, 0, 0, 0, -1}, want: ImagePlaneCoronal},
		{name: "coronal transposed", iop: []float64{0, 0, -1, 1, 0, 0}, want: ImagePlaneCoronal},
		{name: "sagittal", iop: []float64{0, 1, 0, 0, 0, -1}, want: ImagePlaneSagittal},
		{name: "sagittal unnormalized", iop: []float64{0, 2, 0, 0, 0, -3}, want: ImagePlaneSagittal},
		{name: "oblique 40 degrees", iop: []float64{c40, s40, 0, 0, 0, -1}, want: ImagePlaneOblique},
		{name: "oblique 45 degrees both", iop: []float64{math.Sqrt2 / 2, math.Sqrt2 / 2, 0, 0, math.Sqrt2 / 2, -math.Sqrt2 / 2}, want: ImagePlaneOblique},
		{name: "same major axis", iop: []float64{1, 0, 0, 1, 0, 0}, want: ImagePlaneOblique},
		{name: "short", iop: []float64{1, 0, 0, 0, 1}, want: ImagePlaneUnknown},
		{name: "zero", iop: []float64{0, 0, 0, 0, 0, 0}, want: ImagePlaneUnknown},
		{name: "nan", iop: []float64{math.NaN(), 0, 0, 0, 1, 0}, want: ImagePlaneUnknown},
		{name: "missing", want: ImagePlaneUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ClassifyImagePlane(tt.iop); got != tt.want {
				t.Fatalf("ClassifyImagePlane(%v) = %q, want %q", tt.iop, got, tt.want)
			}
		})
	}
}

func TestEdgeLabelsForImageOrientation(t *testing.T) {
	axial := []float64{1, 0, 0, 0, 1, 0}
	coronal := []float64{1, 0, 0, 0, 0, -1}
	sagittal := []float64{0, 1, 0, 0, 0, -1}
	cos30, sin30 := math.Cos(math.Pi/6), math.Sin(math.Pi/6)
	obliqueAxial := []float64{cos30, sin30, 0, -sin30, cos30, 0}
	si := OrientationLabelOptions{SuperiorInferior: true}
	tests := []struct {
		name      string
		iop       []float64
		transform DisplayTransform
		opts      OrientationLabelOptions
		want      OrientationEdgeLabels
	}{
		{name: "axial", iop: axial, want: OrientationEdgeLabels{Top: "A", Bottom: "P", Left: "R", Right: "L"}},
		{name: "axial prone", iop: []float64{-1, 0, 0, 0, -1, 0}, want: OrientationEdgeLabels{Top: "P", Bottom: "A", Left: "L", Right: "R"}},
		{name: "axial feet-first", iop: []float64{-1, 0, 0, 0, 1, 0}, want: OrientationEdgeLabels{Top: "A", Bottom: "P", Left: "L", Right: "R"}},
		{name: "coronal", iop: coronal, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "R", Right: "L"}},
		{name: "coronal S/I", iop: coronal, opts: si, want: OrientationEdgeLabels{Top: "S", Bottom: "I", Left: "R", Right: "L"}},
		{name: "sagittal", iop: sagittal, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "A", Right: "P"}},
		{name: "sagittal S/I", iop: sagittal, opts: si, want: OrientationEdgeLabels{Top: "S", Bottom: "I", Left: "A", Right: "P"}},
		{name: "oblique axial", iop: obliqueAxial, want: OrientationEdgeLabels{Top: "AL", Bottom: "PR", Left: "RA", Right: "LP"}},
		{name: "oblique double angulation", iop: []float64{0.8, 0.5, -0.33, 0, 0.55, 0.83}, want: OrientationEdgeLabels{Top: "FA", Bottom: "HP", Left: "RAH", Right: "LPF"}},
		{name: "axial rotate 90", iop: axial, transform: DisplayTransform{RotationDegrees: 90}, want: OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "P", Right: "A"}},
		{name: "axial rotate 180", iop: axial, transform: DisplayTransform{RotationDegrees: 180}, want: OrientationEdgeLabels{Top: "P", Bottom: "A", Left: "L", Right: "R"}},
		{name: "axial rotate 270", iop: axial, transform: DisplayTransform{RotationDegrees: 270}, want: OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "A", Right: "P"}},
		{name: "axial rotate -90", iop: axial, transform: DisplayTransform{RotationDegrees: -90}, want: OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "A", Right: "P"}},
		{name: "axial flip horizontal", iop: axial, transform: DisplayTransform{FlipHorizontal: true}, want: OrientationEdgeLabels{Top: "A", Bottom: "P", Left: "L", Right: "R"}},
		{name: "axial flip vertical", iop: axial, transform: DisplayTransform{FlipVertical: true}, want: OrientationEdgeLabels{Top: "P", Bottom: "A", Left: "R", Right: "L"}},
		{name: "axial flip both", iop: axial, transform: DisplayTransform{FlipHorizontal: true, FlipVertical: true}, want: OrientationEdgeLabels{Top: "P", Bottom: "A", Left: "L", Right: "R"}},
		{name: "rotate 90 then flip horizontal", iop: axial, transform: DisplayTransform{RotationDegrees: 90, FlipHorizontal: true}, want: OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "A", Right: "P"}},
		{name: "flip horizontal then rotate 90", iop: axial, transform: DisplayTransform{RotationDegrees: 90, FlipHorizontal: true, FlipBeforeRotation: true}, want: OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "P", Right: "A"}},
		{name: "sagittal rotate 90", iop: sagittal, transform: DisplayTransform{RotationDegrees: 90}, opts: si, want: OrientationEdgeLabels{Top: "A", Bottom: "P", Left: "I", Right: "S"}},
		{name: "axial rotate 30", iop: axial, transform: DisplayTransform{RotationDegrees: 30}, want: OrientationEdgeLabels{Top: "AR", Bottom: "PL", Left: "RP", Right: "LA"}},
		{name: "oblique undone by rotation", iop: obliqueAxial, transform: DisplayTransform{RotationDegrees: 30}, want: OrientationEdgeLabels{Top: "A", Bottom: "P", Left: "R", Right: "L"}},
		{name: "max letters", iop: []float64{0.8, 0.5, -0.33, 0, 0.55, 0.83}, opts: OrientationLabelOptions{MaxLetters: 1}, want: OrientationEdgeLabels{Top: "F", Bottom: "H", Left: "R", Right: "L"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EdgeLabelsForImageOrientation(tt.iop, tt.transform, tt.opts)
			if !ok {
				t.Fatalf("EdgeLabelsForImageOrientation(%v) reported no labels", tt.iop)
			}
			if got != tt.want {
				t.Fatalf("EdgeLabelsForImageOrientation(%v, %+v) = %+v, want %+v", tt.iop, tt.transform, got, tt.want)
			}
		})
	}
}

func TestEdgeLabelsForImageOrientationRejectsInvalidInput(t *testing.T) {
	for _, iop := range [][]float64{nil, {1, 0, 0}, {0, 0, 0, 0, 1, 0}, {1, 0, 0, math.NaN(), 1, 0}} {
		if got, ok := EdgeLabelsForImageOrientation(iop, DisplayTransform{}, OrientationLabelOptions{}); ok || !got.IsZero() {
			t.Errorf("EdgeLabelsForImageOrientation(%v) = %+v, %v; want no labels", iop, got, ok)
		}
	}
	if _, ok := EdgeLabelsForImageOrientation([]float64{1, 0, 0, 0, 1, 0}, DisplayTransform{RotationDegrees: math.NaN()}, OrientationLabelOptions{}); ok {
		t.Fatal("NaN rotation must not produce labels")
	}
}

func TestEdgeLabelsForPatientOrientation(t *testing.T) {
	si := OrientationLabelOptions{SuperiorInferior: true}
	tests := []struct {
		name      string
		values    []string
		transform DisplayTransform
		opts      OrientationLabelOptions
		want      OrientationEdgeLabels
		wantOK    bool
	}{
		{name: "DX chest PA", values: []string{"L", "F"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "R", Right: "L"}, wantOK: true},
		{name: "DX chest PA S/I", values: []string{"L", "F"}, opts: si, want: OrientationEdgeLabels{Top: "S", Bottom: "I", Left: "R", Right: "L"}, wantOK: true},
		{name: "DX chest AP stored mirrored", values: []string{"R", "F"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "L", Right: "R"}, wantOK: true},
		{name: "DX lateral", values: []string{"A", "F"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "P", Right: "A"}, wantOK: true},
		{name: "lower case and spaces", values: []string{" l ", "f"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F", Left: "R", Right: "L"}, wantOK: true},
		{name: "MG RCC", values: []string{"P", "L"}, want: OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "A", Right: "P"}, wantOK: true},
		{name: "MG LCC", values: []string{"A", "R"}, want: OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "P", Right: "A"}, wantOK: true},
		{name: "MG RMLO", values: []string{"P", "FL"}, want: OrientationEdgeLabels{Top: "HR", Bottom: "FL", Left: "A", Right: "P"}, wantOK: true},
		{name: "MG LMLO S/I", values: []string{"A", "FR"}, opts: si, want: OrientationEdgeLabels{Top: "SL", Bottom: "IR", Left: "P", Right: "A"}, wantOK: true},
		{name: "rotate 90", values: []string{"L", "F"}, transform: DisplayTransform{RotationDegrees: 90}, want: OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "F", Right: "H"}, wantOK: true},
		{name: "rotate 180", values: []string{"L", "F"}, transform: DisplayTransform{RotationDegrees: 180}, want: OrientationEdgeLabels{Top: "F", Bottom: "H", Left: "L", Right: "R"}, wantOK: true},
		{name: "flip horizontal", values: []string{"P", "L"}, transform: DisplayTransform{FlipHorizontal: true}, want: OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "P", Right: "A"}, wantOK: true},
		{name: "flip vertical", values: []string{"P", "FL"}, transform: DisplayTransform{FlipVertical: true}, want: OrientationEdgeLabels{Top: "FL", Bottom: "HR", Left: "A", Right: "P"}, wantOK: true},
		{name: "rotate 90 flip first", values: []string{"L", "F"}, transform: DisplayTransform{RotationDegrees: 90, FlipHorizontal: true, FlipBeforeRotation: true}, want: OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "F", Right: "H"}, wantOK: true},
		{name: "column only", values: []string{"", "F"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F"}, wantOK: true},
		{name: "invalid row letter", values: []string{"X", "F"}, want: OrientationEdgeLabels{Top: "H", Bottom: "F"}, wantOK: true},
		{name: "repeated axis rejected", values: []string{"LR", "HF"}, wantOK: false},
		{name: "empty", values: nil, wantOK: false},
		{name: "blank", values: []string{"", ""}, wantOK: false},
		{name: "non cardinal rotation", values: []string{"L", "F"}, transform: DisplayTransform{RotationDegrees: 30}, wantOK: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, ok := EdgeLabelsForPatientOrientation(tt.values, tt.transform, tt.opts)
			if ok != tt.wantOK || got != tt.want {
				t.Fatalf("EdgeLabelsForPatientOrientation(%q, %+v) = %+v, %v; want %+v, %v", tt.values, tt.transform, got, ok, tt.want, tt.wantOK)
			}
		})
	}
}

func TestMammographyPatientOrientation(t *testing.T) {
	tests := []struct {
		view, laterality string
		want             []string
	}{
		{view: "CC", laterality: "R", want: []string{"P", "L"}},
		{view: "CC", laterality: "L", want: []string{"A", "R"}},
		{view: "xccl", laterality: "r", want: []string{"P", "L"}},
		{view: "MLO", laterality: "R", want: []string{"P", "FL"}},
		{view: "MLO", laterality: "L", want: []string{"A", "FR"}},
		{view: "ML", laterality: "R", want: []string{"P", "F"}},
		{view: "LM", laterality: "L", want: []string{"A", "F"}},
		{view: "CC", laterality: "B"},
		{view: "CC", laterality: ""},
		{view: "FB", laterality: "R"},
		{view: "", laterality: "R"},
	}
	for _, tt := range tests {
		got, ok := MammographyPatientOrientation(tt.view, tt.laterality)
		if ok != (tt.want != nil) || !reflect.DeepEqual(got, tt.want) {
			t.Errorf("MammographyPatientOrientation(%q, %q) = %q, %v; want %q", tt.view, tt.laterality, got, ok, tt.want)
		}
	}
}

func TestOrientationInfoEdgeLabelsPrecedence(t *testing.T) {
	si := OrientationLabelOptions{SuperiorInferior: true}
	tests := []struct {
		name       string
		info       OrientationInfo
		transform  DisplayTransform
		want       OrientationEdgeLabels
		wantSource OrientationSource
	}{
		{
			name:       "image orientation wins over patient orientation",
			info:       OrientationInfo{ImageOrientationPatient: []float64{0, 1, 0, 0, 0, -1}, PatientOrientation: []string{"L", "F"}},
			want:       OrientationEdgeLabels{Top: "S", Bottom: "I", Left: "A", Right: "P"},
			wantSource: OrientationSourceImageOrientation,
		},
		{
			name:       "DX patient orientation",
			info:       OrientationInfo{Modality: "DX", PatientOrientation: []string{"L", "F"}, ViewPosition: "PA"},
			want:       OrientationEdgeLabels{Top: "S", Bottom: "I", Left: "R", Right: "L"},
			wantSource: OrientationSourcePatientOrientation,
		},
		{
			name:       "MG patient orientation wins over view",
			info:       OrientationInfo{Modality: "MG", PatientOrientation: []string{"A", "R"}, ViewPosition: "CC", ImageLaterality: "R"},
			want:       OrientationEdgeLabels{Top: "L", Bottom: "R", Left: "P", Right: "A"},
			wantSource: OrientationSourcePatientOrientation,
		},
		{
			name:       "MG view and image laterality fallback",
			info:       OrientationInfo{Modality: "MG", ViewPosition: "MLO", ImageLaterality: "R"},
			want:       OrientationEdgeLabels{Top: "SR", Bottom: "IL", Left: "A", Right: "P"},
			wantSource: OrientationSourceMammographyView,
		},
		{
			name:       "MG series laterality fallback with rotation",
			info:       OrientationInfo{Modality: "MG", ViewPosition: "CC", Laterality: "L"},
			transform:  DisplayTransform{RotationDegrees: 180},
			want:       OrientationEdgeLabels{Top: "R", Bottom: "L", Left: "A", Right: "P"},
			wantSource: OrientationSourceMammographyView,
		},
		{
			name: "DX view position alone is not enough",
			info: OrientationInfo{Modality: "DX", ViewPosition: "PA", ImageLaterality: "L"},
		},
		{
			name: "MG bilateral without orientation",
			info: OrientationInfo{Modality: "MG", ViewPosition: "CC", ImageLaterality: "B"},
		},
		{
			name: "nothing",
			info: OrientationInfo{Modality: "US"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, source := tt.info.EdgeLabels(tt.transform, si)
			if got != tt.want || source != tt.wantSource {
				t.Fatalf("EdgeLabels = %+v (%q), want %+v (%q)", got, source, tt.want, tt.wantSource)
			}
		})
	}
}

func TestOrientationInfoAtReadsAttributes(t *testing.T) {
	obj := FromElements([]core.Element{
		csMulti(tagOrientationModality, "MG"),
		csMulti(tagPatientOrientation, "P", "FL"),
		csMulti(tagOrientationViewPosition, "MLO"),
		csMulti(tagOrientationImageLaterality, "R"),
		csMulti(tagOrientationSeriesLaterality, "R"),
	}, std.Dictionary)
	info := obj.OrientationInfoAt(0)
	want := OrientationInfo{
		PatientOrientation: []string{"P", "FL"},
		Modality:           "MG",
		ViewPosition:       "MLO",
		ImageLaterality:    "R",
		Laterality:         "R",
	}
	if !reflect.DeepEqual(info, want) {
		t.Fatalf("OrientationInfoAt = %+v, want %+v", info, want)
	}
	file := &File{Dataset: obj}
	if got := file.OrientationInfoAt(0); !reflect.DeepEqual(got, want) {
		t.Fatalf("File.OrientationInfoAt = %+v, want %+v", got, want)
	}
	var nilFile *File
	if got := nilFile.OrientationInfoAt(0); !reflect.DeepEqual(got, OrientationInfo{}) {
		t.Fatalf("nil File.OrientationInfoAt = %+v", got)
	}
}

func TestOrientationInfoAtUsesFunctionalGroups(t *testing.T) {
	perFrame := func(orientation, laterality string) core.DataSet {
		return core.DataSet{Elements: []core.Element{
			geometrySequence(tagPatientOrientationInFrameSeq, core.DataSet{Elements: []core.Element{
				csMulti(tagPatientOrientation, "P", orientation),
			}}),
			geometrySequence(tagOrientationFrameAnatomySeq, core.DataSet{Elements: []core.Element{
				csMulti(tagOrientationFrameLateralityItem, laterality),
			}}),
		}}
	}
	obj := FromElements([]core.Element{
		csMulti(tagOrientationModality, "MG"),
		csMulti(tagPatientOrientation, "A", "R"),
		geometrySequence(tagSharedFunctionalGroups, core.DataSet{Elements: []core.Element{
			geometrySequence(tagPlaneOrientationSequence, core.DataSet{Elements: []core.Element{
				dsMulti(tagImageOrientationPatient, "1", "0", "0", "0", "0", "-1"),
			}}),
		}}),
		geometrySequence(tagPerFrameFunctionalGroups, perFrame("L", "R"), perFrame("FL", "L")),
	}, std.Dictionary)

	first := obj.OrientationInfoAt(0)
	second := obj.OrientationInfoAt(1)
	if !reflect.DeepEqual(first.PatientOrientation, []string{"P", "L"}) || first.ImageLaterality != "R" {
		t.Fatalf("frame 0 = %+v, want per-frame P\\L and laterality R", first)
	}
	if !reflect.DeepEqual(second.PatientOrientation, []string{"P", "FL"}) || second.ImageLaterality != "L" {
		t.Fatalf("frame 1 = %+v, want per-frame P\\FL and laterality L", second)
	}
	if !reflect.DeepEqual(second.ImageOrientationPatient, []float64{1, 0, 0, 0, 0, -1}) {
		t.Fatalf("frame 1 IOP = %v, want shared coronal", second.ImageOrientationPatient)
	}
	if outside := obj.OrientationInfoAt(7); !reflect.DeepEqual(outside.PatientOrientation, []string{"A", "R"}) {
		t.Fatalf("invalid frame = %+v, want top-level A\\R", outside)
	}
}
