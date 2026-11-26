package render

// Crop region bits use X-fast, Y-next, Z-slowest indexing over the three
// intervals below, inside and above each pair of normalized crop planes.
const (
	VRCropAllRegions    uint32 = (1 << 27) - 1
	VRCropSubvolume     uint32 = 1 << 13
	VRCropFence         uint32 = 0x2ebfeba
	VRCropCross         uint32 = 0x0417410
	VRCropInvertedFence uint32 = VRCropAllRegions ^ VRCropFence
	VRCropInvertedCross uint32 = VRCropAllRegions ^ VRCropCross
)

// CropRegionKeeps tests the selected region in normalized texture space.
// Zero flags retain the legacy central-box profile; plane boundaries belong
// to the inside interval. Invalid bits reject all samples.
func CropRegionKeeps(flags uint32, point, min, max Vec3) bool {
	if flags & ^VRCropAllRegions != 0 {
		return false
	}
	if flags == 0 {
		flags = VRCropSubvolume
	}
	axis := func(value, low, high float64) uint32 {
		if value < low {
			return 0
		}
		if value > high {
			return 2
		}
		return 1
	}
	region := axis(point.X, min.X, max.X) + 3*axis(point.Y, min.Y, max.Y) + 9*axis(point.Z, min.Z, max.Z)
	return flags&(1<<region) != 0
}

// SetCropRegionFlags retains the current planes and selects their regions.
func (c *VRClip) SetCropRegionFlags(flags uint32) bool {
	if c == nil || flags & ^VRCropAllRegions != 0 {
		return false
	}
	c.cropRegionFlags = flags
	return true
}

func (c *VRClip) CropRegionFlags() uint32 {
	if c == nil || c.cropRegionFlags == 0 {
		return VRCropSubvolume
	}
	return c.cropRegionFlags
}
