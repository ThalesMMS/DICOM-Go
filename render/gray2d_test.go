package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"image"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

func TestGray2DVOITableMatchesCanonicalStoredProfiles(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, photo := range []string{"MONOCHROME1", "MONOCHROME2"} {
			raw := make([]byte, 128)
			for i := 0; i < 64; i++ {
				binary.LittleEndian.PutUint16(raw[i*2:], uint16(i*71))
			}
			original := append([]byte(nil), raw...)
			rep := uint16(0)
			if signed {
				rep = 1
			}
			f := &Frame{Metadata: pixeldata.Metadata{Rows: 8, Columns: 8, SamplesPerPixel: 1, BitsAllocated: 16, BitsStored: 12, HighBit: 11, PixelRepresentation: rep, PhotometricInterpretation: photo}, ByteOrder: binary.LittleEndian, PixelBytes: raw, Rescale: Rescale{Slope: .5, Intercept: -1024.25}, DefaultWindow: WindowLevel{Center: 40, Width: 400}}
			prepared, err := PrepareGray2D(context.Background(), f, 1<<20)
			if err != nil {
				t.Fatal(err)
			}
			lut, _ := display.NewLUT([]int{4, -1024, 8}, []uint16{0, 80, 160, 255})
			for _, w := range []WindowLevel{{Center: 0, Width: 1500}, {Center: -512, Width: 1}, {Center: -100, Width: 50, Function: display.VOISigmoid}, {Center: 40, Width: 400, Function: display.VOILinearExact}, {LUT: lut}} {
				table, err := prepared.GrayTable(context.Background(), w)
				if err != nil {
					t.Fatal(err)
				}
				reference, err := NewRenderCache(1<<20).RenderFrame(f, w)
				if err != nil {
					t.Fatal(err)
				}
				gray := reference.(*image.Gray)
				for i, id := range prepared.Indices {
					if uint8(table[id]) != gray.Pix[i] {
						t.Fatalf("%s signed=%v pixel%d GPU table=%d CPU=%d", photo, signed, i, table[id], gray.Pix[i])
					}
				}
			}
			if !bytes.Equal(original, raw) {
				t.Fatal("source pixels mutated")
			}
		}
	}
}

func TestGray2DViewportPlanPreservesCanonicalCoverageAndNearest(t *testing.T) {
	ctx := context.Background()
	source := image.NewGray(image.Rect(0, 0, 4, 3))
	for i := range source.Pix {
		source.Pix[i] = byte(i * 19)
	}
	for _, affine := range [][6]float64{{1, 0, 0, 1, 0, 0}, {2, 0, 0, 2, -.25, .75}, {-1.25, 0, 0, .75, 5, -.125}, {1, 0, 0, 1, 100, -100}} {
		r := RasterViewport2DRequest{Width: 7, Height: 5, Affine: affine, Sampling: RasterNearest2D, MaxBytes: 1 << 20}
		plan, err := PlanGray2DViewport(source.Bounds().Size(), r)
		if err != nil {
			t.Fatal(err)
		}
		reference, err := RasterViewport2D(ctx, RasterRegion2D{Image: source, Grid: source.Bounds(), FullSize: source.Bounds().Size()}, r)
		if err != nil {
			t.Fatal(err)
		}
		for y, ay := range plan.Y {
			for x, ax := range plan.X {
				want := byte(0)
				if ax.Inside != 0 && ay.Inside != 0 {
					want = source.GrayAt(int(ax.First), int(ay.First)).Y
				}
				if reference.NRGBAAt(x, y).R != want {
					t.Fatalf("geometry changed %v at%d,%d", affine, x, y)
				}
			}
		}
	}
	// The float64 edge decision stays authoritative even though shader arithmetic is f32.
	r := RasterViewport2DRequest{Width: 1, Height: 1, Affine: [6]float64{1, 0, 0, 1, math.Nextafter(.5, 1), 0}, MaxBytes: 4}
	p, err := PlanGray2DViewport(image.Pt(4, 3), r)
	if err != nil || p.X[0].Inside != 0 {
		t.Fatal("edge admission rounded on GPU", p, err)
	}
	r.Affine[1] = 1
	if _, err = PlanGray2DViewport(image.Pt(4, 3), r); !errors.Is(err, ErrGray2DProfile) {
		t.Fatal(err)
	}
}

func TestGray2DRefusesUnsupportedAndCancelledWithoutLoading(t *testing.T) {
	called := false
	f := &Frame{Metadata: pixeldata.Metadata{Rows: 1, Columns: 1, BitsAllocated: 8, SamplesPerPixel: 3}, LoadPixels: func(context.Context) (*Frame, error) { called = true; return nil, nil }}
	if _, err := PrepareGray2D(context.Background(), f, 1<<20); !errors.Is(err, ErrGray2DProfile) || called {
		t.Fatal(err, called)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := PrepareGray2D(ctx, f, 1<<20); !errors.Is(err, context.Canceled) || called {
		t.Fatal(err, called)
	}
}

// Float32 interpolation is deliberately a separate oracle from the canonical
// float64 raster. This fixes the declared GPU tolerance BEFORE live execution.
func TestGray2DBilinearPlanMatchesCanonicalWithinOneByte(t *testing.T) {
	src := image.NewGray(image.Rect(0, 0, 7, 5))
	for i := range src.Pix {
		src.Pix[i] = byte((i*73 + 19) % 256)
	}
	for _, a := range [][6]float64{{1, 0, 0, 1, 0, 0}, {1.37, 0, 0, 2.13, -.31, .61}, {-.83, 0, 0, 1.11, 5.77, -1.23}} {
		q := RasterViewport2DRequest{Width: 11, Height: 9, Affine: a, Sampling: RasterBilinear2D, MaxBytes: 1 << 20}
		plan, err := PlanGray2DViewport(src.Bounds().Size(), q)
		if err != nil {
			t.Fatal(err)
		}
		want, err := RasterViewport2D(context.Background(), RasterRegion2D{Image: src, Grid: src.Bounds(), FullSize: src.Bounds().Size()}, q)
		if err != nil {
			t.Fatal(err)
		}
		for y, ay := range plan.Y {
			for x, ax := range plan.X {
				got := 0
				if ax.Inside != 0 && ay.Inside != 0 {
					wx, wy := math.Float32frombits(ax.Weight), math.Float32frombits(ay.Weight)
					value := func(x, y uint32) float32 { return float32(src.GrayAt(int(x), int(y)).Y) }
					top := value(ax.First, ay.First)*(1-wx) + value(ax.Second, ay.First)*wx
					bottom := value(ax.First, ay.Second)*(1-wx) + value(ax.Second, ay.Second)*wx
					got = int(math.Floor(float64(top*(1-wy)+bottom*wy) + .5))
				}
				r, _, _, _ := want.At(x, y).RGBA()
				delta := got - int(r>>8)
				if delta < -1 || delta > 1 {
					t.Fatalf("float32 raster %d,%d got%d want%d", x, y, got, r>>8)
				}
			}
		}
	}
}
