package hdf5

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type boundedHDFWriter struct {
	bytes.Buffer
	maximum int
	calls   int
	cancel  context.CancelFunc
	err     error
	short   bool
}

func (w *boundedHDFWriter) Write(p []byte) (int, error) {
	w.calls++
	if len(p) > w.maximum {
		w.maximum = len(p)
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.short {
		return len(p) - 1, nil
	}
	n, err := w.Buffer.Write(p)
	if w.cancel != nil {
		w.cancel()
	}
	return n, err
}

func TestWriteClassicStringsVectorsAndHierarchy(t *testing.T) {
	empty, text := "", "ASCII metadata"
	ds := []WriteDataset{{Path: "/EmptyString", Text: &empty}, {Path: "/Text", Text: &text}, {Path: "/EmptyVector"}, {Path: "/LargeVector", Count: 20000, Float64At: func(i int) float64 { return float64(i) / 8 }}}
	// Multiple leaf and internal symbol-table nodes, independently traversed.
	for i := 0; i < 1024; i++ {
		ds = append(ds, WriteDataset{Path: fmt.Sprintf("/Group/%04d/Value", i), Count: 1, Float64At: func(int) float64 { return 3.25 }})
	}
	w := &boundedHDFWriter{}
	if err := Write(nil, w, ds, 32<<20); err != nil {
		t.Fatal(err)
	}
	if w.maximum > writeChunk {
		t.Fatal("unbounded Write size", w.maximum)
	}
	f := open(t, w.Bytes())
	if f.Superblock != 0 {
		t.Fatal("superblock")
	}
	for name, want := range map[string]string{"/EmptyString": "", "/Text": text} {
		if got, err := f.ReadString(name); err != nil || got != want {
			t.Fatal("VLEN string", name, got, err)
		}
	}
	got, err := f.ReadFloat64s("/LargeVector", 20000)
	if err != nil || len(got) != 20000 {
		t.Fatal("large vector", err)
	}
	for i, v := range got {
		if v != float64(i)/8 {
			t.Fatal("vector value", i, v)
		}
	}
	got, err = f.ReadFloat64s("/EmptyVector", 0)
	if err != nil || len(got) != 0 {
		t.Fatal("empty vector", err)
	}
	links, err := f.Links("/Group")
	if err != nil || len(links) != 1024 {
		t.Fatal("large symbol table", len(links), err)
	}
	for _, name := range []string{"0000", "0255", "0256", "1023"} {
		got, err := f.ReadFloat64s("/Group/"+name+"/Value", 1)
		if err != nil || len(got) != 1 || got[0] != 3.25 {
			t.Fatal("tree boundaries", name, err)
		}
	}
	p, err := planWrite(context.Background(), ds, 32<<20)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range p.objects {
		if o.data == nil || o.data.Text == nil {
			continue
		}
		start := o.stringHeap
		b := w.Bytes()[start : start+4096]
		if string(b[:4]) != "GCOL" || b[4] != 1 || binary.LittleEndian.Uint64(b[8:]) != 4096 || binary.LittleEndian.Uint16(b[16:]) != 1 || binary.LittleEndian.Uint16(b[18:]) != 1 || binary.LittleEndian.Uint64(b[24:]) != uint64(len(*o.data.Text)) {
			t.Fatal("bounded global heap layout")
		}
		id := w.Bytes()[o.raw : o.raw+16]
		if binary.LittleEndian.Uint32(id) != uint32(len(*o.data.Text)) || binary.LittleEndian.Uint64(id[4:]) != start || binary.LittleEndian.Uint32(id[12:]) != 1 {
			t.Fatal("VLEN ID")
		}
	}
	if dir := os.Getenv("ITK1345_HDF_OUTPUT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "classic-empty-large.h5"), w.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestWriteAdmissionBeforeCallbacksAndBytes(t *testing.T) {
	calls := 0
	d := WriteDataset{Path: "/Vector", Count: 1, Float64At: func(int) float64 { calls++; return 1 }}
	var out bytes.Buffer
	if err := Write(nil, &out, []WriteDataset{d}, 1); !errors.Is(err, ErrWriteBudget) || calls != 0 || out.Len() != 0 {
		t.Fatal("budget before vector callback", calls, out.Len(), err)
	}
	if err := Write(nil, &out, []WriteDataset{d}, 1<<20); err != nil {
		t.Fatal(err)
	}
	exact := out.Len()
	out.Reset()
	calls = 0
	if err := Write(nil, &out, []WriteDataset{d}, int64(exact-1)); !errors.Is(err, ErrWriteBudget) || calls != 0 || out.Len() != 0 {
		t.Fatal("exact boundary", err, calls)
	}
	if err := Write(nil, &out, []WriteDataset{d}, int64(exact)); err != nil || out.Len() != exact {
		t.Fatal("exact cap", err)
	}
	long, nonASCII, withNul := strings.Repeat("x", 1025), "ação", "x\x00y"
	for _, text := range []string{long, nonASCII, withNul} {
		out.Reset()
		if err := Write(nil, &out, []WriteDataset{{Path: "/S", Text: &text}}, 1<<20); !errors.Is(err, ErrFormat) || out.Len() != 0 {
			t.Fatal("string admission", err)
		}
	}
	text := strings.Repeat("x", 1024)
	out.Reset()
	if err := Write(nil, &out, []WriteDataset{{Path: "/S", Text: &text}}, 1<<20); err != nil {
		t.Fatal("maximum string", err)
	}
	if got, err := open(t, out.Bytes()).ReadString("/S"); err != nil || got != text {
		t.Fatal("maximum string read", err)
	}
	bad := [][]WriteDataset{
		{}, {{Path: "/X", Count: -1}}, {{Path: "/X", Count: writeVectorElements + 1}}, {{Path: "/X", Count: 1}},
		{{Path: "/X", Text: &text, Count: 1}}, {{Path: "X"}}, {{Path: "/a//b"}}, {{Path: "/a/../b"}},
		{{Path: "/X"}, {Path: "/X"}}, {{Path: "/X"}, {Path: "/X/Y"}}, {{Path: "/a/b/c/d/e/f/g/h/i"}},
	}
	for _, ds := range bad {
		out.Reset()
		if err := Write(nil, &out, ds, 32<<20); !errors.Is(err, ErrFormat) || out.Len() != 0 {
			t.Fatal("invalid metadata admission", ds, err)
		}
	}
	tooMany := make([]WriteDataset, writeDatasets+1)
	out.Reset()
	if err := Write(nil, &out, tooMany, 32<<20); !errors.Is(err, ErrFormat) || out.Len() != 0 {
		t.Fatal("dataset count", err)
	}
	objects := make([]WriteDataset, 1100)
	for i := range objects {
		objects[i].Path = fmt.Sprintf("/G%d/a/b/c/d/e/f/g", i)
	}
	if err := Write(nil, &out, objects, 32<<20); !errors.Is(err, ErrFormat) || out.Len() != 0 {
		t.Fatal("metadata object limit", err)
	}
	if _, err := writeCheckedMul(math.MaxUint64, 8); !errors.Is(err, ErrFormat) {
		t.Fatal("checked multiplication", err)
	}
	p := writePlan{end: math.MaxInt64 - 3, max: math.MaxInt64}
	if _, err := p.alloc(8); !errors.Is(err, ErrWriteBudget) {
		t.Fatal("offset arithmetic", err)
	}
}

func TestWriteFiniteCancellationAndIO(t *testing.T) {
	var out bytes.Buffer
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		out.Reset()
		if err := Write(nil, &out, []WriteDataset{{Path: "/X", Count: 1, Float64At: func(int) float64 { return value }}}, 1<<20); !errors.Is(err, ErrFormat) || out.Len() != 0 {
			t.Fatal("finite scan before bytes", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := WriteDataset{Path: "/X", Count: 1, Float64At: func(int) float64 { t.Fatal("precancel callback"); return 0 }}
	if err := Write(ctx, &out, []WriteDataset{d}, 1<<20); !errors.Is(err, context.Canceled) || out.Len() != 0 {
		t.Fatal("precancel", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	calls := 0
	d.Count = 20000
	d.Float64At = func(int) float64 { calls++; cancel(); return 0 }
	if err := Write(ctx, &out, []WriteDataset{d}, 1<<20); !errors.Is(err, context.Canceled) || out.Len() != 0 || calls > 8192 {
		t.Fatal("preflight cancellation bounded polling", calls, err)
	}
	d.Count = 1
	d.Float64At = func(int) float64 { return 1 }
	ctx, cancel = context.WithCancel(context.Background())
	w := &boundedHDFWriter{cancel: cancel}
	if err := Write(ctx, w, []WriteDataset{d}, 1<<20); !errors.Is(err, context.Canceled) || w.calls != 1 {
		t.Fatal("mid-write cancellation", w.calls, err)
	}
	w = &boundedHDFWriter{short: true}
	if err := Write(nil, w, []WriteDataset{d}, 1<<20); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short write", err)
	}
	sentinel := errors.New("sink failure")
	w = &boundedHDFWriter{err: sentinel}
	if err := Write(nil, w, []WriteDataset{d}, 1<<20); !errors.Is(err, sentinel) {
		t.Fatal("IO error", err)
	}
	if err := Write(nil, nil, []WriteDataset{d}, 1<<20); !errors.Is(err, ErrFormat) {
		t.Fatal("nil writer", err)
	}
}
