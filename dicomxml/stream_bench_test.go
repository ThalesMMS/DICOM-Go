package dicomxml

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

var xmlBenchmarkObject *object.Object

func largeXMLFixture(kind string) ([]byte, *object.Object, int) {
	root := `<NativeDicomModel xmlns="` + Namespace + `" xml:space="preserve">`
	if kind == "inline" {
		raw := bytes.Repeat([]byte{0x10, 0x20, 0x30, 0x40}, 2<<20)
		elem := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.RawValue(raw)}
		doc := root + `<DicomAttribute tag="7FE00010" vr="OB" keyword="PixelData"><InlineBinary>` + base64.StdEncoding.EncodeToString(raw) + `</InlineBinary></DicomAttribute></NativeDicomModel>`
		return []byte(doc), object.FromElements([]core.Element{elem}, std.Dictionary), len(raw)
	}
	const count = 4000
	value := strings.Repeat("synthetic", 32)
	var doc strings.Builder
	doc.WriteString(root + `<DicomAttribute tag="00081115" vr="SQ" keyword="ReferencedSeriesSequence">`)
	items := make([]core.DataSet, count)
	for i := 0; i < count; i++ {
		fmt.Fprintf(&doc, `<Item number="%d"><DicomAttribute tag="77760001" vr="LO"><Value number="1">%s</Value></DicomAttribute><DicomAttribute tag="77760002" vr="UV"><Value number="1">18446744073709551615</Value></DicomAttribute></Item>`, i+1, value)
		items[i] = core.DataSet{Elements: []core.Element{
			{Header: core.ElementHeader{Tag: core.NewTag(0x7776, 1), VR: core.VRLO}, Value: core.StringValue{value}},
			core.NewRawElement(core.NewTag(0x7776, 2), core.VRUV, bytes.Repeat([]byte{255}, 8)),
		}}
	}
	doc.WriteString(`</DicomAttribute></NativeDicomModel>`)
	obj := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(8, 0x1115), VR: core.VRSQ}, Value: core.SequenceValue{Items: items}}}, std.Dictionary)
	return []byte(doc.String()), obj, count * (len(value) + 8)
}

func BenchmarkNativeXMLStreaming(b *testing.B) {
	for _, kind := range []string{"metadata", "inline"} {
		doc, obj, _ := largeXMLFixture(kind)
		b.Run("Decode/"+kind, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(doc)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				got, err := Decode(bytes.NewReader(doc), std.Dictionary, UnmarshalOptions{})
				if err != nil {
					b.Fatal(err)
				}
				xmlBenchmarkObject = got
			}
		})
		b.Run("Encode/"+kind, func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(doc)))
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := Encode(io.Discard, obj, Options{}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// This opt-in local process probe complements B/op with the OS high-water RSS
// from the caller's /usr/bin/time -l. It performs no external I/O or downloads.
// Heap peak is sampled and may miss brief peaks; retained heap includes the
// final Object on Decode, and metadata/allocator noise. Neither is a hard bound.
func TestNativeXMLMemoryProbe(t *testing.T) {
	mode := os.Getenv("DICOMXML_MEMORY_PROBE")
	if mode == "" {
		t.Skip("set DICOMXML_MEMORY_PROBE=Decode/metadata, Decode/inline, Encode/metadata or Encode/inline")
	}
	parts := strings.Split(mode, "/")
	if len(parts) != 2 || (parts[0] != "Decode" && parts[0] != "Encode") || (parts[1] != "metadata" && parts[1] != "inline") {
		t.Fatal("invalid memory probe mode")
	}
	doc, obj, valueBytes := largeXMLFixture(parts[1])
	xmlBenchmarkObject = nil
	runtime.GC()
	var before, after, retained runtime.MemStats
	runtime.ReadMemStats(&before)
	var peak atomic.Uint64
	peak.Store(before.HeapAlloc)
	done := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				var m runtime.MemStats
				runtime.ReadMemStats(&m)
				if m.HeapAlloc > peak.Load() {
					peak.Store(m.HeapAlloc)
				}
			case <-done:
				return
			}
		}
	}()
	start := time.Now()
	var err error
	if parts[0] == "Decode" {
		xmlBenchmarkObject, err = DecodeContext(context.Background(), bytes.NewReader(doc), std.Dictionary, UnmarshalOptions{})
	} else {
		err = EncodeContext(context.Background(), io.Discard, obj, Options{})
	}
	duration := time.Since(start)
	runtime.ReadMemStats(&after)
	close(done)
	<-stopped
	if err != nil {
		t.Fatal(err)
	}
	if after.HeapAlloc > peak.Load() {
		peak.Store(after.HeapAlloc)
	}
	runtime.GC()
	runtime.ReadMemStats(&retained)
	payload := map[string]any{"mode": mode, "input_xml_bytes": len(doc), "primitive_value_bytes": valueBytes, "wall_ns": duration.Nanoseconds(), "allocated_bytes": after.TotalAlloc - before.TotalAlloc, "allocations": after.Mallocs - before.Mallocs, "baseline_heap_bytes": before.HeapAlloc, "sampled_peak_heap_bytes": peak.Load(), "retained_heap_bytes": retained.HeapAlloc, "retained_heap_delta": int64(retained.HeapAlloc) - int64(before.HeapAlloc), "sampled_transient_heap_delta": int64(peak.Load()) - int64(retained.HeapAlloc)}
	encoded, _ := json.Marshal(payload)
	fmt.Println(string(encoded))
	runtime.KeepAlive(doc)
	runtime.KeepAlive(obj)
	runtime.KeepAlive(xmlBenchmarkObject)
}
