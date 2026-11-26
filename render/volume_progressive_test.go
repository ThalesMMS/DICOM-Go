package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

// snapshotBytes returns the modality float32 stream and the raw payload of
// v's canonical generation.
func snapshotBytes(t *testing.T, v *Volume) (modality, payload []byte, descriptor VolumeDescriptor) {
	t.Helper()
	lease, err := v.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	var m, p bytes.Buffer
	if err := snapshot.WriteModalityF32To(&m); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.WritePayloadTo(&p); err != nil {
		t.Fatal(err)
	}
	descriptor, _ = snapshot.Descriptor()
	return m.Bytes(), p.Bytes(), descriptor
}

func TestProgressiveCancellationAtFinalBandDoesNotInstallCanonicalGeneration(t *testing.T) {
	for name, build := range map[string]func() *Stack{
		"stored16": func() *Stack {
			return stack16(5, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
		},
		"float32": func() *Stack { return sphereStack(5, 2) },
	} {
		t.Run(name, func(t *testing.T) {
			volume, err := BuildVolume(build())
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			_, err = volume.PrepareSnapshotProgressiveWithOptionsContext(ctx, VolumeDecodeOptions{SkipPreview: true}, func(event VolumeDecodeProgress) {
				if event.Phase == VolumeDecodeBand && event.BandEnd == volume.Depth {
					cancel()
				}
			})
			if !errors.Is(err, context.Canceled) || volume.SnapshotReady() || volume.VolumeStoreStats().LiveBytes != 0 {
				t.Fatal("cancelled final band installed source", err)
			}
			if _, err := volume.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, func(VolumeDecodeProgress) {}); err != nil {
				t.Fatal(err)
			}
			_, _, d := snapshotBytes(t, volume)
			if d.VolumeGeneration != 1 {
				t.Fatal("cancelled installation consumed generation", d.VolumeGeneration)
			}
		})
	}
}

// TestPrepareSnapshotProgressive (#1230): the even slices decode first into
// a 2× preview, full-resolution z-bands are reported in order as they
// complete, and the installed generation equals the one-pass build byte for
// byte, for packed 16-bit and float32 stacks.
func TestPrepareSnapshotProgressive(t *testing.T) {
	const n = 13
	stacks := map[string]func() *Stack{
		"packed int16": func() *Stack {
			return stack16(n, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
		},
		"float32": func() *Stack { return sphereStack(n, 4) },
	}
	for name, build := range stacks {
		t.Run(name, func(t *testing.T) {
			reference, err := BuildVolume(build())
			if err != nil {
				t.Fatal(err)
			}
			defer reference.Close()
			if _, err := reference.PrepareSnapshotContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantModality, wantPayload, wantDescriptor := snapshotBytes(t, reference)

			v, err := BuildVolume(build())
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var events []VolumeDecodeProgress
			var bandModality, bandStored bytes.Buffer
			var preview *Volume
			_, err = v.PrepareSnapshotProgressiveContext(context.Background(), func(event VolumeDecodeProgress) {
				if v.SnapshotReady() && event.Phase != VolumeDecodeComplete {
					t.Errorf("snapshot ready during %d", event.Phase)
				}
				switch event.Phase {
				case VolumeDecodePreview:
					preview = event.Preview
				case VolumeDecodeBand:
					if err := event.Band.WriteModalityF32To(&bandModality); err != nil {
						t.Error(err)
					}
					if event.Descriptor.ScalarFormat != VolumeScalarF32ModalityLE {
						if err := event.Band.WriteStored16To(&bandStored); err != nil {
							t.Error(err)
						}
					}
				}
				event.Band = VolumeBandReader{}
				event.Preview = nil
				events = append(events, event)
			})
			if err != nil {
				t.Fatal(err)
			}
			if !v.SnapshotReady() {
				t.Fatal("snapshot not ready after completion")
			}
			// The preview follows the even slices; the bands cover every slice
			// once, in z order; the completion is last.
			if events[0].Phase != VolumeDecodePreview || events[0].DecodedSlices != (n+1)/2 {
				t.Fatalf("first event = %+v", events[0])
			}
			next := 0
			for _, event := range events[1 : len(events)-1] {
				if event.Phase != VolumeDecodeBand || event.BandStart != next || event.BandEnd <= next ||
					(event.BandEnd < n && event.BandEnd-event.BandStart < progressiveBandSlices) {
					t.Fatalf("band event = %+v after slice %d", event, next)
				}
				next = event.BandEnd
			}
			last := events[len(events)-1]
			if next != n || last.Phase != VolumeDecodeComplete || last.Descriptor.VolumeGeneration == 0 {
				t.Fatalf("bands end at %d, last event %+v", next, last)
			}
			modality, payload, descriptor := snapshotBytes(t, v)
			descriptor.VolumeGeneration = wantDescriptor.VolumeGeneration
			if !bytes.Equal(modality, wantModality) || !bytes.Equal(payload, wantPayload) || descriptor != wantDescriptor {
				t.Fatal("the progressive generation differs from the one-pass build")
			}
			if !bytes.Equal(bandModality.Bytes(), wantModality) {
				t.Fatal("the bands differ from the installed modality values")
			}
			if wantDescriptor.ScalarFormat != VolumeScalarF32ModalityLE && !bytes.Equal(bandStored.Bytes(), wantPayload) {
				t.Fatal("the stored bands differ from the installed payload")
			}
			last.Descriptor.VolumeGeneration = 0
			if events[0].Descriptor != last.Descriptor {
				t.Fatalf("event descriptor %+v, installed %+v", events[0].Descriptor, last.Descriptor)
			}

			// The preview averages 2×2 blocks of the even slices at their
			// patient positions.
			if preview == nil {
				t.Fatal("no preview")
			}
			defer preview.Close()
			if preview.Cols != n/2 || preview.Rows != n/2 || preview.Depth != (n+1)/2 ||
				preview.SliceSpacing != 2*v.SliceSpacing || preview.ColSpacing != 2*v.ColSpacing {
				t.Fatalf("preview %dx%dx%d spacing %v/%v", preview.Cols, preview.Rows, preview.Depth, preview.ColSpacing, preview.SliceSpacing)
			}
			for _, voxel := range [][3]int{{0, 0, 0}, {3, 2, 1}, {5, 5, 6}} {
				x, y, z := voxel[0], voxel[1], voxel[2]
				want := 0.0
				for _, d := range [][2]int{{0, 0}, {1, 0}, {0, 1}, {1, 1}} {
					value, _, _ := v.ValueAt(2*x+d[0], 2*y+d[1], 2*z)
					want += value / 4
				}
				got, _, ok := preview.ValueAt(x, y, z)
				if !ok || math.Abs(got-want) > 1e-3 {
					t.Fatalf("preview %v = %v, want %v", voxel, got, want)
				}
				at := preview.VoxelToPatient(Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				source := v.VoxelToPatient(Vec3{X: float64(2*x) + 0.5, Y: float64(2*y) + 0.5, Z: float64(2 * z)})
				if at.Sub(source).Length() > 1e-9 {
					t.Fatalf("preview voxel %v at %v, source block centre %v", voxel, at, source)
				}
			}
		})
	}
}

// TestPrepareSnapshotProgressiveReadyAndCancelled: a prepared volume reports
// only the completion; a cancelled progressive decode installs nothing and a
// later preparation succeeds.
func TestPrepareSnapshotProgressiveReadyAndCancelled(t *testing.T) {
	v, err := BuildVolume(stack16(6, true, func(int) Rescale { return Rescale{Slope: 1} }, ctValue))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ctx, cancel := context.WithCancel(context.Background())
	_, err = v.PrepareSnapshotProgressiveContext(ctx, func(event VolumeDecodeProgress) {
		if event.Phase == VolumeDecodePreview {
			event.Preview.Close()
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || v.SnapshotReady() {
		t.Fatalf("cancelled decode: err %v, ready %v", err, v.SnapshotReady())
	}
	if _, err := v.PrepareSnapshotContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	var phases []VolumeDecodePhase
	if _, err := v.PrepareSnapshotProgressiveContext(context.Background(), func(event VolumeDecodeProgress) {
		phases = append(phases, event.Phase)
	}); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0] != VolumeDecodeComplete {
		t.Fatalf("phases of a prepared volume = %v", phases)
	}
}

// BenchmarkPrepareSnapshotProgressive compares the time to the 2× preview
// and to the complete generation of a progressive decode with the one-pass
// preparation of the same 256³ int16 CT stack (#1230).
func BenchmarkPrepareSnapshotProgressive(b *testing.B) {
	stack := stack16(256, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
	b.Run("one-pass", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			v, err := BuildVolume(stack)
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			if _, err := v.PrepareSnapshotContext(context.Background()); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			v.Close()
			b.StartTimer()
		}
	})
	b.Run("progressive", func(b *testing.B) {
		var preview time.Duration
		for i := 0; i < b.N; i++ {
			b.StopTimer()
			v, err := BuildVolume(stack)
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			started := time.Now()
			if _, err := v.PrepareSnapshotProgressiveContext(context.Background(), func(event VolumeDecodeProgress) {
				if event.Phase == VolumeDecodePreview {
					preview += time.Since(started)
					event.Preview.Close()
				}
			}); err != nil {
				b.Fatal(err)
			}
			b.StopTimer()
			v.Close()
			b.StartTimer()
		}
		b.ReportMetric(float64(preview.Microseconds())/1000/float64(b.N), "preview-ms/op")
	})
}

// A nil sample store is an intentional trap: computing d.preview would panic.
// This proves SkipPreview acts before the derivation, rather than dropping its event.
func TestBandsOnlyDecoderNeverBuildsPreview(t *testing.T) {
	var phases []VolumeDecodePhase
	d := &progressiveDecoder{volume: &Volume{Cols: 2, Rows: 2, Depth: 1}, ready: make([]bool, 1), skipPreview: true, observe: func(e VolumeDecodeProgress) { phases = append(phases, e.Phase) }}
	if err := d.sliceDecoded(0); err != nil {
		t.Fatal(err)
	}
	if len(phases) != 1 || phases[0] != VolumeDecodeBand {
		t.Fatalf("phases=%v", phases)
	}
}
func TestPrepareSnapshotBandsOnlyMatchesFullStoredGeneration(t *testing.T) {
	for name, build := range map[string]func() *Stack{"packed": func() *Stack {
		return stack16(17, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
	}, "float32": func() *Stack { return sphereStack(17, 5) }} {
		t.Run(name, func(t *testing.T) {
			reference, err := BuildVolume(build())
			if err != nil {
				t.Fatal(err)
			}
			defer reference.Close()
			if _, err = reference.PrepareSnapshotContext(context.Background()); err != nil {
				t.Fatal(err)
			}
			wantModality, wantPayload, wantDescriptor := snapshotBytes(t, reference)
			v, err := BuildVolume(build())
			if err != nil {
				t.Fatal(err)
			}
			defer v.Close()
			var modality, payload bytes.Buffer
			next, complete := 0, false
			_, err = v.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, func(e VolumeDecodeProgress) {
				if e.Preview != nil || e.Phase == VolumeDecodePreview {
					t.Fatal("CPU-derived preview computed/published")
				}
				if e.Phase == VolumeDecodeBand {
					if e.BandStart != next || e.BandEnd <= next {
						t.Fatalf("noncontiguous band %d:%d after%d", e.BandStart, e.BandEnd, next)
					}
					next = e.BandEnd
					if err := e.Band.WriteModalityF32To(&modality); err != nil {
						t.Fatal(err)
					}
					if e.Descriptor.ScalarFormat != VolumeScalarF32ModalityLE {
						if err := e.Band.WriteStored16To(&payload); err != nil {
							t.Fatal(err)
						}
					}
				} else if e.Phase == VolumeDecodeComplete {
					complete = true
					if e.Descriptor.VolumeGeneration == 0 {
						t.Fatal("missing completed generation")
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			gotModality, gotPayload, gotDescriptor := snapshotBytes(t, v)
			if !complete || next != v.Depth || !bytes.Equal(modality.Bytes(), wantModality) || !bytes.Equal(gotModality, wantModality) || !bytes.Equal(gotPayload, wantPayload) || gotDescriptor.Dimensions != wantDescriptor.Dimensions || gotDescriptor.VolumeGeneration != wantDescriptor.VolumeGeneration {
				t.Fatal("bands-only changed full values/geometry/generation")
			}
			if gotDescriptor.ScalarFormat != VolumeScalarF32ModalityLE && !bytes.Equal(payload.Bytes(), wantPayload) {
				t.Fatal("packed band payload changed")
			}
			replay := 0
			_, err = v.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, func(e VolumeDecodeProgress) {
				replay++
				if e.Phase != VolumeDecodeComplete {
					t.Fatal("warm snapshot rebuilt bands/preview")
				}
			})
			if err != nil || replay != 1 {
				t.Fatalf("reused completion=%d err=%v", replay, err)
			}
		})
	}
}
func TestPrepareSnapshotBandsOnlyCancellationRestartsWithoutPreview(t *testing.T) {
	v, err := BuildVolume(sphereStack(17, 5))
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	bands := 0
	_, err = v.PrepareSnapshotProgressiveWithOptionsContext(ctx, VolumeDecodeOptions{SkipPreview: true}, func(e VolumeDecodeProgress) {
		if e.Phase == VolumeDecodePreview || e.Preview != nil {
			t.Fatal("preview on cancellation path")
		}
		if e.Phase == VolumeDecodeBand {
			bands++
			cancel()
		}
	})
	if !errors.Is(err, context.Canceled) || bands != 1 || v.SnapshotReady() {
		t.Fatalf("cancelled bands=%d ready=%v err=%v", bands, v.SnapshotReady(), err)
	}
	next, complete := 0, false
	_, err = v.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, func(e VolumeDecodeProgress) {
		if e.Phase == VolumeDecodePreview || e.Preview != nil {
			t.Fatal("preview on resume")
		}
		if e.Phase == VolumeDecodeBand {
			if e.BandStart != next {
				t.Fatal("resume bands not restarted")
			}
			next = e.BandEnd
		}
		complete = e.Phase == VolumeDecodeComplete
	})
	if err != nil || !complete || next != v.Depth || !v.SnapshotReady() {
		t.Fatalf("resume next=%d complete=%v err=%v", next, complete, err)
	}
}

func TestRetainedVolumeBandsRemainImmutableDuringDecodeCancelRetryAndClose(t *testing.T) {
	builders := map[string]func() *Stack{
		"i16": func() *Stack {
			return stack16(33, true, func(int) Rescale { return Rescale{Slope: 2, Intercept: -1024} }, ctValue)
		},
		"u16": func() *Stack {
			return stack16(33, false, func(int) Rescale { return Rescale{Slope: 2, Intercept: -1024} }, func(x, y, z int) int { return x*7 + y*3 + z*5 })
		},
		"f32": func() *Stack { return sphereStack(33, 10) },
	}
	for name, build := range builders {
		t.Run(name, func(t *testing.T) {
			stack := build()
			volume, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var retained RetainedVolumeBandReader
			var want, storedWant []byte
			bands := 0
			done := make(chan struct{})
			var wg sync.WaitGroup
			_, err = volume.PrepareSnapshotProgressiveWithOptionsContext(ctx, VolumeDecodeOptions{SkipPreview: true}, func(event VolumeDecodeProgress) {
				if event.Phase != VolumeDecodeBand {
					return
				}
				bands++
				if bands == 1 {
					retained, err = event.Band.Retain()
					if err != nil {
						t.Error(err)
						return
					}
					var modality, stored bytes.Buffer
					if e := event.Band.WriteModalityF32To(&modality); e != nil {
						t.Error(e)
					}
					want = append([]byte(nil), modality.Bytes()...)
					if name != "f32" {
						if e := event.Band.WriteStored16To(&stored); e != nil {
							t.Error(e)
						}
						storedWant = append([]byte(nil), stored.Bytes()...)
					}
					started := make(chan struct{})
					wg.Add(1)
					go func() {
						defer wg.Done()
						first := true
						for {
							var out bytes.Buffer
							e := retained.WriteModalityF32To(&out)
							if e != nil || !bytes.Equal(out.Bytes(), want) {
								t.Error("retained prefix changed during later-slice decode", e)
							}
							if first {
								close(started)
								first = false
							}
							select {
							case <-done:
								return
							default:
							}
						}
					}()
					<-started
				} else {
					// The first retained prefix is read while later disjoint slices decode.
					time.Sleep(time.Millisecond)
					cancel()
				}
			})
			close(done)
			wg.Wait()
			if !errors.Is(err, context.Canceled) || bands != 2 || volume.SnapshotReady() {
				t.Fatalf("cancelled decode: bands %d ready %v err %v", bands, volume.SnapshotReady(), err)
			}
			// Retry decodes different input into a fresh allocation, never the old reader.
			for _, frame := range stack.Frames {
				for i := range frame.PixelBytes {
					frame.PixelBytes[i] ^= 0x55
				}
			}
			var retry RetainedVolumeBandReader
			_, err = volume.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true}, func(event VolumeDecodeProgress) {
				if event.Phase == VolumeDecodeBand && event.BandStart == 0 {
					var e error
					retry, e = event.Band.Retain()
					if e != nil {
						t.Error(e)
					}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			var changed bytes.Buffer
			if err := retry.WriteModalityF32To(&changed); err != nil {
				t.Fatal(err)
			}
			if bytes.Equal(changed.Bytes(), want) {
				t.Fatal("retry did not observe changed inputs")
			}
			if err := volume.Close(); err != nil {
				t.Fatal(err)
			}
			volume.Cols, volume.Rows = 1, 1 // retained geometry is independent of source fields
			var after bytes.Buffer
			if err := retained.WriteModalityF32To(&after); err != nil || !bytes.Equal(after.Bytes(), want) {
				t.Fatal("retained reader changed after retry/close", err)
			}
			if name != "f32" {
				after.Reset()
				if err := retained.WriteStored16To(&after); err != nil || !bytes.Equal(after.Bytes(), storedWant) {
					t.Fatal("retained stored reader changed after retry/close", err)
				}
			}
		})
	}
}

func TestRetainedVolumeBandCapturesGeometryAndBackingHeaders(t *testing.T) {
	payload := &packed16VolumePayload{unsigned: []uint16{7, 9}}
	volume := &Volume{Cols: 2, Rows: 1}
	borrowed := VolumeBandReader{volume: volume, start: 0, end: 1, descriptor: VolumeDescriptor{RescaleSlope: 2, RescaleIntercept: -1}, packed: payload}
	retained, err := borrowed.Retain()
	if err != nil {
		t.Fatal(err)
	}
	payload.unsigned = nil
	volume.Cols, volume.Rows = 123, 456
	borrowed.descriptor.RescaleSlope = 99
	var out bytes.Buffer
	if err := retained.WriteStored16To(&out); err != nil || !bytes.Equal(out.Bytes(), []byte{7, 0, 9, 0}) {
		t.Fatal("mutable source headers reached retained reader", err)
	}
	out.Reset()
	if err := retained.WriteModalityF32To(&out); err != nil {
		t.Fatal(err)
	}
	if len(out.Bytes()) != 8 || math.Float32frombits(binary.LittleEndian.Uint32(out.Bytes()[:4])) != 13 || math.Float32frombits(binary.LittleEndian.Uint32(out.Bytes()[4:])) != 17 {
		t.Fatal("retained geometry/modality descriptor changed")
	}
	for _, reader := range []VolumeBandReader{{}, {cols: 2, rows: 1, start: 0, end: 1, float32Data: []float32{1}}} {
		if _, err := reader.Retain(); !errors.Is(err, ErrInvalidVolumeSnapshot) {
			t.Fatalf("invalid retain: %v", err)
		}
	}
	var zero RetainedVolumeBandReader
	if err := zero.WriteModalityF32To(&out); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatal("zero retained reader did not refuse", err)
	}
	if err := zero.WriteStored16To(&out); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatal("zero stored reader did not refuse", err)
	}
}

// TestPrepareSnapshotProgressiveDecodesSlicesConcurrently: a slow per-slice
// decoder (an external JPEG 2000 process) runs on several workers at once,
// while the even slices are still reported before the odd ones and the bands
// in z order, and the installed generation equals the one-pass build.
func TestPrepareSnapshotProgressiveDecodesSlicesConcurrently(t *testing.T) {
	if parallel.DefaultWorkers() < 2 {
		t.Skip("one worker")
	}
	const n = 48 // above the tiny-volume single-worker threshold
	build := func() *Stack {
		return stack16(n, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue)
	}
	reference, err := BuildVolume(build())
	if err != nil {
		t.Fatal(err)
	}
	defer reference.Close()
	if _, err := reference.PrepareSnapshotContext(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, wantPayload, _ := snapshotBytes(t, reference)

	stack := build()
	var mu sync.Mutex
	inFlight, peak := 0, 0
	var decodedOrder []int
	for z, frame := range stack.Frames {
		loaded := *frame
		deferred := *frame
		deferred.PixelBytes = nil
		deferred.LoadPixels = func(context.Context) (*Frame, error) {
			mu.Lock()
			inFlight++
			peak = max(peak, inFlight)
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			mu.Lock()
			inFlight--
			decodedOrder = append(decodedOrder, z)
			mu.Unlock()
			copy := loaded
			return &copy, nil
		}
		stack.Frames[z] = &deferred
	}
	v, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer v.Close()
	evenReported, next := false, 0
	_, err = v.PrepareSnapshotProgressiveWithOptionsContext(context.Background(), VolumeDecodeOptions{SkipPreview: true, ReportEvenSlices: true}, func(event VolumeDecodeProgress) {
		switch event.Phase {
		case VolumeDecodeEvenSlices:
			mu.Lock()
			for _, z := range decodedOrder[:event.DecodedSlices] {
				if z%2 != 0 {
					t.Errorf("odd slice %d decoded before the even slices were reported", z)
				}
			}
			mu.Unlock()
			evenReported = true
		case VolumeDecodeBand:
			if event.BandStart != next || event.BandEnd <= next {
				t.Errorf("band [%d,%d) after slice %d", event.BandStart, event.BandEnd, next)
			}
			next = event.BandEnd
		}
	})
	if err != nil {
		t.Fatal(err)
	}
	if !evenReported || next != n {
		t.Fatalf("even slices reported=%v, bands ended at %d of %d", evenReported, next, n)
	}
	if peak < 2 {
		t.Fatalf("slices decoded one at a time (peak %d)", peak)
	}
	if _, payload, _ := snapshotBytes(t, v); !bytes.Equal(payload, wantPayload) {
		t.Fatal("progressive payload differs from the one-pass build")
	}
}
