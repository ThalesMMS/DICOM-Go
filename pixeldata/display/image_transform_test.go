package display

import (
	"errors"
	"image"
	"image/color"
	"math"
	"reflect"
	"testing"
)

func TestTransformImageCoordinatesAndPrecision(t *testing.T) {
	src := image.NewGray16(image.Rect(10, 20, 13, 22))
	for i, v := range []uint16{1, 2, 3, 4, 5, 65534} {
		src.SetGray16(10+i%3, 20+i/3, color.Gray16{Y: v})
	}
	for _, tc := range []struct {
		name string
		opts ImageTransform
		w, h int
		want []uint16
	}{
		{"90", ImageTransform{RotationDegrees: 90}, 2, 3, []uint16{4, 1, 5, 2, 65534, 3}},
		{"180", ImageTransform{RotationDegrees: 180}, 3, 2, []uint16{65534, 5, 4, 3, 2, 1}},
		{"270", ImageTransform{RotationDegrees: 270}, 2, 3, []uint16{3, 65534, 2, 5, 1, 4}},
		{"horizontal", ImageTransform{FlipHorizontal: true}, 3, 2, []uint16{3, 2, 1, 65534, 5, 4}},
		{"vertical", ImageTransform{FlipVertical: true}, 3, 2, []uint16{4, 5, 65534, 1, 2, 3}},
		{"rotate-flip-scale", ImageTransform{RotationDegrees: 90, FlipHorizontal: true, Width: 4, Height: 3}, 4, 3, []uint16{1, 1, 4, 4, 2, 2, 5, 5, 3, 3, 65534, 65534}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := TransformImage(src, tc.opts)
			if err != nil {
				t.Fatal(err)
			}
			g, ok := got.(*image.Gray16)
			if !ok || g.Bounds().Dx() != tc.w || g.Bounds().Dy() != tc.h {
				t.Fatalf("type/bounds %T %v", got, got.Bounds())
			}
			values := []uint16{}
			for y := 0; y < tc.h; y++ {
				for x := 0; x < tc.w; x++ {
					values = append(values, g.Gray16At(x, y).Y)
				}
			}
			if !reflect.DeepEqual(values, tc.want) {
				t.Fatalf("pixels %v want %v", values, tc.want)
			}
		})
	}
	identity, err := TransformImage(src, ImageTransform{})
	if err != nil || identity != src {
		t.Fatal("identity did not preserve image")
	}
	if src.Gray16At(12, 21).Y != 65534 {
		t.Fatal("input mutated")
	}
}

func TestTransformImageColorAndBounds(t *testing.T) {
	colorImage := image.NewRGBA(image.Rect(0, 0, 2, 1))
	colorImage.SetRGBA(0, 0, color.RGBA{R: 10, G: 20, B: 30, A: 255})
	colorImage.SetRGBA(1, 0, color.RGBA{R: 40, G: 50, B: 60, A: 255})
	got, err := TransformImage(colorImage, ImageTransform{Scale: 2})
	if err != nil {
		t.Fatal(err)
	}
	if got.Bounds() != image.Rect(0, 0, 4, 2) || got.(*image.RGBA).RGBAAt(3, 1).R != 40 {
		t.Fatal("color scale")
	}
	for _, opts := range []ImageTransform{{RotationDegrees: 45}, {Scale: math.NaN()}, {Scale: 65}, {Scale: 1.0 / 65}, {Width: 1}, {Width: MaxExportPixels, Height: 2}, {Width: 1, Height: 1, Scale: 2}, {Scale: 1.0 / 64}} {
		if _, err := TransformImage(colorImage, opts); !errors.Is(err, ErrInvalidImageTransform) {
			t.Fatalf("%+v: %v", opts, err)
		}
	}
	if _, err := TransformImage(nil, ImageTransform{}); !errors.Is(err, ErrInvalidImageTransform) {
		t.Fatal(err)
	}
}
