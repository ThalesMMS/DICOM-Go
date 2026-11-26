package microscopy

import (
	"context"
	"image"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoaderSupersessionRetainsPhysicalAdmissionAndCloseJoins(t *testing.T) {
	started := make(chan int, 2)
	release := make(chan struct{})
	var active, published atomic.Int32
	source := TileSourceFunc(func(ctx context.Context, tile Tile) (image.Image, error) {
		active.Add(1)
		defer active.Add(-1)
		started <- tile.FrameNumber
		// Deliberately retain the source operation after cancellation until released.
		<-release
		return image.NewRGBA(image.Rect(0, 0, 2, 2)), nil
	})
	loader := NewLoader(source, 1024, 1)
	first, err := loader.Request(context.Background(), []Tile{{FrameNumber: 1}}, func(TileResult) { published.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first source did not start")
	}
	second, err := loader.Request(context.Background(), []Tile{{FrameNumber: 2}}, func(TileResult) { published.Add(1) })
	if err != nil {
		t.Fatal(err)
	}
	closed := make(chan struct{})
	go func() { loader.Close(); close(closed) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		loader.mu.Lock()
		closing := loader.closed
		loader.mu.Unlock()
		if closing {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("close did not begin")
		}
		time.Sleep(time.Millisecond)
	}
	select {
	case <-closed:
		t.Fatal("Close returned before source retired")
	default:
	}
	if active.Load() != 1 {
		t.Fatalf("physical source count=%d", active.Load())
	}
	select {
	case frame := <-started:
		t.Fatalf("superseding source borrowed occupied slot: %d", frame)
	default:
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("close failed to join")
	}
	<-first
	<-second
	if active.Load() != 0 || published.Load() != 0 || loader.CacheStats().Entries != 0 {
		t.Fatalf("after close active=%d published=%d cache=%+v", active.Load(), published.Load(), loader.CacheStats())
	}
}
