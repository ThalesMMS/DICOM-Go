package render

// First-order fast marching (Sethian 1996) for the Eikonal equation
// |∇T| = 1/F on a regular, possibly anisotropic 3D grid, the upwind gradient
// of the arrival time and a subvoxel minimal path by gradient descent
// (#1100). The behaviour follows ITK's FastMarchingImageFilter,
// FastMarchingUpwindGradientImageFilter and SpeedFunctionToPathFilter (ITK
// 35b196e13c, Apache-2.0, Copyright NumFOCUS); this is an independent
// implementation. Differences: seeds are plain trial points with their own
// value, stopping can wait for target points, and the path descent falls back
// to a discrete steepest descent over frozen nodes instead of failing when
// the interpolated gradient stalls.

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// ErrFastMarchingInput reports an invalid grid, seed or option.
var ErrFastMarchingInput = errors.New("render: invalid fast marching input")

// MaxFastMarchingNodes bounds the grid size FastMarching accepts.
const MaxFastMarchingNodes = 64 << 20

// FastMarchingGrid is a regular grid of propagation speeds. Index (i, j, k)
// is stored at (k*Ny+j)*Nx+i; Spacing is the physical step along each axis.
// A speed that is not positive and finite blocks the node.
type FastMarchingGrid struct {
	Nx, Ny, Nz int
	Spacing    Vec3
	Speed      []float64
}

// FastMarchingSeed starts the front at a grid node with a known arrival time.
type FastMarchingSeed struct {
	Index [3]int
	Value float64
}

// FastMarchingOptions bounds the propagation. The zero value marches over
// every reachable node.
type FastMarchingOptions struct {
	// StoppingValue, when positive, stops the front once the next arrival
	// time exceeds it.
	StoppingValue float64
	// Targets, when set, stop the front once every reachable target is
	// frozen and the front has advanced two grid steps beyond the latest, so
	// the gradient around each target is defined.
	Targets [][3]int
	// MaxFrozen, when positive, bounds the number of frozen nodes; reaching
	// it is an error.
	MaxFrozen int
	// MaxTrial, when positive, bounds the number of nodes on the front at
	// once; exceeding it is an error.
	MaxTrial int
}

// FastMarchingResult holds the arrival times; unreached nodes are +Inf.
type FastMarchingResult struct {
	Nx, Ny, Nz int
	Spacing    Vec3
	Times      []float64
	Seeds      [][3]int
	// Frozen counts the nodes whose arrival time is final.
	Frozen int
	// TargetsReached reports whether every target was frozen.
	TargetsReached bool
}

// FastMarchingLimitError reports that MaxFrozen ("frozen") or MaxTrial
// ("trial") was exceeded.
type FastMarchingLimitError struct {
	Resource string
	Limit    int
}

func (e *FastMarchingLimitError) Error() string {
	return fmt.Sprintf("render: fast marching exceeded %d %s nodes", e.Limit, e.Resource)
}

const (
	fmFar byte = iota
	fmTrial
	fmFrozen
	fmTopologyRejected
	fmTopologySeedTrial
)

// FastMarching propagates the front from the seeds and returns the arrival
// time of every reached node.
func FastMarching(ctx context.Context, grid FastMarchingGrid, seeds []FastMarchingSeed, options FastMarchingOptions) (*FastMarchingResult, error) {
	return fastMarching(ctx, grid, seeds, options, false)
}

// FastMarchingFixedBuffers propagates the same front as FastMarching, with
// exact backing capacities for its grid arrays and returned seed indices.
// The trial heap reserves one int32 per grid node, and the returned Seeds
// reserves one index per supplied seed, including duplicates. Grid.Speed is
// read without copying. These array capacities are not a process memory quota.
func FastMarchingFixedBuffers(ctx context.Context, grid FastMarchingGrid, seeds []FastMarchingSeed, options FastMarchingOptions) (*FastMarchingResult, error) {
	return fastMarching(ctx, grid, seeds, options, true)
}

func fastMarching(ctx context.Context, grid FastMarchingGrid, seeds []FastMarchingSeed, options FastMarchingOptions, fixedBuffers bool) (*FastMarchingResult, error) {
	return fastMarchingControlled(ctx, grid, seeds, options, fixedBuffers, nil)
}

func fastMarchingControlled(ctx context.Context, grid FastMarchingGrid, seeds []FastMarchingSeed, options FastMarchingOptions, fixedBuffers bool, control *fmTopology) (*FastMarchingResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if grid.Nx <= 0 || grid.Ny <= 0 || grid.Nz <= 0 {
		return nil, fmt.Errorf("%w: grid size %dx%dx%d", ErrFastMarchingInput, grid.Nx, grid.Ny, grid.Nz)
	}
	count, ok := boundedCPRAssistantProduct(grid.Nx, grid.Ny, grid.Nz, MaxFastMarchingNodes)
	if !ok || int(count) != len(grid.Speed) {
		return nil, fmt.Errorf("%w: %d speeds for a %dx%dx%d grid", ErrFastMarchingInput, len(grid.Speed), grid.Nx, grid.Ny, grid.Nz)
	}
	if !positiveFinite(grid.Spacing.X) || !positiveFinite(grid.Spacing.Y) || !positiveFinite(grid.Spacing.Z) {
		return nil, fmt.Errorf("%w: spacing must be positive and finite", ErrFastMarchingInput)
	}
	if len(seeds) == 0 {
		return nil, fmt.Errorf("%w: no seed", ErrFastMarchingInput)
	}
	if options.StoppingValue < 0 || math.IsNaN(options.StoppingValue) || options.MaxFrozen < 0 || options.MaxTrial < 0 {
		return nil, fmt.Errorf("%w: invalid stopping criteria", ErrFastMarchingInput)
	}
	if fixedBuffers {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	m := &fastMarcher{
		nx: grid.Nx, ny: grid.Ny, nz: grid.Nz,
		h:     [3]float64{grid.Spacing.X, grid.Spacing.Y, grid.Spacing.Z},
		speed: grid.Speed,
		times: make([]float64, count),
		state: make([]byte, count),
	}
	m.queue = fmHeap{times: m.times, pos: make([]int32, count)}
	if fixedBuffers {
		// pos allows only one queued entry for each node, and frozen nodes
		// are excluded by updateNeighbours. A decrease updates the existing
		// entry in place, so append never grows this N-node backing array.
		m.queue.nodes = make([]int32, 0, count)
	}
	for i := range m.queue.pos {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m.queue.pos[i] = -1
	}
	for i := range m.times {
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		m.times[i] = math.Inf(1)
	}
	result := &FastMarchingResult{Nx: grid.Nx, Ny: grid.Ny, Nz: grid.Nz, Spacing: grid.Spacing, Times: m.times}
	if fixedBuffers {
		result.Seeds = make([][3]int, 0, len(seeds))
	}
	for _, seed := range seeds {
		if !m.inside(seed.Index) || math.IsNaN(seed.Value) || math.IsInf(seed.Value, 0) {
			return nil, fmt.Errorf("%w: seed %v", ErrFastMarchingInput, seed.Index)
		}
		index := m.index(seed.Index[0], seed.Index[1], seed.Index[2])
		if seed.Value < m.times[index] {
			m.times[index] = seed.Value
			m.state[index] = fmTrial
			if control != nil {
				m.state[index] = fmTopologySeedTrial
			}
			m.queue.update(index)
		}
		result.Seeds = append(result.Seeds, seed.Index)
	}
	targets := make([]int, 0, len(options.Targets))
	for _, target := range options.Targets {
		if control != nil {
			if err := control.charge(1); err != nil {
				return nil, err
			}
		}
		if !m.inside(target) {
			return nil, fmt.Errorf("%w: target %v", ErrFastMarchingInput, target)
		}
		targets = append(targets, m.index(target[0], target[1], target[2]))
	}
	if control != nil && options.MaxTrial > 0 && len(m.queue.nodes) > options.MaxTrial {
		return nil, &FastMarchingLimitError{Resource: "trial", Limit: options.MaxTrial}
	}
	pending := len(targets)
	pendingSeeds := 0
	if control != nil {
		pendingSeeds = len(m.queue.nodes)
	}
	stopAfter := math.Inf(1)
	for pops := 0; len(m.queue.nodes) > 0; pops++ {
		if pops&4095 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if next := m.times[m.queue.nodes[0]]; (options.StoppingValue > 0 && next > options.StoppingValue) || (next > stopAfter && pendingSeeds == 0) {
			if control != nil {
				if options.StoppingValue > 0 && next > options.StoppingValue {
					control.report.StopReason = "stopping-value"
				} else {
					control.report.StopReason = "targets"
				}
			}
			break
		}
		index := m.queue.pop()
		if m.state[index] == fmTopologySeedTrial {
			pendingSeeds--
		}
		if control != nil {
			allow, err := control.admit(index)
			if err != nil {
				return nil, err
			}
			if !allow {
				m.state[index] = fmTopologyRejected
				continue
			}
		}
		item := fmItem{index: index, time: m.times[index]}
		m.state[item.index] = fmFrozen
		result.Frozen++
		if control != nil {
			control.report.Frozen = result.Frozen
		}
		if options.MaxFrozen > 0 && result.Frozen > options.MaxFrozen {
			return nil, &FastMarchingLimitError{Resource: "frozen", Limit: options.MaxFrozen}
		}
		if pending > 0 {
			for _, target := range targets {
				if control != nil {
					if err := control.charge(1); err != nil {
						return nil, err
					}
				}
				if target == item.index {
					pending--
				}
			}
			if pending == 0 {
				stopAfter = item.time + 2*m.maxStep()/m.speedAt(item.index)
			}
		}
		m.updateNeighbours(item.index)
		if options.MaxTrial > 0 && len(m.queue.nodes) > options.MaxTrial {
			return nil, &FastMarchingLimitError{Resource: "trial", Limit: options.MaxTrial}
		}
	}
	result.TargetsReached = len(targets) > 0 && pending <= 0
	for _, target := range targets {
		if control != nil {
			if err := control.charge(1); err != nil {
				return nil, err
			}
		}
		if m.state[target] != fmFrozen {
			result.TargetsReached = false
		}
	}
	// Trial nodes keep tentative, larger-than-final times; mark them unreached
	// so the field only holds final values.
	for i, state := range m.state {
		if control != nil {
			if err := control.charge(1); err != nil {
				return nil, err
			}
		}
		if i%4096 == 0 && ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if state != fmFrozen {
			m.times[i] = math.Inf(1)
		}
	}
	if control != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	return result, nil
}

type fastMarcher struct {
	nx, ny, nz int
	h          [3]float64
	speed      []float64
	times      []float64
	state      []byte
	queue      fmHeap
}

func (m *fastMarcher) inside(index [3]int) bool {
	return index[0] >= 0 && index[1] >= 0 && index[2] >= 0 && index[0] < m.nx && index[1] < m.ny && index[2] < m.nz
}

func (m *fastMarcher) index(x, y, z int) int { return (z*m.ny+y)*m.nx + x }

func (m *fastMarcher) maxStep() float64 { return math.Max(m.h[0], math.Max(m.h[1], m.h[2])) }

func (m *fastMarcher) speedAt(index int) float64 {
	if speed := m.speed[index]; positiveFinite(speed) {
		return speed
	}
	return math.SmallestNonzeroFloat64
}

func (m *fastMarcher) updateNeighbours(index int) {
	x := index % m.nx
	y := (index / m.nx) % m.ny
	z := index / (m.nx * m.ny)
	for _, n := range [6][3]int{{x - 1, y, z}, {x + 1, y, z}, {x, y - 1, z}, {x, y + 1, z}, {x, y, z - 1}, {x, y, z + 1}} {
		if !m.inside(n) {
			continue
		}
		neighbour := m.index(n[0], n[1], n[2])
		// Constrained seeds are the declared initial foreground, with fixed
		// values. Preserve them even if an Eikonal update rounds below a seed.
		if m.state[neighbour] == fmFrozen || m.state[neighbour] == fmTopologyRejected || m.state[neighbour] == fmTopologySeedTrial || !positiveFinite(m.speed[neighbour]) {
			continue
		}
		if value := m.solve(n[0], n[1], n[2], neighbour); value < m.times[neighbour] {
			m.times[neighbour] = value
			m.state[neighbour] = fmTrial
			m.queue.update(neighbour)
		}
	}
}

// solve returns the first-order upwind solution at a node from its frozen
// neighbours: Σ ((T - aᵢ)/hᵢ)² = 1/F² over the axes whose smallest frozen
// neighbour aᵢ is below T.
func (m *fastMarcher) solve(x, y, z, index int) float64 {
	var a, h [3]float64
	n := 0
	coords := [3]int{x, y, z}
	sizes := [3]int{m.nx, m.ny, m.nz}
	for axis := 0; axis < 3; axis++ {
		best := math.Inf(1)
		for _, delta := range [2]int{-1, 1} {
			c := coords
			c[axis] += delta
			if c[axis] < 0 || c[axis] >= sizes[axis] {
				continue
			}
			neighbour := m.index(c[0], c[1], c[2])
			if m.state[neighbour] == fmFrozen && m.times[neighbour] < best {
				best = m.times[neighbour]
			}
		}
		if !math.IsInf(best, 1) {
			a[n], h[n] = best, m.h[axis]
			n++
		}
	}
	// Insertion sort of at most three values by arrival time.
	for i := 1; i < n; i++ {
		for j := i; j > 0 && a[j] < a[j-1]; j-- {
			a[j], a[j-1] = a[j-1], a[j]
			h[j], h[j-1] = h[j-1], h[j]
		}
	}
	rhs := 1 / (m.speed[index] * m.speed[index])
	var sumW, sumWA, sumWA2 float64
	solution := math.Inf(1)
	for k := 0; k < n; k++ {
		if k > 0 && solution <= a[k] {
			break
		}
		w := 1 / (h[k] * h[k])
		sumW += w
		sumWA += w * a[k]
		sumWA2 += w * a[k] * a[k]
		// sumW·T² - 2·sumWA·T + sumWA2 - rhs = 0
		disc := sumWA*sumWA - sumW*(sumWA2-rhs)
		if disc < 0 {
			break
		}
		solution = (sumWA + math.Sqrt(disc)) / sumW
	}
	return solution
}

// Gradient returns the upwind gradient of the arrival time at a frozen node,
// in time per physical unit: along each axis, the one-sided difference to the
// smaller frozen neighbour when it is below the node. Its norm is 1/F.
func (r *FastMarchingResult) Gradient(index [3]int) (Vec3, bool) {
	if index[0] < 0 || index[1] < 0 || index[2] < 0 || index[0] >= r.Nx || index[1] >= r.Ny || index[2] >= r.Nz {
		return Vec3{}, false
	}
	at := r.Times[(index[2]*r.Ny+index[1])*r.Nx+index[0]]
	if math.IsInf(at, 1) {
		return Vec3{}, false
	}
	sizes := [3]int{r.Nx, r.Ny, r.Nz}
	h := [3]float64{r.Spacing.X, r.Spacing.Y, r.Spacing.Z}
	var g [3]float64
	for axis := 0; axis < 3; axis++ {
		best, sign := at, 0.0
		for _, delta := range [2]int{-1, 1} {
			c := index
			c[axis] += delta
			if c[axis] < 0 || c[axis] >= sizes[axis] {
				continue
			}
			if value := r.Times[(c[2]*r.Ny+c[1])*r.Nx+c[0]]; value < best {
				best, sign = value, float64(-delta)
			}
		}
		if sign != 0 {
			g[axis] = sign * (at - best) / h[axis]
		}
	}
	return Vec3{X: g[0], Y: g[1], Z: g[2]}, true
}

// timeAt returns the arrival time at a node, +Inf outside the grid.
func (r *FastMarchingResult) timeAt(x, y, z int) float64 {
	if x < 0 || y < 0 || z < 0 || x >= r.Nx || y >= r.Ny || z >= r.Nz {
		return math.Inf(1)
	}
	return r.Times[(z*r.Ny+y)*r.Nx+x]
}

// interpolate trilinearly blends the upwind gradient and the arrival time of
// the reached corners of the cell holding p (continuous grid index).
func (r *FastMarchingResult) interpolate(p Vec3) (Vec3, float64, bool) {
	base := [3]int{}
	frac := [3]float64{}
	sizes := [3]int{r.Nx, r.Ny, r.Nz}
	for axis, value := range [3]float64{p.X, p.Y, p.Z} {
		if sizes[axis] == 1 {
			continue
		}
		b := clampInt(int(math.Floor(value)), 0, sizes[axis]-2)
		base[axis] = b
		frac[axis] = math.Min(1, math.Max(0, value-float64(b)))
	}
	var gradient Vec3
	var time, weights float64
	for corner := 0; corner < 8; corner++ {
		c := base
		w := 1.0
		for axis := 0; axis < 3; axis++ {
			if corner>>axis&1 == 1 {
				if sizes[axis] == 1 {
					w = 0
					break
				}
				c[axis]++
				w *= frac[axis]
			} else {
				w *= 1 - frac[axis]
			}
		}
		if w == 0 {
			continue
		}
		g, ok := r.Gradient(c)
		if !ok {
			continue
		}
		gradient = gradient.Add(g.Scale(w))
		time += w * r.timeAt(c[0], c[1], c[2])
		weights += w
	}
	if weights < 1e-9 {
		return Vec3{}, 0, false
	}
	return gradient.Scale(1 / weights), time / weights, true
}

// MinimalPath descends the arrival time from `from` (a continuous grid index)
// to the nearest seed in physical steps of stepSize, returning continuous grid
// indices from `from` to that seed. The descent follows the interpolated
// upwind gradient; when it stalls it finishes by a discrete steepest descent
// over frozen nodes, which always reaches a seed.
func (r *FastMarchingResult) MinimalPath(ctx context.Context, from Vec3, stepSize float64) ([]Vec3, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if !finiteVec3(from) || from.X < 0 || from.Y < 0 || from.Z < 0 ||
		from.X > float64(r.Nx-1) || from.Y > float64(r.Ny-1) || from.Z > float64(r.Nz-1) {
		return nil, fmt.Errorf("%w: path start %+v is outside the grid", ErrFastMarchingInput, from)
	}
	minStep := math.Min(r.Spacing.X, math.Min(r.Spacing.Y, r.Spacing.Z))
	if stepSize <= 0 || math.IsNaN(stepSize) || math.IsInf(stepSize, 0) {
		stepSize = minStep / 2
	}
	if len(r.Seeds) == 0 {
		return nil, fmt.Errorf("%w: no seed", ErrFastMarchingInput)
	}
	physical := func(p Vec3) Vec3 { return Vec3{X: p.X * r.Spacing.X, Y: p.Y * r.Spacing.Y, Z: p.Z * r.Spacing.Z} }
	seedDistance := func(p Vec3) (float64, Vec3) {
		best, at := math.Inf(1), Vec3{}
		for _, seed := range r.Seeds {
			s := Vec3{X: float64(seed[0]), Y: float64(seed[1]), Z: float64(seed[2])}
			if d := physical(p.Sub(s)).Length(); d < best {
				best, at = d, s
			}
		}
		return best, at
	}
	path := []Vec3{from}
	p := from
	extent := physical(Vec3{X: float64(r.Nx), Y: float64(r.Ny), Z: float64(r.Nz)}).Length()
	maxSteps := int(math.Min(4*extent/stepSize+64, 4_000_000))
	bestTime, sinceBest := math.Inf(1), 0
	for step := 0; step < maxSteps; step++ {
		if step&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if d, seed := seedDistance(p); d <= 1.5*stepSize {
			if d > 0 {
				path = append(path, seed)
			}
			return path, nil
		}
		gradient, time, ok := r.interpolate(p)
		norm := gradient.Length()
		if !ok || !(norm > 0) || math.IsInf(norm, 0) {
			break
		}
		if time < bestTime-1e-12 {
			bestTime, sinceBest = time, 0
		} else if sinceBest++; sinceBest > 32 {
			break
		}
		// Move stepSize (physical) against the gradient; convert the physical
		// displacement back to index units per axis.
		direction := gradient.Scale(1 / norm)
		next := Vec3{
			X: p.X - direction.X*stepSize/r.Spacing.X,
			Y: p.Y - direction.Y*stepSize/r.Spacing.Y,
			Z: p.Z - direction.Z*stepSize/r.Spacing.Z,
		}
		next.X = math.Min(float64(r.Nx-1), math.Max(0, next.X))
		next.Y = math.Min(float64(r.Ny-1), math.Max(0, next.Y))
		next.Z = math.Min(float64(r.Nz-1), math.Max(0, next.Z))
		p = next
		path = append(path, p)
	}
	return r.finishDiscrete(ctx, path)
}

// finishDiscrete continues from the node nearest the last point by moving to
// the 26-neighbour with the smallest arrival time until a seed is reached.
func (r *FastMarchingResult) finishDiscrete(ctx context.Context, path []Vec3) ([]Vec3, error) {
	last := path[len(path)-1]
	x := clampInt(NearestIndex(last.X), 0, r.Nx-1)
	y := clampInt(NearestIndex(last.Y), 0, r.Ny-1)
	z := clampInt(NearestIndex(last.Z), 0, r.Nz-1)
	if math.IsInf(r.timeAt(x, y, z), 1) {
		// Rounding can land on an unreached node; use the reached corner with
		// the smallest time instead.
		best := math.Inf(1)
		for dz := 0; dz <= 1; dz++ {
			for dy := 0; dy <= 1; dy++ {
				for dx := 0; dx <= 1; dx++ {
					cx, cy, cz := int(math.Floor(last.X))+dx, int(math.Floor(last.Y))+dy, int(math.Floor(last.Z))+dz
					if value := r.timeAt(cx, cy, cz); value < best {
						best, x, y, z = value, cx, cy, cz
					}
				}
			}
		}
		if math.IsInf(best, 1) {
			return nil, fmt.Errorf("%w: the path start was not reached by the front", ErrFastMarchingInput)
		}
	}
	isSeed := func(x, y, z int) bool {
		for _, seed := range r.Seeds {
			if seed == [3]int{x, y, z} {
				return true
			}
		}
		return false
	}
	for steps := 0; ; steps++ {
		if steps&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		node := Vec3{X: float64(x), Y: float64(y), Z: float64(z)}
		if path[len(path)-1] != node {
			path = append(path, node)
		}
		if isSeed(x, y, z) {
			return path, nil
		}
		current := r.timeAt(x, y, z)
		bx, by, bz, best := x, y, z, current
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if value := r.timeAt(x+dx, y+dy, z+dz); value < best {
						bx, by, bz, best = x+dx, y+dy, z+dz, value
					}
				}
			}
		}
		if best >= current {
			// A local minimum that is not a seed: another seed's basin.
			return path, nil
		}
		x, y, z = bx, by, bz
	}
}

type fmItem struct {
	index int
	time  float64
}

// fmHeap is a binary min-heap of trial nodes keyed by their arrival time,
// with a position index so a lowered time moves the node up in place: the
// queue never holds more entries than nodes.
type fmHeap struct {
	nodes []int32
	pos   []int32
	times []float64
}

func (h *fmHeap) update(node int) {
	at := h.pos[node]
	if at < 0 {
		at = int32(len(h.nodes))
		h.nodes = append(h.nodes, int32(node))
		h.pos[node] = at
	}
	h.up(int(at))
}

func (h *fmHeap) pop() int {
	top := int(h.nodes[0])
	last := len(h.nodes) - 1
	h.swap(0, last)
	h.nodes = h.nodes[:last]
	h.pos[top] = -1
	if last > 0 {
		h.down(0)
	}
	return top
}

func (h *fmHeap) less(i, j int) bool { return h.times[h.nodes[i]] < h.times[h.nodes[j]] }

func (h *fmHeap) swap(i, j int) {
	h.nodes[i], h.nodes[j] = h.nodes[j], h.nodes[i]
	h.pos[h.nodes[i]] = int32(i)
	h.pos[h.nodes[j]] = int32(j)
}

func (h *fmHeap) up(i int) {
	for i > 0 {
		parent := (i - 1) / 2
		if !h.less(i, parent) {
			return
		}
		h.swap(i, parent)
		i = parent
	}
}

func (h *fmHeap) down(i int) {
	n := len(h.nodes)
	for {
		smallest := i
		for _, child := range [2]int{2*i + 1, 2*i + 2} {
			if child < n && h.less(child, smallest) {
				smallest = child
			}
		}
		if smallest == i {
			return
		}
		h.swap(i, smallest)
		i = smallest
	}
}
