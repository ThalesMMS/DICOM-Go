package fitmap

import (
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Frame is one image of a multi-echo or multi-b series to group: its fit
// parameter (echo time or b-value), its position along the slice normal
// (mm) and the caller's index for it.
type Frame struct {
	Parameter float64
	Position  float64
	Index     int
}

// Grouping is a series sorted for fitting: Images[p][s] is the index of the
// frame at Parameters[p] and Positions[s].
type Grouping struct {
	Parameters []float64
	Positions  []float64
	Images     [][]int
	// Dropped counts the frames left out: at a position not acquired at
	// every parameter, or repeating a parameter and position.
	Dropped int
}

// Group sorts frames into one volume per parameter, as Horos's T2 fit map
// groups its images by position: parameters within parameterTolerance are
// one value (their first, in ascending order) and positions within
// positionTolerance one slice; only the positions acquired at every
// parameter are kept. It needs two parameters and one common position.
func Group(frames []Frame, parameterTolerance, positionTolerance float64) (Grouping, error) {
	if !(parameterTolerance >= 0) || !(positionTolerance >= 0) {
		return Grouping{}, fmt.Errorf("%w: negative tolerance", ErrFit)
	}
	sorted := append([]Frame(nil), frames...)
	for _, f := range sorted {
		if !finite(f.Parameter) || !finite(f.Position) || f.Parameter < 0 {
			return Grouping{}, fmt.Errorf("%w: frame %d has parameter %v at %v mm", ErrFit, f.Index, f.Parameter, f.Position)
		}
	}
	sort.SliceStable(sorted, func(a, b int) bool { return sorted[a].Parameter < sorted[b].Parameter })
	var out Grouping
	var groups [][]Frame
	for _, f := range sorted {
		if n := len(out.Parameters); n > 0 && f.Parameter-out.Parameters[n-1] <= parameterTolerance {
			groups[n-1] = append(groups[n-1], f)
			continue
		}
		out.Parameters = append(out.Parameters, f.Parameter)
		groups = append(groups, []Frame{f})
	}
	if len(out.Parameters) < 2 {
		return Grouping{}, fmt.Errorf("%w: %d distinct parameter values", ErrFit, len(out.Parameters))
	}
	// Positions of the first group, then kept when every group has one.
	slot := func(group []Frame, position float64) int {
		for i, f := range group {
			if math.Abs(f.Position-position) <= positionTolerance {
				return i
			}
		}
		return -1
	}
	first := append([]Frame(nil), groups[0]...)
	sort.SliceStable(first, func(a, b int) bool { return first[a].Position < first[b].Position })
	used := make([]map[int]bool, len(groups))
	for g := range used {
		used[g] = map[int]bool{}
	}
	out.Images = make([][]int, len(groups))
	for _, f := range first {
		if n := len(out.Positions); n > 0 && math.Abs(f.Position-out.Positions[n-1]) <= positionTolerance {
			continue // a repeat of the previous position
		}
		row := make([]int, len(groups))
		complete := true
		for g, group := range groups {
			i := slot(group, f.Position)
			if i < 0 {
				complete = false
				break
			}
			row[g] = i
		}
		if !complete {
			continue
		}
		out.Positions = append(out.Positions, f.Position)
		for g, i := range row {
			out.Images[g] = append(out.Images[g], groups[g][i].Index)
			used[g][i] = true
		}
	}
	if len(out.Positions) == 0 {
		return Grouping{}, fmt.Errorf("%w: no position is acquired at every parameter", ErrFit)
	}
	for g, group := range groups {
		out.Dropped += len(group) - len(used[g])
	}
	return out, nil
}

var (
	tagEchoTime          = core.NewTag(0x0018, 0x0081)
	tagMREchoSequence    = core.NewTag(0x0018, 0x9114)
	tagEffectiveEchoTime = core.NewTag(0x0018, 0x9082)
)

// EchoTime returns the echo time in ms of a frame (0-based) of an MR
// object: the Effective Echo Time of the frame's MR Echo functional group in
// enhanced objects, otherwise Echo Time (0018,0081). ok is false when
// neither is a finite, non-negative number.
func EchoTime(obj *object.Object, frameIndex int) (float64, bool) {
	if obj == nil {
		return 0, false
	}
	if item, _, err := obj.FunctionalGroupItem(frameIndex, tagMREchoSequence); err == nil && item != nil {
		if value, err := item.GetFloat(tagEffectiveEchoTime); err == nil && finite(value) && value >= 0 {
			return value, true
		}
	}
	if value, err := obj.GetFloat(tagEchoTime); err == nil && finite(value) && value >= 0 {
		return value, true
	}
	return 0, false
}
