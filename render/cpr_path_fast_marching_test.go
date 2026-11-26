package render

import (
	"context"
	"errors"
	"math"
	"testing"
)

// arcTubeStack is a quarter-torus tube: centreline radius radius around
// (cx, cy) in the slice z = cz, tube radius tube, spacing 1 mm.
func arcTubeStack(cols, rows, depth int, cx, cy, cz, radius, tube float64) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < depth; z++ {
		data := make([]byte, rows*cols)
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				if arcTubeDistance(Vec3{X: float64(x), Y: float64(y), Z: float64(z)}, cx, cy, cz, radius) <= tube {
					data[y*cols+x] = 200
				}
			}
		}
		stack.Frames = append(stack.Frames, volumeTestFrame(rows, cols, data, z))
	}
	return stack
}

// arcTubeDistance is the distance from p to the quarter-circle centreline
// (angles 0..90° measured from +x towards +y).
func arcTubeDistance(p Vec3, cx, cy, cz, radius float64) float64 {
	angle := math.Atan2(p.Y-cy, p.X-cx)
	angle = math.Max(0, math.Min(math.Pi/2, angle))
	c := Vec3{X: cx + radius*math.Cos(angle), Y: cy + radius*math.Sin(angle), Z: cz}
	return p.Sub(c).Length()
}

func arcTubeRouteError(t *testing.T, vol *Volume, method CPRPathMethod, options CPRPathAssistantOptions) float64 {
	t.Helper()
	const cx, cy, cz, radius = 4, 4, 6, 24
	start := vol.VoxelToPatient(Vec3{X: cx + radius, Y: cy, Z: cz})
	end := vol.VoxelToPatient(Vec3{X: cx, Y: cy + radius, Z: cz})
	options.Method = method
	options.MarginMM = 4
	options.OutputSpacingMM = 1
	got, err := FindCPRAssistedPath(context.Background(), vol, []Vec3{start, end}, options)
	if err != nil {
		t.Fatalf("method %d: %v", method, err)
	}
	if got[0] != start || got[len(got)-1] != end {
		t.Fatalf("method %d moved a waypoint", method)
	}
	worst := 0.0
	for _, p := range got {
		worst = math.Max(worst, arcTubeDistance(vol.PatientToVoxel(p), cx, cy, cz, radius))
	}
	return worst
}

func TestFindCPRAssistedPathFastMarchingCentresCurvedTube(t *testing.T) {
	vol, err := BuildVolume(arcTubeStack(34, 34, 13, 4, 4, 6, 24, 4))
	if err != nil {
		t.Fatal(err)
	}
	fastMarching := arcTubeRouteError(t, vol, CPRPathFastMarching, CPRPathAssistantOptions{})
	dijkstra := arcTubeRouteError(t, vol, CPRPathDijkstra, CPRPathAssistantOptions{GridSpacingMM: 1})
	uncentred := arcTubeRouteError(t, vol, CPRPathFastMarching, CPRPathAssistantOptions{NoCentring: true})
	t.Logf("worst distance to the centreline: fast marching %.2f mm, without centring %.2f mm, Dijkstra %.2f mm", fastMarching, uncentred, dijkstra)
	// The 8 mm lumen bends on a 24 mm radius: an uncentred shortest route
	// cuts the inner side of the bend.
	if fastMarching > 0.5 {
		t.Fatalf("fast marching route strays %.2f mm from the centreline", fastMarching)
	}
	if fastMarching >= dijkstra || fastMarching >= uncentred {
		t.Fatalf("centred fast marching (%.2f mm) is not better than Dijkstra (%.2f mm) and the uncentred route (%.2f mm)", fastMarching, dijkstra, uncentred)
	}
}

func TestFindCPRAssistedPathFollowsSpeedVolume(t *testing.T) {
	// The intensity volume is uniform; only the speed volume (a vesselness
	// stand-in peaking on the arc centreline) carries the route.
	const cx, cy, cz, radius = 4, 4, 6, 24
	flat := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	response := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < 13; z++ {
		uniform := make([]byte, 34*34)
		ridge := make([]byte, 34*34)
		for y := 0; y < 34; y++ {
			for x := 0; x < 34; x++ {
				uniform[y*34+x] = 100
				d := arcTubeDistance(Vec3{X: float64(x), Y: float64(y), Z: float64(z)}, cx, cy, cz, radius)
				ridge[y*34+x] = byte(math.Round(250 * math.Exp(-d*d/(2*2*2))))
			}
		}
		flat.Frames = append(flat.Frames, volumeTestFrame(34, 34, uniform, z))
		response.Frames = append(response.Frames, volumeTestFrame(34, 34, ridge, z))
	}
	vol, err := BuildVolume(flat)
	if err != nil {
		t.Fatal(err)
	}
	speed, err := BuildVolume(response)
	if err != nil {
		t.Fatal(err)
	}
	worst := arcTubeRouteError(t, vol, CPRPathFastMarching, CPRPathAssistantOptions{SpeedVolume: speed})
	if worst > 0.75 {
		t.Fatalf("speed-volume route strays %.2f mm from the ridge", worst)
	}
	if _, err := FindCPRAssistedPath(context.Background(), vol, []Vec3{vol.VoxelToPatient(Vec3{X: 28, Y: 4, Z: 6}), vol.VoxelToPatient(Vec3{X: 4, Y: 28, Z: 6})},
		CPRPathAssistantOptions{Method: CPRPathDijkstra, SpeedVolume: speed}); !errors.Is(err, ErrCPRPathAssistantInput) {
		t.Fatalf("speed volume with Dijkstra: %v", err)
	}
	if _, err := FindCPRAssistedPath(context.Background(), vol, []Vec3{vol.VoxelToPatient(Vec3{X: 28, Y: 4, Z: 6}), vol.VoxelToPatient(Vec3{X: 4, Y: 28, Z: 6})},
		CPRPathAssistantOptions{Method: CPRPathMethod(7)}); !errors.Is(err, ErrCPRPathAssistantInput) {
		t.Fatalf("unknown method: %v", err)
	}
}

func TestSquaredDistanceToFeaturesMatchesBruteForce(t *testing.T) {
	nx, ny, nz := 7, 5, 4
	spacing := Vec3{X: 0.5, Y: 1.5, Z: 2}
	features := make([]bool, nx*ny*nz)
	for _, i := range []int{3, 40, 77, 139} {
		features[i] = true
	}
	got, err := squaredDistanceToFeatures(context.Background(), features, nx, ny, nz, spacing)
	if err != nil {
		t.Fatal(err)
	}
	for i := range features {
		x, y, z := i%nx, i/nx%ny, i/(nx*ny)
		want := math.Inf(1)
		for j, feature := range features {
			if !feature {
				continue
			}
			fx, fy, fz := j%nx, j/nx%ny, j/(nx*ny)
			d := math.Pow(float64(x-fx)*spacing.X, 2) + math.Pow(float64(y-fy)*spacing.Y, 2) + math.Pow(float64(z-fz)*spacing.Z, 2)
			want = math.Min(want, d)
		}
		if math.Abs(got[i]-want) > 1e-9 {
			t.Fatalf("node %d: %g, want %g", i, got[i], want)
		}
	}
}

func BenchmarkFindCPRAssistedPath(b *testing.B) {
	stack := &Stack{PixelSpacing: []float64{0.8, 0.8}, SliceThickness: 1}
	const n = 128
	for z := 0; z < n; z++ {
		data := make([]byte, n*n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				p := Vec3{X: float64(x) * 0.8, Y: float64(y) * 0.8, Z: float64(z)}
				if distanceToSegment(p, Vec3{X: 10, Y: 12, Z: 8}, Vec3{X: 90, Y: 88, Z: 110}) <= 3 {
					data[y*n+x] = 220
				}
			}
		}
		stack.Frames = append(stack.Frames, volumeTestFrame(n, n, data, z))
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		b.Fatal(err)
	}
	waypoints := []Vec3{
		vol.VoxelToPatient(Vec3{X: 10 / 0.8, Y: 12 / 0.8, Z: 8}),
		vol.VoxelToPatient(Vec3{X: 90 / 0.8, Y: 88 / 0.8, Z: 110}),
	}
	for _, method := range []struct {
		name   string
		method CPRPathMethod
	}{{"FastMarching", CPRPathFastMarching}, {"Dijkstra", CPRPathDijkstra}} {
		b.Run(method.name, func(b *testing.B) {
			b.ReportAllocs()
			for i := 0; i < b.N; i++ {
				if _, err := FindCPRAssistedPath(context.Background(), vol, waypoints, CPRPathAssistantOptions{Method: method.method}); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
