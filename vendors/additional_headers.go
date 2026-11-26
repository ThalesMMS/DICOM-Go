package vendors

import (
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

// ErrUnsupportedHeader reports an unknown private format, type or encoding.
var ErrUnsupportedHeader = headers.ErrUnsupportedHeader

// DecodeToshibaCT3 decodes the known CT Private Data 2 wrapper and dataset.
func DecodeToshibaCT3(raw []byte, l Limits) (*object.Object, error) {
	return headers.DecodeToshibaCT3(raw, headers.Limits(l))
}

// ToshibaCT3Of locates CT Private Data 2 by its private creator.
func ToshibaCT3Of(o *object.Object, l Limits) (*object.Object, error) {
	return headers.ToshibaCT3Of(o, headers.Limits(l))
}

// DecodePMTF reverses the complete bytes and reads explicit VR LE DICOM.
func DecodePMTF(raw []byte, l Limits) (*object.Object, error) {
	return headers.DecodePMTF(raw, headers.Limits(l))
}

// PMTFOf resolves item-local PMTF/Canon/Toshiba creator reservations.
func PMTFOf(o *object.Object, creator string, offset uint8, l Limits) ([]*object.Object, error) {
	return headers.PMTFOf(o, creator, offset, headers.Limits(l))
}

// DecodeCanonPAS validates a single bounded UTF-8 XML document.
func DecodeCanonPAS(raw []byte, l Limits) (string, error) {
	return headers.DecodeCanonPAS(raw, headers.Limits(l))
}

// CanonPASOf locates PAS Reproduct Information by its private creator.
func CanonPASOf(o *object.Object, l Limits) (string, error) {
	return headers.CanonPASOf(o, headers.Limits(l))
}
