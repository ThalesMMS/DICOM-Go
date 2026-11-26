package sr

import (
	"bytes"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DCMTK's tsrdoc.cc exercises patient and study fields under an explicit
// Specific Character Set. Here the source object's metadata supplies those
// fields and the authored SR adds further text in the same wire repertoire.
func TestDCMTKReferenceSRPatientAndContentRepertoire(t *testing.T) {
	charsetTag := core.NewTag(0x0008, 0x0005)
	nameTag := core.NewTag(0x0010, 0x0010)
	for _, tc := range []struct {
		name, charset, patient string
		patientBytes           []byte
		finding                string
	}{
		{"default-ASCII", "", "Phantom^Alpha", []byte("Phantom^Alpha"), "Synthetic note"},
		{"Latin-1", "ISO_IR 100", "Phantom^José", []byte("Phantom^Jos\xe9"), "Café technique"},
		{"UTF-8", "ISO_IR 192", "Phantom^画像", []byte("Phantom^画像"), "Imagem sintética – 画像"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fields := []struct {
				tag  core.Tag
				vr   core.VR
				want string
			}{
				{core.NewTag(0x0010, 0x0020), core.VRLO, "SYN-001"},
				{core.NewTag(0x0010, 0x0021), core.VRLO, "Synthetic issuer"},
				{core.NewTag(0x0010, 0x0030), core.VRDA, "20010203"},
				{core.NewTag(0x0010, 0x0040), core.VRCS, "F"},
				{core.NewTag(0x0010, 0x1010), core.VRAS, "025Y"},
				{core.NewTag(0x0010, 0x1020), core.VRDS, "1.71"},
				{core.NewTag(0x0010, 0x1030), core.VRDS, "72.4"},
				{core.NewTag(0x0008, 0x1030), core.VRLO, "Synthetic study"},
			}
			source := sourceImage()
			if tc.charset != "" {
				source.Put(strElem(charsetTag, core.VRCS, tc.charset))
			}
			source.Put(core.NewRawElement(nameTag, core.VRPN, tc.patientBytes))
			for _, field := range fields {
				source.Put(strElem(field.tag, field.vr, field.want))
			}
			concept := CodedEntry{"121106", "DCM", "Comment"}
			doc := &Document{
				SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.3.91", Title: concept,
				ContentDate: "20261010", ContentTime: "120000",
				Common:  object.DerivedCommon{Source: source, Now: time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)},
				Content: []ContentItem{{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: concept, Text: tc.finding}},
			}
			dataset := mustDocumentDataset(t, doc)
			// The built report owns the inherited content after the source changes.
			source.Put(strElem(nameTag, core.VRPN, "Replaced^Source"))
			var encoded bytes.Buffer
			file := &object.File{Dataset: dataset, TransferSyntax: transfer.ExplicitVRLittleEndian}
			if err := object.WriteFile(&encoded, file); err != nil {
				t.Fatal(err)
			}
			decoded, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = decoded.Close() })
			if charset, present := decoded.Dataset.GetString(charsetTag); charset != tc.charset || present != (tc.charset != "") {
				t.Fatalf("Specific Character Set = %q (present %v), want %q", charset, present, tc.charset)
			}
			if raw, ok := decoded.Dataset.GetRaw(nameTag); !ok || !bytes.Equal(bytes.TrimRight(raw, " "), tc.patientBytes) {
				t.Fatalf("patient name wire bytes = % x, want % x plus optional padding", raw, tc.patientBytes)
			}
			if got, ok := decoded.Dataset.GetString(nameTag); !ok || got != tc.patient {
				t.Fatalf("patient name = %q, want %q", got, tc.patient)
			}
			for _, field := range fields {
				element, present := decoded.Dataset.Get(field.tag)
				if got, ok := decoded.Dataset.GetString(field.tag); !present || element.VR() != field.vr || !ok || got != field.want {
					t.Errorf("%v = %q with VR %s, want %q with VR %s", field.tag, got, element.VR(), field.want, field.vr)
				}
			}
			report, err := ReadDocument(decoded.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if report.StudyInstanceUID != testStudy || len(report.Content) != 1 || report.Content[0].Text != tc.finding {
				t.Fatalf("decoded SR study/content: %+v", report)
			}
		})
	}
}

// Follow dcmsr_changeDocumentType_2 and dcmsr_extractDocSubTree_2 with the
// same kind of internal edge: a CODE item inferred from a sibling NUM. Go
// callers explicitly rewrite the destination ordinal after moving a branch.
func TestDCMTKReferenceSRSubtreeRebasing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		wrap     bool
		sopClass string
		syntax   transfer.Syntax
	}{
		{"sibling-prefix", false, ComprehensiveSRStorage, transfer.ExplicitVRLittleEndian},
		{"container-prefix", true, Comprehensive3DSRStorage, transfer.ExplicitVRBigEndian},
	} {
		t.Run(tc.name, func(t *testing.T) {
			concept := CodedEntry{"121111", "DCM", "Summary"}
			const relationship = RelationshipInferredFrom
			oldTarget := ContentItemIdentifier{1, 1, 1}
			oldSlot := ContentItemIdentifier{1, 1, 2, 1}
			branch := ContentItem{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: concept, Text: "Synthetic summary",
				Children: []ContentItem{
					{ValueType: ValueNum, RelationshipType: RelationshipHasProperties, ConceptName: CodedEntry{"121206", "DCM", "Distance"},
						Measurement: &Measurement{Value: 17.25, Units: MillimeterUnit()}},
					{ValueType: ValueCode, RelationshipType: RelationshipHasProperties, ConceptName: concept, Code: concept,
						Children: []ContentItem{{RelationshipType: relationship, ReferencedContentItemIdentifier: oldTarget.Clone()}}},
				}}
			prefix := ContentItem{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: concept, Text: "Prefix"}
			doc := &Document{SOPClassUID: tc.sopClass, SOPInstanceUID: "1.2.3.92", Title: concept,
				ContentDate: "20261010", ContentTime: "120000", Content: []ContentItem{branch}}
			oldIndex, _, err := ResolveReferences(doc, DefaultReferenceOptions())
			if err != nil {
				t.Fatal(err)
			}
			if path, err := oldIndex.TargetFor(oldSlot); err != nil || !slices.Equal(path, oldTarget) {
				t.Fatalf("original target = %v, %v, want %v", path, err, oldTarget)
			}
			var parent ContentItemIdentifier
			if tc.wrap {
				doc.Content = []ContentItem{{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept,
					Children: []ContentItem{prefix, branch}}}
				parent = ContentItemIdentifier{1, 1, 2}
			} else {
				doc.Content = []ContentItem{prefix, branch}
				parent = ContentItemIdentifier{1, 2}
			}
			if _, err := oldIndex.Edges(); !errors.Is(err, ErrStaleReferenceIndex) {
				t.Fatalf("old index after move = %v", err)
			}
			source := append(parent.Clone(), 2)
			slot := append(source.Clone(), 1)
			target := append(parent.Clone(), 1)
			var referenceErr *ReferenceError
			if tc.wrap {
				// With a container prefix the old ordinal now identifies a valid,
				// different item. A freshly built index follows the encoded path.
				index, _, err := ResolveReferences(doc, DefaultReferenceOptions())
				if err != nil {
					t.Fatal(err)
				}
				if got, err := index.TargetFor(slot); err != nil || !slices.Equal(got, oldTarget) {
					t.Fatalf("target before explicit rebasing = %v, %v, want prefix at %v", got, err, oldTarget)
				}
			} else {
				if dataset, err := doc.Dataset(); dataset != nil || !errors.As(err, &referenceErr) || referenceErr.Code != ReferenceCodeDangling ||
					!slices.Equal(referenceErr.Slot, slot) || !slices.Equal(referenceErr.Target, oldTarget) {
					t.Fatalf("write with old path = %v, want dangling target %v", err, oldTarget)
				}
			}
			branch.Children[1].Children[0].ReferencedContentItemIdentifier = target
			// The same numerical content is valid in both Comprehensive profiles,
			// but Enhanced forbids transmitting the edge by reference.
			doc.SOPClassUID = EnhancedSRStorage
			if dataset, err := doc.Dataset(); dataset != nil || !errors.As(err, &referenceErr) || referenceErr.Code != ReferenceCodeForbiddenProfile {
				t.Fatalf("Enhanced write with reference = %v", err)
			}
			doc.SOPClassUID = tc.sopClass
			var encoded bytes.Buffer
			if err := object.WriteDataSet(&encoded, mustDocumentDataset(t, doc), tc.syntax); err != nil {
				t.Fatal(err)
			}
			decoded, err := object.ReadDataSet(bytes.NewReader(encoded.Bytes()), tc.syntax)
			if err != nil {
				t.Fatal(err)
			}
			result, err := ReadDocumentWithOptions(decoded, ReadOptions{ResolveReferences: true, ReferenceMode: ValidationModeStrict,
				ValidateRelationships: true, RelationshipMode: ValidationModeStrict})
			if err != nil || len(result.Report.Findings) != 0 {
				t.Fatalf("rebased SR read: %v, %+v", err, result.Report.Findings)
			}
			incoming, err := result.References.ReferencesTo(target)
			if err != nil || len(incoming) != 1 || !slices.Equal(incoming[0].Source, source) ||
				!slices.Equal(incoming[0].Slot, slot) || incoming[0].RelationshipType != relationship {
				t.Fatalf("incoming edge for %v = %+v, %v", target, incoming, err)
			}
			items := result.Document.Content
			if tc.wrap {
				if len(items) != 1 || items[0].ValueType != ValueContainer {
					t.Fatalf("container branch root: %+v", items)
				}
				items = items[0].Children
			}
			if len(items) != 2 || items[0].Text != prefix.Text || items[1].Text != branch.Text || len(items[1].Children) != 2 {
				t.Fatalf("sibling/content order: %+v", items)
			}
			if got := items[1].Children[0].Measurement; got == nil || *got != *branch.Children[0].Measurement {
				t.Fatalf("target measurement = %+v", got)
			}
		})
	}
}
