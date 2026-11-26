package multiframe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	iodrules "github.com/ThalesMMS/dicom-go/validation/iod"
)

// checkIOD fails the test on each IOD error of obj (validation/iod).
func checkIOD(t *testing.T, label string, obj *object.Object) {
	t.Helper()
	report, err := iodrules.Validate(obj, iodrules.Options{})
	if err != nil {
		t.Fatalf("%s: %v", label, err)
	}
	for _, f := range report.Errors() {
		t.Errorf("%s (%s): %s", label, report.IOD, f)
	}
}

var legacyTestTime = time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)

// classicTestSeries builds n classic images of iod, one per slice along z,
// with a private attribute all share and a Slice Location each has its own.
func classicTestSeries(t *testing.T, iod imagebuild.IOD, n int) []*object.File {
	t.Helper()
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.8, 0.8},
		RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 2, FrameOfReferenceUID: "1.2.826.0.1.3680043.10.1.1042.9"}
	slices := make([]imagebuild.Slice, n)
	for z := range slices {
		slices[z].Position = [3]float64{-10, -20, 30 + 2*float64(z)}
		for p := 0; p < 20; p++ {
			slices[z].Values = append(slices[z].Values, float64(-500+p*37+z*101))
		}
	}
	files, err := imagebuild.Build(geometry, slices, imagebuild.Options{IOD: iod, SeriesNumber: 3, RescaleType: map[imagebuild.IOD]string{imagebuild.CT: "HU"}[iod],
		SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1042.2", ContentTime: legacyTestTime})
	if err != nil {
		t.Fatal(err)
	}
	for z, file := range files {
		ds := file.Dataset
		ds.Put(str(core.NewTag(0x0018, 0x0015), core.VRCS, "CHEST"))
		ds.Put(str(core.NewTag(0x0020, 0x1041), core.VRDS, strconv.Itoa(30+2*z)))
		ds.Put(str(core.NewTag(0x0029, 0x0010), core.VRLO, "SYNTHETIC CREATOR"))
		ds.Put(core.NewRawElement(core.NewTag(0x0029, 0x1001), core.VRLO, []byte("shared")))
	}
	return files
}

// petTestSeries turns a CT series into classic PET images.
func petTestSeries(t *testing.T, n int) []*object.File {
	t.Helper()
	files := classicTestSeries(t, imagebuild.CT, n)
	for z, file := range files {
		ds := file.Dataset
		for _, tag := range []core.Tag{tagKVP, tagRescaleType, core.NewTag(0x0018, 0x9004), core.NewTag(0x0018, 0x5100)} {
			ds.Remove(tag)
		}
		for _, element := range ds.Elements() {
			if element.Tag().Group == 0x0018 && element.Tag() != core.NewTag(0x0018, 0x0015) && element.Tag() != tagSliceThickness && element.Tag() != core.NewTag(0x0018, 0x5100) {
				ds.Remove(element.Tag())
			}
		}
		ds.Put(ui(tagSOPClassUID, PETImageStorageUID))
		ds.Put(str(tagModality, core.VRCS, "PT"))
		ds.Put(stringsElement(tagImageType, core.VRCS, []string{"ORIGINAL", "PRIMARY"}))
		for _, e := range []core.Element{
			str(tagSeriesDate, core.VRDA, "20260926"), str(tagSeriesTime, core.VRTM, "100000"),
			str(core.NewTag(0x0008, 0x0022), core.VRDA, "20260926"), str(core.NewTag(0x0008, 0x0032), core.VRTM, "100000"),
			str(tagUnits, core.VRCS, "BQML"), str(tagCountsSource, core.VRCS, "EMISSION"),
			stringsElement(tagSeriesType, core.VRCS, []string{"STATIC", "IMAGE"}),
			stringsElement(tagCorrectedImage, core.VRCS, []string{"DECY", "ATTN"}), str(tagDecayCorrection, core.VRCS, "START"),
			str(core.NewTag(0x0018, 0x1181), core.VRCS, "NONE"), str(tagFrameReferenceTime, core.VRDS, "0"),
			str(tagActualFrameDuration, core.VRIS, "180000"), str(tagDecayFactor, core.VRDS, "1.05"),
			seq(core.NewTag(0x0054, 0x0016)), seq(core.NewTag(0x0054, 0x0410)), seq(core.NewTag(0x0054, 0x0414)),
		} {
			ds.Put(e)
		}
		putUS16(ds, tagNumberOfSlices, n)
		putUS16(ds, tagImageIndex, z+1)
		// PET stores 16 bits.
		putUS16(ds, tagBitsStored, 16)
		putUS16(ds, tagHighBit, 15)
	}
	return files
}

func legacyOptions() LegacyOptions {
	return LegacyOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.1.1042.50", SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1042.51",
		Equipment: ContributingEquipment{Manufacturer: "dicom-go"}, Now: func() time.Time { return legacyTestTime }}
}

func nativePixels(t *testing.T, file *object.File) []byte {
	t.Helper()
	frames, err := pixeldata.ExtractNativeFramesView(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	return bytes.Join(frames.Data, nil)
}

func perFrameItem(t *testing.T, ds *object.Object, frame int, macro core.Tag) *object.Object {
	t.Helper()
	item, _, _ := ds.FunctionalGroupItem(frame, macro)
	if item == nil {
		t.Fatalf("frame %d has no %s", frame, macro)
	}
	return item
}

func TestConvertToLegacyEnhancedAndBack(t *testing.T) {
	for _, c := range []struct {
		name   string
		build  func(*testing.T, int) []*object.File
		target string
	}{
		{"CT", func(t *testing.T, n int) []*object.File { return classicTestSeries(t, imagebuild.CT, n) }, LegacyConvertedEnhancedCTImageStorageUID},
		{"MR", func(t *testing.T, n int) []*object.File { return classicTestSeries(t, imagebuild.MR, n) }, LegacyConvertedEnhancedMRImageStorageUID},
		{"PET", petTestSeries, LegacyConvertedEnhancedPETImageStorageUID},
	} {
		t.Run(c.name, func(t *testing.T) {
			sources := c.build(t, 4)
			// Out of order: the conversion sorts along the normal.
			shuffled := []*object.File{sources[2], sources[0], sources[3], sources[1]}
			legacy, report, err := ConvertToLegacyEnhanced(context.Background(), shuffled, legacyOptions())
			if err != nil {
				t.Fatal(err)
			}
			ds := legacy.Dataset
			checkIOD(t, "legacy converted", ds)
			if got, _ := ds.GetString(tagSOPClassUID); got != c.target || report.Frames != 4 || report.OrderedBy != "position" {
				t.Fatalf("SOP Class %s, report %+v", got, report)
			}
			var want []byte
			for _, src := range sources {
				want = append(want, nativePixels(t, src)...)
			}
			if !bytes.Equal(nativePixels(t, legacy), want) {
				t.Fatal("frames are not the sources' pixels in position order")
			}
			shared, _ := ds.GetSequence(tagSharedFunctionalGroupsSequence)
			for _, macro := range []core.Tag{tagPixelMeasuresSequence, tagPlaneOrientationSequence, tagFrameAnatomySequence, tagUnassignedSharedConverted} {
				if !shared[0].Has(macro) {
					t.Errorf("shared functional groups lack %s", macro)
				}
			}
			anatomy, _ := shared[0].GetSequence(tagFrameAnatomySequence)
			regions, _ := anatomy[0].GetSequence(tagAnatomicRegionSequence)
			if code, _ := regions[0].GetString(tagCodeValue); code != bodyPartRegions["CHEST"][1] {
				t.Errorf("Frame Anatomy code %q", code)
			}
			unassigned, _ := shared[0].GetSequence(tagUnassignedSharedConverted)
			if creator, _ := unassigned[0].GetString(core.NewTag(0x0029, 0x0010)); creator != "SYNTHETIC CREATOR" {
				t.Errorf("private creator in the shared item: %q", creator)
			}
			for i, src := range sources {
				position := perFrameItem(t, ds, i, tagPlanePositionSequence)
				got, _ := position.GetFloats(tagImagePositionPatient)
				wantPosition, _ := src.Dataset.GetFloats(tagImagePositionPatient)
				if fmt.Sprint(got) != fmt.Sprint(wantPosition) {
					t.Fatalf("frame %d at %v, want %v", i, got, wantPosition)
				}
				converted := perFrameItem(t, ds, i, tagUnassignedPerFrameConverted)
				if loc, _ := converted.GetString(core.NewTag(0x0020, 0x1041)); strings.TrimSpace(loc) != strconv.Itoa(30+2*i) {
					t.Errorf("frame %d Slice Location %q", i, loc)
				}
				reference := perFrameItem(t, ds, i, tagConversionSourceAttributesSequence)
				if uid, _ := reference.GetString(tagReferencedSOPInstanceUID); uid != mustString(t, src.Dataset, tagSOPInstanceUID) {
					t.Errorf("frame %d references %s", i, uid)
				}
			}

			classic, _, err := ConvertToClassic(context.Background(), legacy, Options{
				SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1042.60", Equipment: ContributingEquipment{Manufacturer: "dicom-go"},
				SOPInstanceUIDForFrame: func(i int) (string, error) { return "1.2.826.0.1.3680043.10.1.1042.7" + strconv.Itoa(i), nil },
				Now:                    func() time.Time { return legacyTestTime },
			})
			if err != nil {
				t.Fatal(err)
			}
			for i, out := range classic {
				checkIOD(t, fmt.Sprintf("classic frame %d", i), out.Dataset)
				if !bytes.Equal(nativePixels(t, out), nativePixels(t, sources[i])) {
					t.Fatalf("round trip frame %d pixels differ", i)
				}
				for _, tag := range []core.Tag{tagImagePositionPatient, tagImageOrientationPatient, tagPixelSpacing, tagRescaleSlope, tagRescaleIntercept} {
					a, _ := out.Dataset.GetFloats(tag)
					b, _ := sources[i].Dataset.GetFloats(tag)
					if fmt.Sprint(a) != fmt.Sprint(b) {
						t.Errorf("round trip frame %d %s = %v, want %v", i, tag, a, b)
					}
				}
				if a, b := strings.Join(mustStrings(t, out.Dataset, tagImageType), `\`), strings.Join(mustStrings(t, sources[i].Dataset, tagImageType), `\`); a != b {
					t.Errorf("round trip frame %d Image Type %q, want %q", i, a, b)
				}
				if c.name == "PET" {
					for tag, want := range map[core.Tag]string{tagUnits: "BQML", tagDecayCorrection: "START", tagSeriesType: "STATIC"} {
						if got := stringOf(out.Dataset, tag); got != want {
							t.Errorf("PET %s = %q, want %q", tag, got, want)
						}
					}
				}
				for _, tag := range []core.Tag{tagPixelPresentation, tagReferencedSOPInstanceUID, tagFrameLaterality} {
					if out.Dataset.Has(tag) {
						t.Errorf("classic frame %d keeps Enhanced-only %s", i, tag)
					}
				}
			}
		})
	}
}

func mustStrings(t *testing.T, ds *object.Object, tag core.Tag) []string {
	t.Helper()
	values, ok := ds.GetStrings(tag)
	if !ok {
		t.Fatalf("missing %s", tag)
	}
	for i := range values {
		values[i] = cleanText(values[i])
	}
	return values
}

func mustString(t *testing.T, ds *object.Object, tag core.Tag) string {
	t.Helper()
	v, ok := ds.GetString(tag)
	if !ok {
		t.Fatalf("missing %s", tag)
	}
	return cleanText(v)
}

func TestConvertToClassicPreservesPrivateCreators(t *testing.T) {
	sources := classicTestSeries(t, imagebuild.CT, 2)
	for i, file := range sources {
		file.Dataset.Put(str(core.NewTag(0x0029, 0x0010), core.VRLO, "VARYING CREATOR"))
		file.Dataset.Put(str(core.NewTag(0x0029, 0x1001), core.VRLO, fmt.Sprintf("varying %d", i)))
		file.Dataset.Put(str(core.NewTag(0x0029, 0x0011), core.VRLO, "SHARED CREATOR"))
		file.Dataset.Put(str(core.NewTag(0x0029, 0x1101), core.VRLO, "shared"))
		file.Dataset.Put(str(core.NewTag(0x0029, 0x1102), core.VRLO, fmt.Sprintf("shared creator frame %d", i)))
	}
	legacy, _, err := ConvertToLegacyEnhanced(context.Background(), sources, legacyOptions())
	if err != nil {
		t.Fatal(err)
	}
	// The target also has a reserved block before either converted item is flattened.
	legacy.Dataset.Put(str(core.NewTag(0x0029, 0x0010), core.VRLO, "TOP CREATOR"))
	legacy.Dataset.Put(str(core.NewTag(0x0029, 0x1001), core.VRLO, "top"))
	classic, _, err := ConvertToClassic(context.Background(), legacy, validOptions())
	if err != nil {
		t.Fatal(err)
	}
	for i, file := range classic {
		if err := file.Dataset.ValidatePrivateReservations(); err != nil {
			t.Fatalf("frame %d: %v", i, err)
		}
		for _, want := range []struct {
			creator, value string
			offset         uint8
		}{
			{"TOP CREATOR", "top", 1},
			{"SHARED CREATOR", "shared", 1},
			{"SHARED CREATOR", fmt.Sprintf("shared creator frame %d", i), 2},
			{"VARYING CREATOR", fmt.Sprintf("varying %d", i), 1},
		} {
			tag, err := file.Dataset.PrivateTag(0x0029, want.creator, want.offset)
			if err != nil {
				t.Errorf("frame %d creator %q: %v", i, want.creator, err)
				continue
			}
			if got := stringOf(file.Dataset, tag); got != want.value {
				t.Errorf("frame %d %q offset %d: %q, want %q", i, want.creator, want.offset, got, want.value)
			}
		}
	}
}

func TestConvertToLegacyEnhancedRejects(t *testing.T) {
	ct := classicTestSeries(t, imagebuild.CT, 2)
	mr := classicTestSeries(t, imagebuild.MR, 1)
	other := classicTestSeries(t, imagebuild.CT, 1)
	other[0].Dataset.Put(ui(tagSeriesInstanceUID, "1.2.826.0.1.3680043.10.1.1042.99"))
	mono1 := classicTestSeries(t, imagebuild.CT, 1)
	mono1[0].Dataset.Put(str(tagPhotometricInterpretation, core.VRCS, "MONOCHROME1"))
	for name, c := range map[string]struct {
		files []*object.File
		want  error
	}{
		"none":        {nil, ErrInvalidSource},
		"mixed class": {[]*object.File{ct[0], mr[0]}, ErrInvalidSource},
		"two series":  {[]*object.File{ct[0], other[0]}, ErrInvalidSource},
		"duplicate":   {[]*object.File{ct[0], ct[0]}, ErrInvalidSource},
		"MONOCHROME1": {mono1, ErrUnsupportedSOPClass},
	} {
		if _, _, err := ConvertToLegacyEnhanced(context.Background(), c.files, legacyOptions()); !errors.Is(err, c.want) {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// enhancedPETTestFile builds a minimal Enhanced PET image with two temporal
// positions of two slices, correction flags and frame timing.
func enhancedPETTestFile(t *testing.T) *object.File {
	t.Helper()
	sources := petTestSeries(t, 4)
	legacy, _, err := ConvertToLegacyEnhanced(context.Background(), sources, legacyOptions())
	if err != nil {
		t.Fatal(err)
	}
	ds := legacy.Dataset
	ds.Put(ui(tagSOPClassUID, EnhancedPETImageStorageUID))
	for _, tag := range []core.Tag{tagUnits, tagSeriesType, tagCorrectedImage, tagDecayCorrection, tagSeriesDate, tagSeriesTime, tagNumberOfSlices} {
		ds.Remove(tag)
	}
	ds.Put(str(tagCountsSource, core.VRCS, "EMISSION"))
	ds.Put(str(core.NewTag(0x0018, 0x9758), core.VRCS, "YES"))
	ds.Put(str(core.NewTag(0x0018, 0x9759), core.VRCS, "YES"))
	ds.Put(str(core.NewTag(0x0018, 0x9760), core.VRCS, "NO"))
	ds.Put(str(tagDecayCorrection, core.VRCS, "START"))
	ds.Put(str(tagAcquisitionDateTime, core.VRDT, "20260926100000"))
	// Drop the classic attributes the Legacy item kept.
	shared, _ := ds.GetSequence(tagSharedFunctionalGroupsSequence)
	sharedItem := shared[0]
	sharedItem.Remove(tagUnassignedSharedConverted)
	sharedItem.Remove(tagFrameAnatomySequence)
	perFrame, _ := ds.GetSequence(tagPerFrameFunctionalGroupsSequence)
	items := make([]core.DataSet, len(perFrame))
	for i, item := range perFrame {
		item.Remove(tagUnassignedPerFrameConverted)
		item.Remove(tagConversionSourceAttributesSequence)
		content := dataSet(
			str(tagFrameAcquisitionDateTime, core.VRDT, fmt.Sprintf("202609261%d0000", i/2)),
			str(tagFrameReferenceDateTime, core.VRDT, fmt.Sprintf("202609261%d0100", i/2)),
			core.NewRawElement(tagFrameAcquisitionDuration, core.VRFD, binary.LittleEndian.AppendUint64(nil, math.Float64bits(120000))),
			core.NewRawElement(tagTemporalPositionIndex, core.VRUL, binary.LittleEndian.AppendUint32(nil, uint32(i/2+1))),
		)
		item.Put(seq(tagFrameContentSequence, content))
		item.Put(seq(tagPETFrameCorrectionFactors, dataSet(str(tagDecayFactor, core.VRDS, strconv.FormatFloat(1.05+float64(i)/10, 'f', 2, 64)))))
		items[i] = item.ToDataSet()
	}
	ds.Put(seq(tagSharedFunctionalGroupsSequence, sharedItem.ToDataSet()))
	ds.Put(seq(tagPerFrameFunctionalGroupsSequence, items...))
	return legacy
}

func TestConvertToClassicEnhancedPETDecayCorrection(t *testing.T) {
	for _, tc := range []struct {
		name, decayTime, acquisitionTime, timezone, want string
		missingFactor, wantError                         bool
	}{
		{name: "start", decayTime: "20260926100000", want: "START"},
		{name: "administration", decayTime: "20260926090000", want: "ADMIN"},
		{name: "fractional start", decayTime: "20260926100000.5", acquisitionTime: "20260926100000.5", want: "START"},
		{name: "different fraction", decayTime: "20260926100000.1", want: "ADMIN"},
		{name: "explicit timezone", decayTime: "20260926130000+0000", acquisitionTime: "20260926100000-0300", want: "START"},
		{name: "dataset timezone", decayTime: "20260926130000+0000", timezone: "-0300", want: "START"},
		{name: "different acquisition timezone", decayTime: "20260926130000+0000", acquisitionTime: "20260926130000+0000", timezone: "-0300", want: "START"},
		{name: "different timezone", decayTime: "20260926100000+0000", timezone: "-0300", want: "ADMIN"},
		{name: "missing datetime", wantError: true},
		{name: "invalid datetime", decayTime: "invalid", wantError: true},
		{name: "missing factor", decayTime: "20260926100000", missingFactor: true, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := enhancedPETTestFile(t)
			source.Dataset.Remove(tagDecayCorrection)
			source.Dataset.Remove(tagDecayFactor)
			if tc.decayTime != "" {
				source.Dataset.Put(str(tagDecayCorrectionDateTime, core.VRDT, tc.decayTime))
			}
			if tc.acquisitionTime != "" {
				source.Dataset.Put(str(tagAcquisitionDateTime, core.VRDT, tc.acquisitionTime))
			}
			if tc.timezone != "" {
				source.Dataset.Put(str(core.NewTag(0x0008, 0x0201), core.VRSH, tc.timezone))
			}
			if tc.missingFactor {
				perFrame, _ := source.Dataset.GetSequence(tagPerFrameFunctionalGroupsSequence)
				items := make([]core.DataSet, len(perFrame))
				for i, item := range perFrame {
					if i == 1 {
						item.Remove(tagPETFrameCorrectionFactors)
					}
					items[i] = item.ToDataSet()
				}
				source.Dataset.Put(seq(tagPerFrameFunctionalGroupsSequence, items...))
			}
			classic, _, err := ConvertToClassic(context.Background(), source, validOptions())
			if tc.wantError {
				if !errors.Is(err, ErrNonconformant) || classic != nil {
					t.Fatalf("classic=%v error=%v", classic, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			for i, file := range classic {
				if got := stringOf(file.Dataset, tagDecayCorrection); got != tc.want {
					t.Errorf("frame %d Decay Correction %q, want %q", i, got, tc.want)
				}
				wantFactor := strconv.FormatFloat(1.05+float64(i)/10, 'f', 2, 64)
				if got := stringOf(file.Dataset, tagDecayFactor); got != wantFactor {
					t.Errorf("frame %d Decay Factor %q, want %q", i, got, wantFactor)
				}
				if tc.timezone != "" || strings.ContainsAny(tc.acquisitionTime, "+-") {
					wantReference := float64((i/2*3600 + 60) * 1000)
					if got, err := file.Dataset.GetFloats(tagFrameReferenceTime); err != nil || len(got) != 1 || got[0] != wantReference {
						t.Errorf("frame %d Frame Reference Time %v, error %v, want %g", i, got, err, wantReference)
					}
				}
			}
		})
	}
}

func TestConvertToClassicEnhancedPET(t *testing.T) {
	source := enhancedPETTestFile(t)
	classic, report, err := ConvertToClassic(context.Background(), source, Options{
		SeriesInstanceUID: "1.2.826.0.1.3680043.10.1.1042.80", Equipment: ContributingEquipment{Manufacturer: "dicom-go"},
		SOPInstanceUIDForFrame: func(i int) (string, error) { return "1.2.826.0.1.3680043.10.1.1042.8" + strconv.Itoa(i), nil },
		Now:                    func() time.Time { return legacyTestTime },
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.TargetSOPClassUID != PETImageStorageUID || len(classic) != 4 {
		t.Fatalf("report %+v", report)
	}
	third := classic[2].Dataset
	for tag, want := range map[core.Tag]string{
		tagUnits: "BQML", tagSeriesType: `DYNAMIC\IMAGE`, tagCorrectedImage: `DECY\ATTN`, tagDecayCorrection: "START",
		tagSeriesDate: "20260926", tagSeriesTime: "100000", tagActualFrameDuration: "120000",
		core.NewTag(0x0008, 0x0032): "110000",
	} {
		values, _ := third.GetStrings(tag)
		for i := range values {
			values[i] = cleanText(values[i])
		}
		if got := strings.Join(values, `\`); got != want {
			t.Errorf("%s = %q, want %q", tag, got, want)
		}
	}
	// Frame 3 is referenced at 11:01:00, 3660 s after the series start.
	if ms, _ := third.GetFloats(tagFrameReferenceTime); len(ms) != 1 || math.Abs(ms[0]-3660000) > 0.5 {
		t.Errorf("Frame Reference Time %v", ms)
	}
	for tag, want := range map[core.Tag]uint16{tagNumberOfSlices: 2, tagNumberOfTimeSlices: 2, tagImageIndex: 3} {
		if got, _ := uint16Value(third, tag); got != want {
			t.Errorf("%s = %d, want %d", tag, got, want)
		}
	}
	if third.Has(tagTemporalPositionIndex) || third.Has(tagFrameReferenceDateTime) {
		t.Error("Enhanced-only Frame Content leaves kept")
	}
}
