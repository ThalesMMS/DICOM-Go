package imagefilter

import (
	"context"
	"fmt"
	"math"
	"testing"
)

func BenchmarkTensorDiffusionCost(b *testing.B) {
	for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
		b.Run(string(method), func(b *testing.B) {
			for _, cell := range []struct {
				name string
				dims [3]int
			}{{"2D64", [3]int{64, 64, 1}}, {"3D16", [3]int{16, 16, 16}}} {
				b.Run(cell.name, func(b *testing.B) {
					v := tensorPhantom("oblique", cell.dims, [3]float64{.7, 1.3, 2.1})
					o := tensorTestOptions(method)
					o.Iterations = 10
					o.Workers = 4
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						if _, err := TensorAnisotropicDiffusion(context.Background(), v, o); err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		})
	}
}

func TestTensorDiffusionObliqueImpulse(t *testing.T) {
	for _, dims := range [][3]int{{17, 17, 1}, {11, 11, 11}} {
		n, _ := voxelCount(dims)
		u := make([]float64, n)
		center := ((dims[2]/2)*dims[1]+dims[1]/2)*dims[0] + dims[0]/2
		u[center] = 1
		p := tensorDiffusionPlan{n: n, strides: [3]int{1, dims[0], dims[0] * dims[1]}}
		var d [6][]float64
		for k := range d {
			d[k] = make([]float64, n)
		}
		for a := 0; a < 3; a++ {
			if dims[a] > 1 {
				p.active = append(p.active, a)
				p.inverse[a] = 1
			}
		}
		nd := float64(len(p.active))
		for i := 0; i < n; i++ {
			for _, a := range p.active {
				for _, b := range p.active {
					value := .95 / nd
					if a == b {
						value += .05
					}
					d[tensorComponent(a, b)][i] = value
				}
			}
		}
		dt := 1 / (2 * nd)
		previousL2 := 1.0
		for step := 0; step < 40; step++ {
			next := make([]float64, n)
			mass, l2 := 0.0, 0.0
			lo, hi := math.Inf(1), math.Inf(-1)
			for i := range u {
				next[i] = u[i] + dt*p.update(dims, u, d, i)
				mass += next[i]
				l2 += next[i] * next[i]
				lo = math.Min(lo, next[i])
				hi = math.Max(hi, next[i])
			}
			if math.Abs(mass-1) > 3e-14 || l2 > previousL2*(1+5e-14) {
				t.Fatalf("step%d mass%g L2%g previous%g", step, mass, l2, previousL2)
			}
			if step == 0 {
				if lo >= 0 {
					t.Fatal("oblique stencil unexpectedly monotone")
				}
				t.Logf("impulse dims=%v dt=%.12g alpha=.05 D=.05I+.95vvT v=unit(1,...,1) min=%.12g max=%.12g L2_squared=%.12g", dims, dt, lo, hi, l2)
			}
			u = next
			previousL2 = l2
		}
	}
}

func TestTensorDiffusionMeasuredStructures(t *testing.T) {
	dims := [3]int{33, 33, 1}
	spacing := [3]float64{.7, 1.3, 0}
	for _, kind := range []string{"constant", "line", "edge", "junction"} {
		truth := NewVolume(dims, spacing)
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				xx, yy := float64(x-16), float64(y-16)
				v := 0.0
				switch kind {
				case "constant":
					v = 3
				case "line":
					v = 4 * math.Exp(-xx*xx/2)
				case "edge":
					if xx >= 0 {
						v = 4
					}
				case "junction":
					v = 4*math.Exp(-xx*xx/2) + 4*math.Exp(-yy*yy/2)
				}
				truth.Data[truth.Index(x, y, 0)] = float32(v)
			}
		}
		noisy := truth.Clone()
		state := uint32(1510)
		for i := range noisy.Data {
			state ^= state << 13
			state ^= state >> 17
			state ^= state << 5
			noisy.Data[i] += float32(float64(state)/float64(math.MaxUint32) - .5)
		}
		methods := []struct {
			name string
			run  func() (*Volume, error)
		}{
			{"input", func() (*Volume, error) { return noisy, nil }},
		}
		for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
			methods = append(methods, struct {
				name string
				run  func() (*Volume, error)
			}{string(method), func() (*Volume, error) {
				o := tensorTestOptions(method)
				o.SigmaMM[2] = 0
				o.RhoMM[2] = 0
				o.Iterations = 20
				o.TimeStep = .04
				r, e := TensorAnisotropicDiffusion(context.Background(), noisy, o)
				if e != nil {
					return nil, e
				}
				return r.Volume, nil
			}})
		}
		methods = append(methods, struct {
			name string
			run  func() (*Volume, error)
		}{"PeronaMalik", func() (*Volume, error) {
			return GradientAnisotropicDiffusion(context.Background(), noisy, AnisotropicDiffusionOptions{Iterations: 20, TimeStep: .04, Conductance: 1, Workers: 4})
		}}, struct {
			name string
			run  func() (*Volume, error)
		}{"MCDE", func() (*Volume, error) {
			return CurvatureAnisotropicDiffusion(context.Background(), noisy, AnisotropicDiffusionOptions{Iterations: 20, TimeStep: .04, Conductance: 1, Workers: 4})
		}})
		for _, method := range methods {
			out, e := method.run()
			if e != nil {
				t.Fatal(e)
			}
			mse, residual, along, center, edgeContrast := 0.0, 0.0, 0.0, 0.0, 0.0
			lo, hi := math.Inf(1), math.Inf(-1)
			for y := 0; y < dims[1]; y++ {
				for x := 0; x < dims[0]; x++ {
					i := out.Index(x, y, 0)
					v := float64(out.Data[i])
					difference := v - float64(truth.Data[i])
					mse += difference * difference
					difference = v - float64(noisy.Data[i])
					residual += difference * difference
					if y+1 < dims[1] {
						d := v - float64(out.At(x, y+1, 0))
						along += d * d
					}
					if x == 16 {
						center += v / float64(dims[1])
					}
					if x == 32 {
						edgeContrast += v / float64(dims[1])
					}
					if x == 0 {
						edgeContrast -= v / float64(dims[1])
					}
					lo = math.Min(lo, v)
					hi = math.Max(hi, v)
				}
			}
			mse /= float64(len(out.Data))
			residual = math.Sqrt(residual / float64(len(out.Data)))
			along = math.Sqrt(along / float64(dims[0]*(dims[1]-1)))
			if math.IsNaN(mse) || math.IsInf(mse, 0) {
				t.Fatal("nonfinite metric")
			}
			t.Log(fmt.Sprintf("structure=%s method=%s MSE=%.9g residual_RMS=%.9g adjacent_y_RMS=%.9g center_line_mean=%.9g side_contrast=%.9g min=%.9g max=%.9g", kind, method.name, mse, residual, along, center, edgeContrast, lo, hi))
			if kind == "constant" && method.name != "input" && mse >= .085 {
				t.Fatal("constant noise worsened")
			}
		}
	}
}
