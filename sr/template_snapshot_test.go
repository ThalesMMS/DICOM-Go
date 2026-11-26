package sr

import (
	"bytes"
	"context"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
)

func snapshotFixture() (*Document, TemplateValidationMetadata) {
	concept := CodedEntry{CodeValue: "COMMENT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Same concept"}
	doc := &Document{SOPClassUID: ComprehensiveSRStorage, ContentDate: "20261009", ContentTime: "120000",
		Title: CodedEntry{CodeValue: "ROOT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Root"},
		Content: []ContentItem{
			{ValueType: ValueContainer, RelationshipType: RelationshipContains, ConceptName: concept, Children: []ContentItem{
				{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: concept, Text: "first"},
				{ValueType: ValueText, RelationshipType: RelationshipContains, ConceptName: concept, Text: "second"},
			}},
		},
	}
	metadata := TemplateValidationMetadata{
		CodeContexts: []ContentItemCodeContexts{
			{Path: "1.1.1", ConceptName: CodeContext{CodingSchemeVersion: "FIRST"}, ConceptNameEquivalentCodes: []EquivalentCodeEntry{{Code: CodedEntry{CodeValue: "ALT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Alternate"}}}},
			{Path: "1.1.2", ConceptName: CodeContext{CodingSchemeVersion: "SECOND"}},
		},
		TemplateIdentifications: []TemplateIdentification{{Path: "1", MappingResource: "99TEST", Identifier: "ROOT"}, {Path: "1.1", MappingResource: "99TEST", Identifier: "CHILD"}},
	}
	return doc, metadata
}

func mustSnapshot(t *testing.T, doc *Document, metadata TemplateValidationMetadata) *TemplateMetadataSnapshot {
	t.Helper()
	s, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func requireSnapshotCode(t *testing.T, err error, code string) {
	t.Helper()
	var typed *MetadataSnapshotError
	if !errors.Is(err, ErrMetadataSnapshot) || !errors.As(err, &typed) || typed.Code != code {
		t.Fatalf("error = %v, want typed snapshot code %s", err, code)
	}
}

func TestMetadataSnapshotRoundTripAndDefensiveCopies(t *testing.T) {
	doc, metadata := snapshotFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	metadata.CodeContexts[0].ConceptName.CodingSchemeVersion = "MUTATED"
	metadata.CodeContexts[0].ConceptNameEquivalentCodes[0].Code.CodeValue = "MUTATED"
	metadata.TemplateIdentifications[0].Identifier = "MUTATED"
	copy := snapshot.Metadata()
	copy.CodeContexts[0].ConceptNameEquivalentCodes[0].Code.CodeValue = "ALSO_MUTATED"
	if snapshot.Metadata().CodeContexts[0].ConceptName.CodingSchemeVersion != "FIRST" || snapshot.Metadata().CodeContexts[0].ConceptNameEquivalentCodes[0].Code.CodeValue != "ALT" {
		t.Fatal("snapshot metadata aliases caller storage")
	}
	dataset, err := doc.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	read, rebound, err := ReadDocumentWithMetadataSnapshot(dataset, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(read.TemplateMetadata, snapshot.Metadata()) {
		t.Fatal("enhanced/template metadata changed")
	}
	if err := rebound.Check(read.Document); err != nil {
		t.Fatal(err)
	}
	if _, err := read.Document.DatasetWithMetadataSnapshot(rebound); err != nil {
		t.Fatal(err)
	}
	// The explicit legacy writer still retains its documented caller ownership.
	if _, err := doc.DatasetWithTemplateMetadata(metadata); err != nil {
		t.Fatal(err)
	}
}

func TestMetadataSnapshotRejectsStructuralMutationBeforeConsumers(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Document)
	}{
		{"same-concept-type-swap", func(d *Document) { c := d.Content[0].Children; c[0], c[1] = c[1], c[0] }},
		{"nested-insert", func(d *Document) {
			d.Content[0].Children = append(d.Content[0].Children, ContentItem{ValueType: ValueText, Text: "third"})
		}},
		{"nested-remove", func(d *Document) { d.Content[0].Children = d.Content[0].Children[1:] }},
		{"replace-backing-storage", func(d *Document) { d.Content[0].Children = append([]ContentItem(nil), d.Content[0].Children...) }},
		{"root-insert", func(d *Document) { d.Content = append(d.Content, ContentItem{ValueType: ValueText, Text: "extra"}) }},
		{"title-change", func(d *Document) { d.Title.CodeMeaning = "SYN_SECRET_1425" }},
		{"sop-class-change", func(d *Document) { d.SOPClassUID = Comprehensive3DSRStorage }},
		{"root-continuity-change", func(d *Document) { d.ContinuityOfContent = "CONTINUOUS" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, metadata := snapshotFixture()
			snapshot := mustSnapshot(t, doc, metadata)
			tc.mutate(doc)
			err := snapshot.Check(doc)
			requireSnapshotCode(t, err, MetadataSnapshotStale)
			if strings.Contains(err.Error(), "SYN_SECRET") {
				t.Fatal("diagnostic disclosed content")
			}
			if elements, err := doc.ElementsWithMetadataSnapshot(snapshot); elements != nil || err == nil {
				t.Fatal("stale elements returned")
			}
			if dataset, err := doc.DatasetWithMetadataSnapshot(snapshot); dataset != nil || err == nil {
				t.Fatal("stale dataset returned")
			}
			var validator *TemplateValidator
			if report, err := validator.ValidateWithMetadataSnapshot(context.Background(), doc, TemplateKey{}, snapshot); err == nil || len(report.Findings) != 0 {
				t.Fatal("stale validator ran")
			}
			for _, opts := range []RenderOptions{{}, {ShowCodes: true, MaxItems: 1}} {
				var text, html bytes.Buffer
				if err := RenderTextWithMetadataSnapshot(&text, doc, snapshot, opts); err == nil || text.Len() != 0 {
					t.Fatal("stale text published")
				}
				if err := RenderHTMLWithMetadataSnapshot(&html, doc, snapshot, opts); err == nil || html.Len() != 0 {
					t.Fatal("stale HTML published")
				}
			}
			// Reassociation remains an explicit caller decision, not a heuristic.
			if tc.name == "same-concept-type-swap" {
				metadata.CodeContexts[0].Path, metadata.CodeContexts[1].Path = metadata.CodeContexts[1].Path, metadata.CodeContexts[0].Path
				rebound := mustSnapshot(t, doc, metadata)
				out, err := RenderItemsWithMetadataSnapshot(doc, rebound, RenderOptions{ShowCodes: true})
				if err != nil || !strings.Contains(out.Items[1].Concept, "SECOND") || !strings.Contains(out.Items[2].Concept, "FIRST") {
					t.Fatal("explicit rebind assigned wrong item")
				}
			}
		})
	}
}

func TestMetadataSnapshotRejectsAmbiguousAssociation(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(map[bool]string{false: "siblings", true: "subtrees"}[nested], func(t *testing.T) {
			a, ac := NewCode("long-code-identifier-A", "99TEST", "Same")
			b, bc := NewCode("long-code-identifier-B", "99TEST", "Same")
			doc := &Document{Title: CodedEntry{CodeValue: "R", CodingSchemeDesignator: "99TEST", CodeMeaning: "Root"}, Content: []ContentItem{
				{ValueType: ValueCode, RelationshipType: RelationshipContains, Code: a}, {ValueType: ValueCode, RelationshipType: RelationshipContains, Code: b},
			}}
			metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1.1", Value: ac}, {Path: "1.2", Value: bc}}}
			if nested {
				doc.Content = []ContentItem{{ValueType: ValueContainer, Children: []ContentItem{{ValueType: ValueCode, Code: a}}}, {ValueType: ValueContainer, Children: []ContentItem{{ValueType: ValueCode, Code: b}}}}
				metadata.CodeContexts[0].Path, metadata.CodeContexts[1].Path = "1.1.1", "1.2.1"
			}
			_, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{})
			requireSnapshotCode(t, err, MetadataSnapshotAmbiguous)
			if !nested {
				doc.SOPClassUID, doc.ContentDate, doc.ContentTime = ComprehensiveSRStorage, "20261009", "120000"
				dataset, err := doc.DatasetWithTemplateMetadata(metadata)
				if err != nil {
					t.Fatal(err)
				}
				read, rejected, err := ReadDocumentWithMetadataSnapshot(dataset, ReadOptions{}, MetadataSnapshotOptions{})
				requireSnapshotCode(t, err, MetadataSnapshotAmbiguous)
				if read.Document == nil || rejected != nil || len(read.TemplateMetadata.CodeContexts) != 2 {
					t.Fatal("binding failure discarded preserved read result")
				}
			}
			metadata.CodeContexts[1].Value = ac
			snapshot := mustSnapshot(t, doc, metadata)
			doc.Content[0], doc.Content[1] = doc.Content[1], doc.Content[0]
			if nested {
				// Identical subtree moves still change child backing identities.
				requireSnapshotCode(t, snapshot.Check(doc), MetadataSnapshotStale)
			} else if err := snapshot.Check(doc); err != nil {
				t.Fatal("association-equivalent identical values rejected")
			}
		})
	}
}

func TestMetadataSnapshotPayloadCoverage(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ContentItem)
	}{
		{"text", func(i *ContentItem) { i.Text = "changed" }},
		{"code", func(i *ContentItem) { i.Code.CodeMeaning = "changed" }},
		{"measurement-presence", func(i *ContentItem) { i.Measurement = &Measurement{Value: 1} }},
		{"qualifier", func(i *ContentItem) { i.NumericValueQualifier.CodeValue = "Q" }},
		{"datetime", func(i *ContentItem) { i.DateTime = "202610091200" }},
		{"date", func(i *ContentItem) { i.Date = "20261010" }},
		{"time", func(i *ContentItem) { i.Time = "130000" }},
		{"uid", func(i *ContentItem) { i.UID = "1.2.3" }},
		{"person-name", func(i *ContentItem) { i.PersonName = "Synthetic^Name" }},
		{"image-frame", func(i *ContentItem) { i.Image.Frames = []int{3} }},
		{"image-hierarchy", func(i *ContentItem) { i.Image.SeriesInstanceUID = "1.2.4" }},
		{"composite", func(i *ContentItem) { i.Composite.SOPInstanceUID = "1.2.5" }},
		{"waveform-channel", func(i *ContentItem) { i.Waveform.Channels = []WaveformChannel{{1, 2}} }},
		{"spatial-point", func(i *ContentItem) { i.Spatial.Coordinates = []Point2D{{2, 3}} }},
		{"spatial-fiducial", func(i *ContentItem) { i.Spatial.FiducialUID = "1.2.6" }},
		{"spatial3d", func(i *ContentItem) { i.Spatial3D.Coordinates = []Point3D{{2, 3, 4}} }},
		{"temporal-position", func(i *ContentItem) { i.Temporal.SamplePositions = []uint32{2} }},
		{"temporal-offset", func(i *ContentItem) { i.Temporal.TimeOffsets = []float64{0.25} }},
		{"temporal-datetime", func(i *ContentItem) { i.Temporal.DateTimes = []string{"20261009"} }},
		{"reference", func(i *ContentItem) { i.ReferencedContentItemIdentifier = ContentItemIdentifier{1, 1} }},
		{"encoding-provenance", func(i *ContentItem) { i.Encoding.EmptyReferenceMacroPresent = true }},
		{"continuity", func(i *ContentItem) { i.ContinuityOfContent = "SEPARATE" }},
		{"raw-payload", func(i *ContentItem) {
			i.ValueElements = []core.Element{core.NewRawElement(core.NewTag(0x7777, 0x10), core.VROB, []byte{2})}
		}},
		{"nested-payload", func(i *ContentItem) {
			i.ValueElements = []core.Element{{Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{Value: core.Float64Value{0.25}}}}}}}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc, metadata := snapshotFixture()
			snapshot := mustSnapshot(t, doc, metadata)
			tc.mutate(&doc.Content[0].Children[0])
			requireSnapshotCode(t, snapshot.Check(doc), MetadataSnapshotStale)
		})
	}
	// Mutable nested typed values are guarded by value, including signed zero.
	doc, metadata := snapshotFixture()
	doc.Content[0].Children[0].ValueElements = []core.Element{{Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{{Value: core.Uint16Value{1, 2}}}}}}}}
	snapshot := mustSnapshot(t, doc, metadata)
	doc.Content[0].Children[0].ValueElements[0].Value.(core.SequenceValue).Items[0].Elements[0].Value.(core.Uint16Value)[1] = 3
	requireSnapshotCode(t, snapshot.Check(doc), MetadataSnapshotStale)
}

type snapshotCustomValue struct{ called *bool }

func (v snapshotCustomValue) Kind() core.ValueKind {
	*v.called = true
	panic("custom callback must not run")
}
func (v snapshotCustomValue) EncodedLength() (core.Length, bool) {
	*v.called = true
	panic("custom callback must not run")
}

func TestMetadataSnapshotBoundsUnsupportedAndNil(t *testing.T) {
	for _, options := range []MetadataSnapshotOptions{{MaxDepth: -1}, {MaxDepth: 65}, {MaxItems: -1}, {MaxValueBytes: -1}, {MaxDepth: 1}, {MaxItems: 1}, {MaxValueBytes: 1}} {
		doc, metadata := snapshotFixture()
		_, err := BindTemplateMetadata(doc, metadata, options)
		requireSnapshotCode(t, err, MetadataSnapshotLimit)
		if !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatal("limit error not detectable")
		}
	}
	doc, metadata := snapshotFixture()
	called := false
	doc.Content[0].Children[0].ValueElements = []core.Element{{Value: snapshotCustomValue{called: &called}}}
	_, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{})
	requireSnapshotCode(t, err, MetadataSnapshotUnsupported)
	if called {
		t.Fatal("custom value callback invoked")
	}
	doc, metadata = snapshotFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	doc.Content[0].Children[0].ValueElements = []core.Element{{Value: snapshotCustomValue{called: &called}}}
	var out bytes.Buffer
	requireSnapshotCode(t, RenderHTMLWithMetadataSnapshot(&out, doc, snapshot, RenderOptions{}), MetadataSnapshotUnsupported)
	if out.Len() != 0 || called {
		t.Fatal("unsupported data caused output or I/O callback")
	}
	var nilSnapshot *TemplateMetadataSnapshot
	requireSnapshotCode(t, nilSnapshot.Check(doc), MetadataSnapshotStale)
	if _, err := BindTemplateMetadata(nil, metadata, MetadataSnapshotOptions{}); !errors.Is(err, ErrNilDocument) {
		t.Fatal(err)
	}
	if _, _, err := ReadDocumentWithMetadataSnapshot(nil, ReadOptions{}, MetadataSnapshotOptions{}); !errors.Is(err, ErrNoDataset) {
		t.Fatal(err)
	}
	// Cyclic content is bounded by depth, not followed without limit.
	doc, _ = snapshotFixture()
	doc.Content[0].Children = doc.Content
	_, err = BindTemplateMetadata(doc, TemplateValidationMetadata{}, MetadataSnapshotOptions{})
	requireSnapshotCode(t, err, MetadataSnapshotLimit)
}

func TestMetadataSnapshotAllowsHeaderChangesAndConcurrentReaders(t *testing.T) {
	doc, metadata := snapshotFixture()
	snapshot := mustSnapshot(t, doc, metadata)
	doc.SOPInstanceUID, doc.ContentDate, doc.VerificationFlag = "1.2.3.44", "20261010", "UNVERIFIED"
	doc.CompletionFlagDescription = "Synthetic header change"
	if err := snapshot.Check(doc); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if err := snapshot.Check(doc); err != nil {
					t.Error(err)
					return
				}
				if _, err := RenderItemsWithMetadataSnapshot(doc, snapshot, RenderOptions{ShowCodes: true}); err != nil {
					t.Error(err)
					return
				}
				if _, err := doc.DatasetWithMetadataSnapshot(snapshot); err != nil {
					t.Error(err)
					return
				}
				copy := snapshot.Metadata()
				copy.CodeContexts[0].ConceptNameEquivalentCodes[0].Code.CodeMeaning = "changed copy"
			}
		}()
	}
	wg.Wait()
}

func TestMetadataSnapshotTemplateValidationUsesBoundMetadata(t *testing.T) {
	key := TemplateKey{MappingResource: "99TEST", Identifier: "BOUND", Version: "20261009"}
	registry, err := NewTemplateRegistry([]TemplateDefinition{{Key: key, Rows: []TemplateRow{{
		RuleID: "text", ValueTypes: []ValueType{ValueText},
		ConceptNames: []CodeKey{{CodeValue: "COMMENT", CodingSchemeDesignator: "99TEST", CodingSchemeVersion: "1"}},
		Cardinality:  Cardinality{Min: 1, Max: 1}, Requirement: RequirementRequired,
	}}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	doc := &Document{Title: CodedEntry{CodeValue: "ROOT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Root"}, Content: []ContentItem{{
		ValueType: ValueText, RelationshipType: RelationshipContains,
		ConceptName: CodedEntry{CodeValue: "COMMENT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Comment"}, Text: "synthetic",
	}}}
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1.1", ConceptName: CodeContext{CodingSchemeVersion: "1"}}},
		TemplateIdentifications: []TemplateIdentification{{Path: "1", MappingResource: "99TEST", Identifier: "BOUND", Version: "20261009"}}}
	snapshot := mustSnapshot(t, doc, metadata)
	for _, mode := range []ValidationMode{ValidationModeStrict, ValidationModeWarn} {
		validator, err := NewTemplateValidator(registry, TemplateValidationOptions{Mode: mode})
		if err != nil {
			t.Fatal(err)
		}
		report, err := validator.ValidateWithMetadataSnapshot(context.Background(), doc, key, snapshot)
		if err != nil || len(report.Findings) != 0 {
			t.Fatalf("bound version metadata ignored: %v %+v", err, report)
		}
		doc.Content[0].Text = "changed"
		_, err = validator.ValidateWithMetadataSnapshot(context.Background(), doc, key, snapshot)
		requireSnapshotCode(t, err, MetadataSnapshotStale)
		doc.Content[0].Text = "synthetic"
	}
}

func TestMetadataSnapshotNativeFloatBits(t *testing.T) {
	for _, tc := range []struct {
		name             string
		initial, changed uint32
	}{
		{"signaling-to-quiet", 0x7f800001, 0x7fc00001},
		{"nan-payload", 0x7fc00001, 0x7fc00002},
		{"signed-zero", 0, 0x80000000},
		{"infinity-sign", 0x7f800000, 0xff800000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, metadata := snapshotFixture()
			resident := core.Float32Value{math.Float32frombits(tc.initial)}
			doc.Content[0].Children[0].ValueElements = []core.Element{{Value: resident}}
			snapshot := mustSnapshot(t, doc, metadata)
			resident[0] = math.Float32frombits(tc.changed)
			requireSnapshotCode(t, snapshot.Check(doc), MetadataSnapshotStale)
		})
	}
	for _, tc := range []struct {
		name             string
		initial, changed uint64
	}{
		{"signaling-to-quiet-fd", 0x7ff0000000000001, 0x7ff8000000000001},
		{"nan-payload-fd", 0x7ff8000000000001, 0x7ff8000000000002},
		{"signed-zero-fd", 0, 0x8000000000000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc, metadata := snapshotFixture()
			resident := core.Float64Value{math.Float64frombits(tc.initial)}
			doc.Content[0].Children[0].ValueElements = []core.Element{{Value: resident}}
			snapshot := mustSnapshot(t, doc, metadata)
			resident[0] = math.Float64frombits(tc.changed)
			requireSnapshotCode(t, snapshot.Check(doc), MetadataSnapshotStale)
		})
	}
}

func TestMetadataSnapshotPreservedSequenceDepthBoundary(t *testing.T) {
	makePayload := func(depth int) []core.Element {
		elements := []core.Element{{Value: core.Uint32Value{42}}}
		for n := 0; n < depth; n++ {
			elements = []core.Element{{Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}}}
		}
		return elements
	}
	doc, metadata := snapshotFixture()
	doc.Content[0].Children[0].ValueElements = makePayload(64)
	snapshot := mustSnapshot(t, doc, metadata)
	if err := snapshot.Check(doc); err != nil {
		t.Fatal(err)
	}
	doc.Content[0].Children[0].ValueElements = makePayload(65)
	_, err := BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{})
	requireSnapshotCode(t, err, MetadataSnapshotLimit)
	// A cyclic preserved sequence is also stopped without invoking callbacks.
	doc, metadata = snapshotFixture()
	elements := make([]core.Element, 1)
	elements[0].Value = core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}
	doc.Content[0].Children[0].ValueElements = elements
	_, err = BindTemplateMetadata(doc, metadata, MetadataSnapshotOptions{})
	requireSnapshotCode(t, err, MetadataSnapshotLimit)
}
