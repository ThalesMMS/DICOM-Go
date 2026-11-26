package roi

import "testing"

func BenchmarkPCAShapeTrain(b *testing.B) {
	f := pcaNumpyFixture(b)
	samples := make([]PCAShapeTrainingSample, len(f.Training))
	for i, s := range f.Training {
		samples[i] = PCAShapeTrainingSample{ID: s.ID, Geometry: f.Geometry, FieldMM: s.FieldMM}
	}
	o := PCAShapeTrainOptions{ID: "benchmark-spheres", Provenance: "project-authored NumPy corpus", Components: 6}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := TrainPCAShapeModel(nil, samples, o); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkPCAShapeFit(b *testing.B) {
	f := pcaNumpyFixture(b)
	d := PCAShapeModelData{Version: 1, ID: "benchmark", Provenance: "independent NumPy corpus", Convention: PCAShapeConvention, Geometry: f.Geometry, TrainingSamples: 8, MeanMM: f.MeanMM, Components: f.Components, StandardDeviationsMM: f.StandardDeviationsMM}
	m, e := NewPCAShapeModel(nil, d, PCAShapeLimits{})
	if e != nil {
		b.Fatal(e)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := m.Fit(nil, f.Geometry, f.HeldOut[0].FieldMM, 3, .1, PCAShapeLimits{}); e != nil {
			b.Fatal(e)
		}
	}
}
func BenchmarkPCAShapePriorEvolution(b *testing.B) {
	_, v, init, p, prior := pcaPriorInputs(b)
	o := LevelSetOptions{MarginMM: 100}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, e := ShapePriorDetectionLevelSet(nil, v, init, p, o, prior); e != nil {
			b.Fatal(e)
		}
	}
}
