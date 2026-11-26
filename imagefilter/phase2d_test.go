package imagefilter

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync/atomic"
	"testing"
)

func TestWavelet2DReconstructionAndOwnedGeometry(t *testing.T) {
	for _, boundary := range []Wavelet2DBoundary{WaveletPeriodic, WaveletReflect} {
		for _, signal := range []string{"constant", "impulse", "sinusoid"} {
			t.Run(signal+map[Wavelet2DBoundary]string{WaveletPeriodic: "/periodic", WaveletReflect: "/reflect"}[boundary], func(t *testing.T) {
				src := NewVolume([3]int{32, 16, 1}, [3]float64{.7, 1.3, 0})
				for y := 0; y < 16; y++ {
					for x := 0; x < 32; x++ {
						switch signal {
						case "constant":
							src.Data[y*32+x] = 7
						case "sinusoid":
							src.Data[y*32+x] = float32(2 + math.Sin(2*math.Pi*float64(x)/8) + .5*math.Cos(2*math.Pi*float64(y)/16))
						}
					}
				}
				if signal == "impulse" {
					src.Data[7*32+3] = 1
				}
				before := append([]float32(nil), src.Data...)
				opts := Wavelet2DOptions{ScalesMM: []float64{.7, 1.4, 2.8}, Boundary: boundary}
				bank, err := DecomposeWavelet2D(nil, src, opts)
				if err != nil {
					t.Fatal(err)
				}
				out, err := ReconstructWavelet2D(nil, bank, Phase2DLimits{})
				if err != nil {
					t.Fatal(err)
				}
				maximum := 1.0
				for _, v := range before {
					maximum = math.Max(maximum, math.Abs(float64(v)))
				}
				for i, v := range out.Data {
					if math.Abs(float64(v-before[i])) > 2e-6*maximum {
						t.Fatalf("reconstruction[%d] = %g, want %g", i, v, before[i])
					}
				}
				if !reflect.DeepEqual(src.Data, before) || out.Dims != src.Dims || out.SpacingMM != src.SpacingMM {
					t.Fatal("source/geometry changed")
				}
				if !bank.Metadata.Experimental || bank.Metadata.Workers != 1 || bank.Metadata.Family != wavelet2DFamily {
					t.Fatal(bank.Metadata)
				}
				for _, band := range append(bank.Bands, bank.Lowpass) {
					if band.Dims != src.Dims || band.SpacingMM != src.SpacingMM {
						t.Fatal("coefficient geometry changed")
					}
				}
				if signal == "constant" {
					for _, band := range bank.Bands {
						for _, v := range band.Data {
							if math.Abs(float64(v)) > 1e-6 {
								t.Fatal("nonzero DC wavelet response")
							}
						}
					}
				}
				opts.ScalesMM[0] = 99
				bank.Bands[0].Data[0] = 99
				out.Data[1] = 99
				if bank.Metadata.ScalesMM[0] != .7 || !reflect.DeepEqual(src.Data, before) {
					t.Fatal("output/option alias")
				}
			})
		}
	}
	// Non-radix-2 shapes are supported through declared reflected padding.
	src := NewVolume([3]int{13, 9, 1}, [3]float64{.5, 2, 0})
	src.Data[0] = 1
	bank, err := DecomposeWavelet2D(nil, src, Wavelet2DOptions{ScalesMM: []float64{1, 2}})
	if err != nil {
		t.Fatal(err)
	}
	out, err := ReconstructWavelet2D(nil, bank, Phase2DLimits{})
	if err != nil || math.Abs(float64(out.Data[0])-1) > 2e-6 {
		t.Fatal(out, err)
	}
}

func TestWavelet2DAnalyticPhysicalFrequencyAndPhase(t *testing.T) {
	// A single physical Fourier mode has known transfer amplitudes and Riesz
	// quadrature. Unequal spacing tests radial physical rather than pixel units.
	src := NewVolume([3]int{16, 8, 1}, [3]float64{.5, 2, 0})
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			src.Data[y*16+x] = float32(math.Cos(2 * math.Pi * (float64(x)/16 + float64(y)/8)))
		}
	}
	opts := Wavelet2DOptions{ScalesMM: []float64{.2, .4, .8}, Boundary: WaveletPeriodic}
	bank, err := DecomposeWavelet2D(nil, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	phase, err := PhaseSymmetry2D(nil, src, PhaseSymmetry2DOptions{Wavelet: opts, AmplitudeFloor: 1e-8})
	if err != nil {
		t.Fatal(err)
	}
	r2 := math.Pow(2*math.Pi/(16*.5), 2) + math.Pow(2*math.Pi/(8*2), 2)
	previous := 0.0
	total := 0.0
	for j, sigma := range opts.ScalesMM {
		weight := math.Exp(-.5*previous*previous*r2) - math.Exp(-.5*sigma*sigma*r2)
		total += weight
		for i, v := range bank.Bands[j].Data {
			if math.Abs(float64(v)-weight*float64(src.Data[i])) > 2e-6 {
				t.Fatalf("band %d physical transfer error at %d", j, i)
			}
		}
		previous = sigma
	}
	for y := 0; y < 8; y++ {
		for x := 0; x < 16; x++ {
			i := y*16 + x
			angle := 2 * math.Pi * (float64(x)/16 + float64(y)/8)
			e, o := math.Cos(angle), math.Abs(math.Sin(angle))
			want := math.Max(math.Abs(e)-o, 0)
			if !phase.Valid[i] || math.Abs(float64(phase.Symmetry.Data[i])-want) > 2e-6 || math.Abs(float64(phase.Amplitude.Data[i])-total) > 2e-6 || math.Abs(float64(phase.Phase.Data[i])-math.Atan2(o, e)) > 2e-6 {
				t.Fatalf("analytic phase %d: %g %g %g", i, phase.Symmetry.Data[i], phase.Amplitude.Data[i], phase.Phase.Data[i])
			}
		}
	}
}

func TestPhaseSymmetry2DContrastPolarityAndInvalidAmplitude(t *testing.T) {
	opts := PhaseSymmetry2DOptions{Wavelet: Wavelet2DOptions{ScalesMM: []float64{.5, 1, 2}, Boundary: WaveletPeriodic}, AmplitudeFloor: 1e-6}
	src := NewVolume([3]int{32, 16, 1}, [3]float64{1, 1, 0})
	for i := range src.Data {
		src.Data[i] = float32(math.Cos(2 * math.Pi * float64(i%32) / 16))
	}
	base, err := PhaseSymmetry2D(nil, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, gain := range []float64{.25, 4, -2} {
		changed := src.Clone()
		for i, v := range changed.Data {
			// Binary power-of-two gains preserve source rounding exactly.
			// Adding a large offset would also add float32 quantization noise,
			// which is not an exact affine transform of these stored samples.
			changed.Data[i] = float32(gain * float64(v))
		}
		out, err := PhaseSymmetry2D(nil, changed, opts)
		if err != nil {
			t.Fatal(err)
		}
		for i, v := range out.Symmetry.Data {
			if math.Abs(float64(v-base.Symmetry.Data[i])) > 5e-6 {
				t.Fatalf("contrast symmetry gain %g sample %d", gain, i)
			}
		}
	}
	for _, polarity := range []Phase2DPolarity{Phase2DBright, Phase2DDark} {
		opts.Polarity = polarity
		out, err := PhaseSymmetry2D(nil, src, opts)
		if err != nil {
			t.Fatal(err)
		}
		bright, dark := out.Symmetry.Data[0], out.Symmetry.Data[8]
		if polarity == Phase2DBright && (bright < .99 || dark != 0) || polarity == Phase2DDark && (dark < .99 || bright != 0) {
			t.Fatal(polarity, bright, dark)
		}
	}
	constant := src.Clone()
	for i := range constant.Data {
		constant.Data[i] = 7
	}
	out, err := PhaseSymmetry2D(nil, constant, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, valid := range out.Valid {
		if valid || out.Symmetry.Data[i] != 0 || out.Phase.Data[i] != 0 || out.ScaleMM.Data[i] != 0 {
			t.Fatal("undefined constant phase presented as valid")
		}
	}
	tiny := src.Clone()
	for i := range tiny.Data {
		tiny.Data[i] *= 1e-8
	}
	out, err = PhaseSymmetry2D(nil, tiny, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, valid := range out.Valid {
		if valid {
			t.Fatal("near-null phase marked valid")
		}
	}
	// Sampled Nyquist has no unique odd quadrature: its multiplier is zero.
	for i := range src.Data {
		src.Data[i] = 1
		if i%2 != 0 {
			src.Data[i] = -1
		}
	}
	opts.Polarity = Phase2DBoth
	out, err = PhaseSymmetry2D(nil, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for i, v := range out.Phase.Data {
		want := 0.0
		if i%2 != 0 {
			want = math.Pi
		}
		if math.Abs(float64(v)-want) > 2e-6 || out.Symmetry.Data[i] < .99 {
			t.Fatal("Nyquist convention", i, v)
		}
	}
}

func TestWavelet2DRejectsInvalidAndBoundsBeforeAllocation(t *testing.T) {
	src := NewVolume([3]int{16, 8, 1}, [3]float64{1, 2, 0})
	opts := Wavelet2DOptions{ScalesMM: []float64{1, 2}, Boundary: WaveletPeriodic}
	for name, change := range map[string]func(*Volume, *Wavelet2DOptions){
		"3d":             func(v *Volume, o *Wavelet2DOptions) { v.Dims[2] = 2 },
		"nan":            func(v *Volume, o *Wavelet2DOptions) { v.Data[1] = float32(math.NaN()) },
		"spacing":        func(v *Volume, o *Wavelet2DOptions) { v.SpacingMM[0] = 0 },
		"sigma-nan":      func(v *Volume, o *Wavelet2DOptions) { o.ScalesMM[0] = math.NaN() },
		"sigma-order":    func(v *Volume, o *Wavelet2DOptions) { o.ScalesMM[1] = .5 },
		"periodic-shape": func(v *Volume, o *Wavelet2DOptions) { v.Dims[0] = 15; v.Data = v.Data[:120] },
		"boundary":       func(v *Volume, o *Wavelet2DOptions) { o.Boundary = 99 },
		"limits":         func(v *Volume, o *Wavelet2DOptions) { o.Limits.MaxWorkingBytes = -1 },
	} {
		t.Run(name, func(t *testing.T) {
			v := src.Clone()
			o := opts
			o.ScalesMM = append([]float64(nil), opts.ScalesMM...)
			change(v, &o)
			if out, err := DecomposeWavelet2D(nil, v, o); out != nil || !errors.Is(err, ErrPhase2DInput) {
				t.Fatal(out, err)
			}
		})
	}
	for _, limits := range []Phase2DLimits{{MaxPixels: 1}, {MaxPaddedPixels: 1}, {MaxScales: 1}, {MaxWorkingBytes: 1}} {
		o := opts
		o.Limits = limits
		if out, err := DecomposeWavelet2D(nil, src, o); out != nil || !errors.Is(err, ErrPhase2DLimit) {
			t.Fatal(out, err)
		}
	}
	for _, phase := range []bool{false, true} {
		p, err := admitWavelet2D(context.Background(), src, opts, phase)
		if err != nil {
			t.Fatal(err)
		}
		o := opts
		o.Limits.MaxWorkingBytes = p.working
		if phase {
			if _, err := PhaseSymmetry2D(nil, src, PhaseSymmetry2DOptions{Wavelet: o}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := DecomposeWavelet2D(nil, src, o); err != nil {
				t.Fatal(err)
			}
		}
		o.Limits.MaxWorkingBytes--
		if phase {
			if out, err := PhaseSymmetry2D(nil, src, PhaseSymmetry2DOptions{Wavelet: o}); out != nil || !errors.Is(err, ErrPhase2DLimit) {
				t.Fatal(err)
			}
		} else {
			if out, err := DecomposeWavelet2D(nil, src, o); out != nil || !errors.Is(err, ErrPhase2DLimit) {
				t.Fatal(err)
			}
		}
	}
	o := opts
	o.Boundary = WaveletReflect
	o.ScalesMM = []float64{1e9}
	if out, err := DecomposeWavelet2D(nil, src, o); out != nil || !errors.Is(err, ErrPhase2DLimit) {
		t.Fatal(err)
	}
	for _, bad := range []PhaseSymmetry2DOptions{{Wavelet: opts, NoiseThreshold: math.NaN()}, {Wavelet: opts, AmplitudeFloor: -1}, {Wavelet: opts, Polarity: 99}} {
		if out, err := PhaseSymmetry2D(nil, src, bad); out != nil || !errors.Is(err, ErrPhase2DInput) {
			t.Fatal(out, err)
		}
	}
	bank, err := DecomposeWavelet2D(nil, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	if out, err := ReconstructWavelet2D(nil, bank, Phase2DLimits{MaxScales: 1}); out != nil || !errors.Is(err, ErrPhase2DLimit) {
		t.Fatal(out, err)
	}
	bank.Bands[0].SpacingMM[0] = 2
	if out, err := ReconstructWavelet2D(nil, bank, Phase2DLimits{}); out != nil || !errors.Is(err, ErrPhase2DInput) {
		t.Fatal(out, err)
	}
	bank.Bands[0].SpacingMM[0] = 1
	for i := range bank.Lowpass.Data {
		bank.Lowpass.Data[i] = math.MaxFloat32
		bank.Bands[0].Data[i] = math.MaxFloat32
	}
	if out, err := ReconstructWavelet2D(nil, bank, Phase2DLimits{}); out != nil || !errors.Is(err, ErrPhase2DNumeric) {
		t.Fatal(out, err)
	}
}

type phase2DCancelContext struct {
	context.Context
	checks atomic.Int64
	after  int64
}

func (c *phase2DCancelContext) Err() error {
	if c.checks.Add(1) >= c.after {
		return context.Canceled
	}
	return nil
}

func TestWavelet2DCancellationDiscardsPartialMaps(t *testing.T) {
	src := NewVolume([3]int{32, 32, 1}, [3]float64{1, 1, 0})
	opts := Wavelet2DOptions{ScalesMM: []float64{1, 2, 4}, Boundary: WaveletPeriodic}
	bank, err := DecomposeWavelet2D(nil, src, opts)
	if err != nil {
		t.Fatal(err)
	}
	for _, after := range []int64{1, 50, 500, 1500} {
		ctx := &phase2DCancelContext{Context: context.Background(), after: after}
		out, err := DecomposeWavelet2D(ctx, src, opts)
		if err == nil {
			if ctx.checks.Load() >= after {
				t.Fatal("missed cancellation")
			}
		} else if out != nil || !errors.Is(err, context.Canceled) {
			t.Fatal(out, err)
		}
		ctx = &phase2DCancelContext{Context: context.Background(), after: after}
		phase, err := PhaseSymmetry2D(ctx, src, PhaseSymmetry2DOptions{Wavelet: opts})
		if err == nil {
			if ctx.checks.Load() >= after {
				t.Fatal("missed cancellation")
			}
		} else if phase != nil || !errors.Is(err, context.Canceled) {
			t.Fatal(phase, err)
		}
	}
	// Force the final public checkpoint too: no completed-looking result may
	// accompany cancellation after all coefficient/output arrays were filled.
	for _, phase := range []bool{false, true} {
		count := &phase2DCancelContext{Context: context.Background(), after: math.MaxInt64}
		if phase {
			if _, err := PhaseSymmetry2D(count, src, PhaseSymmetry2DOptions{Wavelet: opts}); err != nil {
				t.Fatal(err)
			}
		} else {
			if _, err := DecomposeWavelet2D(count, src, opts); err != nil {
				t.Fatal(err)
			}
		}
		last := &phase2DCancelContext{Context: context.Background(), after: count.checks.Load()}
		if phase {
			if out, err := PhaseSymmetry2D(last, src, PhaseSymmetry2DOptions{Wavelet: opts}); out != nil || !errors.Is(err, context.Canceled) {
				t.Fatal(out, err)
			}
		} else {
			if out, err := DecomposeWavelet2D(last, src, opts); out != nil || !errors.Is(err, context.Canceled) {
				t.Fatal(out, err)
			}
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := ReconstructWavelet2D(ctx, bank, Phase2DLimits{}); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
	if out, err := DecomposeWavelet2D(ctx, &Volume{Dims: [3]int{1 << 28, 1 << 28, 1}}, opts); out != nil || !errors.Is(err, context.Canceled) {
		t.Fatal(out, err)
	}
}
