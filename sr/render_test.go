package sr

import (
	"bytes"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var updateRenderGolden = flag.Bool("update-render", false, "rewrite the SR render golden files")

// renderFixture is a synthetic Comprehensive 3D SR with every modeled value
// type, a by-reference relationship and hostile text.
func renderFixture() *Document {
	code := func(value, scheme, meaning string) CodedEntry {
		return CodedEntry{CodeValue: value, CodingSchemeDesignator: scheme, CodeMeaning: meaning}
	}
	return &Document{
		SOPClassUID:      Comprehensive3DSRStorage,
		SOPInstanceUID:   "1.2.826.0.1.3680043.8.498.20260926.30",
		Title:            code("126000", "DCM", "Imaging Measurement Report"),
		CompletionFlag:   "COMPLETE",
		VerificationFlag: "UNVERIFIED",
		ContentDate:      "20260926",
		ContentTime:      "120000",
		Content: []ContentItem{
			{RelationshipType: RelationshipHasObsContext, ValueType: ValuePName, ConceptName: code("121008", "DCM", "Person Observer Name"), PersonName: "Doe^Jane"},
			{RelationshipType: RelationshipContains, ValueType: ValueContainer, ConceptName: code("125007", "DCM", "Measurement Group"), Children: []ContentItem{
				{RelationshipType: RelationshipContains, ValueType: ValueText, ConceptName: code("121106", "DCM", "Comment"),
					Text: "<script>alert(1)</script> & \"quoted\"\nsecond line \x1b[31mred‮"},
				{RelationshipType: RelationshipContains, ValueType: ValueCode, ConceptName: code("121071", "DCM", "Finding"), Code: code("52988006", "SCT", "Lesion <b>")},
				{RelationshipType: RelationshipContains, ValueType: ValueNum, ConceptName: code("81827009", "SCT", "Diameter"),
					Measurement: &Measurement{Value: 12.5, Units: code("mm", "UCUM", "millimeter")}, Children: []ContentItem{
						{RelationshipType: RelationshipInferredFrom, ValueType: ValueImage, ConceptName: code("121112", "DCM", "Source of Measurement"),
							Image: ImageReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.2", SOPInstanceUID: "1.2.3.4", Frames: []int{2, 3}}},
						{RelationshipType: RelationshipInferredFrom, ValueType: ValueSCoord, Spatial: SpatialCoordinates{GraphicType: "POLYLINE", Coordinates: []Point2D{{1, 2}, {3.5, 4}}}},
					}},
				{RelationshipType: RelationshipContains, ValueType: ValueNum, ConceptName: code("118565006", "SCT", "Volume"),
					NumericValueQualifier: code("114006", "DCM", "Measurement failure")},
				{RelationshipType: RelationshipContains, ValueType: ValueSCoord3D, ConceptName: code("111030", "DCM", "Image Region"),
					Spatial3D: SpatialReference{FrameOfReferenceUID: "1.2.3.5", GraphicType: "POINT", Coordinates: []Point3D{{1, 2, 3}}}},
				{RelationshipType: RelationshipContains, ValueType: ValueDateTime, ConceptName: code("111526", "DCM", "DateTime Started"), DateTime: "20260926123045"},
				{RelationshipType: RelationshipContains, ValueType: ValueDate, ConceptName: code("111060", "DCM", "Study Date"), Date: "20260926"},
				{RelationshipType: RelationshipContains, ValueType: ValueTime, ConceptName: code("111061", "DCM", "Study Time"), Time: "0930"},
				{RelationshipType: RelationshipContains, ValueType: ValueUIDRef, ConceptName: code("112040", "DCM", "Tracking Unique Identifier"), UID: "1.2.3.6"},
				{RelationshipType: RelationshipContains, ValueType: ValueComposite, ConceptName: code("121191", "DCM", "Referenced Segment"),
					Composite: CompositeReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.66.4", SOPInstanceUID: "1.2.3.7"}},
				{RelationshipType: RelationshipContains, ValueType: ValueWaveform, ConceptName: code("109070", "DCM", "ECG"),
					Waveform: WaveformReference{CompositeReference: CompositeReference{SOPClassUID: "1.2.840.10008.5.1.4.1.1.9.1.1", SOPInstanceUID: "1.2.3.8"}, Channels: []WaveformChannel{{1, 1}, {1, 2}}}},
				{RelationshipType: RelationshipContains, ValueType: ValueTCoord, ConceptName: code("121213", "DCM", "Waveform Region"),
					Temporal: TemporalCoordinates{RangeType: "SEGMENT", TimeOffsets: []float64{0.5, 1.25}}},
				{RelationshipType: RelationshipInferredFrom, ReferencedContentItemIdentifier: ContentItemIdentifier{1, 2, 1}},
			}},
		},
	}
}

func TestRenderMatchesGoldenFiles(t *testing.T) {
	opts := RenderOptions{Header: []RenderField{{"Patient", "Doe^Jane <Synthetic>"}}, ShowCodes: true}
	for _, tc := range []struct {
		name   string
		render func(*bytes.Buffer) error
	}{
		{"document.txt", func(b *bytes.Buffer) error { return RenderText(b, renderFixture(), opts) }},
		{"document.html", func(b *bytes.Buffer) error { return RenderHTML(b, renderFixture(), opts) }},
	} {
		var got bytes.Buffer
		if err := tc.render(&got); err != nil {
			t.Fatal(err)
		}
		path := filepath.Join("testdata", "render", tc.name)
		if *updateRenderGolden {
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, got.Bytes(), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("%v (run with -update-render)", err)
		}
		if !bytes.Equal(got.Bytes(), bytes.ReplaceAll(want, []byte("\r\n"), []byte("\n"))) {
			t.Errorf("%s differs from the golden file:\n%s", tc.name, got.String())
		}
	}
}

// TestRenderHTMLCannotBeInjected checks that no content reaches the page as
// markup: the only tags are the renderer's own, and hostile text appears
// escaped.
func TestRenderHTMLCannotBeInjected(t *testing.T) {
	var page bytes.Buffer
	opts := RenderOptions{Header: []RenderField{{"<img src=x onerror=alert(1)>", "\"><script>x</script>"}}, ShowCodes: true}
	if err := RenderHTML(&page, renderFixture(), opts); err != nil {
		t.Fatal(err)
	}
	out := page.String()
	allowed := map[string]bool{"!DOCTYPE": true, "html": true, "head": true, "meta": true, "title": true, "style": true, "body": true,
		"h1": true, "table": true, "tr": true, "td": true, "ul": true, "li": true, "span": true, "a": true, "p": true}
	for i := 0; i < len(out); i++ {
		if out[i] != '<' {
			continue
		}
		name := strings.TrimPrefix(out[i+1:], "/")
		end := strings.IndexAny(name, " >")
		if end < 0 || !allowed[name[:end]] {
			t.Fatalf("unexpected markup at %d: %.40q", i, out[i:])
		}
	}
	for _, escaped := range []string{"&lt;script&gt;alert(1)&lt;/script&gt;", "&amp;", "&#34;quoted&#34;", "&lt;img src=x onerror=alert(1)&gt;", "Lesion &lt;b&gt;"} {
		if !strings.Contains(out, escaped) {
			t.Errorf("missing escaped %q", escaped)
		}
	}
	if strings.Contains(out, "\x1b") || strings.Contains(out, "‮") {
		t.Error("control characters reached the page")
	}
	if !strings.Contains(out, `<a href="#item-1.2.1">`) || !strings.Contains(out, `id="item-1.2.1"`) {
		t.Error("the by-reference relationship is not linked to its target")
	}
}

func TestRenderTextEscapesControlCharacters(t *testing.T) {
	var text bytes.Buffer
	if err := RenderText(&text, renderFixture(), RenderOptions{}); err != nil {
		t.Fatal(err)
	}
	out := text.String()
	if strings.Contains(out, "\x1b") || strings.Contains(out, "‮") || !strings.Contains(out, `\x1b[31mred\x202e`) {
		t.Fatalf("control characters: %q", out)
	}
	if _, err := RenderItems(nil, RenderOptions{}); !errors.Is(err, ErrNilDocument) {
		t.Fatalf("nil document: %v", err)
	}
}

func TestRenderLimits(t *testing.T) {
	rendered, err := RenderItems(renderFixture(), RenderOptions{MaxItems: 3, MaxPoints: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(rendered.Items) != 3 || rendered.Truncated != 14 {
		t.Fatalf("%d items, %d truncated", len(rendered.Items), rendered.Truncated)
	}
	all, _ := RenderItems(renderFixture(), RenderOptions{MaxPoints: 1})
	for _, item := range all.Items {
		if item.ValueType == ValueSCoord && item.Value != "POLYLINE (1,2) ... (2 points)" {
			t.Fatalf("points: %q", item.Value)
		}
	}
}
