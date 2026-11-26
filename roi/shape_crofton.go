package roi

import "math"

// Crofton perimeter and surface estimates from intercept counts, without a
// mesh.
//
// By the Cauchy-Crofton formula the boundary measure is the mean over line
// directions of half the number of boundary crossings integrated over the
// lines: S = 4·E_u[½∫#intercepts] in 3D and L = π·E_θ[½∫#intercepts] in
// 2D. On the voxel lattice the directions are the 13 (3D) or 4 (2D)
// neighbour directions, and lines along direction d carry cellVolume/|d|
// of cross-section each. Each direction pair is weighted by the share of
// the unit sphere (circle) in its Voronoi cell for the isotropic lattice;
// these are ITK's ShapeLabelMapFilter constants, and ITK likewise keeps
// them for anisotropic spacing.
//
// Measured accuracy (tests in shape_stats_test.go): with isotropic
// spacing, spheres, ellipsoids and disks of 10+ voxels radius are within
// about 2%, and an axis-aligned cube is under-estimated by 8-9% (the
// 13-direction quadrature's limit for flat faces is 7.3%). Anisotropic
// spacing tilts the lattice directions toward the coarse axis and biases
// the estimate with the object's orientation: a 12 × 20 × 30 mm ellipsoid
// on 0.5 × 0.8 × 1.5 mm voxels reads 17% low with its long axis along the
// coarse axis and 14% high across it, while a sphere on 0.5 × 0.5 × 2.5 mm
// voxels stays within 3%. Recomputing the Voronoi weights for the
// physical directions was tried: it narrowed the ellipsoid error only to
// 12% and made spheres, disks and boxes worse, so ITK's weights are kept.
// Compare surfaces and sphericity across studies on isotropic (resampled)
// grids.

// croftonWeights3D are the weights of the ± direction pairs of each class,
// in counts order (x, y, z, xy, xz, yz, xyz); with the 2, 2, 2 and 4 sign
// variants of the diagonal classes they sum to 1.
var croftonWeights3D = [7]float64{
	0.04577789120476 * 2, 0.04577789120476 * 2, 0.04577789120476 * 2,
	0.03698062787608 * 2, 0.03698062787608 * 2, 0.03698062787608 * 2,
	0.03519563978232 * 2,
}

// croftonClasses3D are the representative steps of the direction classes.
var croftonClasses3D = [7][3]float64{{1, 0, 0}, {0, 1, 0}, {0, 0, 1}, {1, 1, 0}, {1, 0, 1}, {0, 1, 1}, {1, 1, 1}}

// croftonPerimeter estimates the perimeter (2D) or surface area (3D) from
// the number of object/background transitions along the lattice
// directions, counted between each row of runs and its neighbour rows:
// along x every run has two intercepts; along a direction with a y or z
// step, a voxel whose neighbour in that direction (without or with a ±1 x
// step) is background is one intercept.
func (g *shapeGrid) croftonPerimeter(runs []shapeRun, lo, hi [3]int) float64 {
	ny, nz := hi[1]-lo[1]+1, hi[2]-lo[2]+1
	// rowStart[i]..rowStart[i+1] are the runs of bounding-box row i.
	rowStart := make([]int, ny*nz+1)
	for _, r := range runs {
		rowStart[(r.z-lo[2])*ny+(r.y-lo[1])+1]++
	}
	for i := 1; i < len(rowStart); i++ {
		rowStart[i] += rowStart[i-1]
	}
	row := func(y, z int) []shapeRun {
		if y < lo[1] || y > hi[1] || z < lo[2] || z > hi[2] {
			return nil
		}
		i := (z-lo[2])*ny + (y - lo[1])
		return runs[rowStart[i]:rowStart[i+1]]
	}
	// counts[0..6]: x, y, z, xy, xz, yz, xyz, each lumping the sign
	// variants of its direction class.
	var counts [7]float64
	dzs := []int{-1, 0, 1}
	if g.dim == 2 {
		dzs = []int{0}
	}
	for z := lo[2]; z <= hi[2]; z++ {
		for y := lo[1]; y <= hi[1]; y++ {
			ls := row(y, z)
			if len(ls) == 0 {
				continue
			}
			length := 0
			for _, l := range ls {
				length += l.x1 - l.x0 + 1
			}
			counts[0] += float64(2 * len(ls))
			for _, dz := range dzs {
				for dy := -1; dy <= 1; dy++ {
					if dy == 0 && dz == 0 {
						continue
					}
					ns := row(y+dy, z+dz)
					straight := length - runOverlap(ls, ns, 0)
					diagonal := 2*length - runOverlap(ls, ns, 1) - runOverlap(ls, ns, -1)
					switch {
					case dz == 0: // y, xy
						counts[1] += float64(straight)
						counts[3] += float64(diagonal)
					case dy == 0: // z, xz
						counts[2] += float64(straight)
						counts[4] += float64(diagonal)
					default: // yz, xyz
						counts[5] += float64(straight)
						counts[6] += float64(diagonal)
					}
				}
			}
		}
	}
	dx, dy, dz := g.space[0], g.space[1], g.space[2]
	if g.dim == 2 {
		// Four directions of weight 1/4 each: L = π·Σ w·spacing·n/2.
		p := dy*counts[0]/2 + dx*counts[1]/2 + dx*dy/math.Hypot(dx, dy)*counts[3]/2
		return math.Pi / 4 * p
	}
	vol := dx * dy * dz
	area := 0.0
	for i, step := range croftonClasses3D {
		length := math.Sqrt(step[0]*dx*dx + step[1]*dy*dy + step[2]*dz*dz)
		area += croftonWeights3D[i] * vol / length * counts[i] / 2
	}
	return 4 * area
}

// runOverlap counts the voxels of ls that are also in ns shifted by shift
// along x. Both lists are sorted and disjoint.
func runOverlap(ls, ns []shapeRun, shift int) int {
	total := 0
	for i, j := 0, 0; i < len(ls) && j < len(ns); {
		n0, n1 := ns[j].x0+shift, ns[j].x1+shift
		if lo, hi := max(ls[i].x0, n0), min(ls[i].x1, n1); hi >= lo {
			total += hi - lo + 1
		}
		if ls[i].x1 < n1 {
			i++
		} else {
			j++
		}
	}
	return total
}
