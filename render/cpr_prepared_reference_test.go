package render

import (
	"context"
	"image"
	"math"
)

// Literal baseline loops, frozen before production changes. Interpolation,
// slabs, limits and windowing are unchanged dependencies; this reference
// deliberately retains per-pixel bases and PatientToVoxel calls.
func cpr1432ReferenceSampleLongitudinalCPR(ctx context.Context, req CPRRequest, stretched bool, size func(cols, rows int), sink cprSink) error {
	vol := req.Volume
	sampleCount, err := req.Path.SampleCountChecked(req.arcSpacing())
	if err != nil {
		return err
	}
	width := req.width()
	cross := req.crossSpacing()
	slabSamples, err := cprSlabSampleCountChecked(req)
	if err != nil {
		return err
	}
	if err := validateCPROutput(sampleCount, width, slabSamples); err != nil {
		return err
	}
	samples, err := req.Path.ResampleChecked(req.arcSpacing())
	if err != nil {
		return err
	}
	slabOffsets, err := cprSlabSampleOffsetsChecked(req, cross)
	if err != nil {
		return err
	}
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		return ErrCPRInput
	}
	defer sampler.Close()
	half := float64(width-1) / 2
	photometric := vol.Photometric()

	ref := req.StretchDir
	if stretched && ref == (Vec3{}) {
		ref = samples[0].Normal
	}

	size(len(samples), width)
	return parallelRowsContext(ctx, width, func(r int) {
		offset := (float64(r) - half) * cross
		for c, s := range samples {
			crossDir := s.Normal
			if stretched {
				crossDir = ref.Sub(s.Tangent.Scale(ref.Dot(s.Tangent))).Normalize()
				if crossDir == (Vec3{}) {
					crossDir = s.Normal
				}
			}
			crossDir, slabDir := rotateCPRBasis(crossDir, s.Tangent, req.RotationDegrees)
			base := s.Position.Add(crossDir.Scale(offset))
			if len(slabOffsets) > 0 {
				value, ph, ok := reduceSlab(req.SlabMode, 0, len(slabOffsets)-1, func(k int) (float64, string, bool) {
					p := base.Add(slabDir.Scale(slabOffsets[k]))
					v, sampleOK := sampler.sampleAt(sampler.vol.PatientToVoxel(p), req.Interpolation)
					return v, photometric, sampleOK
				})
				sink(c, r, value, ph, ok)
				continue
			}
			val, ok := sampler.sampleAt(sampler.vol.PatientToVoxel(base), req.Interpolation)
			sink(c, r, val, photometric, ok)
		}
	})
}

func cpr1432ReferenceSampleTransverseCPR(ctx context.Context, req CPRRequest, size func(cols, rows int), sink cprSink) (Plane, error) {
	vol := req.Volume
	frame := req.Path.FrameAt(req.ArcLength)
	normal, binormal := rotateCPRBasis(frame.Normal, frame.Tangent, req.RotationDegrees)
	side := req.width()
	cross := req.crossSpacing()
	slabSamples, err := cprSlabSampleCountChecked(req)
	if err != nil {
		return Plane{}, err
	}
	if err := validateCPROutput(side, side, slabSamples); err != nil {
		return Plane{}, err
	}
	slabOffsets, err := cprSlabSampleOffsetsChecked(req, cross)
	if err != nil {
		return Plane{}, err
	}
	sampler, ok := newVolumeSampler(vol)
	if !ok {
		return Plane{}, ErrCPRInput
	}
	defer sampler.Close()
	half := float64(side-1) / 2
	photometric := vol.Photometric()
	extent := float64(side-1) * cross
	plane := Plane{
		Origin: frame.Position.Add(normal.Scale(-half * cross)).Add(binormal.Scale(-half * cross)),
		U:      normal.Scale(extent),
		V:      binormal.Scale(extent),
	}

	size(side, side)
	err = parallelRowsContext(ctx, side, func(j int) {
		voff := (float64(j) - half) * cross
		for i := 0; i < side; i++ {
			uoff := (float64(i) - half) * cross
			base := frame.Position.Add(normal.Scale(uoff)).Add(binormal.Scale(voff))
			if len(slabOffsets) > 0 {
				value, ph, ok := reduceSlab(req.SlabMode, 0, len(slabOffsets)-1, func(k int) (float64, string, bool) {
					p := base.Add(frame.Tangent.Scale(slabOffsets[k]))
					v, sampleOK := sampler.sampleAt(sampler.vol.PatientToVoxel(p), req.Interpolation)
					return v, photometric, sampleOK
				})
				sink(i, j, value, ph, ok)
				continue
			}
			val, ok := sampler.sampleAt(sampler.vol.PatientToVoxel(base), req.Interpolation)
			sink(i, j, val, photometric, ok)
		}
	})
	return plane, err
}

func cpr1432ReferenceScalar(ctx context.Context, req CPRRequest) (*CPRScalar, error) {
	if err := cpr1432ReferenceValidateCPRRequest(req); err != nil {
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
	if req.Mode == CPRTransverse {
		plane, err := cpr1432ReferenceSampleTransverseCPR(ctx, req, size, sink)
		out.Plane = &plane
		out.ColumnSpacing = req.crossSpacing()
		return out, err
	}
	out.ColumnSpacing = req.arcSpacing()
	err := cpr1432ReferenceSampleLongitudinalCPR(ctx, req, req.Mode == CPRStretched, size, sink)
	return out, err
}
func cpr1432ReferenceGray(ctx context.Context, req CPRRequest) (*image.Gray, error) {
	if err := cpr1432ReferenceValidateCPRRequest(req); err != nil {
		return nil, err
	}
	mapper := prepareWindow(normalizeWindow(req.Window, WindowLevel{Center: defaultWindowCenter, Width: defaultWindowWidth}))
	var out *image.Gray
	size := func(cols, rows int) { out = image.NewGray(image.Rect(0, 0, cols, rows)) }
	sink := func(x, y int, value float64, ph string, ok bool) {
		if ok {
			out.Pix[out.PixOffset(x, y)] = displayGrayMapped(value, mapper, ph)
		}
	}
	if req.Mode == CPRTransverse {
		_, err := cpr1432ReferenceSampleTransverseCPR(ctx, req, size, sink)
		return out, err
	}
	err := cpr1432ReferenceSampleLongitudinalCPR(ctx, req, req.Mode == CPRStretched, size, sink)
	return out, err
}

func cpr1432ReferenceValidateCPRRequest(req CPRRequest) error {
	if req.Width < 0 {
		return &CPRInputError{Field: "width", Reason: "width cannot be negative"}
	}
	if req.width() > MaxCPROutputDimension {
		return &CPRLimitError{Resource: "output_dimension", Value: uint64(req.width()), Limit: MaxCPROutputDimension}
	}
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"arc_spacing", req.ArcSpacing},
		{"cross_spacing", req.CrossSpacing},
		{"thickness_mm", req.ThicknessMM},
	} {
		if field.value < 0 || math.IsNaN(field.value) || math.IsInf(field.value, 0) {
			return &CPRInputError{Field: field.name, Reason: "value must be zero or positive and finite"}
		}
	}
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"arc_length", req.ArcLength},
		{"rotation_degrees", req.RotationDegrees},
		{"window_center", req.Window.Center},
		{"window_width", req.Window.Width},
	} {
		if math.IsNaN(field.value) || math.IsInf(field.value, 0) {
			return &CPRInputError{Field: field.name, Reason: "value is not finite"}
		}
	}
	if req.StretchDir != (Vec3{}) && !finiteVec3(req.StretchDir) {
		return &CPRInputError{Field: "stretch_direction", Reason: "coordinate is not finite"}
	}
	if req.Thickness < 0 {
		return &CPRInputError{Field: "thickness", Reason: "sample count cannot be negative"}
	}
	if req.Thickness > MaxCPRSlabSamples {
		return &CPRLimitError{Resource: "slab_samples", Value: uint64(req.Thickness), Limit: MaxCPRSlabSamples}
	}
	if req.SlabMode < SlabNone || req.SlabMode > SlabAverage {
		return &CPRInputError{Field: "slab_mode", Reason: "mode is not supported"}
	}
	if req.Mode < CPRStraightened || req.Mode > CPRSlab {
		return &CPRInputError{Field: "mode", Reason: "mode is not supported"}
	}
	return nil
}
