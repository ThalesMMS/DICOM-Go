package compare

import (
	"context"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/render/qualification"
	stdimage "image"
	"math"
)

var (
	ErrPresentedConfiguration = errors.New("dicom/compare: invalid presented comparison configuration")
	ErrPresentedLimit         = errors.New("dicom/compare: presented comparison budget exceeded")
)

// PresentedFrame is an already-produced presentation plane, not a renderer.
// Values are tightly packed unassociated components in [0,1], with no padding.
// Depth records the original quantization (8 or 16); values are not requantized.
// Encoding is "gray-pvalue", "srgb-code" or "linear-rgb". RGB has three
// components; gray has one. Caller storage must remain unchanged during use.
// GPU-produced frames may be supplied here without generating a CPU 3D frame.
type PresentedFrame struct {
	Width, Height, Components int
	Depth                     int
	Encoding                  string
	Values                    []float64
}

// SSIMOptions uses population moments, C1=(K1*Range)^2, C2=(K2*Range)^2.
// Window is odd, 1..31. Kernel is uniform or gaussian; gaussian uses Sigma.
// Border is crop (renormalize in-bounds weights), reflect (without repeating
// the boundary pixel), or valid (omit centres without a complete window).
// Range is explicitly 1 for normalized presented planes; it is never estimated
// separately from image extrema, including for constant images.
type SSIMOptions struct {
	Window int     `json:"window"`
	Kernel string  `json:"kernel"`
	Sigma  float64 `json:"sigma"`
	K1, K2 float64
	Range  float64 `json:"range"`
	Border string  `json:"border"`
}

type ComparisonRegion struct {
	Name   string             `json:"name"`
	Bounds stdimage.Rectangle `json:"bounds"`
}

// PresentedOptions has finite defaults only when DefaultPresentedOptions is
// explicitly selected. Mask is shared for both inputs, one bool per pixel;
// excluded neighbours have zero weight and retained weights are renormalized.
// Excluded map entries are zero with Valid=false, never numerical observations.
// Difference is signed candidate-reference or absolute; visual Scale is fixed
// in normalized component units, never auto-fitted per image or frame.
type PresentedOptions struct {
	SSIM            SSIMOptions        `json:"ssim"`
	Difference      string             `json:"difference"`
	Scale           float64            `json:"scale"`
	MaxPixels       int64              `json:"max_pixels"`
	MaxWorkingBytes int64              `json:"max_working_bytes"`
	Mask            []bool             `json:"-"`
	Regions         []ComparisonRegion `json:"regions"`
}

func DefaultPresentedOptions() PresentedOptions {
	return PresentedOptions{SSIM: SSIMOptions{Window: 7, Kernel: "gaussian", Sigma: 1.5, K1: .01, K2: .03, Range: 1, Border: "reflect"}, Difference: "absolute", Scale: 1, MaxPixels: 4 << 20, MaxWorkingBytes: 128 << 20}
}

// PresentedStatistics separates numerical sample errors from local similarity.
// SSIM aggregation is the arithmetic mean across valid centres and components,
// with the minimum published alongside it to expose localized differences.
type PresentedStatistics struct {
	Name        string             `json:"name"`
	Bounds      stdimage.Rectangle `json:"bounds"`
	Numerical   Metrics            `json:"-"`
	SSIMSamples int64              `json:"ssim_samples"`
	MeanSSIM    float64            `json:"mean_ssim"`
	MinimumSSIM float64            `json:"minimum_ssim"`
}

type PresentedResult struct {
	Schema                           string `json:"schema"`
	Width, Height, Components, Depth int
	Encoding                         string                `json:"encoding"`
	Options                          PresentedOptions      `json:"options"`
	Difference                       []float64             `json:"-"`
	SSIM                             []float64             `json:"-"`
	Valid                            []bool                `json:"-"`
	Summary                          PresentedStatistics   `json:"summary"`
	Regions                          []PresentedStatistics `json:"regions"`
	// LogicalWorkingBytes is the admitted comparison buffers, not RSS, codec
	// scratch, caller input planes or storage retained by a result consumer.
	ImagePolicy         *PresentedImagePolicy                  `json:"image_policy,omitempty"`
	RawRGBA             *qualification.RGBAFrameErrorMetricsV1 `json:"raw_rgba8_before_background,omitempty"`
	LogicalWorkingBytes int64                                  `json:"logical_working_bytes"`
}

func finite(v float64) bool { return !math.IsNaN(v) && !math.IsInf(v, 0) }
func presentedShape(f PresentedFrame) (int64, error) {
	if f.Width <= 0 || f.Height <= 0 || int64(f.Width) > math.MaxInt64/int64(f.Height) {
		return 0, fmt.Errorf("%w: extent", ErrMismatch)
	}
	n := int64(f.Width) * int64(f.Height)
	if (f.Components != 1 && f.Components != 3) || (f.Depth != 8 && f.Depth != 16) || n > int64(math.MaxInt)/int64(f.Components) {
		return 0, fmt.Errorf("%w: depth/components", ErrMismatch)
	}
	if (f.Components == 1 && f.Encoding != "gray-pvalue") || (f.Components == 3 && f.Encoding != "srgb-code" && f.Encoding != "linear-rgb") || int64(len(f.Values)) != n*int64(f.Components) {
		return 0, fmt.Errorf("%w: encoding/length", ErrMismatch)
	}
	return n, nil
}
func validatePresented(f PresentedFrame, o PresentedOptions) (int64, error) {
	if _, err := presentedShape(f); err != nil {
		return 0, err
	}
	return presentedBudget(f.Width, f.Height, f.Components, o, 0)
}
func presentedBudget(width, height, components int, o PresentedOptions, extraPerPixel int64) (int64, error) {
	if width <= 0 || height <= 0 || int64(width) > math.MaxInt64/int64(height) || (components != 1 && components != 3) {
		return 0, ErrMismatch
	}
	n := int64(width) * int64(height)
	s := o.SSIM
	if s.Window < 1 || s.Window > 31 || s.Window%2 == 0 || (s.Kernel != "uniform" && s.Kernel != "gaussian") || !finite(s.Sigma) || s.Sigma < 0 || (s.Kernel == "gaussian" && s.Sigma <= 0) || !finite(s.K1) || !finite(s.K2) || s.K1 < 1e-6 || s.K2 < 1e-6 || s.K1 > 1 || s.K2 > 1 || s.K1*s.K1 == 0 || s.K2*s.K2 == 0 || s.Range != 1 || (s.Border != "crop" && s.Border != "reflect" && s.Border != "valid") {
		return 0, fmt.Errorf("%w: SSIM", ErrPresentedConfiguration)
	}
	if (o.Difference != "signed" && o.Difference != "absolute") || !finite(o.Scale) || o.Scale <= 0 || o.MaxPixels <= 0 || o.MaxWorkingBytes <= 0 || len(o.Regions) > 256 || (len(o.Mask) != 0 && int64(len(o.Mask)) != n) {
		return 0, fmt.Errorf("%w: difference/mask/budget", ErrPresentedConfiguration)
	}
	// Two float64 maps + two validity masks (one snapshot, one result), kernel,
	// and detached bounded region records. Arithmetic is checked before allocation.
	if n > (math.MaxInt64-8192-256*1024)/(16*int64(components)+2+extraPerPixel) {
		return 0, ErrPresentedLimit
	}
	bytes := n*(16*int64(components)+2+extraPerPixel) + int64(s.Window*s.Window)*8 + int64(len(o.Regions))*1024
	if n > o.MaxPixels || bytes > o.MaxWorkingBytes {
		return 0, ErrPresentedLimit
	}
	bounds := stdimage.Rect(0, 0, width, height)
	seen := map[string]bool{}
	for _, r := range o.Regions {
		if r.Name == "" || len(r.Name) > 256 || seen[r.Name] || r.Bounds.Empty() || !r.Bounds.In(bounds) {
			return 0, fmt.Errorf("%w: region", ErrPresentedConfiguration)
		}
		seen[r.Name] = true
	}
	return bytes, nil
}

// ComparePresented compares a single pair and returns numerical and local maps.
// It does no auto-window, color transform, registration, resizing or rendering.
// Invalid input or cancellation returns no partial result. Inputs/options must
// not be mutated concurrently. Mask/region metadata is detached in the result.
func ComparePresented(ctx context.Context, reference, candidate PresentedFrame, o PresentedOptions) (*PresentedResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	bytes, err := validatePresented(reference, o)
	if err != nil {
		return nil, err
	}
	if _, err := presentedShape(candidate); err != nil {
		return nil, err
	}
	if reference.Width != candidate.Width || reference.Height != candidate.Height || reference.Components != candidate.Components || reference.Depth != candidate.Depth || reference.Encoding != candidate.Encoding {
		return nil, ErrMismatch
	}
	for i, v := range reference.Values {
		if i%4096 == 0 {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
		}
		if !finite(v) || v < 0 || v > 1 || !finite(candidate.Values[i]) || candidate.Values[i] < 0 || candidate.Values[i] > 1 {
			return nil, fmt.Errorf("%w: sample outside normalized range", ErrMismatch)
		}
	}
	o.Mask = append([]bool(nil), o.Mask...)
	o.Regions = append([]ComparisonRegion(nil), o.Regions...)
	n := reference.Width * reference.Height
	c := reference.Components
	r := &PresentedResult{Schema: "presented-comparison-v1", Width: reference.Width, Height: reference.Height, Components: c, Depth: reference.Depth, Encoding: reference.Encoding, Options: o, Difference: make([]float64, n*c), SSIM: make([]float64, n*c), Valid: make([]bool, n), LogicalWorkingBytes: bytes}
	weights := ssimWeights(o.SSIM)
	rad := o.SSIM.Window / 2
	for y := 0; y < r.Height; y++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		for x := 0; x < r.Width; x++ {
			if x%32 == 0 {
				if err := ctx.Err(); err != nil {
					return nil, err
				}
			}
			p := y*r.Width + x
			if len(o.Mask) > 0 && !o.Mask[p] {
				continue
			}
			for ch := 0; ch < c; ch++ {
				i := p*c + ch
				d := candidate.Values[i] - reference.Values[i]
				if o.Difference == "absolute" {
					d = math.Abs(d)
				}
				r.Difference[i] = d
			}
			if o.SSIM.Border == "valid" && (x < rad || y < rad || x+rad >= r.Width || y+rad >= r.Height) {
				continue
			}
			r.Valid[p] = true
			for ch := 0; ch < c; ch++ {
				r.SSIM[p*c+ch] = localSSIM(reference, candidate, o, weights, x, y, ch)
			}
		}
	}
	r.Summary, err = presentedStatistics(ctx, reference, candidate, r, "all", stdimage.Rect(0, 0, r.Width, r.Height))
	if err != nil {
		return nil, err
	}
	if r.Summary.Numerical.Samples == 0 || r.Summary.SSIMSamples == 0 {
		return nil, fmt.Errorf("%w: empty mask or valid interior", ErrPresentedConfiguration)
	}
	for _, region := range o.Regions {
		stats, err := presentedStatistics(ctx, reference, candidate, r, region.Name, region.Bounds)
		if err != nil {
			return nil, err
		}
		r.Regions = append(r.Regions, stats)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r, nil
}
func presentedStatistics(ctx context.Context, a, b PresentedFrame, r *PresentedResult, name string, rect stdimage.Rectangle) (PresentedStatistics, error) {
	stats := PresentedStatistics{Name: name, Bounds: rect, MinimumSSIM: 1}
	var acc Accumulator
	sum := float64(0)
	for y := rect.Min.Y; y < rect.Max.Y; y++ {
		if err := ctx.Err(); err != nil {
			return stats, err
		}
		for x := rect.Min.X; x < rect.Max.X; x++ {
			p := y*r.Width + x
			if len(r.Options.Mask) > 0 && !r.Options.Mask[p] {
				continue
			}
			for c := 0; c < r.Components; c++ {
				i := p*r.Components + c
				acc.Add(a.Values[i], b.Values[i])
				if r.Valid[p] {
					v := r.SSIM[i]
					sum += v
					stats.SSIMSamples++
					stats.MinimumSSIM = math.Min(stats.MinimumSSIM, v)
				}
			}
		}
	}
	stats.Numerical = acc.Metrics()
	stats.Numerical.Frames = 1
	if stats.SSIMSamples > 0 {
		stats.MeanSSIM = sum / float64(stats.SSIMSamples)
	} else {
		stats.MinimumSSIM = 0
	}
	return stats, nil
}
