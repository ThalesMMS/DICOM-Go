package dimse

import (
	"bytes"
	"fmt"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func EncodeCommandSet(elements []core.Element) ([]byte, error) {
	withoutGroupLength := make([]core.Element, 0, len(elements))
	for _, elem := range elements {
		if elem.Tag() == CommandGroupLength {
			continue
		}
		withoutGroupLength = append(withoutGroupLength, commandUIDElement(elem))
	}

	obj := object.FromElements(withoutGroupLength, std.Dictionary)
	var command bytes.Buffer
	if err := object.WriteDataSet(&command, obj, transfer.ImplicitVRLittleEndian); err != nil {
		return nil, fmt.Errorf("dicom dimse: encode command set: %w", err)
	}
	if uint64(command.Len()) > math.MaxUint32 {
		return nil, fmt.Errorf("dicom dimse: command group length %d exceeds uint32", command.Len())
	}

	withGroupLength := append([]core.Element{
		newULCommandElement(CommandGroupLength, uint32(command.Len())),
	}, withoutGroupLength...)
	obj = object.FromElements(withGroupLength, std.Dictionary)

	var out bytes.Buffer
	if err := object.WriteDataSet(&out, obj, transfer.ImplicitVRLittleEndian); err != nil {
		return nil, fmt.Errorf("dicom dimse: encode command set with group length: %w", err)
	}
	return out.Bytes(), nil
}

func DecodeCommandSet(data []byte) (*object.Object, error) {
	obj, err := object.ReadDataSet(bytes.NewReader(data), transfer.ImplicitVRLittleEndian)
	if err != nil {
		return nil, fmt.Errorf("dicom dimse: decode command set: %w", err)
	}
	return obj, nil
}

// commandUIDElement encodes a command UI element as raw, NUL-padded bytes.
// Command UIDs repeat the identity of a dataset or service, which may be a
// legacy value read from a file; forwarding such an object must not fail on
// the copy. Authored dataset UIDs are still validated by the writer, and
// receivers apply their own UID checks.
func commandUIDElement(elem core.Element) core.Element {
	values, ok := elem.Value.(core.StringValue)
	if !ok || elem.Header.VR != core.VRUI {
		return elem
	}
	raw := []byte(strings.Join(values, "\\"))
	if len(raw)%2 != 0 {
		raw = append(raw, 0)
	}
	return core.NewRawElement(elem.Tag(), core.VRUI, raw)
}
