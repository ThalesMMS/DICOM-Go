package parallel

import (
	"context"
	"errors"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// withBudget sets the helper budget for one test and restores the previous
// setting.
func withBudget(t testing.TB, n int) {
	t.Helper()
	previous := SetBudget(n)
	t.Cleanup(func() { SetBudget(previous) })
}

func TestRunVisitsEveryItemOnce(t *testing.T) {
	for _, budget := range []int{0, 1, 3, 16} {
		withBudget(t, budget)
		for _, workers := range []int{0, 1, 2, 5, 64} {
			for _, chunk := range []int{0, 1, 7} {
				for _, count := range []int{0, 1, 31, 100, 1025} {
					counts := make([]atomic.Int32, count)
					if err := Run(context.Background(), count, workers, chunk, func() func(int) {
						return func(i int) { counts[i].Add(1) }
					}); err != nil {
						t.Fatal(err)
					}
					for i := range counts {
						if got := counts[i].Load(); got != 1 {
							t.Fatalf("budget %d workers %d chunk %d count %d: item %d visited %d times", budget, workers, chunk, count, i, got)
						}
					}
					if InUse() != 0 {
						t.Fatalf("%d helper slots still reserved after Run", InUse())
					}
				}
			}
		}
	}
}

func TestDefaultBudgetTracksGOMAXPROCS(t *testing.T) {
	withBudget(t, -1)
	original := runtime.GOMAXPROCS(3)
	defer runtime.GOMAXPROCS(original)
	if Budget() != 2 || DefaultWorkers() != 3 || Workers(0) != 3 || Workers(5) != 5 {
		t.Fatalf("budget %d, default workers %d, Workers(0) %d, Workers(5) %d with GOMAXPROCS 3", Budget(), DefaultWorkers(), Workers(0), Workers(5))
	}
	runtime.GOMAXPROCS(1)
	if Budget() != 0 || DefaultWorkers() != 1 {
		t.Fatalf("budget %d with GOMAXPROCS 1", Budget())
	}
	if previous := SetBudget(4); previous != -1 || Budget() != 4 {
		t.Fatalf("SetBudget returned %d, budget %d", previous, Budget())
	}
}

// TestRunWithoutBudgetUsesTheCallingGoroutine: with no helper slot a call
// runs on its own goroutine, with one worker state.
func TestRunWithoutBudgetUsesTheCallingGoroutine(t *testing.T) {
	withBudget(t, 0)
	var states atomic.Int32
	if err := Run(context.Background(), 1000, 8, 1, func() func(int) {
		states.Add(1)
		return func(int) {}
	}); err != nil {
		t.Fatal(err)
	}
	if states.Load() != 1 {
		t.Fatalf("%d workers ran with an empty budget", states.Load())
	}
}

// TestConcurrentCallsShareTheBudget runs many concurrent calls that each
// ask for every core; the helpers running at once never exceed the budget,
// and every call finishes.
func TestConcurrentCallsShareTheBudget(t *testing.T) {
	const budget = 3
	withBudget(t, budget)
	ResetPeak()
	var running, maxRunning atomic.Int32
	var wg sync.WaitGroup
	const calls = 6
	for c := 0; c < calls; c++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Do(context.Background(), 64, 64, func(int) {
				n := running.Add(1)
				for {
					m := maxRunning.Load()
					if n <= m || maxRunning.CompareAndSwap(m, n) {
						break
					}
				}
				time.Sleep(200 * time.Microsecond)
				running.Add(-1)
			})
		}()
	}
	wg.Wait()
	if Peak() > budget || Peak() == 0 {
		t.Fatalf("peak %d helper slots, budget %d", Peak(), budget)
	}
	// Each call runs on its calling goroutine plus helpers.
	if got := int(maxRunning.Load()); got > budget+calls {
		t.Fatalf("%d items ran at once, more than %d helpers plus %d callers", got, budget, calls)
	}
	if InUse() != 0 {
		t.Fatalf("%d helper slots still reserved", InUse())
	}
}

// TestNestedCallsDoNotDeadlock: an item that itself runs a parallel call
// finds the budget taken and runs inline.
func TestNestedCallsDoNotDeadlock(t *testing.T) {
	withBudget(t, 2)
	var total atomic.Int64
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = Do(context.Background(), 16, 0, func(int) {
			_ = Do(context.Background(), 16, 0, func(int) { total.Add(1) })
		})
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("nested parallel calls deadlocked")
	}
	if total.Load() != 256 {
		t.Fatalf("%d inner items ran, want 256", total.Load())
	}
}

func TestReserveIsBoundedAndReleasedOnce(t *testing.T) {
	withBudget(t, 2)
	workers, done := Reserve(10)
	if workers != 3 || InUse() != 2 {
		t.Fatalf("Reserve(10) granted %d workers with %d slots in use", workers, InUse())
	}
	other, otherDone := Reserve(4)
	if other != 1 {
		t.Fatalf("second Reserve granted %d workers from an exhausted budget", other)
	}
	otherDone()
	done()
	done()
	if InUse() != 0 {
		t.Fatalf("%d slots in use after release", InUse())
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	withBudget(t, 3)
	ctx, cancel := context.WithCancel(context.Background())
	var ran atomic.Int32
	err := Do(ctx, 10000, 4, func(int) {
		if ran.Add(1) == 10 {
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v, want context.Canceled", err)
	}
	if ran.Load() > 100 {
		t.Fatalf("%d items ran after cancellation", ran.Load())
	}
	if err := Do(nil, 4, 2, func(int) {}); err != nil {
		t.Fatal(err)
	}
}

func BenchmarkRunOverhead(b *testing.B) {
	data := make([]float64, 1<<16)
	for i := 0; i < b.N; i++ {
		_ = Run(context.Background(), len(data), 0, 0, func() func(int) {
			return func(j int) { data[j] += 1 }
		})
	}
}

// Background work hands its helpers back while interactive work runs and
// still visits every item exactly once (#1613).
func TestBackgroundYieldsHelpersToInteractiveWork(t *testing.T) {
	withBudget(t, 3)
	const count = 400
	visits := make([]atomic.Int32, count)
	started := make(chan struct{})
	var once sync.Once
	var end func()
	done := make(chan error, 1)
	go func() {
		done <- DoBackground(context.Background(), count, 4, func(item int) {
			once.Do(func() {
				end = BeginInteractive()
				close(started)
			})
			visits[item].Add(1)
			time.Sleep(100 * time.Microsecond)
		})
	}()
	<-started
	deadline := time.Now().Add(time.Second)
	for InUse() != 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if InUse() != 0 {
		t.Fatalf("background work kept %d helper slots during interactive work", InUse())
	}
	// An interactive call meanwhile gets the whole budget.
	if workers, release := Reserve(4); workers != 4 {
		release()
		t.Fatalf("interactive call got %d workers, want 4", workers)
	} else {
		release()
	}
	end()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for item := range visits {
		if got := visits[item].Load(); got != 1 {
			t.Fatalf("item %d visited %d times", item, got)
		}
	}
	if InUse() != 0 {
		t.Fatalf("%d slots in use after background work", InUse())
	}
}

// Interactive work that waits for background work, such as a render that
// needs a volume still decoding, does not wait forever: the background
// caller keeps working alone while helpers yield (#1613).
func TestBackgroundAwaitedByInteractiveWorkCompletes(t *testing.T) {
	withBudget(t, 3)
	end := BeginInteractive()
	defer end()
	var ran atomic.Int32
	done := make(chan error, 1)
	go func() {
		done <- DoBackground(context.Background(), 64, 4, func(int) {
			ran.Add(1)
			if InUse() != 0 {
				t.Error("background helpers ran during interactive work")
			}
		})
	}()
	select {
	case err := <-done:
		if err != nil || ran.Load() != 64 {
			t.Fatalf("ran %d items, err %v", ran.Load(), err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("background work awaited by interactive work did not finish")
	}
}

// Without interactive work, background calls take helpers like Run.
func TestBackgroundUsesHelpersWhenQuiet(t *testing.T) {
	withBudget(t, 3)
	for Interactive() {
		time.Sleep(10 * time.Millisecond)
	}
	ResetPeak()
	var ran atomic.Int32
	if err := DoBackground(context.Background(), 64, 4, func(int) {
		ran.Add(1)
		time.Sleep(200 * time.Microsecond)
	}); err != nil {
		t.Fatal(err)
	}
	if ran.Load() != 64 || Peak() == 0 {
		t.Fatalf("ran %d items with peak %d helpers, want 64 items with helpers", ran.Load(), Peak())
	}
}

// BenchmarkInteractiveUnderBackgroundLoad times an interactive call while a
// background call keeps every helper busy, with the background work taking
// helpers as Do does (not_yielding) or yielding them as DoBackground does
// (yielding) (#1613).
func BenchmarkInteractiveUnderBackgroundLoad(b *testing.B) {
	spin := func(d time.Duration) {
		for start := time.Now(); time.Since(start) < d; {
		}
	}
	for _, mode := range []string{"not_yielding", "yielding"} {
		b.Run(mode, func(b *testing.B) {
			ctx, cancel := context.WithCancel(context.Background())
			background := Do
			if mode == "yielding" {
				background = DoBackground
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = background(ctx, 1<<30, 0, func(int) { spin(500 * time.Microsecond) })
			}()
			time.Sleep(20 * time.Millisecond)
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				end := BeginInteractive()
				_ = Do(context.Background(), 64, 0, func(int) { spin(100 * time.Microsecond) })
				end()
			}
			b.StopTimer()
			cancel()
			<-done
			for Interactive() {
				time.Sleep(10 * time.Millisecond)
			}
		})
	}
}
