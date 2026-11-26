package datasettime

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	studyDate     = core.NewTag(0x0008, 0x0020)
	studyTime     = core.NewTag(0x0008, 0x0030)
	acquisitionDT = core.NewTag(0x0008, 0x002A)
	sequenceTag   = core.NewTag(0x0040, 0xA730)
)

func text(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

func get(ds core.DataSet, tag core.Tag) core.Element {
	for _, e := range ds.Elements {
		if e.Tag() == tag {
			return e
		}
	}
	return core.Element{}
}

func TestConvertPairedRolloverAndFractions(t *testing.T) {
	for _, tc := range []struct{ name, source, target, date, clock, wantDate, wantClock string }{
		{"half-hour east leap day", "+0000", "+0530", "20240228", "234512.123456", "20240229", "051512.123456"},
		{"half-hour west year", "+0530", "-0330", "20240101", "001500.1", "20231231", "151500.1"},
		{"hour precision rollover", "-1200", "+1400", "20241231", "23", "20250102", "01"},
		{"minute precision", "+0545", "+0530", "20240501", "0010", "20240430", "2355"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := core.DataSet{Elements: []core.Element{text(studyDate, core.VRDA, tc.date), text(studyTime, core.VRTM, tc.clock), text(timezoneTag, core.VRSH, tc.source)}}
			out, err := Convert(src, tc.target, Options{})
			if err != nil {
				t.Fatal(err)
			}
			if got := get(out, studyDate).StringValue(); got != tc.wantDate {
				t.Fatalf("date %q want %q", got, tc.wantDate)
			}
			if got := get(out, studyTime).StringValue(); got != tc.wantClock {
				t.Fatalf("time %q want %q", got, tc.wantClock)
			}
			if get(src, studyDate).StringValue() != tc.date || get(src, studyTime).StringValue() != tc.clock || get(src, timezoneTag).StringValue() != tc.source {
				t.Fatal("source changed")
			}
			if get(out, timezoneTag).StringValue() != tc.target {
				t.Fatal("offset not updated")
			}
		})
	}
}

func TestConvertNestedContextsAndRawVM(t *testing.T) {
	child := core.DataSet{ItemOffset: 123, ItemOffsetSet: true, Elements: []core.Element{
		text(timezoneTag, core.VRSH, "-0330"),
		core.NewRawElement(studyDate, core.VRDA, []byte("20240101\\20241231 ")),
		core.NewRawElement(studyTime, core.VRTM, []byte("230000\\230000 ")),
	}}
	inherited := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, "20240228234500.00001")}}
	src := core.DataSet{Elements: []core.Element{text(timezoneTag, core.VRSH, "+0000"), {Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ, Length: 999, LengthSet: true}, Value: core.SequenceValue{Items: []core.DataSet{child, inherited}}}}}
	out, err := Convert(src, "+0530", Options{})
	if err != nil {
		t.Fatal(err)
	}
	seq := get(out, sequenceTag).Value.(core.SequenceValue)
	if !reflect.DeepEqual(get(seq.Items[0], studyDate).StringValues(), []string{"20240102", "20250101"}) || !reflect.DeepEqual(get(seq.Items[0], studyTime).StringValues(), []string{"080000", "080000"}) {
		t.Fatalf("paired VM: %+v", seq.Items[0])
	}
	if get(seq.Items[0], timezoneTag).StringValue() != "+0530" || get(seq.Items[1], timezoneTag).Value != nil {
		t.Fatal("nested override/inheritance")
	}
	if get(seq.Items[1], acquisitionDT).StringValue() != "20240229051500.00001" {
		t.Fatal("inherited DT conversion")
	}
	if seq.Items[0].ItemOffset != 123 || !seq.Items[0].ItemOffsetSet {
		t.Fatal("item metadata lost")
	}
	if get(out, sequenceTag).Header.LengthSet || get(seq.Items[0], studyTime).Header.LengthSet {
		t.Fatal("stale encoded lengths")
	}
	if !reflect.DeepEqual(get(child, studyDate).StringValues(), []string{"20240101", "20241231"}) || get(child, timezoneTag).StringValue() != "-0330" {
		t.Fatal("source item changed")
	}
}

func TestConvertExplicitDTAndPrecision(t *testing.T) {
	explicit := []string{"2024+0530", "20240229-1200", "20240229124500.123456+1400", "20161231235960+0000"}
	src := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, explicit...)}}
	out, err := Convert(src, "+0330", Options{})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(get(out, acquisitionDT).StringValues(), explicit) {
		t.Fatal("explicit DT changed")
	}
	for _, value := range []string{"2024022823", "202402282345", "20240228234500", "20240228234500.1", "20240228234500.123456"} {
		src := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, value), text(timezoneTag, core.VRSH, "+0000")}}
		out, err := Convert(src, "+0100", Options{})
		if err != nil {
			t.Fatal(err)
		}
		want := strings.Replace(value, "2024022823", "2024022900", 1)
		if got := get(out, acquisitionDT).StringValue(); got != want {
			t.Fatalf("%q => %q, want %q", value, got, want)
		}
	}
}

func TestConvertClockFallbackAndIdempotence(t *testing.T) {
	tag := core.NewTag(0x0018, 0x1042)
	src := core.DataSet{Elements: []core.Element{text(tag, core.VRTM, "2345", "001200.123")}}
	out, err := Convert(src, "+0530", Options{SourceOffset: "+0000"})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(get(out, tag).StringValues(), []string{"0515", "054200.123"}) {
		t.Fatal("standalone clock")
	}
	second, err := Convert(out, "+0530", Options{})
	if err != nil || !reflect.DeepEqual(second, out) {
		t.Fatalf("not idempotent: %v", err)
	}
	back, err := Convert(out, "+0000", Options{})
	if err != nil || !reflect.DeepEqual(get(back, tag), get(src, tag)) {
		t.Fatalf("clock round trip: %v", err)
	}
	// Existing source offset has precedence over an explicit fallback.
	src.Elements = append(src.Elements, text(timezoneTag, core.VRSH, "+0530"))
	out, err = Convert(src, "+0530", Options{SourceOffset: "-1200"})
	if err != nil || !reflect.DeepEqual(get(out, tag), get(src, tag)) {
		t.Fatal("fallback overrides dataset")
	}
}

func TestConvertAtomicFailures(t *testing.T) {
	for _, tc := range []struct {
		name     string
		elements []core.Element
		target   string
		opts     Options
		want     error
	}{
		{"missing source", []core.Element{text(acquisitionDT, core.VRDT, "20240228234500")}, "+0100", Options{}, ErrMissingOffset},
		{"invalid source", []core.Element{text(timezoneTag, core.VRSH, "-0000")}, "+0100", Options{}, ErrInvalidOffset},
		{"bad target", nil, "+1401", Options{}, ErrInvalidOffset},
		{"invalid date", []core.Element{text(studyDate, core.VRDA, "20240230"), text(studyTime, core.VRTM, "120000")}, "+0100", Options{SourceOffset: "+0000"}, ErrInvalidValue},
		{"unpaired date", []core.Element{text(studyDate, core.VRDA, "20240228")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"partial DT day", []core.Element{text(acquisitionDT, core.VRDT, "20240228")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"partial hour half shift", []core.Element{text(studyDate, core.VRDA, "20240228"), text(studyTime, core.VRTM, "23")}, "+0030", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"partial DT hour half shift", []core.Element{text(acquisitionDT, core.VRDT, "2024022823")}, "+0030", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"leap second", []core.Element{text(acquisitionDT, core.VRDT, "20161231235960")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"leap clock", []core.Element{text(studyTime, core.VRTM, "235960.123456")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"VM mismatch", []core.Element{text(studyDate, core.VRDA, "20240228"), text(studyTime, core.VRTM, "1200", "1300")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"empty pair component", []core.Element{text(studyDate, core.VRDA, "20240228"), text(studyTime, core.VRTM, "")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"duplicate", []core.Element{text(acquisitionDT, core.VRDT, "202401011200"), text(acquisitionDT, core.VRDT, "202401011300")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"pair VR", []core.Element{text(studyDate, core.VRLO, "20240228"), text(studyTime, core.VRTM, "120000")}, "+0100", Options{SourceOffset: "+0000"}, ErrInvalidValue},
		{"deferred DT", []core.Element{{Header: core.ElementHeader{Tag: acquisitionDT, VR: core.VRDT, Length: 14, LengthSet: true}}}, "+0100", Options{SourceOffset: "+0000"}, ErrUnsupported},
		{"query range", []core.Element{text(studyTime, core.VRTM, "120000-130000")}, "+0100", Options{SourceOffset: "+0000"}, ErrInvalidValue},
		{"year overflow", []core.Element{text(acquisitionDT, core.VRDT, "99991231235900")}, "+0100", Options{SourceOffset: "+0000"}, ErrAmbiguous},
		{"overlapping pairs", nil, "+0000", Options{Pairs: []Pair{{studyDate, studyTime}}}, ErrAmbiguous},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A valid value processed first proves that a later error publishes no
			// result and never writes even already-converted source text.
			first := text(core.NewTag(0x0018, 0x1042), core.VRTM, "230000")
			src := core.DataSet{Elements: append([]core.Element{first}, tc.elements...)}
			before := fmtSnapshot(src)
			out, err := Convert(src, tc.target, tc.opts)
			if !errors.Is(err, tc.want) {
				t.Fatalf("error %v want %v", err, tc.want)
			}
			if !reflect.DeepEqual(out, core.DataSet{}) {
				t.Fatal("partial output published")
			}
			if fmtSnapshot(src) != before {
				t.Fatal("source modified on failure")
			}
		})
	}
}

func fmtSnapshot(ds core.DataSet) string {
	// Serialization-independent lexical snapshot, sufficient for test primitive values.
	var b strings.Builder
	for _, e := range ds.Elements {
		b.WriteString(e.Tag().String())
		b.WriteString(e.VR().String())
		b.WriteString(strings.Join(e.StringValues(), "\\"))
	}
	return b.String()
}

func TestConvertOffsetDomainAndIdentityPartials(t *testing.T) {
	for _, value := range []string{"-1200", "-0030", "+0000", "+0030", "+0545", "+1400", "+0530 "} {
		if _, err := parseOffset(value); err != nil {
			t.Fatalf("valid %q: %v", value, err)
		}
	}
	for _, value := range []string{"-1201", "-1300", "+1401", "+1500", "+0060", "-0000", "0000", "+050", "+05:30", " +0530", "+0530\x00", "+0a00"} {
		if _, err := parseOffset(value); !errors.Is(err, ErrInvalidOffset) {
			t.Fatalf("invalid %q accepted", value)
		}
	}
	for _, value := range []string{"2024", "202402", "20240228", "20161231235960.1"} {
		src := core.DataSet{Elements: []core.Element{text(timezoneTag, core.VRSH, "+0000"), text(acquisitionDT, core.VRDT, value)}}
		out, err := Convert(src, "+0000", Options{})
		if err != nil || !reflect.DeepEqual(src, out) {
			t.Fatalf("identity %q: %v", value, err)
		}
	}
}

func TestConvertContextGroupVersionConditionalException(t *testing.T) {
	for _, resource := range []core.Element{text(mappingResourceTag, core.VRCS, "DCMR"), text(mappingResourceTag, core.VRCS, " DCMR "), text(mappingResourceUIDTag, core.VRUI, "1.2.840.10008.8.1.1")} {
		src := core.DataSet{Elements: []core.Element{resource, text(contextVersionTag, core.VRDT, "20240101")}}
		out, err := Convert(src, "+0530", Options{})
		if err != nil || get(out, contextVersionTag).StringValue() != "20240101" {
			t.Fatalf("DCMR version: %v", err)
		}
	}
	for _, elements := range [][]core.Element{
		{text(mappingResourceTag, core.VRCS, "PRIVATE"), text(contextVersionTag, core.VRDT, "20240101")},
		{text(mappingResourceTag, core.VRCS, "DCMR"), text(mappingResourceUIDTag, core.VRUI, "1.2.3"), text(contextVersionTag, core.VRDT, "20240101")},
		{text(mappingResourceTag, core.VRCS, "DCMR"), text(core.NewTag(0x0008, 0x0107), core.VRDT, "20240101")},
	} {
		if _, err := Convert(core.DataSet{Elements: elements}, "+0530", Options{SourceOffset: "+0000"}); !errors.Is(err, ErrAmbiguous) {
			t.Fatalf("nonexempt: %v", err)
		}
	}
}

func TestConvertPreserveCalendarDatePolicyAndReport(t *testing.T) {
	birthDate := core.NewTag(0x0010, 0x0030)
	for _, addEmptyBirthTime := range []bool{false, true} {
		src := core.DataSet{Elements: []core.Element{
			text(timezoneTag, core.VRSH, "+0000"),
			text(birthDate, core.VRDA, "19790501"),
			text(studyDate, core.VRDA, "20240228"), text(studyTime, core.VRTM, "234500"),
		}}
		if addEmptyBirthTime {
			src.Elements = append(src.Elements, text(core.NewTag(0x0010, 0x0032), core.VRTM))
		}
		out, report, err := ConvertWithReport(src, "+0530", Options{})
		if !errors.Is(err, ErrAmbiguous) || len(out.Elements) != 0 || len(report.PreservedDates) != 0 {
			t.Fatalf("default must reject unknown birth instant: %v", err)
		}
		out, report, err = ConvertWithReport(src, "+0530", Options{UnpairedDates: PreserveCalendarDate})
		if err != nil {
			t.Fatal(err)
		}
		if get(out, birthDate).StringValue() != "19790501" || get(out, studyDate).StringValue() != "20240229" || get(out, studyTime).StringValue() != "051500" {
			t.Fatal("calendar policy changed birth or failed study conversion")
		}
		want := []PreservedDate{{ItemPath: "dataset", Tag: birthDate, Component: 0}}
		if !reflect.DeepEqual(report.PreservedDates, want) {
			t.Fatalf("report %+v want %+v", report, want)
		}
		if get(src, studyDate).StringValue() != "20240228" || get(src, timezoneTag).StringValue() != "+0000" {
			t.Fatal("source changed")
		}
	}
	// Reporting itself is atomic: an earlier preserved date must not be
	// reported as success when an implicit partial DT later prevents conversion.
	src := core.DataSet{Elements: []core.Element{text(birthDate, core.VRDA, "19790501"), text(acquisitionDT, core.VRDT, "2024")}}
	out, report, err := ConvertWithReport(src, "+0530", Options{SourceOffset: "+0000", UnpairedDates: PreserveCalendarDate})
	if !errors.Is(err, ErrAmbiguous) || len(out.Elements) != 0 || len(report.PreservedDates) != 0 {
		t.Fatalf("partial report published: %v", err)
	}
}

func TestConvertDCMRUTCVersionAndResourcePadding(t *testing.T) {
	src := core.DataSet{Elements: []core.Element{text(mappingResourceTag, core.VRCS, " DCMR "), text(mappingResourceUIDTag, core.VRUI, "1.2.840.10008.8.1.1"), text(contextVersionTag, core.VRDT, "20240101+0000")}}
	out, err := Convert(src, "+0530", Options{})
	if err != nil || get(out, contextVersionTag).StringValue() != "20240101+0000" {
		t.Fatalf("UTC version: %v", err)
	}
	src.Elements[1] = text(mappingResourceUIDTag, core.VRUI, "1.2.3")
	if _, err := Convert(src, "+0530", Options{}); !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("UTC resource conflict: %v", err)
	}
}

func TestConvertPrivatePairsAndStandardDictionary(t *testing.T) {
	for _, p := range StandardPairs() {
		da, daOK := std.Dictionary.ByTag(p.Date)
		tm, tmOK := std.Dictionary.ByTag(p.Time)
		if !daOK || !tmOK || da.VR != core.VRDA || tm.VR != core.VRTM {
			t.Fatalf("invalid standard pair %+v", p)
		}
	}
	private := Pair{core.NewTag(0x0011, 0x1010), core.NewTag(0x0011, 0x1011)}
	src := core.DataSet{Elements: []core.Element{text(private.Date, core.VRDA, "20240101"), text(private.Time, core.VRTM, "001500")}}
	out, err := Convert(src, "-0030", Options{SourceOffset: "+0000", Pairs: []Pair{private}})
	if err != nil || get(out, private.Date).StringValue() != "20231231" || get(out, private.Time).StringValue() != "234500" {
		t.Fatalf("private pair: %v", err)
	}
}

func TestConvertStructureOwnershipAndConcurrentReads(t *testing.T) {
	pixels := core.RawValue{1, 2, 3, 4}
	pixelTag := core.NewTag(0x7FE0, 0x0010)
	child := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, "20240101120000")}}
	src := core.DataSet{Elements: []core.Element{text(timezoneTag, core.VRSH, "+0000"), text(sequenceTag, core.VRSQ), {Header: core.ElementHeader{Tag: pixelTag, VR: core.VROB, Length: 4, LengthSet: true}, Value: pixels}}}
	src.Elements[1].Value = core.SequenceValue{Items: []core.DataSet{child}}
	out, err := Convert(src, "+0100", Options{})
	if err != nil {
		t.Fatal(err)
	}
	outPixels := get(out, pixelTag).Value.(core.RawValue)
	if &outPixels[0] != &pixels[0] {
		t.Fatal("unchanged pixel data unexpectedly copied")
	}
	seq := get(out, sequenceTag).Value.(core.SequenceValue)
	seq.Items[0].Elements[0].Value.(core.StringValue)[0] = "changed"
	seq.Items[0].ItemOffset = 42
	out.Elements[0].Value.(core.StringValue)[0] = "changed"
	if get(child, acquisitionDT).StringValue() != "20240101120000" || get(src, timezoneTag).StringValue() != "+0000" || child.ItemOffset != 0 {
		t.Fatal("converted text/structure shares source")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				if _, err := Convert(src, "+0530", Options{}); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	// A cyclic authored sequence fails at the bound rather than recursing forever.
	cycle := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}}}}
	cycle.Elements[0].Value = core.SequenceValue{Items: []core.DataSet{cycle}}
	if out, err := Convert(cycle, "+0000", Options{MaxSequenceDepth: 2}); !errors.Is(err, ErrLimitExceeded) || len(out.Elements) != 0 {
		t.Fatalf("cyclic structure: %v", err)
	}
}

func TestConvertStructuralBudgetsBeforeExpansion(t *testing.T) {
	leaf := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, "20240101120000")}}
	dag := leaf
	for level := 0; level < 6; level++ {
		// Only seven unique element slices and twelve unique item references,
		// but traversal creates127element copies and126item copies.
		dag = core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{dag, dag}}}}}
	}
	for _, opts := range []Options{
		{SourceOffset: "+0000", MaxElements: 100},
		{SourceOffset: "+0000", MaxItems: 100},
	} {
		out, report, err := ConvertWithReport(dag, "+0100", opts)
		if !errors.Is(err, ErrLimitExceeded) || len(out.Elements) != 0 || len(report.PreservedDates) != 0 {
			t.Fatalf("DAG budget: %v", err)
		}
		if get(leaf, acquisitionDT).StringValue() != "20240101120000" {
			t.Fatal("DAG error changed leaf")
		}
	}
	if _, err := Convert(dag, "+0100", Options{SourceOffset: "+0000", MaxElements: 128, MaxItems: 126}); err != nil {
		t.Fatalf("exact structural budgets: %v", err)
	}
	// Fanout with no elements still allocates DataSet item records. It must be
	// bounded before the output item slice is allocated.
	wide := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: make([]core.DataSet, 100001)}}}}
	out, err := Convert(wide, "+0000", Options{})
	if !errors.Is(err, ErrLimitExceeded) || len(out.Elements) != 0 || len(wide.Elements[0].Value.(core.SequenceValue).Items) != 100001 {
		t.Fatalf("empty item default budget: %v", err)
	}
	for _, opts := range []Options{{MaxElements: -1}, {MaxItems: -1}, {MaxSequenceDepth: -1}} {
		if _, err := Convert(core.DataSet{}, "+0000", opts); !errors.Is(err, ErrLimitExceeded) {
			t.Fatalf("negative budget accepted: %v", err)
		}
	}
	if _, err := Convert(core.DataSet{}, "+0000", Options{MaxElements: 1, Pairs: []Pair{{core.NewTag(0x0011, 0x1010), core.NewTag(0x0011, 0x1011)}, {core.NewTag(0x0011, 0x1012), core.NewTag(0x0011, 0x1013)}}}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("configuration pair budget: %v", err)
	}
	// A generated root0201counts as an element copy too.
	if _, err := Convert(leaf, "+0100", Options{SourceOffset: "+0000", MaxElements: 1}); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("root generated element uncharged: %v", err)
	}
}

func TestConvertObjectAndEncodedRoundTrip(t *testing.T) {
	obj := object.FromElements([]core.Element{text(studyDate, core.VRDA, "20240228"), text(studyTime, core.VRTM, "234500.123456"), text(timezoneTag, core.VRSH, "+0000")}, std.Dictionary)
	out, err := Convert(obj.ToDataSet(), "+0530", Options{})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	writer := parser.NewWriter(&encoded, transfer.ExplicitVRLittleEndian)
	for _, e := range out.Elements {
		if err := writer.WriteElement(e); err != nil {
			t.Fatal(err)
		}
	}
	reader := parser.NewReader(bytes.NewReader(encoded.Bytes()), transfer.ExplicitVRLittleEndian, parser.ReaderOptions{Dictionary: std.Dictionary})
	decoded, err := reader.ReadDataSet()
	if err != nil {
		t.Fatal(err)
	}
	result := object.FromDataSet(decoded, std.Dictionary)
	date, _ := result.GetString(studyDate)
	clock, _ := result.GetString(studyTime)
	if date != "20240229" || clock != "051500.123456" {
		t.Fatalf("encoded result %s %s", date, clock)
	}
	if sourceDate, _ := obj.GetString(studyDate); sourceDate != "20240228" {
		t.Fatal("Object source changed")
	}
}

func TestConvertRecoveredUNAndNestedFailureAtomicity(t *testing.T) {
	item := core.DataSet{Elements: []core.Element{text(acquisitionDT, core.VRDT, "20240101234500")}}
	un := core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRUN}, Value: core.SequenceValue{Items: []core.DataSet{item}}}
	src := core.DataSet{Elements: []core.Element{text(timezoneTag, core.VRSH, "+0000"), un}}
	out, err := Convert(src, "+0030", Options{})
	if err != nil {
		t.Fatal(err)
	}
	seq := get(out, sequenceTag).Value.(core.SequenceValue)
	if get(out, sequenceTag).VR() != core.VRUN || get(seq.Items[0], acquisitionDT).StringValue() != "20240102001500" {
		t.Fatal("recovered UN sequence was not traversed")
	}
	// Success in one item must not leak when the next item cannot be converted.
	un.Value = core.SequenceValue{Items: []core.DataSet{item, {Elements: []core.Element{text(acquisitionDT, core.VRDT, "2024")}}}}
	src.Elements[1] = un
	out, err = Convert(src, "+0030", Options{})
	if !errors.Is(err, ErrAmbiguous) || len(out.Elements) != 0 || !strings.Contains(err.Error(), "[1]") {
		t.Fatalf("nested atomic failure: %v", err)
	}
	if get(item, acquisitionDT).StringValue() != "20240101234500" || get(src, timezoneTag).StringValue() != "+0000" {
		t.Fatal("nested failure mutated source")
	}
	for _, vr := range []core.VR{core.VRDT, core.VRDA, core.VRTM, core.VRLO} {
		src := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: acquisitionDT, VR: vr}, Value: core.SequenceValue{Items: []core.DataSet{item}}}}}
		out, err := Convert(src, "+0030", Options{SourceOffset: "+0000"})
		if !errors.Is(err, ErrInvalidValue) || len(out.Elements) != 0 {
			t.Fatalf("sequence with invalid VR %s: %v", vr, err)
		}
	}
}

func TestConvertEmptyValuesBirthPairAndFractions(t *testing.T) {
	src := core.DataSet{Elements: []core.Element{
		text(studyDate, core.VRDA), text(studyTime, core.VRTM, ""),
		text(acquisitionDT, core.VRDT, ""),
	}}
	out, err := Convert(src, "+0530", Options{})
	if err != nil || !reflect.DeepEqual(out.Elements[:len(src.Elements)], src.Elements) {
		t.Fatalf("empty values require invented context: %v", err)
	}
	birth := Pair{core.NewTag(0x0010, 0x0030), core.NewTag(0x0010, 0x0032)}
	for digits := 1; digits <= 6; digits++ {
		fraction := "123456"[:digits]
		src := core.DataSet{Elements: []core.Element{
			text(birth.Date, core.VRDA, "20240101"), text(birth.Time, core.VRTM, "234500."+fraction),
			text(acquisitionDT, core.VRDT, "20240101234500."+fraction),
		}}
		out, err := Convert(src, "+0030", Options{SourceOffset: "+0000"})
		if err != nil {
			t.Fatal(err)
		}
		if get(out, birth.Date).StringValue() != "20240102" || get(out, birth.Time).StringValue() != "001500."+fraction || get(out, acquisitionDT).StringValue() != "20240102001500."+fraction {
			t.Fatalf("fraction %s or birth pair changed precision", fraction)
		}
		back, err := Convert(out, "+0000", Options{})
		if err != nil || !reflect.DeepEqual(back.Elements[:len(src.Elements)], src.Elements) {
			t.Fatalf("dated roundtrip fraction %s: %v", fraction, err)
		}
	}
}
