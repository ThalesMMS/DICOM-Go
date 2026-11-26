package render

import (
	"bufio"
	"compress/lzw"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"io"
	"math/bits"
)

// Streaming GIF output (#1241). image/gif's EncodeAll needs every frame in
// memory before it writes a byte; GIFWriter writes the header with the first
// frame and each later frame as it arrives, so a cine of any length holds one
// frame at a time. It suits movies with one fixed palette known up front (a
// grayscale cine); EncodeAdaptiveGIFProducer samples a replayable source
// before streaming adaptive colour frames through the same writer.

var (
	// ErrGIFFrameSize reports a frame whose size differs from the first frame.
	ErrGIFFrameSize = errors.New("render: GIF frame size differs from the movie")
	// ErrGIFClosed reports use of a closed GIFWriter.
	ErrGIFClosed = errors.New("render: GIF writer is closed")
)

// gifMaxDimension is the GIF logical-screen limit (16-bit fields).
const gifMaxDimension = 1<<16 - 1

// GIFStreamOptions configures NewGIFWriter.
type GIFStreamOptions struct {
	// Palette is the global colour table shared by every frame (2..256
	// colours). Frames index into it.
	Palette color.Palette
	// DelayCs is the per-frame delay in hundredths of a second (minimum 1).
	DelayCs int
	// LoopCount follows image/gif: 0 loops forever, -1 plays once, n>0
	// repeats n extra times.
	LoopCount int
}

// GIFWriter streams paletted frames into an animated GIF89a.
type GIFWriter struct {
	w             *bufio.Writer
	opts          GIFStreamOptions
	litWidth      int
	width, height int
	frames        int
	err           error
	closed        bool
}

// NewGIFWriter validates options and returns a writer. Nothing is written
// until the first frame.
func NewGIFWriter(w io.Writer, opts GIFStreamOptions) (*GIFWriter, error) {
	if w == nil {
		return nil, fmt.Errorf("render: nil GIF destination")
	}
	if n := len(opts.Palette); n < 2 || n > MaxGIFColors {
		return nil, fmt.Errorf("render: GIF palette has %d colours, want 2..%d", n, MaxGIFColors)
	}
	if opts.DelayCs < 1 {
		opts.DelayCs = 1
	}
	if opts.DelayCs > 1<<16-1 {
		opts.DelayCs = 1<<16 - 1
	}
	if opts.LoopCount < -1 || opts.LoopCount > 1<<16-1 {
		return nil, fmt.Errorf("render: GIF loop count %d out of range", opts.LoopCount)
	}
	// The colour table holds 2^litWidth entries; LZW needs litWidth >= 2.
	litWidth := max(bits.Len(uint(len(opts.Palette)-1)), 2)
	return &GIFWriter{w: bufio.NewWriter(w), opts: opts, litWidth: litWidth}, nil
}

// Frames reports how many frames have been written.
func (g *GIFWriter) Frames() int { return g.frames }

// WriteFrame appends img with the writer's DelayCs. The first frame fixes the
// movie dimensions; later frames must match (ErrGIFFrameSize). Every pixel
// must index the writer's palette.
func (g *GIFWriter) WriteFrame(img *image.Paletted) error {
	return g.WriteFrameDelay(img, g.opts.DelayCs)
}

// WriteFrameDelay is WriteFrame with this frame's delay in hundredths of a
// second (clamped to 1..65535), for movies with an irregular cadence.
func (g *GIFWriter) WriteFrameDelay(img *image.Paletted, delayCs int) error {
	if g.closed {
		return ErrGIFClosed
	}
	if g.err != nil {
		return g.err
	}
	if img == nil || img.Bounds().Empty() {
		return fmt.Errorf("render: empty GIF frame %d", g.frames+1)
	}
	b := img.Bounds()
	if g.frames == 0 {
		if b.Dx() > gifMaxDimension || b.Dy() > gifMaxDimension {
			return fmt.Errorf("render: GIF frame %dx%d exceeds %d pixels per side", b.Dx(), b.Dy(), gifMaxDimension)
		}
		g.width, g.height = b.Dx(), b.Dy()
	} else if b.Dx() != g.width || b.Dy() != g.height {
		return fmt.Errorf("%w: frame %d is %dx%d, movie is %dx%d", ErrGIFFrameSize, g.frames+1, b.Dx(), b.Dy(), g.width, g.height)
	}
	limit := uint8(len(g.opts.Palette) - 1)
	for y := 0; y < g.height; y++ {
		row := img.Pix[y*img.Stride : y*img.Stride+g.width]
		for _, index := range row {
			if index > limit {
				return fmt.Errorf("render: GIF frame %d uses colour index %d outside the %d-colour palette", g.frames+1, index, len(g.opts.Palette))
			}
		}
	}
	if g.frames == 0 {
		if err := g.writeHeader(); err != nil {
			return g.fail(err)
		}
	}
	if err := g.writeImage(img, min(max(delayCs, 1), 1<<16-1)); err != nil {
		return g.fail(err)
	}
	g.frames++
	return nil
}

// Close writes the GIF trailer and flushes. A writer with no frames returns an
// error and writes nothing.
func (g *GIFWriter) Close() error {
	if g.closed {
		return ErrGIFClosed
	}
	g.closed = true
	if g.err != nil {
		return g.err
	}
	if g.frames == 0 {
		return fmt.Errorf("render: no frames to encode")
	}
	if err := g.w.WriteByte(0x3b); err != nil {
		return err
	}
	return g.w.Flush()
}

func (g *GIFWriter) fail(err error) error {
	g.err = err
	return err
}

func (g *GIFWriter) writeHeader() error {
	le := binary.LittleEndian
	b := []byte("GIF89a")
	b = le.AppendUint16(b, uint16(g.width))
	b = le.AppendUint16(b, uint16(g.height))
	// Global colour table present, 8-bit colour resolution, table size.
	b = append(b, 0x80|0x70|byte(g.litWidth-1), 0, 0)
	for i := 0; i < 1<<g.litWidth; i++ {
		var r, gr, bl uint32
		if i < len(g.opts.Palette) {
			r, gr, bl, _ = g.opts.Palette[i].RGBA()
		}
		b = append(b, byte(r>>8), byte(gr>>8), byte(bl>>8))
	}
	if g.opts.LoopCount >= 0 {
		b = append(b, 0x21, 0xff, 0x0b)
		b = append(b, "NETSCAPE2.0"...)
		b = append(b, 0x03, 0x01)
		b = le.AppendUint16(b, uint16(g.opts.LoopCount))
		b = append(b, 0x00)
	}
	_, err := g.w.Write(b)
	return err
}

func (g *GIFWriter) writeImage(img *image.Paletted, delayCs int) error {
	le := binary.LittleEndian
	// Graphic control extension: no disposal, no transparency, the delay.
	b := []byte{0x21, 0xf9, 0x04, 0x00}
	b = le.AppendUint16(b, uint16(delayCs))
	b = append(b, 0x00, 0x00)
	// Image descriptor at (0, 0) without a local colour table.
	b = append(b, 0x2c, 0, 0, 0, 0)
	b = le.AppendUint16(b, uint16(g.width))
	b = le.AppendUint16(b, uint16(g.height))
	b = append(b, 0x00, byte(g.litWidth))
	if _, err := g.w.Write(b); err != nil {
		return err
	}
	blocks := &gifBlockWriter{w: g.w}
	lzww := lzw.NewWriter(blocks, lzw.LSB, g.litWidth)
	for y := 0; y < g.height; y++ {
		if _, err := lzww.Write(img.Pix[y*img.Stride : y*img.Stride+g.width]); err != nil {
			_ = lzww.Close()
			return err
		}
	}
	if err := lzww.Close(); err != nil {
		return err
	}
	return blocks.close()
}

// gifBlockWriter splits LZW output into the GIF's 255-byte data sub-blocks.
type gifBlockWriter struct {
	w   *bufio.Writer
	buf [255]byte
	n   int
}

func (b *gifBlockWriter) Write(p []byte) (int, error) {
	written := 0
	for len(p) > 0 {
		c := copy(b.buf[b.n:], p)
		b.n += c
		p = p[c:]
		written += c
		if b.n == len(b.buf) {
			if err := b.flush(); err != nil {
				return written, err
			}
		}
	}
	return written, nil
}

func (b *gifBlockWriter) flush() error {
	if b.n == 0 {
		return nil
	}
	if err := b.w.WriteByte(byte(b.n)); err != nil {
		return err
	}
	_, err := b.w.Write(b.buf[:b.n])
	b.n = 0
	return err
}

// close flushes the last sub-block and writes the block terminator.
func (b *gifBlockWriter) close() error {
	if err := b.flush(); err != nil {
		return err
	}
	return b.w.WriteByte(0x00)
}
