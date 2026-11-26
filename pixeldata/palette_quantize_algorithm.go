package pixeldata

import (
	"context"
	"math"
	"sort"
)

type paletteQuantizeBin struct {
	count uint64
	sum   [3]uint64
}

type paletteQuantizeColor struct {
	bin paletteQuantizeBin
	rgb [3]uint8
	key int
}

type paletteQuantizeBox struct {
	colors []paletteQuantizeColor
	axis   int
	score  float64
}

func paletteQuantizeSample(frame []byte, m Metadata, pixel int) [3]uint8 {
	if m.PlanarConfiguration == 1 {
		n := int(m.Rows) * int(m.Columns)
		return [3]uint8{frame[pixel], frame[n+pixel], frame[2*n+pixel]}
	}
	p := 3 * pixel
	return [3]uint8{frame[p], frame[p+1], frame[p+2]}
}

func paletteQuantizeBuild(ctx context.Context, native *NativeFrames, maxColors int) ([][3]uint8, error) {
	// Fixed 5-bit/channel bins bound the histogram independently of input
	// color diversity. Exact colors are retained while they fit the budget.
	bins := make([]paletteQuantizeBin, 1<<15)
	exact := make(map[[3]uint8]struct{}, maxColors+1)
	m := native.Metadata
	for _, frame := range native.Data {
		for y := 0; y < int(m.Rows); y++ {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			for x := 0; x < int(m.Columns); x++ {
				rgb := paletteQuantizeSample(frame, m, y*int(m.Columns)+x)
				if exact != nil {
					exact[rgb] = struct{}{}
					if len(exact) > maxColors {
						exact = nil
					}
				}
				bin := &bins[int(rgb[0]>>3)<<10|int(rgb[1]>>3)<<5|int(rgb[2]>>3)]
				bin.count++
				for c := range rgb {
					bin.sum[c] += uint64(rgb[c])
				}
			}
		}
	}
	if exact != nil {
		palette := make([][3]uint8, 0, len(exact))
		for rgb := range exact {
			palette = append(palette, rgb)
		}
		paletteQuantizeSort(palette)
		return palette, nil
	}
	colors := make([]paletteQuantizeColor, 0)
	for key, bin := range bins {
		if bin.count == 0 {
			continue
		}
		color := paletteQuantizeColor{bin: bin, key: key}
		for c := range color.rgb {
			color.rgb[c] = uint8((bin.sum[c] + bin.count/2) / bin.count)
		}
		colors = append(colors, color)
	}
	boxes := []paletteQuantizeBox{paletteQuantizeMeasure(colors)}
	for len(boxes) < maxColors {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		chosen := -1
		for i, box := range boxes {
			if len(box.colors) > 1 && (chosen < 0 || box.score > boxes[chosen].score) {
				chosen = i
			}
		}
		if chosen < 0 {
			break
		}
		box := boxes[chosen]
		sort.Slice(box.colors, func(i, j int) bool {
			if box.colors[i].rgb[box.axis] != box.colors[j].rgb[box.axis] {
				return box.colors[i].rgb[box.axis] < box.colors[j].rgb[box.axis]
			}
			return box.colors[i].key < box.colors[j].key
		})
		var total, accumulated uint64
		for _, color := range box.colors {
			total += color.bin.count
		}
		cut := 1
		for cut < len(box.colors)-1 {
			accumulated += box.colors[cut-1].bin.count
			if accumulated >= (total+1)/2 {
				break
			}
			cut++
		}
		boxes[chosen] = paletteQuantizeMeasure(box.colors[:cut])
		boxes = append(boxes, paletteQuantizeMeasure(box.colors[cut:]))
	}
	palette := make([][3]uint8, 0, len(boxes))
	for _, box := range boxes {
		var count uint64
		var sums [3]uint64
		for _, color := range box.colors {
			count += color.bin.count
			for c := range sums {
				sums[c] += color.bin.sum[c]
			}
		}
		var rgb [3]uint8
		for c := range rgb {
			rgb[c] = uint8((sums[c] + count/2) / count)
		}
		palette = append(palette, rgb)
	}
	paletteQuantizeSort(palette)
	// Different boxes can round to the same representative color.
	unique := palette[:0]
	for _, rgb := range palette {
		if len(unique) == 0 || unique[len(unique)-1] != rgb {
			unique = append(unique, rgb)
		}
	}
	return unique, nil
}

func paletteQuantizeSort(palette [][3]uint8) {
	sort.Slice(palette, func(i, j int) bool {
		for c := 0; c < 3; c++ {
			if palette[i][c] != palette[j][c] {
				return palette[i][c] < palette[j][c]
			}
		}
		return false
	})
}

func paletteQuantizeMeasure(colors []paletteQuantizeColor) paletteQuantizeBox {
	box := paletteQuantizeBox{colors: colors}
	var weight float64
	var sums, squares [3]float64
	for _, color := range colors {
		n := float64(color.bin.count)
		weight += n
		for c := range sums {
			v := float64(color.rgb[c])
			sums[c] += n * v
			squares[c] += n * v * v
		}
	}
	var largest float64
	for c := range sums {
		variance := math.Max(0, squares[c]-sums[c]*sums[c]/weight)
		box.score += variance
		if variance > largest {
			largest, box.axis = variance, c
		}
	}
	return box
}

func paletteQuantizeNearest(rgb [3]float64, palette [][3]uint8) int {
	best, distance := 0, math.Inf(1)
	for i, candidate := range palette {
		var squared float64
		for c := range rgb {
			d := rgb[c] - float64(candidate[c])
			squared += d * d
		}
		if squared < distance {
			best, distance = i, squared
		}
	}
	return best
}

func paletteQuantizeMap(ctx context.Context, native *NativeFrames, palette [][3]uint8, dither bool) ([]byte, float64, uint8, error) {
	m := native.Metadata
	width, height := int(m.Columns), int(m.Rows)
	indices := make([]byte, width*height*len(native.Data), width*height*len(native.Data)+1)
	var squaredError float64
	var maxError uint8
	for f, frame := range native.Data {
		var current, next [][3]float64
		if dither {
			current, next = make([][3]float64, width+2), make([][3]float64, width+2)
		}
		for y := 0; y < height; y++ {
			if err := ctx.Err(); err != nil {
				return nil, 0, 0, err
			}
			direction, start := 1, 0
			if dither && y%2 == 1 {
				direction, start = -1, width-1
			}
			for x := start; x >= 0 && x < width; x += direction {
				original := paletteQuantizeSample(frame, m, y*width+x)
				var corrected [3]float64
				for c := range corrected {
					corrected[c] = float64(original[c])
					if dither {
						corrected[c] = math.Max(0, math.Min(255, corrected[c]+current[x+1][c]))
					}
				}
				index := paletteQuantizeNearest(corrected, palette)
				indices[f*width*height+y*width+x] = byte(index)
				for c := range corrected {
					d := int(original[c]) - int(palette[index][c])
					squaredError += float64(d * d)
					absolute := d
					if absolute < 0 {
						absolute = -absolute
					}
					if uint8(absolute) > maxError {
						maxError = uint8(absolute)
					}
					if dither {
						error := corrected[c] - float64(palette[index][c])
						current[x+1+direction][c] += error * 7 / 16
						next[x+1-direction][c] += error * 3 / 16
						next[x+1][c] += error * 5 / 16
						next[x+1+direction][c] += error / 16
					}
				}
			}
			if dither {
				current, next = next, current
				clear(next)
			}
		}
	}
	return indices, squaredError, maxError, nil
}
