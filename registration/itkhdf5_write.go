package registration

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"

	"github.com/ThalesMMS/dicom-go/internal/hdf5"
)

// DefaultITKHDF5MaxBytes caps the entire encoded output, including metadata.
// It does not cap input arrays, reader allocations or process RSS.
const DefaultITKHDF5MaxBytes int64 = 256 << 20

// ErrTransformFileBudget identifies an encoded-file byte admission failure.
var ErrTransformFileBudget = errors.New("dicom/registration: transform file exceeds byte budget")

// ITKHDF5WriteOptions controls canonical uncompressed classic HDF5 output.
// MaxBytes zero uses DefaultITKHDF5MaxBytes; positive overrides it, negative
// is invalid. No append/compression/general-HDF options are exposed.
type ITKHDF5WriteOptions struct{ MaxBytes int64 }

func hdf5WriteFailure(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, hdf5.ErrWriteBudget) {
		err = fmt.Errorf("%w: %w", ErrTransformFileBudget, err)
	}
	return fmt.Errorf("%w: %w", ErrTransformFile, err)
}
func hdf5WriteBudget(o ITKHDF5WriteOptions) (int64, error) {
	if o.MaxBytes < 0 {
		return 0, ErrInvalidParameters
	}
	if o.MaxBytes == 0 {
		return DefaultITKHDF5MaxBytes, nil
	}
	return o.MaxBytes, nil
}
func stringHDF5(path, value string) hdf5.WriteDataset {
	return hdf5.WriteDataset{Path: path, Text: &value}
}
func sliceHDF5(path string, p []float64) hdf5.WriteDataset {
	return hdf5.WriteDataset{Path: path, Count: len(p), Float64At: func(i int) float64 { return p[i] }}
}
func hdf5RootDatasets() []hdf5.WriteDataset {
	return []hdf5.WriteDataset{stringHDF5("/HDFVersion", "HDF5 superblock 0; pure Go"), stringHDF5("/ITKVersion", "dicom-go; ITK 5.4 transform schema")}
}
func denseHDF5Size(kind Kind, count uint64, f []float64) error {
	if err := checkLength(f, 18, "dense fixed parameters"); err != nil {
		return err
	}
	minimum := 1.0
	if kind == KindBSpline {
		minimum = 4
	}
	want := uint64(3)
	for _, size := range f[:3] {
		if size < minimum || size > 1<<20 || size != math.Trunc(size) || want > maxDenseTransformParameters/uint64(size) {
			return fmt.Errorf("%w: dense geometry/parameter count", ErrInvalidParameters)
		}
		want *= uint64(size)
	}
	if count != want || count > maxDenseTransformParameters {
		return fmt.Errorf("%w: dense shape requires%d parameters, got%d", ErrInvalidParameters, want, count)
	}
	for _, spacing := range f[6:9] {
		if spacing <= 0 {
			return ErrInvalidParameters
		}
	}
	m := Matrix3{{f[9], f[10], f[11]}, {f[12], f[13], f[14]}, {f[15], f[16], f[17]}}
	if _, err := m.Inverse(); err != nil {
		return err
	}
	return nil
}

// Serialization must describe the stored mapping, including its concrete
// family. This is a roundoff-level coefficient check, not a broad geometric
// tolerance: 32 float64 epsilons times max(1, |actual|, |reconstructed|).
const hdf5CanonicalTolerance = 32 * 2.220446049250313e-16

func hdf5LinearKind(t any) Kind {
	switch t.(type) {
	case *Translation3D, *Translation2D:
		return KindTranslation
	case *ScaleVersor3D:
		return KindScaleVersor3D
	case *ScaleSkewVersor3D:
		return KindScaleSkewVersor3D
	case *Euler3D:
		return KindEuler3D
	case *VersorRigid3D:
		return KindVersorRigid3D
	case *Similarity3D:
		return KindSimilarity3D
	case *Euler2D:
		return KindEuler2D
	case *Similarity2D:
		return KindSimilarity2D
	case *Affine, *Affine2D:
		return KindAffine
	}
	return ""
}

func hdf5CanonicalLinear(t itkParameters, dimension int, p, f []float64) error {
	kind := hdf5LinearKind(t)
	if kind == "" || kind != t.Kind() {
		return fmt.Errorf("%w: uninitialized or noncanonical linear transform family; use its constructor", ErrInvalidParameters)
	}
	// Factories here can only instantiate the whitelisted small linears. Dense
	// transforms never use this path and their borrowed arrays are not cloned.
	canonical, err := newITKParameters(fmt.Sprintf("%s_double_%d_%d", kind, dimension, dimension), dimension)
	if err != nil {
		return err
	}
	if err := setITKParameters(canonical, p, f); err != nil {
		return err
	}
	equivalent := func(actual, expected float64) bool {
		return finite(actual) && finite(expected) && math.Abs(actual-expected) <= hdf5CanonicalTolerance*math.Max(1, math.Max(math.Abs(actual), math.Abs(expected)))
	}
	valid := true
	if dimension == 3 {
		actual, expected := t.(MatrixTransform), canonical.(MatrixTransform)
		a, b := actual.Matrix(), expected.Matrix()
		for i := 0; i < 3; i++ {
			for j := 0; j < 3; j++ {
				valid = valid && equivalent(a[i][j], b[i][j])
			}
		}
		x, y := actual.Offset(), expected.Offset()
		valid = valid && equivalent(x.X, y.X) && equivalent(x.Y, y.Y) && equivalent(x.Z, y.Z)
	} else {
		actual, expected := t.(MatrixTransform2D), canonical.(MatrixTransform2D)
		a, b := actual.Matrix(), expected.Matrix()
		for i := 0; i < 2; i++ {
			for j := 0; j < 2; j++ {
				valid = valid && equivalent(a[i][j], b[i][j])
			}
		}
		x, y := actual.Offset(), expected.Offset()
		valid = valid && equivalent(x[0], y[0]) && equivalent(x[1], y[1])
	}
	if !valid {
		return fmt.Errorf("%w: linear state changes geometry when native parameters are reread; use a canonical constructor/setter state or affine representation", ErrInvalidParameters)
	}
	return nil
}

func hdf5Record3D(base string, t Transform) ([]hdf5.WriteDataset, error) {
	valid := false
	dense := false
	switch v := t.(type) {
	case *Translation3D:
		valid = v != nil
	case *ScaleVersor3D:
		valid = v != nil
	case *ScaleSkewVersor3D:
		valid = v != nil
	case *Euler3D:
		valid = v != nil
	case *VersorRigid3D:
		valid = v != nil
	case *Similarity3D:
		valid = v != nil
	case *Affine:
		valid = v != nil
	case *BSpline:
		valid = v != nil
		dense = true
	case *DisplacementFieldTransform:
		valid = v != nil
		dense = true
	}
	if !valid {
		return nil, fmt.Errorf("%w: HDF5 requires a built-in 3D transform", ErrUnsupportedTransform)
	}
	name, err := itkTypeName(t)
	if err != nil {
		return nil, err
	}
	fixed := t.FixedParameters()
	ds := []hdf5.WriteDataset{stringHDF5(base+"/TransformType", name), sliceHDF5(base+"/TransformFixedParameters", fixed)}
	if dense {
		var d hdf5.WriteDataset
		d.Path = base + "/TransformParameters"
		switch v := t.(type) {
		case *BSpline:
			d.Count = len(v.coefficients)
			d.Float64At = func(i int) float64 { return v.coefficients[i] }
		case *DisplacementFieldTransform:
			if uint64(len(v.field.Vectors)) > maxDenseTransformParameters/3 {
				return nil, ErrInvalidParameters
			}
			d.Count = 3 * len(v.field.Vectors)
			d.Float64At = func(i int) float64 {
				p := v.field.Vectors[i/3]
				switch i % 3 {
				case 0:
					return p.X
				case 1:
					return p.Y
				default:
					return p.Z
				}
			}
		}
		if err := denseHDF5Size(t.Kind(), uint64(d.Count), fixed); err != nil {
			return nil, err
		}
		ds = append(ds, d)
	} else {
		m := t.(MatrixTransform)
		if !m.Matrix().Finite() || !finiteVec(m.Offset()) {
			return nil, ErrInvalidMatrix
		}
		p := t.Parameters()
		if len(p) != t.NumberOfParameters() || len(p) > 64 || len(fixed) > 64 {
			return nil, ErrInvalidParameters
		}
		if err := hdf5CanonicalLinear(t, 3, p, fixed); err != nil {
			return nil, err
		}
		ds = append(ds, sliceHDF5(base+"/TransformParameters", p))
	}
	return ds, nil
}
func builtin2D(t Transform2D, composite bool) bool {
	switch v := t.(type) {
	case *Translation2D:
		return v != nil
	case *Euler2D:
		return v != nil
	case *Similarity2D:
		return v != nil
	case *Affine2D:
		return v != nil
	case *Composite2D:
		return composite && v != nil
	}
	return false
}
func hdf5Record2D(base string, t Transform2D) ([]hdf5.WriteDataset, error) {
	if !builtin2D(t, false) {
		return nil, ErrUnsupportedTransform
	}
	m := t.(MatrixTransform2D)
	if !m.Matrix().Finite() || !finitePoint2(m.Offset()) {
		return nil, ErrInvalidMatrix
	}
	name, err := itkTypeName2D(t)
	if err != nil {
		return nil, err
	}
	p, f := t.Parameters(), t.FixedParameters()
	if len(p) != t.NumberOfParameters() || len(p) > 64 || len(f) > 64 {
		return nil, ErrInvalidParameters
	}
	if err := hdf5CanonicalLinear(t, 2, p, f); err != nil {
		return nil, err
	}
	return []hdf5.WriteDataset{stringHDF5(base+"/TransformType", name), sliceHDF5(base+"/TransformFixedParameters", f), sliceHDF5(base+"/TransformParameters", p)}, nil
}

// WriteITKHDF5 writes one built-in 3D transform, possibly composite, with
// the default finite file cap. It serializes existing data without sampling.
func WriteITKHDF5(w io.Writer, t Transform) error {
	return WriteITKHDF5Context(context.Background(), w, t, ITKHDF5WriteOptions{})
}

// WriteITKHDF5Context admits layout/geometry/finite values before the first
// byte, then streams blocks <=64KiB. Stable read-only input is borrowed.
// Cancellation/IO errors can leave partial output for the caller to discard.
// This cannot interrupt a blocking Writer. Nested/empty composites are refused.
func WriteITKHDF5Context(ctx context.Context, w io.Writer, t Transform, o ITKHDF5WriteOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return hdf5WriteFailure(err)
	}
	maxBytes, err := hdf5WriteBudget(o)
	if err != nil {
		return hdf5WriteFailure(err)
	}
	if w == nil {
		return hdf5WriteFailure(ErrInvalidParameters)
	}
	ds := hdf5RootDatasets()
	if c, ok := t.(*Composite); ok {
		if c == nil || c.Len() == 0 || c.Len()+1 > maxHDF5Transforms {
			return hdf5WriteFailure(ErrUnsupportedTransform)
		}
		ds = append(ds, stringHDF5("/TransformGroup/0/TransformType", "CompositeTransform_double_3_3"))
		for i := c.Len() - 1; i >= 0; i-- {
			if err := ctx.Err(); err != nil {
				return hdf5WriteFailure(err)
			}
			rs, err := hdf5Record3D("/TransformGroup/"+strconv.Itoa(c.Len()-i), c.Transform(i))
			if err != nil {
				return hdf5WriteFailure(err)
			}
			ds = append(ds, rs...)
		}
	} else {
		rs, err := hdf5Record3D("/TransformGroup/0", t)
		if err != nil {
			return hdf5WriteFailure(err)
		}
		ds = append(ds, rs...)
	}
	return hdf5WriteFailure(hdf5.Write(ctx, w, ds, maxBytes))
}

// WriteITKHDF52D writes one supported native 2D transform with a finite cap.
func WriteITKHDF52D(w io.Writer, t Transform2D) error {
	return WriteITKHDF52DContext(context.Background(), w, t, ITKHDF5WriteOptions{})
}

// WriteITKHDF52DContext shares the bounded classic format, retaining 2D types.
func WriteITKHDF52DContext(ctx context.Context, w io.Writer, t Transform2D, o ITKHDF5WriteOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return hdf5WriteFailure(err)
	}
	maxBytes, err := hdf5WriteBudget(o)
	if err != nil {
		return hdf5WriteFailure(err)
	}
	if w == nil || !builtin2D(t, true) {
		return hdf5WriteFailure(ErrUnsupportedTransform)
	}
	ds := hdf5RootDatasets()
	if c, ok := t.(*Composite2D); ok {
		if c.Len() == 0 || c.Len()+1 > maxHDF5Transforms {
			return hdf5WriteFailure(ErrUnsupportedTransform)
		}
		ds = append(ds, stringHDF5("/TransformGroup/0/TransformType", "CompositeTransform_double_2_2"))
		for i := c.Len() - 1; i >= 0; i-- {
			rs, err := hdf5Record2D("/TransformGroup/"+strconv.Itoa(c.Len()-i), c.Transform(i))
			if err != nil {
				return hdf5WriteFailure(err)
			}
			ds = append(ds, rs...)
		}
	} else {
		rs, err := hdf5Record2D("/TransformGroup/0", t)
		if err != nil {
			return hdf5WriteFailure(err)
		}
		ds = append(ds, rs...)
	}
	return hdf5WriteFailure(hdf5.Write(ctx, w, ds, maxBytes))
}
