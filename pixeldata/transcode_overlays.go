package pixeldata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ErrTranscodeEmbeddedOverlay refuses an encoded target that would drop
// overlays stored in unused bits of the pixel samples; see
// TranscodeOptions.ExtractEmbeddedOverlays.
var ErrTranscodeEmbeddedOverlay = errors.New("dicom: transcode would drop overlays embedded in pixel data")

// moveEmbeddedOverlays returns the decoded data set with every overlay that
// lives in unused sample bits moved to 1-bit Overlay Data (60xx,3000), those
// bits cleared from the samples, and the count moved. Without extract, such
// overlays refuse the transcode.
func moveEmbeddedOverlays(decoded *object.Object, extract bool) (*object.Object, int, error) {
	native, ok := decoded.GetRaw(core.TagPixelData)
	if !ok {
		return decoded, 0, nil
	}
	overlays, err := display.EmbeddedOverlays(decoded, native, binary.LittleEndian)
	if err != nil {
		return nil, 0, fmt.Errorf("%w: %w", ErrTranscodeEmbeddedOverlay, err)
	}
	if len(overlays) == 0 {
		return decoded, 0, nil
	}
	if !extract {
		return nil, 0, fmt.Errorf("%w: %d overlay group(s); set TranscodeOptions.ExtractEmbeddedOverlays", ErrTranscodeEmbeddedOverlay, len(overlays))
	}
	metadata, err := ExtractMetadata(decoded)
	if err != nil {
		return nil, 0, err
	}
	width := int(metadata.BitsAllocated) / 8
	pixels := append([]byte(nil), native...)
	var clear uint16
	for _, overlay := range overlays {
		clear |= 1 << uint(overlay.BitPosition)
	}
	for offset := 0; offset+width <= len(pixels); offset += width {
		if width == 2 {
			binary.LittleEndian.PutUint16(pixels[offset:], binary.LittleEndian.Uint16(pixels[offset:])&^clear)
		} else {
			pixels[offset] &^= byte(clear)
		}
	}
	elements := replaceElement(decoded.Elements(), core.NewRawElement(core.TagPixelData, nativePixelDataVR(metadata), pixels))
	for _, overlay := range overlays {
		data := overlay.Data
		if len(data)%2 != 0 {
			data = append(append([]byte(nil), data...), 0)
		}
		elements = replaceElement(elements, uint16RawElement(core.NewTag(overlay.Group, 0x0010), uint16(overlay.Rows)))
		elements = replaceElement(elements, uint16RawElement(core.NewTag(overlay.Group, 0x0011), uint16(overlay.Columns)))
		elements = replaceElement(elements, uint16RawElement(core.NewTag(overlay.Group, 0x0100), 1))
		elements = replaceElement(elements, uint16RawElement(core.NewTag(overlay.Group, 0x0102), 0))
		elements = replaceElement(elements, core.Element{
			Header: core.ElementHeader{Tag: core.NewTag(overlay.Group, 0x0015), VR: core.VRIS},
			Value:  core.StringValue{strconv.Itoa(overlay.FrameCount())},
		})
		elements = replaceElement(elements, uint16RawElement(core.NewTag(overlay.Group, 0x0051), uint16(overlay.FrameOrigin)))
		elements = replaceElement(elements, core.NewRawElement(core.NewTag(overlay.Group, 0x3000), core.VROW, data))
	}
	out := object.FromElements(elements, nil)
	out.SetValueByteOrder(binary.LittleEndian)
	return out, len(overlays), nil
}
