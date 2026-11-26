package render

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// FastMarchingTopologyMode selects an explicit discrete/cubical constraint.
// Foreground is 8-connected in XY 2D or 26-connected in XYZ 3D; background
// is respectively 4/6-connected. See docs/FAST_MARCHING_TOPOLOGY.md.
type FastMarchingTopologyMode uint8

const (
	FastMarchingTopologyNone FastMarchingTopologyMode = iota
	// Strict preserves initial components, handles and cavities.
	FastMarchingTopologyStrict
	// NoHandles permits component mergers, but preserves handles and cavities.
	// This is conservative: filling a preexisting cavity is refused too.
	FastMarchingTopologyNoHandles
)

// FastMarchingTopologyOptions bounds the opt-in topology operation. Zero
// quotas default to 512 MiB logical payload and 1 billion charged work units.
// Context deadlines bound elapsed time. The implementation is serial.
type FastMarchingTopologyOptions struct {
	Mode     FastMarchingTopologyMode
	MaxBytes uint64
	MaxWork  uint64
}

// FastMarchingTopologyReport is diagnostic metadata, not a convergence claim.
// Rejections are terminal: a rejected node is not reconsidered later.
type FastMarchingTopologyReport struct {
	Mode                                                                  FastMarchingTopologyMode
	Dimension                                                             int
	InitialComponents, FinalComponents                                    int
	RejectedCandidates, RejectedMerge, RejectedHandle, RejectedBackground int
	AvailableUnreached, Unavailable                                       int
	Frozen                                                                int
	LogicalBytes, Work                                                    uint64
	StopReason                                                            string
	Complete                                                              bool
}

// FastMarchingTopologyResult keeps the existing arrival field and a separate
// topology report. The source speed buffer is borrowed/stable until return;
// arrival field and seed indices belong to the caller.
type FastMarchingTopologyResult struct {
	*FastMarchingResult
	Report FastMarchingTopologyReport
}

// FastMarchingTopologyFailure returns diagnostic counts without publishing a
// partial arrival field. Unwrap retains input/limit/context error matching.
type FastMarchingTopologyFailure struct {
	Cause  error
	Report FastMarchingTopologyReport
}

func (e *FastMarchingTopologyFailure) Error() string {
	return "render: topology fast marching: " + e.Cause.Error()
}
func (e *FastMarchingTopologyFailure) Unwrap() error { return e.Cause }

var ErrFastMarchingTopologyLimit = errors.New("render: topology fast marching resource limit")

// FastMarchingConstrained uses the existing first-order Eikonal solver with
// admission at each candidate. None delegates to the unchanged legacy API.
// Constrained seeds define the initial foreground and must have equal finite
// arrival values, positive finite speeds, and precede the stopping threshold.
// Other unavailable speeds are background barriers, not initial foreground.
func FastMarchingConstrained(ctx context.Context, grid FastMarchingGrid, seeds []FastMarchingSeed, options FastMarchingOptions, topology FastMarchingTopologyOptions) (*FastMarchingTopologyResult, error) {
	if topology.Mode > FastMarchingTopologyNoHandles {
		return nil, fmt.Errorf("%w: topology mode", ErrFastMarchingInput)
	}
	if topology.Mode == FastMarchingTopologyNone {
		r, e := FastMarching(ctx, grid, seeds, options)
		if e != nil {
			return nil, e
		}
		return &FastMarchingTopologyResult{FastMarchingResult: r, Report: FastMarchingTopologyReport{Mode: topology.Mode, StopReason: "disabled"}}, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c, e := newFMTopology(ctx, grid, seeds, options, topology)
	if e != nil {
		report := FastMarchingTopologyReport{Mode: topology.Mode}
		if grid.Nz == 1 {
			report.Dimension = 2
		} else if grid.Nz > 1 {
			report.Dimension = 3
		}
		var failure *FastMarchingTopologyFailure
		if errors.As(e, &failure) {
			report = failure.Report
			e = failure.Cause
		}
		return nil, fmTopologyFailure(e, report)
	}
	if e := c.charge(uint64(len(grid.Speed)) * 2); e != nil {
		return nil, fmTopologyFailure(e, c.report)
	}
	r, e := fastMarchingControlled(ctx, grid, seeds, options, true, c)
	if e != nil {
		return nil, fmTopologyFailure(e, c.report)
	}
	c.report.Frozen = r.Frozen
	for i, t := range r.Times {
		if e := c.charge(1); e != nil {
			return nil, fmTopologyFailure(e, c.report)
		}
		if positiveFinite(grid.Speed[i]) {
			if math.IsInf(t, 1) {
				c.report.AvailableUnreached++
			}
		} else {
			c.report.Unavailable++
		}
	}
	if c.report.StopReason == "" {
		if c.report.RejectedCandidates > 0 {
			c.report.StopReason = "topology-rejections"
		} else {
			c.report.StopReason = "front-exhausted"
		}
	}
	c.report.Complete = c.report.AvailableUnreached == 0
	if e := ctx.Err(); e != nil {
		return nil, fmTopologyFailure(e, c.report)
	}
	return &FastMarchingTopologyResult{FastMarchingResult: r, Report: c.report}, nil
}

func fmTopologyFailure(cause error, report FastMarchingTopologyReport) *FastMarchingTopologyFailure {
	report.Complete = false
	report.StopReason = "error"
	var limit *FastMarchingLimitError
	switch {
	case errors.Is(cause, ErrFastMarchingTopologyLimit), errors.As(cause, &limit):
		report.StopReason = "budget"
	case errors.Is(cause, context.DeadlineExceeded):
		report.StopReason = "deadline"
	case errors.Is(cause, context.Canceled):
		report.StopReason = "canceled"
	case errors.Is(cause, ErrFastMarchingInput):
		report.StopReason = "invalid-input"
	}
	return &FastMarchingTopologyFailure{Cause: cause, Report: report}
}

type fmTopology struct {
	ctx             context.Context
	nx, ny, nz, dim int
	parent          []int32
	rank            []byte
	limit           uint64
	report          FastMarchingTopologyReport
}

func newFMTopology(ctx context.Context, g FastMarchingGrid, seeds []FastMarchingSeed, o FastMarchingOptions, t FastMarchingTopologyOptions) (*fmTopology, error) {
	n, ok := boundedCPRAssistantProduct(g.Nx, g.Ny, g.Nz, MaxFastMarchingNodes)
	if g.Nx < 2 || g.Ny < 2 || g.Nz < 1 || !ok || int(n) != len(g.Speed) || !positiveFinite(g.Spacing.X) || !positiveFinite(g.Spacing.Y) || !positiveFinite(g.Spacing.Z) || len(seeds) == 0 {
		return nil, fmt.Errorf("%w: constrained 2D/3D geometry/domain", ErrFastMarchingInput)
	}
	if o.StoppingValue < 0 || math.IsNaN(o.StoppingValue) || o.MaxFrozen < 0 || o.MaxTrial < 0 {
		return nil, fmt.Errorf("%w: invalid stopping criteria", ErrFastMarchingInput)
	}
	if t.MaxBytes == 0 {
		t.MaxBytes = 512 << 20
	}
	if t.MaxWork == 0 {
		t.MaxWork = 1_000_000_000
	}
	// N*(borrowed speeds8+times8+state1+heap pos4+heap nodes4+DSU4+rank1)
	// plus borrowed seeds32 + retained seed indices24 rounded to64 per seed,
	// borrowed targets24 + retained target indices8 per target, and constant local arrays.
	// Checked before allocation.
	bytes := uint64(n)*30 + 8192
	for _, q := range []struct {
		n int
		b uint64
	}{{len(seeds), 64}, {len(o.Targets), 32}} {
		if uint64(q.n) > (^uint64(0)-bytes)/q.b {
			return nil, fmt.Errorf("%w: bytes overflow", ErrFastMarchingTopologyLimit)
		}
		bytes += uint64(q.n) * q.b
	}
	if bytes > t.MaxBytes {
		return nil, &FastMarchingTopologyFailure{Cause: fmt.Errorf("%w: %d bytes > %d", ErrFastMarchingTopologyLimit, bytes, t.MaxBytes), Report: FastMarchingTopologyReport{Mode: t.Mode, LogicalBytes: bytes, StopReason: "budget"}}
	}
	if uint64(n)*3 > t.MaxWork || uint64(len(o.Targets)) > (t.MaxWork-uint64(n)*3)/2 || uint64(len(seeds)) > (t.MaxWork-uint64(n)*3-2*uint64(len(o.Targets)))/8192 {
		return nil, &FastMarchingTopologyFailure{Cause: fmt.Errorf("%w: initial work", ErrFastMarchingTopologyLimit), Report: FastMarchingTopologyReport{Mode: t.Mode, Dimension: func() int {
			if g.Nz == 1 {
				return 2
			}
			return 3
		}(), LogicalBytes: bytes, StopReason: "budget"}}
	}
	if e := ctx.Err(); e != nil {
		return nil, e
	}
	for i, target := range o.Targets {
		if i&1023 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		if target[0] < 0 || target[0] >= g.Nx || target[1] < 0 || target[1] >= g.Ny || target[2] < 0 || target[2] >= g.Nz {
			return nil, fmt.Errorf("%w: target", ErrFastMarchingInput)
		}
	}
	first := seeds[0].Value
	for i, s := range seeds {
		if i&1023 == 0 {
			if e := ctx.Err(); e != nil {
				return nil, e
			}
		}
		if s.Index[0] < 0 || s.Index[0] >= g.Nx || s.Index[1] < 0 || s.Index[1] >= g.Ny || s.Index[2] < 0 || s.Index[2] >= g.Nz || math.IsNaN(s.Value) || math.IsInf(s.Value, 0) || s.Value != first || (o.StoppingValue > 0 && s.Value > o.StoppingValue) {
			return nil, fmt.Errorf("%w: incompatible initial topology seed", ErrFastMarchingInput)
		}
		if !positiveFinite(g.Speed[(s.Index[2]*g.Ny+s.Index[1])*g.Nx+s.Index[0]]) {
			return nil, fmt.Errorf("%w: seed on unavailable mask node", ErrFastMarchingInput)
		}
	}
	c := &fmTopology{ctx: ctx, nx: g.Nx, ny: g.Ny, nz: g.Nz, dim: 3, parent: make([]int32, n), rank: make([]byte, n), limit: t.MaxWork, report: FastMarchingTopologyReport{Mode: t.Mode, LogicalBytes: bytes, Work: uint64(len(o.Targets))}}
	if g.Nz == 1 {
		c.dim = 2
	}
	c.report.Dimension = c.dim
	for i := range c.parent {
		if e := c.charge(1); e != nil {
			return nil, &FastMarchingTopologyFailure{Cause: e, Report: c.report}
		}
		c.parent[i] = -1
	}
	for _, s := range seeds {
		i := (s.Index[2]*g.Ny+s.Index[1])*g.Nx + s.Index[0]
		if c.parent[i] < 0 {
			c.parent[i] = int32(i)
			c.report.FinalComponents++
		}
	}
	for _, s := range seeds {
		if e := c.charge(8192); e != nil {
			return nil, &FastMarchingTopologyFailure{Cause: e, Report: c.report}
		}
		i := (s.Index[2]*g.Ny+s.Index[1])*g.Nx + s.Index[0]
		c.joinNeighbors(i)
	}
	c.report.InitialComponents = c.report.FinalComponents
	return c, nil
}
func (c *fmTopology) charge(n uint64) error {
	if e := c.ctx.Err(); e != nil {
		return e
	}
	if n > c.limit-c.report.Work {
		return fmt.Errorf("%w: work > %d", ErrFastMarchingTopologyLimit, c.limit)
	}
	c.report.Work += n
	return nil
}
func (c *fmTopology) at(x, y, z int) int {
	if x < 0 || x >= c.nx || y < 0 || y >= c.ny || z < 0 || z >= c.nz {
		return -1
	}
	return (z*c.ny+y)*c.nx + x
}
func (c *fmTopology) coords(i int) (int, int, int) {
	return i % c.nx, (i / c.nx) % c.ny, i / (c.nx * c.ny)
}
func (c *fmTopology) occupied(x, y, z int) bool {
	i := c.at(x, y, z)
	return i >= 0 && c.parent[i] >= 0
}
func (c *fmTopology) find(i int) int {
	root := i
	for c.parent[root] != int32(root) {
		root = int(c.parent[root])
	}
	for i != root {
		next := int(c.parent[i])
		c.parent[i] = int32(root)
		i = next
	}
	return root
}
func (c *fmTopology) union(a, b int) {
	a = c.find(a)
	b = c.find(b)
	if a == b {
		return
	}
	if c.rank[a] < c.rank[b] {
		a, b = b, a
	}
	c.parent[b] = int32(a)
	if c.rank[a] == c.rank[b] {
		c.rank[a]++
	}
	c.report.FinalComponents--
}
func (c *fmTopology) joinNeighbors(i int) {
	x, y, z := c.coords(i)
	lo, hi := -1, 1
	if c.dim == 2 {
		lo, hi = 0, 0
	}
	for dz := lo; dz <= hi; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				j := c.at(x+dx, y+dy, z+dz)
				if j >= 0 && c.parent[j] >= 0 {
					c.union(i, j)
				}
			}
		}
	}
}
func (c *fmTopology) admit(i int) (bool, error) {
	if e := c.charge(8192); e != nil {
		return false, e
	}
	if c.parent[i] >= 0 {
		return true, nil
	} // admitted initial seeds
	x, y, z := c.coords(i)
	var roots [26]int
	nr := 0
	lo, hi := -1, 1
	if c.dim == 2 {
		lo, hi = 0, 0
	}
	for dz := lo; dz <= hi; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				j := c.at(x+dx, y+dy, z+dz)
				if j < 0 || c.parent[j] < 0 {
					continue
				}
				r := c.find(j)
				seen := false
				for k := 0; k < nr; k++ {
					if roots[k] == r {
						seen = true
						break
					}
				}
				if !seen {
					roots[nr] = r
					nr++
				}
			}
		}
	}
	if c.report.Mode == FastMarchingTopologyStrict && nr != 1 {
		c.report.RejectedCandidates++
		c.report.RejectedMerge++
		return false, nil
	}
	// In 3D a connected nonempty local complement conservatively fixes beta2.
	// In 2D beta2 is identically zero; Euler plus the exact DSU component delta
	// already fixes beta1. A separate local complement test would falsely reject
	// a bridge between distinct components whose global background stays connected.
	if c.dim == 3 && c.backgroundComponents(x, y, z) != 1 {
		c.report.RejectedCandidates++
		c.report.RejectedBackground++
		return false, nil
	}
	// For a cubical complex chi=beta0-beta1+beta2. In 2D beta2=0; in 3D
	// the complement condition fixes beta2, and DSU gives delta beta0=1-nr, hence this exact local Euler delta
	// preserves beta1, allowing only distinct-component mergers in NoHandles.
	if nr == 0 || c.eulerDelta(x, y, z) != 1-nr {
		c.report.RejectedCandidates++
		c.report.RejectedHandle++
		return false, nil
	}
	c.parent[i] = int32(i)
	c.report.FinalComponents++
	c.joinNeighbors(i)
	return true, nil
}

// Each subcell of the new cube is already covered iff an incident neighboring
// cube is occupied. State 0/1 means fixed lower/upper coordinate; 2 is an interval.
func (c *fmTopology) eulerDelta(x, y, z int) int {
	delta := 0
	zmax := 2
	if c.dim == 2 {
		zmax = 0
	}
	for a := 0; a < 3; a++ {
		for b := 0; b < 3; b++ {
			for d := 0; d <= zmax; d++ {
				s := [3]int{a, b, d}
				degree := 0
				for axis := 0; axis < c.dim; axis++ {
					if s[axis] == 2 {
						degree++
					}
				}
				covered := false
				for bits := 0; bits < (1 << c.dim); bits++ {
					off := [3]int{}
					for axis := 0; axis < c.dim; axis++ {
						if s[axis] != 2 && bits&(1<<axis) != 0 {
							if s[axis] == 0 {
								off[axis] = -1
							} else {
								off[axis] = 1
							}
						}
					}
					if off == [3]int{} {
						continue
					}
					if c.occupied(x+off[0], y+off[1], z+off[2]) {
						covered = true
						break
					}
				}
				if !covered {
					if degree&1 == 0 {
						delta++
					} else {
						delta--
					}
				}
			}
		}
	}
	return delta
}
func (c *fmTopology) backgroundComponents(x, y, z int) int {
	var bg, seen [27]bool
	var queue [27]int
	lo, hi := -1, 1
	if c.dim == 2 {
		lo, hi = 0, 0
	}
	for dz := lo; dz <= hi; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				j := (dz+1)*9 + (dy+1)*3 + dx + 1
				bg[j] = (dx != 0 || dy != 0 || dz != 0) && !c.occupied(x+dx, y+dy, z+dz)
			}
		}
	}
	count := 0
	for start := 0; start < 27; start++ {
		if !bg[start] || seen[start] {
			continue
		}
		tail := 1
		queue[0] = start
		seen[start] = true
		touches := false
		for head := 0; head < tail; head++ {
			j := queue[head]
			qx, qy, qz := j%3, j/3%3, j/9
			dist := absFMTopology(qx-1) + absFMTopology(qy-1) + absFMTopology(qz-1)
			if dist == 1 {
				touches = true
			}
			for _, step := range [6][3]int{{-1, 0, 0}, {1, 0, 0}, {0, -1, 0}, {0, 1, 0}, {0, 0, -1}, {0, 0, 1}} {
				if c.dim == 2 && step[2] != 0 {
					continue
				}
				nx, ny, nz := qx+step[0], qy+step[1], qz+step[2]
				if nx < 0 || nx > 2 || ny < 0 || ny > 2 || nz < 0 || nz > 2 {
					continue
				}
				k := nz*9 + ny*3 + nx
				if bg[k] && !seen[k] {
					seen[k] = true
					queue[tail] = k
					tail++
				}
			}
		}
		if touches {
			count++
		}
	}
	return count
}
func absFMTopology(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
