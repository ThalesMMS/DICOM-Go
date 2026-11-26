package hdf5

import (
	"bytes"
	"encoding/binary"
	"errors"
	"slices"
	"testing"
)

func TestDenseLinkBTreeHDF5Fixture(t *testing.T) {
	data := fixture(t, "many_dense.h5")
	f := open(t, data)
	header := bytes.Index(data, []byte("BTHD"))
	if header < 0 {
		t.Fatal("dense link B-tree header missing from fixture")
	}
	var keys []uint64
	var offsets []uint64
	count, err := f.walkLinkBTree(uint64(header), linkNameIndex, 7, func(key uint64, id []byte) error {
		keys = append(keys, key)
		offsets = append(offsets, leUint(id[1:5]))
		return nil
	})
	if err != nil || count != 10 {
		t.Fatalf("walkLinkBTree = %d, %v, want 10, nil", count, err)
	}
	if !slices.IsSorted(keys) {
		t.Fatalf("name hashes not ordered: %x", keys)
	}
	slices.Sort(offsets)
	if want := []uint64{21, 33, 45, 57, 69, 81, 93, 105, 117, 129}; !slices.Equal(offsets, want) {
		t.Fatalf("heap offsets = %v, want %v", offsets, want)
	}
}

func TestDenseLinkBTreeInternalRecords(t *testing.T) {
	for _, kind := range []uint8{linkNameIndex, linkCreationOrderIndex} {
		image := testLinkBTree(kind)
		var keys []uint64
		count, err := openTestLinkBTree(image).walkLinkBTree(0, kind, 7, func(key uint64, id []byte) error {
			if expected := key; leUint(id) != expected {
				t.Errorf("record %d has ID %x, want %d", key, id, expected)
			}
			keys = append(keys, key)
			return nil
		})
		if err != nil || count != 15 {
			t.Fatalf("kind %d: walkLinkBTree = %d, %v, want 15, nil", kind, count, err)
		}
		for i, key := range keys {
			if key != uint64(i+1) {
				t.Fatalf("kind %d: ordered keys = %v", kind, keys)
			}
		}
	}
}

func TestDenseLinkBTreeRejectsDamagedIndex(t *testing.T) {
	const root = 64
	const rootUsed = 10 + 11 + 2*10
	const firstLeaf = 4 * 64
	const leafUsed = 10 + 3*11
	for _, test := range []struct {
		name    string
		corrupt func([]byte)
	}{
		{"header checksum", func(b []byte) { b[34] ^= 1 }},
		{"node checksum", func(b []byte) { b[root+rootUsed-1] ^= 1 }},
		{"resource limit", func(b []byte) {
			binary.LittleEndian.PutUint64(b[26:], maxLinks+1)
			testLinkBTreeSetChecksum(b[:38])
		}},
		{"root count", func(b []byte) {
			binary.LittleEndian.PutUint16(b[24:], 65535)
			testLinkBTreeSetChecksum(b[:38])
		}},
		{"root total", func(b []byte) {
			binary.LittleEndian.PutUint64(b[26:], 16)
			testLinkBTreeSetChecksum(b[:38])
		}},
		{"wrong child total", func(b []byte) {
			b[root+26] = 6
			testLinkBTreeSetChecksum(b[root : root+rootUsed])
		}},
		{"repeated node", func(b []byte) {
			copy(b[root+27:root+35], b[root+17:root+25])
			testLinkBTreeSetChecksum(b[root : root+rootUsed])
		}},
		{"cycle", func(b []byte) {
			binary.LittleEndian.PutUint64(b[root+17:], root)
			testLinkBTreeSetChecksum(b[root : root+rootUsed])
		}},
		{"misordered keys", func(b []byte) {
			binary.LittleEndian.PutUint32(b[firstLeaf+6:], 2)
			binary.LittleEndian.PutUint32(b[firstLeaf+17:], 1)
			testLinkBTreeSetChecksum(b[firstLeaf : firstLeaf+leafUsed])
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			b := testLinkBTree(linkNameIndex)
			test.corrupt(b)
			if _, err := openTestLinkBTree(b).walkLinkBTree(0, linkNameIndex, 7, func(uint64, []byte) error {
				return nil
			}); !errors.Is(err, ErrFormat) {
				t.Fatalf("walkLinkBTree: %v, want ErrFormat", err)
			}
		})
	}
}

func TestDenseLinkBTreeAllowsNameHashCollisionOnly(t *testing.T) {
	const firstLeaf = 4 * 64
	for _, test := range []struct {
		kind    uint8
		keySize int
		want    error
	}{
		{linkNameIndex, 4, nil},
		{linkCreationOrderIndex, 8, ErrFormat},
	} {
		// The last key in the first leaf matches its separating key.
		b := testLinkBTree(test.kind)
		recordSize := test.keySize + 7
		b[firstLeaf+6+2*recordSize] = 4
		testLinkBTreeSetChecksum(b[firstLeaf : firstLeaf+10+3*recordSize])
		if _, err := openTestLinkBTree(b).walkLinkBTree(0, test.kind, 7, func(uint64, []byte) error {
			return nil
		}); !errors.Is(err, test.want) {
			t.Fatalf("kind %d: walkLinkBTree: %v, want %v", test.kind, err, test.want)
		}
	}
}

func openTestLinkBTree(image []byte) *File {
	return &File{r: bytes.NewReader(image), size: int64(len(image)), offsetSize: 8, lengthSize: 8}
}

func testLinkBTree(kind uint8) []byte {
	const nodeSize = 64
	image := make([]byte, 8*nodeSize)
	header := image[:38]
	keySize := 4
	if kind == linkCreationOrderIndex {
		keySize = 8
	}
	recordSize := keySize + 7
	copy(header, "BTHD")
	header[5] = kind
	binary.LittleEndian.PutUint32(header[6:], nodeSize)
	binary.LittleEndian.PutUint16(header[10:], uint16(recordSize))
	binary.LittleEndian.PutUint16(header[12:], 2)
	header[14], header[15] = 100, 40
	binary.LittleEndian.PutUint64(header[16:], nodeSize)
	binary.LittleEndian.PutUint16(header[24:], 1)
	binary.LittleEndian.PutUint64(header[26:], 15)
	testLinkBTreeSetChecksum(header)

	type testChild struct {
		block int
		count byte
	}
	writeNode := func(block int, keys []byte, children []testChild, childTotal byte) {
		node := image[block*nodeSize : (block+1)*nodeSize]
		if len(children) > 0 {
			copy(node, "BTIN")
		} else {
			copy(node, "BTLF")
		}
		node[5] = kind
		pos := 6
		for _, key := range keys {
			node[pos] = key
			node[pos+keySize] = key
			pos += recordSize
		}
		for _, child := range children {
			binary.LittleEndian.PutUint64(node[pos:], uint64(child.block*nodeSize))
			node[pos+8] = child.count
			pos += 9
			if childTotal > 0 {
				node[pos] = childTotal
				pos++
			}
		}
		testLinkBTreeSetChecksum(node[:pos+4])
	}
	writeNode(1, []byte{8}, []testChild{{2, 1}, {3, 1}}, 7)
	writeNode(2, []byte{4}, []testChild{{4, 3}, {5, 3}}, 0)
	writeNode(3, []byte{12}, []testChild{{6, 3}, {7, 3}}, 0)
	writeNode(4, []byte{1, 2, 3}, nil, 0)
	writeNode(5, []byte{5, 6, 7}, nil, 0)
	writeNode(6, []byte{9, 10, 11}, nil, 0)
	writeNode(7, []byte{13, 14, 15}, nil, 0)
	return image
}

func testLinkBTreeSetChecksum(block []byte) {
	binary.LittleEndian.PutUint32(block[len(block)-4:], metadataChecksum(block[:len(block)-4]))
}
