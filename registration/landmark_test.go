package registration

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/spatialreg"
)

func landmarkCloud(rng *rand.Rand, n int) []render.Vec3 {
	points := make([]render.Vec3, n)
	for i := range points {
		points[i] = render.Vec3{X: rng.Float64()*200 - 100, Y: rng.Float64()*200 - 100, Z: rng.Float64()*150 - 75}
	}
	return points
}

func applyLinear(m Matrix3, offset render.Vec3, points []render.Vec3) []render.Vec3 {
	out := make([]render.Vec3, len(points))
	for i, p := range points {
		out[i] = m.MulVec(p).Add(offset)
	}
	return out
}

func requireMatrixNear(t *testing.T, got, want Matrix3, tolerance float64) {
	t.Helper()
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if math.Abs(got[r][c]-want[r][c]) > tolerance {
				t.Fatalf("matrix %v, want %v", got, want)
			}
		}
	}
}

func TestRegisterLandmarksRecoversSyntheticTransforms(t *testing.T) {
	rng := rand.New(rand.NewSource(1077))
	fixed := landmarkCloud(rng, 6)
	rotation, err := VersorFromAxisAngle(render.Vec3{X: 1, Y: 2, Z: 3}, 0.6)
	if err != nil {
		t.Fatal(err)
	}
	r := rotation.Matrix()
	offset := render.Vec3{X: 12.5, Y: -7, Z: 30}
	affine := Matrix3{{1.1, 0.05, -0.02}, {0.03, 0.95, 0.1}, {-0.04, 0.02, 1.2}}
	for _, c := range []struct {
		mode LandmarkMode
		m    Matrix3
		kind string
	}{
		{LandmarkRigid, r, string(spatialreg.TransformRigid)},
		{LandmarkSimilarity, r.Scale(1.25), string(spatialreg.TransformRigidScale)},
		{LandmarkAffine, affine, string(spatialreg.TransformAffine)},
	} {
		t.Run(c.mode.String(), func(t *testing.T) {
			moving := applyLinear(c.m, offset, fixed)
			result, err := RegisterLandmarks(fixed, moving, c.mode)
			if err != nil {
				t.Fatal(err)
			}
			requireMatrixNear(t, result.Transform.Matrix(), c.m, 1e-9)
			if d := result.Transform.Offset().Sub(offset).Length(); d > 1e-7 {
				t.Fatalf("offset off by %g mm", d)
			}
			if result.FRE > 1e-7 || len(result.Residuals) != len(fixed) {
				t.Fatalf("FRE %g with %d residuals", result.FRE, len(result.Residuals))
			}
			matrix, err := DICOMMatrix(result.Transform)
			if err != nil || matrix.Type != c.kind {
				t.Fatalf("DICOM matrix %q: %v", matrix.Type, err)
			}
		})
	}
}

func TestRegisterLandmarksReportsFREWithNoise(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	fixed := landmarkCloud(rng, 12)
	rotation, _ := VersorFromAxisAngle(render.Vec3{Z: 1}, 0.2)
	moving := applyLinear(rotation.Matrix(), render.Vec3{X: 5}, fixed)
	for i := range moving {
		moving[i] = moving[i].Add(render.Vec3{X: rng.NormFloat64(), Y: rng.NormFloat64(), Z: rng.NormFloat64()})
	}
	result, err := RegisterLandmarks(fixed, moving, LandmarkRigid)
	if err != nil {
		t.Fatal(err)
	}
	// Noise of 1 mm per axis: the FRE is about sqrt(3·(n−6)/n) mm.
	if result.FRE < 0.8 || result.FRE > 2.2 {
		t.Fatalf("FRE %.2f mm", result.FRE)
	}
	angle := math.Acos(math.Min(1, (result.Transform.Matrix()[0][0]+result.Transform.Matrix()[1][1]+result.Transform.Matrix()[2][2]-1)/2))
	if math.Abs(angle-0.2) > 0.02 {
		t.Fatalf("rotation %.3f rad, want about 0.2", angle)
	}
	rms := 0.0
	for _, r := range result.Residuals {
		rms += r * r
	}
	if math.Abs(math.Sqrt(rms/12)-result.FRE) > 1e-12 {
		t.Fatal("FRE is not the RMS of the residuals")
	}
}

func TestRegisterLandmarksRejectsDegenerateSets(t *testing.T) {
	line := []render.Vec3{{}, {X: 1}, {X: 2}, {X: 3}}
	plane := []render.Vec3{{}, {X: 1}, {Y: 1}, {X: 1, Y: 1}}
	for name, c := range map[string]struct {
		fixed, moving []render.Vec3
		mode          LandmarkMode
	}{
		"sizes":        {line, line[:3], LandmarkRigid},
		"too few":      {line[:2], line[:2], LandmarkRigid},
		"collinear":    {line, line, LandmarkRigid},
		"coplanar":     {plane, plane, LandmarkAffine},
		"non-finite":   {[]render.Vec3{{X: math.NaN()}, {X: 1}, {Y: 1}}, plane[:3], LandmarkRigid},
		"unknown mode": {plane, plane, LandmarkMode(9)},
	} {
		if _, err := RegisterLandmarks(c.fixed, c.moving, c.mode); !errors.Is(err, ErrLandmarks) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := RegisterLandmarks(plane, plane, LandmarkRigid); err != nil {
		t.Fatalf("coplanar rigid fit: %v", err)
	}
}

func TestIterativeClosestPointAlignsSurfaces(t *testing.T) {
	// A synthetic ellipsoidal surface with a bump, so the rotation is
	// determined, sampled twice with different point sets.
	surface := func(rng *rand.Rand, n int) []render.Vec3 {
		points := make([]render.Vec3, n)
		for i := range points {
			z := rng.Float64()*2 - 1
			phi := rng.Float64() * 2 * math.Pi
			r := math.Sqrt(1 - z*z)
			p := render.Vec3{X: 60 * r * math.Cos(phi), Y: 40 * r * math.Sin(phi), Z: 30 * z}
			if p.X > 30 && p.Z > 0 {
				p = p.Scale(1.15)
			}
			points[i] = p
		}
		return points
	}
	rng := rand.New(rand.NewSource(12))
	fixed := surface(rng, 4000)
	rotation, _ := VersorFromAxisAngle(render.Vec3{X: 0.3, Y: 1, Z: 0.2}, 0.12)
	offset := render.Vec3{X: 4, Y: -3, Z: 2}
	moving := applyLinear(rotation.Matrix(), offset, surface(rng, 5000))
	result, err := IterativeClosestPoint(context.Background(), fixed, moving, ICPOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Two independent samplings of the surface are about 1 mm apart even
	// when aligned exactly (the point spacing is about 2 mm).
	if !result.Converged || result.MeanDistance > 1.5 {
		t.Fatalf("converged %v after %d iterations, mean distance %.3f mm", result.Converged, result.Iterations, result.MeanDistance)
	}
	// The recovered transform maps the fixed surface points close to their
	// true images.
	worst := 0.0
	for _, p := range fixed[:200] {
		want := rotation.Matrix().MulVec(p).Add(offset)
		worst = math.Max(worst, result.Transform.TransformPoint(p).Sub(want).Length())
	}
	if worst > 2 {
		t.Fatalf("ICP transform is off by %.2f mm", worst)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := IterativeClosestPoint(ctx, fixed, moving, ICPOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
}

func TestKDTreeNearestMatchesBruteForce(t *testing.T) {
	rng := rand.New(rand.NewSource(8))
	points := landmarkCloud(rng, 500)
	tree := newKDTree(points)
	for q := 0; q < 200; q++ {
		query := landmarkCloud(rng, 1)[0]
		_, got := tree.nearest(query)
		want := math.Inf(1)
		for _, p := range points {
			want = math.Min(want, p.Sub(query).Dot(p.Sub(query)))
		}
		if math.Abs(got-want) > 1e-9 {
			t.Fatalf("query %d: %g, want %g", q, got, want)
		}
	}
}
