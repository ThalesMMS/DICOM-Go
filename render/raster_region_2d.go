package render

import (
	"context"
	"errors"
	"image"
	"image/color"
	"math"
	"runtime"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

var ErrRasterRegion2D = errors.New("render: invalid or incomplete regional 2D raster")

// RasterRegion2D describes a bounded decoded/windowed image on the source's
// reduced sample lattice. Grid uses ReducedRegion coordinates, independent of
// Image.Bounds().Min. FullSize always describes the original source pixels.
type RasterRegion2D struct {
	Image          image.Image
	Grid           image.Rectangle
	FullSize       image.Point
	ReductionLevel int
}

type RasterSampling2D uint8

const (
	RasterNearest2D RasterSampling2D = iota
	RasterBilinear2D
)

// RasterViewport2DRequest maps original source pixel EDGES into output pixel
// edges: x'=A*x+B*y+TX, y'=C*x+D*y+TY (Affine order A,B,C,D,TX,TY).
// SourceColor optionally performs source-coordinate masking on each sampled
// texel. It must be immutable for the call; processing does not touch 3D data.
type RasterViewport2DRequest struct {
	Width, Height int
	Affine        [6]float64
	Sampling      RasterSampling2D
	Background    color.NRGBA
	MaxBytes      uint64
	SourceColor   func(x, y int, sampled color.NRGBA) color.NRGBA
}

// RasterViewport2D allocates only the viewport raster, never a virtual full-size
// source image or texture. Missing requested source samples are an explicit
// error rather than black pixels that conceal an incomplete regional decode.
func RasterViewport2D(ctx context.Context, source RasterRegion2D, request RasterViewport2DRequest) (*image.NRGBA, error) {
	out, _, err := rasterViewport2D(ctx, source, request)
	return out, err
}

// RasterViewport2DPresentation renders like RasterViewport2D and returns the
// same bytes as an *image.RGBA when every output pixel is opaque: opaque
// premultiplied and non-premultiplied pixels are encoded identically, so the
// RGBA image aliases the rendered pixels without a copy. Presenters that
// upload *image.RGBA directly (Fyne's GL painter) then skip a conversion
// pass. Output with any non-opaque pixel is returned as the *image.NRGBA.
func RasterViewport2DPresentation(ctx context.Context, source RasterRegion2D, request RasterViewport2DRequest) (image.Image, error) {
	out, opaque, err := rasterViewport2D(ctx, source, request)
	if err != nil {
		return nil, err
	}
	if opaque {
		return &image.RGBA{Pix: out.Pix, Stride: out.Stride, Rect: out.Rect}, nil
	}
	return out, nil
}

// rasterViewport2D also reports whether every output pixel is opaque.
func rasterViewport2D(ctx context.Context, source RasterRegion2D, request RasterViewport2DRequest) (*image.NRGBA, bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	if source.FullSize.X < 1 || source.FullSize.Y < 1 || source.FullSize.X > 65535 || source.FullSize.Y > 65535 || source.ReductionLevel < 0 || source.ReductionLevel > pixeldata.MaxReductionLevel || request.Width < 1 || request.Height < 1 || request.Width > 16384 || request.Height > 16384 || request.Sampling > RasterBilinear2D {
		return nil, false, ErrRasterRegion2D
	}
	full := image.Rect(0, 0, source.FullSize.X, source.FullSize.Y)
	lattice := pixeldata.ReducedRegion(full, source.ReductionLevel)
	if !source.Grid.Empty() {
		if source.Image == nil || source.Image.Bounds().Size() != source.Grid.Size() || !source.Grid.In(lattice) {
			return nil, false, ErrRasterRegion2D
		}
	} else if source.Image != nil {
		return nil, false, ErrRasterRegion2D
	}
	budget := request.MaxBytes
	if budget == 0 {
		budget = 64 << 20
	}
	pixels := uint64(request.Width) * uint64(request.Height)
	if pixels > budget/4 {
		return nil, false, ErrRasterRegion2D
	}
	for _, v := range request.Affine {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, false, ErrRasterRegion2D
		}
	}
	a, b, c, d, tx, ty := request.Affine[0], request.Affine[1], request.Affine[2], request.Affine[3], request.Affine[4], request.Affine[5]
	determinant := a*d - b*c
	if determinant == 0 || math.IsNaN(determinant) || math.IsInf(determinant, 0) {
		return nil, false, ErrRasterRegion2D
	}
	step := float64(uint64(1) << source.ReductionLevel)
	fetch := func(x, y int) (color.NRGBA, bool) {
		x = max(lattice.Min.X, min(lattice.Max.X-1, x))
		y = max(lattice.Min.Y, min(lattice.Max.Y-1, y))
		if source.Image == nil || !image.Pt(x, y).In(source.Grid) {
			return color.NRGBA{}, false
		}
		bounds := source.Image.Bounds()
		value := rasterNRGBASample(source.Image, bounds.Min.X+x-source.Grid.Min.X, bounds.Min.Y+y-source.Grid.Min.Y)
		if request.SourceColor != nil {
			sx := min(source.FullSize.X-1, int((float64(x)+.5)*step))
			sy := min(source.FullSize.Y-1, int((float64(y)+.5)*step))
			value = request.SourceColor(sx, sy, value)
		}
		return value, true
	}
	out := image.NewNRGBA(image.Rect(0, 0, request.Width, request.Height))
	// The general sampler clears opaque when it writes a non-opaque pixel.
	var opaque atomic.Bool
	opaque.Store(true)
	renderRows := func(first, last int) error {
		translucent := false
		for y := first; y < last; y++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for x := 0; x < request.Width; x++ {
				px, py := float64(x)+.5-tx, float64(y)+.5-ty
				sx, sy := (d*px-b*py)/determinant, (-c*px+a*py)/determinant
				if math.IsNaN(sx) || math.IsNaN(sy) || math.IsInf(sx, 0) || math.IsInf(sy, 0) {
					return ErrRasterRegion2D
				}
				value := request.Background
				if sx >= 0 && sy >= 0 && sx < float64(source.FullSize.X) && sy < float64(source.FullSize.Y) {
					if request.Sampling == RasterNearest2D {
						var ok bool
						value, ok = fetch(int(math.Floor(sx/step)), int(math.Floor(sy/step)))
						if !ok {
							return ErrRasterRegion2D
						}
					} else {
						u, v := sx/step-.5, sy/step-.5
						ix, iy := int(math.Floor(u)), int(math.Floor(v))
						fx, fy := u-float64(ix), v-float64(iy)
						weights := [4]float64{(1 - fx) * (1 - fy), fx * (1 - fy), (1 - fx) * fy, fx * fy}
						points := [4]image.Point{image.Pt(ix, iy), image.Pt(ix+1, iy), image.Pt(ix, iy+1), image.Pt(ix+1, iy+1)}
						var alpha, red, green, blue float64
						for i, p := range points {
							if weights[i] == 0 {
								continue
							}
							sample, ok := fetch(p.X, p.Y)
							if !ok {
								return ErrRasterRegion2D
							}
							weighted := weights[i] * float64(sample.A)
							alpha += weighted
							red += weighted * float64(sample.R)
							green += weighted * float64(sample.G)
							blue += weighted * float64(sample.B)
						}
						value = color.NRGBA{}
						if alpha > 0 {
							value = color.NRGBA{R: uint8(math.Round(red / alpha)), G: uint8(math.Round(green / alpha)), B: uint8(math.Round(blue / alpha)), A: uint8(math.Round(alpha))}
						}
					}
				}
				translucent = translucent || value.A != 255
				out.SetNRGBA(x, y, value)
			}
		}
		if translucent {
			opaque.Store(false)
		}
		return nil
	}
	// Axial grayscale views repeat the same horizontal coordinates on every
	// row. Hoist the transform, clamp and coverage checks while retaining the
	// general sampler's floating-point accumulation and rounding order.
	if gray, ok := source.Image.(*image.Gray); ok && request.SourceColor == nil && b == 0 && c == 0 {
		var grayOpaque bool
		var err error
		renderRows, grayOpaque, err = rasterGrayAxisRows(ctx, gray, source, request, out, determinant, step)
		if err != nil {
			return nil, false, err
		}
		opaque.Store(grayOpaque)
	}
	// Standard immutable rasters permit independent row reads. Caller-defined
	// images and masking callbacks retain serial execution and callback order.
	workers := 1
	if pixels >= 256*256 && request.SourceColor == nil {
		switch source.Image.(type) {
		case *image.Gray, *image.Gray16, *image.NRGBA, *image.RGBA:
			workers = min(8, runtime.GOMAXPROCS(0), request.Height)
		}
	}
	if workers == 1 {
		if err := renderRows(0, request.Height); err != nil {
			return nil, false, err
		}
	} else {
		results := make(chan error, workers-1)
		for worker := 1; worker < workers; worker++ {
			first, last := request.Height*worker/workers, request.Height*(worker+1)/workers
			go func() { results <- renderRows(first, last) }()
		}
		// The calling renderer already owns a runnable worker. Keep one row
		// range here instead of parking it and scheduling another goroutine.
		failure := renderRows(0, request.Height/workers)
		for worker := 1; worker < workers; worker++ {
			if err := <-results; err != nil {
				failure = err
			}
		}
		if failure != nil {
			return nil, false, failure
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, false, err
	}
	return out, opaque.Load(), nil
}

// rasterNRGBASample avoids boxing Gray/RGBA colors for every interpolation tap.
// Uncommon color models and non-opaque premultiplied RGBA retain Go's canonical
// color conversion, including its unpremultiplication and rounding semantics.
func rasterNRGBASample(src image.Image, x, y int) color.NRGBA {
	switch image := src.(type) {
	case *image.NRGBA:
		return image.NRGBAAt(x, y)
	case *image.Gray:
		value := image.GrayAt(x, y).Y
		return color.NRGBA{R: value, G: value, B: value, A: 255}
	case *image.Gray16:
		value := uint8(image.Gray16At(x, y).Y >> 8)
		return color.NRGBA{R: value, G: value, B: value, A: 255}
	case *image.RGBA:
		value := image.RGBAAt(x, y)
		if value.A == 255 {
			return color.NRGBA{R: value.R, G: value.G, B: value.B, A: 255}
		}
		return color.NRGBAModel.Convert(value).(color.NRGBA)
	default:
		return color.NRGBAModel.Convert(src.At(x, y)).(color.NRGBA)
	}
}
