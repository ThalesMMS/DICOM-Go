package render

import (
	"context"
	"fmt"
	"image"
	"io"
)

// GIFFrameProducer returns the same immutable frame for each index on replay.
// It must honor ctx and retain no previous frames. The encoder does not close it.
type GIFFrameProducer func(context.Context, int) (image.Image, error)

// AdaptiveGIFStreamOptions sets explicit working-memory and palette sample bounds.
// MaxFramePixels includes the producer's frame; MaxWorkspaceBytes reserves
// 64 MiB for the bounded histogram and 16 bytes/pixel plus dither rows and I/O.
// External renderers must enforce their own GPU/source residency budget.
type AdaptiveGIFStreamOptions struct {
	GIFOptions
	MaxFramePixels    int64
	MaxWorkspaceBytes int64
	SampleFrames      int
	SamplePixels      int
}

// EncodeAdaptiveGIFProducer samples at most SampleFrames evenly spaced frames,
// then replays the entire sequence with one stable median-cut palette. Defaults
// are 32 frames, 2^19 pixels, 16 Mi pixels/frame and 384 MiB workspace. A rare
// colour absent from the temporal sample may be approximated; no per-frame
// palette or silent grayscale fallback is used. All dimensions must match.
func EncodeAdaptiveGIFProducer(ctx context.Context, out io.Writer, total int, at GIFFrameProducer, opts AdaptiveGIFStreamOptions, progress func(int, int)) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if total < 1 || total > 1<<30 || at == nil || out == nil {
		return fmt.Errorf("render: invalid GIF producer")
	}
	if opts.Palette != GIFPaletteGlobal {
		return fmt.Errorf("render: streaming adaptive GIF requires global palette")
	}
	colors := opts.Colors
	if colors == 0 {
		colors = 256
	}
	if colors < 2 || colors > 256 {
		return fmt.Errorf("render: invalid GIF palette size")
	}
	if opts.MaxFramePixels == 0 {
		opts.MaxFramePixels = 16 << 20
	}
	if opts.MaxWorkspaceBytes == 0 {
		opts.MaxWorkspaceBytes = 384 << 20
	}
	if opts.SampleFrames == 0 {
		opts.SampleFrames = 32
	}
	if opts.SamplePixels == 0 {
		opts.SamplePixels = gifPaletteSamples
	}
	if opts.MaxFramePixels < 1 || opts.MaxWorkspaceBytes < 65<<20 || opts.SampleFrames < 1 || opts.SampleFrames > 32 || opts.SamplePixels < 1 || opts.SamplePixels > gifPaletteSamples {
		return fmt.Errorf("render: invalid GIF memory/sample bounds")
	}
	width, height := 0, 0
	frame := func(index int) (image.Image, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		img, err := at(ctx, index)
		if err != nil {
			return nil, fmt.Errorf("render: produce GIF frame %d: %w", index+1, err)
		}
		if img == nil || img.Bounds().Empty() {
			return nil, fmt.Errorf("render: empty GIF frame %d", index+1)
		}
		w, h := img.Bounds().Dx(), img.Bounds().Dy()
		if w > gifMaxDimension || h > gifMaxDimension || int64(w) > opts.MaxFramePixels/int64(h) {
			return nil, fmt.Errorf("render: GIF frame exceeds pixel budget")
		}
		if 64<<20+int64(w)*int64(h)*16+int64(w+2)*24+1<<20 > opts.MaxWorkspaceBytes {
			return nil, fmt.Errorf("render: GIF workspace exceeds budget")
		}
		if width == 0 {
			width, height = w, h
		} else if width != w || height != h {
			return nil, ErrGIFFrameSize
		}
		return img, ctx.Err()
	}
	n := min(total, opts.SampleFrames, opts.SamplePixels)
	samples := image.NewRGBA(image.Rect(0, 0, opts.SamplePixels, 1))
	used := 0
	for k := 0; k < n; k++ {
		index := 0
		if n > 1 {
			index = int(int64(k) * int64(total-1) / int64(n-1))
		}
		img, err := frame(index)
		if err != nil {
			return err
		}
		b := img.Bounds()
		pixels := int64(b.Dx()) * int64(b.Dy())
		quota := min(int64(opts.SamplePixels/n), pixels)
		for j := int64(0); j < quota; j++ {
			if j%4096 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			pos := j * pixels / quota
			r, g, bl, _ := img.At(b.Min.X+int(pos%int64(b.Dx())), b.Min.Y+int(pos/int64(b.Dx()))).RGBA()
			i := used * 4
			samples.Pix[i], samples.Pix[i+1], samples.Pix[i+2], samples.Pix[i+3] = byte(r>>8), byte(g>>8), byte(bl>>8), 255
			used++
		}
	}
	samples.Rect.Max.X = used
	palette := medianCutRGBA([]*image.RGBA{samples}, colors)
	if len(palette) == 1 {
		palette = append(palette, palette[0])
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	writer, err := NewGIFWriter(gifContextWriter{ctx, out}, GIFStreamOptions{Palette: palette, DelayCs: opts.DelayCs, LoopCount: opts.LoopCount})
	if err != nil {
		return err
	}
	samples = nil
	for i := 0; i < total; i++ {
		img, err := frame(i)
		if err != nil {
			return err
		}
		paletted, err := quantizeRGBAContext(ctx, toOpaqueRGBA(img), palette, !opts.NoDither)
		if err != nil {
			return err
		}
		if err := writer.WriteFrame(paletted); err != nil {
			return err
		}
		if progress != nil {
			progress(i+1, total)
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writer.Close()
}

type gifContextWriter struct {
	ctx context.Context
	out io.Writer
}

func (w gifContextWriter) Write(p []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.out.Write(p)
}
