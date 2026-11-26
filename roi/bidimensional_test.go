package roi

import (
	"math"
	"math/rand"
	"testing"
)

func assertBidimensionalConstraint(t *testing.T, b Bidimensional, sp MeasureSpacing, context string) {
	t.Helper()
	if e := b.PerpendicularityError(sp); !(math.Abs(e) < 1e-7) {
		t.Fatalf("%s: axes %.9f° from perpendicular: %+v", context, e, b)
	}
	f, ok := b.frame(sp)
	if !ok {
		t.Fatalf("%s: invalid frame %+v", context, b)
	}
	if f.t < -1e-9 || f.t > 1+1e-9 {
		t.Fatalf("%s: short axis crosses the long axis at t=%v, outside it", context, f.t)
	}
	if f.d0*f.d1 > 0 && math.Min(math.Abs(f.d0), math.Abs(f.d1)) > 1e-9*math.Max(1, f.length) {
		t.Fatalf("%s: short axis endpoints on the same side (%v, %v)", context, f.d0, f.d1)
	}
}

func TestNewBidimensionalPlacesASymmetricPerpendicularShortAxis(t *testing.T) {
	sp := MeasureSpacing{X: 0.5, Y: 1}
	b, ok := NewBidimensional(Point2D{X: 10, Y: 10}, Point2D{X: 30, Y: 10}, Point2D{X: 16, Y: 14}, sp)
	if !ok {
		t.Fatal("NewBidimensional failed")
	}
	if b.ShortStart != (Point2D{X: 16, Y: 14}) || math.Abs(b.ShortEnd.X-16) > 1e-9 || math.Abs(b.ShortEnd.Y-6) > 1e-9 {
		t.Fatalf("short axis = %v → %v, want (16,14) → (16,6)", b.ShortStart, b.ShortEnd)
	}
	long, short, ok := BidimensionalAxesMM(b, sp)
	if !ok || math.Abs(long-10) > 1e-9 || math.Abs(short-8) > 1e-9 {
		t.Fatalf("axes = %v × %v mm, want 10 × 8", long, short)
	}
	// A point beyond the long axis puts the short axis at its end.
	b, ok = NewBidimensional(Point2D{X: 10, Y: 10}, Point2D{X: 30, Y: 10}, Point2D{X: 40, Y: 12}, sp)
	if !ok || math.Abs(b.ShortStart.X-30) > 1e-9 || math.Abs(b.ShortEnd.X-30) > 1e-9 {
		t.Fatalf("clamped short axis = %+v", b)
	}
	if _, ok := NewBidimensional(Point2D{X: 1, Y: 1}, Point2D{X: 1, Y: 1}, Point2D{X: 3, Y: 3}, sp); ok {
		t.Fatal("zero-length long axis accepted")
	}
	if _, ok := NewBidimensional(Point2D{X: 0, Y: 0}, Point2D{X: 4, Y: 0}, Point2D{X: 2, Y: 0}, sp); ok {
		t.Fatal("short point on the long axis accepted")
	}
}

func TestBidimensionalPerpendicularInMillimetresNotPixels(t *testing.T) {
	// With rows 3 mm apart and columns 1 mm, a pixel-perpendicular short axis
	// to a diagonal long axis is not perpendicular on the patient.
	sp := MeasureSpacing{X: 1, Y: 3}
	b, ok := NewBidimensional(Point2D{}, Point2D{X: 10, Y: 10}, Point2D{X: 2, Y: 8}, sp)
	if !ok {
		t.Fatal("NewBidimensional failed")
	}
	assertBidimensionalConstraint(t, b, sp, "anisotropic placement")
	pixelDot := (b.ShortEnd.X-b.ShortStart.X)*10 + (b.ShortEnd.Y-b.ShortStart.Y)*10
	if math.Abs(pixelDot) < 1e-6 {
		t.Fatal("short axis is perpendicular in pixels, not in mm")
	}
}

// TestBidimensionalStaysPerpendicularUnderDrag drives random handle drags,
// slides and anisotropic spacings: after every accepted edit the axes remain
// perpendicular in mm and cross within both segments.
func TestBidimensionalStaysPerpendicularUnderDrag(t *testing.T) {
	rng := rand.New(rand.NewSource(1074))
	for trial := 0; trial < 50; trial++ {
		sp := MeasureSpacing{X: 0.3 + rng.Float64()*2, Y: 0.3 + rng.Float64()*2}
		b, ok := NewBidimensional(
			Point2D{X: rng.Float64() * 100, Y: rng.Float64() * 100},
			Point2D{X: rng.Float64() * 100, Y: rng.Float64() * 100},
			Point2D{X: rng.Float64() * 100, Y: rng.Float64() * 100}, sp)
		if !ok {
			continue
		}
		assertBidimensionalConstraint(t, b, sp, "placement")
		for step := 0; step < 40; step++ {
			target := Point2D{X: rng.Float64()*140 - 20, Y: rng.Float64()*140 - 20}
			var next Bidimensional
			var ok bool
			if choice := rng.Intn(5); choice < 4 {
				next, ok = MoveBidimensionalHandle(b, choice, target, sp)
			} else {
				next, ok = SlideBidimensionalShortAxis(b, target, sp)
			}
			if !ok {
				continue
			}
			b = next
			assertBidimensionalConstraint(t, b, sp, "drag")
		}
	}
}

func TestMoveBidimensionalHandleRules(t *testing.T) {
	sp := MeasureSpacing{X: 1, Y: 1}
	b, _ := NewBidimensional(Point2D{X: 0, Y: 0}, Point2D{X: 10, Y: 0}, Point2D{X: 4, Y: 3}, sp)

	// Turning the long axis carries the short axis: same lengths each side,
	// same relative position.
	turned, ok := MoveBidimensionalHandle(b, BidimensionalLongEnd, Point2D{X: 0, Y: 20}, sp)
	if !ok || turned.LongEnd != (Point2D{X: 0, Y: 20}) {
		t.Fatalf("turned = %+v, %v", turned, ok)
	}
	if foot, _ := turned.Intersection(sp); math.Abs(foot.X) > 1e-9 || math.Abs(foot.Y-8) > 1e-9 {
		t.Fatalf("foot after turn = %v, want (0, 8)", foot)
	}
	if _, short, _ := BidimensionalAxesMM(turned, sp); math.Abs(short-6) > 1e-9 {
		t.Fatalf("short axis after turn = %v, want 6", short)
	}

	// A short endpoint slides along the short axis only.
	moved, ok := MoveBidimensionalHandle(b, BidimensionalShortStart, Point2D{X: 7, Y: 5}, sp)
	if !ok || math.Abs(moved.ShortStart.X-4) > 1e-9 || math.Abs(moved.ShortStart.Y-5) > 1e-9 || moved.ShortEnd != b.ShortEnd {
		t.Fatalf("moved short start = %+v", moved)
	}
	// It cannot cross to the other side: it stops on the long axis.
	crossed, ok := MoveBidimensionalHandle(b, BidimensionalShortStart, Point2D{X: 4, Y: -2}, sp)
	if !ok || math.Abs(crossed.ShortStart.Y) > 1e-9 {
		t.Fatalf("crossed short start = %+v", crossed)
	}

	// Sliding moves the crossing along the long axis, within it.
	slid, ok := SlideBidimensionalShortAxis(b, Point2D{X: 50, Y: 9}, sp)
	if foot, _ := slid.Intersection(sp); !ok || math.Abs(foot.X-10) > 1e-9 {
		t.Fatalf("slid foot = %v", foot)
	}

	if _, ok := MoveBidimensionalHandle(b, BidimensionalLongEnd, b.LongStart, sp); ok {
		t.Fatal("collapsing the long axis was accepted")
	}
	if _, ok := MoveBidimensionalHandle(b, 7, Point2D{}, sp); ok {
		t.Fatal("unknown handle accepted")
	}
	if _, ok := MoveBidimensionalHandle(b, 0, Point2D{X: math.NaN()}, sp); ok {
		t.Fatal("NaN target accepted")
	}
}

func TestBidimensionalConstrainedRepairsPersistedPoints(t *testing.T) {
	sp := MeasureSpacing{X: 1, Y: 1}
	skewed := Bidimensional{LongStart: Point2D{}, LongEnd: Point2D{X: 10}, ShortStart: Point2D{X: 4.2, Y: 3}, ShortEnd: Point2D{X: 3.8, Y: -3}}
	fixed, ok := skewed.Constrained(sp)
	if !ok {
		t.Fatal("Constrained failed")
	}
	assertBidimensionalConstraint(t, fixed, sp, "constrained")
	if math.Abs(fixed.ShortStart.X-4) > 1e-9 || math.Abs(fixed.ShortEnd.X-4) > 1e-9 {
		t.Fatalf("constrained short axis = %v → %v, want x = 4", fixed.ShortStart, fixed.ShortEnd)
	}
	oneSided := Bidimensional{LongStart: Point2D{}, LongEnd: Point2D{X: 10}, ShortStart: Point2D{X: 4, Y: 1}, ShortEnd: Point2D{X: 4, Y: 5}}
	if fixed, ok := oneSided.Constrained(sp); !ok || math.Abs(fixed.ShortStart.Y) > 1e-9 {
		t.Fatalf("one-sided short axis = %+v, %v; want the nearer endpoint moved onto the long axis", fixed, ok)
	}
	if got, ok := BidimensionalFromPoints(fixed.Points()); !ok || got != fixed {
		t.Fatalf("points round trip = %+v", got)
	}
	if _, ok := BidimensionalFromPoints(fixed.Points()[:3]); ok {
		t.Fatal("three points accepted")
	}
}
