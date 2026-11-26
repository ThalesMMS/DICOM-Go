package headers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/object"
	"golang.org/x/text/encoding/japanese"
)

// MECMR3Options select finite limits and an explicit encoding for Unicode and
// STR40 fields. Empty selects UTF-8; Shift-JIS and EUC-JP require opt-in. A
// character-set record is exposed as bytes, not treated as an encoding guess.
type MECMR3Options struct {
	Limits       Limits
	TextEncoding string
}

// MECMR3Record identifies a record numerically. No private dictionary names
// are inferred, and Values are detached from the caller's byte storage.
type MECMR3Record struct {
	Group     uint8
	Key, Type uint32
	Values    []string
}

// DecodeMECMR3 decodes Toshiba Original Data (700d,xx08), using strict UTF-8.
func DecodeMECMR3(raw []byte, l Limits) ([]MECMR3Record, error) {
	return DecodeMECMR3WithOptions(raw, MECMR3Options{Limits: l})
}

// DecodeMECMR3WithOptions decodes the counted groups, record signatures and
// typed payloads of MEC MR3 Original Data; unknown types are explicit errors.
func DecodeMECMR3WithOptions(raw []byte, opts MECMR3Options) ([]MECMR3Record, error) {
	b := newHeaderBudget(opts.Limits)
	if err := b.take(0, len(raw), 0); err != nil {
		return nil, err
	}
	enc := opts.TextEncoding
	if enc == "" {
		enc = "UTF-8"
	}
	if enc != "UTF-8" && enc != "Shift-JIS" && enc != "EUC-JP" {
		return nil, ErrUnsupportedHeader
	}
	c := cursor{data: raw}
	var records []MECMR3Record
	groups := 6
	for group := 1; group <= groups; group++ {
		if group == 6 {
			n, err := c.uint32()
			if err != nil {
				return nil, err
			}
			if n == 0 || n > 250 {
				return nil, ErrInvalid
			}
			groups = 5 + int(n)
		}
		count, err := c.uint32()
		if err != nil {
			return nil, err
		}
		if group <= 6 && count == 0 {
			return nil, ErrInvalid
		}
		if uint64(count) > uint64(b.limits.MaxEntries-b.entries) {
			return nil, ErrLimit
		}
		if uint64(count) > uint64(c.remaining()/32) {
			return nil, ErrInvalid
		}
		if err := b.take(1+int(count), 0, 0); err != nil {
			return nil, err
		}
		for i := uint32(0); i < count; i++ {
			key, e := c.uint32()
			if e != nil {
				return nil, e
			}
			typ, e := c.uint32()
			if e != nil {
				return nil, e
			}
			n, e := c.uint32()
			if e != nil {
				return nil, e
			}
			sig := [5]uint32{}
			for j := range sig {
				sig[j], e = c.uint32()
				if e != nil {
					return nil, e
				}
			}
			if sig[0] != 0 || sig[2] != 12 || sig[3] != 0 {
				return nil, fmt.Errorf("%w: MEC record signature", ErrInvalid)
			}
			if uint64(n) > uint64(c.remaining()) {
				return nil, ErrInvalid
			}
			payload := c.data[c.pos : c.pos+int(n)]
			c.pos += int(n)
			values, e := decodeMECValues(typ, payload, enc, b)
			if e != nil {
				return nil, fmt.Errorf("MEC group %d key %08x: %w", group, key, e)
			}
			records = append(records, MECMR3Record{Group: uint8(group), Key: key, Type: typ, Values: values})
		}
	}
	if c.remaining() == 1 && c.data[c.pos] == 0 {
		c.pos++
	}
	if c.remaining() != 0 {
		return nil, fmt.Errorf("%w: MEC trailing bytes", ErrInvalid)
	}
	return records, nil
}
func decodeMECValues(typ uint32, p []byte, enc string, b *headerBudget) ([]string, error) {
	var values []string
	add := func(s string) error {
		if err := b.take(1, 0, len(s)); err != nil {
			return err
		}
		values = append(values, s)
		return nil
	}
	switch typ {
	case 0xff000400, 0xff002a00, 0xff000800, 0xff002900, 0xff002400, 0xff002800, 0x500, 0x600:
		width := 4
		if typ == 0xff002900 {
			width = 8
		}
		if len(p) == 0 || len(p)%width != 0 {
			return nil, ErrInvalid
		}
		if (typ == 0xff000400 || typ == 0xff002a00 || typ == 0xff000800 || typ == 0xff002900) && len(p) != width {
			return nil, ErrInvalid
		}
		if typ == 0x500 && len(p)%8 != 0 || typ == 0x600 && len(p)%12 != 0 {
			return nil, ErrInvalid
		}
		if len(p)/width > b.limits.MaxEntries-b.entries {
			return nil, ErrLimit
		}
		for i := 0; i < len(p); i += width {
			s := ""
			u := binary.LittleEndian.Uint32(p[i:])
			switch typ {
			case 0xff000400:
				s = strconv.FormatUint(uint64(u), 10)
			case 0xff002a00:
				if u > 1 {
					return nil, ErrInvalid
				}
				s = strconv.FormatBool(u == 1)
			case 0xff002400:
				s = strconv.FormatInt(int64(int32(u)), 10)
			default:
				f := float64(math.Float32frombits(u))
				bits := 32
				if width == 8 {
					f = math.Float64frombits(binary.LittleEndian.Uint64(p[i:]))
					bits = 64
				}
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, ErrInvalid
				}
				s = strconv.FormatFloat(f, 'g', -1, bits)
			}
			if err := add(s); err != nil {
				return nil, err
			}
		}
	case 0xff002c00:
		s, err := mecText(p, enc)
		if err != nil {
			return nil, err
		}
		if err := add(s); err != nil {
			return nil, err
		}
	case 0x300:
		q := p
		if len(p) >= 3 && bytes.Equal(p[:3], []byte{0xdf, 0xff, 0x79}) {
			if len(p) < 19 || len(p) > 259 || int(p[3]) != len(p)-4 || p[4] != 1 || p[5] != 9 || p[6] != 0 || string(p[7:16]) != "ISO8859-1" || p[16] != 2 || int(p[17]) != len(p)-19 || p[18] != 0 {
				return nil, ErrInvalid
			}
			q = p[19:]
			if !utf8.Valid(q) && enc == "UTF-8" {
				return nil, fmt.Errorf("%w: legacy marked text needs explicit encoding", ErrUnsupportedHeader)
			}
			text, err := mecText(q, enc)
			if err != nil {
				return nil, err
			}
			if err := add("ISO8859-1 marker: " + text); err != nil {
				return nil, err
			}
			return values, nil
		}
		s, err := headerASCII(bytes.TrimRight(q, "\x00"))
		if err != nil {
			return nil, err
		}
		if err := add(s); err != nil {
			return nil, err
		}
	case 0xe00:
		s, err := headerASCII(bytes.TrimRight(p, "\x00"))
		if err != nil {
			return nil, err
		}
		if len(s) != 19 {
			return nil, ErrInvalid
		}
		for _, r := range s {
			if !(r >= '0' && r <= '9') && r != '/' && r != ',' && r != ':' {
				return nil, ErrInvalid
			}
		}
		if err := add(s); err != nil {
			return nil, err
		}
	case 0x1f4000:
		if len(p) == 0 || len(p)%340 != 0 {
			return nil, ErrInvalid
		}
		for pos := 0; pos < len(p); pos += 340 {
			if err := add(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(p[pos:])), 10)); err != nil {
				return nil, err
			}
			for j := 0; j < 7; j++ {
				s, err := mecText(p[pos+4+j*48:pos+4+(j+1)*48], enc)
				if err != nil {
					return nil, err
				}
				if err := add(s); err != nil {
					return nil, err
				}
			}
		}
	case 0x1f4100, 0x1f4300, 0x1f4400, 0x1f4600:
		var ranges [][2]int
		switch typ {
		case 0x1f4100:
			if len(p) != 136 || binary.LittleEndian.Uint32(p) != 0 || binary.LittleEndian.Uint16(p[134:]) != 0 {
				return nil, ErrInvalid
			}
			ranges = [][2]int{{4, 69}, {69, 134}}
		case 0x1f4300:
			if len(p) != 436 || binary.LittleEndian.Uint32(p) != 0 {
				return nil, ErrInvalid
			}
			ranges = [][2]int{{4, 21}, {21, 30}, {30, 44}, {44, 73}, {73, 329}, {329, 394}, {394, 411}, {411, 414}, {414, 432}}
		case 0x1f4400:
			if len(p) != 516 {
				return nil, ErrInvalid
			}
			ranges = [][2]int{{0, 65}, {65, 86}, {86, 342}, {342, 359}, {359, 424}, {424, 489}}
		case 0x1f4600:
			if len(p) != 325 {
				return nil, ErrInvalid
			}
			for i := 0; i < 5; i++ {
				ranges = append(ranges, [2]int{i * 65, (i + 1) * 65})
			}
		}
		for _, r := range ranges {
			s, err := mecFixedASCII(p[r[0]:r[1]])
			if err != nil {
				return nil, err
			}
			if err := add(s); err != nil {
				return nil, err
			}
		}
		if typ == 0x1f4300 {
			if err := add(strconv.FormatUint(uint64(binary.LittleEndian.Uint32(p[432:])), 10)); err != nil {
				return nil, err
			}
		}
		if typ == 0x1f4400 {
			for i := 0; i < 6; i++ {
				u := binary.LittleEndian.Uint32(p[492+i*4:])
				if u > 1 {
					return nil, ErrInvalid
				}
				if err := add(strconv.FormatBool(u == 1)); err != nil {
					return nil, err
				}
			}
		}
	case 0xff002300:
		// The structure of this private charset field is not interpreted. Keeping
		// its exact bytes exposes it without silently selecting an encoding.
		if len(p) > (b.limits.MaxDecodedBytes-b.decoded)/2 {
			return nil, ErrLimit
		}
		if err := add(fmt.Sprintf("charset bytes: %x (not interpreted)", p)); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: MEC type %08x", ErrUnsupportedHeader, typ)
	}
	return values, nil
}
func mecFixedASCII(p []byte) (string, error) {
	k := bytes.IndexByte(p, 0)
	if k < 0 {
		return "", ErrInvalid
	}
	for _, v := range p[k:] {
		if v != 0 {
			return "", ErrInvalid
		}
	}
	return headerASCII(p[:k])
}
func mecText(p []byte, enc string) (string, error) {
	k := bytes.IndexByte(p, 0)
	if k >= 0 {
		for _, v := range p[k:] {
			if v != 0 {
				return "", ErrInvalid
			}
		}
		p = p[:k]
	}
	var s string
	switch enc {
	case "UTF-8":
		if !utf8.Valid(p) {
			return "", fmt.Errorf("%w: MEC text requires explicit encoding", ErrUnsupportedHeader)
		}
		s = string(p)
	case "Shift-JIS":
		out, err := japanese.ShiftJIS.NewDecoder().Bytes(p)
		if err != nil {
			return "", ErrInvalid
		}
		s = string(out)
	case "EUC-JP":
		out, err := japanese.EUCJP.NewDecoder().Bytes(p)
		if err != nil {
			return "", ErrInvalid
		}
		s = string(out)
	}
	if strings.ContainsRune(s, utf8.RuneError) {
		return "", ErrInvalid
	}
	return s, nil
}

// ToshibaMECMR3Of locates the Original Data blob by creator.
func ToshibaMECMR3Of(o *object.Object, l Limits) ([]MECMR3Record, error) {
	l = l.normalized()
	raw, _, err := privateValue(o, 0x700d, []string{"TOSHIBA_MEC_MR3"}, 0x08, l)
	if err != nil {
		return nil, err
	}
	return DecodeMECMR3(raw, l)
}
func mecMR3Lines(records []MECMR3Record, l Limits) ([]string, error) {
	b := newHeaderBudget(l)
	var lines []string
	for _, r := range records {
		if err := b.line(&lines, fmt.Sprintf("(%02x,%08x) type=%08x [%s]", r.Group, r.Key, r.Type, strings.Join(r.Values, `\`))); err != nil {
			return nil, err
		}
	}
	return lines, nil
}
