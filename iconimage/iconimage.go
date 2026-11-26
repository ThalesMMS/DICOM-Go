// Package iconimage generates standard Icon Image Sequences and reads the
// small preview images that DICOM instances can carry, so that a browser can
// show a thumbnail without decoding the image:
// the Icon Image Sequence (0088,0200; PS3.3 F.7) and two GE private
// sequences, the GEIIS thumbnail of group 0009 and the
// GEMS_Ultrasound_ImageGroup_001 icon of group 6003.
//
// Only native (uncompressed) icon Pixel Data is rendered. An icon whose
// Pixel Data is shorter than its Image Pixel attributes describe, such as a
// compressed private icon, is skipped and the next candidate is tried.
package iconimage

import (
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

// Source names where an icon came from.
type Source string

const (
	// SourceIconImageSequence is the standard Icon Image Sequence.
	SourceIconImageSequence Source = "icon-image-sequence"
	// SourceGEIIS is the GEIIS private thumbnail sequence (0009,xx10).
	SourceGEIIS Source = "geiis"
	// SourceGEUltrasound is the GEMS_Ultrasound_ImageGroup_001 private icon
	// sequence (6003,xx10).
	SourceGEUltrasound Source = "gems-ultrasound"
)

// ErrNoIcon reports a data set without an icon that renders.
var ErrNoIcon = errors.New("iconimage: no renderable embedded icon")

// maxIconPixels bounds an icon's Rows x Columns; icons are small by
// definition (PS3.3 F.7 recommends 64 or 128 pixels across).
const maxIconPixels = 1024 * 1024

var tagIconImageSequence = core.NewTag(0x0088, 0x0200)

// privateIcon is a private sequence whose first item holds an icon.
type privateIcon struct {
	group   uint16
	creator string
	offset  uint8
	source  Source
}

var privateIcons = []privateIcon{
	{group: 0x0009, creator: "GEIIS", offset: 0x10, source: SourceGEIIS},
	{group: 0x6003, creator: "GEMS_Ultrasound_ImageGroup_001", offset: 0x10, source: SourceGEUltrasound},
}

// Icon is one rendered embedded icon.
type Icon struct {
	Image  image.Image
	Source Source
}

// Read returns the first embedded icon of dataset that renders, trying the
// standard Icon Image Sequence before the GE private icons. Each sequence
// item is a candidate in order. It returns ErrNoIcon, wrapping the last
// rendering error, when none renders.
func Read(dataset *object.Object) (Icon, error) {
	if dataset == nil {
		return Icon{}, ErrNoIcon
	}
	var last error
	try := func(tag core.Tag, source Source) (Icon, bool) {
		items, ok := dataset.GetSequence(tag)
		if !ok {
			return Icon{}, false
		}
		for _, item := range items {
			img, err := RenderItem(item)
			if err == nil {
				return Icon{Image: img, Source: source}, true
			}
			last = err
		}
		return Icon{}, false
	}
	if icon, ok := try(tagIconImageSequence, SourceIconImageSequence); ok {
		return icon, nil
	}
	for _, private := range privateIcons {
		tag, err := dataset.PrivateTag(private.group, private.creator, private.offset)
		if err != nil {
			continue
		}
		if icon, ok := try(tag, private.source); ok {
			return icon, nil
		}
	}
	if last != nil {
		return Icon{}, fmt.Errorf("%w: %w", ErrNoIcon, last)
	}
	return Icon{}, ErrNoIcon
}

// RenderItem renders the Image Pixel attributes and native Pixel Data of
// one icon item: MONOCHROME1/2 over the full stored range (icons carry no
// VOI), PALETTE COLOR through its LUTs, and RGB or YBR_FULL as stored.
func RenderItem(item *object.Object) (image.Image, error) {
	if item == nil {
		return nil, errors.New("iconimage: nil icon item")
	}
	metadata, err := pixeldata.ExtractMetadata(item)
	if err != nil {
		return nil, err
	}
	rows, columns := int(metadata.Rows), int(metadata.Columns)
	if rows <= 0 || columns <= 0 || rows*columns > maxIconPixels {
		return nil, fmt.Errorf("iconimage: icon of %dx%d pixels", columns, rows)
	}
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 {
		return nil, fmt.Errorf("iconimage: icon with BitsAllocated %d", metadata.BitsAllocated)
	}
	samples := int(metadata.SamplesPerPixel)
	if samples != 1 && samples != 3 {
		return nil, fmt.Errorf("iconimage: icon with %d samples per pixel", samples)
	}
	element, ok := item.Get(core.TagPixelData)
	if !ok {
		return nil, errors.New("iconimage: icon without Pixel Data")
	}
	raw, ok := element.RawBytes()
	if !ok {
		// Encapsulated (compressed) icons have fragments, not raw bytes.
		return nil, errors.New("iconimage: icon Pixel Data is not native")
	}
	size := rows * columns * samples * int(metadata.BitsAllocated/8)
	if len(raw) < size {
		return nil, fmt.Errorf("iconimage: icon Pixel Data holds %d bytes, its attributes describe %d", len(raw), size)
	}
	raw = raw[:size]
	order := item.ValueByteOrder()
	photometric := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	if photometric == "PALETTE COLOR" {
		palette, err := display.PaletteFromObject(item)
		if err != nil {
			return nil, err
		}
		if palette == nil {
			return nil, errors.New("iconimage: PALETTE COLOR icon without palette LUTs")
		}
		return display.RenderColor(display.ColorFrame{
			Rows:            rows,
			Columns:         columns,
			Pixels:          raw,
			Photometric:     "PALETTE COLOR",
			SamplesPerPixel: 1,
			Format: display.PixelFormat{
				BitsAllocated: int(metadata.BitsAllocated),
				BitsStored:    int(metadata.BitsStored),
				HighBit:       int(metadata.HighBit),
				ByteOrder:     order,
			},
			Palette: palette,
		})
	}
	frame := &render.Frame{
		Metadata:   metadata,
		ByteOrder:  order,
		PixelBytes: raw,
		Rescale:    render.Rescale{Slope: 1},
	}
	window := render.WindowLevel{}
	if samples == 1 {
		// The full stored range: icons are already windowed for display.
		bits := int(metadata.BitsStored)
		if bits <= 0 || bits > int(metadata.BitsAllocated) {
			bits = int(metadata.BitsAllocated)
		}
		span := float64(uint64(1) << bits)
		window = render.WindowLevel{Center: span / 2, Width: span}
		if metadata.PixelRepresentation == 1 {
			window.Center = 0
		}
	}
	frame.DefaultWindow = window
	return render.RenderFrame(frame, window)
}
