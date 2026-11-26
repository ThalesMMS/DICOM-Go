package parser_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// This own synthetic harness compiles unchanged against published #1506. It
// never sets the new API's policy fields to obtain favourable comparison data.
// Construction is excluded; the destination retains only a SHA256 state.
type validationCopyCase struct {
	name           string
	payload, depth int
	kind           string
}

var validationCopyCases = []validationCopyCase{
	{"Nil1MiBDepth1", 1 << 20, 1, "element"}, {"Nil8MiBDepth1", 8 << 20, 1, "element"},
	{"Nil32MiBDepth1", 32 << 20, 1, "element"}, {"Nil64MiBDepth1", 64 << 20, 1, "element"},
	{"Nil1MiBDepth4", 1 << 20, 4, "element"}, {"Nil8MiBDepth4", 8 << 20, 4, "element"}, {"Nil1MiBDepth16", 1 << 20, 16, "element"},
	{"Observer1MiBDepth4", 1 << 20, 4, "observer"}, {"Transform1MiBDepth4", 1 << 20, 4, "transform"},
	{"Rule1MiBDepth4", 1 << 20, 4, "rule"}, {"Resolver1MiBDepth4", 1 << 20, 4, "resolver"},
	{"DataSet8MiBDepth4", 8 << 20, 4, "dataset"}, {"ObjectDeferred8MiBDepth4", 8 << 20, 4, "object"},
}

type validationCopyFixture struct {
	element core.Element
	opts    validation.Options
	obj     *object.Object
}
type validationCopyDictionary struct{}

func (validationCopyDictionary) ByTag(tag core.Tag) (dictionary.Entry, bool) {
	vr := core.VROB
	if tag.Group == 0x0008 {
		vr = core.VRSQ
	}
	return dictionary.Entry{Tag: tag, VR: vr, VM: "1"}, true
}
func (validationCopyDictionary) ByKeyword(string) (dictionary.Entry, bool) {
	return dictionary.Entry{}, false
}
func buildValidationCopyFixture(tb testing.TB, c validationCopyCase) validationCopyFixture {
	tb.Helper()
	data := make([]byte, c.payload)
	for i := range data {
		data[i] = byte(i*31 + i/(64<<10))
	}
	const leaf = 64 << 10
	elements := make([]core.Element, c.payload/leaf)
	for i := range elements {
		elements[i] = core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, uint16(0x1000+i)), VR: core.VROB, Length: leaf, LengthSet: true}, Value: core.RawValue(data[i*leaf : (i+1)*leaf])}
	}
	root := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}}
	for i := 1; i < c.depth; i++ {
		root = core.Element{Header: root.Header, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{root}}}}}
	}
	f := validationCopyFixture{element: root}
	if c.kind == "observer" || c.kind == "transform" {
		var err error
		f.opts.Hooks, err = validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
			if c.kind == "transform" {
				if raw, ok := event.Element.Value.(core.RawValue); ok {
					replacement := *event.Element
					owned := make(core.RawValue, len(raw))
					owned[0] = raw[0] ^ 1
					copy(owned[1:], raw[1:])
					replacement.Value = owned
					return validation.HookDecision{Element: &replacement}, nil
				}
			}
			return validation.HookDecision{}, nil
		})})
		if err != nil {
			tb.Fatal(err)
		}
	}
	if c.kind == "rule" {
		f.opts.DataSetRules = []validation.DataSetRuleRegistration{{Name: "read-only", Rule: validation.DataSetRuleFunc(func(context.Context, validation.DataSetContext) []validation.Finding { return nil })}}
	}
	if c.kind == "resolver" {
		f.opts.Dictionary = validationCopyDictionary{}
		f.opts.ResolveVR = func(_ validation.ElementContext, e dictionary.Entry) []core.VR { return []core.VR{e.VR} }
	}
	if c.kind == "object" {
		// Root native Pixel Data is publicly admitted by the validated Object
		// facade. Only the small materialized metadata has SQ depth c.depth;
		// this control does not claim nested-deferred validated write support.
		root = core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ},
			Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{
				{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1150), VR: core.VRUI}, Value: core.StringValue{"1.2.30"}},
				{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1155), VR: core.VRUI}, Value: core.StringValue{"1.2.32"}},
			}}}},
		}
		for i := 1; i < c.depth; i++ {
			root = core.Element{Header: root.Header, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{root}}}}}
		}
		f.element = root
		us := func(element, value uint16) core.Element {
			return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0028, element), VR: core.VRUS}, Value: core.Uint16Value{value}}
		}
		elements := []core.Element{
			root, us(0x0002, 1),
			{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{"MONOCHROME2"}},
			us(0x0010, 2048), us(0x0011, 4096), us(0x0100, 8), us(0x0101, 8), us(0x0102, 7), us(0x0103, 0),
			{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.Length(c.payload), LengthSet: true}, Value: core.RawValue(data)},
		}
		var input bytes.Buffer
		encoder := parser.NewWriterWithOptions(&input, transfer.ExplicitVRLittleEndian, parser.WriterOptions{LengthPolicy: parser.LengthPolicyDefined})
		for _, element := range elements {
			if err := encoder.WriteElement(element); err != nil {
				tb.Fatal(err)
			}
		}
		var err error
		f.obj, err = object.ReadDataSetWithOptions(bytes.NewReader(input.Bytes()), transfer.ExplicitVRLittleEndian, object.ReadFileOptions{
			DeferPixelData: true, MaxElementBytes: 64 << 10, MaxPixelDataBytes: int64(c.payload),
			MaxTotalBytes: 9 << 20, MaxElements: 64, MaxSequenceDepth: 12,
		})
		if err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { f.obj.Close() })
	}
	return f
}

type validationCopyHash struct {
	hash hash.Hash
	n    int64
}

func (w *validationCopyHash) Write(p []byte) (int, error) {
	n, e := w.hash.Write(p)
	w.n += int64(n)
	return n, e
}
func writeValidationCopy(f validationCopyFixture, c validationCopyCase, out io.Writer) error {
	if c.kind == "object" {
		_, err := object.WriteDataSetWithValidation(context.Background(), out, f.obj, transfer.ExplicitVRLittleEndian, f.opts)
		return err
	}
	w, err := parser.NewWriterWithValidation(context.Background(), out, transfer.ExplicitVRLittleEndian, parser.WriterOptions{LengthPolicy: parser.LengthPolicyDefined}, f.opts)
	if err != nil {
		return err
	}
	if c.kind == "dataset" {
		return w.WriteDataSet(core.DataSet{Elements: []core.Element{f.element}})
	}
	return w.WriteElement(f.element)
}
func BenchmarkValidatedWriterCopy(b *testing.B) {
	for _, c := range validationCopyCases {
		b.Run(c.name, func(b *testing.B) {
			f := buildValidationCopyFixture(b, c)
			out := &validationCopyHash{hash: sha256.New()}
			if err := writeValidationCopy(f, c, out); err != nil {
				b.Fatal(err)
			}
			sum, size := hex.EncodeToString(out.hash.Sum(nil)), out.n
			b.SetBytes(int64(c.payload))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out.hash.Reset()
				out.n = 0
				if err := writeValidationCopy(f, c, out); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if out.n != size || hex.EncodeToString(out.hash.Sum(nil)) != sum {
				b.Fatal("unstable output")
			}
			b.ReportMetric(float64(size), "wire-B/op")
		})
	}
}
func TestValidatedWriterMemoryProbe(t *testing.T) {
	name := os.Getenv("DICOM_VALIDATION_MEMORY_CASE")
	if name == "" {
		t.Skip("opt-in standalone memory probe, exclusive reservation required")
	}
	var selected *validationCopyCase
	for i := range validationCopyCases {
		if validationCopyCases[i].name == name {
			selected = &validationCopyCases[i]
			break
		}
	}
	if selected == nil {
		t.Fatal("unknown synthetic case")
	}
	fixture := buildValidationCopyFixture(t, *selected)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	baseline, _ := json.Marshal(map[string]any{"pid": os.Getpid(), "heap": before.HeapAlloc, "inuse": before.HeapInuse})
	fmt.Printf("VALIDATION_BASELINE %s\n", baseline)
	if gate := os.Getenv("DICOM_VALIDATION_MEMORY_GATE"); gate != "" {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			} else if !os.IsNotExist(err) {
				t.Fatal(err)
			}
			if time.Now().After(deadline) {
				t.Fatal("release gate timeout")
			}
			time.Sleep(time.Millisecond)
		}
	}
	peakHeap, peakInuse := before.HeapAlloc, before.HeapInuse
	var mu sync.Mutex
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				mu.Lock()
				if m.HeapAlloc > peakHeap {
					peakHeap = m.HeapAlloc
				}
				if m.HeapInuse > peakInuse {
					peakInuse = m.HeapInuse
				}
				mu.Unlock()
			}
		}
	}()
	out := &validationCopyHash{hash: sha256.New()}
	started := time.Now()
	err := writeValidationCopy(fixture, *selected, out)
	elapsed := time.Since(started)
	runtime.ReadMemStats(&after)
	close(stop)
	<-done
	if after.HeapAlloc > peakHeap {
		peakHeap = after.HeapAlloc
	}
	if after.HeapInuse > peakInuse {
		peakInuse = after.HeapInuse
	}
	runtime.KeepAlive(fixture)
	if err != nil {
		t.Fatal(err)
	}
	result := map[string]any{"case": name, "payload": selected.payload, "depth": selected.depth, "bytes": out.n, "sha256": hex.EncodeToString(out.hash.Sum(nil)), "duration_ns": elapsed.Nanoseconds(), "before_heap": before.HeapAlloc, "peak_heap": peakHeap, "additional_heap": peakHeap - before.HeapAlloc, "before_inuse": before.HeapInuse, "peak_inuse": peakInuse, "additional_inuse": peakInuse - before.HeapInuse, "total_alloc": after.TotalAlloc - before.TotalAlloc, "mallocs": after.Mallocs - before.Mallocs, "heap_sample_ns": int64(time.Millisecond)}
	encoded, _ := json.Marshal(result)
	fmt.Printf("VALIDATION_MEMORY %s\n", encoded)
}

func TestValidatedWriterRootDeferredFixtureCorrectness(t *testing.T) {
	c := validationCopyCase{"ObjectDeferred8MiBDepth4", 8 << 20, 4, "object"}
	f := buildValidationCopyFixture(t, c)
	pixel, ok := f.obj.Get(core.TagPixelData)
	if !ok || pixel.Value != nil || pixel.VR() != core.VROB || int64(pixel.Header.Length) != int64(c.payload) {
		t.Fatal("root deferred Pixel Data state changed")
	}
	current := f.obj
	for i := 0; i < c.depth; i++ {
		items, ok := current.GetSequence(f.element.Tag())
		if !ok || len(items) != 1 {
			t.Fatal("metadata SQ depth changed")
		}
		current = items[0]
	}
	if _, ok := current.GetSequence(f.element.Tag()); ok || !current.Has(core.NewTag(0x0008, 0x1155)) {
		t.Fatal("metadata does not terminate at SQ depth four")
	}
	for element, want := range map[uint16]int64{0x0002: 1, 0x0010: 2048, 0x0011: 4096, 0x0100: 8, 0x0101: 8, 0x0102: 7, 0x0103: 0} {
		got, err := f.obj.GetInt(core.NewTag(0x0028, element))
		if err != nil || got != want {
			t.Fatal("root Pixel Data geometry changed")
		}
	}
	if value, ok := f.obj.GetString(core.NewTag(0x0028, 0x0004)); !ok || value != "MONOCHROME2" {
		t.Fatal("photometric interpretation changed")
	}
	payload := sha256.New()
	n, err := f.obj.CopyValueTo(core.TagPixelData, payload)
	if err != nil || n != int64(c.payload) || hex.EncodeToString(payload.Sum(nil)) != "0602e4cad64ca6769e070acacfa425d5a82ef2198a3fbc9d3cdea42d3f8fc4a2" {
		t.Fatal("CopyValueTo count/hash changed")
	}
	out := &validationCopyHash{hash: sha256.New()}
	result, err := object.WriteDataSetWithValidation(context.Background(), out, f.obj, transfer.ExplicitVRLittleEndian, f.opts)
	if err != nil || !result.Complete || result.BytesWritten != 8388882 || out.n != result.BytesWritten ||
		hex.EncodeToString(out.hash.Sum(nil)) != "7c0c2e7aa8b3efd211068ef908ae396fd4f2a4b8d2890a231049263240e3031d" ||
		len(result.Report.Findings) != 0 || len(result.Report.Changes) != 0 || result.Report.Truncated || result.Report.Dropped != 0 {
		t.Fatalf("root deferred fixture differs from qualified resident control: %#v,%v", result, err)
	}
}
