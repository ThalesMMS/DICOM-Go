package render

// VoxelToNormalized maps voxel coordinates to the [0,1]³ texture coordinates
// of the renderers, where 0 and 1 are the first and last voxel centres
// (component i divided by n-1; TextureToVoxel is the inverse). It used to
// divide by n, which put voxel 2 of 4 at 0.5, a texture coordinate that
// samples voxel 1.5 (#1115).
func (v *Volume) VoxelToNormalized(p Vec3) Vec3 {
	return Vec3{
		X: safeDiv(p.X, float64(v.Cols-1)),
		Y: safeDiv(p.Y, float64(v.Rows-1)),
		Z: safeDiv(p.Z, float64(v.Depth-1)),
	}
}

// NormalizedToVoxel is the inverse of VoxelToNormalized.
func (v *Volume) NormalizedToVoxel(n Vec3) Vec3 {
	return Vec3{
		X: n.X * float64(max(v.Cols-1, 0)),
		Y: n.Y * float64(max(v.Rows-1, 0)),
		Z: n.Z * float64(max(v.Depth-1, 0)),
	}
}

// CenterVoxel is the voxel coordinate of the volume centre.
func (v *Volume) CenterVoxel() Vec3 {
	return Vec3{
		X: float64(v.Cols-1) / 2,
		Y: float64(v.Rows-1) / 2,
		Z: float64(v.Depth-1) / 2,
	}
}

// Center is the patient-space coordinate of the volume centre.
func (v *Volume) Center() Vec3 { return v.VoxelToPatient(v.CenterVoxel()) }

func safeDiv(a, b float64) float64 {
	if b <= 0 {
		return 0
	}
	return a / b
}
