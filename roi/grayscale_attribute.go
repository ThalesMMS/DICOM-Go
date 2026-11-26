package roi

import (
	"context"
	"errors"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/imagefilter"
)

var (
	ErrInvalidGrayscaleAttribute = errors.New("roi: invalid grayscale attribute morphology")
	ErrGrayscaleAttributeLimit   = errors.New("roi: grayscale attribute resource limit")
)

// AttributeDimension selects the topology explicitly. Auto treats a single
// z slice as 2D; ThreeDimensional can instead model a thin physical 3D volume.
type AttributeDimension uint8

const (
	AttributeAuto AttributeDimension = iota
	Attribute2D
	Attribute3D
)

// AttributeSizeUnit measures connected upper/lower level-set components.
type AttributeSizeUnit uint8

const (
	AttributeSamples AttributeSizeUnit = iota
	AttributeSquareMM
	AttributeCubicMM
)

// GrayscaleAttributeOptions applies to both operators. Connectivity is 4 or 8
// in 2D, 6, 18 or 26 in 3D; zero chooses 4 or 6. Threshold comparison keeps
// components whose size is >= threshold. Samples may use fractional thresholds.
// MaxVoxels and MaxBytes zero select finite defaults (64M samples and 1GiB).
// MaxBytes budgets all algorithm arrays, including sort scratch and output,
// plus fixed workspace; it excludes caller-owned input and Go allocator/RSS
// overhead. Negative limits are invalid. The grid is global, never tiled.
type GrayscaleAttributeOptions struct {
	Dimension    AttributeDimension
	Connectivity int
	Unit         AttributeSizeUnit
	MaxVoxels    int
	MaxBytes     int64
}

// GrayscaleAttributeOpen removes small bright connected peaks by area/volume,
// preserving surviving shapes. It is the upper-level-set attribute opening,
// not opening with a ball or filtering a single binarized mask. The root level
// is retained even when threshold exceeds the whole image size, so output is
// then the global minimum. Zero threshold returns an owned identity copy.
// Finite float32 intensities are ordered directly without quantization;
// NaN/Inf are rejected. The input, dimensions and spacing are never changed.
func GrayscaleAttributeOpen(ctx context.Context, src *imagefilter.Volume, threshold float64, opts GrayscaleAttributeOptions) (*imagefilter.Volume, error) {
	return grayscaleAttribute(ctx, src, threshold, opts, false)
}

// GrayscaleAttributeClose is the lower-level-set dual of opening: small dark
// connected valleys are filled, and an excessive threshold yields the global
// maximum. No negation/subtraction is used, including at float32 extremes.
func GrayscaleAttributeClose(ctx context.Context, src *imagefilter.Volume, threshold float64, opts GrayscaleAttributeOptions) (*imagefilter.Volume, error) {
	return grayscaleAttribute(ctx, src, threshold, opts, true)
}

// Per-sample storage: output4 + order4 + merge scratch4 + union-find4 +
// component parent4 + component count4 bytes. Int32 indices bound the topology.
const attributeBytesPerSample int64 = 24
const attributeFixedBytes int64 = 1024

func grayscaleAttribute(ctx context.Context, src *imagefilter.Volume, threshold float64, opts GrayscaleAttributeOptions, close bool) (*imagefilter.Volume, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if src == nil || threshold < 0 || math.IsNaN(threshold) || math.IsInf(threshold, 0) || opts.MaxVoxels < 0 || opts.MaxBytes < 0 {
		return nil, fmt.Errorf("%w: input, threshold or limits", ErrInvalidGrayscaleAttribute)
	}
	n := 1
	for _, d := range src.Dims {
		if d <= 0 || d > math.MaxInt32/n {
			return nil, fmt.Errorf("%w: dimensions %v", ErrInvalidGrayscaleAttribute, src.Dims)
		}
		n *= d
	}
	if len(src.Data) != n {
		return nil, fmt.Errorf("%w: data length", ErrInvalidGrayscaleAttribute)
	}
	if opts.MaxVoxels == 0 {
		opts.MaxVoxels = 64 << 20
	}
	if opts.MaxBytes == 0 {
		opts.MaxBytes = 1 << 30
	}
	if n > opts.MaxVoxels || int64(n) > (opts.MaxBytes-attributeFixedBytes)/attributeBytesPerSample || opts.MaxBytes < attributeFixedBytes {
		return nil, fmt.Errorf("%w: %d samples need %d array/workspace bytes", ErrGrayscaleAttributeLimit, n, int64(n)*attributeBytesPerSample+attributeFixedBytes)
	}
	dim := opts.Dimension
	if dim == AttributeAuto {
		dim = Attribute3D
		if src.Dims[2] == 1 {
			dim = Attribute2D
		}
	}
	if (dim != Attribute2D && dim != Attribute3D) || (dim == Attribute2D && src.Dims[2] != 1) {
		return nil, fmt.Errorf("%w: dimension", ErrInvalidGrayscaleAttribute)
	}
	conn := opts.Connectivity
	if conn == 0 {
		conn = 6
		if dim == Attribute2D {
			conn = 4
		}
	}
	if (dim == Attribute2D && conn != 4 && conn != 8) || (dim == Attribute3D && conn != 6 && conn != 18 && conn != 26) {
		return nil, fmt.Errorf("%w: connectivity %d", ErrInvalidGrayscaleAttribute, conn)
	}
	if opts.Unit > AttributeCubicMM || (opts.Unit == AttributeSquareMM && dim != Attribute2D) || (opts.Unit == AttributeCubicMM && dim != Attribute3D) {
		return nil, fmt.Errorf("%w: unit/dimension", ErrInvalidGrayscaleAttribute)
	}
	measure := float64(1)
	for axis, spacing := range src.SpacingMM {
		required := src.Dims[axis] > 1 || (opts.Unit != AttributeSamples && (dim == Attribute3D || axis < 2))
		if required && (!(spacing > 0) || math.IsInf(spacing, 0) || math.IsNaN(spacing)) {
			return nil, fmt.Errorf("%w: spacing axis %d", ErrInvalidGrayscaleAttribute, axis)
		}
		if opts.Unit != AttributeSamples && (dim == Attribute3D || axis < 2) {
			measure *= spacing
		}
	}
	if !(measure > 0) || math.IsInf(measure, 0) || math.IsNaN(measure) {
		return nil, fmt.Errorf("%w: physical sample measure", ErrInvalidGrayscaleAttribute)
	}
	for i, value := range src.Data {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
			return nil, fmt.Errorf("%w: nonfinite sample %d", ErrInvalidGrayscaleAttribute, i)
		}
	}
	out := &imagefilter.Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, n)}
	if threshold == 0 {
		for i, v := range src.Data {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			out.Data[i] = v
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		return out, nil
	}
	order, scratch := make([]int32, n), make([]int32, n)
	parent, union, count := make([]int32, n), make([]int32, n), make([]uint32, n)
	for i := range order {
		if i&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		order[i] = int32(i)
		union[i] = -1
	}
	if err := attributeSort(ctx, order, scratch, src.Data, close); err != nil {
		return nil, err
	}
	// Sweep decreasing upper levels (opening) or increasing lower levels
	// (closing). Each newly activated sample parents adjacent active roots.
	// Equal-level edges are canonicalized after the complete plateau is built;
	// no plateau outcome depends on index tie order.
	var offsets [26][3]int
	no := 0
	for dz := -1; dz <= 1; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				axes := 0
				for _, d := range [3]int{dx, dy, dz} {
					if d != 0 {
						axes++
					}
				}
				if axes == 0 || (dim == Attribute2D && dz != 0) || ((conn == 4 || conn == 6) && axes > 1) || (conn == 18 && axes > 2) {
					continue
				}
				offsets[no] = [3]int{dx, dy, dz}
				no++
			}
		}
	}
	visits := 0
	find := func(index int32) (int32, error) {
		root := index
		for union[root] != root {
			visits++
			if visits&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return 0, err
				}
			}
			root = union[root]
		}
		for index != root {
			next := union[index]
			union[index] = root
			index = next
		}
		return root, nil
	}
	nx, ny, nz := src.Dims[0], src.Dims[1], src.Dims[2]
	for j, p := range order {
		if j&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		parent[p], union[p], count[p] = p, p, 1
		x, y, z := int(p)%nx, (int(p)/nx)%ny, int(p)/(nx*ny)
		for _, delta := range offsets[:no] {
			qx, qy, qz := x+delta[0], y+delta[1], z+delta[2]
			if qx < 0 || qx >= nx || qy < 0 || qy >= ny || qz < 0 || qz >= nz {
				continue
			}
			q := (qz*ny+qy)*nx + qx
			if union[q] < 0 {
				continue
			}
			root, err := find(int32(q))
			if err != nil {
				return nil, err
			}
			if root != p {
				parent[root], union[root] = p, p
				count[p] += count[root]
			}
		}
	}
	// Parents occur later in the sweep, so reverse traversal visits ancestors
	// first. Canonicalization skips redundant same-level ancestors. Reconstruct
	// each rejected node from its closest retained ancestor; the global root is
	// unconditional, making thresholds larger than the domain well defined.
	for j := n - 1; j >= 0; j-- {
		if j&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		p := order[j]
		q := parent[p]
		if src.Data[q] == src.Data[parent[q]] {
			parent[p] = parent[q]
			q = parent[p]
		}
		keep := float64(count[p])*measure >= threshold
		if p == q || keep {
			out.Data[p] = src.Data[p]
		} else {
			out.Data[p] = out.Data[q]
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// Iterative stable merge sorting bounds memory and checks cancellation inside
// the comparison/copy loops. Intensity ordering is exact float32; index breaks
// equal-intensity ties without converting intensities to integer bins.
func attributeSort(ctx context.Context, order, scratch []int32, values []float32, close bool) error {
	n := len(order)
	steps := 0
	before := func(a, b int32) bool {
		if values[a] == values[b] {
			return a < b
		}
		if close {
			return values[a] < values[b]
		}
		return values[a] > values[b]
	}
	for width := 1; width < n; width *= 2 {
		for begin := 0; begin < n; {
			mid := begin + min(width, n-begin)
			end := mid + min(width, n-mid)
			a, b := begin, mid
			for k := begin; k < end; k++ {
				steps++
				if steps&4095 == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if a < mid && (b >= end || before(order[a], order[b])) {
					scratch[k] = order[a]
					a++
				} else {
					scratch[k] = order[b]
					b++
				}
			}
			begin = end
		}
		for i := range order {
			if i&4095 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			order[i] = scratch[i]
		}
		if width > n/2 {
			break
		}
	}
	return ctx.Err()
}
