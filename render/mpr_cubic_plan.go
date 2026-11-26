package render

// Precomputed Catmull-Rom taps for separable cubic reslices (#1256), after
// the per-axis weight tables of VTK's vtkImageReslice (no VTK code is
// copied). When a reslice row, or a slab ray, runs along one voxel axis of
// an untilted volume, two of its three voxel coordinates are the same for
// every sample and the third repeats from row to row: the clamped indices,
// weights and voxel offsets of each axis are computed once per row (or once
// per frame for the varying axis) instead of once per sample. Every sample
// reads the same taps with the same weights in the same order as cubicAt,
// so the images are identical bit for bit. Oblique rows, tilted volumes and
// CPR keep the per-sample path.

// cubicAxisTaps are the Catmull-Rom taps of one voxel coordinate: false ok
// when the coordinate is more than half a voxel outside the axis, otherwise
// cubicTaps of the clamped coordinate with the voxel offsets of the taps
// (index × the axis stride).
type cubicAxisTaps struct {
	ok     bool
	n      int
	index  [4]int
	offset [4]int
	weight [4]float64
}

func newCubicAxisTaps(v float64, n, stride int) cubicAxisTaps {
	v, ok := clampToGrid(v, n)
	if !ok {
		return cubicAxisTaps{}
	}
	taps := cubicAxisTaps{ok: true}
	taps.index, taps.weight, taps.n = cubicTaps(v, n)
	for i := 0; i < taps.n; i++ {
		taps.offset[i] = taps.index[i] * stride
	}
	return taps
}

// cubicFromTaps is cubicAt with the axes' taps precomputed: the same voxel
// values, weights and summation order.
func (s *volumeSampler) cubicFromTaps(x, y, z *cubicAxisTaps) (float64, bool) {
	if !x.ok || !y.ok || !z.ok {
		return 0, false
	}
	sum := 0.0
	switch {
	case s.directValues():
		for k := 0; k < z.n; k++ {
			plane := 0.0
			for j := 0; j < y.n; j++ {
				base := z.offset[k] + y.offset[j]
				row := 0.0
				for i := 0; i < x.n; i++ {
					row += x.weight[i] * float64(s.values[base+x.offset[i]])
				}
				plane += y.weight[j] * row
			}
			sum += z.weight[k] * plane
		}
	case s.packed16():
		for k := 0; k < z.n; k++ {
			plane := 0.0
			for j := 0; j < y.n; j++ {
				base := z.offset[k] + y.offset[j]
				row := 0.0
				for i := 0; i < x.n; i++ {
					row += x.weight[i] * s.packed16At(base+x.offset[i])
				}
				plane += y.weight[j] * row
			}
			sum += z.weight[k] * plane
		}
	default:
		for k := 0; k < z.n; k++ {
			plane := 0.0
			for j := 0; j < y.n; j++ {
				row := 0.0
				for i := 0; i < x.n; i++ {
					row += x.weight[i] * s.valueOrZero(x.index[i], y.index[j], z.index[k])
				}
				plane += y.weight[j] * row
			}
			sum += z.weight[k] * plane
		}
	}
	return sum, true
}

// directValues reports whether every voxel is in the packed float32 values,
// which valueAt reads first.
func (s *volumeSampler) directValues() bool {
	return len(s.values) > 0 && len(s.values) >= s.cols*s.rows*s.depth
}

// cubicLineAxis is the voxel axis along which a separable line varies.
type cubicLineAxis uint8

const (
	cubicLineConstant cubicLineAxis = iota // no coordinate varies
	cubicLineX
	cubicLineY
	cubicLineZ
)

// separableAxis returns the one voxel axis along which the affine line
// varies, or false when the line is not affine or varies along several.
func (l voxelLine) separableAxis() (cubicLineAxis, bool) {
	if !l.affine {
		return 0, false
	}
	axis, varying := cubicLineConstant, 0
	if l.dx != 0 {
		axis, varying = cubicLineX, varying+1
	}
	if l.dy != 0 {
		axis, varying = cubicLineY, varying+1
	}
	if l.dpos != 0 {
		axis, varying = cubicLineZ, varying+1
	}
	return axis, varying <= 1
}

// cubicLineKey identifies the varying coordinate of a separable line: its
// axis, start and step. Lines with equal keys sample that axis at the same
// coordinates.
type cubicLineKey struct {
	axis        cubicLineAxis
	start, step float64
}

func (l voxelLine) cubicKey(axis cubicLineAxis) cubicLineKey {
	switch axis {
	case cubicLineX:
		return cubicLineKey{axis: axis, start: l.x0, step: l.dx}
	case cubicLineY:
		return cubicLineKey{axis: axis, start: l.y0, step: l.dy}
	case cubicLineZ:
		return cubicLineKey{axis: axis, start: l.pos0, step: l.dpos}
	}
	return cubicLineKey{axis: axis}
}

// cubicLinePlan is one worker's precomputed taps of separable lines sampled
// at the parameters params(i), i < count. The varying axis's table is kept
// across lines with the same key; the constant axes are recomputed per line.
type cubicLinePlan struct {
	sampler *volumeSampler
	count   int
	param   func(i int) float64
	key     cubicLineKey
	table   []cubicAxisTaps
	valid   bool
	// x, y, z are the current line's constant axes (the varying one is
	// read from table).
	x, y, z cubicAxisTaps
	axis    cubicLineAxis
}

func newCubicLinePlan(sampler *volumeSampler, count int, param func(i int) float64) *cubicLinePlan {
	return &cubicLinePlan{sampler: sampler, count: count, param: param}
}

// prepare readies the plan for line and reports whether it is separable;
// the per-sample path must be used otherwise.
func (p *cubicLinePlan) prepare(line voxelLine) bool {
	axis, ok := line.separableAxis()
	if !ok {
		return false
	}
	s := p.sampler
	planeStride := s.cols * s.rows
	first := line.at(p.param(0))
	p.axis = axis
	p.x = newCubicAxisTaps(first.X, s.cols, 1)
	p.y = newCubicAxisTaps(first.Y, s.rows, s.cols)
	p.z = newCubicAxisTaps(first.Z, s.depth, planeStride)
	if axis == cubicLineConstant {
		return true
	}
	key := line.cubicKey(axis)
	if p.valid && key == p.key {
		return true
	}
	if cap(p.table) < p.count {
		p.table = make([]cubicAxisTaps, p.count)
	}
	p.table = p.table[:p.count]
	for i := range p.table {
		position := line.at(p.param(i))
		switch axis {
		case cubicLineX:
			p.table[i] = newCubicAxisTaps(position.X, s.cols, 1)
		case cubicLineY:
			p.table[i] = newCubicAxisTaps(position.Y, s.rows, s.cols)
		default:
			p.table[i] = newCubicAxisTaps(position.Z, s.depth, planeStride)
		}
	}
	p.key, p.valid = key, true
	return true
}

// sample is the cubic sample i of the prepared line.
func (p *cubicLinePlan) sample(i int) (float64, bool) {
	switch p.axis {
	case cubicLineX:
		return p.sampler.cubicFromTaps(&p.table[i], &p.y, &p.z)
	case cubicLineY:
		return p.sampler.cubicFromTaps(&p.x, &p.table[i], &p.z)
	case cubicLineZ:
		return p.sampler.cubicFromTaps(&p.x, &p.y, &p.table[i])
	}
	return p.sampler.cubicFromTaps(&p.x, &p.y, &p.z)
}
