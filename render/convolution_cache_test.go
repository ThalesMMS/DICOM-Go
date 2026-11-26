package render

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestSourceConvolutionCacheRewindowDoesNotLoadOrRecompute(t *testing.T) {
	frame := volumeTestFrame(3, 3, []byte{0, 1, 2, 3, 128, 5, 6, 7, 8}, 0)
	frame.CacheIdentity = &FrameIdentity{}
	kernel := SourceKernel3x3{Weights: [9]float64{1, 1, 1, 1, 1, 1, 1, 1, 1}, Divisor: 9}
	cache := NewSourceConvolutionCache(1024)
	ctx := context.Background()
	prepared, err := cache.PrepareContext(ctx, frame, kernel)
	if err != nil {
		t.Fatal(err)
	}
	var first, second uint32
	for width, dest := range map[float64]*uint32{64: &first, 512: &second} {
		id := Frame{CacheIdentity: frame.CacheIdentity, FrameIndex: frame.FrameIndex}
		resident, ok := cache.Lookup(&id, kernel)
		if !ok || resident != prepared {
			t.Fatal("resident lookup needed original pixels")
		}
		img, err := resident.RenderContext(ctx, WindowLevel{Center: 32, Width: width})
		if err != nil {
			t.Fatal(err)
		}
		*dest, _, _, _ = img.At(1, 1).RGBA()
	}
	if first == second || cache.Stats().Computations != 1 {
		t.Fatalf("window did not re-map cached samples: %d %d, stats %+v", first, second, cache.Stats())
	}
	otherIndex := *frame
	otherIndex.FrameIndex++
	if _, hit := cache.Lookup(&otherIndex, kernel); hit {
		t.Fatal("lookup accepted another physical frame")
	}
	otherIdentity := *frame
	otherIdentity.CacheIdentity = &FrameIdentity{}
	if _, hit := cache.Lookup(&otherIdentity, kernel); hit {
		t.Fatal("lookup accepted changed source identity")
	}
	otherKernel := kernel
	otherKernel.MidrangeBias = true
	if _, hit := cache.Lookup(frame, otherKernel); hit {
		t.Fatal("lookup accepted another kernel")
	}
}

func TestSourceConvolutionCacheConcurrentCancelClearAndBudget(t *testing.T) {
	ctx := context.Background()
	pixelFrame := volumeTestFrame(3, 3, []byte{0, 1, 2, 3, 128, 5, 6, 7, 8}, 0)
	frame := *pixelFrame
	frame.CacheIdentity = &FrameIdentity{}
	frame.PixelBytes = nil
	var loads atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	frame.LoadPixels = func(ctx context.Context) (*Frame, error) {
		if loads.Add(1) == 1 {
			close(started)
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-release:
			}
		}
		return pixelFrame, nil
	}
	cache := NewSourceConvolutionCache(80)
	kernel := SourceKernel3x3{Weights: [9]float64{4: 1}, Divisor: 1}
	near, cancel := context.WithCancel(ctx)
	defer cancel()
	producerErr := make(chan error, 1)
	go func() {
		_, err := cache.PrepareContext(near, &frame, kernel)
		producerErr <- err
	}()
	<-started
	cache.Clear()
	cancel()
	if err := <-producerErr; !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled prepare: %v", err)
	}
	if _, hit := cache.Lookup(&frame, kernel); hit {
		t.Fatal("canceled work populated the cache")
	}
	close(release)
	if _, err := cache.PrepareContext(ctx, &frame, kernel); err != nil {
		t.Fatal(err)
	}
	next := frame
	next.CacheIdentity = &FrameIdentity{}
	if _, err := cache.PrepareContext(ctx, &next, kernel); err != nil {
		t.Fatal(err)
	}
	if stats := cache.Stats(); stats.RetainedBytes != 72 || stats.Entries != 1 {
		t.Fatalf("wrong bounded occupancy %+v", stats)
	}
	if _, ok := cache.Lookup(&frame, kernel); ok {
		t.Fatal("the older result survived byte eviction")
	}
	if _, err := NewSourceConvolutionCache(71).PrepareContext(ctx, &frame, kernel); !errors.Is(err, ErrSourceConvolutionLimit) {
		t.Fatalf("overbudget frame: %v", err)
	}
}

func TestSourceConvolutionCacheClearDoesNotRepopulate(t *testing.T) {
	ctx := context.Background()
	frame := volumeTestFrame(2, 2, []byte{1, 2, 3, 4}, 0)
	deferred := *frame
	deferred.CacheIdentity = &FrameIdentity{}
	deferred.PixelBytes = nil
	started, release := make(chan struct{}), make(chan struct{})
	deferred.LoadPixels = func(context.Context) (*Frame, error) {
		close(started)
		<-release
		return frame, nil
	}
	var cache SourceConvolutionCache
	kernel := SourceKernel3x3{Weights: [9]float64{4: 1}, Divisor: 1}
	done := make(chan error, 1)
	go func() {
		_, err := cache.PrepareContext(ctx, &deferred, kernel)
		done <- err
	}()
	<-started
	cache.Clear()
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if stats := cache.Stats(); stats.Entries != 0 || stats.RetainedBytes != 0 {
		t.Fatalf("stale generation retained data: %+v", stats)
	}
}
