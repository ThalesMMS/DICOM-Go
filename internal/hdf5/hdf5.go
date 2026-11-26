// Package hdf5 reads the small subset of HDF5 files that transform writers
// such as ITK's HDF5TransformIO (and so ANTs, Slicer and elastix) produce
// (#1252): groups of named datasets holding one-dimensional floating-point
// vectors and strings. It is a clean-room implementation of the HDF5 file
// format specification (version 3), written without the HDF5 library and
// without cgo.
//
// Supported:
//
//   - superblock versions 0 to 3 (a user block before the superblock is
//     skipped), offsets and lengths of 2, 4 or 8 bytes;
//   - object headers version 1 and 2, with continuation blocks;
//   - groups stored as symbol tables (version 1 B-trees, local heaps and
//     symbol table nodes), compact link messages or dense hard links
//     (version 2 B-trees and unfiltered managed fractal heap objects);
//   - datasets whose layout is compact, contiguous or chunked with a
//     version 1 B-tree index (layout messages versions 1 to 3), filtered by
//     deflate, shuffle or Fletcher-32 (the checksum is not verified);
//   - IEEE floating-point data of 4 or 8 bytes in either byte order, fixed
//     length strings and variable-length strings in global heaps.
//
// Not supported: filtered fractal heaps, tiny or huge heap link objects, the
// chunk indexes of layout version 4, external and soft links, shared or
// committed datatypes, and other filters. Dense heap and index checksums are
// verified where present. Unsupported combinations fail with ErrUnsupported.
// Every read is bounded by the file size and by
// the caller's element limit, so a damaged file fails with ErrFormat
// instead of exhausting memory.
//
// A File caches group listings and is not safe for concurrent use.
package hdf5

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
)

var (
	// ErrFormat reports a file that is not HDF5 or is damaged.
	ErrFormat = errors.New("hdf5: invalid file")
	// ErrUnsupported reports an HDF5 feature outside the supported subset.
	ErrUnsupported = errors.New("hdf5: unsupported feature")
	// ErrNotFound reports a path that names no object.
	ErrNotFound = errors.New("hdf5: no such object")
)

// Signature starts the superblock.
var Signature = []byte("\x89HDF\r\n\x1a\n")

// Limits on the structures walked, against loops and damaged files.
const (
	maxHeaderBlocks   = 256
	maxMessages       = 4096
	maxBTreeDepth     = 32
	maxBTreeNodes     = 1 << 16
	maxLinks          = 1 << 16
	maxChunkBytes     = 1 << 30
	maxPathComponents = 64
)

// File is an open HDF5 file.
type File struct {
	r          io.ReaderAt
	size       int64
	base       uint64
	offsetSize int
	lengthSize int
	root       uint64
	// Superblock is the superblock version (0 to 3).
	Superblock int
	links      map[uint64][]Link
}

// Link is a named hard link of a group.
type Link struct {
	Name    string
	address uint64
}

// IsHDF5 reports whether data starts with the HDF5 signature (at offset 0;
// files with a user block are recognized by Open).
func IsHDF5(data []byte) bool { return bytes.HasPrefix(data, Signature) }

// Open reads the superblock of an HDF5 file of size bytes. The superblock
// is searched at offsets 0, 512, 1024, 2048 and so on, as HDF5 does.
func Open(r io.ReaderAt, size int64) (*File, error) {
	f := &File{r: r, size: size, links: map[uint64][]Link{}}
	var at int64 = -1
	signature := make([]byte, len(Signature))
	for offset := int64(0); offset+int64(len(Signature)) <= size; offset = max(512, offset*2) {
		if _, err := r.ReadAt(signature, offset); err != nil {
			return nil, fmt.Errorf("%w: %v", ErrFormat, err)
		}
		if bytes.Equal(signature, Signature) {
			at = offset
			break
		}
	}
	if at < 0 {
		return nil, fmt.Errorf("%w: no HDF5 signature", ErrFormat)
	}
	header := make([]byte, min(int64(256), size-at))
	if _, err := r.ReadAt(header, at); err != nil && !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	d := &decoder{b: header, p: 8}
	f.Superblock = int(d.u8())
	switch f.Superblock {
	case 0, 1:
		d.skip(4) // free-space, root symbol table, reserved, shared header versions
		f.offsetSize, f.lengthSize = int(d.u8()), int(d.u8())
		d.skip(1 + 2 + 2 + 4) // reserved, leaf and internal K, consistency flags
		if f.Superblock == 1 {
			d.skip(4) // indexed storage K, reserved
		}
		if err := f.checkSizes(); err != nil {
			return nil, err
		}
		f.base = d.uint(f.offsetSize)
		d.skip(3 * f.offsetSize) // free space, end of file, driver block
		// Root group symbol table entry: name offset, object header.
		d.skip(f.offsetSize)
		f.root = d.uint(f.offsetSize)
	case 2, 3:
		f.offsetSize, f.lengthSize = int(d.u8()), int(d.u8())
		d.skip(1) // consistency flags
		if err := f.checkSizes(); err != nil {
			return nil, err
		}
		f.base = d.uint(f.offsetSize)
		d.skip(2 * f.offsetSize) // superblock extension, end of file
		f.root = d.uint(f.offsetSize)
	default:
		return nil, fmt.Errorf("%w: superblock version %d", ErrUnsupported, f.Superblock)
	}
	if d.err != nil {
		return nil, fmt.Errorf("%w: truncated superblock", ErrFormat)
	}
	if f.base == 0 && at > 0 {
		// Addresses are relative to the base, normally the superblock.
		f.base = uint64(at)
	}
	return f, nil
}

func (f *File) checkSizes() error {
	for _, n := range []int{f.offsetSize, f.lengthSize} {
		if n != 2 && n != 4 && n != 8 {
			return fmt.Errorf("%w: offset or length size %d", ErrFormat, n)
		}
	}
	return nil
}

// undefined reports the all-ones undefined address.
func (f *File) undefined(addr uint64) bool {
	if f.offsetSize == 8 {
		return addr == math.MaxUint64
	}
	return addr == (uint64(1)<<(8*f.offsetSize))-1
}

// read returns n bytes at a file address.
func (f *File) read(addr uint64, n int) ([]byte, error) {
	if f.undefined(addr) {
		return nil, fmt.Errorf("%w: undefined address", ErrFormat)
	}
	abs := f.base + addr
	if n < 0 || abs < addr || abs > uint64(f.size) || uint64(n) > uint64(f.size)-abs {
		return nil, fmt.Errorf("%w: %d bytes at %d beyond the end of the file", ErrFormat, n, abs)
	}
	b := make([]byte, n)
	if _, err := f.r.ReadAt(b, int64(abs)); err != nil && !(errors.Is(err, io.EOF) && n == 0) {
		return nil, fmt.Errorf("%w: %v", ErrFormat, err)
	}
	return b, nil
}

// decoder reads little-endian fields, recording the first overrun.
type decoder struct {
	b   []byte
	p   int
	err error
}

func (d *decoder) take(n int) []byte {
	if d.err != nil || n < 0 || n > len(d.b)-d.p {
		if d.err == nil {
			d.err = fmt.Errorf("%w: structure overruns its block", ErrFormat)
		}
		return make([]byte, max(n, 0))
	}
	out := d.b[d.p : d.p+n]
	d.p += n
	return out
}

func (d *decoder) skip(n int)        { d.take(n) }
func (d *decoder) u8() uint8         { return d.take(1)[0] }
func (d *decoder) u16() uint16       { return binary.LittleEndian.Uint16(d.take(2)) }
func (d *decoder) u32() uint32       { return binary.LittleEndian.Uint32(d.take(4)) }
func (d *decoder) left() int         { return len(d.b) - d.p }
func (d *decoder) uint(n int) uint64 { return leUint(d.take(n)) }

func leUint(b []byte) uint64 {
	var v uint64
	for i := len(b) - 1; i >= 0; i-- {
		v = v<<8 | uint64(b[i])
	}
	return v
}

// message is one object header message.
type message struct {
	kind  uint16
	flags uint8
	data  []byte
}

// Message types used here.
const (
	msgNil          = 0x00
	msgDataspace    = 0x01
	msgLinkInfo     = 0x02
	msgDatatype     = 0x03
	msgLink         = 0x06
	msgLayout       = 0x08
	msgFilters      = 0x0B
	msgContinuation = 0x10
	msgSymbolTable  = 0x11
)

// messages returns the messages of the object header at addr, following
// continuation blocks.
func (f *File) messages(addr uint64) ([]message, error) {
	prefix, err := f.read(addr, min(16, int(uint64(f.size)-min(uint64(f.size), f.base+addr))))
	if err != nil {
		return nil, err
	}
	type block struct {
		addr   uint64
		length uint64
		v2     bool
	}
	var blocks []block
	var v2 bool
	var creationOrder bool
	expected := -1
	switch {
	case len(prefix) >= 4 && string(prefix[:4]) == "OHDR":
		v2 = true
		head, err := f.read(addr, min(4+1+1+16+4+8, int(uint64(f.size)-(f.base+addr))))
		if err != nil {
			return nil, err
		}
		d := &decoder{b: head, p: 4}
		if version := d.u8(); version != 2 {
			return nil, fmt.Errorf("%w: object header version %d", ErrUnsupported, version)
		}
		flags := d.u8()
		creationOrder = flags&0x04 != 0
		if flags&0x20 != 0 {
			d.skip(16)
		}
		if flags&0x10 != 0 {
			d.skip(4)
		}
		size := d.uint(1 << (flags & 3))
		if d.err != nil {
			return nil, d.err
		}
		blocks = append(blocks, block{addr: addr + uint64(d.p), length: size, v2: true})
	case len(prefix) >= 16 && prefix[0] == 1:
		d := &decoder{b: prefix, p: 2}
		expected = int(d.u16())
		d.skip(4)
		size := d.u32()
		blocks = append(blocks, block{addr: addr + 16, length: uint64(size)})
	default:
		return nil, fmt.Errorf("%w: no object header at %d", ErrFormat, addr)
	}
	var out []message
	seen := map[uint64]bool{}
	for i := 0; i < len(blocks); i++ {
		b := blocks[i]
		if len(blocks) > maxHeaderBlocks || seen[b.addr] {
			return nil, fmt.Errorf("%w: looping object header continuations", ErrFormat)
		}
		seen[b.addr] = true
		if b.length > uint64(f.size) {
			return nil, fmt.Errorf("%w: object header block of %d bytes", ErrFormat, b.length)
		}
		data, err := f.read(b.addr, int(b.length))
		if err != nil {
			return nil, err
		}
		d := &decoder{b: data}
		if v2 && i > 0 {
			if string(d.take(4)) != "OCHK" {
				return nil, fmt.Errorf("%w: missing continuation signature", ErrFormat)
			}
			d.b = d.b[:max(len(d.b)-4, d.p)] // checksum
		}
		header := 8
		if v2 {
			header = 4
			if creationOrder {
				header = 6
			}
		}
		for d.left() >= header && d.err == nil {
			var m message
			var size int
			if v2 {
				m.kind = uint16(d.u8())
				size = int(d.u16())
				m.flags = d.u8()
				if creationOrder {
					d.skip(2)
				}
			} else {
				m.kind = d.u16()
				size = int(d.u16())
				m.flags = d.u8()
				d.skip(3)
			}
			m.data = d.take(size)
			if d.err != nil {
				return nil, d.err
			}
			if len(out) >= maxMessages {
				return nil, fmt.Errorf("%w: too many header messages", ErrFormat)
			}
			if m.kind == msgContinuation {
				c := &decoder{b: m.data}
				next := block{addr: c.uint(f.offsetSize), length: c.uint(f.lengthSize), v2: v2}
				if c.err != nil {
					return nil, c.err
				}
				blocks = append(blocks, next)
				continue
			}
			if m.kind != msgNil {
				out = append(out, m)
			}
			if expected >= 0 && len(out) >= expected {
				break
			}
		}
	}
	return out, nil
}

// Links lists the hard links of the group at path ("/" for the root) in
// storage order: symbol-table groups by name, compact groups in message order,
// and dense groups by creation-order index if present or by name hash.
func (f *File) Links(path string) ([]Link, error) {
	addr, err := f.lookup(path)
	if err != nil {
		return nil, err
	}
	return f.groupLinks(addr)
}

// Exists reports whether path names an object.
func (f *File) Exists(path string) bool {
	_, err := f.lookup(path)
	return err == nil
}

func (f *File) lookup(path string) (uint64, error) {
	addr := f.root
	parts := strings.Split(strings.Trim(path, "/"), "/")
	if len(parts) > maxPathComponents {
		return 0, fmt.Errorf("%w: path %q is too deep", ErrNotFound, path)
	}
	var ancestors [maxPathComponents]uint64
	count := 0
	for _, part := range parts {
		if part == "" {
			continue
		}
		ancestors[count] = addr
		count++
		links, err := f.groupLinks(addr)
		if err != nil {
			return 0, err
		}
		found := false
		for _, l := range links {
			if l.Name == part {
				addr, found = l.address, true
				break
			}
		}
		if !found {
			return 0, fmt.Errorf("%w: %s", ErrNotFound, path)
		}
		for _, ancestor := range ancestors[:count] {
			if addr == ancestor {
				return 0, fmt.Errorf("%w: cyclic group link in %q", ErrFormat, path)
			}
		}
	}
	return addr, nil
}

func (f *File) groupLinks(addr uint64) ([]Link, error) {
	if links, ok := f.links[addr]; ok {
		return links, nil
	}
	messages, err := f.messages(addr)
	if err != nil {
		return nil, err
	}
	var links []Link
	var info linkStorage
	hasInfo, symbolTable, compact := false, false, false
	for _, m := range messages {
		switch m.kind {
		case msgSymbolTable:
			if symbolTable || hasInfo || compact {
				return nil, fmt.Errorf("%w: conflicting group link storage", ErrFormat)
			}
			symbolTable = true
			d := &decoder{b: m.data}
			tree, heap := d.uint(f.offsetSize), d.uint(f.offsetSize)
			if d.err != nil {
				return nil, d.err
			}
			if links, err = f.symbolTable(tree, heap); err != nil {
				return nil, err
			}
		case msgLink:
			if symbolTable {
				return nil, fmt.Errorf("%w: conflicting group link storage", ErrFormat)
			}
			compact = true
			l, err := f.linkMessage(m.data)
			if err != nil {
				return nil, err
			}
			if l.hard {
				links = append(links, l.Link)
			}
		case msgLinkInfo:
			if hasInfo || symbolTable {
				return nil, fmt.Errorf("%w: conflicting group link storage", ErrFormat)
			}
			hasInfo = true
			info, err = f.linkInfo(m.data)
			if err != nil {
				return nil, err
			}
		}
	}
	if !hasInfo && !symbolTable && !compact {
		return nil, fmt.Errorf("%w: object at %d is not a group", ErrNotFound, addr)
	}
	if hasInfo && !f.undefined(info.heap) {
		if compact {
			return nil, fmt.Errorf("%w: both compact and dense links", ErrFormat)
		}
		links, err = f.denseLinks(addr, info)
		if err != nil {
			return nil, err
		}
	}
	f.links[addr] = links
	return links, nil
}

type decodedLink struct {
	Link
	order    uint64
	ordered  bool
	hard     bool
	trailing int
}

// linkMessage decodes a compact or dense link message. Non-hard links
// are identified but never resolved.
func (f *File) linkMessage(data []byte) (decodedLink, error) {
	var l decodedLink
	d := &decoder{b: data}
	version, flags := d.u8(), d.u8()
	if d.err != nil {
		return l, d.err
	}
	if version != 1 || flags&^0x1f != 0 {
		return l, fmt.Errorf("%w: link message version or flags", ErrUnsupported)
	}
	kind := uint8(0)
	if flags&0x08 != 0 {
		kind = d.u8()
	}
	l.ordered = flags&0x04 != 0
	if l.ordered {
		l.order = d.uint(8)
	}
	if flags&0x10 != 0 && d.u8() > 1 {
		return l, fmt.Errorf("%w: link name character set", ErrUnsupported)
	}
	length := d.uint(1 << (flags & 3))
	if d.err != nil {
		return l, d.err
	}
	if length > uint64(d.left()) {
		return l, fmt.Errorf("%w: link name overruns its message", ErrFormat)
	}
	l.Name = string(d.take(int(length)))
	if kind != 0 {
		return l, nil
	}
	l.hard = true
	l.address = d.uint(f.offsetSize)
	if d.err != nil {
		return l, d.err
	}
	l.trailing = d.left()
	return l, nil
}

// symbolTable lists an old-style group: a version 1 B-tree of symbol table
// nodes whose entries name links through a local heap.
func (f *File) symbolTable(tree, heapAddr uint64) ([]Link, error) {
	head, err := f.read(heapAddr, 8+2*f.lengthSize+f.offsetSize)
	if err != nil {
		return nil, err
	}
	d := &decoder{b: head}
	if string(d.take(4)) != "HEAP" {
		return nil, fmt.Errorf("%w: missing local heap", ErrFormat)
	}
	d.skip(4)
	heapSize := d.uint(f.lengthSize)
	d.skip(f.lengthSize)
	heapData := d.uint(f.offsetSize)
	if heapSize > uint64(f.size) {
		return nil, fmt.Errorf("%w: local heap of %d bytes", ErrFormat, heapSize)
	}
	heap, err := f.read(heapData, int(heapSize))
	if err != nil {
		return nil, err
	}
	var links []Link
	nodes := 0
	err = f.walkBTree(tree, 0, 0, &nodes, func(child uint64, _ []byte) error {
		node, err := f.read(child, 8)
		if err != nil {
			return err
		}
		if string(node[:4]) != "SNOD" {
			return fmt.Errorf("%w: missing symbol table node", ErrFormat)
		}
		count := int(binary.LittleEndian.Uint16(node[6:8]))
		entrySize := 2*f.offsetSize + 24
		entries, err := f.read(child+8, count*entrySize)
		if err != nil {
			return err
		}
		e := &decoder{b: entries}
		for i := 0; i < count; i++ {
			nameOffset := e.uint(f.offsetSize)
			address := e.uint(f.offsetSize)
			e.skip(24)
			if e.err != nil {
				return e.err
			}
			if nameOffset >= uint64(len(heap)) {
				return fmt.Errorf("%w: link name outside the local heap", ErrFormat)
			}
			name := heap[nameOffset:]
			if end := bytes.IndexByte(name, 0); end >= 0 {
				name = name[:end]
			}
			if len(links) >= maxLinks {
				return fmt.Errorf("%w: too many links", ErrFormat)
			}
			links = append(links, Link{Name: string(name), address: address})
		}
		return nil
	})
	return links, err
}

// walkBTree visits the leaf children of a version 1 B-tree of the given
// node type (0 groups, 1 chunks; dims is the chunk key dimensionality),
// in key order, passing each child's key.
func (f *File) walkBTree(addr uint64, kind uint8, dims int, nodes *int, visit func(child uint64, key []byte) error) error {
	return f.walkNode(addr, kind, dims, nodes, -1, 0, visit)
}

func (f *File) walkNode(addr uint64, kind uint8, dims int, nodes *int, wantLevel int, depth int, visit func(uint64, []byte) error) error {
	*nodes++
	if *nodes > maxBTreeNodes || depth > maxBTreeDepth {
		return fmt.Errorf("%w: B-tree too large or looping", ErrFormat)
	}
	head, err := f.read(addr, 8+2*f.offsetSize)
	if err != nil {
		return err
	}
	d := &decoder{b: head}
	if string(d.take(4)) != "TREE" {
		return fmt.Errorf("%w: missing B-tree node", ErrFormat)
	}
	if d.u8() != kind {
		return fmt.Errorf("%w: B-tree node type", ErrFormat)
	}
	level := int(d.u8())
	if wantLevel >= 0 && level != wantLevel {
		return fmt.Errorf("%w: B-tree levels", ErrFormat)
	}
	entries := int(d.u16())
	keySize := f.lengthSize
	if kind == 1 {
		keySize = 8 + 8*dims
	}
	body, err := f.read(addr+uint64(len(head)), entries*(keySize+f.offsetSize)+keySize)
	if err != nil {
		return err
	}
	b := &decoder{b: body}
	for i := 0; i < entries; i++ {
		key := b.take(keySize)
		child := b.uint(f.offsetSize)
		if b.err != nil {
			return b.err
		}
		if level > 0 {
			if err := f.walkNode(child, kind, dims, nodes, level-1, depth+1, visit); err != nil {
				return err
			}
		} else if err := visit(child, key); err != nil {
			return err
		}
	}
	return nil
}

// datatype is the part of a datatype message used here.
type datatype struct {
	class     uint8
	size      int
	bigEndian bool
	// vlenString marks a variable-length string.
	vlenString bool
}

func parseDatatype(data []byte) (datatype, error) {
	d := &decoder{b: data}
	head := d.u8()
	bits := d.take(3)
	t := datatype{class: head & 0x0F, size: int(d.u32())}
	if d.err != nil {
		return t, d.err
	}
	switch t.class {
	case 1: // floating point
		t.bigEndian = bits[0]&1 != 0
		if bits[0]&0x40 != 0 {
			return t, fmt.Errorf("%w: VAX floating point", ErrUnsupported)
		}
		if t.size != 4 && t.size != 8 {
			return t, fmt.Errorf("%w: %d-byte floating point", ErrUnsupported, t.size)
		}
	case 3: // fixed-length string
	case 9: // variable length
		if bits[0]&0x0F != 1 {
			return t, fmt.Errorf("%w: variable-length sequence", ErrUnsupported)
		}
		t.vlenString = true
	default:
		return t, fmt.Errorf("%w: datatype class %d", ErrUnsupported, t.class)
	}
	return t, nil
}

// layout is a parsed data layout message.
type layout struct {
	class   uint8 // 0 compact, 1 contiguous, 2 chunked
	address uint64
	size    uint64
	compact []byte
	chunk   []uint32 // chunk dimensions, the last the element size
}

func (f *File) parseLayout(data []byte) (layout, error) {
	d := &decoder{b: data}
	version := d.u8()
	var l layout
	switch version {
	case 1, 2:
		rank := int(d.u8())
		l.class = d.u8()
		d.skip(5)
		if l.class != 0 {
			l.address = d.uint(f.offsetSize)
		}
		for i := 0; i < rank; i++ {
			l.chunk = append(l.chunk, d.u32())
		}
		if l.class == 0 {
			l.compact = d.take(int(d.u32()))
		}
		if l.class != 2 {
			l.chunk = nil
		}
	case 3, 4:
		l.class = d.u8()
		switch l.class {
		case 0:
			l.compact = d.take(int(d.u16()))
		case 1:
			l.address = d.uint(f.offsetSize)
			l.size = d.uint(f.lengthSize)
		case 2:
			if version == 4 {
				return l, fmt.Errorf("%w: version 4 chunk indexes", ErrUnsupported)
			}
			rank := int(d.u8())
			l.address = d.uint(f.offsetSize)
			for i := 0; i < rank; i++ {
				l.chunk = append(l.chunk, d.u32())
			}
		default:
			return l, fmt.Errorf("%w: layout class %d", ErrUnsupported, l.class)
		}
	default:
		return l, fmt.Errorf("%w: layout message version %d", ErrUnsupported, version)
	}
	if d.err != nil {
		return l, d.err
	}
	if l.class == 2 && len(l.chunk) < 2 {
		return l, fmt.Errorf("%w: chunk dimensionality", ErrFormat)
	}
	return l, nil
}

// filter is one stage of a filter pipeline.
type filter struct {
	id     uint16
	values []uint32
}

func parseFilters(data []byte) ([]filter, error) {
	d := &decoder{b: data}
	version := d.u8()
	count := int(d.u8())
	if version == 1 {
		d.skip(6)
	} else if version != 2 {
		return nil, fmt.Errorf("%w: filter pipeline version %d", ErrUnsupported, version)
	}
	var out []filter
	for i := 0; i < count; i++ {
		var fl filter
		fl.id = d.u16()
		nameLength := 0
		if version == 1 || fl.id >= 256 {
			nameLength = int(d.u16())
		}
		d.skip(2) // flags
		values := int(d.u16())
		if version == 1 && nameLength%8 != 0 {
			nameLength += 8 - nameLength%8
		}
		d.skip(nameLength)
		for j := 0; j < values; j++ {
			fl.values = append(fl.values, d.u32())
		}
		if version == 1 && values%2 == 1 {
			d.skip(4)
		}
		if d.err != nil {
			return nil, d.err
		}
		switch fl.id {
		case 1, 2, 3:
		default:
			return nil, fmt.Errorf("%w: filter %d", ErrUnsupported, fl.id)
		}
		out = append(out, fl)
	}
	return out, nil
}

// unfilter reverses the pipeline on one chunk, skipping the filters set in
// mask; the result is at most limit bytes.
func unfilter(data []byte, filters []filter, mask uint32, limit int) ([]byte, error) {
	for i := len(filters) - 1; i >= 0; i-- {
		if i < 32 && mask&(1<<i) != 0 {
			continue
		}
		switch filters[i].id {
		case 1:
			z, err := zlib.NewReader(bytes.NewReader(data))
			if err != nil {
				return nil, fmt.Errorf("%w: deflate: %v", ErrFormat, err)
			}
			out, err := io.ReadAll(io.LimitReader(z, int64(limit)+1))
			if err != nil {
				return nil, fmt.Errorf("%w: deflate: %v", ErrFormat, err)
			}
			if len(out) > limit {
				return nil, fmt.Errorf("%w: chunk inflates beyond its size", ErrFormat)
			}
			data = out
		case 2:
			size := 0
			if len(filters[i].values) > 0 {
				size = int(filters[i].values[0])
			}
			if size > 1 {
				n := len(data) / size
				out := make([]byte, len(data))
				for b := 0; b < size; b++ {
					for e := 0; e < n; e++ {
						out[e*size+b] = data[b*n+e]
					}
				}
				copy(out[n*size:], data[n*size:])
				data = out
			}
		case 3:
			if len(data) < 4 {
				return nil, fmt.Errorf("%w: Fletcher-32 chunk", ErrFormat)
			}
			data = data[:len(data)-4]
		}
	}
	return data, nil
}

// dataset is the description of a dataset.
type dataset struct {
	elements uint64
	rank     int
	dtype    datatype
	layout   layout
	filters  []filter
}

func (f *File) dataset(path string) (*dataset, error) {
	addr, err := f.lookup(path)
	if err != nil {
		return nil, err
	}
	messages, err := f.messages(addr)
	if err != nil {
		return nil, err
	}
	ds := &dataset{}
	var haveSpace, haveType, haveLayout bool
	for _, m := range messages {
		if m.flags&0x02 != 0 && (m.kind == msgDatatype || m.kind == msgDataspace || m.kind == msgLayout || m.kind == msgFilters) {
			return nil, fmt.Errorf("%w: shared message in %s", ErrUnsupported, path)
		}
		switch m.kind {
		case msgDataspace:
			if ds.elements, ds.rank, err = f.parseDataspace(m.data); err != nil {
				return nil, err
			}
			haveSpace = true
		case msgDatatype:
			if ds.dtype, err = parseDatatype(m.data); err != nil {
				return nil, fmt.Errorf("%s: %w", path, err)
			}
			haveType = true
		case msgLayout:
			if ds.layout, err = f.parseLayout(m.data); err != nil {
				return nil, err
			}
			haveLayout = true
		case msgFilters:
			if ds.filters, err = parseFilters(m.data); err != nil {
				return nil, err
			}
		}
	}
	if !haveSpace || !haveType || !haveLayout {
		return nil, fmt.Errorf("%w: %s is not a dataset", ErrNotFound, path)
	}
	return ds, nil
}

// parseDataspace returns the number of elements and the rank.
func (f *File) parseDataspace(data []byte) (uint64, int, error) {
	d := &decoder{b: data}
	version := d.u8()
	rank := int(d.u8())
	d.skip(1) // flags
	switch version {
	case 1:
		d.skip(5)
	case 2:
		if kind := d.u8(); kind == 2 {
			return 0, 0, nil // null dataspace
		}
	default:
		return 0, 0, fmt.Errorf("%w: dataspace version %d", ErrUnsupported, version)
	}
	elements := uint64(1)
	for i := 0; i < rank; i++ {
		n := d.uint(f.lengthSize)
		if n != 0 && elements > math.MaxUint64/n {
			return 0, 0, fmt.Errorf("%w: dataspace too large", ErrFormat)
		}
		elements *= n
	}
	return elements, rank, d.err
}

// raw reads the dataset's bytes, elements × element size.
func (f *File) raw(ds *dataset, elementSize int) ([]byte, error) {
	total := ds.elements * uint64(elementSize)
	if total > uint64(f.size)*1024 || total > maxChunkBytes {
		return nil, fmt.Errorf("%w: dataset of %d bytes", ErrFormat, total)
	}
	l := ds.layout
	switch l.class {
	case 0:
		if uint64(len(l.compact)) < total {
			return nil, fmt.Errorf("%w: short compact dataset", ErrFormat)
		}
		return l.compact[:total], nil
	case 1:
		if f.undefined(l.address) {
			return make([]byte, total), nil // never written: the fill value
		}
		if len(ds.filters) > 0 {
			return nil, fmt.Errorf("%w: filtered contiguous dataset", ErrFormat)
		}
		return f.read(l.address, int(total))
	}
	// Chunked: only one-dimensional datasets.
	if len(l.chunk) != 2 || ds.rank != 1 {
		return nil, fmt.Errorf("%w: chunked dataset of rank %d", ErrUnsupported, ds.rank)
	}
	if int(l.chunk[1]) != elementSize {
		return nil, fmt.Errorf("%w: chunk element size %d, datatype %d", ErrFormat, l.chunk[1], elementSize)
	}
	out := make([]byte, total)
	if f.undefined(l.address) {
		return out, nil
	}
	chunkBytes := int(l.chunk[0]) * elementSize
	if l.chunk[0] == 0 || chunkBytes > maxChunkBytes {
		return nil, fmt.Errorf("%w: chunk of %d elements", ErrFormat, l.chunk[0])
	}
	nodes := 0
	err := f.walkBTree(l.address, 1, len(l.chunk), &nodes, func(child uint64, key []byte) error {
		k := &decoder{b: key}
		size := k.u32()
		mask := k.u32()
		offset := binary.LittleEndian.Uint64(k.take(8))
		if k.err != nil {
			return k.err
		}
		if offset >= ds.elements || offset%uint64(l.chunk[0]) != 0 {
			return fmt.Errorf("%w: chunk offset %d", ErrFormat, offset)
		}
		if uint64(size) > uint64(f.size) {
			return fmt.Errorf("%w: chunk of %d bytes", ErrFormat, size)
		}
		stored, err := f.read(child, int(size))
		if err != nil {
			return err
		}
		data, err := unfilter(stored, ds.filters, mask, chunkBytes)
		if err != nil {
			return err
		}
		start := offset * uint64(elementSize)
		n := min(uint64(len(data)), total-start, uint64(chunkBytes))
		copy(out[start:start+n], data[:n])
		return nil
	})
	return out, err
}

// ReadFloat64s reads a floating-point dataset of rank 0 or 1 holding at
// most limit elements.
func (f *File) ReadFloat64s(path string, limit int) ([]float64, error) {
	ds, err := f.dataset(path)
	if err != nil {
		return nil, err
	}
	if ds.rank > 1 {
		return nil, fmt.Errorf("%w: %s has rank %d", ErrUnsupported, path, ds.rank)
	}
	if ds.dtype.class != 1 {
		return nil, fmt.Errorf("%w: %s is not floating point", ErrUnsupported, path)
	}
	if ds.elements > uint64(limit) {
		return nil, fmt.Errorf("%w: %s has %d elements, at most %d are read", ErrFormat, path, ds.elements, limit)
	}
	raw, err := f.raw(ds, ds.dtype.size)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	var order binary.ByteOrder = binary.LittleEndian
	if ds.dtype.bigEndian {
		order = binary.BigEndian
	}
	out := make([]float64, ds.elements)
	for i := range out {
		if ds.dtype.size == 8 {
			out[i] = math.Float64frombits(order.Uint64(raw[8*i:]))
		} else {
			out[i] = float64(math.Float32frombits(order.Uint32(raw[4*i:])))
		}
	}
	return out, nil
}

// ReadString reads the first string of a string dataset of rank 0 or 1,
// fixed-length (trailing NULs and spaces removed) or variable-length.
func (f *File) ReadString(path string) (string, error) {
	ds, err := f.dataset(path)
	if err != nil {
		return "", err
	}
	if ds.elements < 1 || ds.rank > 1 {
		return "", fmt.Errorf("%w: %s holds no single string", ErrFormat, path)
	}
	switch {
	case ds.dtype.class == 3:
		one := *ds
		one.elements = 1
		raw, err := f.raw(&one, ds.dtype.size)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if end := bytes.IndexByte(raw, 0); end >= 0 {
			raw = raw[:end]
		}
		return strings.TrimRight(string(raw), " "), nil
	case ds.dtype.vlenString:
		size := 4 + f.offsetSize + 4
		one := *ds
		one.elements = 1
		raw, err := f.raw(&one, size)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		d := &decoder{b: raw}
		length := d.u32()
		collection := d.uint(f.offsetSize)
		index := d.u32()
		value, err := f.globalHeapObject(collection, index)
		if err != nil {
			return "", fmt.Errorf("%s: %w", path, err)
		}
		if uint64(length) > uint64(len(value)) {
			return "", fmt.Errorf("%w: %s: string longer than its heap object", ErrFormat, path)
		}
		value = value[:length]
		if end := bytes.IndexByte(value, 0); end >= 0 {
			value = value[:end]
		}
		return string(value), nil
	}
	return "", fmt.Errorf("%w: %s is not a string", ErrUnsupported, path)
}

// globalHeapObject returns object index of the global heap collection at
// addr.
func (f *File) globalHeapObject(addr uint64, index uint32) ([]byte, error) {
	head, err := f.read(addr, 8+f.lengthSize)
	if err != nil {
		return nil, err
	}
	d := &decoder{b: head}
	if string(d.take(4)) != "GCOL" {
		return nil, fmt.Errorf("%w: missing global heap collection", ErrFormat)
	}
	d.skip(4)
	size := d.uint(f.lengthSize)
	if size > uint64(f.size) || size < uint64(len(head)) {
		return nil, fmt.Errorf("%w: global heap collection of %d bytes", ErrFormat, size)
	}
	collection, err := f.read(addr, int(size))
	if err != nil {
		return nil, err
	}
	c := &decoder{b: collection, p: len(head)}
	for c.left() >= 8+f.lengthSize {
		id := c.u16()
		c.skip(6)
		objectSize := c.uint(f.lengthSize)
		if id == 0 || c.err != nil {
			break
		}
		if objectSize > uint64(c.left()) {
			return nil, fmt.Errorf("%w: global heap object overruns its collection", ErrFormat)
		}
		data := c.take(int(objectSize))
		if pad := int(objectSize % 8); pad != 0 {
			c.skip(min(8-pad, c.left()))
		}
		if uint32(id) == index {
			return data, nil
		}
	}
	return nil, fmt.Errorf("%w: global heap object %d not found", ErrFormat, index)
}
