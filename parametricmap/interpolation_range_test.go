package parametricmap

import (
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/render"
)

func TestParametricMapInterpolatesOppositeSignExtremeValues(t *testing.T) {
	scale := math.Ldexp(1, 1023)
	file := testMapFile(t, PayloadFloat64, []float64{-1, 1, 1, -1}, 1, false, core.Tag{})
	file.Dataset.Put(derivedio.Seq(tagRealWorldValueMappingSeq,
		testBoundedMapping("scaled", "ms", "milliseconds", "", -1, 1, scale, 0),
	))
	m, err := Read(file)
	if err != nil {
		t.Fatal(err)
	}

	for _, sample := range []struct {
		point render.Vec3
		value float64
	}{
		{render.Vec3{X: .5}, 0},
		{render.Vec3{Y: .5}, 0},
		{render.Vec3{X: .25, Y: .25}, -scale / 4},
	} {
		got, ok, err := m.SamplePatient(sample.point)
		if err != nil || !ok || got != sample.value {
			t.Errorf("SamplePatient(%v) = %g, %t, %v; want %g", sample.point, got, ok, err, sample.value)
		}
	}

	target := m.Frames[0].Geometry
	target.Origin.X = .25
	target.Origin.Y = .25
	target.Rows, target.Columns = 1, 1
	values, valid, err := m.Resample(target)
	if err != nil || len(values) != 1 || len(valid) != 1 || !valid[0] || values[0] != -scale/4 {
		t.Fatalf("Resample = %v, %v, %v; want [%g], [true], nil", values, valid, err, -scale/4)
	}
}
