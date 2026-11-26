package imagefilter

import (
	"context"
	"math"
	"slices"
	"testing"
)

// ITK's itkHessianToObjectnessMeasureImageFilterTest runs 2-D dark lines and
// blobs. Analytic Gaussian profiles give known center Hessians for both
// object dimensions, including a line with a nonzero mixed derivative.
func TestITKReferenceVesselness2DObjectness(t *testing.T) {
	const sigma, amplitude, gamma = 1.5, 80.0, 20.0
	line := func(x, _ float64) float64 { return x * x }
	oblique := func(x, y float64) float64 {
		normalDistance := (3*x + 4*y) / 5
		return normalDistance * normalDistance
	}
	blob := func(x, y float64) float64 { return x*x + y*y }
	lineCurvature := amplitude / (2 * math.Sqrt2)
	cases := []struct {
		name       string
		distance2  func(float64, float64) float64
		curvatures [2]float64
	}{
		{"line", line, [2]float64{0, lineCurvature}},
		{"oblique-line", oblique, [2]float64{0, lineCurvature}},
		{"blob", blob, [2]float64{amplitude / 4, amplitude / 4}},
	}
	for _, phantom := range cases {
		t.Run(phantom.name, func(t *testing.T) {
			for _, polarity := range []struct {
				name string
				sign float64
			}{{"bright", 1}, {"dark", -1}} {
				t.Run(polarity.name, func(t *testing.T) {
					src := fillVolume([3]int{81, 61, 1}, [3]float64{0.25, 0.4, 0}, func(x, y, _ float64) float64 {
						return 100 + polarity.sign*amplitude*math.Exp(-phantom.distance2(x, y)/(2*sigma*sigma))
					})
					small, large := phantom.curvatures[0], phantom.curvatures[1]
					ratio := small / large
					strength := 1 - math.Exp(-(small*small+large*large)/(2*gamma*gamma))
					wantLine := strength * math.Exp(-2*ratio*ratio)
					wantBlob := strength * (1 - math.Exp(-2*ratio*ratio))
					for _, object := range []struct {
						name      string
						structure HessianStructure
						scaled    bool
						want      float64
					}{
						{"line-response", StructureTube, false, wantLine},
						{"line-scaled", StructureTube, true, wantLine * large},
						{"blob-response", StructureBlob, false, wantBlob},
						{"blob-scaled", StructureBlob, true, wantBlob * large},
					} {
						t.Run(object.name, func(t *testing.T) {
							opts := VesselnessOptions{
								Structure: object.structure, SigmasMM: []float64{sigma},
								Gamma: gamma, DarkObjects: polarity.sign < 0,
								ScaleByLargestEigenvalue: object.scaled, Workers: 1,
							}
							result := itkReferenceVesselness(t, src, opts)
							got := float64(result.Response.Data[centre(src)])
							// The recursive Gaussian is an approximation to the continuum formula.
							if !(math.Abs(got-object.want) <= 1e-4+0.02*object.want) {
								t.Fatalf("center response %.8g, want %.8g", got, object.want)
							}
							opts.DarkObjects = !opts.DarkObjects
							reversed := itkReferenceVesselness(t, src, opts)
							if got := reversed.Response.Data[centre(src)]; got != 0 {
								t.Fatalf("opposite polarity center responded %g", got)
							}
						})
					}
				})
			}
		})
	}
}

// itkMultiScaleHessianBasedMeasureImageFilterTest exercises both schedules and
// requests a scale image. Compare the public multiscale result at every pixel
// against separate single-scale evaluations on two distinct dark line widths.
func TestITKReferenceVesselnessScaleMap(t *testing.T) {
	dims := [3]int{129, 17, 1}
	spacing := [3]float64{0.25, 0.5, 0}
	src := fillVolume(dims, spacing, func(x, _, _ float64) float64 {
		narrow, wide := (x+8)/0.55, (x-8)/1.9
		return -80*math.Exp(-narrow*narrow/2) - 80*math.Exp(-wide*wide/2)
	})
	for _, tc := range []struct {
		name    string
		maximum float64
		steps   int
		linear  bool
		want    []float64
	}{
		{"logarithmic", 6, 4, false, []float64{0.75, 1.5, 3, 6}},
		{"equispaced", 3, 4, true, []float64{0.75, 1.5, 2.25, 3}},
		{"default-steps", 6, 0, false, []float64{0.75, 1.5, 3, 6}},
		{"one-step", 6, 1, false, []float64{0.75}},
		{"equal-endpoints", 0.75, 4, true, []float64{0.75}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := VesselnessOptions{
				DarkObjects: true, Gamma: 5, ScaleByLargestEigenvalue: true,
				SigmaMinMM: 0.75, SigmaMaxMM: tc.maximum, SigmaSteps: tc.steps,
				EquispacedSigmas: tc.linear, ScalesOutput: true, Workers: 1,
			}
			result := itkReferenceVesselness(t, src, opts)
			if len(result.SigmasMM) != len(tc.want) {
				t.Fatalf("scales %v, want %v", result.SigmasMM, tc.want)
			}
			if result.ScaleMM == nil || result.ScaleMM.Dims != dims || result.ScaleMM.SpacingMM != spacing {
				t.Fatal("scale output did not preserve the source grid")
			}
			wantResponse := make([]float32, len(src.Data))
			wantScale := make([]float32, len(src.Data))
			for i, sigma := range result.SigmasMM {
				if math.Abs(sigma-tc.want[i]) > 1e-12 {
					t.Fatalf("scale %d is %.16g, want %g", i, sigma, tc.want[i])
				}
				single := opts
				single.SigmasMM = []float64{sigma}
				single.ScalesOutput = false
				response := itkReferenceVesselness(t, src, single)
				if response.ScaleMM != nil {
					t.Fatal("unexpected scale image with output disabled")
				}
				for j, value := range response.Response.Data {
					if value > wantResponse[j] {
						wantResponse[j], wantScale[j] = value, float32(sigma)
					}
				}
			}
			for i, value := range result.Response.Data {
				if value != wantResponse[i] || result.ScaleMM.Data[i] != wantScale[i] {
					t.Fatalf("sample %d got response %g at σ=%g, want %g at σ=%g",
						i, value, result.ScaleMM.Data[i], wantResponse[i], wantScale[i])
				}
			}
			left, right := src.Index(32, 8, 0), src.Index(96, 8, 0)
			if a, b := result.Response.Data[left], result.Response.Data[right]; !(a > 5 && b > 5) {
				t.Fatalf("line center responses %g and %g, want both > 5", a, b)
			}
			if len(tc.want) > 1 && !(result.ScaleMM.Data[left] < result.ScaleMM.Data[right]) {
				t.Fatal("different line widths did not select different scales")
			}
		})
	}
}

func TestITKReferenceVesselnessScaleTies(t *testing.T) {
	// An ideal line's unscaled objectness without gamma is one at every
	// positive scale. The first configured scale therefore owns the center.
	src := fillVolume([3]int{81, 19, 1}, [3]float64{0.25, 0.5, 0}, func(x, _, _ float64) float64 {
		return math.Exp(-x * x / 2)
	})
	for _, tc := range []struct {
		name  string
		order []float64
	}{
		{"ascending", []float64{1, 2}},
		{"descending", []float64{2, 1}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			result := itkReferenceVesselness(t, src, VesselnessOptions{
				SigmasMM: tc.order, Gamma: -1, ScalesOutput: true, Workers: 1,
			})
			if !slices.Equal(result.SigmasMM, tc.order) {
				t.Fatalf("evaluated scales %v, want %v", result.SigmasMM, tc.order)
			}
			c := centre(src)
			if got, scale := result.Response.Data[c], result.ScaleMM.Data[c]; got != 1 || scale != float32(tc.order[0]) {
				t.Fatalf("center response %g at σ=%g, want 1 at σ=%g", got, scale, tc.order[0])
			}
			result.SigmasMM[0] = -1
			if tc.order[0] < 0 {
				t.Fatal("result aliases the requested scales")
			}
		})
	}
}

// ITK's synthetic Sato line has zero axial curvature. Add bowls and saddles
// with positive and negative axial curvature to isolate the two alpha weights.
func TestITKReferenceSatoAxialCurvature(t *testing.T) {
	const sigma = 1.2
	for _, curvature := range []struct {
		name  string
		axial float64
	}{{"negative", -0.5}, {"positive", 0.5}} {
		t.Run(curvature.name, func(t *testing.T) {
			src := fillVolume([3]int{41, 29, 25}, [3]float64{0.5, 0.75, 1}, func(x, y, z float64) float64 {
				return (-2*x*x - y*y + curvature.axial*z*z) / 2
			})
			for _, weights := range []struct {
				name           string
				alpha1, alpha2 float64
			}{{"ITK-defaults", 0.5, 2}, {"reversed-weights", 2, 0.5}} {
				t.Run(weights.name, func(t *testing.T) {
					result := itkReferenceVesselness(t, src, VesselnessOptions{
						Measure: VesselnessSato, SigmasMM: []float64{sigma},
						Alpha1: weights.alpha1, Alpha2: weights.alpha2, Workers: 1,
					})
					alpha := weights.alpha2
					if curvature.axial < 0 {
						alpha = weights.alpha1
					}
					ratio := curvature.axial / alpha
					want := sigma * sigma * math.Exp(-ratio*ratio/2)
					if got := float64(result.Response.Data[centre(src)]); !(math.Abs(got-want) <= 0.01*want) {
						t.Fatalf("quadratic center response %g, want %g", got, want)
					}
				})
			}
		})
	}
}

func itkReferenceVesselness(t *testing.T, src *Volume, opts VesselnessOptions) *VesselnessResult {
	t.Helper()
	result, err := Vesselness(context.Background(), src, opts)
	if err != nil {
		t.Fatal(err)
	}
	return result
}
