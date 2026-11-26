package microscopy

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/registration"
	"github.com/ThalesMMS/dicom-go/render"
)

var (
	ErrMosaic             = errors.New("microscopy: mosaic admission")
	ErrMosaicLimit        = errors.New("microscopy: mosaic quota")
	ErrMosaicInconclusive = errors.New("microscopy: inconclusive mosaic")
)

// MosaicTile borrows one scalar acquisition tile and its optional independent
// mask. Grid identifies approximate rectangular acquisition neighbours, not a
// pixel location. Image.Origin is the declared sample-zero LPS position in mm.
// Inputs must remain stable during registration/composition. No WSI is repaired.
type MosaicTile struct {
	ID          string
	Grid        image.Point
	Image, Mask *registration.Image
}

// MosaicOptions requires an explicit positive correction/search bound in mm.
// Other zeros select finite defaults. Only immediate horizontal/vertical grid
// neighbours are matched. Correlation peaks have no subpixel refinement;
// continuous global fitting may yield fractional origins within tolerance.
type MosaicOptions struct {
	AnchorID                                         string
	MaxCorrectionMM                                  float64
	MinOverlap                                       int
	MinOverlapFraction, MinScore, AmbiguityTolerance float64
	MaxCycleErrorMM, MaxGainRatio, MaxOffsetFraction float64
	MaxTiles                                         int
	MaxSamples, MaxBytes, MaxFFTWork, MaxWork        uint64
}

type MosaicPlacement struct {
	ID                   string
	Grid                 image.Point
	Size                 [3]int
	SourceOrigin, Origin render.Vec3
	SourceDigest         [32]byte
	Component            int
}

// MosaicLink.Offset is the proposed B-origin minus A-origin, in LPS mm.
// Registration.TranslationLPS instead maps declared fixed points to moving
// points; its negative is the relative origin correction. Rejected proposals
// remain visible and never participate in the solve. Gain/OffsetIntensity fit
// moving intensities = Gain*fixed + OffsetIntensity over actual valid overlap.
type MosaicLink struct {
	A, B                                   int
	Registration                           registration.MaskedFFTResult
	Offset                                 render.Vec3
	Accepted                               bool
	Reason                                 string
	Gain, OffsetIntensity, OffsetFraction  float64
	ResidualMM, CycleClosureMM             float64
	ResidualMeasured, CycleClosureMeasured bool
}

// MosaicLayout owns metadata, not source pixels. Conclusive requires a connected
// accepted graph and bounded fundamental-cycle closure and least-squares
// residuals. A connected tree has no measured cycles; CycleRank makes this
// absence explicit. This is not a confidence or anatomical correspondence claim.
type MosaicLayout struct {
	ID, AnchorID                     string
	Tiles                            []MosaicPlacement
	Links                            []MosaicLink
	Conclusive                       bool
	Components, CycleRank            int
	MaxResidualMM, MaxCycleClosureMM float64
	MaxDisplacementMM                float64
	Diagnostics                      []string
	Spacing                          render.Vec3
	Direction                        registration.Matrix3
	AnchorOrigin                     render.Vec3
	ScratchBytes, FFTWork, Work      uint64
	seal                             [32]byte
}

type mosaicRun struct {
	ctx  context.Context
	o    MosaicOptions
	work uint64
}

func (r *mosaicRun) step(n uint64) error {
	if err := r.ctx.Err(); err != nil {
		return err
	}
	if n > r.o.MaxWork-r.work {
		return fmt.Errorf("%w: logical work", ErrMosaicLimit)
	}
	r.work += n
	return nil
}
func mosaicFinite(x float64) bool        { return !math.IsNaN(x) && !math.IsInf(x, 0) }
func mosaicLength(v render.Vec3) float64 { return math.Sqrt(v.X*v.X + v.Y*v.Y + v.Z*v.Z) }
func mosaicOptions(o MosaicOptions) (MosaicOptions, error) {
	if !finitePositive(o.MaxCorrectionMM) {
		return o, fmt.Errorf("%w: explicit positive search bound required", ErrMosaic)
	}
	if o.MinOverlap == 0 {
		o.MinOverlap = 16
	}
	if o.MinOverlapFraction == 0 {
		o.MinOverlapFraction = .1
	}
	if o.MinScore == 0 {
		o.MinScore = .9
	}
	if o.AmbiguityTolerance == 0 {
		o.AmbiguityTolerance = 1e-8
	}
	if o.MaxCycleErrorMM == 0 {
		o.MaxCycleErrorMM = 1e-6
	}
	if o.MaxGainRatio == 0 {
		o.MaxGainRatio = 1.25
	}
	if o.MaxOffsetFraction == 0 {
		o.MaxOffsetFraction = .1
	}
	if o.MaxTiles == 0 {
		o.MaxTiles = 128
	}
	if o.MaxSamples == 0 {
		o.MaxSamples = 8 << 20
	}
	if o.MaxBytes == 0 {
		o.MaxBytes = 256 << 20
	}
	if o.MaxFFTWork == 0 {
		o.MaxFFTWork = 1 << 32
	}
	if o.MaxWork == 0 {
		o.MaxWork = 64 << 20
	}
	if o.MinOverlap < 3 || !finitePositive(o.MinOverlapFraction) || o.MinOverlapFraction > 1 || !finitePositive(o.MinScore) || o.MinScore > 1 || !finitePositive(o.AmbiguityTolerance) || o.AmbiguityTolerance > 2 || !finitePositive(o.MaxCycleErrorMM) || !mosaicFinite(o.MaxGainRatio) || o.MaxGainRatio < 1 || !finitePositive(o.MaxOffsetFraction) || o.MaxTiles < 2 || o.MaxTiles > 1024 || o.MaxSamples > math.MaxInt32 || len(o.AnchorID) > 256 {
		return o, fmt.Errorf("%w: options", ErrMosaic)
	}
	return o, nil
}

// RegisterMosaic estimates bounded integer relative translations with masked
// FFT NCC, solves all accepted links with an anchored equal-weight least-
// squares graph, and reports rejected links and global inconsistency. Errors
// publish nil. Inconclusive geometry publishes diagnostics but is ineligible
// for composition. No source pixels, origins or declared WSI tiles are changed.
func RegisterMosaic(ctx context.Context, tiles []MosaicTile, options MosaicOptions) (*MosaicLayout, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	o, err := mosaicOptions(options)
	if err != nil {
		return nil, err
	}
	r := &mosaicRun{ctx: ctx, o: o}
	if len(tiles) < 2 || len(tiles) > o.MaxTiles {
		return nil, fmt.Errorf("%w: tile count", ErrMosaicLimit)
	}
	// Conservative live metadata/dense graph envelope, independent of FFT
	// scratch. Caller-owned input data are excluded, generated masks included.
	n := uint64(len(tiles))
	base := n*n*32 + n*4096
	if base >= o.MaxBytes {
		return nil, fmt.Errorf("%w: graph scratch", ErrMosaicLimit)
	}
	result := &MosaicLayout{Tiles: make([]MosaicPlacement, len(tiles))}
	masks := make([]*registration.Image, len(tiles))
	var samples uint64
	for i, t := range tiles {
		if err := r.step(1); err != nil {
			return nil, err
		}
		if t.Image == nil || len(t.ID) == 0 || len(t.ID) > 256 || t.Grid.X < 0 || t.Grid.Y < 0 || t.Grid.X > 1000000 || t.Grid.Y > 1000000 {
			return nil, fmt.Errorf("%w: tile identity/grid/image", ErrMosaic)
		}
		for _, prev := range tiles[:i] {
			if prev.ID == t.ID || prev.Grid == t.Grid {
				return nil, fmt.Errorf("%w: duplicate tile identity/grid", ErrMosaic)
			}
		}
		size := t.Image.Size()
		if size[2] != 1 || size[0] < 2 || size[1] < 2 {
			return nil, fmt.Errorf("%w: scalar 2D dimensions", ErrMosaic)
		}
		if uint64(size[0]) > o.MaxSamples/uint64(size[1]) {
			return nil, fmt.Errorf("%w: tile samples", ErrMosaicLimit)
		}
		count := uint64(size[0]) * uint64(size[1])
		if count > o.MaxSamples-samples || count != uint64(len(t.Image.Data())) {
			return nil, fmt.Errorf("%w: total samples", ErrMosaicLimit)
		}
		samples += count
		if i == 0 {
			result.Spacing = t.Image.Spacing()
			result.Direction = t.Image.Direction()
		}
		if t.Image.Spacing() != result.Spacing || t.Image.Direction() != result.Direction || size != tiles[0].Image.Size() {
			return nil, fmt.Errorf("%w: incompatible dimensions/spacing/direction", ErrMosaic)
		}
		// Common rigid, orthonormal acquisition plane. No shear/scale fitting.
		d := result.Direction
		for a := 0; a < 3; a++ {
			for b := 0; b < 3; b++ {
				dot := 0.
				for k := 0; k < 3; k++ {
					dot += d[k][a] * d[k][b]
				}
				want := 0.
				if a == b {
					want = 1
				}
				if !mosaicFinite(dot) || math.Abs(dot-want) > 1e-8 {
					return nil, fmt.Errorf("%w: nonorthonormal plane", ErrMosaic)
				}
			}
		}
		normal := render.Vec3{X: d[0][2], Y: d[1][2], Z: d[2][2]}
		delta := t.Image.Origin().Sub(tiles[0].Image.Origin())
		if math.Abs(delta.X*normal.X+delta.Y*normal.Y+delta.Z*normal.Z) > 1e-8 {
			return nil, fmt.Errorf("%w: different focal plane", ErrMosaic)
		}
		masks[i] = t.Mask
		if t.Mask == nil {
			if count > (o.MaxBytes-base)/4 {
				return nil, fmt.Errorf("%w: mask scratch", ErrMosaicLimit)
			}
			base += count * 4
			data := make([]float32, int(count))
			for j := range data {
				if err := r.step(1); err != nil {
					return nil, err
				}
				data[j] = 1
			}
			masks[i], err = registration.NewImage(size, data, t.Image.Origin(), result.Spacing, result.Direction)
			if err != nil {
				return nil, err
			}
		} else if t.Mask.Size() != size || t.Mask.Origin() != t.Image.Origin() || t.Mask.Spacing() != result.Spacing || t.Mask.Direction() != result.Direction || len(t.Mask.Data()) != int(count) {
			return nil, fmt.Errorf("%w: independent mask grid", ErrMosaic)
		}
		digest, err := r.sourceDigest(t)
		if err != nil {
			return nil, err
		}
		result.Tiles[i] = MosaicPlacement{ID: t.ID, Grid: t.Grid, Size: size, SourceOrigin: t.Image.Origin(), Origin: t.Image.Origin(), SourceDigest: digest}
	}
	anchor := 0
	for i, t := range tiles {
		if o.AnchorID == "" {
			if t.Grid.Y < tiles[anchor].Grid.Y || t.Grid.Y == tiles[anchor].Grid.Y && t.Grid.X < tiles[anchor].Grid.X {
				anchor = i
			}
		} else if t.ID == o.AnchorID {
			anchor = i
		}
	}
	if o.AnchorID != "" && tiles[anchor].ID != o.AnchorID {
		return nil, fmt.Errorf("%w: missing anchor", ErrMosaic)
	}
	result.AnchorID = tiles[anchor].ID
	result.AnchorOrigin = tiles[anchor].Image.Origin()
	result.ScratchBytes = base
	for a := range tiles {
		for b := a + 1; b < len(tiles); b++ {
			if err := r.step(1); err != nil {
				return nil, err
			}
			dx, dy := tiles[b].Grid.X-tiles[a].Grid.X, tiles[b].Grid.Y-tiles[a].Grid.Y
			if absMosaic(dx)+absMosaic(dy) != 1 {
				continue
			}
			if result.FFTWork >= o.MaxFFTWork || base >= o.MaxBytes {
				return nil, fmt.Errorf("%w: FFT work/scratch", ErrMosaicLimit)
			}
			bound := o.MaxCorrectionMM
			fftOptions := registration.MaskedFFTOptions{MinOverlap: o.MinOverlap, MinOverlapFraction: o.MinOverlapFraction, AmbiguityTolerance: o.AmbiguityTolerance, Search: &registration.MaskedFFTSearch{Min: render.Vec3{X: -bound, Y: -bound, Z: -bound}, Max: render.Vec3{X: bound, Y: bound, Z: bound}}, MaxInputVoxels: o.MaxSamples * 2, MaxBytes: o.MaxBytes - base, MaxFFTWork: o.MaxFFTWork - result.FFTWork}
			bytes, work, e := registration.MaskedFFTWorkspace(tiles[a].Image, tiles[b].Image, masks[a], masks[b], fftOptions)
			if e != nil {
				return nil, fmt.Errorf("%w: FFT admission: %w", ErrMosaicLimit, e)
			}
			result.FFTWork += work
			result.ScratchBytes = max(result.ScratchBytes, base+bytes)
			proposal, e := registration.EstimateMaskedFFTTranslation(ctx, tiles[a].Image, tiles[b].Image, masks[a], masks[b], fftOptions)
			link := MosaicLink{A: a, B: b, Registration: proposal}
			if e != nil {
				if errors.Is(e, registration.ErrMaskedFFTNoOverlap) || errors.Is(e, registration.ErrMaskedFFTVariance) {
					link.Reason = e.Error()
				} else {
					return nil, e
				}
			} else {
				s := proposal.IndexShift
				link.Offset = result.Direction.MulVec(render.Vec3{X: -float64(s[0]) * result.Spacing.X, Y: -float64(s[1]) * result.Spacing.Y})
				switch {
				case proposal.Ambiguous:
					link.Reason = "ambiguous correlation peaks"
				case !proposal.Usable:
					link.Reason = "unusable linear boundary"
				case proposal.SearchBoundary:
					link.Reason = "search bound reached"
				case proposal.Score < o.MinScore:
					link.Reason = "correlation below threshold"
				default:
					link.Gain, link.OffsetIntensity, link.OffsetFraction, err = r.illumination(tiles[a].Image, tiles[b].Image, masks[a], masks[b], s)
					if err != nil {
						return nil, err
					}
					if link.Gain < 1/o.MaxGainRatio || link.Gain > o.MaxGainRatio || link.OffsetFraction > o.MaxOffsetFraction {
						link.Reason = "incompatible illumination"
					} else {
						link.Accepted = true
					}
				}
			}
			result.Links = append(result.Links, link)
		}
	}
	if err := r.solve(result, anchor); err != nil {
		return nil, err
	}
	result.Work = r.work
	result.seal, err = mosaicSeal(result)
	if err != nil {
		return nil, err
	}
	result.ID = "mosaic-v1:" + hex.EncodeToString(result.seal[:])
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return result, nil
}

func absMosaic(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

func (r *mosaicRun) sourceDigest(t MosaicTile) ([32]byte, error) {
	h := sha256.New()
	var b [8]byte
	put := func(v uint64) { binary.LittleEndian.PutUint64(b[:], v); h.Write(b[:]) }
	for _, v := range t.Image.Size() {
		put(uint64(v))
	}
	for _, v := range []float64{t.Image.Origin().X, t.Image.Origin().Y, t.Image.Origin().Z, t.Image.Spacing().X, t.Image.Spacing().Y, t.Image.Spacing().Z} {
		put(math.Float64bits(v))
	}
	for _, row := range t.Image.Direction() {
		for _, v := range row {
			put(math.Float64bits(v))
		}
	}
	for _, im := range []*registration.Image{t.Image, t.Mask} {
		if im == nil {
			put(0)
			continue
		}
		put(uint64(len(im.Data())))
		for _, v := range im.Data() {
			if err := r.step(1); err != nil {
				return [32]byte{}, err
			}
			if !mosaicFinite(float64(v)) {
				return [32]byte{}, fmt.Errorf("%w: nonfinite input", ErrMosaic)
			}
			put(uint64(math.Float32bits(v)))
		}
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out, nil
}

func mosaicSeal(m *MosaicLayout) ([32]byte, error) {
	// Hash all public derivation metadata with ID cleared; identity is not
	// self-referential. Private seal rejects later metadata mutation on compose.
	v := *m
	v.ID = ""
	v.seal = [32]byte{}
	b, err := json.Marshal(v)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: derivation metadata", ErrMosaic)
	}
	return sha256.Sum256(b), nil
}

func (r *mosaicRun) illumination(f, m, fm, mm *registration.Image, s [3]int) (float64, float64, float64, error) {
	n := 0.
	mf, mt, cff, cft := 0., 0., 0., 0.
	lo, hi := math.Inf(1), math.Inf(-1)
	fs, ms := f.Size(), m.Size()
	for y := 0; y < fs[1]; y++ {
		for x := 0; x < fs[0]; x++ {
			if err := r.step(1); err != nil {
				return 0, 0, 0, err
			}
			j, k := x+s[0], y+s[1]
			if j < 0 || k < 0 || j >= ms[0] || k >= ms[1] || fm.At(x, y, 0) <= 0 || mm.At(j, k, 0) <= 0 {
				continue
			}
			a, b := float64(f.At(x, y, 0)), float64(m.At(j, k, 0))
			n++
			da, db := a-mf, b-mt
			mf += da / n
			mt += db / n
			cff += da * (a - mf)
			cft += da * (b - mt)
			lo = math.Min(lo, a)
			hi = math.Max(hi, a)
		}
	}
	if cff <= 0 || hi <= lo {
		return 0, 0, 0, fmt.Errorf("%w: unresolved illumination", ErrMosaic)
	}
	gain := cft / cff
	offset := mt - gain*mf
	fraction := math.Abs(offset) / (hi - lo)
	if !mosaicFinite(gain) || !mosaicFinite(offset) || !mosaicFinite(fraction) {
		return 0, 0, 0, fmt.Errorf("%w: illumination arithmetic", ErrMosaic)
	}
	return gain, offset, fraction, nil
}
