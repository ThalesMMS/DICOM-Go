// Adapted from github.com/amrshadid/go-dicom compress/jpeg2000/decode.go at
// commit daecea14be3fbc27300aff20912ed7c8c8a0d599 (v1.6.0, MIT License, see
// LICENSE.amrshadid-go-dicom and PROVENANCE.md in this directory).

package j2kcore

import (
	"context"
	"fmt"
	"image"
	"math"

	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
)

// Decoding a codestream to samples, PS 15444-1 Annexes E and G.
//
// This is where the stages meet. Tier-2 said which bytes belong to which
// code-block, tier-1 turned those bytes into coefficients, and Annex F put the
// subbands back together; what is left is the arithmetic that surrounds them.
// Dequantization (E.1) turns a quantized integer into the value it stands for.
// The component transform (G.2, G.3) undoes the decorrelation an encoder
// applies across color channels. The DC level shift (G.1.2) moves samples
// back from the signed range they were coded in to the unsigned one a picture
// is stored in.

// Image is a decoded codestream.
type Image struct {
	Width, Height int
	// Components holds one plane per component, in raster order, each already
	// level shifted and clamped to the component's declared depth. Planes are
	// on the grid of the most finely sampled component; a component sampled
	// more coarsely repeats each sample over the positions it covers.
	Components [][]int32
	Depths     []int
	Signed     []bool
}

// DecodeOptions selects how much of a codestream is reconstructed.
type DecodeOptions struct {
	// AnnexJ restores the bounded difference collection after the spatial
	// inverse wavelet and before rounding, level shifting, or range clamping.
	// The caller must validate the matching header with j2kpart2.Inspect.
	AnnexJ *j2kpart2.Profile
	// Reduce discards the Reduce finest resolution levels: the image comes
	// out at 1/2^Reduce of its size, synthesized from the wavelet's own lower
	// resolutions (Annex F), for thumbnails and previews. The packets of the
	// discarded levels are read but not decoded. It must not exceed the
	// decomposition levels of any tile-component.
	Reduce int
	// Region limits the decode to a rectangle of the output image (at the
	// reduction): only the tiles it overlaps are decoded, and the image holds
	// only the region. The zero rectangle decodes the whole image.
	Region image.Rectangle
	// Layers keeps the first Layers quality layers of every tile and drops
	// the coding passes later layers contribute (B.10, B.12): every packet
	// header is still read, since the progression interleaves layers, but
	// the bodies of the dropped layers are not attached to their
	// code-blocks, and a progression whose remaining packets are all dropped
	// is not read further. 0 keeps every layer; a value above a tile's layer
	// count keeps all of that tile's.
	Layers int
}

// outputScale is how many reference grid positions one output sample spans
// across and down: the finest component sampling times 2^reduce.
func (c *Codestream) outputScale(reduce int) (int, int) {
	sx, sy := 0, 0
	for _, comp := range c.Components {
		if sx == 0 || comp.DX < sx {
			sx = comp.DX
		}
		if sy == 0 || comp.DY < sy {
			sy = comp.DY
		}
	}
	return maxInt(sx, 1) << reduce, maxInt(sy, 1) << reduce
}

// OutputSize is the size of the decoded image: the image area of the
// reference grid, sampled as the most finely sampled component is and reduced
// by 2^reduce (B.2, B.5).
func (c *Codestream) OutputSize(reduce int) (int, int) {
	sx, sy := c.outputScale(reduce)
	return ceilDivInt(int(c.Width), sx) - ceilDivInt(int(c.XOffset), sx),
		ceilDivInt(int(c.Height), sy) - ceilDivInt(int(c.YOffset), sy)
}

// OutputOrigin is where the decoded image begins on the output grid at a
// reduction: the image area's offset, sampled as OutputSize samples it. A
// decoded sample (x, y) lies at (x+ox, y+oy) of that grid.
func (c *Codestream) OutputOrigin(reduce int) (int, int) {
	sx, sy := c.outputScale(reduce)
	return ceilDivInt(int(c.XOffset), sx), ceilDivInt(int(c.YOffset), sy)
}

// Decode decodes a whole codestream to samples.
//
// Every tile is decoded independently and written into the image at the place
// its coordinates give, which is what makes a tiled file work without any
// special handling: a tile knows where it belongs.
func Decode(c *Codestream) (*Image, error) {
	return DecodeContext(context.Background(), c, DefaultLimits())
}

// DecodeContext is Decode with cooperative cancellation and budgets. It checks
// every budget before allocating, and checks ctx before every tile,
// tile-component, packet, code-block and wavelet level. On error or
// cancellation it returns no image.
func DecodeContext(ctx context.Context, c *Codestream, limits Limits) (*Image, error) {
	return DecodeContextOptions(ctx, c, limits, DecodeOptions{})
}

// DecodeContextOptions is DecodeContext with DecodeOptions.
func DecodeContextOptions(ctx context.Context, c *Codestream, limits Limits, options DecodeOptions) (*Image, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return nil, errorf("no codestream")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	for _, component := range c.Components {
		if component.Depth > 16 && options.AnnexJ == nil {
			return nil, &ErrUnsupported{Feature: "components above 16 bits"}
		}
	}
	if profile := options.AnnexJ; profile != nil && (!profile.Valid() || profile.Frames != len(c.Components) ||
		options.Reduce != 0 || !options.Region.Empty() || options.Layers != 0) {
		return nil, &ErrUnsupported{Feature: "Annex J reconstruction profile"}
	}
	if options.Reduce < 0 || options.Reduce > 32 {
		return nil, errorf("a reduction of %d resolution levels", options.Reduce)
	}
	if options.Layers < 0 {
		return nil, errorf("%d quality layers", options.Layers)
	}
	width, height := c.OutputSize(options.Reduce)
	if width <= 0 || height <= 0 {
		return nil, errorf("an image of %dx%d samples", width, height)
	}
	if width > 1<<16 || height > 1<<16 || width*height > 1<<28 {
		return nil, errorf("an image of %dx%d samples is larger than this decoder accepts",
			width, height)
	}
	region := image.Rect(0, 0, width, height)
	if !options.Region.Empty() {
		if !options.Region.In(region) {
			return nil, errorf("a region %v outside the %dx%d image", options.Region, width, height)
		}
		region = options.Region
	}
	width, height = region.Dx(), region.Dy()
	limits = limits.normalized()
	if err := limits.check(c); err != nil {
		return nil, err
	}

	img := &Image{Width: width, Height: height}
	for _, comp := range c.Components {
		img.Components = append(img.Components, make([]int32, width*height))
		img.Depths = append(img.Depths, comp.Depth)
		img.Signed = append(img.Signed, comp.Signed)
	}
	if options.AnnexJ != nil {
		for i := range img.Depths {
			img.Depths[i] = options.AnnexJ.Bits
			img.Signed[i] = options.AnnexJ.Signed
		}
	}

	sx, sy := c.outputScale(options.Reduce)
	originX, originY := ceilDivInt(int(c.XOffset), sx), ceilDivInt(int(c.YOffset), sy)
	for tile := 0; tile < c.NumTiles(); tile++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		tx0, ty0, tx1, ty1 := c.TileBounds(tile)
		covered := image.Rect(ceilDivInt(tx0, sx)-originX, ceilDivInt(ty0, sy)-originY, ceilDivInt(tx1, sx)-originX, ceilDivInt(ty1, sy)-originY)
		if !covered.Overlaps(region) {
			continue
		}
		if err := c.decodeTile(ctx, tile, img, options.Reduce, options.Layers, region.Min, options.AnnexJ); err != nil {
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return img, nil
}

// decodeTile decodes one tile and writes its samples into the image.
// The image holds the output samples from origin on. layers, when above 0,
// keeps that many quality layers.
func (c *Codestream) decodeTile(ctx context.Context, tile int, img *Image, reduce, layers int, origin image.Point, annexJ *j2kpart2.Profile) error {
	for comp := range c.Components {
		if style := c.CodingForTile(tile, comp); reduce > style.Levels {
			return errorf("a reduction of %d levels, but tile %d component %d has %d", reduce, tile, comp, style.Levels)
		}
	}
	data, err := c.readTilePacketsFiltered(ctx, tile, nil, packetFilter{layers: layers, reduce: reduce})
	if err != nil {
		return err
	}

	planes := make([][]float32, len(c.Components))
	reduced := make([]TileComponent, len(c.Components))
	for comp := range c.Components {
		if err := ctx.Err(); err != nil {
			return err
		}
		style := c.CodingForTile(tile, comp)
		// The tile-component at the resolution kept: its rectangle is that
		// resolution's, and the finer resolutions are dropped.
		tc := data.Components[comp]
		keep := len(tc.Resolutions) - reduce
		last := tc.Resolutions[keep-1]
		tc.Resolutions = tc.Resolutions[:keep]
		tc.X0, tc.Y0, tc.X1, tc.Y1 = last.X0, last.Y0, last.X1, last.Y1
		reduced[comp] = tc
		quant := c.QuantForTile(tile, comp)
		reversible := style.Wavelet == Wavelet53Reversible

		bands, err := c.subbandsOf(ctx, tile, comp, tc, quant, style, data.Blocks)
		if err != nil {
			return err
		}
		samples, err := inverseDWT(ctx, tc, bands, reversible)
		if err != nil {
			return err
		}
		planes[comp] = samples
	}

	style := c.CodingForTile(tile, 0)
	if style.MCT && len(planes) >= 3 {
		if err := inverseComponentTransform(planes, style.Wavelet == Wavelet53Reversible); err != nil {
			return err
		}
	}
	if annexJ != nil {
		for component := 1; component < len(planes); component++ {
			if err := ctx.Err(); err != nil {
				return err
			}
			for i, previous := range planes[component-1] {
				planes[component][i] += previous
			}
		}
	}

	// And into the image, with the level shift each component's own depth and
	// signedness decide. An output sample at (ox, oy) of the output grid lies
	// at (ox*sx, oy*sy) on the reference grid, which a component sampled
	// every sxc, syc positions (after the reduction) covers with its sample
	// (floor(x/sxc), floor(y/syc)).
	sx, sy := c.outputScale(reduce)
	tx0, ty0, tx1, ty1 := c.TileBounds(tile)
	originX, originY := ceilDivInt(int(c.XOffset), sx)+origin.X, ceilDivInt(int(c.YOffset), sy)+origin.Y
	for comp, plane := range planes {
		tc := reduced[comp]
		if tc.Width() <= 0 || tc.Height() <= 0 {
			continue
		}
		sxc, syc := c.Components[comp].DX<<reduce, c.Components[comp].DY<<reduce
		outputComponent := c.Components[comp]
		if annexJ != nil {
			outputComponent.Depth, outputComponent.Signed = annexJ.Bits, annexJ.Signed
		}
		shift := int32(0)
		if !outputComponent.Signed {
			shift = 1 << uint(outputComponent.Depth-1)
		}
		lo, hi := sampleRange(outputComponent)

		for oy := ceilDivInt(ty0, sy); oy < ceilDivInt(ty1, sy); oy++ {
			row := oy - originY
			if row < 0 || row >= img.Height {
				continue
			}
			yc := minInt(maxInt(floorDivInt(oy*sy, syc), tc.Y0), tc.Y1-1)
			for ox := ceilDivInt(tx0, sx); ox < ceilDivInt(tx1, sx); ox++ {
				col := ox - originX
				if col < 0 || col >= img.Width {
					continue
				}
				xc := minInt(maxInt(floorDivInt(ox*sx, sxc), tc.X0), tc.X1-1)
				v := int32(roundSample(plane[(yc-tc.Y0)*tc.Width()+(xc-tc.X0)])) + shift
				img.Components[comp][row*img.Width+col] = clampInt32(v, lo, hi)
			}
		}
	}
	return nil
}

// floorDivInt is a / b rounded towards negative infinity, for b > 0.
func floorDivInt(a, b int) int {
	if a >= 0 {
		return a / b
	}
	return -((-a + b - 1) / b)
}

// subbandsOf decodes every code-block of one tile-component and lays the
// dequantized coefficients out band by band.
func (c *Codestream) subbandsOf(ctx context.Context, tile, comp int, tc TileComponent, quant Quantization,
	style CodingStyle, blocks []CodeBlockData) (map[int]map[int]*subbandGrid, error) {

	bands := map[int]map[int]*subbandGrid{}
	for r, res := range tc.Resolutions {
		bands[r] = map[int]*subbandGrid{}
		for _, band := range res.Bands {
			w, h := band.Width(), band.Height()
			bands[r][band.Type] = &subbandGrid{
				x0: band.X0, y0: band.Y0, x1: band.X1, y1: band.Y1,
				values: make([]float32, maxInt(0, w*h)),
			}
		}
	}

	reversible := style.Wavelet == Wavelet53Reversible
	roiShift := c.ROIShiftForTile(tile, comp)
	for _, block := range blocks {
		if block.Component != comp || block.Resolution >= len(tc.Resolutions) {
			// Another component's, or a resolution the reduction drops.
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res := tc.Resolutions[block.Resolution]
		band := Band{}
		for _, candidate := range res.Bands {
			if candidate.Type == block.BandType {
				band = candidate
			}
		}

		magnitudeBits, err := MagnitudeBits(quant, band, block.Resolution, style.Levels)
		if err != nil {
			return nil, err
		}
		if roiShift > 0 {
			// Maxshift (H.2): the region's coefficients were scaled up by
			// 2^s before coding, so the band holds Mb + s bit planes and
			// the zero bit planes count down from there.
			if block.Style&BlockHT != 0 {
				return nil, &ErrUnsupported{Feature: "region of interest (RGN) with HT code-blocks"}
			}
			if magnitudeBits+roiShift-1-block.ZeroBitPlanes > 29 {
				return nil, &ErrUnsupported{Feature: fmt.Sprintf(
					"region of interest shift %d past the 30 bit planes a code-block holds here", roiShift)}
			}
			magnitudeBits += roiShift
		}
		coefficients, err := DecodeBlock(block, magnitudeBits)
		if err != nil {
			return nil, err
		}
		if roiShift > 0 {
			descaleROI(coefficients, roiShift)
		}

		step, err := stepSize(quant, band, block.Resolution, style.Levels,
			c.Components[comp].Depth)
		if err != nil {
			return nil, err
		}

		grid := bands[block.Resolution][block.BandType]
		if grid == nil {
			return nil, errorf("a code-block in a band the resolution does not have")
		}
		bw := block.Block.Width()
		for y := 0; y < block.Block.Height(); y++ {
			row := block.Block.Y0 + y
			if row < grid.y0 || row >= grid.y1 {
				continue
			}
			for x := 0; x < bw; x++ {
				col := block.Block.X0 + x
				if col < grid.x0 || col >= grid.x1 {
					continue
				}
				// The coefficients arrive in halves; the step carries the
				// factor of two that turns them back into whole ones.
				grid.values[(row-grid.y0)*grid.width()+(col-grid.x0)] =
					scaleCoefficient(coefficients[y*bw+x], step, reversible)
			}
		}
	}
	return bands, nil
}

// descaleROI undoes the Maxshift scaling of a code-block (H.2): a coefficient
// whose magnitude reaches 2^s belongs to the region and is shifted back down by
// s bit planes; anything smaller is background and is already at its own
// scale, because the encoder chose s so that no background coefficient
// reaches 2^s.
//
// The values are in halves (see DecodeBlock), so a magnitude of 2^s is 2^(s+1)
// here. The low bit of a value carries the half that estimates a fully decoded
// coefficient. A region coefficient's own low s bit planes are zero, so the
// shift drops that estimate when the block was decoded through them, and it
// is restored; a coefficient truncated above them keeps its estimate, now s
// planes lower.
func descaleROI(coefficients []int32, shift int) {
	if shift >= 30 {
		// No value a code-block decodes to here reaches 2^30 halves (see
		// decodeBlock): with so large a shift, every one is background.
		return
	}
	threshold := int32(1) << uint(shift+1)
	below := threshold>>1 - 1 // the bits the shift discards
	for i, v := range coefficients {
		magnitude := v
		if magnitude < 0 {
			magnitude = -magnitude
		}
		if magnitude < threshold {
			continue
		}
		scaled := magnitude >> uint(shift)
		if magnitude&below != 0 {
			scaled |= 1
		}
		if v < 0 {
			scaled = -scaled
		}
		coefficients[i] = scaled
	}
}

// scaleCoefficient turns one code-block value, which is in halves, into the
// coefficient the wavelet filter wants.
//
// The reversible path must divide exactly and round towards zero, because
// lossless depends on it: the half is below the last bit a lossless decode
// reaches, and dropping it is what makes the result the encoder's own integer.
// The lossy path keeps the half through the multiplication, where it is worth
// a real part of a quantization step.
func scaleCoefficient(value int32, step float32, reversible bool) float32 {
	if reversible {
		return float32(value / 2)
	}
	return float32(value) * step / 2
}

// stepSize is the quantization step a subband's coefficients are multiplied by
// (E-3). A codestream that is not quantized at all has a step of one: the
// coefficient tier-1 produced is already the value.
//
// The nominal range Rb is the component's depth alone. Table E.1's subband
// gains belong to the reversible filter, which is never quantized; the
// irreversible filter carries its normalization in its own coefficients, so
// adding a gain here would scale every detail band by two or four.
func stepSize(q Quantization, band Band, resolution, levels, depth int) (float32, error) {
	if q.Style == QuantNone {
		return 1, nil
	}

	index := SubbandIndex(resolution, band.Type)
	var exponent, mantissa int
	switch q.Style {
	case QuantDerived:
		if len(q.Exponents) == 0 || len(q.Mantissas) == 0 {
			return 0, errorf("a derived quantization with no values")
		}
		exponent = q.Exponents[0] + band.Level - levels
		mantissa = q.Mantissas[0]
	default:
		if index < 0 || index >= len(q.Exponents) || index >= len(q.Mantissas) {
			return 0, errorf("subband %d has no quantization step, of %d given",
				index, len(q.Exponents))
		}
		exponent = q.Exponents[index]
		mantissa = q.Mantissas[index]
	}

	power := depth - exponent
	if power < -31 || power > 31 {
		return 0, errorf("a quantization step of 2^%d", power)
	}
	return float32(math.Ldexp(1+float64(mantissa)/2048, power)), nil
}

// inverseComponentTransform undoes the decorrelation across three components
// (G.2, G.3). The reversible transform is integer and exact; the irreversible
// one is the familiar YCbCr matrix.
func inverseComponentTransform(planes [][]float32, reversible bool) error {
	if len(planes) < 3 {
		return errorf("a component transform needs three components, not %d", len(planes))
	}
	n := len(planes[0])
	if len(planes[1]) != n || len(planes[2]) != n {
		return errorf("the three transformed components are different sizes")
	}

	for i := 0; i < n; i++ {
		y0, y1, y2 := planes[0][i], planes[1][i], planes[2][i]
		if reversible {
			// RCT (G.2.2). The green channel is recovered first, because the
			// other two were coded as differences from it.
			g := int32(y0) - floorDiv4Int(int32(y1)+int32(y2))
			planes[0][i] = float32(int32(y2) + g)
			planes[1][i] = float32(g)
			planes[2][i] = float32(int32(y1) + g)
			continue
		}
		// ICT (G.3.2).
		planes[0][i] = y0 + 1.402*y2
		planes[1][i] = y0 - 0.344136*y1 - 0.714136*y2
		planes[2][i] = y0 + 1.772*y1
	}
	return nil
}

// sampleRange is what a component's samples may be after the level shift.
// Clamping is part of the standard (G.1.2), not a safety net: an irreversible
// transform can overshoot, and a lossy file is allowed to.
func sampleRange(comp Component) (int32, int32) {
	if comp.Signed {
		return -(1 << uint(comp.Depth-1)), 1<<uint(comp.Depth-1) - 1
	}
	return 0, 1<<uint(comp.Depth) - 1
}

func clampInt32(v, lo, hi int32) int32 {
	switch {
	case v < lo:
		return lo
	case v > hi:
		return hi
	}
	return v
}

// roundSample rounds a reconstructed sample to the nearest integer, with ties
// going to the even one. A reversible decode produces whole numbers already, so
// this only matters for the irreversible path — but there it matters a lot:
// rounding halves upwards instead moved 497 of a 1024 sample test image by one.
func roundSample(v float32) float64 {
	return math.RoundToEven(float64(v))
}

func floorDiv4Int(x int32) int32 { return x >> 2 }
