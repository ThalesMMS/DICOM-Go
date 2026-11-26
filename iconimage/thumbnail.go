package iconimage

import (
	"context"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"io"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	dicomframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
	"github.com/ThalesMMS/dicom-go/render"
)

var tagRepresentativeFrameNumber = core.NewTag(0x0028, 0x6010)

// RepresentativeFrameIndex returns the zero-based index specified by
// Representative Frame Number (0028,6010). It requires exactly one US value
// in the frame range. The caller selects a fallback when ok is false.
func RepresentativeFrameIndex(dataset *object.Object, frames int) (index int, ok bool) {
	element, present := dataset.Get(tagRepresentativeFrameNumber)
	if !present || element.VR() != core.VRUS || dataset.HasDuplicateParsedTag(tagRepresentativeFrameNumber) {
		return 0, false
	}
	values, err := dataset.GetInts(tagRepresentativeFrameNumber)
	if err != nil || len(values) != 1 || values[0] < 1 || values[0] > int64(frames) {
		return 0, false
	}
	return int(values[0]) - 1, true
}

// ThumbnailFrameOptions controls frame selection and reduction for a preview.
type ThumbnailFrameOptions struct {
	// Source is required when the file has deferred Pixel Data.
	Source io.ReaderAt
	// Registry supplies any compressed transfer syntax codec needed.
	Registry pixeldata.Registry
	// TargetSide is the desired longest side after final fitting. Zero
	// requests display at full resolution.
	TargetSide int
	// FallbackFrame is the zero-based frame to use without a representative
	// number. The zero value selects the first frame.
	FallbackFrame int
	// FrameIndex selects one exact zero-based frame when provided, even if
	// the object declares a different representative frame.
	FrameIndex *int
	// CodecReductionOnly requires reduced codec-native decoding. Callers can
	// retry with their usual decode registry and allow averaging.
	CodecReductionOnly bool
}

// RenderThumbnailFrame decodes the selected frame and applies its display
// transforms. The decode reduction preserves enough detail
// for TargetSide, when possible. Use FitThumbnail for the final size bound.
func RenderThumbnailFrame(ctx context.Context, file *object.File, opts ThumbnailFrameOptions) (image.Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if opts.TargetSide < 0 {
		return nil, fmt.Errorf("%w: negative thumbnail size", ErrInvalidOptions)
	}
	reader, err := pixeldata.NewFrameReader(ctx, file, opts.Source, pixeldata.FrameReaderOptions{Registry: opts.Registry})
	if err != nil {
		return nil, err
	}
	defer reader.Close()
	metadata := reader.Metadata()
	index := opts.FallbackFrame
	if opts.FrameIndex != nil {
		index = *opts.FrameIndex
	} else if representative, ok := RepresentativeFrameIndex(file.Dataset, metadata.NumberOfFrames); ok {
		index = representative
	}
	level := reductionLevel(max(int(metadata.Rows), int(metadata.Columns)), opts.TargetSide)
	if opts.CodecReductionOnly {
		if level > 0 {
			capabilities, err := reader.PartialDecodeCapabilities(ctx, index)
			if err != nil {
				return nil, err
			}
			level = min(level, capabilities.ReductionLevels)
		}
		if level == 0 {
			return nil, fmt.Errorf("%w: no lower resolution available for thumbnail", pixeldata.ErrPartialDecodeUnsupported)
		}
	}
	return renderThumbnailFrame(ctx, file.Dataset, reader, index, level, opts.CodecReductionOnly)
}

// reductionLevel is the largest n for which the longest side reduced by
// 2^n still covers size.
func reductionLevel(longest, size int) int {
	if size == 0 {
		return 0
	}
	level := 0
	for level < pixeldata.MaxReductionLevel && longest>>(level+1) >= size {
		level++
	}
	return level
}

func renderThumbnailFrame(ctx context.Context, dataset *object.Object, reader *pixeldata.FrameReader, index, level int, codecOnly bool) (image.Image, error) {
	result, err := reader.DecodePartial(ctx, pixeldata.PartialDecodeRequest{Frame: index, ReductionLevel: level, NoFallback: codecOnly})
	if err != nil {
		return nil, err
	}
	if strings.EqualFold(strings.TrimSpace(result.Metadata.PhotometricInterpretation), "PALETTE COLOR") {
		palette, err := display.PaletteFromObject(dataset)
		if err != nil || palette == nil {
			return nil, fmt.Errorf("iconimage: PALETTE COLOR without palette LUTs: %v", err)
		}
		return display.RenderColor(display.ColorFrame{
			Rows:            int(result.Metadata.Rows),
			Columns:         int(result.Metadata.Columns),
			Pixels:          result.Data,
			Photometric:     "PALETTE COLOR",
			SamplesPerPixel: 1,
			Format: display.PixelFormat{
				BitsAllocated: int(result.Metadata.BitsAllocated),
				BitsStored:    int(result.Metadata.BitsStored),
				HighBit:       int(result.Metadata.HighBit),
				ByteOrder:     binary.LittleEndian,
			},
			Palette: palette,
		})
	}
	rescale := dicomframe.RescaleAt(dataset, index)
	frame := &render.Frame{
		FrameIndex: index,
		Metadata:   result.Metadata,
		ByteOrder:  binary.LittleEndian,
		PixelBytes: result.Data,
		Rescale:    render.Rescale{Slope: rescale.Slope, Intercept: rescale.Intercept},
	}
	window, ok := render.WindowLevel{}, false
	if voi, found := dicomframe.WindowAt(dataset, index); found {
		window, ok = render.WindowLevel{Center: voi.Center, Width: voi.Width, Function: voi.Function}, true
	}
	if !ok && result.Metadata.SamplesPerPixel == 1 {
		window, _ = render.AutoWindow(frame)
	}
	frame.DefaultWindow = window
	return render.RenderFrame(frame, window)
}

// FitThumbnail reduces img to at most size pixels on its longest side by averaging
// (never enlarging), as 8-bit gray when every pixel is gray. It returns nil
// for a nil or empty image or a nonpositive size.
func FitThumbnail(img image.Image, size int) image.Image {
	if img == nil || img.Bounds().Empty() || size <= 0 {
		return nil
	}
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	targetW, targetH := width, height
	if longest := max(width, height); longest > size {
		targetW = max(1, int((int64(width)*int64(size)+int64(longest)/2)/int64(longest)))
		targetH = max(1, int((int64(height)*int64(size)+int64(longest)/2)/int64(longest)))
	}
	// Each output pixel is the mean of the source cell [x0,x1) x [y0,y1).
	cell := func(t, target, extent int) (int, int) {
		from := t * extent / target
		return from, max(from+1, (t+1)*extent/target)
	}
	if gray, ok := img.(*image.Gray); ok {
		if targetW == width && targetH == height {
			return gray
		}
		out := image.NewGray(image.Rect(0, 0, targetW, targetH))
		for ty := range targetH {
			y0, y1 := cell(ty, targetH, height)
			for tx := range targetW {
				x0, x1 := cell(tx, targetW, width)
				sum, n := 0, 0
				for y := y0; y < y1; y++ {
					row := gray.Pix[y*gray.Stride:]
					for x := x0; x < x1; x++ {
						sum += int(row[x])
						n++
					}
				}
				out.Pix[ty*out.Stride+tx] = uint8((sum + n/2) / n)
			}
		}
		return out
	}
	// Other images: through the generic color model, kept as gray when
	// every output pixel is gray.
	isGray := true
	out := image.NewNRGBA(image.Rect(0, 0, targetW, targetH))
	for ty := range targetH {
		y0, y1 := cell(ty, targetH, height)
		for tx := range targetW {
			x0, x1 := cell(tx, targetW, width)
			var r, g, b, a, n uint64
			for y := y0; y < y1; y++ {
				for x := x0; x < x1; x++ {
					pr, pg, pb, pa := img.At(bounds.Min.X+x, bounds.Min.Y+y).RGBA()
					r, g, b, a, n = r+uint64(pr), g+uint64(pg), b+uint64(pb), a+uint64(pa), n+1
				}
			}
			px := color.NRGBA64{R: uint16(r / n), G: uint16(g / n), B: uint16(b / n), A: uint16(a / n)}
			c := color.NRGBAModel.Convert(px).(color.NRGBA)
			if c.R != c.G || c.G != c.B {
				isGray = false
			}
			out.SetNRGBA(tx, ty, c)
		}
	}
	if !isGray {
		return out
	}
	grayImage := image.NewGray(out.Bounds())
	for i := range grayImage.Pix {
		grayImage.Pix[i] = out.Pix[i*4]
	}
	return grayImage
}
