package object

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var stripSQ = core.NewTag(0x0040, 0xA730)
var stripPrivate = core.NewTag(0x0029, 0x1010)
var stripCreator = core.NewTag(0x0029, 0x0010)
var stripPublic = core.NewTag(0x0042, 0x0011)
var stripRetired = core.NewTag(0x0008, 0x0001)

func stripFixture() *Object {
	private := core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2, 3, 4})
	creator := core.NewRawElement(stripCreator, core.VRLO, []byte("TEST"))
	retired := core.NewRawElement(stripRetired, core.VRUL, []byte{1, 0, 0, 0})
	return FromElements([]core.Element{private, creator, retired,
		core.NewRawElement(core.NewTag(0x7778, 0x1010), core.VROB, []byte{9, 8}),
		dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: []core.Element{creator, private, retired, core.NewRawElement(stripPublic, core.VROB, []byte{7, 6})}}),
	}, std.Dictionary)
}

func TestStripAttributesPrivateRetiredRecursiveAndOrder(t *testing.T) {
	entry, ok := std.Dictionary.ByTag(stripRetired)
	if !ok || !entry.Retired {
		t.Fatal("fixture tag must be retired in the pinned dictionary")
	}
	obj := stripFixture()
	unknown, _ := obj.Get(core.NewTag(0x7778, 0x1010))
	originalUnknown, _ := unknown.RawBytes()
	report, err := StripAttributes(context.Background(), obj, StripAttributesOptions{RemovePrivate: true, RemoveRetired: true})
	if err != nil {
		t.Fatal(err)
	}
	if report.RemovedPrivate != 4 || report.RemovedRetired != 2 || report.VisitedItems != 1 || report.VisitedElements != 9 {
		t.Fatalf("counts=%+v", report)
	}
	if len(obj.Elements()) != 2 || obj.Elements()[0].Tag() != unknown.Tag() || obj.Elements()[1].Tag() != stripSQ {
		t.Fatal("retained order changed")
	}
	items, ok := obj.GetSequence(stripSQ)
	if !ok || len(items) != 1 || len(items[0].Elements()) != 1 || items[0].Elements()[0].Tag() != stripPublic {
		t.Fatal("nested filter was not committed to parent")
	}
	kept, _ := obj.Get(unknown.Tag())
	raw, _ := kept.RawBytes()
	if !bytes.Equal(raw, originalUnknown) || &raw[0] != &originalUnknown[0] {
		t.Fatal("retained raw buffer changed/copied")
	}
	// Private Information in even File Meta group is not an odd private tag.
	meta := FromElements([]core.Element{core.NewRawElement(core.NewTag(2, 0x0102), core.VROB, []byte{1, 2})}, std.Dictionary)
	if _, err := StripAttributes(nil, meta, StripAttributesOptions{RemovePrivate: true}); err != nil || len(meta.Elements()) != 1 {
		t.Fatal("even-group private info removed")
	}
}

type stripCancelContext struct {
	context.Context
	calls, at int
}

func (c *stripCancelContext) Err() error {
	c.calls++
	if c.calls >= c.at {
		return context.Canceled
	}
	return nil
}

func TestStripAttributesFailureIsAtomic(t *testing.T) {
	for _, tc := range []struct {
		name    string
		options StripAttributesOptions
		ctx     context.Context
		want    error
	}{
		{"elements", StripAttributesOptions{RemovePrivate: true, MaxElements: 6}, context.Background(), ErrStripAttributesLimit},
		{"items", StripAttributesOptions{RemovePrivate: true, MaxItems: 1}, context.Background(), ErrStripAttributesLimit},
		{"depth", StripAttributesOptions{RemovePrivate: true, MaxDepth: 1}, context.Background(), ErrStripAttributesLimit},
		{"negative", StripAttributesOptions{RemovePrivate: true, MaxItems: -1}, context.Background(), ErrStripAttributesOptions},
		{"cancel", StripAttributesOptions{RemovePrivate: true}, &stripCancelContext{Context: context.Background(), at: 5}, context.Canceled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			obj := stripFixture()
			seq, _ := obj.Get(stripSQ)
			v := seq.Value.(core.SequenceValue)
			v.Items = append(v.Items, core.DataSet{Elements: []core.Element{dicomtest.NewSequenceElement(stripSQ, core.DataSet{})}})
			seq.Value = v
			obj.Put(seq)
			before := obj.Elements()
			provider := obj.valueProvider
			report, err := StripAttributes(tc.ctx, obj, tc.options)
			if !errors.Is(err, tc.want) || report != (StripAttributesReport{}) {
				t.Fatalf("report=%+v err=%v", report, err)
			}
			if !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider {
				t.Fatal("failed preflight mutated object")
			}
		})
	}
	if _, err := StripAttributes(nil, nil, StripAttributesOptions{}); !errors.Is(err, ErrStripAttributesOptions) {
		t.Fatal("nil accepted")
	}
	bad := FromElements([]core.Element{core.NewRawElement(stripSQ, core.VRSQ, nil)}, std.Dictionary)
	if _, err := StripAttributes(nil, bad, StripAttributesOptions{RemovePrivate: true}); !errors.Is(err, ErrStripAttributesOptions) {
		t.Fatal("malformed SQ accepted")
	}
}

func TestStripAttributesCountsEmptyItemsAndRemovedBranches(t *testing.T) {
	privateSQ := core.NewTag(0x0029, 0x1020)
	obj := FromElements([]core.Element{dicomtest.NewSequenceElement(privateSQ, core.DataSet{}, core.DataSet{}, core.DataSet{})}, std.Dictionary)
	before := obj.Elements()
	if _, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true, MaxItems: 2}); !errors.Is(err, ErrStripAttributesLimit) || !reflect.DeepEqual(before, obj.Elements()) {
		t.Fatal("removed branch escaped item limit/atomicity")
	}
	if r, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true, MaxItems: 3}); err != nil || r.VisitedItems != 3 || len(obj.Elements()) != 0 {
		t.Fatalf("empty items: %+v %v", r, err)
	}
}

type stripReadSpy struct {
	*bytes.Reader
	reads  int
	closed bool
}

func (s *stripReadSpy) Read(p []byte) (int, error) {
	s.reads++
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.Reader.Read(p)
}

func (s *stripReadSpy) Seek(offset int64, whence int) (int64, error) {
	if s.closed {
		return 0, io.ErrClosedPipe
	}
	return s.Reader.Seek(offset, whence)
}
func (s *stripReadSpy) Close() error { s.closed = true; return nil }

func stripDeferredFixture(t *testing.T) (*File, *stripReadSpy, []byte) {
	t.Helper()
	payload := patternBytes(2048, 17)
	inner := core.DataSet{Elements: []core.Element{
		core.NewRawElement(stripPublic, core.VROB, payload), core.NewRawElement(stripCreator, core.VRLO, []byte("TEST")), core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}),
	}}
	elements := append([]core.Element{}, dicomtest.MinimalDataset()...)
	elements = append(elements, core.NewRawElement(stripPrivate, core.VROB, []byte{3, 4}), dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: []core.Element{dicomtest.NewSequenceElement(stripSQ, inner), core.NewRawElement(stripPrivate, core.VROB, []byte{5, 6})}}))
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	spy := &stripReadSpy{Reader: bytes.NewReader(data)}
	file, err := ReadFileWithOptions(spy, ReadFileOptions{InlineValueBytesThreshold: 128, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return file, spy, payload
}

func stripBulkPath() []parser.BulkDataPathStep {
	return []parser.BulkDataPathStep{{Tag: stripSQ, ItemIndex: 0}, {Tag: stripSQ, ItemIndex: 0}, {Tag: stripPublic, ItemIndex: -1}}
}

func TestStripAttributesRetainsNestedDeferredSourcePrivately(t *testing.T) {
	file, spy, payload := stripDeferredFixture(t)
	original := *file.Dataset
	provider := file.Dataset.valueProvider.(*readerValueProvider)
	beforeBindings := len(provider.bulkItemBindings)
	ref, ok, err := file.Dataset.DeferredBulkDataReference(stripBulkPath())
	if err != nil || !ok {
		t.Fatalf("original reference: %+v %v %v", ref, ok, err)
	}
	reads := spy.reads
	if _, err := StripAttributes(nil, file.Dataset, StripAttributesOptions{RemovePrivate: true}); err != nil {
		t.Fatal(err)
	}
	if spy.reads != reads {
		t.Fatal("filter read deferred payload")
	}
	if file.Dataset.valueProvider == provider || len(provider.bulkItemBindings) != beforeBindings {
		t.Fatal("shared original provider changed")
	}
	after, ok, err := file.Dataset.DeferredBulkDataReference(stripBulkPath())
	if err != nil || !ok || !reflect.DeepEqual(after, ref) {
		t.Fatalf("derived reference lost source: %+v %v %v", after, ok, err)
	}
	if _, ok, err := original.DeferredBulkDataReference(stripBulkPath()); err != nil || !ok {
		t.Fatal("original provider/facade lost bindings")
	}
	var copied bytes.Buffer
	if _, err := file.Dataset.CopyBulkDataValueTo(nil, stripBulkPath(), &copied); err != nil || !bytes.Equal(copied.Bytes(), payload) {
		t.Fatalf("retained bulk copied wrong: %v", err)
	}
	for _, policy := range []SequenceLengthPolicy{SequenceLengthUndefined, SequenceLengthPreserve, SequenceLengthDefined} {
		var encoded bytes.Buffer
		if err := WriteFileWithOptions(&encoded, file, WriteFileOptions{SequenceLength: policy}); err != nil {
			t.Fatal(err)
		}
		read, err := ReadFile(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		outer, _ := read.Dataset.GetSequence(stripSQ)
		inner, _ := outer[0].GetSequence(stripSQ)
		got, _ := inner[0].GetRaw(stripPublic)
		if !bytes.Equal(got, payload) {
			t.Fatal("nested bytes changed during serialize/read")
		}
		if _, found := inner[0].Get(stripPrivate); found {
			t.Fatal("nested private retained")
		}
		read.Close()
	}
	file.Close()
	if spy.closed {
		t.Fatal("borrowed source closed by file/filter")
	}
	spy.Close()
	copied.Reset()
	if _, err := file.Dataset.CopyBulkDataValueTo(nil, stripBulkPath(), &copied); !errors.Is(err, io.ErrClosedPipe) || copied.Len() != 0 {
		t.Fatalf("closed borrowed source replay=%v bytes=%d", err, copied.Len())
	}
	file.Dataset.Close()
	if _, _, err := file.Dataset.DeferredBulkDataReference(stripBulkPath()); err == nil {
		t.Fatal("released provider accepted")
	}
}

func TestStripAttributesRejectsForgedDeferredItemWithoutRebinding(t *testing.T) {
	file, spy, _ := stripDeferredFixture(t)
	provider := file.Dataset.valueProvider.(*readerValueProvider)
	seq, _ := file.Dataset.Get(stripSQ)
	original := seq.Value.(core.SequenceValue)
	// Copy an item, retaining its offset and nested payload metadata. Address
	// provenance is deliberately absent from the original provider.
	seq.Value = core.SequenceValue{Items: append([]core.DataSet(nil), original.Items...)}
	file.Dataset.Put(seq)
	before := file.Dataset.Elements()
	reads := spy.reads
	bindings := len(provider.bulkItemBindings)
	if _, err := StripAttributes(nil, file.Dataset, StripAttributesOptions{RemovePrivate: true}); !errors.Is(err, ErrStripAttributesOptions) {
		t.Fatalf("unbound forged item accepted: %v", err)
	}
	if !reflect.DeepEqual(before, file.Dataset.Elements()) || file.Dataset.valueProvider != provider || len(provider.bulkItemBindings) != bindings || spy.reads != reads {
		t.Fatal("refusal mutated source/identity or read payload")
	}
}

func stripWideChain() []core.Element {
	var elements []core.Element
	for depth := 0; depth < 12; depth++ {
		next := make([]core.Element, 80)
		next[0] = dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: elements})
		for i := 1; i < len(next); i++ {
			next[i] = core.NewRawElement(core.NewTag(0x7778, uint16(i)), core.VROB, []byte{1, 2})
		}
		elements = next
	}
	return elements
}

func TestStripAttributesReservesWideContainersBeforeDescent(t *testing.T) {
	ctx := &stripCancelContext{Context: context.Background(), at: 40}
	obj := FromElements(stripWideChain(), std.Dictionary)
	before := obj.Elements()
	report, err := StripAttributes(ctx, obj, StripAttributesOptions{RemovePrivate: true, MaxElements: 120, MaxItems: 2000})
	if !errors.Is(err, ErrStripAttributesLimit) || report != (StripAttributesReport{}) {
		t.Fatalf("container budget was not reserved before descent: report=%+v error=%v checks=%d", report, err, ctx.calls)
	}
	if !reflect.DeepEqual(before, obj.Elements()) {
		t.Fatal("admission failure mutated input")
	}
}

func TestStripAttributesAllocatedContainerCapacityStaysWithinCaps(t *testing.T) {
	elements := stripWideChain()
	state := stripState{ctx: context.Background(), opts: StripAttributesOptions{RemovePrivate: true, MaxDepth: 64, MaxElements: 120, MaxItems: 2000}}
	_, _, err := state.prepare(elements, 0, false, false, nil)
	if !errors.Is(err, ErrStripAttributesLimit) {
		t.Fatal(err)
	}
	// Every prepared element container (root snapshot reused) and every item
	// container is charged in full before make, across the entire recursion.
	if state.chargedElements > 120 || state.chargedItems > 2000 || state.chargedElements != 80 {
		t.Fatalf("capacity sums escaped cap: elements=%d items=%d", state.chargedElements, state.chargedItems)
	}
}

func TestStripAttributesNonBulkSourceRepresentation(t *testing.T) {
	textTag := core.NewTag(0x7778, 0x1000)
	payload := bytes.Repeat([]byte("X\\"), 256)
	for _, bound := range []bool{false, true} {
		t.Run(map[bool]string{false: "nonbulk-only", true: "with-bound-bulk"}[bound], func(t *testing.T) {
			inner := []core.Element{core.NewRawElement(textTag, core.VRLO, payload), core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2})}
			if bound {
				inner = append(inner, core.NewRawElement(stripPublic, core.VROB, patternBytes(1024, 11)))
			}
			elements := append(dicomtest.MinimalDataset(), core.NewRawElement(stripPublic, core.VROB, patternBytes(1024, 3)), dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: inner}))
			data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
			if err != nil {
				t.Fatal(err)
			}
			read := func() (*File, *stripReadSpy) {
				spy := &stripReadSpy{Reader: bytes.NewReader(data)}
				file, err := ReadFileWithOptions(spy, ReadFileOptions{InlineValueBytesThreshold: 128, DeferNestedValues: true})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { file.Close() })
				return file, spy
			}
			file, spy := read()
			items, _ := file.Dataset.GetSequence(stripSQ)
			value, _ := items[0].GetRaw(textTag)
			if !bytes.Equal(value, payload) || len(file.ValueLocations(textTag)) != 0 {
				t.Fatal("reader must materialize nested LO without deferred source location")
			}
			reads := spy.reads
			if _, err := StripAttributes(nil, file.Dataset, StripAttributesOptions{RemovePrivate: true}); err != nil || spy.reads != reads {
				t.Fatalf("legitimate materialized text was not preserved without source reads: %v", err)
			}
			var out bytes.Buffer
			if err := WriteFile(&out, file); err != nil {
				t.Fatal(err)
			}
			materialized, err := ReadFile(bytes.NewReader(out.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer materialized.Close()
			items, _ = materialized.Dataset.GetSequence(stripSQ)
			got, _ := items[0].GetRaw(textTag)
			if !bytes.Equal(got, payload) {
				t.Fatal("retained nonbulk bytes changed")
			}
			for _, representation := range []string{"original-item-nil", "copied-item-nil", "forged-offset", "path-spoof"} {
				bad, badSpy := read()
				seq, _ := bad.Dataset.Get(stripSQ)
				sequence := seq.Value.(core.SequenceValue)
				// Making text nil cannot invent a parser source record. Even an
				// original item bound by its adjacent OB cannot authenticate this LO.
				if representation == "copied-item-nil" {
					sequence.Items = append([]core.DataSet(nil), sequence.Items...)
					sequence.Items[0].Elements = append([]core.Element(nil), sequence.Items[0].Elements...)
				}
				sequence.Items[0].Elements[0].Value = nil
				if representation == "forged-offset" {
					sequence.Items[0].ItemOffset++
				}
				if representation == "path-spoof" {
					bad.Dataset.Remove(stripSQ)
					seq.Header.Tag = core.NewTag(0x7778, 0x2000)
				}
				seq.Value = sequence
				bad.Dataset.Put(seq)
				before, provider := bad.Dataset.Elements(), bad.Dataset.valueProvider
				reads := badSpy.reads
				report, err := StripAttributes(nil, bad.Dataset, StripAttributesOptions{RemovePrivate: true})
				if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, bad.Dataset.Elements()) || bad.Dataset.valueProvider != provider || badSpy.reads != reads {
					t.Fatalf("unverifiable nonbulk accepted/mutated: representation=%s error=%v", representation, err)
				}
			}
		})
	}
}

func stripValidatedText(t *testing.T) (*Object, *stripReadSpy, core.Tag, []byte) {
	t.Helper()
	tag := core.NewTag(0x7778, 0x1000)
	payload := []byte("SOURCE-TEXT ")
	var encoded bytes.Buffer
	if err := WriteDataSet(&encoded, FromElements([]core.Element{
		core.NewRawElement(tag, core.VRLO, payload), core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}),
	}, nil), transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	chain, err := validation.NewHookChain(validation.HookRegistration{Name: "defer-root-text", Points: []validation.HookPoint{validation.HookElementHeaderRead}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
		if event.Header.Tag == tag {
			return validation.HookDecision{DeferValue: true}, nil
		}
		return validation.HookDecision{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	spy := &stripReadSpy{Reader: bytes.NewReader(encoded.Bytes())}
	obj, _, err := ReadDataSetWithValidation(context.Background(), spy, transfer.ExplicitVRLittleEndian, ReadFileOptions{}, validation.Options{Hooks: chain})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { obj.Close() })
	elem, ok := obj.Get(tag)
	if !ok || elem.Value != nil || !elem.Header.HasLength() || int(elem.Header.Length) != len(payload) {
		t.Fatal("real validation hook did not defer root LO")
	}
	provider, ok := obj.valueProvider.(*readerValueProvider)
	if !ok || provider.reader == nil {
		t.Fatal("validation reader did not preserve deferred origin")
	}
	loc, ok := provider.reader.ValueLocation(tag)
	if !ok || loc.ItemOffsetSet || loc.Length != int64(len(payload)) || loc.ValueOffset != int64(bytes.Index(encoded.Bytes(), payload)) {
		t.Fatal("validation reader origin is not a unique root value location")
	}
	return obj, spy, tag, payload
}

func TestStripAttributesRetainsValidatedTopLevelNonBulkDeferred(t *testing.T) {
	obj, spy, tag, payload := stripValidatedText(t)
	reads := spy.reads
	position, _ := spy.Reader.Seek(0, io.SeekCurrent)
	report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
	if err != nil || report.RemovedPrivate != 1 || spy.reads != reads {
		t.Fatalf("legitimate root text origin refused/read: report=%+v error=%v", report, err)
	}
	if after, _ := spy.Reader.Seek(0, io.SeekCurrent); after != position {
		t.Fatal("source reference probe changed cursor")
	}
	var copied bytes.Buffer
	if _, err := obj.CopyValueTo(tag, &copied); err != nil || !bytes.Equal(copied.Bytes(), payload) {
		t.Fatalf("root source text lost: %v %x", err, copied.Bytes())
	}
	var encoded bytes.Buffer
	if err := WriteDataSet(&encoded, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDataSet(bytes.NewReader(encoded.Bytes()), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	got, ok := read.GetRaw(tag)
	if !ok || !bytes.Equal(got, payload) {
		t.Fatal("validated deferred text changed during real readback")
	}
}

func TestStripAttributesValidatedRootOriginRefusalsAreAtomic(t *testing.T) {
	for _, change := range []string{"VR", "length", "tag", "closed-source", "missing-reference"} {
		t.Run(change, func(t *testing.T) {
			obj, spy, tag, _ := stripValidatedText(t)
			elem, _ := obj.Get(tag)
			switch change {
			case "VR":
				elem.Header.VR = core.VRCS
				obj.Put(elem)
			case "length":
				elem.Header.Length += 2
				obj.Put(elem)
			case "tag":
				obj.Remove(tag)
				elem.Header.Tag = core.NewTag(0x7778, 0x1002)
				obj.Put(elem)
			case "closed-source":
				spy.Close()
			case "missing-reference":
				elem.Header.Tag = core.NewTag(0x7778, 0x1004)
				obj.Put(elem)
			}
			before, provider := obj.Elements(), obj.valueProvider
			position, _ := spy.Reader.Seek(0, io.SeekCurrent)
			reads := spy.reads
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
			afterPosition, _ := spy.Reader.Seek(0, io.SeekCurrent)
			if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || provider != obj.valueProvider || reads != spy.reads || position != afterPosition {
				t.Fatalf("origin %s accepted/mutated/read: %+v %v", change, report, err)
			}
		})
	}
}

func stripRootBulk(t *testing.T) (*File, *stripReadSpy, []byte) {
	t.Helper()
	payload := patternBytes(2048, 43)
	elements := append(dicomtest.MinimalDataset(), core.NewRawElement(stripPublic, core.VROB, payload), core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}))
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	spy := &stripReadSpy{Reader: bytes.NewReader(data)}
	file, err := ReadFileWithOptions(spy, ReadFileOptions{InlineValueBytesThreshold: 128, DeferValueTags: []core.Tag{stripPublic}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	element, _ := file.Dataset.Get(stripPublic)
	if element.Value != nil {
		t.Fatal("real reader did not defer root OB")
	}
	return file, spy, payload
}

func TestStripAttributesRootDefinedBulkAuthenticatesSource(t *testing.T) {
	for _, change := range []string{"valid", "tag", "VR", "length", "length-zero", "length-unset", "missing-reference", "closed-source"} {
		t.Run(change, func(t *testing.T) {
			file, spy, payload := stripRootBulk(t)
			obj := file.Dataset
			elem, _ := obj.Get(stripPublic)
			switch change {
			case "tag":
				obj.Remove(stripPublic)
				elem.Header.Tag = core.NewTag(0x7778, 0x4000)
				obj.Put(elem)
			case "VR":
				elem.Header.VR = core.VRUN
				obj.Put(elem)
			case "length":
				elem.Header.Length += 2
				obj.Put(elem)
			case "length-zero":
				elem.Header.Length = 0
				obj.Put(elem)
			case "length-unset":
				elem.Header.LengthSet = false
				obj.Put(elem)
			case "missing-reference":
				elem.Header.Tag = core.NewTag(0x7778, 0x4002)
				obj.Put(elem)
			case "closed-source":
				spy.Close()
			}
			before, provider := obj.Elements(), obj.valueProvider
			reads := spy.reads
			position, _ := spy.Reader.Seek(0, io.SeekCurrent)
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
			afterPosition, _ := spy.Reader.Seek(0, io.SeekCurrent)
			if change != "valid" {
				if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || provider != obj.valueProvider || reads != spy.reads || position != afterPosition {
					t.Fatalf("forged root OB %s accepted/mutated: report=%+v error=%v", change, report, err)
				}
				return
			}
			if err != nil || report.RemovedPrivate != 1 || reads != spy.reads || position != afterPosition {
				t.Fatalf("legitimate root OB refused/read: %+v %v", report, err)
			}
			var encoded bytes.Buffer
			if err := WriteFile(&encoded, file); err != nil {
				t.Fatal(err)
			}
			read, err := ReadFile(bytes.NewReader(encoded.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer read.Close()
			got, _ := read.GetRaw(stripPublic)
			if !bytes.Equal(got, payload) {
				t.Fatal("root OB readback changed")
			}
		})
	}
}

// Both values were deferred by the real reader. Global tag lookup is
// deliberately ambiguous, but the one root record remains authentic.
func TestStripAttributesRootAndNestedSameTagBulkReplay(t *testing.T) {
	rootPayload, nestedPayload := patternBytes(2048, 7), patternBytes(1024, 19)
	elements := append(dicomtest.MinimalDataset(), core.NewRawElement(stripPublic, core.VROB, rootPayload), core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}), dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: []core.Element{core.NewRawElement(stripPublic, core.VROB, nestedPayload), core.NewRawElement(stripPrivate, core.VROB, []byte{3, 4})}}))
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, elements...)
	if err != nil {
		t.Fatal(err)
	}
	spy := &stripReadSpy{Reader: bytes.NewReader(data)}
	file, err := ReadFileWithOptions(spy, ReadFileOptions{InlineValueBytesThreshold: 128, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	reader := file.Dataset.valueProvider.(*readerValueProvider).reader
	if _, ok := reader.ValueLocation(stripPublic); ok {
		t.Fatal("fixture must retain legacy global ambiguity")
	}
	if loc, ok := reader.RootValueLocation(stripPublic); !ok || loc.ItemOffsetSet || loc.Length != int64(len(rootPayload)) {
		t.Fatal("unique root origin missing")
	}
	reads := spy.reads
	position, _ := spy.Seek(0, io.SeekCurrent)
	report, err := StripAttributes(nil, file.Dataset, StripAttributesOptions{RemovePrivate: true})
	after, _ := spy.Seek(0, io.SeekCurrent)
	if err != nil || report.RemovedPrivate != 2 || reads != spy.reads || position != after {
		t.Fatalf("same-tag root/nested admission: %+v %v", report, err)
	}
	for _, policy := range []SequenceLengthPolicy{SequenceLengthUndefined, SequenceLengthPreserve, SequenceLengthDefined} {
		var encoded bytes.Buffer
		if err := WriteFileWithOptions(&encoded, file, WriteFileOptions{SequenceLength: policy}); err != nil {
			t.Fatal(err)
		}
		read, err := ReadFile(bytes.NewReader(encoded.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		got, _ := read.GetRaw(stripPublic)
		items, _ := read.Dataset.GetSequence(stripSQ)
		nested, _ := items[0].GetRaw(stripPublic)
		if !bytes.Equal(got, rootPayload) || !bytes.Equal(nested, nestedPayload) {
			t.Fatal("same-tag deferred bytes changed")
		}
		if _, ok := read.Dataset.Get(stripPrivate); ok {
			t.Fatal("root private retained")
		}
		if _, ok := items[0].Get(stripPrivate); ok {
			t.Fatal("nested private retained")
		}
		read.Close()
	}
}

func TestStripAttributesDuplicateRootBulkRefusal(t *testing.T) {
	elem := core.NewRawElement(stripPublic, core.VROB, []byte{1, 2, 3, 4})
	wire := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, elem, elem, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}))
	spy := &stripReadSpy{Reader: bytes.NewReader(wire)}
	obj, err := ReadDataSetWithOptions(spy, transfer.ExplicitVRLittleEndian, ReadFileOptions{InlineValueBytesThreshold: 1, DeferValueTags: []core.Tag{stripPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	element, _ := obj.Get(stripPublic)
	if element.Value != nil {
		t.Fatal("fixture must defer root OB")
	}
	before, provider := obj.Elements(), obj.valueProvider
	reads := spy.reads
	report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
	if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads {
		t.Fatalf("duplicate root admitted/mutated: %+v %v", report, err)
	}
}

func TestStripAttributesUndefinedEncapsulatedOriginReplay(t *testing.T) {
	pixel := dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0, 12, 0, 0, 0}, []byte{0xaa, 0xbb, 0xcc, 0xdd}, []byte{1, 2, 3, 4, 5, 6})
	want := pixel.Value.(core.FragmentSequence)
	elements := append(dicomtest.MinimalDataset(), pixel, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}))
	data, err := dicomtest.Part10File(transfer.JPEGBaseline, elements...)
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []string{"valid", "tag", "VR", "length", "missing-root-reference", "closed-source"} {
		t.Run(change, func(t *testing.T) {
			spy := &stripReadSpy{Reader: bytes.NewReader(data)}
			file, err := ReadFileWithOptions(spy, ReadFileOptions{DeferPixelData: true})
			if err != nil {
				t.Fatal(err)
			}
			defer file.Close()
			obj := file.Dataset
			elem, _ := obj.Get(core.TagPixelData)
			if elem.Value != nil || !elem.EncodedLength().IsUndefined() {
				t.Fatal("real reader did not defer undefined Pixel Data")
			}
			switch change {
			case "tag":
				obj.Remove(core.TagPixelData)
				elem.Header.Tag = stripPublic
				obj.Put(elem)
			case "VR":
				elem.Header.VR = core.VRUN
				obj.Put(elem)
			case "length":
				elem.Header.Length = 2
				obj.Put(elem)
			case "missing-root-reference":
				elem.Header.Tag = core.NewTag(0x7778, 0x4010)
				obj.Put(elem)
			case "closed-source":
				spy.Close()
			}
			before, provider := obj.Elements(), obj.valueProvider
			reads := spy.reads
			position, _ := spy.Reader.Seek(0, io.SeekCurrent)
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
			after, _ := spy.Reader.Seek(0, io.SeekCurrent)
			if change != "valid" {
				if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads || position != after {
					t.Fatalf("bad encapsulated origin accepted/mutated: %+v %v", report, err)
				}
				return
			}
			if err != nil || report.RemovedPrivate != 1 || reads != spy.reads || position != after {
				t.Fatalf("valid undefined source refused/read: %+v %v", report, err)
			}
			for _, policy := range []SequenceLengthPolicy{SequenceLengthUndefined, SequenceLengthPreserve, SequenceLengthDefined} {
				var out bytes.Buffer
				if err := WriteFileWithOptions(&out, file, WriteFileOptions{SequenceLength: policy}); err != nil {
					t.Fatal(err)
				}
				read, err := ReadFile(bytes.NewReader(out.Bytes()))
				if err != nil {
					t.Fatal(err)
				}
				got, _ := read.Dataset.Get(core.TagPixelData)
				if !reflect.DeepEqual(got.Value, want) {
					t.Fatalf("BOT/fragment replay changed: %#v", got.Value)
				}
				read.Close()
			}
		})
	}
}

func TestStripAttributesUndefinedNativeAndNestedOnlyRootRefused(t *testing.T) {
	for _, tc := range []string{"native-undefined", "nested-only-root"} {
		t.Run(tc, func(t *testing.T) {
			elem := core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 4})
			elements := []core.Element{elem}
			if tc == "nested-only-root" {
				elements = []core.Element{dicomtest.NewSequenceElement(stripSQ, core.DataSet{Elements: []core.Element{elem}})}
			}
			elements = append(elements, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}))
			spy := &stripReadSpy{Reader: bytes.NewReader(dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, elements...))}
			obj, err := ReadDataSetWithOptions(spy, transfer.ExplicitVRLittleEndian, ReadFileOptions{BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer obj.Close()
			if tc == "native-undefined" {
				elem, _ = obj.Get(core.TagPixelData)
				elem.Header.Length = core.UndefinedLength
			} else {
				elem.Value = nil
			}
			obj.Put(elem)
			before, provider := obj.Elements(), obj.valueProvider
			reads := spy.reads
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
			if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads {
				t.Fatalf("invented root admitted/mutated: %+v %v", report, err)
			}
		})
	}
}

type stripProbeCancelSource struct {
	*stripReadSpy
	cancel  context.CancelFunc
	enabled bool
}

func (s *stripProbeCancelSource) Seek(offset int64, whence int) (int64, error) {
	n, err := s.stripReadSpy.Seek(offset, whence)
	if s.enabled && offset == 0 && whence == io.SeekCurrent {
		s.cancel()
	}
	return n, err
}
func TestStripAttributesCancellationAfterSourceProbeIsAtomic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	payload := core.NewRawElement(stripPublic, core.VROB, []byte{1, 2, 3, 4})
	spy := &stripProbeCancelSource{stripReadSpy: &stripReadSpy{Reader: bytes.NewReader(dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, payload, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2})))}, cancel: cancel}
	obj, err := ReadDataSetWithOptions(spy, transfer.ExplicitVRLittleEndian, ReadFileOptions{InlineValueBytesThreshold: 1, DeferValueTags: []core.Tag{stripPublic}})
	if err != nil {
		t.Fatal(err)
	}
	defer obj.Close()
	element, _ := obj.Get(stripPublic)
	if element.Value != nil {
		t.Fatal("fixture must defer root OB before cancellation")
	}
	before, provider := obj.Elements(), obj.valueProvider
	reads := spy.reads
	spy.enabled = true
	report, err := StripAttributes(ctx, obj, StripAttributesOptions{RemovePrivate: true})
	if !errors.Is(err, context.Canceled) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads {
		t.Fatalf("probe cancel accepted/mutated/read: %+v %v", report, err)
	}
}

func TestStripAttributesOriginalLengthFormAuthenticatesSource(t *testing.T) {
	for _, originalUndefined := range []bool{false, true} {
		name := "defined-to-undefined"
		if originalUndefined {
			name = "undefined-to-defined"
		}
		t.Run(name, func(t *testing.T) {
			pixel := core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 4})
			if originalUndefined {
				pixel = dicomtest.NewFragmentSequenceElement(core.TagPixelData, []byte{0, 0, 0, 0}, []byte{1, 2, 3, 4})
			}
			// Public raw-data-set reader deliberately preserves the original VL form.
			wire := dicomtest.EncodeElements(transfer.JPEGBaseline, pixel, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2}))
			spy := &stripReadSpy{Reader: bytes.NewReader(wire)}
			obj, err := ReadDataSetWithOptions(spy, transfer.JPEGBaseline, ReadFileOptions{DeferPixelData: true, BulkDataDescriptor: &parser.BulkDataDescriptor{Rules: []parser.BulkDataRule{{VR: core.VROB}}}})
			if err != nil {
				t.Fatalf("real reader did not admit fixture: %v", err)
			}
			defer obj.Close()
			elem, _ := obj.Get(core.TagPixelData)
			if elem.Value != nil || elem.EncodedLength().IsUndefined() != originalUndefined {
				t.Fatal("fixture was not deferred with original VL form")
			}
			location, ok := obj.valueProvider.(*readerValueProvider).reader.RootValueLocation(core.TagPixelData)
			if !ok {
				t.Fatal("original root reference missing")
			}
			if originalUndefined {
				elem.Header.Length = core.Length(location.Length)
			} else {
				elem.Header.Length = core.UndefinedLength
			}
			obj.Put(elem)
			before, provider := obj.Elements(), obj.valueProvider
			reads := spy.reads
			position, _ := spy.Seek(0, io.SeekCurrent)
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
			after, _ := spy.Seek(0, io.SeekCurrent)
			if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) || !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads || position != after {
				t.Fatalf("VL form forgery admitted/mutated/read: report=%+v err=%v", report, err)
			}
		})
	}
}

func TestStripAttributesSourceValidationWithoutSelectedRemoval(t *testing.T) {
	for _, change := range []string{"valid", "length-zero", "length-unset"} {
		t.Run(change, func(t *testing.T) {
			file, spy, _ := stripRootBulk(t)
			obj := file.Dataset
			elem, _ := obj.Get(stripPublic)
			if change == "length-zero" {
				elem.Header.Length = 0
				obj.Put(elem)
			}
			if change == "length-unset" {
				elem.Header.LengthSet = false
				obj.Put(elem)
			}
			before, provider := obj.Elements(), obj.valueProvider
			reads := spy.reads
			position, _ := spy.Seek(0, io.SeekCurrent)
			// No public retired attribute is selected. Active options still validate
			// every retained nil source representation before the unchanged return.
			report, err := StripAttributes(nil, obj, StripAttributesOptions{RemoveRetired: true})
			after, _ := spy.Seek(0, io.SeekCurrent)
			if change == "valid" {
				if err != nil || report.VisitedElements != len(before) || report.RemovedPrivate != 0 || report.RemovedRetired != 0 {
					t.Fatalf("unchanged valid metadata rejected: %+v %v", report, err)
				}
			} else if !errors.Is(err, ErrStripAttributesOptions) || report != (StripAttributesReport{}) {
				t.Fatalf("no-removal origin forgery accepted: %+v %v", report, err)
			}
			if !reflect.DeepEqual(before, obj.Elements()) || obj.valueProvider != provider || reads != spy.reads || position != after {
				t.Fatal("unchanged/error case mutated or read source")
			}
		})
	}
}

func TestStripAttributesRetainsExplicitSourceFreeEmptyLiteral(t *testing.T) {
	empty := core.NewRawElement(stripPublic, core.VROB, nil)
	obj := FromElements([]core.Element{empty, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2})}, nil)
	report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
	if err != nil || report.RemovedPrivate != 1 {
		t.Fatalf("source-free empty refused: %+v %v", report, err)
	}
	var out bytes.Buffer
	if err := WriteDataSet(&out, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	read, err := ReadDataSet(bytes.NewReader(out.Bytes()), transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	defer read.Close()
	elem, ok := read.Get(stripPublic)
	if !ok || !elem.Header.HasLength() || elem.EncodedLength() != 0 {
		t.Fatal("empty literal readback changed")
	}
}

func TestStripAttributesRetainsSourceFreeExplicitNilPlaceholderMetadata(t *testing.T) {
	// Metadata filtering is not general writer or IOD validation. A declared
	// source-free zero-length placeholder has no raw source payload to forge.
	empty := core.Element{Header: core.ElementHeader{Tag: stripPublic, VR: core.VROB, LengthSet: true}}
	obj := FromElements([]core.Element{empty, core.NewRawElement(stripPrivate, core.VROB, []byte{1, 2})}, nil)
	report, err := StripAttributes(nil, obj, StripAttributesOptions{RemovePrivate: true})
	if err != nil || report.RemovedPrivate != 1 || !reflect.DeepEqual(obj.Elements(), []core.Element{empty}) {
		t.Fatalf("empty metadata changed: %+v %v", report, err)
	}
}
