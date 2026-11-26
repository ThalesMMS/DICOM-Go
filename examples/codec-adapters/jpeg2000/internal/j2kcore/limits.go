// Local addition for dicom-go (not part of the adapted upstream files).

package j2kcore

import (
	"fmt"
	"math"
)

// Limits bounds one codestream decode. Zero fields mean the default in
// DefaultLimits; the caller's values replace the upstream fixed ceilings, which
// DecodeContext still applies as an outer bound.
type Limits struct {
	MaxComponents        int
	MaxTiles             int
	MaxResolutions       int   // decomposition levels + 1, per tile-component
	MaxSamples           int64 // width * height * components of the output
	MaxTileWorkBytes     int64 // float32 planes held while one tile is decoded
	MaxCodeBlocksPerTile int
}

// DefaultLimits covers the images the adapter accepts from DICOM: 16 bits per
// sample, 65535x65535 at most, and at most four components.
func DefaultLimits() Limits {
	return Limits{
		MaxComponents:        4,
		MaxTiles:             4096,
		MaxResolutions:       33,
		MaxSamples:           1 << 28,
		MaxTileWorkBytes:     2 << 30,
		MaxCodeBlocksPerTile: 1 << 20,
	}
}

// ErrLimit reports the resource a codestream would exceed. It carries sizes
// only, never sample values.
type ErrLimit struct {
	Resource string
	Value    int64
	Limit    int64
}

func (e *ErrLimit) Error() string {
	return fmt.Sprintf("jpeg2000: %s %d exceeds the limit %d", e.Resource, e.Value, e.Limit)
}

func (l Limits) normalized() Limits {
	d := DefaultLimits()
	if l.MaxComponents <= 0 {
		l.MaxComponents = d.MaxComponents
	}
	if l.MaxTiles <= 0 {
		l.MaxTiles = d.MaxTiles
	}
	if l.MaxResolutions <= 0 {
		l.MaxResolutions = d.MaxResolutions
	}
	if l.MaxSamples <= 0 {
		l.MaxSamples = d.MaxSamples
	}
	if l.MaxTileWorkBytes <= 0 {
		l.MaxTileWorkBytes = d.MaxTileWorkBytes
	}
	if l.MaxCodeBlocksPerTile <= 0 {
		l.MaxCodeBlocksPerTile = d.MaxCodeBlocksPerTile
	}
	return l
}

// check validates the header against the budgets before image planes or
// packet state are allocated. Products use int64 with explicit overflow checks, so
// the result is the same on 32- and 64-bit targets.
func (l Limits) check(c *Codestream) error {
	l = l.normalized()
	components := len(c.Components)
	if components == 0 || components > l.MaxComponents {
		return &ErrLimit{Resource: "components", Value: int64(components), Limit: int64(l.MaxComponents)}
	}
	tiles := c.NumTiles()
	if tiles <= 0 || tiles > l.MaxTiles {
		return &ErrLimit{Resource: "tiles", Value: int64(tiles), Limit: int64(l.MaxTiles)}
	}
	width := int64(c.Width) - int64(c.XOffset)
	height := int64(c.Height) - int64(c.YOffset)
	samples, ok := mulInt64(width, height, int64(components))
	if !ok || samples > l.MaxSamples {
		return &ErrLimit{Resource: "output samples", Value: samples, Limit: l.MaxSamples}
	}
	for tile := 0; tile < tiles; tile++ {
		x0, y0, x1, y1 := c.TileBounds(tile)
		area, ok := mulInt64(int64(x1-x0), int64(y1-y0))
		if !ok {
			return &ErrLimit{Resource: "tile area", Value: math.MaxInt64, Limit: l.MaxTileWorkBytes}
		}
		// One float32 plane per component for the samples and one for the
		// subband coefficients being synthesized.
		work, ok := mulInt64(area, int64(components), 8)
		if !ok || work > l.MaxTileWorkBytes {
			return &ErrLimit{Resource: "tile work bytes", Value: work, Limit: l.MaxTileWorkBytes}
		}
		var blocks int64
		for comp := 0; comp < components; comp++ {
			if levels := c.CodingForTile(tile, comp).Levels; levels+1 > l.MaxResolutions {
				return &ErrLimit{Resource: "resolutions", Value: int64(levels + 1), Limit: int64(l.MaxResolutions)}
			}
			tc, err := c.tileComponentGeometry(tile, comp)
			if err != nil {
				return err
			}
			for _, res := range tc.Resolutions {
				for _, band := range res.Bands {
					if band.Width() <= 0 || band.Height() <= 0 {
						continue
					}
					wide := ceilDivPow2(band.X1, res.CodeBlockW) - floorDivPow2(band.X0, res.CodeBlockW)
					high := ceilDivPow2(band.Y1, res.CodeBlockH) - floorDivPow2(band.Y0, res.CodeBlockH)
					count, ok := mulInt64(int64(wide), int64(high))
					if !ok || blocks > math.MaxInt64-count {
						return &ErrLimit{Resource: "code-blocks per tile", Value: math.MaxInt64, Limit: int64(l.MaxCodeBlocksPerTile)}
					}
					blocks += count
					if blocks > int64(l.MaxCodeBlocksPerTile) {
						return &ErrLimit{Resource: "code-blocks per tile", Value: blocks, Limit: int64(l.MaxCodeBlocksPerTile)}
					}
				}
			}
		}
	}
	return nil
}

func mulInt64(values ...int64) (int64, bool) {
	product := int64(1)
	for _, v := range values {
		if v < 0 {
			return math.MaxInt64, false
		}
		if v != 0 && product > math.MaxInt64/v {
			return math.MaxInt64, false
		}
		product *= v
	}
	return product, true
}
