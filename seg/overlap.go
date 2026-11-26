package seg

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/roi"
)

// ErrOverlappingSegments reports segments that share pixels where a
// representation (a label map) needs them disjoint.
var ErrOverlappingSegments = errors.New("dicom/seg: segments overlap")

// SegmentOverlap is the overlap of two segments.
type SegmentOverlap struct {
	// First and Second are segment numbers, First < Second.
	First, Second int
	// Pixels is the number of shared pixels over all positions.
	Pixels int64
	// Positions is the number of frame positions where they share pixels.
	Positions int
}

// OverlapReport lists the pairs of segments that share at least one pixel.
type OverlapReport struct {
	// Segments are the segment numbers present in the frames, ascending.
	Segments []int
	// Overlaps lists the overlapping pairs by First, then Second.
	Overlaps []SegmentOverlap
}

// Overlapping reports whether segments a and b share a pixel.
func (report OverlapReport) Overlapping(a, b int) bool {
	if a > b {
		a, b = b, a
	}
	for _, overlap := range report.Overlaps {
		if overlap.First == a && overlap.Second == b {
			return true
		}
	}
	return false
}

// DisjointGroups splits the segments into groups whose members do not
// overlap, each of which could be one label map. Like DCMTK's OverlapUtil it
// is greedy: each segment, in ascending order, joins the first group it does
// not overlap, so the grouping is valid but not necessarily minimal.
func (report OverlapReport) DisjointGroups() [][]int {
	var groups [][]int
	for _, segment := range report.Segments {
		placed := false
		for index, group := range groups {
			disjoint := true
			for _, member := range group {
				if report.Overlapping(segment, member) {
					disjoint = false
					break
				}
			}
			if disjoint {
				groups[index] = append(group, segment)
				placed = true
				break
			}
		}
		if !placed {
			groups = append(groups, []int{segment})
		}
	}
	return groups
}

// positionKey identifies the plane of a frame: its Image Position (Patient)
// on a 1 µm grid when the frame has geometry, otherwise its source image
// frame, otherwise its slice index.
type positionKey struct {
	kind    int
	x, y, z int64
	uid     string
	frame   int
}

func framePosition(frame Frame) positionKey {
	if validSliceGeometry(frame.Geometry) {
		quantize := func(value float64) int64 { return int64(math.Round(value * 1000)) }
		origin := frame.Geometry.Origin
		return positionKey{kind: 1, x: quantize(origin.X), y: quantize(origin.Y), z: quantize(origin.Z)}
	}
	if frame.ReferencedSOPInstanceUID != "" {
		return positionKey{kind: 2, uid: frame.ReferencedSOPInstanceUID, frame: frame.ReferencedFrameNumber}
	}
	return positionKey{kind: 3, frame: frame.SliceIndex}
}

// presenceMask is the mask of the pixels of a BINARY or FRACTIONAL frame that
// belong to its segment (fractional samples above zero).
func presenceMask(doc *Document, frame Frame) *roi.RasterMask {
	if len(frame.Fractional) > 0 {
		mask := roi.NewRasterMask(doc.Columns, doc.Rows)
		for y := 0; y < doc.Rows; y++ {
			runStart := -1
			for x := 0; x <= doc.Columns; x++ {
				index := y*doc.Columns + x
				present := x < doc.Columns && index < len(frame.Fractional) && frame.Fractional[index] > 0
				switch {
				case present && runStart < 0:
					runStart = x
				case !present && runStart >= 0:
					mask.SetRun(y, runStart, x)
					runStart = -1
				}
			}
		}
		return mask
	}
	return frame.Mask
}

type runEvent struct {
	x       int
	segment int
	delta   int
}

// Overlaps computes which segments of a BINARY or FRACTIONAL document share
// pixels at the same frame position. It works on the run-length form of the
// masks, sweeping the runs of each row, so its cost follows the number of
// runs rather than of pixels. A label map has no overlaps by construction.
func Overlaps(doc *Document) (OverlapReport, error) {
	if doc == nil {
		return OverlapReport{}, fmt.Errorf("%w: document is nil", ErrInvalidObject)
	}
	present := map[int]bool{}
	if doc.SOPClassUID == LabelMapSegmentationStorage {
		for _, frame := range doc.Frames {
			for _, value := range frame.LabelMap {
				present[int(value)] = true
			}
		}
		return OverlapReport{Segments: sortedKeys(present)}, nil
	}

	// Union the frames of each segment at each position.
	type positionMasks struct {
		segments map[int]*roi.RasterMask
		owned    map[int]bool // masks built here, safe to modify
	}
	positions := map[positionKey]*positionMasks{}
	for _, frame := range doc.Frames {
		mask := presenceMask(doc, frame)
		if mask == nil || mask.Empty() {
			continue
		}
		present[frame.SegmentNumber] = true
		key := framePosition(frame)
		group := positions[key]
		if group == nil {
			group = &positionMasks{segments: map[int]*roi.RasterMask{}, owned: map[int]bool{}}
			positions[key] = group
		}
		existing := group.segments[frame.SegmentNumber]
		switch {
		case existing == nil:
			group.segments[frame.SegmentNumber] = mask
			group.owned[frame.SegmentNumber] = mask != frame.Mask
		default:
			if !group.owned[frame.SegmentNumber] {
				existing = existing.Clone()
				group.segments[frame.SegmentNumber] = existing
				group.owned[frame.SegmentNumber] = true
			}
			existing.Union(mask)
		}
	}

	type pair struct{ first, second int }
	pixels := map[pair]int64{}
	positionCounts := map[pair]int{}
	for _, group := range positions {
		if len(group.segments) < 2 {
			continue
		}
		segments := sortedKeys(group.segments)
		atPosition := map[pair]bool{}
		var events []runEvent
		var active []int
		for y := 0; y < doc.Rows; y++ {
			events = events[:0]
			for _, segment := range segments {
				for _, run := range group.segments[segment].Runs(y) {
					events = append(events, runEvent{run.Start, segment, 1}, runEvent{run.End, segment, -1})
				}
			}
			if len(events) < 4 {
				continue
			}
			sort.Slice(events, func(i, j int) bool {
				if events[i].x != events[j].x {
					return events[i].x < events[j].x
				}
				return events[i].delta < events[j].delta
			})
			active = active[:0]
			previous := events[0].x
			for _, event := range events {
				if width := event.x - previous; width > 0 && len(active) > 1 {
					for i := 0; i < len(active); i++ {
						for j := i + 1; j < len(active); j++ {
							key := pair{active[i], active[j]}
							pixels[key] += int64(width)
							atPosition[key] = true
						}
					}
				}
				previous = event.x
				if event.delta > 0 {
					index := sort.SearchInts(active, event.segment)
					active = append(active, 0)
					copy(active[index+1:], active[index:])
					active[index] = event.segment
				} else if index := sort.SearchInts(active, event.segment); index < len(active) && active[index] == event.segment {
					active = append(active[:index], active[index+1:]...)
				}
			}
		}
		for key := range atPosition {
			positionCounts[key]++
		}
	}

	report := OverlapReport{Segments: sortedKeys(present)}
	for key, count := range pixels {
		report.Overlaps = append(report.Overlaps, SegmentOverlap{First: key.first, Second: key.second, Pixels: count, Positions: positionCounts[key]})
	}
	sort.Slice(report.Overlaps, func(i, j int) bool {
		if report.Overlaps[i].First != report.Overlaps[j].First {
			return report.Overlaps[i].First < report.Overlaps[j].First
		}
		return report.Overlaps[i].Second < report.Overlaps[j].Second
	})
	return report, nil
}

func sortedKeys[V any](values map[int]V) []int {
	keys := make([]int, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}
