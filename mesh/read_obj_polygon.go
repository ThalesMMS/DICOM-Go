package mesh

import (
	"context"
	"fmt"
	"math"
)

func triangulateOBJ(ctx context.Context, m *Mesh, corners []objCorner) ([][3]int, error) {
	n := len(corners)
	if n == 3 {
		return [][3]int{{0, 1, 2}}, nil
	}
	points := make([][3]float64, n)
	var normal [3]float64
	for i, c := range corners {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		points[i] = m.point(c.vertex)
		for j := 0; j < i; j++ {
			if points[j] == points[i] {
				return nil, fmt.Errorf("%w: polygon repeats a point", ErrInvalidModel)
			}
		}
	}
	for i, a := range points {
		b := points[(i+1)%n]
		normal[0] += (a[1] - b[1]) * (a[2] + b[2])
		normal[1] += (a[2] - b[2]) * (a[0] + b[0])
		normal[2] += (a[0] - b[0]) * (a[1] + b[1])
	}
	length := math.Sqrt(dot3(normal, normal))
	if length == 0 {
		return nil, fmt.Errorf("%w: zero-area polygon", ErrInvalidModel)
	}
	axis := 0
	for i := 1; i < 3; i++ {
		if math.Abs(normal[i]) > math.Abs(normal[axis]) {
			axis = i
		}
	}
	extent := 0.0
	for _, p := range points {
		extent = math.Max(extent, math.Sqrt(dot3(sub3(p, points[0]), sub3(p, points[0]))))
	}
	tolerance := math.Max(extent*1e-6, 1e-12)
	projected := make([][2]float64, n)
	for i, p := range points {
		if math.Abs(dot3(normal, sub3(p, points[0]))/length) > tolerance {
			return nil, fmt.Errorf("%w: non-planar OBJ polygon", ErrInvalidModel)
		}
		switch axis {
		case 0:
			projected[i] = [2]float64{p[1], p[2]}
		case 1:
			projected[i] = [2]float64{p[0], p[2]}
		case 2:
			projected[i] = [2]float64{p[0], p[1]}
		}
	}
	cross := func(a, b, c [2]float64) float64 { return (b[0]-a[0])*(c[1]-a[1]) - (b[1]-a[1])*(c[0]-a[0]) }
	area := 0.0
	for i, a := range projected {
		b := projected[(i+1)%n]
		area += a[0]*b[1] - b[0]*a[1]
	}
	if area == 0 {
		return nil, fmt.Errorf("%w: zero projected polygon area", ErrInvalidModel)
	}
	sign := 1.0
	if area < 0 {
		sign = -1
	}
	// Reject crossings, tangencies and overlapping nonadjacent edges.
	onSegment := func(a, b, p [2]float64) bool {
		return cross(a, b, p) == 0 && p[0] >= math.Min(a[0], b[0]) && p[0] <= math.Max(a[0], b[0]) && p[1] >= math.Min(a[1], b[1]) && p[1] <= math.Max(a[1], b[1])
	}
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			if j == i+1 || (i == 0 && j == n-1) {
				continue
			}
			a, b, c, d := projected[i], projected[(i+1)%n], projected[j], projected[(j+1)%n]
			if cross(a, b, c)*cross(a, b, d) < 0 && cross(c, d, a)*cross(c, d, b) < 0 || onSegment(a, b, c) || onSegment(a, b, d) || onSegment(c, d, a) || onSegment(c, d, b) {
				return nil, fmt.Errorf("%w: self-intersecting polygon", ErrInvalidModel)
			}
		}
	}
	remaining := make([]int, n)
	for i := range remaining {
		remaining[i] = i
	}
	out := make([][3]int, 0, n-2)
	for len(remaining) > 3 {
		found := false
		for i, b := range remaining {
			if ctx != nil {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			a, c := remaining[(i+len(remaining)-1)%len(remaining)], remaining[(i+1)%len(remaining)]
			if cross(projected[a], projected[b], projected[c])*sign <= 0 {
				continue
			}
			blocked := false
			for _, p := range remaining {
				if p == a || p == b || p == c {
					continue
				}
				if cross(projected[a], projected[b], projected[p])*sign >= 0 && cross(projected[b], projected[c], projected[p])*sign >= 0 && cross(projected[c], projected[a], projected[p])*sign >= 0 {
					blocked = true
					break
				}
			}
			if blocked {
				continue
			}
			out = append(out, [3]int{a, b, c})
			remaining = append(remaining[:i], remaining[i+1:]...)
			found = true
			break
		}
		if !found {
			return nil, fmt.Errorf("%w: degenerate/non-simple OBJ polygon", ErrInvalidModel)
		}
	}
	out = append(out, [3]int{remaining[0], remaining[1], remaining[2]})
	return out, nil
}
