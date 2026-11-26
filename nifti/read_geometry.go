package nifti

import (
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// XformPolicy selects which NIfTI spatial transform a reader uses.
type XformPolicy uint8

const (
	// XformAuto applies the documented precedence (see NIFTI_IMPORT.md): an
	// orthonormal sform wins when there is no usable qform, when its code is
	// scanner-anatomical, or when it agrees with the qform; otherwise a valid
	// qform wins; a non-orthogonal sform is used only in permissive mode.
	XformAuto XformPolicy = iota
	// XformQForm requires and uses the qform.
	XformQForm
	// XformSForm requires and uses the sform.
	XformSForm
)

// XformSource names the transform a Geometry was derived from.
type XformSource string

const (
	XformSourceQForm               XformSource = "qform"
	XformSourceSForm               XformSource = "sform"
	XformSourceSFormOrthogonalized XformSource = "sform-orthogonalized"
	XformSourcePixDim              XformSource = "pixdim"
)

// Stable, PHI-free warning codes reported in Geometry.Warnings and
// Volume.Warnings.
const (
	WarningQFormInvalid          = "qform-invalid-ignored"
	WarningSFormIgnored          = "sform-disagrees-with-qform-ignored"
	WarningSFormNonOrthogonal    = "sform-non-orthogonal-ignored"
	WarningSFormOrthogonalized   = "sform-non-orthogonal-orthogonalized"
	WarningSFormPixDimMismatch   = "sform-pixdim-mismatch"
	WarningNoSpatialTransform    = "no-spatial-transform-pixdim-used"
	WarningVoxelOffsetAdjusted   = "vox-offset-below-header-adjusted"
	WarningBitPixMismatch        = "bitpix-datatype-mismatch"
	WarningTrailingData          = "trailing-data-ignored"
	WarningNonScannerCoordinates = "non-scanner-coordinate-system"
)

const (
	// orthonormalTolerance bounds |cos| between normalized sform columns, the
	// same 1e-4 tolerance ITK applies to R*R^T.
	orthonormalTolerance = 1e-4
	// singularTolerance is the scale-free invertibility bound: the determinant
	// of the column-normalized matrix. Tiny voxels (ITK 6278c4171f) do not
	// shrink it, unlike an absolute determinant threshold.
	singularTolerance = 1e-6
	// quaternionSnap zeroes rotation coefficients below 2^-22 that float32
	// quaternion storage leaves behind for axis-aligned volumes.
	quaternionSnap = 1.0 / (1 << 22)
	// DefaultPermissiveMaxDeviationDegrees bounds how far a non-orthogonal
	// sform column may lie from its orthogonalized direction.
	DefaultPermissiveMaxDeviationDegrees = 10.0
)

// Geometry is the resolved voxel-to-patient mapping of a NIfTI volume. All
// coordinates are millimeters: xyzt_units meters and micrometers are scaled.
// IndexToRAS maps a zero-based (i, j, k) voxel index to NIfTI RAS+ world
// coordinates; IndexToLPS maps it to the DICOM patient coordinate system
// (the same affine premultiplied by diag(-1,-1,1,1)).
type Geometry struct {
	Source     XformSource
	Code       XformCode
	QFormCode  XformCode
	SFormCode  XformCode
	IndexToRAS render.GeometryAffine
	IndexToLPS render.GeometryAffine
	// Spacing is the positive voxel size along i, j, k in millimeters.
	Spacing [3]float64
	// LeftHanded reports a negative determinant (qfac -1 or a flipped sform):
	// slice k then advances against RowDir x ColDir.
	LeftHanded bool
	// QFormSFormAgree reports that both transforms are present, valid,
	// orthonormal and describe the same grid within float32 precision.
	QFormSFormAgree bool
	// SFormCorrected reports that a non-orthogonal sform was replaced by its
	// nearest orthonormal frame (permissive mode, as ITK's
	// ITK_sform_corrected); SFormDeviationDegrees is the largest angle between
	// an original sform column and its corrected direction.
	SFormCorrected        bool
	SFormDeviationDegrees float64
	Warnings              []string
}

// SliceGeometry returns the DICOM-convention geometry of slice k of an
// nx-by-ny grid: voxel index i runs along RowDir (DICOM columns), j along
// ColDir (DICOM rows). Directions are orthonormalized like the exporter's
// strict slice geometry.
func (g Geometry) SliceGeometry(k, columns, rows int) render.SliceGeometry {
	a := g.IndexToLPS
	origin := a.TransformPoint(render.Vec3{Z: float64(k)})
	x := render.Vec3{X: a[0], Y: a[4], Z: a[8]}
	y := render.Vec3{X: a[1], Y: a[5], Z: a[9]}
	row := x.Normalize()
	column := y.Sub(row.Scale(y.Dot(row))).Normalize()
	return render.SliceGeometry{
		Origin:     origin,
		RowDir:     row,
		ColDir:     column,
		Normal:     row.Cross(column).Normalize(),
		RowSpacing: g.Spacing[1],
		ColSpacing: g.Spacing[0],
		Rows:       rows,
		Columns:    columns,
	}
}

type xform struct {
	columns [3]render.Vec3
	norms   [3]float64
	offset  render.Vec3
}

func (x xform) affine() render.GeometryAffine {
	return render.GeometryAffine{
		x.columns[0].X, x.columns[1].X, x.columns[2].X, x.offset.X,
		x.columns[0].Y, x.columns[1].Y, x.columns[2].Y, x.offset.Y,
		x.columns[0].Z, x.columns[1].Z, x.columns[2].Z, x.offset.Z,
		0, 0, 0, 1,
	}
}

func (x xform) unit(index int) render.Vec3 { return x.columns[index].Scale(1 / x.norms[index]) }

func (x xform) determinant() float64 {
	return x.columns[0].Dot(x.columns[1].Cross(x.columns[2]))
}

func (x xform) orthonormal() bool {
	for first := 0; first < 3; first++ {
		for second := first + 1; second < 3; second++ {
			if math.Abs(x.unit(first).Dot(x.unit(second))) > orthonormalTolerance {
				return false
			}
		}
	}
	return true
}

func newXform(columns [3]render.Vec3, offset render.Vec3, field string) (xform, error) {
	out := xform{columns: columns, offset: offset}
	if !finiteVec(offset) {
		return xform{}, readError(ReadCodeGeometry, field, ErrInvalidTransform, nil)
	}
	for index, column := range columns {
		out.norms[index] = column.Length()
		if !finiteVec(column) || !finitePositive(out.norms[index]) {
			return xform{}, readError(ReadCodeGeometry, field, ErrInvalidTransform, nil)
		}
	}
	normalized := out.unit(0).Dot(out.unit(1).Cross(out.unit(2)))
	if !(math.Abs(normalized) > singularTolerance) {
		return xform{}, readError(ReadCodeGeometry, field, ErrInvalidTransform, nil)
	}
	return out, nil
}

// qformTransform rebuilds NIfTI method 2: R(quaternion) * diag(dx, dy, qfac*dz)
// plus qoffset.
func qformTransform(header Header) (xform, error) {
	b, c, d := header.QuaternB, header.QuaternC, header.QuaternD
	if !finite(b) || !finite(c) || !finite(d) {
		return xform{}, readError(ReadCodeGeometry, "quatern", ErrInvalidTransform, nil)
	}
	for index := 1; index <= 3; index++ {
		if !finitePositive(header.PixDim[index]) {
			return xform{}, readError(ReadCodeGeometry, "pixdim", ErrInvalidTransform, nil)
		}
	}
	sum := b*b + c*c + d*d
	if sum > 1+1e-4 {
		return xform{}, readError(ReadCodeGeometry, "quatern", ErrInvalidTransform, nil)
	}
	var a float64
	if residual := 1 - sum; residual < 1e-7 {
		// A 180-degree rotation: a is zero and (b, c, d) is renormalized.
		length := math.Sqrt(sum)
		b, c, d = b/length, c/length, d/length
	} else {
		a = math.Sqrt(residual)
	}
	r := [3][3]float64{
		{a*a + b*b - c*c - d*d, 2 * (b*c - a*d), 2 * (b*d + a*c)},
		{2 * (b*c + a*d), a*a + c*c - b*b - d*d, 2 * (c*d - a*b)},
		{2 * (b*d - a*c), 2 * (c*d + a*b), a*a + d*d - c*c - b*b},
	}
	scales := [3]float64{header.PixDim[1], header.PixDim[2], header.QFac() * header.PixDim[3]}
	var columns [3]render.Vec3
	for column := 0; column < 3; column++ {
		direction := render.Vec3{X: r[0][column], Y: r[1][column], Z: r[2][column]}
		direction = snapSmall(direction).Normalize()
		columns[column] = direction.Scale(scales[column])
	}
	offset := render.Vec3{X: header.QOffset[0], Y: header.QOffset[1], Z: header.QOffset[2]}
	return newXform(columns, offset, "qform")
}

func snapSmall(v render.Vec3) render.Vec3 {
	if math.Abs(v.X) < quaternionSnap {
		v.X = 0
	}
	if math.Abs(v.Y) < quaternionSnap {
		v.Y = 0
	}
	if math.Abs(v.Z) < quaternionSnap {
		v.Z = 0
	}
	return v
}

// sformTransform reads NIfTI method 3 (srow_x/y/z). It checks only that the
// matrix is finite and invertible; orthonormality is a separate decision.
func sformTransform(header Header) (xform, error) {
	var columns [3]render.Vec3
	for column := 0; column < 3; column++ {
		columns[column] = render.Vec3{X: header.SRow[0][column], Y: header.SRow[1][column], Z: header.SRow[2][column]}
	}
	offset := render.Vec3{X: header.SRow[0][3], Y: header.SRow[1][3], Z: header.SRow[2][3]}
	return newXform(columns, offset, "sform")
}

func pixdimTransform(header Header) (xform, error) {
	var columns [3]render.Vec3
	for index := 1; index <= 3; index++ {
		if !finitePositive(header.PixDim[index]) {
			return xform{}, readError(ReadCodeGeometry, "pixdim", ErrInvalidTransform, nil)
		}
	}
	columns[0] = render.Vec3{X: header.PixDim[1]}
	columns[1] = render.Vec3{Y: header.PixDim[2]}
	columns[2] = render.Vec3{Z: header.PixDim[3]}
	return newXform(columns, render.Vec3{}, "pixdim")
}

// sameTransform compares two transforms at float32 storage precision: each
// column within 1e-4 of its length and origins within 1e-3 of the smallest
// voxel (plus float32 relative precision), ITK's "very similar" test.
func sameTransform(a, b xform) bool {
	for index := 0; index < 3; index++ {
		scale := math.Max(a.norms[index], b.norms[index])
		if a.columns[index].Sub(b.columns[index]).Length() > 1e-4*scale {
			return false
		}
	}
	smallest := math.Min(a.norms[0], math.Min(a.norms[1], a.norms[2]))
	tolerance := 1e-3*smallest + 1e-6*math.Max(a.offset.Length(), b.offset.Length())
	return a.offset.Sub(b.offset).Length() <= tolerance
}

// orthogonalizeSForm replaces the direction part of a non-orthogonal sform by
// the orthonormal frame nearest (Frobenius norm) to its normalized columns,
// keeping the translation and taking spacing from pixdim when it is usable.
// The handedness of the original matrix is preserved.
func orthogonalizeSForm(x xform, header Header, maxDeviationDegrees float64) (xform, float64, error) {
	var matrix [3][3]float64
	for column := 0; column < 3; column++ {
		unit := x.unit(column)
		matrix[0][column], matrix[1][column], matrix[2][column] = unit.X, unit.Y, unit.Z
	}
	rotation, ok := polarFactor(matrix)
	if !ok {
		return xform{}, 0, readError(ReadCodeGeometry, "sform", ErrNonOrthogonalSForm, nil)
	}
	var (
		columns   [3]render.Vec3
		deviation float64
	)
	for column := 0; column < 3; column++ {
		direction := render.Vec3{X: rotation[0][column], Y: rotation[1][column], Z: rotation[2][column]}.Normalize()
		cosine := math.Max(-1, math.Min(1, direction.Dot(x.unit(column))))
		deviation = math.Max(deviation, math.Acos(cosine)*180/math.Pi)
		spacing := x.norms[column]
		if pixdim := header.PixDim[column+1]; finitePositive(pixdim) {
			spacing = pixdim
		}
		columns[column] = direction.Scale(spacing)
	}
	if !(deviation <= maxDeviationDegrees) {
		return xform{}, deviation, readError(ReadCodeGeometry, "sform", ErrNonOrthogonalSForm, nil)
	}
	out, err := newXform(columns, x.offset, "sform")
	return out, deviation, err
}

// polarFactor returns the orthogonal factor Q of M = Q*S with Newton's
// iteration Q <- (Q + Q^-T)/2, which converges quadratically for invertible M
// and keeps the sign of det(M).
func polarFactor(m [3][3]float64) ([3][3]float64, bool) {
	q := m
	for iteration := 0; iteration < 64; iteration++ {
		inverse, ok := invert3(q)
		if !ok {
			return [3][3]float64{}, false
		}
		var next [3][3]float64
		change := 0.0
		for r := 0; r < 3; r++ {
			for c := 0; c < 3; c++ {
				next[r][c] = 0.5 * (q[r][c] + inverse[c][r])
				change = math.Max(change, math.Abs(next[r][c]-q[r][c]))
			}
		}
		q = next
		if change < 1e-15 {
			break
		}
	}
	for r := 0; r < 3; r++ {
		for c := 0; c < 3; c++ {
			if !finite(q[r][c]) {
				return [3][3]float64{}, false
			}
		}
	}
	return q, true
}

func invert3(m [3][3]float64) ([3][3]float64, bool) {
	a, b, c := m[0][0], m[0][1], m[0][2]
	d, e, f := m[1][0], m[1][1], m[1][2]
	g, h, i := m[2][0], m[2][1], m[2][2]
	det := a*(e*i-f*h) - b*(d*i-f*g) + c*(d*h-e*g)
	if !finite(det) || math.Abs(det) < 1e-300 {
		return [3][3]float64{}, false
	}
	inv := 1 / det
	return [3][3]float64{
		{(e*i - f*h) * inv, (c*h - b*i) * inv, (b*f - c*e) * inv},
		{(f*g - d*i) * inv, (a*i - c*g) * inv, (c*d - a*f) * inv},
		{(d*h - e*g) * inv, (b*g - a*h) * inv, (a*e - b*d) * inv},
	}, true
}

// resolveGeometry applies the qform/sform precedence documented in
// NIFTI_IMPORT.md and converts the chosen transform to millimeters, RAS and
// LPS.
func resolveGeometry(header Header, options ReadOptions) (Geometry, error) {
	if !header.QFormCode.valid() {
		return Geometry{}, readError(ReadCodeHeader, "qform_code", ErrMalformedHeader, nil)
	}
	if !header.SFormCode.valid() {
		return Geometry{}, readError(ReadCodeHeader, "sform_code", ErrMalformedHeader, nil)
	}
	geometry := Geometry{QFormCode: header.QFormCode, SFormCode: header.SFormCode}
	var (
		qform, sform xform
		qErr, sErr   error
	)
	hasQ, hasS := header.QFormCode != XformUnknown, header.SFormCode != XformUnknown
	if hasQ {
		qform, qErr = qformTransform(header)
	}
	if hasS {
		sform, sErr = sformTransform(header)
	}
	qValid, sValid := hasQ && qErr == nil, hasS && sErr == nil
	sOrthonormal := sValid && sform.orthonormal()
	geometry.QFormSFormAgree = qValid && sOrthonormal && sameTransform(qform, sform)
	maxDeviation := options.PermissiveMaxDeviationDegrees
	if maxDeviation == 0 {
		maxDeviation = DefaultPermissiveMaxDeviationDegrees
	}

	var chosen xform
	useSForm := func() error {
		if !sOrthonormal {
			if !options.SFormPermissive {
				return readError(ReadCodeGeometry, "sform", ErrNonOrthogonalSForm, nil)
			}
			corrected, deviation, err := orthogonalizeSForm(sform, header, maxDeviation)
			if err != nil {
				return err
			}
			chosen = corrected
			geometry.Source = XformSourceSFormOrthogonalized
			geometry.SFormCorrected = true
			geometry.SFormDeviationDegrees = deviation
			geometry.Warnings = append(geometry.Warnings, WarningSFormOrthogonalized)
		} else {
			chosen = sform
			geometry.Source = XformSourceSForm
			for index := 0; index < 3; index++ {
				pixdim := header.PixDim[index+1]
				if finitePositive(pixdim) && !nearlyEqual(sform.norms[index], pixdim, 1e-3) {
					geometry.Warnings = append(geometry.Warnings, WarningSFormPixDimMismatch)
					break
				}
			}
		}
		geometry.Code = header.SFormCode
		return nil
	}
	useQForm := func() {
		chosen = qform
		geometry.Source = XformSourceQForm
		geometry.Code = header.QFormCode
	}

	switch options.Xform {
	case XformQForm:
		if !hasQ {
			return Geometry{}, readError(ReadCodeGeometry, "qform", ErrNoSpatialTransform, nil)
		}
		if qErr != nil {
			return Geometry{}, qErr
		}
		useQForm()
	case XformSForm:
		if !hasS {
			return Geometry{}, readError(ReadCodeGeometry, "sform", ErrNoSpatialTransform, nil)
		}
		if sErr != nil {
			return Geometry{}, sErr
		}
		if err := useSForm(); err != nil {
			return Geometry{}, err
		}
	case XformAuto:
		switch {
		case !hasQ && !hasS:
			if !options.AllowMissingTransform {
				return Geometry{}, readError(ReadCodeGeometry, "xform", ErrNoSpatialTransform, nil)
			}
			pixdim, err := pixdimTransform(header)
			if err != nil {
				return Geometry{}, err
			}
			chosen = pixdim
			geometry.Source = XformSourcePixDim
			geometry.Warnings = append(geometry.Warnings, WarningNoSpatialTransform)
		case sOrthonormal && (!qValid || header.SFormCode == XformScannerAnat || geometry.QFormSFormAgree):
			if hasQ && !qValid {
				geometry.Warnings = append(geometry.Warnings, WarningQFormInvalid)
			}
			if err := useSForm(); err != nil {
				return Geometry{}, err
			}
		case qValid:
			if sValid && !sOrthonormal {
				geometry.Warnings = append(geometry.Warnings, WarningSFormNonOrthogonal)
			} else if sOrthonormal {
				geometry.Warnings = append(geometry.Warnings, WarningSFormIgnored)
			}
			useQForm()
		case sValid:
			if hasQ {
				geometry.Warnings = append(geometry.Warnings, WarningQFormInvalid)
			}
			if err := useSForm(); err != nil {
				return Geometry{}, err
			}
		case sErr != nil:
			return Geometry{}, sErr
		default:
			return Geometry{}, qErr
		}
	default:
		return Geometry{}, readError(ReadCodeGeometry, "xform", ErrInvalidTransform, nil)
	}

	scale := header.SpatialUnit().millimeters()
	for index := range chosen.columns {
		chosen.columns[index] = chosen.columns[index].Scale(scale)
		geometry.Spacing[index] = chosen.norms[index] * scale
	}
	chosen.offset = chosen.offset.Scale(scale)
	geometry.IndexToRAS = chosen.affine()
	geometry.IndexToLPS = affineLPS2RAS(geometry.IndexToRAS)
	if !geometry.IndexToRAS.Finite() || !geometry.IndexToLPS.Finite() {
		return Geometry{}, readError(ReadCodeGeometry, "xform", ErrInvalidTransform, nil)
	}
	for _, spacing := range geometry.Spacing {
		if !finitePositive(spacing) {
			return Geometry{}, readError(ReadCodeGeometry, "xform", ErrInvalidTransform, nil)
		}
	}
	geometry.LeftHanded = chosen.determinant() < 0
	if geometry.Source != XformSourcePixDim && geometry.Code != XformScannerAnat {
		geometry.Warnings = append(geometry.Warnings, WarningNonScannerCoordinates)
	}
	return geometry, nil
}
