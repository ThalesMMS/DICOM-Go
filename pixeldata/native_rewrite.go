package pixeldata

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/derivation"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// ErrNativeRewrite reports a stored pixel representation that RewriteNative
// cannot change as requested.
var ErrNativeRewrite = errors.New("dicom: native pixel data rewrite unsupported")

var (
	tagSmallestImagePixelValue    = core.NewTag(0x0028, 0x0106)
	tagLargestImagePixelValue     = core.NewTag(0x0028, 0x0107)
	tagPixelIntensitySign         = core.NewTag(0x0028, 0x1041)
	tagWindowCenter               = core.NewTag(0x0028, 0x1050)
	tagVOILUTFunction             = core.NewTag(0x0028, 0x1056)
	tagRescaleIntercept           = core.NewTag(0x0028, 0x1052)
	tagRescaleSlope               = core.NewTag(0x0028, 0x1053)
	tagPaletteColorLUTUID         = core.NewTag(0x0028, 0x1199)
	tagModalityLUTSequence        = core.NewTag(0x0028, 0x3000)
	tagVOILUTSequence             = core.NewTag(0x0028, 0x3010)
	tagPresentationLUTShape       = core.NewTag(0x2050, 0x0020)
	tagSharedFunctionalGroupsSeq  = core.NewTag(0x5200, 0x9229)
	paletteDescriptorTags         = [3]core.Tag{core.NewTag(0x0028, 0x1101), core.NewTag(0x0028, 0x1102), core.NewTag(0x0028, 0x1103)}
	paletteDataTags               = [3]core.Tag{core.NewTag(0x0028, 0x1201), core.NewTag(0x0028, 0x1202), core.NewTag(0x0028, 0x1203)}
	segmentedPaletteTags          = [3]core.Tag{core.NewTag(0x0028, 0x1221), core.NewTag(0x0028, 0x1222), core.NewTag(0x0028, 0x1223)}
	nativeRewritePaletteFreeAttrs = []core.Tag{tagSmallestImagePixelValue, tagLargestImagePixelValue, tagPixelPaddingValue, tagPixelPaddingRangeLimit, tagPaletteColorLUTUID}
)

// NativeRewrite selects changes to the stored representation of native
// (uncompressed) pixel data. The steps apply in the order of the fields;
// unset fields leave the representation alone.
type NativeRewrite struct {
	// PaletteToRGB expands PALETTE COLOR indices through the palette (linear
	// or segmented) into RGB samples of the palette's entry depth, and drops
	// the palette and the index-valued attributes (Smallest/Largest Image
	// Pixel Value, Pixel Padding).
	PaletteToRGB bool
	// LinearizeSegmentedPalette keeps PALETTE COLOR but replaces Segmented
	// Palette Color LUT Data with the equivalent linear Palette Color LUT
	// Data (PS3.3 C.7.9.2), which more readers support.
	LinearizeSegmentedPalette bool
	// YBRFullToRGB converts unsigned YBR_FULL samples to RGB with the
	// inverse of the PS3.3 C.7.6.3.1.2 equations, rounded and clamped.
	YBRFullToRGB bool
	// Monochrome1ToMonochrome2 inverts MONOCHROME1 stored values (v becomes
	// min+max-v of the Bits Stored range) and carries the inversion into the
	// Window Center, Pixel Padding, Smallest/Largest Image Pixel Value, Pixel
	// Intensity Relationship Sign and Presentation LUT Shape, so the image
	// displays the same. A Modality or VOI LUT Sequence, or functional groups,
	// are refused.
	Monochrome1ToMonochrome2 bool
	// PlanarConfiguration, when set, reorders color samples: 0 interleaved
	// (R1G1B1R2...), 1 by plane (R1R2...G1G2...B1B2...).
	PlanarConfiguration *uint16
}

// NativeRewriteReport lists the steps RewriteNative applied.
type NativeRewriteReport struct {
	Frames int
	// Steps names each applied change, e.g. "PALETTE COLOR to RGB".
	Steps []string
}

// RewriteNative returns a copy of dataset whose native pixel data and the
// attributes describing it are changed as rewrite asks; dataset is not
// modified. Steps that change pixel values (palette expansion, YBR_FULL to
// RGB, MONOCHROME1 inversion) are appended to the Derivation Description;
// the SOP Instance UID is kept, since the image content is unchanged.
// Encapsulated pixel data must be decompressed first (or use
// TranscodeOptions.Rewrite).
func RewriteNative(ctx context.Context, dataset *object.Object, rewrite NativeRewrite) (*object.Object, NativeRewriteReport, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if dataset == nil {
		return nil, NativeRewriteReport{}, fmt.Errorf("%w: dataset is nil", ErrMissingMetadata)
	}
	native, err := ExtractNativeFrames(dataset)
	if err != nil {
		return nil, NativeRewriteReport{}, err
	}
	m := native.Metadata
	if m.BitsAllocated != 8 && m.BitsAllocated != 16 || m.BitsStored == 0 || m.BitsStored > m.BitsAllocated || m.HighBit != m.BitsStored-1 {
		return nil, NativeRewriteReport{}, fmt.Errorf("%w: Bits Allocated %d, Bits Stored %d, High Bit %d", ErrNativeRewrite, m.BitsAllocated, m.BitsStored, m.HighBit)
	}
	r := &nativeRewriter{ctx: ctx, source: dataset, out: object.FromElements(dataset.Elements(), nil), m: m, frames: native.Data, order: dataset.ValueByteOrder()}
	r.out.SetValueByteOrder(r.order)
	photometric := strings.ToUpper(strings.TrimSpace(m.PhotometricInterpretation))
	steps := []struct {
		on    bool
		apply func() error
	}{
		{rewrite.PaletteToRGB && photometric == "PALETTE COLOR", r.paletteToRGB},
		{rewrite.LinearizeSegmentedPalette && !rewrite.PaletteToRGB && photometric == "PALETTE COLOR", r.linearizePalette},
		{rewrite.YBRFullToRGB && photometric == "YBR_FULL", r.ybrFullToRGB},
		{rewrite.Monochrome1ToMonochrome2 && photometric == "MONOCHROME1", r.monochrome1ToMonochrome2},
		{rewrite.PlanarConfiguration != nil, func() error { return r.planar(*rewrite.PlanarConfiguration) }},
	}
	for _, step := range steps {
		if !step.on {
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, NativeRewriteReport{}, err
		}
		if err := step.apply(); err != nil {
			return nil, NativeRewriteReport{}, err
		}
	}
	if len(r.steps) == 0 {
		return r.out, NativeRewriteReport{Frames: len(r.frames)}, nil
	}
	raw := make([]byte, 0, int(r.m.FrameSize())*len(r.frames)+1)
	for _, frame := range r.frames {
		raw = append(raw, frame...)
	}
	if len(raw)%2 == 1 {
		raw = append(raw, 0)
	}
	r.out.Put(core.NewRawElement(core.TagPixelData, nativePixelDataVR(r.m), raw))
	r.out.Put(photometricInterpretationElement(r.m.PhotometricInterpretation))
	for tag, value := range map[core.Tag]uint16{tagSamplesPerPixel: r.m.SamplesPerPixel, tagBitsAllocated: r.m.BitsAllocated, tagBitsStored: r.m.BitsStored, tagHighBit: r.m.HighBit, tagPixelRepresentation: r.m.PixelRepresentation} {
		r.out.Put(r.us(tag, value))
	}
	if r.m.SamplesPerPixel > 1 {
		r.out.Put(r.us(tagPlanarConfiguration, r.m.PlanarConfiguration))
	} else {
		r.out.Remove(tagPlanarConfiguration)
	}
	if len(r.derived) > 0 {
		if err := derivation.Apply(r.out, derivation.Derivation{Description: strings.Join(r.derived, "; ")}); err != nil {
			return nil, NativeRewriteReport{}, err
		}
	}
	return r.out, NativeRewriteReport{Frames: len(r.frames), Steps: r.steps}, nil
}

type nativeRewriter struct {
	ctx     context.Context
	source  *object.Object
	out     *object.Object
	m       Metadata
	frames  [][]byte
	order   binary.ByteOrder
	steps   []string
	derived []string
}

func (r *nativeRewriter) us(tag core.Tag, value uint16) core.Element {
	raw := make([]byte, 2)
	r.order.PutUint16(raw, value)
	return core.NewRawElement(tag, core.VRUS, raw)
}

func (r *nativeRewriter) record(step string, derived bool) {
	r.steps = append(r.steps, step)
	if derived {
		r.derived = append(r.derived, step)
	}
}

// sample reads sample i of frame as a signed or unsigned stored value.
func (r *nativeRewriter) sample(frame []byte, i int) int {
	var v uint32
	if r.m.BitsAllocated == 8 {
		v = uint32(frame[i])
	} else {
		v = uint32(r.order.Uint16(frame[2*i:]))
	}
	v &= 1<<r.m.BitsStored - 1
	if r.m.PixelRepresentation == 1 && v&(1<<(r.m.BitsStored-1)) != 0 {
		return int(v) - 1<<r.m.BitsStored
	}
	return int(v)
}

// put writes a stored value into a frame of the current layout.
func putSample(frame []byte, i, bitsAllocated int, order binary.ByteOrder, v int) {
	if bitsAllocated == 8 {
		frame[i] = byte(v)
		return
	}
	order.PutUint16(frame[2*i:], uint16(v))
}

func (r *nativeRewriter) samplesPerFrame() int {
	return int(r.m.Rows) * int(r.m.Columns) * int(r.m.SamplesPerPixel)
}

func (r *nativeRewriter) paletteToRGB() error {
	if r.m.SamplesPerPixel != 1 {
		return fmt.Errorf("%w: PALETTE COLOR with %d samples per pixel", ErrNativeRewrite, r.m.SamplesPerPixel)
	}
	palette, err := display.PaletteFromObject(r.source)
	if err != nil {
		return err
	}
	if palette == nil || palette.Red == nil || palette.Green == nil || palette.Blue == nil {
		return fmt.Errorf("%w: PALETTE COLOR without palette LUTs", ErrNativeRewrite)
	}
	bits := palette.Red.BitsPerEntry
	if palette.Green.BitsPerEntry != bits || palette.Blue.BitsPerEntry != bits {
		return fmt.Errorf("%w: palette LUTs of different depths", ErrNativeRewrite)
	}
	allocated := 16
	if bits <= 8 {
		allocated = 8
	}
	pixels := int(r.m.Rows) * int(r.m.Columns)
	for f, frame := range r.frames {
		out := make([]byte, 3*pixels*allocated/8)
		for i := 0; i < pixels; i++ {
			index := r.sample(frame, i)
			for c, lut := range []*display.LUT{palette.Red, palette.Green, palette.Blue} {
				putSample(out, 3*i+c, allocated, r.order, int(lut.Lookup(index)))
			}
		}
		r.frames[f] = out
	}
	r.m.SamplesPerPixel, r.m.PhotometricInterpretation, r.m.PlanarConfiguration = 3, "RGB", 0
	r.m.BitsAllocated, r.m.BitsStored, r.m.HighBit, r.m.PixelRepresentation = uint16(allocated), uint16(bits), uint16(bits-1), 0
	for _, tags := range [][3]core.Tag{paletteDescriptorTags, paletteDataTags, segmentedPaletteTags} {
		for _, tag := range tags {
			r.out.Remove(tag)
		}
	}
	for _, tag := range nativeRewritePaletteFreeAttrs {
		r.out.Remove(tag)
	}
	r.record("PALETTE COLOR to RGB", true)
	return nil
}

func (r *nativeRewriter) linearizePalette() error {
	segmented := false
	for _, tag := range segmentedPaletteTags {
		segmented = segmented || r.source.Has(tag)
	}
	if !segmented {
		return nil
	}
	palette, err := display.PaletteFromObject(r.source)
	if err != nil {
		return err
	}
	if palette == nil || palette.Red == nil || palette.Green == nil || palette.Blue == nil {
		return fmt.Errorf("%w: PALETTE COLOR without palette LUTs", ErrNativeRewrite)
	}
	for c, lut := range []*display.LUT{palette.Red, palette.Green, palette.Blue} {
		entries := len(lut.Entries)
		if entries < 1 || entries > 1<<16 {
			return fmt.Errorf("%w: palette of %d entries", ErrNativeRewrite, entries)
		}
		count := entries
		if count == 1<<16 {
			count = 0
		}
		descriptor := make([]byte, 6)
		r.order.PutUint16(descriptor, uint16(count))
		r.order.PutUint16(descriptor[2:], uint16(lut.FirstMapped))
		r.order.PutUint16(descriptor[4:], uint16(lut.BitsPerEntry))
		vr := core.VRUS
		if r.m.PixelRepresentation == 1 {
			vr = core.VRSS
		}
		r.out.Put(core.NewRawElement(paletteDescriptorTags[c], vr, descriptor))
		data := display.EncodeLUTData(lut.Entries, lut.BitsPerEntry)
		if r.order == binary.BigEndian {
			for i := 0; i+1 < len(data); i += 2 {
				data[i], data[i+1] = data[i+1], data[i]
			}
		}
		r.out.Put(core.NewRawElement(paletteDataTags[c], core.VROW, data))
		r.out.Remove(segmentedPaletteTags[c])
	}
	r.record("segmented palette linearized", false)
	return nil
}

func (r *nativeRewriter) ybrFullToRGB() error {
	if r.m.SamplesPerPixel != 3 || r.m.PixelRepresentation != 0 {
		return fmt.Errorf("%w: YBR_FULL needs 3 unsigned samples per pixel", ErrNativeRewrite)
	}
	maxValue := float64(int(1)<<r.m.BitsStored - 1)
	half := float64(int(1) << (r.m.BitsStored - 1))
	pixels := int(r.m.Rows) * int(r.m.Columns)
	index := func(i, c int) int {
		if r.m.PlanarConfiguration == 1 {
			return c*pixels + i
		}
		return 3*i + c
	}
	clamp := func(v float64) int { return int(math.Round(math.Max(0, math.Min(maxValue, v)))) }
	for _, frame := range r.frames {
		for i := 0; i < pixels; i++ {
			y := float64(r.sample(frame, index(i, 0)))
			cb := float64(r.sample(frame, index(i, 1))) - half
			cr := float64(r.sample(frame, index(i, 2))) - half
			rgb := [3]int{clamp(y + 1.402*cr), clamp(y - 0.344136*cb - 0.714136*cr), clamp(y + 1.772*cb)}
			for c := range rgb {
				putSample(frame, index(i, c), int(r.m.BitsAllocated), r.order, rgb[c])
			}
		}
	}
	r.m.PhotometricInterpretation = "RGB"
	r.record("YBR_FULL to RGB", true)
	return nil
}

func (r *nativeRewriter) monochrome1ToMonochrome2() error {
	for _, tag := range []core.Tag{tagModalityLUTSequence, tagVOILUTSequence, tagSharedFunctionalGroupsSeq} {
		if r.source.Has(tag) {
			return fmt.Errorf("%w: MONOCHROME1 with %s", ErrNativeRewrite, tag)
		}
	}
	if r.m.SamplesPerPixel != 1 {
		return fmt.Errorf("%w: MONOCHROME1 with %d samples per pixel", ErrNativeRewrite, r.m.SamplesPerPixel)
	}
	// Inverting within the stored range maps v to sum-v, where sum is the
	// range's min+max: 2^bits-1 unsigned, -1 signed.
	sum := int(1)<<r.m.BitsStored - 1
	if r.m.PixelRepresentation == 1 {
		sum = -1
	}
	n := r.samplesPerFrame()
	for _, frame := range r.frames {
		for i := 0; i < n; i++ {
			putSample(frame, i, int(r.m.BitsAllocated), r.order, sum-r.sample(frame, i))
		}
	}
	// Stored-value attributes invert too; Smallest and Largest swap.
	invert := func(tag core.Tag) (int, bool) {
		values, err := derivedio.LookupInts(r.source, tag)
		if err != nil || len(values) != 1 {
			return 0, false
		}
		return sum - int(values[0]), true
	}
	put := func(tag core.Tag, v int) {
		raw := make([]byte, 2)
		r.order.PutUint16(raw, uint16(v))
		vr := core.VRUS
		if r.m.PixelRepresentation == 1 {
			vr = core.VRSS
		}
		r.out.Put(core.NewRawElement(tag, vr, raw))
	}
	smallest, hasSmallest := invert(tagLargestImagePixelValue)
	largest, hasLargest := invert(tagSmallestImagePixelValue)
	r.out.Remove(tagSmallestImagePixelValue)
	r.out.Remove(tagLargestImagePixelValue)
	if hasSmallest {
		put(tagSmallestImagePixelValue, smallest)
	}
	if hasLargest {
		put(tagLargestImagePixelValue, largest)
	}
	for _, tag := range []core.Tag{tagPixelPaddingValue, tagPixelPaddingRangeLimit} {
		if v, ok := invert(tag); ok {
			put(tag, v)
		}
	}
	if v, err := derivedio.LookupInts(r.source, tagPixelIntensitySign); err == nil && len(v) == 1 && (v[0] == 1 || v[0] == -1) {
		r.out.Put(core.Element{Header: core.ElementHeader{Tag: tagPixelIntensitySign, VR: core.VRSS}, Value: core.Int16Value{int16(-v[0])}})
	}
	if shape, ok := r.source.GetString(tagPresentationLUTShape); ok && strings.TrimSpace(shape) == "INVERSE" {
		r.out.Put(core.Element{Header: core.ElementHeader{Tag: tagPresentationLUTShape, VR: core.VRCS}, Value: core.StringValue{"IDENTITY"}})
	}
	if err := r.invertWindows(sum); err != nil {
		return err
	}
	r.m.PhotometricInterpretation = "MONOCHROME2"
	r.record("MONOCHROME1 inverted to MONOCHROME2", true)
	return nil
}

// invertWindows moves each Window Center so that the MONOCHROME2 window of
// the inverted modality values x' = k-x gives 1 minus the MONOCHROME1 window,
// k = slope*sum + 2*intercept: c' = k-c+1 for LINEAR (whose thresholds sit at
// c-0.5), k-c for LINEAR_EXACT and SIGMOID (PS3.3 C.11.2.1.2).
func (r *nativeRewriter) invertWindows(sum int) error {
	centers, ok := r.source.GetStrings(tagWindowCenter)
	if !ok || len(centers) == 0 {
		return nil
	}
	number := func(tag core.Tag, fallback float64) (float64, error) {
		v, ok := r.source.GetString(tag)
		if !ok || strings.TrimSpace(v) == "" {
			return fallback, nil
		}
		return strconv.ParseFloat(strings.TrimSpace(v), 64)
	}
	slope, err := number(tagRescaleSlope, 1)
	if err != nil {
		return fmt.Errorf("%w: Rescale Slope: %v", ErrNativeRewrite, err)
	}
	intercept, err := number(tagRescaleIntercept, 0)
	if err != nil {
		return fmt.Errorf("%w: Rescale Intercept: %v", ErrNativeRewrite, err)
	}
	offset := 1.0
	if function, ok := r.source.GetString(tagVOILUTFunction); ok {
		switch strings.ToUpper(strings.TrimSpace(function)) {
		case "LINEAR_EXACT", "SIGMOID":
			offset = 0
		}
	}
	k := slope*float64(sum) + 2*intercept
	out := make([]string, len(centers))
	for i, c := range centers {
		center, err := strconv.ParseFloat(strings.TrimSpace(c), 64)
		if err != nil {
			return fmt.Errorf("%w: Window Center %q", ErrNativeRewrite, c)
		}
		if out[i], err = object.FormatDS(k - center + offset); err != nil {
			return fmt.Errorf("%w: Window Center: %v", ErrNativeRewrite, err)
		}
	}
	r.out.Put(core.Element{Header: core.ElementHeader{Tag: tagWindowCenter, VR: core.VRDS}, Value: core.StringValue(out)})
	return nil
}

func (r *nativeRewriter) planar(target uint16) error {
	if target > 1 {
		return fmt.Errorf("%w: Planar Configuration %d", ErrNativeRewrite, target)
	}
	if r.m.SamplesPerPixel == 1 || r.m.PlanarConfiguration == target {
		return nil
	}
	for f, frame := range r.frames {
		out, err := reorderNativeFrame(r.ctx, frame, r.m, target)
		if err != nil {
			return err
		}
		r.frames[f] = out
	}
	r.m.PlanarConfiguration = target
	r.record(fmt.Sprintf("Planar Configuration %d", target), false)
	return nil
}

// reorderNativeFrame borrows unchanged data and owns a changed result. Sample
// bytes are copied verbatim, so it works in either native byte order. Internal
// callers must supply a non-nil context, normalized at their public boundary.
func reorderNativeFrame(ctx context.Context, frame []byte, m Metadata, target uint16) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if target > 1 || m.PlanarConfiguration > 1 || m.BitsAllocated != 8 && m.BitsAllocated != 16 {
		return nil, fmt.Errorf("%w: planar sample layout", ErrNativeRewrite)
	}
	if int64(len(frame)) != m.FrameSize() {
		return nil, ErrPixelDataSizeMismatch
	}
	if m.SamplesPerPixel == 1 || m.PlanarConfiguration == target {
		return frame, nil
	}
	samples, pixels, width := int(m.SamplesPerPixel), int(m.Rows)*int(m.Columns), int(m.BitsAllocated)/8
	out := make([]byte, len(frame))
	for i := 0; i < pixels; i++ {
		if i%65536 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		for c := 0; c < samples; c++ {
			interleaved, planar := (samples*i+c)*width, (c*pixels+i)*width
			if target == 1 {
				copy(out[planar:planar+width], frame[interleaved:interleaved+width])
			} else {
				copy(out[interleaved:interleaved+width], frame[planar:planar+width])
			}
		}
	}
	return out, nil
}
