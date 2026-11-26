package mesh

import (
	"bytes"
	"context"
	"fmt"
	"image"
	"math"
)

var negInf32 = float32(math.Inf(-1))

// mcTriangleCount is the number of triangles of each case.
var mcTriangleCount = func() (n [256]uint8) {
	for c := range n {
		n[c] = uint8(len(mcTable.triangles(c)) / 3)
	}
	return n
}()

// cubesExtractor runs Marching Cubes on the grid extended by one padding
// layer of outside samples on every side (pad = 1) unless the boundary is
// open. Extended index X corresponds to field index X-pad.
type cubesExtractor struct {
	field      Field
	frame      affineFrame
	opts       Options
	iso        float64
	nx, ny, nz int
	pad        int
	nX, nY, nZ int
	plane      int
	flip       bool

	// Per extended plane Z: vertices on its x and y edges (xy) and on the
	// z edges from Z to Z+1 (z); per layer Z (between planes Z and Z+1):
	// triangles. counts keep the first pass, offsets the prefix sums.
	xyCount, zCount, triCount []int64
	xyOffset, zOffset, triOff []int64

	mesh *Mesh
	need []bool
}

func newCubesExtractor(field Field, frame affineFrame, opts Options) *cubesExtractor {
	d := field.Dims()
	e := &cubesExtractor{field: field, frame: frame, opts: opts, iso: opts.Iso, nx: d[0], ny: d[1], nz: d[2]}
	if !opts.OpenBoundary {
		e.pad = 1
	}
	e.nX, e.nY, e.nZ = e.nx+2*e.pad, e.ny+2*e.pad, e.nz+2*e.pad
	e.plane = e.nX * e.nY
	e.flip = frame.det < 0
	return e
}

func (e *cubesExtractor) run(ctx context.Context, workers int) (*Mesh, error) {
	layers := e.nZ - 1
	if e.nX < 2 || e.nY < 2 || layers < 1 {
		return &Mesh{}, nil
	}
	e.xyCount = make([]int64, e.nZ)
	e.zCount = make([]int64, layers)
	e.triCount = make([]int64, layers)
	if err := forEachSlab(ctx, layers, workers, func() slabFunc { return e.newWorker(false).count }); err != nil {
		return nil, err
	}
	// Vertex order: plane 0 x/y edges, layer 0 z edges, plane 1 x/y edges...
	e.xyOffset = make([]int64, e.nZ)
	e.zOffset = make([]int64, layers)
	var vertices int64
	for z := 0; z < e.nZ; z++ {
		e.xyOffset[z] = vertices
		vertices += e.xyCount[z]
		if z < layers {
			e.zOffset[z] = vertices
			vertices += e.zCount[z]
		}
	}
	e.triOff = append([]int64(nil), e.triCount...)
	triangles := prefix(e.triOff)
	normals := !e.opts.SkipNormals
	if err := budget(e.opts, vertices, triangles, normals); err != nil {
		return nil, err
	}
	e.mesh = &Mesh{Vertices: make([][3]float32, vertices), Triangles: make([][3]uint32, triangles)}
	if normals {
		e.mesh.Normals = make([][3]float32, vertices)
		e.need = make([]bool, vertices)
	}
	if err := forEachSlab(ctx, layers, workers, func() slabFunc { return e.newWorker(true).generate }); err != nil {
		return nil, err
	}
	if normals {
		for _, n := range e.need {
			if n {
				faceNormals(e.mesh, e.need)
				break
			}
		}
	}
	return e.mesh, nil
}

// cubesWorker caches four consecutive extended planes of samples and
// inside flags, in slots Z&3.
type cubesWorker struct {
	e       *cubesExtractor
	z       [4]int
	vals    [4][]float32
	in      [4][]uint8
	inside  [4]image.Rectangle
	scratch []float32
	// Vertex ids on the x and y edges of planes of parity Z&1 and on the z
	// edges of the current layer.
	xID, yID [2][]uint32
	zID      []uint32
	// Per cube edge: the offset of its base sample from the cell's first
	// sample within a plane (the plane itself follows from the base corner).
	edgeOffset [12]int
}

func (e *cubesExtractor) newWorker(ids bool) *cubesWorker {
	w := &cubesWorker{e: e, z: [4]int{-1, -1, -1, -1}}
	if ids {
		for i := range w.xID {
			w.xID[i] = make([]uint32, e.plane)
			w.yID[i] = make([]uint32, e.plane)
		}
		w.zID = make([]uint32, e.plane)
	}
	for i := range w.vals {
		w.vals[i] = make([]float32, e.plane)
		w.in[i] = make([]uint8, e.plane)
	}
	if e.pad == 1 {
		w.scratch = make([]float32, e.nx*e.ny)
	}
	for i, edge := range mcTable.edges {
		dx, dy := int(edge.base&1), int(edge.base>>1&1)
		w.edgeOffset[i] = dy*e.nX + dx
	}
	return w
}

// planeAt returns the samples and inside flags of extended plane Z.
func (w *cubesWorker) planeAt(ctx context.Context, z int) ([]float32, []uint8, error) {
	slot := z & 3
	if w.z[slot] == z {
		return w.vals[slot], w.in[slot], nil
	}
	e := w.e
	w.z[slot] = -1
	vals, in := w.vals[slot], w.in[slot]
	if e.pad == 1 && (z == 0 || z == e.nZ-1) {
		for i := range vals {
			vals[i] = negInf32
		}
	} else if e.pad == 0 {
		if err := e.field.ReadSlice(ctx, z, vals); err != nil {
			return nil, nil, fmt.Errorf("mesh: read slice %d: %w", z, err)
		}
	} else {
		if err := e.field.ReadSlice(ctx, z-1, w.scratch); err != nil {
			return nil, nil, fmt.Errorf("mesh: read slice %d: %w", z-1, err)
		}
		for i := 0; i < e.nX; i++ {
			vals[i] = negInf32
			vals[e.plane-1-i] = negInf32
		}
		for y := 0; y < e.ny; y++ {
			row := vals[(y+1)*e.nX : (y+2)*e.nX]
			row[0], row[e.nX-1] = negInf32, negInf32
			copy(row[1:e.nX-1], w.scratch[y*e.nx:(y+1)*e.nx])
		}
	}
	for i, v := range vals {
		if !finite32(v) {
			vals[i] = negInf32
			in[i] = 0
		} else if float64(v) >= e.iso {
			in[i] = 1
		} else {
			in[i] = 0
		}
	}
	w.inside[slot] = insideBounds(in, e.nX)
	w.z[slot] = z
	return vals, in, nil
}

// insideBounds encloses all inside samples in a plane. A crossed edge or
// active cell must touch at least one such sample.
func insideBounds(in []uint8, width int) image.Rectangle {
	bounds := image.Rectangle{Min: image.Pt(width, len(in)/width)}
	for y, offset := 0, 0; offset < len(in); y, offset = y+1, offset+width {
		row := in[offset : offset+width]
		first := bytes.IndexByte(row, 1)
		if first < 0 {
			continue
		}
		bounds.Min.X = min(bounds.Min.X, first)
		bounds.Min.Y = min(bounds.Min.Y, y)
		bounds.Max.X = max(bounds.Max.X, bytes.LastIndexByte(row, 1)+1)
		bounds.Max.Y = y + 1
	}
	return bounds
}

// precedingBases includes edges and cells that start one sample before the
// inside bounds on either axis. It leaves empty planes empty.
func precedingBases(bounds image.Rectangle) image.Rectangle {
	if bounds.Empty() {
		return image.Rectangle{}
	}
	bounds.Min.X = max(0, bounds.Min.X-1)
	bounds.Min.Y = max(0, bounds.Min.Y-1)
	return bounds
}

func (e *cubesExtractor) cellBounds(inside image.Rectangle) image.Rectangle {
	bounds := precedingBases(inside)
	bounds.Max.X = min(bounds.Max.X, e.nX-1)
	bounds.Max.Y = min(bounds.Max.Y, e.nY-1)
	return bounds
}

func cellCase(in0, in1 []uint8, i, nX int) int {
	return int(in0[i]) | int(in0[i+1])<<1 | int(in0[i+nX])<<2 | int(in0[i+nX+1])<<3 |
		int(in1[i])<<4 | int(in1[i+1])<<5 | int(in1[i+nX])<<6 | int(in1[i+nX+1])<<7
}

// count is the first pass over layers [z0, z1).
func (w *cubesWorker) count(ctx context.Context, z0, z1 int) error {
	e := w.e
	var prev []uint8
	for z := z0; z <= z1; z++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, in, err := w.planeAt(ctx, z)
		if err != nil {
			return err
		}
		if z < z1 || z == e.nZ-1 {
			e.xyCount[z] = e.countXY(in, precedingBases(w.inside[z&3]))
		}
		if z > z0 {
			var edges, tris int64
			inside := w.inside[(z-1)&3].Union(w.inside[z&3])
			for y := inside.Min.Y; y < inside.Max.Y; y++ {
				row := y * e.nX
				for i := row + inside.Min.X; i < row+inside.Max.X; i++ {
					if in[i] != prev[i] {
						edges++
					}
				}
			}
			cells := e.cellBounds(inside)
			for y := cells.Min.Y; y < cells.Max.Y; y++ {
				row := y * e.nX
				for i := row + cells.Min.X; i < row+cells.Max.X; i++ {
					tris += int64(mcTriangleCount[cellCase(prev, in, i, e.nX)])
				}
			}
			e.zCount[z-1], e.triCount[z-1] = edges, tris
		}
		prev = in
	}
	return nil
}

func (e *cubesExtractor) countXY(in []uint8, bounds image.Rectangle) int64 {
	var n int64
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x, i := bounds.Min.X, y*e.nX+bounds.Min.X; x < bounds.Max.X; x, i = x+1, i+1 {
			if x+1 < e.nX && in[i] != in[i+1] {
				n++
			}
			if y+1 < e.nY && in[i] != in[i+e.nX] {
				n++
			}
		}
	}
	return n
}

// generate is the second pass over layers [z0, z1): it numbers the vertices
// of the planes and layers it reads in the order of the first pass, writes
// the vertices it owns (planes z0..z1-1, plane z1 only when it is the last,
// and the z edges of its layers) and the triangles of its layers.
func (w *cubesWorker) generate(ctx context.Context, z0, z1 int) error {
	e := w.e
	if err := w.numberXY(ctx, z0, true); err != nil {
		return err
	}
	for z := z0; z < z1; z++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		top := z + 1
		if err := w.numberXY(ctx, top, top < z1 || top == e.nZ-1); err != nil {
			return err
		}
		if err := w.numberZ(ctx, z); err != nil {
			return err
		}
		if err := w.triangles(ctx, z); err != nil {
			return err
		}
	}
	return nil
}

// numberXY assigns ids to the x and y edges of plane z crossed by the
// surface, in row-major order with the x edge of a sample before its y edge,
// and writes those vertices when emit is set.
func (w *cubesWorker) numberXY(ctx context.Context, z int, emit bool) error {
	e := w.e
	vals, in, err := w.planeAt(ctx, z)
	if err != nil {
		return err
	}
	xID, yID := w.xID[z&1], w.yID[z&1]
	id := e.xyOffset[z]
	bounds := precedingBases(w.inside[z&3])
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x, i := bounds.Min.X, y*e.nX+bounds.Min.X; x < bounds.Max.X; x, i = x+1, i+1 {
			if x+1 < e.nX && in[i] != in[i+1] {
				xID[i] = uint32(id)
				if emit {
					if err := w.vertex(ctx, id, x, y, z, 0, vals[i], vals[i+1]); err != nil {
						return err
					}
				}
				id++
			}
			if y+1 < e.nY && in[i] != in[i+e.nX] {
				yID[i] = uint32(id)
				if emit {
					if err := w.vertex(ctx, id, x, y, z, 1, vals[i], vals[i+e.nX]); err != nil {
						return err
					}
				}
				id++
			}
		}
	}
	if id-e.xyOffset[z] != e.xyCount[z] {
		return fmt.Errorf("mesh: plane %d has %d vertices, counted %d", z, id-e.xyOffset[z], e.xyCount[z])
	}
	return nil
}

// numberZ assigns ids to, and writes, the vertices on the z edges of layer
// z.
func (w *cubesWorker) numberZ(ctx context.Context, z int) error {
	e := w.e
	v0, in0, err := w.planeAt(ctx, z)
	if err != nil {
		return err
	}
	v1, in1, err := w.planeAt(ctx, z+1)
	if err != nil {
		return err
	}
	id := e.zOffset[z]
	bounds := w.inside[z&3].Union(w.inside[(z+1)&3])
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x, i := bounds.Min.X, y*e.nX+bounds.Min.X; x < bounds.Max.X; x, i = x+1, i+1 {
			if in0[i] != in1[i] {
				w.zID[i] = uint32(id)
				if err := w.vertex(ctx, id, x, y, z, 2, v0[i], v1[i]); err != nil {
					return err
				}
				id++
			}
		}
	}
	if id-e.zOffset[z] != e.zCount[z] {
		return fmt.Errorf("mesh: layer %d has %d z-edge vertices, counted %d", z, id-e.zOffset[z], e.zCount[z])
	}
	return nil
}

func (w *cubesWorker) triangles(ctx context.Context, z int) error {
	e := w.e
	_, in0, err := w.planeAt(ctx, z)
	if err != nil {
		return err
	}
	_, in1, err := w.planeAt(ctx, z+1)
	if err != nil {
		return err
	}
	// Bind each case-table edge to its ID plane and base offset once per
	// layer. Each triangle then reads IDs directly at the cell's index.
	var edgeIDs [12][]uint32
	for i, edge := range mcTable.edges {
		top := int(edge.base >> 2 & 1)
		offset := w.edgeOffset[i]
		switch edge.axis {
		case 0:
			edgeIDs[i] = w.xID[(z+top)&1][offset:]
		case 1:
			edgeIDs[i] = w.yID[(z+top)&1][offset:]
		default:
			edgeIDs[i] = w.zID[offset:]
		}
	}
	k := e.triOff[z]
	out := e.mesh.Triangles
	bounds := e.cellBounds(w.inside[z&3].Union(w.inside[(z+1)&3]))
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		row := y * e.nX
		for i := row + bounds.Min.X; i < row+bounds.Max.X; i++ {
			c := cellCase(in0, in1, i, e.nX)
			if c == 0 || c == 255 {
				continue
			}
			tris := mcTable.triangles(c)
			for j := 0; j < len(tris); j += 3 {
				a := edgeIDs[tris[j]][i]
				b := edgeIDs[tris[j+1]][i]
				cc := edgeIDs[tris[j+2]][i]
				if e.flip {
					b, cc = cc, b
				}
				out[k] = [3]uint32{a, b, cc}
				k++
			}
		}
	}
	if k != e.triOff[z]+e.triCount[z] {
		return fmt.Errorf("mesh: layer %d has %d triangles, counted %d", z, k-e.triOff[z], e.triCount[z])
	}
	return nil
}

// vertex writes vertex id on the edge from extended sample (x, y, z) to its
// neighbour along axis, with samples v0 and v1 (one inside, one outside).
func (w *cubesWorker) vertex(ctx context.Context, id int64, x, y, z, axis int, v0, v1 float32) error {
	e := w.e
	boundary := !finite32(v0) || !finite32(v1)
	t := 0.5
	if !boundary {
		t = (e.iso - float64(v0)) / (float64(v1) - float64(v0))
		t = math.Max(0, math.Min(1, t))
	}
	p := [3]float64{float64(x - e.pad), float64(y - e.pad), float64(z - e.pad)}
	p[axis] += t
	e.mesh.Vertices[id] = e.frame.point(p[0], p[1], p[2])
	if e.mesh.Normals == nil {
		return nil
	}
	var n [3]float64
	if boundary {
		// The surface closes half a sample outside the finite sample: its
		// normal is the grid axis, pointing to the outside sample.
		var dir [3]float64
		dir[axis] = 1
		if !finite32(v1) {
			n = e.frame.patientGradient(dir)
		} else {
			dir[axis] = -1
			n = e.frame.patientGradient(dir)
		}
	} else {
		g0, err := w.gradient(ctx, x, y, z)
		if err != nil {
			return err
		}
		q := [3]int{x, y, z}
		q[axis]++
		g1, err := w.gradient(ctx, q[0], q[1], q[2])
		if err != nil {
			return err
		}
		g := [3]float64{g0[0] + t*(g1[0]-g0[0]), g0[1] + t*(g1[1]-g0[1]), g0[2] + t*(g1[2]-g0[2])}
		pg := e.frame.patientGradient(g)
		// The inside has the larger samples: the outward normal is the
		// negative gradient.
		n = [3]float64{-pg[0], -pg[1], -pg[2]}
	}
	e.mesh.Normals[id] = unit32(n)
	if e.mesh.Normals[id] == ([3]float32{}) {
		e.need[id] = true
	}
	return nil
}

// gradient is the central-difference gradient at extended sample (x, y, z)
// in index space, one-sided where a neighbour is missing or outside the
// finite samples.
func (w *cubesWorker) gradient(ctx context.Context, x, y, z int) ([3]float64, error) {
	e := w.e
	vals, _, err := w.planeAt(ctx, z)
	if err != nil {
		return [3]float64{}, err
	}
	i := y*e.nX + x
	c := vals[i]
	var g [3]float64
	sample := func(v []float32, j int) (float32, bool) { return v[j], finite32(v[j]) }
	var m, p float32
	var hm, hp bool
	if x > 0 {
		m, hm = sample(vals, i-1)
	}
	if x+1 < e.nX {
		p, hp = sample(vals, i+1)
	}
	g[0] = difference(c, m, p, hm, hp)
	hm, hp = false, false
	if y > 0 {
		m, hm = sample(vals, i-e.nX)
	}
	if y+1 < e.nY {
		p, hp = sample(vals, i+e.nX)
	}
	g[1] = difference(c, m, p, hm, hp)
	hm, hp = false, false
	if z > 0 {
		below, _, err := w.planeAt(ctx, z-1)
		if err != nil {
			return g, err
		}
		m, hm = sample(below, i)
	}
	if z+1 < e.nZ {
		above, _, err := w.planeAt(ctx, z+1)
		if err != nil {
			return g, err
		}
		p, hp = sample(above, i)
	}
	g[2] = difference(c, m, p, hm, hp)
	return g, nil
}

func difference(c, m, p float32, hm, hp bool) float64 {
	switch {
	case hm && hp:
		return (float64(p) - float64(m)) / 2
	case hp:
		return float64(p) - float64(c)
	case hm:
		return float64(c) - float64(m)
	}
	return 0
}
