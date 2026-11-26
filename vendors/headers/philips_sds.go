package headers

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// SDSValue is a Philips v8 directory field. Type is 0=float32, 1=int32,
// 2=81-byte NUL-terminated string or 4=uint32. Values are owned strings.
type SDSValue struct {
	Name   string
	Type   uint32
	Values []string
}

// PhilipsSDSItem is one Protocol Data item in (2005,xx32).
type PhilipsSDSItem struct {
	Name, DataType, Boolean string
	Values                  []SDSValue
}

// DecodePhilipsSDS decodes the known v8 binary directory: a 32-byte prefix,
// little endian version/count at 24/28, 50-byte descriptors and descriptor-
// relative value offsets. Direct-int/direct-string variants are not guessed.
func DecodePhilipsSDS(raw []byte, l Limits) ([]SDSValue, error) {
	return decodePhilipsSDS(raw, newHeaderBudget(l))
}
func decodePhilipsSDS(raw []byte, b *headerBudget) ([]SDSValue, error) {
	if len(raw) > b.limits.MaxBytes {
		return nil, ErrLimit
	}
	if len(raw) > 0 && raw[0] == 1 {
		return nil, fmt.Errorf("%w: Philips direct record", ErrUnsupportedHeader)
	}
	if len(raw) < 32 {
		return nil, ErrInvalid
	}
	if binary.LittleEndian.Uint32(raw[24:]) != 8 {
		return nil, fmt.Errorf("%w: Philips version", ErrUnsupportedHeader)
	}
	count := uint64(binary.LittleEndian.Uint32(raw[28:]))
	if count > uint64(b.limits.MaxEntries-b.entries) {
		return nil, ErrLimit
	}
	if count > uint64((len(raw)-32)/50) {
		return nil, ErrInvalid
	}
	if err := b.take(int(count), 0, 0); err != nil {
		return nil, err
	}
	end := 32 + int(count)*50
	var out []SDSValue
	for i := 0; i < int(count); i++ {
		start := 32 + i*50
		d := raw[start : start+50]
		nameRaw := d[:34]
		nul := bytes.IndexByte(nameRaw, 0)
		if nul < 0 {
			return nil, fmt.Errorf("%w: Philips field name", ErrInvalid)
		}
		name, err := headerASCII(nameRaw[:nul])
		if err != nil {
			return nil, err
		}
		typ := binary.LittleEndian.Uint32(d[34:])
		n := uint64(binary.LittleEndian.Uint32(d[38:]))
		offset := uint64(binary.LittleEndian.Uint32(d[46:]))
		if name == "" {
			continue
		}
		if n == 0 {
			return nil, ErrInvalid
		}
		if n > uint64(b.limits.MaxEntries-b.entries) {
			return nil, ErrLimit
		}
		width := uint64(4)
		if typ == 2 {
			width = 81
		} else if typ != 0 && typ != 1 && typ != 4 {
			return nil, fmt.Errorf("%w: Philips field type %d", ErrUnsupportedHeader, typ)
		}
		pos := uint64(start+46) + offset
		if pos < uint64(end) || pos > uint64(len(raw)) || n > (uint64(len(raw))-pos)/width {
			return nil, fmt.Errorf("%w: Philips value span", ErrInvalid)
		}
		if err := b.take(int(n), 0, len(name)); err != nil {
			return nil, err
		}
		field := SDSValue{Name: name, Type: typ}
		for j := uint64(0); j < n; j++ {
			v := raw[int(pos+j*width):int(pos+(j+1)*width)]
			s := ""
			switch typ {
			case 0:
				f := float64(math.Float32frombits(binary.LittleEndian.Uint32(v)))
				if math.IsNaN(f) || math.IsInf(f, 0) {
					return nil, ErrInvalid
				}
				s = strconv.FormatFloat(f, 'g', -1, 32)
			case 1:
				s = strconv.FormatInt(int64(int32(binary.LittleEndian.Uint32(v))), 10)
			case 4:
				s = strconv.FormatUint(uint64(binary.LittleEndian.Uint32(v)), 10)
			case 2:
				k := bytes.IndexByte(v, 0)
				if k < 0 {
					return nil, ErrInvalid
				}
				s, err = headerASCII(v[:k])
				if err != nil {
					return nil, err
				}
			}
			if err := b.take(0, 0, len(s)); err != nil {
				return nil, err
			}
			field.Values = append(field.Values, s)
		}
		out = append(out, field)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("%w: empty Philips directory", ErrInvalid)
	}
	return out, nil
}
func headerASCII(raw []byte) (string, error) {
	for _, v := range raw {
		if v > 127 {
			return "", fmt.Errorf("%w: undeclared header text encoding", ErrUnsupportedHeader)
		}
		if v < 32 && v != '\n' && v != '\r' && v != '\t' {
			return "", ErrInvalid
		}
	}
	return string(raw), nil
}

// PhilipsSDSOf resolves every sequence item's own creator and declared DICOM
// character set for names/types; binary directory strings use the known ASCII
// subset only. The dataset and its item values are not changed.
func PhilipsSDSOf(o *object.Object, l Limits) ([]PhilipsSDSItem, error) {
	const creator = "Philips MR Imaging DD 002"
	tag, err := privateTagRequired(o, 0x2005, creator, 0x32)
	if err != nil {
		return nil, err
	}
	b := newHeaderBudget(l)
	items, err := privateSequence(o, tag, b)
	if err != nil {
		return nil, err
	}
	if len(items) == 0 {
		return nil, ErrInvalid
	}
	var out []PhilipsSDSItem
	for _, item := range items {
		var v PhilipsSDSItem
		for _, f := range []struct {
			offset uint8
			target *string
		}{{0x37, &v.Name}, {0x39, &v.DataType}, {0x47, &v.Boolean}} {
			tag, err := privateTagRequired(item, 0x2005, creator, f.offset)
			if err != nil {
				return nil, err
			}
			s, err := item.LookupString(tag)
			if err != nil {
				return nil, fmt.Errorf("%w: Philips item text", ErrInvalid)
			}
			if err := b.take(0, 0, len(s)); err != nil {
				return nil, err
			}
			*f.target = s
		}
		raw, _, err := privateValue(item, 0x2005, []string{creator}, 0x44, b.limits)
		if err != nil {
			return nil, err
		}
		v.Values, err = decodePhilipsSDS(raw, b)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
func philipsSDSLines(items []PhilipsSDSItem, l Limits) ([]string, error) {
	b := newHeaderBudget(l)
	var lines []string
	for i, item := range items {
		if err := b.line(&lines, fmt.Sprintf("Item %d: %s / %s / %s", i, item.Name, item.DataType, item.Boolean)); err != nil {
			return nil, err
		}
		for _, v := range item.Values {
			if err := b.line(&lines, v.Name+" = "+strings.Join(v.Values, `\`)); err != nil {
				return nil, err
			}
		}
	}
	return lines, nil
}
