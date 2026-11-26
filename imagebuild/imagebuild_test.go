package imagebuild

import (
	"bytes"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// reopen writes a built file as Part 10 and reads it back.
func reopen(t *testing.T, file *object.File) *object.File {
	t.Helper()
	var b bytes.Buffer
	if err := object.WriteFile(&b, file); err != nil {
		t.Fatal(err)
	}
	back, err := object.ReadFile(&b)
	if err != nil {
		t.Fatal(err)
	}
	return back
}

// modality decodes the stored values through the data set's own rescale.
func modality(t *testing.T, obj *object.Object) []float64 {
	t.Helper()
	native, err := pixeldata.ExtractNativeFrames(obj)
	if err != nil {
		t.Fatal(err)
	}
	m := native.Metadata
	lut, err := display.ModalityLUTFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	values, err := display.DecodeModality(display.Frame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: native.Data[0], Modality: lut,
		Format: display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit), Signed: m.PixelRepresentation == 1}})
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func obliqueGeometry() Geometry {
	s := math.Sqrt(0.5)
	return Geometry{Rows: 3, Columns: 4, PixelSpacing: [2]float64{0.7, 0.5},
		RowDirection: [3]float64{s, s, 0}, ColumnDirection: [3]float64{0, 0, -1}, SliceThickness: 1.25}
}

func TestBuildPreservesScalarValidityInStoredPixels(t *testing.T) {
	geometry := obliqueGeometry()
	geometry.Rows, geometry.Columns = 2, 2
	for _, tc := range []struct {
		name     string
		iod      IOD
		values   []float64
		signed   bool
		maxError float64
		rescale  *Rescale
	}{
		{"signed CT", CT, []float64{-1024, -1024, 0, 2048}, true, 0, nil},
		{"signed MR", MR, []float64{-400, -400, 0, 230}, true, 0, nil},
		{"full unsigned range", CT, []float64{0, math.NaN(), 32768, 65535}, false, 0.51, nil},
		{"fractional SC", SecondaryCapture, []float64{4.125, 4.125, 4.5, 9.25}, false, 0.00005, nil},
		{"constant SC", SecondaryCapture, []float64{4.125, 4.125, 4.125, 4.125}, false, 0, nil},
		{"negative slope", CT, []float64{-4, math.Inf(1), -6, -10}, false, 0, &Rescale{Slope: -2, Intercept: 2}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			valid := []bool{true, false, true, true}
			files, err := Build(geometry, []Slice{{Values: tc.values, Valid: valid}}, Options{IOD: tc.iod, Rescale: tc.rescale})
			if err != nil {
				t.Fatal(err)
			}
			defer files[0].Close()
			file := reopen(t, files[0])
			defer file.Close()
			report, err := iod.Validate(file.Dataset, iod.Options{CheckValues: true})
			if err != nil || len(report.Errors()) != 0 {
				t.Fatalf("IOD validation: %v %v", err, report.Errors())
			}
			dataset := file.Dataset
			padding, err := dataset.GetInts(tagPixelPaddingValue)
			if err != nil || len(padding) != 1 {
				t.Fatalf("Pixel Padding Value = %v, %v", padding, err)
			}
			element, _ := dataset.Get(tagPixelPaddingValue)
			wantVR := core.VRUS
			if tc.signed {
				wantVR = core.VRSS
			}
			if element.VR() != wantVR {
				t.Fatalf("Pixel Padding Value VR = %v, want %v", element.VR(), wantVR)
			}
			native, err := pixeldata.ExtractNativeFrames(dataset)
			if err != nil {
				t.Fatal(err)
			}
			if (native.Metadata.PixelRepresentation == 1) != tc.signed {
				t.Fatalf("unexpected Pixel Representation %d", native.Metadata.PixelRepresentation)
			}
			got := modality(t, dataset)
			for i, value := range tc.values {
				u := binary.LittleEndian.Uint16(native.Data[0][2*i:])
				stored := int64(u)
				if tc.signed {
					stored = int64(int16(u))
				}
				if (stored != padding[0]) != valid[i] {
					t.Fatalf("pixel %d stored as %d with padding %d (valid=%v)", i, stored, padding[0], valid[i])
				}
				if valid[i] && math.Abs(got[i]-value) > tc.maxError {
					t.Fatalf("pixel %d recovered %g, want %g within %g", i, got[i], value, tc.maxError)
				}
			}
		})
	}
}

func TestBuildMaskSharesOnePaddingCodeAcrossSlices(t *testing.T) {
	geometry := obliqueGeometry()
	geometry.Rows, geometry.Columns = 2, 2
	input := []Slice{
		{Values: []float64{0, 1, 2, 3}},
		{Values: []float64{0, 1, 2, 3}, Valid: []bool{true, false, true, true}},
		{Values: []float64{math.NaN(), math.NaN(), math.NaN(), math.NaN()}, Valid: make([]bool, 4)},
	}
	files, err := Build(geometry, input, Options{IOD: CT})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		for _, file := range files {
			_ = file.Close()
		}
	}()
	if files[0].Dataset.Has(tagPixelPaddingValue) {
		t.Fatal("all-valid slice has a padding declaration")
	}
	first, err := files[1].Dataset.GetInts(tagPixelPaddingValue)
	if err != nil || len(first) != 1 {
		t.Fatal(first, err)
	}
	last, err := files[2].Dataset.GetInts(tagPixelPaddingValue)
	if err != nil || len(last) != 1 || first[0] != last[0] {
		t.Fatal("inconsistent padding values", first, last, err)
	}
}

func TestBuildRefusesUnsupportedMaskedRepresentations(t *testing.T) {
	geometry := obliqueGeometry()
	geometry.Rows, geometry.Columns = 2, 2
	tests := []struct {
		name  string
		slice Slice
		opts  Options
	}{
		{"no valid samples", Slice{Values: []float64{1, 2, 3, 4}, Valid: make([]bool, 4)}, Options{IOD: CT}},
		{"invalid mask size", Slice{Values: []float64{1, 2, 3, 4}, Valid: []bool{false}}, Options{IOD: CT}},
		{"no fixed padding code", Slice{Values: []float64{0, math.MaxUint16, 100, 0}, Valid: []bool{true, true, true, false}}, Options{IOD: CT, Rescale: &Rescale{Slope: 1}}},
		{"classic MR fractional", Slice{Values: []float64{0, 1.125, 100, 0}, Valid: []bool{true, true, true, false}}, Options{IOD: MR}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if files, err := Build(geometry, []Slice{tc.slice}, tc.opts); !errors.Is(err, ErrInvalid) || len(files) != 0 {
				t.Fatalf("built %d files with error %v; want ErrInvalid", len(files), err)
			}
		})
	}
}

func TestBuildSecondaryCaptureOptionalImagePlane(t *testing.T) {
	geometry := obliqueGeometry()
	geometry.FrameOfReferenceUID = "1.2.926.42"
	slices := []Slice{{Values: make([]float64, 12), Position: [3]float64{-1, 2, 3}}}
	for _, spatial := range []bool{false, true} {
		files, err := Build(geometry, slices, Options{IOD: SecondaryCapture, SecondaryCaptureImagePlane: spatial})
		if err != nil {
			t.Fatal(err)
		}
		file := reopen(t, files[0])
		for _, tag := range []core.Tag{tagImageOrientation, tagImagePosition, tagPixelSpacing, tagFrameOfReferenceUID, core.NewTag(0x0020, 0x1040)} {
			if file.Has(tag) != spatial {
				t.Fatalf("image plane requested = %v, inconsistent tag %v", spatial, tag)
			}
		}
		if spatial {
			uid, _ := file.GetString(tagFrameOfReferenceUID)
			if uid != geometry.FrameOfReferenceUID {
				t.Fatalf("wrong Frame of Reference UID %q", uid)
			}
		}
		_ = file.Close()
		_ = files[0].Close()
	}
	geometry.RowDirection = [3]float64{}
	if _, err := Build(geometry, slices, Options{IOD: SecondaryCapture, SecondaryCaptureImagePlane: true}); !errors.Is(err, ErrInvalid) {
		t.Fatal("spatial SC accepted a degenerate orientation", err)
	}
}

func TestBuildCTFromHounsfieldUnits(t *testing.T) {
	geometry := obliqueGeometry()
	slices := []Slice{{Position: [3]float64{-10, 20.5, 30}}, {Position: [3]float64{-9.5, 20, 30}}}
	for i := range slices {
		for p := 0; p < 12; p++ {
			slices[i].Values = append(slices[i].Values, float64(-1024+p*300+i*7))
		}
	}
	source := commontest.Source()
	source.Put(core.Element{Header: core.ElementHeader{Tag: tagModality, VR: core.VRCS}, Value: core.StringValue{"CT"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0018, 0x0060), VR: core.VRDS}, Value: core.StringValue{"120"}})
	files, err := Build(geometry, slices, Options{
		IOD: CT, Common: object.DerivedCommon{Source: source}, SeriesNumber: 900, SeriesDescription: "MPR oblique",
		RescaleType: "HU", Window: &Window{Center: 40, Width: 400}, ContentTime: time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC),
		Derivation: derivation.Derivation{Description: "Oblique MPR", Codes: []derivation.Code{derivation.MultiplanarReformatting},
			Sources: []derivation.Source{{SOPClassUID: ctImageStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1039.5", Purpose: &derivation.SourceImageForProcessing}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("%d files", len(files))
	}
	series := ""
	for i, file := range files {
		back := reopen(t, file)
		obj := back.Dataset
		for j, v := range modality(t, obj) {
			if v != slices[i].Values[j] {
				t.Fatalf("slice %d pixel %d: %v HU, want %v", i+1, j, v, slices[i].Values[j])
			}
		}
		if m, err := pixeldata.ExtractMetadata(obj); err != nil || m.PixelRepresentation != 1 {
			t.Fatalf("Pixel Representation %d (%v), want signed for HU", m.PixelRepresentation, err)
		}
		position, _ := obj.GetFloats(tagImagePosition)
		orientation, _ := obj.GetFloats(tagImageOrientation)
		spacing, _ := obj.GetFloats(tagPixelSpacing)
		if len(position) != 3 || position[0] != slices[i].Position[0] || position[1] != slices[i].Position[1] ||
			len(orientation) != 6 || math.Abs(orientation[0]-math.Sqrt(0.5)) > 1e-6 || orientation[5] != -1 ||
			len(spacing) != 2 || spacing[0] != 0.7 || spacing[1] != 0.5 {
			t.Fatalf("geometry %v %v %v", position, orientation, spacing)
		}
		for tag, want := range map[core.Tag]string{
			derivedioTag(0x0008, 0x0016): ctImageStorage, tagModality: "CT", tagInstanceNumber: []string{"1", "2"}[i],
			tagSeriesNumber: "900", tagRescaleType: "HU", core.NewTag(0x0018, 0x0060): "120", tagWindowCenter: "40",
			tagFrameOfReferenceUID: commontest.FrameOfReferenceUID, core.NewTag(0x0010, 0x0020): "SYN-1047",
			core.NewTag(0x0008, 0x2111): "Oblique MPR", tagSliceThickness: "1.25",
		} {
			if got, _ := obj.LookupString(tag); strings.TrimSpace(got) != want {
				t.Errorf("slice %d %s = %q, want %q", i+1, tag, got, want)
			}
		}
		uid, _ := obj.LookupString(derivedioTag(0x0020, 0x000e))
		if series == "" {
			series = uid
		} else if uid != series {
			t.Fatal("slices in different series")
		}
		codes, _ := obj.GetSequence(core.NewTag(0x0008, 0x9215))
		if len(codes) != 1 {
			t.Fatal("Derivation Code Sequence missing")
		}
		commontest.Check(t, obj, true)
	}
}

func TestBuildRescalesRealValues(t *testing.T) {
	geometry := obliqueGeometry()
	slice := Slice{}
	for p := 0; p < 12; p++ {
		slice.Values = append(slice.Values, 0.123*float64(p)-0.4)
	}
	files, err := Build(geometry, []Slice{slice}, Options{IOD: MR})
	if err != nil {
		t.Fatal(err)
	}
	obj := reopen(t, files[0]).Dataset
	slope, _ := obj.GetFloats(tagRescaleSlope)
	for j, v := range modality(t, obj) {
		if math.Abs(v-slice.Values[j]) > slope[0]/2+1e-9 {
			t.Fatalf("pixel %d: %v, want %v within %v", j, v, slice.Values[j], slope[0]/2)
		}
	}
	// Without a source the MR Type 1 attributes take their defaults.
	for tag, want := range map[core.Tag]string{core.NewTag(0x0018, 0x0020): "RM", core.NewTag(0x0018, 0x0021): "NONE"} {
		if got, _ := obj.LookupString(tag); got != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
	if !obj.Has(tagFrameOfReferenceUID) {
		t.Fatal("no Frame of Reference generated")
	}
	// Without a source the generated study is shared by the whole series.
	files, err = Build(geometry, []Slice{slice, slice}, Options{IOD: MR})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := files[0].Dataset.LookupString(tagStudyInstanceUID)
	second, _ := files[1].Dataset.LookupString(tagStudyInstanceUID)
	if first == "" || first != second {
		t.Fatalf("Study Instance UIDs %q and %q", first, second)
	}
}

func TestBuildSecondaryCaptureAndErrors(t *testing.T) {
	values := make([]float64, 12)
	files, err := Build(Geometry{Rows: 3, Columns: 4}, []Slice{{Values: values}}, Options{IOD: SecondaryCapture})
	if err != nil {
		t.Fatal(err)
	}
	obj := files[0].Dataset
	for _, tag := range []core.Tag{tagImagePosition, tagImageOrientation, tagPixelSpacing, tagFrameOfReferenceUID} {
		if obj.Has(tag) {
			t.Errorf("Secondary Capture has %s", tag)
		}
	}
	if !obj.Has(tagPatientOrientation) || !obj.Has(tagConversionType) {
		t.Fatal("Secondary Capture modules missing")
	}
	geometry := obliqueGeometry()
	good := []Slice{{Values: make([]float64, 12)}}
	for name, err := range map[string]error{
		"unknown IOD":       firstErr(Build(geometry, good, Options{})),
		"value count":       firstErr(Build(geometry, []Slice{{Values: make([]float64, 5)}}, Options{IOD: CT})),
		"NaN":               firstErr(Build(geometry, []Slice{{Values: append(make([]float64, 11), math.NaN())}}, Options{IOD: CT})),
		"not orthogonal":    firstErr(Build(Geometry{Rows: 3, Columns: 4, PixelSpacing: [2]float64{1, 1}, RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{1, 0, 0}}, good, Options{IOD: CT})),
		"no spacing":        firstErr(Build(Geometry{Rows: 3, Columns: 4, RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}}, good, Options{IOD: CT})),
		"rescale overflows": firstErr(Build(geometry, []Slice{{Values: append(make([]float64, 11), 1e6)}}, Options{IOD: CT, Rescale: &Rescale{Slope: 1}})),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestBuildRecordsPerSliceSourcesAndReferencedInstances: each instance
// references the series' sources plus its own slice's sources, and the
// Referenced Instance Sequence names a related instance with its purpose.
func TestBuildRecordsPerSliceSourcesAndReferencedInstances(t *testing.T) {
	registrationUID := "1.2.826.0.1.3680043.10.1.1179.9"
	slices := []Slice{
		{Values: make([]float64, 4*3), Position: [3]float64{0, 0, 0},
			Sources: []derivation.Source{{SOPClassUID: ctImageStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1179.11", Purpose: &derivation.SourceImageForProcessing}}},
		{Values: make([]float64, 4*3), Position: [3]float64{0, 0, 2}},
	}
	files, err := Build(obliqueGeometry(), slices, Options{
		IOD: CT, RescaleType: "HU",
		Derivation: derivation.Derivation{Description: "Resampled", Codes: []derivation.Code{derivation.SpatialResampling},
			Sources: []derivation.Source{{SOPClassUID: ctImageStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1179.10", Purpose: &derivation.MaskImageForProcessing}}},
		ReferencedInstances: []derivation.Source{{SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.1", SOPInstanceUID: registrationUID, Purpose: &derivation.SpatialRegistration}},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []int{2, 1} {
		obj := reopen(t, files[i]).Dataset
		sources, _ := obj.GetSequence(core.NewTag(0x0008, 0x2112))
		if len(sources) != want {
			t.Fatalf("instance %d has %d sources, want %d", i+1, len(sources), want)
		}
		related, _ := obj.GetSequence(core.NewTag(0x0008, 0x114A))
		if len(related) != 1 {
			t.Fatalf("instance %d has %d referenced instances", i+1, len(related))
		}
		uid, _ := related[0].GetString(core.NewTag(0x0008, 0x1155))
		purpose, _ := related[0].GetSequence(core.NewTag(0x0040, 0xA170))
		if strings.TrimRight(uid, "\x00") != registrationUID || len(purpose) != 1 {
			t.Fatalf("referenced instance %q with %d purposes", uid, len(purpose))
		}
	}
	if _, err := Build(obliqueGeometry(), slices[1:], Options{IOD: CT, ReferencedInstances: []derivation.Source{{SOPClassUID: "1.2.3", SOPInstanceUID: "1.2.4"}}}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("a referenced instance without purpose: %v", err)
	}
}

func firstErr(_ []*object.File, err error) error { return err }

func derivedioTag(group, element uint16) core.Tag { return core.NewTag(group, element) }
