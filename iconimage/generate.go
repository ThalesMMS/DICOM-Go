package iconimage

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"image/color"
	"image/color/palette"
	"image/draw"
	"io"
	"math"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/builtin"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// WindowMode selects the grayscale VOI used by Generate.
type WindowMode uint8

const (
	// WindowFromSource uses the frame's VOI LUT/window, or automatic min/max
	// when neither is present. This is the default.
	WindowFromSource WindowMode = iota
	// WindowAutomatic ignores the stored VOI and uses usable modality values.
	WindowAutomatic
	// WindowMinMax maps Min to black and Max to white in the modality domain.
	WindowMinMax
)

var (
	ErrInvalidOptions    = errors.New("iconimage: invalid generation options")
	ErrUnsupportedSource = errors.New("iconimage: unsupported source representation")
	ErrNoUsablePixels    = errors.New("iconimage: no pixels outside padding")
)

// GenerateOptions controls the generation of a native 8-bit icon.
type GenerateOptions struct {
	// Width and Height independently default to 64; each must be 1..1024.
	Width, Height int
	// FrameIndex selects a zero-based frame. Nil uses a valid one-based
	// Representative Frame Number, otherwise Number of Frames / 3.
	FrameIndex *int
	Window     WindowMode
	Min, Max   float64
	// IgnorePadding excludes declared Pixel Padding Value/Range Limit from
	// grayscale statistics and paints those samples black after presentation.
	IgnorePadding bool
	// Padding overrides the declared padding with a stored-value range. Its
	// presence implies IgnorePadding. Padding does not apply to color indices.
	Padding *pixeldata.PixelPadding
	// Monochrome reduces color to luminance. Otherwise color becomes PALETTE
	// COLOR using Go's fixed Plan9 palette; grayscale always becomes MONOCHROME2.
	Monochrome bool
	// Source is the same seekable input used to read deferred Pixel Data.
	// It must stay open and unchanged until Generate returns; materialized
	// Pixel Data needs no Source. Generate does not close file or Source.
	Source io.ReaderAt
	// MaxFragments bounds the frame index; zero selects 100000.
	MaxFragments int
	// Registry is used for compressed source pixels; nil selects built-in codecs.
	Registry pixeldata.Registry
	// Limits bound source pixels across all frames, bytes and frame count,
	// including native inputs. Zero fields use DefaultGenerateLimits.
	Limits pixeldata.DecompressLimits
	// ByteOrder selects the order of the returned US and palette OW values.
	// Nil preserves file.Dataset.ValueByteOrder(). Set it to the destination
	// object's order when embedding in a different dataset (e.g. a DICOMDIR).
	ByteOrder binary.ByteOrder
}

// DefaultGenerateLimits bounds generation before frame views, decompression
// and source-sized render allocations. Only the selected frame is decoded.
func DefaultGenerateLimits() pixeldata.DecompressLimits {
	return pixeldata.DecompressLimits{MaxFrames: 1024, MaxPixels: 16 << 20,
		MaxNativeBytes: 64 << 20, MaxInputBytes: 64 << 20, MaxExpansionRatio: 1024}
}

// Generate returns an owned Icon Image Sequence (0088,0200) with one item,
// unsigned 8-bit MONOCHROME2 or PALETTE COLOR pixels and Pixel Aspect Ratio
// 1:1 (PS3.3 F.7). It fits the source's physical aspect into a black canvas.
// It borrows source values without changing them; the caller must keep file
// alive and must not mutate it during the call. Numeric raw values in the
// result use options.ByteOrder (or the source dataset's order), because a
// core.Element itself has no byte-order property.
//
// Context is checked between bounded library stages and during our pixel,
// resize and palette loops. Codec cancellation follows FrameReader's
// context-aware codec contract. No partial element is returned on failure.
func Generate(ctx context.Context, file *object.File, options GenerateOptions) (core.Element, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return core.Element{}, err
	}
	if file == nil || file.Dataset == nil {
		return core.Element{}, fmt.Errorf("%w: nil file", ErrUnsupportedSource)
	}
	if _, partial := file.PartialRead(); partial {
		return core.Element{}, object.ErrPartialFile
	}
	opts, err := normalizeGenerateOptions(options)
	if err != nil {
		return core.Element{}, err
	}
	order := opts.ByteOrder
	if order == nil {
		order = file.Dataset.ValueByteOrder()
	}
	if order != binary.LittleEndian && order != binary.BigEndian {
		return core.Element{}, fmt.Errorf("%w: ByteOrder", ErrInvalidOptions)
	}
	// Private facades keep the display/functional-group accessors' lazy caches
	// request-local while continuing to borrow the original pixel buffers.
	dataset := object.FromElements(file.Dataset.Elements(), nil)
	dataset.SetValueByteOrder(file.Dataset.ValueByteOrder())
	metadata, err := pixeldata.ExtractMetadata(dataset)
	if err != nil {
		return core.Element{}, err
	}
	if err := preflightGenerate(dataset, metadata, opts.Limits); err != nil {
		return core.Element{}, err
	}
	photo := strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	if photo != "MONOCHROME1" && photo != "MONOCHROME2" && opts.Window != WindowFromSource {
		return core.Element{}, fmt.Errorf("%w: window requires monochrome source", ErrInvalidOptions)
	}
	if err := preflightInput(ctx, file, metadata, opts); err != nil {
		return core.Element{}, err
	}
	index, present := RepresentativeFrameIndex(dataset, metadata.NumberOfFrames)
	if !present {
		index = metadata.NumberOfFrames / 3
	}
	if opts.FrameIndex != nil {
		index = *opts.FrameIndex
	}
	if index < 0 || index >= metadata.NumberOfFrames {
		return core.Element{}, fmt.Errorf("%w: FrameIndex", ErrInvalidOptions)
	}
	registry := opts.Registry
	if registry == nil {
		registry, err = builtin.NewRegistry()
		if err != nil {
			return core.Element{}, err
		}
	}
	var source io.ReaderAt
	if opts.Source != nil {
		source = contextIconSource{ctx: ctx, source: opts.Source}
	}
	reader, err := pixeldata.NewFrameReader(ctx, file, source, pixeldata.FrameReaderOptions{
		Registry: registry, Limits: opts.Limits, MaxFrames: opts.Limits.MaxFrames, MaxFragments: opts.MaxFragments,
	})
	if err != nil {
		return core.Element{}, err
	}
	defer reader.Close()
	data, metadata, err := reader.Frame(ctx, index)
	if err != nil {
		return core.Element{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.Element{}, err
	}
	// Frame supplies effective decoded layout and native Little Endian bytes;
	// display transforms remain attached to the original selected frame.
	if err := preflightGenerate(dataset, metadata, opts.Limits); err != nil {
		return core.Element{}, err
	}
	photo = strings.ToUpper(strings.TrimSpace(metadata.PhotometricInterpretation))
	mono := photo == "MONOCHROME1" || photo == "MONOCHROME2"
	if !mono && opts.Window != WindowFromSource {
		return core.Element{}, fmt.Errorf("%w: window requires monochrome source", ErrInvalidOptions)
	}
	var img image.Image
	if mono {
		img, err = generateGray(ctx, dataset, metadata, data, index, opts)
	} else {
		var lut *display.PaletteColorLUT
		if photo == "PALETTE COLOR" {
			lut, err = display.PaletteFromObject(dataset)
			if err != nil {
				return core.Element{}, err
			}
		}
		format := metadata.SampleFormat(binary.LittleEndian)
		img, err = display.RenderColor(display.ColorFrame{Rows: int(metadata.Rows), Columns: int(metadata.Columns),
			Pixels: data, Photometric: photo, SamplesPerPixel: int(metadata.SamplesPerPixel),
			PlanarConfiguration: int(metadata.PlanarConfiguration), Format: display.PixelFormat{
				BitsAllocated: format.BitsAllocated, BitsStored: format.BitsStored, HighBit: format.HighBit,
				Signed: format.Signed, ByteOrder: format.ByteOrder}, Palette: lut})
	}
	if err != nil {
		return core.Element{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.Element{}, err
	}
	aspect, err := generateAspect(dataset, index, metadata)
	if err != nil {
		return core.Element{}, err
	}
	gray := mono || opts.Monochrome
	scaled, err := fitIcon(ctx, img, opts.Width, opts.Height, aspect, gray)
	if err != nil {
		return core.Element{}, err
	}
	var pixels []byte
	var colors color.Palette
	if gray {
		pixels = scaled.(*image.Gray).Pix
	} else {
		colors = palette.Plan9
		out := image.NewPaletted(scaled.Bounds(), colors)
		for y := 0; y < opts.Height; y++ {
			if err := ctx.Err(); err != nil {
				return core.Element{}, err
			}
			r := image.Rect(0, y, opts.Width, y+1)
			draw.Draw(out, r, scaled, r.Min, draw.Src)
		}
		pixels = out.Pix
	}
	if err := ctx.Err(); err != nil {
		return core.Element{}, err
	}
	return generatedSequence(pixels, opts.Width, opts.Height, colors, order), nil
}

func normalizeGenerateOptions(opts GenerateOptions) (GenerateOptions, error) {
	if opts.Width == 0 {
		opts.Width = 64
	}
	if opts.Height == 0 {
		opts.Height = 64
	}
	if opts.Width < 1 || opts.Width > 1024 || opts.Height < 1 || opts.Height > 1024 || opts.Window > WindowMinMax || opts.MaxFragments < 0 {
		return opts, ErrInvalidOptions
	}
	if opts.Window == WindowMinMax {
		if _, err := minMaxVOI(opts.Min, opts.Max); err != nil {
			return opts, err
		}
	}
	if opts.MaxFragments == 0 {
		opts.MaxFragments = 100000
	}
	defaults := DefaultGenerateLimits()
	if opts.Limits.MaxFrames < 0 || opts.Limits.MaxPixels < 0 || opts.Limits.MaxNativeBytes < 0 || opts.Limits.MaxInputBytes < 0 || opts.Limits.MaxExpansionRatio < 0 {
		return opts, ErrInvalidOptions
	}
	if opts.Limits.MaxFrames == 0 {
		opts.Limits.MaxFrames = defaults.MaxFrames
	}
	if opts.Limits.MaxPixels == 0 {
		opts.Limits.MaxPixels = defaults.MaxPixels
	}
	if opts.Limits.MaxNativeBytes == 0 {
		opts.Limits.MaxNativeBytes = defaults.MaxNativeBytes
	}
	if opts.Limits.MaxInputBytes == 0 {
		opts.Limits.MaxInputBytes = defaults.MaxInputBytes
	}
	if opts.Limits.MaxExpansionRatio == 0 {
		opts.Limits.MaxExpansionRatio = defaults.MaxExpansionRatio
	}
	return opts, nil
}

func preflightGenerate(obj *object.Object, m pixeldata.Metadata, limits pixeldata.DecompressLimits) error {
	photo := strings.ToUpper(strings.TrimSpace(m.PhotometricInterpretation))
	switch photo {
	case "MONOCHROME1", "MONOCHROME2", "PALETTE COLOR":
		if m.SamplesPerPixel != 1 {
			return ErrUnsupportedSource
		}
	case "RGB", "YBR_FULL", "YBR_FULL_422", "YBR_PARTIAL_422", "YBR_PARTIAL_420":
		if m.SamplesPerPixel != 3 || m.PixelRepresentation != 0 || m.PlanarConfiguration > 1 || !m.PlanarConfigurationPresent {
			return ErrUnsupportedSource
		}
	default:
		return fmt.Errorf("%w: photometric interpretation", ErrUnsupportedSource)
	}
	if m.Rows == 0 || m.Columns == 0 || m.PixelRepresentation > 1 || (m.BitsAllocated != 8 && m.BitsAllocated != 16) {
		return ErrUnsupportedSource
	}
	if err := m.SampleFormat(obj.ValueByteOrder()).Validate(); err != nil {
		return err
	}
	if m.NumberOfFrames > limits.MaxFrames {
		return generateLimit("MaxFrames", int64(m.NumberOfFrames), int64(limits.MaxFrames))
	}
	// Rows and Columns are uint16; bounding frames first makes this safe.
	pixels := int64(m.Rows) * int64(m.Columns)
	if int64(m.NumberOfFrames) > limits.MaxPixels/pixels {
		actual := int64(math.MaxInt64)
		if int64(m.NumberOfFrames) <= math.MaxInt64/pixels {
			actual = pixels * int64(m.NumberOfFrames)
		}
		return generateLimit("MaxPixels", actual, limits.MaxPixels)
	}
	bytes := m.TotalSize()
	if bytes <= 0 {
		return ErrUnsupportedSource
	}
	if bytes > limits.MaxNativeBytes {
		return generateLimit("MaxNativeBytes", bytes, limits.MaxNativeBytes)
	}
	return nil
}

// Materialized encapsulated limits count BOT and fragment payload bytes;
// deferred limits conservatively count the encoded value including Item
// headers. Both admissions precede frame indexing and selected-frame reads.
func preflightInput(ctx context.Context, file *object.File, m pixeldata.Metadata, opts GenerateOptions) error {
	element, ok := file.Dataset.Get(core.TagPixelData)
	if !ok {
		return pixeldata.ErrPixelDataNotFound
	}
	var inputBytes int64
	encapsulated := false
	if element.Value == nil {
		locations := file.ValueLocations(core.TagPixelData)
		if opts.Source == nil || len(locations) != 1 {
			return fmt.Errorf("%w: deferred Pixel Data requires Source", pixeldata.ErrFrameLayout)
		}
		inputBytes = locations[0].Length
		encapsulated = element.Header.Length.IsUndefined()
		if inputBytes < 0 {
			return pixeldata.ErrFrameLayout
		}
	} else {
		pixel, err := pixeldata.ExtractView(file.Dataset)
		if err != nil {
			return err
		}
		inputBytes = int64(len(pixel.Raw))
		if pixel.Encapsulated {
			encapsulated = true
			if len(pixel.Sequence.Fragments) > opts.MaxFragments {
				return generateLimit("MaxFragments", int64(len(pixel.Sequence.Fragments)), int64(opts.MaxFragments))
			}
			inputBytes = int64(len(pixel.Sequence.OffsetTable))
			for i, fragment := range pixel.Sequence.Fragments {
				if i&0x3fff == 0 {
					if err := ctx.Err(); err != nil {
						return err
					}
				}
				if int64(len(fragment)) > opts.Limits.MaxInputBytes-inputBytes {
					return generateLimit("MaxInputBytes", math.MaxInt64, opts.Limits.MaxInputBytes)
				}
				inputBytes += int64(len(fragment))
			}
		}
	}
	if inputBytes > opts.Limits.MaxInputBytes {
		return generateLimit("MaxInputBytes", inputBytes, opts.Limits.MaxInputBytes)
	}
	if !encapsulated {
		total := m.TotalSize()
		excess := inputBytes - total
		if excess < 0 || (excess >= m.FrameSize() && !(total%2 == 1 && excess == 1)) {
			return pixeldata.ErrPixelDataSizeMismatch
		}
	}
	return nil
}

// ReaderAt has no context parameter. Check around each index/frame read;
// cancellation cannot interrupt an underlying ReadAt already in progress.
type contextIconSource struct {
	ctx    context.Context
	source io.ReaderAt
}

func (s contextIconSource) ReadAt(p []byte, offset int64) (int, error) {
	if err := s.ctx.Err(); err != nil {
		return 0, err
	}
	n, err := s.source.ReadAt(p, offset)
	if canceled := s.ctx.Err(); canceled != nil {
		return n, canceled
	}
	return n, err
}

func generateLimit(name string, actual, maximum int64) error {
	return &pixeldata.DecompressResourceLimitError{Limit: name, Actual: actual, Maximum: maximum}
}

func minMaxVOI(low, high float64) (display.VOILUT, error) {
	center, width := low/2+high/2, high-low
	if !finite(low) || !finite(high) || !(high > low) || !finite(center) || !finite(width) {
		return display.VOILUT{}, fmt.Errorf("%w: finite increasing min/max required", ErrInvalidOptions)
	}
	return display.VOILUT{Center: center, Width: width, Function: display.VOILinearExact}, nil
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }

func generateGray(ctx context.Context, obj *object.Object, m pixeldata.Metadata, data []byte, index int, opts GenerateOptions) (*image.Gray, error) {
	format := m.SampleFormat(binary.LittleEndian)
	reader, err := format.Reader()
	if err != nil {
		return nil, err
	}
	modality, err := display.ModalityLUTAt(obj, index)
	if err != nil {
		return nil, err
	}
	if !finite(modality.Slope) || !finite(modality.Intercept) {
		return nil, fmt.Errorf("%w: non-finite modality", ErrUnsupportedSource)
	}
	var padding *pixeldata.PixelPadding
	if opts.Padding != nil {
		copy := *opts.Padding
		padding = &copy
		lo, hi, _ := format.Range()
		if padding.Value < lo || padding.Value > hi || (padding.HasRange && (padding.RangeLimit < lo || padding.RangeLimit > hi)) {
			return nil, pixeldata.ErrInvalidPixelPadding
		}
	} else if opts.IgnorePadding {
		padding, err = pixeldata.PixelPaddingFromObject(obj, format)
		if err != nil {
			return nil, err
		}
	}
	var voi display.VOILUT
	if opts.Window == WindowMinMax {
		voi, _ = minMaxVOI(opts.Min, opts.Max)
	} else if opts.Window == WindowFromSource {
		voi, err = display.VOIAt(obj, index)
		if err != nil {
			return nil, err
		}
		if !finite(voi.Center) || !finite(voi.Width) {
			return nil, fmt.Errorf("%w: non-finite VOI", ErrUnsupportedSource)
		}
	}
	automatic := voi.LUT == nil && voi.Width <= 0
	constant, usable := false, false
	if automatic {
		low, high := math.Inf(1), math.Inf(-1)
		for i := 0; i < int(m.Rows)*int(m.Columns); i++ {
			if i&0xffff == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			stored := reader.Value(data[i*reader.Width():])
			if padding != nil && padding.Contains(stored) {
				continue
			}
			value := modality.Apply(int(stored))
			if !finite(value) {
				return nil, fmt.Errorf("%w: non-finite modality value", ErrUnsupportedSource)
			}
			low, high, usable = math.Min(low, value), math.Max(high, value), true
		}
		if !usable {
			return nil, ErrNoUsablePixels
		}
		constant = low == high
		if !constant {
			voi, err = minMaxVOI(low, high)
			if err != nil {
				return nil, err
			}
		}
	}
	mapper := display.NewVOIByteMapper(voi)
	out := image.NewGray(image.Rect(0, 0, int(m.Columns), int(m.Rows)))
	usable = false
	for i := range out.Pix {
		if i&0xffff == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		stored := reader.Value(data[i*reader.Width():])
		if padding != nil && padding.Contains(stored) {
			continue
		}
		usable = true
		value := modality.Apply(int(stored))
		if !finite(value) {
			return nil, fmt.Errorf("%w: non-finite modality value", ErrUnsupportedSource)
		}
		if constant {
			out.Pix[i] = 128
		} else {
			out.Pix[i] = mapper.Byte(value)
		}
		if strings.EqualFold(m.PhotometricInterpretation, "MONOCHROME1") {
			out.Pix[i] = 255 - out.Pix[i]
		}
	}
	if !usable {
		return nil, ErrNoUsablePixels
	}
	return out, nil
}

func generateAspect(obj *object.Object, index int, m pixeldata.Metadata) (float64, error) {
	row, column := 1.0, 1.0
	if geometry := obj.FrameGeometryAt(index); geometry.PixelSpacingErr != nil {
		return 0, geometry.PixelSpacingErr
	}
	if spacing := obj.PixelSpacingAt(index); spacing.Valid() {
		row, column = spacing.RowSpacing, spacing.ColumnSpacing
	} else if obj.Has(core.NewTag(0x0028, 0x0034)) {
		ratio, err := obj.GetInts(core.NewTag(0x0028, 0x0034))
		if err != nil || len(ratio) != 2 || ratio[0] <= 0 || ratio[1] <= 0 {
			return 0, fmt.Errorf("%w: PixelAspectRatio", ErrUnsupportedSource)
		}
		row, column = float64(ratio[0]), float64(ratio[1])
	}
	aspect := float64(m.Columns) / float64(m.Rows) * (column / row)
	if !finite(aspect) || aspect <= 0 {
		return 0, fmt.Errorf("%w: aspect ratio", ErrUnsupportedSource)
	}
	return aspect, nil
}

// fitIcon averages source samples under each target sample. The bounding
// rectangle follows physical aspect while the source samples retain their
// grid coordinates; the black letterbox uses square output pixels.
func fitIcon(ctx context.Context, img image.Image, width, height int, aspect float64, gray bool) (image.Image, error) {
	tw, th := width, height
	if aspect > float64(width)/float64(height) {
		th = max(1, int(math.Round(float64(width)/aspect)))
	} else {
		tw = max(1, int(math.Round(float64(height)*aspect)))
	}
	ox, oy := (width-tw)/2, (height-th)/2
	b := img.Bounds()
	var out draw.Image
	if gray {
		out = image.NewGray(image.Rect(0, 0, width, height))
	} else {
		out = image.NewRGBA(image.Rect(0, 0, width, height))
	}
	for y := 0; y < height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < width; x++ {
			out.Set(x, y, color.Black)
		}
	}
	for ty := 0; ty < th; ty++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		y0, y1 := ty*b.Dy()/th, max(ty*b.Dy()/th+1, (ty+1)*b.Dy()/th)
		for tx := 0; tx < tw; tx++ {
			x0, x1 := tx*b.Dx()/tw, max(tx*b.Dx()/tw+1, (tx+1)*b.Dx()/tw)
			var r, g, blue, count uint64
			for y := y0; y < y1; y++ {
				if y&63 == 0 {
					if err := ctx.Err(); err != nil {
						return nil, err
					}
				}
				for x := x0; x < x1; x++ {
					c := img.At(b.Min.X+x, b.Min.Y+y)
					if gray {
						c = color.GrayModel.Convert(c)
					}
					cr, cg, cb, _ := c.RGBA()
					r += uint64(cr)
					g += uint64(cg)
					blue += uint64(cb)
					count++
				}
			}
			if gray {
				out.(*image.Gray).SetGray(ox+tx, oy+ty, color.Gray{Y: uint8((r/257 + count/2) / count)})
			} else {
				out.Set(ox+tx, oy+ty, color.RGBA64{R: uint16((r + count/2) / count), G: uint16((g + count/2) / count), B: uint16((blue + count/2) / count), A: 65535})
			}
		}
	}
	return out, nil
}

func generatedSequence(pixels []byte, width, height int, colors color.Palette, order binary.ByteOrder) core.Element {
	us := func(tag core.Tag, values ...uint16) core.Element {
		raw := make([]byte, 2*len(values))
		for i, v := range values {
			order.PutUint16(raw[2*i:], v)
		}
		return core.NewRawElement(tag, core.VRUS, raw)
	}
	photo := "MONOCHROME2"
	if colors != nil {
		photo = "PALETTE COLOR"
	}
	item := core.DataSet{Elements: []core.Element{
		us(core.NewTag(0x0028, 0x0002), 1),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0004), VR: core.VRCS}, Value: core.StringValue{photo}},
		us(core.NewTag(0x0028, 0x0010), uint16(height)), us(core.NewTag(0x0028, 0x0011), uint16(width)),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0034), VR: core.VRIS}, Value: core.StringValue{"1", "1"}},
		us(core.NewTag(0x0028, 0x0100), 8), us(core.NewTag(0x0028, 0x0101), 8), us(core.NewTag(0x0028, 0x0102), 7), us(core.NewTag(0x0028, 0x0103), 0),
	}}
	if colors != nil {
		channels := [3][]uint16{make([]uint16, 256), make([]uint16, 256), make([]uint16, 256)}
		for i, c := range colors {
			r, g, b, _ := c.RGBA()
			for ch, v := range []uint32{r, g, b} {
				channels[ch][i] = uint16(v >> 8)
			}
		}
		for ch := range 3 {
			item.Elements = append(item.Elements, us(core.NewTag(0x0028, uint16(0x1101+ch)), 256, 0, 8))
		}
		for ch := range 3 {
			data := display.EncodeLUTData(channels[ch], 8)
			if order == binary.BigEndian {
				for i := 0; i < len(data); i += 2 {
					data[i], data[i+1] = data[i+1], data[i]
				}
			}
			item.Elements = append(item.Elements, core.NewRawElement(core.NewTag(0x0028, uint16(0x1201+ch)), core.VROW, data))
		}
	}
	item.Elements = append(item.Elements, core.NewRawElement(core.TagPixelData, core.VROB, pixels))
	return core.Element{Header: core.ElementHeader{Tag: tagIconImageSequence, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{item}}}
}
