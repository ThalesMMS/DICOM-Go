package render

import "math"

// Clipping primitives for the 3D renderer (3d-toolbar-plan.md §5.9): an
// axis-aligned crop box plus up to three half-space clip planes, both in
// texture [0,1]³ space. Each ray is first narrowed to the interval the crop
// box and planes leave (rayInterval), on the same sample grid; samples failing
// the test inside it are skipped in the march. This is also the foundation the
// scalpel writes into (#162), which stays per sample.

// VRClip carries the optional geometric clipping (crop box + planes, #143) and
// the scalpel cut mask (#162). A nil *VRClip keeps every sample.
type VRClip struct {
	hasCrop          bool
	cropMin, cropMax Vec3 // texture-space [0,1]³
	cropRegionFlags  uint32
	planes           []clipPlane
	cut              *VRCutMask
	// labels colours segments inside the volume (#1062).
	labels *VRLabelMap
	// capOpacity draws the faces of cut planes (#1067); zero disables.
	capOpacity float64
	// capWindow maps the cap's section to gray; a zero width uses the VR
	// window.
	capWindow WindowLevel
	// geometry is composited with depth into the frame (#1068).
	geometry *VRGeometry
	// fullRays disables rayInterval and empty-space skipping: tests compare
	// against the full per-sample march.
	fullRays bool
}

type clipPlane struct {
	normal Vec3 // texture space
	d      float64
	// cut marks a plane that cuts the volume open (#1067): the gradient
	// treats its clipped side as empty, and the cap draws its face.
	cut bool
}

// keep reports whether a texture-space sample survives the clipping.
func (c *VRClip) keep(tex Vec3) bool {
	if c == nil {
		return true
	}
	if c.hasCrop {
		if !CropRegionKeeps(c.cropRegionFlags, tex, c.cropMin, c.cropMax) {
			return false
		}
	}
	for _, p := range c.planes {
		if tex.Dot(p.normal)-p.d > 0 {
			return false
		}
	}
	if c.cut != nil && c.cut.cut(tex) {
		return false
	}
	return true
}

// rayIntervalSlack is how far, in texture units, a sample must lie beyond a
// crop face or plane for rayInterval to leave it out. It dwarfs the rounding
// of keep's texture coordinates, so every sample keep would accept stays in
// the interval and the output is the full march's.
const rayIntervalSlack = 1e-9

// rayInterval returns the ray parameters [lo,hi] outside which keep rejects
// every sample of the world-space ray o+t·d, from the crop box and the planes
// (as VTK's ClipRayAgainstVolume and ClipRayAgainstClippingPlanes). The march
// keeps its sample grid and still calls keep inside the interval (the scalpel
// mask is per sample). visible is false when no sample can be kept.
func (c *VRClip) rayInterval(o, d, boxMin, size Vec3) (lo, hi float64, visible bool) {
	lo, hi = math.Inf(-1), math.Inf(1)
	if c == nil || c.fullRays {
		return lo, hi, true
	}
	// Texture coordinates along the ray: a + t·b, as keep computes them.
	a := Vec3{X: (o.X - boxMin.X) / size.X, Y: (o.Y - boxMin.Y) / size.Y, Z: (o.Z - boxMin.Z) / size.Z}
	b := Vec3{X: d.X / size.X, Y: d.Y / size.Y, Z: d.Z / size.Z}
	// limit keeps the samples where value(t) = base + t·slope <= 0.
	limit := func(base, slope float64) bool {
		if slope == 0 {
			return base <= rayIntervalSlack
		}
		t := (rayIntervalSlack - base) / slope
		if slope > 0 {
			hi = math.Min(hi, t)
		} else {
			lo = math.Max(lo, t)
		}
		return true
	}
	if c.hasCrop && c.CropRegionFlags() == VRCropSubvolume {
		for _, axis := range [3][4]float64{
			{a.X, b.X, c.cropMin.X, c.cropMax.X},
			{a.Y, b.Y, c.cropMin.Y, c.cropMax.Y},
			{a.Z, b.Z, c.cropMin.Z, c.cropMax.Z},
		} {
			if axis[1] == 0 {
				// The ray is parallel to the faces: keep sees a (to rounding).
				if axis[0] < axis[2]-rayIntervalSlack || axis[0] > axis[3]+rayIntervalSlack {
					return 0, 0, false
				}
				continue
			}
			limit(axis[2]-axis[0], -axis[1]) // cropMin <= a + t·b
			limit(axis[0]-axis[3], axis[1])  // a + t·b <= cropMax
		}
	}
	for _, p := range c.planes {
		if !limit(a.Dot(p.normal)-p.d, b.Dot(p.normal)) {
			return 0, 0, false
		}
	}
	return lo, hi, lo <= hi
}

// clipRay narrows the march [t0,t1] to the clip's ray interval. start is the
// first sample of the unchanged grid t0, t0+step, ... inside it (reached by the
// same additions as the full march, so sample positions are bit-identical).
func (c *VRClip) clipRay(o, d, boxMin, size Vec3, t0, t1, step float64) (start, end float64, visible bool) {
	lo, hi, visible := c.rayInterval(o, d, boxMin, size)
	if !visible || lo > t1 || hi < t0 {
		return 0, 0, false
	}
	start, end = t0, math.Min(t1, hi)
	for start < lo {
		start += step
	}
	return start, end, start <= end
}

// NewVRClip returns an empty clip that keeps every sample.
func NewVRClip() *VRClip { return &VRClip{} }

// SetCropBox restricts rendering to the texture-space box [min,max].
func (c *VRClip) SetCropBox(min, max Vec3) {
	if c == nil {
		return
	}
	c.cropMin, c.cropMax = min, max
	c.hasCrop = true
}

// ClearCropBox removes the crop box.
func (c *VRClip) ClearCropBox() {
	if c == nil {
		return
	}
	c.hasCrop = false
}

// AddClipPlane adds a half-space clip: samples where dot(tex,normal) - d > 0 are
// removed. Up to three planes are kept.
func (c *VRClip) AddClipPlane(normal Vec3, d float64) {
	if c == nil || len(c.planes) >= 3 {
		return
	}
	c.planes = append(c.planes, clipPlane{normal: normal, d: d})
}

// AddCutPlane adds a clip plane, as AddClipPlane, that cuts the volume open
// (#1067): shading gradients read voxels on its clipped side as the volume's
// lowest value (VTK's UseClippedVoxelIntensity), so the cut face is lit as a
// surface, and SetCapOpacity draws the face as a grayscale section.
func (c *VRClip) AddCutPlane(normal Vec3, d float64) {
	if c == nil || len(c.planes) >= 3 {
		return
	}
	c.planes = append(c.planes, clipPlane{normal: normal, d: d, cut: true})
}

// SetCapOpacity sets the opacity, in [0,1], of the cap drawn where a DVR ray
// enters the volume through a cut plane: the section's modality value mapped
// through the window to gray, as the MPR shows it. Zero disables the cap.
func (c *VRClip) SetCapOpacity(opacity float64) {
	if c == nil {
		return
	}
	if !finite(opacity) {
		opacity = 0
	}
	c.capOpacity = clampUnit(opacity)
}

// SetCapWindow sets the window mapping the cap's section to gray, such as
// the MPR's that drives the cut; a zero width uses the VR window.
func (c *VRClip) SetCapWindow(window WindowLevel) {
	if c == nil {
		return
	}
	c.capWindow = window
}

// capWindowOr returns the cap window, or the VR window when none is set.
func (c *VRClip) capWindowOr(window WindowLevel) WindowLevel {
	if c != nil && c.capWindow.Width > 0 && finite(c.capWindow.Width) && finite(c.capWindow.Center) {
		return c.capWindow
	}
	return window
}

// CapOpacity returns the cut-face cap opacity.
func (c *VRClip) CapOpacity() float64 {
	if c == nil {
		return 0
	}
	return c.capOpacity
}

// hasCutPlanes reports whether any plane cuts the volume open.
func (c *VRClip) hasCutPlanes() bool {
	if c == nil {
		return false
	}
	for _, p := range c.planes {
		if p.cut {
			return true
		}
	}
	return false
}

// cutAway reports whether a cut plane removes the texture-space point.
func (c *VRClip) cutAway(tex Vec3) bool {
	if c == nil {
		return false
	}
	for _, p := range c.planes {
		if p.cut && tex.Dot(p.normal)-p.d > 0 {
			return true
		}
	}
	return false
}

// capsAt reports whether a DVR ray whose first kept sample is at tex, the
// sample before it at previous, entered through a cut plane's face: the
// previous sample lies inside the volume and the crop box but on a cut
// plane's clipped side. The cap then sits at the first kept sample, within
// one step of the plane.
func (c *VRClip) capsAt(previous Vec3) bool {
	if c == nil || c.capOpacity <= 0 {
		return false
	}
	if previous.X < 0 || previous.Y < 0 || previous.Z < 0 || previous.X > 1 || previous.Y > 1 || previous.Z > 1 {
		return false
	}
	if c.hasCrop && !CropRegionKeeps(c.cropRegionFlags, previous, c.cropMin, c.cropMax) {
		return false
	}
	return c.cutAway(previous)
}

// ClearPlanes removes all clip planes.
func (c *VRClip) ClearPlanes() {
	if c == nil {
		return
	}
	c.planes = nil
}

// SetCut installs the scalpel cut mask (#162).
func (c *VRClip) SetCut(m *VRCutMask) {
	if c == nil {
		return
	}
	c.cut = m
}

// CutMask returns the current scalpel cut mask (may be nil).
func (c *VRClip) CutMask() *VRCutMask {
	if c == nil {
		return nil
	}
	return c.cut
}

// Empty reports whether the clip removes nothing.
func (c *VRClip) Empty() bool {
	return c == nil || (!c.hasCrop && len(c.planes) == 0 && c.cut == nil)
}
