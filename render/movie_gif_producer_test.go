package render

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/gif"
	"io"
	"reflect"
	"testing"
)

func producerRamp(ctx context.Context, index int) (image.Image, error) {
	img := image.NewRGBA(image.Rect(0, 0, 128, 48))
	for y := 0; y < 48; y++ {
		for x := 0; x < 128; x++ {
			img.SetRGBA(x, y, color.RGBA{uint8(2 * x), uint8(x/2 + y), uint8(255 - 2*x), 255})
		}
	}
	return img, ctx.Err()
}
func TestAdaptiveGIFProducerStablePaletteShadingAndCadence(t *testing.T) {
	var out bytes.Buffer
	var done []int
	err := EncodeAdaptiveGIFProducer(context.Background(), &out, 40, producerRamp, AdaptiveGIFStreamOptions{GIFOptions: GIFOptions{DelayCs: 7, LoopCount: -1}}, func(i, n int) {
		if n != 40 {
			t.Fatal(n)
		}
		done = append(done, i)
	})
	if err != nil {
		t.Fatal(err)
	}
	movie, err := gif.DecodeAll(bytes.NewReader(out.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if len(movie.Image) != 40 || movie.LoopCount != -1 || len(done) != 40 {
		t.Fatal("lost frames/cadence/progress")
	}
	for i, frame := range movie.Image {
		if movie.Delay[i] != 7 || !reflect.DeepEqual(frame.Palette, movie.Image[0].Palette) || !bytes.Equal(frame.Pix, movie.Image[0].Pix) {
			t.Fatal("artificial temporal flicker or delay change")
		}
	}
	source, _ := producerRamp(context.Background(), 0)
	var legacy bytes.Buffer
	if err := EncodeAdaptiveGIF(&legacy, []image.Image{source}, GIFOptions{DelayCs: 7, LoopCount: -1}); err != nil {
		t.Fatal(err)
	}
	oracle, err := gif.DecodeAll(&legacy)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(movie.Image[0].Palette, oracle.Image[0].Palette) || !bytes.Equal(movie.Image[0].Pix, oracle.Image[0].Pix) {
		t.Fatal("unchanged ramp shading differs from independent in-memory encoder")
	}
}

type cancelGIFOutput struct {
	cancel context.CancelFunc
	fail   bool
}

func (w cancelGIFOutput) Write(p []byte) (int, error) {
	w.cancel()
	if w.fail {
		return 0, io.ErrClosedPipe
	}
	return len(p), nil
}

type quantCancelContext struct {
	context.Context
	n int
}

func (c *quantCancelContext) Err() error {
	c.n++
	if c.n > 3 {
		return context.Canceled
	}
	return nil
}
func TestAdaptiveGIFProducerCancellationFailureAndLimits(t *testing.T) {
	for _, stage := range []string{"production", "writing", "finalizing", "disk"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var out io.Writer = io.Discard
			at := producerRamp
			if stage == "production" {
				at = func(ctx context.Context, i int) (image.Image, error) { cancel(); return nil, ctx.Err() }
			}
			if stage == "writing" || stage == "disk" {
				out = cancelGIFOutput{cancel: cancel, fail: stage == "disk"}
			}
			err := EncodeAdaptiveGIFProducer(ctx, out, 3, at, AdaptiveGIFStreamOptions{}, func(i, n int) {
				if stage == "finalizing" && i == n {
					cancel()
				}
			})
			if stage == "disk" {
				if !errors.Is(err, io.ErrClosedPipe) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
	img, _ := producerRamp(context.Background(), 0)
	if _, err := quantizeRGBAContext(&quantCancelContext{Context: context.Background()}, toOpaqueRGBA(img), grayPaletteForTest(), true); !errors.Is(err, context.Canceled) {
		t.Fatalf("quantization cancellation: %v", err)
	}
	for _, opts := range []AdaptiveGIFStreamOptions{{MaxFramePixels: 1}, {MaxWorkspaceBytes: 1}, {SampleFrames: 33}, {GIFOptions: GIFOptions{Palette: GIFPalettePerFrame}}} {
		if err := EncodeAdaptiveGIFProducer(context.Background(), io.Discard, 1, producerRamp, opts, nil); err == nil {
			t.Fatal("limit not enforced")
		}
	}
	calls := 0
	at := func(ctx context.Context, i int) (image.Image, error) {
		calls++
		if calls > 1 {
			return image.NewRGBA(image.Rect(0, 0, 4, 4)), nil
		}
		return producerRamp(ctx, i)
	}
	if err := EncodeAdaptiveGIFProducer(context.Background(), io.Discard, 2, at, AdaptiveGIFStreamOptions{}, nil); !errors.Is(err, ErrGIFFrameSize) {
		t.Fatal(err)
	}
}
func grayPaletteForTest() color.Palette {
	p := make(color.Palette, 256)
	for i := range p {
		p[i] = color.Gray{uint8(i)}
	}
	return p
}
