package render

import "fmt"

type ComponentPresentationMode uint32

const (
	ComponentPresentationScalar ComponentPresentationMode = iota
	ComponentPresentationRGB
	ComponentPresentationIndependent
)

// ComponentPresentation describes source channels without rewriting them.
// RGB uses direct normalized color; three-channel VR opacity is the maximum
// normalized RGB channel, four-channel VR opacity uses the fourth channel.
// Independent components each have a scalar-domain RGBA transfer and weight.
// Their weighted premultiplied colors are summed and alpha is clamped to one.
type ComponentPresentation struct {
	Mode         ComponentPresentationMode
	RGBMaximum   float64
	OpacityScale float64
	Transfers    []TransferLUT
	Weights      []float64
}

// Clone owns each transfer and weight before a presentation crosses a job boundary.
func (p ComponentPresentation) Clone() ComponentPresentation {
	p.Weights = append([]float64(nil), p.Weights...)
	p.Transfers = append([]TransferLUT(nil), p.Transfers...)
	for i := range p.Transfers {
		p.Transfers[i].Samples = append([]TransferSample(nil), p.Transfers[i].Samples...)
	}
	return p
}

func (p ComponentPresentation) Validate() error {
	fail := func(reason string) error {
		return fmt.Errorf("%w: component presentation: %s", ErrInvalidViewState, reason)
	}
	if p.Mode == ComponentPresentationScalar {
		if p.RGBMaximum != 0 || p.OpacityScale != 0 || len(p.Transfers) != 0 || len(p.Weights) != 0 {
			return fail("scalar mode carries component parameters")
		}
		return nil
	}
	if !finite(p.OpacityScale) || p.OpacityScale <= 0 || p.OpacityScale > 16 {
		return fail("opacity scale outside (0,16]")
	}
	switch p.Mode {
	case ComponentPresentationRGB:
		if !finite(p.RGBMaximum) || p.RGBMaximum <= 0 || len(p.Transfers) != 0 || len(p.Weights) != 0 {
			return fail("invalid RGB domain or independent transfers")
		}
	case ComponentPresentationIndependent:
		if p.RGBMaximum != 0 || len(p.Transfers) < 2 || len(p.Transfers) > 4 || len(p.Weights) != len(p.Transfers) {
			return fail("two to four transfers and matching weights required")
		}
		count := 0
		for i, lut := range p.Transfers {
			if !finite(lut.DomainMin) || !finite(lut.DomainMax) || lut.DomainMin >= lut.DomainMax || len(lut.Samples) < 2 || len(lut.Samples) > MaxTransferLUTSamples {
				return fail("invalid component transfer domain or size")
			}
			if !finite(p.Weights[i]) || p.Weights[i] < 0 || p.Weights[i] > 1 {
				return fail("weight outside [0,1]")
			}
			count += len(lut.Samples)
			for _, sample := range lut.Samples {
				for _, v := range []float32{sample.R, sample.G, sample.B, sample.A} {
					if !finite(float64(v)) || v < 0 || v > 1 {
						return fail("invalid transfer RGBA")
					}
				}
			}
		}
		if count > MaxTransferLUTSamples {
			return fail("total transfer sample limit exceeded")
		}
	default:
		return fail("unknown component mode")
	}
	return nil
}

// ValidateComponentViewForVolume prevents implicit channel selection or a
// scalar request against a multicomponent generation.
func ValidateComponentViewForVolume(descriptor VolumeDescriptor, state ViewState) error {
	if err := ValidateVolumeDescriptor(descriptor); err != nil {
		return err
	}
	if err := ValidateViewState(state); err != nil {
		return err
	}
	p := state.Components
	valid := descriptor.Components == 1 && p.Mode == ComponentPresentationScalar ||
		(descriptor.Components == 3 || descriptor.Components == 4) && p.Mode == ComponentPresentationRGB ||
		descriptor.Components > 1 && p.Mode == ComponentPresentationIndependent && len(p.Transfers) == int(descriptor.Components)
	if !valid {
		return fmt.Errorf("%w: component material does not match volume channels", ErrInvalidViewState)
	}
	return nil
}
