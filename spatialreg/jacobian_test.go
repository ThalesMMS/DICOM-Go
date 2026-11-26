package spatialreg

import (
	"context"
	"errors"
	"math"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// anisotropicGrid samples field at the nodes of an axis-aligned grid with
// per-axis spacing.
func anisotropicGrid(t *testing.T, origin, spacing render.Vec3, dimensions [3]uint32, field func(p render.Vec3) render.Vec3) *Grid {
	t.Helper()
	var vectors []render.Vec3
	for z := 0; z < int(dimensions[2]); z++ {
		for y := 0; y < int(dimensions[1]); y++ {
			for x := 0; x < int(dimensions[0]); x++ {
				vectors = append(vectors, field(origin.Add(render.Vec3{X: float64(x) * spacing.X, Y: float64(y) * spacing.Y, Z: float64(z) * spacing.Z})))
			}
		}
	}
	g, err := NewGrid(origin, render.Vec3{X: 1}, render.Vec3{Y: 1}, spacing, dimensions, vectors)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// TestJacobianMatchesAnAnalyticQuadraticField (#1257): for the quadratic
// displacement D(p) = (αx², βxy, γz²) after the pre-matrix A = diag(1.1,
// 0.9, 1), det(A + ∇D)/det(A) = (1.1 + 2αx)(0.9 + βx)(1 + 2γz)/0.99, which
// central differences reproduce (up to the float32 vectors) at the interior
// nodes of an anisotropic grid.
func TestJacobianMatchesAnAnalyticQuadraticField(t *testing.T) {
	const alpha, beta, gamma = 0.004, -0.006, 0.003
	origin, spacing := render.Vec3{X: -10, Y: -8, Z: -6}, render.Vec3{X: 2, Y: 1.5, Z: 2.5}
	dimensions := [3]uint32{12, 10, 8}
	grid := anisotropicGrid(t, origin, spacing, dimensions, func(p render.Vec3) render.Vec3 {
		return render.Vec3{X: alpha * p.X * p.X, Y: beta * p.X * p.Y, Z: gamma * p.Z * p.Z}
	})
	pre := identityMatrix()
	pre.Values[0], pre.Values[5], pre.Values[3] = 1.1, 0.9, 4
	r := &Registration{Pre: pre, Post: identityMatrix(), Grid: grid}
	change, err := r.Jacobian(context.Background(), JacobianOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if change.Dimensions != dimensions || change.Region != grid.WholeRegion() || len(change.Values) != 12*10*8 || change.Undefined != 0 {
		t.Fatalf("change %+v", change)
	}
	for z := 1; z < 7; z++ {
		for y := 1; y < 9; y++ {
			for x := 1; x < 11; x++ {
				p := grid.NodePoint(x, y, z)
				want := (1.1 + 2*alpha*p.X) * (0.9 + beta*p.X) * (1 + 2*gamma*p.Z) / 0.99
				if got := change.Values[(z*10+y)*12+x]; math.Abs(got-want) > 1e-6 {
					t.Fatalf("node %d,%d,%d: %.12f, want %.12f", x, y, z, got, want)
				}
			}
		}
	}
	// The pointwise volume change agrees with the analytic determinant up
	// to the trilinear interpolant's cell-wise gradient.
	p := grid.NodePoint(5, 4, 3).Add(render.Vec3{X: 0.5, Y: 0.4, Z: 0.3})
	at, err := r.VolumeChangeAt(p)
	want := (1.1 + 2*alpha*p.X) * (0.9 + beta*p.X) * (1 + 2*gamma*p.Z) / 0.99
	if err != nil || math.Abs(at-want) > 0.01 {
		t.Fatalf("VolumeChangeAt %v (%v), want %v", at, err, want)
	}
	// VolumeChange is the whole-grid Jacobian.
	whole, err := r.VolumeChange()
	if err != nil || whole.Min != change.Min || whole.Max != change.Max || len(whole.Values) != len(change.Values) {
		t.Fatalf("VolumeChange %+v, %v", whole, err)
	}
}

// foldField is a Gaussian bump that pulls x towards x0: D = (−s·(x−x0)·e, 0,
// 0) with e = exp(−|p−c|²/(2σ²)), whose volume change 1 − s·e·(1 −
// (x−x0)²/σ²) is negative (space folds) in a ball around c.
func foldField(c render.Vec3, s, sigma float64) func(render.Vec3) render.Vec3 {
	return func(p render.Vec3) render.Vec3 {
		d := p.Sub(c)
		return render.Vec3{X: -s * d.X * math.Exp(-d.Dot(d)/(2*sigma*sigma))}
	}
}

func foldDeterminant(c render.Vec3, s, sigma float64, p render.Vec3) float64 {
	d := p.Sub(c)
	return 1 - s*math.Exp(-d.Dot(d)/(2*sigma*sigma))*(1-d.X*d.X/(sigma*sigma))
}

// TestJacobianLocatesAKnownFold (#1257): the fold of a synthetic field is
// found where the analytic determinant is negative, its bounds enclose the
// fold and nothing else, a region around it gives the same values as the
// whole grid, and the summary-only mode agrees without allocating the grid.
func TestJacobianLocatesAKnownFold(t *testing.T) {
	centre := render.Vec3{X: 20, Y: 22, Z: 18}
	const s, sigma = 2.0, 6.0
	grid := anisotropicGrid(t, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{48, 44, 40}, foldField(centre, s, sigma))
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: grid}
	whole, err := r.Jacobian(context.Background(), JacobianOptions{Workers: 3})
	if err != nil {
		t.Fatal(err)
	}
	if whole.Folded == 0 || whole.Min >= 0 {
		t.Fatalf("no fold found: %+v", whole.FoldedBounds)
	}
	folded := 0
	for z := 1; z < 39; z++ {
		for y := 1; y < 43; y++ {
			for x := 1; x < 47; x++ {
				want := foldDeterminant(centre, s, sigma, grid.NodePoint(x, y, z))
				got := whole.Values[(z*44+y)*48+x]
				if math.Abs(got-want) > 0.04 {
					t.Fatalf("node %d,%d,%d: %.4f, analytic %.4f", x, y, z, got, want)
				}
				if got <= 0 {
					folded++
					if want > 0.04 {
						t.Fatalf("node %d,%d,%d folded, analytic %.4f", x, y, z, want)
					}
				} else if want < -0.04 {
					t.Fatalf("node %d,%d,%d not folded, analytic %.4f", x, y, z, want)
				}
			}
		}
	}
	if folded != whole.Folded {
		t.Fatalf("%d folded interior nodes, %d reported", folded, whole.Folded)
	}
	// The fold lies where e > 1/2: within 1.2σ of the centre.
	for axis, c := range [3]float64{centre.X, centre.Y, centre.Z} {
		if float64(whole.FoldedBounds.Min[axis]) < c-1.2*sigma || float64(whole.FoldedBounds.Max[axis]-1) > c+1.2*sigma {
			t.Fatalf("folded bounds %+v around %v", whole.FoldedBounds, centre)
		}
	}
	if whole.MinNode != [3]int{20, 22, 18} {
		t.Fatalf("most negative node %v, want the centre", whole.MinNode)
	}

	// A region: the same values as the whole grid, faces included.
	region := GridRegion{Min: [3]int{15, 0, 16}, Max: [3]int{26, 44, 19}}
	part, err := r.Jacobian(context.Background(), JacobianOptions{Region: &region})
	if err != nil {
		t.Fatal(err)
	}
	if part.Region != region || part.Dimensions != [3]uint32{11, 44, 3} || len(part.Values) != 11*44*3 {
		t.Fatalf("region %+v", part)
	}
	for z := 16; z < 19; z++ {
		for y := 0; y < 44; y++ {
			for x := 15; x < 26; x++ {
				if got, want := part.Values[((z-16)*44+y)*11+x-15], whole.Values[(z*44+y)*48+x]; got != want {
					t.Fatalf("region node %d,%d,%d: %v, whole grid %v", x, y, z, got, want)
				}
			}
		}
	}
	// A region beyond the grid is clipped; one outside it is empty.
	clipped := GridRegion{Min: [3]int{40, 40, 38}, Max: [3]int{60, 60, 60}}
	if part, err := r.Jacobian(context.Background(), JacobianOptions{Region: &clipped}); err != nil || part.Dimensions != [3]uint32{8, 4, 2} {
		t.Fatalf("clipped region %+v, %v", part.Dimensions, err)
	}
	outside := GridRegion{Min: [3]int{60, 0, 0}, Max: [3]int{70, 1, 1}}
	if part, err := r.Jacobian(context.Background(), JacobianOptions{Region: &outside}); err != nil || part.Values != nil || part.Folded != 0 {
		t.Fatalf("outside region %+v, %v", part, err)
	}

	// Summary only: the same summary, without the grid.
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	summary, err := r.Jacobian(context.Background(), JacobianOptions{SummaryOnly: true})
	runtime.ReadMemStats(&after)
	if err != nil {
		t.Fatal(err)
	}
	if summary.Values != nil || summary.Min != whole.Min || summary.Max != whole.Max || summary.Folded != whole.Folded ||
		summary.FoldedBounds != whole.FoldedBounds || summary.MinNode != whole.MinNode {
		t.Fatalf("summary %+v, whole %+v", summary, whole)
	}
	gridBytes := uint64(48 * 44 * 40 * 8)
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > gridBytes/8 {
		t.Fatalf("summary allocated %d bytes, the grid is %d", allocated, gridBytes)
	}
}

// TestJacobianCancelsAndRefuses: a cancelled context ends the computation
// and a registration without a grid or with a singular pre-matrix is
// refused.
func TestJacobianCancelsAndRefuses(t *testing.T) {
	grid := anisotropicGrid(t, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{8, 8, 8}, func(render.Vec3) render.Vec3 { return render.Vec3{} })
	r := &Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: grid}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Jacobian(ctx, JacobianOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
	if _, err := (&Registration{Pre: identityMatrix()}).Jacobian(context.Background(), JacobianOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("no grid: %v", err)
	}
	singular := identityMatrix()
	singular.Values[0] = 0
	if _, err := (&Registration{Pre: singular, Post: identityMatrix(), Grid: grid}).Jacobian(context.Background(), JacobianOptions{}); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("singular: %v", err)
	}
	// Undefined vectors are counted and give NaN.
	vectors := make([]render.Vec3, 27)
	vectors[13] = render.Vec3{X: math.NaN(), Y: math.NaN(), Z: math.NaN()}
	holed, err := NewGrid(render.Vec3{}, render.Vec3{X: 1}, render.Vec3{Y: 1}, render.Vec3{X: 1, Y: 1, Z: 1}, [3]uint32{3, 3, 3}, vectors)
	if err != nil {
		t.Fatal(err)
	}
	change, err := (&Registration{Pre: identityMatrix(), Post: identityMatrix(), Grid: holed}).Jacobian(context.Background(), JacobianOptions{})
	if err != nil || change.Undefined == 0 || !math.IsNaN(change.Values[4]) || change.Min != 1 {
		t.Fatalf("undefined vectors: %+v, %v", change, err)
	}
}
