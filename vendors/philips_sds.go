package vendors

import (
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/vendors/headers"
)

// SDSValue is a typed Philips version 8 field with owned strings.
type SDSValue = headers.SDSValue

// PhilipsSDSItem is one Protocol Data sequence item.
type PhilipsSDSItem = headers.PhilipsSDSItem

// DecodePhilipsSDS decodes the known v8 binary directory.
func DecodePhilipsSDS(raw []byte, l Limits) ([]SDSValue, error) {
	return headers.DecodePhilipsSDS(raw, headers.Limits(l))
}

// PhilipsSDSOf locates the sequence through its actual private creator.
func PhilipsSDSOf(o *object.Object, l Limits) ([]PhilipsSDSItem, error) {
	return headers.PhilipsSDSOf(o, headers.Limits(l))
}
