package compare

import (
	"context"
	"encoding/binary"
	"fmt"
	stdimage "image"
	"math"
	"reflect"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// PresentedReader is satisfied by *pixeldata.FrameReader. It is borrowed: the
// caller keeps it and its file/source alive, immutable, and closes it afterwards.
// Frame must return one decoded native little-endian frame. Codec/index budgets
// remain the caller's FrameReaderOptions responsibility.
type PresentedReader interface {
	Metadata() pixeldata.Metadata
	File() *object.File
	Frame(context.Context, int) ([]byte, pixeldata.Metadata, error)
}

// PresentationProfile supplies explicit VOI and post-VOI presentation. There is
// no object-derived or auto-extrema VOI fallback. Invert is the effective final
// inversion (including MONOCHROME1 if desired), not an additional hidden flip.
// Color ignores no grayscale transform: supplying one for color is refused.
type PresentationProfile struct {
	VOI    display.VOILUT `json:"voi"`
	LUT    *display.LUT   `json:"presentation_lut,omitempty"`
	Invert bool           `json:"invert"`
}

type PresentedReaderOptions struct {
	Comparison PresentedOptions
	Image      PresentedImagePolicy
	// Mode is common (Reference applied to both) or per-image (both explicit).
	Mode                 string
	Reference, Candidate PresentationProfile
	MaxFrames            int
}

type PresentedFrameRecord struct {
	Index                                int    `json:"index"`
	Mode                                 string `json:"mode"`
	ReferenceMetadata, CandidateMetadata pixeldata.Metadata
	ReferenceProfile, CandidateProfile   PresentationProfile
	ReferenceModality, CandidateModality display.ModalityLUT
	ReferencePalette, CandidatePalette   *display.PaletteColorLUT
	Result                               *PresentedResult `json:"result"`
}

func clonePresentationLUT(l *display.LUT) (*display.LUT, error) {
	if l == nil {
		return nil, nil
	}
	if l.FirstMapped < -2147483648 || l.FirstMapped > 2147483647 || len(l.Entries) == 0 || len(l.Entries) > 65536 || len(l.Adjustments) > 32 {
		return nil, ErrPresentedConfiguration
	}
	for _, adjustment := range l.Adjustments {
		if len(adjustment) > 256 {
			return nil, ErrPresentedConfiguration
		}
	}
	out, err := display.NewLUT([]int{len(l.Entries), l.FirstMapped, l.BitsPerEntry}, l.Entries)
	if err != nil {
		return nil, err
	}
	out.Entries = append([]uint16(nil), out.Entries...)
	out.Adjustments = append([]display.LUTAdjustment(nil), l.Adjustments...)
	return out, nil
}
func cloneProfile(p PresentationProfile, gray bool) (PresentationProfile, error) {
	if !gray {
		if p.VOI.Width != 0 || p.VOI.Center != 0 || p.VOI.LUT != nil || p.LUT != nil || p.Invert || p.VOI.Function != 0 || p.VOI.Explanation != "" {
			return p, ErrPresentedConfiguration
		}
		return p, nil
	}
	if !finite(p.VOI.Center) || !finite(p.VOI.Width) || len(p.VOI.Explanation) > 1024 || p.VOI.Function < display.VOILinear || p.VOI.Function > display.VOISigmoid {
		return p, ErrPresentedConfiguration
	}
	if p.VOI.LUT == nil && (p.VOI.Width <= 0 || (p.VOI.Function == display.VOILinear && p.VOI.Width < 1)) {
		return p, fmt.Errorf("%w: explicit VOI required", ErrPresentedConfiguration)
	}
	var err error
	p.VOI.LUT, err = clonePresentationLUT(p.VOI.LUT)
	if err != nil {
		return p, err
	}
	p.LUT, err = clonePresentationLUT(p.LUT)
	return p, err
}
func missingPresentedReader(r PresentedReader) bool {
	if r == nil {
		return true
	}
	v := reflect.ValueOf(r)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Func, reflect.Slice:
		return v.IsNil()
	}
	return false
}
func clonePalette(p *display.PaletteColorLUT) (*display.PaletteColorLUT, error) {
	if p == nil {
		return nil, nil
	}
	out := &display.PaletteColorLUT{}
	var err error
	out.Red, err = clonePresentationLUT(p.Red)
	if err != nil {
		return nil, err
	}
	out.Green, err = clonePresentationLUT(p.Green)
	if err != nil {
		return nil, err
	}
	out.Blue, err = clonePresentationLUT(p.Blue)
	if err != nil {
		return nil, err
	}
	if out.Red == nil || out.Green == nil || out.Blue == nil {
		return nil, ErrMismatch
	}
	return out, nil
}
func readerPalette(r PresentedReader, m pixeldata.Metadata) (*display.PaletteColorLUT, error) {
	photo := strings.TrimSpace(strings.ToUpper(m.PhotometricInterpretation))
	if photo != "PALETTE COLOR" && photo != "ARGB" {
		return nil, nil
	}
	p, err := display.PaletteFromObject(r.File().Dataset)
	if err != nil {
		return nil, err
	}
	return clonePalette(p)
}
func isGray(m pixeldata.Metadata) bool {
	s := strings.TrimSpace(strings.ToUpper(m.PhotometricInterpretation))
	return m.SamplesPerPixel == 1 && (s == "MONOCHROME1" || s == "MONOCHROME2")
}

// ComparePresentedReaders processes one pair at a time. A synchronous consumer
// receives each complete frame; it may export immediately. Completed is the
// successfully consumed prefix, never a claim that a failed/cancelled run passed.
// No all-frame result array is retained. Consumer retention, file/index buffers,
// codec scratch and process RSS are outside the logical comparison quota.
// There is no CPU 3D rendering: these are ordinary DICOM 2D decoded frames.
func ComparePresentedReaders(ctx context.Context, a, b PresentedReader, o PresentedReaderOptions, consume func(context.Context, PresentedFrameRecord) error) (completed int, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err = ctx.Err(); err != nil {
		return
	}
	if missingPresentedReader(a) || missingPresentedReader(b) || consume == nil || a.File() == nil || b.File() == nil || a.File().Dataset == nil || b.File().Dataset == nil {
		return 0, ErrMismatch
	}
	am, bm := a.Metadata(), b.Metadata()
	gray := isGray(am)
	if am.Rows != bm.Rows || am.Columns != bm.Columns || gray != isGray(bm) || am.NumberOfFrames != bm.NumberOfFrames || am.NumberOfFrames <= 0 {
		return 0, ErrMismatch
	}
	if o.MaxFrames <= 0 {
		return 0, ErrPresentedConfiguration
	}
	if am.NumberOfFrames > o.MaxFrames {
		return 0, ErrPresentedLimit
	}
	if err = o.Image.validate(); err != nil {
		return
	}
	if gray != (o.Image.Encoding == "gray-pvalue") {
		return 0, ErrMismatch
	}
	if o.Mode != "common" && o.Mode != "per-image" {
		return 0, ErrPresentedConfiguration
	}
	c := 3
	if gray {
		c = 1
	}
	// Conservative own-buffer allowance for two decoded frames, rendering and
	// float planes in addition to maps. LUT/profile copies reserve 4 MiB. A frame
	// reader/codec with other scratch requirements needs its own separate limit.
	budget, err := presentedBudget(int(am.Columns), int(am.Rows), c, o.Comparison, 128)
	if err != nil {
		return 0, err
	}
	if budget > o.Comparison.MaxWorkingBytes-(4<<20) {
		return 0, ErrPresentedLimit
	}
	budget += 4 << 20
	ar, err := cloneProfile(o.Reference, gray)
	if err != nil {
		return 0, err
	}
	br := ar
	if o.Mode == "per-image" {
		br, err = cloneProfile(o.Candidate, gray)
		if err != nil {
			return 0, err
		}
	}
	for _, r := range []PresentedReader{a, b} {
		if _, exists := r.File().Dataset.Get(core.NewTag(0x0028, 0x2000)); exists {
			return 0, fmt.Errorf("%w: ICC-managed presentation must be supplied as a produced image", ErrMismatch)
		}
	}
	// Snapshot the shared mask/regions before callbacks can change caller options.
	o.Comparison.Mask = append([]bool(nil), o.Comparison.Mask...)
	o.Comparison.Regions = append([]ComparisonRegion(nil), o.Comparison.Regions...)
	for i := 0; i < am.NumberOfFrames; i++ {
		if err = ctx.Err(); err != nil {
			return
		}
		var av, bv PresentedFrame
		var ad, bd pixeldata.Metadata
		var ma, mb display.ModalityLUT
		av, ad, ma, err = readerPlane(ctx, a, i, am, ar, o.Image, o.Comparison)
		if err != nil {
			return
		}
		bv, bd, mb, err = readerPlane(ctx, b, i, bm, br, o.Image, o.Comparison)
		if err != nil {
			return
		}
		var result *PresentedResult
		result, err = ComparePresented(ctx, av, bv, o.Comparison)
		if err != nil {
			return
		}
		imagePolicy := o.Image
		result.ImagePolicy = &imagePolicy
		result.LogicalWorkingBytes = budget
		// Metadata arrays are detached per delivered frame, so a consumer cannot
		// mutate a profile used for a later frame.
		arp, _ := cloneProfile(ar, gray)
		brp, _ := cloneProfile(br, gray)
		record := PresentedFrameRecord{Index: i, Mode: o.Mode, ReferenceMetadata: ad, CandidateMetadata: bd, ReferenceProfile: arp, CandidateProfile: brp, ReferenceModality: ma, CandidateModality: mb, Result: result}
		record.ReferencePalette, err = readerPalette(a, ad)
		if err != nil {
			return
		}
		record.CandidatePalette, err = readerPalette(b, bd)
		if err != nil {
			return
		}
		if err = ctx.Err(); err != nil {
			return
		}
		if err = consume(ctx, record); err != nil {
			return
		}
		completed++
	}
	err = ctx.Err()
	return
}
func readerPlane(ctx context.Context, r PresentedReader, index int, expected pixeldata.Metadata, p PresentationProfile, policy PresentedImagePolicy, o PresentedOptions) (PresentedFrame, pixeldata.Metadata, display.ModalityLUT, error) {
	var f PresentedFrame
	var modality display.ModalityLUT
	raw, m, err := r.Frame(ctx, index)
	if err != nil {
		return f, m, modality, err
	}
	if err = ctx.Err(); err != nil {
		return f, m, modality, err
	}
	if m.Rows != expected.Rows || m.Columns != expected.Columns || isGray(m) != isGray(expected) || m.SamplesPerPixel == 0 || m.SamplesPerPixel > 4 || m.BitsAllocated > 32 || m.FrameSize() <= 0 || int64(len(raw)) != m.FrameSize() {
		return f, m, modality, ErrMismatch
	}
	n := int(m.Rows) * int(m.Columns)
	if int64(len(raw)) > int64(n)*16 {
		return f, m, modality, ErrPresentedLimit
	}
	if isGray(m) {
		modality, err = display.ModalityLUTAt(r.File().Dataset, index)
		if err != nil {
			return f, m, modality, err
		}
		if !finite(modality.Slope) || !finite(modality.Intercept) {
			return f, m, modality, ErrMismatch
		}
		modality.LUT, err = clonePresentationLUT(modality.LUT)
		if err != nil {
			return f, m, modality, err
		}
		reader, err := m.SampleFormat(binary.LittleEndian).Reader()
		if err != nil {
			return f, m, modality, err
		}
		if reader.Width() == 0 || n > len(raw)/reader.Width() {
			return f, m, modality, ErrMismatch
		}
		mapper := display.NewVOIByteMapper(p.VOI)
		f = PresentedFrame{Width: int(m.Columns), Height: int(m.Rows), Components: 1, Depth: policy.Depth, Encoding: "gray-pvalue", Values: make([]float64, n)}
		// Apply the same existing presentation implementation, one row at a time,
		// retaining bounded cancellation checkpoints and its depth-specific LUT rule.
		max := float64(255)
		if policy.Depth == 16 {
			max = 65535
		}
		row := make([]byte, int(m.Columns)*(policy.Depth/8))
		for y := 0; y < int(m.Rows); y++ {
			if err = ctx.Err(); err != nil {
				return f, m, modality, err
			}
			for x := 0; x < int(m.Columns); x++ {
				i := y*int(m.Columns) + x
				stored := reader.Value(raw[i*reader.Width():])
				if stored > int64(math.MaxInt) || stored < int64(math.MinInt) {
					return f, m, modality, ErrMismatch
				}
				value := modality.Apply(int(stored))
				if !finite(value) {
					return f, m, modality, ErrMismatch
				}
				q := uint16(math.Round(mapper.Unit(value) * max))
				if policy.Depth == 8 {
					row[x] = byte(q)
				} else {
					binary.BigEndian.PutUint16(row[x*2:], q)
				}
			}
			presentation := display.Presentation{LUT: p.LUT, Invert: p.Invert}
			if policy.Depth == 8 {
				presentation.Apply(&stdimage.Gray{Pix: row, Stride: len(row), Rect: stdimage.Rect(0, 0, int(m.Columns), 1)})
			} else {
				presentation.ApplyGray16(&stdimage.Gray16{Pix: row, Stride: len(row), Rect: stdimage.Rect(0, 0, int(m.Columns), 1)})
			}
			for x := 0; x < int(m.Columns); x++ {
				q := uint16(row[x])
				if policy.Depth == 16 {
					q = binary.BigEndian.Uint16(row[x*2:])
				}
				f.Values[y*int(m.Columns)+x] = float64(q) / max
			}
		}
		return f, m, modality, ctx.Err()
	}
	format := display.PixelFormat{BitsAllocated: int(m.BitsAllocated), BitsStored: int(m.BitsStored), HighBit: int(m.HighBit), Signed: m.PixelRepresentation == 1, ByteOrder: binary.LittleEndian}
	cf := display.ColorFrame{Rows: int(m.Rows), Columns: int(m.Columns), Pixels: raw, Photometric: m.PhotometricInterpretation, SamplesPerPixel: int(m.SamplesPerPixel), PlanarConfiguration: int(m.PlanarConfiguration), Format: format}
	if strings.EqualFold(strings.TrimSpace(m.PhotometricInterpretation), "PALETTE COLOR") || strings.EqualFold(strings.TrimSpace(m.PhotometricInterpretation), "ARGB") {
		cf.Palette, err = display.PaletteFromObject(r.File().Dataset)
		if err != nil {
			return f, m, modality, err
		}
	}
	var im stdimage.Image
	if policy.Depth == 8 {
		im, err = display.RenderColor(cf)
	} else {
		im, err = display.RenderColor16(cf)
	}
	if err != nil {
		return f, m, modality, err
	}
	f, err = imagePlane(ctx, im, policy, o)
	return f, m, modality, err
}
