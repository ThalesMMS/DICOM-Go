package object

import (
	"bytes"
	"io"
	"runtime"
	"testing"
)

// countingDiscard counts bytes and keeps a sample of what it receives.
type countingDiscard struct {
	n      int64
	sample bytes.Buffer
}

func (c *countingDiscard) Write(p []byte) (int, error) {
	if c.sample.Len() < 16 {
		c.sample.Write(p[:min(len(p), 16-c.sample.Len())])
	}
	c.n += int64(len(p))
	return len(p), nil
}

// A deferred Big Endian value is reversed while it streams: the writer keeps
// one bounded buffer instead of a copy of every write, so converting a large
// Pixel Data value does not allocate its size (#1988).
func TestReversingWriterStreamsWithoutAllocatingTheValue(t *testing.T) {
	const size = 32 << 20
	source := make([]byte, size)
	for index := range source {
		source[index] = byte(index)
	}
	destination := &countingDiscard{}
	writer := &reversingWriter{w: destination, width: 2}
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// Odd chunk sizes split units across writes.
	if _, err := io.CopyBuffer(writer, bytes.NewReader(source), make([]byte, 32<<10+1)); err != nil {
		t.Fatal(err)
	}
	if err := writer.flush(); err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&after)
	allocated := after.TotalAlloc - before.TotalAlloc
	if destination.n != size {
		t.Fatalf("wrote %d bytes, want %d", destination.n, size)
	}
	if want := []byte{1, 0, 3, 2, 5, 4, 7, 6}; !bytes.Equal(destination.sample.Bytes()[:8], want) {
		t.Fatalf("first units = %v, want %v", destination.sample.Bytes()[:8], want)
	}
	if allocated > 1<<20 {
		t.Errorf("reversing %d bytes allocated %d bytes", size, allocated)
	}
}

func TestReversingWriterReversesUnitsAcrossWrites(t *testing.T) {
	for _, width := range []int{2, 4, 8} {
		var source []byte
		for index := range 3*width*reversingWriterChunk/width + width {
			source = append(source, byte(index*7))
		}
		var out bytes.Buffer
		writer := &reversingWriter{w: &out, width: width}
		for offset := 0; offset < len(source); {
			n := min(len(source)-offset, 1+offset%(3*width))
			if _, err := writer.Write(source[offset : offset+n]); err != nil {
				t.Fatal(err)
			}
			offset += n
		}
		if err := writer.flush(); err != nil {
			t.Fatal(err)
		}
		got := out.Bytes()
		for unit := 0; unit+width <= len(source); unit += width {
			for index := 0; index < width; index++ {
				if got[unit+index] != source[unit+width-1-index] {
					t.Fatalf("width %d: byte %d = %d, want %d", width, unit+index, got[unit+index], source[unit+width-1-index])
				}
			}
		}
	}
}
