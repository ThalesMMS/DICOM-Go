package printmgmt

import (
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"math"
	"strings"
)

const (
	PresentationLUTSOPClassUID = "1.2.840.10008.5.1.1.23"
	AnnotationBoxSOPClassUID   = "1.2.840.10008.5.1.1.15"
	MaxNativeLUTs              = 64
	MaxNativeAnnotationBoxes   = 64
	MaxNativeLUTBytes          = 512 << 10
)

var (
	TagPresentationLUTSequence         = core.NewTag(0x2050, 0x0010)
	TagPresentationLUTShape            = core.NewTag(0x2050, 0x0020)
	TagLUTDescriptor                   = core.NewTag(0x0028, 0x3002)
	TagLUTData                         = core.NewTag(0x0028, 0x3006)
	TagLUTExplanation                  = core.NewTag(0x0028, 0x3003)
	TagReferencedAnnotationBoxSequence = core.NewTag(0x2010, 0x0520)
	TagAnnotationPosition              = core.NewTag(0x2030, 0x0010)
	TagAnnotationText                  = core.NewTag(0x2030, 0x0020)
	TagSpecificCharacterSet            = core.NewTag(0x0008, 0x0005)
	ErrNativeUnsupported               = errors.New("printmgmt: requested native print objects are unavailable or unsupported")
)

// NativeBackend must affirm composition, not just DIMSE attribute acceptance.
// Presentation LUTs must be applied with the GSDF and the supplied density and
// viewing conditions. Annotation text must be composed at the configured slots.
// MemoryBackend captures objects only and does not implement this contract.
type NativeBackend interface {
	HonorsPresentationLUTs() bool
	HonorsBasicAnnotationBoxes() bool
}

// AnnotationFormat identifies printer-specific slots, never page coordinates.
type AnnotationFormat struct {
	ID    string
	Slots int
}

type NativeLimits struct {
	MaxPresentationLUTs int // default 16, ceiling 64
	MaxLUTBytes         int // association resident LUT data; default 128 KiB, ceiling 512 KiB
	MaxAnnotationBoxes  int // per film; default 16, ceiling 64
}

type NativeServiceOptions struct {
	PresentationLUT   bool
	AnnotationFormats []AnnotationFormat // at most 32; copied at NewService
	Limits            NativeLimits
}

func normalizeNativeOptions(backend Backend, p *NativeServiceOptions) (*NativeServiceOptions, error) {
	if p == nil {
		return nil, nil
	}
	out := *p
	capable, ok := backend.(NativeBackend)
	if (p.PresentationLUT || len(p.AnnotationFormats) > 0) && (!ok || (p.PresentationLUT && !capable.HonorsPresentationLUTs()) || (len(p.AnnotationFormats) > 0 && !capable.HonorsBasicAnnotationBoxes())) {
		return nil, ErrNativeUnsupported
	}
	l := &out.Limits
	if l.MaxPresentationLUTs == 0 {
		l.MaxPresentationLUTs = 16
	}
	if l.MaxLUTBytes == 0 {
		l.MaxLUTBytes = 128 << 10
	}
	if l.MaxAnnotationBoxes == 0 {
		l.MaxAnnotationBoxes = 16
	}
	if l.MaxPresentationLUTs < 1 || l.MaxPresentationLUTs > MaxNativeLUTs || l.MaxLUTBytes < 1 || l.MaxLUTBytes > MaxNativeLUTBytes || l.MaxAnnotationBoxes < 1 || l.MaxAnnotationBoxes > MaxNativeAnnotationBoxes || len(p.AnnotationFormats) > 32 {
		return nil, ErrInvalidAttributes
	}
	out.AnnotationFormats = append([]AnnotationFormat(nil), p.AnnotationFormats...)
	seen := map[string]bool{}
	for _, f := range out.AnnotationFormats {
		if !validNativeCS(f.ID) || f.ID == "" || f.Slots < 1 || f.Slots > l.MaxAnnotationBoxes || seen[f.ID] {
			return nil, invalid(TagAnnotationDisplayFormatID)
		}
		seen[f.ID] = true
	}
	return &out, nil
}

func validNativeCS(s string) bool {
	return s != "" && len(s) <= 16 && strings.Trim(s, " ") == s && physicalCSPattern.MatchString(s)
}

func validateNativeDensity(d display.PrintDensity) error {
	for _, v := range []float64{d.MinDensity, d.MaxDensity, d.Illumination, d.ReflectedAmbientLight} {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < 0 {
			return invalid(TagMinDensity, TagMaxDensity, TagIllumination, TagReflectedAmbientLight)
		}
	}
	if d.MaxDensity <= d.MinDensity || d.Illumination <= 0 || !(d.Luminance(d.MinDensity) > d.Luminance(d.MaxDensity)) || d.Luminance(d.MaxDensity) <= 0 {
		return invalid(TagMinDensity, TagMaxDensity, TagIllumination, TagReflectedAmbientLight)
	}
	return nil
}

// NativePrintDensity preserves supplied zero density/ambient light; invalid
// ranges are refused rather than replaced by a purported calibrated default.
func (p FilmBoxPresentation) NativePrintDensity() (display.PrintDensity, error) {
	for _, v := range []int{p.MinDensity, p.MaxDensity, p.Illumination, p.ReflectedAmbientLight} {
		if v < 0 || v > 65535 {
			return display.PrintDensity{}, invalid(TagMinDensity, TagMaxDensity, TagIllumination, TagReflectedAmbientLight)
		}
	}
	d := display.DefaultPrintDensity
	if p.MinDensitySet || p.MinDensity > 0 {
		d.MinDensity = float64(p.MinDensity) / 100
	}
	if p.MaxDensitySet || p.MaxDensity > 0 {
		d.MaxDensity = float64(p.MaxDensity) / 100
	}
	if p.IlluminationSet || p.Illumination > 0 {
		d.Illumination = float64(p.Illumination)
	}
	if p.ReflectedAmbientLightSet || p.ReflectedAmbientLight > 0 {
		d.ReflectedAmbientLight = float64(p.ReflectedAmbientLight)
	}
	return d, validateNativeDensity(d)
}

// AnnotationBox is a native text object owned by its parent Film Box. Text is
// independent of image pixels. Position refers to the configured format slot.
type AnnotationBox struct {
	SOPInstanceUID string
	Position       int
	Text           string
}
type Annotation struct {
	Position int
	Text     string
}
type AnnotationBoxReference struct {
	SOPInstanceUID string
	Position       int
}
type FilmBoxReferences struct {
	ImageBoxes      []ImageBoxReference
	AnnotationBoxes []AnnotationBoxReference
}

type PresentationLUTInstance struct {
	SOPInstanceUID string
	LUT            PresentationLUT
}

func (p PresentationLUTInstance) clone() PresentationLUTInstance { p.LUT = p.LUT.clone(); return p }

// NativeFilmOptions optionally creates one LUT referenced by the Film Box and
// sets independent annotation strings. Required objects always fail closed.
type NativeFilmOptions struct {
	PresentationLUT           *PresentationLUT
	AnnotationDisplayFormatID string
	Annotations               []Annotation
}

func (n NativeFilmOptions) Specified() bool {
	return n.PresentationLUT != nil || n.AnnotationDisplayFormatID != "" || len(n.Annotations) > 0
}
