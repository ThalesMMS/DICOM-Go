package roi

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrSignedDistanceLimit reports a count or managed-array reservation limit.
var ErrSignedDistanceLimit = errors.New("roi: signed distance resource limit")

const (
	DefaultSignedDistanceMaxVoxels = 1 << 26
	DefaultSignedDistanceMaxBytes  = int64(1 << 30)
)

// SignedDistanceOptions controls the sign, units and finite admission bounds.
// The zero value returns millimetres, negative inside and positive outside.
type SignedDistanceOptions struct {
	InsideIsPositive bool
	// SquaredDistance returns signed squared millimetres, rather than mm.
	SquaredDistance bool
	// Zero limits select defaults; positive values are explicit finite bounds.
	// MaxBytes bounds array capacities, excluding caller input and runtime/RSS.
	MaxVoxels int
	MaxBytes  int64
}

// SignedDistanceTransform returns distances to foreground boundary voxel
// centres. Boundary means a foreground voxel with an in-grid background
// neighbour in the full 3^D neighbourhood; missing neighbours beyond the image
// are ignored. Boundary values are signed zero. A mask with no boundary gives
// signed infinities, positive outside/negative inside with default options.
//
// Coordinates are patient-space mm on orthogonal independent axes. Anisotropic
// and irregular ascending axes are supported. This is a discrete boundary map,
// not distance to a continuous subvoxel surface. Source arrays must remain
// stable for the call; returned storage is owned and cancellation returns nil.
func (m *VolumeMask) SignedDistanceTransform(ctx context.Context, opts SignedDistanceOptions) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil {
		return nil, ErrInvalidVolumeMask
	}
	n, err := signedDistanceAdmission(m.Columns, m.Rows, m.Slices, false, opts)
	if err != nil {
		return nil, err
	}
	if len(m.Voxels) != n || len(m.X) != m.Columns || len(m.Y) != m.Rows || len(m.Z) != m.Slices {
		return nil, ErrInvalidVolumeMask
	}
	return signedDistance(ctx, m, opts)
}

// SignedDistanceTransform is the 2D boundary map in physical millimetres.
// columnSpacing and rowSpacing must be positive and finite. RasterMask runs
// and dimensions must remain stable for the call. Existing interpolation's
// private signed field retains its independent historical convention.
func (m *RasterMask) SignedDistanceTransform(ctx context.Context, columnSpacing, rowSpacing float64, opts SignedDistanceOptions) ([]float64, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if m == nil || !(columnSpacing > 0) || !(rowSpacing > 0) || math.IsInf(columnSpacing, 0) || math.IsInf(rowSpacing, 0) {
		return nil, ErrInvalidVolumeMask
	}
	n, err := signedDistanceAdmission(m.Columns, m.Rows, 1, true, opts)
	if err != nil {
		return nil, err
	}
	if len(m.runs) != m.Rows {
		return nil, ErrInvalidVolumeMask
	}
	for y, runs := range m.runs {
		if y&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if len(runs) > m.Columns {
			return nil, ErrInvalidVolumeMask
		}
		end := 0
		for _, run := range runs {
			if run.Start < end || run.Start < 0 || run.End <= run.Start || run.End > m.Columns {
				return nil, ErrInvalidVolumeMask
			}
			end = run.End
		}
	}
	volume := &VolumeMask{Columns: m.Columns, Rows: m.Rows, Slices: 1,
		X: make([]float64, m.Columns), Y: make([]float64, m.Rows), Z: []float64{0}, Voxels: make([]bool, n)}
	for x := range volume.X {
		volume.X[x] = float64(x) * columnSpacing
	}
	for y := range volume.Y {
		volume.Y[y] = float64(y) * rowSpacing
		if y&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for _, run := range m.runs[y] {
			for x := run.Start; x < run.End; x++ {
				if x&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				volume.Voxels[y*m.Columns+x] = true
			}
		}
	}
	return signedDistance(ctx, volume, opts)
}

func signedDistanceAdmission(nx, ny, nz int, raster bool, opts SignedDistanceOptions) (int, error) {
	if nx <= 0 || ny <= 0 || nz <= 0 || opts.MaxVoxels < 0 || opts.MaxBytes < 0 {
		return 0, ErrInvalidVolumeMask
	}
	limit, bytes := opts.MaxVoxels, opts.MaxBytes
	if limit == 0 {
		limit = DefaultSignedDistanceMaxVoxels
	}
	if bytes == 0 {
		bytes = DefaultSignedDistanceMaxBytes
	}
	// Products, sums and capacity calculations are admitted by division before
	// arithmetic. unsigned DistanceTransform's dimensional product is now safe.
	if nx > limit || ny > limit/nx || nz > limit/(nx*ny) {
		return 0, fmt.Errorf("%w: voxel count exceeds %d", ErrSignedDistanceLimit, limit)
	}
	n := nx * ny * nz
	perVoxel := int64(9) // owned boundary bool plus distance float64
	axisCopies := int64(1)
	if raster {
		perVoxel++   // owned dense source
		axisCopies++ // uniform 2D source axes and centred transform axes
	}
	remaining := bytes
	reserve := func(count, width int64) bool {
		if count > remaining/width {
			return false
		}
		remaining -= count * width
		return true
	}
	// Existing DistanceTransform uses at most 16 workers, each with three
	// float64 lines, one int line and the final bound; int is at most 8 bytes.
	if !reserve(int64(n), perVoxel) || !reserve(int64(nx), 8*axisCopies+512) || !reserve(int64(ny), 8*axisCopies+512) || !reserve(int64(nz), 8*axisCopies+512) || !reserve(1, 8192) {
		return 0, fmt.Errorf("%w: managed array reservation exceeds %d bytes", ErrSignedDistanceLimit, bytes)
	}
	return n, nil
}

func signedDistance(ctx context.Context, source *VolumeMask, opts SignedDistanceOptions) ([]float64, error) {
	// Subtract each axis's origin before squaring: large patient-coordinate
	// translations must not change the envelope intersections.
	axes := [3][]float64{}
	for i, axis := range [][]float64{source.X, source.Y, source.Z} {
		var err error
		axes[i], err = signedDistanceAxis(ctx, axis)
		if err != nil {
			return nil, err
		}
	}
	boundary := &VolumeMask{Columns: source.Columns, Rows: source.Rows, Slices: source.Slices,
		X: axes[0], Y: axes[1], Z: axes[2], Voxels: make([]bool, len(source.Voxels))}
	nx, ny, nz := source.Columns, source.Rows, source.Slices
	for z := 0; z < nz; z++ {
		for y := 0; y < ny; y++ {
			for x := 0; x < nx; x++ {
				index := (z*ny+y)*nx + x
				if index&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				if !source.Voxels[index] {
					continue
				}
				for zz := max(0, z-1); zz <= min(nz-1, z+1) && !boundary.Voxels[index]; zz++ {
					for yy := max(0, y-1); yy <= min(ny-1, y+1) && !boundary.Voxels[index]; yy++ {
						for xx := max(0, x-1); xx <= min(nx-1, x+1); xx++ {
							if !source.Voxels[(zz*ny+yy)*nx+xx] {
								boundary.Voxels[index] = true
								break
							}
						}
					}
				}
			}
		}
	}
	out, err := boundary.DistanceTransform(ctx, true)
	if err != nil {
		return nil, err
	}
	for i, inside := range source.Voxels {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if opts.SquaredDistance {
			out[i] *= out[i]
		}
		if inside != opts.InsideIsPositive {
			out[i] = -out[i]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func signedDistanceAxis(ctx context.Context, axis []float64) ([]float64, error) {
	for i, value := range axis {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if math.IsNaN(value) || math.IsInf(value, 0) || i > 0 && value <= axis[i-1] {
			return nil, fmt.Errorf("%w: finite ascending axes required", ErrInvalidVolumeMask)
		}
	}
	span := axis[len(axis)-1] - axis[0]
	if math.IsInf(span, 0) || span > math.Sqrt(math.MaxFloat64/64) {
		return nil, fmt.Errorf("%w: squared coordinate span is not representable", ErrInvalidVolumeMask)
	}
	out := make([]float64, len(axis))
	for i, value := range axis {
		out[i] = value - axis[0]
		if i > 0 {
			gap := out[i] - out[i-1]
			if gap <= 0 || gap*gap == 0 {
				return nil, fmt.Errorf("%w: squared coordinate spacing is not representable", ErrInvalidVolumeMask)
			}
		}
	}
	return out, ctx.Err()
}
