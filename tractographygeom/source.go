package tractographygeom

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/tractography"
)

func operation(ctx context.Context, r *tractography.Results) (context.Context, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := tractography.Validate(ctx, r, tractography.Limits{}); err != nil {
		return ctx, err
	}
	for _, s := range r.TrackSets {
		for _, t := range s.Tracks {
			for i, p := range t.Points {
				if i&255 == 0 {
					if err := ctx.Err(); err != nil {
						return ctx, err
					}
				}
				for _, v := range p {
					if !finite(float64(v)) {
						return ctx, fmt.Errorf("%w: nonfinite point", ErrInvalidData)
					}
				}
			}
		}
	}
	return ctx, ctx.Err()
}

func finite(v float64) bool                  { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func vec(p [3]float32) [3]float64            { return [3]float64{float64(p[0]), float64(p[1]), float64(p[2])} }
func add(a, b [3]float64) [3]float64         { return [3]float64{a[0] + b[0], a[1] + b[1], a[2] + b[2]} }
func sub(a, b [3]float64) [3]float64         { return [3]float64{a[0] - b[0], a[1] - b[1], a[2] - b[2]} }
func mul(a [3]float64, b float64) [3]float64 { return [3]float64{a[0] * b, a[1] * b, a[2] * b} }
func dot(a, b [3]float64) float64            { return a[0]*b[0] + a[1]*b[1] + a[2]*b[2] }
func cross(a, b [3]float64) [3]float64 {
	return [3]float64{a[1]*b[2] - a[2]*b[1], a[2]*b[0] - a[0]*b[2], a[0]*b[1] - a[1]*b[0]}
}
func norm(a [3]float64) float64 { return math.Hypot(math.Hypot(a[0], a[1]), a[2]) }
func unit(a [3]float64) [3]float64 {
	n := norm(a)
	if n == 0 {
		return [3]float64{}
	}
	return mul(a, 1/n)
}

type selectedTrack struct {
	id    TrackID
	set   *tractography.TrackSet
	track *tractography.Track
}

func resolve(ctx context.Context, r *tractography.Results, ids []TrackID) ([]selectedTrack, error) {
	sets := make(map[uint32]*tractography.TrackSet, len(r.TrackSets))
	for i := range r.TrackSets {
		sets[r.TrackSets[i].Number] = &r.TrackSets[i]
	}
	seen := make(map[TrackID]bool, len(ids))
	out := make([]selectedTrack, 0, len(ids))
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		s := sets[id.SetNumber]
		if s == nil || id.TrackIndex < 0 || id.TrackIndex >= len(s.Tracks) || seen[id] {
			return nil, fmt.Errorf("%w: missing or duplicate track %v", ErrInvalidOptions, id)
		}
		seen[id] = true
		out = append(out, selectedTrack{id, s, &s.Tracks[id.TrackIndex]})
	}
	return out, nil
}

// Analyze measures every original segment in float64. Returned measurement
// entries contain just this track, with original point index order and codes;
// stored NaN measurement payloads are preserved, never treated as zero.
func Analyze(ctx context.Context, r *tractography.Results, ids []TrackID) (*Analysis, error) {
	ctx, err := operation(ctx, r)
	if err != nil {
		return nil, err
	}
	tracks, err := resolve(ctx, r, ids)
	if err != nil {
		return nil, err
	}
	a := &Analysis{Tracks: make([]TrackAnalysis, 0, len(tracks)), SOPInstanceUID: r.SOPInstanceUID, StudyInstanceUID: r.StudyInstanceUID, SeriesInstanceUID: r.SeriesInstanceUID, FrameOfReferenceUID: r.FrameOfReferenceUID}
	seenSets := make(map[uint32]bool)
	for _, s := range tracks {
		if !seenSets[s.id.SetNumber] {
			set := SetAnalysis{SetNumber: s.id.SetNumber}
			for _, m := range s.set.Statistics {
				m.Quantity = cloneCode(m.Quantity)
				m.Modifier = cloneCode(m.Modifier)
				m.Units = cloneCode(m.Units)
				set.Statistics = append(set.Statistics, m)
			}
			a.Sets = append(a.Sets, set)
			seenSets[s.id.SetNumber] = true
		}
		t := TrackAnalysis{ID: s.id}
		for i := 1; i < len(s.track.Points); i++ {
			if i&255 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			t.LengthMM += norm(sub(vec(s.track.Points[i]), vec(s.track.Points[i-1])))
		}
		for _, m := range s.set.Measurements {
			v := m.Tracks[s.id.TrackIndex]
			t.Measurements = append(t.Measurements, tractography.Measurement{Quantity: cloneCode(m.Quantity), Units: cloneCode(m.Units), Tracks: []tractography.MeasurementValues{{Values: append([]float32(nil), v.Values...), PointIndices: cloneIndices(v.PointIndices)}}})
		}
		for _, m := range s.set.TrackStatistics {
			t.Statistics = append(t.Statistics, tractography.TrackStatistic{Quantity: cloneCode(m.Quantity), Modifier: cloneCode(m.Modifier), Units: cloneCode(m.Units), Values: []float32{m.Values[s.id.TrackIndex]}})
		}
		a.TotalLengthMM += t.LengthMM
		a.Tracks = append(a.Tracks, t)
	}
	for _, v := range r.References {
		v.Frames = append([]int32(nil), v.Frames...)
		v.Segments = append([]uint16(nil), v.Segments...)
		a.References = append(a.References, v)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return a, nil
}
func cloneIndices(v []uint32) []uint32 {
	if v == nil {
		return nil
	}
	return append(make([]uint32, 0, len(v)), v...)
}
func cloneCode(v tractography.Code) tractography.Code {
	if v.Context != nil {
		c := *v.Context
		v.Context = &c
	}
	if v.Equivalent != nil {
		eq := make([]tractography.Code, len(v.Equivalent))
		for i := range eq {
			eq[i] = cloneCode(v.Equivalent[i])
		}
		v.Equivalent = eq
	}
	return v
}
