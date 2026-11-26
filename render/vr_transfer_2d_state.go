package render

import (
	"fmt"
	"math"
)

// VRTransfer2DState (V22) owns the linear-light, non-premultiplied table of a
// GPU classification request. Samples travel as row-major RGBA float32 words
// beside the request header. GradientMax is in modality units per mm.
type VRTransfer2DState struct {
	Width, Height                           uint32
	IntensityMin, IntensityMax, GradientMax float64
	Samples                                 []TransferSample `json:"-"`
}

func (s VRTransfer2DState) Enabled() bool { return s.Width != 0 || s.Height != 0 }

// PayloadBytes validates metadata before any transport allocation. A zero
// header is the absent table; partially populated headers are malformed.
func (s VRTransfer2DState) PayloadBytes() (uint64, error) {
	if !s.Enabled() {
		if s.IntensityMin != 0 || s.IntensityMax != 0 || s.GradientMax != 0 {
			return 0, ErrInvalidVRTransfer2D
		}
		return 0, nil
	}
	if s.Width < 2 || s.Height < 2 || s.Width > MaxVRTransfer2DSide || s.Height > MaxVRTransfer2DSide || !finite(s.IntensityMin) || !finite(s.IntensityMax) || s.IntensityMax <= s.IntensityMin || !finite(s.GradientMax) || s.GradientMax <= 0 {
		return 0, ErrInvalidVRTransfer2D
	}
	return uint64(s.Width) * uint64(s.Height) * 16, nil
}

func (s VRTransfer2DState) Validate() error {
	bytes, err := s.PayloadBytes()
	if err != nil {
		return err
	}
	if uint64(len(s.Samples))*16 != bytes {
		return fmt.Errorf("%w: incomplete GPU table", ErrInvalidVRTransfer2D)
	}
	for _, sample := range s.Samples {
		for _, value := range [4]float32{sample.R, sample.G, sample.B, sample.A} {
			if math.IsNaN(float64(value)) || value < 0 || value > 1 {
				return fmt.Errorf("%w: invalid GPU table channel", ErrInvalidVRTransfer2D)
			}
		}
	}
	return nil
}

// VRTransfer2DStateFromTable copies table entries only; it never reads a volume.
func VRTransfer2DStateFromTable(table *VRTransfer2D) VRTransfer2DState {
	if table == nil {
		return VRTransfer2DState{}
	}
	w, h := table.Size()
	low, high := table.IntensityRange()
	s := VRTransfer2DState{Width: uint32(w), Height: uint32(h), IntensityMin: low, IntensityMax: high, GradientMax: table.GradientMax(), Samples: make([]TransferSample, w*h)}
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			v := table.At(x, y)
			s.Samples[y*w+x] = TransferSample{R: float32(v.R), G: float32(v.G), B: float32(v.B), A: float32(v.A)}
		}
	}
	return s
}
