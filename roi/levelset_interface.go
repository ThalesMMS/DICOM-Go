package roi

import (
	"errors"
	"fmt"
	"math"
)

// ShapeEvolutionProfile selects a numerical schedule, not a fallback.
type ShapeEvolutionProfile uint8

const (
	// DenseBand preserves the existing Shape solver and defaults.
	DenseBand ShapeEvolutionProfile = iota
	// InterfaceLocal updates the nearest supported endpoints of crossing faces.
	InterfaceLocal
)

// ShapeSurfaceSampling selects where the supplied potential is sampled.
type ShapeSurfaceSampling uint8

const (
	GridCentres ShapeSurfaceSampling = iota
	InterpolatedInterface
)

// ShapeInitializationProfile selects the initial interface seed metric.
// Layer transfer and PDE derivatives retain their existing contracts.
type ShapeInitializationProfile uint8

const (
	// CityBlockSeeds preserves the minimum-spacing-scaled axial seed gauge.
	CityBlockSeeds ShapeInitializationProfile = iota
	// BoundedFaceNormalSeeds combines local physical face intersections,
	// capped by the CityBlockSeeds magnitude. It is not a global Euclidean
	// distance transform or a declaration of ITK numerical parity.
	BoundedFaceNormalSeeds
)

// ErrShapeInterfaceDomain reports holes/nonfinite values in the dense profile.
var ErrShapeInterfaceDomain = errors.New("roi: InterfaceLocal Shape requires a finite dense domain")

func shapeProfileOptions(p ShapeDetectionParams) error {
	if p.Evolution > InterfaceLocal || p.SurfaceSampling > InterpolatedInterface || p.Initialization > BoundedFaceNormalSeeds || p.Evolution == DenseBand && (p.SurfaceSampling != GridCentres || p.Initialization != CityBlockSeeds) {
		return fmt.Errorf("%w: Shape evolution/sampling", ErrLevelSetInvalidInput)
	}
	if p.Evolution == InterfaceLocal && p.Input != EdgePotentialImage {
		return fmt.Errorf("%w: InterfaceLocal requires supplied edge potential", ErrLevelSetInvalidInput)
	}
	return nil
}

// interfaceActive visits the sorted band once. The nearest endpoint of every
// sign-crossing face belongs to the updated set; exact ties include both.
// Only endpoints supporting the centered derivative footprint are updated.
// Boundary embedding values may still change through layer transfer.
// Each index occurs once in ascending order.
func (s *levelSetState) interfaceActive(out []int32) ([]int32, error) {
	out = out[:0]
	for k, index := range s.band {
		if k%4096 == 0 {
			if err := levelSetContextError(s.ctx); err != nil {
				return nil, err
			}
		}
		i := int(index)
		pos := s.coordsArray(i)
		supported := true
		for a := 0; a < s.dims; a++ {
			if pos[a] == 0 || pos[a] == s.size[a]-1 {
				supported = false
				break
			}
		}
		if !supported {
			continue
		}
		inside := s.phi[i] < 0
		for a := 0; a < s.dims; a++ {
			found := false
			for _, d := range [2]int{-1, 1} {
				if pos[a]+d < 0 || pos[a]+d >= s.size[a] {
					continue
				}
				j := i + d*s.stride[a]
				if (s.phi[j] < 0) != inside && abs32(s.phi[i]) <= abs32(s.phi[j]) {
					found = true
					break
				}
			}
			if found {
				out = append(out, index)
				break
			}
		}
	}
	return out, levelSetContextError(s.ctx)
}

// interfaceCrossing distinguishes domain-limited active exhaustion from the
// disappearance of the zero surface. It never substitutes a farther anchor.
func (s *levelSetState) interfaceCrossing() (bool, error) {
	visits := 0
	for i := 0; i < s.n; i++ {
		pos := s.coordsArray(i)
		for a := 0; a < s.dims; a++ {
			for _, direction := range [2]int{-1, 1} {
				if visits%4096 == 0 {
					if err := levelSetContextError(s.ctx); err != nil {
						return false, err
					}
				}
				visits++
				if pos[a]+direction < 0 || pos[a]+direction >= s.size[a] {
					continue
				}
				j := i + direction*s.stride[a]
				if (s.phi[i] < 0) != (s.phi[j] < 0) {
					return true, nil
				}
			}
		}
	}
	return false, levelSetContextError(s.ctx)
}

// initialInterfaceActive retains all equally nearest crossing endpoints.
// Exact ties belong to both physical samples, independent of storage order or
// foreground phase. Both half-layer endpoints are valid initially; subsequent
// promotion uses the fixed half-open phase.
func (s *levelSetState) initialInterfaceActive(out []int32) ([]int32, error) {
	out, e := s.interfaceActive(out)
	if e != nil {
		return nil, e
	}
	half := float32(s.hmin / 2)
	selected := out[:0]
	for k, index := range out {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		p := s.phi[index]
		if p >= -half && p <= half {
			selected = append(selected, index)
		}
	}
	return selected, levelSetContextError(s.ctx)
}

// pruneInterfaceAnchors removes numerical members that no longer touch any
// crossing. A partial reduction requires one rebuild from the final anchors.
// Capturing current phase makes that rebuild sign-preserving, so support cannot
// change again and no iterative stabilization is needed.
func (s *levelSetState) pruneInterfaceAnchors(active []int32, saved []float32, member []byte) ([]int32, error) {
	selected := active[:0]
	visits := 0
	for k, index := range active {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		i := int(index)
		pos, inside := s.coordsArray(i), s.phi[i] < 0
		crossing := false
		for axis := 0; axis < s.dims && !crossing; axis++ {
			for _, direction := range [2]int{-1, 1} {
				if visits%4096 == 0 {
					if e := levelSetContextError(s.ctx); e != nil {
						return nil, e
					}
				}
				visits++
				if pos[axis]+direction >= 0 && pos[axis]+direction < s.size[axis] && (s.phi[i+direction*s.stride[axis]] < 0) != inside {
					crossing = true
					break
				}
			}
		}
		if crossing {
			selected = append(selected, index)
		}
	}
	if len(selected) == len(active) || len(selected) == 0 {
		return selected, levelSetContextError(s.ctx)
	}
	for i, p := range s.phi {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		saved[i] = p
	}
	// Preserve the previous-step derivative readiness while using current
	// phase for the extra stored-field projection. Neither policy is reset.
	if e := s.refreshInterfaceLayersReadiness(selected, saved, member, saved, true); e != nil {
		return nil, e
	}
	return selected, levelSetContextError(s.ctx)
}

func (s *levelSetState) coordsArray(i int) [3]int { x, y, z := s.coords(i); return [3]int{x, y, z} }

// initializeInterfaceAnchors uses a minimum-spacing-scaled city-block
// embedding. The derivatives in ratePotential still use physical spacing.
func (s *levelSetState) initializeInterfaceAnchors() error {
	return s.initializeInterfaceAnchorsWithProfile(CityBlockSeeds)
}

func (s *levelSetState) initializeInterfaceAnchorsWithProfile(profile ShapeInitializationProfile) error {
	if profile > BoundedFaceNormalSeeds {
		return fmt.Errorf("%w: interface seed profile", ErrLevelSetInvalidInput)
	}
	if err := levelSetContextError(s.ctx); err != nil {
		return err
	}
	type seed struct {
		i int32
		d float32
	}
	seeds := make([]seed, 0, s.n)
	visits := 0
	for i, value := range s.phi {
		pos := s.coordsArray(i)
		best := math.Inf(1)
		faceDistances := [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
		for a := 0; a < s.dims; a++ {
			for _, direction := range [2]int{-1, 1} {
				if visits%4096 == 0 {
					if err := levelSetContextError(s.ctx); err != nil {
						return err
					}
				}
				visits++
				if pos[a]+direction < 0 || pos[a]+direction >= s.size[a] {
					continue
				}
				other := s.phi[i+direction*s.stride[a]]
				if (value < 0) == (other < 0) {
					continue
				}
				distance := 0.0
				if total := math.Abs(float64(value)) + math.Abs(float64(other)); total > 0 {
					distance = s.hmin * math.Abs(float64(value)) / total
					if profile == BoundedFaceNormalSeeds {
						physical := s.h[a] * (math.Abs(float64(value)) / total)
						faceDistances[a] = math.Min(faceDistances[a], physical)
					}
				}
				best = math.Min(best, distance)
			}
		}
		if profile == BoundedFaceNormalSeeds && best > 0 && !math.IsInf(best, 1) {
			nearest := math.Inf(1)
			for a := 0; a < s.dims; a++ {
				nearest = math.Min(nearest, faceDistances[a])
			}
			// Scale the inverse-distance norm by its smallest face distance,
			// avoiding reciprocal overflow for very small finite distances.
			if nearest == 0 {
				best = 0
			} else if !math.IsInf(nearest, 1) {
				normal := 0.0
				for a := 0; a < s.dims; a++ {
					if !math.IsInf(faceDistances[a], 1) {
						normal = math.Hypot(normal, nearest/faceDistances[a])
					}
				}
				best = math.Min(best, nearest/normal)
			}
		}
		if !math.IsInf(best, 1) {
			seeds = append(seeds, seed{int32(i), float32(best)})
		}
	}
	for i, p := range s.phi {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		s.phi[i] = s.width
		if p < 0 {
			s.phi[i] = -s.width
		}
	}
	band := s.band[:0]
	for k, seed := range seeds {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		i := seed.i
		p := seed.d
		if s.phi[i] < 0 {
			p = -p
		}
		s.phi[i] = p
		band = append(band, i)
	}
	s.band = band
	return levelSetContextError(s.ctx)
}

// interfacePromote preserves an explicit half-open active range. Promotion
// values come from signed anchors, so the two sides complement one layer.
func (s *levelSetState) interfacePromote(active, next []int32, old, staged []float32, member []byte) ([]int32, error) {
	half, c := float32(s.hmin/2), float32(s.hmin)
	for i := range member {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		member[i] = 0
	}
	for k, index := range active {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		p := s.phi[index]
		if p > -half && p <= half {
			member[index] = 1
		} else {
			member[index] = 3
		}
		if p < -half-c || p > half+c {
			return nil, fmt.Errorf("%w: interface update jumps beyond one layer", ErrLevelSetInvalidInput)
		}
	}
	visits := 0
	for k, index := range active {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		if member[index] != 3 {
			continue
		}
		i := int(index)
		pos := s.coordsArray(i)
		p := s.phi[i]
		for a := 0; a < s.dims; a++ {
			for _, direction := range [2]int{-1, 1} {
				if visits%4096 == 0 {
					if e := levelSetContextError(s.ctx); e != nil {
						return nil, e
					}
				}
				visits++
				if pos[a]+direction < 0 || pos[a]+direction >= s.size[a] {
					continue
				}
				j := i + direction*s.stride[a]
				if member[j] == 1 || member[j] == 3 {
					continue
				}
				target := s.coordsArray(j)
				supported := true
				for axis := 0; axis < s.dims; axis++ {
					if target[axis] == 0 || target[axis] == s.size[axis]-1 {
						supported = false
						break
					}
				}
				if !supported {
					continue
				}
				candidate := float32(0)
				if p <= -half && old[j] >= 0 {
					candidate = p + c
				} else if p > half && old[j] < 0 {
					candidate = p - c
				} else {
					continue
				}
				if candidate <= -half || candidate > half {
					return nil, fmt.Errorf("%w: interface promotion exceeds range", ErrLevelSetInvalidInput)
				}
				// Preserve the signed zero-envelope of every arriving front.
				// Minimum magnitude could discard a candidate already across
				// zero; classify expansion/contraction from the old snapshot.
				if member[j] != 2 || old[j] >= 0 && candidate < staged[j] || old[j] < 0 && candidate > staged[j] {
					member[j] = 2
					staged[j] = candidate
				}
			}
		}
	}
	next = next[:0]
	for i, m := range member {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return nil, e
			}
		}
		if m == 1 || m == 2 {
			if m == 2 {
				s.phi[i] = staged[i]
			}
			next = append(next, int32(i))
		}
	}
	return next, levelSetContextError(s.ctx)
}

// refreshInterfaceLayers uses one signed graph per side and one shared heap.
// The boundary projection only copies interior values with compatible zero
// normal change; unsupported outflow is exposed as domain exhaustion.
func (s *levelSetState) refreshInterfaceLayers(active []int32, saved []float32, member []byte) error {
	return s.refreshInterfaceLayersPrevious(active, saved, member, nil)
}

// previous can alias saved: every phase-compatibility read completes before
// anchors or the rebuilt field overwrite that buffer. A boundary-normal
// crossing already present in previous must not disappear through projection.
func (s *levelSetState) refreshInterfaceLayersPrevious(active []int32, saved []float32, member []byte, previous []float32) error {
	return s.refreshInterfaceLayersReadiness(active, saved, member, previous, false)
}

func (s *levelSetState) refreshInterfaceLayersReadiness(active []int32, saved []float32, member []byte, previous []float32, preserveReadiness bool) error {
	if e := levelSetContextError(s.ctx); e != nil {
		return e
	}
	// Without anchors no distance transform or boundary derivative can be
	// inferred. Keep the last finite field and report explicit exhaustion.
	if len(active) == 0 {
		return nil
	}
	for i := range member {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		priorReadiness := member[i] & 4
		member[i] = 0
		if preserveReadiness {
			member[i] = priorReadiness << 1
		}
		if s.phi[i] < 0 {
			member[i] |= 2
		}
		if previous != nil {
			position := s.coordsArray(i)
			projected := position
			boundary := false
			for axis := 0; axis < s.dims; axis++ {
				if position[axis] == 0 {
					projected[axis], boundary = 1, true
				} else if position[axis] == s.size[axis]-1 {
					projected[axis], boundary = s.size[axis]-2, true
				}
			}
			if boundary {
				j := s.index(projected[0], projected[1], projected[2])
				if (previous[i] < 0) == (previous[j] < 0) && (!preserveReadiness || priorReadiness != 0) {
					member[i] |= 4
				}
			}
		}
		s.status[i] = lsFar
	}
	for k, index := range active {
		if k%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		member[index] |= 1
		saved[index] = s.phi[index]
		s.status[index] = lsFrozen
	}
	for i, m := range member {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		if m&1 == 0 {
			s.phi[i] = s.width
			if m&2 != 0 {
				s.phi[i] = -s.width
			}
		}
	}
	s.heap.items = s.heap.items[:0]
	visits := 0
	neighbours := func(i int, visit func(int)) error {
		pos := s.coordsArray(i)
		for a := 0; a < s.dims; a++ {
			for _, direction := range [2]int{-1, 1} {
				if visits%4096 == 0 {
					if e := levelSetContextError(s.ctx); e != nil {
						return e
					}
				}
				visits++
				if pos[a]+direction >= 0 && pos[a]+direction < s.size[a] {
					visit(i + direction*s.stride[a])
				}
			}
		}
		return nil
	}
	relax := func(j int, value float32) {
		if s.status[j] == lsFrozen || abs32(value) >= abs32(s.phi[j]) {
			return
		}
		s.phi[j] = value
		s.status[j] = lsTrial
		s.heap.push(levelSetHeapItem{d: abs32(value), i: int32(j)})
	}
	c := float32(s.hmin)
	for _, index := range active {
		p := s.phi[index]
		if e := neighbours(int(index), func(j int) {
			v := p + c
			if member[j]&2 != 0 {
				v = p - c
			}
			relax(j, v)
		}); e != nil {
			return e
		}
	}
	for pops := 0; len(s.heap.items) > 0; pops++ {
		if pops%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		item := s.heap.pop()
		i := int(item.i)
		if s.status[i] == lsFrozen || item.d != abs32(s.phi[i]) {
			continue
		}
		if item.d >= s.width {
			break
		}
		s.status[i] = lsFrozen
		value := s.phi[i] + c
		if member[i]&2 != 0 {
			value = s.phi[i] - c
		}
		if e := neighbours(i, func(j int) {
			if member[j]&2 == member[i]&2 {
				relax(j, value)
			}
		}); e != nil {
			return e
		}
	}
	if s.heap.overflow {
		return fmt.Errorf("%w: interface layer heap", ErrLevelSetResourceLimit)
	}
	s.heap.items = s.heap.items[:0]
	allowed := member
	if previous == nil {
		allowed = nil
	}
	if e := s.projectInterfaceBoundaryCompatible(saved, allowed); e != nil {
		return e
	}
	band := s.band[:0]
	for i, p := range s.phi {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		if abs32(p) < s.width {
			band = append(band, int32(i))
		}
		if preserveReadiness {
			member[i] = member[i]&^12 | (member[i]&8)>>1
		}
	}
	s.band = band
	return levelSetContextError(s.ctx)
}

// projectInterfaceBoundary applies the explicit zero-normal compatibility rule.
func (s *levelSetState) projectInterfaceBoundary(saved []float32) error {
	return s.projectInterfaceBoundaryCompatible(saved, nil)
}

func (s *levelSetState) projectInterfaceBoundaryCompatible(saved []float32, allowed []byte) error {
	// Reuse the admitted old-field buffer after promotion. Size-three axes
	// may compare against a boundary neighbour; all comparisons must see
	// the same field, independent of projection iteration order.
	for i, p := range s.phi {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		saved[i] = p
	}
	for i := 0; i < s.n; i++ {
		if i%4096 == 0 {
			if e := levelSetContextError(s.ctx); e != nil {
				return e
			}
		}
		pos := s.coordsArray(i)
		projected := pos
		directions := [3]int{}
		boundary := false
		for a := 0; a < s.dims; a++ {
			if pos[a] == 0 {
				projected[a] = 1
				directions[a] = 1
				boundary = true
			} else if pos[a] == s.size[a]-1 {
				projected[a] = s.size[a] - 2
				directions[a] = -1
				boundary = true
			}
		}
		if !boundary || allowed != nil && allowed[i]&4 == 0 {
			continue
		}
		j := s.index(projected[0], projected[1], projected[2])
		// Saturated far-field values carry no derivative information.
		compatible := abs32(saved[j]) < s.width
		for a := 0; a < s.dims; a++ {
			if directions[a] != 0 && saved[j] != saved[j+directions[a]*s.stride[a]] {
				compatible = false
				break
			}
		}
		if compatible {
			s.phi[i] = saved[j]
		}
	}
	return levelSetContextError(s.ctx)
}

// interfaceTimeStep uses the public canonical constraint 1/(2D) globally.
// first is speed (mm/time) and curvature is the normalized tangential
// diffusivity (mm²/time); h is the smallest physical spacing in mm.
// The Godunov derivative coefficient sum is bounded by sqrt(2D)*first/h.
// A conservative principal diffusion bound is 2D*curvature/h². The combined
// guard bounds their explicit-step sum by one, including local extrema.
// This schedule applies only to InterfaceLocal; levelSetTimeStep is unchanged.
func interfaceTimeStep(first, curvature, h float64, dimensions int) float64 {
	if first == 0 && curvature == 0 {
		return 0
	}
	constraint := 1 / (2 * float64(dimensions))
	dt := math.Inf(1)
	if first > 0 {
		dt = constraint * h / first
	}
	if curvature > 0 {
		dt = math.Min(dt, constraint*h*h/curvature)
	}
	combined := math.Sqrt(2*float64(dimensions))*first/h + 2*float64(dimensions)*curvature/(h*h)
	if combined > 0 {
		dt = math.Min(dt, 1/combined)
	}
	return dt
}

// interfacePotential reconstructs the zero crossing along the physical
// central gradient and samples g multilinearly. Zero gradients sample the
// centre. Coordinates clamp to the domain; no padding/frozen layer is added.
func (s *levelSetState) interfacePotential(i int, field []float32) float32 {
	return s.interfacePotentialBoundary(i, field, nil)
}

// interfaceStencilValue makes the derivative extension independent of the
// stored graph's exact-equality projection branch. The prior phase readiness
// preserves actual samples where an unsupported normal crossing already exists.
func (s *levelSetState) interfaceStencilValue(i int, allowed []byte) float32 {
	if allowed[i]&4 == 0 {
		return s.phi[i]
	}
	pos := s.coordsArray(i)
	for axis := 0; axis < s.dims; axis++ {
		pos[axis] = max(1, min(pos[axis], s.size[axis]-2))
	}
	j := s.index(pos[0], pos[1], pos[2])
	// Rates and sampled gradients all read the same complete current field,
	// before any PDE update. Preserve a normal crossing born in the last
	// step as well as one already present in the previous field.
	if (s.phi[i] < 0) != (s.phi[j] < 0) {
		return s.phi[i]
	}
	return s.phi[j]
}

func (s *levelSetState) interfacePotentialBoundary(i int, field []float32, allowed []byte) float32 {
	pos := s.coordsArray(i)
	var gradient [3]float64
	norm := 0.0
	for a := 0; a < s.dims; a++ {
		lo, hi := s.neighbour(i, pos, a, -1), s.neighbour(i, pos, a, 1)
		gradient[a] = float64(s.phi[hi]-s.phi[lo]) / (2 * s.h[a])
		if allowed != nil {
			gradient[a] = float64(s.interfaceStencilValue(hi, allowed)-s.interfaceStencilValue(lo, allowed)) / (2 * s.h[a])
		}
		norm += gradient[a] * gradient[a]
	}
	if norm <= 1e-12 {
		return field[i]
	}
	var lo [3]int
	var fraction [3]float64
	for a := 0; a < s.dims; a++ {
		c := math.Max(0, math.Min(float64(s.size[a]-1), float64(pos[a])-float64(s.phi[i])*gradient[a]/(norm*s.h[a])))
		lo[a] = int(math.Floor(c))
		fraction[a] = c - float64(lo[a])
	}
	value := 0.0
	for corner := 0; corner < 1<<s.dims; corner++ {
		weight, j := 1.0, 0
		for a := 0; a < 3; a++ {
			p := lo[a]
			if a < s.dims {
				if corner&(1<<a) != 0 {
					p = min(p+1, s.size[a]-1)
					weight *= fraction[a]
				} else {
					weight *= 1 - fraction[a]
				}
			}
			j += p * s.stride[a]
		}
		value += weight * float64(field[j])
	}
	return float32(math.Max(0, math.Min(1, value)))
}

func (g *levelSetGrid) evolveInterface(phi []float32, terms levelSetTerms, sampling ShapeSurfaceSampling, maximum int, tolerance float64, progress func(LevelSetProgress)) (*LevelSetResult, error) {
	return g.evolveInterfaceWithInitialization(phi, terms, sampling, CityBlockSeeds, maximum, tolerance, progress)
}

func (g *levelSetGrid) evolveInterfaceWithInitialization(phi []float32, terms levelSetTerms, sampling ShapeSurfaceSampling, initialization ShapeInitializationProfile, maximum int, tolerance float64, progress func(LevelSetProgress)) (*LevelSetResult, error) {
	if err := levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	s := &levelSetState{levelSetGrid: g, phi: phi, width: float32(float64(g.dims+1) * g.hmin), status: make([]byte, g.n), band: make([]int32, 0, g.n)}
	s.heap.maxItems = 7 * g.n
	if err := s.initializeInterfaceAnchorsWithProfile(initialization); err != nil {
		return nil, err
	}
	active, rates, window := make([]int32, 0, g.n), make([]float32, g.n), make([]bool, g.n)
	spare := make([]int32, 0, g.n)
	old, staged, membership := make([]float32, g.n), make([]float32, g.n), make([]byte, g.n)
	var initError error
	active, initError = s.initialInterfaceActive(active)
	if initError != nil {
		return nil, initError
	}
	for i, p := range s.phi {
		if i%4096 == 0 {
			if e := levelSetContextError(g.ctx); e != nil {
				return nil, e
			}
		}
		old[i] = p
	}
	if e := s.refreshInterfaceLayersPrevious(active, old, membership, old); e != nil {
		return nil, e
	}
	active, initError = s.pruneInterfaceAnchors(active, old, membership)
	if initError != nil {
		return nil, initError
	}
	snapshot := func() error {
		for i, p := range s.phi {
			if i%4096 == 0 {
				if err := levelSetContextError(g.ctx); err != nil {
					return err
				}
			}
			window[i] = p < 0
		}
		return levelSetContextError(g.ctx)
	}
	if err := snapshot(); err != nil {
		return nil, err
	}
	iterations, quiet, rms, converged, reason := 0, 0, 0.0, false, LevelSetStopIterationLimit
	for iterations < maximum {
		if len(active) == 0 {
			crossing, err := s.interfaceCrossing()
			if err != nil {
				return nil, err
			}
			reason = LevelSetStopEmptyBand
			if crossing {
				reason = LevelSetStopBoundaryActiveExhausted
			}
			break
		}
		first, curvature := 0.0, 0.0
		for k, index := range active {
			if k%4096 == 0 {
				if err := levelSetContextError(g.ctx); err != nil {
					return nil, err
				}
			}
			i := int(index)
			var rate float32
			var f, c float64
			if sampling == InterpolatedInterface {
				rate, f, c = s.ratePotentialBoundary(i, terms, true, s.interfacePotentialBoundary(i, terms.speed, membership), 1e-6, membership)
			} else {
				rate, f, c = s.ratePotentialBoundary(i, terms, false, 0, 1e-6, membership)
			}
			if math.IsNaN(float64(rate)) || math.IsInf(float64(rate), 0) || math.IsNaN(f) || math.IsInf(f, 0) || math.IsNaN(c) || math.IsInf(c, 0) {
				return nil, fmt.Errorf("%w: nonfinite interface rate", ErrLevelSetInvalidInput)
			}
			rates[k] = rate
			first = math.Max(first, f)
			curvature = math.Max(curvature, c)
		}
		dt := interfaceTimeStep(first, curvature, g.hmin, g.dims)
		if dt == 0 {
			reason = LevelSetStopZeroTimeStep
			break
		}
		for i, p := range s.phi {
			if i%4096 == 0 {
				if e := levelSetContextError(g.ctx); e != nil {
					return nil, e
				}
			}
			old[i] = p
		}
		sum := 0.0
		for k, index := range active {
			if k%4096 == 0 {
				if err := levelSetContextError(g.ctx); err != nil {
					return nil, err
				}
			}
			p := s.phi[index]
			old[index] = p
			change := dt * float64(rates[k])
			next := float64(p) + change
			if math.IsNaN(next) || math.IsInf(next, 0) || math.Abs(next) > math.MaxFloat32 {
				return nil, fmt.Errorf("%w: nonfinite interface update", ErrLevelSetInvalidInput)
			}
			s.phi[index] = float32(next)
			actual := float64(s.phi[index] - p)
			sum += actual * actual
		}
		iterations++
		rms = math.Sqrt(sum / float64(len(active)))
		// Complete the accepted step before inspecting stopping/mask state.
		// RMS measures only the actual PDE changes at the active anchors.
		next, err := s.interfacePromote(active, spare, old, staged, membership)
		if err != nil {
			return nil, err
		}
		if err := s.refreshInterfaceLayersPrevious(next, old, membership, old); err != nil {
			return nil, err
		}
		spare, active = active, next
		active, err = s.pruneInterfaceAnchors(active, old, membership)
		if err != nil {
			return nil, err
		}
		// A value can remain in the half-open active range after the last
		// negative sample disappears. No zero surface remains to evolve,
		// even though that numerical membership has not yet demoted.
		crossing, err := s.interfaceCrossing()
		if err != nil {
			return nil, err
		}
		if len(active) == 0 || !crossing {
			reason = LevelSetStopEmptyBand
			if crossing {
				reason = LevelSetStopBoundaryActiveExhausted
			}
			if progress != nil {
				progress(LevelSetProgress{Iteration: iterations, MaxIterations: maximum, RMSChange: rms})
			}
			break
		}
		if progress != nil {
			progress(LevelSetProgress{Iteration: iterations, MaxIterations: maximum, RMSChange: rms})
		}
		if err := levelSetContextError(g.ctx); err != nil {
			return nil, err
		}
		if rms < tolerance*g.hmin && terms.stopping != FixedIterations && terms.stopping != MaskStabilityOnly {
			reason = LevelSetStopRMS
			converged = true
			break
		}
		if iterations%levelSetWindow == 0 {
			flips := 0
			for i, p := range s.phi {
				if i%4096 == 0 {
					if err := levelSetContextError(g.ctx); err != nil {
						return nil, err
					}
				}
				if (p < 0) != window[i] {
					flips++
				}
			}
			if float64(flips) <= levelSetWindowFlips*float64(len(active)) {
				quiet++
			} else {
				quiet = 0
			}
			if quiet >= 2 && terms.stopping != RMSOnly && terms.stopping != FixedIterations {
				reason = LevelSetStopStableMask
				converged = true
				break
			}
			if err := snapshot(); err != nil {
				return nil, err
			}
		}
	}
	result, err := g.interfaceResult(s.phi)
	if err != nil {
		return nil, err
	}
	result.Iterations, result.RMSChange, result.Converged, result.StopReason = iterations, rms, converged, reason
	if err := levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	return result, nil
}

// interfaceResult counts stable owned phi before allocating exact run storage.
// All rows use capacity-limited views of one arena, so appending to a returned
// row cannot overwrite another row. The source-height headers are admitted by
// newEdgeLevelSetGridProfile before source reads.
func (g *levelSetGrid) interfaceResult(phi []float32) (*LevelSetResult, error) {
	if err := levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	counts := make([]int, g.ny*g.nz)
	total, masks := 0, 0
	for z := 0; z < g.nz; z++ {
		nonempty := false
		for y := 0; y < g.ny; y++ {
			previous := false
			for x := 0; x < g.nx; x++ {
				i := g.index(x, y, z)
				if i%4096 == 0 {
					if err := levelSetContextError(g.ctx); err != nil {
						return nil, err
					}
				}
				inside := phi[i] < 0
				if inside && !previous {
					counts[y+g.ny*z]++
					total++
					nonempty = true
				}
				previous = inside
			}
		}
		if nonempty {
			masks++
		}
	}
	if total > g.n {
		return nil, ErrLevelSetResourceLimit
	}
	if err := levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	arena := make([]MaskRun, total)
	out := &LevelSetResult{Masks: make(map[int]*RasterMask, masks), RegionOrigin: g.origin, RegionSize: g.size}
	sizes := [3]int{g.volume.Columns, g.volume.Rows, g.volume.Slices}
	at := 0
	for z := 0; z < g.nz; z++ {
		var mask *RasterMask
		for y := 0; y < g.ny; y++ {
			n := counts[y+g.ny*z]
			if n == 0 {
				continue
			}
			if err := levelSetContextError(g.ctx); err != nil {
				return nil, err
			}
			if mask == nil {
				mask = NewRasterMask(g.volume.Columns, g.volume.Rows)
			}
			row := arena[at : at+n : at+n]
			at += n
			k, start := 0, -1
			for x := 0; x <= g.nx; x++ {
				if x%4096 == 0 {
					if err := levelSetContextError(g.ctx); err != nil {
						return nil, err
					}
				}
				inside := x < g.nx && phi[g.index(x, y, z)] < 0
				if inside {
					out.VoxelCount++
					if start < 0 {
						start = x
					}
					for a, c := range [3]int{x, y, z} {
						if a >= g.dims {
							continue
						}
						v := c + g.origin[a]
						if v == 0 || v == sizes[a]-1 {
							out.TouchesVolumeBoundary = true
						}
						if c == 0 && g.origin[a] > 0 || c == g.size[a]-1 && g.origin[a]+g.size[a] < sizes[a] {
							out.TouchesRegionBoundary = true
						}
					}
				} else if start >= 0 {
					row[k] = MaskRun{Start: start + g.origin[0], End: x + g.origin[0]}
					k++
					start = -1
				}
			}
			if k != n {
				return nil, fmt.Errorf("%w: unstable owned interface field", ErrLevelSetInvalidInput)
			}
			mask.runs[y+g.origin[1]] = row
		}
		if mask != nil {
			out.Masks[z+g.origin[2]] = mask
		}
	}
	voxel := g.h[0] * g.h[1]
	if g.dims == 3 {
		voxel *= g.h[2]
	}
	out.VolumeMM3 = float64(out.VoxelCount) * voxel
	if err := levelSetContextError(g.ctx); err != nil {
		return nil, err
	}
	return out, nil
}
