package dicomdir

import (
	"context"
	"encoding/binary"
	"fmt"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/iconimage"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const defaultIconSize = 64

var (
	tagNumberOfFramesFS = core.NewTag(0x0028, 0x0008)
	tagPixelDataFS      = core.NewTag(0x7FE0, 0x0010)
)

// renderIcon delegates generation to the reusable public API. DICOMDIR
// profiles retain their MONOCHROME2 representation and Little Endian values.
func renderIcon(ctx context.Context, file *object.File, size int, registry pixeldata.Registry) (core.Element, error) {
	icon, err := iconimage.Generate(ctx, file, iconimage.GenerateOptions{
		Width: size, Height: size, Monochrome: true, IgnorePadding: true,
		Registry: registry, ByteOrder: binary.LittleEndian,
		// Keep the prior compressed-source admission for larger multi-frame
		// objects. Native sources now receive these same finite bounds.
		Limits: pixeldata.DefaultDecompressLimits(),
	})
	if err != nil {
		return core.Element{}, fmt.Errorf("%w: %w", ErrIconUnavailable, err)
	}
	return icon, nil
}

// iconSequence encodes an Icon Image Sequence (0088,0200) item (PS3.3
// F.7): 8-bit unsigned MONOCHROME2 with Pixel Data as OB.
func iconSequence(pixels []byte, size int) core.Element {
	us := func(tag core.Tag, value int) core.Element {
		raw := make([]byte, 2)
		binary.LittleEndian.PutUint16(raw, uint16(value))
		return core.NewRawElement(tag, core.VRUS, raw)
	}
	item := core.DataSet{Elements: []core.Element{
		us(tagSamplesPerPixelFS, 1),
		textElement(tagPhotometricFS, core.VRCS, "MONOCHROME2"),
		us(tagRowsFS, size),
		us(tagColumnsFS, size),
		textValuesElement(core.NewTag(0x0028, 0x0034), core.VRIS, []string{"1", "1"}),
		us(tagBitsAllocatedFS, 8),
		us(tagBitsStoredFS, 8),
		us(tagHighBitFS, 7),
		us(tagPixelRepresentationFS, 0),
		core.NewRawElement(tagPixelDataFS, core.VROB, pixels),
	}}
	return core.Element{
		Header: core.ElementHeader{Tag: tagIconImageSequence, VR: core.VRSQ},
		Value:  core.SequenceValue{Items: []core.DataSet{item}},
	}
}

func validIconSize(size int) bool { return size >= 1 && size <= 128 }
