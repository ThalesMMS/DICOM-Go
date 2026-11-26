package render

import (
	"context"
	"errors"
	"fmt"
	"math"
)

// findCPRFastMarchingSegment connects two waypoints by the minimal path of
// the speed field on the search grid: fast marching from the first waypoint
// until the second is reached, then a subvoxel descent of the arrival time
// from the second. The route starts and ends exactly on the waypoints.
func findCPRFastMarchingSegment(ctx context.Context, volume *Volume, start, end Vec3, options CPRPathAssistantOptions) ([]Vec3, error) {
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
	spacing := grid.physicalSpacing()
	speed, err := grid.fastMarchingSpeed(ctx, startIndex, endIndex, spacing, options)
	if err != nil {
		return nil, err
	}
	sx, sy, sz := grid.xyz(startIndex)
	ex, ey, ez := grid.xyz(endIndex)
	arrival, err := FastMarching(ctx, FastMarchingGrid{Nx: grid.nx, Ny: grid.ny, Nz: grid.nz, Spacing: spacing, Speed: speed},
		[]FastMarchingSeed{{Index: [3]int{sx, sy, sz}}},
		FastMarchingOptions{Targets: [][3]int{{ex, ey, ez}}, MaxFrozen: options.MaxExpandedNodes, MaxTrial: options.MaxQueueEntries})
	var limit *FastMarchingLimitError
	if errors.As(err, &limit) {
		resource := "expanded_nodes"
		if limit.Resource == "trial" {
			resource = "queue_entries"
		}
		return nil, &CPRPathAssistantLimitError{Resource: resource, Value: limit.Limit + 1, Limit: limit.Limit}
	}
	if err != nil {
		return nil, err
	}
	if !arrival.TargetsReached {
		return nil, ErrCPRPathAssistantNotFound
	}
	endVoxel := volume.PatientToVoxel(end)
	from := Vec3{
		X: clampFloat((endVoxel.X-grid.min.X)/grid.step.X, 0, float64(grid.nx-1)),
		Y: clampFloat((endVoxel.Y-grid.min.Y)/grid.step.Y, 0, float64(grid.ny-1)),
		Z: clampFloat((endVoxel.Z-grid.min.Z)/grid.step.Z, 0, float64(grid.nz-1)),
	}
	step := math.Min(spacing.X, math.Min(spacing.Y, spacing.Z)) / 2
	route, err := arrival.MinimalPath(ctx, from, step)
	if err != nil {
		return nil, err
	}
	last := route[len(route)-1]
	if last != (Vec3{X: float64(sx), Y: float64(sy), Z: float64(sz)}) {
		// The descent ended in a basin without the seed.
		return nil, ErrCPRPathAssistantNotFound
	}
	path := make([]Vec3, 0, len(route)+1)
	path = append(path, start)
	for i := len(route) - 2; i > 0; i-- {
		path = append(path, volume.VoxelToPatient(grid.continuousVoxel(route[i])))
	}
	path = append(path, end)
	return path, nil
}

// physicalSpacing is the grid step in mm along each volume axis.
func (g *cprAssistantGrid) physicalSpacing() Vec3 {
	spacing := Vec3{X: g.volume.ColSpacing, Y: g.volume.RowSpacing, Z: g.volume.SliceSpacing}
	for _, value := range []*float64{&spacing.X, &spacing.Y, &spacing.Z} {
		if !positiveFinite(*value) {
			*value = 1
		}
	}
	return Vec3{X: g.step.X * spacing.X, Y: g.step.Y * spacing.Y, Z: g.step.Z * spacing.Z}
}

// continuousVoxel maps a continuous grid index to a volume voxel coordinate.
func (g *cprAssistantGrid) continuousVoxel(p Vec3) Vec3 {
	return Vec3{
		X: math.Min(float64(g.volume.Cols-1), g.min.X+p.X*g.step.X),
		Y: math.Min(float64(g.volume.Rows-1), g.min.Y+p.Y*g.step.Y),
		Z: math.Min(float64(g.volume.Depth-1), g.min.Z+p.Z*g.step.Z),
	}
}

// fastMarchingSpeed returns the propagation speed of every grid node: the
// SpeedVolume response, or the intensity similarity to the waypoints weighted
// by the distance to dissimilar nodes. Invalid samples block the node.
func (g *cprAssistantGrid) fastMarchingSpeed(ctx context.Context, startIndex, endIndex int, spacing Vec3, options CPRPathAssistantOptions) ([]float64, error) {
	speed := make([]float64, len(g.values))
	if options.SpeedVolume != nil {
		return speed, g.sampleSpeedVolume(ctx, speed, options)
	}
	seedValue := (g.values[startIndex] + g.values[endIndex]) / 2
	valueRange := g.valueMax - g.valueMin
	if !positiveFinite(valueRange) {
		valueRange = 1
	}
	outside := make([]bool, len(speed))
	anyOutside := false
	for index, value := range g.values {
		if !g.valid[index] {
			outside[index] = true
			anyOutside = true
			continue
		}
		deviation := (value - seedValue) / valueRange
		penalty := 1 + options.IntensityWeight*deviation*deviation
		speed[index] = 1 / penalty
		// Nodes that cost more than twice a matching node are "wall".
		if penalty > 2 {
			outside[index] = true
			anyOutside = true
		}
	}
	if options.NoCentring || !anyOutside {
		return speed, nil
	}
	distance, err := squaredDistanceToFeatures(ctx, outside, g.nx, g.ny, g.nz, spacing)
	if err != nil {
		return nil, err
	}
	h := math.Min(spacing.X, math.Min(spacing.Y, spacing.Z))
	for index := range speed {
		if speed[index] > 0 {
			speed[index] *= math.Pow(math.Sqrt(distance[index])+h, options.CentringExponent)
		}
	}
	return speed, nil
}

func (g *cprAssistantGrid) sampleSpeedVolume(ctx context.Context, speed []float64, options CPRPathAssistantOptions) error {
	sampler, ok := newVolumeSampler(options.SpeedVolume)
	if !ok {
		return fmt.Errorf("%w: speed volume cannot be sampled", ErrCPRPathAssistantInput)
	}
	defer sampler.Close()
	maxValue := 0.0
	for index := range speed {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		if !g.valid[index] {
			continue
		}
		patient := g.volume.VoxelToPatient(g.voxelAt(index))
		value, sampleOK := sampler.trilinearAt(options.SpeedVolume.PatientToVoxel(patient))
		if !sampleOK || !(value > 0) || math.IsInf(value, 0) {
			speed[index] = -1 // valid but zero response: floor speed below
			continue
		}
		speed[index] = value
		maxValue = math.Max(maxValue, value)
	}
	if !(maxValue > 0) {
		return fmt.Errorf("%w: the speed volume has no positive response between the waypoints", ErrCPRPathAssistantNotFound)
	}
	for index, value := range speed {
		switch {
		case value > 0:
			speed[index] = math.Pow(value/maxValue, options.SpeedExponent) + cprAssistantSpeedFloor
		case value < 0:
			speed[index] = cprAssistantSpeedFloor
		}
	}
	return nil
}

// squaredDistanceToFeatures returns, for every node of an nx×ny×nz grid, the
// squared Euclidean distance in physical units to the nearest feature node
// (Felzenszwalb and Huttenlocher's separable lower-envelope transform).
func squaredDistanceToFeatures(ctx context.Context, features []bool, nx, ny, nz int, spacing Vec3) ([]float64, error) {
	out := make([]float64, len(features))
	for i, feature := range features {
		if !feature {
			out[i] = math.Inf(1)
		}
	}
	longest := max(nx, max(ny, nz))
	f := make([]float64, longest)
	d := make([]float64, longest)
	v := make([]int, longest)
	z := make([]float64, longest+1)
	pass := func(n, lines int, step float64, at func(line, i int) int) error {
		for line := 0; line < lines; line++ {
			if line&255 == 0 {
				if err := ctx.Err(); err != nil {
					return err
				}
			}
			for i := 0; i < n; i++ {
				f[i] = out[at(line, i)]
			}
			lowerEnvelope(f[:n], d[:n], v, z, step*step)
			for i := 0; i < n; i++ {
				out[at(line, i)] = d[i]
			}
		}
		return nil
	}
	if err := pass(nx, ny*nz, spacing.X, func(line, i int) int { return line*nx + i }); err != nil {
		return nil, err
	}
	if err := pass(ny, nx*nz, spacing.Y, func(line, i int) int { return (line/nx*ny+i)*nx + line%nx }); err != nil {
		return nil, err
	}
	if err := pass(nz, nx*ny, spacing.Z, func(line, i int) int { return i*nx*ny + line }); err != nil {
		return nil, err
	}
	return out, nil
}

// lowerEnvelope computes d[q] = min_p (q-p)²·w + f[p] for one line.
func lowerEnvelope(f, d []float64, v []int, z []float64, w float64) {
	n := len(f)
	k := -1
	for q := 0; q < n; q++ {
		if math.IsInf(f[q], 1) {
			continue
		}
		for k >= 0 {
			p := v[k]
			s := ((f[q] + w*float64(q*q)) - (f[p] + w*float64(p*p))) / (2 * w * float64(q-p))
			if s > z[k] {
				k++
				v[k], z[k], z[k+1] = q, s, math.Inf(1)
				break
			}
			k--
		}
		if k < 0 {
			k = 0
			v[0], z[0], z[1] = q, math.Inf(-1), math.Inf(1)
		}
	}
	if k < 0 {
		for q := range d {
			d[q] = math.Inf(1)
		}
		return
	}
	j := 0
	for q := 0; q < n; q++ {
		for z[j+1] < float64(q) {
			j++
		}
		p := v[j]
		d[q] = w*float64((q-p)*(q-p)) + f[p]
	}
}
