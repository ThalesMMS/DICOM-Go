package render

import "context"

// CPRScalar is a CPR view sampled as modality values rather than a windowed
// image, for quantitative export.
type CPRScalar struct {
	Width, Height int
	// Values holds Width x Height values in row-major order (the slab
	// reduction when a slab is set); Valid marks the samples inside the
	// volume, and Values is zero where it is false.
	Values []float64
	Valid  []bool
	// Plane locates a transverse section (CPRTransverse): pixel (i, j) lies
	// at Plane.Origin + i/(Width-1)*Plane.U + j/(Height-1)*Plane.V.
	// Longitudinal views are curved and have none.
	Plane *Plane
	// ColumnSpacing and RowSpacing are the distances in mm between adjacent
	// columns (arc length for longitudinal views) and rows.
	ColumnSpacing, RowSpacing float64
}

// RenderCPRScalar samples the requested CPR view like RenderCPR, returning
// the modality values instead of mapping them through the window. It honors
// ctx cancellation between output rows.
func RenderCPRScalar(ctx context.Context, req CPRRequest) (*CPRScalar, error) {
	if req.Volume == nil || req.Path == nil {
		return nil, ErrCPRInput
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := validateCPRRequest(req); err != nil {
		return nil, err
	}
	out := &CPRScalar{RowSpacing: req.crossSpacing()}
	size := func(cols, rows int) {
		out.Width, out.Height = cols, rows
		out.Values = make([]float64, cols*rows)
		out.Valid = make([]bool, cols*rows)
	}
	sink := func(x, y int, value float64, _ string, ok bool) {
		if ok {
			out.Values[y*out.Width+x], out.Valid[y*out.Width+x] = value, true
		}
	}
	switch req.Mode {
	case CPRTransverse:
		plane, err := sampleTransverseCPR(ctx, req, size, sink)
		if err != nil {
			return nil, err
		}
		out.Plane, out.ColumnSpacing = &plane, req.crossSpacing()
	default:
		if req.Mode == CPRSlab {
			req.Mode = CPRStraightened
		}
		if err := sampleLongitudinalCPR(ctx, req, req.Mode == CPRStretched, size, sink); err != nil {
			return nil, err
		}
		out.ColumnSpacing = req.arcSpacing()
	}
	return out, nil
}
