package render

import "context"

// CPRColumn describes geometry only. OriginLPS is row zero, StepVLPS advances
// one output row, and SlabAxisLPS is the unit slab direction for this column.
// Preparing it never samples, regularizes or acquires a volume generation.
type CPRColumn struct {
	OriginLPS, StepVLPS, SlabAxisLPS [3]float64
}

type CPRGeometry struct {
	Columns                              []CPRColumn
	Width, Height                        int
	SlabThicknessMM, SlabSampleSpacingMM float64
	ColumnSpacingMM, RowSpacingMM        float64
	// Plane exists only for a transverse section; longitudinal columns are curved.
	Plane *Plane
}

// PrepareCPRGeometry exposes the same transported/rotated bases used by the
// headless CPR renderer to GPU consumers. Volume is used only for spacing
// defaults; the caller retains ownership of the source and path.
func PrepareCPRGeometry(ctx context.Context, req CPRRequest) (CPRGeometry, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return CPRGeometry{}, err
	}
	if err := validateCPRRequest(req); err != nil {
		return CPRGeometry{}, err
	}
	if req.Path == nil {
		return CPRGeometry{}, ErrCPRInput
	}
	rows := req.width()
	count := rows
	var samples []CPRPathSample
	if req.Mode != CPRTransverse {
		var err error
		count, err = req.Path.SampleCountChecked(req.arcSpacing())
		if err != nil {
			return CPRGeometry{}, err
		}
	}
	slabCount, err := cprSlabSampleCountChecked(req)
	if err != nil {
		return CPRGeometry{}, err
	}
	if err := validateCPROutput(count, rows, slabCount); err != nil {
		return CPRGeometry{}, err
	}
	offsets, err := cprSlabSampleOffsetsChecked(req, req.crossSpacing())
	if err != nil {
		return CPRGeometry{}, err
	}
	geometry := CPRGeometry{Width: count, Height: rows, Columns: make([]CPRColumn, count)}
	geometry.ColumnSpacingMM, geometry.RowSpacingMM = req.arcSpacing(), req.crossSpacing()
	if len(offsets) > 1 {
		geometry.SlabThicknessMM = offsets[len(offsets)-1] - offsets[0]
		geometry.SlabSampleSpacingMM = offsets[1] - offsets[0]
	}
	cross := req.crossSpacing()
	half := float64(rows-1) / 2
	array := func(v Vec3) [3]float64 { return [3]float64{v.X, v.Y, v.Z} }
	if req.Mode == CPRTransverse {
		frame := req.Path.FrameAt(req.ArcLength)
		u, v := rotateCPRBasis(frame.Normal, frame.Tangent, req.RotationDegrees)
		for x := range geometry.Columns {
			if err := ctx.Err(); err != nil {
				return CPRGeometry{}, err
			}
			origin := frame.Position.Add(u.Scale((float64(x) - half) * cross)).Add(v.Scale(-half * cross))
			geometry.Columns[x] = CPRColumn{array(origin), array(v.Scale(cross)), array(frame.Tangent)}
		}
		geometry.ColumnSpacingMM = cross
		plane := Plane{Origin: frame.Position.Add(u.Scale(-half * cross)).Add(v.Scale(-half * cross)), U: u.Scale(float64(count-1) * cross), V: v.Scale(float64(rows-1) * cross)}
		geometry.Plane = &plane
	} else {
		samples, err = req.Path.ResampleChecked(req.arcSpacing())
		if err != nil {
			return CPRGeometry{}, err
		}
		bases, err := prepareCPRBases(ctx, req, samples, req.Mode == CPRStretched)
		if err != nil {
			return CPRGeometry{}, err
		}
		for x, sample := range samples {
			if err := ctx.Err(); err != nil {
				return CPRGeometry{}, err
			}
			basis := bases[x]
			geometry.Columns[x] = CPRColumn{array(sample.Position.Add(basis.cross.Scale(-half * cross))), array(basis.cross.Scale(cross)), array(basis.slab)}
		}
	}
	return geometry, nil
}

func validateCPRColumns(state MPRViewState, width, height uint32, fail func(string, ...any) error) error {
	if len(state.CPRColumns) != int(width) {
		return fail("CPR column count does not match output width")
	}
	for _, column := range state.CPRColumns {
		stepNorm, axisNorm := norm3(column.StepVLPS), norm3(column.SlabAxisLPS)
		if !finite3(column.OriginLPS) || !finite3(column.StepVLPS) || !finite3(column.SlabAxisLPS) || !finite(stepNorm) || stepNorm <= 0 || !finite(axisNorm) || axisNorm < 1-1e-6 || axisNorm > 1+1e-6 {
			return fail("invalid CPR column basis")
		}
		for axis := range 3 {
			extent := column.OriginLPS[axis] + float64(height-1)*column.StepVLPS[axis]
			if !finite(extent) || !finite(extent+state.SlabThicknessMM*.5*column.SlabAxisLPS[axis]) || !finite(extent-state.SlabThicknessMM*.5*column.SlabAxisLPS[axis]) {
				return fail("CPR extent overflows")
			}
		}
	}
	return nil
}
