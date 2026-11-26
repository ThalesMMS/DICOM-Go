package parser

import (
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
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

// This harness also compiles unchanged against the pre-streaming baseline.
// Fixtures are synthetic; the destination retains only a SHA-256 state.
// Memory probes are opt-in and must run separately from correctness tests.
type sequenceCampaignCase struct {
	name                                    string
	payload, depth                          int
	deferred, policy, validated, active, un bool
}

var sequenceCampaignCases = []sequenceCampaignCase{
	{name: "Raw1MiBDepth1", payload: 1 << 20, depth: 1},
	{name: "Raw8MiBDepth1", payload: 8 << 20, depth: 1},
	{name: "Raw32MiBDepth1", payload: 32 << 20, depth: 1},
	{name: "Raw64MiBDepth1", payload: 64 << 20, depth: 1},
	{name: "Raw1MiBDepth16", payload: 1 << 20, depth: 16},
	{name: "Raw1MiBDepth64", payload: 1 << 20, depth: 64},
	{name: "Deferred64MiBDepth1", payload: 64 << 20, depth: 1, deferred: true},
	{name: "GroupPadding8MiBDepth4", payload: 8 << 20, depth: 4, policy: true},
	{name: "ValidationNil8MiBDepth4", payload: 8 << 20, depth: 4, validated: true},
	{name: "ValidationActive1MiBDepth4", payload: 1 << 20, depth: 4, validated: true, active: true, policy: true},
	{name: "UNBigEndian8MiBDepth4", payload: 8 << 20, depth: 4, un: true},
}

type sequenceCampaignFixture struct {
	element    core.Element
	syntax     transfer.Syntax
	opts       WriterOptions
	validation validation.Options
}

func buildSequenceCampaignFixture(tb testing.TB, c sequenceCampaignCase) sequenceCampaignFixture {
	tb.Helper()
	const leaf = 64 << 10
	f := sequenceCampaignFixture{syntax: transfer.ExplicitVRLittleEndian, opts: WriterOptions{LengthPolicy: LengthPolicyDefined}}
	elements := make([]core.Element, c.payload/leaf)
	var source *os.File
	if c.deferred {
		var err error
		source, err = os.CreateTemp(tb.TempDir(), "deferred-*.bin")
		if err != nil {
			tb.Fatal(err)
		}
		if err = source.Truncate(int64(c.payload)); err != nil {
			tb.Fatal(err)
		}
		tb.Cleanup(func() { _ = source.Close() })
		f.opts.DeferredValue = func(_ core.DataSet, el core.Element) (func(io.Writer) (int64, error), bool) {
			if el.Value != nil {
				return nil, false
			}
			offset := int64(int(el.Tag().Element)-0x1000) * leaf
			return func(w io.Writer) (int64, error) { return io.Copy(w, io.NewSectionReader(source, offset, leaf)) }, true
		}
	} else {
		data := make([]byte, c.payload)
		for i := range data {
			data[i] = byte(i*31 + i/leaf)
		}
		for i := range elements {
			elements[i].Value = core.RawValue(data[i*leaf : (i+1)*leaf])
		}
	}
	for i := range elements {
		elements[i].Header = core.ElementHeader{Tag: core.NewTag(0x0011, uint16(0x1000+i)), VR: core.VROB, Length: leaf, LengthSet: true}
	}
	current := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}}
	for i := 1; i < c.depth; i++ {
		current = core.Element{Header: current.Header, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{current}}}}}
	}
	if c.policy {
		f.opts.GroupLength = GroupLengthCreate
		f.opts.Padding = PaddingOptions{Policy: PaddingCreate, ItemAlignment: 64}
	}
	if c.active {
		var err error
		f.validation.Hooks, err = validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
			return validation.HookDecision{}, nil
		})})
		if err != nil {
			tb.Fatal(err)
		}
	}
	if c.un {
		current.Header.VR = core.VRUN
		f.syntax = transfer.ExplicitVRBigEndian
		// Exercise payload normalization, not only the UN structural copy.
		for i := range elements {
			elements[i].Header.VR = core.VROW
		}
	}
	f.element = current
	return f
}

type sequenceHashWriter struct {
	hash hash.Hash
	n    int64
}

func (w *sequenceHashWriter) Write(p []byte) (int, error) {
	n, err := w.hash.Write(p)
	w.n += int64(n)
	return n, err
}
func writeSequenceCampaign(f sequenceCampaignFixture, c sequenceCampaignCase, w io.Writer) error {
	if c.validated {
		writer, err := NewWriterWithValidation(context.Background(), w, f.syntax, f.opts, f.validation)
		if err != nil {
			return err
		}
		return writer.WriteElement(f.element)
	}
	return NewWriterWithOptions(w, f.syntax, f.opts).WriteElement(f.element)
}
func BenchmarkDefinedSequenceStreaming(b *testing.B) {
	for _, c := range sequenceCampaignCases {
		b.Run(c.name, func(b *testing.B) {
			fixture := buildSequenceCampaignFixture(b, c)
			dst := &sequenceHashWriter{hash: sha256.New()}
			if err := writeSequenceCampaign(fixture, c, dst); err != nil {
				b.Fatal(err)
			}
			sum := hex.EncodeToString(dst.hash.Sum(nil))
			size := dst.n
			b.SetBytes(int64(c.payload))
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				dst.hash.Reset()
				dst.n = 0
				if err := writeSequenceCampaign(fixture, c, dst); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			if dst.n != size || hex.EncodeToString(dst.hash.Sum(nil)) != sum {
				b.Fatal("unstable serialized bytes")
			}
			b.ReportMetric(float64(size), "wire-B/op")
		})
	}
}

func TestDefinedSequenceMemoryProbe(t *testing.T) {
	name := os.Getenv("DICOM_SEQUENCE_MEMORY_CASE")
	if name == "" {
		t.Skip("opt-in standalone memory probe")
	}
	var selected *sequenceCampaignCase
	for i := range sequenceCampaignCases {
		if sequenceCampaignCases[i].name == name {
			selected = &sequenceCampaignCases[i]
			break
		}
	}
	if selected == nil {
		t.Fatal("unknown synthetic memory case")
	}
	fixture := buildSequenceCampaignFixture(t, *selected)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	baseline, _ := json.Marshal(struct {
		PID         int `json:"pid"`
		Heap, Inuse uint64
	}{os.Getpid(), before.HeapAlloc, before.HeapInuse})
	fmt.Printf("SEQUENCE_BASELINE %s\n", baseline)
	if gate := os.Getenv("DICOM_SEQUENCE_MEMORY_GATE"); gate != "" {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(gate); err == nil {
				break
			} else if !os.IsNotExist(err) {
				t.Fatal("memory probe release check failed")
			}
			if time.Now().After(deadline) {
				t.Fatal("memory probe release deadline")
			}
			time.Sleep(time.Millisecond)
		}
	}
	var peakHeap, peakInuse uint64 = before.HeapAlloc, before.HeapInuse
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
	dst := &sequenceHashWriter{hash: sha256.New()}
	started := time.Now()
	err := writeSequenceCampaign(fixture, *selected, dst)
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
	result := struct {
		Case                                                           string `json:"case"`
		Payload, Depth                                                 int
		Bytes                                                          int64
		SHA256                                                         string
		DurationNS                                                     int64
		BaselineHeap, PeakHeap, PeakInuse, AllocatedBytes, Allocations uint64
	}{name, selected.payload, selected.depth, dst.n, hex.EncodeToString(dst.hash.Sum(nil)), elapsed.Nanoseconds(), before.HeapAlloc, peakHeap, peakInuse, after.TotalAlloc - before.TotalAlloc, after.Mallocs - before.Mallocs}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("SEQUENCE_MEMORY %s\n", data)
}
