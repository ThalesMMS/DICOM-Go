package render

import (
	"context"
	"errors"
	"math"
	"sync/atomic"
	"testing"
	"time"
)

func cprBifurcationCalciumPhantom(t *testing.T) (*Volume, [][2]Vec3) {
	t.Helper()
	const cols, rows, depth = 60, 52, 32
	segments := [][2]Vec3{
		{{X: 6, Y: 10, Z: 16}, {X: 27, Y: 26, Z: 16}},
		{{X: 27, Y: 26, Z: 16}, {X: 53, Y: 13, Z: 16}},
		{{X: 27, Y: 26, Z: 16}, {X: 53, Y: 40, Z: 16}},
	}
	stack := &Stack{}
	values := make([]float32, cols*rows*depth)
	for z := 0; z < depth; z++ {
		stack.Frames = append(stack.Frames, volumeTestFrame(rows, cols, make([]byte, rows*cols), z))
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				p := Vec3{X: float64(x), Y: float64(y), Z: float64(z)}
				d := math.Inf(1)
				for _, segment := range segments {
					d = math.Min(d, distanceToSegment(p, segment[0], segment[1]))
				}
				value := float32(-600)
				if d <= 3 {
					value = 350 // contrast-filled lumen
				}
				if x >= 17 && x <= 23 && d >= 2.4 && d <= 3.8 && z >= 16 {
					value = 1800 // asymmetric calcified wall along the trunk
				}
				values[(z*rows+y)*cols+x] = value
			}
		}
	}
	base, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer base.Close()
	volume, err := newDerivedFloat32Volume(base, cols, rows, depth, base.Origin, 1, 1, 1, values)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	return volume, segments
}

func TestCPRVesselCropBoundsMemoryAndPreservesFineObliqueGeometry(t *testing.T) {
	source := &Volume{Cols: 101, Rows: 81, Depth: 61, Origin: Vec3{X: 100, Y: -20, Z: 7}, AxisX: Vec3{Y: 1}, AxisY: Vec3{X: -1}, Normal: Vec3{Z: 1}, ColSpacing: .4, RowSpacing: .5, SliceSpacing: .6, regular: true}
	last := Vec3{X: 100, Y: 80, Z: 60}
	points := []Vec3{source.Origin, source.VoxelToPatient(last)}
	crop, err := planCPRVesselCrop(source, points, 30)
	if err != nil {
		t.Fatal(err)
	}
	if crop.dims != [3]int{55, 55, 49} {
		t.Fatal("fine acquisition was not bounded at analysis resolution", crop.dims)
	}
	endVoxel := Vec3{
		X: float64(crop.low[0]) + float64(crop.dims[0]-1)*crop.spacing[0]/crop.nativeSpacing[0],
		Y: float64(crop.low[1]) + float64(crop.dims[1]-1)*crop.spacing[1]/crop.nativeSpacing[1],
		Z: float64(crop.low[2]) + float64(crop.dims[2]-1)*crop.spacing[2]/crop.nativeSpacing[2],
	}
	if source.VoxelToPatient(endVoxel).Sub(points[1]).Length() > 1e-10 {
		t.Fatal("resampled ROI lost its last patient-space endpoint")
	}
	large := &Volume{Cols: 1024, Rows: 1024, Depth: 1024, AxisX: Vec3{X: 1}, AxisY: Vec3{Y: 1}, Normal: Vec3{Z: 1}, ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1, regular: true}
	_, err = planCPRVesselCrop(large, []Vec3{{}, {X: 1023, Y: 1023, Z: 1023}}, 30)
	var limit *CPRPathAssistantLimitError
	if !errors.Is(err, ErrCPRPathAssistantMemory) || !errors.As(err, &limit) || limit.Resource != "vessel_voxels" || limit.Limit != MaxCPRVesselVoxels {
		t.Fatal("oversized analysis did not fail before allocation", err)
	}
}

type pausedCPRContext struct {
	context.Context
	checks  atomic.Int32
	entered chan struct{}
	release chan struct{}
}

func (c *pausedCPRContext) Err() error {
	if c.checks.Add(1) == 2 {
		close(c.entered)
		<-c.release
	}
	return c.Context.Err()
}

func TestVesselGuidedCPRCancellationDuringActivePreparation(t *testing.T) {
	volume, segments := cprBifurcationCalciumPhantom(t)
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &pausedCPRContext{Context: base, entered: make(chan struct{}), release: make(chan struct{})}
	done := make(chan error, 1)
	go func() {
		_, err := FindCPRAssistedPath(ctx, volume, []Vec3{volume.VoxelToPatient(segments[0][0]), volume.VoxelToPatient(segments[1][1])}, CPRPathAssistantOptions{VesselGuided: true})
		done <- err
	}()
	select {
	case <-ctx.entered:
	case <-time.After(5 * time.Second):
		close(ctx.release)
		t.Fatal("preparation did not start")
	}
	cancel()
	close(ctx.release)
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal("active vessel preparation ignored cancellation", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled preparation did not exit")
	}
}

func TestVesselGuidedCPRStaysInBifurcatingCalcifiedLumen(t *testing.T) {
	volume, segments := cprBifurcationCalciumPhantom(t)
	for branch := 1; branch <= 2; branch++ {
		waypoints := []Vec3{volume.VoxelToPatient(segments[0][0]), volume.VoxelToPatient(segments[branch][1])}
		path, err := FindCPRAssistedPath(context.Background(), volume, waypoints, CPRPathAssistantOptions{
			VesselGuided: true, GridSpacingMM: .75, OutputSpacingMM: .5, MarginMM: 20,
		})
		if err != nil {
			t.Fatalf("branch %d: %v", branch, err)
		}
		if path[0] != waypoints[0] || path[len(path)-1] != waypoints[1] {
			t.Fatal("vessel guidance moved a waypoint")
		}
		for i, patient := range path {
			p := volume.PatientToVoxel(patient)
			d := math.Min(distanceToSegment(p, segments[0][0], segments[0][1]), distanceToSegment(p, segments[branch][0], segments[branch][1]))
			if d > 3 {
				t.Fatalf("branch %d point %d left lumen: %+v distance %.3fmm", branch, i, p, d)
			}
		}
	}
}

func TestVesselGuidedCPRRejectsConflictingSpeedAndHonorsCancellation(t *testing.T) {
	volume, segments := cprBifurcationCalciumPhantom(t)
	points := []Vec3{volume.VoxelToPatient(segments[0][0]), volume.VoxelToPatient(segments[1][1])}
	for _, opts := range []CPRPathAssistantOptions{{VesselGuided: true, SpeedVolume: volume}, {VesselGuided: true, Method: CPRPathDijkstra}} {
		if _, err := FindCPRAssistedPath(context.Background(), volume, points, opts); !errors.Is(err, ErrCPRPathAssistantInput) {
			t.Fatal("invalid vessel guidance options accepted", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := FindCPRAssistedPath(ctx, volume, points, CPRPathAssistantOptions{VesselGuided: true}); !errors.Is(err, context.Canceled) {
		t.Fatal("cancelled vessel guidance proceeded", err)
	}
}
