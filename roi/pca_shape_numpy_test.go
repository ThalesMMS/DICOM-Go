package roi

import (
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"os"
	"testing"
)

type pcaNumpyReceipt struct {
	Schema                                       int
	Engine, Version, Convention, GeneratorSHA256 string
	Geometry                                     PCAShapeGeometry
	RetainedRank                                 int
	Training                                     []struct {
		ID      string
		FieldMM []float64
	}
	MeanMM               []float64
	Components           [][]float64
	StandardDeviationsMM []float64
	HeldOut              []struct {
		ID                                        string
		FieldMM, CoefficientsSD, ReconstructionMM []float64
		ResidualRMSMM                             float64
	}
}

func pcaNumpyFixture(t testing.TB) pcaNumpyReceipt {
	t.Helper()
	f, e := os.Open("testdata/pca_shape/numpy.json.gz")
	if e != nil {
		t.Fatal(e)
	}
	defer f.Close()
	r, e := gzip.NewReader(f)
	if e != nil {
		t.Fatal(e)
	}
	defer r.Close()
	b, e := io.ReadAll(io.LimitReader(r, 4<<20))
	if e != nil {
		t.Fatal(e)
	}
	var receipt pcaNumpyReceipt
	if e = json.Unmarshal(b, &receipt); e != nil {
		t.Fatal(e)
	}
	if receipt.Schema != 1 || receipt.Engine != "NumPy" || receipt.Convention != PCAShapeConvention || len(receipt.Training) != 8 || len(receipt.HeldOut) != 3 || receipt.RetainedRank != 6 || len(receipt.GeneratorSHA256) != 64 {
		t.Fatal("invalid independent receipt")
	}
	source, e := os.ReadFile("testdata/pca_shape/generate.py")
	if e != nil {
		t.Fatal(e)
	}
	hash := sha256.Sum256(source)
	if fmt.Sprintf("%x", hash) != receipt.GeneratorSHA256 {
		t.Fatal("NumPy fixture generator hash mismatch")
	}
	return receipt
}
func TestPCAShapeIndependentNumpyMultiComponent(t *testing.T) {
	f := pcaNumpyFixture(t)
	samples := make([]PCAShapeTrainingSample, len(f.Training))
	for i, s := range f.Training {
		samples[i] = PCAShapeTrainingSample{ID: s.ID, Geometry: f.Geometry, FieldMM: s.FieldMM}
	}
	m, e := TrainPCAShapeModel(nil, samples, PCAShapeTrainOptions{ID: "numpy-spheres-v1", Provenance: "project-authored sphere SDFs; NumPy SVD independent oracle", Components: f.RetainedRank})
	if e != nil {
		t.Fatal(e)
	}
	d, e := m.Data(nil, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	if len(d.Components) != f.RetainedRank {
		t.Fatal("rank mismatch")
	}
	for i, mu := range f.MeanMM {
		if math.Abs(d.MeanMM[i]-mu) > 1e-12 {
			t.Fatalf("mean %d", i)
		}
	}
	for i, sigma := range f.StandardDeviationsMM {
		if math.Abs(d.StandardDeviationsMM[i]-sigma) > 1e-9*math.Max(1, sigma) {
			t.Fatalf("sigma %d %g want %g", i, d.StandardDeviationsMM[i], sigma)
		}
	}
	for _, h := range f.HeldOut {
		fit, e := m.Fit(nil, f.Geometry, h.FieldMM, 3, .1, PCAShapeLimits{})
		if e != nil {
			t.Fatal(e)
		}
		if math.Abs(fit.ResidualRMSMM-h.ResidualRMSMM) > 1e-9 {
			t.Fatalf("%s independent residual %g want %g", h.ID, fit.ResidualRMSMM, h.ResidualRMSMM)
		}
		for i, want := range h.ReconstructionMM {
			got := d.MeanMM[i]
			for a, c := range fit.CoefficientsSD {
				got += c * d.StandardDeviationsMM[a] * d.Components[a][i]
			}
			if math.Abs(got-want) > 1e-8 {
				t.Fatalf("%s reconstruction at %d: %g want %g", h.ID, i, got, want)
			}
		}
		if h.ID == "held-2" && !fit.OutsideDistribution {
			t.Fatal("unseen elongation did not disclose OOD")
		}
		t.Logf("%s independent residual=%g mm; outside-distribution=%v", h.ID, fit.ResidualRMSMM, fit.OutsideDistribution)
	}
}

func TestPCAShapePriorHeldOutThreeDimensionalSphere(t *testing.T) {
	f := pcaNumpyFixture(t)
	samples := make([]PCAShapeTrainingSample, len(f.Training))
	for i, s := range f.Training {
		samples[i] = PCAShapeTrainingSample{ID: s.ID, Geometry: f.Geometry, FieldMM: s.FieldMM}
	}
	m, e := TrainPCAShapeModel(nil, samples, PCAShapeTrainOptions{ID: "numpy-spheres", Provenance: "independent synthetic sphere corpus", Components: 6})
	if e != nil {
		t.Fatal(e)
	}
	fit, e := m.Fit(nil, f.Geometry, f.HeldOut[0].FieldMM, 3, .1, PCAShapeLimits{})
	if e != nil {
		t.Fatal(e)
	}
	d := f.Geometry.Dimensions
	h := f.Geometry.SpacingMM
	initial := map[int]*RasterMask{}
	for z := 0; z < d[2]; z++ {
		mask := NewRasterMask(d[0], d[1])
		for y := 0; y < d[1]; y++ {
			for x := 0; x < d[0]; x++ {
				radius := math.Sqrt(sq(float64(x-5)*h[0]) + sq(float64(y-4)*h[1]) + sq(float64(z-3)*h[2]))
				if radius < 1.4 {
					mask.SetRun(y, x, x+1)
				}
			}
		}
		if mask.Count() > 0 {
			initial[z] = mask
		}
	}
	v := LevelSetVolume{Columns: d[0], Rows: d[1], Slices: d[2], SpacingMM: h, ValueAt: func(x, y, z int) (float64, bool) { return 0, true }}
	p := ShapeDetectionParams{Input: EdgePotentialImage, Stopping: FixedIterations, MaxIterations: 150}
	r, e := ShapePriorDetectionLevelSet(nil, v, LevelSetInitialization{Masks: initial}, p, LevelSetOptions{MarginMM: 100}, PCAShapePriorOptions{Model: m, Geometry: f.Geometry, CoefficientsSD: fit.CoefficientsSD, Pose: IdentityPCAShapePose(), Weight: 1, MaxResidualMM: 10})
	if e != nil {
		t.Fatal(e)
	}
	intersection, total := 0, 0
	for z := 0; z < d[2]; z++ {
		for y := 0; y < d[1]; y++ {
			for x := 0; x < d[0]; x++ {
				mask := r.Segmentation.Masks[z]
				a := mask != nil && mask.Get(x, y)
				b := f.HeldOut[0].FieldMM[(z*d[1]+y)*d[0]+x] < 0
				if a && b {
					intersection++
				}
				if a {
					total++
				}
				if b {
					total++
				}
			}
		}
	}
	dice := 2 * float64(intersection) / float64(total)
	t.Logf("unseen 3D sphere Dice=%g, field RMS %g -> %g mm", dice, r.Prior.InitialFieldRMSMM, r.Prior.FinalFieldRMSMM)
	if dice < .95 || r.Prior.FinalFieldRMSMM >= r.Prior.InitialFieldRMSMM {
		t.Fatalf("held-out 3D %g %+v", dice, r.Prior)
	}
}
