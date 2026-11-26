package object

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrPixelDataEncodingMismatch reports top-level Pixel Data whose encoding
// contradicts the transfer syntax: encapsulated fragments under a native
// syntax, or native (defined-length) bytes under an encapsulated one.
var ErrPixelDataEncodingMismatch = errors.New("dicom: Pixel Data encoding does not match the transfer syntax")

// CheckPixelDataEncoding verifies PS3.5 A.4 for top-level Pixel Data
// (7FE0,0010): its value is encapsulated (undefined length, Items and a
// Sequence Delimitation Item) exactly when syntax is encapsulated. Parsed
// elements use their wire length to identify the encoding. An empty value, an absent element and values whose form is not yet known pass. Pixel
// Data nested in items (for example Icon Image Sequence) is not constrained
// here.
func (o *Object) CheckPixelDataEncoding(syntax transfer.Syntax) error {
	if o == nil {
		return nil
	}
	element, ok := o.Get(core.TagPixelData)
	if !ok {
		return nil
	}
	encapsulated, known := pixelDataEncapsulated(element, o.parsedPixelData)
	if !known || encapsulated == syntax.Encapsulated {
		return nil
	}
	if encapsulated {
		return fmt.Errorf("%w: encapsulated Pixel Data under native transfer syntax %s", ErrPixelDataEncodingMismatch, syntax.UID)
	}
	return fmt.Errorf("%w: native Pixel Data under encapsulated transfer syntax %s", ErrPixelDataEncodingMismatch, syntax.UID)
}

func pixelDataEncapsulated(element core.Element, parsed bool) (encapsulated, known bool) {
	if parsed && element.Header.HasLength() {
		length := element.EncodedLength()
		return length.IsUndefined(), length != 0
	}
	switch value := element.Value.(type) {
	case core.FragmentSequence:
		return true, true
	case core.RawValue:
		return bytes.HasPrefix(value, []byte{0xfe, 0xff, 0x00, 0xe0}) ||
			bytes.HasPrefix(value, []byte{0xff, 0xfe, 0xe0, 0x00}), len(value) > 0
	case core.BulkDataValue:
		return false, true
	case nil:
		if !element.Header.HasLength() || element.Header.Length == 0 {
			return false, false
		}
		return element.Header.Length.IsUndefined(), true
	default:
		return false, false
	}
}
