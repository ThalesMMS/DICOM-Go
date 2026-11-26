package sr

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func lifecycleDocument() *Document {
	return &Document{
		SOPClassUID:       ComprehensiveSRStorage,
		SOPInstanceUID:    "1.2.826.0.1.3680043.8.498.20260926.40",
		StudyInstanceUID:  "1.2.826.0.1.3680043.8.498.20260926.41",
		SeriesInstanceUID: "1.2.826.0.1.3680043.8.498.20260926.42",
		Title:             CodedEntry{CodeValue: "18748-4", CodingSchemeDesignator: "LN", CodeMeaning: "Diagnostic imaging report"},
		ContentDate:       "20260926",
		ContentTime:       "120000",
		Content: []ContentItem{{
			RelationshipType: RelationshipContains, ValueType: ValueText,
			ConceptName: CodedEntry{CodeValue: "121071", CodingSchemeDesignator: "DCM", CodeMeaning: "Finding"}, Text: "No abnormality",
		}},
	}
}

func roundTripDocument(t *testing.T, doc *Document) *Document {
	t.Helper()
	dataset, err := doc.Dataset()
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadDocument(dataset)
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func TestDocumentLifecycle(t *testing.T) {
	doc := lifecycleDocument()
	observer := VerifyingObserver{Name: "Doe^Jane", Organization: "Synthetic Hospital", DateTime: "20260926130000",
		Code: CodedEntry{CodeValue: "12345", CodingSchemeDesignator: "99LOCAL", CodeMeaning: "Staff ID"}}
	if err := doc.Verify(observer); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("verified a partial document: %v", err)
	}
	if err := doc.Complete("Final report"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Complete(""); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("completed twice: %v", err)
	}
	if err := doc.Verify(VerifyingObserver{Name: "Doe^Jane"}); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("verified without an organization: %v", err)
	}
	if err := doc.Verify(observer); err != nil {
		t.Fatal(err)
	}
	second := VerifyingObserver{Name: "Roe^Richard", Organization: "Synthetic Hospital"}
	if err := doc.Verify(second); err != nil {
		t.Fatal(err)
	}
	if doc.VerificationFlag != verificationVerified || len(doc.VerifyingObservers) != 2 || doc.VerifyingObservers[1].DateTime == "" {
		t.Fatalf("verification: %+v", doc)
	}
	read := roundTripDocument(t, doc)
	if read.CompletionFlag != completionComplete || read.CompletionFlagDescription != "Final report" || read.VerificationFlag != verificationVerified ||
		!reflect.DeepEqual(read.VerifyingObservers, doc.VerifyingObservers) {
		t.Fatalf("verified document read back: %+v", read)
	}

	original := doc.SOPInstanceUID
	if err := doc.Revise(""); err != nil {
		t.Fatal(err)
	}
	if doc.SOPInstanceUID == original || doc.CompletionFlag != completionPartial || doc.VerificationFlag != verificationUnverified ||
		doc.VerifyingObservers != nil || doc.CompletionFlagDescription != "" || doc.ContentDate != "" {
		t.Fatalf("revision: %+v", doc)
	}
	want := []DocumentReference{{StudyInstanceUID: doc.StudyInstanceUID, SeriesInstanceUID: doc.SeriesInstanceUID, SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: original}}
	if !reflect.DeepEqual(doc.PredecessorDocuments, want) {
		t.Fatalf("predecessors: %+v", doc.PredecessorDocuments)
	}
	if err := doc.Revise(""); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("revised a partial document: %v", err)
	}
	if read := roundTripDocument(t, doc); !reflect.DeepEqual(read.PredecessorDocuments, want) || read.VerifyingObservers != nil {
		t.Fatalf("revision read back: %+v", read)
	}

	kos := &Document{SOPClassUID: KeyObjectSelectionDocumentStorage}
	if err := kos.Complete(""); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("completed a KOS: %v", err)
	}
	if err := (&Document{SOPClassUID: ComprehensiveSRStorage}).Complete(strings.Repeat("x", 65)); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("accepted a long description: %v", err)
	}
	unidentified := lifecycleDocument()
	unidentified.StudyInstanceUID = ""
	_ = unidentified.Complete("")
	if err := unidentified.Revise(""); !errors.Is(err, ErrDocumentLifecycle) {
		t.Fatalf("revised a document without its study: %v", err)
	}
	if _, err := (&Document{SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.3", CompletionFlag: "COMPLETE", VerificationFlag: "VERIFIED"}).Dataset(); !errors.Is(err, ErrInvalidDocument) {
		t.Fatalf("wrote VERIFIED without an observer: %v", err)
	}
}

func TestMeasurementReportLifecycleRoundTrips(t *testing.T) {
	report := withEvidence(dcmtkOracleReport())
	report.CompletionFlag, report.CompletionFlagDescription, report.VerificationFlag = completionComplete, "Signed", verificationVerified
	report.VerifyingObservers = []VerifyingObserver{{Name: "Doe^Jane", Organization: "Synthetic Hospital", DateTime: "20260926130000"}}
	report.PredecessorDocuments = []DocumentReference{{StudyInstanceUID: "1.2.3.1", SeriesInstanceUID: "1.2.3.2", SOPClassUID: EnhancedSRStorage, SOPInstanceUID: "1.2.3.3"}}
	file, err := WriteMeasurementReport(report)
	if err != nil {
		t.Fatal(err)
	}
	read, err := ReadMeasurementReport(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if read.CompletionFlagDescription != "Signed" || !reflect.DeepEqual(read.VerifyingObservers, report.VerifyingObservers) ||
		!reflect.DeepEqual(read.PredecessorDocuments, report.PredecessorDocuments) {
		t.Fatalf("lifecycle read back: %+v", read)
	}
}

// TestDocumentLifecyclePassesDSRDump writes a verified and a revised
// document and checks them with dsrdump. Enable with
// DICOMGO_DCMTK_INTEGRATION=1 and dsrdump on PATH.
func TestDocumentLifecyclePassesDSRDump(t *testing.T) {
	dsrdump := dcmtkoracle.Tool(t, "dsrdump")
	doc := lifecycleDocument()
	if err := doc.Complete("Final report"); err != nil {
		t.Fatal(err)
	}
	if err := doc.Verify(VerifyingObserver{Name: "Doe^Jane", Organization: "Synthetic Hospital", DateTime: "20260926130000"}); err != nil {
		t.Fatal(err)
	}
	dump := func(doc *Document) string {
		dataset, err := doc.Dataset()
		if err != nil {
			t.Fatal(err)
		}
		file := &object.File{Dataset: dataset, Meta: object.New(nil), TransferSyntax: transfer.ExplicitVRLittleEndian}
		if err := file.RebuildFileMeta(); err != nil {
			t.Fatal(err)
		}
		var encoded bytes.Buffer
		if err := object.WriteFile(&encoded, file); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "sr.dcm")
		if err := os.WriteFile(path, encoded.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		out := dcmtkoracle.Run(t, dcmtkoracle.Accepted, dsrdump, "+Pf", path)
		if strings.Contains(string(out), "W: ") || strings.Contains(string(out), "E: ") {
			t.Fatalf("dsrdump:\n%s", out)
		}
		return string(out)
	}
	if out := dump(doc); !strings.Contains(out, "VERIFIED") || !strings.Contains(out, "Doe^Jane") {
		t.Fatalf("verified document:\n%s", out)
	}
	if err := doc.Revise(""); err != nil {
		t.Fatal(err)
	}
	if out := dump(doc); !strings.Contains(out, "PARTIAL") {
		t.Fatalf("revised document:\n%s", out)
	}
}
