package object

import (
	"bytes"
	"compress/flate"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestDeflateLevels(t *testing.T) {
	for _, data := range [][]byte{nil, bytes.Repeat([]byte("level fixture"), 1000), func() []byte {
		b := make([]byte, 8192)
		var x uint32 = 23
		for i := range b {
			x = x*1664525 + 1013904223
			b[i] = byte(x >> 24)
		}
		return b
	}()} {
		obj := FromElements([]core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, data)}, nil)
		var plain, defaultOut bytes.Buffer
		if err := WriteDataSet(&plain, obj, transfer.ExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		if err := WriteDataSet(&defaultOut, obj, transfer.DeflatedExplicitVRLittleEndian); err != nil {
			t.Fatal(err)
		}
		for level := -1; level <= 9; level++ {
			var selected *int
			if level >= 0 {
				selected = &level
			}
			var out bytes.Buffer
			if err := WriteDataSetWithOptions(&out, obj, transfer.DeflatedExplicitVRLittleEndian, WriteDataSetOptions{DeflateLevel: selected}); err != nil {
				t.Fatal(err)
			}
			if level == -1 && !bytes.Equal(out.Bytes(), defaultOut.Bytes()) {
				t.Fatal("nil level changed default bytes")
			}
			// bytes.Reader implements ReadByte, so flate does not read ahead into
			// the one possible A.5 NULL byte.
			source := bytes.NewReader(out.Bytes())
			r := flate.NewReader(source)
			got, err := io.ReadAll(r)
			if err != nil {
				t.Fatal(err)
			}
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, plain.Bytes()) {
				t.Fatalf("level %d changed dataset", level)
			}
			tail, _ := io.ReadAll(source)
			compressed := out.Len() - len(tail)
			if out.Len()%2 != 0 || (compressed%2 == 0 && len(tail) != 0) || (compressed%2 != 0 && !bytes.Equal(tail, []byte{0})) {
				t.Fatalf("level %d A.5 tail %x compressed %d", level, tail, compressed)
			}
		}
	}
}

func TestDeflateLevelsEmptyAndCloseErrors(t *testing.T) {
	for level := 0; level <= 9; level++ {
		var out bytes.Buffer
		if err := WriteDataSetWithOptions(&out, New(nil), transfer.DeflatedExplicitVRLittleEndian, WriteDataSetOptions{DeflateLevel: &level}); err != nil {
			t.Fatal(err)
		}
		r := flate.NewReader(bytes.NewReader(out.Bytes()))
		plain, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil || len(plain) != 0 || out.Len()%2 != 0 {
			t.Fatalf("empty level %d: %v", level, err)
		}
		short := &deflateCappedDestination{}
		d, err := newPaddedDeflaterWithLevel(short, level)
		if err != nil {
			t.Fatal(err)
		}
		if err := d.Close(); !errors.Is(err, io.ErrShortWrite) {
			t.Fatalf("level %d short close: %v", level, err)
		}
		// Find an odd raw stream independently of paddedDeflater, then fail
		// exactly on its required final NULL write.
		foundOdd := false
		for size := 0; size < 128; size++ {
			payload := bytes.Repeat([]byte{'a'}, size)
			var raw bytes.Buffer
			f, _ := flate.NewWriter(&raw, level)
			_, _ = f.Write(payload)
			_ = f.Close()
			if raw.Len()%2 == 0 {
				continue
			}
			foundOdd = true
			sentinel := errors.New("final NULL failed")
			destination := &deflateCappedDestination{remaining: raw.Len(), err: sentinel}
			d, err := newPaddedDeflaterWithLevel(destination, level)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Write(payload); err != nil {
				t.Fatal(err)
			}
			if err := d.Close(); !errors.Is(err, sentinel) {
				t.Fatalf("level %d NULL failure: %v", level, err)
			}
			break
		}
		if !foundOdd {
			t.Fatalf("no odd stream exercised for level %d", level)
		}
	}
}

func TestDeflateLevelsConcurrentSource(t *testing.T) {
	obj := FromElements(frameTestDataSet(1, 8), nil)
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 0}))
	padding := PaddingOptions{Policy: PaddingCreate, Alignment: 128}
	var expected, before bytes.Buffer
	if err := WriteDataSetWithOptions(&expected, obj, transfer.ExplicitVRLittleEndian, WriteDataSetOptions{Padding: padding}); err != nil {
		t.Fatal(err)
	}
	if err := WriteDataSet(&before, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	results := make(chan error, 20)
	for i := 0; i < 20; i++ {
		go func(level int) {
			var out bytes.Buffer
			if err := WriteDataSetWithOptions(&out, obj, transfer.DeflatedExplicitVRLittleEndian, WriteDataSetOptions{DeflateLevel: &level, Padding: padding}); err != nil {
				results <- err
				return
			}
			r := flate.NewReader(bytes.NewReader(out.Bytes()))
			got, err := io.ReadAll(r)
			_ = r.Close()
			if err == nil && !bytes.Equal(got, expected.Bytes()) {
				err = fmt.Errorf("level %d concurrent output changed", level)
			}
			results <- err
		}(i % 10)
	}
	for i := 0; i < 20; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	var after bytes.Buffer
	if err := WriteDataSet(&after, obj, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("concurrent writes changed source")
	}
}

type deflateCappedDestination struct {
	remaining int
	err       error
}

func (w *deflateCappedDestination) Write(p []byte) (int, error) {
	if len(p) <= w.remaining {
		w.remaining -= len(p)
		return len(p), nil
	}
	n := w.remaining
	w.remaining = 0
	return n, w.err
}

func TestDeflateLevelsPart10AndInvalid(t *testing.T) {
	obj := FromElements(frameTestDataSet(1, 8), nil)
	obj.Put(core.NewRawElement(core.TagPixelData, core.VROB, []byte{1, 2, 3, 0}))
	for level := 0; level <= 9; level++ {
		var out bytes.Buffer
		if err := WriteFileWithOptions(&out, &File{Dataset: obj, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}, WriteFileOptions{DeflateLevel: &level}); err != nil {
			t.Fatal(err)
		}
		b := out.Bytes()
		offset := 144 + int(binary.LittleEndian.Uint32(b[140:144]))
		if !bytes.Equal(b[128:132], []byte("DICM")) || string(b[136:138]) != "UL" {
			t.Fatal("meta compressed")
		}
		r := flate.NewReader(bytes.NewReader(b[offset:]))
		plain, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(plain, []byte{1, 2, 3, 0}) {
			t.Fatal("pixel data missing")
		}
		back, err := ReadFile(bytes.NewReader(b))
		if err != nil {
			t.Fatal(err)
		}
		if !back.TransferSyntax.Deflated {
			t.Fatal("wrong syntax")
		}
	}
	for _, level := range []int{-2, -1, 10} {
		var out bytes.Buffer
		if err := WriteFileWithOptions(&out, &File{Dataset: obj, TransferSyntax: transfer.DeflatedExplicitVRLittleEndian}, WriteFileOptions{DeflateLevel: &level}); err == nil || out.Len() != 0 {
			t.Fatal("invalid level wrote a prefix")
		}
	}
	level := 0
	var out bytes.Buffer
	if err := WriteFileWithOptions(&out, &File{Dataset: obj, TransferSyntax: transfer.ExplicitVRLittleEndian}, WriteFileOptions{DeflateLevel: &level}); err == nil || out.Len() != 0 {
		t.Fatal("native target accepted level")
	}
}
