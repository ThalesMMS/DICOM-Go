package spatialreg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/render"
)

// VectorBytes reports the logical displacement-vector payload actually stored
// by this grid. It excludes slice headers, capacity, and runtime overhead and
// does not derive its value from mutable Dimensions. A nil grid reports zero.
// Callers must not mutate the grid concurrently with this read or preparation.
func (g *Grid) VectorBytes() uint64 {
	if g == nil {
		return 0
	}
	return uint64(len(g.vectors)) * uint64(unsafe.Sizeof(vector3f{}))
}

// ErrInverseClosed reports queries made after a prepared inverter is closed.
var ErrInverseClosed = errors.New("dicom/spatialreg: prepared inverse is closed")

// PreparedInverse owns one immutable snapshot of the matrices, grid metadata
// and displacement vectors. It caches only affine constants, not inverse
// points or a dense inverse field. Independent queries/batches are concurrent;
// Close waits for their completion and releases the snapshot. Do not copy it.
type PreparedInverse struct {
	mu                sync.RWMutex
	registration      *Registration
	constants         inverseConstants
	options           InverseOptions
	retainedGridBytes uint64
}

// PrepareInverse validates/inverts the constant matrices and snapshots the
// registration. The caller must keep r and its grid unchanged for this call;
// subsequent mutation does not affect the prepared generation. Grid-domain
// and folding diagnostics remain per query, with the original definitions.
func (r *Registration) PrepareInverse(opts InverseOptions) (*PreparedInverse, error) {
	if r == nil {
		return nil, fmt.Errorf("%w: missing registration", ErrInvalidObject)
	}
	c, err := prepareInverseConstants(r)
	if err != nil {
		return nil, err
	}
	snapshot := &Registration{Pre: r.Pre, Post: r.Post}
	var retained uint64
	if r.Grid != nil {
		grid := *r.Grid
		grid.vectors = append([]vector3f(nil), r.Grid.vectors...)
		snapshot.Grid = &grid
		retained = uint64(len(grid.vectors)) * 12
	}
	return &PreparedInverse{registration: snapshot, constants: c, options: opts.normalized(), retainedGridBytes: retained}, nil
}

// InvertPoint evaluates a source point against this prepared generation.
// A canceled operation returns a zero result and the context error. Numerical
// failures retain InvertPoint's diagnostic result and wrapped sentinel.
func (p *PreparedInverse) InvertPoint(ctx context.Context, source render.Vec3) (InverseResult, error) {
	if p == nil {
		return InverseResult{}, ErrInverseClosed
	}
	if ctx == nil {
		return InverseResult{}, fmt.Errorf("%w: missing context", ErrInvalidObject)
	}
	if err := ctx.Err(); err != nil {
		return InverseResult{}, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.registration == nil {
		return InverseResult{}, ErrInverseClosed
	}
	if !finiteVec(source) {
		return InverseResult{}, fmt.Errorf("%w: non-finite source point", ErrInvalidObject)
	}
	if err := ctx.Err(); err != nil {
		return InverseResult{}, err
	}
	result, err := p.registration.invertWithConstants(ctx, source, p.options, p.constants)
	// Cancellation wins at publication, including no-iteration paths and
	// numerical diagnostic returns. The original non-context API is unchanged.
	if canceled := ctx.Err(); canceled != nil {
		return InverseResult{}, canceled
	}
	return result, err
}

// InverseBatchError identifies the failing input without publishing a partial
// batch. Result holds only that input's original numerical diagnostic result.
type InverseBatchError struct {
	Index  int
	Result InverseResult
	Err    error
}

func (e *InverseBatchError) Error() string {
	return fmt.Sprintf("dicom/spatialreg: inverse batch point %d: %v", e.Index, e.Err)
}
func (e *InverseBatchError) Unwrap() error { return e.Err }

// InvertPoints serially evaluates a batch in its original order. Points must
// remain unchanged until return. It never seeds a query from a previous one.
// Any numerical error or cancellation returns nil, never partial success.
// The caller admits the result payload (one InverseResult per input); no
// dense inverse field or background worker is allocated.
func (p *PreparedInverse) InvertPoints(ctx context.Context, points []render.Vec3) ([]InverseResult, error) {
	if p == nil {
		return nil, ErrInverseClosed
	}
	if ctx == nil {
		return nil, fmt.Errorf("%w: missing context", ErrInvalidObject)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	if p.registration == nil {
		return nil, ErrInverseClosed
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	out := make([]InverseResult, len(points))
	for i, source := range points {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !finiteVec(source) {
			return nil, &InverseBatchError{Index: i, Err: fmt.Errorf("%w: non-finite source point", ErrInvalidObject)}
		}
		result, err := p.registration.invertWithConstants(ctx, source, p.options, p.constants)
		if canceled := ctx.Err(); canceled != nil {
			return nil, &InverseBatchError{Index: i, Err: canceled}
		}
		if err != nil {
			return nil, &InverseBatchError{Index: i, Result: result, Err: err}
		}
		out[i] = result
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// RetainedGridBytes is the logical copied float32 vector payload, excluding
// headers, matrices, allocator/runtime overhead and concurrent batch outputs.
func (p *PreparedInverse) RetainedGridBytes() uint64 {
	if p == nil {
		return 0
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.retainedGridBytes
}

// Close waits for active operations, drops owned storage and is idempotent.
// Waiting for a caller-supplied context implementation is not a time bound.
func (p *PreparedInverse) Close() error {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.registration = nil
	p.constants = inverseConstants{}
	p.retainedGridBytes = 0
	return nil
}
