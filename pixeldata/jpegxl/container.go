package jpegxl

import (
	"bytes"
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/internal/brotli"
)

// ISO/IEC 18181-2 file format.

var containerSignature = []byte{0, 0, 0, 0x0C, 'J', 'X', 'L', ' ', 0x0D, 0x0A, 0x87, 0x0A}

// maxBoxPayload bounds decompressed metadata boxes (brob, jbrd tail data).
const maxBoxPayload = 64 << 20

type container struct {
	codestream []byte
	jbrd       []byte
	exif       []byte
	xmp        []byte
	boxed      bool
}

// parseContainer splits a JPEG XL file into its codestream and the boxes this
// package uses. A bare codestream (starting FF 0A) is returned as is.
func parseContainer(data []byte) (container, error) {
	if len(data) >= 2 && data[0] == 0xFF && data[1] == 0x0A {
		return container{codestream: data}, nil
	}
	if !bytes.HasPrefix(data, containerSignature) {
		return container{}, corrupt("not a JPEG XL file")
	}
	c := container{boxed: true}
	var parts []byte
	sawJxlc, sawJxlp, lastPart := false, false, false
	expectIndex := uint32(0)
	pos := len(containerSignature)
	for pos < len(data) {
		if len(data)-pos == 1 && data[pos] == 0 {
			break // the NULL pad of an odd-length DICOM fragment
		}
		if len(data)-pos < 8 {
			return container{}, corrupt("truncated box header")
		}
		size := uint64(binary.BigEndian.Uint32(data[pos:]))
		typ := string(data[pos+4 : pos+8])
		header := 8
		switch size {
		case 0:
			size = uint64(len(data) - pos)
		case 1:
			if len(data)-pos < 16 {
				return container{}, corrupt("truncated box header")
			}
			size = binary.BigEndian.Uint64(data[pos+8:])
			header = 16
		}
		if size < uint64(header) || size > uint64(len(data)-pos) {
			return container{}, corrupt("box %q size", typ)
		}
		payload := data[pos+header : pos+int(size)]
		pos += int(size)
		if typ == "brob" {
			if len(payload) < 4 {
				return container{}, corrupt("brob box too small")
			}
			inner := string(payload[:4])
			if inner == "jxlc" || inner == "jxlp" || inner == "brob" {
				return container{}, corrupt("brob box wraps %q", inner)
			}
			decoded, err := brotli.Decode(payload[4:], maxBoxPayload)
			if err != nil {
				return container{}, corrupt("brob box: %v", err)
			}
			typ, payload = inner, decoded
		}
		switch typ {
		case "jxlc":
			if sawJxlc || sawJxlp {
				return container{}, corrupt("multiple codestream boxes")
			}
			sawJxlc = true
			parts = payload
		case "jxlp":
			if sawJxlc || lastPart {
				return container{}, corrupt("unexpected jxlp box")
			}
			if len(payload) < 4 {
				return container{}, corrupt("jxlp box too small")
			}
			idx := binary.BigEndian.Uint32(payload)
			if idx&0x7FFFFFFF != expectIndex {
				return container{}, corrupt("jxlp boxes out of order")
			}
			expectIndex++
			lastPart = idx&0x80000000 != 0
			sawJxlp = true
			parts = append(parts, payload[4:]...)
		case "jbrd":
			if c.jbrd != nil {
				return container{}, corrupt("multiple jbrd boxes")
			}
			c.jbrd = payload
		case "Exif":
			if c.exif == nil {
				c.exif = payload
			}
		case "xml ":
			if c.xmp == nil {
				c.xmp = payload
			}
		}
	}
	if !sawJxlc && !sawJxlp {
		return container{}, corrupt("no codestream box")
	}
	if sawJxlp && !lastPart {
		return container{}, corrupt("missing final jxlp box")
	}
	c.codestream = parts
	return c, nil
}
