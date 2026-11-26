package sr

import (
	"bytes"
	"errors"
	"math"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func numericFixture(t *testing.T, r NumericRepresentation, longUnits bool) *object.Object {
	t.Helper()
	value, _, err := r.Resolve()
	if err != nil {
		t.Fatal(err)
	}
	units := CodedEntry{CodeValue: "mm", CodingSchemeDesignator: "UCUM", CodeMeaning: "millimeter"}
	metadata := TemplateValidationMetadata{}
	if longUnits {
		var context CodeContext
		units, context = NewCode("synthetic-long-unit-code", "99UNIT", "Synthetic unit")
		context.CodingSchemeVersion = "2026"
		metadata.CodeContexts = []ContentItemCodeContexts{{Path: "1.1", MeasurementUnits: context}}
	}
	doc := &Document{SOPClassUID: ComprehensiveSRStorage, SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1526.1", StudyInstanceUID: "1.2.826.0.1.3680043.10.543.1526.2", SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1526.3", ContentDate: "20261009", ContentTime: "120000", Title: CodedEntry{CodeValue: "ROOT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Root"}, Content: []ContentItem{{ValueType: ValueNum, RelationshipType: RelationshipContains, ConceptName: CodedEntry{CodeValue: "VALUE", CodingSchemeDesignator: "99TEST", CodeMeaning: "Value"}, Measurement: &Measurement{Value: value, Units: units}}}}
	snapshot, err := BindNumericRepresentations(doc, metadata, NumericRepresentationMetadata{Entries: []NumericRepresentationEntry{{Path: "1.1", Representation: r}}}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	dataset, err := doc.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return dataset
}
func numericObject(t *testing.T, obj *object.Object) *object.Object {
	t.Helper()
	items, ok := obj.GetSequence(tagContentSequence)
	if !ok || len(items) != 1 {
		t.Fatal("content sequence")
	}
	mv, ok := items[0].GetSequence(tagMeasuredValueSeq)
	if !ok || len(mv) != 1 {
		t.Fatal("measured sequence")
	}
	return mv[0]
}
func numericMutate(t *testing.T, obj *object.Object, change func(*[]core.Element)) *object.Object {
	t.Helper()
	elements := obj.Elements()
	ci := elementIndex(elements, tagContentSequence)
	cs := elements[ci].Value.(core.SequenceValue)
	mi := elementIndex(cs.Items[0].Elements, tagMeasuredValueSeq)
	ms := cs.Items[0].Elements[mi].Value.(core.SequenceValue)
	change(&ms.Items[0].Elements)
	cs.Items[0].Elements[mi].Value = ms
	elements[ci].Value = cs
	return object.FromElements(elements, nil)
}
func numericRemove(elements *[]core.Element, tag core.Tag) {
	i := elementIndex(*elements, tag)
	if i >= 0 {
		*elements = append((*elements)[:i], (*elements)[i+1:]...)
	}
}
func numericSame(t *testing.T, obj *object.Object, want NumericRepresentation) {
	t.Helper()
	got, present, err := readNumericMacro(func() *object.Object { items, _ := obj.GetSequence(tagContentSequence); return items[0] }())
	if err != nil || !present {
		t.Fatalf("numeric attrs: %v %v", present, err)
	}
	if got.DecimalString != want.DecimalString || got.HasFloatingPoint != want.HasFloatingPoint || got.HasRational != want.HasRational || got.HasDecimalString != want.HasDecimalString || math.Float64bits(got.FloatingPoint) != math.Float64bits(want.FloatingPoint) || got.RationalNumerator != want.RationalNumerator || got.RationalDenominator != want.RationalDenominator {
		t.Fatalf("numeric attrs changed: %#v -> %#v", want, got)
	}
}

func TestNumericRepresentationsRoundTrip(t *testing.T) {
	cases := []NumericRepresentation{
		{DecimalString: " 1.2300E-01 ", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .123, HasRational: true, RationalNumerator: 123, RationalDenominator: 1000},
		{DecimalString: ".333333333333333", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: 1.0 / 3, HasRational: true, RationalNumerator: 1, RationalDenominator: 3},
		{DecimalString: "-0", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: math.Copysign(0, -1)},
		{DecimalString: "1e-999", HasDecimalString: true},
		{DecimalString: "0.10", HasDecimalString: true, HasRational: true, RationalNumerator: 1, RationalDenominator: 10},
		{DecimalString: "-0.500000000116 ", HasDecimalString: true, HasRational: true, RationalNumerator: math.MinInt32, RationalDenominator: math.MaxUint32},
		{DecimalString: "123.00", HasDecimalString: true},
		{DecimalString: "0.25", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .25},
	}
	for i, r := range cases {
		t.Run(string(rune('A'+i)), func(t *testing.T) {
			for _, long := range []bool{false, true} {
				source := numericFixture(t, r, long)
				read, snapshot, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
				if err != nil {
					t.Fatal(err)
				}
				want, provenance, _ := r.Resolve()
				if math.Float64bits(read.Document.Content[0].Measurement.Value) != math.Float64bits(want) {
					t.Fatal("wrong preferred scalar")
				}
				t.Logf("selected %s; exact %v", provenance.Source, provenance.Exact)
				rewritten, err := read.Document.DatasetWithMetadataSnapshot(snapshot)
				if err != nil {
					t.Fatal(err)
				}
				numericSame(t, rewritten, r)
				if long && read.TemplateMetadata.CodeContexts[0].MeasurementUnits.LongCodeValue != "synthetic-long-unit-code" {
					t.Fatal("lost units context")
				}
				for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
					var encoded bytes.Buffer
					if err := object.WriteFile(&encoded, &object.File{Dataset: rewritten, TransferSyntax: syntax}); err != nil {
						t.Fatal(err)
					}
					parsed, err := object.ReadFile(bytes.NewReader(encoded.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					after, bound, err := ReadDocumentWithNumericRepresentations(parsed.Dataset, ReadOptions{}, MetadataSnapshotOptions{})
					if err != nil {
						t.Fatalf("%s: %v", syntax.UID, err)
					}
					expected := r
					if len(expected.DecimalString)%2 == 1 {
						expected.DecimalString += " "
					}
					numericSame(t, parsed.Dataset, expected)
					out, err := after.Document.DatasetWithMetadataSnapshot(bound)
					if err != nil {
						t.Fatal(err)
					}
					numericSame(t, out, expected)
				}
			}
		})
	}
}

func TestNumericProfilePrecedencePrecisionAndCells(t *testing.T) {
	for _, tc := range []struct {
		r      NumericRepresentation
		source NumericSource
		exact  bool
	}{
		{NumericRepresentation{DecimalString: "0.1", HasDecimalString: true}, NumericSourceDecimal, false},
		{NumericRepresentation{DecimalString: "0.25", HasDecimalString: true}, NumericSourceDecimal, true},
		{NumericRepresentation{DecimalString: "1e-999", HasDecimalString: true}, NumericSourceDecimal, false},
		{NumericRepresentation{DecimalString: "0.33333333333333", HasDecimalString: true, HasRational: true, RationalNumerator: 1, RationalDenominator: 3}, NumericSourceRational, false},
		{NumericRepresentation{DecimalString: "0.5", HasDecimalString: true, HasRational: true, RationalNumerator: 1, RationalDenominator: 2}, NumericSourceRational, true},
		{NumericRepresentation{DecimalString: "1", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: 1.5}, NumericSourceFloatingPoint, true},
		{NumericRepresentation{DecimalString: "1", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .5}, NumericSourceFloatingPoint, true},
	} {
		_, p, e := tc.r.Resolve()
		if e != nil || p.Source != tc.source || p.Exact != tc.exact {
			t.Fatalf("%#v: %#v %v", tc.r, p, e)
		}
	}
	r := NumericRepresentation{DecimalString: "1", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: math.Nextafter(1.5, 2)}
	if _, _, err := r.Resolve(); !errors.Is(err, ErrNumericRepresentation) {
		t.Fatal("outside cell accepted")
	}
	r.FloatingPoint = math.Nextafter(.5, 0)
	if _, _, err := r.Resolve(); !errors.Is(err, ErrNumericRepresentation) {
		t.Fatal("outside lower cell accepted")
	}
	r = NumericRepresentation{DecimalString: "-0", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: math.Copysign(0, -1), HasRational: true, RationalDenominator: 1}
	v, p, e := r.Resolve()
	if e != nil || math.Float64bits(v) != 1<<63 || p.Source != NumericSourceFloatingPoint {
		t.Fatal("negative zero not retained")
	}
}

func TestNumericRepresentationInvalidStringsAndAlternatives(t *testing.T) {
	for _, ds := range []string{"", " ", "1 2", "1\\2", "1\x00", "NaN", "Inf", "0x1p0", ".", "+", "1e", "1e+", "1e1025", "1e-1025", "1e309", "12345678901234567", "1\t", "１"} {
		t.Run(strings.ReplaceAll(ds, "\x00", "NUL"), func(t *testing.T) {
			_, _, err := (NumericRepresentation{DecimalString: ds, HasDecimalString: true}).Resolve()
			if !errors.Is(err, ErrNumericRepresentation) {
				t.Fatal("bad DS accepted")
			}
			if strings.Contains(err.Error(), ds) && len(ds) > 4 && ds != "NaN" {
				t.Fatal("value leaked")
			}
		})
	}
	cases := []NumericRepresentation{
		{DecimalString: "1"},
		{DecimalString: "1", HasDecimalString: true, FloatingPoint: 2},
		{DecimalString: "1", HasDecimalString: true, RationalDenominator: 2},
		{DecimalString: "1", HasDecimalString: true, HasRational: true},
		{DecimalString: "1", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: math.NaN()},
		{DecimalString: "1", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: math.Inf(-1)},
		{DecimalString: "1.00", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: 2},
		{DecimalString: "1.00", HasDecimalString: true, HasRational: true, RationalNumerator: 2, RationalDenominator: 1},
		{DecimalString: "1", HasDecimalString: true, HasRational: true, RationalNumerator: 1, RationalDenominator: 1, HasFloatingPoint: true, FloatingPoint: 1.25},
	}
	for _, r := range cases {
		if _, _, err := r.Resolve(); !errors.Is(err, ErrNumericRepresentation) {
			t.Fatalf("invalid alternative %#v", r)
		}
	}
}

type numericCallbackValue struct{ calls *atomic.Int32 }

func (v numericCallbackValue) Kind() core.ValueKind {
	v.calls.Add(1)
	panic("numeric callback invoked")
}
func (v numericCallbackValue) EncodedLength() (core.Length, bool) {
	v.calls.Add(1)
	panic("numeric callback invoked")
}
func TestNumericReaderRefusesResidentShapeWithoutCallbacks(t *testing.T) {
	valid := NumericRepresentation{DecimalString: "1.00", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: 1, HasRational: true, RationalNumerator: 1, RationalDenominator: 1}
	var calls atomic.Int32
	invalid := []struct {
		name        string
		tag         core.Tag
		replacement *core.Element
	}{
		{"missing DS", tagNumericValue, nil}, {"missing units", tagMeasurementUnitsCode, nil}, {"missing numerator", tagRationalNumeratorValue, nil}, {"missing denominator", tagRationalDenominatorValue, nil},
		{"wrong DS VR", tagNumericValue, &core.Element{Header: core.ElementHeader{Tag: tagNumericValue, VR: core.VRLO}, Value: core.StringValue{"1.00"}}},
		{"DS VM2", tagNumericValue, &core.Element{Header: core.ElementHeader{Tag: tagNumericValue, VR: core.VRDS}, Value: core.StringValue{"1", "1"}}},
		{"DS deferred", tagNumericValue, &core.Element{Header: core.ElementHeader{Tag: tagNumericValue, VR: core.VRDS}, Value: core.BulkDataValue{}}},
		{"DS discarded", tagNumericValue, &core.Element{Header: core.ElementHeader{Tag: tagNumericValue, VR: core.VRDS}, Value: core.DiscardedValue{}}},
		{"DS callback", tagNumericValue, &core.Element{Header: core.ElementHeader{Tag: tagNumericValue, VR: core.VRDS}, Value: numericCallbackValue{&calls}}},
		{"FD callback", tagFloatingPointValue, &core.Element{Header: core.ElementHeader{Tag: tagFloatingPointValue, VR: core.VRFD}, Value: numericCallbackValue{&calls}}},
	}
	for _, tag := range []core.Tag{tagFloatingPointValue, tagRationalNumeratorValue, tagRationalDenominatorValue} {
		vr := core.VRFD
		if tag == tagRationalNumeratorValue {
			vr = core.VRSL
		}
		if tag == tagRationalDenominatorValue {
			vr = core.VRUL
		}
		for _, n := range []int{0, 1, 3, 5, 9} {
			el := core.NewRawElement(tag, vr, make([]byte, n))
			invalid = append(invalid, struct {
				name        string
				tag         core.Tag
				replacement *core.Element
			}{name: tag.String() + string(rune('A'+n)), tag: tag, replacement: &el})
		}
		el := core.NewRawElement(tag, core.VRDS, []byte("1"))
		invalid = append(invalid, struct {
			name        string
			tag         core.Tag
			replacement *core.Element
		}{"wrong binary VR" + tag.String(), tag, &el})
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			source := numericFixture(t, valid, false)
			source = numericMutate(t, source, func(elements *[]core.Element) {
				numericRemove(elements, tc.tag)
				if tc.replacement != nil {
					*elements = append(*elements, *tc.replacement)
				}
			})
			result, snapshot, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
			if !errors.Is(err, ErrNumericRepresentation) || result.Document != nil || snapshot != nil {
				t.Fatalf("refusal: %v", err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("callback called")
	}
}

func TestNumericBindingStaleEditsCopiesAndLimits(t *testing.T) {
	r := NumericRepresentation{DecimalString: "0.10", HasDecimalString: true, HasRational: true, RationalNumerator: 1, RationalDenominator: 10}
	source := numericFixture(t, r, true)
	result, snapshot, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	copy := snapshot.NumericRepresentations()
	copy.Entries[0].Representation.DecimalString = "9.99"
	if snapshot.NumericRepresentations().Entries[0].Representation != r {
		t.Fatal("sidecar alias")
	}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 8; j++ {
				if _, err := result.Document.ElementsWithMetadataSnapshot(snapshot); err != nil {
					t.Error(err)
				}
			}
		}()
	}
	wg.Wait()
	original := result.Document.Content[0].Measurement.Units
	result.Document.Content[0].Measurement.Units.CodeMeaning = "Edited unit"
	if elements, err := result.Document.ElementsWithMetadataSnapshot(snapshot); elements != nil || !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("edited unit output")
	}
	result.Document.Content[0].Measurement.Units = original
	result.Document.Content[0].Measurement.Value = .2
	if elements, err := result.Document.ElementsWithMetadataSnapshot(snapshot); elements != nil || !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("edited value output")
	}
	if _, err := BindNumericRepresentations(result.Document, result.TemplateMetadata, snapshot.NumericRepresentations(), MetadataSnapshotOptions{}); !errors.Is(err, ErrNumericRepresentation) {
		t.Fatal("stale value bind")
	}
	rebound, err := BindNumericRepresentations(result.Document, result.TemplateMetadata, NumericRepresentationMetadata{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := result.Document.DatasetWithMetadataSnapshot(rebound)
	if err != nil {
		t.Fatal(err)
	}
	if numericObject(t, fresh).Has(tagRationalNumeratorValue) || numericObject(t, fresh).Has(tagFloatingPointValue) {
		t.Fatal("old alternatives survived edit")
	}
	result.Document.Content[0].Measurement.Value = .1
	for _, metadata := range []NumericRepresentationMetadata{{Entries: []NumericRepresentationEntry{{Path: "secret-value", Representation: r}}}, {Entries: []NumericRepresentationEntry{{Path: "1.99", Representation: r}}}, {Entries: []NumericRepresentationEntry{{Path: "1.1", Representation: r}, {Path: "1.1", Representation: r}}}} {
		if _, err := BindNumericRepresentations(result.Document, result.TemplateMetadata, metadata, MetadataSnapshotOptions{}); !errors.Is(err, ErrNumericRepresentation) {
			t.Fatal("bad association accepted")
		}
	}
	for _, options := range []MetadataSnapshotOptions{{MaxItems: 1}, {MaxValueBytes: 1}, {MaxDepth: -1}} {
		if _, err := BindNumericRepresentations(result.Document, result.TemplateMetadata, NumericRepresentationMetadata{Entries: []NumericRepresentationEntry{{Path: "1.1", Representation: r}}}, options); !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatal("budget accepted")
		}
	}
	// Identical model nodes cannot prove distinct lexical metadata ownership.
	item := result.Document.Content[0]
	result.Document.Content = []ContentItem{item, item}
	different := r
	different.DecimalString = ".100"
	m := NumericRepresentationMetadata{Entries: []NumericRepresentationEntry{{Path: "1.1", Representation: r}, {Path: "1.2", Representation: different}}}
	if _, err := BindNumericRepresentations(result.Document, TemplateValidationMetadata{}, m, MetadataSnapshotOptions{}); !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("ambiguous numeric association accepted")
	}
	m.Entries[1].Representation = r
	if _, err := BindNumericRepresentations(result.Document, TemplateValidationMetadata{}, m, MetadataSnapshotOptions{}); err != nil {
		t.Fatal("equivalent intact copies refused", err)
	}
}

func TestNumericLegacyRemainsLossyAndSourceIntactOnRefusal(t *testing.T) {
	r := NumericRepresentation{DecimalString: " 1.2300E-01 ", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .123, HasRational: true, RationalNumerator: 123, RationalDenominator: 1000}
	source := numericFixture(t, r, true)
	legacy, err := ReadDocumentWithOptions(source, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	rewritten, err := legacy.Document.DatasetWithTemplateMetadata(legacy.TemplateMetadata)
	if err != nil {
		t.Fatal(err)
	}
	if numericObject(t, rewritten).Has(tagFloatingPointValue) || numericObject(t, rewritten).Has(tagRationalNumeratorValue) {
		t.Fatal("legacy output changed")
	}
	numericSame(t, source, r)
	source = numericMutate(t, source, func(elements *[]core.Element) {
		replaceOrAppendElement(elements, core.Element{Header: core.ElementHeader{Tag: tagFloatingPointValue, VR: core.VRFD}, Value: core.Float64Value{math.Inf(1)}})
	})
	before := source.Elements()
	if _, _, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{}); !errors.Is(err, ErrNumericRepresentation) {
		t.Fatal("profile refusal missing")
	}
	if !reflect.DeepEqual(before, source.Elements()) {
		t.Fatal("source changed on refusal")
	}
}

func numericChangeItem(t *testing.T, obj *object.Object, change func(*[]core.Element)) *object.Object {
	t.Helper()
	elements := obj.Elements()
	i := elementIndex(elements, tagContentSequence)
	sequence := elements[i].Value.(core.SequenceValue)
	change(&sequence.Items[0].Elements)
	elements[i].Value = sequence
	return object.FromElements(elements, nil)
}
func TestNumericMacroShapeEmptyAndTraversalLimits(t *testing.T) {
	r := NumericRepresentation{DecimalString: "1.00", HasDecimalString: true}
	for _, tc := range []struct {
		name   string
		change func(*[]core.Element)
	}{
		{"missing measured", func(e *[]core.Element) { numericRemove(e, tagMeasuredValueSeq) }},
		{"wrong measured VR", func(e *[]core.Element) { i := elementIndex(*e, tagMeasuredValueSeq); (*e)[i].Header.VR = core.VRDS }},
		{"wrong measured kind", func(e *[]core.Element) { i := elementIndex(*e, tagMeasuredValueSeq); (*e)[i].Value = core.RawValue{} }},
		{"multiple measured items", func(e *[]core.Element) {
			i := elementIndex(*e, tagMeasuredValueSeq)
			v := (*e)[i].Value.(core.SequenceValue)
			v.Items = append(v.Items, v.Items[0])
			(*e)[i].Value = v
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			source := numericChangeItem(t, numericFixture(t, r, false), tc.change)
			if _, _, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{}); !errors.Is(err, ErrNumericRepresentation) {
				t.Fatal("macro accepted")
			}
		})
	}
	for _, tag := range []core.Tag{tagMeasurementUnitsCode, tagFloatingPointValue, tagRationalNumeratorValue, tagRationalDenominatorValue} {
		for _, n := range []int{0, 2} {
			t.Run(tag.String()+string(rune('A'+n)), func(t *testing.T) {
				source := numericFixture(t, r, false)
				source = numericMutate(t, source, func(e *[]core.Element) {
					var value core.Value
					vr := core.VRSQ
					switch tag {
					case tagMeasurementUnitsCode:
						value = core.SequenceValue{Items: make([]core.DataSet, n)}
					case tagFloatingPointValue:
						vr = core.VRFD
						value = core.Float64Value(make([]float64, n))
					case tagRationalNumeratorValue:
						vr = core.VRSL
						value = core.Int32Value(make([]int32, n))
						replaceOrAppendElement(e, core.Element{Header: core.ElementHeader{Tag: tagRationalDenominatorValue, VR: core.VRUL}, Value: core.Uint32Value{1}})
					case tagRationalDenominatorValue:
						vr = core.VRUL
						value = core.Uint32Value(make([]uint32, n))
						replaceOrAppendElement(e, core.Element{Header: core.ElementHeader{Tag: tagRationalNumeratorValue, VR: core.VRSL}, Value: core.Int32Value{1}})
					}
					replaceOrAppendElement(e, core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: value})
				})
				if _, _, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{}); !errors.Is(err, ErrNumericRepresentation) {
					t.Fatal("invalid VM accepted")
				}
			})
		}
	}
	source := numericFixture(t, r, false)
	source = numericChangeItem(t, source, func(e *[]core.Element) {
		replaceOrAppendElement(e, seqElement(tagMeasuredValueSeq))
		replaceOrAppendElement(e, codeSequence(tagNumericValueQualifierCodeSeq, CodedEntry{CodeValue: "ABSENT", CodingSchemeDesignator: "99TEST", CodeMeaning: "Absent"}))
	})
	read, snapshot, err := ReadDocumentWithNumericRepresentations(source, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.NumericRepresentations().Entries) != 0 || read.Document.Content[0].Measurement != nil {
		t.Fatal("empty measurement invented")
	}
	if _, err := read.Document.DatasetWithMetadataSnapshot(snapshot); err != nil {
		t.Fatal(err)
	}
	// Construct a finite nested source through resident sequence values.
	nested := numericFixture(t, r, false).Elements()
	ci := elementIndex(nested, tagContentSequence)
	child := nested[ci].Value.(core.SequenceValue).Items[0]
	for i := 0; i < 4; i++ {
		child = core.DataSet{Elements: []core.Element{strElem(tagValueType, core.VRCS, "CONTAINER"), seqElement(tagContentSequence, child)}}
	}
	nested[ci].Value = core.SequenceValue{Items: []core.DataSet{child}}
	for _, options := range []ReadOptions{{MaxDepth: 3}, {MaxItems: 3}, {MaxDepth: -1}} {
		if _, _, err := ReadDocumentWithNumericRepresentations(object.FromElements(nested, nil), options, MetadataSnapshotOptions{}); !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatal("tree limit accepted")
		}
	}
}

func TestNumericURNUnitsAndNestedPath(t *testing.T) {
	r := NumericRepresentation{DecimalString: "0.25", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: .25}
	source := numericFixture(t, r, false)
	read, err := ReadDocumentWithOptions(source, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	units, context := NewCode("urn:example:unit", "", "Synthetic unit")
	read.Document.Content[0].Measurement.Units = units
	item := read.Document.Content[0]
	read.Document.Content = []ContentItem{{ValueType: ValueContainer, RelationshipType: RelationshipContains, Children: []ContentItem{item}}}
	metadata := TemplateValidationMetadata{CodeContexts: []ContentItemCodeContexts{{Path: "1.1.1", MeasurementUnits: context}}}
	snapshot, err := BindNumericRepresentations(read.Document, metadata, NumericRepresentationMetadata{Entries: []NumericRepresentationEntry{{Path: "1.1.1", Representation: r}}}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := read.Document.DatasetWithMetadataSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	after, bound, err := ReadDocumentWithNumericRepresentations(out, ReadOptions{}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if after.Document.Content[0].Children[0].Measurement.Units.CodeValue != units.CodeValue || bound.NumericRepresentations().Entries[0].Path != "1.1.1" || bound.Metadata().CodeContexts[0].MeasurementUnits.URNCodeValue != context.URNCodeValue {
		t.Fatal("nested unit/numeric association changed")
	}
	after.Document.Content[0].Children = append([]ContentItem(nil), after.Document.Content[0].Children...)
	if elements, err := after.Document.ElementsWithMetadataSnapshot(bound); elements != nil || !errors.Is(err, ErrMetadataSnapshot) {
		t.Fatal("copied backing accepted")
	}
}

func TestNumericReferenceIndexUsesPreferredScalar(t *testing.T) {
	r := NumericRepresentation{DecimalString: "1 ", HasDecimalString: true, HasFloatingPoint: true, FloatingPoint: 1.25}
	result, snapshot, err := ReadDocumentWithNumericRepresentations(numericFixture(t, r, false), ReadOptions{ResolveReferences: true, ValidateRelationships: true}, MetadataSnapshotOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Document.Content[0].Measurement.Value != 1.25 || result.References == nil {
		t.Fatal("preferred scalar/reference index missing")
	}
	if _, err := result.References.Edges(); err != nil {
		t.Fatal("returned stale reference index", err)
	}
	if err := snapshot.Check(result.Document); err != nil {
		t.Fatal(err)
	}
	result.Document.Content[0].Measurement.Value = 1
	if _, err := result.References.Edges(); !errors.Is(err, ErrStaleReferenceIndex) {
		t.Fatal("edit did not stale existing reference guard")
	}
}
