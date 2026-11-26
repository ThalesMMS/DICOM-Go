package microscopy

import (
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

func (r *mosaicRun) solve(m *MosaicLayout, anchor int) error {
	n := len(m.Tiles)
	seen := make([]bool, n)
	queue := make([]int, 0, n)
	roots := make([]int, 0, n)
	roots = append(roots, anchor)
	for i := range m.Tiles {
		if i != anchor {
			roots = append(roots, i)
		}
	}
	accepted := 0
	treeEdge := make([]bool, len(m.Links))
	for _, e := range m.Links {
		if e.Accepted {
			accepted++
		}
	}
	for _, root := range roots {
		if seen[root] {
			continue
		}
		seen[root] = true
		m.Tiles[root].Component = m.Components
		queue = queue[:0]
		queue = append(queue, root)
		for at := 0; at < len(queue); at++ {
			a := queue[at]
			for linkIndex, e := range m.Links {
				if err := r.step(1); err != nil {
					return err
				}
				if !e.Accepted {
					continue
				}
				b := e.B
				offset := e.Offset
				if e.B == a {
					b = e.A
					offset = render.Vec3{}.Sub(offset)
				} else if e.A != a {
					continue
				}
				if !seen[b] {
					treeEdge[linkIndex] = true
					seen[b] = true
					m.Tiles[b].Component = m.Components
					m.Tiles[b].Origin = m.Tiles[a].Origin.Add(offset)
					queue = append(queue, b)
				}
			}
		}
		m.Components++
	}
	m.CycleRank = accepted - n + m.Components
	for i := range m.Links {
		e := &m.Links[i]
		if !e.Accepted || treeEdge[i] {
			continue
		}
		e.CycleClosureMeasured = true
		e.CycleClosureMM = mosaicLength(m.Tiles[e.B].Origin.Sub(m.Tiles[e.A].Origin).Sub(e.Offset))
		m.MaxCycleClosureMM = math.Max(m.MaxCycleClosureMM, e.CycleClosureMM)
	}
	if m.Components != 1 {
		m.Diagnostics = append(m.Diagnostics, "accepted links do not connect all tiles")
		return r.ctx.Err()
	}
	// Solve an anchored graph Laplacian. The anchor's unknown correction is
	// zero; solving offsets from that anchor avoids large-coordinate products.
	unknown := n - 1
	index := make([]int, n)
	at := 0
	for i := range index {
		index[i] = -1
		if i != anchor {
			index[i] = at
			at++
		}
	}
	l := make([]float64, unknown*unknown)
	b := make([][3]float64, unknown)
	for _, e := range m.Links {
		if err := r.step(1); err != nil {
			return err
		}
		if !e.Accepted {
			continue
		}
		a, c := index[e.A], index[e.B]
		v := [3]float64{e.Offset.X, e.Offset.Y, e.Offset.Z}
		if a >= 0 {
			l[a*unknown+a]++
			for axis := 0; axis < 3; axis++ {
				b[a][axis] -= v[axis]
			}
		}
		if c >= 0 {
			l[c*unknown+c]++
			for axis := 0; axis < 3; axis++ {
				b[c][axis] += v[axis]
			}
		}
		if a >= 0 && c >= 0 {
			l[a*unknown+c]--
			l[c*unknown+a]--
		}
	}
	for i := 0; i < unknown; i++ {
		for j := 0; j <= i; j++ {
			s := l[i*unknown+j]
			for k := 0; k < j; k++ {
				if err := r.step(1); err != nil {
					return err
				}
				s -= l[i*unknown+k] * l[j*unknown+k]
			}
			if i == j {
				if !finitePositive(s) {
					return fmt.Errorf("%w: singular graph", ErrMosaic)
				}
				l[i*unknown+j] = math.Sqrt(s)
			} else {
				l[i*unknown+j] = s / l[j*unknown+j]
			}
		}
	}
	for i := 0; i < unknown; i++ {
		for j := 0; j < i; j++ {
			if err := r.step(1); err != nil {
				return err
			}
			for axis := 0; axis < 3; axis++ {
				b[i][axis] -= l[i*unknown+j] * b[j][axis]
			}
		}
		for axis := 0; axis < 3; axis++ {
			b[i][axis] /= l[i*unknown+i]
		}
	}
	for i := unknown - 1; i >= 0; i-- {
		for j := i + 1; j < unknown; j++ {
			if err := r.step(1); err != nil {
				return err
			}
			for axis := 0; axis < 3; axis++ {
				b[i][axis] -= l[j*unknown+i] * b[j][axis]
			}
		}
		for axis := 0; axis < 3; axis++ {
			b[i][axis] /= l[i*unknown+i]
		}
	}
	for i := range m.Tiles {
		if i != anchor {
			p := b[index[i]]
			m.Tiles[i].Origin = m.AnchorOrigin.Add(render.Vec3{X: p[0], Y: p[1], Z: p[2]})
		}
		d := mosaicLength(m.Tiles[i].Origin.Sub(m.Tiles[i].SourceOrigin))
		if !mosaicFinite(d) {
			return fmt.Errorf("%w: graph arithmetic", ErrMosaic)
		}
		m.MaxDisplacementMM = math.Max(m.MaxDisplacementMM, d)
	}
	for i := range m.Links {
		e := &m.Links[i]
		if !e.Accepted {
			continue
		}
		e.ResidualMM = mosaicLength(m.Tiles[e.B].Origin.Sub(m.Tiles[e.A].Origin).Sub(e.Offset))
		e.ResidualMeasured = true
		m.MaxResidualMM = math.Max(m.MaxResidualMM, e.ResidualMM)
	}
	if m.MaxResidualMM > r.o.MaxCycleErrorMM || m.MaxCycleClosureMM > r.o.MaxCycleErrorMM {
		m.Diagnostics = append(m.Diagnostics, "global residual or fundamental-cycle closure exceeds tolerance")
	}
	if m.MaxDisplacementMM > r.o.MaxCorrectionMM {
		m.Diagnostics = append(m.Diagnostics, "anchored displacement exceeds correction bound")
	}
	m.Conclusive = len(m.Diagnostics) == 0
	return r.ctx.Err()
}
