package roi

import (
	"image"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

// centreInsidePolygon is the per-pixel reference for RasterizePolygon: the
// even-odd crossing test at the pixel centre (x, y) with the same half-open
// edge rule as roiPointInPolygon.
func centreInsidePolygon(x, y int, points []Point2D) bool {
	px, py := float64(x), float64(y)
	inside := false
	for i := range points {
		current, previous := points[i], points[(i+len(points)-1)%len(points)]
		if (current.Y > py) != (previous.Y > py) {
			cross := (previous.X-current.X)*(py-current.Y)/(previous.Y-current.Y) + current.X
			if px < cross {
				inside = !inside
			}
		}
	}
	return inside
}

func centreSamplingReference(points []Point2D, columns, rows int) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if len(points) < 3 {
		return mask
	}
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			if centreInsidePolygon(x, y, points) {
				mask.Set(x, y, true)
			}
		}
	}
	return mask
}

func TestRasterizePolygonMatchesCentreSamplingReference(t *testing.T) {
	random := rand.New(rand.NewSource(1016))
	for iteration := 0; iteration < 500; iteration++ {
		points := make([]Point2D, 3+random.Intn(30))
		for i := range points {
			x, y := random.Float64()*97-24, random.Float64()*97-24
			// Put some vertices on pixel centres and pixel edges so ties
			// between centres and edges are exercised too.
			switch random.Intn(4) {
			case 0:
				x, y = math.Round(x), math.Round(y)
			case 1:
				x, y = math.Floor(x)+0.5, math.Floor(y)+0.5
			}
			points[i] = Point2D{X: x, Y: y}
		}
		got := RasterizePolygon(points, 48, 48)
		want := centreSamplingReference(points, 48, 48)
		for y := 0; y < 48; y++ {
			if !reflect.DeepEqual(got.Runs(y), want.Runs(y)) {
				t.Fatalf("iteration %d row %d differs\npoints=%v\ngot=%v\nwant=%v", iteration, y, points, got.Runs(y), want.Runs(y))
			}
		}
	}
}

// TestRasterizePolygonIntegerVerticesMatchVectorROI pins the compatibility of
// the shared filler: integer vertices give the VectorROI polygon mask.
func TestRasterizePolygonIntegerVerticesMatchVectorROI(t *testing.T) {
	random := rand.New(rand.NewSource(949))
	for iteration := 0; iteration < 300; iteration++ {
		points := make([]image.Point, 3+random.Intn(20))
		for i := range points {
			points[i] = image.Pt(random.Intn(97)-24, random.Intn(97)-24)
		}
		assertRasterMasksEqual(t, RasterizePolygon(imagePointsToPoint2D(points), 48, 48), VectorROI{Shape: ROIPolygon, Points: points}.Rasterize(48, 48))
	}
}

// TestRasterizePolygonSubPixelShiftedSquares checks exact centre sampling of
// axis-aligned squares at sub-pixel offsets: pixel (x, y) is set exactly when
// x0 <= x < x1 and y0 <= y < y1. Averaged over a uniform grid of offsets the
// pixel count equals the analytic area, whereas rounding the vertices first
// would, for example, turn [2.6, 7.4)^2 (area 23.04, 25 centres) into
// [3, 7)^2 with 16 pixels.
func TestRasterizePolygonSubPixelShiftedSquares(t *testing.T) {
	const side, steps = 5.5, 20
	total := 0
	for i := 0; i < steps; i++ {
		for j := 0; j < steps; j++ {
			x0 := 2 + float64(i)/steps
			y0 := 3 + float64(j)/steps
			x1, y1 := x0+side, y0+side
			mask := RasterizePolygon([]Point2D{{x0, y0}, {x1, y0}, {x1, y1}, {x0, y1}}, 16, 16)
			for y := 0; y < 16; y++ {
				for x := 0; x < 16; x++ {
					want := float64(x) >= x0 && float64(x) < x1 && float64(y) >= y0 && float64(y) < y1
					if mask.Get(x, y) != want {
						t.Fatalf("square at (%g, %g): pixel (%d, %d) = %v, want %v", x0, y0, x, y, mask.Get(x, y), want)
					}
				}
			}
			total += mask.Count()
		}
	}
	if mean := float64(total) / (steps * steps); math.Abs(mean-side*side) > 1e-9 {
		t.Fatalf("mean pixel count %g, want area %g", mean, side*side)
	}
	mask := RasterizePolygon([]Point2D{{2.6, 2.6}, {7.4, 2.6}, {7.4, 7.4}, {2.6, 7.4}}, 16, 16)
	if got := mask.Count(); got != 25 || !mask.Get(3, 3) || !mask.Get(7, 7) || mask.Get(8, 7) {
		t.Fatalf("[2.6, 7.4)^2 has %d pixels, want the 25 centres 3..7", got)
	}
}

// TestRasterizePolygonCircleAreaMatchesAnalytic rasterizes finely sampled
// circles with sub-pixel centres: every pixel count stays within the lattice
// point error of pi*r^2, and the mean over sub-pixel offsets is within 0.5%.
func TestRasterizePolygonCircleAreaMatchesAnalytic(t *testing.T) {
	const vertices, offsets = 1440, 6
	for _, radius := range []float64{3.3, 7.7, 20.25, 45.6} {
		area := math.Pi * radius * radius
		sum := 0.0
		for i := 0; i < offsets; i++ {
			for j := 0; j < offsets; j++ {
				cx := 50 + float64(i)/offsets + 0.013
				cy := 50 + float64(j)/offsets + 0.029
				points := make([]Point2D, vertices)
				for k := range points {
					angle := 2 * math.Pi * float64(k) / vertices
					points[k] = Point2D{X: cx + radius*math.Cos(angle), Y: cy + radius*math.Sin(angle)}
				}
				count := float64(RasterizePolygon(points, 101, 101).Count())
				// A lattice count differs from the area by less than the
				// area of the band of half-width 1/sqrt(2) around the edge.
				if limit := 2 * math.Pi * radius * math.Sqrt2 / 2; math.Abs(count-area) > limit {
					t.Fatalf("r=%g centre (%g, %g): %g pixels, area %g", radius, cx, cy, count, area)
				}
				sum += count
			}
		}
		if mean := sum / (offsets * offsets); math.Abs(mean-area)/area > 0.005 {
			t.Fatalf("r=%g: mean %g pixels, area %g", radius, mean, area)
		}
	}
}

// TestRasterizePolygonSharedEdgesTile checks the tie rule: polygons that share
// an edge, including edges through pixel centres, partition the pixels of
// their union without gaps or double coverage.
func TestRasterizePolygonSharedEdgesTile(t *testing.T) {
	quad := []Point2D{{1.25, 2}, {17, 1.5}, {18.5, 14}, {2, 16}}
	cases := map[string][2][]Point2D{
		"slanted diagonal": {
			{quad[0], quad[1], quad[2]},
			{quad[0], quad[2], quad[3]},
		},
		"vertical split on centres": {
			{{2, 2}, {9, 2}, {9, 12}, {2, 12}},
			{{9, 2}, {15, 2}, {15, 12}, {9, 12}},
		},
		"horizontal split on centres": {
			{{2, 2}, {15, 2}, {15, 7}, {2, 7}},
			{{2, 7}, {15, 7}, {15, 12}, {2, 12}},
		},
	}
	for name, polygons := range cases {
		t.Run(name, func(t *testing.T) {
			a := RasterizePolygon(polygons[0], 20, 20)
			b := RasterizePolygon(polygons[1], 20, 20)
			both := a.Clone()
			both.Intersect(b)
			if !both.Empty() {
				t.Fatalf("%d pixels covered twice", both.Count())
			}
			union := a.Clone()
			union.Union(b)
			var whole []Point2D
			if name == "slanted diagonal" {
				whole = quad
			} else {
				whole = boundingSquare(polygons[0], polygons[1])
			}
			assertRasterMasksEqual(t, union, RasterizePolygon(whole, 20, 20))
		})
	}
}

func boundingSquare(polygons ...[]Point2D) []Point2D {
	minX, minY, maxX, maxY := math.Inf(1), math.Inf(1), math.Inf(-1), math.Inf(-1)
	for _, polygon := range polygons {
		for _, point := range polygon {
			minX, maxX = math.Min(minX, point.X), math.Max(maxX, point.X)
			minY, maxY = math.Min(minY, point.Y), math.Max(maxY, point.Y)
		}
	}
	return []Point2D{{minX, minY}, {maxX, minY}, {maxX, maxY}, {minX, maxY}}
}

// TestRasterizePolygonXORGivesEvenOddHoles checks that XOR-combining the
// masks of an outer contour and an inner contour (the RTSTRUCT
// CLOSEDPLANAR_XOR rule) is the even-odd fill of both, i.e. a hole.
func TestRasterizePolygonXORGivesEvenOddHoles(t *testing.T) {
	outer := []Point2D{{1.4, 1.2}, {20.7, 2.1}, {19.3, 21.6}, {2.2, 20.4}}
	inner := []Point2D{{6.5, 7.25}, {14.1, 6.8}, {13.6, 14.9}, {7.3, 13.5}}
	mask := RasterizePolygon(outer, 24, 24)
	mask.XOR(RasterizePolygon(inner, 24, 24))
	for y := 0; y < 24; y++ {
		for x := 0; x < 24; x++ {
			want := centreInsidePolygon(x, y, outer) != centreInsidePolygon(x, y, inner)
			if mask.Get(x, y) != want {
				t.Fatalf("pixel (%d, %d) = %v, want %v", x, y, mask.Get(x, y), want)
			}
		}
	}
	if mask.Get(10, 10) || !mask.Get(3, 3) {
		t.Fatal("inner contour is not a hole")
	}
}

// TestRasterizePolygonSnapsRoundOffOntoPixelCentres checks that coordinate
// round-off (here up to 1e-9 px) does not move edge ties, while an offset of
// 1e-4 px is a real sub-pixel position and does.
func TestRasterizePolygonSnapsRoundOffOntoPixelCentres(t *testing.T) {
	integer := []Point2D{{3, 2}, {9, 2}, {9, 7}, {3, 7}}
	want := RasterizePolygon(integer, 12, 12)
	noisy := []Point2D{{3 + 1e-9, 2 + 4e-13}, {9 - 3e-12, 2 - 1e-9}, {9 + 1e-10, 7 - 2e-10}, {3 + 5e-13, 7 + 1e-9}}
	assertRasterMasksEqual(t, RasterizePolygon(noisy, 12, 12), want)
	shifted := RasterizePolygon([]Point2D{{3 + 1e-4, 2}, {9, 2}, {9, 7}, {3 + 1e-4, 7}}, 12, 12)
	if shifted.Get(3, 2) || !shifted.Get(4, 2) || want.Count()-shifted.Count() != 5 {
		t.Fatalf("a 1e-4 px offset must exclude column 3: %d pixels, integer square %d", shifted.Count(), want.Count())
	}
}

func TestRasterizePolygonDegenerateInput(t *testing.T) {
	square := []Point2D{{1, 1}, {5, 1}, {5, 5}, {1, 5}}
	cases := []struct {
		name          string
		points        []Point2D
		columns, rows int
		want          int
	}{
		{"nil", nil, 8, 8, 0},
		{"two points", square[:2], 8, 8, 0},
		{"zero area", []Point2D{{1, 1}, {5, 5}, {3, 3}}, 8, 8, 0},
		{"NaN vertex", []Point2D{{1, 1}, {math.NaN(), 1}, {5, 5}}, 8, 8, 0},
		{"infinite vertex", []Point2D{{1, 1}, {math.Inf(1), 1}, {5, 5}}, 8, 8, 0},
		{"empty grid", square, 0, 0, 0},
		{"outside grid", []Point2D{{20, 20}, {30, 20}, {30, 30}}, 8, 8, 0},
		{"covers grid", []Point2D{{-1e300, -1e300}, {1e300, -1e300}, {1e300, 1e300}, {-1e300, 1e300}}, 8, 8, 64},
		{"clipped", []Point2D{{-3.5, -3.5}, {2.5, -3.5}, {2.5, 2.5}, {-3.5, 2.5}}, 8, 8, 9},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mask := RasterizePolygon(tc.points, tc.columns, tc.rows)
			if got := mask.Count(); got != tc.want {
				t.Fatalf("count = %d, want %d", got, tc.want)
			}
			if mask.Columns != max(tc.columns, 0) || mask.Rows != max(tc.rows, 0) {
				t.Fatalf("mask size %dx%d", mask.Columns, mask.Rows)
			}
		})
	}
}

// pointSegmentDistance2D is the Euclidean distance from p to segment ab.
func pointSegmentDistance2D(p, a, b Point2D) float64 {
	dx, dy := b.X-a.X, b.Y-a.Y
	t := 0.0
	if length2 := dx*dx + dy*dy; length2 > 0 {
		t = math.Max(0, math.Min(1, ((p.X-a.X)*dx+(p.Y-a.Y)*dy)/length2))
	}
	return math.Hypot(p.X-a.X-t*dx, p.Y-a.Y-t*dy)
}

// TestSubPixelPolygonRasterMatchesVTKStencil compares the sub-pixel filler
// with the vtkLassoStencilSource baselines. The exact float vertices agree
// with VTK except on centres lying on an edge (VTK closes those), and after
// jittering every vertex by up to 0.05 px the masks still differ only at
// centres within that jitter of the original outline, so no rounding of
// vertices to the pixel grid is taking place.
func TestSubPixelPolygonRasterMatchesVTKStencil(t *testing.T) {
	const jitter = 0.05
	fixture := loadStencilFixture(t)
	random := rand.New(rand.NewSource(2026))
	for _, p := range fixture.Polygons {
		t.Run(p.Name, func(t *testing.T) {
			exact := make([]Point2D, len(p.Vertices))
			jittered := make([]Point2D, len(p.Vertices))
			for i, v := range p.Vertices {
				exact[i] = Point2D{X: float64(v[0]), Y: float64(v[1])}
				jittered[i] = Point2D{
					X: exact[i].X + jitter*(2*random.Float64()-1),
					Y: exact[i].Y + jitter*(2*random.Float64()-1),
				}
			}
			nearOutline := func(limit float64) func(x, y int) bool {
				return func(x, y int) bool {
					centre := Point2D{X: float64(x), Y: float64(y)}
					for i := range exact {
						if pointSegmentDistance2D(centre, exact[i], exact[(i+1)%len(exact)]) <= limit {
							return true
						}
					}
					return false
				}
			}
			mask := RasterizePolygon(exact, fixture.Width, fixture.Height)
			maskDifferences(t, mask, p.Mask, fixture.Width, fixture.Height, nearOutline(1e-9))
			moved := maskDifferences(t, RasterizePolygon(jittered, fixture.Width, fixture.Height), p.Mask, fixture.Width, fixture.Height, nearOutline(jitter*math.Sqrt2))
			t.Logf("%d centres near the jittered outline resolved differently", moved)
		})
	}
}

func BenchmarkRasterizePolygonSubPixelCircle512(b *testing.B) {
	points := make([]Point2D, 1024)
	for k := range points {
		angle := 2 * math.Pi * float64(k) / float64(len(points))
		points[k] = Point2D{X: 255.3 + 190*math.Cos(angle), Y: 255.7 + 190*math.Sin(angle)}
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		benchmarkRasterMask = RasterizePolygon(points, 512, 512)
	}
}
