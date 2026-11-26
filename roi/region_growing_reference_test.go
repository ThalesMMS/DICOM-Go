package roi

import (
	"context"
	"math"
	"math/rand"
	"testing"
)

// The reference below is a direct dense re-expression of ITK's
// RegionGrowing filters (commit 35b196e1, Apache-2.0, Copyright NumFOCUS):
// a breadth-first flood over a dense visited image, the clamped-box seed
// statistics of ConfidenceConnectedImageFilter, the clamped-box test of
// NeighborhoodBinaryThresholdImageFunction and the bisection of
// IsolatedConnectedImageFilter. The span-fill implementation must produce
// the same regions and values on random volumes.

type denseVolume struct {
	columns, rows, slices int
	values                []float64
}

func (d denseVolume) at(x, y, z int) float64 {
	return d.values[(z*d.rows+y)*d.columns+x]
}

func (d denseVolume) volume() RegionGrowVolume {
	return RegionGrowVolume{Columns: d.columns, Rows: d.rows, Slices: d.slices, ValueAt: func(x, y, z int) (float64, bool) {
		return d.at(x, y, z), true
	}}
}

func randomDenseVolume(rng *rand.Rand, levels int) denseVolume {
	d := denseVolume{columns: 3 + rng.Intn(10), rows: 3 + rng.Intn(10), slices: 1 + rng.Intn(4)}
	d.values = make([]float64, d.columns*d.rows*d.slices)
	// Smooth-ish values so regions are neither trivial nor a single voxel.
	for i := range d.values {
		d.values[i] = float64(rng.Intn(levels))
		if i > 0 && rng.Intn(3) > 0 {
			d.values[i] = d.values[i-1]
		}
	}
	return d
}

func referenceOffsets(connectivity Connectivity3D) [][3]int {
	var out [][3]int
	for dz := -1; dz <= 1; dz++ {
		for dy := -1; dy <= 1; dy++ {
			for dx := -1; dx <= 1; dx++ {
				n := abs(dx) + abs(dy) + abs(dz)
				if n == 0 || (connectivity == Connectivity6 && n > 1) || (connectivity == Connectivity18 && n > 2) {
					continue
				}
				out = append(out, [3]int{dx, dy, dz})
			}
		}
	}
	return out
}

// referenceFlood is ITK's FloodFilledFunctionConditionalConstIterator: seeds
// inside the image that satisfy include start a queue over a dense visited
// image.
func referenceFlood(d denseVolume, seeds []RegionGrowSeed, connectivity Connectivity3D, include func(x, y, z int) bool) []bool {
	out := make([]bool, len(d.values))
	visited := make([]bool, len(d.values))
	index := func(x, y, z int) int { return (z*d.rows+y)*d.columns + x }
	var queue [][3]int
	for _, s := range seeds {
		if s.X < 0 || s.Y < 0 || s.Slice < 0 || s.X >= d.columns || s.Y >= d.rows || s.Slice >= d.slices {
			continue
		}
		if i := index(s.X, s.Y, s.Slice); !visited[i] && include(s.X, s.Y, s.Slice) {
			visited[i], out[i] = true, true
			queue = append(queue, [3]int{s.X, s.Y, s.Slice})
		}
	}
	offsets := referenceOffsets(connectivity)
	for len(queue) > 0 {
		p := queue[0]
		queue = queue[1:]
		for _, o := range offsets {
			x, y, z := p[0]+o[0], p[1]+o[1], p[2]+o[2]
			if x < 0 || y < 0 || z < 0 || x >= d.columns || y >= d.rows || z >= d.slices {
				continue
			}
			i := index(x, y, z)
			if visited[i] {
				continue
			}
			visited[i] = true
			if include(x, y, z) {
				out[i] = true
				queue = append(queue, [3]int{x, y, z})
			}
		}
	}
	return out
}

func clampInt(v, lo, hi int) int { return min(max(v, lo), hi) }

func referenceNeighborhood(d denseVolume, seeds []RegionGrowSeed, lower, upper float64, radius NeighborhoodRadius, connectivity Connectivity3D) []bool {
	if d.slices == 1 {
		radius.Slice = 0
	}
	return referenceFlood(d, seeds, connectivity, func(x, y, z int) bool {
		for dz := -radius.Slice; dz <= radius.Slice; dz++ {
			for dy := -radius.Y; dy <= radius.Y; dy++ {
				for dx := -radius.X; dx <= radius.X; dx++ {
					v := d.at(clampInt(x+dx, 0, d.columns-1), clampInt(y+dy, 0, d.rows-1), clampInt(z+dz, 0, d.slices-1))
					if lower > v || v > upper {
						return false
					}
				}
			}
		}
		return true
	})
}

type referenceConfidence struct {
	region       []bool
	mean, varnce float64
	lower, upper float64
}

func referenceConfidenceConnected(d denseVolume, seeds []RegionGrowSeed, p ConfidenceConnectedParams, connectivity Connectivity3D) referenceConfidence {
	r := UniformNeighborhoodRadius(p.InitialRadius)
	if d.slices == 1 {
		r.Slice = 0
	}
	var mean, sumSquares float64
	num := 0
	boxSize := (2*r.X + 1) * (2*r.Y + 1) * (2*r.Slice + 1)
	lowest, highest := math.Inf(1), math.Inf(-1)
	for _, s := range seeds {
		var sum, squares float64
		for dz := -r.Slice; dz <= r.Slice; dz++ {
			for dy := -r.Y; dy <= r.Y; dy++ {
				for dx := -r.X; dx <= r.X; dx++ {
					v := d.at(clampInt(s.X+dx, 0, d.columns-1), clampInt(s.Y+dy, 0, d.rows-1), clampInt(s.Slice+dz, 0, d.slices-1))
					sum += v
					squares += v * v
				}
			}
		}
		mean += sum / float64(boxSize)
		sumSquares += squares
		num++
		lowest = math.Min(lowest, d.at(s.X, s.Y, s.Slice))
		highest = math.Max(highest, d.at(s.X, s.Y, s.Slice))
	}
	total := float64(num * boxSize)
	mean /= float64(num)
	variance := 0.0
	if total > 1 {
		variance = math.Max(0, (sumSquares-mean*mean*total)/(total-1))
	}
	bounds := func(m, v float64) (float64, float64) {
		lo, hi := m-p.Multiplier*math.Sqrt(v), m+p.Multiplier*math.Sqrt(v)
		return math.Min(lo, lowest), math.Max(hi, highest)
	}
	lower, upper := bounds(mean, variance)
	include := func(lo, hi float64) func(x, y, z int) bool {
		return func(x, y, z int) bool { v := d.at(x, y, z); return v >= lo && v <= hi }
	}
	region := referenceFlood(d, seeds, connectivity, include(lower, upper))
	out := referenceConfidence{region: region, mean: mean, varnce: variance, lower: lower, upper: upper}
	for loop := 0; loop < p.Iterations; loop++ {
		var sum, squares float64
		n := 0
		for i, in := range region {
			if in {
				sum += d.values[i]
				squares += d.values[i] * d.values[i]
				n++
			}
		}
		if n == 0 {
			break
		}
		m := sum / float64(n)
		v := 0.0
		if n > 1 {
			v = math.Max(0, (squares-sum*sum/float64(n))/float64(n-1))
		}
		if v <= 0.1*epsilon64 {
			break
		}
		lower, upper = bounds(m, v)
		region = referenceFlood(d, seeds, connectivity, include(lower, upper))
		out = referenceConfidence{region: region, mean: m, varnce: v, lower: lower, upper: upper}
	}
	return out
}

func referenceIsolated(d denseVolume, seeds1, seeds2 []RegionGrowSeed, p IsolatedConnectedParams, connectivity Connectivity3D) ([]bool, float64) {
	index := func(s RegionGrowSeed) int { return (s.Slice*d.rows+s.Y)*d.columns + s.X }
	fill := func(lo, hi float64) []bool {
		return referenceFlood(d, seeds1, connectivity, func(x, y, z int) bool { v := d.at(x, y, z); return v >= lo && v <= hi })
	}
	hits := func(region []bool) bool {
		for _, s := range seeds2 {
			if region[index(s)] {
				return true
			}
		}
		return false
	}
	lower, upper := p.Lower, p.Upper
	if p.FindUpperThreshold {
		for guess := upper; lower+p.Tolerance < guess; guess = (lower + upper) / 2 {
			if hits(fill(p.Lower, guess)) {
				upper = guess
			} else {
				lower = guess
			}
		}
		return fill(p.Lower, lower), lower
	}
	for guess := lower; guess < upper-p.Tolerance; guess = (lower + upper) / 2 {
		if hits(fill(guess, p.Upper)) {
			lower = guess
		} else {
			upper = guess
		}
	}
	return fill(upper, p.Upper), upper
}

func compareDense(t *testing.T, label string, d denseVolume, masks map[int]*RasterMask, count int, want []bool) {
	t.Helper()
	wantCount := 0
	for z := 0; z < d.slices; z++ {
		for y := 0; y < d.rows; y++ {
			for x := 0; x < d.columns; x++ {
				w := want[(z*d.rows+y)*d.columns+x]
				if w {
					wantCount++
				}
				if masks[z].Get(x, y) != w {
					t.Fatalf("%s: voxel (%d,%d,%d) = %v, reference %v", label, x, y, z, !w, w)
				}
			}
		}
	}
	if count != wantCount {
		t.Fatalf("%s: VoxelCount %d, reference %d", label, count, wantCount)
	}
}

func randomSeeds(rng *rand.Rand, d denseVolume, n int) []RegionGrowSeed {
	seeds := make([]RegionGrowSeed, n)
	for i := range seeds {
		seeds[i] = RegionGrowSeed{X: rng.Intn(d.columns), Y: rng.Intn(d.rows), Slice: rng.Intn(d.slices)}
	}
	return seeds
}

func TestRegionGrowingMatchesDenseITKReference(t *testing.T) {
	rng := rand.New(rand.NewSource(1095))
	connectivities := []Connectivity3D{Connectivity6, Connectivity18, Connectivity26}
	ctx := context.Background()
	for trial := 0; trial < 300; trial++ {
		d := randomDenseVolume(rng, 6)
		connectivity := connectivities[trial%3]
		options := RegionGrowOptions{Connectivity: connectivity}
		seeds := randomSeeds(rng, d, 1+rng.Intn(3))

		lower := float64(rng.Intn(6))
		upper := lower + float64(rng.Intn(4))
		threshold, err := GrowConnectedThreshold(ctx, d.volume(), seeds, lower, upper, options)
		if err != nil {
			t.Fatal(err)
		}
		compareDense(t, "threshold", d, threshold.Masks, threshold.VoxelCount,
			referenceFlood(d, seeds, connectivity, func(x, y, z int) bool { v := d.at(x, y, z); return v >= lower && v <= upper }))

		radius := NeighborhoodRadius{X: rng.Intn(2), Y: rng.Intn(2), Slice: rng.Intn(2)}
		neighborhood, err := GrowNeighborhoodConnected(ctx, d.volume(), seeds, lower, upper+1, radius, options)
		if err != nil {
			t.Fatal(err)
		}
		compareDense(t, "neighbourhood", d, neighborhood.Masks, neighborhood.VoxelCount,
			referenceNeighborhood(d, seeds, lower, upper+1, radius, connectivity))

		params := ConfidenceConnectedParams{Multiplier: 0.5 + 2*rng.Float64(), Iterations: rng.Intn(5), InitialRadius: rng.Intn(3)}
		confidence, err := GrowConfidenceConnected(ctx, d.volume(), seeds, params, options)
		if err != nil {
			t.Fatal(err)
		}
		ref := referenceConfidenceConnected(d, seeds, params, connectivity)
		compareDense(t, "confidence", d, confidence.Masks, confidence.VoxelCount, ref.region)
		if math.Abs(confidence.Mean-ref.mean) > 1e-9 || math.Abs(confidence.Variance-ref.varnce) > 1e-9 ||
			math.Abs(confidence.Lower-ref.lower) > 1e-9 || math.Abs(confidence.Upper-ref.upper) > 1e-9 {
			t.Fatalf("confidence statistics %g/%g [%g, %g], reference %g/%g [%g, %g]",
				confidence.Mean, confidence.Variance, confidence.Lower, confidence.Upper, ref.mean, ref.varnce, ref.lower, ref.upper)
		}

		seeds2 := randomSeeds(rng, d, 1+rng.Intn(2))
		iso := IsolatedConnectedParams{Lower: 0, Upper: 6, FindUpperThreshold: trial%2 == 0, Tolerance: 0.25}
		isolated, err := GrowIsolatedConnected(ctx, d.volume(), seeds, seeds2, iso, options)
		if err != nil {
			t.Fatal(err)
		}
		region, value := referenceIsolated(d, seeds, seeds2, iso, connectivity)
		compareDense(t, "isolated", d, isolated.Masks, isolated.VoxelCount, region)
		if isolated.IsolatedValue != value {
			t.Fatalf("isolated value %g, reference %g", isolated.IsolatedValue, value)
		}
	}
}
