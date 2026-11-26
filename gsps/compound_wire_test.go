package gsps

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

func TestGSPSCompoundWireRoundTrip(t *testing.T) {
	state, axis, rectangle := compoundWireFixture()
	written, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	file := compoundWireReadBack(t, written)
	groups := derivedio.Sequence(file.Dataset, core.NewTag(0x0070, 0x0001))
	if len(groups) != 2 {
		t.Fatalf("annotation items = %d, want two independently linked compound and fallback items", len(groups))
	}
	text := derivedio.Sequence(groups[0], core.NewTag(0x0070, 0x0008))
	fallback := derivedio.Sequence(groups[0], core.NewTag(0x0070, 0x0009))
	first := derivedio.Sequence(groups[0], core.NewTag(0x0070, 0x0209))
	second := derivedio.Sequence(groups[1], core.NewTag(0x0070, 0x0209))
	if len(text) != 1 || len(fallback) != 1 || len(first) != 1 || len(second) != 1 {
		t.Fatalf("text, graphic, compound cardinalities: %d/%d/%d/%d", len(text), len(fallback), len(first), len(second))
	}
	assertCompoundWireVR(t, first[0], core.NewTag(0x0070, 0x0230), core.VRFD, 8)
	for _, item := range []*object.Object{text[0], fallback[0], first[0], second[0]} {
		assertCompoundWireVR(t, item, core.NewTag(0x0070, 0x0226), core.VRUL, 4)
		assertCompoundWireVR(t, item, core.NewTag(0x0070, 0x0295), core.VRUL, 4)
	}
	line := derivedio.Sequence(first[0], core.NewTag(0x0070, 0x0232))
	fill := derivedio.Sequence(second[0], core.NewTag(0x0070, 0x0233))
	if len(line) != 1 || len(fill) != 1 {
		t.Fatalf("style counts: line=%d fill=%d", len(line), len(fill))
	}
	assertCompoundWireVR(t, line[0], core.NewTag(0x0070, 0x0255), core.VRUL, 4)
	assertCompoundWireVR(t, fill[0], core.NewTag(0x0070, 0x0256), core.VROB, 128)

	result, err := Read(file.Dataset)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(result.GraphicGroups, state.GraphicGroups) || len(result.GraphicAnnotations) != 5 {
		t.Fatalf("graphic groups = %+v, annotations = %d", result.GraphicGroups, len(result.GraphicAnnotations))
	}
	if got := result.GraphicAnnotations[2].Compound; !reflect.DeepEqual(got, axis) {
		t.Fatalf("axis did not round-trip its exact FD angle, ticks and style: %+v", got)
	}
	if got := result.GraphicAnnotations[4].Compound; !reflect.DeepEqual(got, rectangle) {
		t.Fatalf("rectangle did not round-trip its fill and opacity fields: %+v", got)
	}
	for i, annotation := range result.GraphicAnnotations {
		originalScope := state.GraphicAnnotations[0].ReferencedImages[0]
		if len(annotation.ReferencedImages) != 1 ||
			annotation.ReferencedImages[0].SOPInstanceUID != originalScope.SOPInstanceUID ||
			!reflect.DeepEqual(annotation.ReferencedImages[0].Frames, originalScope.Frames) {
			t.Fatalf("annotation %d lost its SOP or frame binding: %+v", i, annotation.ReferencedImages)
		}
		if i < 3 {
			if annotation.GroupIndex != 1 || annotation.CompoundGraphicID != 0xf0080207 || annotation.GraphicGroupID != 31 {
				t.Fatalf("axis linkage lost: %+v", annotation)
			}
		} else if annotation.GroupIndex != 2 || annotation.CompoundGraphicID != 0 ||
			!annotation.HasCompoundGraphicID || !annotation.HasGraphicGroupID || annotation.GraphicGroupID != 0 {
			t.Fatalf("explicit zero linkage lost: %+v", annotation)
		}
	}
}

func TestGSPSCompoundMalformedWireRetainsAlternatives(t *testing.T) {
	state, _, _ := compoundWireFixture()
	for name, replacement := range map[string]core.Element{
		"wrong rotation VR": derivedio.FL(core.NewTag(0x0070, 0x0230), 0.5),
		"wrong dimensions":  derivedio.US(core.NewTag(0x0070, 0x0020), 3),
		"wrong point count": derivedio.US(core.NewTag(0x0070, 0x0021), 3),
		"malformed fill": derivedio.Seq(core.NewTag(0x0070, 0x0233), derivedio.DataSet(
			derivedio.CS(core.NewTag(0x0070, 0x0257), "STIPPELED"),
			derivedio.Raw(core.NewTag(0x0070, 0x0256), core.VROB, []byte{0xff, 0x00}),
		)),
	} {
		t.Run(name, func(t *testing.T) {
			written, err := Write(state)
			if err != nil {
				t.Fatal(err)
			}
			groups := derivedio.Sequence(written.Dataset, tagGraphicAnnotationSequence)
			compound := derivedio.Sequence(groups[0], tagCompoundGraphicSequence)[0]
			compound.Put(replacement)
			groups[0].Put(derivedio.Seq(tagCompoundGraphicSequence, compound.ToDataSet()))
			written.Dataset.Put(derivedio.Seq(tagGraphicAnnotationSequence, groups[0].ToDataSet(), groups[1].ToDataSet()))
			result, err := Read(compoundWireReadBack(t, written).Dataset)
			if err != nil {
				t.Fatal(err)
			}
			if len(result.GraphicAnnotations) != 5 {
				t.Fatalf("malformed compound removed other annotations: %+v", result.GraphicAnnotations)
			}
			text, line, invalid := result.GraphicAnnotations[0], result.GraphicAnnotations[1], result.GraphicAnnotations[2]
			if invalid.Compound == nil || validateCompoundGraphic(invalid.Compound) == nil ||
				text.Text != "Scale" || len(line.Polyline) != 2 ||
				text.CompoundGraphicID != invalid.CompoundGraphicID || line.CompoundGraphicID != invalid.CompoundGraphicID {
				t.Fatalf("malformed compound or its linked alternatives were lost: %+v", result.GraphicAnnotations[:3])
			}
			if _, err := Write(result); !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("rewriting a malformed compound: %v, want ErrInvalidObject", err)
			}
		})
	}
}

func TestGSPSCompoundWriteRefusesAmbiguousLinkage(t *testing.T) {
	for name, mutate := range map[string]func(*State){
		"duplicate compound ID": func(s *State) {
			s.GraphicAnnotations[1].CompoundGraphicID = s.GraphicAnnotations[0].CompoundGraphicID
		},
		"missing alternative": func(s *State) {
			s.GraphicAnnotations[2].CompoundGraphicID = 0
			s.GraphicAnnotations[2].HasCompoundGraphicID = false
			s.GraphicAnnotations[3].CompoundGraphicID = 0
			s.GraphicAnnotations[3].HasCompoundGraphicID = false
		},
		"alternative on another frame": func(s *State) {
			s.GraphicAnnotations[2].ReferencedImages = cloneReferences(s.GraphicAnnotations[2].ReferencedImages)
			s.GraphicAnnotations[2].ReferencedImages[0].Frames = []int{1}
		},
		"alternative in another annotation item": func(s *State) {
			s.GraphicAnnotations[3].GroupIndex = 10
		},
		"alternative on another layer": func(s *State) {
			s.GraphicAnnotations[3].LayerName = "OTHER"
		},
		"alternative names another graphic group": func(s *State) {
			s.GraphicAnnotations[3].GraphicGroupID = 0
			s.GraphicAnnotations[3].HasGraphicGroupID = true
		},
		"alternative has unknown link": func(s *State) {
			s.GraphicAnnotations[3].CompoundGraphicID = 57
		},
		"undefined graphic group": func(s *State) {
			s.GraphicGroups = nil
		},
	} {
		t.Run(name, func(t *testing.T) {
			state, _, _ := compoundWireFixture()
			mutate(state)
			file, err := Write(state)
			if file != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("Write = %v, %v, want ErrInvalidObject with no file", file, err)
			}
		})
	}
}

func TestGSPSCompoundPrivateTypeRetainsItsWireAlternative(t *testing.T) {
	state, axis, _ := compoundWireFixture()
	axis.Type = "PRIVATE_WEDGE"
	written, err := Write(state)
	if err != nil {
		t.Fatal(err)
	}
	result, err := Read(compoundWireReadBack(t, written).Dataset)
	if err != nil {
		t.Fatal(err)
	}
	text, line, compound := result.GraphicAnnotations[0], result.GraphicAnnotations[1], result.GraphicAnnotations[2]
	if compound.Compound.Type != axis.Type || standardCompoundType(compound.Compound.Type) ||
		text.CompoundGraphicID != compound.CompoundGraphicID || line.CompoundGraphicID != compound.CompoundGraphicID {
		t.Fatalf("private Compound Graphic or its alternatives lost on wire: %+v", result.GraphicAnnotations[:3])
	}
}

func compoundWireFixture() (*State, *CompoundGraphic, *CompoundGraphic) {
	ref := ReferencedImage{
		SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.40.7",
		SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
		SOPInstanceUID:    "1.2.826.0.1.3680043.9.7433.40.8",
		Frames:            []int{2},
	}
	foreground := &[3]uint16{45000, 50000, 20000}
	background := &[3]uint16{10000, 32896, 32896}
	axis := &CompoundGraphic{
		Type: CompoundTypeAxis, Units: AnnotationUnitsDisplay,
		Points:        []Point2D{{X: 0.25, Y: 0.5}, {X: 0.75, Y: 0.5}},
		RotationAngle: compoundWirePtr(47.12345678901234), RotationPoint: &Point2D{X: 0.5, Y: 0.5},
		TickAlignment: "TOP", TickLabelAlignment: "BOTTOM", ShowTickLabel: compoundWirePtr(true),
		MajorTicks: []CompoundTick{{Position: 0, Label: "0"}, {Position: 0.5, Label: "25"}, {Position: 1, Label: "50"}},
		LineStyle: &CompoundLineStyle{
			OnColor: foreground, OffColor: background, OnOpacity: compoundWirePtr(0.75),
			OffOpacity: compoundWirePtr(0.0), Thickness: compoundWirePtr(0.03125),
			Dashing: "DASHED", Pattern: compoundWirePtr(uint32(0x0f1f03ff)),
			Shadow: "NORMAL", ShadowColor: background, ShadowOffsetX: compoundWirePtr(-0.015625),
			ShadowOffsetY: compoundWirePtr(0.03125), ShadowOpacity: compoundWirePtr(0.5),
		},
		TextStyle: &CompoundTextStyle{
			FontName: "Helvetica", FontNameType: "ISO_32000", CSSFontName: "sans-serif",
			Color: foreground, HorizontalAlignment: "LEFT", VerticalAlignment: "BOTTOM",
			Shadow: "OUTLINED", ShadowColor: background, ShadowOffsetX: compoundWirePtr(0.015625),
			ShadowOffsetY: compoundWirePtr(0.0), ShadowOpacity: compoundWirePtr(0.25),
			Underlined: compoundWirePtr(false), Bold: compoundWirePtr(true), Italic: compoundWirePtr(false),
		},
	}
	rectangle := &CompoundGraphic{
		Type: CompoundTypeRectangle, Units: AnnotationUnitsPixel, Filled: compoundWirePtr(true),
		Points: []Point2D{{X: 8, Y: 8}, {X: 12, Y: 12}},
		FillStyle: &CompoundFillStyle{
			Mode: "STIPPELED", Pattern: bytes.Repeat([]byte{0xaa, 0x03}, 64),
			OnColor: foreground, OffColor: background, OnOpacity: compoundWirePtr(1.0), OffOpacity: compoundWirePtr(0.25),
		},
	}
	axisAnnotation := GraphicAnnotation{
		LayerName: "MEASUREMENT", CompoundGraphicID: 0xf0080207,
		GraphicGroupID: 31, ReferencedImages: []ReferencedImage{ref},
	}
	rectangleAnnotation := GraphicAnnotation{
		LayerName: "MEASUREMENT", HasCompoundGraphicID: true,
		HasGraphicGroupID: true, ReferencedImages: []ReferencedImage{ref},
	}
	axisFallbackText, axisFallbackLine := axisAnnotation, axisAnnotation
	axisFallbackText.Text = "Scale"
	axisFallbackText.Anchor = Point2D{X: 8, Y: 8}
	axisFallbackLine.Polyline = []Point2D{{X: 8, Y: 16}, {X: 24, Y: 16}}
	rectangleFallback := rectangleAnnotation
	rectangleFallback.Polyline = []Point2D{{X: 8, Y: 8}, {X: 12, Y: 8}, {X: 12, Y: 12}, {X: 8, Y: 12}, {X: 8, Y: 8}}
	axisAnnotation.Compound = axis
	rectangleAnnotation.Compound = rectangle
	state := &State{
		SOPInstanceUID:    "1.2.826.0.1.3680043.9.7433.40.1",
		StudyInstanceUID:  "1.2.826.0.1.3680043.9.7433.40.2",
		SeriesInstanceUID: "1.2.826.0.1.3680043.9.7433.40.3",
		ReferencedImages:  []ReferencedImage{ref},
		DisplayedArea:     DisplayedArea{TopLeftX: 1, TopLeftY: 1, BottomRightX: 32, BottomRightY: 32},
		GraphicGroups:     []GraphicGroup{{ID: 31, Label: "Axis", Description: "Measurement scale"}, {ID: 0, Label: "Region"}},
		GraphicAnnotations: []GraphicAnnotation{axisAnnotation, rectangleAnnotation,
			axisFallbackText, axisFallbackLine, rectangleFallback},
	}
	return state, axis, rectangle
}

func compoundWirePtr[T any](value T) *T { return &value }

func compoundWireReadBack(t *testing.T, file *object.File) *object.File {
	t.Helper()
	var wire bytes.Buffer
	if err := object.WriteFile(&wire, file); err != nil {
		t.Fatal(err)
	}
	read, err := object.ReadFile(bytes.NewReader(wire.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	return read
}

func assertCompoundWireVR(t *testing.T, obj *object.Object, tag core.Tag, vr core.VR, length core.Length) {
	t.Helper()
	element, ok := obj.Get(tag)
	if !ok || element.VR() != vr || element.Length() != length {
		t.Fatalf("wire attribute %s = %s length %d present %t, want %s length %d", tag, element.VR(), element.Length(), ok, vr, length)
	}
}
