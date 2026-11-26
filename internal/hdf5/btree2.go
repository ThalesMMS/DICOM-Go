package hdf5

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/bits"
)

const (
	linkNameIndex          = 5
	linkCreationOrderIndex = 6
	denseLinkIDSize        = 7
	denseNodePrefix        = 6 // Signature, version and kind.
	denseNodeOverhead      = denseNodePrefix + 4
	maxDenseBTreeNodeBytes = 1 << 20
)

type linkIndexLevel struct {
	capacity       int
	childTotalSize int
}

type linkBTree struct {
	file           *File
	kind           uint8
	keySize        int
	recordSize     int
	nodeSize       int
	childCountSize int
	levels         []linkIndexLevel
	seen           map[uint64]bool
	remaining      int
	lastKey        uint64
	visited        bool
	visit          func(uint64, []byte) error
}

// walkLinkBTree visits the records of one dense group index in key order.
// Internal separators are also records. Equal name hashes are valid, while
// creation-order keys must be unique. See HDF5 File Format Specification v3,
// section III.A.2.
func (f *File) walkLinkBTree(addr uint64, kind uint8, idSize int, visit func(key uint64, id []byte) error) (int, error) {
	var keySize int
	switch kind {
	case linkNameIndex:
		keySize = 4
	case linkCreationOrderIndex:
		keySize = 8
	default:
		return 0, fmt.Errorf("%w: v2 link B-tree type %d", ErrUnsupported, kind)
	}
	if idSize != denseLinkIDSize {
		return 0, fmt.Errorf("%w: dense link B-tree ID size %d", ErrUnsupported, idSize)
	}
	header, err := f.read(addr, 22+f.offsetSize+f.lengthSize)
	if err != nil {
		return 0, err
	}
	d := decoder{b: header}
	if string(d.take(4)) != "BTHD" {
		return 0, fmt.Errorf("%w: missing dense link B-tree header", ErrFormat)
	}
	if version := d.u8(); version != 0 {
		return 0, fmt.Errorf("%w: link B-tree version %d", ErrUnsupported, version)
	}
	if d.u8() != kind {
		return 0, fmt.Errorf("%w: incorrect dense link B-tree type", ErrFormat)
	}
	nodeSize := d.u32()
	recordSize, depth := int(d.u16()), int(d.u16())
	d.skip(2) // Split and merge thresholds are used by the writer.
	root, rootRecords := d.uint(f.offsetSize), int(d.u16())
	total := d.uint(f.lengthSize)
	if checksum := d.u32(); metadataChecksum(header[:len(header)-4]) != checksum {
		return 0, fmt.Errorf("%w: dense link B-tree header checksum", ErrFormat)
	}
	if recordSize != keySize+idSize || nodeSize < uint32(denseNodeOverhead+recordSize) ||
		nodeSize > maxDenseBTreeNodeBytes || depth > maxBTreeDepth || total > maxLinks {
		return 0, fmt.Errorf("%w: dense link B-tree dimensions or record limit", ErrFormat)
	}
	if total == 0 && depth == 0 && rootRecords == 0 && f.undefined(root) {
		return 0, nil
	}
	if f.undefined(root) || rootRecords == 0 || uint64(rootRecords) > total {
		return 0, fmt.Errorf("%w: invalid dense link B-tree root", ErrFormat)
	}
	leafCapacity := (int(nodeSize) - denseNodeOverhead) / recordSize
	if leafCapacity > math.MaxUint16 {
		return 0, fmt.Errorf("%w: dense link B-tree leaf capacity", ErrFormat)
	}
	index := linkBTree{
		file:           f,
		kind:           kind,
		keySize:        keySize,
		recordSize:     recordSize,
		nodeSize:       int(nodeSize),
		childCountSize: linkIndexUintBytes(uint64(leafCapacity)),
		levels:         make([]linkIndexLevel, depth+1),
		seen:           make(map[uint64]bool),
		remaining:      int(total),
		visit:          visit,
	}
	index.levels[0].capacity = leafCapacity
	cumulative := uint64(leafCapacity)
	for level := 1; level <= depth; level++ {
		// Pointers to leaves omit the subtree total. The stored width for
		// every child-local count comes from the leaf capacity.
		totalSize := 0
		if level > 1 {
			totalSize = linkIndexUintBytes(cumulative)
		}
		pointerSize := f.offsetSize + index.childCountSize + totalSize
		space := index.nodeSize - denseNodeOverhead - pointerSize
		if space < recordSize+pointerSize {
			return 0, fmt.Errorf("%w: dense link B-tree internal node capacity", ErrFormat)
		}
		capacity := space / (recordSize + pointerSize)
		index.levels[level] = linkIndexLevel{capacity, totalSize}
		// Each interior record adds a separator, plus a child for each
		// gap. Saturate at the largest encodable count.
		if cumulative > (math.MaxUint64-uint64(capacity))/uint64(capacity+1) {
			cumulative = math.MaxUint64
		} else {
			cumulative = uint64(capacity+1)*cumulative + uint64(capacity)
		}
	}
	if err := index.walk(root, depth, rootRecords, int(total)); err != nil {
		return 0, err
	}
	return int(total), nil
}

func linkIndexUintBytes(n uint64) int { return (bits.Len64(n) + 7) / 8 }

func (t *linkBTree) walk(addr uint64, depth, records, expected int) error {
	if t.seen[addr] || len(t.seen) >= maxBTreeNodes {
		return fmt.Errorf("%w: repeated dense link B-tree node or node limit", ErrFormat)
	}
	t.seen[addr] = true
	level := t.levels[depth]
	if records <= 0 || records > level.capacity || records > expected || expected > t.remaining {
		return fmt.Errorf("%w: invalid dense link B-tree node count", ErrFormat)
	}
	absolute := t.file.base + addr
	if absolute < addr || absolute > uint64(t.file.size) || uint64(t.nodeSize) > uint64(t.file.size)-absolute {
		return fmt.Errorf("%w: dense link B-tree node outside file", ErrFormat)
	}
	signature := "BTLF"
	used := denseNodeOverhead + records*t.recordSize
	if depth > 0 {
		signature = "BTIN"
		used += (records + 1) * (t.file.offsetSize + t.childCountSize + level.childTotalSize)
	}
	node, err := t.file.read(addr, used)
	if err != nil {
		return err
	}
	if string(node[:4]) != signature || node[4] != 0 || node[5] != t.kind {
		return fmt.Errorf("%w: invalid dense link B-tree node signature, version or type", ErrFormat)
	}
	// The checksum precedes any unused padding in the allocated node.
	if metadataChecksum(node[:len(node)-4]) != binary.LittleEndian.Uint32(node[len(node)-4:]) {
		return fmt.Errorf("%w: dense link B-tree node checksum", ErrFormat)
	}
	body := node[denseNodePrefix : len(node)-4]
	keys := body[:records*t.recordSize]
	children := decoder{b: body[len(keys):]}
	start := t.remaining
	for i := 0; i <= records; i++ {
		if depth > 0 {
			child := children.uint(t.file.offsetSize)
			childRecords := int(children.uint(t.childCountSize))
			childTotal := uint64(childRecords)
			if level.childTotalSize > 0 {
				childTotal = children.uint(level.childTotalSize)
			}
			if childTotal > uint64(t.remaining) {
				return fmt.Errorf("%w: invalid dense link B-tree subtree count", ErrFormat)
			}
			if err := t.walk(child, depth-1, childRecords, int(childTotal)); err != nil {
				return err
			}
		}
		if i == records {
			break
		}
		record := keys[i*t.recordSize : (i+1)*t.recordSize]
		key := leUint(record[:t.keySize])
		if t.visited && (key < t.lastKey || key == t.lastKey && t.kind == linkCreationOrderIndex) {
			return fmt.Errorf("%w: unordered dense link B-tree records", ErrFormat)
		}
		if t.remaining == 0 {
			return fmt.Errorf("%w: too many dense link B-tree records", ErrFormat)
		}
		t.lastKey, t.visited = key, true
		t.remaining--
		if err := t.visit(key, record[t.keySize:]); err != nil {
			return err
		}
	}
	if start-t.remaining != expected {
		return fmt.Errorf("%w: dense link B-tree subtree count mismatch", ErrFormat)
	}
	return nil
}
