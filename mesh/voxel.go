package mesh

import (
	"context"
	"fmt"
)

// voxelExtractor builds the voxel-exact surface: a quad (two triangles) on
// every face between an inside voxel and an outside voxel or the grid
// boundary. Vertices are the voxel corners, lattice point (i, j, k) at index
// (i-0.5, j-0.5, k-0.5), shared by all the faces that meet there, so voxels
// touching only along an edge or at a corner share that edge or corner
// (the mesh is then closed and consistently oriented, but not a manifold
// there).
type voxelExtractor struct {
	field      Field
	frame      affineFrame
	opts       Options
	iso        float64
	nx, ny, nz int
	cX, cY     int
	flip       bool

	// usedCount per corner plane k in [0, nz]; faceCount per voxel layer.
	usedCount, faceCount []int64
	usedOff, faceOff     []int64

	mesh *Mesh
}

func newVoxelExtractor(field Field, frame affineFrame, opts Options) *voxelExtractor {
	d := field.Dims()
	return &voxelExtractor{
		field: field, frame: frame, opts: opts, iso: opts.Iso,
		nx: d[0], ny: d[1], nz: d[2], cX: d[0] + 1, cY: d[1] + 1,
		flip: frame.det < 0,
	}
}

func (e *voxelExtractor) run(ctx context.Context, workers int) (*Mesh, error) {
	e.usedCount = make([]int64, e.nz+1)
	e.faceCount = make([]int64, e.nz)
	if err := forEachSlab(ctx, e.nz, workers, func() slabFunc { return e.newWorker(false).count }); err != nil {
		return nil, err
	}
	e.usedOff = append([]int64(nil), e.usedCount...)
	vertices := prefix(e.usedOff)
	e.faceOff = append([]int64(nil), e.faceCount...)
	faces := prefix(e.faceOff)
	normals := !e.opts.SkipNormals
	if err := budget(e.opts, vertices, 2*faces, normals); err != nil {
		return nil, err
	}
	e.mesh = &Mesh{Vertices: make([][3]float32, vertices), Triangles: make([][3]uint32, 2*faces)}
	if err := forEachSlab(ctx, e.nz, workers, func() slabFunc { return e.newWorker(true).generate }); err != nil {
		return nil, err
	}
	if normals {
		e.mesh.Normals = make([][3]float32, vertices)
		faceNormals(e.mesh, nil)
	}
	return e.mesh, nil
}

// voxelWorker caches the inside flags of voxel layers in slots z&3; layers
// outside the grid are all outside.
type voxelWorker struct {
	e       *voxelExtractor
	z       [4]int
	in      [4][]uint8
	vals    []float32
	outside []uint8
	id      [2][]uint32
}

func (e *voxelExtractor) newWorker(ids bool) *voxelWorker {
	w := &voxelWorker{e: e, z: [4]int{-1, -1, -1, -1}}
	if ids {
		for i := range w.id {
			w.id[i] = make([]uint32, e.cX*e.cY)
		}
	}
	plane := e.nx * e.ny
	for i := range w.in {
		w.in[i] = make([]uint8, plane)
	}
	w.vals = make([]float32, plane)
	w.outside = make([]uint8, plane)
	return w
}

func (w *voxelWorker) layer(ctx context.Context, z int) ([]uint8, error) {
	e := w.e
	if z < 0 || z >= e.nz {
		return w.outside, nil
	}
	slot := z & 3
	if w.z[slot] == z {
		return w.in[slot], nil
	}
	w.z[slot] = -1
	if err := e.field.ReadSlice(ctx, z, w.vals); err != nil {
		return nil, fmt.Errorf("mesh: read slice %d: %w", z, err)
	}
	in := w.in[slot]
	for i, v := range w.vals {
		in[i] = 0
		if finite32(v) && float64(v) >= e.iso {
			in[i] = 1
		}
	}
	w.z[slot] = z
	return in, nil
}

// at is the inside flag of voxel (x, y) of a layer, outside the grid 0.
func (e *voxelExtractor) at(in []uint8, x, y int) uint8 {
	if x < 0 || y < 0 || x >= e.nx || y >= e.ny {
		return 0
	}
	return in[y*e.nx+x]
}

// cornerUsed reports whether lattice corner (i, j) between voxel layers
// below and above touches a boundary face: the eight voxels around it are
// neither all inside nor all outside.
func (e *voxelExtractor) cornerUsed(below, above []uint8, i, j int) bool {
	s := e.at(below, i-1, j-1) + e.at(below, i, j-1) + e.at(below, i-1, j) + e.at(below, i, j) +
		e.at(above, i-1, j-1) + e.at(above, i, j-1) + e.at(above, i-1, j) + e.at(above, i, j)
	return s > 0 && s < 8
}

// faces visits the faces of voxel layer z: its x and y faces, the z faces
// on corner plane z and, for the last layer, on plane nz. Each face is
// reported by its four lattice corners, counter-clockwise about the
// outward normal, as (i, j, dk) with dk the corner plane relative to z.
func (e *voxelExtractor) faces(below, cur, above []uint8, z int, visit func(q [4][3]int)) {
	for y := 0; y < e.ny; y++ {
		for x := 0; x <= e.nx; x++ {
			a, b := e.at(cur, x-1, y), e.at(cur, x, y)
			if a == b {
				continue
			}
			q := [4][3]int{{x, y, 0}, {x, y + 1, 0}, {x, y + 1, 1}, {x, y, 1}} // about +x
			if b == 1 {
				q[1], q[3] = q[3], q[1]
			}
			visit(q)
		}
	}
	for y := 0; y <= e.ny; y++ {
		for x := 0; x < e.nx; x++ {
			a, b := e.at(cur, x, y-1), e.at(cur, x, y)
			if a == b {
				continue
			}
			q := [4][3]int{{x, y, 0}, {x, y, 1}, {x + 1, y, 1}, {x + 1, y, 0}} // about +y
			if b == 1 {
				q[1], q[3] = q[3], q[1]
			}
			visit(q)
		}
	}
	zFaces := func(lo, hi []uint8, dk int) {
		for y := 0; y < e.ny; y++ {
			for x := 0; x < e.nx; x++ {
				a, b := lo[y*e.nx+x], hi[y*e.nx+x]
				if a == b {
					continue
				}
				q := [4][3]int{{x, y, dk}, {x + 1, y, dk}, {x + 1, y + 1, dk}, {x, y + 1, dk}} // about +z
				if b == 1 {
					q[1], q[3] = q[3], q[1]
				}
				visit(q)
			}
		}
	}
	zFaces(below, cur, 0)
	if z == e.nz-1 {
		zFaces(cur, above, 1)
	}
}

func (w *voxelWorker) count(ctx context.Context, z0, z1 int) error {
	e := w.e
	last := z1
	if z1 == e.nz {
		last = e.nz + 1
	}
	for k := z0; k < last; k++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		below, err := w.layer(ctx, k-1)
		if err != nil {
			return err
		}
		cur, err := w.layer(ctx, k)
		if err != nil {
			return err
		}
		var used int64
		for j := 0; j < e.cY; j++ {
			for i := 0; i < e.cX; i++ {
				if e.cornerUsed(below, cur, i, j) {
					used++
				}
			}
		}
		e.usedCount[k] = used
		if k < z1 {
			above, err := w.layer(ctx, k+1)
			if err != nil {
				return err
			}
			var faces int64
			e.faces(below, cur, above, k, func([4][3]int) { faces++ })
			e.faceCount[k] = faces
		}
	}
	return nil
}

// number assigns ids to the used corners of plane k in row-major order and
// writes them when emit is set.
func (w *voxelWorker) number(ctx context.Context, k int, emit bool) error {
	e := w.e
	below, err := w.layer(ctx, k-1)
	if err != nil {
		return err
	}
	cur, err := w.layer(ctx, k)
	if err != nil {
		return err
	}
	ids := w.id[k&1]
	id := e.usedOff[k]
	for j := 0; j < e.cY; j++ {
		for i := 0; i < e.cX; i++ {
			if !e.cornerUsed(below, cur, i, j) {
				continue
			}
			ids[j*e.cX+i] = uint32(id)
			if emit {
				e.mesh.Vertices[id] = e.frame.point(float64(i)-0.5, float64(j)-0.5, float64(k)-0.5)
			}
			id++
		}
	}
	if id-e.usedOff[k] != e.usedCount[k] {
		return fmt.Errorf("mesh: corner plane %d has %d vertices, counted %d", k, id-e.usedOff[k], e.usedCount[k])
	}
	return nil
}

func (w *voxelWorker) generate(ctx context.Context, z0, z1 int) error {
	e := w.e
	if err := w.number(ctx, z0, true); err != nil {
		return err
	}
	for z := z0; z < z1; z++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := w.number(ctx, z+1, z+1 < z1 || z+1 == e.nz); err != nil {
			return err
		}
		below, err := w.layer(ctx, z-1)
		if err != nil {
			return err
		}
		cur, err := w.layer(ctx, z)
		if err != nil {
			return err
		}
		above, err := w.layer(ctx, z+1)
		if err != nil {
			return err
		}
		k := 2 * e.faceOff[z]
		out := e.mesh.Triangles
		e.faces(below, cur, above, z, func(q [4][3]int) {
			var v [4]uint32
			for i, c := range q {
				v[i] = w.id[(z+c[2])&1][c[1]*e.cX+c[0]]
			}
			if e.flip {
				v[1], v[3] = v[3], v[1]
			}
			out[k] = [3]uint32{v[0], v[1], v[2]}
			out[k+1] = [3]uint32{v[0], v[2], v[3]}
			k += 2
		})
		if k != 2*(e.faceOff[z]+e.faceCount[z]) {
			return fmt.Errorf("mesh: layer %d has %d faces, counted %d", z, k/2-e.faceOff[z], e.faceCount[z])
		}
	}
	return nil
}
