package render

import (
	"fmt"
	"math"
)

// VolumeReader holds one read lease on the canonical immutable volume
// generation. It is safe to share between rendering workers as long as Close
// runs after those workers finish.
//
// Acquiring one reader per render avoids store synchronization in inner pixel
// loops while preserving generation lifetime across replacement and eviction.
type VolumeReader struct {
	source  *Volume
	sampler *volumeSampler
}

// AcquireReader acquires the volume generation once for repeated sampling.
// Geometry that requires correction is resolved to its explicit regularized
// generation before the reader is returned.
func (v *Volume) AcquireReader() (*VolumeReader, error) {
	if v == nil {
		return nil, fmt.Errorf("render: nil volume")
	}
	if v.closed.Load() {
		return nil, ErrVolumeStoreClosed
	}
	sampler, err := newVolumeSamplerWithError(v)
	if err != nil {
		if v.closed.Load() {
			return nil, ErrVolumeStoreClosed
		}
		return nil, err
	}
	return &VolumeReader{source: v, sampler: sampler}, nil
}

// TrilinearAt samples fractional voxel coordinates in the source Volume's
// coordinate system. If the source required regularization, the coordinate is
// mapped through patient space to the canonical regular grid.
func (r *VolumeReader) TrilinearAt(p Vec3) (float64, bool) {
	if r == nil || r.source == nil || r.sampler == nil {
		return 0, false
	}
	if r.sampler.vol != r.source {
		p = r.sampler.vol.PatientToVoxel(r.source.VoxelToPatient(p))
	}
	return r.sampler.trilinearAt(p)
}

// SamplePatient samples a point expressed in DICOM patient LPS coordinates.
func (r *VolumeReader) SamplePatient(patient Vec3) (float64, bool) {
	if r == nil || r.sampler == nil || r.sampler.vol == nil {
		return 0, false
	}
	return r.sampler.trilinearAt(r.sampler.vol.PatientToVoxel(patient))
}

// Close releases the generation lease. Close is idempotent and must run after
// all concurrent sampling calls have completed.
func (r *VolumeReader) Close() error {
	if r == nil || r.sampler == nil {
		return nil
	}
	r.sampler.Close()
	r.sampler = nil
	r.source = nil
	return nil
}

type volumeSampler struct {
	vol        *Volume
	lease      *VolumeLease
	descriptor VolumeDescriptor
	payload    volumePayload
	values     []float32
	// Packed 16-bit stored samples (#1065) with their rescale.
	signed16         []int16
	unsigned16       []uint16
	slope, intercept float64
	// Selected once: bounded affine reassociation for trilinear-only sampling.
	packed16Affine bool
	cols           int
	rows           int
	depth          int
	photometric    string
	invert         bool
}

// newVolumeSampler creates a volumeSampler from the given Volume, validating its dimensions and determining its photometric interpretation. It returns the sampler and true if vol is valid and non-nil, or nil and false otherwise.
func newVolumeSampler(vol *Volume) (*volumeSampler, bool) {
	sampler, err := newVolumeSamplerWithError(vol)
	return sampler, err == nil
}

func newVolumeSamplerWithError(vol *Volume) (*volumeSampler, error) {
	if vol == nil || vol.Cols <= 0 || vol.Rows <= 0 || vol.Depth <= 0 {
		return nil, fmt.Errorf("render: invalid volume dimensions")
	}
	if vol.geometry.RequiresResampling {
		grid, err := vol.RegularGrid()
		if err != nil {
			return nil, err
		}
		vol = grid
	}
	lease, err := vol.acquireDirectSnapshot()
	if err != nil {
		return nil, err
	}
	return newVolumeSamplerFromLease(vol, lease)
}

// newVolumeSamplerFromLease resolves one already-acquired immutable generation
// into the lock-free sampler used by CPU rendering and render-only preparation.
// Ownership of lease transfers to the returned sampler; failures release it.
func newVolumeSamplerFromLease(vol *Volume, lease *VolumeLease) (*volumeSampler, error) {
	if vol == nil || lease == nil || vol.Cols <= 0 || vol.Rows <= 0 || vol.Depth <= 0 {
		if lease != nil {
			_ = lease.Release()
		}
		return nil, fmt.Errorf("render: invalid volume sampler lease")
	}
	// Resolve the immutable backing record once while the lease is active.
	// Sampling through VolumeSnapshot.ModalityAt would intentionally revalidate
	// the public lease on every call and therefore take the store mutex in the
	// hottest MPR/VR loop.
	record, err := lease.record()
	if err != nil {
		_ = lease.Release()
		return nil, err
	}
	photometric := normalizedPhotometric(vol.Photometric())
	sampler := &volumeSampler{
		vol:         vol,
		lease:       lease,
		descriptor:  record.descriptor,
		payload:     record.payload,
		cols:        vol.Cols,
		rows:        vol.Rows,
		depth:       vol.Depth,
		photometric: photometric,
		invert:      photometric == "MONOCHROME1",
	}
	packedRow := uint64(vol.Cols) * 4
	if payload, ok := record.payload.(*float32VolumePayload); ok &&
		record.descriptor.RowStrideBytes == packedRow &&
		record.descriptor.SliceStrideBytes == uint64(vol.Rows)*packedRow {
		sampler.values = payload.data
	}
	if payload, ok := record.payload.(*packed16VolumePayload); ok &&
		record.descriptor.RowStrideBytes == uint64(vol.Cols)*2 &&
		record.descriptor.SliceStrideBytes == uint64(vol.Rows)*uint64(vol.Cols)*2 {
		sampler.signed16, sampler.unsigned16 = payload.signed, payload.unsigned
		sampler.slope, sampler.intercept = record.descriptor.RescaleSlope, record.descriptor.RescaleIntercept
		maxStored := float64(65535)
		if payload.signed != nil {
			maxStored = 32768
		}
		// The old and reassociated paths each perform three convex lerps and
		// an affine rescale. 64 machine epsilons conservatively cover their
		// combined rounding error, including rounded (1-f) weights. Overflow
		// or a large rescale stays on the original per-corner path.
		scale := maxStored*math.Abs(sampler.slope) + math.Abs(sampler.intercept)
		sampler.packed16Affine = scale <= packed16AffineErrorBudget/(64*0x1p-52)
	}
	return sampler, nil
}

// packed16At is the modality value of packed 16-bit sample offset.
func (s *volumeSampler) packed16At(offset int) float64 {
	if s.signed16 != nil {
		return float64(s.signed16[offset])*s.slope + s.intercept
	}
	return float64(s.unsigned16[offset])*s.slope + s.intercept
}

// packed16 reports whether packed 16-bit samples back the sampler.
func (s *volumeSampler) packed16() bool { return s.signed16 != nil || s.unsigned16 != nil }

func newDecodedVolumeSampler(vol *Volume) (*volumeSampler, bool) {
	if vol == nil || vol.Cols <= 0 || vol.Rows <= 0 || vol.Depth <= 0 {
		return nil, false
	}
	values, err := decodeVolumeFloat32(vol)
	if err != nil {
		return nil, false
	}
	photometric := normalizedPhotometric(vol.Photometric())
	return &volumeSampler{
		vol:         vol,
		values:      values,
		cols:        vol.Cols,
		rows:        vol.Rows,
		depth:       vol.Depth,
		photometric: photometric,
		invert:      photometric == "MONOCHROME1",
	}, true
}

func (s *volumeSampler) Close() {
	if s == nil || s.lease == nil {
		return
	}
	_ = s.lease.Release()
	s.lease = nil
	s.descriptor = VolumeDescriptor{}
	s.payload = nil
	s.values = nil
	s.signed16, s.unsigned16 = nil, nil
	s.packed16Affine = false
}

func (s *volumeSampler) valueAt(col, row, slice int) (float64, bool) {
	if s == nil || s.vol == nil || slice < 0 || slice >= s.depth || col < 0 || row < 0 || col >= s.cols || row >= s.rows {
		return 0, false
	}
	if len(s.values) > 0 {
		offset := (slice*s.rows+row)*s.cols + col
		if offset >= 0 && offset < len(s.values) {
			return float64(s.values[offset]), true
		}
	}
	if s.packed16() {
		return s.packed16At((slice*s.rows+row)*s.cols + col), true
	}
	if s.payload == nil {
		return 0, false
	}
	scalarSize, ok := s.descriptor.ScalarFormat.scalarSize()
	if !ok {
		return 0, false
	}
	offset := uint64(slice)*s.descriptor.SliceStrideBytes +
		uint64(row)*s.descriptor.RowStrideBytes +
		uint64(col)*scalarSize
	return s.payload.modalityAt(offset, s.descriptor)
}

func (s *volumeSampler) trilinearAt(p Vec3) (float64, bool) {
	// A point on the voxel grid (axis-aligned planes through voxel centres)
	// is the voxel itself: skip the eight-corner gather (#1007).
	if s != nil && p.X >= 0 && p.Y >= 0 && p.Z >= 0 &&
		p.X == math.Trunc(p.X) && p.Y == math.Trunc(p.Y) && p.Z == math.Trunc(p.Z) &&
		p.X < float64(s.cols) && p.Y < float64(s.rows) && p.Z < float64(s.depth) {
		return s.valueAt(int(p.X), int(p.Y), int(p.Z))
	}
	if s != nil && s.packed16Affine {
		return s.packed16TrilinearAt(p)
	}
	sample, ok := s.voxelSample(p)
	return sample.value(), ok
}

// packed16AffineErrorBudget is an absolute modality-value error bound relative
// to the former per-corner path, not a displayed-pixel or clinical tolerance.
const packed16AffineErrorBudget = 1e-8

// packed16TrilinearAt interpolates normalized stored samples and applies the
// common rescale once. voxelSample remains in modality units: derivative/DVR
// callers need its original corner values, including large rescale fallbacks.
func (s *volumeSampler) packed16TrilinearAt(p Vec3) (float64, bool) {
	var ok bool
	if p.X, ok = clampToGrid(p.X, s.cols); !ok {
		return 0, false
	}
	if p.Y, ok = clampToGrid(p.Y, s.rows); !ok {
		return 0, false
	}
	if p.Z, ok = clampToGrid(p.Z, s.depth); !ok {
		return 0, false
	}
	x0, y0, z0 := int(math.Floor(p.X)), int(math.Floor(p.Y)), int(math.Floor(p.Z))
	x1, y1, z1 := min(x0+1, s.cols-1), min(y0+1, s.rows-1), min(z0+1, s.depth-1)
	plane := s.rows * s.cols
	r00, r10 := z0*plane+y0*s.cols, z0*plane+y1*s.cols
	r01, r11 := z1*plane+y0*s.cols, z1*plane+y1*s.cols
	sample := volumeTextureSample{
		fx: p.X - float64(x0), fy: p.Y - float64(y0), fz: p.Z - float64(z0),
	}
	if s.signed16 != nil {
		v := s.signed16
		sample.c000, sample.c100 = float64(v[r00+x0]), float64(v[r00+x1])
		sample.c010, sample.c110 = float64(v[r10+x0]), float64(v[r10+x1])
		sample.c001, sample.c101 = float64(v[r01+x0]), float64(v[r01+x1])
		sample.c011, sample.c111 = float64(v[r11+x0]), float64(v[r11+x1])
	} else {
		v := s.unsigned16
		sample.c000, sample.c100 = float64(v[r00+x0]), float64(v[r00+x1])
		sample.c010, sample.c110 = float64(v[r10+x0]), float64(v[r10+x1])
		sample.c001, sample.c101 = float64(v[r01+x0]), float64(v[r01+x1])
		sample.c011, sample.c111 = float64(v[r11+x0]), float64(v[r11+x1])
	}
	return sample.value()*s.slope + s.intercept, true
}

func (s *volumeSampler) textureAt(tex Vec3) (float64, bool) {
	return s.trilinearAt(Vec3{
		X: tex.X * float64(s.cols-1),
		Y: tex.Y * float64(s.rows-1),
		Z: tex.Z * float64(s.depth-1),
	})
}

// volumeTextureSample retains the eight voxel values and fractional position
// of one trilinear sample. DVR can use the same values for both the HU lookup
// and the trilinear field derivative instead of issuing six more trilinear
// samples for an opaque point.
type volumeTextureSample struct {
	x0, y0, z0             int
	fx, fy, fz             float64
	c000, c100, c010, c110 float64
	c001, c101, c011, c111 float64
}

func (s *volumeSampler) textureSample(tex Vec3) (volumeTextureSample, bool) {
	if s == nil {
		return volumeTextureSample{}, false
	}
	p := Vec3{
		X: tex.X * float64(s.cols-1),
		Y: tex.Y * float64(s.rows-1),
		Z: tex.Z * float64(s.depth-1),
	}
	return s.voxelSample(p)
}

// voxelBorderTolerance widens the half-voxel border so a position that
// rounding puts a hair past it (a plane moved 0.499999 slice beyond the last
// one) still samples the edge voxels (VTK bb7d5f73).
const voxelBorderTolerance = 1e-3

// voxelSample is the trilinear sample at fractional voxel coordinates p under
// the volume's border policy (#1005): a position within half a voxel of the
// grid is clamped to it, so edge samples repeat the edge voxels instead of
// blending with a value the volume does not hold (0 is water in CT); a
// position farther out is outside (ok false) and callers paint their
// background.
func (s *volumeSampler) voxelSample(p Vec3) (volumeTextureSample, bool) {
	if s == nil {
		return volumeTextureSample{}, false
	}
	var ok bool
	if p.X, ok = clampToGrid(p.X, s.cols); !ok {
		return volumeTextureSample{}, false
	}
	if p.Y, ok = clampToGrid(p.Y, s.rows); !ok {
		return volumeTextureSample{}, false
	}
	if p.Z, ok = clampToGrid(p.Z, s.depth); !ok {
		return volumeTextureSample{}, false
	}
	x0 := int(math.Floor(p.X))
	y0 := int(math.Floor(p.Y))
	z0 := int(math.Floor(p.Z))
	x1, y1, z1 := min(x0+1, s.cols-1), min(y0+1, s.rows-1), min(z0+1, s.depth-1)
	if len(s.values) > 0 {
		// Packed float32 voxels: the eight corners by offset (#1007).
		plane := s.rows * s.cols
		r00, r10 := z0*plane+y0*s.cols, z0*plane+y1*s.cols
		r01, r11 := z1*plane+y0*s.cols, z1*plane+y1*s.cols
		v := s.values
		return volumeTextureSample{
			x0: x0, y0: y0, z0: z0,
			fx: p.X - float64(x0), fy: p.Y - float64(y0), fz: p.Z - float64(z0),
			c000: float64(v[r00+x0]), c100: float64(v[r00+x1]),
			c010: float64(v[r10+x0]), c110: float64(v[r10+x1]),
			c001: float64(v[r01+x0]), c101: float64(v[r01+x1]),
			c011: float64(v[r11+x0]), c111: float64(v[r11+x1]),
		}, true
	}
	if s.packed16() {
		// Packed 16-bit stored samples (#1065), rescaled per corner.
		plane := s.rows * s.cols
		r00, r10 := z0*plane+y0*s.cols, z0*plane+y1*s.cols
		r01, r11 := z1*plane+y0*s.cols, z1*plane+y1*s.cols
		return volumeTextureSample{
			x0: x0, y0: y0, z0: z0,
			fx: p.X - float64(x0), fy: p.Y - float64(y0), fz: p.Z - float64(z0),
			c000: s.packed16At(r00 + x0), c100: s.packed16At(r00 + x1),
			c010: s.packed16At(r10 + x0), c110: s.packed16At(r10 + x1),
			c001: s.packed16At(r01 + x0), c101: s.packed16At(r01 + x1),
			c011: s.packed16At(r11 + x0), c111: s.packed16At(r11 + x1),
		}, true
	}
	return volumeTextureSample{
		x0: x0, y0: y0, z0: z0,
		fx: p.X - float64(x0), fy: p.Y - float64(y0), fz: p.Z - float64(z0),
		c000: s.valueOrZero(x0, y0, z0), c100: s.valueOrZero(x1, y0, z0),
		c010: s.valueOrZero(x0, y1, z0), c110: s.valueOrZero(x1, y1, z0),
		c001: s.valueOrZero(x0, y0, z1), c101: s.valueOrZero(x1, y0, z1),
		c011: s.valueOrZero(x0, y1, z1), c111: s.valueOrZero(x1, y1, z1),
	}, true
}

// clampToGrid clamps a voxel coordinate on an axis of n voxels to [0,n-1]
// when it lies inside the axis (-0.5 <= v < n-0.5, #1115) widened by
// voxelBorderTolerance.
func clampToGrid(v float64, n int) (float64, bool) {
	return ClampContinuousIndex(v, n, voxelBorderTolerance)
}

// clampIndex clamps a voxel coordinate to the grid [0,n-1] on every axis: the
// gradient stencils repeat the edge voxels (VTK's clamp-to-edge volume
// texture) rather than reading past them.
func (s *volumeSampler) clampIndex(p Vec3) Vec3 {
	return Vec3{
		X: math.Max(0, math.Min(float64(s.cols-1), p.X)),
		Y: math.Max(0, math.Min(float64(s.rows-1), p.Y)),
		Z: math.Max(0, math.Min(float64(s.depth-1), p.Z)),
	}
}

// valueClamped is the voxel at the indices clamped to the grid.
func (s *volumeSampler) valueClamped(col, row, slice int) float64 {
	return s.valueOrZero(max(0, min(col, s.cols-1)), max(0, min(row, s.rows-1)), max(0, min(slice, s.depth-1)))
}

func (sample volumeTextureSample) value() float64 {
	c00 := sample.c000*(1-sample.fx) + sample.c100*sample.fx
	c10 := sample.c010*(1-sample.fx) + sample.c110*sample.fx
	c01 := sample.c001*(1-sample.fx) + sample.c101*sample.fx
	c11 := sample.c011*(1-sample.fx) + sample.c111*sample.fx
	c0 := c00*(1-sample.fy) + c10*sample.fy
	c1 := c01*(1-sample.fy) + c11*sample.fy
	return c0*(1-sample.fz) + c1*sample.fz
}

// volumeGradientCell retains the central-difference gradient at the eight
// corners of one voxel cell. Full-quality DVR takes several sub-voxel samples
// before crossing into the next cell, so the stencil is normally prepared once
// and then only trilinearly interpolated for adjacent samples.
type volumeGradientCell struct {
	sampler    *volumeSampler
	x0, y0, z0 int
	gradients  [2][2][2]Vec3
}

func (cell *volumeGradientCell) gradient(s *volumeSampler, sample volumeTextureSample) Vec3 {
	return s.gradientOpacityVector(cell.gradientIndex(s, sample))
}

func (cell *volumeGradientCell) gradientIndex(s *volumeSampler, sample volumeTextureSample) Vec3 {
	if cell == nil || s == nil {
		return Vec3{}
	}
	if cell.sampler != s || cell.x0 != sample.x0 || cell.y0 != sample.y0 || cell.z0 != sample.z0 {
		cell.prepare(s, sample)
	}
	g := cell.gradients
	g00 := lerpVec3(g[0][0][0], g[0][0][1], sample.fx)
	g10 := lerpVec3(g[0][1][0], g[0][1][1], sample.fx)
	g01 := lerpVec3(g[1][0][0], g[1][0][1], sample.fx)
	g11 := lerpVec3(g[1][1][0], g[1][1][1], sample.fx)
	return lerpVec3(lerpVec3(g00, g10, sample.fy), lerpVec3(g01, g11, sample.fy), sample.fz)
}

func (cell *volumeGradientCell) prepare(s *volumeSampler, sample volumeTextureSample) {
	cell.sampler = s
	cell.x0, cell.y0, cell.z0 = sample.x0, sample.y0, sample.z0
	c := [2][2][2]float64{
		{{sample.c000, sample.c100}, {sample.c010, sample.c110}},
		{{sample.c001, sample.c101}, {sample.c011, sample.c111}},
	}
	for z := 0; z < 2; z++ {
		for y := 0; y < 2; y++ {
			left := s.valueClamped(sample.x0-1, sample.y0+y, sample.z0+z)
			right := s.valueClamped(sample.x0+2, sample.y0+y, sample.z0+z)
			cell.gradients[z][y][0].X = c[z][y][1] - left
			cell.gradients[z][y][1].X = right - c[z][y][0]
		}
	}
	for z := 0; z < 2; z++ {
		for x := 0; x < 2; x++ {
			below := s.valueClamped(sample.x0+x, sample.y0-1, sample.z0+z)
			above := s.valueClamped(sample.x0+x, sample.y0+2, sample.z0+z)
			cell.gradients[z][0][x].Y = c[z][1][x] - below
			cell.gradients[z][1][x].Y = above - c[z][0][x]
		}
	}
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			behind := s.valueClamped(sample.x0+x, sample.y0+y, sample.z0-1)
			inFront := s.valueClamped(sample.x0+x, sample.y0+y, sample.z0+2)
			cell.gradients[0][y][x].Z = c[1][y][x] - behind
			cell.gradients[1][y][x].Z = inFront - c[0][y][x]
		}
	}
}

func lerpVec3(a, b Vec3, fraction float64) Vec3 {
	return Vec3{
		X: lerp(a.X, b.X, fraction),
		Y: lerp(a.Y, b.Y, fraction),
		Z: lerp(a.Z, b.Z, fraction),
	}
}

func (s *volumeSampler) valueOrZero(col, row, slice int) float64 {
	value, _ := s.valueAt(col, row, slice)
	return value
}

func (s *volumeSampler) gradientAt(tex Vec3) Vec3 {
	dx := 1.0 / float64(maxInt(s.cols, 2))
	dy := 1.0 / float64(maxInt(s.rows, 2))
	dz := 1.0 / float64(maxInt(s.depth, 2))
	clamped := func(t Vec3) float64 {
		v, _ := s.textureAt(Vec3{X: math.Max(0, math.Min(1, t.X)), Y: math.Max(0, math.Min(1, t.Y)), Z: math.Max(0, math.Min(1, t.Z))})
		return v
	}
	hx1 := clamped(Vec3{tex.X + dx, tex.Y, tex.Z})
	hx0 := clamped(Vec3{tex.X - dx, tex.Y, tex.Z})
	hy1 := clamped(Vec3{tex.X, tex.Y + dy, tex.Z})
	hy0 := clamped(Vec3{tex.X, tex.Y - dy, tex.Z})
	hz1 := clamped(Vec3{tex.X, tex.Y, tex.Z + dz})
	hz0 := clamped(Vec3{tex.X, tex.Y, tex.Z - dz})
	return Vec3{X: hx1 - hx0, Y: hy1 - hy0, Z: hz1 - hz0}
}

// gradientIndexAt matches the WebGPU central-difference stencil: samples are
// one complete voxel apart in index space. The result is not yet divided by
// two so both backends apply the same 0.5 during patient-space conversion.
func (s *volumeSampler) gradientIndexAt(tex Vec3) Vec3 {
	index := Vec3{
		X: tex.X * float64(s.cols-1),
		Y: tex.Y * float64(s.rows-1),
		Z: tex.Z * float64(s.depth-1),
	}
	hx1, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{X: 1})))
	hx0, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{X: -1})))
	hy1, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{Y: 1})))
	hy0, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{Y: -1})))
	hz1, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{Z: 1})))
	hz0, _ := s.trilinearAt(s.clampIndex(index.Add(Vec3{Z: -1})))
	return Vec3{X: hx1 - hx0, Y: hy1 - hy0, Z: hz1 - hz0}
}

// clippedGradientIndex is gradientIndexAt with the neighbours that a cut
// plane removes read as clippedValue (VTK's ClippedVoxelIntensity, #1067), so
// a cut face has the gradient of a surface. ok is false when no neighbour is
// cut: the caller keeps its usual gradient.
func (s *volumeSampler) clippedGradientIndex(tex Vec3, clip *VRClip, clippedValue float64) (gradient Vec3, ok bool) {
	index := Vec3{
		X: tex.X * float64(s.cols-1),
		Y: tex.Y * float64(s.rows-1),
		Z: tex.Z * float64(s.depth-1),
	}
	texOf := func(q Vec3) Vec3 {
		axis := func(v float64, n int) float64 {
			if n <= 1 {
				return 0
			}
			return v / float64(n-1)
		}
		return Vec3{X: axis(q.X, s.cols), Y: axis(q.Y, s.rows), Z: axis(q.Z, s.depth)}
	}
	var neighbours [6]Vec3
	for i, offset := range [6]Vec3{{X: 1}, {X: -1}, {Y: 1}, {Y: -1}, {Z: 1}, {Z: -1}} {
		neighbours[i] = s.clampIndex(index.Add(offset))
		if clip.cutAway(texOf(neighbours[i])) {
			ok = true
		}
	}
	if !ok {
		return Vec3{}, false
	}
	var values [6]float64
	for i, q := range neighbours {
		if clip.cutAway(texOf(q)) {
			values[i] = clippedValue
			continue
		}
		values[i], _ = s.trilinearAt(q)
	}
	return Vec3{X: values[0] - values[1], Y: values[2] - values[3], Z: values[4] - values[5]}, true
}

func (s *volumeSampler) gradientOpacityVector(indexGradient Vec3) Vec3 {
	return Vec3{
		X: indexGradient.X * float64(s.cols-1) / float64(maxInt(s.cols, 2)),
		Y: indexGradient.Y * float64(s.rows-1) / float64(maxInt(s.rows, 2)),
		Z: indexGradient.Z * float64(s.depth-1) / float64(maxInt(s.depth, 2)),
	}
}

func (s *volumeSampler) displayGrayMapped(value float64, mapper preparedVOI) uint8 {
	gray := windowedGrayMapped(value, mapper)
	if s != nil && s.invert {
		return 255 - gray
	}
	return gray
}
