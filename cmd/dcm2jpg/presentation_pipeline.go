package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"github.com/ThalesMMS/dicom-go/render"
)

func imageOverlays(dataset *object.Object, data []byte, order binary.ByteOrder, index int) ([]display.Overlay, error) {
	planes, err := display.OverlaysFromObject(dataset)
	if err != nil {
		return nil, err
	}
	selected := make([]display.Overlay, 0, len(planes))
	for _, p := range planes {
		if frame, ok := p.ForImageFrame(index); ok {
			selected = append(selected, frame)
		}
	}
	embedded, err := display.EmbeddedOverlaysForFrame(dataset, data, order, index)
	if err != nil {
		return nil, err
	}
	return append(selected, embedded...), nil
}

func selectVOI(dataset *object.Object, metadata pixeldata.Metadata, frame display.Frame, index int, opts options) (display.VOILUT, error) {
	if opts.window != nil {
		return *opts.window, nil
	}
	if opts.autoWindow != "" {
		if frame.Modality.LUT != nil {
			return display.VOILUT{}, errors.New("automatic strategies/region do not support a nonlinear Modality LUT; use -min-max or -window")
		}
		if !opts.region.Empty() && !opts.region.In(image.Rect(0, 0, frame.Columns, frame.Rows)) {
			return display.VOILUT{}, errors.New("window region must be inside the input frame")
		}
		padding, err := pixeldata.PixelPaddingFromObject(dataset, pixeldata.SampleFormat{BitsAllocated: frame.Format.BitsAllocated, BitsStored: frame.Format.BitsStored, HighBit: frame.Format.HighBit, Signed: frame.Format.Signed, ByteOrder: frame.Format.ByteOrder})
		if err != nil {
			return display.VOILUT{}, err
		}
		input := &render.Frame{Metadata: metadata, PixelBytes: frame.Pixels, ByteOrder: frame.Format.ByteOrder, Rescale: render.Rescale{Slope: frame.Modality.Slope, Intercept: frame.Modality.Intercept}, PixelPadding: padding}
		auto := render.AutoWindowOptions{Region: opts.region, HistogramIgnore: opts.histogramIgnore}
		switch opts.autoWindow {
		case "ignore-extremes":
			auto.Strategy = render.AutoWindowMinMaxIgnoreExtremes
		case "histogram":
			auto.Strategy = render.AutoWindowHistogram
		}
		window, ok := render.AutoWindowWith(input, auto)
		if !ok {
			return display.VOILUT{}, errors.New("automatic window has no usable nonconstant range")
		}
		return display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function}, nil
	}
	if !opts.minMax {
		vois, err := display.VOIsAt(dataset, index)
		if err != nil {
			return display.VOILUT{}, err
		}
		if len(vois) >= opts.voi {
			return vois[opts.voi-1], nil
		}
		if opts.voi > 1 {
			return display.VOILUT{}, fmt.Errorf("VOI %d requested; the frame has %d", opts.voi, len(vois))
		}
	}
	return minMaxWindow(frame)
}

func renderPresentedGray(dataset *object.Object, metadata pixeldata.Metadata, data []byte, index int, order binary.ByteOrder, opts options) (image.Image, error) {
	modality, err := display.ModalityLUTAt(dataset, index)
	if err != nil {
		return nil, err
	}
	frame := display.Frame{Rows: int(metadata.Rows), Columns: int(metadata.Columns), Pixels: data, Format: display.PixelFormat{BitsAllocated: int(metadata.BitsAllocated), BitsStored: int(metadata.BitsStored), HighBit: int(metadata.HighBit), Signed: metadata.PixelRepresentation == 1, ByteOrder: order}, Modality: modality}
	if frame.Rows <= 0 || frame.Columns <= 0 || frame.Rows > display.MaxExportPixels/frame.Columns {
		return nil, errors.New("frame exceeds 67108864 output pixels")
	}
	if opts.state != nil {
		if opts.bits == 16 {
			return nil, errors.New("GSPS rendering supports 8-bit output")
		}
		uid, ok := dataset.GetUID(core.NewTag(0x0008, 0x0018))
		if !ok || strings.TrimSpace(uid) == "" {
			return nil, errors.New("GSPS rendering requires the image SOP Instance UID")
		}
		planes, err := display.OverlaysFromObject(dataset)
		if err != nil {
			return nil, err
		}
		// Embedded overlays are already restricted to this image frame. Restore
		// its origin/count for gsps.Render's frame selection.
		embedded, err := display.EmbeddedOverlaysForFrame(dataset, data, order, index)
		if err != nil {
			return nil, err
		}
		for i := range embedded {
			embedded[i].FrameOrigin = index + 1
		}
		img, err := gsps.Render(frame, opts.state, gsps.ImageState{SOPInstanceUID: uid, FrameNumber: index + 1, Rows: frame.Rows, Columns: frame.Columns}, gsps.RenderOptions{ImageOverlays: append(planes, embedded...)})
		if err != nil {
			return nil, err
		}
		if opts.displayLUT != nil {
			if err := opts.displayLUT.ApplyGray(img); err != nil {
				return nil, err
			}
		}
		return img, nil
	}
	frame.VOI, err = selectVOI(dataset, metadata, frame, index, opts)
	if err != nil {
		return nil, err
	}
	pres := display.Presentation{}
	if opts.presentation {
		pres, err = display.PresentationFromObject(dataset)
		if err != nil {
			return nil, err
		}
		if opts.bits == 16 && pres.LinOD {
			pres.LUT, err = display.PrintDensityFromObject(dataset).LinODPresentationLUT(16)
			if err != nil {
				return nil, err
			}
		}
		pres.Overlays = nil
	}
	// MONOCHROME1 inversion and the Presentation LUT INVERSE combine by XOR.
	pres.Invert = pres.Invert != strings.EqualFold(strings.TrimSpace(metadata.PhotometricInterpretation), "MONOCHROME1")
	if opts.overlays {
		pres.Overlays, err = imageOverlays(dataset, data, order, index)
		if err != nil {
			return nil, err
		}
	}
	if opts.bits == 16 {
		img, err := display.RenderGray16(frame)
		if err != nil {
			return nil, err
		}
		pres.ApplyGray16(img)
		if opts.displayLUT != nil {
			img = opts.displayLUT.ApplyGray16(img)
		}
		return img, nil
	}
	img, err := display.RenderGray(frame)
	if err != nil {
		return nil, err
	}
	pres.Apply(img)
	if opts.displayLUT != nil {
		if err := opts.displayLUT.ApplyGray(img); err != nil {
			return nil, err
		}
	}
	return img, nil
}
