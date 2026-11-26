package tractographygeom

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/tractography"
)

// Select returns IDs in stored set/track order. It changes neither source nor
// measurements, and never uses display LOD geometry for region intersection.
func Select(ctx context.Context, r *tractography.Results, o SelectionOptions) ([]TrackID, error) {
	if len(o.Include)+len(o.Exclude) > 1024 {
		return nil, ErrLimitExceeded
	}
	for _, regions := range [][]Region{o.Include, o.Exclude} {
		for _, q := range regions {
			if !validRegion(q) {
				return nil, fmt.Errorf("%w: region", ErrInvalidOptions)
			}
		}
	}
	ctx, err := operation(ctx, r)
	if err != nil {
		return nil, err
	}
	out := make([]TrackID, 0)
	for _, s := range r.TrackSets {
		for ti, t := range s.Tracks {
			include := len(o.Include) == 0 || o.RequireAllIncludes
			for _, q := range o.Include {
				hit, e := intersects(ctx, t.Points, q)
				if e != nil {
					return nil, e
				}
				if o.RequireAllIncludes {
					include = include && hit
					if !include {
						break
					}
				} else {
					include = include || hit
					if include {
						break
					}
				}
			}
			if !include {
				continue
			}
			excluded := false
			for _, q := range o.Exclude {
				hit, e := intersects(ctx, t.Points, q)
				if e != nil {
					return nil, e
				}
				if hit {
					excluded = true
					break
				}
			}
			if !excluded {
				out = append(out, TrackID{s.Number, ti})
			}
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
func validRegion(q Region) bool {
	for _, p := range [][3]float64{q.Center, q.Min, q.Max} {
		for _, v := range p {
			if !finite(v) {
				return false
			}
		}
	}
	if !finite(q.RadiusMM) {
		return false
	}
	switch q.Kind {
	case Sphere:
		return q.RadiusMM > 0 && q.Min == [3]float64{} && q.Max == [3]float64{}
	case Box:
		if q.Center != [3]float64{} || q.RadiusMM != 0 {
			return false
		}
		for i := 0; i < 3; i++ {
			if q.Min[i] > q.Max[i] {
				return false
			}
		}
		return true
	}
	return false
}
func intersects(ctx context.Context, points [][3]float32, q Region) (bool, error) {
	for i := 1; i < len(points); i++ {
		if i&255 == 0 {
			if err := ctx.Err(); err != nil {
				return false, err
			}
		}
		a, b := vec(points[i-1]), vec(points[i])
		if segmentHit(a, b, q) {
			return true, nil
		}
	}
	return false, ctx.Err()
}
func segmentHit(a, b [3]float64, q Region) bool {
	d := sub(b, a)
	if q.Kind == Sphere {
		t := 0.
		if dd := dot(d, d); dd > 0 {
			t = math.Max(0, math.Min(1, dot(sub(q.Center, a), d)/dd))
		}
		return norm(sub(add(a, mul(d, t)), q.Center)) <= q.RadiusMM
	}
	lo, hi := 0., 1.
	for k := 0; k < 3; k++ {
		if d[k] == 0 {
			if a[k] < q.Min[k] || a[k] > q.Max[k] {
				return false
			}
			continue
		}
		x, y := (q.Min[k]-a[k])/d[k], (q.Max[k]-a[k])/d[k]
		if x > y {
			x, y = y, x
		}
		lo = math.Max(lo, x)
		hi = math.Min(hi, y)
		if lo > hi {
			return false
		}
	}
	return true
}
