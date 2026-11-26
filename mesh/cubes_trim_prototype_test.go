package mesh

import (
	"context"
	"fmt"
	"image"
)

// This opt-in test-only candidate leaves Extract unchanged until promotion
// gates pass. Prefix/budget/edge ownership are the production contracts;
// only row classification and emission differ.
type trimPrototypeWorker struct {
	*cubesWorker
	cases  []uint8
	lo, hi []int32
}

func trimNewWorker(e *cubesExtractor, ids bool) *trimPrototypeWorker {
	w := &trimPrototypeWorker{cubesWorker: e.newWorker(ids)}
	if ids {
		w.cases = make([]uint8, e.plane)
		w.lo = make([]int32, e.nY)
		w.hi = make([]int32, e.nY)
	}
	return w
}

// A column carries corners 0,2,4,6; the next column carries 1,3,5,7.
// Reuse of the preceding column is independent of which axis is crossed.
func trimColumnCase(in0, in1 []uint8, i, nX int) uint8 {
	return in0[i] | in0[i+nX]<<2 | in1[i]<<4 | in1[i+nX]<<6
}

func (w *trimPrototypeWorker) classify(in0, in1 []uint8) {
	e := w.e
	for y := 0; y+1 < e.nY; y++ {
		i := y * e.nX
		left := trimColumnCase(in0, in1, i, e.nX)
		lo, hi := e.nX-1, 0
		for x := 0; x+1 < e.nX; x, i = x+1, i+1 {
			right := trimColumnCase(in0, in1, i+1, e.nX)
			c := left | right<<1
			w.cases[i] = c
			if c != 0 && c != 255 {
				if lo == e.nX-1 {
					lo = x
				}
				hi = x + 1
			}
			left = right
		}
		w.lo[y], w.hi[y] = int32(lo), int32(hi)
	}
}

func trimPrototypeRun(ctx context.Context, field Field, opts Options) (*Mesh, error) {
	frame, err := newAffineFrame(field.IndexToPatient())
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	e := newCubesExtractor(field, frame, opts)
	workers := opts.Workers
	if workers <= 0 {
		workers = 2
	}
	layers := e.nZ - 1
	if e.nX < 2 || e.nY < 2 || layers < 1 {
		return &Mesh{}, nil
	}
	e.xyCount = make([]int64, e.nZ)
	e.zCount = make([]int64, layers)
	e.triCount = make([]int64, layers)
	if err := forEachSlab(ctx, layers, workers, func() slabFunc { return trimNewWorker(e, false).count }); err != nil {
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
	if err := forEachSlab(ctx, layers, workers, func() slabFunc { return trimNewWorker(e, true).generate }); err != nil {
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

func (w *trimPrototypeWorker) count(ctx context.Context, z0, z1 int) error {
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
			e.xyCount[z] = e.countXY(in, image.Rect(0, 0, e.nX, e.nY))
		}
		if z > z0 {
			var edges, tris int64
			for i := range in {
				if in[i] != prev[i] {
					edges++
				}
			}
			for y := 0; y+1 < e.nY; y++ {
				i := y * e.nX
				left := trimColumnCase(prev, in, i, e.nX)
				for x := 0; x+1 < e.nX; x, i = x+1, i+1 {
					right := trimColumnCase(prev, in, i+1, e.nX)
					tris += int64(mcTriangleCount[left|right<<1])
					left = right
				}
			}
			e.zCount[z-1], e.triCount[z-1] = edges, tris
		}
		prev = in
	}
	return nil
}

func (w *trimPrototypeWorker) generate(ctx context.Context, z0, z1 int) error {
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

func (w *trimPrototypeWorker) triangles(ctx context.Context, z int) error {
	e := w.e
	_, in0, err := w.planeAt(ctx, z)
	if err != nil {
		return err
	}
	_, in1, err := w.planeAt(ctx, z+1)
	if err != nil {
		return err
	}
	ids := [3][]uint32{}
	k := e.triOff[z]
	out := e.mesh.Triangles
	w.classify(in0, in1)
	for y := 0; y+1 < e.nY; y++ {
		for x, i := int(w.lo[y]), y*e.nX+int(w.lo[y]); x < int(w.hi[y]); x, i = x+1, i+1 {
			c := int(w.cases[i])
			if c == 0 || c == 255 {
				continue
			}
			tris := mcTable.triangles(c)
			for j := 0; j < len(tris); j += 3 {
				for v := 0; v < 3; v++ {
					edge := mcTable.edges[tris[j+v]]
					top := int(edge.base >> 2 & 1)
					switch edge.axis {
					case 0:
						ids[v] = w.xID[(z+top)&1]
					case 1:
						ids[v] = w.yID[(z+top)&1]
					default:
						ids[v] = w.zID
					}
				}
				o0, o1, o2 := w.edgeOffset[tris[j]], w.edgeOffset[tris[j+1]], w.edgeOffset[tris[j+2]]
				a, b, cc := ids[0][i+o0], ids[1][i+o1], ids[2][i+o2]
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
