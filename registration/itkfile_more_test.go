package registration

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
)

func TestITKNativeDimensionAndTypeRefusals(t *testing.T) {
	for _, c := range itk1345Fixtures(t) {
		if c.Name != "translation2" && c.Name != "translation3" {
			continue
		}
		tr := itk1345Create(t, c.itk1345Descriptor)
		for _, format := range []string{"tfm", "mat", "h5"} {
			var b bytes.Buffer
			var err error
			if c.Dimension == 2 {
				v := tr.(Transform2D)
				switch format {
				case "tfm":
					err = WriteITKText2D(&b, v)
				case "mat":
					err = WriteITKMatlab2D(&b, v)
				case "h5":
					err = WriteITKHDF52D(&b, v)
				}
			} else {
				v := tr.(Transform)
				switch format {
				case "tfm":
					err = WriteITKText(&b, v)
				case "mat":
					err = WriteITKMatlab(&b, v)
				case "h5":
					err = WriteITKHDF5(&b, v)
				}
			}
			if err != nil {
				t.Fatal(err)
			}
			if c.Dimension == 2 {
				if _, err := ReadTransformFile(b.Bytes()); !errors.Is(err, ErrUnsupportedTransform) {
					t.Fatal("2D lifted into3D", format, err)
				}
			} else {
				if _, err := ReadTransformFile2D(b.Bytes()); !errors.Is(err, ErrUnsupportedTransform) {
					t.Fatal("3D reduced into2D", format, err)
				}
			}
		}
	}
	for _, typeName := range []string{"BSplineTransform_double_2_2", "ScaleVersor3DTransform_double_2_2", "TranslationTransform_double_2_3", "TranslationTransform_long_2_2"} {
		if _, err := newITKTransform2D(typeName); !errors.Is(err, ErrUnsupportedTransform) {
			t.Fatal("unknown2D", typeName, err)
		}
	}
	for _, kind := range []string{"TranslationTransform", "ScaleVersor3DTransform", "ScaleSkewVersor3DTransform"} {
		for _, suffix := range []string{"float_3_3", "double_3_3"} {
			if _, err := newITKTransform(kind + "_" + suffix); err != nil {
				t.Fatal("new3D dispatch", err)
			}
		}
	}
	text := "#Insight Transform File V1.0\n# Transform0\nTransform: MatrixOffsetTransformBase_float_2_2\nParameters: 1 .2 -.3 2 4 -5\nFixedParameters: 2 -3\n"
	tr, err := ReadTransformFile2D([]byte(text))
	if err != nil || tr.Kind() != KindAffine {
		t.Fatal("2D affine alias", err)
	}
	for _, bad := range []string{
		"Transform: TranslationTransform_double_2_2\nParameters: 1 2\nFixedParameters: 0 0\n",
		"Transform: Euler2DTransform_double_2_2\nParameters: 0 1\nFixedParameters: 0 0\n",
		"Transform: Similarity2DTransform_double_2_2\nParameters: NaN 0 1 2\nFixedParameters: 0 0\n",
		"Transform: CompositeTransform_double_2_2\nParameters: 1\nFixedParameters: \n",
	} {
		if _, err := ReadITKText2D(strings.NewReader("#Insight Transform File V1.0\n" + bad)); err == nil {
			t.Fatal("invalid2D record admitted", bad)
		}
	}
	var many strings.Builder
	many.WriteString("#Insight Transform File V1.0\n")
	for i := 0; i < 1025; i++ {
		fmt.Fprintln(&many, "Transform: TranslationTransform_double_2_2\nParameters: 0 0\nFixedParameters:")
	}
	if _, err := ReadITKText2D(strings.NewReader(many.String())); err == nil {
		t.Fatal("typed2D record admission")
	}
	var out bytes.Buffer
	empty, _ := NewComposite2D()
	if err := WriteITKText2D(&out, empty); err == nil || out.Len() != 0 {
		t.Fatal("empty2D text composite", err)
	}
	empty.Append(NewTranslation2D())
	if err := WriteITKMatlab2D(&out, empty); !errors.Is(err, ErrTransformFile) || out.Len() != 0 {
		t.Fatal("2D MAT composite", err)
	}
}

func TestITKDensePreflightBeforeAllocation(t *testing.T) {
	for _, name := range []string{"BSplineTransform_double_3_3", "DisplacementFieldTransform_double_3_3"} {
		tr, err := newITKTransform(name)
		if err != nil {
			t.Fatal(err)
		}
		originalParameters, originalFixed := tr.Parameters(), tr.FixedParameters()
		for _, sizes := range [][3]float64{{1 << 20, 1 << 20, 1 << 20}, {math.MaxFloat64, 5, 5}, {5.5, 5, 5}, {0, 5, 5}} {
			fixed := []float64{sizes[0], sizes[1], sizes[2], 0, 0, 0, 1, 1, 1, 1, 0, 0, 0, 1, 0, 0, 0, 1}
			if err := setITKParameters(tr, []float64{1, 2, 3}, fixed); err == nil {
				t.Fatal("dense oversized/mismatched shape admitted", name, sizes)
			}
			if !reflect.DeepEqual(tr.Parameters(), originalParameters) || !reflect.DeepEqual(tr.FixedParameters(), originalFixed) {
				t.Fatal("dense storage changed before count admission")
			}
		}
	}
}
