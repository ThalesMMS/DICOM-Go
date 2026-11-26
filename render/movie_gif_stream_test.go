package render

import (
	"bytes"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"slices"
	"testing"
)

func grayStreamPalette() color.Palette {
	p := make(color.Palette, 256)
	for i := range p {
		p[i] = color.Gray{Y: uint8(i)}
	}
	return p
}

func streamTestFrame(width, height, seed int, palette color.Palette) *image.Paletted {
	img := image.NewPaletted(image.Rect(0, 0, width, height), palette)
	for i := range img.Pix {
		img.Pix[i] = uint8((i*7 + seed*31) % len(palette))
	}
	return img
}

func TestGIFWriterMatchesImageGIFDecode(t *testing.T) {
	for _, tc := range []struct {
		name      string
		palette   color.Palette
		loopCount int
	}{
		{"gray256-loop", grayStreamPalette(), 0},
		{"gray256-once", grayStreamPalette(), -1},
		{"three-colours", color.Palette{color.Black, color.White, color.Gray{Y: 128}}, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			w, err := NewGIFWriter(&buf, GIFStreamOptions{Palette: tc.palette, DelayCs: 8, LoopCount: tc.loopCount})
			if err != nil {
				t.Fatal(err)
			}
			// 300 bytes per frame forces several LZW sub-blocks.
			frames := []*image.Paletted{streamTestFrame(20, 15, 0, tc.palette), streamTestFrame(20, 15, 1, tc.palette), streamTestFrame(20, 15, 2, tc.palette)}
			for _, frame := range frames {
				if err := w.WriteFrame(frame); err != nil {
					t.Fatal(err)
				}
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			decoded, err := gif.DecodeAll(&buf)
			if err != nil {
				t.Fatalf("decode: %v", err)
			}
			if len(decoded.Image) != len(frames) || decoded.LoopCount != tc.loopCount {
				t.Fatalf("frames, loop = %d, %d; want %d, %d", len(decoded.Image), decoded.LoopCount, len(frames), tc.loopCount)
			}
			if decoded.Config.Width != 20 || decoded.Config.Height != 15 {
				t.Fatalf("screen = %dx%d, want 20x15", decoded.Config.Width, decoded.Config.Height)
			}
			for i, frame := range frames {
				if decoded.Delay[i] != 8 {
					t.Fatalf("frame %d delay = %d, want 8", i, decoded.Delay[i])
				}
				if !bytes.Equal(decoded.Image[i].Pix, frame.Pix) {
					t.Fatalf("frame %d pixels differ", i)
				}
				for j, c := range tc.palette {
					if decoded.Image[i].Palette[j] != color.RGBAModel.Convert(c) {
						t.Fatalf("frame %d palette[%d] = %v, want %v", i, j, decoded.Image[i].Palette[j], c)
					}
				}
			}
		})
	}
}

func TestGIFWriterRejectsInvalidInput(t *testing.T) {
	if _, err := NewGIFWriter(&bytes.Buffer{}, GIFStreamOptions{Palette: color.Palette{color.Black}}); err == nil {
		t.Fatal("one-colour palette accepted")
	}
	palette := color.Palette{color.Black, color.White}
	var buf bytes.Buffer
	w, err := NewGIFWriter(&buf, GIFStreamOptions{Palette: palette})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err == nil || buf.Len() != 0 {
		t.Fatalf("Close with no frames = %v and %d bytes, want an error and nothing written", err, buf.Len())
	}

	w, _ = NewGIFWriter(&buf, GIFStreamOptions{Palette: palette})
	if err := w.WriteFrame(streamTestFrame(4, 4, 0, palette)); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(streamTestFrame(5, 4, 0, palette)); !errors.Is(err, ErrGIFFrameSize) {
		t.Fatalf("mismatched frame = %v, want ErrGIFFrameSize", err)
	}
	outOfPalette := image.NewPaletted(image.Rect(0, 0, 4, 4), grayStreamPalette())
	outOfPalette.Pix[3] = 200
	if err := w.WriteFrame(outOfPalette); err == nil {
		t.Fatal("index outside the palette accepted")
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	if err := w.WriteFrame(streamTestFrame(4, 4, 0, palette)); !errors.Is(err, ErrGIFClosed) {
		t.Fatalf("write after close = %v, want ErrGIFClosed", err)
	}
}

func TestGIFWriterPerFrameDelay(t *testing.T) {
	palette := color.Palette{color.Black, color.White}
	var buf bytes.Buffer
	w, err := NewGIFWriter(&buf, GIFStreamOptions{Palette: palette, DelayCs: 5})
	if err != nil {
		t.Fatal(err)
	}
	for _, delay := range []int{4, 0, 70000} {
		if err := w.WriteFrameDelay(streamTestFrame(3, 3, delay, palette), delay); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.WriteFrame(streamTestFrame(3, 3, 0, palette)); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	decoded, err := gif.DecodeAll(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if want := []int{4, 1, 1<<16 - 1, 5}; !slices.Equal(decoded.Delay, want) {
		t.Fatalf("delays = %v, want %v", decoded.Delay, want)
	}
}
