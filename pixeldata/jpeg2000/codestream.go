package jpeg2000

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"runtime"
	"sync"
	"sync/atomic"

	"github.com/ThalesMMS/dicom-go/internal/htj2k"
	"github.com/ThalesMMS/dicom-go/internal/j2kpart2"
)

// coding is the codestream an encoder produces for every frame.
type coding struct {
	ht         bool // HT block coder (ISO/IEC 15444-15)
	reversible bool // 5/3 and no quantization, else 9/7 and scalar quantization
	mct        bool // RCT or ICT on three components
	rpcl       bool // RPCL progression and a TLM marker segment
	levels     int  // 0: automatic
	step       float64
	annexJ     *j2kpart2.Profile
}

const (
	codeBlockExponent = 6 // 64x64 code-blocks
	codeBlockSize     = 1 << codeBlockExponent
	maxAutoLevels     = 5
	minGuardBits      = 2
)

var errCoefficientRange = errors.New("jpeg2000: coefficient range exceeds the codestream's bit-planes")

// image is one frame's components as integers: unsigned samples before the
// DC level shift, signed samples as they are.
type image struct {
	width, height int
	depth         int
	signed        bool
	planes        [][]int32
}

// subband is one band of one component after the transform.
type subband struct {
	orientation int
	level       int // decomposition level; the LL band has the deepest
	x0, y0      int // position in the transformed plane
	w, h        int
	gain        int // log2 of the nominal gain (reversible exponents)
	coeffs      []int32
	exponent    int // epsilon_b
	mantissa    int // mu_b (irreversible)
	maxMag      int32
}

// resolutionsOf lists the bands of each resolution, LL first.
func resolutionsOf(w, h, levels int) [][]subband {
	res := make([][]subband, levels+1)
	res[0] = []subband{{orientation: orientLL, level: levels, w: levelSize(w, levels), h: levelSize(h, levels)}}
	for r := 1; r <= levels; r++ {
		n := levels - r + 1
		cw, ch := levelSize(w, n-1), levelSize(h, n-1)
		lw, lh := levelSize(w, n), levelSize(h, n)
		res[r] = []subband{
			{orientation: orientHL, level: n, x0: lw, w: cw - lw, h: lh, gain: 1},
			{orientation: orientLH, level: n, y0: lh, w: lw, h: ch - lh, gain: 1},
			{orientation: orientHH, level: n, x0: lw, y0: lh, w: cw - lw, h: ch - lh, gain: 2},
		}
	}
	return res
}

func autoLevels(w, h int) int {
	levels := 0
	for side := min(w, h); side >= 2 && levels < maxAutoLevels; side /= 2 {
		levels++
	}
	return levels
}

// encodeImage produces a complete single-tile JPEG 2000 codestream.
func encodeImage(ctx context.Context, img image, c coding) ([]byte, error) {
	w, h := img.width, img.height
	levels := autoLevels(w, h)
	if c.levels > 0 {
		limit := 0
		for side := min(w, h); side >= 2 && limit < 32; side /= 2 {
			limit++
		}
		levels = min(c.levels, limit)
	}
	components := len(img.planes)
	mct := c.mct && components == 3

	resolutions := make([][][]subband, components) // [component][resolution][band]
	if c.reversible {
		planes := make([][]int32, components)
		for i, p := range img.planes {
			planes[i] = append([]int32(nil), p...)
			if !img.signed {
				shift := int32(1) << (img.depth - 1)
				for k := range planes[i] {
					planes[i][k] -= shift
				}
			}
		}
		if mct {
			forwardRCT(planes[0], planes[1], planes[2])
		}
		for i, plane := range planes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			forwardDWT53(plane, w, h, levels)
			resolutions[i] = cutBands(plane, w, h, levels)
			for r := range resolutions[i] {
				for b := range resolutions[i][r] {
					band := &resolutions[i][r][b]
					band.exponent = img.depth + band.gain
					if mct {
						band.exponent++
					}
				}
			}
		}
	} else {
		planes := make([][]float64, components)
		for i, p := range img.planes {
			planes[i] = make([]float64, len(p))
			shift := 0.0
			if !img.signed {
				shift = float64(int32(1) << (img.depth - 1))
			}
			for k, v := range p {
				planes[i][k] = float64(v) - shift
			}
		}
		if mct {
			forwardICT(planes[0], planes[1], planes[2])
		}
		for i, plane := range planes {
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			forwardDWT97(plane, w, h, levels)
			resolutions[i] = quantizeBands(plane, w, h, levels, img.depth, c.step)
		}
	}

	// Guard bits: enough for the largest index of every band to fit in the
	// band's Mb = G + epsilon - 1 magnitude bit-planes.
	guard := minGuardBits
	for _, comp := range resolutions {
		for _, res := range comp {
			for _, band := range res {
				if need := bitLength(int(band.maxMag)) - band.exponent + 1; need > guard {
					guard = need
				}
			}
		}
	}
	if guard > 7 {
		return nil, errCoefficientRange
	}
	magb := 0
	for _, comp := range resolutions {
		for _, res := range comp {
			for _, band := range res {
				magb = max(magb, guard+band.exponent-1)
			}
		}
	}

	// Tier-1 and tier-2: one packet per resolution and component.
	packets := make([][][]packetBand, len(resolutions[0]))
	var jobs []blockJob
	for r := range packets {
		packets[r] = make([][]packetBand, components)
		for comp := 0; comp < components; comp++ {
			packets[r][comp] = make([]packetBand, len(resolutions[comp][r]))
			for b := range resolutions[comp][r] {
				band := &resolutions[comp][r][b]
				pb := &packets[r][comp][b]
				*pb = packetBand{mb: guard + band.exponent - 1}
				if band.w == 0 || band.h == 0 {
					continue
				}
				pb.cols = (band.w + codeBlockSize - 1) / codeBlockSize
				pb.rows = (band.h + codeBlockSize - 1) / codeBlockSize
				pb.blocks = make([]codedBlock, pb.cols*pb.rows)
				for i := range pb.blocks {
					jobs = append(jobs, blockJob{band: band, out: &pb.blocks[i], x0: i % pb.cols * codeBlockSize, y0: i / pb.cols * codeBlockSize})
				}
			}
		}
	}
	if err := codeBlocks(ctx, jobs, c.ht); err != nil {
		return nil, err
	}
	var body []byte
	for r := range packets {
		for comp := 0; comp < components; comp++ {
			body = writePacket(body, packets[r][comp], c.ht)
		}
	}
	return assemble(img, c, levels, mct, guard, magb, resolutions[0], body)
}

// cutBands copies each band of a transformed plane out of the plane.
func cutBands(plane []int32, w, h, levels int) [][]subband {
	res := resolutionsOf(w, h, levels)
	for r := range res {
		for b := range res[r] {
			band := &res[r][b]
			band.coeffs = make([]int32, band.w*band.h)
			for y := 0; y < band.h; y++ {
				for x := 0; x < band.w; x++ {
					v := plane[(band.y0+y)*w+band.x0+x]
					band.coeffs[y*band.w+x] = v
					if v < 0 {
						v = -v
					}
					band.maxMag = max(band.maxMag, v)
				}
			}
		}
	}
	return res
}

// quantizeBands quantizes each band of an irreversibly transformed plane
// with a step inversely proportional to the norm of the band's synthesis
// basis, so that every index contributes about the same error to the image
// (step is that error's quantization step in sample units).
func quantizeBands(plane []float64, w, h, levels, depth int, step float64) [][]subband {
	res := resolutionsOf(w, h, levels)
	for r := range res {
		for b := range res[r] {
			band := &res[r][b]
			horizontal := band.orientation == orientHL || band.orientation == orientHH
			vertical := band.orientation == orientLH || band.orientation == orientHH
			norm := 1.0
			if band.level > 0 {
				norm = synthesisNorm(band.level, horizontal) * synthesisNorm(band.level, vertical)
			}
			band.exponent, band.mantissa = stepParameters(step/norm, depth)
			delta := math.Ldexp(1+float64(band.mantissa)/2048, depth-band.exponent)
			band.coeffs = make([]int32, band.w*band.h)
			for y := 0; y < band.h; y++ {
				for x := 0; x < band.w; x++ {
					v := plane[(band.y0+y)*w+band.x0+x]
					q := math.Floor(math.Abs(v) / delta)
					if q > float64(htj2k.MaxMagnitude) {
						q = float64(htj2k.MaxMagnitude)
					}
					m := int32(q)
					band.maxMag = max(band.maxMag, m)
					if v < 0 {
						m = -m
					}
					band.coeffs[y*band.w+x] = m
				}
			}
		}
	}
	return res
}

// stepParameters encodes a quantization step as the exponent and 11-bit
// mantissa of E-3: delta = 2^(depth-epsilon) * (1 + mu/2048).
func stepParameters(delta float64, depth int) (int, int) {
	e := int(math.Floor(math.Log2(delta)))
	mu := int(math.Round((delta/math.Ldexp(1, e) - 1) * 2048))
	if mu >= 2048 {
		mu, e = 0, e+1
	}
	epsilon := depth - e
	switch {
	case epsilon < 0:
		return 0, 2047
	case epsilon > 31:
		return 31, 0
	}
	return epsilon, mu
}

// blockJob is one code-block to code: its band, its origin in the band and
// where its result goes.
type blockJob struct {
	band   *subband
	x0, y0 int
	out    *codedBlock
}

// codeBlocks runs tier-1 over every code-block, spreading them over up to
// GOMAXPROCS goroutines. Results land in place, so the codestream does not
// depend on the scheduling.
func codeBlocks(ctx context.Context, jobs []blockJob, ht bool) error {
	workers := min(runtime.GOMAXPROCS(0), len(jobs))
	if workers <= 1 {
		var coder blockCoder
		block := make([]int32, codeBlockSize*codeBlockSize)
		for _, job := range jobs {
			if err := codeBlock(ctx, &coder, block, job, ht); err != nil {
				return err
			}
		}
		return nil
	}
	var (
		next     atomic.Int64
		wg       sync.WaitGroup
		failOnce sync.Once
		failure  error
	)
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var coder blockCoder
			block := make([]int32, codeBlockSize*codeBlockSize)
			for {
				i := int(next.Add(1)) - 1
				if i >= len(jobs) {
					return
				}
				if err := codeBlock(ctx, &coder, block, jobs[i], ht); err != nil {
					failOnce.Do(func() { failure = err })
					next.Store(int64(len(jobs)))
					return
				}
			}
		}()
	}
	wg.Wait()
	return failure
}

// codeBlock codes one code-block; an all-zero block is left uncoded.
func codeBlock(ctx context.Context, coder *blockCoder, scratch []int32, job blockJob, ht bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	band := job.band
	bw, bh := min(codeBlockSize, band.w-job.x0), min(codeBlockSize, band.h-job.y0)
	samples := scratch[:bw*bh]
	nonzero := false
	for y := 0; y < bh; y++ {
		row := band.coeffs[(job.y0+y)*band.w+job.x0 : (job.y0+y)*band.w+job.x0+bw]
		copy(samples[y*bw:], row)
		for _, v := range row {
			nonzero = nonzero || v != 0
		}
	}
	switch {
	case !nonzero:
	case ht:
		data, err := htj2k.EncodeCleanup(samples, bw, bh)
		if err != nil {
			return fmt.Errorf("jpeg2000: HT code-block: %w", err)
		}
		*job.out = codedBlock{data: data, passes: 1}
	default:
		data, passes, planes := coder.encodeBlock(samples, bw, bh, band.orientation)
		*job.out = codedBlock{data: data, passes: passes, planes: planes}
	}
	return nil
}

// assemble writes the main header, the tile-part and EOC around body.
func assemble(img image, c coding, levels int, mct bool, guard, magb int, bands [][]subband, body []byte) ([]byte, error) {
	components := len(img.planes)
	out := make([]byte, 0, len(body)+256)
	be16 := func(v int) { out = binary.BigEndian.AppendUint16(out, uint16(v)) }
	be32 := func(v int) { out = binary.BigEndian.AppendUint32(out, uint32(v)) }

	be16(0xFF4F) // SOC

	be16(0xFF51) // SIZ
	be16(38 + 3*components)
	rsiz := 0
	if c.ht {
		rsiz = 0x4000
	}
	if c.annexJ != nil {
		rsiz = j2kpart2.Capabilities
	}
	be16(rsiz)
	be32(img.width)
	be32(img.height)
	be32(0)
	be32(0)
	be32(img.width)
	be32(img.height)
	be32(0)
	be32(0)
	be16(components)
	for i := 0; i < components; i++ {
		ssiz := img.depth - 1
		if img.signed {
			ssiz |= 0x80
		}
		out = append(out, byte(ssiz), 1, 1)
	}

	if c.ht {
		be16(0xFF50) // CAP
		be16(8)
		be32(0x00020000) // Part 15 capabilities follow
		ccap := magbParameter(magb)
		if !c.reversible {
			ccap |= 0x0020
		}
		be16(ccap)
	}

	be16(0xFF52) // COD
	be16(12)
	out = append(out, 0) // default precincts, no SOP or EPH
	progression := byte(0)
	if c.rpcl {
		progression = 2
	}
	out = append(out, progression)
	be16(1) // one quality layer
	if c.annexJ != nil {
		out = append(out, 2)
	} else if mct {
		out = append(out, 1)
	} else {
		out = append(out, 0)
	}
	style := byte(0)
	if c.ht {
		style = 0x40
	}
	transform := byte(0)
	if c.reversible {
		transform = 1
	}
	out = append(out, byte(levels), codeBlockExponent-2, codeBlockExponent-2, style, transform)

	be16(0xFF5C) // QCD
	n := 0
	for _, res := range bands {
		n += len(res)
	}
	if c.reversible {
		be16(3 + n)
		out = append(out, byte(guard<<5))
		for _, res := range bands {
			for _, band := range res {
				out = append(out, byte(band.exponent<<3))
			}
		}
	} else {
		be16(3 + 2*n)
		out = append(out, byte(guard<<5|2))
		for _, res := range bands {
			for _, band := range res {
				be16(band.exponent<<11 | band.mantissa)
			}
		}
	}

	if c.annexJ != nil {
		out = append(out, c.annexJ.Markers()...)
	}
	psot := 12 + 2 + len(body)
	if c.rpcl {
		be16(0xFF55) // TLM: one tile-part of tile 0
		be16(9)
		out = append(out, 0, 0x50, 0)
		be32(psot)
	}

	be16(0xFF90) // SOT
	be16(10)
	be16(0)
	be32(psot)
	out = append(out, 0, 1)
	be16(0xFF93) // SOD
	out = append(out, body...)
	be16(0xFFD9) // EOC
	return out, nil
}

// magbParameter is the MAGB field of Ccap15: the bit-planes B any
// code-block may need, coded as 0 for up to 8, B-8 below 28, 13+B/4 below
// 48 and 31 beyond.
func magbParameter(b int) int {
	switch {
	case b <= 8:
		return 0
	case b < 28:
		return b - 8
	case b < 48:
		return 13 + b>>2
	default:
		return 31
	}
}

// forwardRCT is the reversible component transform (G.2).
func forwardRCT(r, g, b []int32) {
	for i := range r {
		y := (r[i] + 2*g[i] + b[i]) >> 2
		u := b[i] - g[i]
		v := r[i] - g[i]
		r[i], g[i], b[i] = y, u, v
	}
}

// forwardICT is the irreversible component transform (G.3).
func forwardICT(r, g, b []float64) {
	for i := range r {
		y := 0.299*r[i] + 0.587*g[i] + 0.114*b[i]
		cb := -0.16875*r[i] - 0.33126*g[i] + 0.5*b[i]
		cr := 0.5*r[i] - 0.41869*g[i] - 0.08131*b[i]
		r[i], g[i], b[i] = y, cb, cr
	}
}
