package render

import (
	"errors"
	"fmt"
	"math"
	"sort"
)

// Volume bricks (#1246) partition a volume's voxel grid into bricks so a
// backend can keep only some of them resident: VolumeBrickGrid is the
// partition, RequiredVolumeBricks the bricks a view reads, VolumeBrickAtlas
// the arrangement of brick slots in one texture, and VolumeBrickPageTable
// the resident bricks with least-recently-used replacement. A backend that
// samples through the page table reads the same voxels as a whole-volume
// texture, so frames do not change while a view's required bricks are
// resident. VolumeSnapshot.WriteBrickContext streams one brick.

// DefaultVolumeBrickExtent is the largest brick edge for PlanVolumeBrickGrid
// callers without a constraint of their own: small enough that an MPR plane
// needs a thin layer of bricks, large enough that the page table of a 4096³
// volume has 32³ entries.
const DefaultVolumeBrickExtent uint32 = 128

// ErrInvalidVolumeBricks reports a brick grid, brick, atlas, or plan that
// does not describe its volume.
var ErrInvalidVolumeBricks = errors.New("render: invalid volume bricks")

// ErrVolumeBrickBudget reports a view that reads more bricks than a page
// table can hold.
var ErrVolumeBrickBudget = errors.New("render: required volume bricks exceed the brick capacity")

// VolumeBrickGrid partitions Dimensions into bricks of BrickSize voxels; the
// last brick along an axis holds the remainder. Bricks are indexed x fastest,
// then y, then z.
type VolumeBrickGrid struct {
	Dimensions [3]uint32
	BrickSize  [3]uint32
}

// VolumeBrick is one brick of a grid: the voxels [Origin, Origin+Size).
type VolumeBrick struct {
	Index  int
	Origin [3]uint32
	Size   [3]uint32
}

// Voxels is the number of voxels of the brick.
func (b VolumeBrick) Voxels() uint64 {
	return uint64(b.Size[0]) * uint64(b.Size[1]) * uint64(b.Size[2])
}

// PlanVolumeBrickGrid splits each axis into the fewest bricks of at most
// maxExtent voxels, all of one size but the last.
func PlanVolumeBrickGrid(dimensions [3]uint32, maxExtent uint32) (VolumeBrickGrid, error) {
	if maxExtent == 0 {
		return VolumeBrickGrid{}, fmt.Errorf("%w: zero brick extent", ErrInvalidVolumeBricks)
	}
	grid := VolumeBrickGrid{Dimensions: dimensions}
	for axis, dimension := range dimensions {
		if dimension == 0 {
			return VolumeBrickGrid{}, fmt.Errorf("%w: empty axis %d", ErrInvalidVolumeBricks, axis)
		}
		count := brickCeilDiv(dimension, maxExtent)
		grid.BrickSize[axis] = brickCeilDiv(dimension, count)
	}
	return grid, grid.Validate()
}

// Validate reports whether the grid has bricks and an index for each one.
func (g VolumeBrickGrid) Validate() error {
	bricks := uint64(1)
	for axis := range g.Dimensions {
		if g.Dimensions[axis] == 0 || g.BrickSize[axis] == 0 || g.BrickSize[axis] > g.Dimensions[axis] {
			return fmt.Errorf("%w: axis %d brick %d of %d voxels", ErrInvalidVolumeBricks, axis, g.BrickSize[axis], g.Dimensions[axis])
		}
		bricks *= uint64(brickCeilDiv(g.Dimensions[axis], g.BrickSize[axis]))
	}
	if bricks > math.MaxInt32 {
		return fmt.Errorf("%w: %d bricks", ErrInvalidVolumeBricks, bricks)
	}
	return nil
}

// Counts is the number of bricks along each axis.
func (g VolumeBrickGrid) Counts() [3]uint32 {
	var counts [3]uint32
	for axis := range counts {
		if g.BrickSize[axis] != 0 {
			counts[axis] = brickCeilDiv(g.Dimensions[axis], g.BrickSize[axis])
		}
	}
	return counts
}

// Len is the number of bricks.
func (g VolumeBrickGrid) Len() int {
	counts := g.Counts()
	return int(counts[0]) * int(counts[1]) * int(counts[2])
}

// Brick returns brick index.
func (g VolumeBrickGrid) Brick(index int) (VolumeBrick, error) {
	if err := g.Validate(); err != nil {
		return VolumeBrick{}, err
	}
	if index < 0 || index >= g.Len() {
		return VolumeBrick{}, fmt.Errorf("%w: brick %d of %d", ErrInvalidVolumeBricks, index, g.Len())
	}
	counts := g.Counts()
	coordinate := [3]uint32{
		uint32(index % int(counts[0])),
		uint32(index / int(counts[0]) % int(counts[1])),
		uint32(index / (int(counts[0]) * int(counts[1]))),
	}
	brick := VolumeBrick{Index: index}
	for axis := range coordinate {
		brick.Origin[axis] = coordinate[axis] * g.BrickSize[axis]
		brick.Size[axis] = min(g.BrickSize[axis], g.Dimensions[axis]-brick.Origin[axis])
	}
	return brick, nil
}

// BricksInBox returns, in index order, the bricks holding a voxel of the
// box [lo, hi] (voxel indices, inclusive), clipped to the volume.
func (g VolumeBrickGrid) BricksInBox(lo, hi [3]float64) []int {
	if g.Validate() != nil {
		return nil
	}
	counts := g.Counts()
	var first, last [3]int
	for axis := range lo {
		if !(lo[axis] <= hi[axis]) || hi[axis] < 0 || lo[axis] > float64(g.Dimensions[axis]-1) {
			return []int{}
		}
		low := max(0, math.Floor(lo[axis]))
		high := min(float64(g.Dimensions[axis]-1), math.Floor(hi[axis]))
		first[axis] = int(low) / int(g.BrickSize[axis])
		last[axis] = min(int(high)/int(g.BrickSize[axis]), int(counts[axis])-1)
	}
	bricks := []int{}
	for z := first[2]; z <= last[2]; z++ {
		for y := first[1]; y <= last[1]; y++ {
			for x := first[0]; x <= last[0]; x++ {
				bricks = append(bricks, x+int(counts[0])*(y+int(counts[1])*z))
			}
		}
	}
	return bricks
}

// volumeBrickStencilMargin is the voxel distance, beyond a sample point, of
// the voxels a sampler reads (reach: one voxel for nearest and trilinear
// reads, two for cubic reads and VR gradients), plus one voxel that absorbs
// the clamping of reads past the volume edge.
func volumeBrickStencilMargin(reach float64) float64 { return reach + 1 }

// RequiredVolumeBricks returns, in index order, the bricks whose voxels view
// can read on a volume described by descriptor and partitioned by grid. It
// is conservative: every voxel a backend reads for the view belongs to a
// returned brick, as long as the backend samples the view's geometry as the
// CPU renderers do. An MPR view reads the bricks within the sampler's reach
// of its pixel rectangle and slab; a VR view the bricks within reach of its
// camera frustum up to FarMM (scattering, whose shadow rays leave the
// frustum, reads every brick). A view the test cannot bound reads every
// brick.
func RequiredVolumeBricks(grid VolumeBrickGrid, descriptor VolumeDescriptor, view ViewState) ([]int, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if grid.Dimensions != descriptor.Dimensions {
		return nil, fmt.Errorf("%w: grid of %v voxels for a %v volume", ErrInvalidVolumeBricks, grid.Dimensions, descriptor.Dimensions)
	}
	var bricks []int
	switch view.Kind {
	case ViewKindMPR:
		bricks = requiredMPRBricks(grid, descriptor, view)
	case ViewKindVR:
		bricks = requiredVRBricks(grid, descriptor, view)
	default:
		return nil, fmt.Errorf("%w: view kind %d", ErrInvalidViewState, view.Kind)
	}
	if bricks == nil {
		bricks = make([]int, grid.Len())
		for index := range bricks {
			bricks[index] = index
		}
	}
	return bricks, nil
}

// requiredMPRBricks keeps the bricks whose dilated box meets the sampled
// parallelepiped origin + x·U + y·V + s·A (x, y over the pixels, s over the
// slab) in index space, by separating axes: the box axes and the
// parallelepiped's face normals. nil means every brick.
func requiredMPRBricks(grid VolumeBrickGrid, descriptor VolumeDescriptor, view ViewState) []int {
	mpr := view.MPR
	toIndex := descriptor.PatientLPSToIndex
	origin, okOrigin := brickAffinePoint(toIndex, mpr.PixelOriginLPS)
	u, okU := brickAffineVector(toIndex, mpr.PixelStepULPS)
	v, okV := brickAffineVector(toIndex, mpr.PixelStepVLPS)
	if !okOrigin || !okU || !okV || view.OutputWidth == 0 || view.OutputHeight == 0 {
		return nil
	}
	u = brickScale(u, float64(view.OutputWidth-1))
	v = brickScale(v, float64(view.OutputHeight-1))
	// A single slice is the parallelogram; the slab axis then has no extent.
	var a [3]float64
	extent := 0.0
	if mpr.SlabMode != SlabNone {
		axis, ok := brickAffineVector(toIndex, mpr.SlabAxisLPS)
		samples := math.Floor(mpr.SlabThicknessMM/mpr.SlabSampleSpacingMM) + 1
		if !ok || !finiteScalar(samples) || samples < 1 {
			return nil
		}
		a, extent = axis, 0.5*(samples-1)*mpr.SlabSampleSpacingMM
		if !finiteScalar(extent) {
			return nil
		}
	}
	reach := 1.0
	if mpr.Interpolation == InterpolationCubic {
		reach = 2
	}
	margin := volumeBrickStencilMargin(reach)
	start := brickAdd(origin, brickScale(a, -extent))
	a = brickScale(a, 2*extent)
	if !brickFinite(start) || !brickFinite(a) {
		return nil
	}
	corners := brickParallelepiped(start, u, v, a)
	lo, hi := brickCornerBounds(corners[:])
	for axis := range lo {
		lo[axis] -= margin
		hi[axis] += margin
	}
	candidates := grid.BricksInBox(lo, hi)
	n := brickCross(u, v)
	normals := [][3]float64{brickCross(v, a), brickCross(a, u), n}
	if brickDot(a, a) == 0 {
		// The parallelogram's in-plane edge normals replace the degenerate
		// slab faces.
		normals = [][3]float64{n, brickCross(n, u), brickCross(n, v)}
	}
	required := candidates[:0]
	for _, index := range candidates {
		brick, _ := grid.Brick(index)
		box := brickDilatedCorners(brick, margin)
		separated := false
		for _, normal := range normals {
			if brickDot(normal, normal) == 0 {
				continue
			}
			pLo, pHi := brickProjection(corners[:], normal)
			bLo, bHi := brickProjection(box[:], normal)
			if pHi < bLo || bHi < pLo {
				separated = true
				break
			}
		}
		if !separated {
			required = append(required, index)
		}
	}
	return required
}

// requiredVRBricks keeps the bricks whose dilated box, in patient space, is
// not entirely outside one plane of the camera frustum the ray casters
// build. nil means every brick.
func requiredVRBricks(grid VolumeBrickGrid, descriptor VolumeDescriptor, view ViewState) []int {
	vr := view.VR
	if vr.Scattering.Blend > 0 || view.OutputWidth == 0 || view.OutputHeight == 0 {
		return nil
	}
	forward := brickNormalize(brickSub(vr.FocalPointLPS, vr.PositionLPS))
	viewUp := brickNormalize(vr.ViewUpLPS)
	rightRaw := brickCross(forward, viewUp)
	if brickDot(rightRaw, rightRaw) == 0 {
		alternate := [3]float64{0, 0, 1}
		if math.Abs(forward[2]) > math.Abs(forward[0]) {
			alternate = [3]float64{1, 0, 0}
		}
		rightRaw = brickCross(forward, alternate)
	}
	right := brickNormalize(rightRaw)
	up := brickNormalize(brickCross(right, forward))
	if brickDot(forward, forward) == 0 || brickDot(right, right) == 0 || brickDot(up, up) == 0 {
		return nil
	}
	aspect := float64(view.OutputWidth) / float64(view.OutputHeight)
	// Each half-space keeps the points p with dot(normal, p-position) <= offset.
	type halfSpace struct {
		normal [3]float64
		offset float64
	}
	var planes []halfSpace
	switch vr.Projection {
	case ProjectionPerspective, ProjectionEndoscopy:
		tangent := math.Tan(0.5 * vr.VerticalFOVRadians)
		if !finiteScalar(tangent) || tangent <= 0 {
			return nil
		}
		// The side planes through the eye bound the ray directions
		// forward + a·right + b·up, |a| <= tan·aspect, |b| <= tan; together
		// they also exclude the points behind the eye.
		horizontal, vertical := tangent*aspect, tangent
		planes = []halfSpace{
			{normal: brickSub(right, brickScale(forward, horizontal))},
			{normal: brickSub(brickScale(right, -1), brickScale(forward, horizontal))},
			{normal: brickSub(up, brickScale(forward, vertical))},
			{normal: brickSub(brickScale(up, -1), brickScale(forward, vertical))},
		}
	case ProjectionParallel:
		halfHeight := vr.ParallelScaleMM
		if !finiteScalar(halfHeight) || halfHeight <= 0 {
			return nil
		}
		planes = []halfSpace{
			{normal: right, offset: halfHeight * aspect},
			{normal: brickScale(right, -1), offset: halfHeight * aspect},
			{normal: up, offset: halfHeight},
			{normal: brickScale(up, -1), offset: halfHeight},
		}
	default:
		return nil
	}
	if finiteScalar(vr.FarMM) && vr.FarMM > 0 {
		// Samples lie within FarMM of the eye along their ray, so their depth
		// along forward is at most FarMM.
		planes = append(planes, halfSpace{normal: forward, offset: vr.FarMM})
	}
	toPatient := descriptor.IndexToPatientLPS
	margin := volumeBrickStencilMargin(2)
	required := []int{}
	for index := 0; index < grid.Len(); index++ {
		brick, _ := grid.Brick(index)
		box := brickDilatedCorners(brick, margin)
		var corners [8][3]float64
		for corner, point := range box {
			patient, ok := brickAffinePoint(toPatient, point)
			if !ok {
				return nil
			}
			corners[corner] = brickSub(patient, vr.PositionLPS)
		}
		outside := false
		for _, plane := range planes {
			// A relative tolerance keeps rounding from culling a brick that
			// touches the plane.
			allOutside := true
			for _, corner := range corners {
				value := brickDot(plane.normal, corner)
				if value <= plane.offset+1e-9*(math.Abs(value)+math.Abs(plane.offset)+1) {
					allOutside = false
					break
				}
			}
			if allOutside {
				outside = true
				break
			}
		}
		if !outside {
			required = append(required, index)
		}
	}
	return required
}

func brickDilatedCorners(brick VolumeBrick, margin float64) [8][3]float64 {
	var lo, hi [3]float64
	for axis := range lo {
		lo[axis] = float64(brick.Origin[axis]) - margin
		hi[axis] = float64(brick.Origin[axis]+brick.Size[axis]-1) + margin
	}
	var corners [8][3]float64
	for corner := range corners {
		for axis := range lo {
			if corner&(1<<axis) != 0 {
				corners[corner][axis] = hi[axis]
			} else {
				corners[corner][axis] = lo[axis]
			}
		}
	}
	return corners
}

func brickParallelepiped(origin, u, v, a [3]float64) [8][3]float64 {
	var corners [8][3]float64
	for corner := range corners {
		point := origin
		if corner&1 != 0 {
			point = brickAdd(point, u)
		}
		if corner&2 != 0 {
			point = brickAdd(point, v)
		}
		if corner&4 != 0 {
			point = brickAdd(point, a)
		}
		corners[corner] = point
	}
	return corners
}

func brickCornerBounds(corners [][3]float64) (lo, hi [3]float64) {
	lo = [3]float64{math.Inf(1), math.Inf(1), math.Inf(1)}
	hi = [3]float64{math.Inf(-1), math.Inf(-1), math.Inf(-1)}
	for _, corner := range corners {
		for axis := range corner {
			lo[axis] = min(lo[axis], corner[axis])
			hi[axis] = max(hi[axis], corner[axis])
		}
	}
	return lo, hi
}

func brickProjection(corners [][3]float64, normal [3]float64) (lo, hi float64) {
	lo, hi = math.Inf(1), math.Inf(-1)
	for _, corner := range corners {
		value := brickDot(normal, corner)
		lo, hi = min(lo, value), max(hi, value)
	}
	// A relative tolerance keeps rounding from separating touching shapes.
	slack := 1e-9 * (math.Abs(lo) + math.Abs(hi) + 1)
	return lo - slack, hi + slack
}

func brickAffinePoint(matrix GeometryAffine, point [3]float64) ([3]float64, bool) {
	var result [3]float64
	for row := range result {
		result[row] = matrix[row*4]*point[0] + matrix[row*4+1]*point[1] + matrix[row*4+2]*point[2] + matrix[row*4+3]
	}
	return result, brickFinite(result)
}

func brickAffineVector(matrix GeometryAffine, vector [3]float64) ([3]float64, bool) {
	var result [3]float64
	for row := range result {
		result[row] = matrix[row*4]*vector[0] + matrix[row*4+1]*vector[1] + matrix[row*4+2]*vector[2]
	}
	return result, brickFinite(result)
}

func brickAdd(a, b [3]float64) [3]float64 { return [3]float64{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }

func brickSub(a, b [3]float64) [3]float64 { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }

func brickScale(a [3]float64, scale float64) [3]float64 {
	return [3]float64{a[0] * scale, a[1] * scale, a[2] * scale}
}

func brickDot(a, b [3]float64) float64 { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }

func brickCross(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}

func brickNormalize(a [3]float64) [3]float64 {
	length := math.Sqrt(brickDot(a, a))
	if length == 0 || !finiteScalar(length) {
		return [3]float64{}
	}
	return brickScale(a, 1/length)
}

func brickFinite(a [3]float64) bool {
	return finiteScalar(a[0]) && finiteScalar(a[1]) && finiteScalar(a[2])
}

func brickCeilDiv(value, divisor uint32) uint32 {
	return uint32((uint64(value) + uint64(divisor) - 1) / uint64(divisor))
}

// VolumeBrickAtlas arranges brick slots in one 3D texture: Slots slots along
// each axis, each of the grid's BrickSize, slot index x fastest.
type VolumeBrickAtlas struct {
	Slots [3]uint32
}

// PlanVolumeBrickAtlas returns the atlas with the fewest slots that holds
// capacity bricks of grid (at most every brick) within maxExtent voxels on
// each axis; among those, the one with the smallest largest extent.
func PlanVolumeBrickAtlas(grid VolumeBrickGrid, capacity int, maxExtent uint32) (VolumeBrickAtlas, error) {
	if err := grid.Validate(); err != nil {
		return VolumeBrickAtlas{}, err
	}
	capacity = min(capacity, grid.Len())
	if capacity <= 0 {
		return VolumeBrickAtlas{}, fmt.Errorf("%w: atlas capacity %d", ErrInvalidVolumeBricks, capacity)
	}
	var limit [3]int
	for axis := range limit {
		limit[axis] = int(maxExtent / grid.BrickSize[axis])
		if limit[axis] == 0 {
			return VolumeBrickAtlas{}, fmt.Errorf("%w: brick of %d voxels exceeds the %d-voxel atlas extent", ErrInvalidVolumeBricks, grid.BrickSize[axis], maxExtent)
		}
	}
	best, bestSlots, bestExtent := VolumeBrickAtlas{}, math.MaxInt, uint32(math.MaxUint32)
	for x := 1; x <= min(capacity, limit[0]); x++ {
		for y := 1; y <= min(brickCeilDivInt(capacity, x), limit[1]); y++ {
			z := brickCeilDivInt(capacity, x*y)
			if z > limit[2] {
				continue
			}
			candidate := VolumeBrickAtlas{Slots: [3]uint32{uint32(x), uint32(y), uint32(z)}}
			dimensions := candidate.Dimensions(grid)
			extent := max(dimensions[0], dimensions[1], dimensions[2])
			if slots := x * y * z; slots < bestSlots || slots == bestSlots && extent < bestExtent {
				best, bestSlots, bestExtent = candidate, slots, extent
			}
		}
	}
	if bestSlots == math.MaxInt {
		return VolumeBrickAtlas{}, fmt.Errorf("%w: %d bricks exceed an atlas of %v slots", ErrInvalidVolumeBricks, capacity, limit)
	}
	return best, nil
}

// MaxVolumeBrickSlots is the largest number of grid's bricks an atlas within
// maxExtent voxels on each axis holds.
func MaxVolumeBrickSlots(grid VolumeBrickGrid, maxExtent uint32) int {
	if grid.Validate() != nil {
		return 0
	}
	slots := 1
	for axis := range grid.BrickSize {
		slots *= int(maxExtent / grid.BrickSize[axis])
		if slots == 0 || slots >= grid.Len() {
			return min(slots, grid.Len())
		}
	}
	return slots
}

func brickCeilDivInt(value, divisor int) int { return (value + divisor - 1) / divisor }

// Capacity is the number of slots.
func (a VolumeBrickAtlas) Capacity() int {
	return int(a.Slots[0]) * int(a.Slots[1]) * int(a.Slots[2])
}

// Dimensions is the atlas texture's extent in voxels.
func (a VolumeBrickAtlas) Dimensions(grid VolumeBrickGrid) [3]uint32 {
	return [3]uint32{a.Slots[0] * grid.BrickSize[0], a.Slots[1] * grid.BrickSize[1], a.Slots[2] * grid.BrickSize[2]}
}

// SlotOrigin is the first voxel of slot in the atlas texture.
func (a VolumeBrickAtlas) SlotOrigin(grid VolumeBrickGrid, slot int) [3]uint32 {
	x := uint32(slot) % a.Slots[0]
	y := uint32(slot) / a.Slots[0] % a.Slots[1]
	z := uint32(slot) / (a.Slots[0] * a.Slots[1])
	return [3]uint32{x * grid.BrickSize[0], y * grid.BrickSize[1], z * grid.BrickSize[2]}
}

// VolumeBrickLoad places Brick in Slot.
type VolumeBrickLoad struct {
	Brick int
	Slot  int
}

// VolumeBrickPlan changes a page table: Evict frees the slots of its bricks,
// then each Load fills a free slot.
type VolumeBrickPlan struct {
	Evict []int             `json:",omitempty"`
	Load  []VolumeBrickLoad `json:",omitempty"`
}

// VolumeBrickPageTable maps the resident bricks of a grid to the slots of an
// atlas, replacing the least recently used bricks. Two tables that start
// alike and receive the same plans and touches stay alike, so a backend and
// its controller can each keep one.
type VolumeBrickPageTable struct {
	grid    VolumeBrickGrid
	slotOf  []int32 // per brick, -1 when not resident
	brickOf []int32 // per slot, -1 when free
	used    []uint64
	tick    uint64
}

// NewVolumeBrickPageTable returns an empty page table of capacity slots, at
// most one per brick.
func NewVolumeBrickPageTable(grid VolumeBrickGrid, capacity int) (*VolumeBrickPageTable, error) {
	if err := grid.Validate(); err != nil {
		return nil, err
	}
	if capacity <= 0 || capacity > grid.Len() {
		return nil, fmt.Errorf("%w: capacity %d for %d bricks", ErrInvalidVolumeBricks, capacity, grid.Len())
	}
	table := &VolumeBrickPageTable{
		grid:    grid,
		slotOf:  make([]int32, grid.Len()),
		brickOf: make([]int32, capacity),
		used:    make([]uint64, capacity),
	}
	for index := range table.slotOf {
		table.slotOf[index] = -1
	}
	for slot := range table.brickOf {
		table.brickOf[slot] = -1
	}
	return table, nil
}

// InitialVolumeBrickPlan fills an empty table of capacity slots with the
// first bricks in index order, brick i in slot i: the bricks an upload that
// streams the volume in z order can write as it passes them.
func InitialVolumeBrickPlan(grid VolumeBrickGrid, capacity int) VolumeBrickPlan {
	var plan VolumeBrickPlan
	for index := 0; index < min(capacity, grid.Len()); index++ {
		plan.Load = append(plan.Load, VolumeBrickLoad{Brick: index, Slot: index})
	}
	return plan
}

// Clone returns an independent copy of the table, to try a plan on.
func (t *VolumeBrickPageTable) Clone() *VolumeBrickPageTable {
	return &VolumeBrickPageTable{
		grid:    t.grid,
		slotOf:  append([]int32(nil), t.slotOf...),
		brickOf: append([]int32(nil), t.brickOf...),
		used:    append([]uint64(nil), t.used...),
		tick:    t.tick,
	}
}

// Grid is the table's brick grid.
func (t *VolumeBrickPageTable) Grid() VolumeBrickGrid { return t.grid }

// Capacity is the number of slots.
func (t *VolumeBrickPageTable) Capacity() int { return len(t.brickOf) }

// Slot returns the slot of a resident brick.
func (t *VolumeBrickPageTable) Slot(brick int) (int, bool) {
	if brick < 0 || brick >= len(t.slotOf) || t.slotOf[brick] < 0 {
		return 0, false
	}
	return int(t.slotOf[brick]), true
}

// Resident returns the resident bricks in index order.
func (t *VolumeBrickPageTable) Resident() []int {
	var bricks []int
	for brick, slot := range t.slotOf {
		if slot >= 0 {
			bricks = append(bricks, brick)
		}
	}
	return bricks
}

// Missing returns the bricks of required that are not resident.
func (t *VolumeBrickPageTable) Missing(required []int) []int {
	var missing []int
	for _, brick := range required {
		if _, ok := t.Slot(brick); !ok {
			missing = append(missing, brick)
		}
	}
	return missing
}

// Plan returns the plan that makes required resident: missing bricks, in
// index order, fill free slots, then the slots of the least recently used
// bricks that are not required (ties broken by brick index). It returns
// ErrVolumeBrickBudget when required holds more bricks than the table.
func (t *VolumeBrickPageTable) Plan(required []int) (VolumeBrickPlan, error) {
	wanted, err := t.normalize(required)
	if err != nil {
		return VolumeBrickPlan{}, err
	}
	if len(wanted) > len(t.brickOf) {
		return VolumeBrickPlan{}, fmt.Errorf("%w: %d bricks, capacity %d", ErrVolumeBrickBudget, len(wanted), len(t.brickOf))
	}
	missing := t.Missing(wanted)
	if len(missing) == 0 {
		return VolumeBrickPlan{}, nil
	}
	keep := make(map[int]bool, len(wanted))
	for _, brick := range wanted {
		keep[brick] = true
	}
	var free, victims []int
	for slot, brick := range t.brickOf {
		switch {
		case brick < 0:
			free = append(free, slot)
		case !keep[int(brick)]:
			victims = append(victims, slot)
		}
	}
	sort.SliceStable(victims, func(i, j int) bool {
		a, b := victims[i], victims[j]
		if t.used[a] != t.used[b] {
			return t.used[a] < t.used[b]
		}
		return t.brickOf[a] < t.brickOf[b]
	})
	var plan VolumeBrickPlan
	for _, brick := range missing {
		var slot int
		if len(free) != 0 {
			slot, free = free[0], free[1:]
		} else {
			slot, victims = victims[0], victims[1:]
			plan.Evict = append(plan.Evict, int(t.brickOf[slot]))
		}
		plan.Load = append(plan.Load, VolumeBrickLoad{Brick: brick, Slot: slot})
	}
	return plan, nil
}

// Apply validates and applies plan; on error it changes nothing.
func (t *VolumeBrickPageTable) Apply(plan VolumeBrickPlan) error {
	freed := make(map[int]bool, len(plan.Evict))
	for _, brick := range plan.Evict {
		slot, ok := t.Slot(brick)
		if !ok || freed[slot] {
			return fmt.Errorf("%w: evicting brick %d that is not resident", ErrInvalidVolumeBricks, brick)
		}
		freed[slot] = true
	}
	filled := make(map[int]bool, len(plan.Load))
	loaded := make(map[int]bool, len(plan.Load))
	for _, load := range plan.Load {
		if load.Brick < 0 || load.Brick >= len(t.slotOf) || load.Slot < 0 || load.Slot >= len(t.brickOf) ||
			filled[load.Slot] || loaded[load.Brick] || (t.brickOf[load.Slot] >= 0 && !freed[load.Slot]) {
			return fmt.Errorf("%w: loading brick %d into slot %d", ErrInvalidVolumeBricks, load.Brick, load.Slot)
		}
		if slot, resident := t.Slot(load.Brick); resident && !freed[slot] {
			return fmt.Errorf("%w: brick %d is already resident", ErrInvalidVolumeBricks, load.Brick)
		}
		filled[load.Slot] = true
		loaded[load.Brick] = true
	}
	t.tick++
	for _, brick := range plan.Evict {
		slot := t.slotOf[brick]
		t.slotOf[brick] = -1
		t.brickOf[slot] = -1
	}
	for _, load := range plan.Load {
		t.slotOf[load.Brick] = int32(load.Slot)
		t.brickOf[load.Slot] = int32(load.Brick)
		t.used[load.Slot] = t.tick
	}
	return nil
}

// Touch marks the resident bricks among bricks as used now.
func (t *VolumeBrickPageTable) Touch(bricks []int) {
	t.tick++
	for _, brick := range bricks {
		if slot, ok := t.Slot(brick); ok {
			t.used[slot] = t.tick
		}
	}
}

func (t *VolumeBrickPageTable) normalize(bricks []int) ([]int, error) {
	sorted := append([]int(nil), bricks...)
	sort.Ints(sorted)
	unique := sorted[:0]
	for index, brick := range sorted {
		if brick < 0 || brick >= len(t.slotOf) {
			return nil, fmt.Errorf("%w: brick %d of %d", ErrInvalidVolumeBricks, brick, len(t.slotOf))
		}
		if index == 0 || brick != sorted[index-1] {
			unique = append(unique, brick)
		}
	}
	return unique, nil
}
