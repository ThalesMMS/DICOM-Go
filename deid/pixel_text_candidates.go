package deid

import (
	"context"
	"errors"
	"image"
	"image/color"
	"sort"
)

const (
	maxTextCandidatePixels = 1 << 20
	maxTextCandidateGlyphs = 1024
	maxTextCandidates      = 64
	textCandidateContrast  = 56
)

// ErrPixelTextReviewInconclusive indicates a frame that is outside the bounded
// candidate profile or too cluttered to return a complete list of its proposals.
var ErrPixelTextReviewInconclusive = errors.New("deid: pixel text candidate scan is inconclusive")

// PixelTextCandidates proposes text-like groups in the upper and lower image
// quarters. Coordinates are relative to the frame's minimum point. The scan
// returns rectangles only, leaves the frame untouched, and offers no statement
// about identifying information. An empty result still requires visual review.
func PixelTextCandidates(ctx context.Context, frame image.Image) ([]image.Rectangle, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if frame == nil {
		return nil, ErrPixelTextReviewInconclusive
	}
	bounds := frame.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 24 || height < 32 || width > maxTextCandidatePixels/height {
		return nil, ErrPixelTextReviewInconclusive
	}
	bandHeight := height / 4
	var candidates []image.Rectangle
	for _, offset := range []int{0, height - bandHeight} {
		band := image.Rect(0, offset, width, offset+bandHeight)
		boxes, err := textBandCandidates(ctx, frame, band)
		if err != nil {
			return nil, err
		}
		if len(boxes) > maxTextCandidates-len(candidates) {
			return nil, ErrPixelTextReviewInconclusive
		}
		candidates = append(candidates, boxes...)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return candidates, nil
}

func textBandCandidates(ctx context.Context, frame image.Image, band image.Rectangle) ([]image.Rectangle, error) {
	width, height := band.Dx(), band.Dy()
	luminance := make([]byte, width*height)
	var histogram [256]int
	origin := frame.Bounds().Min.Add(band.Min)
	for y := range height {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := range width {
			value := color.GrayModel.Convert(frame.At(origin.X+x, origin.Y+y)).(color.Gray).Y
			luminance[y*width+x] = value
			histogram[value]++
		}
	}
	median := 0
	count := 0
	for value, frequency := range histogram {
		count += frequency
		if count > len(luminance)/2 {
			median = value
			break
		}
	}
	var boxes []image.Rectangle
	for _, direction := range []int{1, -1} {
		threshold := direction*median + textCandidateContrast
		if threshold > max(255*direction, 0) {
			continue
		}
		glyphs, err := textGlyphBoxes(ctx, luminance, width, height, direction, threshold)
		if err != nil {
			return nil, err
		}
		for _, run := range textGlyphRuns(glyphs) {
			boxes = append(boxes, run.Inset(-2).Add(band.Min).Intersect(band))
		}
	}
	sort.Slice(boxes, func(i, j int) bool {
		if boxes[i].Min.Y == boxes[j].Min.Y {
			return boxes[i].Min.X < boxes[j].Min.X
		}
		return boxes[i].Min.Y < boxes[j].Min.Y
	})
	return boxes, nil
}

func textGlyphBoxes(ctx context.Context, luminance []byte, width, height, direction, threshold int) ([]image.Rectangle, error) {
	seen := make([]bool, len(luminance))
	var glyphs []image.Rectangle
	queue := make([]int, 0, 64)
	for index, value := range luminance {
		if index&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if seen[index] || direction*int(value) < threshold {
			continue
		}
		queue = append(queue[:0], index)
		seen[index] = true
		x, y := index%width, index/width
		box := image.Rect(x, y, x+1, y+1)
		for head := 0; head < len(queue); head++ {
			if head&1023 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			x, y := queue[head]%width, queue[head]/width
			box.Min.X, box.Min.Y = min(box.Min.X, x), min(box.Min.Y, y)
			box.Max.X, box.Max.Y = max(box.Max.X, x+1), max(box.Max.Y, y+1)
			for ny := max(0, y-1); ny <= min(height-1, y+1); ny++ {
				for nx := max(0, x-1); nx <= min(width-1, x+1); nx++ {
					next := ny*width + nx
					if !seen[next] && direction*int(luminance[next]) >= threshold {
						seen[next] = true
						queue = append(queue, next)
					}
				}
			}
		}
		if box.Dy() < 4 || box.Dy() > min(height, 64) || box.Dx() < 2 || box.Dx() > box.Dy()*2 || len(queue) < 6 {
			continue
		}
		if len(glyphs) == maxTextCandidateGlyphs {
			return nil, ErrPixelTextReviewInconclusive
		}
		glyphs = append(glyphs, box)
	}
	return glyphs, nil
}

// Glyphs that share a baseline and are separated by text-sized gaps form one
// review rectangle. The count excludes isolated components such as markers.
func textGlyphRuns(glyphs []image.Rectangle) []image.Rectangle {
	type textRun struct {
		box, last image.Rectangle
		count     int
	}
	sort.Slice(glyphs, func(i, j int) bool {
		if glyphs[i].Min.X == glyphs[j].Min.X {
			return glyphs[i].Min.Y < glyphs[j].Min.Y
		}
		return glyphs[i].Min.X < glyphs[j].Min.X
	})
	var runs []textRun
	for _, glyph := range glyphs {
		match := -1
		bestGap := 1 << 30
		for i, run := range runs {
			gap := glyph.Min.X - run.last.Max.X
			tall := max(glyph.Dy(), run.last.Dy())
			short := min(glyph.Dy(), run.last.Dy())
			if gap >= 0 && gap <= tall*2 && gap < bestGap && tall <= short*2 &&
				min(glyph.Max.Y, run.last.Max.Y)-max(glyph.Min.Y, run.last.Min.Y) >= short/2 {
				match, bestGap = i, gap
			}
		}
		if match < 0 {
			runs = append(runs, textRun{box: glyph, last: glyph, count: 1})
			continue
		}
		run := &runs[match]
		run.box, run.last = run.box.Union(glyph), glyph
		run.count++
	}
	var boxes []image.Rectangle
	for _, run := range runs {
		if run.count >= 3 {
			boxes = append(boxes, run.box)
		}
	}
	return boxes
}
