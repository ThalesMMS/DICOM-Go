package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeferredVolumePixelsPlanWithoutIOAndMatchResidentSamples(t *testing.T) {
	for _, bits := range []int{8, 16} {
		for _, progressive := range []bool{false, true} {
			t.Run(fmt.Sprintf("bits%d/progressive%v", bits, progressive), func(t *testing.T) {
				resident := gradientXZStack(4, 5, 8)
				if bits == 16 {
					for _, f := range resident.Frames {
						data := make([]byte, len(f.PixelBytes)*2)
						for i, x := range f.PixelBytes {
							binary.LittleEndian.PutUint16(data[2*i:], uint16(x))
						}
						f.PixelBytes = data
						f.Metadata.BitsAllocated, f.Metadata.BitsStored, f.Metadata.HighBit = 16, 16, 15
					}
				}
				deferred := &Stack{PixelSpacing: resident.PixelSpacing, SliceThickness: 1}
				counts := make([]atomic.Int32, len(resident.Frames))
				for i, original := range resident.Frames {
					f := *original
					f.PixelBytes = nil
					f.LoadPixels = func(context.Context) (*Frame, error) { counts[i].Add(1); return original, nil }
					deferred.Frames = append(deferred.Frames, &f)
				}
				t.Cleanup(func() { _ = deferred.Close(); _ = resident.Close() })
				volume, err := deferred.Volume()
				if err != nil {
					t.Fatal(err)
				}
				if _, err := volume.EstimateDirectPreparation(); err != nil {
					t.Fatal(err)
				}
				for i := range counts {
					if counts[i].Load() != 0 {
						t.Fatal("metadata planning performed IO")
					}
				}
				if progressive {
					_, err = volume.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, nil)
				} else {
					_, err = volume.PrepareSnapshotContext(context.Background())
				}
				if err != nil {
					t.Fatal(err)
				}
				lease, err := volume.AcquireSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				defer lease.Release()
				reference, err := resident.Volume()
				if err != nil {
					t.Fatal(err)
				}
				want, err := reference.AcquireSnapshot()
				if err != nil {
					t.Fatal(err)
				}
				defer want.Release()
				gotSnapshot, err := lease.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				wantSnapshot, err := want.Snapshot()
				if err != nil {
					t.Fatal(err)
				}
				var gotBytes, wantBytes bytes.Buffer
				if err := gotSnapshot.WriteModalityF32To(&gotBytes); err != nil {
					t.Fatal(err)
				}
				if err := wantSnapshot.WriteModalityF32To(&wantBytes); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(gotBytes.Bytes(), wantBytes.Bytes()) {
					t.Fatal("deferred pixels changed canonical samples")
				}
				for i := range counts {
					if counts[i].Load() != 1 {
						t.Fatalf("frame %d loaded %d times", i, counts[i].Load())
					}
				}
			})
		}
	}
}

func TestDeferredPixelsPropagateCancellationAndRefuseChangedMetadata(t *testing.T) {
	frame := volumeTestFrame(2, 2, []byte{1, 2, 3, 4}, 0)
	planned := *frame
	planned.PixelBytes = nil
	planned.LoadPixels = func(ctx context.Context) (*Frame, error) { <-ctx.Done(); return nil, ctx.Err() }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := loadFrameContext(ctx, &planned); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	planned.LoadPixels = func(context.Context) (*Frame, error) {
		changed := *frame
		changed.Metadata.Columns++
		return &changed, nil
	}
	if _, err := loadFrameContext(context.Background(), &planned); err == nil {
		t.Fatal("changed geometry accepted")
	}
}

func TestDeferredVolumeDecodeCancelsBlockedSourceAndRetries(t *testing.T) {
	stack := gradientXZStack(4, 4, 3)
	t.Cleanup(func() { _ = stack.Close() })
	started := make(chan struct{})
	var first atomic.Bool
	original := *stack.Frames[0]
	stack.Frames[0].PixelBytes = nil
	stack.Frames[0].LoadPixels = func(ctx context.Context) (*Frame, error) {
		if first.CompareAndSwap(false, true) {
			close(started)
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &original, nil
	}
	volume, err := stack.Volume()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, err := volume.PrepareSnapshotContext(ctx); done <- err }()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("source was not requested")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked IO ignored cancellation")
	}
	if volume.VolumeStoreStats().LiveBytes != 0 {
		t.Fatal("cancelled source published a volume")
	}
	if _, err := volume.PrepareSnapshotContext(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestDeferredFramePreservesHeadlessDisplayWindowAndProbe(t *testing.T) {
	resident := volumeTestFrame(2, 2, []byte{10, 20, 30, 40}, 0)
	deferred := *resident
	deferred.PixelBytes = nil
	deferred.LoadPixels = func(context.Context) (*Frame, error) { return resident, nil }
	window := WindowLevel{Center: 25.5, Width: 31}
	want, err := RenderFrame(resident, window)
	if err != nil {
		t.Fatal(err)
	}
	got, err := RenderFrame(&deferred, window)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatal("headless display changed", err)
	}
	a, aok := AutoWindowWith(resident, AutoWindowOptions{})
	b, bok := AutoWindowWith(&deferred, AutoWindowOptions{})
	if a != b || aok != bok {
		t.Fatal("automatic window changed")
	}
	x, xok := PixelValueAt(resident, 1, 1)
	y, yok := PixelValueAt(&deferred, 1, 1)
	if x != y || xok != yok {
		t.Fatal("probe changed")
	}
}
