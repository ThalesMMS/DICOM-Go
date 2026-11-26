package registration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func TestWriteHDF5BasicInterchange(t *testing.T) {
	ts := []Transform{NewTranslation3D(), NewScaleVersor3D(), NewScaleSkewVersor3D(), NewEuler3D(), NewAffine()}
	for _, v := range ts {
		t.Run(string(v.Kind()), func(t *testing.T) {
			p := v.Parameters()
			for i := range p {
				p[i] += .015 * float64(i+1)
			}
			if err := v.SetParameters(p); err != nil {
				t.Fatal(err)
			}
			f := v.FixedParameters()
			for i := range f {
				f[i] = float64(i + 1)
			}
			if v.Kind() == KindEuler3D {
				f[3] = 0
			}
			if err := v.SetFixedParameters(f); err != nil {
				t.Fatal(err)
			}
			var b bytes.Buffer
			if err := WriteITKHDF5(&b, v); err != nil {
				t.Fatal(err)
			}
			out, err := ReadTransformFile(b.Bytes())
			if err != nil {
				t.Fatal(err)
			}
			pt := render.Vec3{X: 4, Y: 5, Z: 6}
			if out.Kind() != v.Kind() || out.TransformPoint(pt).Sub(v.TransformPoint(pt)).Length() > 1e-12 || !reflect.DeepEqual(out.FixedParameters(), v.FixedParameters()) {
				t.Fatal("roundtrip")
			}
			if dir := os.Getenv("ITK1345_QUICK_OUTPUT"); dir != "" {
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, string(v.Kind())+".h5"), b.Bytes(), 0600); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
	v := NewSimilarity2D()
	_ = v.SetParameters([]float64{1.2, .4, 2, -3})
	_ = v.SetFixedParameters([]float64{4, -5})
	var b bytes.Buffer
	if err := WriteITKHDF52D(&b, v); err != nil {
		t.Fatal(err)
	}
	o, err := ReadTransformFile2D(b.Bytes())
	if err != nil || o.TransformPoint(Point2{2, 3}).Sub(v.TransformPoint(Point2{2, 3})) != (Point2{}) {
		t.Fatal("2D roundtrip", err)
	}
}

type itk1345Writer struct {
	b      bytes.Buffer
	max    int
	cancel context.CancelFunc
	err    error
	short  bool
}

func (w *itk1345Writer) Write(p []byte) (int, error) {
	if len(p) > w.max {
		w.max = len(p)
	}
	if w.err != nil {
		return 0, w.err
	}
	if w.short {
		return len(p) - 1, nil
	}
	n, e := w.b.Write(p)
	if w.cancel != nil {
		w.cancel()
	}
	return n, e
}
func TestWriteHDF5AdmissionAndIO(t *testing.T) {
	v := NewTranslation3D()
	for _, cap := range []int64{1, -1} {
		var b bytes.Buffer
		err := WriteITKHDF5Context(context.Background(), &b, v, ITKHDF5WriteOptions{MaxBytes: cap})
		if !errors.Is(err, ErrTransformFile) || b.Len() != 0 {
			t.Fatal("cap preflight", cap, err)
		}
		if cap == 1 && !errors.Is(err, ErrTransformFileBudget) {
			t.Fatal(err)
		}
	}
	var exact bytes.Buffer
	if err := WriteITKHDF5(&exact, v); err != nil {
		t.Fatal(err)
	}
	var b bytes.Buffer
	if err := WriteITKHDF5Context(nil, &b, v, ITKHDF5WriteOptions{MaxBytes: int64(exact.Len())}); err != nil || !bytes.Equal(b.Bytes(), exact.Bytes()) {
		t.Fatal("exact cap", err)
	}
	b.Reset()
	if err := WriteITKHDF5Context(nil, &b, v, ITKHDF5WriteOptions{MaxBytes: int64(exact.Len() - 1)}); !errors.Is(err, ErrTransformFileBudget) || b.Len() != 0 {
		t.Fatal("cap boundary", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteITKHDF5Context(ctx, &b, v, ITKHDF5WriteOptions{}); !errors.Is(err, context.Canceled) || b.Len() != 0 {
		t.Fatal("precancel", err)
	}
	ctx, cancel = context.WithCancel(context.Background())
	w := &itk1345Writer{cancel: cancel}
	if err := WriteITKHDF5Context(ctx, w, v, ITKHDF5WriteOptions{}); !errors.Is(err, context.Canceled) || w.b.Len() == 0 {
		t.Fatal("midwrite", err)
	}
	w = &itk1345Writer{short: true}
	if err := WriteITKHDF5(w, v); !errors.Is(err, io.ErrShortWrite) {
		t.Fatal("short write", err)
	}
	sentinel := errors.New("writer stopped")
	w = &itk1345Writer{err: sentinel}
	if err := WriteITKHDF5(w, v); !errors.Is(err, sentinel) {
		t.Fatal("IO cause", err)
	}
	v.translation.X = math.NaN()
	b.Reset()
	if err := WriteITKHDF5(&b, v); !errors.Is(err, ErrTransformFile) || b.Len() != 0 {
		t.Fatal("finite admission", err)
	}
	var nilV *ScaleSkewVersor3D
	if err := WriteITKHDF5(&b, nilV); !errors.Is(err, ErrUnsupportedTransform) || b.Len() != 0 {
		t.Fatal("typed nil", err)
	}
}

type itk1345Custom3D struct {
	*Translation3D
	calls *int
}

func (v *itk1345Custom3D) Parameters() []float64 { *v.calls++; return v.Translation3D.Parameters() }
func TestWriteHDF5BuiltinsDenseAndRecordBounds(t *testing.T) {
	calls := 0
	var out bytes.Buffer
	for _, zero := range []Transform{&BSpline{}, &DisplacementFieldTransform{}} {
		if err := WriteITKHDF5(&out, zero); !errors.Is(err, ErrTransformFile) || out.Len() != 0 {
			t.Fatal("zero dense geometry must be refused before output", err)
		}
	}
	if err := WriteITKHDF5(&out, &itk1345Custom3D{NewTranslation3D(), &calls}); !errors.Is(err, ErrUnsupportedTransform) || out.Len() != 0 || calls != 0 {
		t.Fatal("custom callback invoked", calls, err)
	}
	var empty *Composite
	if err := WriteITKHDF5(&out, empty); !errors.Is(err, ErrUnsupportedTransform) || out.Len() != 0 {
		t.Fatal("typed nil composite", err)
	}
	empty, _ = NewComposite()
	if err := WriteITKHDF5(&out, empty); err == nil || out.Len() != 0 {
		t.Fatal("empty composite", err)
	}
	inner, _ := NewComposite(NewTranslation3D())
	outer, _ := NewComposite(inner)
	if err := WriteITKHDF5(&out, outer); !errors.Is(err, ErrUnsupportedTransform) || out.Len() != 0 {
		t.Fatal("nested composite", err)
	}
	components := make([]Transform, 1023)
	for i := range components {
		tr := NewTranslation3D()
		if err := tr.SetParameters([]float64{.125, -.0625, .03125}); err != nil {
			t.Fatal(err)
		}
		components[i] = tr
	}
	c, err := NewComposite(components...)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteITKHDF5(&out, c); err != nil {
		t.Fatal("maximum records", err)
	}
	result, err := ReadTransformFile(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if result.(*Composite).Len() != 1023 || result.TransformPoint(render.Vec3{}) != (render.Vec3{X: 1023 * .125, Y: -1023 * .0625, Z: 1023 * .03125}) {
		t.Fatal("maximum records mapping")
	}
	if dir := os.Getenv("ITK1345_HDF_OUTPUT_DIR"); dir != "" {
		if err := os.MkdirAll(dir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "maximum-records.h5"), out.Bytes(), 0644); err != nil {
			t.Fatal(err)
		}
	}
	out.Reset()
	if err := c.Append(NewTranslation3D()); err != nil {
		t.Fatal(err)
	}
	if err := WriteITKHDF5(&out, c); err == nil || out.Len() != 0 {
		t.Fatal("one over maximum records", err)
	}
	for _, fixture := range itk1345Fixtures(t) {
		if fixture.Name != "bspline3" && fixture.Name != "field3" {
			continue
		}
		v := itk1345Create(t, fixture.itk1345Descriptor).(Transform)
		before, originalFixed := v.Parameters(), v.FixedParameters()
		w := &itk1345Writer{}
		if err := WriteITKHDF5(w, v); err != nil || w.max > 64<<10 {
			t.Fatal("dense streaming", err, w.max)
		}
		if !reflect.DeepEqual(v.Parameters(), before) || !reflect.DeepEqual(v.FixedParameters(), originalFixed) {
			t.Fatal("dense source mutated")
		}
		out.Reset()
		if err := WriteITKHDF5Context(nil, &out, v, ITKHDF5WriteOptions{MaxBytes: 1}); !errors.Is(err, ErrTransformFileBudget) || out.Len() != 0 {
			t.Fatal("dense byte admission", err)
		}
		if b, ok := v.(*BSpline); ok {
			b.coefficients = b.coefficients[:len(b.coefficients)-1]
		} else {
			d := v.(*DisplacementFieldTransform)
			d.field.Vectors = d.field.Vectors[:len(d.field.Vectors)-1]
		}
		if err := WriteITKHDF5(&out, v); err == nil || out.Len() != 0 {
			t.Fatal("dense geometry mismatch", err)
		}
	}
	var nil2 *Affine2D
	for _, bad := range []Transform2D{nil, nil2, &itk1345Custom2D{NewTranslation2D()}} {
		out.Reset()
		if err := WriteITKHDF52D(&out, bad); !errors.Is(err, ErrUnsupportedTransform) || out.Len() != 0 {
			t.Fatal("2D builtin restriction", err)
		}
	}
}

func TestWriteHDF5NoncanonicalLinearRefusal(t *testing.T) {
	for _, tr := range []Transform{&Euler3D{}, &ScaleSkewVersor3D{}} {
		var out bytes.Buffer
		beforeMatrix, beforeOffset := tr.(MatrixTransform).Matrix(), tr.(MatrixTransform).Offset()
		beforeP, beforeF := tr.Parameters(), tr.FixedParameters()
		if err := WriteITKHDF5(&out, tr); !errors.Is(err, ErrTransformFile) || !errors.Is(err, ErrInvalidParameters) || out.Len() != 0 {
			t.Fatal("uninitialized noncanonical linear state emitted bytes", tr.Kind(), err)
		}
		if tr.(MatrixTransform).Matrix() != beforeMatrix || tr.(MatrixTransform).Offset() != beforeOffset || !reflect.DeepEqual(tr.Parameters(), beforeP) || !reflect.DeepEqual(tr.FixedParameters(), beforeF) {
			t.Fatal("refusal changed source")
		}
	}
	scale := NewScaleVersor3D()
	scale.offset.X = 1
	var out bytes.Buffer
	if err := WriteITKHDF5(&out, scale); !errors.Is(err, ErrInvalidParameters) || out.Len() != 0 {
		t.Fatal("noncanonical3D offset", err)
	}
	euler2 := NewEuler2D()
	euler2.matrix = Matrix2{}
	affine2 := NewAffine2D()
	affine2.offset[0] = 1
	for _, tr := range []Transform2D{euler2, affine2, &Euler2D{}} {
		out.Reset()
		if err := WriteITKHDF52D(&out, tr); !errors.Is(err, ErrTransformFile) || out.Len() != 0 {
			t.Fatal("noncanonical2D geometry/family", err)
		}
	}
	for _, tr := range []Transform{NewScaleVersor3D(), NewScaleSkewVersor3D()} {
		p := tr.Parameters()
		p[6] = 0
		p[7] = -1
		if err := tr.SetParameters(p); err != nil {
			t.Fatal(err)
		}
		out.Reset()
		if err := WriteITKHDF5(&out, tr); err != nil {
			t.Fatal("valid constructed zero/negative forward state refused", err)
		}
		actual, err := ReadTransformFile(out.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		point := render.Vec3{X: 1, Y: 2, Z: 3}
		if actual.TransformPoint(point) != tr.TransformPoint(point) {
			t.Fatal("constructed zero/negative mapping changed")
		}
	}
}

func TestWriteHDF5VersorRepresentationBoundaryRefusal(t *testing.T) {
	tr := NewScaleVersor3D()
	if err := tr.UpdateParameters([]float64{math.Pi, 0, 0, 0, 0, 0, 0, 0, 0}, 1); err != nil {
		t.Fatal(err)
	}
	// This valid forward rotation is not represented by the native right part
	// after its setter's ITK clamp. An exporter must refuse rather than change
	// its geometry. An explicit affine representation retains the mapping.
	var out bytes.Buffer
	if err := WriteITKHDF5(&out, tr); !errors.Is(err, ErrInvalidParameters) || out.Len() != 0 {
		t.Fatal("unrepresentable native versor geometry emitted", err)
	}
	a := NewAffine()
	if err := a.SetMatrix(tr.Matrix()); err != nil {
		t.Fatal(err)
	}
	if err := a.SetOffset(tr.Offset()); err != nil {
		t.Fatal(err)
	}
	if err := WriteITKHDF5(&out, a); err != nil {
		t.Fatal("equivalent affine representation", err)
	}
	actual, err := ReadTransformFile(out.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	p := render.Vec3{X: 1, Y: 2, Z: 3}
	if actual.TransformPoint(p).Sub(tr.TransformPoint(p)).Length() > 1e-12 {
		t.Fatal("affine geometry changed")
	}
}
