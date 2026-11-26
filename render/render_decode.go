package render

import (
	"context"
	"fmt"
	"image"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	dicomframe "github.com/ThalesMMS/dicom-go/pixeldata/frame"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type decodedFrame struct {
	rows        int
	cols        int
	photometric string
	grayscale   []float32
	grayscale64 []float64
	rgba        []byte
	// levelIndex and levels are the optional compact form of grayscale kept
	// by RenderCache: levels[levelIndex[i]] == grayscale[i] for every pixel.
	// One level per stored value lets a window change map each level once and
	// gather the output instead of evaluating the VOI function per pixel
	// (#1222). Both are nil when the frame has no compact form.
	levelIndex []uint16
	levels     []float32
}

// maxDecodedLevels bounds the level table of a compact frame to what a
// uint16 index addresses.
const maxDecodedLevels = 1 << 16

// decodeCacheFrame selects a binary64 plane only for callers that explicitly
// require it. Volume decode keeps its separate existing float32 contract.
func decodeCacheFrame(slice *Frame) (*decodedFrame, error) {
	var err error
	slice, err = loadFrameContext(context.Background(), slice)
	if err != nil {
		return nil, err
	}
	if slice != nil && !slice.PreserveModalityPrecision && slice.Metadata.SamplesPerPixel == 1 {
		return decodeCompactGrayscaleFrame(slice)
	}
	if slice == nil || !slice.PreserveModalityPrecision || slice.Metadata.SamplesPerPixel != 1 {
		return decodeSliceFrame(slice)
	}
	metadata := effectiveFrameMetadata(slice)
	rows, cols := int(metadata.Rows), int(metadata.Columns)
	frame, err := grayscaleDisplayFrame(slice, metadata, normalizedPhotometric(metadata.PhotometricInterpretation), rows, cols)
	if err != nil {
		return nil, err
	}
	values, err := display.DecodeModality(frame)
	if err != nil {
		return nil, err
	}
	return &decodedFrame{rows: rows, cols: cols, photometric: normalizedPhotometric(metadata.PhotometricInterpretation), grayscale64: values}, nil
}

// decodeCompactGrayscaleFrame decodes a cached 8- or 16-bit grayscale frame
// straight to its compact form: a 2-byte stored-level index and the float32
// modality value of each level (#1612). It keeps no float32 plane, so the
// cache holds half the bytes, and every window maps one value per level. A
// frame whose stored range is too wide decodes like decodeSliceFrame.
func decodeCompactGrayscaleFrame(slice *Frame) (*decodedFrame, error) {
	metadata := effectiveFrameMetadata(slice)
	rows, cols := int(metadata.Rows), int(metadata.Columns)
	if rows <= 0 || cols <= 0 || (metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16) {
		return decodeSliceFrame(slice)
	}
	photometric := normalizedPhotometric(metadata.PhotometricInterpretation)
	frame, err := grayscaleDisplayFrame(slice, metadata, photometric, rows, cols)
	if err != nil {
		return nil, err
	}
	index, levels, ok, err := display.DecodeModalityLevels(frame)
	if err != nil {
		return nil, err
	}
	if !ok {
		return decodeSliceFrame(slice)
	}
	return &decodedFrame{rows: rows, cols: cols, photometric: photometric, levelIndex: index, levels: levels}, nil
}

func decodeSliceFrame(slice *Frame) (*decodedFrame, error) {
	var err error
	slice, err = loadFrameContext(context.Background(), slice)
	if err != nil {
		return nil, err
	}
	if slice == nil {
		return nil, fmt.Errorf("render: nil slice")
	}
	metadata := effectiveFrameMetadata(slice)
	rows := int(metadata.Rows)
	cols := int(metadata.Columns)
	if rows <= 0 || cols <= 0 {
		return nil, fmt.Errorf("%w: Rows=%d Columns=%d", dicomframe.ErrInvalidFrameMetadata, metadata.Rows, metadata.Columns)
	}
	photometric := normalizedPhotometric(metadata.PhotometricInterpretation)
	switch metadata.SamplesPerPixel {
	case 1:
		return decodeGrayscaleSlice(slice, metadata, photometric, rows, cols)
	case 3:
		return decodeColorSlice(slice, metadata, photometric, rows, cols)
	default:
		return nil, fmt.Errorf("%w: SamplesPerPixel=%d", dicomframe.ErrUnsupportedSamplesPerPixel, metadata.SamplesPerPixel)
	}
}

func decodeGrayscaleSlice(slice *Frame, metadata pixeldata.Metadata, photometric string, rows, cols int) (*decodedFrame, error) {
	frame, err := grayscaleDisplayFrame(slice, metadata, photometric, rows, cols)
	if err != nil {
		return nil, err
	}
	modality, err := display.DecodeModalityFloat32(frame)
	if err != nil {
		return nil, err
	}
	return &decodedFrame{rows: rows, cols: cols, photometric: photometric, grayscale: modality}, nil
}

// indexDecodedLevels builds decoded.levelIndex and decoded.levels from the
// cached modality plane alone, so it needs neither the stored pixel bytes nor
// a second stored-sample pass. The linear modality transform is undone per
// pixel (level = round((v-intercept)/slope)) and every level keeps the float32
// modality value of the pixels that reach it; a level reached by two
// different float32 values, or a level range wider than 16 bits or than the
// pixel count, abandons the index. levels[levelIndex[i]] therefore equals
// grayscale[i] bit for bit, which keeps a table-driven window identical to
// mapping grayscale pixel by pixel.
func indexDecodedLevels(decoded *decodedFrame, slice *Frame) {
	if decoded == nil || slice == nil || len(decoded.grayscale) == 0 || len(decoded.grayscale) != decoded.rows*decoded.cols {
		return
	}
	slope, intercept := sanitizedRescale(slice)
	inverse := 1 / slope
	level := func(value float32) float64 {
		return math.Round((float64(value) - intercept) * inverse)
	}
	lowest, highest := decoded.grayscale[0], decoded.grayscale[0]
	for _, value := range decoded.grayscale[1:] {
		if value < lowest {
			lowest = value
		} else if value > highest {
			highest = value
		}
	}
	first, last := level(lowest), level(highest)
	if first > last {
		first, last = last, first
	}
	span := last - first + 1
	if math.IsNaN(span) || span > maxDecodedLevels || span > float64(len(decoded.grayscale)) {
		return
	}
	levels := make([]float32, int(span))
	seen := make([]bool, len(levels))
	index := make([]uint16, len(decoded.grayscale))
	for i, value := range decoded.grayscale {
		offset := level(value) - first
		if !(offset >= 0 && offset < span) {
			return
		}
		k := int(offset)
		if seen[k] {
			if math.Float32bits(levels[k]) != math.Float32bits(value) {
				return
			}
		} else {
			seen[k] = true
			levels[k] = value
		}
		index[i] = uint16(k)
	}
	decoded.levelIndex = index
	decoded.levels = levels
}

// sanitizedRescale is the rescale the grayscale decode applies: a zero or
// non-finite slope is 1 and a non-finite intercept is 0.
func sanitizedRescale(slice *Frame) (slope, intercept float64) {
	slope = slice.Rescale.Slope
	if slope == 0 || math.IsNaN(slope) || math.IsInf(slope, 0) {
		slope = 1
	}
	intercept = slice.Rescale.Intercept
	if math.IsNaN(intercept) || math.IsInf(intercept, 0) {
		intercept = 0
	}
	return slope, intercept
}

func decodeGrayscaleSliceInto(destination []float32, slice *Frame) error {
	if slice == nil {
		return fmt.Errorf("render: nil slice")
	}
	metadata := effectiveFrameMetadata(slice)
	rows := int(metadata.Rows)
	cols := int(metadata.Columns)
	if rows <= 0 || cols <= 0 {
		return fmt.Errorf("%w: Rows=%d Columns=%d", dicomframe.ErrInvalidFrameMetadata, metadata.Rows, metadata.Columns)
	}
	frame, err := grayscaleDisplayFrame(slice, metadata, normalizedPhotometric(metadata.PhotometricInterpretation), rows, cols)
	if err != nil {
		return err
	}
	return display.DecodeModalityFloat32Into(destination, frame)
}

func grayscaleDisplayFrame(slice *Frame, metadata pixeldata.Metadata, photometric string, rows, cols int) (display.Frame, error) {
	// Legacy headless scalar/Gray16 callers also accept deferred stacks. Volume
	// workers resolve with their context first, so this does not load twice or
	// replace the worker's cancellation with a background context.
	var err error
	slice, err = loadFrameContext(context.Background(), slice)
	if err != nil {
		return display.Frame{}, err
	}
	if metadata.BitsAllocated != 8 && metadata.BitsAllocated != 16 && metadata.BitsAllocated != 32 {
		return display.Frame{}, fmt.Errorf("%w: BitsAllocated=%d", dicomframe.ErrUnsupportedBitsAllocated, metadata.BitsAllocated)
	}
	if photometric != "MONOCHROME1" && photometric != "MONOCHROME2" {
		return display.Frame{}, fmt.Errorf("%w: %q", dicomframe.ErrUnsupportedPhotometricInterpretation, metadata.PhotometricInterpretation)
	}
	bytesPerSample := int(metadata.BitsAllocated / 8)
	if expected := rows * cols * bytesPerSample; len(slice.PixelBytes) < expected {
		return display.Frame{}, fmt.Errorf("%w: got %d bytes, want %d", dicomframe.ErrPixelDataTooShort, len(slice.PixelBytes), expected)
	}
	slope, intercept := sanitizedRescale(slice)
	// Decode stored pixels to modality values through the dicom-go display
	// pipeline so this cached decode shares the library's stored-pixel and
	// modality transform logic. The viewer keeps only the slope/intercept
	// sanitization for non-finite rescale metadata.
	return display.Frame{
		Rows:    rows,
		Columns: cols,
		Pixels:  slice.PixelBytes,
		Format: display.PixelFormat{
			BitsAllocated: int(metadata.BitsAllocated),
			BitsStored:    int(metadata.BitsStored),
			HighBit:       int(metadata.HighBit),
			Signed:        metadata.PixelRepresentation != 0,
			ByteOrder:     slice.ByteOrder,
		},
		Modality: display.ModalityLUT{Slope: slope, Intercept: intercept},
	}, nil
}

func decodeColorSlice(slice *Frame, metadata pixeldata.Metadata, photometric string, rows, cols int) (*decodedFrame, error) {
	if !slice.Encapsulated {
		if err := display.ValidateColorPhotometric(metadata.PhotometricInterpretation, slice.TransferSyntaxUID); err != nil {
			return nil, err
		}
	}
	img, err := dicomframe.RenderColor(slice.PixelBytes, metadata, slice.ByteOrder)
	if err != nil {
		return nil, err
	}
	return &decodedFrame{rows: rows, cols: cols, photometric: photometric, rgba: img.Pix}, nil
}

func effectiveFrameMetadata(slice *Frame) pixeldata.Metadata {
	if slice == nil {
		return pixeldata.Metadata{}
	}
	metadata := slice.Metadata
	if !slice.Encapsulated || metadata.SamplesPerPixel != 3 {
		return metadata
	}
	uid := transfer.NormalizeUID(slice.TransferSyntaxUID)
	switch uid {
	case transfer.RLELossless.UID:
		// RLE decodes color-by-plane segments to interleaved samples.
		metadata.PlanarConfiguration = 0
		metadata.PlanarConfigurationPresent = true
	case transfer.JPEGBaseline.UID, transfer.JPEGExtended.UID:
		switch normalizedPhotometric(metadata.PhotometricInterpretation) {
		case "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420":
			metadata.PhotometricInterpretation = "RGB"
			metadata.PlanarConfiguration = 0
			metadata.PlanarConfigurationPresent = true
		}
	default:
		if transfer.IsJPEG2000TransferSyntax(uid) || transfer.IsJPEGXLTransferSyntax(uid) {
			switch normalizedPhotometric(metadata.PhotometricInterpretation) {
			case "YBR_FULL", "YBR_FULL_422", "YBR_RCT", "YBR_ICT":
				metadata.PhotometricInterpretation = "RGB"
				metadata.PlanarConfiguration = 0
				metadata.PlanarConfigurationPresent = true
			}
		}
	}
	return metadata
}

func renderDecodedFrame(decoded *decodedFrame, window WindowLevel) (image.Image, error) {
	if decoded == nil {
		return nil, fmt.Errorf("render: nil decoded frame")
	}
	if decoded.rgba != nil {
		out := image.NewRGBA(image.Rect(0, 0, decoded.cols, decoded.rows))
		copy(out.Pix, decoded.rgba)
		return out, nil
	}
	if !decoded.hasGrayscale() {
		return nil, fmt.Errorf("%w: missing grayscale pixels", dicomframe.ErrInvalidFrameMetadata)
	}
	invert := decoded.photometric == "MONOCHROME1"
	mapper := display.NewVOIByteMapper(display.VOILUT{Center: window.Center, Width: window.Width, Function: window.Function, LUT: window.LUT})
	if decoded.grayscale64 != nil {
		return decoded.mapGray64(func(value float64) uint16 {
			gray := mapper.Byte(value)
			if invert {
				gray = 255 - gray
			}
			return uint16(gray)
		}, false), nil
	}

	return decoded.mapGray(func(modality float32) uint8 {
		gray := mapper.Byte(float64(modality))
		if invert {
			gray = 255 - gray
		}
		return gray
	}), nil
}

// mapGray returns the 8-bit image whose pixel i is toGray(grayscale[i]).
// toGray must depend on its argument alone. A compact frame evaluates it once
// per level and gathers the output; other frames evaluate it per pixel. Both
// spread rows over the shared row workers and give identical images.
func (d *decodedFrame) mapGray(toGray func(float32) uint8) *image.Gray {
	out := image.NewGray(image.Rect(0, 0, d.cols, d.rows))
	if d.hasLevelIndex() {
		// A full-width table lets the uint16 index skip bounds checks.
		table := new([maxDecodedLevels]uint8)
		for level, modality := range d.levels {
			table[level] = toGray(modality)
		}
		gatherRows(d.rows, len(d.levelIndex), func(y int) {
			start := y * d.cols
			row := out.Pix[start : start+d.cols]
			for i, level := range d.levelIndex[start : start+d.cols] {
				row[i] = table[level]
			}
		})
		return out
	}
	parallelRows(d.rows, func(y int) {
		start := y * d.cols
		row := out.Pix[start : start+d.cols]
		for i, modality := range d.grayscale[start : start+d.cols] {
			row[i] = toGray(modality)
		}
	})
	return out
}

// mapGray16 is mapGray with 16-bit big-endian output samples.
func (d *decodedFrame) mapGray16(toGray16 func(float32) uint16) *image.Gray16 {
	out := image.NewGray16(image.Rect(0, 0, d.cols, d.rows))
	store := func(offset int, p uint16) {
		out.Pix[2*offset], out.Pix[2*offset+1] = uint8(p>>8), uint8(p)
	}
	if d.hasLevelIndex() {
		table := new([maxDecodedLevels]uint16)
		for level, modality := range d.levels {
			table[level] = toGray16(modality)
		}
		gatherRows(d.rows, len(d.levelIndex), func(y int) {
			start := y * d.cols
			for i, level := range d.levelIndex[start : start+d.cols] {
				store(start+i, table[level])
			}
		})
		return out
	}
	parallelRows(d.rows, func(y int) {
		start := y * d.cols
		for i, modality := range d.grayscale[start : start+d.cols] {
			store(start+i, toGray16(modality))
		}
	})
	return out
}

// The precision plane has no float32 level table. Its 8N retained bytes are
// fully charged to the same bounded decoded store; only the requested output
// is allocated while mapping. Regular images retain their existing compact path.
func (d *decodedFrame) mapGray64(mapper func(float64) uint16, wide bool) image.Image {
	if wide {
		out := image.NewGray16(image.Rect(0, 0, d.cols, d.rows))
		parallelRows(d.rows, func(y int) {
			start := y * d.cols
			for i, v := range d.grayscale64[start : start+d.cols] {
				value := mapper(v)
				offset := 2 * (start + i)
				out.Pix[offset] = byte(value >> 8)
				out.Pix[offset+1] = byte(value)
			}
		})
		return out
	}
	out := image.NewGray(image.Rect(0, 0, d.cols, d.rows))
	parallelRows(d.rows, func(y int) {
		start := y * d.cols
		for i, v := range d.grayscale64[start : start+d.cols] {
			out.Pix[start+i] = byte(mapper(v))
		}
	})
	return out
}

// parallelGatherPixels is the frame size from which a table gather is spread
// over the row workers. A gather costs about a nanosecond per pixel, so waking
// workers for a CT-sized slice costs more than it saves.
const parallelGatherPixels = 1 << 20

// gatherRows runs a table gather on the calling goroutine for frames below
// parallelGatherPixels and on the shared row workers above it.
func gatherRows(rows, pixels int, gatherRow func(int)) {
	if pixels < parallelGatherPixels {
		for y := 0; y < rows; y++ {
			gatherRow(y)
		}
		return
	}
	parallelRows(rows, gatherRow)
}

// hasLevelIndex reports whether d carries its compact form. A frame decoded
// compact (#1612) has no float32 plane; one indexed later keeps both.
func (d *decodedFrame) hasLevelIndex() bool {
	return d != nil && d.levels != nil && len(d.levelIndex) == d.rows*d.cols && (d.grayscale == nil || len(d.levelIndex) == len(d.grayscale))
}

// hasGrayscale reports whether d holds grayscale modality values in any form.
func (d *decodedFrame) hasGrayscale() bool {
	return d != nil && (d.grayscale != nil || d.grayscale64 != nil || d.hasLevelIndex())
}

func (d *decodedFrame) cacheBytes() int64 {
	if d == nil {
		return 0
	}
	return int64(len(d.grayscale64)*8 + len(d.grayscale)*4 + len(d.rgba) + len(d.levelIndex)*2 + len(d.levels)*4)
}

func normalizedPhotometric(value string) string {
	return strings.Trim(strings.ToUpper(value), " \x00")
}
