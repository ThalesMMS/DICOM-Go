package hdf5

import (
	"fmt"
	"math"
	"strings"
)

type linkStorage struct {
	heap, names, creationOrder uint64
	tracksCreationOrder        bool
	indexesCreationOrder       bool
	maxCreationOrder           uint64
}

// linkInfo validates the version 0 link-info message shared by compact and
// dense groups. An undefined heap address identifies compact storage.
func (f *File) linkInfo(data []byte) (linkStorage, error) {
	var info linkStorage
	d := &decoder{b: data}
	version, flags := d.u8(), d.u8()
	if d.err != nil {
		return info, d.err
	}
	if version != 0 || flags&^3 != 0 {
		return info, fmt.Errorf("%w: link info version or flags", ErrUnsupported)
	}
	info.tracksCreationOrder = flags&1 != 0
	info.indexesCreationOrder = flags&2 != 0
	if info.indexesCreationOrder && !info.tracksCreationOrder {
		return info, fmt.Errorf("%w: link order is indexed without tracking", ErrFormat)
	}
	if info.tracksCreationOrder {
		info.maxCreationOrder = d.uint(8)
		if info.maxCreationOrder > math.MaxInt64 {
			return info, fmt.Errorf("%w: invalid maximum link creation order", ErrFormat)
		}
	}
	info.heap, info.names = d.uint(f.offsetSize), d.uint(f.offsetSize)
	if info.indexesCreationOrder {
		info.creationOrder = d.uint(f.offsetSize)
	}
	if d.err != nil {
		return info, d.err
	}
	isCompact := f.undefined(info.heap)
	if f.undefined(info.names) != isCompact ||
		(info.indexesCreationOrder && f.undefined(info.creationOrder) != isCompact) {
		return info, fmt.Errorf("%w: inconsistent dense link index addresses", ErrFormat)
	}
	return info, nil
}

// denseLinks reads link messages through the name index. Storage order is
// order by name hash unless the file also stores a creation-order index.
func (f *File) denseLinks(group uint64, info linkStorage) ([]Link, error) {
	h, err := f.openLinkHeap(info.heap)
	if err != nil {
		return nil, err
	}
	links := make([]Link, 0, min(int(h.managedCount), 64))
	byID := make(map[[denseLinkIDSize]byte]int)
	names := make(map[string]bool)
	orders := make(map[uint64]bool)
	creationOrder := make([]uint64, 0, cap(links))
	n, err := f.walkLinkBTree(info.names, linkNameIndex, h.idSize, func(hash uint64, id []byte) error {
		idKey := [denseLinkIDSize]byte(id)
		if _, duplicate := byID[idKey]; duplicate {
			return fmt.Errorf("%w: repeated link heap ID", ErrFormat)
		}
		p, err := h.linkObject(id)
		if err != nil {
			return err
		}
		l, err := f.linkMessage(p)
		if err != nil {
			return err
		}
		if !l.hard {
			return fmt.Errorf("%w: non-hard link in dense group", ErrUnsupported)
		}
		if l.trailing != 0 || l.Name == "" || strings.ContainsAny(l.Name, "/\x00") ||
			l.address == group || f.undefined(l.address) ||
			l.address >= uint64(f.size)-min(f.base, uint64(f.size)) {
			return fmt.Errorf("%w: invalid dense link name or target", ErrFormat)
		}
		if names[l.Name] || metadataChecksum([]byte(l.Name)) != uint32(hash) {
			return fmt.Errorf("%w: inconsistent dense name index", ErrFormat)
		}
		if l.ordered != info.tracksCreationOrder ||
			(l.ordered && (l.order >= info.maxCreationOrder || orders[l.order])) {
			return fmt.Errorf("%w: inconsistent dense link creation order", ErrFormat)
		}
		names[l.Name] = true
		if l.ordered {
			orders[l.order] = true
		}
		byID[idKey] = len(links)
		links = append(links, l.Link)
		creationOrder = append(creationOrder, l.order)
		return nil
	})
	if err != nil {
		return nil, err
	}
	if uint64(n) != h.managedCount {
		return nil, fmt.Errorf("%w: dense heap and name index counts differ", ErrFormat)
	}
	if err := h.validateObjects(); err != nil {
		return nil, err
	}
	if !info.indexesCreationOrder {
		return links, nil
	}
	ordered := make([]Link, 0, n)
	o, err := f.walkLinkBTree(info.creationOrder, linkCreationOrderIndex, h.idSize, func(order uint64, id []byte) error {
		index, ok := byID[[denseLinkIDSize]byte(id)]
		if !ok || creationOrder[index] != order {
			return fmt.Errorf("%w: creation order index does not match link", ErrFormat)
		}
		ordered = append(ordered, links[index])
		return nil
	})
	if err != nil {
		return nil, err
	}
	if o != n {
		return nil, fmt.Errorf("%w: dense link index counts differ", ErrFormat)
	}
	return ordered, nil
}
