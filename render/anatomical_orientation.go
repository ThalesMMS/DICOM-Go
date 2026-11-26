package render

import (
	"image"
	"image/color"
	"math"
)

// Anatomical plane assignment for MPR views.
//
// A volume's index axes (columns, rows, slices) follow the acquisition, so the
// orthogonal MPRPlane names describe the stack rather than the anatomy: the
// "axial" plane of a sagittal acquisition is sagittal. The helpers below map
// each anatomical plane to the index plane nearest to it and to the exact
// pixel permutation that presents that plane in the radiological convention.
//
// The assignment is greedy on the global maximum, the behaviour of ITK's
// AnatomicalOrientation::ConvertDirectionToPositiveEnum: the largest absolute
// direction cosine among all (image axis, patient axis) pairs fixes one axis
// pair, both are removed, and the process repeats. Unlike an independent
// argmax per vector, two image axes never collapse onto the same patient axis,
// even for a 45 degree oblique acquisition. The permutation and mirroring
// without resampling follow itk::OrientImageFilter. This is an independent Go
// implementation of that behaviour; no ITK source is copied.

// PatientAxis identifies one axis of the DICOM patient coordinate system
// (LPS): x grows toward the patient's left, y toward posterior and z toward
// the head.
type PatientAxis int

const (
	PatientAxisX PatientAxis = iota // right to left
	PatientAxisY                    // anterior to posterior
	PatientAxisZ                    // feet to head
)

// Unit returns the positive unit direction of the patient axis.
func (a PatientAxis) Unit() Vec3 {
	switch a {
	case PatientAxisX:
		return Vec3{X: 1}
	case PatientAxisY:
		return Vec3{Y: 1}
	case PatientAxisZ:
		return Vec3{Z: 1}
	default:
		return Vec3{}
	}
}

// AnatomicalAxis is the patient axis assigned to one image axis. Negative
// reports that the image axis points toward the negative end of that patient
// axis (right, anterior or feet).
type AnatomicalAxis struct {
	Axis     PatientAxis
	Negative bool
}

// Direction returns the signed patient unit vector of the assignment.
func (a AnatomicalAxis) Direction() Vec3 {
	if a.Negative {
		return a.Axis.Unit().Scale(-1)
	}
	return a.Axis.Unit()
}

// anatomicalAxesDegenerateDeterminant rejects image axes that do not span
// patient space (parallel or nearly parallel directions).
const anatomicalAxesDegenerateDeterminant = 1e-6

// NearestAnatomicalAxes assigns a distinct patient axis to each of three
// image axes (for a volume: column, row and slice directions) by the greedy
// global-maximum rule described above. Exact ties resolve toward the lower
// image axis and then the lower patient axis, so the result is deterministic.
// The input vectors need not be unit length. It reports false when a vector
// is zero or non-finite or the three vectors do not span patient space.
func NearestAnatomicalAxes(axes [3]Vec3) ([3]AnatomicalAxis, bool) {
	var unit [3][3]float64
	for index, axis := range axes {
		normalized, ok := finiteUnit(axis)
		if !ok {
			return [3]AnatomicalAxis{}, false
		}
		unit[index] = [3]float64{normalized.X, normalized.Y, normalized.Z}
	}
	determinant := unit[0][0]*(unit[1][1]*unit[2][2]-unit[1][2]*unit[2][1]) -
		unit[0][1]*(unit[1][0]*unit[2][2]-unit[1][2]*unit[2][0]) +
		unit[0][2]*(unit[1][0]*unit[2][1]-unit[1][1]*unit[2][0])
	if math.Abs(determinant) <= anatomicalAxesDegenerateDeterminant {
		return [3]AnatomicalAxis{}, false
	}
	var result [3]AnatomicalAxis
	var imageUsed, patientUsed [3]bool
	for step := 0; step < 3; step++ {
		bestImage, bestPatient, best := -1, -1, -1.0
		for imageAxis := 0; imageAxis < 3; imageAxis++ {
			if imageUsed[imageAxis] {
				continue
			}
			for patient := 0; patient < 3; patient++ {
				if patientUsed[patient] {
					continue
				}
				if magnitude := math.Abs(unit[imageAxis][patient]); magnitude > best {
					bestImage, bestPatient, best = imageAxis, patient, magnitude
				}
			}
		}
		imageUsed[bestImage], patientUsed[bestPatient] = true, true
		result[bestImage] = AnatomicalAxis{Axis: PatientAxis(bestPatient), Negative: unit[bestImage][bestPatient] < 0}
	}
	return result, true
}

// AxisReorientation permutes and mirrors three image axes into patient order
// without resampling: output axis j is input axis Order[j], reversed when
// Flip[j], so that it points toward the positive end of patient axis j (L, P,
// H). It is the index bookkeeping of itk::OrientImageFilter for an LPS target.
type AxisReorientation struct {
	Order [3]int
	Flip  [3]bool
}

// ReorientedAxes returns the permutation and mirrors that bring axes closest
// to the patient axes, using NearestAnatomicalAxes for the assignment.
func ReorientedAxes(axes [3]Vec3) (AxisReorientation, bool) {
	assignment, ok := NearestAnatomicalAxes(axes)
	if !ok {
		return AxisReorientation{}, false
	}
	var reorientation AxisReorientation
	for imageAxis, anatomical := range assignment {
		reorientation.Order[anatomical.Axis] = imageAxis
		reorientation.Flip[anatomical.Axis] = anatomical.Negative
	}
	return reorientation, true
}

// Axes applies the reorientation to axes, returning the direction of each
// output axis.
func (r AxisReorientation) Axes(axes [3]Vec3) [3]Vec3 {
	var out [3]Vec3
	for output := range out {
		axis := axes[r.Order[output]]
		if r.Flip[output] {
			axis = axis.Scale(-1)
		}
		out[output] = axis
	}
	return out
}

// ImageOrientation is one of the eight exact symmetries of an image grid: an
// optional transpose followed by optional mirrors of the resulting columns
// (FlipHorizontal) and rows (FlipVertical). Applying it permutes pixels and
// never resamples. The zero value is the identity.
type ImageOrientation struct {
	Transpose      bool
	FlipHorizontal bool
	FlipVertical   bool
}

// IsIdentity reports whether the orientation leaves the image unchanged.
func (o ImageOrientation) IsIdentity() bool {
	return o == ImageOrientation{}
}

// Inverse returns the orientation that undoes o.
func (o ImageOrientation) Inverse() ImageOrientation {
	if !o.Transpose {
		return o
	}
	return ImageOrientation{Transpose: true, FlipHorizontal: o.FlipVertical, FlipVertical: o.FlipHorizontal}
}

// Size returns the oriented size of a w×h image.
func (o ImageOrientation) Size(w, h int) (int, int) {
	if o.Transpose {
		return h, w
	}
	return w, h
}

// Point maps a pixel-centre coordinate of a w×h source image (0..w-1,
// 0..h-1) to the oriented image.
func (o ImageOrientation) Point(x, y float64, w, h int) (float64, float64) {
	if o.Transpose {
		x, y = y, x
	}
	outW, outH := o.Size(w, h)
	if o.FlipHorizontal {
		x = float64(outW-1) - x
	}
	if o.FlipVertical {
		y = float64(outH-1) - y
	}
	return x, y
}

// SourcePoint is the inverse of Point: it maps a pixel-centre coordinate of
// the oriented image back to the w×h source image.
func (o ImageOrientation) SourcePoint(x, y float64, w, h int) (float64, float64) {
	outW, outH := o.Size(w, h)
	if o.FlipHorizontal {
		x = float64(outW-1) - x
	}
	if o.FlipVertical {
		y = float64(outH-1) - y
	}
	if o.Transpose {
		x, y = y, x
	}
	return x, y
}

// Fraction maps a fractional image position (0 at the left/top edge, 1 at the
// right/bottom edge) of the source image to the oriented image.
func (o ImageOrientation) Fraction(fx, fy float64) (float64, float64) {
	if o.Transpose {
		fx, fy = fy, fx
	}
	if o.FlipHorizontal {
		fx = 1 - fx
	}
	if o.FlipVertical {
		fy = 1 - fy
	}
	return fx, fy
}

// SourceFraction is the inverse of Fraction.
func (o ImageOrientation) SourceFraction(fx, fy float64) (float64, float64) {
	if o.FlipHorizontal {
		fx = 1 - fx
	}
	if o.FlipVertical {
		fy = 1 - fy
	}
	if o.Transpose {
		fx, fy = fy, fx
	}
	return fx, fy
}

// Axes maps the patient directions of the source image x (toward the right
// edge) and y (toward the bottom edge) axes to those of the oriented image.
func (o ImageOrientation) Axes(right, down Vec3) (Vec3, Vec3) {
	if o.Transpose {
		right, down = down, right
	}
	if o.FlipHorizontal {
		right = right.Scale(-1)
	}
	if o.FlipVertical {
		down = down.Scale(-1)
	}
	return right, down
}

// Plane maps the reslice plane of the source image to the plane of the
// oriented image, so that sampling the returned plane reproduces the oriented
// pixels.
func (o ImageOrientation) Plane(plane Plane) Plane {
	if o.Transpose {
		plane.U, plane.V = plane.V, plane.U
	}
	if o.FlipHorizontal {
		plane.Origin = plane.Origin.Add(plane.U)
		plane.U = plane.U.Scale(-1)
	}
	if o.FlipVertical {
		plane.Origin = plane.Origin.Add(plane.V)
		plane.V = plane.V.Scale(-1)
	}
	return plane
}

// Image returns img with its pixels permuted by o. The identity returns img
// itself. Gray, Gray16, RGBA and NRGBA images keep their type; other images
// are converted to NRGBA.
func (o ImageOrientation) Image(img image.Image) image.Image {
	if img == nil || o.IsIdentity() {
		return img
	}
	bounds := img.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	outW, outH := o.Size(w, h)
	rect := image.Rect(0, 0, outW, outH)
	sourceAt := func(x, y int) (int, int) {
		sx, sy := o.SourcePoint(float64(x), float64(y), w, h)
		return bounds.Min.X + int(sx), bounds.Min.Y + int(sy)
	}
	switch src := img.(type) {
	case *image.Gray:
		out := image.NewGray(rect)
		for y := 0; y < outH; y++ {
			for x := 0; x < outW; x++ {
				sx, sy := sourceAt(x, y)
				out.Pix[y*out.Stride+x] = src.Pix[src.PixOffset(sx, sy)]
			}
		}
		return out
	case *image.Gray16:
		out := image.NewGray16(rect)
		for y := 0; y < outH; y++ {
			for x := 0; x < outW; x++ {
				sx, sy := sourceAt(x, y)
				offset := src.PixOffset(sx, sy)
				copy(out.Pix[y*out.Stride+2*x:y*out.Stride+2*x+2], src.Pix[offset:offset+2])
			}
		}
		return out
	case *image.RGBA:
		out := image.NewRGBA(rect)
		for y := 0; y < outH; y++ {
			for x := 0; x < outW; x++ {
				sx, sy := sourceAt(x, y)
				offset := src.PixOffset(sx, sy)
				copy(out.Pix[y*out.Stride+4*x:y*out.Stride+4*x+4], src.Pix[offset:offset+4])
			}
		}
		return out
	case *image.NRGBA:
		out := image.NewNRGBA(rect)
		for y := 0; y < outH; y++ {
			for x := 0; x < outW; x++ {
				sx, sy := sourceAt(x, y)
				offset := src.PixOffset(sx, sy)
				copy(out.Pix[y*out.Stride+4*x:y*out.Stride+4*x+4], src.Pix[offset:offset+4])
			}
		}
		return out
	default:
		out := image.NewNRGBA(rect)
		for y := 0; y < outH; y++ {
			for x := 0; x < outW; x++ {
				sx, sy := sourceAt(x, y)
				out.Set(x, y, color.NRGBAModel.Convert(img.At(sx, sy)))
			}
		}
		return out
	}
}

// AnatomicalPlane names the anatomical plane an MPR view presents.
type AnatomicalPlane string

const (
	AnatomicalAxial    AnatomicalPlane = "axial"
	AnatomicalCoronal  AnatomicalPlane = "coronal"
	AnatomicalSagittal AnatomicalPlane = "sagittal"
)

// AnatomicalPlaneOrder returns the three anatomical planes in the conventional
// axial, coronal, sagittal order used by AnatomicalMPRViews.
func AnatomicalPlaneOrder() [3]AnatomicalPlane {
	return [3]AnatomicalPlane{AnatomicalAxial, AnatomicalCoronal, AnatomicalSagittal}
}

// NormalAxis returns the patient axis perpendicular to the plane.
func (p AnatomicalPlane) NormalAxis() (PatientAxis, bool) {
	switch p {
	case AnatomicalAxial:
		return PatientAxisZ, true
	case AnatomicalCoronal:
		return PatientAxisY, true
	case AnatomicalSagittal:
		return PatientAxisX, true
	default:
		return 0, false
	}
}

// RadiologicalImageAxes returns the patient directions of the screen right
// and down axes in the radiological display convention: axial viewed from the
// feet (patient left on screen right, posterior down), coronal viewed from the
// front (left on the right, feet down) and sagittal viewed from the patient's
// left (posterior on the right, feet down).
func RadiologicalImageAxes(p AnatomicalPlane) (right, down Vec3, ok bool) {
	switch p {
	case AnatomicalAxial:
		return Vec3{X: 1}, Vec3{Y: 1}, true
	case AnatomicalCoronal:
		return Vec3{X: 1}, Vec3{Z: -1}, true
	case AnatomicalSagittal:
		return Vec3{Y: 1}, Vec3{Z: -1}, true
	default:
		return Vec3{}, Vec3{}, false
	}
}

// AnatomicalMPRView assigns an anatomical plane to the orthogonal index plane
// that renders it. Orientation is the exact pixel permutation to apply to the
// RenderMPRPlane output (and, through ImageOrientation.Plane, to
// Volume.OrthogonalPlane) so the image follows the radiological convention.
type AnatomicalMPRView struct {
	Anatomical  AnatomicalPlane
	Plane       MPRPlane
	Orientation ImageOrientation
}

// IndexMPRViews is the fallback assignment for a stack without usable
// orientation: each anatomical name keeps the index plane of the same name
// and no permutation.
func IndexMPRViews() [3]AnatomicalMPRView {
	return [3]AnatomicalMPRView{
		{Anatomical: AnatomicalAxial, Plane: MPRPlaneAxial},
		{Anatomical: AnatomicalCoronal, Plane: MPRPlaneCoronal},
		{Anatomical: AnatomicalSagittal, Plane: MPRPlaneSagittal},
	}
}

// AnatomicalMPRViews returns the axial, coronal and sagittal views (in that
// order) of a stack whose columns run along axisX and rows along axisY, with
// slices stacked along axisX × axisY as RenderMPRPlane assumes. Each
// anatomical plane is rendered by the index plane whose normal the greedy
// assignment gives to that plane's patient axis, and is permuted and mirrored
// so its screen axes are the nearest to RadiologicalImageAxes. An axial
// acquisition in the standard supine, head-first orientation needs no
// permutation. It reports false for degenerate axes.
func AnatomicalMPRViews(axisX, axisY Vec3) ([3]AnatomicalMPRView, bool) {
	axisX, axisY = axisX.Normalize(), axisY.Normalize()
	normal := axisX.Cross(axisY).Normalize()
	assignment, ok := NearestAnatomicalAxes([3]Vec3{axisX, axisY, normal})
	if !ok {
		return IndexMPRViews(), false
	}
	// indexPlanes lists the index plane whose normal is volume axis k.
	indexPlanes := [3]MPRPlane{MPRPlaneSagittal, MPRPlaneCoronal, MPRPlaneAxial}
	var views [3]AnatomicalMPRView
	for viewIndex, anatomical := range AnatomicalPlaneOrder() {
		normalAxis, _ := anatomical.NormalAxis()
		volumeAxis := 0
		for axis := range assignment {
			if assignment[axis].Axis == normalAxis {
				volumeAxis = axis
			}
		}
		plane := indexPlanes[volumeAxis]
		right, down, _ := MPRPlaneImageAxes(plane, axisX, axisY)
		views[viewIndex] = AnatomicalMPRView{
			Anatomical:  anatomical,
			Plane:       plane,
			Orientation: radiologicalOrientation(anatomical, right, down, assignment, [3]Vec3{axisX, axisY, normal}),
		}
	}
	return views, true
}

// radiologicalOrientation chooses the image permutation for an index plane
// whose native screen axes are right and down (each ± one volume axis) using
// the patient axes the greedy assignment gave those volume axes, so the
// choice stays consistent with the plane assignment at exact 45 degree ties.
func radiologicalOrientation(anatomical AnatomicalPlane, right, down Vec3, assignment [3]AnatomicalAxis, volumeAxes [3]Vec3) ImageOrientation {
	targetRight, targetDown, _ := RadiologicalImageAxes(anatomical)
	patientDirection := func(vector Vec3) Vec3 {
		best, bestDot := 0, 0.0
		for axis, volumeAxis := range volumeAxes {
			if dot := vector.Dot(volumeAxis); math.Abs(dot) > math.Abs(bestDot) {
				best, bestDot = axis, dot
			}
		}
		direction := assignment[best].Direction()
		if bestDot < 0 {
			direction = direction.Scale(-1)
		}
		return direction
	}
	right, down = patientDirection(right), patientDirection(down)
	var orientation ImageOrientation
	if right.Dot(targetRight) == 0 {
		orientation.Transpose = true
		right, down = down, right
	}
	orientation.FlipHorizontal = right.Dot(targetRight) < 0
	orientation.FlipVertical = down.Dot(targetDown) < 0
	return orientation
}

// MPRViewForPlane returns the view in views that renders the index plane.
func MPRViewForPlane(views [3]AnatomicalMPRView, plane MPRPlane) (AnatomicalMPRView, bool) {
	for _, view := range views {
		if view.Plane == plane {
			return view, true
		}
	}
	return AnatomicalMPRView{}, false
}

// MPRViewForAnatomical returns the view in views that presents anatomical.
func MPRViewForAnatomical(views [3]AnatomicalMPRView, anatomical AnatomicalPlane) (AnatomicalMPRView, bool) {
	for _, view := range views {
		if view.Anatomical == anatomical {
			return view, true
		}
	}
	return AnatomicalMPRView{}, false
}

func finiteUnit(v Vec3) (Vec3, bool) {
	if math.IsNaN(v.X) || math.IsNaN(v.Y) || math.IsNaN(v.Z) ||
		math.IsInf(v.X, 0) || math.IsInf(v.Y, 0) || math.IsInf(v.Z, 0) {
		return Vec3{}, false
	}
	unit := v.Normalize()
	if unit == (Vec3{}) {
		return Vec3{}, false
	}
	return unit, true
}
