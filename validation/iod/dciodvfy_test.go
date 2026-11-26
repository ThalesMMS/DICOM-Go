package iod_test

import (
	"bytes"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/encapimage"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/rtstruct"
	"github.com/ThalesMMS/dicom-go/sr"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// syntheticObjects are objects dicom-go writes: CT and Secondary Capture
// images, a presentation state of the CT, a Key Object Selection and an
// encapsulated PDF, all from one synthetic patient.
func syntheticObjects(t *testing.T) map[string]*object.Object {
	t.Helper()
	s := math.Sqrt(0.5)
	geometry := imagebuild.Geometry{Rows: 4, Columns: 5, PixelSpacing: [2]float64{0.7, 0.5},
		RowDirection: [3]float64{s, s, 0}, ColumnDirection: [3]float64{0, 0, -1}, SliceThickness: 1.25}
	slice := imagebuild.Slice{Position: [3]float64{-10, 20.5, 30}}
	for p := 0; p < 20; p++ {
		slice.Values = append(slice.Values, float64(-1024+p*100))
	}
	source := commontest.Source()
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0060), VR: core.VRCS}, Value: core.StringValue{"CT"}})
	source.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0018, 0x0060), VR: core.VRDS}, Value: core.StringValue{"120"}})
	when := time.Date(2026, 9, 26, 10, 0, 0, 0, time.UTC)
	ct, err := imagebuild.Build(geometry, []imagebuild.Slice{slice}, imagebuild.Options{
		IOD: imagebuild.CT, Common: object.DerivedCommon{Source: source}, SeriesNumber: 900, RescaleType: "HU",
		Window: &imagebuild.Window{Center: 40, Width: 400}, ContentTime: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	sc, err := imagebuild.Build(geometry, []imagebuild.Slice{slice}, imagebuild.Options{
		IOD: imagebuild.SecondaryCapture, Common: object.DerivedCommon{Source: commontest.Source()}, SeriesNumber: 901, ContentTime: when,
	})
	if err != nil {
		t.Fatal(err)
	}
	state, err := gsps.FromImage(ct[0].Dataset, gsps.FromImageOptions{})
	if err != nil {
		t.Fatal(err)
	}
	pr, err := gsps.Write(state)
	if err != nil {
		t.Fatal(err)
	}
	sopClass, _ := ct[0].Dataset.GetUID(core.NewTag(0x0008, 0x0016))
	sopInstance, _ := ct[0].Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	study, _ := ct[0].Dataset.GetUID(core.NewTag(0x0020, 0x000D))
	series, _ := ct[0].Dataset.GetUID(core.NewTag(0x0020, 0x000E))
	kos := sr.NewKeyObjectSelection("2.25.9870001", []sr.ImageReference{{StudyInstanceUID: study, SeriesInstanceUID: series, SOPClassUID: sopClass, SOPInstanceUID: sopInstance}}, nil)
	kos.Common = object.DerivedCommon{Source: commontest.Source()}
	kosDataset, err := kos.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := encapdoc.NewPDF(encapdoc.PDFOptions{
		SOPInstanceUID: "2.25.9870002", SeriesInstanceUID: "2.25.9870003", DocumentTitle: "Synthetic",
		Data: []byte("%PDF-1.4\n%%EOF\n"), Common: object.DerivedCommon{Source: commontest.Source()},
	})
	if err != nil {
		t.Fatal(err)
	}
	objects := map[string]*object.Object{"ct": ct[0].Dataset, "sc": sc[0].Dataset, "gsps": pr.Dataset, "kos": kosDataset, "pdf": pdf.Dataset}
	// The other Encapsulated Document IODs (#1051).
	document := func(uid string, data []byte) encapdoc.DocumentOptions {
		return encapdoc.DocumentOptions{SOPInstanceUID: uid, SeriesInstanceUID: uid + ".1", DocumentTitle: "Synthetic", Data: data,
			ContentDate: "20260926", ContentTime: "100000", Common: object.DerivedCommon{Source: commontest.Source()}}
	}
	stl := make([]byte, 84+50)
	stl[80] = 1
	for name, build := range map[string]func() (*object.File, error){
		"cda": func() (*object.File, error) {
			return encapdoc.NewCDA(encapdoc.CDAOptions{DocumentOptions: document("2.25.9870010", []byte(`<ClinicalDocument xmlns="urn:hl7-org:v3"><id root="2.25.9870011"/></ClinicalDocument>`))})
		},
		"stl": func() (*object.File, error) {
			return encapdoc.NewSTL(encapdoc.ModelOptions{DocumentOptions: document("2.25.9870012", stl)})
		},
		"obj": func() (*object.File, error) {
			return encapdoc.NewOBJ(encapdoc.ModelOptions{DocumentOptions: document("2.25.9870013", []byte("v 0 0 0\nv 1 0 0\nv 0 1 0\nf 1 2 3\n"))})
		},
		"mtl": func() (*object.File, error) {
			return encapdoc.NewMTL(encapdoc.ModelOptions{DocumentOptions: document("2.25.9870014", []byte("newmtl bone\nKd 1 1 1\n"))})
		},
	} {
		f, err := build()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		objects[name] = f.Dataset
	}
	return objects
}

// dciodvfyMissing matches dciodvfy's report of an absent unconditional
// top-level Attribute. dciodvfy also checks Modality (0008,0060), Type 1 in
// the General Series Module, as the Type 1C it gives the IOD's own Modality
// Values; that report counts as Type 1.
var dciodvfyMissing = regexp.MustCompile(`^Error - </[A-Za-z0-9]+\(([0-9a-fA-F]{4}),([0-9a-fA-F]{4})\)> - Missing attribute for Type ([12]) Required|^Error - </Modality\((0008),(0060)\)> - Missing attribute for Type (1)C Conditional - Module=<GeneralSeries>`)

// writeObject writes obj as a Part 10 file in dir.
func writeObject(t *testing.T, dir, name string, obj *object.Object) string {
	t.Helper()
	return writeObjectAs(t, dir, name, obj, transfer.ExplicitVRLittleEndian)
}

// writeObjectAs writes obj as a Part 10 file in dir with syntax.
func writeObjectAs(t *testing.T, dir, name string, obj *object.Object, syntax transfer.Syntax) string {
	t.Helper()
	sopClass, _ := obj.GetUID(core.NewTag(0x0008, 0x0016))
	sopInstance, _ := obj.GetUID(core.NewTag(0x0008, 0x0018))
	file := &object.File{Dataset: obj, Meta: object.New(nil), TransferSyntax: syntax}
	file.Meta.Put(core.NewRawElement(core.NewTag(0x0002, 0x0002), core.VRUI, []byte(evenUID(sopClass))))
	file.Meta.Put(core.NewRawElement(core.NewTag(0x0002, 0x0003), core.VRUI, []byte(evenUID(sopInstance))))
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, file); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, name+".dcm")
	if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func runDCIODVFY(t *testing.T, tool, dir, name string, obj *object.Object) map[string]bool {
	t.Helper()
	out, _ := exec.Command(tool, "-new", writeObject(t, dir, name, obj)).CombinedOutput()
	missing := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		if m := dciodvfyMissing.FindStringSubmatch(strings.TrimSpace(line)); m != nil {
			if m[1] == "" {
				m = m[3:]
			}
			missing[strings.ToUpper("("+m[1]+","+m[2]+")")+" type "+m[3]] = true
		}
	}
	return missing
}

// knownDifferences are reports only Validate makes, where it follows PS3.3
// and dciodvfy does not: Modality is Type 1 in the General Series Module of
// the Secondary Capture Image IOD, which dciodvfy does not check.
var knownDifferences = map[string]string{
	"sc-without-0008,0060": "(0008,0060) type 1",
}

func evenUID(uid string) string {
	if len(uid)%2 == 1 {
		return uid + "\x00"
	}
	return uid
}

// ourMissing is the same set from Validate: absent top-level Type 1 and 2
// Attributes, including the warnings of a Module inferred from its other
// Attributes (dciodvfy reports those as errors).
func ourMissing(t *testing.T, obj *object.Object) map[string]bool {
	t.Helper()
	report, err := iod.Validate(obj, iod.Options{})
	if err != nil {
		t.Fatal(err)
	}
	missing := map[string]bool{}
	for _, f := range report.Findings {
		if strings.Contains(f.Path, "/") {
			continue
		}
		switch f.Kind {
		case iod.MissingType1:
			missing[f.Path+" type 1"] = true
		case iod.MissingType2:
			missing[f.Path+" type 2"] = true
		}
	}
	return missing
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// TestValidateAgreesWithDCIODVFY checks dicom-go's objects and copies with
// a top-level Type 1 or Type 2 Attribute removed: Validate and dciodvfy must
// report the same absent unconditional top-level Attributes (none for the
// objects as written). Enable with DICOMGO_DCIODVFY naming the dciodvfy
// executable (dicom3tools).
func TestValidateAgreesWithDCIODVFY(t *testing.T) {
	tool := os.Getenv("DICOMGO_DCIODVFY")
	if tool == "" {
		t.Skip("opt-in: DICOMGO_DCIODVFY=/path/to/dciodvfy")
	}
	dir := t.TempDir()
	for name, obj := range syntheticObjects(t) {
		t.Run(name, func(t *testing.T) {
			compare := func(label string, obj *object.Object) {
				theirs, ours := runDCIODVFY(t, tool, dir, name+"-"+label, obj), ourMissing(t, obj)
				if known, ok := knownDifferences[name+"-"+label]; ok {
					if !ours[known] || theirs[known] {
						t.Errorf("%s: known difference %s no longer holds: dciodvfy %v, Validate %v", label, known, sortedKeys(theirs), sortedKeys(ours))
					}
					delete(ours, known)
				}
				if strings.Join(sortedKeys(theirs), ",") != strings.Join(sortedKeys(ours), ",") {
					t.Errorf("%s: dciodvfy %v, Validate %v", label, sortedKeys(theirs), sortedKeys(ours))
				}
			}
			compare("as-written", obj)
			// Remove each top-level Attribute in turn (but the SOP Class and
			// Instance UIDs, which identify the IOD and the file, and the
			// Specific Character Set the synthetic names need).
			for _, element := range obj.Elements() {
				tag := element.Header.Tag
				if tag.Group == 0x0008 && (tag.Element == 0x0005 || tag.Element == 0x0016 || tag.Element == 0x0018) {
					continue
				}
				mutated := object.FromElements(obj.Elements(), nil)
				mutated.Remove(tag)
				compare("without-"+strings.Trim(tag.String(), "()"), mutated)
			}
		})
	}
}

// writerObjects are files of the SR, RT Structure Set and encapsulated
// JPEG writers, with the common modules of a synthetic source image.
func writerObjects(t *testing.T) map[string]*object.File {
	t.Helper()
	const study, ctSeries, segSeries = "1.2.826.0.1.3680043.8.498.20260926.70", "1.2.826.0.1.3680043.8.498.20260926.71", "1.2.826.0.1.3680043.8.498.20260926.72"
	ct := sr.ImageReference{StudyInstanceUID: study, SeriesInstanceUID: ctSeries, SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.73"}
	enhanced := sr.ImageReference{StudyInstanceUID: study, SeriesInstanceUID: ctSeries, SOPClassUID: "1.2.840.10008.5.1.4.1.1.2.1", SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.74", Frames: []int{2}}
	common := func() object.DerivedCommon { return object.DerivedCommon{Source: commontest.Source()} }
	out := map[string]*object.File{}
	file := func(obj *object.Object) *object.File {
		return &object.File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}
	}

	report, err := sr.WriteMeasurementReport(&sr.MeasurementReport{
		SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.75", ContentDate: "20260926", ContentTime: "120000",
		Groups: []sr.MeasurementGroup{
			{
				Tracking: sr.TrackingIdentifier{Identifier: "Lesion 1", UID: "1.2.826.0.1.3680043.8.498.20260926.76"},
				ReferencedSegment: sr.SegmentReference{StudyInstanceUID: study, SeriesInstanceUID: segSeries,
					SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4", SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.77", SegmentNumber: 1},
				SourceSeriesUID: ctSeries,
				Measurements: []sr.ReportMeasurement{{
					ConceptName: sr.CodedEntry{CodeValue: "118565006", CodingSchemeDesignator: "SCT", CodeMeaning: "Volume"},
					Value:       1234.5, Units: sr.CodedEntry{CodeValue: "mm3", CodingSchemeDesignator: "UCUM", CodeMeaning: "cubic millimeter"},
				}},
			},
			{
				Tracking: sr.TrackingIdentifier{Identifier: "Lesion 2", UID: "1.2.826.0.1.3680043.8.498.20260926.78"},
				Measurements: []sr.ReportMeasurement{
					{ConceptName: sr.CodedEntry{CodeValue: "81827009", CodingSchemeDesignator: "SCT", CodeMeaning: "Diameter"}, Value: 12.5, Units: sr.MillimeterUnit(), Image: ct},
					{ConceptName: sr.CodedEntry{CodeValue: "81827009", CodingSchemeDesignator: "SCT", CodeMeaning: "Diameter"}, Value: 8, Units: sr.MillimeterUnit(), Image: enhanced},
				},
			},
		},
		Common: common(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out["measurement-report"] = report

	for name, sopClass := range map[string]string{"basic-text-sr": sr.BasicTextSRStorage, "comprehensive-sr": sr.ComprehensiveSRStorage} {
		doc := &sr.Document{
			SOPClassUID: sopClass, SOPInstanceUID: sr.DefaultObserverDeviceUID + ".1", ContentDate: "20260926", ContentTime: "120000",
			Title: sr.CodedEntry{CodeValue: "18748-4", CodingSchemeDesignator: "LN", CodeMeaning: "Diagnostic imaging report"},
			Content: []sr.ContentItem{{
				ValueType: sr.ValueText, RelationshipType: sr.RelationshipContains, Text: "No abnormality",
				ConceptName: sr.CodedEntry{CodeValue: "121071", CodingSchemeDesignator: "DCM", CodeMeaning: "Finding"},
				Children: []sr.ContentItem{
					{ValueType: sr.ValueImage, RelationshipType: sr.RelationshipInferredFrom, Image: ct},
					{ValueType: sr.ValueImage, RelationshipType: sr.RelationshipInferredFrom, Image: enhanced},
				},
			}},
			Common: common(),
		}
		dataset, err := doc.Dataset()
		if err != nil {
			t.Fatal(err)
		}
		out[name] = file(dataset)
	}

	// A Key Object Selection without a Series Instance UID gets one.
	kos := sr.NewKeyObjectSelection("1.2.826.0.1.3680043.8.498.20260926.79", []sr.ImageReference{ct, enhanced}, nil)
	kos.SeriesInstanceUID, kos.Common = "", common()
	kosDataset, err := kos.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	out["kos"] = file(kosDataset)

	structureSet, err := rtstruct.Write(&rtstruct.StructureSet{
		SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.80", StudyInstanceUID: study,
		SeriesInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.81", FrameOfReferenceUID: commontest.FrameOfReferenceUID,
		ROIs: []rtstruct.ROI{{Number: 1, Name: "PTV", Contours: []rtstruct.Contour{{
			GeometricType:    rtstruct.ContourClosedPlanar,
			Points:           []rtstruct.Point3D{{X: 1, Y: 1}, {X: 4, Y: 1}, {X: 4, Y: 4}, {X: 1, Y: 4}},
			ReferencedImages: []rtstruct.ReferencedImage{{StudyInstanceUID: study, SeriesInstanceUID: ctSeries, SOPClassUID: ct.SOPClassUID, SOPInstanceUID: ct.SOPInstanceUID}},
		}}}},
		Common: common(),
	})
	if err != nil {
		t.Fatal(err)
	}
	out["rtstruct"] = structureSet

	jpeg, err := os.ReadFile(filepath.Join("..", "..", "encapimage", "testdata", "baseline_gray.jpg"))
	if err != nil {
		t.Fatal(err)
	}
	capture, _, err := encapimage.FromJPEG(jpeg, encapimage.JPEGOptions{SOPInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.82", Common: common()})
	if err != nil {
		t.Fatal(err)
	}
	out["encapsulated-sc"] = capture

	// VL Photographic from every frame it accepts: gray, YCbCr lossy (4:4:4,
	// 4:2:2, progressive) and RGB lossless.
	for i, name := range []string{"baseline_gray.jpg", "baseline_444.jpg", "baseline_422.jpg", "progressive.jpg", "lossless_p4_rgb.jpg"} {
		jpeg, err := os.ReadFile(filepath.Join("..", "..", "encapimage", "testdata", name))
		if err != nil {
			t.Fatal(err)
		}
		photo, _, err := encapimage.FromJPEG(jpeg, encapimage.JPEGOptions{
			SOPClassUID: encapimage.VLPhotographicImageStorage, SOPInstanceUID: fmt.Sprintf("1.2.826.0.1.3680043.8.498.20260926.9%d", i),
			AllowRetiredProgressive: true, Common: common(),
		})
		if err != nil {
			t.Fatal(err)
		}
		out["encapsulated-vl-photographic-"+strings.TrimSuffix(name, ".jpg")] = photo
	}
	return out
}

// TestWrittenObjectsPassDCIODVFY requires dciodvfy -new to report no error
// on the objects of the SR, RT Structure Set and encapsulated JPEG (Secondary
// Capture and VL Photographic) writers.
// Enable with DICOMGO_DCIODVFY naming the dciodvfy executable (dicom3tools).
func TestWrittenObjectsPassDCIODVFY(t *testing.T) {
	tool := os.Getenv("DICOMGO_DCIODVFY")
	if tool == "" {
		t.Skip("opt-in: DICOMGO_DCIODVFY=/path/to/dciodvfy")
	}
	dir := t.TempDir()
	for name, file := range writerObjects(t) {
		t.Run(name, func(t *testing.T) {
			out, _ := exec.Command(tool, "-new", writeObjectAs(t, dir, name, file.Dataset, file.TransferSyntax)).CombinedOutput()
			for _, line := range strings.Split(string(out), "\n") {
				if strings.HasPrefix(line, "Error") {
					t.Errorf("dciodvfy: %s", strings.TrimSpace(line))
				}
			}
		})
	}
}
