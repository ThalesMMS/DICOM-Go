package render

import (
	"context"
	"math"
	"time"
)

// surfaceVRBlockGrid shares the prepared cache with the other ray casters.
// A contended surface call can return on cancellation while another
// presentation builds the same immutable grid.
func surfaceVRBlockGrid(ctx context.Context, prepared *VRPreparedVolume, sampler *volumeSampler) (*vrBlockGrid, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !prepared.blocksMu.TryLock() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		for !prepared.blocksMu.TryLock() {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ticker.C:
			}
		}
	}
	defer prepared.blocksMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if prepared.blocks != nil {
		return prepared.blocks, nil
	}
	grid, err := buildVRBlockGrid(ctx, sampler)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	prepared.blocks = grid
	return grid, nil
}

// Every sample in an empty block is below the first-solid predicate value>=iso.
// A wholly solid block cannot be skipped: its first sample may be the hit.
// Unknown/non-finite blocks follow the original interpolation path.
func skipsVRSurfaceBlock(grid *vrBlockGrid, block int, iso float64) bool {
	lo, hi := float64(grid.min[block]), float64(grid.max[block])
	if math.IsNaN(lo) || math.IsNaN(hi) {
		return false
	}
	_, hi = widen(lo, hi)
	return hi < iso
}

// advanceVRSurfaceLeap skips only samples strictly inside this block's faces.
// Repeated addition preserves the original floating-point sample positions.
// Each skipped logical sample retains the original budget and cancellation
// check; bisection still starts at hit-step, including skipped air.
func advanceVRSurfaceLeap(ctx context.Context, leap *vrSpaceLeap, block int, a, b Vec3, t, t1, step float64, guard *vrRayGuard, sampleIndex int) (float64, int, bool) {
	bx := block % leap.grid.nx
	by := (block / leap.grid.nx) % leap.grid.ny
	bz := block / (leap.grid.nx * leap.grid.ny)
	tIn, tOut := math.Inf(-1), math.Inf(1)
	for _, axis := range [3][3]float64{{a.X, b.X, float64(bx)}, {a.Y, b.Y, float64(by)}, {a.Z, b.Z, float64(bz)}} {
		lower := axis[2]*vrBlockSize + vrLeapMargin
		upper := (axis[2]+1)*vrBlockSize - vrLeapMargin
		if axis[1] == 0 {
			if axis[0] < lower || axis[0] > upper {
				return t, sampleIndex, true
			}
			continue
		}
		enter, exit := (lower-axis[0])/axis[1], (upper-axis[0])/axis[1]
		if enter > exit {
			enter, exit = exit, enter
		}
		tIn, tOut = math.Max(tIn, enter), math.Min(tOut, exit)
	}
	if t < tIn {
		return t, sampleIndex, true
	}
	for {
		next := t + step
		if next <= t || next >= tOut || next > t1 {
			return t, sampleIndex, true
		}
		if !guard.take() {
			return t, sampleIndex, false
		}
		if sampleIndex%vrCancellationCheckStride == 0 && ctx.Err() != nil {
			return t, sampleIndex, false
		}
		sampleIndex++
		t = next
	}
}
