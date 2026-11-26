package spatialreg

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// InspectionField exports original grid nodes for GPU presentation. It performs
// no interpolation, displacement evaluation or presentation mesh generation.
// The caller establishes the directly applied registration and its generation.
func (r *Registration) InspectionField(ctx context.Context, generation uint64, registeredFrameUID string) (render.DeformationFieldDescriptor, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return render.DeformationFieldDescriptor{}, nil, err
	}
	if r == nil || r.Grid == nil {
		return render.DeformationFieldDescriptor{}, nil, fmt.Errorf("%w: deformation grid required", ErrInvalidObject)
	}
	g := r.Grid
	m := render.GeometryAffine{g.RowDirection.X * g.Spacing.X, g.ColumnDirection.X * g.Spacing.Y, g.NormalDirection.X * g.Spacing.Z, g.Origin.X, g.RowDirection.Y * g.Spacing.X, g.ColumnDirection.Y * g.Spacing.Y, g.NormalDirection.Y * g.Spacing.Z, g.Origin.Y, g.RowDirection.Z * g.Spacing.X, g.ColumnDirection.Z * g.Spacing.Y, g.NormalDirection.Z * g.Spacing.Z, g.Origin.Z, 0, 0, 0, 1}
	d := render.DeformationFieldDescriptor{Generation: generation, RegisteredFrameUID: registeredFrameUID, SourceFrameUID: r.SourceFrameOfReferenceUID, Dimensions: g.Dimensions, GridToPatientLPS: m, Pre: r.Pre.Values, Post: r.Post.Values}
	if err := d.Validate(); err != nil {
		return d, nil, err
	}
	size, _ := d.PayloadBytes()
	if uint64(len(g.vectors))*16 != size {
		return d, nil, fmt.Errorf("%w: grid node count changed", ErrInvalidObject)
	}
	data := make([]byte, int(size))
	for i, v := range g.vectors {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return d, nil, err
			}
		}
		if undefinedVector(v) {
			continue
		}
		for a, value := range [4]float32{v.X, v.Y, v.Z, 1} {
			binary.LittleEndian.PutUint32(data[i*16+a*4:], math.Float32bits(value))
		}
	}
	if err := d.ValidatePayload(data); err != nil {
		return d, nil, err
	}
	if err := ctx.Err(); err != nil {
		return d, nil, err
	}
	return d, data, nil
}
