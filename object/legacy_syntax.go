package object

import (
	"encoding/binary"
	"errors"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// ErrLegacySyntaxFrameSink refuses frame and encapsulated sinks for legacy
// encodings: they stream values during the parse, before normalization.
var ErrLegacySyntaxFrameSink = errors.New("dicom: legacy transfer syntaxes cannot stream frames to a sink")

var tagBitsAllocated = core.NewTag(0x0028, 0x0100)

// normalizeLegacyDataSet rewrites a data set read with a legacy encoding as
// Implicit VR Little Endian values: every fixed-width value of an ACR-NEMA
// Implicit VR Big Endian data set is reversed (OW, Pixel Data included, by
// 16-bit words), and the Big Endian 16-bit native Pixel Data of the GE
// private syntax is swapped. UN values keep their bytes.
func normalizeLegacyDataSet(ds core.DataSet, legacy transfer.Syntax) core.DataSet {
	switch legacy.UID {
	case transfer.ImplicitVRBigEndianACRNEMA.UID:
		return littleEndianValues(ds)
	case transfer.PrivateGEImplicitVRBigEndianPixelData.UID:
		if bitsAllocated(ds, binary.LittleEndian) != 16 {
			return ds
		}
		elements := append([]core.Element(nil), ds.Elements...)
		for i, element := range elements {
			if raw, ok := element.Value.(core.RawValue); ok && element.Tag() == core.TagPixelData {
				element.Value = core.RawValue(valueencode.ReverseFixedWidth(raw, 2))
				elements[i] = element
			}
		}
		ds.Elements = elements
	}
	return ds
}

func littleEndianValues(ds core.DataSet) core.DataSet {
	elements := make([]core.Element, len(ds.Elements))
	for i, element := range ds.Elements {
		switch value := element.Value.(type) {
		case core.RawValue:
			if width := valueencode.ByteOrderWidth(element.VR()); width > 1 && len(value)%width == 0 {
				element.Value = core.RawValue(valueencode.ReverseFixedWidth(value, width))
			}
		case core.SequenceValue:
			items := make([]core.DataSet, len(value.Items))
			for j, item := range value.Items {
				items[j] = littleEndianValues(item)
			}
			element.Value = core.SequenceValue{Items: items}
		}
		elements[i] = element
	}
	ds.Elements = elements
	return ds
}

func bitsAllocated(ds core.DataSet, order binary.ByteOrder) int {
	for _, element := range ds.Elements {
		if element.Tag() != tagBitsAllocated {
			continue
		}
		if raw, ok := element.Value.(core.RawValue); ok && len(raw) >= 2 {
			return int(order.Uint16(raw))
		}
	}
	return 0
}

// legacySwapWidths returns, for deferred top-level values of a legacy data
// set, the unit reversed while they are copied, matching normalizeLegacyDataSet.
func legacySwapWidths(ds core.DataSet, legacy transfer.Syntax) func(core.Tag) int {
	switch legacy.UID {
	case transfer.ImplicitVRBigEndianACRNEMA.UID:
		widths := make(map[core.Tag]int)
		for _, element := range ds.Elements {
			widths[element.Tag()] = valueencode.ByteOrderWidth(element.VR())
		}
		return func(tag core.Tag) int { return widths[tag] }
	case transfer.PrivateGEImplicitVRBigEndianPixelData.UID:
		if bitsAllocated(ds, binary.LittleEndian) != 16 {
			return nil
		}
		return func(tag core.Tag) int {
			if tag == core.TagPixelData {
				return 2
			}
			return 0
		}
	}
	return nil
}

// normalizeLegacyResolution records a legacy encoding in Legacy and makes
// Implicit VR Little Endian, the encoding of the normalized values, the
// effective syntax.
func normalizeLegacyResolution(resolution *TransferSyntaxResolution) {
	if legacy, ok := transfer.LegacySyntax(resolution.Syntax.UID); ok {
		resolution.Legacy = legacy
		resolution.Syntax = transfer.ImplicitVRLittleEndian
	}
}
