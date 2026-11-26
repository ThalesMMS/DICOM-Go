package pixeldata

import (
	"context"
	"errors"
	"fmt"
	"sync"
)

// ErrInvalidAdmission reports a MemoryAdmission capacity or request below
// one byte.
var ErrInvalidAdmission = errors.New("dicom: invalid memory admission")

// MemoryAdmission bounds the bytes that frame decodes hold at the same time:
// a weighted semaphore that a CodecRouter acquires around each frame, sized
// by the frame's estimated working set, and that any number of routers,
// readers and goroutines may share. Requests are granted in arrival order,
// so a large frame is not starved by a stream of small ones. A request
// larger than the capacity is granted alone, once nothing else is held,
// rather than never; Stats counts those.
//
// The bytes are a plan, not a measurement: what the caller's estimate says a
// decode will hold, not the process's resident memory.
type MemoryAdmission struct {
	mu       sync.Mutex
	capacity int64
	held     int64
	waiters  []*admissionWaiter
	stats    MemoryAdmissionStats
}

type admissionWaiter struct {
	bytes   int64
	granted chan struct{}
}

// MemoryAdmissionStats is a snapshot of a MemoryAdmission.
type MemoryAdmissionStats struct {
	Capacity int64
	// Held is the bytes granted and not yet released, Peak its largest value.
	// Peak exceeds Capacity only by a request larger than Capacity, granted
	// alone.
	Held, Peak int64
	// Granted counts requests granted, Waited those that had to wait,
	// Oversized those larger than Capacity, and Canceled those whose context
	// ended while they waited.
	Granted, Waited, Oversized, Canceled int64
	// Queued is the number of requests waiting now.
	Queued int
}

// NewMemoryAdmission returns an admission of capacity bytes.
func NewMemoryAdmission(capacity int64) (*MemoryAdmission, error) {
	if capacity < 1 {
		return nil, fmt.Errorf("%w: capacity %d", ErrInvalidAdmission, capacity)
	}
	return &MemoryAdmission{capacity: capacity, stats: MemoryAdmissionStats{Capacity: capacity}}, nil
}

// Acquire waits until bytes can be held, or ctx ends, and returns the
// function that releases them; it is safe to call more than once.
func (a *MemoryAdmission) Acquire(ctx context.Context, bytes int64) (release func(), err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if bytes < 1 {
		return nil, fmt.Errorf("%w: request of %d bytes", ErrInvalidAdmission, bytes)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	a.mu.Lock()
	if len(a.waiters) == 0 && a.fits(bytes) {
		a.grant(bytes)
		a.mu.Unlock()
		return a.releaser(bytes), nil
	}
	w := &admissionWaiter{bytes: bytes, granted: make(chan struct{})}
	a.waiters = append(a.waiters, w)
	a.stats.Waited++
	a.mu.Unlock()

	select {
	case <-w.granted:
		return a.releaser(bytes), nil
	case <-ctx.Done():
		a.mu.Lock()
		select {
		case <-w.granted:
			// Granted while the context ended: give the bytes back.
			a.mu.Unlock()
			a.releaser(bytes)()
		default:
			for i, other := range a.waiters {
				if other == w {
					a.waiters = append(a.waiters[:i], a.waiters[i+1:]...)
					break
				}
			}
			a.stats.Canceled++
			// The head may have been waiting behind this request.
			a.wake()
			a.mu.Unlock()
		}
		return nil, ctx.Err()
	}
}

// Stats returns a snapshot of the admission's counters.
func (a *MemoryAdmission) Stats() MemoryAdmissionStats {
	a.mu.Lock()
	defer a.mu.Unlock()
	stats := a.stats
	stats.Held, stats.Queued = a.held, len(a.waiters)
	return stats
}

// fits reports whether bytes can be granted now: within the capacity, or
// alone when nothing is held.
func (a *MemoryAdmission) fits(bytes int64) bool {
	return a.held == 0 || bytes <= a.capacity-a.held
}

func (a *MemoryAdmission) grant(bytes int64) {
	a.held += bytes
	a.stats.Granted++
	if bytes > a.capacity {
		a.stats.Oversized++
	}
	a.stats.Peak = max(a.stats.Peak, a.held)
}

// wake grants waiting requests in order while the first one fits.
func (a *MemoryAdmission) wake() {
	for len(a.waiters) > 0 && a.fits(a.waiters[0].bytes) {
		w := a.waiters[0]
		a.waiters = a.waiters[1:]
		a.grant(w.bytes)
		close(w.granted)
	}
}

func (a *MemoryAdmission) releaser(bytes int64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			a.mu.Lock()
			a.held -= bytes
			a.wake()
			a.mu.Unlock()
		})
	}
}
