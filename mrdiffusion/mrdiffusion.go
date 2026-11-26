// Package mrdiffusion reads the diffusion weighting of MR images: the
// b-value and gradient direction of each frame, and where they were found.
//
// The standard MR Diffusion Macro (PS3.3 C.8.13.5.9, in the Shared or
// Per-Frame Functional Groups of enhanced objects) is read first, then the
// same attributes at the top level of classic images, then the private
// attributes vendors used before (and next to) the standard ones:
//
//   - Siemens: B_value and DiffusionGradientDirection of the CSA image header
//     (0029,xx10 "SIEMENS CSA HEADER"), else (0019,xx0C) and (0019,xx0E) of
//     "SIEMENS MR HEADER".
//   - GE: the first value of (0043,xx39) "GEMS_PARM_01" (Slop_int_6), with
//     the gradient components in (0019,xxBB), (0019,xxBC) and (0019,xxBD) of
//     "GEMS_ACQU_01".
//   - Philips: (2001,xx03) "Philips Imaging DD 001" (Diffusion B-Factor),
//     with (2005,xxB0), (2005,xxB1) and (2005,xxB2) of
//     "Philips MR Imaging DD 001" (directions RL, AP and FH).
//   - Toshiba/Canon: "b=1000(x,y,z)" in Image Comments (0020,4000) of
//     Toshiba and Canon images that carry no standard attribute.
//
// Vendor directions are reported in the axes the vendor stores them in (see
// DirectionFrame); only the standard attributes and Siemens use the DICOM
// patient axes. GE writes (0043,xx39) in every MR image, so a GE image whose
// first value is 0 is not necessarily diffusion weighted: callers should
// treat a series as diffusion weighted only when one of its frames has a
// positive b-value. The implementation follows the attribute definitions of
// the standard and of the public vendor dictionaries; no code was ported.
package mrdiffusion

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/siemens"
	"github.com/ThalesMMS/dicom-go/vendors"
)

var (
	// ErrNotFound reports a frame without any b-value.
	ErrNotFound = errors.New("dicom/mrdiffusion: no diffusion b-value")
	// ErrInvalid reports a b-value or direction that is not a finite number,
	// or a negative b-value.
	ErrInvalid = errors.New("dicom/mrdiffusion: invalid diffusion attribute")
)

// Source names where a frame's diffusion attributes were found.
type Source string

const (
	// SourceMRDiffusionMacro is the MR Diffusion Sequence (0018,9117) of the
	// frame's Per-Frame or of the Shared Functional Groups.
	SourceMRDiffusionMacro Source = "mr-diffusion-macro"
	// SourceStandardAttributes is Diffusion b-value (0018,9087) at the top
	// level of a classic image.
	SourceStandardAttributes Source = "standard-attributes"
	// SourceSiemensCSA is the CSA image header.
	SourceSiemensCSA Source = "siemens-csa"
	// SourceSiemensMRHeader is (0019,xx0C) of "SIEMENS MR HEADER".
	SourceSiemensMRHeader Source = "siemens-mr-header"
	// SourceGE is (0043,xx39) of "GEMS_PARM_01".
	SourceGE Source = "ge-private"
	// SourcePhilips is (2001,xx03) of "Philips Imaging DD 001".
	SourcePhilips Source = "philips-private"
	// SourceCanonImageComments is "b=" in the Image Comments of a Toshiba or
	// Canon image.
	SourceCanonImageComments Source = "canon-image-comments"
)

// DirectionFrame names the axes of a gradient direction.
type DirectionFrame string

const (
	// FramePatient is the DICOM patient coordinate system (x to the
	// patient's left, y posterior, z head): the standard attributes and the
	// Siemens headers.
	FramePatient DirectionFrame = "patient"
	// FramePhilipsRLAPFH is the Philips (RL, AP, FH) order of the patient
	// axes, as stored in (2005,xxB0..B2).
	FramePhilipsRLAPFH DirectionFrame = "philips-rl-ap-fh"
	// FrameGEGradient is the GE gradient (logical) axes of (0019,xxBB..BD).
	FrameGEGradient DirectionFrame = "ge-gradient"
	// FrameUnspecified is a direction whose axes the source does not define
	// (Toshiba/Canon Image Comments).
	FrameUnspecified DirectionFrame = "unspecified"
)

// Frame is the diffusion weighting of one frame.
type Frame struct {
	// FrameIndex is the zero-based frame the values apply to.
	FrameIndex int
	// BValue is the b-value in s/mm².
	BValue float64
	// Direction is the gradient direction, in DirectionFrame axes, when
	// HasDirection; a zero vector (as b=0 images store) is not a direction.
	Direction      [3]float64
	HasDirection   bool
	DirectionFrame DirectionFrame
	// Directionality is Diffusion Directionality (0018,9075), or the Siemens
	// DiffusionDirectionality, in upper case: DIRECTIONAL, ISOTROPIC,
	// BMATRIX or NONE; empty when the source has none.
	Directionality string
	Source         Source
	// FunctionalGroup tells which functional group held the macro, for
	// SourceMRDiffusionMacro.
	FunctionalGroup object.FunctionalGroupSource
	// Tag is the attribute that held the b-value.
	Tag core.Tag
}

// IsDiffusionWeighted reports a positive b-value.
func (f Frame) IsDiffusionWeighted() bool { return f.BValue > 0 }

var (
	tagMRDiffusionSequence      = core.NewTag(0x0018, 0x9117)
	tagDiffusionBValue          = core.NewTag(0x0018, 0x9087)
	tagDiffusionDirectionality  = core.NewTag(0x0018, 0x9075)
	tagDiffusionGradientDirSeq  = core.NewTag(0x0018, 0x9076)
	tagDiffusionGradientOrient  = core.NewTag(0x0018, 0x9089)
	tagManufacturer             = core.NewTag(0x0008, 0x0070)
	tagImageComments            = core.NewTag(0x0020, 0x4000)
	siemensMRHeaderCreator      = "SIEMENS MR HEADER"
	geParmCreator               = "GEMS_PARM_01"
	geAcquCreator               = "GEMS_ACQU_01"
	philipsImagingCreators      = []string{"Philips Imaging DD 001", "PHILIPS IMAGING DD 001"}
	philipsMRImagingCreators    = []string{"Philips MR Imaging DD 001", "PHILIPS MR IMAGING DD 001"}
	canonImageCommentExpression = regexp.MustCompile(`(?i)\bb\s*=\s*([0-9]+(?:\.[0-9]*)?)\s*(?:\(\s*([-+0-9.eE]+)\s*,\s*([-+0-9.eE]+)\s*,\s*([-+0-9.eE]+)\s*\))?`)
)

// Read returns the diffusion weighting of the zero-based frameIndex of
// dataset (0 for a single-frame image), from the first source that has a
// b-value, in the order of the package documentation. It returns
// ErrNotFound when no source has one and ErrInvalid when the first source
// that has one holds an unusable value.
func Read(dataset *object.Object, frameIndex int) (Frame, error) {
	if dataset == nil {
		return Frame{}, ErrNotFound
	}
	for _, read := range []func(*object.Object, int) (Frame, bool, error){
		readMacro, readStandard, readSiemensCSA, readSiemensMRHeader, readGE, readPhilips, readCanonComments,
	} {
		frame, ok, err := read(dataset, frameIndex)
		if err != nil {
			return Frame{}, err
		}
		if ok {
			frame.FrameIndex = frameIndex
			return frame, nil
		}
	}
	return Frame{}, ErrNotFound
}

// ReadFrames returns Read for frames 0 to frames-1. A frame without a
// b-value has no entry; the first ErrInvalid stops the read.
func ReadFrames(dataset *object.Object, frames int) ([]Frame, error) {
	var out []Frame
	for index := 0; index < frames; index++ {
		frame, err := Read(dataset, index)
		if errors.Is(err, ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", index, err)
		}
		out = append(out, frame)
	}
	return out, nil
}

func readMacro(dataset *object.Object, frameIndex int) (Frame, bool, error) {
	item, source, _ := dataset.FunctionalGroupItem(frameIndex, tagMRDiffusionSequence)
	if item == nil || !item.Has(tagDiffusionBValue) {
		return Frame{}, false, nil
	}
	frame, err := standardFrame(item, SourceMRDiffusionMacro)
	frame.FunctionalGroup = source
	return frame, err == nil, err
}

func readStandard(dataset *object.Object, _ int) (Frame, bool, error) {
	if !dataset.Has(tagDiffusionBValue) {
		return Frame{}, false, nil
	}
	if values, _ := numbers(dataset, tagDiffusionBValue, core.VRFD); len(values) == 0 {
		// An empty Type 2 attribute is not a b-value.
		return Frame{}, false, nil
	}
	frame, err := standardFrame(dataset, SourceStandardAttributes)
	return frame, err == nil, err
}

// standardFrame reads the MR Diffusion Macro attributes of obj (a macro
// item, or the top level of a classic image).
func standardFrame(obj *object.Object, source Source) (Frame, error) {
	frame := Frame{Source: source, Tag: tagDiffusionBValue, DirectionFrame: FramePatient}
	b, err := firstNumber(obj, tagDiffusionBValue, core.VRFD)
	if err != nil {
		return Frame{}, err
	}
	if frame.BValue, err = checkB(b); err != nil {
		return Frame{}, err
	}
	frame.Directionality = upperString(obj, tagDiffusionDirectionality)
	direction := obj
	if items, ok := obj.GetSequence(tagDiffusionGradientDirSeq); ok && len(items) > 0 && items[0] != nil {
		direction = items[0]
	}
	values, err := numbers(direction, tagDiffusionGradientOrient, core.VRFD)
	if err != nil {
		return Frame{}, err
	}
	if len(values) >= 3 {
		if err := frame.setDirection(values[:3]); err != nil {
			return Frame{}, err
		}
	}
	return frame, nil
}

func readSiemensCSA(dataset *object.Object, _ int) (Frame, bool, error) {
	header, err := siemens.ImageHeader(dataset, siemens.LookupOptions{})
	if err != nil {
		return Frame{}, false, nil
	}
	element, ok := header.Element("B_value")
	if !ok || len(nonEmpty(element.Strings())) == 0 {
		return Frame{}, false, nil
	}
	values, err := element.Float64s()
	if err != nil || len(values) == 0 {
		return Frame{}, false, fmt.Errorf("%w: CSA B_value", ErrInvalid)
	}
	frame := Frame{Source: SourceSiemensCSA, Tag: header.Tag, DirectionFrame: FramePatient}
	if frame.BValue, err = checkB(values[0]); err != nil {
		return Frame{}, false, err
	}
	if element, ok := header.Element("DiffusionDirectionality"); ok && len(element.Values) > 0 {
		frame.Directionality = strings.ToUpper(strings.TrimSpace(element.Strings()[0]))
	}
	if element, ok := header.Element("DiffusionGradientDirection"); ok && len(element.Values) >= 3 {
		values, err := element.Float64s()
		if err != nil {
			return Frame{}, false, fmt.Errorf("%w: CSA DiffusionGradientDirection", ErrInvalid)
		}
		if err := frame.setDirection(values[:3]); err != nil {
			return Frame{}, false, err
		}
	}
	return frame, true, nil
}

func readSiemensMRHeader(dataset *object.Object, _ int) (Frame, bool, error) {
	tag, ok := privateTag(dataset, 0x0019, []string{siemensMRHeaderCreator}, 0x0C)
	if !ok {
		return Frame{}, false, nil
	}
	values, err := numbers(dataset, tag, core.VRIS)
	if err != nil {
		return Frame{}, false, err
	}
	if len(values) == 0 {
		return Frame{}, false, nil
	}
	frame := Frame{Source: SourceSiemensMRHeader, Tag: tag, DirectionFrame: FramePatient}
	if frame.BValue, err = checkB(values[0]); err != nil {
		return Frame{}, false, err
	}
	if tag, ok := privateTag(dataset, 0x0019, []string{siemensMRHeaderCreator}, 0x0D); ok {
		frame.Directionality = upperString(dataset, tag)
	}
	if tag, ok := privateTag(dataset, 0x0019, []string{siemensMRHeaderCreator}, 0x0E); ok {
		values, err := numbers(dataset, tag, core.VRFD)
		if err != nil {
			return Frame{}, false, err
		}
		if len(values) >= 3 {
			if err := frame.setDirection(values[:3]); err != nil {
				return Frame{}, false, err
			}
		}
	}
	return frame, true, nil
}

// geFlagOffset is added by some GE software to the b-value of (0043,xx39).
const geFlagOffset = 1_000_000_000

func readGE(dataset *object.Object, _ int) (Frame, bool, error) {
	tag, ok := privateTag(dataset, 0x0043, []string{geParmCreator}, 0x39)
	if !ok {
		return Frame{}, false, nil
	}
	values, err := numbers(dataset, tag, core.VRIS)
	if err != nil {
		return Frame{}, false, err
	}
	if len(values) == 0 {
		return Frame{}, false, nil
	}
	b := values[0]
	if b >= geFlagOffset {
		b = math.Mod(b, geFlagOffset)
	}
	frame := Frame{Source: SourceGE, Tag: tag, DirectionFrame: FrameGEGradient}
	if frame.BValue, err = checkB(b); err != nil {
		return Frame{}, false, err
	}
	direction, err := componentDirection(dataset, 0x0019, []string{geAcquCreator}, [3]uint8{0xBB, 0xBC, 0xBD}, core.VRDS)
	if err != nil {
		return Frame{}, false, err
	}
	if direction != nil {
		if err := frame.setDirection(direction); err != nil {
			return Frame{}, false, err
		}
	}
	return frame, true, nil
}

func readPhilips(dataset *object.Object, _ int) (Frame, bool, error) {
	tag, ok := privateTag(dataset, 0x2001, philipsImagingCreators, 0x03)
	if !ok {
		return Frame{}, false, nil
	}
	values, err := numbers(dataset, tag, core.VRFL)
	if err != nil {
		return Frame{}, false, err
	}
	if len(values) == 0 {
		return Frame{}, false, nil
	}
	frame := Frame{Source: SourcePhilips, Tag: tag, DirectionFrame: FramePhilipsRLAPFH}
	if frame.BValue, err = checkB(values[0]); err != nil {
		return Frame{}, false, err
	}
	direction, err := componentDirection(dataset, 0x2005, philipsMRImagingCreators, [3]uint8{0xB0, 0xB1, 0xB2}, core.VRFL)
	if err != nil {
		return Frame{}, false, err
	}
	if direction != nil {
		if err := frame.setDirection(direction); err != nil {
			return Frame{}, false, err
		}
	}
	return frame, true, nil
}

func readCanonComments(dataset *object.Object, _ int) (Frame, bool, error) {
	manufacturer, _ := dataset.GetString(tagManufacturer)
	if vendors.ManufacturerOfName(manufacturer) != vendors.ManufacturerCanon {
		return Frame{}, false, nil
	}
	comments, ok := dataset.GetString(tagImageComments)
	if !ok {
		return Frame{}, false, nil
	}
	match := canonImageCommentExpression.FindStringSubmatch(comments)
	if match == nil {
		return Frame{}, false, nil
	}
	b, err := strconv.ParseFloat(match[1], 64)
	if err != nil {
		return Frame{}, false, fmt.Errorf("%w: Image Comments b-value %q", ErrInvalid, match[1])
	}
	frame := Frame{Source: SourceCanonImageComments, Tag: tagImageComments, DirectionFrame: FrameUnspecified}
	if frame.BValue, err = checkB(b); err != nil {
		return Frame{}, false, err
	}
	if match[2] != "" {
		direction := make([]float64, 3)
		for i := range direction {
			if direction[i], err = strconv.ParseFloat(match[2+i], 64); err != nil {
				return Frame{}, false, fmt.Errorf("%w: Image Comments direction %q", ErrInvalid, match[2+i])
			}
		}
		if err := frame.setDirection(direction); err != nil {
			return Frame{}, false, err
		}
	}
	return frame, true, nil
}

func (f *Frame) setDirection(values []float64) error {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return fmt.Errorf("%w: gradient direction %v", ErrInvalid, values)
		}
	}
	if values[0] == 0 && values[1] == 0 && values[2] == 0 {
		return nil
	}
	f.Direction = [3]float64{values[0], values[1], values[2]}
	f.HasDirection = true
	return nil
}

func checkB(b float64) (float64, error) {
	if math.IsNaN(b) || math.IsInf(b, 0) || b < 0 {
		return 0, fmt.Errorf("%w: b-value %v", ErrInvalid, b)
	}
	return b, nil
}

// componentDirection reads a direction stored as three private attributes;
// nil when any of them is absent or empty.
func componentDirection(dataset *object.Object, group uint16, creators []string, offsets [3]uint8, vr core.VR) ([]float64, error) {
	direction := make([]float64, 3)
	for i, offset := range offsets {
		tag, ok := privateTag(dataset, group, creators, offset)
		if !ok {
			return nil, nil
		}
		values, err := numbers(dataset, tag, vr)
		if err != nil {
			return nil, err
		}
		if len(values) == 0 {
			return nil, nil
		}
		direction[i] = values[0]
	}
	return direction, nil
}

// privateTag locates a present private attribute through its creator.
func privateTag(dataset *object.Object, group uint16, creators []string, offset uint8) (core.Tag, bool) {
	for _, creator := range creators {
		tag, err := dataset.PrivateTag(group, creator, offset)
		if err == nil && dataset.Has(tag) {
			return tag, true
		}
	}
	return core.Tag{}, false
}

func firstNumber(obj *object.Object, tag core.Tag, vr core.VR) (float64, error) {
	values, err := numbers(obj, tag, vr)
	if err != nil {
		return 0, err
	}
	if len(values) == 0 {
		return 0, fmt.Errorf("%w: %s is empty", ErrInvalid, tag)
	}
	return values[0], nil
}

// numbers decodes a numeric attribute. Private attributes are often read
// without their VR (Implicit VR, or UN after an archive), so a UN, OB or
// unknown value is decoded as the expected VR want: binary FL or FD in the
// data set byte order, or backslash-separated text for DS and IS.
func numbers(obj *object.Object, tag core.Tag, want core.VR) ([]float64, error) {
	element, ok := obj.Get(tag)
	if !ok {
		return nil, nil
	}
	vr := element.VR()
	switch vr {
	case core.VRFL, core.VRFD, core.VRDS, core.VRIS:
	default:
		if vr.IsStringLike() {
			vr = core.VRDS
		} else {
			vr = want
		}
	}
	if (vr == core.VRFL || vr == core.VRFD) && element.VR() == vr {
		values, err := obj.GetFloats(tag)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, tag, err)
		}
		return finite(tag, values)
	}
	if vr == core.VRFL || vr == core.VRFD {
		raw, ok := obj.GetRaw(tag)
		if !ok {
			return nil, fmt.Errorf("%w: %s has no byte value", ErrInvalid, tag)
		}
		width := 4
		if vr == core.VRFD {
			width = 8
		}
		if len(raw)%width != 0 {
			return nil, fmt.Errorf("%w: %s has %d bytes for VR %s", ErrInvalid, tag, len(raw), vr)
		}
		order := obj.ValueByteOrder()
		values := make([]float64, len(raw)/width)
		for i := range values {
			if width == 4 {
				values[i] = float64(math.Float32frombits(order.Uint32(raw[4*i:])))
			} else {
				values[i] = math.Float64frombits(order.Uint64(raw[8*i:]))
			}
		}
		return finite(tag, values)
	}
	var texts []string
	if element.VR().IsStringLike() {
		lookedUp, err := obj.LookupStrings(tag)
		if err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalid, tag, err)
		}
		texts = lookedUp
	} else {
		raw, ok := obj.GetRaw(tag)
		if !ok {
			return nil, fmt.Errorf("%w: %s has no byte value", ErrInvalid, tag)
		}
		texts = strings.Split(string(raw), `\`)
	}
	var values []float64
	for _, text := range texts {
		text = strings.Trim(text, " \x00")
		if text == "" {
			continue
		}
		value, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return nil, fmt.Errorf("%w: %s value %q", ErrInvalid, tag, text)
		}
		values = append(values, value)
	}
	return finite(tag, values)
}

func finite(tag core.Tag, values []float64) ([]float64, error) {
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return nil, fmt.Errorf("%w: %s is not finite", ErrInvalid, tag)
		}
	}
	return values, nil
}

func upperString(obj *object.Object, tag core.Tag) string {
	value, _ := obj.GetString(tag)
	return strings.ToUpper(strings.Trim(value, " \x00"))
}

func nonEmpty(values []string) []string {
	out := values[:0:0]
	for _, value := range values {
		if strings.Trim(value, " \x00") != "" {
			out = append(out, value)
		}
	}
	return out
}
