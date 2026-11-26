package gsps

import (
	"fmt"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
)

// VOISelection selects the VOI transform FromImage copies from the image.
type VOISelection int

const (
	// VOIPreferLUT copies the first VOI LUT, or else the first window.
	VOIPreferLUT VOISelection = iota
	// VOIPreferWindow copies the first window, or else the first VOI LUT.
	VOIPreferWindow
	// VOIIgnore copies no VOI transform.
	VOIIgnore
)

// OverlayLayer is the graphic layer FromImage activates the image's
// overlays on.
const OverlayLayer = "OVERLAY"

// FromImageOptions tunes FromImage.
type FromImageOptions struct {
	// SOPInstanceUID and SeriesInstanceUID identify the presentation state;
	// empty values are generated.
	SOPInstanceUID    string
	SeriesInstanceUID string
	// VOI selects the image's VOI transform to copy.
	VOI VOISelection
	// IgnoreShutter leaves out the image's display shutter.
	IgnoreShutter bool
	// IgnoreOverlays leaves the image's overlay planes inactive.
	IgnoreOverlays bool
	// IgnorePresentationLUTShape leaves out the image's own Presentation
	// LUT Shape; the shape then follows the Photometric Interpretation.
	IgnorePresentationLUTShape bool
}

// FromImage builds the default presentation state of an image, as DCMTK
// dcmpsmk does: the state references the image and inherits its Patient and
// Study modules, displays the whole image at its pixel spacing or aspect
// ratio, copies its modality transform, its first VOI transform (see
// FromImageOptions.VOI) and its display shutter, activates its overlay
// planes on the OVERLAY layer and presents MONOCHROME1 images inverted.
// The state has no annotations; add them before Write.
func FromImage(image *object.Object, opts FromImageOptions) (*State, error) {
	if image == nil {
		return nil, fmt.Errorf("%w: image is nil", ErrInvalidObject)
	}
	ref := ReferencedImage{
		SeriesInstanceUID: derivedio.CleanUID(image, derivedio.TagSeriesInstanceUID),
		SOPClassUID:       derivedio.CleanUID(image, derivedio.TagSOPClassUID),
		SOPInstanceUID:    derivedio.CleanUID(image, derivedio.TagSOPInstanceUID),
	}
	studyUID := derivedio.CleanUID(image, derivedio.TagStudyInstanceUID)
	if studyUID == "" || ref.SeriesInstanceUID == "" || ref.SOPClassUID == "" || ref.SOPInstanceUID == "" {
		return nil, fmt.Errorf("%w: the image needs its study, series, SOP Class and SOP Instance UIDs", ErrMissingReference)
	}
	rows, columns := derivedio.Int(image, derivedio.TagRows), derivedio.Int(image, derivedio.TagColumns)
	if rows <= 0 || columns <= 0 {
		return nil, fmt.Errorf("%w: the image has no Rows and Columns", ErrInvalidObject)
	}
	if samples := derivedio.Int(image, derivedio.TagSamplesPerPixel); samples > 1 {
		return nil, fmt.Errorf("%w: a grayscale presentation state needs a grayscale image", ErrUnsupportedSOPClass)
	}
	state := &State{
		Common:            object.DerivedCommon{Source: image},
		SOPInstanceUID:    opts.SOPInstanceUID,
		StudyInstanceUID:  studyUID,
		SeriesInstanceUID: opts.SeriesInstanceUID,
		SeriesNumber:      1,
		ReferencedImages:  []ReferencedImage{ref},
	}
	for _, uid := range []*string{&state.SOPInstanceUID, &state.SeriesInstanceUID} {
		if *uid == "" {
			generated, err := newUID()
			if err != nil {
				return nil, err
			}
			*uid = generated
		}
	}
	switch laterality := derivedio.CleanString(image, tagLaterality); laterality {
	case "L", "R":
		state.Laterality = laterality
	}

	area := DisplayedArea{Defined: true, TopLeftX: 1, TopLeftY: 1, BottomRightX: columns, BottomRightY: rows, PresentationSizeMode: PresentationSizeScaleToFit}
	if spacing := imagePixelSpacing(image); spacing != nil {
		area.PixelSpacing = spacing
	} else if ratio := derivedio.Ints(image, tagPixelAspectRatio); len(ratio) == 2 && ratio[0] > 0 && ratio[1] > 0 {
		area.PixelAspectRatio = []int{int(ratio[0]), int(ratio[1])}
	}
	state.DisplayedArea = area

	if modality, err := imageModalityLUT(image); err != nil {
		return nil, err
	} else if modality != nil {
		state.ModalityLUT = modality
	}
	if opts.VOI != VOIIgnore {
		voi, err := imageVOI(image, opts.VOI)
		if err != nil {
			return nil, err
		}
		if voi != nil {
			state.SoftcopyVOI = *voi
		}
	}
	if !opts.IgnoreShutter {
		shutter, err := display.ShutterFromObject(image)
		if err != nil {
			return nil, fmt.Errorf("dicom/gsps: image shutter: %w", err)
		}
		state.Shutter = shutter
	}
	if !opts.IgnoreOverlays {
		overlays, _ := display.OverlaysFromObject(image)
		for _, overlay := range overlays {
			if state.Shutter != nil && state.Shutter.Bitmap != nil && state.Shutter.Bitmap.Group == overlay.Group {
				continue
			}
			state.OverlayActivations = append(state.OverlayActivations, OverlayActivation{Group: overlay.Group, Layer: OverlayLayer})
		}
	}
	shape := ""
	if !opts.IgnorePresentationLUTShape {
		switch value := strings.ToUpper(derivedio.CleanString(image, tagPresentationLUTShape)); value {
		case PresentationLUTIdentity, PresentationLUTInverse:
			shape = value
		}
	}
	if shape == "" {
		shape = PresentationLUTIdentity
		if strings.ToUpper(derivedio.CleanString(image, derivedio.TagPhotometricInterpretation)) == "MONOCHROME1" {
			shape = PresentationLUTInverse
		}
	}
	state.PresentationLUTShape = shape
	return state, nil
}

var (
	tagPixelSpacing       = core.NewTag(0x0028, 0x0030)
	tagImagerPixelSpacing = core.NewTag(0x0018, 0x1164)
	tagPixelAspectRatio   = core.NewTag(0x0028, 0x0034)
)

// imagePixelSpacing returns the image's Pixel Spacing, or else its Imager
// Pixel Spacing, when both values are positive.
func imagePixelSpacing(image *object.Object) []float64 {
	for _, tag := range []core.Tag{tagPixelSpacing, tagImagerPixelSpacing} {
		if values := derivedio.Floats(image, tag); len(values) == 2 && values[0] > 0 && values[1] > 0 {
			return values
		}
	}
	return nil
}

// imageModalityLUT copies the image's rescale or Modality LUT, which a
// presentation state must repeat (PS3.3 C.11.1).
func imageModalityLUT(image *object.Object) (*ModalityLUT, error) {
	modality, err := readModalityLUT(image)
	if err != nil || modality == nil {
		return modality, err
	}
	if modality.LUT == nil && modality.Slope == 0 {
		modality.Slope = 1
	}
	// dcmpsmk writes "US" (unspecified) for images without the types.
	if modality.LUT == nil && modality.RescaleType == "" {
		modality.RescaleType = "US"
	}
	if modality.LUT != nil && modality.LUTType == "" {
		modality.LUTType = "US"
	}
	return modality, nil
}

// imageVOI returns the image's first VOI LUT or window, as selection
// prefers.
func imageVOI(image *object.Object, selection VOISelection) (*SoftcopyVOI, error) {
	transforms, err := display.VOIsAt(image, 0)
	if err != nil && len(transforms) == 0 {
		return nil, fmt.Errorf("dicom/gsps: image VOI: %w", err)
	}
	var window, lut *display.VOILUT
	for i := range transforms {
		if transforms[i].LUT != nil && lut == nil {
			lut = &transforms[i]
		} else if transforms[i].LUT == nil && window == nil {
			window = &transforms[i]
		}
	}
	chosen := lut
	if selection == VOIPreferWindow && window != nil || chosen == nil {
		chosen = window
	}
	if chosen == nil {
		return nil, nil
	}
	return &SoftcopyVOI{
		WindowCenter: chosen.Center, WindowWidth: chosen.Width, Function: chosen.Function,
		LUT: chosen.LUT, Explanation: chosen.Explanation,
	}, nil
}

func newUID() (string, error) {
	return core.NewUID()
}
