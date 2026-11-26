// Package presentationcli wraps GSPS creation and headless PGM rendering.
package presentationcli

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/gsps"
	"github.com/ThalesMMS/dicom-go/internal/wrappercli"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"image"
	"io"
)

func Execute(ctx context.Context, tool string, args []string, out, errout io.Writer) error {
	f := wrappercli.Flags(tool, "[flags] image.dcm output", errout)
	var statePath string
	var frame int
	f.StringVar(&statePath, "presentation-state", "", "GSPS file for dcmp2pgm (otherwise generated from image)")
	f.IntVar(&frame, "frame", 1, "one-based frame to render")
	if err := wrappercli.Parse(f, args, 2, 2); err != nil {
		return err
	}
	if frame <= 0 {
		return wrappercli.ErrUsage
	}
	file, err := wrappercli.ReadDICOM(f.Arg(0), tool == "dcmpsmk")
	if err != nil {
		return err
	}
	defer file.Close()
	state, err := gsps.FromImage(file.Dataset, gsps.FromImageOptions{})
	if err != nil {
		return err
	}
	if tool == "dcmpsmk" {
		if statePath != "" || frame != 1 {
			return wrappercli.ErrUsage
		}
		gs, err := gsps.Write(state)
		if err != nil {
			return err
		}
		return wrappercli.WriteFile(f.Arg(1), gs)
	}
	if statePath != "" {
		gs, err := wrappercli.ReadDICOM(statePath, true)
		if err != nil {
			return err
		}
		defer gs.Close()
		state, err = gsps.Read(gs.Dataset)
		if err != nil {
			return err
		}
	}
	registry, err := builtin.NewRegistry()
	if err != nil {
		return err
	}
	reader, err := pixeldata.NewFrameReader(ctx, file, nil, pixeldata.FrameReaderOptions{Registry: registry, Limits: pixeldata.DecompressLimits{MaxNativeBytes: wrappercli.MaxInputBytes, MaxInputBytes: wrappercli.MaxInputBytes, MaxPixels: 1 << 24}})
	if err != nil {
		return err
	}
	defer reader.Close()
	pixels, metadata, err := reader.Frame(ctx, frame-1)
	if err != nil {
		return err
	}
	rows, cols := int(metadata.Rows), int(metadata.Columns)
	if rows <= 0 || cols <= 0 || rows > (1<<24)/cols {
		return errors.New("frame exceeds 16777216 pixels")
	}
	if metadata.SamplesPerPixel != 1 || (metadata.PhotometricInterpretation != "MONOCHROME1" && metadata.PhotometricInterpretation != "MONOCHROME2") {
		return errors.New("PGM requires a monochrome image")
	}
	order := binary.ByteOrder(binary.LittleEndian)
	if !file.TransferSyntax.Encapsulated {
		order = file.TransferSyntax.ByteOrder
	}
	modality, err := display.ModalityLUTFromObject(file.Dataset)
	if err != nil {
		return err
	}
	uid, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	overlays, err := display.OverlaysFromObject(file.Dataset)
	if err != nil {
		return err
	}
	embedded, err := display.EmbeddedOverlaysForFrame(file.Dataset, pixels, order, frame-1)
	if err != nil {
		return err
	}
	img, err := gsps.Render(display.Frame{Rows: rows, Columns: cols, Pixels: pixels, Format: display.PixelFormat{BitsAllocated: int(metadata.BitsAllocated), BitsStored: int(metadata.BitsStored), HighBit: int(metadata.HighBit), Signed: metadata.PixelRepresentation == 1, ByteOrder: order}, Modality: modality}, state, gsps.ImageState{SOPInstanceUID: uid, FrameNumber: frame, Rows: rows, Columns: cols}, gsps.RenderOptions{WholeImage: true, ImageOverlays: append(overlays, embedded...)})
	if err != nil {
		return err
	}
	return wrappercli.WriteNew(f.Arg(1), func(w io.Writer) error { return WritePGM(w, img) })
}
func WritePGM(w io.Writer, img *image.Gray) error {
	if _, err := fmt.Fprintf(w, "P5\n%d %d\n255\n", img.Rect.Dx(), img.Rect.Dy()); err != nil {
		return err
	}
	for y := img.Rect.Min.Y; y < img.Rect.Max.Y; y++ {
		start := img.PixOffset(img.Rect.Min.X, y)
		if _, err := w.Write(img.Pix[start : start+img.Rect.Dx()]); err != nil {
			return err
		}
	}
	return nil
}
