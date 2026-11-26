package printmgmt

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
)

// Support is configured conformance information, not a negotiated DICOM capability.
type Support string

const (
	SupportUnknown     Support = ""
	SupportSupported   Support = "supported"
	SupportUnsupported Support = "unsupported"
)

func (s Support) Valid() bool {
	return s == SupportUnknown || s == SupportSupported || s == SupportUnsupported
}

// DecimateCropBehavior specifies the requested oversize policy (PS3.3 C.13.5).
type DecimateCropBehavior string

const (
	Decimate DecimateCropBehavior = "DECIMATE"
	Crop     DecimateCropBehavior = "CROP"
	Fail     DecimateCropBehavior = "FAIL"
)

const MaxPhysicalSizeMM = 10000

// ImageBoxPresentation contains optional per-image presentation attributes.
// RequestedWidthMM is the image's horizontal width in millimetres, not height,
// DPI, pixel spacing or a magnification factor. Zero and empty values omit tags.
type ImageBoxPresentation struct {
	RequestedWidthMM     float64
	DecimateCropBehavior DecimateCropBehavior
	MagnificationType    string
}

func (p ImageBoxPresentation) Specified() bool {
	return p.RequestedWidthMM != 0 || p.DecimateCropBehavior != "" || p.MagnificationType != ""
}
func (p ImageBoxPresentation) Validate() error {
	if p.RequestedWidthMM != 0 && (!finitePositive(p.RequestedWidthMM) || p.RequestedWidthMM > MaxPhysicalSizeMM) {
		return invalid(TagRequestedImageSize)
	}
	if p.DecimateCropBehavior != "" && p.DecimateCropBehavior != Decimate && p.DecimateCropBehavior != Crop && p.DecimateCropBehavior != Fail {
		return invalid(TagRequestedDecimateCrop)
	}
	if p.MagnificationType != "" && p.MagnificationType != "NONE" && p.MagnificationType != "REPLICATE" && p.MagnificationType != "BILINEAR" && p.MagnificationType != "CUBIC" {
		return invalid(TagMagnificationType)
	}
	return nil
}

// PhysicalCapabilities is an explicit per-printer conformance profile.
type PhysicalCapabilities struct{ RequestedSize, Decimate, Crop, Fail Support }

func (c PhysicalCapabilities) Validate() error {
	if !c.RequestedSize.Valid() || !c.Decimate.Valid() || !c.Crop.Valid() || !c.Fail.Valid() {
		return ErrInvalidAttributes
	}
	return nil
}
func (c PhysicalCapabilities) Behavior(b DecimateCropBehavior) Support {
	switch b {
	case Decimate:
		return c.Decimate
	case Crop:
		return c.Crop
	case Fail:
		return c.Fail
	}
	return SupportUnknown
}

// ImageBoxArea is the actual printable area of one box. NativePixelPitchMM,
// if known, is horizontal mm per source pixel without magnification. Source
// Pixel Aspect Ratio supplies vertical extent. No area is inferred from paper size.
type ImageBoxArea struct{ WidthMM, HeightMM, NativePixelPitchMM float64 }

func (a ImageBoxArea) Validate() error {
	if !finitePositive(a.WidthMM) || !finitePositive(a.HeightMM) || a.WidthMM > MaxPhysicalSizeMM || a.HeightMM > MaxPhysicalSizeMM || (a.NativePixelPitchMM != 0 && (!finitePositive(a.NativePixelPitchMM) || a.NativePixelPitchMM > MaxPhysicalSizeMM)) {
		return ErrPhysicalGeometry
	}
	return nil
}

var (
	ErrPhysicalGeometry    = errors.New("printmgmt: physical image geometry is unavailable or invalid")
	ErrPhysicalUnsupported = errors.New("printmgmt: requested physical presentation is not supported")
)

// PhysicalBackend must affirm that Print applies every resolved ImageBox.Plan,
// including centered crop or resampling using effective MagnificationType.
// Advertising optional DIMSE attributes alone is insufficient. An application
// must not affirm this contract for a backend that discards the plans.
type PhysicalBackend interface{ HonorsPhysicalImageBoxPlans() bool }

// PhysicalServiceOptions opts into resolved physical presentation. Area runs
// synchronously under the binding lock; it must be bounded, observe ctx, and
// not call back into the binding. The supplied film/layout/position are values.
// The backend must implement PhysicalBackend and explicitly affirm its contract.
type PhysicalServiceOptions struct {
	Capabilities PhysicalCapabilities
	Area         func(ctx context.Context, film FilmBoxPresentation, columns, rows, position int) (ImageBoxArea, error)
	// DefaultBehavior is used for an omitted tag; empty selects FAIL. No default
	// silently reduces a requested physical width unless explicitly configured.
	DefaultBehavior DecimateCropBehavior
}

// PhysicalPlan is an immutable-by-value presentation to compose when printing.
// SourceLeft/Top/Right/Bottom delimit the centered retained source interval in
// normalized image coordinates; the source pixels themselves are unchanged.
// Accepted width/height are the bounds of that retained image, in millimetres.
// Resolved means this geometry was accepted for processing, not physically measured.
type PhysicalPlan struct {
	Resolved                                         bool
	Outcome                                          DecimateCropBehavior // empty means the whole requested image fits
	RequestedWidthMM, RequestedHeightMM              float64
	AcceptedWidthMM, AcceptedHeightMM                float64
	SourceLeft, SourceTop, SourceRight, SourceBottom float64
	MagnificationType                                string
}

func finitePositive(v float64) bool { return v > 0 && !math.IsInf(v, 0) && !math.IsNaN(v) }

// PlanImageBox resolves the physical framing without reading or changing pixels.
// Empty requested width uses optimal fit when behavior is absent, or the known
// native horizontal pitch when behavior is specified or magnification is NONE.
// Oversized FAIL, and DECIMATE with NONE, wrap a C603 StatusError.
func PlanImageBox(rows, columns int, aspect [2]int, p ImageBoxPresentation, a ImageBoxArea, filmMagnification string, defaultBehavior DecimateCropBehavior) (PhysicalPlan, error) {
	var out PhysicalPlan
	if err := p.Validate(); err != nil {
		return out, err
	}
	if err := a.Validate(); err != nil {
		return out, err
	}
	if rows < 1 || columns < 1 || rows > 65535 || columns > 65535 {
		return out, ErrPhysicalGeometry
	}
	if aspect == [2]int{} {
		aspect = [2]int{1, 1}
	}
	if aspect[0] < 1 || aspect[1] < 1 || int64(aspect[0]) > 2147483647 || int64(aspect[1]) > 2147483647 {
		return out, ErrPhysicalGeometry
	}
	ratio := (float64(rows) / float64(columns)) * (float64(aspect[0]) / float64(aspect[1]))
	if !finitePositive(ratio) {
		return out, ErrPhysicalGeometry
	}
	magnification := p.MagnificationType
	if magnification == "" {
		magnification = filmMagnification
	}
	if magnification != "" && magnification != "NONE" && magnification != "REPLICATE" && magnification != "BILINEAR" && magnification != "CUBIC" {
		return out, invalid(TagMagnificationType)
	}
	width := p.RequestedWidthMM
	if width == 0 {
		if p.DecimateCropBehavior != "" || magnification == "NONE" {
			if a.NativePixelPitchMM == 0 {
				return out, ErrPhysicalGeometry
			}
			width = float64(columns) * a.NativePixelPitchMM
		} else {
			width = math.Min(a.WidthMM, a.HeightMM/ratio)
		}
	}
	height := width * ratio
	if !finitePositive(width) || !finitePositive(height) {
		return out, ErrPhysicalGeometry
	}
	out = PhysicalPlan{Resolved: true, RequestedWidthMM: width, RequestedHeightMM: height, AcceptedWidthMM: width, AcceptedHeightMM: height, SourceRight: 1, SourceBottom: 1, MagnificationType: magnification}
	if width <= a.WidthMM && height <= a.HeightMM {
		return out, nil
	}
	behavior := p.DecimateCropBehavior
	if behavior == "" {
		behavior = defaultBehavior
	}
	if behavior == "" {
		behavior = Fail
	}
	switch behavior {
	case Fail:
		return PhysicalPlan{}, &StatusError{Status: 0xC603, Tags: []core.Tag{TagRequestedImageSize, TagRequestedDecimateCrop}, Err: ErrPhysicalGeometry}
	case Decimate:
		if magnification == "NONE" {
			return PhysicalPlan{}, &StatusError{Status: 0xC603, Tags: []core.Tag{TagRequestedDecimateCrop, TagMagnificationType}, Err: ErrPhysicalGeometry}
		}
		factor := math.Min(a.WidthMM/width, a.HeightMM/height)
		out.AcceptedWidthMM, out.AcceptedHeightMM = width*factor, height*factor
	case Crop:
		// Cropping deletes whole source rows/columns. Floor keeps the result
		// inside the available area; an odd discarded pixel goes right/bottom.
		keepColumns := columns
		keepRows := rows
		if width > a.WidthMM {
			keepColumns = int(math.Floor(float64(columns) * (a.WidthMM / width)))
		}
		if height > a.HeightMM {
			keepRows = int(math.Floor(float64(rows) * (a.HeightMM / height)))
		}
		if keepColumns < 1 || keepRows < 1 {
			return PhysicalPlan{}, &StatusError{Status: 0xC603, Tags: []core.Tag{TagRequestedImageSize, TagRequestedDecimateCrop}, Err: ErrPhysicalGeometry}
		}
		left, top := (columns-keepColumns)/2, (rows-keepRows)/2
		out.SourceLeft, out.SourceTop = float64(left)/float64(columns), float64(top)/float64(rows)
		out.SourceRight, out.SourceBottom = float64(left+keepColumns)/float64(columns), float64(top+keepRows)/float64(rows)
		out.AcceptedWidthMM, out.AcceptedHeightMM = width*(float64(keepColumns)/float64(columns)), height*(float64(keepRows)/float64(rows))
	default:
		return PhysicalPlan{}, invalid(TagRequestedDecimateCrop)
	}
	if !finitePositive(out.AcceptedWidthMM) || !finitePositive(out.AcceptedHeightMM) {
		return PhysicalPlan{}, ErrPhysicalGeometry
	}
	out.Outcome = behavior
	return out, nil
}

var (
	physicalDSPattern = regexp.MustCompile(`^[+-]?(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)(?:[eE][+-]?[0-9]+)?$`)
	physicalISPattern = regexp.MustCompile(`^[+-]?[0-9]+$`)
	physicalCSPattern = regexp.MustCompile(`^[A-Z0-9_ ]*$`)
)

// residentTextValues bounds the original value before trimming ASCII spaces.
// Only resident raw/string payloads are accepted; arbitrary Value methods are
// not invoked. PS3.5 6.2 includes padding in the per-value length. For VM > 1,
// 6.4 permits one final SPACE to pad an odd whole field, even when that final
// component then exceeds the per-value limit by one byte.
func residentTextValues(ds *object.Object, tag core.Tag, vr core.VR, vm, limit int) ([]string, bool, error) {
	e, ok := ds.Get(tag)
	if !ok {
		return nil, false, nil
	}
	if e.Header.VR != vr {
		return nil, true, invalid(tag)
	}
	var values []string
	switch value := e.Value.(type) {
	case core.RawValue:
		maximum := vm*limit + vm - 1
		if vm > 1 {
			maximum++
		}
		if len(value) > maximum {
			return nil, true, invalid(tag)
		}
		values = strings.Split(string(value), `\`)
	case core.StringValue:
		values = value
	default:
		return nil, true, invalid(tag)
	}
	if len(values) != vm {
		return nil, true, invalid(tag)
	}
	total := vm - 1
	for _, value := range values {
		if len(value) > limit+1 || strings.Contains(value, `\`) {
			return nil, true, invalid(tag)
		}
		total += len(value)
	}
	out := make([]string, vm)
	for i, value := range values {
		if len(value) > limit && !(vm > 1 && i == vm-1 && len(value) == limit+1 && value[len(value)-1] == ' ' && total%2 == 0) {
			return nil, true, invalid(tag)
		}
		value = strings.Trim(value, " ")
		valid := false
		switch vr {
		case core.VRDS:
			valid = physicalDSPattern.MatchString(value)
		case core.VRIS:
			valid = physicalISPattern.MatchString(value)
		case core.VRCS:
			valid = physicalCSPattern.MatchString(value)
		}
		if !valid {
			return nil, true, invalid(tag)
		}
		out[i] = value
	}
	return out, true, nil
}

func presentationText(ds *object.Object, tag core.Tag, vr core.VR) (string, bool, error) {
	values, present, err := residentTextValues(ds, tag, vr, 1, 16)
	if err != nil || !present {
		return "", present, err
	}
	return values[0], true, nil
}
func parseImagePresentation(r *attributeReader, p ImageBoxPresentation) (ImageBoxPresentation, error) {
	for _, tag := range []core.Tag{TagRequestedImageSize, TagRequestedDecimateCrop, TagMagnificationType} {
		r.used[tag] = true
	}
	if value, ok, err := presentationText(r.ds, TagRequestedImageSize, core.VRDS); err != nil {
		return p, err
	} else if ok {
		n, err := strconv.ParseFloat(value, 64)
		if err != nil || !finitePositive(n) || n > MaxPhysicalSizeMM {
			return p, invalid(TagRequestedImageSize)
		}
		p.RequestedWidthMM = n
	}
	if value, ok, err := presentationText(r.ds, TagRequestedDecimateCrop, core.VRCS); err != nil {
		return p, err
	} else if ok {
		p.DecimateCropBehavior = DecimateCropBehavior(strings.Trim(value, " "))
	}
	if value, ok, err := presentationText(r.ds, TagMagnificationType, core.VRCS); err != nil {
		return p, err
	} else if ok {
		p.MagnificationType = strings.Trim(value, " ")
	}
	return p, p.Validate()
}

// BuildImageBoxWithPresentation extends BuildImageBox without changing pixels.
func BuildImageBoxWithPresentation(position int, img *Image, polarity string, p ImageBoxPresentation) (*object.Object, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	ds, err := BuildImageBox(position, img, polarity)
	if err != nil {
		return nil, err
	}
	if p.RequestedWidthMM != 0 {
		value := strconv.FormatFloat(p.RequestedWidthMM, 'g', -1, 64)
		if len(value) > 16 {
			value = strconv.FormatFloat(p.RequestedWidthMM, 'g', 9, 64)
		}
		ds.Put(text(TagRequestedImageSize, core.VRDS, value))
	}
	if p.DecimateCropBehavior != "" {
		ds.Put(text(TagRequestedDecimateCrop, core.VRCS, string(p.DecimateCropBehavior)))
	}
	if p.MagnificationType != "" {
		ds.Put(text(TagMagnificationType, core.VRCS, p.MagnificationType))
	}
	return ds, nil
}
