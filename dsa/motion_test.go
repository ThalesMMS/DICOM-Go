package dsa

import (
	"errors"
	"math"
	"testing"
)

// motionTexture is an anatomical background: overlapping blobs of different
// sizes, a gentle gradient and a finer structure, in modality-like units.
func motionTexture(x, y float64) float64 {
	blob := func(cx, cy, width, height float64) float64 {
		dx, dy := x-cx, y-cy
		return height * math.Exp(-(dx*dx+dy*dy)/width)
	}
	return 200 + 0.3*x + blob(60, 70, 300, 400) + blob(150, 90, 500, -300) + blob(100, 170, 200, 350) +
		blob(190, 180, 800, 250) + blob(40, 190, 150, -200) + 40*math.Sin(x/9)*math.Cos(y/13) +
		// Fine bone-like structure, without which smooth blocks leave the
		// motion along their edges undetermined.
		25*math.Sin(x/4.3+y/6.1)*math.Cos(y/3.7-x/7.9)
}

// vessel is contrast present only in the live frame: a dark curved band.
func vessel(x, y float64) float64 {
	d := y - (120 + 30*math.Sin(x/40))
	return -250 * math.Exp(-d*d/18)
}

// motionFrames samples the contrast frame (texture plus vessel) and the
// mask, the texture seen where warp puts each contrast pixel: mask(warp(p))
// = texture(p).
func motionFrames(n int, inverse func(x, y float64) (float64, float64)) (contrast, mask []float64) {
	contrast, mask = make([]float64, n*n), make([]float64, n*n)
	for y := 0; y < n; y++ {
		for x := 0; x < n; x++ {
			fx, fy := float64(x), float64(y)
			contrast[y*n+x] = motionTexture(fx, fy) + vessel(fx, fy)
			sx, sy := inverse(fx, fy)
			mask[y*n+x] = motionTexture(sx, sy)
		}
	}
	return contrast, mask
}

func TestRegisterRigidRecoversSmallMotionOnDownsampledFrame(t *testing.T) {
	const n = 128
	truth := Rigid{Angle: 2 * math.Pi / 180, TX: 2.2, TY: -1.4, Center: [2]float64{(n - 1) / 2.0, (n - 1) / 2.0}}
	contrast, mask := motionFrames(n, func(x, y float64) (float64, float64) {
		s, c := math.Sincos(-truth.Angle)
		dx, dy := x-truth.Center[0]-truth.TX, y-truth.Center[1]-truth.TY
		return c*dx - s*dy + truth.Center[0], s*dx + c*dy + truth.Center[1]
	})
	result, err := RegisterRigid(contrast, mask, n, n, RigidOptions{MaxSamples: 64})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rigid
	if math.Abs(got.Angle-truth.Angle)*180/math.Pi > 0.15 || math.Abs(got.TX-truth.TX) > 0.25 || math.Abs(got.TY-truth.TY) > 0.25 {
		t.Fatalf("recovered %+v, want %+v", got, truth)
	}
}

// residual is the RMS of contrast − corrected mask away from the vessel and
// the border, where a perfect correction leaves zero.
func residual(contrast, corrected []float64, n int) float64 {
	var sum float64
	var count int
	for y := 16; y < n-16; y++ {
		for x := 16; x < n-16; x++ {
			if math.Abs(vessel(float64(x), float64(y))) > 1 {
				continue
			}
			d := contrast[y*n+x] - corrected[y*n+x]
			sum += d * d
			count++
		}
	}
	return math.Sqrt(sum / float64(count))
}

// TestRegisterRigidRecoversARotationAndShift: a mask rotated by 3° and
// shifted by (4.5, −2.3) pixels relative to the contrast frame is recovered
// within 0.1° and 0.2 pixel despite the vessel present only in the contrast
// frame, from the phase-correlation-like start of a rounded shift.
func TestRegisterRigidRecoversARotationAndShift(t *testing.T) {
	const n = 224
	truth := Rigid{Angle: 3 * math.Pi / 180, TX: 4.5, TY: -2.3, Center: [2]float64{float64(n-1) / 2, float64(n-1) / 2}}
	// mask(truth(p)) = texture(p): the mask at q shows texture(truth⁻¹(q)).
	inverse := func(x, y float64) (float64, float64) {
		s, c := math.Sincos(-truth.Angle)
		dx, dy := x-truth.Center[0]-truth.TX, y-truth.Center[1]-truth.TY
		return c*dx - s*dy + truth.Center[0], s*dx + c*dy + truth.Center[1]
	}
	contrast, mask := motionFrames(n, inverse)
	result, err := RegisterRigid(contrast, mask, n, n, RigidOptions{Initial: ShiftFromDisplacement(-4, 2)})
	if err != nil {
		t.Fatal(err)
	}
	got := result.Rigid
	t.Logf("angle %.3f°, shift (%.3f, %.3f), similarity %.4f, %d evaluations", got.Angle*180/math.Pi, got.TX, got.TY, result.Similarity, result.Evaluations)
	if math.Abs(got.Angle-truth.Angle)*180/math.Pi > 0.1 || math.Abs(got.TX-truth.TX) > 0.2 || math.Abs(got.TY-truth.TY) > 0.2 {
		t.Fatalf("recovered %+v, want %+v", got, truth)
	}
	corrected, err := ApplyRigid(mask, n, n, got)
	if err != nil {
		t.Fatal(err)
	}
	before, after := residual(contrast, mask, n), residual(contrast, corrected, n)
	t.Logf("residual %.2f before, %.2f after", before, after)
	if after > before/10 {
		t.Fatalf("residual %.2f after correction, %.2f before", after, before)
	}
	// Identical gradients score 1; the refused cases.
	if value, err := GradientDifference(contrast, contrast, n, n); err != nil || math.Abs(value-1) > 1e-12 {
		t.Fatalf("self similarity %v: %v", value, err)
	}
	if _, err := RegisterRigid(contrast[:10], mask[:10], 3, 3, RigidOptions{}); !errors.Is(err, ErrMotion) {
		t.Fatalf("tiny frames: %v", err)
	}
	if _, err := GradientDifference(make([]float64, 100), make([]float64, 100), 10, 10); !errors.Is(err, ErrMotion) {
		t.Fatalf("uniform reference: %v", err)
	}
}

// TestMatchBlocksRecoversASmoothLocalMotion: a mask deformed by a smooth
// displacement of up to 2.5 pixels is corrected by block matching: the
// field matches the true displacement within 0.25 pixel on average inside
// the frame (a pixel at worst where the vessel crosses a block) and the
// subtraction residual drops to under an eighth.
func TestMatchBlocksRecoversASmoothLocalMotion(t *testing.T) {
	const n = 224
	displacement := func(x, y float64) (float64, float64) {
		return 2.5 * math.Sin(y/40), 1.8 * math.Cos(x/50)
	}
	// mask(p + d(p)) = texture(p); for this gentle field, the mask at q
	// shows texture(q − d(q)) to well under a tenth of a pixel after two
	// fixed-point steps.
	inverse := func(x, y float64) (float64, float64) {
		px, py := x, y
		for i := 0; i < 6; i++ {
			dx, dy := displacement(px, py)
			px, py = x-dx, y-dy
		}
		return px, py
	}
	contrast, mask := motionFrames(n, inverse)
	result, err := MatchBlocks(contrast, mask, n, n, BlockOptions{BlockSize: 24, SearchRadius: 5})
	if err != nil {
		t.Fatal(err)
	}
	accepted := 0
	for _, block := range result.Blocks {
		if block.Accepted {
			accepted++
		}
	}
	var worst, total float64
	var samples int
	for y := 30; y < n-30; y += 7 {
		for x := 30; x < n-30; x += 7 {
			dx, dy := displacement(float64(x), float64(y))
			i := y*n + x
			e := math.Hypot(result.Field.DX[i]-dx, result.Field.DY[i]-dy)
			worst, total, samples = math.Max(worst, e), total+e, samples+1
		}
	}
	corrected, err := ApplyField(mask, n, n, result.Field)
	if err != nil {
		t.Fatal(err)
	}
	before, after := residual(contrast, mask, n), residual(contrast, corrected, n)
	mean := total / float64(samples)
	t.Logf("%d of %d blocks accepted, field error mean %.3f px, worst %.3f px, residual %.2f before, %.2f after", accepted, len(result.Blocks), mean, worst, before, after)
	// Blocks that the vessel's edge crosses keep up to about a pixel of
	// error; elsewhere the field is within a few tenths.
	if mean > 0.25 || worst > 1 || after > before/8 {
		t.Fatalf("field error mean %.3f px, worst %.3f px, residual %.2f after, %.2f before", mean, worst, after, before)
	}
	// Matched on frames averaged to half size, the field is scaled back: a
	// coarser but still close estimate.
	coarse, err := MatchBlocks(contrast, mask, n, n, BlockOptions{BlockSize: 12, SearchRadius: 3, MaxSamples: n / 2})
	if err != nil {
		t.Fatal(err)
	}
	var coarseTotal float64
	samples = 0
	for y := 30; y < n-30; y += 7 {
		for x := 30; x < n-30; x += 7 {
			dx, dy := displacement(float64(x), float64(y))
			i := y*n + x
			coarseTotal += math.Hypot(coarse.Field.DX[i]-dx, coarse.Field.DY[i]-dy)
			samples++
		}
	}
	if mean := coarseTotal / float64(samples); mean > 0.5 {
		t.Fatalf("half-size field error mean %.3f px", mean)
	}
	if _, err := MatchBlocks(contrast[:100], mask[:100], 10, 10, BlockOptions{}); !errors.Is(err, ErrMotion) {
		t.Fatalf("tiny frames: %v", err)
	}
	if _, err := ApplyField(mask, n, n, DisplacementField{}); !errors.Is(err, ErrMotion) {
		t.Fatalf("mismatched field: %v", err)
	}
}
