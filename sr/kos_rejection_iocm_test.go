package sr

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
)

// Assembled directly from PS3.3 attributes and PS3.16 codes, independently of
// the factory and generic Document writer, so reciprocal bugs cannot hide.
func kosIndependentFixture(title string, quality bool) *object.Object {
	text := func(t core.Tag, vr core.VR, s string) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: t, VR: vr}, Value: core.StringValue{s}}
	}
	sq := func(t core.Tag, items ...core.DataSet) core.Element {
		return core.Element{Header: core.ElementHeader{Tag: t, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
	}
	code := func(tag core.Tag, value, meaning string) core.Element {
		return sq(tag, core.DataSet{Elements: []core.Element{
			text(core.NewTag(0x0008, 0x0100), core.VRSH, value), text(core.NewTag(0x0008, 0x0102), core.VRSH, "DCM"), text(core.NewTag(0x0008, 0x0104), core.VRLO, meaning),
		}})
	}
	pair := func() core.DataSet {
		return core.DataSet{Elements: []core.Element{text(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"), text(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.3.22")}}
	}
	content := []core.DataSet{{Elements: []core.Element{text(core.NewTag(0x0040, 0xa010), core.VRCS, "CONTAINS"), text(core.NewTag(0x0040, 0xa040), core.VRCS, "IMAGE"), sq(core.NewTag(0x0008, 0x1199), pair())}}}
	if quality {
		content = append([]core.DataSet{{Elements: []core.Element{text(core.NewTag(0x0040, 0xa010), core.VRCS, "HAS CONCEPT MOD"), text(core.NewTag(0x0040, 0xa040), core.VRCS, "CODE"), code(core.NewTag(0x0040, 0xa043), "113011", "Document Title Modifier"), code(core.NewTag(0x0040, 0xa168), "111210", "Motion blur")}}}, content...)
	}
	o := object.FromElements([]core.Element{
		text(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.88.59"), text(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.3.990"),
		text(core.NewTag(0x0008, 0x0060), core.VRCS, "KO"), text(core.NewTag(0x0020, 0x000d), core.VRUI, kosTestStudy), text(core.NewTag(0x0020, 0x000e), core.VRUI, "1.2.3.991"),
		text(core.NewTag(0x0020, 0x0011), core.VRIS, "5"), text(core.NewTag(0x0020, 0x0013), core.VRIS, "1"),
		text(core.NewTag(0x0008, 0x0023), core.VRDA, "20261009"), text(core.NewTag(0x0008, 0x0033), core.VRTM, "200000"),
		text(core.NewTag(0x0040, 0xa040), core.VRCS, "CONTAINER"), text(core.NewTag(0x0040, 0xa050), core.VRCS, "SEPARATE"),
		code(core.NewTag(0x0040, 0xa043), title, "Localized title"), sq(core.NewTag(0x0008, 0x1111)),
		sq(core.NewTag(0x0040, 0xa730), content...),
		sq(core.NewTag(0x0040, 0xa375), core.DataSet{Elements: []core.Element{text(core.NewTag(0x0020, 0x000d), core.VRUI, kosTestStudy), sq(core.NewTag(0x0008, 0x1115), core.DataSet{Elements: []core.Element{text(core.NewTag(0x0020, 0x000e), core.VRUI, "1.2.3.21"), sq(core.NewTag(0x0008, 0x1199), pair())}})}}),
	}, nil)
	return o
}

func TestKOSRejectionIndependentFixtureAndScopedIOD(t *testing.T) {
	for i, code := range []string{"113001", "113037", "113038", "113039"} {
		o := kosIndependentFixture(code, i == 0)
		before := o.Elements()
		note, err := ReadKOSRejection(context.Background(), o, KOSRejectionLimits{})
		if err != nil {
			t.Fatal(err)
		}
		if note.Kind != KOSRejectionKind(i+1) || len(note.References) != 1 || note.Title.CodeMeaning != "Localized title" {
			t.Fatalf("recognition: %+v", note)
		}
		if !reflect.DeepEqual(before, o.Elements()) {
			t.Fatal("reader changed fixture")
		}
		// Fill only common modules for the scoped library IOD check, without
		// using a KOS factory to create the content/evidence being recognized.
		if err = (object.DerivedCommon{}).Apply(o, object.InheritWithoutFrameOfReference); err != nil {
			t.Fatal(err)
		}
		report, err := iod.Validate(o, iod.Options{})
		if err != nil || len(report.Errors()) != 0 {
			t.Fatalf("scoped IOD: %v %+v", err, report.Errors())
		}
	}
}

func kosFixtureSequence(o *object.Object, t core.Tag) core.SequenceValue {
	e, _ := o.Get(t)
	return e.Value.(core.SequenceValue)
}

func TestKOSRejectionReaderErrorsReturnZero(t *testing.T) {
	for name, edit := range map[string]func(*object.Object){
		"other SOP": func(o *object.Object) { o.Put(strElem(tagSOPClassUID, core.VRUI, ComprehensiveSRStorage)) },
		"of interest": func(o *object.Object) {
			v := kosFixtureSequence(o, tagConceptNameCodeSeq)
			v.Items[0].Elements[0] = strElem(tagCodeValue, core.VRSH, "113000")
		},
		"title VM": func(o *object.Object) {
			v := kosFixtureSequence(o, tagConceptNameCodeSeq)
			v.Items[0].Elements[0].Value = core.StringValue{"113037", "113037"}
		},
		"root VR":        func(o *object.Object) { o.Put(strElem(tagValueType, core.VRLO, "CONTAINER")) },
		"root payload":   func(o *object.Object) { o.Put(strElem(tagTextValue, core.VRUT, "unmodeled root value")) },
		"role collision": func(o *object.Object) { o.Put(strElem(tagSOPInstanceUID, core.VRUI, kosTestStudy)) },
		"partial time":   func(o *object.Object) { o.Put(strElem(tagContentTime, core.VRTM, "20")) },
		"bad date":       func(o *object.Object) { o.Put(strElem(tagContentDate, core.VRDA, "20260231")) },
		"empty evidence": func(o *object.Object) { o.Put(seqElement(tagCurrentRequestedProcedureEvidence)) },
		"different study": func(o *object.Object) {
			v := kosFixtureSequence(o, tagCurrentRequestedProcedureEvidence)
			v.Items[0].Elements[0] = strElem(tagStudyInstanceUID, core.VRUI, "1.2.3.9")
		},
		"class mismatch": func(o *object.Object) {
			v := kosFixtureSequence(o, tagContentSequence)
			p := v.Items[0].Elements[2].Value.(core.SequenceValue)
			p.Items[0].Elements[0] = strElem(tagRefSOPClassUID, core.VRUI, "1.2.840.10008.5.1.4.1.1.4")
		},
		"extra evidence": func(o *object.Object) {
			v := kosFixtureSequence(o, tagCurrentRequestedProcedureEvidence)
			s := v.Items[0].Elements[1].Value.(core.SequenceValue)
			p := s.Items[0].Elements[1].Value.(core.SequenceValue)
			p.Items = append(p.Items, core.DataSet{Elements: referencedSOPElements("1.2.840.10008.5.1.4.1.1.2", "1.2.3.23")})
			s.Items[0].Elements[1].Value = p
		},
		"frame selector": func(o *object.Object) {
			v := kosFixtureSequence(o, tagContentSequence)
			p := v.Items[0].Elements[2].Value.(core.SequenceValue)
			p.Items[0].Elements = append(p.Items[0].Elements, strElem(tagRefFrameNumber, core.VRIS, "1"))
		},
		"by reference": func(o *object.Object) {
			v := kosFixtureSequence(o, tagContentSequence)
			v.Items[0].Elements = append(v.Items[0].Elements, contentItemIdentifierElement(ContentItemIdentifier{1, 1}))
		},
		"unknown extension": func(o *object.Object) {
			v := kosFixtureSequence(o, tagContentSequence)
			v.Items[0].Elements = append(v.Items[0].Elements, strElem(core.NewTag(0x0011, 0x1001), core.VRLO, "ignored?"))
		},
		"duplicate item attr": func(o *object.Object) {
			v := kosFixtureSequence(o, tagContentSequence)
			v.Items[0].Elements = append(v.Items[0].Elements, v.Items[0].Elements[0])
		},
		"wrong template": func(o *object.Object) {
			o.Put(seqElement(tagContentTemplateSequence, core.DataSet{Elements: []core.Element{strElem(core.NewTag(0x0008, 0x0105), core.VRCS, "DCMR"), strElem(core.NewTag(0x0040, 0xdb00), core.VRCS, "1500")}}))
		},
	} {
		t.Run(name, func(t *testing.T) {
			o := kosIndependentFixture("113037", false)
			edit(o)
			before := o.Elements()
			got, err := ReadKOSRejection(context.Background(), o, KOSRejectionLimits{})
			if err == nil || !reflect.DeepEqual(got, KOSRejectionNote{}) {
				t.Fatalf("partial note: %+v %v", got, err)
			}
			if !reflect.DeepEqual(before, o.Elements()) {
				t.Fatal("source changed")
			}
		})
	}
	for _, o := range []*object.Object{nil, kosIndependentFixture("113001", false)} {
		if got, err := ReadKOSRejection(context.Background(), o, KOSRejectionLimits{}); err == nil || !reflect.DeepEqual(got, KOSRejectionNote{}) {
			t.Fatal("invalid accepted")
		}
	}
	if _, err := ReadKOSRejection(nil, kosIndependentFixture("113037", false), KOSRejectionLimits{}); err == nil {
		t.Fatal("nil context")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, err := ReadKOSRejection(ctx, kosIndependentFixture("113037", false), KOSRejectionLimits{})
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(got, KOSRejectionNote{}) {
		t.Fatal("cancel returned note")
	}
}

func TestKOSRejectionReaderBudgetsAndConflictingEvidence(t *testing.T) {
	for name, l := range map[string]KOSRejectionLimits{
		"elements": {MaxElements: 1}, "items": {MaxItems: 1}, "bytes": {MaxMetadataBytes: 8}, "depth": {MaxDepth: 1}, "negative": {MaxReferences: -1}, "raised": {MaxDescriptionBytes: 65537},
	} {
		t.Run(name, func(t *testing.T) {
			o := kosIndependentFixture("113037", false)
			before := o.Elements()
			note, err := ReadKOSRejection(context.Background(), o, l)
			if !errors.Is(err, ErrResourceLimitExceeded) || !reflect.DeepEqual(note, KOSRejectionNote{}) {
				t.Fatalf("budget: %+v %v", note, err)
			}
			if !reflect.DeepEqual(before, o.Elements()) {
				t.Fatal("source changed")
			}
		})
	}
	for _, differentClass := range []bool{false, true} {
		o := kosIndependentFixture("113037", false)
		v := kosFixtureSequence(o, tagCurrentRequestedProcedureEvidence)
		s := v.Items[0].Elements[1].Value.(core.SequenceValue)
		class := "1.2.840.10008.5.1.4.1.1.2"
		series := "1.2.3.24"
		if differentClass {
			class = "1.2.840.10008.5.1.4.1.1.4"
			series = "1.2.3.21"
		}
		s.Items = append(s.Items, core.DataSet{Elements: []core.Element{strElem(tagSeriesInstanceUID, core.VRUI, series), seqElement(tagRefSOPSequence, core.DataSet{Elements: referencedSOPElements(class, "1.2.3.22")})}})
		v.Items[0].Elements[1].Value = s
		note, err := ReadKOSRejection(context.Background(), o, KOSRejectionLimits{})
		if err == nil || !reflect.DeepEqual(note, KOSRejectionNote{}) {
			t.Fatalf("conflicting evidence: %+v %v", note, err)
		}
	}
	o := kosIndependentFixture("113037", false)
	v := kosFixtureSequence(o, tagCurrentRequestedProcedureEvidence)
	s := v.Items[0].Elements[1].Value.(core.SequenceValue)
	p := s.Items[0].Elements[1].Value.(core.SequenceValue)
	p.Items = append(p.Items, core.DataSet{Elements: referencedSOPElements("1.2.840.10008.5.1.4.1.1.2", "1.2.3.23")})
	s.Items[0].Elements[1].Value = p
	if note, err := ReadKOSRejection(context.Background(), o, KOSRejectionLimits{MaxReferences: 1}); !errors.Is(err, ErrResourceLimitExceeded) || !reflect.DeepEqual(note, KOSRejectionNote{}) {
		t.Fatalf("reference budget: %+v %v", note, err)
	}
}

type kosCountingReader struct {
	*bytes.Reader
	reads int
}

func (r *kosCountingReader) Read(p []byte) (int, error) { r.reads++; return r.Reader.Read(p) }
func (r *kosCountingReader) ReadAt(p []byte, offset int64) (int, error) {
	r.reads++
	return r.Reader.ReadAt(p, offset)
}

func kosDeferredObject(t *testing.T, o *object.Object, tags []core.Tag, pixels bool) (*object.Object, *kosCountingReader) {
	t.Helper()
	var wire bytes.Buffer
	if err := object.WriteDataSet(&wire, o, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	r := &kosCountingReader{Reader: bytes.NewReader(wire.Bytes())}
	opts := object.ReadFileOptions{InlineValueBytesThreshold: 1, DeferValueTags: tags, DeferPixelData: pixels}
	if !pixels {
		opts = object.ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VRLT}}}}
	}
	got, err := object.ReadDataSetWithOptions(r, transfer.ExplicitVRLittleEndian, opts)
	if err != nil {
		t.Fatal(err)
	}
	r.reads = 0
	return got, r
}

func TestKOSRejectionDeferredMetadataZeroReadsAndPixelsIgnored(t *testing.T) {
	o := kosTestOptions()
	name := core.NewTag(0x0010, 0x4000)
	o.Common.Source.Put(strElem(name, core.VRLT, "Synthetic patient comments"))
	src, r := kosDeferredObject(t, o.Common.Source, []core.Tag{name}, false)
	o.Common.Source = src
	if e, _ := src.Get(name); e.Value != nil {
		t.Fatal("fixture not deferred")
	}
	if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got != nil || err == nil || r.reads != 0 {
		t.Fatalf("factory deferred: %v reads=%d", err, r.reads)
	}
	fixture := kosIndependentFixture("113037", false)
	fixture.Put(strElem(name, core.VRLT, "Synthetic patient comments"))
	src, r = kosDeferredObject(t, fixture, []core.Tag{name}, false)
	if got, err := ReadKOSRejection(context.Background(), src, KOSRejectionLimits{}); err == nil || !reflect.DeepEqual(got, KOSRejectionNote{}) || r.reads != 0 {
		t.Fatalf("reader deferred: %v reads=%d", err, r.reads)
	}
	for _, factory := range []bool{false, true} {
		o := kosTestOptions()
		fixture := kosIndependentFixture("113037", false)
		if factory {
			fixture = o.Common.Source
		}
		fixture.Put(core.NewRawElement(core.TagPixelData, core.VROB, make([]byte, 64)))
		src, r := kosDeferredObject(t, fixture, []core.Tag{core.TagPixelData}, true)
		if e, _ := src.Get(core.TagPixelData); e.Value != nil {
			t.Fatal("pixels not deferred")
		}
		if factory {
			o.Common.Source = src
			if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got == nil || err != nil || got.Has(core.TagPixelData) {
				t.Fatalf("factory pixels: %v", err)
			}
		} else {
			if _, err := ReadKOSRejection(context.Background(), src, KOSRejectionLimits{}); err != nil {
				t.Fatal(err)
			}
		}
		if r.reads != 0 {
			t.Fatalf("bulk backing accessed: %d", r.reads)
		}
	}
}

func TestKOSRejectionConcurrentStableSource(t *testing.T) {
	o := kosTestOptions()
	fixture := kosIndependentFixture("113037", false)
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 4; j++ {
				if _, err := NewKOSPatientSafetyRejection(context.Background(), o); err != nil {
					t.Error(err)
				}
				if _, err := ReadKOSRejection(context.Background(), fixture, KOSRejectionLimits{}); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
}
