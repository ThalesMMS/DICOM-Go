package rtplan

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"sync"
	"testing"
)

type forbiddenValue struct{}

func (forbiddenValue) Kind() core.ValueKind               { return core.ValueBulkData }
func (forbiddenValue) EncodedLength() (core.Length, bool) { panic("implicit value access") }
func rawForAdmission(t core.Tag, b []byte) core.Element   { return core.NewRawElement(t, core.VROB, b) }
func sequenceForAdmission(t core.Tag, items ...core.DataSet) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: t, VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}
}
func TestAdmissionMaterializationAndIgnoredBulk(t *testing.T) {
	for _, value := range []core.Value{nil, core.RawValue(nil), forbiddenValue{}} {
		e := core.Element{Header: core.ElementHeader{Tag: ft(0x77771010), VR: core.VROB, Length: 32, LengthSet: true}, Value: value}
		_, err := preflight(context.Background(), object.FromElements([]core.Element{e}, std.Dictionary), Limits{}, false)
		if !errors.Is(err, ErrUnmaterialized) {
			t.Fatalf("%T: %v", value, err)
		}
	}
	bulk := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW, Length: 1 << 30, LengthSet: true}, Value: forbiddenValue{}}
	obj := object.FromElements([]core.Element{bulk}, std.Dictionary)
	if _, err := preflight(context.Background(), obj, Limits{MaxMetadataBytes: 1}, false); err != nil {
		t.Fatal(err)
	}
	if _, err := preflight(context.Background(), obj, Limits{}, true); !errors.Is(err, ErrUnmaterialized) {
		t.Fatal(err)
	}
}
func TestAdmissionDAGCountsOccurrencesAndRefusesCycles(t *testing.T) {
	leaf := core.DataSet{Elements: []core.Element{rawForAdmission(ft(0x77771010), []byte{1, 2, 3, 4})}}
	obj := object.FromElements([]core.Element{sequenceForAdmission(ft(0x77771020), leaf, leaf)}, std.Dictionary)
	if b, err := preflight(context.Background(), obj, Limits{MaxMetadataBytes: 8}, false); err != nil || b.bytes != 8 {
		t.Fatalf("%v %v", b, err)
	}
	if _, err := preflight(context.Background(), obj, Limits{MaxMetadataBytes: 7}, false); !errors.Is(err, ErrResourceLimit) {
		t.Fatal(err)
	}
	// Build an acyclic Object first: the generic Object constructor itself
	// recursively inventories values and is outside this package's admission.
	cycle := []core.Element{rawForAdmission(ft(0x77771010), nil)}
	cycleObj := object.FromElements([]core.Element{sequenceForAdmission(ft(0x77771020), core.DataSet{Elements: cycle})}, std.Dictionary)
	cycle[0] = sequenceForAdmission(ft(0x77771020), core.DataSet{Elements: cycle})
	if _, err := preflight(context.Background(), cycleObj, Limits{}, false); !errors.Is(err, ErrInvalidObject) {
		t.Fatal(err)
	}

}
func TestAdmissionRemainingModelAndContext(t *testing.T) {
	obj := object.FromElements([]core.Element{rawForAdmission(ft(0x77771010), make([]byte, 1024))}, std.Dictionary)
	if _, err := preflight(context.Background(), obj, Limits{MaxModelBytes: 4095}, false); !errors.Is(err, ErrResourceLimit) {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := preflight(ctx, obj, Limits{}, false); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
func TestReadIndependentFacadesConcurrently(t *testing.T) {
	f := newPlanFixture().file(t)
	// Routine replacements/removals leave stale ordering entries before reads;
	// no caller mutates the Object after the goroutines start.
	for i := 0; i < 256; i++ {
		tag := core.NewTag(0x7776, uint16(i))
		f.Dataset.Put(rawForAdmission(tag, []byte{1, 2}))
		f.Dataset.Put(rawForAdmission(tag, []byte{3, 4}))
	}
	for i := 0; i < 256; i++ {
		f.Dataset.Remove(core.NewTag(0x7776, uint16(i)))
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p, err := Read(f)
			if err != nil {
				t.Error(err)
				return
			}
			p.Beams[0].ControlPoints[0].DevicePositions[0].Positions[0] = 1234
		}()
	}
	wg.Wait()
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	if p.Beams[0].ControlPoints[0].DevicePositions[0].Positions[0] == 1234 {
		t.Fatal("model aliases source")
	}
}

func TestAdmissionIgnoredBulkNeverReachesGenericFacade(t *testing.T) {
	leaf := []core.Element{rawForAdmission(ft(0x77771010), nil)}
	graph := core.DataSet{Elements: leaf}
	for i := 0; i < 64; i++ {
		graph = core.DataSet{Elements: []core.Element{sequenceForAdmission(ft(0x77771020), graph)}}
	}
	f := newBrachyFixture().file(t)
	class, _ := f.Dataset.Get(tagSOPClassUID)
	instance, _ := f.Dataset.Get(tagSOPInstanceUID)
	f.Dataset = object.FromElements([]core.Element{class, instance, core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2})}, std.Dictionary)
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, f); err != nil {
		t.Fatal(err)
	}
	source := &countedAdmissionSource{Reader: bytes.NewReader(encoded.Bytes())}
	parsed, err := object.ReadFileWithOptions(source, object.ReadFileOptions{SkipPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	bulk := sequenceForAdmission(core.TagPixelData, graph)
	obj := parsed.Dataset
	obj.Put(bulk)
	source.reads = 0
	// Install a provider after constructing the finite graph; no generic
	// constructor should ever inventory this ignored tree after admission.
	leaf[0].Value = forbiddenValue{}
	if _, err := preflight(context.Background(), obj, Limits{MaxDepth: 1}, false); err != nil {
		t.Fatal(err)
	}
	facade, err := freshFacade(context.Background(), obj, false)
	if err != nil {
		t.Fatal(err)
	}
	if facade.Has(core.TagPixelData) {
		t.Fatal("unadmitted ignored bulk reached the metadata facade")
	}
	if !obj.Has(core.TagPixelData) || leaf[0].Value == nil || source.reads != 0 {
		t.Fatalf("source bulk backing changed or replayed: reads=%d", source.reads)
	}
}

type countedAdmissionSource struct {
	*bytes.Reader
	reads int
}

func (s *countedAdmissionSource) Read(p []byte) (int, error) { s.reads++; return s.Reader.Read(p) }
func TestIgnoredBulkSourceProviderIsNeverRead(t *testing.T) {
	f := newBrachyFixture().file(t)
	f.Dataset.Put(core.NewRawElement(core.TagPixelData, core.VROW, []byte{1, 2, 3, 4}))
	var encoded bytes.Buffer
	if err := object.WriteFile(&encoded, f); err != nil {
		t.Fatal(err)
	}
	source := &countedAdmissionSource{Reader: bytes.NewReader(encoded.Bytes())}
	parsed, err := object.ReadFileWithOptions(source, object.ReadFileOptions{SkipPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	bulk, ok := parsed.Dataset.Get(core.TagPixelData)
	if !ok || bulk.Value != nil {
		t.Fatal("fixture did not attach deferred bulk backing")
	}
	source.reads = 0
	p, err := ReadWithOptions(context.Background(), parsed, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: parsed})
	if err != nil {
		t.Fatal(err)
	}
	if out.Dataset.Has(core.TagPixelData) || source.reads != 0 {
		t.Fatalf("ignored provider reads=%d", source.reads)
	}
	still, ok := parsed.Dataset.Get(core.TagPixelData)
	if !ok || still.Value != nil || still.Header != bulk.Header {
		t.Fatal("source backing metadata changed")
	}
}
