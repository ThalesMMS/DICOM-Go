package pathtemplate

import (
	"encoding/binary"
	"strconv"
	"unicode/utf8"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

var charsetTag = core.NewTag(0x0008, 0x0005)

// Check inline payloads before any Object decoder allocates a VM slice. Encoded
// header lengths cannot supply this bound: authored lengths can be stale.
func checkPayload(e core.Element, max int) error {
	count, width := 0, 0
	switch v := e.Value.(type) {
	case core.RawValue:
		if len(v) > max {
			return ErrLimitExceeded
		}
		return nil
	case core.StringValue:
		if len(v) > 0 && len(v)-1 > max {
			return ErrLimitExceeded
		}
		total := 0
		if len(v) > 0 {
			total = len(v) - 1
		}
		for _, s := range v {
			if len(s) > max-total {
				return ErrLimitExceeded
			}
			total += len(s)
		}
		return nil
	case core.Uint16Value:
		count, width = len(v), 2
	case core.Int16Value:
		count, width = len(v), 2
	case core.Uint32Value:
		count, width = len(v), 4
	case core.Int32Value:
		count, width = len(v), 4
	case core.Uint64Value:
		count, width = len(v), 8
	case core.Int64Value:
		count, width = len(v), 8
	case core.Float32Value:
		count, width = len(v), 4
	case core.Float64Value:
		count, width = len(v), 8
	case core.TagValue:
		count, width = len(v), 4
	default:
		return ErrValue // nil/deferred/bulk/sequence/unknown remain unavailable
	}
	if count > max/width {
		return ErrLimitExceeded
	}
	return nil
}

func (t *Template) valueIndex(count int, f field) (int, bool, error) {
	if count == 0 {
		return 0, true, nil
	}
	if f.index >= 0 {
		if f.index >= count {
			return 0, true, nil
		}
		return f.index, false, nil
	}
	if count > 1 && t.opts.Multiplicity != VMFirst {
		return 0, false, ErrMultiplicity
	}
	return 0, false, nil
}

func (t *Template) attribute(src *object.Object, f field) (string, core.VR, bool, error) {
	e, present := src.Get(f.tag)
	if !present {
		return "", "", true, nil
	}
	if err := checkPayload(e, t.opts.MaxAttributeBytes); err != nil {
		return "", e.VR(), false, err
	}
	if e.VR().IsStringLike() {
		// CharacterSet uses its scope's inline declaration. Bound that input as
		// well, before LookupStrings can populate caches or split charset codes.
		if _, raw := e.Value.(core.RawValue); raw && e.VR().UsesSpecificCharacterSet() {
			if declaration, ok := src.Get(charsetTag); ok {
				if err := checkPayload(declaration, t.opts.MaxAttributeBytes); err != nil {
					return "", e.VR(), false, err
				}
			}
		}
		values, err := src.LookupStrings(f.tag)
		if err != nil {
			return "", e.VR(), false, ErrValue
		}
		index, missing, err := t.valueIndex(len(values), f)
		if err != nil || missing {
			return "", e.VR(), missing, err
		}
		value := values[index]
		if len(value) > t.opts.MaxAttributeBytes {
			return "", e.VR(), false, ErrLimitExceeded
		}
		if !utf8.ValidString(value) {
			return "", e.VR(), false, ErrValue
		}
		return value, e.VR(), value == "", nil
	}
	value, missing, err := t.numeric(src, e, f)
	if err == nil && len(value) > t.opts.MaxAttributeBytes {
		return "", e.VR(), false, ErrLimitExceeded
	}
	return value, e.VR(), missing, err
}

func (t *Template) numeric(src *object.Object, e core.Element, f field) (string, bool, error) {
	count := 0
	valid := false
	switch v := e.Value.(type) {
	case core.Uint16Value:
		count, valid = len(v), e.VR() == core.VRUS
	case core.Int16Value:
		count, valid = len(v), e.VR() == core.VRSS
	case core.Uint32Value:
		count, valid = len(v), e.VR() == core.VRUL
	case core.Int32Value:
		count, valid = len(v), e.VR() == core.VRSL
	case core.Uint64Value:
		count, valid = len(v), e.VR() == core.VRUV
	case core.Int64Value:
		count, valid = len(v), e.VR() == core.VRSV
	case core.Float32Value:
		count, valid = len(v), e.VR() == core.VRFL
	case core.Float64Value:
		count, valid = len(v), e.VR() == core.VRFD
	case core.TagValue:
		count, valid = len(v), e.VR() == core.VRAT
	case core.RawValue:
		width := numericWidth(e.VR())
		if width != 0 && len(v)%width == 0 {
			count, valid = len(v)/width, true
		}
	}
	if !valid {
		return "", false, ErrValue
	}
	index, missing, err := t.valueIndex(count, f)
	if err != nil || missing {
		return "", missing, err
	}
	// GetInts does not accept these two authored typed representations yet.
	// Selection needs no decoder and does not expand Object/core in this issue.
	switch v := e.Value.(type) {
	case core.Int16Value:
		return strconv.FormatInt(int64(v[index]), 10), false, nil
	case core.Int64Value:
		return strconv.FormatInt(v[index], 10), false, nil
	}
	if e.VR() == core.VRUV {
		// GetInts represents values as int64, which cannot represent valid UV
		// above MaxInt64. Read only the selected uint64 without narrowing it.
		switch v := e.Value.(type) {
		case core.Uint64Value:
			return strconv.FormatUint(v[index], 10), false, nil
		case core.RawValue:
			order := src.ValueByteOrder()
			if order == nil {
				order = binary.LittleEndian
			}
			return strconv.FormatUint(order.Uint64(v[index*8:]), 10), false, nil
		}
	}
	switch e.VR() {
	case core.VRUS, core.VRSS, core.VRUL, core.VRSL, core.VRSV:
		values, err := src.GetInts(e.Tag())
		if err != nil {
			return "", false, ErrValue
		}
		return strconv.FormatInt(values[index], 10), false, nil
	case core.VRFL, core.VRFD:
		values, err := src.GetFloats(e.Tag())
		if err != nil {
			return "", false, ErrValue
		}
		bits := 64
		if e.VR() == core.VRFL {
			bits = 32
		}
		return strconv.FormatFloat(values[index], 'g', -1, bits), false, nil
	case core.VRAT:
		values, ok := src.GetTags(e.Tag())
		if !ok {
			return "", false, ErrValue
		}
		return values[index].HexString(), false, nil
	}
	return "", false, ErrValue
}

func numericWidth(vr core.VR) int {
	switch vr {
	case core.VRUS, core.VRSS:
		return 2
	case core.VRUL, core.VRSL, core.VRFL, core.VRAT:
		return 4
	case core.VRUV, core.VRSV, core.VRFD:
		return 8
	default:
		return 0
	}
}
