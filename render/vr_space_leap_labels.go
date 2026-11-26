package render

const (
	vrBlockVisibleLabel uint8 = 1 << iota
	vrBlockLabelOpacity
)

// Immutable nearest-neighbour label occupancy uses the same 8-voxel grid.
// Boundary voxels belong to both neighbouring blocks because rounding a
// sample just below the boundary can select the next block's first voxel.
func (m *VRLabelMap) prepareBlockOccupancy() {
	nx, ny, nz := (m.cols-1)/vrBlockSize+1, (m.rows-1)/vrBlockSize+1, (m.depth-1)/vrBlockSize+1
	m.blockFlags = make([]uint8, nx*ny*nz)
	for z := 0; z < m.depth; z++ {
		for y := 0; y < m.rows; y++ {
			for x := 0; x < m.cols; x++ {
				style, visible := m.visibleStyle(m.label((z*m.rows+y)*m.cols + x))
				if !visible {
					continue
				}
				flags := vrBlockVisibleLabel
				if style.Blend > 0 && style.Opacity > 0 {
					flags |= vrBlockLabelOpacity
				}
				bx, by, bz := x/vrBlockSize, y/vrBlockSize, z/vrBlockSize
				x0, y0, z0 := bx, by, bz
				if x > 0 && x%vrBlockSize == 0 {
					x0--
				}
				if y > 0 && y%vrBlockSize == 0 {
					y0--
				}
				if z > 0 && z%vrBlockSize == 0 {
					z0--
				}
				for iz := z0; iz <= bz; iz++ {
					for iy := y0; iy <= by; iy++ {
						for ix := x0; ix <= bx; ix++ {
							m.blockFlags[(iz*ny+iy)*nx+ix] |= flags
						}
					}
				}
			}
		}
	}
}
func (l *vrSpaceLeap) limitLabels(m *VRLabelMap) {
	if m == nil || len(m.blockFlags) != len(l.empty) {
		return
	}
	for i, flags := range m.blockFlags {
		if m.onlyInside && flags&vrBlockVisibleLabel == 0 {
			l.empty[i] = true
			continue
		}
		if flags&vrBlockLabelOpacity != 0 {
			l.empty[i] = false
		}
	}
}
