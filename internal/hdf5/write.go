package hdf5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strings"
)

// WriteDataset is the private library transport for a rank1 contiguous
// float64 vector, or one ASCII variable-length metadata string.
// A nonnil Text selects the bounded string subset; no string arrays or generic
// variable-length sequences are supported.
// Float64At borrows stable input; it must not mutate during Write. This
// writes array elements, not evaluations of a spatial transform.
type WriteDataset struct {
	Path      string
	Text      *string
	Count     int
	Float64At func(int) float64
}

// ErrWriteBudget reports the exact encoded-file byte cap, not process RSS.
var ErrWriteBudget = errors.New("hdf5: encoded file exceeds byte budget")

const writeObjects = 8192
const writeDatasets = 4096
const writeVectorElements = 1 << 24
const writeChunk = 64 * 1024
const undefinedWrite = math.MaxUint64

type writeObject struct {
	name                  string
	address, raw          uint64
	stringHeap            uint64
	data                  *WriteDataset
	children              map[string]*writeObject
	ordered               []*writeObject
	heap                  []byte
	names                 map[string]uint64
	heapAddress, heapData uint64
	symbols               []*writeSymbol
	trees                 []*writeTree
	tree                  *writeTree
}
type writeSymbol struct {
	address, lo, hi uint64
	objects         []*writeObject
}
type writeTree struct {
	address, lo, hi, left, right uint64
	level                        uint8
	symbols                      []*writeSymbol
	children                     []*writeTree
}
type writePart struct {
	at, size uint64
	bytes    func() []byte
	data     *WriteDataset
}
type writePlan struct {
	objects []*writeObject
	parts   []writePart
	end     uint64
	root    *writeObject
	max     uint64
}

func asciiWrite(s string) bool {
	for _, v := range []byte(s) {
		if v == 0 || v > 127 {
			return false
		}
	}
	return true
}
func writeCheckedMul(a, b uint64) (uint64, error) {
	if b != 0 && a > math.MaxUint64/b {
		return 0, ErrFormat
	}
	return a * b, nil
}
func (p *writePlan) alloc(size uint64) (uint64, error) {
	if size > math.MaxInt64 || p.end > math.MaxInt64-7 {
		return 0, ErrWriteBudget
	}
	at := (p.end + 7) &^ uint64(7)
	if size > math.MaxInt64-at {
		return 0, ErrWriteBudget
	}
	end := at + size
	if end > p.max {
		return 0, ErrWriteBudget
	}
	p.end = end
	return at, nil
}
func (p *writePlan) metadata(size uint64, makeBytes func() []byte) (uint64, error) {
	at, err := p.alloc(size)
	if err == nil {
		p.parts = append(p.parts, writePart{at: at, size: size, bytes: makeBytes})
	}
	return at, err
}
func planWrite(ctx context.Context, ds []WriteDataset, maxBytes int64) (*writePlan, error) {
	if maxBytes <= 0 || len(ds) == 0 || len(ds) > writeDatasets {
		return nil, fmt.Errorf("%w: writer dataset/budget admission", ErrFormat)
	}
	p := &writePlan{end: 96, max: uint64(maxBytes), root: &writeObject{children: map[string]*writeObject{}}}
	if p.end > p.max {
		return nil, ErrWriteBudget
	}
	count := 1
	for i := range ds {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		d := &ds[i]
		if len(d.Path) > 512 || !strings.HasPrefix(d.Path, "/") || !asciiWrite(d.Path) {
			return nil, fmt.Errorf("%w: dataset path", ErrFormat)
		}
		names := strings.Split(d.Path[1:], "/")
		if len(names) == 0 || len(names) > 8 {
			return nil, ErrFormat
		}
		if d.Text != nil {
			if d.Count != 0 || d.Float64At != nil || len(*d.Text) > 1024 || !asciiWrite(*d.Text) {
				return nil, fmt.Errorf("%w: metadata string", ErrFormat)
			}
		} else if d.Count < 0 || d.Count > writeVectorElements || (d.Count > 0 && d.Float64At == nil) {
			return nil, fmt.Errorf("%w: vector admission", ErrFormat)
		}
		obj := p.root
		for j, name := range names {
			if name == "" || name == "." || name == ".." || len(name) > 256 || obj.data != nil {
				return nil, fmt.Errorf("%w: dataset/group path collision", ErrFormat)
			}
			child := obj.children[name]
			if child == nil {
				count++
				if count > writeObjects {
					return nil, fmt.Errorf("%w: metadata object limit", ErrFormat)
				}
				child = &writeObject{name: name, children: map[string]*writeObject{}}
				obj.children[name] = child
			}
			obj = child
			if j == len(names)-1 {
				if obj.data != nil || len(obj.children) > 0 {
					return nil, fmt.Errorf("%w: duplicate dataset/group", ErrFormat)
				}
				obj.data = d
			}
		}
	}
	var visit func(*writeObject)
	visit = func(o *writeObject) {
		p.objects = append(p.objects, o)
		names := make([]string, 0, len(o.children))
		for name := range o.children {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			child := o.children[name]
			o.ordered = append(o.ordered, child)
			visit(child)
		}
	}
	visit(p.root)
	for _, o := range p.objects {
		size := uint64(40)
		if o.data != nil {
			size = 104
		}
		current := o
		at, err := p.metadata(size, func() []byte { return current.header() })
		if err != nil {
			return nil, err
		}
		o.address = at
	}
	for _, o := range p.objects {
		if o.data != nil {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		o.names = map[string]uint64{}
		o.heap = make([]byte, 8)
		for _, child := range o.ordered {
			o.names[child.name] = uint64(len(o.heap))
			o.heap = append(o.heap, []byte(child.name)...)
			o.heap = append(o.heap, 0)
			for len(o.heap)%8 != 0 {
				o.heap = append(o.heap, 0)
			}
		}
		freeOffset := len(o.heap)
		o.heap = append(o.heap, make([]byte, 16)...)
		put64(o.heap, freeOffset, 1)
		put64(o.heap, freeOffset+8, 16)
		current := o
		at, err := p.metadata(32, func() []byte {
			b := make([]byte, 32)
			copy(b, "HEAP")
			put64(b, 8, uint64(len(current.heap)))
			put64(b, 16, uint64(freeOffset))
			put64(b, 24, current.heapData)
			return b
		})
		if err != nil {
			return nil, err
		}
		o.heapAddress = at
		at, err = p.metadata(uint64(len(o.heap)), func() []byte { return current.heap })
		if err != nil {
			return nil, err
		}
		o.heapData = at
		previous := uint64(0)
		for start := 0; start < len(o.ordered); start += 8 {
			end := min(start+8, len(o.ordered))
			symbol := &writeSymbol{lo: previous, hi: o.names[o.ordered[end-1].name], objects: o.ordered[start:end]}
			previous = symbol.hi
			sym := symbol
			at, err := p.metadata(328, func() []byte { return symbolBytes(sym, current.names) })
			if err != nil {
				return nil, err
			}
			symbol.address = at
			o.symbols = append(o.symbols, symbol)
		}
		var level []*writeTree
		for start := 0; start < len(o.symbols); start += 32 {
			end := min(start+32, len(o.symbols))
			ss := o.symbols[start:end]
			t := &writeTree{lo: ss[0].lo, hi: ss[len(ss)-1].hi, left: undefinedWrite, right: undefinedWrite, symbols: ss}
			level = append(level, t)
		}
		if len(level) == 0 {
			level = []*writeTree{{left: undefinedWrite, right: undefinedWrite}}
		}
		for {
			for _, t := range level {
				tree := t
				at, err := p.metadata(544, func() []byte { return treeBytes(tree) })
				if err != nil {
					return nil, err
				}
				t.address = at
				o.trees = append(o.trees, t)
			}
			for i, t := range level {
				if i > 0 {
					t.left = level[i-1].address
				}
				if i+1 < len(level) {
					t.right = level[i+1].address
				}
			}
			if len(level) == 1 {
				o.tree = level[0]
				break
			}
			var next []*writeTree
			for start := 0; start < len(level); start += 32 {
				end := min(start+32, len(level))
				children := level[start:end]
				next = append(next, &writeTree{lo: children[0].lo, hi: children[len(children)-1].hi, level: children[0].level + 1, left: undefinedWrite, right: undefinedWrite, children: children})
			}
			level = next
		}
	}
	for _, o := range p.objects {
		if o.data == nil {
			continue
		}
		var size uint64
		var err error
		if o.data.Text != nil {
			current := o
			// One bounded collection per string avoids a growing/global heap
			// allocator. The free object fills the remainder of the collection.
			heapAt, err := p.metadata(4096, func() []byte {
				b := make([]byte, 4096)
				copy(b, "GCOL")
				b[4] = 1
				put64(b, 8, 4096)
				put16(b, 16, 1)
				put16(b, 18, 1)
				put64(b, 24, uint64(len(*current.data.Text)))
				copy(b[32:], *current.data.Text)
				free := (32 + len(*current.data.Text) + 7) &^ 7
				put64(b, free+8, uint64(len(b)-free))
				return b
			})
			if err != nil {
				return nil, err
			}
			o.stringHeap = heapAt
			at, err := p.metadata(16, func() []byte {
				b := make([]byte, 16)
				put32(b, 0, uint32(len(*current.data.Text)))
				put64(b, 4, current.stringHeap)
				put32(b, 12, 1)
				return b
			})
			if err != nil {
				return nil, err
			}
			o.raw = at
			continue
		} else {
			size, err = writeCheckedMul(uint64(o.data.Count), 8)
			if err != nil {
				return nil, err
			}
		}
		if size == 0 {
			o.raw = undefinedWrite
			continue
		}
		at, err := p.alloc(size)
		if err != nil {
			return nil, err
		}
		o.raw = at
		p.parts = append(p.parts, writePart{at: at, size: size, data: o.data})
	}
	// Validate every value after layout/cap admission and before emitting any byte.
	for _, d := range ds {
		if d.Text != nil {
			continue
		}
		for i := 0; i < d.Count; i++ {
			if i&8191 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			v := d.Float64At(i)
			if math.IsNaN(v) || math.IsInf(v, 0) {
				return nil, fmt.Errorf("%w: non-finite vector", ErrFormat)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return p, nil
}
func put16(b []byte, at int, v uint16) { binary.LittleEndian.PutUint16(b[at:], v) }
func put32(b []byte, at int, v uint32) { binary.LittleEndian.PutUint32(b[at:], v) }
func put64(b []byte, at int, v uint64) { binary.LittleEndian.PutUint64(b[at:], v) }
func objectHeader(messages ...struct {
	kind uint16
	data []byte
}) []byte {
	size := 16
	for _, m := range messages {
		size += 8 + (len(m.data)+7)&^7
	}
	b := make([]byte, size)
	b[0] = 1
	put16(b, 2, uint16(len(messages)))
	put32(b, 4, 1)
	put32(b, 8, uint32(size-16))
	at := 16
	for _, m := range messages {
		n := (len(m.data) + 7) &^ 7
		put16(b, at, m.kind)
		put16(b, at+2, uint16(n))
		copy(b[at+8:], m.data)
		at += 8 + n
	}
	return b
}
func (o *writeObject) header() []byte {
	type msg = struct {
		kind uint16
		data []byte
	}
	if o.data == nil {
		b := make([]byte, 16)
		put64(b, 0, o.tree.address)
		put64(b, 8, o.heapAddress)
		return objectHeader(msg{17, b})
	}
	count := uint64(o.data.Count)
	dtype := []byte{0x11, 0x20, 0x3f, 0, 8, 0, 0, 0, 0, 0, 64, 0, 52, 11, 0, 52, 0xff, 3, 0, 0}
	size := uint64(o.data.Count) * 8
	if o.data.Text != nil {
		count = 1
		size = 16
		// Class9/string, NULLTERM, ASCII; the recursive parent is unsigned
		// char (class0, eight-bit precision). This charset matches the ITK
		// public writer and its requested variable-length read datatype.
		dtype = []byte{0x19, 1, 0, 0, 16, 0, 0, 0, 0x10, 0, 0, 0, 1, 0, 0, 0, 0, 0, 8, 0}
	}
	space := make([]byte, 16)
	space[0] = 1
	space[1] = 1
	put64(space, 8, count)
	layout := make([]byte, 18)
	layout[0] = 3
	layout[1] = 1
	put64(layout, 2, o.raw)
	put64(layout, 10, size)
	return objectHeader(msg{1, space}, msg{3, dtype}, msg{8, layout})
}
func symbolBytes(s *writeSymbol, names map[string]uint64) []byte {
	b := make([]byte, 328)
	copy(b, "SNOD")
	b[4] = 1
	put16(b, 6, uint16(len(s.objects)))
	for i, o := range s.objects {
		at := 8 + 40*i
		put64(b, at, names[o.name])
		put64(b, at+8, o.address)
		if o.data == nil {
			put32(b, at+16, 1)
			put64(b, at+24, o.tree.address)
			put64(b, at+32, o.heapAddress)
		}
	}
	return b
}
func treeBytes(t *writeTree) []byte {
	b := make([]byte, 544)
	copy(b, "TREE")
	b[5] = t.level
	put64(b, 8, t.left)
	put64(b, 16, t.right)
	count := len(t.symbols)
	if t.level > 0 {
		count = len(t.children)
	}
	put16(b, 6, uint16(count))
	put64(b, 24, t.lo)
	for i := 0; i < count; i++ {
		address, hi := uint64(0), uint64(0)
		if t.level == 0 {
			address, hi = t.symbols[i].address, t.symbols[i].hi
		} else {
			address, hi = t.children[i].address, t.children[i].hi
		}
		put64(b, 32+16*i, address)
		put64(b, 40+16*i, hi)
	}
	return b
}
func (p *writePlan) superblock() []byte {
	b := make([]byte, 96)
	copy(b, Signature)
	b[13], b[14] = 8, 8
	put16(b, 16, 4)
	put16(b, 18, 16)
	put64(b, 32, undefinedWrite)
	put64(b, 40, p.end)
	put64(b, 48, undefinedWrite)
	put64(b, 64, p.root.address)
	put32(b, 72, 1)
	put64(b, 80, p.root.tree.address)
	put64(b, 88, p.root.heapAddress)
	return b
}

// Write emits the canonical classic subset with an explicit positive byte
// cap. Admission and finite checks occur before the first Write. Metadata
// is bounded independently. IO errors/cancellation may leave partial output.
// It cannot interrupt a blocking Writer; no goroutine or worker pool is used.
func Write(ctx context.Context, w io.Writer, ds []WriteDataset, maxBytes int64) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if w == nil {
		return ErrFormat
	}
	p, err := planWrite(ctx, ds, maxBytes)
	if err != nil {
		return err
	}
	emit := func(b []byte) error {
		for len(b) > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			nbytes := min(len(b), writeChunk)
			n, err := w.Write(b[:nbytes])
			if canceled := ctx.Err(); canceled != nil {
				return canceled
			}
			if err != nil {
				return err
			}
			if n != nbytes {
				return io.ErrShortWrite
			}
			b = b[nbytes:]
		}
		return ctx.Err()
	}
	if err := emit(p.superblock()); err != nil {
		return err
	}
	at := uint64(96)
	buf := make([]byte, writeChunk)
	for _, part := range p.parts {
		if gap := part.at - at; gap > 0 {
			if gap > uint64(len(buf)) {
				return ErrFormat
			}
			clear(buf[:gap])
			if err := emit(buf[:gap]); err != nil {
				return err
			}
		}
		if part.bytes != nil {
			data := part.bytes()
			if uint64(len(data)) != part.size {
				return ErrFormat
			}
			if err := emit(data); err != nil {
				return err
			}
		} else {
			d := part.data
			for start := 0; start < d.Count; start += len(buf) / 8 {
				end := min(start+len(buf)/8, d.Count)
				for i := start; i < end; i++ {
					binary.LittleEndian.PutUint64(buf[8*(i-start):], math.Float64bits(d.Float64At(i)))
				}
				if err := emit(buf[:8*(end-start)]); err != nil {
					return err
				}
			}
		}
		at = part.at + part.size
	}
	if at != p.end {
		return ErrFormat
	}
	return ctx.Err()
}
