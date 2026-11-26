// Package parallel is the process-wide concurrency budget shared by the
// module's CPU-heavy kernels (image filters, registration, mesh extraction
// and rendering) (#1188).
//
// Every parallel call runs on its calling goroutine plus helper goroutines
// reserved from one budget. The budget bounds the helpers of all concurrent
// calls together, so an N4 bias correction and a volume render running at
// the same time share the cores instead of each spawning one goroutine per
// core. Reservation never blocks: a call that finds the budget exhausted
// runs on its calling goroutine alone, so nested or concurrent calls cannot
// deadlock and every call always makes progress. Helpers return their slot
// as soon as they run out of work, so a later call can use it.
//
// The budget defaults to GOMAXPROCS-1 helpers, tracking GOMAXPROCS when it
// changes: a lone call then runs on GOMAXPROCS goroutines, as the kernels did
// before the budget existed. Work is claimed dynamically in chunks from a
// shared cursor (as render's row scheduler does), so the result of a kernel
// that writes disjoint output does not depend on how many workers it got.
package parallel

import (
	"context"
	"runtime"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// explicitBudget is the helper budget set by SetBudget; negative
	// selects the GOMAXPROCS-1 default.
	explicitBudget atomic.Int64
	// inUse counts the helper slots currently reserved.
	inUse atomic.Int64
	// peak is the largest inUse observed since ResetPeak.
	peak atomic.Int64
)

func init() { explicitBudget.Store(-1) }

// Budget returns the number of helper goroutines all concurrent calls may
// run together.
func Budget() int {
	if b := explicitBudget.Load(); b >= 0 {
		return int(b)
	}
	return max(0, runtime.GOMAXPROCS(0)-1)
}

// SetBudget sets the helper budget and returns the previous setting (-1
// for the default). A negative n restores the GOMAXPROCS-1 default; zero
// runs every call on its calling goroutine alone. Slots already reserved
// are kept until their helpers finish.
func SetBudget(n int) int {
	if n < 0 {
		n = -1
	}
	return int(explicitBudget.Swap(int64(n)))
}

// DefaultWorkers is the worker count a kernel asks for when its caller does
// not bound it: the calling goroutine plus the whole budget.
func DefaultWorkers() int { return Budget() + 1 }

// Workers resolves a caller's worker bound: requested when positive,
// DefaultWorkers otherwise.
func Workers(requested int) int {
	if requested > 0 {
		return requested
	}
	return DefaultWorkers()
}

// InUse returns the helper slots currently reserved.
func InUse() int { return int(inUse.Load()) }

// Peak returns the most helper slots reserved at once since the last
// ResetPeak; it never exceeds the budget in force when they were reserved.
func Peak() int { return int(peak.Load()) }

// ResetPeak restarts Peak from the slots currently reserved.
func ResetPeak() { peak.Store(inUse.Load()) }

// tryReserve reserves up to n helper slots without waiting and returns how
// many it got.
func tryReserve(n int) int {
	if n <= 0 {
		return 0
	}
	for {
		used := inUse.Load()
		free := int64(Budget()) - used
		if free <= 0 {
			return 0
		}
		got := min(int64(n), free)
		if inUse.CompareAndSwap(used, used+got) {
			for {
				p := peak.Load()
				if used+got <= p || peak.CompareAndSwap(p, used+got) {
					break
				}
			}
			return int(got)
		}
	}
}

func release(n int) {
	if n > 0 {
		inUse.Add(-int64(n))
	}
}

// Reserve reserves helper slots for a call that wants up to want workers in
// total, its calling goroutine included. It returns the number of workers
// granted (at least 1: the caller) and a function returning the helpers'
// slots, which the caller must call exactly once when they have stopped.
// Use it to size a fixed pool of goroutines; Run and Do are simpler when the
// work is a list of independent items.
func Reserve(want int) (workers int, done func()) {
	helpers := tryReserve(want - 1)
	var once sync.Once
	return helpers + 1, func() { once.Do(func() { release(helpers) }) }
}

// Run runs fn for every item in [0, count) on the calling goroutine plus up
// to workers-1 helpers from the budget (workers <= 0 selects
// DefaultWorkers). Workers claim chunk consecutive items at a time from a
// shared cursor (chunk <= 0 selects about eight chunks per worker);
// newWorker is called once by each worker that claims items and returns its
// item function, so per-worker scratch needs no locking. A nil ctx disables
// cancellation; otherwise workers stop claiming items once ctx is done and
// Run returns ctx.Err() after every worker has stopped.
func Run(ctx context.Context, count, workers, chunk int, newWorker func() func(int)) error {
	return run(ctx, count, workers, chunk, newWorker, false)
}

// RunBackground is Run for work nobody is waiting on, such as preparing a
// volume ahead of its first view (#1613). While interactive work is marked
// (BeginInteractive), and for a short quiet period after it, its helpers
// return their slots to the budget and only the calling goroutine continues;
// it takes helpers again once interaction is quiet. The calling goroutine
// never pauses, since interactive work may itself be waiting for the result.
func RunBackground(ctx context.Context, count, workers, chunk int, newWorker func() func(int)) error {
	return run(ctx, count, workers, chunk, newWorker, true)
}

func run(ctx context.Context, count, workers, chunk int, newWorker func() func(int), background bool) error {
	if count <= 0 {
		return ctxErr(ctx)
	}
	workers = max(1, min(Workers(workers), count))
	if chunk <= 0 {
		chunk = max(1, count/(workers*8))
	}
	chunks := (count + chunk - 1) / chunk
	wanted := min(workers, chunks) - 1
	var next atomic.Int64
	// claim runs the next chunk and reports whether one was left.
	claim := func(do *func(int)) bool {
		end := int(next.Add(int64(chunk)))
		start := end - chunk
		if start >= count {
			return false
		}
		if *do == nil {
			*do = newWorker()
		}
		for item := start; item < min(end, count); item++ {
			if ctx != nil && ctx.Err() != nil {
				return false
			}
			(*do)(item)
		}
		return true
	}
	var wg sync.WaitGroup
	var running atomic.Int64
	helper := func() {
		defer wg.Done()
		defer release(1)
		defer running.Add(-1)
		var do func(int)
		for ctx == nil || ctx.Err() == nil {
			if background && Interactive() || !claim(&do) {
				return
			}
		}
	}
	// Only the calling goroutine spawns helpers, so running cannot grow
	// behind its back.
	spawn := func() {
		if background && Interactive() {
			return
		}
		got := tryReserve(wanted - int(running.Load()))
		running.Add(int64(got))
		wg.Add(got)
		for i := 0; i < got; i++ {
			go helper()
		}
	}
	spawn()
	var do func(int)
	for ctx == nil || ctx.Err() == nil {
		if background && int(running.Load()) < wanted {
			spawn()
		}
		if !claim(&do) {
			break
		}
	}
	wg.Wait()
	return ctxErr(ctx)
}

// Do runs fn for every item in [0, count) like Run, with one item per claim
// and no per-worker state.
func Do(ctx context.Context, count, workers int, fn func(item int)) error {
	return Run(ctx, count, workers, 1, func() func(int) { return fn })
}

// DoBackground runs fn for every item like Do, yielding to interactive work
// like RunBackground.
func DoBackground(ctx context.Context, count, workers int, fn func(item int)) error {
	return RunBackground(ctx, count, workers, 1, func() func(int) { return fn })
}

func ctxErr(ctx context.Context) error {
	if ctx == nil {
		return nil
	}
	return ctx.Err()
}

// interactiveQuiet is how long background work keeps yielding after the last
// interactive work ended, so the gaps between the steps of one gesture do not
// hand the cores back.
const interactiveQuiet = 150 * time.Millisecond

var (
	// interactiveHolds counts interactive work in progress.
	interactiveHolds atomic.Int64
	// interactiveUntil is when the quiet period after the last interactive
	// work ends, in Unix nanoseconds.
	interactiveUntil atomic.Int64
)

// BeginInteractive marks interactive work, such as rendering the image a user
// is waiting for, until the returned end function is called (any further
// calls do nothing). Background calls yield their helpers meanwhile.
func BeginInteractive() (end func()) {
	interactiveHolds.Add(1)
	var once sync.Once
	return func() {
		once.Do(func() {
			interactiveUntil.Store(time.Now().Add(interactiveQuiet).UnixNano())
			interactiveHolds.Add(-1)
		})
	}
}

// Interactive reports whether interactive work is in progress or ended less
// than the quiet period ago.
func Interactive() bool {
	return interactiveHolds.Load() > 0 || time.Now().UnixNano() < interactiveUntil.Load()
}
