package dicomxml

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/parser"
)

// attributeElement transfers a parsed attribute's values directly into their
// final representation. No JSON document or intermediary dataset is involved.
func attributeElement(attr nativeAttribute, dict dictionary.DataDictionary, opts UnmarshalOptions, b *budget, path string) (core.Element, error) {
	tag, _ := core.ParseTag(attr.tag)
	vr := core.VR(attr.vr)
	elem := core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}}
	switch {
	case attr.bulk != nil:
		if opts.BulkDataPolicy == RejectBulkData {
			return elem, &Error{Op: "decode", Path: path + "/BulkData", Err: ErrBulkDataRejected}
		}
		uri := attr.bulk.URI
		if uri == "" {
			uri = "urn:uuid:" + attr.bulk.UUID
		}
		elem.Value = core.BulkDataValue{URI: uri}
	case attr.inline != nil:
		if tag == core.TagPixelData && !opts.TransferSyntax.Encapsulated && looksEncapsulatedBytes(attr.inline) {
			return elem, &Error{Op: "decode", Path: path + "/InlineBinary", Err: ErrTransferSyntaxRequired}
		}
		if tag == core.TagPixelData && opts.TransferSyntax.Encapsulated {
			fragments, err := decodeFragments(attr.inline, dict, opts, b)
			if err != nil {
				return elem, &Error{Op: "decode", Path: path + "/InlineBinary", Err: err}
			}
			elem.Value = fragments
			elem.Header.Length = core.UndefinedLength
			elem.Header.LengthSet = true
		} else {
			elem.Value = core.RawValue(attr.inline)
		}
	case vr == core.VRSQ:
		elem.Value = core.SequenceValue{Items: attr.items}
	case vr == core.VRPN:
		values := make(core.StringValue, len(attr.persons))
		for i, pn := range attr.persons {
			var parts [3]string
			last := 0
			for j, group := range pn.groups {
				if group != nil {
					parts[j] = joinNameComponents(*group)
					if parts[j] != "" {
						last = j + 1
					}
				}
			}
			values[i] = strings.Join(parts[:last], "=")
			if strings.Contains(values[i], "\\") {
				return elem, invalidXML(path+"/PersonName", "VR PN value contains the backslash value delimiter")
			}
		}
		elem.Value = values
	case vr.IsStringLike():
		if !vr.UsesTextValueDelimiter() && len(attr.values) > 1 {
			return elem, invalidXML(path+"/Value", "VR %s holds one value, got %d", vr, len(attr.values))
		}
		for i, value := range attr.values {
			if err := b.check(); err != nil {
				return elem, &Error{Op: "decode", Path: path, Err: err}
			}
			if vr.UsesTextValueDelimiter() && strings.Contains(value, "\\") {
				return elem, invalidXML(fmt.Sprintf("%s/Value[%d]", path, i+1), "VR %s value contains the backslash value delimiter", vr)
			}
			if (vr == core.VRDS || vr == core.VRIS) && value != "" {
				if err := valueencode.ValidateNumberString(vr, value); err != nil {
					return elem, invalidXML(fmt.Sprintf("%s/Value[%d]", path, i+1), "%v", err)
				}
			}
		}
		elem.Value = core.StringValue(attr.values)
	case len(attr.values) == 0:
		elem.Value = core.RawValue(nil)
	default:
		data, err := decodeNumericValues(vr, attr.values, opts.ByteOrder, b, path)
		if err != nil {
			return elem, err
		}
		elem.Value = core.RawValue(data)
	}
	return elem, nil
}

func decodeNumericValues(vr core.VR, values []string, order binary.ByteOrder, b *budget, path string) ([]byte, error) {
	width := 0
	switch vr {
	case core.VROB:
		width = 1
	case core.VRSS, core.VRUS, core.VROW:
		width = 2
	case core.VRAT, core.VRFL, core.VROF, core.VRSL, core.VRUL, core.VROL:
		width = 4
	case core.VRFD, core.VROD, core.VRSV, core.VRUV, core.VROV:
		width = 8
	default:
		return nil, invalidXML(path+"/Value", "Value is not supported for VR %s", vr)
	}
	if len(values) > int(^uint(0)>>1)/width {
		return nil, invalidXML(path, "value length overflows address space")
	}
	data := make([]byte, len(values)*width)
	for i, value := range values {
		if err := b.check(); err != nil {
			return nil, &Error{Op: "decode", Path: path, Err: err}
		}
		dest := data[i*width : (i+1)*width]
		var err error
		switch vr {
		case core.VRAT:
			var tag core.Tag
			tag, err = core.ParseTag(value)
			if err == nil {
				order.PutUint16(dest, tag.Group)
				order.PutUint16(dest[2:], tag.Element)
			}
		case core.VRFL, core.VROF, core.VRFD, core.VROD:
			var n float64
			switch strings.ToLower(value) {
			case "nan":
				n = math.NaN()
			case "inf", "+inf", "infinity", "+infinity":
				n = math.Inf(1)
			case "-inf", "-infinity":
				n = math.Inf(-1)
			default:
				n, err = strconv.ParseFloat(value, width*8)
			}
			if width == 4 {
				order.PutUint32(dest, math.Float32bits(float32(n)))
			} else {
				order.PutUint64(dest, math.Float64bits(n))
			}
		case core.VRSS, core.VRSL, core.VRSV:
			var n int64
			n, err = strconv.ParseInt(value, 10, width*8)
			putNativeUint(dest, uint64(n), order)
		default:
			var n uint64
			n, err = strconv.ParseUint(value, 10, width*8)
			putNativeUint(dest, n, order)
		}
		if err != nil {
			return nil, invalidXML(fmt.Sprintf("%s/Value[%d]", path, i+1), "invalid %s value: %v", vr, err)
		}
	}
	return data, nil
}

func putNativeUint(dst []byte, n uint64, order binary.ByteOrder) {
	switch len(dst) {
	case 1:
		dst[0] = byte(n)
	case 2:
		order.PutUint16(dst, uint16(n))
	case 4:
		order.PutUint32(dst, uint32(n))
	case 8:
		order.PutUint64(dst, n)
	}
}

func assembleDataSet(attrs []nativeAttribute, b *budget, path string) ([]core.Element, error) {
	alloc := newPrivateAllocator(attrs)
	elements := make([]core.Element, 0, len(attrs))
	indices := make(map[core.Tag]int, len(attrs))

	for _, attr := range attrs {
		if err := b.check(); err != nil {
			return nil, &Error{Op: "decode", Path: path, Err: err}
		}
		elem := attr.element
		tag := elem.Tag()
		if attr.privateCreator != "" {
			var err error
			tag, err = alloc.allocate(tag, attr.privateCreator)
			if err != nil {
				return nil, &Error{Op: "decode", Path: path, Err: err}
			}
			elem.Header.Tag = tag
			creatorTag := core.NewTag(tag.Group, tag.Element>>8)
			if _, ok := indices[creatorTag]; !ok {

				indices[creatorTag] = len(elements)
				elements = append(elements, core.Element{Header: core.ElementHeader{Tag: creatorTag, VR: core.VRLO}, Value: core.StringValue{attr.privateCreator}})
			}
		}
		if index, ok := indices[tag]; ok {
			existing := elements[index]
			values, vok := elem.Value.(core.StringValue)
			previous, pok := existing.Value.(core.StringValue)
			if attr.privateCreator == "" && dictionary.IsPrivateCreatorTag(tag) && elem.VR() == core.VRLO && existing.VR() == core.VRLO && vok && pok && len(values) == 1 && len(previous) == 1 && values[0] == previous[0] {
				continue
			}
			return nil, invalidXML(path, "duplicate resolved tag %s", tag.HexString())
		}
		indices[tag] = len(elements)
		elements = append(elements, elem)
	}
	// The XML count and final dataset count have independent bounds, as in
	// the former parser/JSON stages. Matching duplicate creator declarations
	// disappear from the final dataset; synthesized creators add to it.
	if len(elements) > b.limits.MaxElements-b.finalElements {
		return nil, &Error{Op: "decode", Path: path, Err: ErrMaxElementsExceeded}
	}
	b.finalElements += len(elements)
	sort.Slice(elements, func(i, j int) bool { return elements[i].Tag().Less(elements[j].Tag()) })
	return elements, nil
}

func decodeFragments(data []byte, dict dictionary.DataDictionary, opts UnmarshalOptions, b *budget) (core.FragmentSequence, error) {
	syntax := opts.TransferSyntax
	order := syntax.ByteOrder
	if order == nil {
		order = binary.LittleEndian
		syntax.ByteOrder = order
	}
	var header [12]byte
	order.PutUint16(header[:2], core.TagPixelData.Group)
	order.PutUint16(header[2:4], core.TagPixelData.Element)
	n := 8
	if syntax.ExplicitVR {
		n = 12
		copy(header[4:6], "OB")
		order.PutUint32(header[8:12], uint32(core.UndefinedLength))
	} else {
		order.PutUint32(header[4:8], uint32(core.UndefinedLength))
	}
	remaining := b.limits.MaxSequenceItems - b.items
	limit := remaining
	if limit == 0 {
		limit = 1
	} // Parser's zero means unlimited; reject any returned fragment below.
	r := parser.NewReader(&contextReader{ctx: b.ctx, reader: io.MultiReader(bytes.NewReader(header[:n]), bytes.NewReader(data))}, syntax, parser.ReaderOptions{Dictionary: dict, MaxPixelDataBytes: int64(len(data)), MaxFragments: limit, StrictReservedBytes: true, OddLengthPolicy: parser.RejectOddLength})
	ds, err := r.ReadDataSet()
	if err != nil {
		if errors.Is(err, parser.ErrMaxFragmentsExceeded) {
			return core.FragmentSequence{}, fmt.Errorf("%w: %v", ErrMaxItemsExceeded, err)
		}
		return core.FragmentSequence{}, fmt.Errorf("%w: encapsulated Pixel Data: %w", ErrInvalidXML, err)
	}
	if err := b.check(); err != nil {
		return core.FragmentSequence{}, err
	}
	if len(ds.Elements) != 1 || ds.Elements[0].Tag() != core.TagPixelData {
		return core.FragmentSequence{}, fmt.Errorf("%w: expected one encapsulated Pixel Data element", ErrInvalidXML)
	}
	fragments, ok := ds.Elements[0].Value.(core.FragmentSequence)
	if !ok {
		return core.FragmentSequence{}, fmt.Errorf("%w: expected encapsulated fragments", ErrInvalidXML)
	}
	if len(fragments.Fragments) > remaining {
		return core.FragmentSequence{}, ErrMaxItemsExceeded
	}
	b.items += len(fragments.Fragments)
	return fragments, nil
}
