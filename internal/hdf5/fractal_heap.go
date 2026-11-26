package hdf5

import (
	"encoding/binary"
	"fmt"
	"math/bits"
	"sort"
)

// This profile only reads managed link objects in unfiltered heaps. The
// budget covers all direct and indirect block bytes read for one group.
const maxLinkHeapBytes = 16 << 20

type linkHeap struct {
	idSize       int
	managedCount uint64
	offsetSize   int
	lengthSize   int
	maxObject    uint64
	blocks       []linkHeapBlock
	objects      map[uint64]uint64 // logical offsets visited through the name index
}

type linkHeapBlock struct {
	offset uint64
	header int
	data   []byte
}

type linkHeapReader struct {
	file        *File
	heap        *linkHeap
	address     uint64
	startSize   uint64
	maxDirect   uint64
	width       int
	checksummed bool
	remaining   uint64
	allocated   uint64
	seen        map[uint64]bool
}

func (f *File) openLinkHeap(addr uint64) (*linkHeap, error) {
	const headerFixedSize = 26
	data, err := f.read(addr, headerFixedSize+3*f.offsetSize+12*f.lengthSize)
	if err != nil {
		return nil, err
	}
	d := &decoder{b: data}
	if string(d.take(4)) != "FRHP" {
		return nil, fmt.Errorf("%w: missing fractal heap header", ErrFormat)
	}
	if d.u8() != 0 {
		return nil, fmt.Errorf("%w: fractal heap header version", ErrUnsupported)
	}
	idSize, filters := int(d.u16()), d.u16()
	flags := d.u8()
	if filters != 0 || flags&^uint8(3) != 0 {
		return nil, fmt.Errorf("%w: filtered fractal heap or unknown heap flags", ErrUnsupported)
	}
	maxObject := uint64(d.u32())
	d.skip(f.lengthSize + f.offsetSize) // huge object ID and tree address
	freeSpace := d.uint(f.lengthSize)
	d.skip(f.offsetSize) // free-space manager address
	space, allocated := d.uint(f.lengthSize), d.uint(f.lengthSize)
	iterator, managed := d.uint(f.lengthSize), d.uint(f.lengthSize)
	hugeBytes, huge := d.uint(f.lengthSize), d.uint(f.lengthSize)
	tinyBytes, tiny := d.uint(f.lengthSize), d.uint(f.lengthSize)
	width := int(d.u16())
	start, maxDirect := d.uint(f.lengthSize), d.uint(f.lengthSize)
	maxIndex, initialRows := int(d.u16()), int(d.u16())
	root, rootRows := d.uint(f.offsetSize), int(d.u16())
	if d.u32() != metadataChecksum(data[:len(data)-4]) {
		return nil, fmt.Errorf("%w: fractal heap header checksum", ErrFormat)
	}
	if hugeBytes != 0 || huge != 0 || tinyBytes != 0 || tiny != 0 {
		return nil, fmt.Errorf("%w: huge or tiny fractal heap link object", ErrUnsupported)
	}
	if managed > maxLinks || allocated > maxLinkHeapBytes {
		return nil, fmt.Errorf("%w: fractal heap resource limit", ErrFormat)
	}
	if freeSpace > space || allocated > space || iterator > space ||
		!heapPowerOfTwo(uint64(width)) || !heapPowerOfTwo(start) || !heapPowerOfTwo(maxDirect) ||
		start > maxDirect || maxIndex == 0 || maxIndex > 8*f.lengthSize {
		return nil, fmt.Errorf("%w: fractal heap dimensions", ErrFormat)
	}
	heapOffsetSize := (maxIndex + 7) / 8
	heapLengthSize := (bits.Len64(maxObject) + 7) / 8
	directHeader := 5 + f.offsetSize + heapOffsetSize
	if flags&2 != 0 {
		directHeader += 4
	}
	if start <= uint64(directHeader) || maxObject == 0 || maxObject > maxDirect-uint64(directHeader) {
		return nil, fmt.Errorf("%w: fractal heap managed object size", ErrFormat)
	}
	if idSize != 1+heapOffsetSize+heapLengthSize {
		return nil, fmt.Errorf("%w: fractal heap managed ID size", ErrUnsupported)
	}
	firstRowBits := bits.TrailingZeros64(start) + bits.TrailingZeros(uint(width))
	maxRows := maxIndex - firstRowBits + 1
	if maxRows < 1 || rootRows > maxRows || initialRows > maxRows {
		return nil, fmt.Errorf("%w: fractal heap doubling table rows", ErrFormat)
	}
	heap := &linkHeap{
		idSize: idSize, managedCount: managed, offsetSize: heapOffsetSize,
		lengthSize: heapLengthSize, maxObject: maxObject,
	}
	if f.undefined(root) {
		if space != 0 || allocated != 0 || managed != 0 || rootRows != 0 {
			return nil, fmt.Errorf("%w: missing fractal heap root", ErrFormat)
		}
		return heap, nil
	}
	rootSpan := start
	if rootRows != 0 {
		shift := firstRowBits + rootRows - 1
		if shift >= 64 {
			return nil, fmt.Errorf("%w: fractal heap address space overflows", ErrFormat)
		}
		rootSpan = uint64(1) << shift
	}
	if space != rootSpan {
		return nil, fmt.Errorf("%w: inconsistent fractal heap address space", ErrFormat)
	}
	reader := linkHeapReader{
		file: f, heap: heap, address: addr, startSize: start, maxDirect: maxDirect,
		width: width, checksummed: flags&2 != 0, remaining: maxLinkHeapBytes - uint64(len(data)),
		allocated: allocated, seen: map[uint64]bool{addr: true},
	}
	if rootRows == 0 {
		err = reader.direct(root, 0, start)
	} else {
		err = reader.indirect(root, 0, rootRows)
	}
	if err != nil {
		return nil, err
	}
	if reader.allocated != 0 {
		return nil, fmt.Errorf("%w: fractal heap allocated byte count", ErrFormat)
	}
	return heap, nil
}

func heapPowerOfTwo(n uint64) bool { return n != 0 && n&(n-1) == 0 }

func (r *linkHeapReader) block(addr uint64, size uint64, signature string, offset uint64) ([]byte, *decoder, error) {
	if r.seen[addr] || len(r.seen) >= maxBTreeNodes || size > r.remaining {
		return nil, nil, fmt.Errorf("%w: fractal heap cycle, repeated block or resource limit", ErrFormat)
	}
	r.seen[addr] = true
	r.remaining -= size
	data, err := r.file.read(addr, int(size))
	if err != nil {
		return nil, nil, err
	}
	d := &decoder{b: data}
	if string(d.take(4)) != signature || d.u8() != 0 ||
		d.uint(r.file.offsetSize) != r.address || d.uint(r.heap.offsetSize) != offset || d.err != nil {
		return nil, nil, fmt.Errorf("%w: invalid fractal heap %s block at address %d", ErrFormat, signature, addr)
	}
	return data, d, nil
}

func (r *linkHeapReader) direct(addr, offset, size uint64) error {
	if size > r.allocated {
		return fmt.Errorf("%w: direct block exceeds fractal heap allocated space", ErrFormat)
	}
	data, d, err := r.block(addr, size, "FHDB", offset)
	if err != nil {
		return err
	}
	if r.checksummed {
		field := data[d.p : d.p+4]
		want := d.u32()
		clear(field)
		got := metadataChecksum(data)
		binary.LittleEndian.PutUint32(field, want)
		if got != want {
			return fmt.Errorf("%w: fractal heap direct block checksum", ErrFormat)
		}
	}
	r.allocated -= size
	r.heap.blocks = append(r.heap.blocks, linkHeapBlock{offset: offset, header: d.p, data: data})
	return nil
}

func (r *linkHeapReader) indirect(addr, offset uint64, rows int) error {
	header := 5 + r.file.offsetSize + r.heap.offsetSize
	size := header + rows*r.width*r.file.offsetSize + 4
	data, d, err := r.block(addr, uint64(size), "FHIB", offset)
	if err != nil {
		return err
	}
	if binary.LittleEndian.Uint32(data[size-4:]) != metadataChecksum(data[:size-4]) {
		return fmt.Errorf("%w: fractal heap indirect block checksum", ErrFormat)
	}

	// Rows zero and one use the same size. Subsequent rows double it.
	// Each child indirect block spans its whole parent slot, so its row
	// count uses that span divided by the width of its table.
	firstRowBits := bits.TrailingZeros64(r.startSize) + bits.TrailingZeros(uint(r.width))
	childOffset, childSize := offset, r.startSize
	for row := 0; row < rows; row++ {
		if row > 1 {
			childSize *= 2
		}
		for column := 0; column < r.width; column++ {
			child := d.uint(r.file.offsetSize)
			if !r.file.undefined(child) {
				if childSize <= r.maxDirect {
					err = r.direct(child, childOffset, childSize)
				} else {
					childRows := bits.Len64(childSize) - firstRowBits
					if childRows < 1 || childRows >= rows {
						return fmt.Errorf("%w: invalid child fractal heap span", ErrFormat)
					}
					err = r.indirect(child, childOffset, childRows)
				}
				if err != nil {
					return err
				}
			}
			childOffset += childSize
		}
	}
	return nil
}

func (h *linkHeap) linkObject(id []byte) ([]byte, error) {
	if len(id) != h.idSize {
		return nil, fmt.Errorf("%w: invalid fractal heap ID length", ErrFormat)
	}
	if id[0]&0xf0 != 0 {
		return nil, fmt.Errorf("%w: fractal heap ID type or version", ErrUnsupported)
	}
	if id[0] != 0 {
		return nil, fmt.Errorf("%w: fractal heap ID flags", ErrFormat)
	}
	offset := leUint(id[1 : 1+h.offsetSize])
	size := leUint(id[1+h.offsetSize : 1+h.offsetSize+h.lengthSize])
	if size == 0 || size > h.maxObject {
		return nil, fmt.Errorf("%w: fractal heap object size", ErrFormat)
	}
	// Blocks are collected in logical offset order by indirect().
	i := sort.Search(len(h.blocks), func(i int) bool { return h.blocks[i].offset > offset })
	if i == 0 {
		return nil, fmt.Errorf("%w: fractal heap link before first block", ErrFormat)
	}
	block := h.blocks[i-1]
	at := offset - block.offset
	if at < uint64(block.header) || at >= uint64(len(block.data)) || size > uint64(len(block.data))-at {
		return nil, fmt.Errorf("%w: fractal heap link outside block data", ErrFormat)
	}
	if known, ok := h.objects[offset]; ok && known != size {
		return nil, fmt.Errorf("%w: conflicting fractal heap object sizes", ErrFormat)
	}
	if h.objects == nil {
		h.objects = make(map[uint64]uint64)
	}
	h.objects[offset] = size
	if uint64(len(h.objects)) > h.managedCount {
		return nil, fmt.Errorf("%w: too many fractal heap objects", ErrFormat)
	}
	return block.data[at : at+size], nil
}

// validateObjects checks the complete set of IDs visited from the name index.
func (h *linkHeap) validateObjects() error {
	if uint64(len(h.objects)) != h.managedCount {
		return fmt.Errorf("%w: fractal heap object count", ErrFormat)
	}
	offsets := make([]uint64, 0, len(h.objects))
	for offset := range h.objects {
		offsets = append(offsets, offset)
	}
	sort.Slice(offsets, func(i, j int) bool { return offsets[i] < offsets[j] })
	var end uint64
	for _, offset := range offsets {
		if offset < end {
			return fmt.Errorf("%w: overlapping fractal heap objects", ErrFormat)
		}
		end = offset + h.objects[offset]
	}
	return nil
}
