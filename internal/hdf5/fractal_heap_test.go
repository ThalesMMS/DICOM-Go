package hdf5

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestDenseLinkHeapRejectsCorruptDirectBlocks(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func([]byte, []byte)
		want  error
	}{
		{"header checksum", func(header, _ []byte) { header[145] ^= 1 }, ErrFormat},
		{"direct checksum", func(_, block []byte) { block[21] ^= 1 }, ErrFormat},
		{"direct owner", func(_, block []byte) { block[5] ^= 1 }, ErrFormat},
		{"filtered heap", func(header, _ []byte) { header[7] = 1 }, ErrUnsupported},
		{"managed count limit", func(header, _ []byte) {
			binary.LittleEndian.PutUint64(header[70:], maxLinks+1)
			putHeapTrailerChecksum(header)
		}, ErrFormat},
		{"byte limit", func(header, _ []byte) {
			binary.LittleEndian.PutUint64(header[54:], maxLinkHeapBytes+1)
			putHeapTrailerChecksum(header)
		}, ErrFormat},
		{"allocation mismatch", func(header, _ []byte) {
			binary.LittleEndian.PutUint64(header[54:], 511)
			putHeapTrailerChecksum(header)
		}, ErrFormat},
	} {
		t.Run(test.name, func(t *testing.T) {
			data := fixture(t, "many_dense.h5")
			header := bytes.Index(data, []byte("FRHP"))
			block := bytes.Index(data, []byte("FHDB"))
			if header < 0 || block < 0 {
				t.Fatal("dense heap fixture lost its header or direct block")
			}
			test.alter(data[header:header+146], data[block:block+512])
			if _, err := open(t, data).openLinkHeap(uint64(header)); !errors.Is(err, test.want) {
				t.Fatalf("openLinkHeap: %v, want %v", err, test.want)
			}
		})
	}
}

func TestDenseLinkHeapReadsNestedIndirectBlocks(t *testing.T) {
	const (
		headerAddr = 32
		rootAddr   = 256
		childAddr  = 384
		direct0    = 512
		direct1    = direct0 + 64
	)
	binaryOrder := binary.LittleEndian
	data := make([]byte, direct1+64)
	header := data[headerAddr : headerAddr+146]
	copy(header, "FRHP")
	binaryOrder.PutUint16(header[5:], 4) // 1 flag byte, 2 offset bytes, 1 length byte
	binaryOrder.PutUint32(header[10:], 20)
	binaryOrder.PutUint64(header[46:], 512) // total space: 3 rows, 2 columns
	binaryOrder.PutUint64(header[54:], 128) // allocated space: two direct blocks
	binaryOrder.PutUint64(header[70:], 2)   // managed objects
	binaryOrder.PutUint16(header[110:], 2)
	binaryOrder.PutUint64(header[112:], 64)
	binaryOrder.PutUint64(header[120:], 64)
	binaryOrder.PutUint16(header[128:], 16)
	binaryOrder.PutUint16(header[130:], 1)
	binaryOrder.PutUint64(header[132:], rootAddr)
	binaryOrder.PutUint16(header[140:], 3)
	putHeapTrailerChecksum(header)
	putBlockHeader := func(block []byte, signature string, offset uint16) {
		copy(block, signature)
		binaryOrder.PutUint64(block[5:], headerAddr)
		binaryOrder.PutUint16(block[13:], offset)
	}
	root := data[rootAddr : rootAddr+15+8*6+4]
	putBlockHeader(root, "FHIB", 0)
	for j := 0; j < 6; j++ {
		binaryOrder.PutUint64(root[15+8*j:], ^uint64(0))
	}
	binaryOrder.PutUint64(root[15:], direct0)
	binaryOrder.PutUint64(root[15+8*4:], childAddr)
	putHeapTrailerChecksum(root)
	child := data[childAddr : childAddr+15+8*2+4]
	putBlockHeader(child, "FHIB", 256)
	binaryOrder.PutUint64(child[15:], direct1)
	binaryOrder.PutUint64(child[23:], ^uint64(0))
	putHeapTrailerChecksum(child)
	putBlockHeader(data[direct0:], "FHDB", 0)
	copy(data[direct0+15:], "abc")
	putBlockHeader(data[direct1:], "FHDB", 256)
	copy(data[direct1+15:], "xyz")
	file := &File{r: bytes.NewReader(data), size: int64(len(data)), offsetSize: 8, lengthSize: 8}
	heap, err := file.openLinkHeap(headerAddr)
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{"abc", "xyz"} {
		got, err := heap.linkObject([]byte{0, 15, byte(i), 3})
		if err != nil || string(got) != want {
			t.Fatalf("link %d = %q, %v; want %q", i, got, err, want)
		}
	}
	if err := heap.validateObjects(); err != nil {
		t.Fatal(err)
	}
	t.Run("indirect checksum", func(t *testing.T) {
		root[len(root)-1] ^= 1
		defer func() { root[len(root)-1] ^= 1 }()
		if _, err := file.openLinkHeap(headerAddr); !errors.Is(err, ErrFormat) {
			t.Fatalf("openLinkHeap: %v, want ErrFormat", err)
		}
	})
	t.Run("indirect cycle", func(t *testing.T) {
		binaryOrder.PutUint64(root[15+8*4:], rootAddr)
		putHeapTrailerChecksum(root)
		if _, err := file.openLinkHeap(headerAddr); !errors.Is(err, ErrFormat) {
			t.Fatalf("openLinkHeap: %v, want ErrFormat", err)
		}
	})
}

func TestDenseLinkHeapRejectsInvalidObjectRanges(t *testing.T) {
	heap := linkHeap{
		idSize: 3, offsetSize: 1, lengthSize: 1, managedCount: 2, maxObject: 16,
		blocks: []linkHeapBlock{{offset: 32, header: 5, data: make([]byte, 32)}},
	}
	for _, id := range [][]byte{{0, 32, 4}, {0, 62, 4}, {0, 37, 17}, {0, 37, 0}, {0, 37}, {1, 37, 2}} {
		if _, err := heap.linkObject(id); !errors.Is(err, ErrFormat) {
			t.Fatalf("linkObject(%v): %v, want ErrFormat", id, err)
		}
	}
	if _, err := heap.linkObject([]byte{0x20, 37, 3}); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("tiny object: %v, want ErrUnsupported", err)
	}
	for _, offset := range []byte{37, 38} {
		if _, err := heap.linkObject([]byte{0, offset, 3}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := heap.linkObject([]byte{0, 37, 4}); !errors.Is(err, ErrFormat) {
		t.Fatalf("conflicting size: %v, want ErrFormat", err)
	}
	if err := heap.validateObjects(); !errors.Is(err, ErrFormat) {
		t.Fatalf("overlapping ranges: %v, want ErrFormat", err)
	}
}

func putHeapTrailerChecksum(b []byte) {
	binary.LittleEndian.PutUint32(b[len(b)-4:], metadataChecksum(b[:len(b)-4]))
}
