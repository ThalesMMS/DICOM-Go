package tractographygeom

import (
	"context"
	"fmt"
	"math"
	"strconv"

	"github.com/ThalesMMS/dicom-go/tractography"
)

func displayOptions(o DisplayOptions) (DisplayOptions, error) {
	if o.Style > Ribbon || o.Color > Scalar || o.PointStride < 0 || !finite(o.RadiusMM) || o.RadiusMM < 0 {
		return o, ErrInvalidOptions
	}
	if o.RadiusMM == 0 {
		o.RadiusMM = .5
	}
	if o.TubeSides == 0 {
		o.TubeSides = 6
	}
	if o.TubeSides < 3 || o.TubeSides > 32 {
		return o, ErrInvalidOptions
	}
	for _, p := range []struct {
		v   *int
		max int
	}{{&o.MaxTracks, 100000}, {&o.MaxPoints, 1000000}, {&o.MaxTriangles, 2000000}, {&o.MaxBytes, 256 << 20}} {
		if *p.v < 0 || *p.v > p.max {
			return o, ErrInvalidOptions
		}
		if *p.v == 0 {
			*p.v = p.max
		}
	}
	if o.PointStride == 0 {
		o.PointStride = 1
	}
	if o.Color == Scalar {
		s := o.Scalar
		if s == nil || s.MeasurementIndex < 0 || s.Quantity.Value == "" || s.Quantity.Scheme == "" || s.Units.Value == "" || s.Units.Scheme != "UCUM" || !finite(s.Min) || !finite(s.Max) || !finite(s.Max-s.Min) || s.Min >= s.Max {
			return o, ErrInvalidOptions
		}
	} else if o.Scalar != nil {
		return o, ErrInvalidOptions
	}
	return o, nil
}

type prepared struct {
	selectedTrack
	indices []int
	sampled []int // positions in indices, after removing LOD-created repeats
	values  []float32
	closed  bool
	target  int
}

// BuildDisplay returns bounded patient-space geometry for a separate GPU scene.
// Selection/analysis always use the originals. Budget-driven LOD samples tracks
// in input order and points uniformly by source index (not by screen distance).
// All selected scalar data is validated even if its track is omitted by LOD.
func BuildDisplay(ctx context.Context, r *tractography.Results, ids []TrackID, o DisplayOptions) (*Display, error) {
	o, err := displayOptions(o)
	if err != nil {
		return nil, err
	}
	ctx, err = operation(ctx, r)
	if err != nil {
		return nil, err
	}
	selected, err := resolve(ctx, r, ids)
	if err != nil {
		return nil, err
	}
	d := &Display{Stats: DisplayStats{InputTracks: len(selected)}}
	ready := make([]prepared, 0, len(selected))
	for _, s := range selected {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		p := prepared{selectedTrack: s}
		d.Stats.InputPoints += len(s.track.Points)
		if o.Color == Scalar {
			p.values, err = scalarValues(ctx, s, o.Scalar)
			if err != nil {
				return nil, err
			}
		}
		for i, v := range s.track.Points {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			if i == 0 || v != s.track.Points[i-1] {
				p.indices = append(p.indices, i)
			}
		}
		if len(p.indices) < 2 {
			d.Stats.DegenerateTracks = append(d.Stats.DegenerateTracks, s.id)
			continue
		}
		p.closed = len(p.indices) >= 3 && s.track.Points[p.indices[0]] == s.track.Points[p.indices[len(p.indices)-1]]
		ready = append(ready, p)
	}
	if len(ready) > o.MaxTracks {
		sample := make([]prepared, o.MaxTracks)
		for i := range sample {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			sample[i] = ready[uniformIndex(i, len(sample), len(ready))]
		}
		ready = sample
		d.Stats.LOD = true
	}
	// Start with explicit stride. Lower all track point budgets evenly until all
	// output ceilings fit. A track always keeps both ends (and a closed triangle).
	for i := range ready {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		n := len(ready[i].indices)
		target := 2 + (n-2)/o.PointStride
		minimum := minimumPoints(ready[i])
		if target < minimum {
			target = minimum
		}
		if target > n {
			target = n
		}
		ready[i].target = target
		if err := ready[i].resample(ctx); err != nil {
			return nil, err
		}
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		points, vertices, triangles, bytes := planned(ready, o)
		if points <= int64(o.MaxPoints) && triangles <= int64(o.MaxTriangles) && bytes <= int64(o.MaxBytes) {
			d.Stats.DisplayedPoints = int(points)
			d.Stats.Vertices = int(vertices)
			d.Stats.Triangles = int(triangles)
			d.Stats.Bytes = int(bytes)
			break
		}
		changed := false
		for i := range ready {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			minimum := minimumPoints(ready[i])
			if ready[i].target > minimum {
				ready[i].target = minimum + (ready[i].target-minimum)/2
				if err := ready[i].resample(ctx); err != nil {
					return nil, err
				}
				changed = true
			}
		}
		if !changed {
			return nil, fmt.Errorf("%w: minimum selected display", ErrLimitExceeded)
		}
		d.Stats.LOD = true
	}
	for _, p := range ready {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if p.target < len(p.indices) {
			d.Stats.LOD = true
		}
		line, e := buildPolyline(ctx, p, o)
		if e != nil {
			return nil, e
		}
		if o.Style == Line {
			d.Lines = append(d.Lines, line)
		} else {
			surface, e := tessellate(ctx, line, p.closed, o)
			if e != nil {
				return nil, e
			}
			d.Surfaces = append(d.Surfaces, surface)
		}
	}
	d.Stats.DisplayedTracks = len(ready)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return d, nil
}

func uniformIndex(i, n, total int) int {
	if n == 1 {
		return 0
	}
	return int(int64(i) * int64(total-1) / int64(n-1))
}

func (p *prepared) resample(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p.sampled = make([]int, 0, p.target)
	for i := 0; i < p.target; i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		k := uniformIndex(i, p.target, len(p.indices))
		if len(p.sampled) > 0 && p.track.Points[p.indices[k]] == p.track.Points[p.indices[p.sampled[len(p.sampled)-1]]] {
			// Keep the final original endpoint index without replacing the first.
			if i == p.target-1 && len(p.sampled) > 1 {
				p.sampled[len(p.sampled)-1] = k
			}
			continue
		}
		p.sampled = append(p.sampled, k)
	}
	// A folded/closed tract can have identical uniformly sampled positions.
	// Keep an actual original excursion, rather than inventing a zero tangent.
	if len(p.sampled) == 1 {
		p.sampled = []int{0, 1, len(p.indices) - 1}
	}
	return ctx.Err()
}

func buildPolyline(ctx context.Context, p prepared, o DisplayOptions) (Polyline, error) {
	if err := ctx.Err(); err != nil {
		return Polyline{}, err
	}
	line := Polyline{Owner: p.id, Points: make([][3]float32, len(p.sampled)), Colors: make([][3]float32, len(p.sampled)), SourcePointIndices: make([]int, len(p.sampled))}
	for j, k := range p.sampled {
		if j&255 == 0 {
			if err := ctx.Err(); err != nil {
				return Polyline{}, err
			}
		}
		ix := p.indices[k]
		line.Points[j] = p.track.Points[ix]
		line.SourcePointIndices[j] = ix
		if o.Color == Direction {
			v := tangent(p.track.Points, p.indices, k, p.closed)
			line.Colors[j] = [3]float32{float32(math.Abs(v[0])), float32(math.Abs(v[1])), float32(math.Abs(v[2]))}
		} else {
			v := math.Max(0, math.Min(1, (float64(p.values[ix])-o.Scalar.Min)/(o.Scalar.Max-o.Scalar.Min)))
			line.Colors[j] = [3]float32{float32(v), 0, float32(1 - v)}
		}
	}
	if err := ctx.Err(); err != nil {
		return Polyline{}, err
	}
	return line, nil
}

func minimumPoints(p prepared) int {
	if p.closed {
		if len(p.indices) < 4 {
			return len(p.indices)
		}
		return 4
	}
	return 2
}
func planned(ps []prepared, o DisplayOptions) (points, vertices, triangles, bytes int64) {
	for _, p := range ps {
		n := int64(len(p.sampled))
		points += n
		v, t := n, int64(0)
		if o.Style == Tube {
			v = n * int64(o.TubeSides)
			t = (n - 1) * int64(o.TubeSides) * 2
		}
		if o.Style == Ribbon {
			v = n * 2
			t = (n - 1) * 2
		}
		vertices += v
		triangles += t
		perVertex := 24 + strconv.IntSize/8
		if o.Style != Line {
			perVertex += 12
		}
		bytes += v*int64(perVertex) + t*12
	}
	return
}

func scalarValues(ctx context.Context, s selectedTrack, m *ScalarMapping) ([]float32, error) {
	if m.MeasurementIndex >= len(s.set.Measurements) {
		return nil, fmt.Errorf("%w: missing measurement", ErrInvalidData)
	}
	measurement := s.set.Measurements[m.MeasurementIndex]
	if measurement.Quantity.Code != m.Quantity || measurement.Units.Code != m.Units {
		return nil, fmt.Errorf("%w: measurement quantity/units mismatch", ErrInvalidData)
	}
	v := measurement.Tracks[s.id.TrackIndex]
	if len(v.Values) != len(s.track.Points) {
		return nil, fmt.Errorf("%w: sparse scalar measurement", ErrInvalidData)
	}
	// Arbitrarily ordered explicit indices remain valid, but must cover each
	// original point exactly once. Never align by value position alone.
	out := make([]float32, len(v.Values))
	seen := make([]bool, len(out))
	for i, value := range v.Values {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !finite(float64(value)) {
			return nil, fmt.Errorf("%w: nonfinite scalar", ErrInvalidData)
		}
		ix := i
		if v.PointIndices != nil {
			ix = int(v.PointIndices[i]) - 1
		}
		if seen[ix] {
			return nil, fmt.Errorf("%w: repeated scalar index", ErrInvalidData)
		}
		seen[ix] = true
		out[ix] = value
	}
	return out, nil
}

func tangent(points [][3]float32, indices []int, i int, closed bool) [3]float64 {
	a, b := i-1, i+1
	if closed && (i == 0 || i == len(indices)-1) {
		a, b = len(indices)-2, 1
	} else {
		if a < 0 {
			a = 0
		}
		if b >= len(indices) {
			b = len(indices) - 1
		}
	}
	v := sub(vec(points[indices[b]]), vec(points[indices[a]]))
	if norm(v) == 0 {
		if i+1 < len(indices) {
			v = sub(vec(points[indices[i+1]]), vec(points[indices[i]]))
		} else {
			v = sub(vec(points[indices[i]]), vec(points[indices[i-1]]))
		}
	}
	return unit(v)
}

func tessellate(ctx context.Context, line Polyline, closed bool, o DisplayOptions) (Surface, error) {
	s := Surface{Owner: line.Owner}
	sides := o.TubeSides
	if o.Style == Ribbon {
		sides = 2
	}
	n := len(line.Points)
	s.Mesh.Vertices = make([][3]float32, 0, n*sides)
	s.Mesh.Normals = make([][3]float32, 0, n*sides)
	s.Colors = make([][3]float32, 0, n*sides)
	s.SourcePointIndices = make([]int, 0, n*sides)
	indices := make([]int, n)
	for i := range indices {
		indices[i] = i
	}
	normal := [3]float64{}
	firstNormal := [3]float64{}
	for i, p := range line.Points {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return Surface{}, err
			}
		}
		t := tangent(line.Points, indices, i, closed)
		projected := sub(normal, mul(t, dot(normal, t)))
		if norm(projected) < 1e-10 {
			axis := [3]float64{1, 0, 0}
			if math.Abs(t[0]) > .8 {
				axis = [3]float64{0, 1, 0}
			}
			projected = cross(t, axis)
		}
		normal = unit(projected)
		if i == 0 {
			firstNormal = normal
		}
		if closed && i == n-1 {
			normal = firstNormal
		}
		binormal := unit(cross(t, normal))
		for side := 0; side < sides; side++ {
			offset := normal
			if o.Style == Ribbon {
				if side == 1 {
					offset = mul(normal, -1)
				}
			} else {
				angle := 2 * math.Pi * float64(side) / float64(sides)
				offset = add(mul(normal, math.Cos(angle)), mul(binormal, math.Sin(angle)))
			}
			position := add(vec(p), mul(offset, o.RadiusMM))
			v := [3]float32{float32(position[0]), float32(position[1]), float32(position[2])}
			for _, x := range v {
				if !finite(float64(x)) {
					return Surface{}, fmt.Errorf("%w: output coordinate overflow", ErrInvalidData)
				}
			}
			nv := offset
			if o.Style == Ribbon {
				nv = binormal
			}
			s.Mesh.Vertices = append(s.Mesh.Vertices, v)
			s.Mesh.Normals = append(s.Mesh.Normals, [3]float32{float32(nv[0]), float32(nv[1]), float32(nv[2])})
			s.Colors = append(s.Colors, line.Colors[i])
			s.SourcePointIndices = append(s.SourcePointIndices, line.SourcePointIndices[i])
		}
	}
	for i := 0; i < n-1; i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return Surface{}, err
			}
		}
		segments := sides
		if o.Style == Ribbon {
			segments = 1
		}
		for side := 0; side < segments; side++ {
			a, b := uint32(i*sides+side), uint32(i*sides+(side+1)%sides)
			c, d := a+uint32(sides), b+uint32(sides)
			s.Mesh.Triangles = append(s.Mesh.Triangles, [3]uint32{a, b, c}, [3]uint32{b, d, c})
		}
	}
	return s, nil
}
