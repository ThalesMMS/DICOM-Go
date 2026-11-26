package dicomdir

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/encapdoc"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/imagebuild"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/rtstruct"
	"github.com/ThalesMMS/dicom-go/sr"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

func TestMixedDirectoryAll34StandardRecordTypes(t *testing.T) {
	root := t.TempDir()
	if export := os.Getenv("DICOMDIR_1328_MATRIX_FIXTURE_DIR"); export != "" {
		root = export
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	set := testDirectorySet(t, root, Options{})
	for i, typ := range allFileRecordTypes {
		id := FileID{fmt.Sprintf("FILE%04d", i)}
		testDirectoryWriteSource(t, root, id, testDirectorySource(t, typ, i+1), transfer.ExplicitVRLittleEndian)
		if err := set.Add(context.Background(), id); err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
	}
	var output bytes.Buffer
	report, err := WriteDICOMDIR(context.Background(), &output, set, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.Records != 34 || report.LogicalReferences != 31 || report.MRDRRecords != 0 {
		t.Fatalf("report %+v", report)
	}
	assertDirectoryByteOffsets(t, output.Bytes(), 31, 0)
	file, err := object.ReadFile(bytes.NewReader(output.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	items, _ := file.Dataset.GetSequence(tagDirectoryRecordSequence)
	types := map[string]bool{}
	for _, item := range items {
		typ, _ := item.GetString(tagDirectoryRecordType)
		types[typ] = true
	}
	if len(types) != 34 {
		t.Fatalf("record type coverage %d", len(types))
	}
	if err := os.WriteFile(filepath.Join(root, "DICOMDIR"), output.Bytes(), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseFileSet(filepath.Join(root, "DICOMDIR")); err != nil {
		t.Fatal(err)
	}
}

// Six full source IOD fixtures qualify ordinary interoperable records separately
// from the 34 directory-schema fixtures above. Export is explicit and synthetic.
func TestMixedDirectoryCompleteSourceIODs(t *testing.T) {
	root := t.TempDir()
	if export := os.Getenv("DICOMDIR_1328_FIXTURE_DIR"); export != "" {
		root = export
		if err := os.MkdirAll(root, 0700); err != nil {
			t.Fatal(err)
		}
	}
	source := commontest.Source()
	source.Put(textElement(tagStudyTimeFS, core.VRTM, "120000"))
	source.Put(textElement(tagStudyIDFS, core.VRSH, "DIRECTORY"))
	source.Put(textElement(tagModalityFS, core.VRCS, "CT"))
	source.Put(textElement(keyTag(0x0018, 0x0060), core.VRDS, "120"))
	built, err := imagebuild.Build(imagebuild.Geometry{Rows: 2, Columns: 3, PixelSpacing: [2]float64{0.7, 0.8}, RowDirection: [3]float64{1, 0, 0}, ColumnDirection: [3]float64{0, 1, 0}, SliceThickness: 1}, []imagebuild.Slice{{Values: []float64{-1000, -500, 0, 100, 500, 1000}, Position: [3]float64{1, 2, 3}}}, imagebuild.Options{IOD: imagebuild.CT, Common: object.DerivedCommon{Source: source}, SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.500.1", SeriesNumber: 1, RescaleType: "HU", ContentTime: time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	ct := built[0].Dataset
	ct.Put(textElement(keyTag(0x0020, 0x0060), core.VRCS, "R"))
	ct.Put(uiElementValue(tagSOPInstanceUIDFS, "1.2.826.0.1.3680043.10.999.500.2"))
	study, _ := ct.GetUID(tagStudyInstanceUIDFS)
	series, _ := ct.GetUID(tagSeriesInstanceUIDFS)
	instance, _ := ct.GetUID(tagSOPInstanceUIDFS)
	class, _ := ct.GetUID(tagSOPClassUIDFS)
	common := object.DerivedCommon{Source: ct}
	state, err := gsps.FromImage(ct, gsps.FromImageOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.999.500.3", SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.500.4"})
	if err != nil {
		t.Fatal(err)
	}
	state.CreationDate = "20261009"
	state.CreationTime = "120000"
	state.ContentLabel = "SYNTHETIC"
	state.GraphicLayers = []gsps.GraphicLayer{{Name: "SYNTHETIC", Order: 1}}
	pr, err := gsps.Write(state)
	if err != nil {
		t.Fatal(err)
	}
	pr.Dataset.Put(textElement(keyTag(0x0020, 0x0060), core.VRCS, "R"))
	ref := sr.ImageReference{StudyInstanceUID: study, SeriesInstanceUID: series, SOPClassUID: class, SOPInstanceUID: instance}
	kos := sr.NewKeyObjectSelection("1.2.826.0.1.3680043.10.999.500.5", []sr.ImageReference{ref}, nil)
	kos.SeriesInstanceUID = "1.2.826.0.1.3680043.10.999.500.6"
	kos.SeriesNumber = "3"
	kos.InstanceNumber = "1"
	kos.ContentDate = "20261009"
	kos.ContentTime = "120000"
	kos.Common = common
	ko, err := kos.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	doc := &sr.Document{SOPClassUID: sr.BasicTextSRStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.999.500.7", StudyInstanceUID: study, SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.500.8", SeriesNumber: "4", InstanceNumber: "1", ContentDate: "20261009", ContentTime: "120000", Common: common, Title: sr.CodedEntry{CodeValue: "18748-4", CodingSchemeDesignator: "LN", CodeMeaning: "Diagnostic imaging study"}, Content: []sr.ContentItem{{ValueType: sr.ValueText, RelationshipType: sr.RelationshipContains, ConceptName: sr.CodedEntry{CodeValue: "121071", CodingSchemeDesignator: "DCM", CodeMeaning: "Finding"}, Text: "Synthetic fixture"}}}
	srObj, err := doc.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	pdf, err := encapdoc.NewPDF(encapdoc.PDFOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.999.500.9", SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.500.10", SeriesNumber: "5", InstanceNumber: "1", ContentDate: "20261009", ContentTime: "120000", DocumentTitle: "Synthetic fixture", Data: []byte("%PDF-1.4\n%%EOF\n"), Common: common})
	if err != nil {
		t.Fatal(err)
	}
	frame, _ := ct.GetUID(keyTag(0x0020, 0x0052))
	structure, err := rtstruct.Write(&rtstruct.StructureSet{Common: common, SOPInstanceUID: "1.2.826.0.1.3680043.10.999.500.11", StudyInstanceUID: study, SeriesInstanceUID: "1.2.826.0.1.3680043.10.999.500.12", SeriesNumber: 6, FrameOfReferenceUID: frame, Label: "SYNTHETIC", Date: "20261009", Time: "120000", ROIs: []rtstruct.ROI{{Number: 1, Name: "SYNTHETIC", GenerationAlgorithm: "MANUAL", ColorRGB: [3]int{255, 0, 0}, Contours: []rtstruct.Contour{{GeometricType: rtstruct.ContourPoint, Points: []rtstruct.Point3D{{X: 1, Y: 2, Z: 3}}, ReferencedImages: []rtstruct.ReferencedImage{{StudyInstanceUID: study, SeriesInstanceUID: series, SOPClassUID: class, SOPInstanceUID: instance}}}}}}})
	if err != nil {
		t.Fatal(err)
	}
	structure.Dataset.Put(textElement(tagInstanceNumberFS, core.VRIS, "1"))
	fixtures := []struct {
		id      string
		dataset *object.Object
		typ     RecordType
	}{{"CT000001", ct, RecordTypeImage}, {"PR000001", pr.Dataset, RecordTypePresentation}, {"KO000001", ko, RecordTypeKeyObjectDocument}, {"SR000001", srObj, RecordTypeSRDocument}, {"PDF00001", pdf.Dataset, RecordTypeEncapsulatedDocument}, {"RTS00001", structure.Dataset, RecordTypeRTStructureSet}}
	set := testDirectorySet(t, root, Options{})
	for _, fixture := range fixtures {
		report, err := iod.Validate(fixture.dataset, iod.Options{CheckValues: true})
		if err != nil || len(report.Errors()) != 0 {
			t.Fatalf("%s IOD: %v %+v", fixture.id, err, report.Errors())
		}
		if _, err := validation.ValidateDataSet(context.Background(), core.DataSet{Elements: fixture.dataset.Elements()}, validation.Options{Mode: validation.ModeStrict, StopFirst: true}); err != nil {
			t.Fatalf("%s VR/VM: %v", fixture.id, err)
		}
		testDirectoryWriteSource(t, root, FileID{fixture.id}, fixture.dataset, transfer.ExplicitVRLittleEndian)
		if err := set.Add(context.Background(), FileID{fixture.id}); err != nil {
			t.Fatalf("%s Add: %v", fixture.id, err)
		}
	}
	report, err := CommitDICOMDIR(context.Background(), set, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if report.LogicalReferences != 6 {
		t.Fatalf("report %+v", report)
	}
	parsed, err := ParseFileSet(filepath.Join(root, "DICOMDIR"))
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Records()) != 6 {
		t.Fatal("records")
	}
	data, _ := os.ReadFile(filepath.Join(root, "DICOMDIR"))
	assertDirectoryByteOffsets(t, data, 6, 0)
}
