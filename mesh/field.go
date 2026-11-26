package mesh

import (
	"context"
	"fmt"
	"math"

	"github.com/ThalesMMS/dicom-go/render"
)

// Field is a scalar volume sampled on a regular grid: Dims()[0] columns (x),
// Dims()[1] rows (y) and Dims()[2] slices (z), the sample (x, y, z) lying at
// IndexToPatient().TransformPoint((x, y, z)) in patient LPS millimetres. The
// affine carries the direction cosines and the (possibly anisotropic)
// spacing, as render.VolumeDescriptor.IndexToPatientLPS does.
//
// ReadSlice fills dst[y*Dims()[0]+x] with sample (x, y, z). The extractor
// calls it from several goroutines at once, for different or equal z, so
// implementations must be safe for concurrent use; each slice is read a
// few times, never the whole volume at once.
type Field interface {
	Dims() [3]int
	IndexToPatient() render.GeometryAffine
	ReadSlice(ctx context.Context, z int, dst []float32) error
}

// DenseField is an in-memory Field, x-fastest like imagefilter.Volume and
// roi.VolumeMask: Data[(z*Size[1]+y)*Size[0]+x].
type DenseField struct {
	Size   [3]int
	Affine render.GeometryAffine
	Data   []float32
}

func (f *DenseField) Dims() [3]int                          { return f.Size }
func (f *DenseField) IndexToPatient() render.GeometryAffine { return f.Affine }

func (f *DenseField) ReadSlice(_ context.Context, z int, dst []float32) error {
	plane := f.Size[0] * f.Size[1]
	if z < 0 || z >= f.Size[2] || len(f.Data) < (z+1)*plane {
		return fmt.Errorf("%w: slice %d of a %v field holding %d samples", ErrInvalidField, z, f.Size, len(f.Data))
	}
	copy(dst[:plane], f.Data[z*plane:(z+1)*plane])
	return nil
}

// FuncField evaluates Value at every sample; it suits analytic phantoms and
// fields too large to hold.
type FuncField struct {
	Size   [3]int
	Affine render.GeometryAffine
	Value  func(x, y, z int) float64
}

func (f *FuncField) Dims() [3]int                          { return f.Size }
func (f *FuncField) IndexToPatient() render.GeometryAffine { return f.Affine }

func (f *FuncField) ReadSlice(_ context.Context, z int, dst []float32) error {
	if f.Value == nil || z < 0 || z >= f.Size[2] {
		return fmt.Errorf("%w: slice %d of a %v function field", ErrInvalidField, z, f.Size)
	}
	i := 0
	for y := 0; y < f.Size[1]; y++ {
		for x := 0; x < f.Size[0]; x++ {
			dst[i] = float32(f.Value(x, y, z))
			i++
		}
	}
	return nil
}

// snapshotField reads the modality values of a render.VolumeSnapshot.
type snapshotField struct {
	snapshot render.VolumeSnapshot
	dims     [3]int
	affine   render.GeometryAffine
}

// SnapshotField returns the modality values (HU for CT) of a volume
// snapshot as a Field on its grid and IndexToPatientLPS affine. The
// snapshot's lease must stay held while the field is in use.
func SnapshotField(snapshot render.VolumeSnapshot) (Field, error) {
	descriptor, err := snapshot.Descriptor()
	if err != nil {
		return nil, err
	}
	if err := render.ValidateModalityF32Conversion(descriptor); err != nil {
		return nil, err
	}
	d := descriptor.Dimensions
	return &snapshotField{
		snapshot: snapshot,
		dims:     [3]int{int(d[0]), int(d[1]), int(d[2])},
		affine:   descriptor.IndexToPatientLPS,
	}, nil
}

func (f *snapshotField) Dims() [3]int                          { return f.dims }
func (f *snapshotField) IndexToPatient() render.GeometryAffine { return f.affine }

func (f *snapshotField) ReadSlice(ctx context.Context, z int, dst []float32) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if z < 0 || z >= f.dims[2] {
		return fmt.Errorf("%w: slice %d of %d", ErrInvalidField, z, f.dims[2])
	}
	return f.snapshot.ReadModalitySliceF32(uint32(z), dst)
}

// GridAffine is the index-to-patient affine of a grid whose sample (0,0,0)
// lies at origin, with unit axis directions axes (columns, rows, slices) and
// spacing mm between neighbouring samples along each axis. For a plain
// spacing-only grid (imagefilter.Volume) pass the identity axes.
func GridAffine(origin render.Vec3, axes [3]render.Vec3, spacing [3]float64) render.GeometryAffine {
	x, y, z := axes[0].Scale(spacing[0]), axes[1].Scale(spacing[1]), axes[2].Scale(spacing[2])
	return render.GeometryAffine{
		x.X, y.X, z.X, origin.X,
		x.Y, y.Y, z.Y, origin.Y,
		x.Z, y.Z, z.Z, origin.Z,
		0, 0, 0, 1,
	}
}

// IdentityAxes are the patient axes, for GridAffine.
var IdentityAxes = [3]render.Vec3{{X: 1}, {Y: 1}, {Z: 1}}

// affineFrame holds the linear part of an index-to-patient affine, its
// determinant and the inverse transpose that maps index-space gradients to
// patient-space gradients.
type affineFrame struct {
	a        render.GeometryAffine
	det      float64
	gradient [3][3]float64
}

func newAffineFrame(a render.GeometryAffine) (affineFrame, error) {
	if !a.Finite() {
		return affineFrame{}, fmt.Errorf("%w: non-finite index-to-patient affine", ErrInvalidField)
	}
	m := [3][3]float64{{a[0], a[1], a[2]}, {a[4], a[5], a[6]}, {a[8], a[9], a[10]}}
	det := m[0][0]*(m[1][1]*m[2][2]-m[1][2]*m[2][1]) -
		m[0][1]*(m[1][0]*m[2][2]-m[1][2]*m[2][0]) +
		m[0][2]*(m[1][0]*m[2][1]-m[1][1]*m[2][0])
	scale := 0.0
	for _, row := range m {
		for _, v := range row {
			scale = math.Max(scale, math.Abs(v))
		}
	}
	if scale == 0 || math.Abs(det) <= 1e-12*scale*scale*scale {
		return affineFrame{}, fmt.Errorf("%w: singular index-to-patient affine", ErrInvalidField)
	}
	// inverse[i][j] = cofactor[j][i]/det; the gradient map is its transpose,
	// cofactor[i][j]/det.
	var g [3][3]float64
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			r0, r1 := (i+1)%3, (i+2)%3
			c0, c1 := (j+1)%3, (j+2)%3
			g[i][j] = (m[r0][c0]*m[r1][c1] - m[r0][c1]*m[r1][c0]) / det
		}
	}
	return affineFrame{a: a, det: det, gradient: g}, nil
}

func (f *affineFrame) point(x, y, z float64) [3]float32 {
	a := &f.a
	return [3]float32{
		float32(a[0]*x + a[1]*y + a[2]*z + a[3]),
		float32(a[4]*x + a[5]*y + a[6]*z + a[7]),
		float32(a[8]*x + a[9]*y + a[10]*z + a[11]),
	}
}

// patientGradient maps an index-space gradient to patient space.
func (f *affineFrame) patientGradient(g [3]float64) [3]float64 {
	m := &f.gradient
	return [3]float64{
		m[0][0]*g[0] + m[0][1]*g[1] + m[0][2]*g[2],
		m[1][0]*g[0] + m[1][1]*g[1] + m[1][2]*g[2],
		m[2][0]*g[0] + m[2][1]*g[1] + m[2][2]*g[2],
	}
}
