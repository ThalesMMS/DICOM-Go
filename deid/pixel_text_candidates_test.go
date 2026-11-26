package deid

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/draw"
	"testing"
	"time"
)

func TestPixelTextCandidatesReturnsOnlyGeometry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	bounds := image.Rect(10, 20, 106, 84)
	for _, test := range []struct {
		name       string
		frame      draw.Image
		pixels     []byte
		background color.Color
		ink        color.Color
		row        int
	}{
		{name: "monochrome", frame: image.NewGray(bounds), background: color.Gray{Y: 20}, ink: color.Gray{Y: 235}, row: 4},
		{name: "color", frame: image.NewNRGBA(bounds), background: color.NRGBA{R: 210, G: 225, B: 230, A: 255},
			ink: color.NRGBA{R: 16, G: 18, B: 22, A: 255}, row: 54},
	} {
		t.Run(test.name, func(t *testing.T) {
			draw.Draw(test.frame, bounds, image.NewUniform(test.background), image.Point{}, draw.Src)
			glyphs := []string{"101", "101", "111", "101", "101", "101"}
			for glyph := range 3 {
				for y, row := range glyphs {
					for x, pixel := range row {
						if pixel == '1' {
							test.frame.Set(bounds.Min.X+6+glyph*6+x, bounds.Min.Y+test.row+y, test.ink)
						}
					}
				}
			}
			// An isolated bright object in the same band and a second string
			// in the center are outside the proposal profile.
			draw.Draw(test.frame, image.Rect(59, test.row, 63, test.row+6).Add(bounds.Min), image.NewUniform(test.ink), image.Point{}, draw.Src)
			for glyph := range 3 {
				start := 6 + glyph*6
				draw.Draw(test.frame, image.Rect(start, 28, start+3, 34).Add(bounds.Min), image.NewUniform(test.ink), image.Point{}, draw.Src)
			}
			switch frame := test.frame.(type) {
			case *image.Gray:
				test.pixels = frame.Pix
			case *image.NRGBA:
				test.pixels = frame.Pix
			}
			original := bytes.Clone(test.pixels)
			regions, err := PixelTextCandidates(ctx, test.frame)
			if err != nil || len(regions) != 1 {
				t.Fatalf("proposed %v, error %v", regions, err)
			}
			if box := regions[0]; !image.Rect(6, test.row, 21, test.row+6).In(box) || image.Pt(59, test.row).In(box) {
				t.Fatalf("incorrect stored-pixel bounds %v", box)
			}
			if !bytes.Equal(test.pixels, original) {
				t.Fatal("candidate scan modified the original image")
			}
		})
	}
}

func TestPixelTextCandidatesIncompleteResults(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame := image.NewGray(image.Rect(0, 0, 80, 64))
	if regions, err := PixelTextCandidates(ctx, frame); err != nil || len(regions) != 0 {
		t.Fatalf("blank image regions=%v err=%v", regions, err)
	}
	cancelledCtx, cancelScan := context.WithCancel(ctx)
	cancelScan()
	if regions, err := PixelTextCandidates(cancelledCtx, frame); regions != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled scan regions=%v err=%v", regions, err)
	}
	if regions, err := PixelTextCandidates(ctx, image.NewGray(image.Rect(0, 0, 24, 31))); regions != nil || !errors.Is(err, ErrPixelTextReviewInconclusive) {
		t.Fatalf("small-frame scan regions=%v err=%v", regions, err)
	}
}

func TestPixelTextCandidatesCanIncludeNonTextShapes(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	frame := image.NewGray(image.Rect(0, 0, 96, 64))
	original := bytes.Clone(frame.Pix)
	for column := range 3 {
		draw.Draw(frame, image.Rect(6+column*7, 4, 10+column*7, 10), image.NewUniform(color.White), image.Point{}, draw.Src)
	}
	original = append(original[:0], frame.Pix...)
	regions, err := PixelTextCandidates(ctx, frame)
	if err != nil || len(regions) != 1 {
		t.Fatalf("nontext shape control regions=%v err=%v", regions, err)
	}
	if !bytes.Equal(original, frame.Pix) {
		t.Fatal("proposing a false positive changed its pixels")
	}
}
