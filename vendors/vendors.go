// Package vendors identifies the manufacturer of an object and decodes the
// private blobs some vendors store in it: the GE Protocol Data Block, the
// ELSCINT1 protocol information and private compressed pixel data
// (PMSCT_RLE1, PMSCT_RGB1) and the VEPRO VIF/VIM data. Siemens headers
// (CSA, MedCom History) are in package siemens; DecodePrivateHeaders renders
// all of them as text for inspectors.
//
// Every parser is an independent, bounded implementation of the layout the
// public tools document (GDCM gdcmdump serves as the oracle; no code was
// ported). The blobs are private and often carry personal data (VEPRO data
// fields and MedCom History hold patient names), so decoding them is
// explicit and nothing here is applied when an object is read.
package vendors

import (
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

var (
	// ErrNotFound reports an object without the requested blob.
	ErrNotFound = headers.ErrNotFound
	// ErrInvalid reports a blob that does not follow its layout.
	ErrInvalid = headers.ErrInvalid
	// ErrLimit reports a blob beyond the parser limits.
	ErrLimit = headers.ErrLimit
)

// Limits bound the parsers. Zero fields select the defaults.
type Limits struct {
	// MaxBytes bounds the stored blob (default 16 MiB).
	MaxBytes int
	// MaxDecodedBytes bounds decompressed text (default 16 MiB).
	MaxDecodedBytes int
	// MaxEntries bounds the entries, values and items decoded (default
	// 100000).
	MaxEntries int
}

func (l Limits) normalized() Limits {
	if l.MaxBytes <= 0 {
		l.MaxBytes = 16 << 20
	}
	if l.MaxDecodedBytes <= 0 {
		l.MaxDecodedBytes = 16 << 20
	}
	if l.MaxEntries <= 0 {
		l.MaxEntries = 100_000
	}
	return l
}

// privateValue returns the value of a private attribute located through its
// creator, checked against limits.
func privateValue(dataset *object.Object, group uint16, creators []string, offset uint8, limits Limits) ([]byte, core.Tag, error) {
	if dataset == nil {
		return nil, core.Tag{}, ErrNotFound
	}
	for _, creator := range creators {
		tag, err := dataset.PrivateTag(group, creator, offset)
		if err != nil || !dataset.Has(tag) {
			continue
		}
		element, _ := dataset.Get(tag)
		if element.Length() != core.UndefinedLength && int64(element.Length()) > int64(limits.MaxBytes) {
			return nil, tag, fmt.Errorf("%w: %d bytes", ErrLimit, element.Length())
		}
		raw, ok := dataset.GetRaw(tag)
		if !ok {
			return nil, tag, fmt.Errorf("%w: %s has no byte value", ErrInvalid, tag)
		}
		if len(raw) > limits.MaxBytes {
			return nil, tag, fmt.Errorf("%w: %d bytes", ErrLimit, len(raw))
		}
		return raw, tag, nil
	}
	return nil, core.Tag{}, ErrNotFound
}

// cursor reads a little endian blob without ever indexing past its end.
type cursor struct {
	data []byte
	pos  int
}

func (c *cursor) remaining() int { return len(c.data) - c.pos }

func (c *cursor) uint32() (uint32, error) {
	if c.remaining() < 4 {
		return 0, fmt.Errorf("%w: truncated at byte %d", ErrInvalid, c.pos)
	}
	v := uint32(c.data[c.pos]) | uint32(c.data[c.pos+1])<<8 | uint32(c.data[c.pos+2])<<16 | uint32(c.data[c.pos+3])<<24
	c.pos += 4
	return v, nil
}

func (c *cursor) peekUint32() (uint32, bool) {
	if c.remaining() < 4 {
		return 0, false
	}
	v, _ := c.uint32()
	c.pos -= 4
	return v, true
}

// cString reads a NUL-terminated string.
func (c *cursor) cString() (string, error) {
	for i := c.pos; i < len(c.data); i++ {
		if c.data[i] == 0 {
			s := string(c.data[c.pos:i])
			c.pos = i + 1
			return s, nil
		}
	}
	return "", fmt.Errorf("%w: unterminated string at byte %d", ErrInvalid, c.pos)
}
