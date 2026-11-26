// Package headers decodes the metadata-only Philips/Toshiba/Canon private
// formats, without image codecs or DICOM networking dependencies. The vendors
// package provides compatibility wrappers and combines them with legacy headers.
package headers

import (
	"encoding/binary"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	ErrNotFound = errors.New("dicom/vendors: private blob not found")
	ErrInvalid  = errors.New("dicom/vendors: invalid private blob")
	ErrLimit    = errors.New("dicom/vendors: private blob exceeds limits")
)

// Limits bound stored bytes, decoded text and counted entries/values/items.
// Zero fields select 16 MiB, 16 MiB and 100000 respectively. These are logical
// limits, not process RSS or input-ownership quotas.
type Limits struct{ MaxBytes, MaxDecodedBytes, MaxEntries int }

func (l Limits) normalized() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 16 << 20
	}
	if l.MaxDecodedBytes <= 0 {
		l.MaxDecodedBytes = 16 << 20
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 100000
	}
	return l
}
func privateValue(o *object.Object, g uint16, creators []string, offset uint8, l Limits) ([]byte, core.Tag, error) {
	if o == nil {
		return nil, core.Tag{}, ErrNotFound
	}
	for _, creator := range creators {
		tag, err := o.PrivateTag(g, creator, offset)
		if err != nil || !o.Has(tag) {
			continue
		}
		el, _ := o.Get(tag)
		// Inspect the concrete resident representation before any Value method or
		// payload copy. Direct Of APIs do not pass through sequence preflight.
		raw, ok := el.Value.(core.RawValue)
		if !ok {
			return nil, tag, ErrInvalid
		}
		if _, err := primitiveHeaderBytes(el, l.MaxBytes); err != nil {
			return nil, tag, err
		}
		return core.CloneBytes(raw), tag, nil
	}
	return nil, core.Tag{}, ErrNotFound
}

type cursor struct {
	data []byte
	pos  int
}

func (c *cursor) remaining() int { return len(c.data) - c.pos }
func (c *cursor) uint32() (uint32, error) {
	if c.remaining() < 4 {
		return 0, fmt.Errorf("%w: truncated at byte %d", ErrInvalid, c.pos)
	}
	v := binary.LittleEndian.Uint32(c.data[c.pos:])
	c.pos += 4
	return v, nil
}
