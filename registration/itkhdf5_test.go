package registration

import (
	"bytes"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// The .h5 files of testdata/itkhdf5 were written by the HDF5 C library in
// ITK's HDF5TransformIO layout (see testdata/itkhdf5/README.md); their
// parameters are those of the ITK-written .tfm files or dyadic fractions
// recomputed here.

func readHDF5Sample(t testing.TB, name string) Transform {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "itkhdf5", name))
	if err != nil {
		t.Fatal(err)
	}
	transform, err := ReadTransformFile(data)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return transform
}

func sameVector(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// sameTransform compares kinds and parameter vectors exactly, recursing
// into composites.
func sameTransform(t *testing.T, name string, got, want Transform) {
	t.Helper()
	if got.Kind() != want.Kind() {
		t.Fatalf("%s: kind %s, want %s", name, got.Kind(), want.Kind())
	}
	if c, ok := got.(*Composite); ok {
		w := want.(*Composite)
		if c.Len() != w.Len() {
			t.Fatalf("%s: %d components, want %d", name, c.Len(), w.Len())
		}
		for i := 0; i < c.Len(); i++ {
			sameTransform(t, name, c.Transform(i), w.Transform(i))
		}
		return
	}
	if !sameVector(got.Parameters(), want.Parameters()) || !sameVector(got.FixedParameters(), want.FixedParameters()) {
		t.Fatalf("%s: %s parameters %v / %v, want %v / %v", name, got.Kind(), got.Parameters(), got.FixedParameters(), want.Parameters(), want.FixedParameters())
	}
}

// TestReadITKHDF5MatchesTheTextFiles: the HDF5 files holding the
// transforms of ITK's affine.tfm and composite.tfm, in both superblock
// formats, read to the same transforms, bit for bit, and map ITK's
// reference points exactly as the text files do.
func TestReadITKHDF5MatchesTheTextFiles(t *testing.T) {
	reference := itkReferencePoints(t)
	for _, c := range []struct{ h5, tfm, points string }{
		{"affine.h5", "affine.tfm", "affine"},
		{"composite.h5", "composite.tfm", "composite"},
		{"composite_v0.h5", "composite.tfm", "composite"},
	} {
		got := readHDF5Sample(t, c.h5)
		want, _ := readITKSample(t, c.tfm)
		sameTransform(t, c.h5, got, want)
		for _, p := range reference[c.points] {
			if g, w := got.TransformPoint(p.in), want.TransformPoint(p.in); g != w {
				t.Fatalf("%s: %v maps to %v, the text file to %v", c.h5, p.in, g, w)
			}
			if d := got.TransformPoint(p.in).Sub(p.out).Length(); d > 1e-9*math.Max(1, p.out.Length()) {
				t.Fatalf("%s: %v is %g from ITK's point", c.h5, p.in, d)
			}
		}
	}
}

func TestReadITKHDF5FloatParametersAndOldNames(t *testing.T) {
	got := readHDF5Sample(t, "affine_float_misspelled_v0.h5")
	want, _ := readITKSample(t, "affine.tfm")
	parameters := want.Parameters()
	for i, v := range parameters {
		parameters[i] = float64(float32(v))
	}
	if got.Kind() != KindAffine || !sameVector(got.Parameters(), parameters) || !sameVector(got.FixedParameters(), want.FixedParameters()) {
		t.Fatalf("float affine %v / %v", got.Parameters(), got.FixedParameters())
	}
}

// fieldFixture is the displacement field of the HDF5 fixtures: 6 × 5 × 4
// samples, origin (−10, 5, 2.5), spacing (2.5, 2, 3), index x along
// patient y and index y along −x, and vector component i equal to
// ((37i mod 101) − 50)/64.
func fieldFixture() (fixed, parameters []float64) {
	fixed = []float64{6, 5, 4, -10, 5, 2.5, 2.5, 2, 3, 0, -1, 0, 1, 0, 0, 0, 0, 1}
	parameters = make([]float64, 6*5*4*3)
	for i := range parameters {
		parameters[i] = float64((i*37)%101-50) / 64
	}
	return fixed, parameters
}

// expectedFieldDisplacement interpolates the fixture field at p
// independently of DisplacementFieldTransform: zero beyond half a sample
// outside the grid, trilinear with clamped neighbours inside.
func expectedFieldDisplacement(p render.Vec3) render.Vec3 {
	fixed, values := fieldFixture()
	size := [3]int{6, 5, 4}
	// index = Dᵀ·(p − origin)/spacing; D's columns are (0, 1, 0),
	// (−1, 0, 0) and (0, 0, 1).
	rel := p.Sub(render.Vec3{X: fixed[3], Y: fixed[4], Z: fixed[5]})
	index := [3]float64{rel.Y / fixed[6], -rel.X / fixed[7], rel.Z / fixed[8]}
	for axis, v := range index {
		if v < -0.5 || v >= float64(size[axis])-0.5 {
			return render.Vec3{}
		}
	}
	var out [3]float64
	for corner := 0; corner < 8; corner++ {
		w := 1.0
		var at [3]int
		for axis := 0; axis < 3; axis++ {
			v := math.Min(math.Max(index[axis], 0), float64(size[axis]-1))
			low := math.Floor(v)
			f := v - low
			if corner>>axis&1 == 1 {
				w *= f
				at[axis] = min(int(low)+1, size[axis]-1)
			} else {
				w *= 1 - f
				at[axis] = int(low)
			}
		}
		k := (at[2]*size[1]+at[1])*size[0] + at[0]
		for d := 0; d < 3; d++ {
			out[d] += w * values[3*k+d]
		}
	}
	return render.Vec3{X: out[0], Y: out[1], Z: out[2]}
}

// TestReadITKHDF5DisplacementFieldComposite: an ANTs-like composite of an
// affine and a deflated displacement field reads with the fixture's
// parameters and applies the field first, then the affine.
func TestReadITKHDF5DisplacementFieldComposite(t *testing.T) {
	got := readHDF5Sample(t, "affine_field.h5")
	c, ok := got.(*Composite)
	if !ok || c.Len() != 2 || c.Transform(0).Kind() != KindDisplacementField || c.Transform(1).Kind() != KindAffine {
		t.Fatalf("read %T %v", got, got.Kind())
	}
	fixed, parameters := fieldFixture()
	field := c.Transform(0)
	if !sameVector(field.FixedParameters(), fixed) || !sameVector(field.Parameters(), parameters) {
		t.Fatal("displacement field parameters differ from the fixture's")
	}
	affine, _ := readITKSample(t, "affine.tfm")
	sameTransform(t, "affine", c.Transform(1), affine)
	for _, p := range []render.Vec3{{X: -14, Y: -1, Z: 3.7}, {X: -12.3, Y: 9.1, Z: 8}, {X: -18.2, Y: 7.6, Z: 11.9}, {X: 0, Y: 0, Z: 0}, {X: 40, Y: 40, Z: 40}} {
		want := affine.TransformPoint(p.Add(expectedFieldDisplacement(p)))
		if d := got.TransformPoint(p).Sub(want).Length(); d > 1e-12 {
			t.Fatalf("%v maps %g from the expected point", p, d)
		}
	}
	// The same composite through an ITK text file.
	var text bytes.Buffer
	if err := WriteITKText(&text, got); err != nil {
		t.Fatal(err)
	}
	again, err := ReadTransformFile(text.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sameTransform(t, "text round trip", again, got)
	// Not linear: no single affine, hence no DICOM matrix.
	if _, err := c.CollapseToAffine(); !errors.Is(err, ErrUnsupportedTransform) {
		t.Fatalf("collapse: %v", err)
	}
}

func TestReadITKHDF5ChunkedFieldAndBSpline(t *testing.T) {
	field := readHDF5Sample(t, "field_chunks_v0.h5")
	fixed, parameters := fieldFixture()
	if field.Kind() != KindDisplacementField || !sameVector(field.FixedParameters(), fixed) || !sameVector(field.Parameters(), parameters) {
		t.Fatal("chunked displacement field differs from the fixture's")
	}
	b := readHDF5Sample(t, "bspline.h5")
	spline, ok := b.(*BSpline)
	if !ok {
		t.Fatalf("read %T", b)
	}
	wantFixed := []float64{5, 5, 4, -20, -15, -10, 10, 8, 12, 1, 0, 0, 0, 1, 0, 0, 0, 1}
	if !sameVector(spline.FixedParameters(), wantFixed) {
		t.Fatalf("B-spline fixed parameters %v", spline.FixedParameters())
	}
	for i, v := range spline.Parameters() {
		if want := float64((i*13)%29-14) / 32; v != want {
			t.Fatalf("B-spline parameter %d is %v, want %v", i, v, want)
		}
	}
	if spline.MeshSize() != [3]int{2, 2, 1} {
		t.Fatalf("mesh %v", spline.MeshSize())
	}
	var text bytes.Buffer
	if err := WriteITKText(&text, spline); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text.String(), "\nTransform: BSplineTransform_double_3_3\n") {
		t.Fatalf("text file:\n%s", text.String())
	}
	again, err := ReadTransformFile(text.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	sameTransform(t, "B-spline text round trip", again, spline)
	if err := WriteITKMatlab(&bytes.Buffer{}, spline); !errors.Is(err, ErrTransformFile) {
		t.Fatalf("B-spline to MATLAB: %v", err)
	}
}

// The same HDF5 C-generated composite uses several symbol-table nodes
// in the earliest format and a dense name index in the HDF5 1.8 format.
func TestReadITKHDF5ManyTransforms(t *testing.T) {
	got := readHDF5Sample(t, "many_v0.h5")
	c, ok := got.(*Composite)
	if !ok || c.Len() != 9 {
		t.Fatalf("read %T", got)
	}
	dense := readHDF5Sample(t, "many_dense.h5")
	sameTransform(t, "dense versus symbol-table order", dense, c)
}

func TestReadITKHDF5Rejects(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("testdata", "itkhdf5", "composite.h5"))
	if err != nil {
		t.Fatal(err)
	}
	for name, damaged := range map[string][]byte{
		"truncated":      data[:len(data)/2],
		"signature only": data[:8],
		"bad superblock": append(append([]byte{}, data[:8]...), 7, 0, 0, 0),
	} {
		if _, err := ReadTransformFile(damaged); !errors.Is(err, ErrTransformFile) {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// TestDisplacementFieldTransformJacobians: the parameter Jacobian is exact
// (the mapping is linear in the vectors), the position Jacobian matches
// central differences inside cells, points outside the field stay, and
// inside the field the transform equals FieldMapping.
func TestDisplacementFieldTransformJacobians(t *testing.T) {
	fixed, parameters := fieldFixture()
	created, err := newITKTransform("DisplacementFieldTransform_double_3_3")
	if err != nil {
		t.Fatal(err)
	}
	transform := created.(*DisplacementFieldTransform)
	if err := transform.SetFixedParameters(fixed); err != nil {
		t.Fatal(err)
	}
	if err := transform.SetParameters(parameters); err != nil {
		t.Fatal(err)
	}
	var jacobian Jacobian
	for _, p := range []render.Vec3{{X: -14.3, Y: -0.6, Z: 3.7}, {X: -12.1, Y: 9.4, Z: 8.2}, {X: -17.7, Y: 7.6, Z: 2.9}} {
		transform.JacobianWithRespectToParameters(p, &jacobian)
		base := transform.TransformPoint(p)
		for _, parameter := range []int{0, 37, 200, 359} {
			moved := transform.Clone()
			delta := make([]float64, transform.NumberOfParameters())
			delta[parameter] = 1
			if err := moved.UpdateParameters(delta, 1); err != nil {
				t.Fatal(err)
			}
			change := moved.TransformPoint(p).Sub(base)
			want := render.Vec3{X: jacobian.At(0, parameter), Y: jacobian.At(1, parameter), Z: jacobian.At(2, parameter)}
			if change.Sub(want).Length() > 1e-12 {
				t.Fatalf("%v, parameter %d: change %v, Jacobian %v", p, parameter, change, want)
			}
		}
		position := transform.JacobianWithRespectToPosition(p)
		const h = 1e-6
		for axis := 0; axis < 3; axis++ {
			var step render.Vec3
			switch axis {
			case 0:
				step.X = h
			case 1:
				step.Y = h
			case 2:
				step.Z = h
			}
			numeric := transform.TransformPoint(p.Add(step)).Sub(transform.TransformPoint(p.Sub(step))).Scale(1 / (2 * h))
			for row, v := range []float64{numeric.X, numeric.Y, numeric.Z} {
				if math.Abs(position[row][axis]-v) > 1e-6 {
					t.Fatalf("%v: ∂T%d/∂x%d = %v, numeric %v", p, row, axis, position[row][axis], v)
				}
			}
		}
	}
	outside := render.Vec3{X: 100, Y: 100, Z: 100}
	if transform.TransformPoint(outside) != outside || transform.JacobianWithRespectToPosition(outside) != Identity3() {
		t.Fatal("a point outside the field moved")
	}
	mapping, err := FieldMapping(transform.Field())
	if err != nil {
		t.Fatal(err)
	}
	p := render.Vec3{X: -12.1, Y: 9.4, Z: 8.2}
	if q, ok := mapping(p); !ok || q != transform.TransformPoint(p) {
		t.Fatalf("FieldMapping %v %v, transform %v", q, ok, transform.TransformPoint(p))
	}
	if err := transform.SetFixedParameters([]float64{0.5, 1, 1}); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("short fixed parameters: %v", err)
	}
	if _, err := NewDisplacementFieldTransform(nil); !errors.Is(err, ErrInvalidParameters) {
		t.Fatalf("nil field: %v", err)
	}
}

func BenchmarkReadITKHDF5Composite(b *testing.B) {
	data, err := os.ReadFile(filepath.Join("testdata", "itkhdf5", "affine_field.h5"))
	if err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(data)))
	for i := 0; i < b.N; i++ {
		if _, err := ReadTransformFile(data); err != nil {
			b.Fatal(err)
		}
	}
}
