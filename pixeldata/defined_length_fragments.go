package pixeldata

import (
	"encoding/binary"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// DiagnosticDefinedLengthFragments: the transfer syntax declared
// encapsulated Pixel Data but the value had a defined length (PS3.5 A.4
// requires an undefined length); its bytes were exactly a Basic Offset
// Table Item and fragment Items, and DecodeRecovery.DefinedLengthFragments
// read them as the fragment sequence.
const DiagnosticDefinedLengthFragments = "encapsulated-defined-length"

// maxDefinedLengthFragments bounds the Items a defined-length value may be
// split into, as the encapsulated frame planner bounds fragments.
const maxDefinedLengthFragments = 100_000

// definedLengthFragments returns dataset, or a copy of it whose Pixel Data
// is the fragment sequence that a defined-length value under an
// encapsulated transfer syntax holds, when the value splits exactly into
// Items (see splitDefinedLengthItems).
func definedLengthFragments(dataset *object.Object, source transfer.Syntax) (*object.Object, bool) {
	if !source.Encapsulated {
		return dataset, false
	}
	pixel, err := ExtractView(dataset)
	if err != nil || pixel.Encapsulated {
		return dataset, false
	}
	sequence, ok := splitDefinedLengthItems(pixel.Raw, maxDefinedLengthFragments)
	if !ok {
		return dataset, false
	}
	element := core.Element{
		Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength},
		Value:  sequence,
	}
	out := object.FromElements(replaceElement(dataset.Elements(), element), nil)
	out.SetValueByteOrder(dataset.ValueByteOrder())
	return out, true
}

// splitDefinedLengthItems splits value into the Items of an encapsulated
// Pixel Data value: a Basic Offset Table Item, at least one fragment Item,
// and optionally a Sequence Delimitation Item of length 0 that ends value.
// Every Item has a defined length within value, and the Items must end
// exactly at the end of value; otherwise ok is false.
func splitDefinedLengthItems(value []byte, maxFragments int) (sequence core.FragmentSequence, ok bool) {
	var items [][]byte
	for pos := 0; pos < len(value); {
		if len(value)-pos < 8 {
			return core.FragmentSequence{}, false
		}
		group := binary.LittleEndian.Uint16(value[pos:])
		element := binary.LittleEndian.Uint16(value[pos+2:])
		length := binary.LittleEndian.Uint32(value[pos+4:])
		switch {
		case group == 0xFFFE && element == 0xE0DD && length == 0 && pos+8 == len(value):
			pos += 8
		case group == 0xFFFE && element == 0xE000 && length != 0xFFFFFFFF && uint64(length) <= uint64(len(value)-pos-8):
			if len(items) > maxFragments {
				return core.FragmentSequence{}, false
			}
			items = append(items, value[pos+8:pos+8+int(length)])
			pos += 8 + int(length)
		default:
			return core.FragmentSequence{}, false
		}
	}
	if len(items) < 2 {
		return core.FragmentSequence{}, false
	}
	return core.FragmentSequence{OffsetTable: items[0], Fragments: items[1:]}, true
}
