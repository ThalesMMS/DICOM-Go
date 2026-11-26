package dsa

import (
	"errors"
	"math"
	"math/rand"
	"testing"
)

// syntheticAngiogram is a smooth background of Gaussian blobs sampled at
// (x + dx, y + dy), so a frame drawn with (dx, dy) is the reference moved by
// (-dx, -dy) and ApplyShift must move it back by (dx, dy).
type syntheticAngiogram struct {
	blobs [][4]float64 // x, y, sigma, amplitude
}

func newSyntheticAngiogram(seed int64, columns, rows int) syntheticAngiogram {
	random := rand.New(rand.NewSource(seed))
	s := syntheticAngiogram{}
	for i := 0; i < 60; i++ {
		s.blobs = append(s.blobs, [4]float64{
			random.Float64() * float64(columns), random.Float64() * float64(rows),
			2 + random.Float64()*10, 100 + random.Float64()*900,
		})
	}
	return s
}

func (s syntheticAngiogram) frame(columns, rows int, dx, dy float64) []float64 {
	out := make([]float64, columns*rows)
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			fx, fy := float64(x)+dx, float64(y)+dy
			value := 500 + 0.8*fx - 0.5*fy
			for _, blob := range s.blobs {
				ex, ey := fx-blob[0], fy-blob[1]
				value += blob[3] * math.Exp(-(ex*ex+ey*ey)/(2*blob[2]*blob[2]))
			}
			out[y*columns+x] = value
		}
	}
	return out
}

func TestPhaseCorrelateRecoversSubpixelShifts(t *testing.T) {
	const columns, rows = 128, 96
	scene := newSyntheticAngiogram(7, columns, rows)
	reference := scene.frame(columns, rows, 0, 0)
	shifts := [][2]float64{{0, 0}, {0.25, -0.4}, {3.3, -1.7}, {-7.6, 5.1}, {11.5, 0.5}}
	tolerance := map[PeakInterpolation]float64{
		PeakParabolic: 0.15, PeakCosine: 0.15, PeakWeightedMeanPhase: 0.12, PeakNone: 0.5,
	}
	worst := map[PeakInterpolation]float64{}
	for _, shift := range shifts {
		moving := scene.frame(columns, rows, shift[0], shift[1])
		for _, method := range []PeakInterpolation{PeakParabolic, PeakCosine, PeakWeightedMeanPhase, PeakNone} {
			got, err := PhaseCorrelate(reference, moving, columns, rows, PhaseCorrelationOptions{Peak: method})
			if err != nil {
				t.Fatal(err)
			}
			errX, errY := math.Abs(got.DX-shift[0]), math.Abs(got.DY-shift[1])
			worst[method] = max(worst[method], errX, errY)
			if errX > tolerance[method] || errY > tolerance[method] {
				t.Errorf("%v: shift %v estimated (%.3f, %.3f)", method, shift, got.DX, got.DY)
			}
			if want := ShiftFromDisplacement(got.DX, got.DY); got.Shift != want {
				t.Errorf("%v: Shift %+v, want %+v", method, got.Shift, want)
			}
			if got.Peak < 0.3 {
				t.Errorf("%v: shift %v peak %.3f, want a clear peak", method, shift, got.Peak)
			}
		}
	}
	t.Logf("worst error in pixels: %v", worst)
}

// The estimated shift, applied to the mask, cancels the background: the
// residual after the subtraction is far below the unregistered one, while a
// vessel present only in the contrast frame survives.
func TestPhaseCorrelateRegistersMaskForSubtraction(t *testing.T) {
	const columns, rows = 128, 128
	scene := newSyntheticAngiogram(11, columns, rows)
	contrast := scene.frame(columns, rows, 0, 0)
	for y := 20; y < 110; y++ {
		for x := 60; x < 64; x++ {
			contrast[y*columns+x] -= 300
		}
	}
	random := rand.New(rand.NewSource(3))
	mask := scene.frame(columns, rows, -2.35, 1.6)
	for i := range mask {
		mask[i] += random.NormFloat64() * 5
	}
	got, err := PhaseCorrelate(contrast, mask, columns, rows, PhaseCorrelationOptions{Peak: PeakWeightedMeanPhase})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.DX+2.35) > 0.15 || math.Abs(got.DY-1.6) > 0.15 {
		t.Fatalf("estimated (%.3f, %.3f), want (-2.35, 1.60)", got.DX, got.DY)
	}
	registered, err := ApplyShift(mask, columns, rows, got.Shift)
	if err != nil {
		t.Fatal(err)
	}
	residual := func(m []float64) float64 {
		var sum float64
		for y := 10; y < rows-10; y++ {
			for x := 10; x < columns-10; x++ {
				if x >= 58 && x < 66 {
					continue
				}
				d := contrast[y*columns+x] - m[y*columns+x]
				sum += d * d
			}
		}
		return math.Sqrt(sum)
	}
	before, after := residual(mask), residual(registered)
	if after > before/5 {
		t.Fatalf("background residual %.1f after registration, %.1f before", after, before)
	}
	if vessel := contrast[64*columns+61] - registered[64*columns+61]; vessel > -250 {
		t.Fatalf("vessel difference %.1f, want about -300", vessel)
	}
}

func TestPhaseCorrelateMaxShiftAndErrors(t *testing.T) {
	const columns, rows = 64, 64
	scene := newSyntheticAngiogram(5, columns, rows)
	reference := scene.frame(columns, rows, 0, 0)
	moving := scene.frame(columns, rows, 9, 0)
	got, err := PhaseCorrelate(reference, moving, columns, rows, PhaseCorrelationOptions{MaxShift: 3})
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got.DX) > 3.5 {
		t.Fatalf("MaxShift 3 estimated dx %.2f", got.DX)
	}
	if _, err := PhaseCorrelate(reference, moving[1:], columns, rows, PhaseCorrelationOptions{}); !errors.Is(err, ErrFrameSizeMismatch) {
		t.Fatalf("size mismatch error %v", err)
	}
	if _, err := PhaseCorrelate(make([]float64, 16), make([]float64, 16), 4, 4, PhaseCorrelationOptions{}); !errors.Is(err, ErrFrameTooSmall) {
		t.Fatalf("small frame error %v", err)
	}
	if _, err := PhaseCorrelate(reference, moving, columns, rows, PhaseCorrelationOptions{Peak: PeakNone + 1}); err == nil {
		t.Fatal("unknown interpolation accepted")
	}
	nan := append([]float64(nil), reference...)
	nan[5] = math.NaN()
	if _, err := PhaseCorrelate(nan, moving, columns, rows, PhaseCorrelationOptions{}); err == nil {
		t.Fatal("non-finite frame accepted")
	}
}

func TestPeakFitsRecoverSampledPeaks(t *testing.T) {
	for _, d := range []float64{-0.45, -0.2, 0, 0.1, 0.33} {
		parabola := func(x float64) float64 { return 1 - (x-d)*(x-d) }
		if got := parabolicOffset(parabola(-1), parabola(0), parabola(1)); math.Abs(got-d) > 1e-12 {
			t.Errorf("parabolic %.2f: %.6f", d, got)
		}
		cosine := func(x float64) float64 { return math.Cos(0.7 * (x - d)) }
		if got := cosineOffset(cosine(-1), cosine(0), cosine(1)); math.Abs(got-d) > 1e-9 {
			t.Errorf("cosine %.2f: %.6f", d, got)
		}
	}
}

func BenchmarkPhaseCorrelate512(b *testing.B) {
	const size = 512
	scene := newSyntheticAngiogram(2, size, size)
	reference, moving := scene.frame(size, size, 0, 0), scene.frame(size, size, 1.3, -2.2)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := PhaseCorrelate(reference, moving, size, size, PhaseCorrelationOptions{Peak: PeakWeightedMeanPhase}); err != nil {
			b.Fatal(err)
		}
	}
}
