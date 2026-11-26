package render

import (
	"context"
	"errors"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ErrGray2DProfile is a complete-render fallback boundary, never partial output.
var ErrGray2DProfile = errors.New("render: unsupported resident grayscale 2D profile")

// Gray2DSource owns a detached display-only index plane. Original stored samples
// remain authoritative for probe, ROI and export. Levels exactly retain the
// canonical render cache's float32 modality values, including rescale rounding.
type Gray2DSource struct {
	Width, Height int
	Indices       []uint32
	Levels        []float32
	Invert        bool
	DefaultWindow WindowLevel
}

// PrepareGray2D accepts native integer MONOCHROME1/2 frames only. The caller
// provides an explicit allocation cap; neither color nor precision maps enter it.
func PrepareGray2D(ctx context.Context, f *Frame, maxBytes uint64) (*Gray2DSource, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := CheckGray2DProfile(f, maxBytes); err != nil {
		return nil, err
	}
	n := uint64(f.Metadata.Rows) * uint64(f.Metadata.Columns)
	photo := normalizedPhotometric(f.Metadata.PhotometricInterpretation)
	loaded, err := loadFrameContext(ctx, f)
	if err != nil {
		return nil, err
	}
	d, err := decodeSliceFrame(loaded)
	if err != nil {
		return nil, err
	}
	if len(d.grayscale) != int(n) {
		return nil, ErrGray2DProfile
	}
	s := &Gray2DSource{Width: d.cols, Height: d.rows, Indices: make([]uint32, int(n)), Invert: photo == "MONOCHROME1", DefaultWindow: f.DefaultWindow}
	ids := make(map[uint32]uint32)
	for i, v := range d.grayscale {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		bits := math.Float32bits(v)
		id, ok := ids[bits]
		if !ok {
			if len(s.Levels) == 65536 {
				return nil, ErrGray2DProfile
			}
			id = uint32(len(s.Levels))
			ids[bits] = id
			s.Levels = append(s.Levels, v)
		}
		s.Indices[i] = id
	}
	return s, nil
}

// GrayTable uses the existing DICOM VOI mapper once per distinct modality
// value. The shader gathers this table BEFORE interpolation, exactly as the
// canonical window-image -> RasterViewport2D pipeline does.
func (s *Gray2DSource) GrayTable(ctx context.Context, w WindowLevel) ([]uint32, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	w = normalizeWindow(w, s.DefaultWindow)
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: w.Center, Width: w.Width, Function: w.Function, LUT: w.LUT})
	out := make([]uint32, len(s.Levels))
	for i, v := range s.Levels {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		g := mapper.Byte(float64(v))
		if s.Invert {
			g = 255 - g
		}
		out[i] = uint32(g)
	}
	return out, nil
}

// Gray2DAxis encodes the canonical float64 geometry decision, not GPU-rounded
// source coordinates. Thus even points arbitrarily near an image edge select
// the same pixels. Only bilinear arithmetic/byte rounding uses GPU float32.
type Gray2DAxis struct{ First, Second, Weight, Inside uint32 }
type Gray2DPlan struct {
	Width, Height, SourceWidth, SourceHeight int
	X, Y                                     []Gray2DAxis
}

func PlanGray2DViewport(size image.Point, r RasterViewport2DRequest) (Gray2DPlan, error) {
	p := Gray2DPlan{Width: r.Width, Height: r.Height, SourceWidth: size.X, SourceHeight: size.Y}
	a := r.Affine
	if size.X < 1 || size.Y < 1 || size.X > 65535 || size.Y > 65535 || r.Width < 1 || r.Height < 1 || r.Width > 16384 || r.Height > 16384 || uint64(r.Width)*uint64(r.Height) > r.MaxBytes/4 || a[1] != 0 || a[2] != 0 || a[0] == 0 || a[3] == 0 || r.Sampling > RasterBilinear2D || r.SourceColor != nil {
		return Gray2DPlan{}, ErrGray2DProfile
	}
	for _, v := range a {
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return Gray2DPlan{}, ErrGray2DProfile
		}
	}
	axis := func(n, source int, scale, translation float64) []Gray2DAxis {
		out := make([]Gray2DAxis, n)
		for i := range out {
			q := (float64(i) + .5 - translation) / scale
			if q < 0 || q >= float64(source) {
				continue
			}
			entry := Gray2DAxis{Inside: 1}
			if r.Sampling == RasterNearest2D {
				entry.First = uint32(math.Floor(q))
				entry.Second = entry.First
			} else {
				u := q - .5
				b := int(math.Floor(u))
				entry.First = uint32(max(0, min(source-1, b)))
				entry.Second = uint32(max(0, min(source-1, b+1)))
				entry.Weight = math.Float32bits(float32(u - float64(b)))
			}
			out[i] = entry
		}
		return out
	}
	p.X = axis(r.Width, size.X, a[0], a[4])
	p.Y = axis(r.Height, size.Y, a[3], a[5])
	return p, nil
}

// Parameters is the bounded u32 ABI of Gray2DShader. Background uses RGBA8.
func (p Gray2DPlan) Parameters(background uint32) []uint32 {
	out := []uint32{uint32(p.Width), uint32(p.Height), uint32(p.SourceWidth), background}
	for _, axis := range [][]Gray2DAxis{p.X, p.Y} {
		for _, v := range axis {
			out = append(out, v.First, v.Second, v.Weight, v.Inside)
		}
	}
	return out
}

// Gray2DShader implements VOI-table lookup, edge-repeat sampling and viewport
// composition without reading stored pixels on the CPU during W/L/pan/zoom.
const Gray2DShader = `
@group(0) @binding(0) var<storage,read> samples:array<u32>;
@group(0) @binding(1) var<storage,read> gray:array<u32>;
@group(0) @binding(2) var<storage,read> p:array<u32>;
@group(0) @binding(3) var<storage,read_write> output:array<u32>;
fn value(x:u32,y:u32)->f32{return f32(gray[samples[y*p[2]+x]]);}
@compute @workgroup_size(8,8)
fn main(@builtin(global_invocation_id) id:vec3<u32>){
 if(id.x>=p[0] || id.y>=p[1]){return;}
 let xi=4u+id.x*4u;let yi=4u+p[0]*4u+id.y*4u;
 let outputIndex=id.y*p[0]+id.x;
 if(p[xi+3u]==0u || p[yi+3u]==0u){output[outputIndex]=p[3];return;}
 let x=bitcast<f32>(p[xi+2u]);let y=bitcast<f32>(p[yi+2u]);
 let a=value(p[xi],p[yi]);let b=value(p[xi+1u],p[yi]);
 let c=value(p[xi],p[yi+1u]);let d=value(p[xi+1u],p[yi+1u]);
 let v=u32(clamp(floor(mix(mix(a,b,x),mix(c,d,x),y)+0.5),0.0,255.0));
 output[outputIndex]=v|(v<<8u)|(v<<16u)|0xff000000u;
}`

// CheckGray2DProfile admits metadata before any lazy source IO or decode. All
// DICOM profile rules live here, shared by the application and preparation.
func CheckGray2DProfile(f *Frame, maxBytes uint64) error {
	if f == nil || f.Encapsulated || f.PreserveModalityPrecision || f.ColorTransform != nil || f.Metadata.SamplesPerPixel != 1 || (f.Metadata.BitsAllocated != 8 && f.Metadata.BitsAllocated != 16) {
		return ErrGray2DProfile
	}
	photo := normalizedPhotometric(f.Metadata.PhotometricInterpretation)
	if photo != "MONOCHROME1" && photo != "MONOCHROME2" {
		return ErrGray2DProfile
	}
	n := uint64(f.Metadata.Rows) * uint64(f.Metadata.Columns)
	if n == 0 || maxBytes == 0 || n > maxBytes/8 || n > 16<<20 {
		return ErrGray2DProfile
	}
	return nil
}
