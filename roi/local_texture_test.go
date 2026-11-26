package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
)

// The oracle enumerates every ordered pair in a window, independently of the
// production offset traversal and dense histogram. It retains only a sparse
// probability table and evaluates the published feature formulas directly.
func directLocalTexture(m *VolumeMask, values []float64, x, y, z int, o LocalTextureOptions) float64 {
	nan := math.NaN()
	index := func(x, y, z int) int { return (z*m.Rows+y)*m.Columns + x }
	eligible := func(i int) bool {
		return m.Voxels[i] && finite(values[i]) && (o.LowerBound == nil || values[i] >= *o.LowerBound)
	}
	if !eligible(index(x, y, z)) {
		return nan
	}
	radius := [3]int{}
	for a, axis := range [][]float64{m.X, m.Y, m.Z} {
		if len(axis) > 1 {
			radius[a] = int(math.Floor(o.RadiusMM[a]/(axis[1]-axis[0]) + 1e-10))
		}
	}
	positions := [][3]int{}
	for k := 0; k < m.Slices; k++ {
		for j := 0; j < m.Rows; j++ {
			for i := 0; i < m.Columns; i++ {
				if abs(i-x) <= radius[0] && abs(j-y) <= radius[1] && abs(k-z) <= radius[2] && eligible(index(i, j, k)) {
					positions = append(positions, [3]int{i, j, k})
				}
			}
		}
	}
	origin := math.Inf(1)
	if o.Quantization == LocalTextureWindow {
		for _, p := range positions {
			origin = math.Min(origin, values[index(p[0], p[1], p[2])])
		}
	} else {
		for i, v := range values {
			if eligible(i) {
				origin = math.Min(origin, v)
			}
		}
		if o.LowerBound != nil {
			origin = *o.LowerBound
		}
	}
	dirs := o.Directions
	if len(dirs) == 0 {
		dirs = textureDirections[:]
	}
	merged := map[[2]int]int{}
	features := []float64{}
	evaluate := func(h map[[2]int]int) float64 {
		total := 0
		for _, c := range h {
			total += c
		}
		s := 0.
		for k, c := range h {
			p := float64(c) / float64(total)
			d := float64(k[0] - k[1])
			switch o.Feature {
			case LocalContrast:
				s += p * d * d
			case LocalEnergy:
				s += p * p
			case LocalEntropy:
				s -= p * math.Log2(p)
			case LocalHomogeneity:
				s += p / (1 + d*d)
			}
		}
		return s
	}
	for _, dir := range dirs {
		h := map[[2]int]int{}
		for _, a := range positions {
			for _, b := range positions {
				d := [3]int{b[0] - a[0], b[1] - a[1], b[2] - a[2]}
				negative := [3]int{-dir[0], -dir[1], -dir[2]}
				if d != dir && d != negative {
					continue
				}
				key := [2]int{int(math.Floor((values[index(a[0], a[1], a[2])] - origin) / o.BinWidth)), int(math.Floor((values[index(b[0], b[1], b[2])] - origin) / o.BinWidth))}
				h[key]++
			}
		}
		if len(h) > 0 {
			features = append(features, evaluate(h))
			for k, c := range h {
				merged[k] += c
			}
		}
	}
	if len(features) == 0 {
		return nan
	}
	if o.Aggregation == TextureMerged {
		return evaluate(merged)
	}
	s := 0.
	for _, v := range features {
		s += v
	}
	return s / float64(len(features))
}

func TestLocalTextureDirectOracleRegionsAndDeterminism(t *testing.T) {
	m := NewVolumeMask(5, 4, 3, [3]float64{.8, 1.3, 2})
	values := make([]float64, len(m.Voxels))
	for _, pattern := range []string{"constant", "periodic", "heterogeneous", "partial mask", "non finite"} {
		t.Run(pattern, func(t *testing.T) {
			for i := range values {
				m.Voxels[i] = true
				switch pattern {
				case "constant":
					values[i] = 7.3
				case "periodic":
					values[i] = float64(i%2) * 3
				default:
					values[i] = float64((i*13)%23) / 3
				}
				if pattern == "partial mask" {
					m.Voxels[i] = i%3 != 0
				}
				if pattern == "non finite" && i%7 == 0 {
					values[i] = math.NaN()
				}
			}
			input := append([]float64(nil), values...)
			mask := m.Clone()
			for _, f := range []LocalTextureFeature{LocalContrast, LocalEntropy, LocalEnergy, LocalHomogeneity} {
				for _, q := range []LocalTextureQuantization{LocalTextureGlobal, LocalTextureWindow} {
					for _, agg := range []TextureAggregation{TextureAveraged, TextureMerged} {
						o := LocalTextureOptions{Feature: f, RadiusMM: [3]float64{1.4, 1.8, 2.5}, BinWidth: .75, Quantization: q, Aggregation: agg, Workers: 1}
						at := func(x, y, z int) (float64, bool) { return values[(z*m.Rows+y)*m.Columns+x], true }
						full, e := m.LocalTexture(context.Background(), at, nil, o)
						if e != nil {
							t.Fatal(e)
						}
						for i, v := range full.Values {
							expected := directLocalTexture(m, values, i%m.Columns, i/m.Columns%m.Rows, i/(m.Columns*m.Rows), o)
							if math.IsNaN(expected) {
								if !math.IsNaN(float64(v)) {
									t.Fatal("undefined output", i, v)
								}
							} else if math.Abs(float64(v)-expected) > 2e-6*math.Max(1, math.Abs(expected)) {
								t.Fatalf("%s/%s/%d at %d: %g want %g", f, q, agg, i, v, expected)
							}
						}
						o.Workers = 4
						region := LocalTextureRegion{Min: [3]int{1, 1, 1}, Max: [3]int{4, 3, 3}}
						part, e := m.LocalTexture(context.Background(), at, &region, o)
						if e != nil {
							t.Fatal(e)
						}
						for i, v := range part.Values {
							x, y, z := i%3+1, i/3%2+1, i/6+1
							if math.Float32bits(v) != math.Float32bits(full.Values[(z*m.Rows+y)*m.Columns+x]) {
								t.Fatal("regional/worker mismatch", i)
							}
						}
					}
				}
			}
			if !reflect.DeepEqual(mask, m) {
				t.Fatal("input mask changed")
			}
			for i, v := range input {
				if math.Float64bits(v) != math.Float64bits(values[i]) {
					t.Fatal("intensity changed")
				}
			}
		})
	}
}

func TestLocalTextureValidityAdmissionAndCancellation(t *testing.T) {
	m := NewVolumeMask(4, 4, 1, [3]float64{.5, 2, 1})
	for i := range m.Voxels {
		m.Voxels[i] = true
	}
	o := LocalTextureOptions{RadiusMM: [3]float64{.5, 2, 0}, BinWidth: 1, Directions: [][3]int{{1, 0, 0}}}
	p, e := m.PlanLocalTexture(nil, o)
	if e != nil || p.RadiusVoxels != [3]int{1, 1, 0} {
		t.Fatal(p, e)
	}
	calls := 0
	at := func(x, y, z int) (float64, bool) { calls++; return 0, true }
	o.MaxWorkingBytes = p.ReservedBytes - 1
	if r, e := m.LocalTexture(nil, at, nil, o); r != nil || !errors.Is(e, ErrLocalTextureLimit) || calls != 0 {
		t.Fatal("allocation admission", r, e, calls)
	}
	o.MaxWorkingBytes = 0
	o.MaxVisits = p.WorstCaseVisits - 1
	if r, e := m.LocalTexture(nil, at, nil, o); r != nil || !errors.Is(e, ErrLocalTextureLimit) || calls != 0 {
		t.Fatal("cost admission", r, e, calls)
	}
	o.MaxVisits = 0
	o.Directions = [][3]int{{1, 0, 0}, {-1, 0, 0}}
	if _, e := m.PlanLocalTexture(nil, o); !errors.Is(e, ErrInvalidTexture) {
		t.Fatal("duplicate direction", e)
	}
	o.Directions = nil
	o.RadiusMM = [3]float64{}
	r, e := m.LocalTexture(nil, at, nil, o)
	if e != nil {
		t.Fatal(e)
	}
	for _, v := range r.Values {
		if !math.IsNaN(float64(v)) {
			t.Fatal("no pairs represented as zero")
		}
	}
	o.RadiusMM = [3]float64{.5, 2, 0}
	ctx, cancel := context.WithCancel(context.Background())
	at = func(x, y, z int) (float64, bool) { cancel(); return 0, true }
	if r, e := m.LocalTexture(ctx, at, nil, o); r != nil || !errors.Is(e, context.Canceled) {
		t.Fatal("partial cancellation", r, e)
	}
	m.X[2] += .1
	if _, e := m.PlanLocalTexture(nil, o); !errors.Is(e, ErrInvalidVolumeMask) {
		t.Fatal("irregular accepted", e)
	}
}

func BenchmarkLocalTextureMap(b *testing.B) {
	m := NewVolumeMask(24, 24, 24, [3]float64{1, 1, 1})
	for i := range m.Voxels {
		m.Voxels[i] = true
	}
	o := LocalTextureOptions{RadiusMM: [3]float64{1, 1, 1}, BinWidth: 1, MaxGreyLevels: 8, Workers: 4}
	at := func(x, y, z int) (float64, bool) { return float64((x + 2*y + 3*z) % 8), true }
	b.ReportAllocs()
	b.SetBytes(int64(len(m.Voxels)) * 4)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := m.LocalTexture(context.Background(), at, nil, o); e != nil {
			b.Fatal(e)
		}
	}
}
