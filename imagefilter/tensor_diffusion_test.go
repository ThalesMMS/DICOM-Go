package imagefilter

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

func tensorTestOptions(method TensorDiffusionMethod) TensorDiffusionOptions {
	return TensorDiffusionOptions{Method: method, SigmaMM: [3]float64{.5, .5, .5}, RhoMM: [3]float64{1, 1, 1}, Alpha: .05, Threshold: .3, Iterations: 4, Workers: 2}
}

func tensorPhantom(kind string, dims [3]int, spacing [3]float64) *Volume {
	v := NewVolume(dims, spacing)
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				xx, yy, zz := float64(x)-float64(dims[0]-1)/2, float64(y)-float64(dims[1]-1)/2, float64(z)-float64(dims[2]-1)/2
				value := 0.0
				switch kind {
				case "constant":
					value = 7
				case "line":
					value = 4*math.Exp(-xx*xx/2) + .5*math.Sin(.8*yy)
				case "edge":
					if xx >= 0 {
						value = 4
					}
					value += .3 * math.Sin(.8*yy)
				case "junction":
					value = 4*math.Exp(-xx*xx/2) + 4*math.Exp(-yy*yy/2)
				case "oblique":
					value = 4*math.Exp(-(xx+.6*yy+.3*zz)*(xx+.6*yy+.3*zz)/3) + .4*math.Sin(1.7*xx+.4*yy+.9*zz)
				default:
					panic(kind)
				}
				v.Data[v.Index(x, y, z)] = float32(value)
			}
		}
	}
	return v
}

// Independent oracle assembles the energy gradient by scattering every
// orthant's differences. Production gathers diagonal/cross terms instead.
func tensorOrthantOracle(dims [3]int, h [3]float64, u []float64, d [][3][3]float64) []float64 {
	active := []int{}
	for a := 0; a < 3; a++ {
		if dims[a] > 1 {
			active = append(active, a)
		}
	}
	orthants := 1 << len(active)
	out := make([]float64, len(u))
	for i := range u {
		coord := [3]int{i % dims[0], i / dims[0] % dims[1], i / (dims[0] * dims[1])}
		for mask := 0; mask < orthants; mask++ {
			var gradient [3]float64
			neighbors := [3]int{-1, -1, -1}
			var derivative [3]float64
			for k, a := range active {
				sign := 1
				if mask&(1<<k) != 0 {
					sign = -1
				}
				c := coord
				c[a] += sign
				if c[a] < 0 || c[a] >= dims[a] {
					continue
				}
				j := (c[2]*dims[1]+c[1])*dims[0] + c[0]
				neighbors[a] = j
				derivative[a] = float64(sign) / h[a]
				gradient[a] = (u[j] - u[i]) * derivative[a]
			}
			for _, a := range active {
				if neighbors[a] < 0 {
					continue
				}
				flux := 0.0
				for _, b := range active {
					flux += d[i][a][b] * gradient[b]
				}
				value := flux * derivative[a] / float64(orthants)
				out[i] += value
				out[neighbors[a]] -= value
			}
		}
	}
	return out
}

func TestTensorDiffusionOrthantReference(t *testing.T) {
	for _, dims := range [][3]int{{5, 4, 1}, {4, 3, 3}, {1, 5, 4}} {
		h := [3]float64{.7, 1.3, 2.1}
		n, _ := voxelCount(dims)
		u := make([]float64, n)
		d := make([][3][3]float64, n)
		p := tensorDiffusionPlan{n: n, strides: [3]int{1, dims[0], dims[0] * dims[1]}}
		var stored [6][]float64
		for k := range stored {
			stored[k] = make([]float64, n)
		}
		for a := 0; a < 3; a++ {
			if dims[a] > 1 {
				p.active = append(p.active, a)
				p.inverse[a] = 1 / h[a]
			}
		}
		for i := range u {
			u[i] = math.Sin(float64(i)*.7) + float64(i%3)
			// Diagonal base plus a rank-one outer product is independently SPD.
			v := [3]float64{.2 * math.Sin(float64(i)), .3, .4 * math.Cos(float64(i))}
			for a := 0; a < 3; a++ {
				for b := 0; b < 3; b++ {
					d[i][a][b] = v[a] * v[b]
					if a == b {
						d[i][a][b] += .15
					}
					stored[tensorComponent(a, b)][i] = d[i][a][b]
				}
			}
		}
		want := tensorOrthantOracle(dims, h, u, d)
		mass, energy := 0.0, 0.0
		for i := range u {
			got := p.update(dims, u, stored, i)
			if math.Abs(got-want[i]) > 2e-14 {
				t.Fatalf("dims%v i%d got%.17g want%.17g", dims, i, got, want[i])
			}
			mass += got
			energy += u[i] * got
		}
		if math.Abs(mass) > 1e-12 || energy > 1e-12 {
			t.Fatalf("mass%g energy%g", mass, energy)
		}
	}
}

func TestTensorDiffusionEigenReference(t *testing.T) {
	alpha, threshold := .05, .3
	for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
		for _, angle := range []float64{0, .3, 1.2, math.Pi / 2} {
			c, s := math.Cos(angle), math.Sin(angle)
			j := [6]float64{4*c*c + .2*s*s, 3.8 * c * s, 0, 4*s*s + .2*c*c, 0, 0}
			d := tensorDiffusivity(j, []int{0, 1}, method, alpha, threshold, 2)
			e := math.Exp(-math.Pow(threshold/3.8, 2))
			hi, lo := alpha, alpha+(1-alpha)*e
			if method == EdgeEnhancing {
				hi, lo = 1-(1-alpha)*e, 1
			}
			want := [6]float64{hi*c*c + lo*s*s, (hi - lo) * c * s, 0, hi*s*s + lo*c*c, 0, 0}
			for k := range d {
				if math.Abs(d[k]-want[k]) > 5e-15 {
					t.Fatalf("%s angle%g component%d got%g want%g", method, angle, k, d[k], want[k])
				}
			}
		}
		for _, active := range [][]int{{0, 1}, {1, 2}, {0, 1, 2}} {
			j := [6]float64{3, 0, 0, 3, 0, 3}
			d := tensorDiffusivity(j, active, method, alpha, threshold, 2)
			want := alpha
			if method == EdgeEnhancing {
				want = 1
			}
			for _, a := range active {
				if math.Abs(d[tensorComponent(a, a)]-want) > 1e-15 {
					t.Fatal("degenerate tensor")
				}
			}
		}
	}
	// A fully rotated known eigensystem, not a second eigensolver.
	v := [3][3]float64{{1 / math.Sqrt(2), 1 / math.Sqrt(6), 1 / math.Sqrt(3)}, {-1 / math.Sqrt(2), 1 / math.Sqrt(6), 1 / math.Sqrt(3)}, {0, -2 / math.Sqrt(6), 1 / math.Sqrt(3)}}
	for _, lambda := range [][3]float64{{.1, 2, 5}, {1, 1, 7}, {0, 0, 0}, {1e-20, 2e-20, 3e-20}, {1e40, 1.1e40, 2e40}} {
		var j [6]float64
		for a := 0; a < 3; a++ {
			for b := a; b < 3; b++ {
				for k := 0; k < 3; k++ {
					j[tensorComponent(a, b)] += lambda[k] * v[a][k] * v[b][k]
				}
			}
		}
		got, q := tensorEigen3(j)
		scale := math.Max(lambda[2], 1e-300)
		for k := 0; k < 3; k++ {
			if math.Abs(got[k]-lambda[k])/scale > 2e-14 {
				t.Fatalf("eigenvalues %v want%v", got, lambda)
			}
			for a := 0; a < 3; a++ {
				residual := 0.0
				for b := 0; b < 3; b++ {
					residual += j[tensorComponent(a, b)] * q[b][k]
				}
				if math.Abs(residual-got[k]*q[a][k])/scale > 2e-14 {
					t.Fatal("eigenvector residual")
				}
			}
		}
		for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
			d := tensorDiffusivity(j, []int{0, 1, 2}, method, alpha, threshold, 2)
			var want [6]float64
			for k := 0; k < 3; k++ {
				gap := lambda[2] - lambda[k]
				if method == EdgeEnhancing {
					gap = lambda[k] - lambda[0]
				}
				e := 0.0
				if gap > 0 {
					e = math.Exp(-math.Pow(threshold/gap, 2))
				}
				weight := alpha + (1-alpha)*e
				if method == EdgeEnhancing {
					weight = 1 - (1-alpha)*e
				}
				for a := 0; a < 3; a++ {
					for b := a; b < 3; b++ {
						want[tensorComponent(a, b)] += weight * v[a][k] * v[b][k]
					}
				}
			}
			for k := range d {
				if math.Abs(d[k]-want[k]) > 5e-14 {
					t.Fatalf("3D %s component %d got%g want%g", method, k, d[k], want[k])
				}
			}
		}
	}
}

// End-to-end independent 2D reference: direct product-kernel convolution,
// angle eigensystem and orthant scatter. Float64 through all iterates.
func tensorReference2D(src *Volume, o TensorDiffusionOptions) []float64 {
	u := make([]float64, len(src.Data))
	for i, v := range src.Data {
		u[i] = float64(v)
	}
	convolve := func(in []float64, scale [3]float64) []float64 {
		var kernel [2][]float64
		for a := 0; a < 2; a++ {
			r := int(math.Ceil(3 * scale[a] / src.SpacingMM[a]))
			kernel[a] = make([]float64, 2*r+1)
			total := 0.0
			for j := -r; j <= r; j++ {
				weight := 1.0
				if scale[a] > 0 {
					x := float64(j) * src.SpacingMM[a] / scale[a]
					weight = math.Exp(-x * x / 2)
				}
				kernel[a][j+r] = weight
				total += weight
			}
			for j := range kernel[a] {
				kernel[a][j] /= total
			}
		}
		out := make([]float64, len(in))
		for y := 0; y < src.Dims[1]; y++ {
			for x := 0; x < src.Dims[0]; x++ {
				for ky, wy := range kernel[1] {
					for kx, wx := range kernel[0] {
						xx := max(0, min(src.Dims[0]-1, x+kx-len(kernel[0])/2))
						yy := max(0, min(src.Dims[1]-1, y+ky-len(kernel[1])/2))
						out[y*src.Dims[0]+x] += wx * wy * in[yy*src.Dims[0]+xx]
					}
				}
			}
		}
		return out
	}
	for step := 0; step < o.Iterations; step++ {
		smoothed := convolve(u, o.SigmaMM)
		components := [3][]float64{make([]float64, len(u)), make([]float64, len(u)), make([]float64, len(u))}
		for y := 0; y < src.Dims[1]; y++ {
			for x := 0; x < src.Dims[0]; x++ {
				i := y*src.Dims[0] + x
				gx := (smoothed[y*src.Dims[0]+min(x+1, src.Dims[0]-1)] - smoothed[y*src.Dims[0]+max(x-1, 0)]) / (2 * src.SpacingMM[0])
				gy := (smoothed[min(y+1, src.Dims[1]-1)*src.Dims[0]+x] - smoothed[max(y-1, 0)*src.Dims[0]+x]) / (2 * src.SpacingMM[1])
				components[0][i] = gx * gx
				components[1][i] = gx * gy
				components[2][i] = gy * gy
			}
		}
		for k := range components {
			components[k] = convolve(components[k], o.RhoMM)
		}
		d := make([][3][3]float64, len(u))
		for i := range u {
			x, y, z := components[0][i], components[1][i], components[2][i]
			angle := .5 * math.Atan2(2*y, x-z)
			c, s := math.Cos(angle), math.Sin(angle)
			gap := math.Sqrt((x-z)*(x-z) + 4*y*y)
			e := 0.0
			if gap > 0 {
				e = math.Exp(-math.Pow(o.Threshold/gap, float64(o.Exponent)))
			}
			hi, lo := o.Alpha, o.Alpha+(1-o.Alpha)*e
			if o.Method == EdgeEnhancing {
				hi, lo = 1-(1-o.Alpha)*e, 1
			}
			d[i][0][0] = hi*c*c + lo*s*s
			d[i][1][1] = hi*s*s + lo*c*c
			d[i][0][1] = (hi - lo) * c * s
			d[i][1][0] = d[i][0][1]
		}
		delta := tensorOrthantOracle(src.Dims, src.SpacingMM, u, d)
		for i := range u {
			u[i] += o.TimeStep * delta[i]
		}
	}
	return u
}

func TestTensorDiffusionIndependentReference(t *testing.T) {
	for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
		v := tensorPhantom("oblique", [3]int{7, 6, 1}, [3]float64{.7, 1.3, 0})
		o := tensorTestOptions(method)
		o.SigmaMM = [3]float64{.5, .7, 0}
		o.RhoMM = [3]float64{.8, .6, 0}
		o.Iterations = 3
		o.TimeStep = .03
		o.Exponent = 2
		got, err := TensorAnisotropicDiffusion(context.Background(), v, o)
		if err != nil {
			t.Fatal(err)
		}
		want := tensorReference2D(v, o)
		for i, v := range got.Volume.Data {
			if math.Abs(float64(v)-want[i]) > 2e-7 {
				t.Fatalf("%s i%d got%.9g want%.9g", method, i, v, want[i])
			}
		}
	}
}

func TestTensorDiffusionStabilityAndPhantoms(t *testing.T) {
	for _, dims := range [][3]int{{23, 21, 1}, {15, 13, 9}} {
		for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
			for _, kind := range []string{"constant", "line", "edge", "junction", "oblique"} {
				v := tensorPhantom(kind, dims, [3]float64{.7, 1.3, 2.1})
				saved := v.Clone()
				o := tensorTestOptions(method)
				o.Iterations = 20
				inverseSum := 0.0
				for a, h := range v.SpacingMM {
					if dims[a] > 1 {
						inverseSum += 1 / (h * h)
					}
				}
				o.TimeStep = 1 / (2 * inverseSum)
				r, err := TensorAnisotropicDiffusion(context.Background(), v, o)
				if err != nil {
					t.Fatal(err)
				}
				m := r.Metadata
				if math.Abs(m.FinalMean-m.InitialMean) > 2e-12 || m.FinalL2Squared > m.InitialL2Squared*(1+3e-14) {
					t.Fatalf("%s %s dims%v mean%g->%g L2%g->%g", method, kind, dims, m.InitialMean, m.FinalMean, m.InitialL2Squared, m.FinalL2Squared)
				}
				if !reflect.DeepEqual(v, saved) {
					t.Fatal("input mutated")
				}
				if kind == "constant" && !reflect.DeepEqual(r.Volume.Data, v.Data) {
					t.Fatal("constant changed")
				}
				if kind != "constant" && m.ResidualRMS <= 0 {
					t.Fatal("no diffusion")
				}
				o.TimeStep = math.Nextafter(o.TimeStep, math.Inf(1))
				if bad, e := TensorAnisotropicDiffusion(context.Background(), v, o); !errors.Is(e, ErrTensorDiffusionInput) || bad != nil {
					t.Fatal("unstable next representable step admitted", e)
				}
			}
		}
	}
}

func tensorTransform(v *Volume, permutation [3]int, reflectX bool) *Volume {
	dims := [3]int{v.Dims[permutation[0]], v.Dims[permutation[1]], v.Dims[permutation[2]]}
	h := [3]float64{v.SpacingMM[permutation[0]], v.SpacingMM[permutation[1]], v.SpacingMM[permutation[2]]}
	out := NewVolume(dims, h)
	for z := 0; z < dims[2]; z++ {
		for y := 0; y < dims[1]; y++ {
			for x := 0; x < dims[0]; x++ {
				c := [3]int{x, y, z}
				if reflectX {
					c[0] = dims[0] - 1 - c[0]
				}
				var old [3]int
				for a := 0; a < 3; a++ {
					old[permutation[a]] = c[a]
				}
				out.Data[out.Index(x, y, z)] = v.At(old[0], old[1], old[2])
			}
		}
	}
	return out
}

func TestTensorDiffusionSymmetryScalingWorkers(t *testing.T) {
	for _, dims := range [][3]int{{17, 15, 1}, {13, 11, 7}} {
		v := tensorPhantom("oblique", dims, [3]float64{.7, 1.3, 2.1})
		for _, method := range []TensorDiffusionMethod{CoherenceEnhancing, EdgeEnhancing} {
			o := tensorTestOptions(method)
			o.Workers = 1
			base, err := TensorAnisotropicDiffusion(context.Background(), v, o)
			if err != nil {
				t.Fatal(err)
			}
			o.Workers = 4
			multi, err := TensorAnisotropicDiffusion(context.Background(), v, o)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(base.Volume.Data, multi.Volume.Data) {
				t.Fatal("worker dependent")
			}
			for _, permutation := range [][3]int{{0, 1, 2}, {1, 0, 2}, {2, 1, 0}} {
				for _, reflection := range []bool{false, true} {
					transformed := tensorTransform(v, permutation, reflection)
					o2 := o
					for a := 0; a < 3; a++ {
						o2.SigmaMM[a] = o.SigmaMM[permutation[a]]
						o2.RhoMM[a] = o.RhoMM[permutation[a]]
					}
					got, e := TensorAnisotropicDiffusion(context.Background(), transformed, o2)
					if e != nil {
						t.Fatal(e)
					}
					want := tensorTransform(base.Volume, permutation, reflection)
					for i, x := range got.Volume.Data {
						if math.Abs(float64(x-want.Data[i])) > 5e-6 {
							t.Fatalf("symmetry %s %v %t i%d", method, permutation, reflection, i)
						}
					}
				}
			}
			// Change physical length units by factor 2: gradient eigenvalues
			// scale by 1/4, scales by 2 and diffusion time by 4.
			physical := v.Clone()
			o2 := o
			for a := 0; a < 3; a++ {
				physical.SpacingMM[a] *= 2
				o2.SigmaMM[a] *= 2
				o2.RhoMM[a] *= 2
			}
			o2.Threshold /= 4
			o2.TimeStep = base.Metadata.TimeStep * 4
			got, e := TensorAnisotropicDiffusion(context.Background(), physical, o2)
			if e != nil {
				t.Fatal(e)
			}
			for i, x := range got.Volume.Data {
				if math.Abs(float64(x-base.Volume.Data[i])) > 1e-6 {
					t.Fatal("physical scaling")
				}
			}
			intensity := v.Clone()
			o2 = o
			o2.Threshold *= 100
			for i := range intensity.Data {
				intensity.Data[i] *= 10
			}
			got, e = TensorAnisotropicDiffusion(context.Background(), intensity, o2)
			if e != nil {
				t.Fatal(e)
			}
			for i, x := range got.Volume.Data {
				if math.Abs(float64(x/10-base.Volume.Data[i])) > 2e-6 {
					t.Fatal("intensity scaling")
				}
			}
		}
	}
}

func TestTensorDiffusionIsotropicHeatReference(t *testing.T) {
	v := tensorPhantom("oblique", [3]int{11, 9, 5}, [3]float64{.7, 1.3, 2.1})
	o := tensorTestOptions(CoherenceEnhancing)
	o.Alpha = 1
	o.Iterations = 7
	r, err := TensorAnisotropicDiffusion(context.Background(), v, o)
	if err != nil {
		t.Fatal(err)
	}
	u := make([]float64, len(v.Data))
	for i, x := range v.Data {
		u[i] = float64(x)
	}
	for step := 0; step < o.Iterations; step++ {
		next := make([]float64, len(u))
		for z := 0; z < v.Dims[2]; z++ {
			for y := 0; y < v.Dims[1]; y++ {
				for x := 0; x < v.Dims[0]; x++ {
					c := [3]int{x, y, z}
					i := v.Index(x, y, z)
					delta := 0.0
					for a := 0; a < 3; a++ {
						for _, sign := range []int{-1, 1} {
							n := c
							n[a] += sign
							if n[a] < 0 || n[a] >= v.Dims[a] {
								continue
							}
							delta += (u[v.Index(n[0], n[1], n[2])] - u[i]) / (v.SpacingMM[a] * v.SpacingMM[a])
						}
					}
					next[i] = u[i] + r.Metadata.TimeStep*delta
				}
			}
		}
		u = next
	}
	for i, x := range r.Volume.Data {
		if math.Abs(float64(x)-u[i]) > 2e-7 {
			t.Fatalf("heat i%d %g %g", i, x, u[i])
		}
	}
}

func TestTensorDiffusionAdmissionAndOwnership(t *testing.T) {
	v := tensorPhantom("line", [3]int{9, 7, 1}, [3]float64{.7, 1.3, 0})
	o := tensorTestOptions(EdgeEnhancing)
	o.SourceReference = "synthetic-line"
	r, e := TensorAnisotropicDiffusion(context.Background(), v, o)
	if e != nil {
		t.Fatal(e)
	}
	if !r.Metadata.Derived || len(r.Metadata.SourceSHA256) != 64 || r.Metadata.SourceReference != o.SourceReference || r.Metadata.IterationsCompleted != o.Iterations {
		t.Fatal("provenance")
	}
	if r.Metadata.ScratchBytes < 80*int64(len(v.Data)) || r.Metadata.WorkingBytes != r.Metadata.ScratchBytes+4*int64(len(v.Data))+int64(len(o.SourceReference)) {
		t.Fatal("payload")
	}
	if _, err := TensorAnisotropicDiffusion(nil, v, o); err != nil {
		t.Fatal("nil context", err)
	}
	for _, invalid := range []*Volume{nil, {Dims: [3]int{9, 7, 1}, SpacingMM: v.SpacingMM}, NewVolume([3]int{9, 1, 1}, [3]float64{1, 0, 0}), NewVolume([3]int{1, 1, 1}, [3]float64{})} {
		if result, err := TensorAnisotropicDiffusion(context.Background(), invalid, o); result != nil || !errors.Is(err, ErrTensorDiffusionInput) {
			t.Fatal("invalid domain", err)
		}
	}
	r.Volume.Data[0] += 10
	if r.Volume.Data[0] == v.Data[0] {
		t.Fatal("output alias")
	}
	for _, test := range []struct {
		name   string
		change func(*Volume, *TensorDiffusionOptions)
		want   error
	}{
		{"nan", func(v *Volume, o *TensorDiffusionOptions) { v.Data[0] = float32(math.NaN()) }, ErrTensorDiffusionInput},
		{"infinity", func(v *Volume, o *TensorDiffusionOptions) { v.Data[0] = float32(math.Inf(1)) }, ErrTensorDiffusionInput},
		{"magnitude", func(v *Volume, o *TensorDiffusionOptions) { v.Data[0] = math.MaxFloat32 }, ErrTensorDiffusionNumeric},
		{"spacing", func(v *Volume, o *TensorDiffusionOptions) { v.SpacingMM[0] = 1e-5 }, ErrTensorDiffusionInput},
		{"method", func(v *Volume, o *TensorDiffusionOptions) { o.Method = "" }, ErrTensorDiffusionInput},
		{"alpha", func(v *Volume, o *TensorDiffusionOptions) { o.Alpha = 0 }, ErrTensorDiffusionInput},
		{"threshold", func(v *Volume, o *TensorDiffusionOptions) { o.Threshold = 0 }, ErrTensorDiffusionInput},
		{"time", func(v *Volume, o *TensorDiffusionOptions) { o.TimeStep = math.NaN() }, ErrTensorDiffusionInput},
		{"exponent", func(v *Volume, o *TensorDiffusionOptions) { o.Exponent = 9 }, ErrTensorDiffusionInput},
		{"workers", func(v *Volume, o *TensorDiffusionOptions) { o.Workers = -1 }, ErrTensorDiffusionInput},
		{"scale", func(v *Volume, o *TensorDiffusionOptions) { o.RhoMM[0] = -1 }, ErrTensorDiffusionInput},
		{"radius", func(v *Volume, o *TensorDiffusionOptions) { o.RhoMM[0] = 100 }, ErrTensorDiffusionLimit},
		{"voxels", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxVoxels = 1 }, ErrTensorDiffusionLimit},
		{"iterations", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxIterations = 1 }, ErrTensorDiffusionLimit},
		{"scratch", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxScratchBytes = 1 }, ErrTensorDiffusionLimit},
		{"working", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxWorkingBytes = 1 }, ErrTensorDiffusionLimit},
		{"visits", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxStencilVisits = 1 }, ErrTensorDiffusionLimit},
		{"negative-limit", func(v *Volume, o *TensorDiffusionOptions) { o.Limits.MaxVoxels = -1 }, ErrTensorDiffusionInput},
	} {
		t.Run(test.name, func(t *testing.T) {
			vv, oo := v.Clone(), o
			test.change(vv, &oo)
			got, err := TensorAnisotropicDiffusion(context.Background(), vv, oo)
			if got != nil || !errors.Is(err, test.want) {
				t.Fatalf("got%v err%v", got, err)
			}
		})
	}
	// Exact required payload admits, a one-byte reduction rejects.
	o.Limits.MaxScratchBytes = r.Metadata.ScratchBytes
	o.Limits.MaxWorkingBytes = r.Metadata.WorkingBytes
	o.Limits.MaxStencilVisits = r.Metadata.StencilVisitBound
	if _, err := TensorAnisotropicDiffusion(context.Background(), v, o); err != nil {
		t.Fatal("exact budget", err)
	}
	o.Limits.MaxScratchBytes--
	if _, err := TensorAnisotropicDiffusion(context.Background(), v, o); !errors.Is(err, ErrTensorDiffusionLimit) {
		t.Fatal(err)
	}
}

type tensorCancelContext struct {
	context.Context
	checks atomic.Int64
	limit  int64
	cancel context.CancelFunc
}

func (c *tensorCancelContext) Err() error {
	if c.checks.Add(1) == c.limit {
		c.cancel()
	}
	return c.Context.Err()
}

func TestTensorDiffusionCancellationAndConcurrentCalls(t *testing.T) {
	v := tensorPhantom("oblique", [3]int{48, 41, 5}, [3]float64{.7, 1.3, 2.1})
	before := v.Clone()
	o := tensorTestOptions(CoherenceEnhancing)
	for _, limit := range []int64{1, 100, 1000, 40000} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &tensorCancelContext{Context: base, limit: limit, cancel: cancel}
		r, err := TensorAnisotropicDiffusion(ctx, v, o)
		cancel()
		if r != nil || !errors.Is(err, context.Canceled) {
			t.Fatalf("limit%d %v", limit, err)
		}
		if parallel.InUse() != 0 {
			t.Fatal("workers not joined")
		}
		if !reflect.DeepEqual(v, before) {
			t.Fatal("cancel input changed")
		}
	}
	small := tensorPhantom("oblique", [3]int{15, 13, 5}, v.SpacingMM)
	reference, e := TensorAnisotropicDiffusion(context.Background(), small, o)
	if e != nil {
		t.Fatal(e)
	}
	var wg sync.WaitGroup
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, err := TensorAnisotropicDiffusion(context.Background(), small, o)
			if err != nil {
				t.Error(err)
				return
			}
			if !reflect.DeepEqual(r.Volume.Data, reference.Volume.Data) {
				t.Error("concurrent result")
			}
		}()
	}
	wg.Wait()
	if parallel.InUse() != 0 {
		t.Fatal("budget leaked")
	}
}
