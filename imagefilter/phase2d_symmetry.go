package imagefilter

import (
	"context"
	"math"
)

type Phase2DPolarity uint8

const (
	Phase2DBoth Phase2DPolarity = iota
	Phase2DBright
	Phase2DDark
)

// PhaseSymmetry2DOptions selects the same radial bank as DecomposeWavelet2D.
// NoiseThreshold is a declared per-band subtraction in input scalar units;
// no noise model is inferred. AmplitudeFloor is a positive validity threshold
// in those units (zero selects 1e-12). It is not added to the denominator.
type PhaseSymmetry2DOptions struct {
	Wavelet                        Wavelet2DOptions
	NoiseThreshold, AmplitudeFloor float64
	Polarity                       Phase2DPolarity
}

type PhaseSymmetry2DResult struct {
	Symmetry, Amplitude, Phase, ScaleMM *Volume
	// Invalid phase samples are explicitly marked; their maps contain zero.
	Valid                          []bool
	Metadata                       Wavelet2DMetadata
	NoiseThreshold, AmplitudeFloor float64
	Polarity                       Phase2DPolarity
}

// PhaseSymmetry2D measures local even symmetry using monogenic Riesz pairs:
// sum_j max(polarity(even_j)-hypot(rx_j,ry_j)-T,0) / sum_j amplitude_j.
// Both uses abs(even), Bright uses even and Dark uses -even. Phase is the
// dominant-amplitude band's atan2(hypot(rx,ry),even), in [0,pi]; it is not
// signed/unwrapped phase. ScaleMM ties retain the first (finest) band.
// The score is dimensionless in [0,1], not probability/anatomical confidence.
// This explicit research profile has no directional bins or clinical default.
// Errors/cancellation publish no partial maps. All output buffers are owned.
func PhaseSymmetry2D(ctx context.Context, src *Volume, opts PhaseSymmetry2DOptions) (*PhaseSymmetry2DResult, error) {
	ctx = phase2DContext(ctx)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	floor := opts.AmplitudeFloor
	if floor == 0 {
		floor = 1e-12
	}
	if !(floor > 0 && floor <= math.MaxFloat64) || !(opts.NoiseThreshold >= 0 && opts.NoiseThreshold <= math.MaxFloat64) || opts.Polarity > Phase2DDark {
		return nil, ErrPhase2DInput
	}
	p, err := admitWavelet2D(ctx, src, opts.Wavelet, true)
	if err != nil {
		return nil, err
	}
	spectrum, work, err := wavelet2DSpectrum(ctx, src, p)
	if err != nil {
		return nil, err
	}
	even, rx := make([]float64, p.n), make([]float64, p.n)
	energy, amplitude, peak := make([]float64, p.n), make([]float64, p.n), make([]float64, p.n)
	volume := func() *Volume { return &Volume{Dims: src.Dims, SpacingMM: src.SpacingMM, Data: make([]float32, p.n)} }
	out := &PhaseSymmetry2DResult{Symmetry: volume(), Amplitude: volume(), Phase: volume(), ScaleMM: volume(), Valid: make([]bool, p.n), Metadata: p.metadata(src, opts.Wavelet), NoiseThreshold: opts.NoiseThreshold, AmplitudeFloor: floor, Polarity: opts.Polarity}
	previous := 0.0
	for _, sigma := range opts.Wavelet.ScalesMM {
		for component := 0; component < 3; component++ {
			if err := wavelet2DFilter(ctx, spectrum, work, src, p, previous, sigma, component); err != nil {
				return nil, err
			}
			for y := 0; y < p.h; y++ {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
				for x := 0; x < p.w; x++ {
					i := y*p.w + x
					v := real(work[(y+p.oy)*p.pw+x+p.ox])
					if math.IsNaN(v) || math.IsInf(v, 0) {
						return nil, ErrPhase2DNumeric
					}
					switch component {
					case 0:
						even[i] = v
					case 1:
						rx[i] = v
					case 2:
						odd := math.Hypot(rx[i], v)
						a := math.Hypot(even[i], odd)
						amplitude[i] += a
						polarity := math.Abs(even[i])
						if opts.Polarity == Phase2DBright {
							polarity = even[i]
						} else if opts.Polarity == Phase2DDark {
							polarity = -even[i]
						}
						energy[i] += math.Max(polarity-odd-opts.NoiseThreshold, 0)
						if a > peak[i] {
							peak[i] = a
							out.Phase.Data[i] = float32(math.Atan2(odd, even[i]))
							out.ScaleMM.Data[i] = float32(sigma)
						}
					}
				}
			}
		}
		previous = sigma
	}
	for i, a := range amplitude {
		if i&1023 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !phase2DRepresentable(a) {
			return nil, ErrPhase2DNumeric
		}
		out.Amplitude.Data[i] = float32(a)
		if a <= floor {
			out.Phase.Data[i], out.ScaleMM.Data[i] = 0, 0
			continue
		}
		out.Valid[i] = true
		out.Symmetry.Data[i] = float32(min(1, energy[i]/a))
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
