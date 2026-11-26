package sr

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

func ilStrings(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

func ilSource(modality string, extra ...core.Element) *object.Object {
	elements := []core.Element{ilStrings(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"), ilStrings(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.121"), ilStrings(core.NewTag(0x0008, 0x0060), core.VRCS, modality)}
	return object.FromElements(append(elements, extra...), nil)
}
func ilDescriptor(t *testing.T, e ImageLibraryExtraction, concept string) ContentItem {
	t.Helper()
	for _, d := range e.Entry.Descriptors {
		if d.ConceptName.CodeValue == concept {
			return d
		}
	}
	t.Fatalf("missing concept %s", concept)
	return ContentItem{}
}
func TestImageLibraryExtractionSyntheticModalities(t *testing.T) {
	cases := []struct {
		name, modality string
		extra          []core.Element
		concept, unit  string
		value          float64
	}{
		{"ct-spacing", "CT", []core.Element{ilStrings(core.NewTag(0x0028, 0x0030), core.VRDS, "0.7", "0.8"), ilStrings(core.NewTag(0x0018, 0x9302), core.VRCS, "SPIRAL")}, "111026", "mm", .8},
		{"projection-spacing", "DX", []core.Element{ilStrings(core.NewTag(0x0018, 0x1164), core.VRDS, "0.3", "0.4")}, "111066", "mm", .3},
		{"mr-field", "MR", []core.Element{ilStrings(core.NewTag(0x0018, 0x0087), core.VRDS, "3"), ilStrings(core.NewTag(0x0018, 0x9005), core.VRSH, "SYNTHETIC")}, "130542", "T", 3},
		{"pet-dose", "PT", []core.Element{seqElement(core.NewTag(0x0054, 0x0016), core.DataSet{Elements: []core.Element{ilStrings(core.NewTag(0x0018, 0x1074), core.VRDS, "12000"), ilStrings(core.NewTag(0x0018, 0x1078), core.VRDT, "20261010110000")}})}, "123006", "Bq", 12000},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			source := ilSource(c.modality, c.extra...)
			// Report evidence requires explicit study/series identity; the extractor
			// must copy these source facts rather than inventing them.
			source.Put(ilStrings(core.NewTag(0x0020, 0x000d), core.VRUI, "1.2.3.4"))
			source.Put(ilStrings(core.NewTag(0x0020, 0x000e), core.VRUI, "1.2.3.5"))
			before := source.Elements()
			got, err := ExtractImageLibraryEntry(context.Background(), source, []int{1}, ImageLibraryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if got.Entry.Image.StudyInstanceUID != "1.2.3.4" || got.Entry.Image.SeriesInstanceUID != "1.2.3.5" {
				t.Fatal("explicit source evidence identity changed")
			}
			d := ilDescriptor(t, got, c.concept)
			if d.Measurement == nil || d.Measurement.Value != c.value || d.Measurement.Units.CodeValue != c.unit {
				t.Fatal("value/unit mismatch", d)
			}
			if len(got.Findings) == 0 {
				t.Fatal("absence not reported")
			}
			if !reflect.DeepEqual(before, source.Elements()) {
				t.Fatal("source mutated")
			}
			for _, desc := range got.Entry.Descriptors {
				if desc.ConceptName.CodeValue == "14749-6" {
					t.Fatal("glucose inferred")
				}
			}
			m, n, err := got.MetadataAt("1.1.1")
			if err != nil {
				t.Fatal(err)
			}
			r := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{got.Entry}})
			r.ImageLibraryMetadata, err = BindImageLibraryMetadata(nil, r.ImageLibraryGroups, m, n, ImageLibraryOptions{})
			if err != nil {
				t.Fatal(err)
			}
			if len(ilRoundTrip(t, r).ImageLibraryGroups) != 1 {
				t.Fatal("extraction disappeared")
			}
		})
	}
}
func TestImageLibraryExtractionAbsenceConflictAndLimits(t *testing.T) {
	source := ilSource("CT")
	got, err := ExtractImageLibraryEntry(context.Background(), source, nil, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Entry.Descriptors) != 1 || len(got.Findings) < 10 {
		t.Fatal("missing source became invented descriptors")
	}
	shared := seqElement(core.NewTag(0x5200, 0x9229), core.DataSet{Elements: []core.Element{seqElement(core.NewTag(0x0028, 0x9110), core.DataSet{Elements: []core.Element{ilStrings(core.NewTag(0x0018, 0x0050), core.VRDS, "2")}})}})
	source = ilSource("CT", ilStrings(core.NewTag(0x0018, 0x0050), core.VRDS, "1"), shared)
	_, err = ExtractImageLibraryEntry(context.Background(), source, []int{1}, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "source-conflict" {
		t.Fatal("conflict ignored", err)
	}
	source = ilSource("CT", ilStrings(core.NewTag(0x0028, 0x0030), core.VRDS, "0.2"))
	if _, err = ExtractImageLibraryEntry(context.Background(), source, nil, ImageLibraryOptions{}); err == nil {
		t.Fatal("invalid VM accepted")
	}
	if _, err = ExtractImageLibraryEntry(context.Background(), ilSource("CT"), nil, ImageLibraryOptions{MaxValueBytes: 1}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("bytes ignored", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err = ExtractImageLibraryEntry(ctx, ilSource("CT"), nil, ImageLibraryOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancel ignored", err)
	}
}
func TestImageLibraryExtractionCodeContext(t *testing.T) {
	source := ilSource("CT", seqElement(core.NewTag(0x0008, 0x2218), core.DataSet{Elements: []core.Element{ilStrings(tagCodeValue, core.VRSH, "123037004"), ilStrings(tagCodingScheme, core.VRSH, "SCT"), ilStrings(tagCodeMeaning, core.VRLO, "Synthetic region"), ilStrings(tagCodingSchemeVersion, core.VRSH, "synthetic-v1")}}))
	source.Put(ilStrings(core.NewTag(0x0020, 0x000d), core.VRUI, "1.2.3.4"))
	source.Put(ilStrings(core.NewTag(0x0020, 0x000e), core.VRUI, "1.2.3.5"))
	got, err := ExtractImageLibraryEntry(context.Background(), source, nil, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if got.Entry.Image.StudyInstanceUID != "1.2.3.4" || got.Entry.Image.SeriesInstanceUID != "1.2.3.5" {
		t.Fatal("explicit source evidence identity changed")
	}
	if len(got.Metadata.CodeContexts) != 1 || got.Metadata.CodeContexts[0].Value.CodingSchemeVersion != "synthetic-v1" {
		t.Fatal("code version flattened")
	}
	m, n, err := got.MetadataAt("1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	report := ilReport(ImageLibraryGroup{Entries: []ImageLibraryEntry{got.Entry}})
	report.ImageLibraryMetadata, err = BindImageLibraryMetadata(nil, report.ImageLibraryGroups, m, n, ImageLibraryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	round := ilRoundTrip(t, report)
	for _, c := range round.ImageLibraryMetadata.Metadata().CodeContexts {
		if c.Value.CodingSchemeVersion == "synthetic-v1" {
			return
		}
	}
	t.Fatal("code context lost in report")
}

type ilPanickingValue struct{}

func (ilPanickingValue) Kind() core.ValueKind               { panic("custom Kind invoked") }
func (ilPanickingValue) EncodedLength() (core.Length, bool) { panic("custom Length invoked") }
func TestImageLibraryExtractionResidentPreflight(t *testing.T) {
	element := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0018, 0x0050), VR: core.VRDS, Length: 1, LengthSet: true}, Value: core.RawValue(make([]byte, 4096))}
	if _, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", element), nil, ImageLibraryOptions{MaxValueBytes: 128}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("declared length hid actual bytes", err)
	}
	element.Value = ilPanickingValue{}
	_, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", element), nil, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "nonresident-or-custom-value" {
		t.Fatal("custom getter path", err)
	}
	empty := ImageLibraryExtraction{Metadata: TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: ""}}}}
	if _, _, err := empty.MetadataAt("1.1.1"); err == nil {
		t.Fatal("invalid detached metadata path accepted")
	}
	if _, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", ilStrings(core.NewTag(0x0028, 0x0008), core.VRIS, "1")), []int{2}, ImageLibraryOptions{}); err == nil {
		t.Fatal("frame outside explicit count accepted")
	}
}

func TestImageLibraryNestedSequencePreflight(t *testing.T) {
	poison := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7777, 0x2001), VR: core.VROB, Length: 1, LengthSet: true}, Value: core.RawValue(make([]byte, 4096))}
	shared := seqElement(core.NewTag(0x5200, 0x9229), core.DataSet{Elements: []core.Element{poison}})
	if _, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", shared), nil, ImageLibraryOptions{MaxValueBytes: 128}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("nested bytes reached facade allocation", err)
	}
	poison.Header.Tag = core.NewTag(0x0008, 0x0005)
	poison.Header.VR = core.VRCS
	poison.Value = ilPanickingValue{}
	shared = seqElement(core.NewTag(0x5200, 0x9229), core.DataSet{Elements: []core.Element{poison}})
	_, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", shared), nil, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "nonresident-or-custom-value" {
		t.Fatal("nested charset getter invoked", err)
	}
}

func TestImageLibraryObjectCountBeforeElementsCopy(t *testing.T) {
	elements := make([]core.Element, 70)
	for i := range elements {
		elements[i] = core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7777, uint16(i+1)), VR: core.VRLO}, Value: core.StringValue{""}}
	}
	source := object.FromElements(elements, nil)
	if err := preflightLibraryObject(context.Background(), source, ImageLibraryOptions{MaxItems: 1, MaxDepth: 1, MaxValueBytes: 1024}); !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatal("object attribute count not admitted before copy", err)
	}
}

func TestImageLibraryMalformedSequenceConcreteType(t *testing.T) {
	bad := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x5200, 0x9229), VR: core.VRSQ}, Value: core.StringValue{"not a sequence"}}
	_, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", bad), nil, ImageLibraryOptions{})
	var typed *ImageLibraryError
	if !errors.As(err, &typed) || typed.Code != "source-sequence" {
		t.Fatal("malformed SQ not refused", err)
	}
}
func TestImageLibraryCodePreflightBeforeFacade(t *testing.T) {
	for _, custom := range []bool{false, true} {
		name := "bytes"
		if custom {
			name = "charset"
		}
		t.Run(name, func(t *testing.T) {
			poison := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x7777, 0x2001), VR: core.VROB, Length: 1, LengthSet: true}, Value: core.RawValue(make([]byte, 4096))}
			opts := ImageLibraryOptions{MaxValueBytes: 128}
			if custom {
				poison.Header.Tag = core.NewTag(0x0008, 0x0005)
				poison.Header.VR = core.VRCS
				poison.Value = ilPanickingValue{}
				opts = ImageLibraryOptions{}
			}
			region := seqElement(core.NewTag(0x0008, 0x2218), core.DataSet{Elements: []core.Element{poison}})
			_, err := ExtractImageLibraryEntry(context.Background(), ilSource("CT", region), nil, opts)
			if !custom {
				if !errors.Is(err, ErrResourceLimitExceeded) {
					t.Fatal("code bytes reached getter", err)
				}
			} else {
				var typed *ImageLibraryError
				if !errors.As(err, &typed) || typed.Code != "nonresident-or-custom-value" {
					t.Fatal("code charset reached getter", err)
				}
			}
		})
	}
}
