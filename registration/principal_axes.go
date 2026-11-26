package registration

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/render"
)

// ErrPrincipalAxes reports invalid/non-finite moment data.
var ErrPrincipalAxes = errors.New("registration: invalid principal-axis moments")

// ErrAmbiguousPrincipalAxes reports repeated or insufficiently separated moments.
var ErrAmbiguousPrincipalAxes = errors.New("registration: ambiguous principal axes")

// PrincipalAxisOptions define nonnegative intensity weights and finite work.
// Weight=max(0, value+IntensityOffset); signed intensities are not signed mass.
// Inputs are borrowed read-only and must remain stable for the operation.
type PrincipalAxisOptions struct {
	IntensityOffset float64
	MaxVoxels       int // zero: 1<<26; negative is invalid
	// RelativeGap is the minimum eigenvalue separation divided by the largest
	// principal moment when producing initialization candidates (zero:1e-6).
	RelativeGap float64
}

// PrincipalMoments are normalized covariance in physical LPS mm², with sorted
// ascending eigenvalues. Axes columns form an orthonormal right-handed basis;
// axis signs are intrinsically ambiguous. Mass is the sum of admitted weights.
type PrincipalMoments struct {
	Mass       float64
	Center     render.Vec3
	Covariance Matrix3
	Values     [3]float64
	Axes       Matrix3
}

func principalOptions(o PrincipalAxisOptions) (PrincipalAxisOptions, error) {
	if !finite(o.IntensityOffset) || o.MaxVoxels < 0 || o.RelativeGap < 0 || !finite(o.RelativeGap) || o.RelativeGap >= 1 {
		return o, fmt.Errorf("%w: options", ErrPrincipalAxes)
	}
	if o.MaxVoxels == 0 {
		o.MaxVoxels = 1 << 26
	}
	if o.RelativeGap == 0 {
		o.RelativeGap = 1e-6
	}
	return o, nil
}

// ImagePrincipalMoments computes physical moments with two bounded passes and
// fixed scratch. Centering relative to the image origin avoids subtracting huge
// patient coordinates. No image, sample array, mapping, or rendering is created.
func ImagePrincipalMoments(ctx context.Context, im *Image, opts PrincipalAxisOptions) (PrincipalMoments, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return PrincipalMoments{}, err
	}
	opts, err := principalOptions(opts)
	if err != nil {
		return PrincipalMoments{}, err
	}
	if im == nil {
		return PrincipalMoments{}, fmt.Errorf("%w: nil image", ErrPrincipalAxes)
	}
	n := 1
	for _, size := range im.size {
		if size < 1 || n > math.MaxInt/size {
			return PrincipalMoments{}, fmt.Errorf("%w: dimensions", ErrPrincipalAxes)
		}
		n *= size
	}
	if n != len(im.data) || n > opts.MaxVoxels {
		return PrincipalMoments{}, fmt.Errorf("%w: sample count or finite voxel limit", ErrPrincipalAxes)
	}
	var mass float64
	var mean render.Vec3
	for i, value := range im.data {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return PrincipalMoments{}, err
			}
		}
		if !finite(float64(value)) {
			return PrincipalMoments{}, fmt.Errorf("%w: non-finite intensity", ErrPrincipalAxes)
		}
		w := math.Max(0, float64(value)+opts.IntensityOffset)
		if !finite(w) {
			return PrincipalMoments{}, fmt.Errorf("%w: weight overflow", ErrPrincipalAxes)
		}
		if w == 0 {
			continue
		}
		p := im.indexToPhysical.MulVec(render.Vec3{X: float64(i % im.size[0]), Y: float64(i / im.size[0] % im.size[1]), Z: float64(i / (im.size[0] * im.size[1]))})
		if !finiteVec(p) || !finite(mass+w) {
			return PrincipalMoments{}, fmt.Errorf("%w: physical moment overflow", ErrPrincipalAxes)
		}
		mass += w
		mean = mean.Add(p.Sub(mean).Scale(w / mass))
		if !finiteVec(mean) {
			return PrincipalMoments{}, fmt.Errorf("%w: center overflow", ErrPrincipalAxes)
		}
	}
	if mass == 0 {
		return PrincipalMoments{}, ErrNoMass
	}
	var covariance, compensation Matrix3
	for i, value := range im.data {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return PrincipalMoments{}, err
			}
		}
		w := math.Max(0, float64(value)+opts.IntensityOffset) / mass
		if w == 0 {
			continue
		}
		p := im.indexToPhysical.MulVec(render.Vec3{X: float64(i % im.size[0]), Y: float64(i / im.size[0] % im.size[1]), Z: float64(i / (im.size[0] * im.size[1]))}).Sub(mean)
		q := [3]float64{p.X, p.Y, p.Z}
		for row := 0; row < 3; row++ {
			for col := row; col < 3; col++ {
				v := w * q[row] * q[col]
				if !finite(v) {
					return PrincipalMoments{}, fmt.Errorf("%w: covariance overflow", ErrPrincipalAxes)
				}
				y := v - compensation[row][col]
				next := covariance[row][col] + y
				compensation[row][col] = (next - covariance[row][col]) - y
				covariance[row][col] = next
			}
		}
	}
	for row := 0; row < 3; row++ {
		for col := row; col < 3; col++ {
			covariance[col][row] = covariance[row][col]
		}
	}
	values, axes, err := principalEigen(covariance)
	if err != nil {
		return PrincipalMoments{}, err
	}
	center := im.origin.Add(mean)
	if !finiteVec(center) {
		return PrincipalMoments{}, fmt.Errorf("%w: physical center overflow", ErrPrincipalAxes)
	}
	if err := ctx.Err(); err != nil {
		return PrincipalMoments{}, err
	}
	return PrincipalMoments{Mass: mass, Center: center, Covariance: covariance, Values: values, Axes: axes}, nil
}

// principalEigen is our fixed-size symmetric Jacobi eigensolver. Scaling keeps
// the rotations bounded; no external implementation is used or copied.
func principalEigen(a Matrix3) ([3]float64, Matrix3, error) {
	scale := 0.0
	for _, row := range a {
		for _, v := range row {
			if !finite(v) {
				return [3]float64{}, Matrix3{}, ErrPrincipalAxes
			}
			scale = math.Max(scale, math.Abs(v))
		}
	}
	if scale == 0 {
		return [3]float64{}, Identity3(), nil
	}
	for i := range a {
		for j := range a[i] {
			a[i][j] /= scale
		}
	}
	axis := Identity3()
	converged := false
	for sweep := 0; sweep < 32; sweep++ {
		largest := math.Max(math.Abs(a[0][1]), math.Max(math.Abs(a[0][2]), math.Abs(a[1][2])))
		if largest <= 1e-14 {
			converged = true
			break
		}
		for p := 0; p < 2; p++ {
			for q := p + 1; q < 3; q++ {
				if math.Abs(a[p][q]) <= 1e-15 {
					continue
				}
				tau := (a[q][q] - a[p][p]) / (2 * a[p][q])
				t := math.Copysign(1, tau) / (math.Abs(tau) + math.Hypot(1, tau))
				c := 1 / math.Hypot(1, t)
				s := t * c
				app, aqq, apq := a[p][p], a[q][q], a[p][q]
				a[p][p] = app - t*apq
				a[q][q] = aqq + t*apq
				a[p][q] = 0
				a[q][p] = 0
				for k := 0; k < 3; k++ {
					if k != p && k != q {
						x, y := a[k][p], a[k][q]
						a[k][p] = c*x - s*y
						a[p][k] = a[k][p]
						a[k][q] = s*x + c*y
						a[q][k] = a[k][q]
					}
					x, y := axis[k][p], axis[k][q]
					axis[k][p] = c*x - s*y
					axis[k][q] = s*x + c*y
				}
			}
		}
	}
	if !converged {
		return [3]float64{}, Matrix3{}, fmt.Errorf("%w: eigen convergence", ErrPrincipalAxes)
	}
	order := []int{0, 1, 2}
	sort.SliceStable(order, func(i, j int) bool { return a[order[i]][order[i]] < a[order[j]][order[j]] })
	var values [3]float64
	var axes Matrix3
	for col, k := range order {
		v := a[k][k]
		if v < -1e-12 {
			return [3]float64{}, Matrix3{}, fmt.Errorf("%w: negative covariance eigenvalue", ErrPrincipalAxes)
		}
		values[col] = math.Max(0, v) * scale
		if !finite(values[col]) {
			return [3]float64{}, Matrix3{}, ErrPrincipalAxes
		}
		for row := 0; row < 3; row++ {
			axes[row][col] = axis[row][k]
		}
	}
	if axes.Determinant() < 0 {
		for row := 0; row < 3; row++ {
			axes[row][2] = -axes[row][2]
		}
	}
	return values, axes, nil
}

// PrincipalAxisCandidates produces the four proper rotations that align the
// ordered axes. Their signs cannot be inferred from second moments: use the
// actual registration metric to choose a candidate. Repeated moments are refused
// instead of inventing an orientation. Every candidate maps fixed COM to moving
// COM and owns its state; no input transform/image is modified.
func PrincipalAxisCandidates(ctx context.Context, fixed, moving *Image, opts PrincipalAxisOptions) ([4]*Euler3D, error) {
	opts, err := principalOptions(opts)
	if err != nil {
		return [4]*Euler3D{}, err
	}
	f, err := ImagePrincipalMoments(ctx, fixed, opts)
	if err != nil {
		return [4]*Euler3D{}, err
	}
	m, err := ImagePrincipalMoments(ctx, moving, opts)
	if err != nil {
		return [4]*Euler3D{}, err
	}
	for _, moments := range []PrincipalMoments{f, m} {
		maximum := moments.Values[2]
		if !(maximum > 0) || (moments.Values[1]-moments.Values[0])/maximum <= opts.RelativeGap || (moments.Values[2]-moments.Values[1])/maximum <= opts.RelativeGap {
			return [4]*Euler3D{}, ErrAmbiguousPrincipalAxes
		}
	}
	signs := [4][3]float64{{1, 1, 1}, {1, -1, -1}, {-1, 1, -1}, {-1, -1, 1}}
	var out [4]*Euler3D
	for i, sign := range signs {
		rotation := m.Axes.Mul(Matrix3{{sign[0], 0, 0}, {0, sign[1], 0}, {0, 0, sign[2]}}).Mul(f.Axes.Transpose())
		t := NewEuler3D()
		if err = t.SetMatrix(rotation); err != nil {
			return [4]*Euler3D{}, err
		}
		if err = t.SetCenter(f.Center); err != nil {
			return [4]*Euler3D{}, err
		}
		if err = t.SetTranslation(m.Center.Sub(f.Center)); err != nil {
			return [4]*Euler3D{}, err
		}
		out[i] = t
	}
	return out, nil
}
