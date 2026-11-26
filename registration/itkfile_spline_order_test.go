package registration

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// TestITKBSplineSplineOrderNames covers the five-field BSplineTransform names
// that ITK (8573199f87) registers for spline orders other than 3, such as
// "BSplineTransform_double_3_3_1" (#2027): order 3 reads as the cubic
// B-spline, other orders are refused with the order named.
func TestITKBSplineSplineOrderNames(t *testing.T) {
	b := randomBSpline(t, [3]int{5, 4, 4}, render.Vec3{X: -3, Y: 4, Z: 1}, render.Vec3{X: 5, Y: 4, Z: 6}, obliqueRotation(t), 2, 7)
	var file bytes.Buffer
	if err := WriteITKText(&file, b); err != nil {
		t.Fatal(err)
	}
	const written = "BSplineTransform_double_3_3"
	if !strings.Contains(file.String(), "Transform: "+written+"\n") {
		t.Fatalf("written file names no %s:\n%.200s", written, file.String())
	}

	for _, name := range []string{"BSplineTransform_double_3_3_3", "BSplineTransform_float_3_3_3"} {
		data := strings.Replace(file.String(), written, name, 1)
		got, err := ReadTransformFile([]byte(data))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got.Kind() != KindBSpline || !reflect.DeepEqual(got.Parameters(), b.Parameters()) || !reflect.DeepEqual(got.FixedParameters(), b.FixedParameters()) {
			t.Fatalf("%s read as %s with different parameters", name, got.Kind())
		}
	}

	for _, order := range []string{"1", "2", "0", "4"} {
		name := "BSplineTransform_double_3_3_" + order
		_, err := ReadTransformFile([]byte(strings.Replace(file.String(), written, name, 1)))
		if !errors.Is(err, ErrTransformFile) || !errors.Is(err, ErrUnsupportedTransform) {
			t.Fatalf("%s: %v, want ErrTransformFile and ErrUnsupportedTransform", name, err)
		}
		if !strings.Contains(err.Error(), `B-spline order "`+order+`"`) {
			t.Fatalf("%s: error %q does not name the spline order", name, err)
		}
	}

	for name, want := range map[string]string{
		"BSplineTransform_double_2_2_1": "not three-dimensional",
		"BSplineTransform_long_3_3_1":   "transform type",
		"AffineTransform_double_3_3_1":  "transform type",
		"BSplineTransform_double_3_3_":  "B-spline order \"\"",
	} {
		_, err := newITKTransform(name)
		if !errors.Is(err, ErrUnsupportedTransform) || !strings.Contains(err.Error(), want) {
			t.Fatalf("%s: %v, want an error containing %q", name, err, want)
		}
	}
}
