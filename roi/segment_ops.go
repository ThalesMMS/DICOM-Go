package roi

import (
	"context"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Segmentation operations on RasterMask / Segmentation3D. Everything here is
// pure Go: brush/fill, thresholding, connected components, and basic morphology
// run without any ITK, Python, or cgo dependency, keeping the default build
// lightweight. Flood fill and connected-component labelling live in
// connected_components.go.

// Brush sets or clears a filled disc of the given radius (pixels) centered at
// (cx,cy) in the mask.
func Brush(mask *RasterMask, cx, cy, radius int, on bool) {
	if mask == nil || radius < 0 {
		return
	}
	r2 := radius * radius
	for dy := -radius; dy <= radius; dy++ {
		for dx := -radius; dx <= radius; dx++ {
			if dx*dx+dy*dy <= r2 {
				mask.Set(cx+dx, cy+dy, on)
			}
		}
	}
}

// BrushSegment sets or clears discs of radius pixels along the segment from
// first to last, including both endpoints. It stamps once per pixel on the
// dominant axis and rounds the other coordinate to the nearest pixel, with
// half-pixel ties rounded up. Stamps are clipped to the mask.
func (m *RasterMask) BrushSegment(first, last image.Point, radius int, on bool) {
	if m == nil || m.Columns == 0 || m.Rows == 0 || radius < 0 {
		return
	}
	if first == last {
		Brush(m, first.X, first.Y, radius, on)
		return
	}

	dx := float64(last.X) - float64(first.X)
	dy := float64(last.Y) - float64(first.Y)
	xMajor := math.Abs(dx) >= math.Abs(dy)
	start, end, size, delta := first.X, last.X, m.Columns, dx
	if !xMajor {
		start, end, size, delta = first.Y, last.Y, m.Rows, dy
	}
	// Skip centers whose discs cannot reach the mask on the dominant axis.
	low := max(min(start, end), -radius)
	high := min(max(start, end), min(size-1, math.MaxInt-radius)+radius)
	for center := low; center <= high; center++ {
		t := (float64(center) - float64(start)) / delta
		if xMajor {
			Brush(m, center, render.NearestIndex(float64(first.Y)+dy*t), radius, on)
		} else {
			Brush(m, render.NearestIndex(float64(first.X)+dx*t), center, radius, on)
		}
		if center == high {
			break
		}
	}
}

// ThresholdMask selects available, finite pixel values in the inclusive range
// [lo, hi]. Infinite bounds can express an open-ended range.
func ThresholdMask(columns, rows int, lo, hi float64, valueAt func(x, y int) (float64, bool)) *RasterMask {
	mask := NewRasterMask(columns, rows)
	if valueAt == nil || !(lo <= hi) {
		return mask
	}
	for y := 0; y < rows; y++ {
		x := 0
		for x < columns {
			v, ok := valueAt(x, y)
			if !ok || !finiteNumber(v) || v < lo || v > hi {
				x++
				continue
			}
			start := x
			for x < columns {
				v, ok := valueAt(x, y)
				if !ok || !finiteNumber(v) || v < lo || v > hi {
					break
				}
				x++
			}
			mask.SetRun(y, start, x)
		}
	}
	return mask
}

// ThresholdSegmentation thresholds every slice of a volume into a Segmentation3D,
// carrying the supplied geometry. valueAt reads a slice's rescaled value.
func ThresholdSegmentation(geometry render.VolumeGeometry, columns, rows int, lo, hi float64, valueAt func(x, y, slice int) (float64, bool)) *Segmentation3D {
	seg := NewSegmentation3D(geometry, columns, rows)
	if valueAt == nil {
		return seg
	}
	for z := 0; z < len(geometry.Slices); z++ {
		slice := z
		mask := ThresholdMask(columns, rows, lo, hi, func(x, y int) (float64, bool) {
			return valueAt(x, y, slice)
		})
		seg.SetMask(slice, mask)
	}
	return seg
}

// InterpolateMask blends two same-sized binary masks at fraction t in [0,1].
// It first maps each endpoint into an interpolated bounding box, then blends
// signed distance fields inside that common frame. That keeps translated or
// resized painted masks moving between slices instead of copying one endpoint.
func InterpolateMask(first, last *RasterMask, t float64) *RasterMask {
	return interpolateMaskWithWorkspace(first, last, t, nil)
}

func interpolateMaskWithWorkspace(first, last *RasterMask, t float64, workspace *maskInterpolationWorkspace) *RasterMask {
	if first == nil || last == nil {
		return nil
	}
	if first.Columns != last.Columns || first.Rows != last.Rows {
		return nil
	}
	if t <= 0 {
		return first.Clone()
	}
	if t >= 1 {
		return last.Clone()
	}
	out := NewRasterMask(first.Columns, first.Rows)
	if first.Columns == 0 || first.Rows == 0 {
		return out
	}
	firstBounds, firstOK := maskBounds(first)
	lastBounds, lastOK := maskBounds(last)
	switch {
	case !firstOK && !lastOK:
		return out
	case !firstOK:
		return last.Clone()
	case !lastOK:
		return first.Clone()
	}
	targetBounds := interpolateBounds(firstBounds, lastBounds, t)
	firstAligned := remapMaskToBounds(first, firstBounds, targetBounds)
	lastAligned := remapMaskToBounds(last, lastBounds, targetBounds)
	if workspace == nil || workspace.columns != first.Columns || workspace.rows != first.Rows {
		workspace = newMaskInterpolationWorkspace(first.Columns, first.Rows)
	}
	signedDistanceFieldInto(workspace.firstField, firstAligned, workspace)
	signedDistanceFieldInto(workspace.lastField, lastAligned, workspace)
	for y := 0; y < first.Rows; y++ {
		rowStart := y * first.Columns
		for x := 0; x < first.Columns; {
			idx := rowStart + x
			if (1-t)*workspace.firstField[idx]+t*workspace.lastField[idx] < 0 {
				x++
				continue
			}
			start := x
			for x < first.Columns {
				idx = rowStart + x
				if (1-t)*workspace.firstField[idx]+t*workspace.lastField[idx] < 0 {
					break
				}
				x++
			}
			out.SetRun(y, start, x)
		}
	}
	return out
}

// InterpolateBetweenSlices fills the slices strictly between two manually edited
// endpoint slices. Existing intermediate masks are replaced with interpolated
// masks, while the endpoint masks are left untouched. It returns the number of
// intermediate slice masks written.
func InterpolateBetweenSlices(seg *Segmentation3D, firstSlice, lastSlice int) int {
	written, _ := InterpolateBetweenSlicesContext(context.Background(), seg, firstSlice, lastSlice)
	return written
}

// InterpolateBetweenSlicesContext is the cancellable form of
// InterpolateBetweenSlices. If ctx is cancelled, it returns the number of
// completed intermediate masks and leaves those masks installed.
func InterpolateBetweenSlicesContext(ctx context.Context, seg *Segmentation3D, firstSlice, lastSlice int) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if seg == nil || firstSlice == lastSlice {
		return 0, nil
	}
	if firstSlice > lastSlice {
		firstSlice, lastSlice = lastSlice, firstSlice
	}
	if lastSlice-firstSlice < 2 {
		return 0, nil
	}
	first, ok := seg.MaskAt(firstSlice)
	if !ok || first == nil || first.Empty() {
		return 0, nil
	}
	last, ok := seg.MaskAt(lastSlice)
	if !ok || last == nil || last.Empty() {
		return 0, nil
	}
	if first.Columns != last.Columns || first.Rows != last.Rows {
		return 0, nil
	}
	written := 0
	span := float64(lastSlice - firstSlice)
	workspace := newMaskInterpolationWorkspace(first.Columns, first.Rows)
	for slice := firstSlice + 1; slice < lastSlice; slice++ {
		if err := ctx.Err(); err != nil {
			return written, err
		}
		mask := interpolateMaskWithWorkspace(first, last, float64(slice-firstSlice)/span, workspace)
		if mask == nil || mask.Empty() {
			continue
		}
		seg.SetMask(slice, mask)
		written++
	}
	return written, nil
}

type rasterMaskBounds struct {
	minX int
	minY int
	maxX int
	maxY int
}

func maskBounds(mask *RasterMask) (rasterMaskBounds, bool) {
	if mask == nil || mask.Empty() {
		return rasterMaskBounds{}, false
	}
	bounds := rasterMaskBounds{
		minX: mask.Columns,
		minY: mask.Rows,
		maxX: -1,
		maxY: -1,
	}
	mask.ForEachRun(func(y int, run MaskRun) {
		if run.Start < bounds.minX {
			bounds.minX = run.Start
		}
		if run.End-1 > bounds.maxX {
			bounds.maxX = run.End - 1
		}
		if y < bounds.minY {
			bounds.minY = y
		}
		if y > bounds.maxY {
			bounds.maxY = y
		}
	})
	return bounds, bounds.maxX >= bounds.minX && bounds.maxY >= bounds.minY
}

func interpolateBounds(first, last rasterMaskBounds, t float64) rasterMaskBounds {
	return rasterMaskBounds{
		minX: render.NearestIndex(lerpFloat(float64(first.minX), float64(last.minX), t)),
		minY: render.NearestIndex(lerpFloat(float64(first.minY), float64(last.minY), t)),
		maxX: render.NearestIndex(lerpFloat(float64(first.maxX), float64(last.maxX), t)),
		maxY: render.NearestIndex(lerpFloat(float64(first.maxY), float64(last.maxY), t)),
	}
}

func remapMaskToBounds(mask *RasterMask, from, to rasterMaskBounds) *RasterMask {
	out := NewRasterMask(mask.Columns, mask.Rows)
	mask.ForEachRun(func(y int, run MaskRun) {
		mappedY := remapMaskCoordinate(y, from.minY, from.maxY, to.minY, to.maxY)
		mappedStart := remapMaskCoordinate(run.Start, from.minX, from.maxX, to.minX, to.maxX)
		mappedEnd := remapMaskCoordinate(run.End-1, from.minX, from.maxX, to.minX, to.maxX)
		if mappedStart > mappedEnd {
			mappedStart, mappedEnd = mappedEnd, mappedStart
		}
		out.SetRun(mappedY, mappedStart, mappedEnd+1)
	})
	return out
}

func remapMaskCoordinate(value, fromMin, fromMax, toMin, toMax int) int {
	if fromMax == fromMin {
		return render.NearestIndex((float64(toMin) + float64(toMax)) / 2)
	}
	position := float64(value-fromMin) / float64(fromMax-fromMin)
	return render.NearestIndex(lerpFloat(float64(toMin), float64(toMax), position))
}

func lerpFloat(a, b, t float64) float64 {
	return a + (b-a)*t
}

type maskInterpolationWorkspace struct {
	columns         int
	rows            int
	features        []bool
	tmp             []float64
	distanceScratch []float64
	firstField      []float64
	lastField       []float64
	lineValues      []float64
	lineOutput      []float64
	locations       []int
	boundaries      []float64
}

func newMaskInterpolationWorkspace(columns, rows int) *maskInterpolationWorkspace {
	total := columns * rows
	lineLength := max(columns, rows)
	return &maskInterpolationWorkspace{
		columns:         columns,
		rows:            rows,
		features:        make([]bool, total),
		tmp:             make([]float64, total),
		distanceScratch: make([]float64, total),
		firstField:      make([]float64, total),
		lastField:       make([]float64, total),
		lineValues:      make([]float64, lineLength),
		lineOutput:      make([]float64, lineLength),
		locations:       make([]int, lineLength),
		boundaries:      make([]float64, lineLength+1),
	}
}

func signedDistanceFieldInto(field []float64, mask *RasterMask, workspace *maskInterpolationWorkspace) {
	clear(workspace.features)
	mask.ForEachRun(func(y int, run MaskRun) {
		rowStart := y * mask.Columns
		for x := run.Start; x < run.End; x++ {
			workspace.features[rowStart+x] = true
		}
	})
	maxDistance := math.Hypot(float64(mask.Columns), float64(mask.Rows))
	euclideanDistanceToFeaturesInto(field, workspace.features, mask.Columns, mask.Rows, maxDistance, workspace)
	for index := range workspace.features {
		workspace.features[index] = !workspace.features[index]
	}
	euclideanDistanceToFeaturesInto(workspace.distanceScratch, workspace.features, mask.Columns, mask.Rows, maxDistance, workspace)
	for index, outside := range workspace.features {
		if outside {
			field[index] = -field[index]
		} else {
			field[index] = workspace.distanceScratch[index]
		}
	}
}

func euclideanDistanceToFeaturesInto(out []float64, features []bool, columns, rows int, maxDistance float64, workspace *maskInterpolationWorkspace) {
	if columns <= 0 || rows <= 0 {
		return
	}
	const largeDistance = 1e12
	for y := 0; y < rows; y++ {
		row := workspace.lineValues[:columns]
		for x := 0; x < columns; x++ {
			if features[y*columns+x] {
				row[x] = 0
			} else {
				row[x] = largeDistance
			}
		}
		transformed := workspace.lineOutput[:columns]
		distanceTransform1DInto(transformed, row, workspace.locations[:columns], workspace.boundaries[:columns+1])
		copy(workspace.tmp[y*columns:(y+1)*columns], transformed)
	}
	for x := 0; x < columns; x++ {
		column := workspace.lineValues[:rows]
		for y := 0; y < rows; y++ {
			column[y] = workspace.tmp[y*columns+x]
		}
		transformed := workspace.lineOutput[:rows]
		distanceTransform1DInto(transformed, column, workspace.locations[:rows], workspace.boundaries[:rows+1])
		for y := 0; y < rows; y++ {
			distance := math.Sqrt(transformed[y])
			if distance > maxDistance {
				distance = maxDistance
			}
			out[y*columns+x] = distance
		}
	}
}

func distanceTransform1DInto(out, values []float64, locations []int, boundaries []float64) {
	n := len(values)
	if n == 0 {
		return
	}
	k := 0
	locations[0] = 0
	boundaries[0] = math.Inf(-1)
	boundaries[1] = math.Inf(1)
	for q := 1; q < n; q++ {
		s := distanceParabolaIntersection(values, q, locations[k])
		for s <= boundaries[k] {
			k--
			s = distanceParabolaIntersection(values, q, locations[k])
		}
		k++
		locations[k] = q
		boundaries[k] = s
		boundaries[k+1] = math.Inf(1)
	}
	k = 0
	for q := 0; q < n; q++ {
		for boundaries[k+1] < float64(q) {
			k++
		}
		delta := float64(q - locations[k])
		out[q] = delta*delta + values[locations[k]]
	}
}

func distanceParabolaIntersection(values []float64, q, p int) float64 {
	qf := float64(q)
	pf := float64(p)
	return ((values[q] + qf*qf) - (values[p] + pf*pf)) / (2*qf - 2*pf)
}

// Dilate grows a mask by one pixel using a 4-connected structuring element.
func Dilate(mask *RasterMask) *RasterMask {
	if mask == nil {
		return nil
	}
	out := NewRasterMask(mask.Columns, mask.Rows)
	for y, runs := range mask.runs {
		expanded := expandRunList(runs, mask.Columns)
		if y > 0 {
			expanded = unionRunLists(expanded, mask.runs[y-1], mask.Columns)
		}
		if y+1 < mask.Rows {
			expanded = unionRunLists(expanded, mask.runs[y+1], mask.Columns)
		}
		out.runs[y] = expanded
	}
	return out
}

func expandRunList(runs []MaskRun, columns int) []MaskRun {
	out := make([]MaskRun, 0, len(runs))
	for _, run := range runs {
		start, end := clampSpan(run.Start-1, run.End+1, columns)
		if start >= end {
			continue
		}
		if len(out) > 0 && start <= out[len(out)-1].End {
			if end > out[len(out)-1].End {
				out[len(out)-1].End = end
			}
			continue
		}
		out = append(out, MaskRun{Start: start, End: end})
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Erode shrinks a mask by one pixel: a pixel survives only if all four of its
// edge neighbors are also set.
func Erode(mask *RasterMask) *RasterMask {
	if mask == nil {
		return nil
	}
	out := NewRasterMask(mask.Columns, mask.Rows)
	for y := 1; y+1 < mask.Rows; y++ {
		horizontal := make([]MaskRun, 0, len(mask.runs[y]))
		for _, run := range mask.runs[y] {
			if run.Start+1 < run.End-1 {
				horizontal = append(horizontal, MaskRun{Start: run.Start + 1, End: run.End - 1})
			}
		}
		horizontal = intersectRunLists(horizontal, mask.runs[y-1])
		out.runs[y] = intersectRunLists(horizontal, mask.runs[y+1])
	}
	return out
}
