package render

import (
	"fmt"
	"math"
)

type ViewportFit uint8

const (
	ViewportFitContain ViewportFit = iota
	ViewportFitCover
)

// ViewportResliceRequest maps an existing source-plane grid to a visible
// viewport. Pan is in source-grid pixels after rotation and zoom, matching
// image display transforms. ViewportWidth/Height are in any consistent units;
// output dimensions independently choose the requested sampling density.
// A zero Zoom means one. The default fit is Contain.
type ViewportResliceRequest struct {
	SourceWidth, SourceHeight     int
	OutputWidth, OutputHeight     int
	ViewportWidth, ViewportHeight float64
	Zoom, PanX, PanY, Rotation    float64
	Fit                           ViewportFit
}

// ViewportReslicePlan retains the source grid for annotations while Plane
// describes only the pixels sampled for the displayed frame. Plane spans
// output pixel centres, not the outer edges of their footprints. Increasing
// output density preserves the covered field of view, including half pixels.
type ViewportReslicePlan struct {
	Plane         Plane
	Width, Height int
	request       ViewportResliceRequest
	scale         float64
}

// PlanViewportReslice performs geometry only, never volume sampling or image
// transforms. It preserves exact output dimensions and rejects unsafe requests
// instead of reducing settled quality. Contain does not paint letterboxing:
// the caller may pass the image's actual draw rectangle as its viewport.
func PlanViewportReslice(source Plane, request ViewportResliceRequest) (ViewportReslicePlan, error) {
	invalid := func(field string) (ViewportReslicePlan, error) {
		return ViewportReslicePlan{}, fmt.Errorf("%w: viewport %s", ErrInvalidViewState, field)
	}
	if !validMPRPlane(source) {
		return invalid("source plane")
	}
	if request.SourceWidth < 2 || request.SourceHeight < 2 || request.OutputWidth < 2 || request.OutputHeight < 2 {
		return invalid("dimensions require at least two pixel centres")
	}
	if uint64(request.OutputWidth) > uint64(MaxViewOutputDimension) || uint64(request.OutputHeight) > uint64(MaxViewOutputDimension) || uint64(request.OutputWidth) > MaxViewOutputBytes/4/uint64(request.OutputHeight) {
		return invalid("output limits")
	}
	finite := func(x float64) bool { return !math.IsNaN(x) && !math.IsInf(x, 0) }
	if !finite(request.ViewportWidth) || !finite(request.ViewportHeight) || request.ViewportWidth <= 0 || request.ViewportHeight <= 0 {
		return invalid("viewport size")
	}
	if request.Zoom == 0 {
		request.Zoom = 1
	}
	if !finite(request.Zoom) || request.Zoom <= 0 || !finite(request.PanX) || !finite(request.PanY) || !finite(request.Rotation) {
		return invalid("transform")
	}
	sx, sy := request.ViewportWidth/float64(request.SourceWidth), request.ViewportHeight/float64(request.SourceHeight)
	scale := math.Min(sx, sy)
	switch request.Fit {
	case ViewportFitContain:
	case ViewportFitCover:
		scale = math.Max(sx, sy)
	default:
		return invalid("fit")
	}
	if !finite(scale) || scale <= 0 || !finite(scale*request.Zoom) || scale*request.Zoom <= 0 {
		return invalid("transform scale")
	}
	plan := ViewportReslicePlan{Width: request.OutputWidth, Height: request.OutputHeight, request: request, scale: scale}
	x0, y0, ok := plan.OutputToSource(0, 0)
	if !ok {
		return invalid("origin")
	}
	x1, y1, ok := plan.OutputToSource(float64(request.OutputWidth-1), 0)
	if !ok {
		return invalid("horizontal edge")
	}
	x2, y2, ok := plan.OutputToSource(0, float64(request.OutputHeight-1))
	if !ok {
		return invalid("vertical edge")
	}
	point := func(x, y float64) Vec3 {
		return source.At(x/float64(request.SourceWidth-1), y/float64(request.SourceHeight-1))
	}
	origin := point(x0, y0)
	plan.Plane = Plane{Origin: origin, U: point(x1, y1).Sub(origin), V: point(x2, y2).Sub(origin)}
	if !validMPRPlane(plan.Plane) {
		return invalid("result plane")
	}
	return plan, nil
}

// OutputToSource maps fractional output pixel centres to source-grid centres.
// Points may be outside the acquired grid; GPU outside-value policy decides
// their presentation. The method neither clamps nor changes ROI coordinates.
func (p ViewportReslicePlan) OutputToSource(x, y float64) (float64, float64, bool) {
	if p.scale <= 0 || p.Width < 2 || p.Height < 2 || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, false
	}
	r := p.request
	dx := ((x+.5)*r.ViewportWidth/float64(p.Width) - r.ViewportWidth/2 - r.PanX*p.scale) / (r.Zoom * p.scale)
	dy := ((y+.5)*r.ViewportHeight/float64(p.Height) - r.ViewportHeight/2 - r.PanY*p.scale) / (r.Zoom * p.scale)
	c, s := math.Cos(r.Rotation), math.Sin(r.Rotation)
	sx := float64(r.SourceWidth-1)/2 + dx*c + dy*s
	sy := float64(r.SourceHeight-1)/2 - dx*s + dy*c
	return sx, sy, !math.IsNaN(sx) && !math.IsNaN(sy) && !math.IsInf(sx, 0) && !math.IsInf(sy, 0)
}

func (p ViewportReslicePlan) SourceToOutput(x, y float64) (float64, float64, bool) {
	if p.scale <= 0 || p.Width < 2 || p.Height < 2 || math.IsNaN(x) || math.IsNaN(y) || math.IsInf(x, 0) || math.IsInf(y, 0) {
		return 0, 0, false
	}
	r := p.request
	dx, dy := x-float64(r.SourceWidth-1)/2, y-float64(r.SourceHeight-1)/2
	c, s := math.Cos(r.Rotation), math.Sin(r.Rotation)
	vx := r.ViewportWidth/2 + r.PanX*p.scale + r.Zoom*p.scale*(dx*c-dy*s)
	vy := r.ViewportHeight/2 + r.PanY*p.scale + r.Zoom*p.scale*(dx*s+dy*c)
	ox := vx*float64(p.Width)/r.ViewportWidth - .5
	oy := vy*float64(p.Height)/r.ViewportHeight - .5
	return ox, oy, !math.IsNaN(ox) && !math.IsNaN(oy) && !math.IsInf(ox, 0) && !math.IsInf(oy, 0)
}
