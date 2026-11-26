package render

import (
	"container/heap"
	"context"
	"errors"
	"fmt"
	"math"
	"sort"
)

var (
	// ErrCPRPathAssistantInput reports an invalid volume or waypoint set. The
	// interactive CPR workflow accepts two through five patient-space points.
	ErrCPRPathAssistantInput = errors.New("render: invalid CPR path assistant input")
	// ErrCPRPathAssistantMemory reports that a bounded search grid could not be
	// produced within the configured memory budget.
	ErrCPRPathAssistantMemory = errors.New("render: CPR path assistant search exceeds memory budget")
	// ErrCPRPathAssistantNotFound reports that no connected image-space route
	// could be reconstructed between two consecutive waypoints.
	ErrCPRPathAssistantNotFound = errors.New("render: CPR path assistant route not found")
)

// CPRPathMethod selects the minimal-path solver of FindCPRAssistedPath.
type CPRPathMethod int

const (
	// CPRPathFastMarching, the default, solves the Eikonal equation for the
	// arrival time from the first waypoint of each segment (FastMarching) and
	// descends it from the second with subvoxel steps: a continuous route
	// without the grid's metrication, pushed towards the lumen centre by a
	// distance-to-wall term (#1100).
	CPRPathFastMarching CPRPathMethod = iota
	// CPRPathDijkstra is the earlier 26-neighbour graph search between grid
	// nodes.
	CPRPathDijkstra
)

// CPRPathAssistantOptions controls the image-derived centerline search. The
// search uses an adaptively downsampled, volume-axis-aligned grid and therefore
// stays bounded for large clinical series.
type CPRPathAssistantOptions struct {
	// Method selects the solver; the zero value is CPRPathFastMarching.
	Method CPRPathMethod
	// GridSpacingMM is the search grid step: 1 mm for fast marching and 2 mm
	// for Dijkstra when zero. The grid coarsens itself to fit MaxGridVoxels.
	GridSpacingMM    float64
	MarginMM         float64
	OutputSpacingMM  float64
	IntensityWeight  float64
	MaxGridVoxels    int
	MaxExpandedNodes int
	MaxQueueEntries  int
	MaxOutputPoints  int
	// SmoothingMM, when positive, low-pass filters each image-derived segment
	// before the output resampling to remove the staircase of the search grid:
	// repeated [1 2 1]/4 binomial passes whose combined Gaussian standard
	// deviation is about SmoothingMM along the path (at most
	// MaxCPRAssistantSmoothingMM). The waypoints stay fixed and remain on the
	// output; bends are pulled inwards by roughly SmoothingMM²/(2·radius).
	// Zero keeps the unsmoothed route.
	SmoothingMM float64
	// SpeedVolume, when set, replaces the intensity-similarity speed of fast
	// marching with an image such as a vesselness response, sampled in
	// patient space: speed = (v/max v)^SpeedExponent plus a small floor that
	// keeps the grid connected. It must share the frame of reference of the
	// volume.
	SpeedVolume *Volume
	// VesselGuided derives a bounded, multiscale vesselness speed volume around
	// the waypoints. It requires fast marching and cannot accompany SpeedVolume.
	VesselGuided bool
	// SpeedExponent sharpens SpeedVolume; 2 when zero.
	SpeedExponent float64
	// CentringExponent weights the intensity-similarity speed of fast
	// marching by (d + h)^CentringExponent, where d is the distance in mm to
	// the nearest dissimilar grid node and h the grid step, so the route
	// runs along the lumen's medial axis; 1 when zero. NoCentring removes the
	// term and keeps the similarity speed alone.
	CentringExponent float64
	NoCentring       bool
}

// CPRPathAssistantLimitError identifies which bounded search resource was
// exhausted. It matches ErrCPRPathAssistantMemory through errors.Is.
type CPRPathAssistantLimitError struct {
	Resource string
	Value    int
	Limit    int
}

func (e *CPRPathAssistantLimitError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%v: %s=%d, limit=%d", ErrCPRPathAssistantMemory, e.Resource, e.Value, e.Limit)
}

func (e *CPRPathAssistantLimitError) Unwrap() error { return ErrCPRPathAssistantMemory }

// CPRPathAssistantSegmentError identifies the waypoint pair that could not be
// connected. From and To are one-based so callers can present A/B/C-style
// feedback without parsing an error string.
type CPRPathAssistantSegmentError struct {
	From int
	To   int
	Err  error
}

func (e *CPRPathAssistantSegmentError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("waypoints %d-%d: %v", e.From, e.To, e.Err)
}

func (e *CPRPathAssistantSegmentError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

const (
	defaultCPRAssistantGridSpacingMM = 2.0
	// defaultCPRAssistantFastMarchingGridMM is finer: the subvoxel descent
	// uses the resolution Dijkstra's metrication wasted.
	defaultCPRAssistantFastMarchingGridMM = 1.0
	defaultCPRAssistantSpeedExponent      = 2.0
	defaultCPRAssistantCentringExponent   = 1.0
	// cprAssistantSpeedFloor keeps SpeedVolume grids connected.
	cprAssistantSpeedFloor             = 1e-3
	defaultCPRAssistantMarginMM        = 30.0
	defaultCPRAssistantOutputSpacingMM = 3.0
	defaultCPRAssistantIntensityWeight = 64.0
	defaultCPRAssistantMaxGridVoxels   = 1_000_000
	defaultCPRAssistantMaxQueueEntries = 2_000_000
	defaultCPRAssistantMaxOutputPoints = 250_000

	MaxCPRAssistantGridVoxels    = 4_000_000
	MaxCPRAssistantExpandedNodes = 4_000_000
	MaxCPRAssistantQueueEntries  = 4_000_000
	MaxCPRAssistantOutputPoints  = MaxCPRPathSamples
	// MaxCPRAssistantSmoothingMM bounds CPRPathAssistantOptions.SmoothingMM.
	MaxCPRAssistantSmoothingMM = 20.0
	// cprAssistantMaxSmoothingPasses bounds the binomial passes per segment.
	cprAssistantMaxSmoothingPasses = 256
)

// FindCPRAssistedPath finds an image-derived patient-space centerline through
// each consecutive pair of waypoints. Traversal favors voxels whose intensity
// resembles the endpoint seeds (or, with SpeedVolume, voxels of high speed),
// while physical distances keep the result spacing-aware. Each segment is re-sampled independently so intermediate
// user waypoints remain exact and the final output is reversible in the UI.
func FindCPRAssistedPath(ctx context.Context, volume *Volume, waypoints []Vec3, options CPRPathAssistantOptions) ([]Vec3, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if volume == nil || volume.Cols <= 0 || volume.Rows <= 0 || volume.Depth <= 0 || len(waypoints) < 2 || len(waypoints) > 5 {
		return nil, ErrCPRPathAssistantInput
	}
	for i, point := range waypoints {
		if !finiteVec3(point) || !voxelInsideVolume(volume, volume.PatientToVoxel(point)) {
			return nil, fmt.Errorf("%w: waypoint %d lies outside the volume", ErrCPRPathAssistantInput, i+1)
		}
	}

	var err error
	options, err = normalizedCPRPathAssistantOptions(options)
	if err != nil {
		return nil, err
	}
	if options.VesselGuided {
		speed, speedErr := prepareCPRVesselSpeed(ctx, volume, waypoints, options)
		if speedErr != nil {
			return nil, speedErr
		}
		defer speed.Close()
		options.SpeedVolume = speed
	}
	centerline := make([]Vec3, 0, len(waypoints)*16)
	for i := 1; i < len(waypoints); i++ {
		find := findCPRFastMarchingSegment
		if options.Method == CPRPathDijkstra {
			find = findCPRAssistedSegment
		}
		segment, err := find(ctx, volume, waypoints[i-1], waypoints[i], options)
		if err != nil {
			return nil, &CPRPathAssistantSegmentError{From: i, To: i + 1, Err: err}
		}
		if options.SmoothingMM > 0 {
			segment, err = smoothCPRAssistedSegment(ctx, segment, options)
			if err != nil {
				return nil, &CPRPathAssistantSegmentError{From: i, To: i + 1, Err: err}
			}
		}
		remaining := options.MaxOutputPoints - len(centerline)
		segment, err = resampleCPRAssistedSegment(ctx, segment, options.OutputSpacingMM, remaining)
		if err != nil {
			return nil, &CPRPathAssistantSegmentError{From: i, To: i + 1, Err: err}
		}
		if len(segment) < 2 {
			return nil, &CPRPathAssistantSegmentError{From: i, To: i + 1, Err: ErrCPRPathAssistantNotFound}
		}
		if len(centerline) > 0 && segment[0].Sub(centerline[len(centerline)-1]).Length() < 1e-9 {
			segment = segment[1:]
		}
		centerline = append(centerline, segment...)
	}
	return centerline, nil
}

func normalizedCPRPathAssistantOptions(options CPRPathAssistantOptions) (CPRPathAssistantOptions, error) {
	for _, field := range []struct {
		name  string
		value float64
	}{
		{"grid_spacing_mm", options.GridSpacingMM},
		{"margin_mm", options.MarginMM},
		{"output_spacing_mm", options.OutputSpacingMM},
		{"intensity_weight", options.IntensityWeight},
		{"smoothing_mm", options.SmoothingMM},
		{"speed_exponent", options.SpeedExponent},
		{"centring_exponent", options.CentringExponent},
	} {
		if field.value < 0 || math.IsNaN(field.value) || math.IsInf(field.value, 0) {
			return CPRPathAssistantOptions{}, fmt.Errorf("%w: %s must be zero or positive and finite", ErrCPRPathAssistantInput, field.name)
		}
	}
	if options.SmoothingMM > MaxCPRAssistantSmoothingMM {
		return CPRPathAssistantOptions{}, fmt.Errorf("%w: smoothing_mm must not exceed %g", ErrCPRPathAssistantInput, MaxCPRAssistantSmoothingMM)
	}
	switch options.Method {
	case CPRPathFastMarching, CPRPathDijkstra:
	default:
		return CPRPathAssistantOptions{}, fmt.Errorf("%w: unknown path method %d", ErrCPRPathAssistantInput, options.Method)
	}
	if (options.SpeedVolume != nil || options.VesselGuided) && options.Method != CPRPathFastMarching {
		return CPRPathAssistantOptions{}, fmt.Errorf("%w: a speed volume requires fast marching", ErrCPRPathAssistantInput)
	}
	if options.VesselGuided && options.SpeedVolume != nil {
		return CPRPathAssistantOptions{}, fmt.Errorf("%w: vessel guidance and an explicit speed volume are mutually exclusive", ErrCPRPathAssistantInput)
	}
	if options.GridSpacingMM == 0 {
		options.GridSpacingMM = defaultCPRAssistantGridSpacingMM
		if options.Method == CPRPathFastMarching {
			options.GridSpacingMM = defaultCPRAssistantFastMarchingGridMM
		}
	}
	if options.SpeedExponent == 0 {
		options.SpeedExponent = defaultCPRAssistantSpeedExponent
	}
	if options.CentringExponent == 0 {
		options.CentringExponent = defaultCPRAssistantCentringExponent
	}
	if options.MarginMM == 0 {
		options.MarginMM = defaultCPRAssistantMarginMM
	}
	if options.OutputSpacingMM == 0 {
		options.OutputSpacingMM = defaultCPRAssistantOutputSpacingMM
	}
	if options.IntensityWeight == 0 {
		options.IntensityWeight = defaultCPRAssistantIntensityWeight
	}
	for _, field := range []struct {
		name  string
		value int
	}{
		{"max_grid_voxels", options.MaxGridVoxels},
		{"max_expanded_nodes", options.MaxExpandedNodes},
		{"max_queue_entries", options.MaxQueueEntries},
		{"max_output_points", options.MaxOutputPoints},
	} {
		if field.value < 0 {
			return CPRPathAssistantOptions{}, fmt.Errorf("%w: %s cannot be negative", ErrCPRPathAssistantInput, field.name)
		}
	}
	if options.MaxGridVoxels == 0 {
		options.MaxGridVoxels = defaultCPRAssistantMaxGridVoxels
	}
	if options.MaxExpandedNodes == 0 {
		options.MaxExpandedNodes = options.MaxGridVoxels
	}
	if options.MaxQueueEntries == 0 {
		options.MaxQueueEntries = defaultCPRAssistantMaxQueueEntries
	}
	if options.MaxOutputPoints == 0 {
		options.MaxOutputPoints = defaultCPRAssistantMaxOutputPoints
	}
	for _, field := range []struct {
		name       string
		value, max int
	}{
		{"grid_voxels", options.MaxGridVoxels, MaxCPRAssistantGridVoxels},
		{"expanded_nodes", options.MaxExpandedNodes, MaxCPRAssistantExpandedNodes},
		{"queue_entries", options.MaxQueueEntries, MaxCPRAssistantQueueEntries},
		{"output_points", options.MaxOutputPoints, MaxCPRAssistantOutputPoints},
	} {
		if field.value > field.max {
			return CPRPathAssistantOptions{}, &CPRPathAssistantLimitError{Resource: field.name, Value: field.value, Limit: field.max}
		}
	}
	return options, nil
}

type cprAssistantGrid struct {
	volume     *Volume
	min        Vec3
	step       Vec3
	nx, ny, nz int
	values     []float64
	valid      []bool
	valueMin   float64
	valueMax   float64
}

func newCPRAssistantGrid(ctx context.Context, volume *Volume, start, end Vec3, options CPRPathAssistantOptions) (*cprAssistantGrid, error) {
	startVoxel := volume.PatientToVoxel(start)
	endVoxel := volume.PatientToVoxel(end)
	spacing := Vec3{X: volume.ColSpacing, Y: volume.RowSpacing, Z: volume.SliceSpacing}
	for _, value := range []*float64{&spacing.X, &spacing.Y, &spacing.Z} {
		if !positiveFinite(*value) {
			*value = 1
		}
	}

	gridSpacing := options.GridSpacingMM
	for attempt := 0; attempt < 8; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		margin := Vec3{X: options.MarginMM / spacing.X, Y: options.MarginMM / spacing.Y, Z: options.MarginMM / spacing.Z}
		minVoxel := Vec3{
			X: math.Max(0, math.Min(startVoxel.X, endVoxel.X)-margin.X),
			Y: math.Max(0, math.Min(startVoxel.Y, endVoxel.Y)-margin.Y),
			Z: math.Max(0, math.Min(startVoxel.Z, endVoxel.Z)-margin.Z),
		}
		maxVoxel := Vec3{
			X: math.Min(float64(volume.Cols-1), math.Max(startVoxel.X, endVoxel.X)+margin.X),
			Y: math.Min(float64(volume.Rows-1), math.Max(startVoxel.Y, endVoxel.Y)+margin.Y),
			Z: math.Min(float64(volume.Depth-1), math.Max(startVoxel.Z, endVoxel.Z)+margin.Z),
		}
		step := Vec3{
			X: math.Max(1, gridSpacing/spacing.X),
			Y: math.Max(1, gridSpacing/spacing.Y),
			Z: math.Max(1, gridSpacing/spacing.Z),
		}
		nx := int(math.Ceil((maxVoxel.X-minVoxel.X)/step.X)) + 1
		ny := int(math.Ceil((maxVoxel.Y-minVoxel.Y)/step.Y)) + 1
		nz := int(math.Ceil((maxVoxel.Z-minVoxel.Z)/step.Z)) + 1
		count64, countOK := boundedCPRAssistantProduct(nx, ny, nz, options.MaxGridVoxels)
		if countOK {
			grid := &cprAssistantGrid{
				volume: volume, min: minVoxel, step: step,
				nx: nx, ny: ny, nz: nz,
				values: make([]float64, int(count64)), valid: make([]bool, int(count64)),
				valueMin: math.Inf(1), valueMax: math.Inf(-1),
			}
			if err := grid.sampleValues(ctx); err != nil {
				return nil, err
			}
			return grid, nil
		}
		if nx <= 0 || ny <= 0 || nz <= 0 {
			break
		}
		countEstimate := float64(nx) * float64(ny) * float64(nz)
		if math.IsInf(countEstimate, 0) || math.IsNaN(countEstimate) {
			return nil, &CPRPathAssistantLimitError{Resource: "grid_voxels", Value: options.MaxGridVoxels + 1, Limit: options.MaxGridVoxels}
		}
		ratio := math.Cbrt(countEstimate / float64(options.MaxGridVoxels))
		if ratio < 1.15 {
			ratio = 1.15
		}
		gridSpacing *= ratio * 1.02
	}
	return nil, &CPRPathAssistantLimitError{Resource: "grid_voxels", Value: options.MaxGridVoxels + 1, Limit: options.MaxGridVoxels}
}

func boundedCPRAssistantProduct(nx, ny, nz, limit int) (int64, bool) {
	if nx <= 0 || ny <= 0 || nz <= 0 || limit <= 0 {
		return 0, false
	}
	product := int64(1)
	for _, value := range []int{nx, ny, nz} {
		if int64(value) > int64(limit)/product {
			return 0, false
		}
		product *= int64(value)
	}
	return product, product <= int64(limit)
}

func (g *cprAssistantGrid) sampleValues(ctx context.Context) error {
	sampler, ok := newVolumeSampler(g.volume)
	if !ok {
		return ErrCPRInput
	}
	defer sampler.Close()
	for index := range g.values {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		voxel := g.voxelAt(index)
		patient := g.volume.VoxelToPatient(voxel)
		value, sampleOK := sampler.trilinearAt(sampler.vol.PatientToVoxel(patient))
		if !sampleOK || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		g.values[index] = value
		g.valid[index] = true
		g.valueMin = math.Min(g.valueMin, value)
		g.valueMax = math.Max(g.valueMax, value)
	}
	return nil
}

func (g *cprAssistantGrid) index(x, y, z int) int { return (z*g.ny+y)*g.nx + x }

func (g *cprAssistantGrid) xyz(index int) (int, int, int) {
	x := index % g.nx
	rest := index / g.nx
	return x, rest % g.ny, rest / g.ny
}

func (g *cprAssistantGrid) voxelAt(index int) Vec3 {
	x, y, z := g.xyz(index)
	return Vec3{
		X: math.Min(float64(g.volume.Cols-1), g.min.X+float64(x)*g.step.X),
		Y: math.Min(float64(g.volume.Rows-1), g.min.Y+float64(y)*g.step.Y),
		Z: math.Min(float64(g.volume.Depth-1), g.min.Z+float64(z)*g.step.Z),
	}
}

func (g *cprAssistantGrid) nearestIndex(voxel Vec3) int {
	x := clampInt(NearestIndex((voxel.X-g.min.X)/g.step.X), 0, g.nx-1)
	y := clampInt(NearestIndex((voxel.Y-g.min.Y)/g.step.Y), 0, g.ny-1)
	z := clampInt(NearestIndex((voxel.Z-g.min.Z)/g.step.Z), 0, g.nz-1)
	return g.index(x, y, z)
}

func findCPRAssistedSegment(ctx context.Context, volume *Volume, start, end Vec3, options CPRPathAssistantOptions) ([]Vec3, error) {
	grid, err := newCPRAssistantGrid(ctx, volume, start, end, options)
	if err != nil {
		return nil, err
	}
	startIndex := grid.nearestIndex(volume.PatientToVoxel(start))
	endIndex := grid.nearestIndex(volume.PatientToVoxel(end))
	if !grid.valid[startIndex] || !grid.valid[endIndex] {
		return nil, ErrCPRPathAssistantNotFound
	}
	if startIndex == endIndex {
		return []Vec3{start, end}, nil
	}

	seedValue := (grid.values[startIndex] + grid.values[endIndex]) / 2
	valueRange := grid.valueMax - grid.valueMin
	if !positiveFinite(valueRange) {
		valueRange = 1
	}
	penaltyAt := func(index int) float64 {
		deviation := math.Abs(grid.values[index]-seedValue) / valueRange
		return 1 + options.IntensityWeight*deviation*deviation
	}

	count := len(grid.values)
	distances := make([]float64, count)
	previous := make([]int32, count)
	visited := make([]bool, count)
	for i := range distances {
		distances[i] = math.Inf(1)
		previous[i] = -1
	}
	distances[startIndex] = 0
	queue := cprAssistantPriorityQueue{{index: startIndex, cost: 0}}
	heap.Init(&queue)
	pushes := 1
	expanded := 0

	for pops := 0; queue.Len() > 0; pops++ {
		if pops&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		current := heap.Pop(&queue).(cprAssistantQueueItem)
		if visited[current.index] || current.cost != distances[current.index] {
			continue
		}
		expanded++
		if expanded > options.MaxExpandedNodes {
			return nil, &CPRPathAssistantLimitError{Resource: "expanded_nodes", Value: expanded, Limit: options.MaxExpandedNodes}
		}
		visited[current.index] = true
		if current.index == endIndex {
			break
		}
		x, y, z := grid.xyz(current.index)
		currentVoxel := grid.voxelAt(current.index)
		currentPenalty := penaltyAt(current.index)
		for dz := -1; dz <= 1; dz++ {
			for dy := -1; dy <= 1; dy++ {
				for dx := -1; dx <= 1; dx++ {
					if dx == 0 && dy == 0 && dz == 0 {
						continue
					}
					nx, ny, nz := x+dx, y+dy, z+dz
					if nx < 0 || ny < 0 || nz < 0 || nx >= grid.nx || ny >= grid.ny || nz >= grid.nz {
						continue
					}
					nextIndex := grid.index(nx, ny, nz)
					if visited[nextIndex] || !grid.valid[nextIndex] {
						continue
					}
					edgeLength := volume.VoxelToPatient(grid.voxelAt(nextIndex)).Sub(volume.VoxelToPatient(currentVoxel)).Length()
					candidate := current.cost + edgeLength*(currentPenalty+penaltyAt(nextIndex))/2
					if candidate < distances[nextIndex] {
						distances[nextIndex] = candidate
						previous[nextIndex] = int32(current.index)
						pushes++
						if pushes > options.MaxQueueEntries {
							return nil, &CPRPathAssistantLimitError{Resource: "queue_entries", Value: pushes, Limit: options.MaxQueueEntries}
						}
						heap.Push(&queue, cprAssistantQueueItem{index: nextIndex, cost: candidate})
					}
				}
			}
		}
	}
	if !visited[endIndex] {
		return nil, ErrCPRPathAssistantNotFound
	}

	indices := make([]int, 0, 64)
	for index := endIndex; index >= 0; index = int(previous[index]) {
		indices = append(indices, index)
		if index == startIndex {
			break
		}
	}
	if len(indices) == 0 || indices[len(indices)-1] != startIndex {
		return nil, ErrCPRPathAssistantNotFound
	}
	path := make([]Vec3, 0, len(indices)+2)
	path = append(path, start)
	for i := len(indices) - 2; i > 0; i-- {
		path = append(path, volume.VoxelToPatient(grid.voxelAt(indices[i])))
	}
	path = append(path, end)
	return path, nil
}

// smoothCPRAssistedSegment resamples a grid route densely and applies
// binomial [1 2 1]/4 passes with both ends pinned. k passes at sample step h
// approximate a Gaussian of standard deviation h·sqrt(k/2).
func smoothCPRAssistedSegment(ctx context.Context, points []Vec3, options CPRPathAssistantOptions) ([]Vec3, error) {
	sigma := options.SmoothingMM
	step := math.Min(options.GridSpacingMM, options.OutputSpacingMM) / 2
	step = math.Max(step, sigma/math.Sqrt(cprAssistantMaxSmoothingPasses/2))
	dense, err := resampleCPRAssistedSegment(ctx, points, step, MaxCPRAssistantOutputPoints)
	if err != nil || len(dense) < 3 {
		return dense, err
	}
	passes := clampInt(int(math.Ceil(2*(sigma/step)*(sigma/step))), 1, cprAssistantMaxSmoothingPasses)
	next := make([]Vec3, len(dense))
	next[0], next[len(next)-1] = dense[0], dense[len(dense)-1]
	for pass := 0; pass < passes; pass++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for i := 1; i < len(dense)-1; i++ {
			next[i] = dense[i-1].Add(dense[i].Scale(2)).Add(dense[i+1]).Scale(0.25)
		}
		dense, next = next, dense
	}
	return dense, nil
}

// resampleCPRAssistedSegment resamples the piecewise-linear route at the given
// arc-length spacing, always keeping both ends. It measures the route itself
// rather than the spline NewCPRPath later fits through the output.
func resampleCPRAssistedSegment(ctx context.Context, points []Vec3, spacing float64, limit int) ([]Vec3, error) {
	path, err := newCPRPolyline(points)
	if err != nil {
		return nil, err
	}
	countFloat := math.Floor(path.Length()/spacing) + 2
	if !positiveFinite(countFloat) {
		return nil, fmt.Errorf("%w: output sample count is not finite", ErrCPRPathAssistantInput)
	}
	if limit < 2 || countFloat > float64(limit) {
		return nil, &CPRPathAssistantLimitError{Resource: "output_points", Value: limit + 1, Limit: max(0, limit)}
	}
	count := int(math.Floor(path.Length() / spacing))
	result := make([]Vec3, 0, count+2)
	for i := 0; i <= count; i++ {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		result = append(result, path.PointAt(float64(i)*spacing))
	}
	end := path.PointAt(path.Length())
	if result[len(result)-1].Sub(end).Length() > 1e-9 {
		result = append(result, end)
	} else {
		result[len(result)-1] = end
	}
	return result, nil
}

// cprPolyline is a piecewise-linear route measured by arc length.
type cprPolyline struct {
	points []Vec3
	cum    []float64 // arc length at each point
}

func newCPRPolyline(points []Vec3) (cprPolyline, error) {
	pts := make([]Vec3, 0, len(points))
	for index, p := range points {
		if !finiteVec3(p) {
			return cprPolyline{}, &CPRInputError{Field: fmt.Sprintf("route[%d]", index), Reason: "coordinate is not finite"}
		}
		if len(pts) > 0 && p == pts[len(pts)-1] {
			continue
		}
		pts = append(pts, p)
	}
	if len(pts) < 2 {
		return cprPolyline{}, &CPRInputError{Field: "route", Reason: "at least two distinct points are required"}
	}
	cum := make([]float64, len(pts))
	for i := 1; i < len(pts); i++ {
		cum[i] = cum[i-1] + pts[i].Sub(pts[i-1]).Length()
		if !positiveFinite(cum[i]) || cum[i] > MaxCPRPathLengthMM {
			return cprPolyline{}, &CPRInputError{Field: "route_length", Reason: "length is not finite or exceeds the CPR path limit"}
		}
	}
	return cprPolyline{points: pts, cum: cum}, nil
}

func (l cprPolyline) Length() float64 { return l.cum[len(l.cum)-1] }

// PointAt returns the point at arc length s, clamped to [0, Length].
func (l cprPolyline) PointAt(s float64) Vec3 {
	if s <= 0 || math.IsNaN(s) {
		return l.points[0]
	}
	last := len(l.points) - 1
	if s >= l.cum[last] {
		return l.points[last]
	}
	i := sort.SearchFloat64s(l.cum, s)
	if l.cum[i] == s {
		return l.points[i]
	}
	lo := i - 1
	frac := (s - l.cum[lo]) / (l.cum[i] - l.cum[lo])
	return l.points[lo].Add(l.points[i].Sub(l.points[lo]).Scale(frac))
}

type cprAssistantQueueItem struct {
	index int
	cost  float64
}

type cprAssistantPriorityQueue []cprAssistantQueueItem

func (q cprAssistantPriorityQueue) Len() int           { return len(q) }
func (q cprAssistantPriorityQueue) Less(i, j int) bool { return q[i].cost < q[j].cost }
func (q cprAssistantPriorityQueue) Swap(i, j int)      { q[i], q[j] = q[j], q[i] }
func (q *cprAssistantPriorityQueue) Push(value any)    { *q = append(*q, value.(cprAssistantQueueItem)) }
func (q *cprAssistantPriorityQueue) Pop() any {
	old := *q
	last := old[len(old)-1]
	*q = old[:len(old)-1]
	return last
}

func voxelInsideVolume(volume *Volume, point Vec3) bool {
	return point.X >= 0 && point.Y >= 0 && point.Z >= 0 &&
		point.X <= float64(volume.Cols-1) && point.Y <= float64(volume.Rows-1) && point.Z <= float64(volume.Depth-1)
}

func finiteVec3(point Vec3) bool {
	return !math.IsNaN(point.X) && !math.IsNaN(point.Y) && !math.IsNaN(point.Z) &&
		!math.IsInf(point.X, 0) && !math.IsInf(point.Y, 0) && !math.IsInf(point.Z, 0)
}

func positiveFinite(value float64) bool {
	return value > 0 && !math.IsNaN(value) && !math.IsInf(value, 0)
}

func clampInt(value, minValue, maxValue int) int {
	if value < minValue {
		return minValue
	}
	if value > maxValue {
		return maxValue
	}
	return value
}
