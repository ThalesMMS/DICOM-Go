package object

import (
	"math"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
)

// Patient orientation labels (PS3.3 C.7.6.1.1.1) describe a direction in the
// DICOM patient coordinate system (LPS: +x toward the patient's Left, +y toward
// Posterior, +z toward Head) with the letters L/R, A/P and H/F. The helpers in
// this file derive such labels from Image Orientation (Patient) (0020,0037) or,
// for projection radiography without a patient-based frame, from Patient
// Orientation (0020,0020). They are pure functions: display code supplies its
// own rotation and mirroring through DisplayTransform.

// ObliquityThresholdCosine is the direction-cosine magnitude above which a
// row or column vector is considered aligned with a single patient axis when
// classifying the image plane (the conventional 0.8 threshold, about 36.9
// degrees).
const ObliquityThresholdCosine = 0.8

// DefaultOrientationMinComponent is the smallest normalized direction-cosine
// magnitude that contributes a letter to an oblique label. It only filters
// floating-point noise; any genuine obliquity produces a compound label.
const DefaultOrientationMinComponent = 1e-4

// MaxOrientationLetters is the maximum number of letters in one label (one per
// patient axis).
const MaxOrientationLetters = 3

// ImagePlane classifies an image plane relative to the patient axes.
type ImagePlane string

const (
	// ImagePlaneUnknown reports missing or invalid Image Orientation (Patient).
	ImagePlaneUnknown ImagePlane = ""
	// ImagePlaneAxial has row and column vectors along the L/R and A/P axes.
	ImagePlaneAxial ImagePlane = "AXIAL"
	// ImagePlaneCoronal has row and column vectors along the L/R and H/F axes.
	ImagePlaneCoronal ImagePlane = "CORONAL"
	// ImagePlaneSagittal has row and column vectors along the A/P and H/F axes.
	ImagePlaneSagittal ImagePlane = "SAGITTAL"
	// ImagePlaneOblique has a row or column vector without a dominant axis
	// (no cosine above ObliquityThresholdCosine).
	ImagePlaneOblique ImagePlane = "OBLIQUE"
)

// OrientationLabelOptions tunes label generation. The zero value selects the
// DICOM letters (H/F), up to three letters and DefaultOrientationMinComponent.
type OrientationLabelOptions struct {
	// MaxLetters limits each label; values outside 1..3 select 3.
	MaxLetters int
	// MinComponent ignores normalized components whose magnitude is not above
	// it; zero, negative or non-finite values select
	// DefaultOrientationMinComponent.
	MinComponent float64
	// SuperiorInferior writes S/I (the radiological display convention) in
	// place of the DICOM H/F letters, including for Patient Orientation input.
	SuperiorInferior bool
}

func (o OrientationLabelOptions) maxLetters() int {
	if o.MaxLetters < 1 || o.MaxLetters > MaxOrientationLetters {
		return MaxOrientationLetters
	}
	return o.MaxLetters
}

func (o OrientationLabelOptions) minComponent() float64 {
	if o.MinComponent <= 0 || math.IsNaN(o.MinComponent) || math.IsInf(o.MinComponent, 0) {
		return DefaultOrientationMinComponent
	}
	return o.MinComponent
}

// PatientDirectionLabel returns the orientation letters for a patient-space
// direction (LPS), ordered by decreasing weight: the axis with the largest
// normalized component first, then the next, while components stay above
// the MinComponent threshold. A pure axis gives one letter ("L"); an oblique
// direction gives a compound label ("LA", "LAH"). Exact ties are broken in
// x, y, z order, so a 45-degree direction still receives a label. A zero or
// non-finite vector returns "".
func PatientDirectionLabel(direction [3]float64, opts OrientationLabelOptions) string {
	unit, ok := normalizeDirection(direction)
	if !ok {
		return ""
	}
	axes := [3]int{0, 1, 2}
	sort.SliceStable(axes[:], func(i, j int) bool {
		return math.Abs(unit[axes[i]]) > math.Abs(unit[axes[j]])
	})
	minimum := opts.minComponent()
	var label strings.Builder
	for _, axis := range axes[:opts.maxLetters()] {
		value := unit[axis]
		if math.Abs(value) <= minimum {
			break
		}
		label.WriteByte(axisLetter(axis, value > 0, opts.SuperiorInferior))
	}
	return label.String()
}

// OppositeOrientationLabel returns the label of the opposite direction
// ("LA" -> "RP", "F" -> "H", "S" -> "I"). A label containing a character other
// than L, R, A, P, H, F, S or I returns "".
func OppositeOrientationLabel(label string) string {
	out := make([]byte, 0, len(label))
	for i := 0; i < len(label); i++ {
		opposite, ok := oppositeLetter(label[i])
		if !ok {
			return ""
		}
		out = append(out, opposite)
	}
	return string(out)
}

// MajorAxisLabel returns the single letter of the patient axis that dominates
// direction, or "" when no normalized component exceeds threshold (an oblique
// direction). A non-positive threshold selects ObliquityThresholdCosine.
func MajorAxisLabel(direction [3]float64, threshold float64, opts OrientationLabelOptions) string {
	unit, ok := normalizeDirection(direction)
	if !ok {
		return ""
	}
	if threshold <= 0 || math.IsNaN(threshold) || math.IsInf(threshold, 0) {
		threshold = ObliquityThresholdCosine
	}
	for axis := 0; axis < 3; axis++ {
		if math.Abs(unit[axis]) > threshold {
			return string(axisLetter(axis, unit[axis] > 0, opts.SuperiorInferior))
		}
	}
	return ""
}

// ClassifyImagePlane classifies Image Orientation (Patient) as axial, coronal,
// sagittal or oblique. Each of the row and column vectors must have a major
// axis (a cosine above ObliquityThresholdCosine) and the two major axes must
// differ; otherwise the plane is oblique. Missing, short, zero or non-finite
// input returns ImagePlaneUnknown.
func ClassifyImagePlane(imageOrientationPatient []float64) ImagePlane {
	row, column, ok := splitImageOrientation(imageOrientationPatient)
	if !ok {
		return ImagePlaneUnknown
	}
	rowAxis, rowOK := majorAxis(row)
	columnAxis, columnOK := majorAxis(column)
	if !rowOK || !columnOK || rowAxis == columnAxis {
		return ImagePlaneOblique
	}
	switch rowAxis + columnAxis {
	case 1: // x and y
		return ImagePlaneAxial
	case 2: // x and z
		return ImagePlaneCoronal
	default: // y and z
		return ImagePlaneSagittal
	}
}

// DisplayTransform describes how a viewer maps the stored image onto the
// screen. Screen x grows to the right and y grows downward.
type DisplayTransform struct {
	// RotationDegrees rotates the image clockwise on screen. Any angle is
	// accepted for Image Orientation (Patient) labels; Patient Orientation
	// labels require a multiple of 90 degrees.
	RotationDegrees float64
	// FlipHorizontal mirrors left and right; FlipVertical mirrors top and
	// bottom.
	FlipHorizontal bool
	FlipVertical   bool
	// FlipBeforeRotation applies the mirrors to the stored image before the
	// rotation. When false the mirrors act on the already rotated image, that
	// is, along the screen axes.
	FlipBeforeRotation bool
}

// OrientationEdgeLabels holds the patient-direction label shown at the middle
// of each screen edge: the direction pointing from the image centre toward
// that edge. An empty field means the direction is unknown.
type OrientationEdgeLabels struct {
	Top, Bottom, Left, Right string
}

// IsZero reports whether no edge has a label.
func (l OrientationEdgeLabels) IsZero() bool {
	return l == OrientationEdgeLabels{}
}

// EdgeLabelsForImageAxes returns the edge labels of an image whose column
// index grows along rowDirection and whose row index grows along
// columnDirection (the Image Orientation (Patient) convention), after the
// display transform. It reports false for zero or non-finite vectors.
func EdgeLabelsForImageAxes(rowDirection, columnDirection [3]float64, transform DisplayTransform, opts OrientationLabelOptions) (OrientationEdgeLabels, bool) {
	row, rowOK := normalizeDirection(rowDirection)
	column, columnOK := normalizeDirection(columnDirection)
	if !rowOK || !columnOK {
		return OrientationEdgeLabels{}, false
	}
	matrix, ok := transform.imageToScreen()
	if !ok {
		return OrientationEdgeLabels{}, false
	}
	label := func(screenX, screenY float64) string {
		// The transform is orthogonal, so its inverse is its transpose.
		imageX := matrix[0][0]*screenX + matrix[1][0]*screenY
		imageY := matrix[0][1]*screenX + matrix[1][1]*screenY
		return PatientDirectionLabel([3]float64{
			imageX*row[0] + imageY*column[0],
			imageX*row[1] + imageY*column[1],
			imageX*row[2] + imageY*column[2],
		}, opts)
	}
	return OrientationEdgeLabels{
		Top:    label(0, -1),
		Bottom: label(0, 1),
		Left:   label(-1, 0),
		Right:  label(1, 0),
	}, true
}

// EdgeLabelsForImageOrientation returns the edge labels for Image Orientation
// (Patient) (0020,0037) under the display transform.
func EdgeLabelsForImageOrientation(imageOrientationPatient []float64, transform DisplayTransform, opts OrientationLabelOptions) (OrientationEdgeLabels, bool) {
	row, column, ok := splitImageOrientation(imageOrientationPatient)
	if !ok {
		return OrientationEdgeLabels{}, false
	}
	return EdgeLabelsForImageAxes(row, column, transform, opts)
}

// EdgeLabelsForPatientOrientation returns the edge labels for Patient
// Orientation (0020,0020): the first value is the direction of the rows
// (toward the right edge of the untransformed image), the second the
// direction of the columns (toward the bottom edge). Values are combinations
// of A, P, R, L, H and F, most significant first. Opposite edges receive the
// opposite letters. One valid value is enough to label its two edges. The
// function reports false when neither value is usable or when the rotation is
// not a multiple of 90 degrees (the letters carry no angle to interpolate).
func EdgeLabelsForPatientOrientation(patientOrientation []string, transform DisplayTransform, opts OrientationLabelOptions) (OrientationEdgeLabels, bool) {
	var values [2]string
	usable := false
	for i := 0; i < 2 && i < len(patientOrientation); i++ {
		values[i] = normalizePatientOrientationValue(patientOrientation[i], opts)
		usable = usable || values[i] != ""
	}
	if !usable {
		return OrientationEdgeLabels{}, false
	}
	matrix, ok := transform.imageToScreen()
	if !ok || !isCardinalRotation(transform.RotationDegrees) {
		return OrientationEdgeLabels{}, false
	}
	label := func(screenX, screenY float64) string {
		imageX := math.Round(matrix[0][0]*screenX + matrix[1][0]*screenY)
		imageY := math.Round(matrix[0][1]*screenX + matrix[1][1]*screenY)
		switch {
		case imageX > 0:
			return values[0]
		case imageX < 0:
			return OppositeOrientationLabel(values[0])
		case imageY > 0:
			return values[1]
		case imageY < 0:
			return OppositeOrientationLabel(values[1])
		}
		return ""
	}
	return OrientationEdgeLabels{
		Top:    label(0, -1),
		Bottom: label(0, 1),
		Left:   label(-1, 0),
		Right:  label(1, 0),
	}, true
}

// MammographyPatientOrientation returns the Patient Orientation (0020,0020)
// that a mammogram has when stored in the conventional hanging orientation
// (chest wall on the outer side of the screen, breasts facing each other): the
// rows run toward the chest wall (P) for the right breast and toward the
// nipple (A) for the left breast; the columns run toward the opposite side for
// cranio-caudal views, toward the feet and the opposite side for
// medio-lateral oblique views and toward the feet for lateral views. It is a
// fallback for objects without Patient Orientation and reports false for
// unknown views or lateralities. viewPosition is View Position (0018,5101);
// laterality is Image Laterality (0020,0062) or Laterality (0020,0060).
func MammographyPatientOrientation(viewPosition, laterality string) ([]string, bool) {
	var row, side string
	switch strings.ToUpper(strings.TrimSpace(laterality)) {
	case "R":
		row, side = "P", "L"
	case "L":
		row, side = "A", "R"
	default:
		return nil, false
	}
	switch strings.ToUpper(strings.TrimSpace(viewPosition)) {
	case "CC", "XCC", "XCCL", "XCCM":
		return []string{row, side}, true
	case "MLO":
		return []string{row, "F" + side}, true
	case "ML", "LM":
		return []string{row, "F"}, true
	default:
		return nil, false
	}
}

// OrientationSource reports which attribute produced orientation labels.
type OrientationSource string

const (
	// OrientationSourceNone means no attribute allowed a label.
	OrientationSourceNone OrientationSource = ""
	// OrientationSourceImageOrientation is Image Orientation (Patient).
	OrientationSourceImageOrientation OrientationSource = "image-orientation-patient"
	// OrientationSourcePatientOrientation is Patient Orientation (0020,0020).
	OrientationSourcePatientOrientation OrientationSource = "patient-orientation"
	// OrientationSourceMammographyView is the conventional orientation derived
	// from View Position and laterality (see MammographyPatientOrientation).
	OrientationSourceMammographyView OrientationSource = "mammography-view"
)

// OrientationInfo gathers the attributes that determine patient orientation
// labels for one frame.
type OrientationInfo struct {
	// ImageOrientationPatient is (0020,0037), per frame when functional groups
	// define it.
	ImageOrientationPatient []float64
	// PatientOrientation is (0020,0020), or the Patient Orientation in Frame
	// Sequence (0020,9450) value of the frame when present.
	PatientOrientation []string
	// Modality is (0008,0060).
	Modality string
	// ViewPosition is (0018,5101).
	ViewPosition string
	// ImageLaterality is (0020,0062); Laterality is the series-level
	// (0020,0060) used when Image Laterality is absent.
	ImageLaterality string
	Laterality      string
}

var (
	tagPatientOrientation             = core.NewTag(0x0020, 0x0020)
	tagPatientOrientationInFrameSeq   = core.NewTag(0x0020, 0x9450)
	tagOrientationModality            = core.NewTag(0x0008, 0x0060)
	tagOrientationViewPosition        = core.NewTag(0x0018, 0x5101)
	tagOrientationImageLaterality     = core.NewTag(0x0020, 0x0062)
	tagOrientationSeriesLaterality    = core.NewTag(0x0020, 0x0060)
	tagOrientationFrameAnatomySeq     = core.NewTag(0x0020, 0x9071)
	tagOrientationFrameLateralityItem = core.NewTag(0x0020, 0x9072)
)

// OrientationInfoAt reads the orientation attributes for a zero-based frame.
// Image Orientation (Patient) follows FrameGeometryAt; Patient Orientation in
// Frame Sequence and Frame Laterality from the Frame Anatomy functional group
// override the top-level values when present.
func (o *Object) OrientationInfoAt(frameIndex int) OrientationInfo {
	if o == nil {
		return OrientationInfo{}
	}
	info := OrientationInfo{
		ImageOrientationPatient: o.FrameGeometryAt(frameIndex).ImageOrientationPatient,
	}
	if values, ok := o.GetStrings(tagPatientOrientation); ok {
		info.PatientOrientation = trimmedValues(values)
	}
	info.Modality = trimmedString(o, tagOrientationModality)
	info.ViewPosition = trimmedString(o, tagOrientationViewPosition)
	info.ImageLaterality = trimmedString(o, tagOrientationImageLaterality)
	info.Laterality = trimmedString(o, tagOrientationSeriesLaterality)
	if item, _, _ := o.FunctionalGroupItem(frameIndex, tagPatientOrientationInFrameSeq); item != nil {
		if values, ok := item.GetStrings(tagPatientOrientation); ok && len(trimmedValues(values)) > 0 {
			info.PatientOrientation = trimmedValues(values)
		}
	}
	if item, _, _ := o.FunctionalGroupItem(frameIndex, tagOrientationFrameAnatomySeq); item != nil {
		if laterality := trimmedString(item, tagOrientationFrameLateralityItem); laterality != "" {
			info.ImageLaterality = laterality
		}
	}
	return info
}

// OrientationInfoAt reads the orientation attributes for a zero-based frame of
// the file's dataset.
func (f *File) OrientationInfoAt(frameIndex int) OrientationInfo {
	if f == nil || f.Dataset == nil {
		return OrientationInfo{}
	}
	return f.Dataset.OrientationInfoAt(frameIndex)
}

// EdgeLabels returns the screen-edge labels and the attribute they came from.
// Image Orientation (Patient) takes precedence; otherwise Patient Orientation
// is used; otherwise, for MG only, the conventional orientation implied by View
// Position and laterality. OrientationSourceNone means no labels.
func (i OrientationInfo) EdgeLabels(transform DisplayTransform, opts OrientationLabelOptions) (OrientationEdgeLabels, OrientationSource) {
	if labels, ok := EdgeLabelsForImageOrientation(i.ImageOrientationPatient, transform, opts); ok {
		return labels, OrientationSourceImageOrientation
	}
	if labels, ok := EdgeLabelsForPatientOrientation(i.PatientOrientation, transform, opts); ok {
		return labels, OrientationSourcePatientOrientation
	}
	if strings.EqualFold(strings.TrimSpace(i.Modality), "MG") {
		laterality := strings.TrimSpace(i.ImageLaterality)
		if laterality == "" || strings.EqualFold(laterality, "B") || strings.EqualFold(laterality, "U") {
			laterality = i.Laterality
		}
		if orientation, ok := MammographyPatientOrientation(i.ViewPosition, laterality); ok {
			if labels, ok := EdgeLabelsForPatientOrientation(orientation, transform, opts); ok {
				return labels, OrientationSourceMammographyView
			}
		}
	}
	return OrientationEdgeLabels{}, OrientationSourceNone
}

// imageToScreen returns the orthogonal 2x2 matrix mapping an image
// displacement (column, row) to a screen displacement (x right, y down).
func (t DisplayTransform) imageToScreen() ([2][2]float64, bool) {
	if math.IsNaN(t.RotationDegrees) || math.IsInf(t.RotationDegrees, 0) {
		return [2][2]float64{}, false
	}
	cosine, sine := rotationCosineSine(t.RotationDegrees)
	// Clockwise on a y-down screen: (1,0) -> (cos, sin).
	rotation := [2][2]float64{{cosine, -sine}, {sine, cosine}}
	flip := [2][2]float64{{1, 0}, {0, 1}}
	if t.FlipHorizontal {
		flip[0][0] = -1
	}
	if t.FlipVertical {
		flip[1][1] = -1
	}
	if t.FlipBeforeRotation {
		return multiply2x2(rotation, flip), true
	}
	return multiply2x2(flip, rotation), true
}

func rotationCosineSine(degrees float64) (float64, float64) {
	normalized := math.Mod(degrees, 360)
	if normalized < 0 {
		normalized += 360
	}
	switch normalized {
	case 0:
		return 1, 0
	case 90:
		return 0, 1
	case 180:
		return -1, 0
	case 270:
		return 0, -1
	}
	radians := normalized * math.Pi / 180
	return math.Cos(radians), math.Sin(radians)
}

func isCardinalRotation(degrees float64) bool {
	normalized := math.Mod(degrees, 90)
	return normalized == 0
}

func multiply2x2(a, b [2][2]float64) [2][2]float64 {
	return [2][2]float64{
		{a[0][0]*b[0][0] + a[0][1]*b[1][0], a[0][0]*b[0][1] + a[0][1]*b[1][1]},
		{a[1][0]*b[0][0] + a[1][1]*b[1][0], a[1][0]*b[0][1] + a[1][1]*b[1][1]},
	}
}

func splitImageOrientation(values []float64) ([3]float64, [3]float64, bool) {
	if len(values) < 6 {
		return [3]float64{}, [3]float64{}, false
	}
	row, rowOK := normalizeDirection([3]float64{values[0], values[1], values[2]})
	column, columnOK := normalizeDirection([3]float64{values[3], values[4], values[5]})
	return row, column, rowOK && columnOK
}

func normalizeDirection(v [3]float64) ([3]float64, bool) {
	for _, component := range v {
		if math.IsNaN(component) || math.IsInf(component, 0) {
			return [3]float64{}, false
		}
	}
	length := math.Sqrt(v[0]*v[0] + v[1]*v[1] + v[2]*v[2])
	if length <= 1e-12 || math.IsInf(length, 0) {
		return [3]float64{}, false
	}
	return [3]float64{v[0] / length, v[1] / length, v[2] / length}, true
}

func majorAxis(unit [3]float64) (int, bool) {
	for axis := 0; axis < 3; axis++ {
		if math.Abs(unit[axis]) > ObliquityThresholdCosine {
			return axis, true
		}
	}
	return 0, false
}

func axisLetter(axis int, positive, superiorInferior bool) byte {
	switch axis {
	case 0:
		if positive {
			return 'L'
		}
		return 'R'
	case 1:
		if positive {
			return 'P'
		}
		return 'A'
	default:
		switch {
		case positive && superiorInferior:
			return 'S'
		case positive:
			return 'H'
		case superiorInferior:
			return 'I'
		default:
			return 'F'
		}
	}
}

func oppositeLetter(letter byte) (byte, bool) {
	switch letter {
	case 'L':
		return 'R', true
	case 'R':
		return 'L', true
	case 'A':
		return 'P', true
	case 'P':
		return 'A', true
	case 'H':
		return 'F', true
	case 'F':
		return 'H', true
	case 'S':
		return 'I', true
	case 'I':
		return 'S', true
	}
	return 0, false
}

// normalizePatientOrientationValue validates one Patient Orientation value:
// it must use only A, P, R, L, H and F, at most once per axis. The result is
// upper case, truncated to MaxLetters and uses S/I when requested.
func normalizePatientOrientationValue(value string, opts OrientationLabelOptions) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if value == "" {
		return ""
	}
	var seen [3]bool
	out := make([]byte, 0, len(value))
	for i := 0; i < len(value); i++ {
		letter := value[i]
		var axis int
		switch letter {
		case 'L', 'R':
			axis = 0
		case 'A', 'P':
			axis = 1
		case 'H', 'F':
			axis = 2
			if opts.SuperiorInferior {
				letter = axisLetter(axis, letter == 'H', true)
			}
		default:
			return ""
		}
		if seen[axis] {
			return ""
		}
		seen[axis] = true
		out = append(out, letter)
	}
	if len(out) > opts.maxLetters() {
		out = out[:opts.maxLetters()]
	}
	return string(out)
}

func trimmedValues(values []string) []string {
	out := make([]string, len(values))
	for i, value := range values {
		out[i] = strings.TrimSpace(value)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	return out
}

func trimmedString(o *Object, tag core.Tag) string {
	value, _ := o.GetString(tag)
	return strings.TrimSpace(value)
}
