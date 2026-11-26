package render

import "testing"

func TestRenderCacheIdentitySharesImmutableAdaptersAndPins(t *testing.T) {
	cache := NewRenderCache(1024)
	first := renderCacheTestFrame([]byte{64, 128})
	first.CacheIdentity = &FrameIdentity{}
	adapter := *first
	window := WindowLevel{Center: 128, Width: 256}
	calls := 0
	cache.decodeFrame = func(frame *Frame) (*decodedFrame, error) { calls++; return decodeCacheFrame(frame) }
	original, err := cache.RenderFrame(first, window)
	if err != nil {
		t.Fatal(err)
	}
	reused, err := cache.RenderFrame(&adapter, window)
	if err != nil {
		t.Fatal(err)
	}
	if original != reused || calls != 1 {
		t.Fatalf("adapter lost window/decode identity: same=%v calls=%d", original == reused, calls)
	}
	if _, err := cache.RenderFrame(&adapter, WindowLevel{Center: 32, Width: 64}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("window-only change decoded again")
	}
	cache.SetPinnedFrames([]PinnedFrame{{Frame: &adapter, Window: window}})
	cache.EvictNonPinned()
	if !cache.ContainsFrame(first, window) {
		t.Fatal("recreated adapter did not pin original identity")
	}
	generation := adapter
	generation.CacheIdentity = &FrameIdentity{}
	generation.PixelBytes = []byte{192, 128}
	changed, err := cache.RenderFrame(&generation, window)
	if err != nil {
		t.Fatal(err)
	}
	if changed == original || calls != 2 {
		t.Fatal("new generation reused stale entry")
	}
	cache.SetPinnedFrames(nil)
	cache.EvictNonPinned()
	stats := cache.Stats()
	if stats.DecodedBytes != 0 || stats.WindowBytes != 0 || stats.PinnedBytes != 0 {
		t.Fatalf("residency survived eviction: %+v", stats)
	}
}

func TestRenderCacheClearRejectsInFlightGeneration(t *testing.T) {
	cache := NewRenderCache(1024)
	frame := renderCacheTestFrame([]byte{64})
	frame.CacheIdentity = &FrameIdentity{}
	started, release, finished := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	cache.decodeFrame = func(frame *Frame) (*decodedFrame, error) { close(started); <-release; return decodeCacheFrame(frame) }
	go func() { _, err := cache.RenderFrame(frame, WindowLevel{Center: 128, Width: 256}); finished <- err }()
	<-started
	cache.Clear()
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if stats := cache.Stats(); stats.DecodedBytes != 0 || stats.WindowBytes != 0 {
		t.Fatalf("retired render repopulated cleared source generation: %+v", stats)
	}
	cache.decodeFrame = decodeCacheFrame
	if _, err := cache.RenderFrame(frame, WindowLevel{Center: 128, Width: 256}); err != nil {
		t.Fatal(err)
	}
	if stats := cache.Stats(); stats.DecodedBytes == 0 || stats.WindowBytes == 0 {
		t.Fatalf("fresh generation not admitted: %+v", stats)
	}
}
