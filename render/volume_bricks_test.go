package render

import (
	"bytes"
	"context"
	"errors"
	"math"
	"slices"
	"testing"
)

func TestPlanVolumeBrickGrid(t *testing.T) {
	cases := []struct {
		dimensions [3]uint32
		max        uint32
		size       [3]uint32
		counts     [3]uint32
	}{
		{dimensions: [3]uint32{512, 512, 300}, max: 128, size: [3]uint32{128, 128, 100}, counts: [3]uint32{4, 4, 3}},
		{dimensions: [3]uint32{2100, 6, 5}, max: 2048, size: [3]uint32{1050, 6, 5}, counts: [3]uint32{2, 1, 1}},
		{dimensions: [3]uint32{9, 10, 1}, max: 4, size: [3]uint32{3, 4, 1}, counts: [3]uint32{3, 3, 1}},
		{dimensions: [3]uint32{12, 12, 12}, max: 5, size: [3]uint32{4, 4, 4}, counts: [3]uint32{3, 3, 3}},
	}
	for _, tc := range cases {
		grid, err := PlanVolumeBrickGrid(tc.dimensions, tc.max)
		if err != nil || grid.BrickSize != tc.size || grid.Counts() != tc.counts {
			t.Fatalf("PlanVolumeBrickGrid(%v, %d) = %+v (counts %v), %v", tc.dimensions, tc.max, grid, grid.Counts(), err)
		}
		// Every voxel belongs to exactly one brick.
		covered := make(map[[3]uint32]int)
		for index := 0; index < grid.Len(); index++ {
			brick, err := grid.Brick(index)
			if err != nil || brick.Voxels() == 0 {
				t.Fatalf("brick %d = %+v, %v", index, brick, err)
			}
			if tc.dimensions[0]*tc.dimensions[1]*tc.dimensions[2] > 1<<16 {
				continue
			}
			for z := brick.Origin[2]; z < brick.Origin[2]+brick.Size[2]; z++ {
				for y := brick.Origin[1]; y < brick.Origin[1]+brick.Size[1]; y++ {
					for x := brick.Origin[0]; x < brick.Origin[0]+brick.Size[0]; x++ {
						covered[[3]uint32{x, y, z}]++
					}
				}
			}
		}
		if voxels := int(tc.dimensions[0] * tc.dimensions[1] * tc.dimensions[2]); voxels <= 1<<16 && len(covered) != voxels {
			t.Fatalf("%v: bricks cover %d of %d voxels", tc.dimensions, len(covered), voxels)
		}
		for voxel, count := range covered {
			if count != 1 {
				t.Fatalf("voxel %v in %d bricks", voxel, count)
			}
		}
	}
	if _, err := PlanVolumeBrickGrid([3]uint32{4, 0, 4}, 8); !errors.Is(err, ErrInvalidVolumeBricks) {
		t.Fatalf("empty axis error = %v", err)
	}
}

func TestPlanVolumeBrickAtlas(t *testing.T) {
	grid, err := PlanVolumeBrickGrid([3]uint32{2100, 512, 300}, 256)
	if err != nil {
		t.Fatal(err)
	}
	if grid.Len() != 9*2*2 {
		t.Fatalf("grid %+v holds %d bricks", grid, grid.Len())
	}
	atlas, err := PlanVolumeBrickAtlas(grid, grid.Len(), 2048)
	if err != nil {
		t.Fatal(err)
	}
	dimensions := atlas.Dimensions(grid)
	if atlas.Capacity() != grid.Len() || max(dimensions[0], dimensions[1], dimensions[2]) > 2048 {
		t.Fatalf("atlas %+v of %v voxels for %d bricks", atlas, dimensions, grid.Len())
	}
	origins := make(map[[3]uint32]bool)
	for slot := 0; slot < atlas.Capacity(); slot++ {
		origin := atlas.SlotOrigin(grid, slot)
		for axis := range origin {
			if origin[axis]+grid.BrickSize[axis] > dimensions[axis] {
				t.Fatalf("slot %d at %v leaves the %v atlas", slot, origin, dimensions)
			}
		}
		origins[origin] = true
	}
	if len(origins) != atlas.Capacity() {
		t.Fatalf("%d distinct slot origins for %d slots", len(origins), atlas.Capacity())
	}
	if small, err := PlanVolumeBrickAtlas(grid, 5, 2048); err != nil || small.Capacity() < 5 || small.Capacity() > 6 {
		t.Fatalf("five-brick atlas = %+v, %v", small, err)
	}
	if slots := MaxVolumeBrickSlots(grid, 2048); slots != grid.Len() {
		t.Fatalf("max slots = %d", slots)
	}
	if _, err := PlanVolumeBrickAtlas(grid, 4, 128); !errors.Is(err, ErrInvalidVolumeBricks) {
		t.Fatalf("brick wider than the atlas extent: %v", err)
	}
}

func TestVolumeBrickPageTablePlansLeastRecentlyUsed(t *testing.T) {
	grid, err := PlanVolumeBrickGrid([3]uint32{8, 8, 8}, 4)
	if err != nil {
		t.Fatal(err)
	}
	table, err := NewVolumeBrickPageTable(grid, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := table.Apply(InitialVolumeBrickPlan(grid, 3)); err != nil {
		t.Fatal(err)
	}
	if got := table.Resident(); !slices.Equal(got, []int{0, 1, 2}) {
		t.Fatalf("initial residents %v", got)
	}
	table.Touch([]int{0})
	table.Touch([]int{2})
	// Brick 1 is the least recently used, then 0.
	plan, err := table.Plan([]int{2, 5})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(plan.Evict, []int{1}) || len(plan.Load) != 1 || plan.Load[0] != (VolumeBrickLoad{Brick: 5, Slot: 1}) {
		t.Fatalf("plan = %+v", plan)
	}
	if err := table.Apply(plan); err != nil {
		t.Fatal(err)
	}
	table.Touch([]int{2, 5})
	if slot, ok := table.Slot(5); !ok || slot != 1 {
		t.Fatalf("brick 5 in slot %d, %v", slot, ok)
	}
	if plan, err := table.Plan([]int{5, 2}); err != nil || len(plan.Load) != 0 || len(plan.Evict) != 0 {
		t.Fatalf("resident plan = %+v, %v", plan, err)
	}
	if _, err := table.Plan([]int{1, 3, 4, 6}); !errors.Is(err, ErrVolumeBrickBudget) {
		t.Fatalf("over-capacity plan error = %v", err)
	}
	if err := table.Apply(VolumeBrickPlan{Load: []VolumeBrickLoad{{Brick: 7, Slot: 1}}}); !errors.Is(err, ErrInvalidVolumeBricks) {
		t.Fatalf("loading into an occupied slot: %v", err)
	}
	if err := table.Apply(VolumeBrickPlan{Evict: []int{6}}); !errors.Is(err, ErrInvalidVolumeBricks) {
		t.Fatalf("evicting a brick that is not resident: %v", err)
	}
	// Two tables fed the same plans and touches stay alike.
	other, _ := NewVolumeBrickPageTable(grid, 3)
	_ = other.Apply(InitialVolumeBrickPlan(grid, 3))
	other.Touch([]int{0})
	other.Touch([]int{2})
	mirrored, _ := other.Plan([]int{2, 5})
	if !slices.Equal(mirrored.Evict, []int{1}) || !slices.Equal(mirrored.Load, []VolumeBrickLoad{{Brick: 5, Slot: 1}}) {
		t.Fatalf("mirrored plan %+v", mirrored)
	}
}

// bricksTestDescriptor is a rotated, anisotropic, offset volume.
func bricksTestDescriptor(dimensions [3]uint32) VolumeDescriptor {
	descriptor := testVolumeDescriptor(dimensions[0], dimensions[1], dimensions[2])
	c, s := math.Cos(0.3), math.Sin(0.3)
	spacing := [3]float64{0.8, 1.1, 1.7}
	rotation := [3][3]float64{{c, -s, 0}, {s * 0.9, c * 0.9, 0.4359}, {-s * 0.4359, -c * 0.4359, 0.9}}
	origin := [3]float64{-20, 13, 40}
	var toPatient GeometryAffine
	for row := 0; row < 3; row++ {
		for column := 0; column < 3; column++ {
			toPatient[row*4+column] = rotation[row][column] * spacing[column]
		}
		toPatient[row*4+3] = origin[row]
	}
	toPatient[15] = 1
	// The inverse of the linear part by cofactors.
	m := func(r, c int) float64 { return toPatient[r*4+c] }
	det := m(0, 0)*(m(1, 1)*m(2, 2)-m(1, 2)*m(2, 1)) - m(0, 1)*(m(1, 0)*m(2, 2)-m(1, 2)*m(2, 0)) + m(0, 2)*(m(1, 0)*m(2, 1)-m(1, 1)*m(2, 0))
	inverse := [3][3]float64{
		{(m(1, 1)*m(2, 2) - m(1, 2)*m(2, 1)) / det, (m(0, 2)*m(2, 1) - m(0, 1)*m(2, 2)) / det, (m(0, 1)*m(1, 2) - m(0, 2)*m(1, 1)) / det},
		{(m(1, 2)*m(2, 0) - m(1, 0)*m(2, 2)) / det, (m(0, 0)*m(2, 2) - m(0, 2)*m(2, 0)) / det, (m(0, 2)*m(1, 0) - m(0, 0)*m(1, 2)) / det},
		{(m(1, 0)*m(2, 1) - m(1, 1)*m(2, 0)) / det, (m(0, 1)*m(2, 0) - m(0, 0)*m(2, 1)) / det, (m(0, 0)*m(1, 1) - m(0, 1)*m(1, 0)) / det},
	}
	var toIndex GeometryAffine
	for row := 0; row < 3; row++ {
		for column := 0; column < 3; column++ {
			toIndex[row*4+column] = inverse[row][column]
		}
		toIndex[row*4+3] = -(inverse[row][0]*origin[0] + inverse[row][1]*origin[1] + inverse[row][2]*origin[2])
	}
	toIndex[15] = 1
	descriptor.IndexToPatientLPS, descriptor.PatientLPSToIndex = toPatient, toIndex
	descriptor.SpacingMM = spacing
	return descriptor
}

// readBricks adds the bricks of the voxels read around index point q with
// taps [lo, hi] relative to floor(q) (or floor(q+0.5) for nearest), clamped.
func readBricks(grid VolumeBrickGrid, q [3]float64, lo, hi int, nearest bool, read map[int]bool) {
	var base [3]int
	for axis := range q {
		if nearest {
			base[axis] = int(math.Floor(q[axis] + 0.5))
		} else {
			base[axis] = int(math.Floor(q[axis]))
		}
	}
	counts := grid.Counts()
	for dz := lo; dz <= hi; dz++ {
		for dy := lo; dy <= hi; dy++ {
			for dx := lo; dx <= hi; dx++ {
				voxel := [3]int{base[0] + dx, base[1] + dy, base[2] + dz}
				var b [3]int
				for axis := range voxel {
					v := min(max(voxel[axis], 0), int(grid.Dimensions[axis])-1)
					b[axis] = v / int(grid.BrickSize[axis])
				}
				read[b[0]+int(counts[0])*(b[1]+int(counts[1])*b[2])] = true
			}
		}
	}
}

func assertBricksCover(t *testing.T, name string, required []int, read map[int]bool, total int, wantCulled bool) {
	t.Helper()
	have := make(map[int]bool, len(required))
	for _, brick := range required {
		have[brick] = true
	}
	for brick := range read {
		if !have[brick] {
			t.Fatalf("%s: brick %d is read but not required (%d required)", name, brick, len(required))
		}
	}
	if wantCulled && len(required) >= total {
		t.Fatalf("%s: every one of %d bricks required", name, total)
	}
	if !slices.IsSorted(required) {
		t.Fatalf("%s: required bricks not in index order", name)
	}
}

// TestRequiredMPRBricksCoverEverySample (#1246) samples MPR views as the
// renderers do and checks that every voxel read lies in a required brick,
// while bricks away from the plane are left out.
func TestRequiredMPRBricksCoverEverySample(t *testing.T) {
	dimensions := [3]uint32{40, 36, 30}
	descriptor := bricksTestDescriptor(dimensions)
	grid, err := PlanVolumeBrickGrid(dimensions, 8)
	if err != nil {
		t.Fatal(err)
	}
	toPatient := descriptor.IndexToPatientLPS
	column := func(c int) [3]float64 { return [3]float64{toPatient[c], toPatient[4+c], toPatient[8+c]} }
	center, _ := brickAffinePoint(toPatient, [3]float64{20, 17, 14})
	type mprCase struct {
		name          string
		u, v, axis    [3]float64
		interpolation Interpolation
		slab          SlabMode
		thickness     float64
		culled        bool
	}
	oblique := brickNormalize([3]float64{0.3, 0.5, 0.8})
	obliqueU := brickNormalize(brickCross(oblique, [3]float64{1, 0, 0}))
	obliqueV := brickCross(oblique, obliqueU)
	cases := []mprCase{
		{name: "index plane linear", u: column(0), v: column(1), axis: brickNormalize(column(2)), interpolation: InterpolationLinear, culled: true},
		{name: "index plane nearest", u: column(0), v: column(1), axis: brickNormalize(column(2)), interpolation: InterpolationNearest, culled: true},
		{name: "index plane cubic", u: column(0), v: column(2), axis: brickNormalize(column(1)), interpolation: InterpolationCubic, culled: true},
		{name: "slab MIP", u: column(0), v: column(1), axis: brickNormalize(column(2)), interpolation: InterpolationLinear, slab: SlabMIP, thickness: 6, culled: true},
		{name: "oblique", u: brickScale(obliqueU, 0.7), v: brickScale(obliqueV, 0.7), axis: oblique, interpolation: InterpolationLinear, culled: true},
		{name: "oblique thick slab", u: brickScale(obliqueU, 0.9), v: brickScale(obliqueV, 0.9), axis: oblique, interpolation: InterpolationCubic, slab: SlabAverage, thickness: 9},
	}
	for _, tc := range cases {
		width, height := uint32(52), uint32(44)
		origin := brickSub(center, brickAdd(brickScale(tc.u, float64(width)/2), brickScale(tc.v, float64(height)/2)))
		view := ViewState{Kind: ViewKindMPR, OutputWidth: width, OutputHeight: height, MPR: MPRViewState{
			PixelOriginLPS: origin, PixelStepULPS: tc.u, PixelStepVLPS: tc.v,
			Interpolation: tc.interpolation, SlabMode: tc.slab, SlabAxisLPS: tc.axis,
			SlabThicknessMM: tc.thickness, SlabSampleSpacingMM: 0.7,
		}}
		required, err := RequiredVolumeBricks(grid, descriptor, view)
		if err != nil {
			t.Fatal(err)
		}
		count := 1
		if tc.slab != SlabNone {
			count = int(math.Floor(tc.thickness/0.7)) + 1
		}
		read := make(map[int]bool)
		for y := 0; y < int(height); y++ {
			for x := 0; x < int(width); x++ {
				for s := 0; s < count; s++ {
					offset := (float64(s) - 0.5*float64(count-1)) * 0.7
					patient := brickAdd(brickAdd(origin, brickScale(tc.u, float64(x))), brickAdd(brickScale(tc.v, float64(y)), brickScale(tc.axis, offset)))
					q, _ := brickAffinePoint(descriptor.PatientLPSToIndex, patient)
					valid := true
					for axis := range q {
						if q[axis] < -0.501 || q[axis] > float64(dimensions[axis])-0.499 {
							valid = false
						}
					}
					if !valid {
						continue
					}
					switch tc.interpolation {
					case InterpolationNearest:
						readBricks(grid, q, 0, 0, true, read)
					case InterpolationCubic:
						readBricks(grid, q, -1, 2, false, read)
					default:
						readBricks(grid, q, 0, 1, false, read)
					}
				}
			}
		}
		if len(read) == 0 {
			t.Fatalf("%s: the view reads no voxel", tc.name)
		}
		assertBricksCover(t, tc.name, required, read, grid.Len(), tc.culled)
	}
}

// TestRequiredVRBricksCoverEveryRaySample (#1246) marches the rays of
// perspective and parallel cameras, reading the gradient stencil around
// each sample, and checks the frustum test keeps every brick read while a
// camera looking at one corner leaves the far bricks out.
func TestRequiredVRBricksCoverEveryRaySample(t *testing.T) {
	dimensions := [3]uint32{40, 36, 30}
	descriptor := bricksTestDescriptor(dimensions)
	grid, err := PlanVolumeBrickGrid(dimensions, 8)
	if err != nil {
		t.Fatal(err)
	}
	center, _ := brickAffinePoint(descriptor.IndexToPatientLPS, [3]float64{19.5, 17.5, 14.5})
	corner, _ := brickAffinePoint(descriptor.IndexToPatientLPS, [3]float64{4, 4, 4})
	type vrCase struct {
		name       string
		position   [3]float64
		focal      [3]float64
		projection Projection
		fov, scale float64
		far        float64
		culled     bool
	}
	cases := []vrCase{
		{name: "perspective overview", position: brickAdd(center, [3]float64{0, -120, 30}), focal: center, projection: ProjectionPerspective, fov: 0.6, far: 400},
		{name: "perspective corner zoom", position: brickAdd(corner, [3]float64{-10, -40, 8}), focal: corner, projection: ProjectionPerspective, fov: 0.25, far: 400, culled: true},
		{name: "parallel corner zoom", position: brickAdd(corner, [3]float64{-60, 0, 10}), focal: corner, projection: ProjectionParallel, scale: 6, far: 400, culled: true},
		{name: "perspective short far", position: brickAdd(center, [3]float64{0, -60, 0}), focal: center, projection: ProjectionPerspective, fov: 0.9, far: 60, culled: true},
		{name: "endoscopy inside", position: center, focal: brickAdd(center, [3]float64{1, 0.2, 0.1}), projection: ProjectionEndoscopy, fov: 1.2, far: 400, culled: true},
	}
	for _, tc := range cases {
		width, height := uint32(48), uint32(36)
		view := ViewState{Kind: ViewKindVR, OutputWidth: width, OutputHeight: height, VR: VRViewState{
			PositionLPS: tc.position, FocalPointLPS: tc.focal, ViewUpLPS: [3]float64{0, 0, 1},
			Projection: tc.projection, VerticalFOVRadians: tc.fov, ParallelScaleMM: tc.scale,
			NearMM: 0, FarMM: tc.far, SampleDistanceMM: 0.5,
		}}
		required, err := RequiredVolumeBricks(grid, descriptor, view)
		if err != nil {
			t.Fatal(err)
		}
		forward := brickNormalize(brickSub(tc.focal, tc.position))
		right := brickNormalize(brickCross(forward, [3]float64{0, 0, 1}))
		up := brickNormalize(brickCross(right, forward))
		aspect := float64(width) / float64(height)
		read := make(map[int]bool)
		for py := 0; py < int(height); py++ {
			for px := 0; px < int(width); px++ {
				ndcX := (2*(float64(px)+0.5)/float64(width) - 1) * aspect
				ndcY := 1 - 2*(float64(py)+0.5)/float64(height)
				origin, direction := tc.position, forward
				if tc.projection == ProjectionParallel {
					origin = brickAdd(origin, brickAdd(brickScale(right, ndcX*tc.scale), brickScale(up, ndcY*tc.scale)))
				} else {
					tangent := math.Tan(tc.fov / 2)
					direction = brickNormalize(brickAdd(forward, brickAdd(brickScale(right, ndcX*tangent), brickScale(up, ndcY*tangent))))
				}
				for distance := 0.0; distance <= tc.far; distance += 0.5 {
					q, _ := brickAffinePoint(descriptor.PatientLPSToIndex, brickAdd(origin, brickScale(direction, distance)))
					inside := true
					for axis := range q {
						if q[axis] < 0 || q[axis] > float64(dimensions[axis]-1) {
							inside = false
						}
					}
					if inside {
						readBricks(grid, q, -1, 2, false, read)
					}
				}
			}
		}
		if len(read) == 0 {
			t.Fatalf("%s: no ray enters the volume", tc.name)
		}
		assertBricksCover(t, tc.name, required, read, grid.Len(), tc.culled)
	}
	// Scattering shadow rays leave the frustum: every brick.
	view := ViewState{Kind: ViewKindVR, OutputWidth: 8, OutputHeight: 8, VR: VRViewState{
		PositionLPS: corner, FocalPointLPS: center, ViewUpLPS: [3]float64{0, 0, 1},
		Projection: ProjectionPerspective, VerticalFOVRadians: 0.1, FarMM: 400,
		Scattering: VRScatteringState{Blend: 0.5, ReachMM: 10},
	}}
	if required, _ := RequiredVolumeBricks(grid, descriptor, view); len(required) != grid.Len() {
		t.Fatalf("scattering requires %d of %d bricks", len(required), grid.Len())
	}
}

func brickTestSnapshot(t *testing.T, stack *Stack) VolumeSnapshot {
	t.Helper()
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { volume.Close() })
	if _, err := volume.PrepareSnapshotContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	lease, err := volume.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { lease.Release() })
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// TestWriteBrickContextCutsTheWholeStream (#1246): each brick's bytes are
// its voxels cut from the whole-volume stream, for stored 16-bit samples and
// float32 modality values.
func TestWriteBrickContextCutsTheWholeStream(t *testing.T) {
	stacks := map[string]*Stack{
		"packed int16": stack16(11, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue),
		"float32":      sphereStack(9, 3),
	}
	for name, stack := range stacks {
		t.Run(name, func(t *testing.T) {
			snapshot := brickTestSnapshot(t, stack)
			descriptor, _ := snapshot.Descriptor()
			for _, stored16 := range []bool{false, true} {
				if stored16 && descriptor.ScalarFormat == VolumeScalarF32ModalityLE {
					continue
				}
				var whole bytes.Buffer
				texel := 4
				if stored16 {
					texel = 2
					if err := snapshot.WriteStored16Context(context.Background(), &whole); err != nil {
						t.Fatal(err)
					}
				} else if err := snapshot.WriteModalityF32To(&whole); err != nil {
					t.Fatal(err)
				}
				grid, err := PlanVolumeBrickGrid(descriptor.Dimensions, 4)
				if err != nil {
					t.Fatal(err)
				}
				for index := 0; index < grid.Len(); index++ {
					brick, _ := grid.Brick(index)
					var got bytes.Buffer
					if err := snapshot.WriteBrickContext(context.Background(), &got, brick, stored16); err != nil {
						t.Fatal(err)
					}
					var want []byte
					dims := descriptor.Dimensions
					for z := brick.Origin[2]; z < brick.Origin[2]+brick.Size[2]; z++ {
						for y := brick.Origin[1]; y < brick.Origin[1]+brick.Size[1]; y++ {
							start := ((int(z)*int(dims[1])+int(y))*int(dims[0]) + int(brick.Origin[0])) * texel
							want = append(want, whole.Bytes()[start:start+int(brick.Size[0])*texel]...)
						}
					}
					if !bytes.Equal(got.Bytes(), want) {
						t.Fatalf("brick %d (stored16 %v) differs from the whole stream", index, stored16)
					}
				}
			}
			outside := VolumeBrick{Origin: descriptor.Dimensions, Size: [3]uint32{1, 1, 1}}
			if err := snapshot.WriteBrickContext(context.Background(), &bytes.Buffer{}, outside, false); !errors.Is(err, ErrInvalidVolumeBricks) {
				t.Fatalf("brick outside the volume: %v", err)
			}
		})
	}
}
