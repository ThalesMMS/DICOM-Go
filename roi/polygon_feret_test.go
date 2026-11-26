package roi

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"reflect"
	"testing"
)

func TestPolygonFeretRotatedRectangleAndIndependentAllPairs(t *testing.T) {
	angle := .73
	points := []Point2D{{}, {X: 4}, {X: 4, Y: 2}, {Y: 2}, {X: 1, Y: 1}, {}}
	for i, p := range points {
		points[i] = Point2D{X: 13 + p.X*math.Cos(angle) - p.Y*math.Sin(angle), Y: -9 + p.X*math.Sin(angle) + p.Y*math.Cos(angle)}
	}
	original := append([]Point2D(nil), points...)
	minimum, maximum, err := PolygonFeretDiametersContext(context.Background(), points)
	if err != nil || math.Abs(minimum-2) > 1e-12 || math.Abs(maximum-math.Sqrt(20)) > 1e-12 || !reflect.DeepEqual(points, original) {
		t.Fatal(minimum, maximum, err)
	}
	rng := rand.New(rand.NewSource(1350))
	for trial := 0; trial < 20; trial++ {
		points = nil
		for i := 0; i < 30; i++ {
			points = append(points, Point2D{X: rng.Float64() * 11, Y: rng.Float64() * 7})
		}
		minimum, maximum, err = PolygonFeretDiametersContext(context.Background(), points)
		if err != nil {
			t.Fatal(err)
		}
		wantMax, wantMin := 0.0, math.Inf(1)
		for i, a := range points {
			for _, b := range points[i+1:] {
				dx, dy := b.X-a.X, b.Y-a.Y
				length := math.Hypot(dx, dy)
				wantMax = math.Max(wantMax, length)
				lo, hi := math.Inf(1), math.Inf(-1)
				for _, p := range points {
					projection := (-dy*p.X + dx*p.Y) / length
					lo = math.Min(lo, projection)
					hi = math.Max(hi, projection)
				}
				wantMin = math.Min(wantMin, hi-lo)
			}
		}
		if math.Abs(maximum-wantMax) > 1e-10 || math.Abs(minimum-wantMin) > 1e-10 {
			t.Fatal(trial, minimum, maximum, wantMin, wantMax)
		}
	}
}

func TestPolygonFeretRejectsDegenerateAndCancelledGeometry(t *testing.T) {
	for _, p := range [][]Point2D{nil, {{}, {X: 1}, {X: 2}}, {{}, {X: math.NaN()}, {Y: 1}}} {
		if _, _, err := PolygonFeretDiametersContext(context.Background(), p); !errors.Is(err, ErrPolygonFeret) {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := PolygonFeretDiametersContext(ctx, []Point2D{{}, {X: 1}, {Y: 1}}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}
