package vendors

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/object"
)

// ElscintEntry is one named entry of ELSCINT1 protocol information.
type ElscintEntry struct {
	Key    string
	Values []string
}

// ElscintGroup is a named group of entries. A single group has one item; a
// multiple group has one entry list per item (possibly empty).
type ElscintGroup struct {
	Name     string
	Multiple bool
	Items    [][]ElscintEntry
}

// ElscintProtocol is decoded (01F7,xx26) of "ELSCINT1". Groups is set for
// the binary layout; Text for the gzip-compressed text variant.
type ElscintProtocol struct {
	Groups []ElscintGroup
	Text   string
}

const (
	elscintMagic    = 0xF22D
	elscintSingle   = 0x0006981A
	elscintMultiple = 0x00000925
	// elscintGzipOffset is where the compressed text variant starts.
	elscintGzipOffset = 0x15F
)

// ElscintProtocolOf decodes the ELSCINT1 protocol information of dataset.
func ElscintProtocolOf(dataset *object.Object, limits Limits) (*ElscintProtocol, error) {
	limits = limits.normalized()
	raw, _, err := privateValue(dataset, 0x01F7, []string{"ELSCINT1"}, 0x26, limits)
	if err != nil {
		return nil, err
	}
	return ParseElscintProtocol(raw, limits)
}

// ParseElscintProtocol decodes an ELSCINT1 protocol information value: a
// header (magic 0xF22D, version 1, group count) and groups, each a 32-bit
// kind, a NUL-terminated name and entries (a NUL-terminated key, a value
// count and NUL-terminated values). Kind 0x6981A holds one entry list;
// kind 0x925 an item count and one counted entry list per item. Values with
// a gzip stream at byte 0x15F are the compressed text variant.
func ParseElscintProtocol(data []byte, limits Limits) (*ElscintProtocol, error) {
	limits = limits.normalized()
	if len(data) > limits.MaxBytes {
		return nil, fmt.Errorf("%w: %d bytes", ErrLimit, len(data))
	}
	if len(data) > elscintGzipOffset+3 && bytes.Equal(data[elscintGzipOffset:elscintGzipOffset+3], []byte{0x1F, 0x8B, 0x08}) {
		reader, err := gzip.NewReader(bytes.NewReader(data[elscintGzipOffset:]))
		if err != nil {
			return nil, fmt.Errorf("%w: ELSCINT1 text: %w", ErrInvalid, err)
		}
		text, err := io.ReadAll(io.LimitReader(reader, int64(limits.MaxDecodedBytes)+1))
		if err != nil && !(errors.Is(err, io.ErrUnexpectedEOF) && len(text) > 0) {
			return nil, fmt.Errorf("%w: ELSCINT1 text: %w", ErrInvalid, err)
		}
		if len(text) > limits.MaxDecodedBytes {
			return nil, fmt.Errorf("%w: ELSCINT1 text decodes to more than %d bytes", ErrLimit, limits.MaxDecodedBytes)
		}
		return &ElscintProtocol{Text: string(text)}, nil
	}
	c := cursor{data: data}
	var header [3]uint32
	for i := range header {
		var err error
		if header[i], err = c.uint32(); err != nil {
			return nil, err
		}
	}
	if header[0] != elscintMagic || header[1] != 1 {
		return nil, fmt.Errorf("%w: ELSCINT1 header %#x version %d", ErrInvalid, header[0], header[1])
	}
	// Every group takes at least 9 bytes: its kind, name and count.
	if int64(header[2])*9 > int64(c.remaining()) {
		return nil, fmt.Errorf("%w: %d groups do not fit", ErrInvalid, header[2])
	}
	budget := limits.MaxEntries
	protocol := &ElscintProtocol{}
	for g := uint32(0); g < header[2]; g++ {
		group, err := c.elscintGroup(&budget)
		if err != nil {
			return nil, fmt.Errorf("group %d: %w", g, err)
		}
		protocol.Groups = append(protocol.Groups, group)
	}
	return protocol, nil
}

func (c *cursor) elscintGroup(budget *int) (ElscintGroup, error) {
	kind, err := c.uint32()
	if err != nil {
		return ElscintGroup{}, err
	}
	name, err := c.cString()
	if err != nil {
		return ElscintGroup{}, err
	}
	group := ElscintGroup{Name: name}
	switch kind {
	case elscintSingle:
		entries, err := c.elscintEntries(budget)
		if err != nil {
			return ElscintGroup{}, err
		}
		group.Items = [][]ElscintEntry{entries}
	case elscintMultiple:
		group.Multiple = true
		items, err := c.uint32()
		if err != nil {
			return ElscintGroup{}, err
		}
		// Each item takes at least its 4-byte entry count.
		if int64(items)*4 > int64(c.remaining()) {
			return ElscintGroup{}, fmt.Errorf("%w: %d items do not fit", ErrInvalid, items)
		}
		for i := uint32(0); i < items; i++ {
			entries, err := c.elscintEntries(budget)
			if err != nil {
				return ElscintGroup{}, fmt.Errorf("item %d: %w", i, err)
			}
			group.Items = append(group.Items, entries)
		}
	default:
		return ElscintGroup{}, fmt.Errorf("%w: group kind %#x", ErrInvalid, kind)
	}
	return group, nil
}

// elscintEntries reads an entry count and the entries.
func (c *cursor) elscintEntries(budget *int) ([]ElscintEntry, error) {
	count, err := c.uint32()
	if err != nil {
		return nil, err
	}
	// An entry takes at least 5 bytes: an empty key and its value count.
	if int64(count)*5 > int64(c.remaining()) {
		return nil, fmt.Errorf("%w: %d entries do not fit", ErrInvalid, count)
	}
	entries := make([]ElscintEntry, 0, count)
	for i := uint32(0); i < count; i++ {
		key, err := c.cString()
		if err != nil {
			return nil, err
		}
		values, err := c.uint32()
		if err != nil {
			return nil, err
		}
		// A value takes at least its NUL.
		if int64(values) > int64(c.remaining()) {
			return nil, fmt.Errorf("%w: %d values do not fit", ErrInvalid, values)
		}
		*budget -= 1 + int(values)
		if *budget < 0 {
			return nil, fmt.Errorf("%w: too many entries", ErrLimit)
		}
		entry := ElscintEntry{Key: key, Values: make([]string, 0, values)}
		for v := uint32(0); v < values; v++ {
			value, err := c.cString()
			if err != nil {
				return nil, err
			}
			entry.Values = append(entry.Values, value)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}
