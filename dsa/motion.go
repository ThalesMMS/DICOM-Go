package dsa

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/ThalesMMS/dicom-go/optimize"
)

// Mask motion correction beyond a global shift (#1090): a rigid motion of
// the mask registered by gradient difference, and a local displacement field
// from block matching. Both describe where each contrast-frame pixel is
// found in the mask frame, so the corrected mask is the mask sampled there.

// ErrMotion reports frames that cannot be registered.
var ErrMotion = errors.New("dsa: mask motion cannot be estimated")

// Rigid is a rigid motion in pixels: contrast-frame pixel p is found in the
// mask at R(Angle)·(p − Center) + Center + (TX, TY), x right and y down, the
// angle in radians.
type Rigid struct {
	Angle, TX, TY float64
	Center        [2]float64
}

// Map returns where contrast-frame pixel (x, y) lies in the mask.
func (r Rigid) Map(x, y float64) (float64, float64) {
	s, c := math.Sincos(r.Angle)
	dx, dy := x-r.Center[0], y-r.Center[1]
	return c*dx - s*dy + r.Center[0] + r.TX, s*dx + c*dy + r.Center[1] + r.TY
}

func (r Rigid) isFinite() bool {
	return finite(r.Angle) && finite(r.TX) && finite(r.TY) && finite(r.Center[0]) && finite(r.Center[1])
}

// RigidOptions configure RegisterRigid.
type RigidOptions struct {
	// Initial is the starting shift, such as the phase-correlation estimate
	// or the stored Mask Module shift; the zero value starts at identity.
	Initial Shift
	// MaxSamples bounds the longer side of the registration images: larger
	// frames are block-averaged down to it (0: 256).
	MaxSamples int
}

// RigidRegistration is the result of RegisterRigid.
type RigidRegistration struct {
	Rigid Rigid
	// Similarity is the mean gradient difference per compared pixel and
	// axis at the result, 1 for identical gradients.
	Similarity  float64
	Evaluations int
}

// RegisterRigid registers the mask (moving) rigidly onto the contrast frame
// (reference) by maximizing the gradient difference similarity of Hipwell et
// al., as ITK's GradientDifferenceImageToImageMetric computes it: the Sobel
// derivatives of the reference and of the moved mask are compared through
// V/(V + d²), V the variance of the reference derivative, which the vessels
// present in only one frame barely influence. Powell's method optimizes the
// angle and translation on a half-resolution level and then the working
// resolution.
func RegisterRigid(reference, moving []float64, columns, rows int, opts RigidOptions) (RigidRegistration, error) {
	if columns < 16 || rows < 16 || !frameSizeMatches(columns, rows, reference, moving) {
		return RigidRegistration{}, fmt.Errorf("%w: need two %dx%d frames of at least 16x16", ErrMotion, columns, rows)
	}
	if !opts.Initial.isFinite() {
		return RigidRegistration{}, fmt.Errorf("%w: initial shift: %w", ErrMotion, ErrInvalidPixelShift)
	}
	maxSamples := opts.MaxSamples
	if maxSamples <= 0 {
		maxSamples = 256
	}
	factor := max(1, (max(columns, rows)+maxSamples-1)/maxSamples)
	centre := [2]float64{float64(columns-1) / 2, float64(rows-1) / 2}
	dx, dy := opts.Initial.Displacement()
	// The mask is sampled at p − displacement for a shift that moves the
	// mask by +displacement.
	params := []float64{0, -dx, -dy}
	result := RigidRegistration{}
	for _, level := range []int{factor * 2, factor} {
		ref, cols, rws := downsample(reference, columns, rows, level)
		mov, _, _ := downsample(moving, columns, rows, level)
		if cols < 8 || rws < 8 {
			continue
		}
		scale := float64(level)
		levelCentre := [2]float64{(centre[0] - (scale-1)/2) / scale, (centre[1] - (scale-1)/2) / scale}
		metric := newGradientDifference(ref, cols, rws)
		radius := math.Hypot(float64(cols), float64(rws)) / 2
		objective := optimize.Func(func(x []float64) (float64, error) {
			rigid := Rigid{Angle: x[0], TX: x[1] / scale, TY: x[2] / scale, Center: levelCentre}
			if !rigid.isFinite() {
				return math.Inf(1), nil
			}
			value, ok := metric.value(warpRigid(mov, cols, rws, rigid))
			if !ok {
				return math.Inf(1), nil
			}
			return -value, nil
		})
		out, err := optimize.Powell{
			StepLength: 2, StepTolerance: 1e-3, ValueTolerance: 1e-7, MaximumIterations: 60,
			// One search unit changes the angle by 1/radius radians and
			// the full-resolution translations by 1/scale pixels.
			Scales: []float64{radius, scale, scale},
		}.Minimize(objective, params)
		if err != nil {
			return RigidRegistration{}, err
		}
		params = out.X
		result.Similarity = -out.Value
		result.Evaluations += out.Evaluations
	}
	if math.IsInf(result.Similarity, 0) || math.IsNaN(result.Similarity) {
		return RigidRegistration{}, fmt.Errorf("%w: the frames do not overlap", ErrMotion)
	}
	result.Rigid = Rigid{Angle: params[0], TX: params[1], TY: params[2], Center: centre}
	return result, nil
}

// ApplyRigid returns the mask sampled bilinearly where r maps each
// contrast-frame pixel (edge pixels repeat outside the frame).
// The transform must be finite.
func ApplyRigid(mask []float64, columns, rows int, r Rigid) ([]float64, error) {
	if !frameSizeMatches(columns, rows, mask) {
		return nil, fmt.Errorf("%w: %d samples for %dx%d", ErrMotion, len(mask), columns, rows)
	}
	if !r.isFinite() {
		return nil, fmt.Errorf("%w: rigid transform contains a non-finite parameter", ErrMotion)
	}
	return warpRigid(mask, columns, rows, r), nil
}

func warpRigid(mask []float64, columns, rows int, r Rigid) []float64 {
	out := make([]float64, len(mask))
	s, c := math.Sincos(r.Angle)
	for y := 0; y < rows; y++ {
		dy := float64(y) - r.Center[1]
		for x := 0; x < columns; x++ {
			dx := float64(x) - r.Center[0]
			out[y*columns+x] = bilinear(mask, columns, rows, c*dx-s*dy+r.Center[0]+r.TX, s*dx+c*dy+r.Center[1]+r.TY)
		}
	}
	return out
}

// gradientDifference holds the Sobel derivatives of the reference and their
// variances.
type gradientDifference struct {
	columns, rows int
	gx, gy        []float64
	vx, vy        float64
}

func newGradientDifference(reference []float64, columns, rows int) *gradientDifference {
	g := &gradientDifference{columns: columns, rows: rows}
	g.gx, g.gy = sobel(reference, columns, rows)
	g.vx, g.vy = variance(g.gx, columns, rows), variance(g.gy, columns, rows)
	return g
}

// value is the mean of V/(V + (∂F − ∂M)²) over the interior pixels and both
// axes; false when the reference has no gradient.
func (g *gradientDifference) value(moved []float64) (float64, bool) {
	if g.vx <= 0 && g.vy <= 0 {
		return 0, false
	}
	mx, my := sobel(moved, g.columns, g.rows)
	var sum float64
	var n int
	for y := 1; y < g.rows-1; y++ {
		for x := 1; x < g.columns-1; x++ {
			i := y*g.columns + x
			if g.vx > 0 {
				d := g.gx[i] - mx[i]
				sum += g.vx / (g.vx + d*d)
				n++
			}
			if g.vy > 0 {
				d := g.gy[i] - my[i]
				sum += g.vy / (g.vy + d*d)
				n++
			}
		}
	}
	return sum / float64(n), n > 0
}

// GradientDifference is the mean gradient difference similarity of two
// frames of the same size (1 for identical Sobel derivatives, lower as they
// differ).
func GradientDifference(reference, moving []float64, columns, rows int) (float64, error) {
	if columns < 3 || rows < 3 || !frameSizeMatches(columns, rows, reference, moving) {
		return 0, fmt.Errorf("%w: need two %dx%d frames", ErrMotion, columns, rows)
	}
	value, ok := newGradientDifference(reference, columns, rows).value(moving)
	if !ok {
		return 0, fmt.Errorf("%w: the reference frame is uniform", ErrMotion)
	}
	return value, nil
}

func sobel(values []float64, columns, rows int) ([]float64, []float64) {
	gx, gy := make([]float64, len(values)), make([]float64, len(values))
	for y := 1; y < rows-1; y++ {
		for x := 1; x < columns-1; x++ {
			at := func(dx, dy int) float64 { return values[(y+dy)*columns+x+dx] }
			i := y*columns + x
			gx[i] = (at(1, -1) + 2*at(1, 0) + at(1, 1) - at(-1, -1) - 2*at(-1, 0) - at(-1, 1)) / 8
			gy[i] = (at(-1, 1) + 2*at(0, 1) + at(1, 1) - at(-1, -1) - 2*at(0, -1) - at(1, -1)) / 8
		}
	}
	return gx, gy
}

func variance(values []float64, columns, rows int) float64 {
	var sum, squares float64
	var n int
	for y := 1; y < rows-1; y++ {
		for x := 1; x < columns-1; x++ {
			v := values[y*columns+x]
			sum += v
			squares += v * v
			n++
		}
	}
	if n == 0 {
		return 0
	}
	mean := sum / float64(n)
	return squares/float64(n) - mean*mean
}

// downsample block-averages by factor.
func downsample(values []float64, columns, rows, factor int) ([]float64, int, int) {
	if factor <= 1 {
		return values, columns, rows
	}
	cols, rws := columns/factor, rows/factor
	out := make([]float64, cols*rws)
	for y := 0; y < rws; y++ {
		for x := 0; x < cols; x++ {
			var sum float64
			for j := 0; j < factor; j++ {
				row := (y*factor + j) * columns
				for i := 0; i < factor; i++ {
					sum += values[row+x*factor+i]
				}
			}
			out[y*cols+x] = sum / float64(factor*factor)
		}
	}
	return out, cols, rws
}

// bilinear samples values at (x, y), clamping to the frame.
func bilinear(values []float64, columns, rows int, x, y float64) float64 {
	x = math.Max(0, math.Min(float64(columns-1), x))
	y = math.Max(0, math.Min(float64(rows-1), y))
	x0, y0 := int(x), int(y)
	x1, y1 := min(x0+1, columns-1), min(y0+1, rows-1)
	fx, fy := x-float64(x0), y-float64(y0)
	top := values[y0*columns+x0]*(1-fx) + values[y0*columns+x1]*fx
	bottom := values[y1*columns+x0]*(1-fx) + values[y1*columns+x1]*fx
	return top*(1-fy) + bottom*fy
}

// BlockOptions configure MatchBlocks.
type BlockOptions struct {
	// BlockSize is the side of the square blocks compared (0: 32 pixels);
	// blocks are centred every BlockSize/2 pixels.
	BlockSize int
	// SearchRadius bounds the displacement searched in pixels (0: 8).
	SearchRadius int
	// MinimumCorrelation is the normalized correlation below which a block
	// match is rejected (0: 0.6).
	MinimumCorrelation float64
	// MaxSamples bounds the longer side of the frames matched: larger frames
	// are block-averaged down to it, BlockSize and SearchRadius apply there,
	// and the field is interpolated back to full size (0: 512).
	MaxSamples int
}

// BlockMatch is the best displacement of one block.
type BlockMatch struct {
	// X, Y is the block centre in the contrast frame; DX, DY where it is
	// found in the mask, relative to it.
	X, Y, DX, DY float64
	Correlation  float64
	// Accepted is false for a flat block, a weak correlation, or an outlier
	// among its neighbours.
	Accepted bool
}

// DisplacementField gives, for each contrast-frame pixel, where it is found
// in the mask relative to itself.
type DisplacementField struct {
	Columns, Rows int
	DX, DY        []float64
}

// BlockMatchResult is the result of MatchBlocks.
type BlockMatchResult struct {
	Field  DisplacementField
	Blocks []BlockMatch
}

// MatchBlocks estimates a smooth local displacement of the mask onto the
// contrast frame, after ITK's BlockMatchingImageFilter: each block of the
// contrast frame is searched for in the mask by normalized correlation over
// integer displacements, refined to sub-pixel by Lucas-Kanade steps; flat
// blocks, weak ones (below MinimumCorrelation or well below the median
// correlation, as where contrast crosses a block) and blocks that disagree
// with the median of their neighbours are rejected, and the accepted displacements are spread
// over the block grid by normalized Gaussian convolution (weighted by the
// correlation) and interpolated bilinearly to every pixel.
func MatchBlocks(reference, moving []float64, columns, rows int, opts BlockOptions) (BlockMatchResult, error) {
	if !frameSizeMatches(columns, rows, reference, moving) {
		return BlockMatchResult{}, fmt.Errorf("%w: %d and %d samples for %dx%d", ErrMotion, len(reference), len(moving), columns, rows)
	}
	maxSamples := opts.MaxSamples
	if maxSamples <= 0 {
		maxSamples = 512
	}
	factor := max(1, (max(columns, rows)+maxSamples-1)/maxSamples)
	if factor == 1 {
		return matchBlocks(reference, moving, columns, rows, opts)
	}
	ref, cols, rws := downsample(reference, columns, rows, factor)
	mov, _, _ := downsample(moving, columns, rows, factor)
	coarse, err := matchBlocks(ref, mov, cols, rws, opts)
	if err != nil {
		return BlockMatchResult{}, err
	}
	// Coarse pixel u covers full pixels f·u … f·u + f − 1, centred at
	// f·u + (f − 1)/2; displacements scale by f.
	scale := float64(factor)
	offset := (scale - 1) / 2
	for i := range coarse.Blocks {
		block := &coarse.Blocks[i]
		block.X, block.Y = block.X*scale+offset, block.Y*scale+offset
		block.DX, block.DY = block.DX*scale, block.DY*scale
	}
	field := DisplacementField{Columns: columns, Rows: rows, DX: make([]float64, columns*rows), DY: make([]float64, columns*rows)}
	for y := 0; y < rows; y++ {
		v := (float64(y) - offset) / scale
		for x := 0; x < columns; x++ {
			u := (float64(x) - offset) / scale
			field.DX[y*columns+x] = scale * bilinear(coarse.Field.DX, cols, rws, u, v)
			field.DY[y*columns+x] = scale * bilinear(coarse.Field.DY, cols, rws, u, v)
		}
	}
	return BlockMatchResult{Field: field, Blocks: coarse.Blocks}, nil
}

func matchBlocks(reference, moving []float64, columns, rows int, opts BlockOptions) (BlockMatchResult, error) {
	size := opts.BlockSize
	if size <= 0 {
		size = 32
	}
	radius := opts.SearchRadius
	if radius <= 0 {
		radius = 8
	}
	minimum := opts.MinimumCorrelation
	if minimum == 0 {
		minimum = 0.6
	}
	shortSide := min(columns, rows)
	if size > shortSide || radius > (shortSide-size)/2 {
		return BlockMatchResult{}, fmt.Errorf("%w: %dx%d frames are too small for %d-pixel blocks searched ±%d", ErrMotion, columns, rows, size, radius)
	}
	step := max(1, size/2)
	half := size / 2
	// Block centres on a regular grid that keeps every search inside.
	var xs, ys []int
	for x := half + radius; x+half+radius <= columns; x += step {
		xs = append(xs, x)
	}
	for y := half + radius; y+half+radius <= rows; y += step {
		ys = append(ys, y)
	}
	blocks := make([]BlockMatch, len(xs)*len(ys))
	for j, cy := range ys {
		for i, cx := range xs {
			blocks[j*len(xs)+i] = matchBlock(reference, moving, columns, cx, cy, half, radius, minimum)
		}
	}
	rejectWeakBlocks(blocks)
	rejectOutliers(blocks, len(xs), len(ys))
	field := spreadBlocks(blocks, xs, ys, columns, rows, float64(step))
	return BlockMatchResult{Field: field, Blocks: blocks}, nil
}

func matchBlock(reference, moving []float64, columns, cx, cy, half, radius int, minimum float64) BlockMatch {
	match := BlockMatch{X: float64(cx), Y: float64(cy)}
	x0, y0 := cx-half, cy-half
	size := 2 * half
	var mean float64
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			mean += reference[(y0+y)*columns+x0+x]
		}
	}
	n := float64(size * size)
	mean /= n
	var refNorm float64
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			d := reference[(y0+y)*columns+x0+x] - mean
			refNorm += d * d
		}
	}
	if refNorm < 1e-9*n {
		return match // a flat block carries no displacement
	}
	best, bestX, bestY := math.Inf(-1), 0, 0
	for oy := -radius; oy <= radius; oy++ {
		for ox := -radius; ox <= radius; ox++ {
			var sum, squares, cross float64
			for y := 0; y < size; y++ {
				row := (y0+y+oy)*columns + x0 + ox
				refRow := (y0+y)*columns + x0
				for x := 0; x < size; x++ {
					m := moving[row+x]
					sum += m
					squares += m * m
					cross += (reference[refRow+x] - mean) * m
				}
			}
			movNorm := squares - sum*sum/n
			score := 0.0
			if movNorm > 1e-12 {
				score = cross / math.Sqrt(refNorm*movNorm)
			}
			if score > best {
				best, bestX, bestY = score, ox, oy
			}
		}
	}
	match.DX, match.DY = float64(bestX), float64(bestY)
	if abs(bestX) < radius && abs(bestY) < radius {
		match.DX, match.DY = refineBlock(reference, moving, columns, x0, y0, size, match.DX, match.DY)
	}
	match.Correlation = best
	// A best match on the search border may lie beyond it.
	match.Accepted = best >= minimum && abs(bestX) < radius && abs(bestY) < radius
	return match
}

// refineBlock refines an integer displacement to sub-pixel by robust
// Gauss-Newton (Lucas-Kanade) steps on the block normalized for gain and
// offset, which keeps the accuracy of smooth images where a parabola through
// correlation scores locks towards whole pixels, and down-weights pixels
// that do not match (contrast in one frame only). The refinement stays
// within a pixel of the integer match.
func refineBlock(reference, moving []float64, columns, x0, y0, size int, dx, dy float64) (float64, float64) {
	rows := len(moving) / columns
	n := size * size
	ref := make([]float64, n)
	var mean, squares float64
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			v := reference[(y0+y)*columns+x0+x]
			ref[y*size+x] = v
			mean += v
			squares += v * v
		}
	}
	mean /= float64(n)
	sd := math.Sqrt(math.Max(squares/float64(n)-mean*mean, 0))
	if sd == 0 {
		return dx, dy
	}
	for i := range ref {
		ref[i] = (ref[i] - mean) / sd
	}
	startX, startY := dx, dy
	moved := make([]float64, n)
	residuals := make([]float64, 0, n)
	for iteration := 0; iteration < 8; iteration++ {
		var mMean, mSquares float64
		for y := 0; y < size; y++ {
			for x := 0; x < size; x++ {
				v := bilinear(moving, columns, rows, float64(x0+x)+dx, float64(y0+y)+dy)
				moved[y*size+x] = v
				mMean += v
				mSquares += v * v
			}
		}
		mMean /= float64(n)
		mSd := math.Sqrt(math.Max(mSquares/float64(n)-mMean*mMean, 0))
		if mSd == 0 {
			break
		}
		// Tukey's biweight on the residuals, scaled by their median absolute
		// value, discounts the pixels only one frame has (the vessels).
		residuals = residuals[:0]
		for y := 1; y < size-1; y++ {
			for x := 1; x < size-1; x++ {
				i := y*size + x
				residuals = append(residuals, math.Abs(ref[i]-(moved[i]-mMean)/mSd))
			}
		}
		cut := 4.685 * 1.4826 * median(residuals)
		var gxx, gxy, gyy, bx, by float64
		for y := 1; y < size-1; y++ {
			for x := 1; x < size-1; x++ {
				i := y*size + x
				gx := (moved[i+1] - moved[i-1]) / (2 * mSd)
				gy := (moved[i+size] - moved[i-size]) / (2 * mSd)
				e := ref[i] - (moved[i]-mMean)/mSd
				w := 1.0
				if cut > 1e-9 {
					if u := e / cut; math.Abs(u) < 1 {
						w = (1 - u*u) * (1 - u*u)
					} else {
						w = 0
					}
				}
				gxx += w * gx * gx
				gxy += w * gx * gy
				gyy += w * gy * gy
				bx += w * gx * e
				by += w * gy * e
			}
		}
		det := gxx*gyy - gxy*gxy
		if det <= 1e-12 || !finite(det) {
			break
		}
		stepX, stepY := (gyy*bx-gxy*by)/det, (gxx*by-gxy*bx)/det
		if !finite(stepX) || !finite(stepY) {
			break
		}
		dx = math.Max(startX-1, math.Min(startX+1, dx+stepX))
		dy = math.Max(startY-1, math.Min(startY+1, dy+stepY))
		if math.Hypot(stepX, stepY) < 1e-3 {
			break
		}
	}
	return dx, dy
}

func abs(v int) int {
	if v < 0 {
		return -v
	}
	return v
}

// rejectWeakBlocks rejects blocks whose correlation falls more than 0.05
// below the median of the accepted blocks: contrast present in only one
// frame (the vessels) lowers the correlation of the blocks it crosses and
// pulls their match.
func rejectWeakBlocks(blocks []BlockMatch) {
	var correlations []float64
	for _, block := range blocks {
		if block.Accepted {
			correlations = append(correlations, block.Correlation)
		}
	}
	if len(correlations) < 4 {
		return
	}
	cut := median(correlations) - 0.05
	for i := range blocks {
		if blocks[i].Accepted && blocks[i].Correlation < cut {
			blocks[i].Accepted = false
		}
	}
}

// rejectOutliers rejects accepted blocks more than 1.5 pixels from the
// median of their accepted neighbours (3 × 3 grid).
func rejectOutliers(blocks []BlockMatch, nx, ny int) {
	reject := make([]bool, len(blocks))
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			block := blocks[j*nx+i]
			if !block.Accepted {
				continue
			}
			var dxs, dys []float64
			for dj := -1; dj <= 1; dj++ {
				for di := -1; di <= 1; di++ {
					if (di == 0 && dj == 0) || i+di < 0 || i+di >= nx || j+dj < 0 || j+dj >= ny {
						continue
					}
					if neighbour := blocks[(j+dj)*nx+i+di]; neighbour.Accepted {
						dxs, dys = append(dxs, neighbour.DX), append(dys, neighbour.DY)
					}
				}
			}
			if len(dxs) < 3 {
				continue
			}
			if math.Hypot(block.DX-median(dxs), block.DY-median(dys)) > 1.5 {
				reject[j*nx+i] = true
			}
		}
	}
	for i := range blocks {
		if reject[i] {
			blocks[i].Accepted = false
		}
	}
}

func median(values []float64) float64 {
	sorted := append([]float64(nil), values...)
	sort.Float64s(sorted)
	n := len(sorted)
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// spreadBlocks fills the block grid by normalized Gaussian convolution of
// the accepted displacements (σ one grid step, weighted by correlation) and
// interpolates it bilinearly to every pixel; pixels beyond the grid take the
// nearest grid value.
func spreadBlocks(blocks []BlockMatch, xs, ys []int, columns, rows int, step float64) DisplacementField {
	nx, ny := len(xs), len(ys)
	gridX, gridY := make([]float64, nx*ny), make([]float64, nx*ny)
	for j := 0; j < ny; j++ {
		for i := 0; i < nx; i++ {
			var sx, sy, sw float64
			for jj := max(0, j-3); jj <= min(ny-1, j+3); jj++ {
				for ii := max(0, i-3); ii <= min(nx-1, i+3); ii++ {
					block := blocks[jj*nx+ii]
					if !block.Accepted {
						continue
					}
					d2 := float64((ii-i)*(ii-i) + (jj-j)*(jj-j))
					w := block.Correlation * math.Exp(-d2/2)
					sx += w * block.DX
					sy += w * block.DY
					sw += w
				}
			}
			if sw > 1e-6 {
				gridX[j*nx+i], gridY[j*nx+i] = sx/sw, sy/sw
			}
		}
	}
	field := DisplacementField{Columns: columns, Rows: rows, DX: make([]float64, columns*rows), DY: make([]float64, columns*rows)}
	if nx == 0 || ny == 0 {
		return field
	}
	for y := 0; y < rows; y++ {
		v := math.Max(0, math.Min(float64(ny-1), (float64(y)-float64(ys[0]))/step))
		for x := 0; x < columns; x++ {
			u := math.Max(0, math.Min(float64(nx-1), (float64(x)-float64(xs[0]))/step))
			field.DX[y*columns+x] = bilinear(gridX, nx, ny, u, v)
			field.DY[y*columns+x] = bilinear(gridY, nx, ny, u, v)
		}
	}
	return field
}

// ApplyField returns the mask sampled bilinearly at each contrast-frame
// pixel plus its finite displacement.
func ApplyField(mask []float64, columns, rows int, field DisplacementField) ([]float64, error) {
	if field.Columns != columns || field.Rows != rows || !frameSizeMatches(columns, rows, mask, field.DX, field.DY) {
		return nil, fmt.Errorf("%w: field and mask sizes differ", ErrMotion)
	}
	out := make([]float64, len(mask))
	for y := 0; y < rows; y++ {
		for x := 0; x < columns; x++ {
			i := y*columns + x
			dx, dy := field.DX[i], field.DY[i]
			if !finite(dx) || !finite(dy) {
				return nil, fmt.Errorf("%w: non-finite displacement at (%d,%d)", ErrMotion, x, y)
			}
			out[i] = bilinear(mask, columns, rows, float64(x)+dx, float64(y)+dy)
		}
	}
	return out, nil
}
