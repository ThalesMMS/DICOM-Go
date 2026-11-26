package render

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/internal/parallel"
)

func TestParallelVolumeSnapshotMatchesSerial(t *testing.T) {
	previous := parallel.SetBudget(0)
	defer parallel.SetBudget(previous)
	for _, tc := range []struct {
		name   string
		signed bool
		scale  func(int) Rescale
	}{
		{"signed16", true, func(int) Rescale { return Rescale{Slope: 2, Intercept: -1024} }},
		{"unsigned16", false, func(int) Rescale { return Rescale{Slope: 1} }},
		{"float32-varying-rescale", true, func(z int) Rescale { return Rescale{Slope: 1.25, Intercept: float64(-z)} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := stack16(48, tc.signed, tc.scale, ctValue)
			defer stack.Close()
			serial, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer serial.Close()
			parallel.SetBudget(0)
			wantModality, wantPayload, wantDescriptor := snapshotBytes(t, serial)
			candidate, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer candidate.Close()
			parallel.SetBudget(3)
			parallel.ResetPeak()
			gotModality, gotPayload, gotDescriptor := snapshotBytes(t, candidate)
			if !bytes.Equal(gotPayload, wantPayload) || !bytes.Equal(gotModality, wantModality) || gotDescriptor != wantDescriptor {
				t.Fatal("parallel canonical snapshot changed bytes, modality values or geometry")
			}
			if parallel.Peak() == 0 || parallel.Peak() > 3 || parallel.InUse() != 0 {
				t.Fatal("volume did not use/release the shared bounded helpers")
			}
		})
	}
}

func TestParallelVolumeSnapshotRejectsCorruptSlicesWithoutPublication(t *testing.T) {
	previous := parallel.SetBudget(3)
	defer parallel.SetBudget(previous)
	stack := stack16(48, true, func(int) Rescale { return Rescale{Slope: 1} }, ctValue)
	stack.Frames[2].PixelBytes = nil
	stack.Frames[5].PixelBytes = nil
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	if _, err := volume.PrepareSnapshotContext(context.Background()); err == nil {
		t.Fatal("corrupt planes were accepted")
	}
	if volume.SnapshotReady() || volume.VolumeStoreStats().Replacements != 0 || parallel.InUse() != 0 {
		t.Fatal("a failed parallel decode published partial data or retained a helper")
	}
}

func TestParallelVolumeDecodeFirstError(t *testing.T) {
	previous := parallel.SetBudget(1)
	defer parallel.SetBudget(previous)
	first, later := errors.New("first input"), errors.New("first completion")
	laterFinished := make(chan struct{})
	err := decodeVolumeSlices(context.Background(), 8, 1<<20, func(index int) error {
		if index == 0 {
			<-laterFinished
			return first
		}
		if index == 1 {
			close(laterFinished)
			return later
		}
		t.Error("decoded a plane beyond a known error")
		return nil
	})
	if !errors.Is(err, first) || parallel.InUse() != 0 {
		t.Fatalf("source-order error or helper lifetime changed: %v", err)
	}
}

func TestParallelVolumeDecodeJoinsOnCancellation(t *testing.T) {
	previous := parallel.SetBudget(1)
	defer parallel.SetBudget(previous)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- decodeVolumeSlices(ctx, 8, 1<<20, func(int) error {
			started <- struct{}{}
			<-release
			return nil
		})
	}()
	<-started
	<-started
	cancel()
	select {
	case <-done:
		t.Fatal("cancellation returned before workers stopped touching the destination")
	default:
	}
	close(release)
	if err := <-done; !errors.Is(err, context.Canceled) || parallel.InUse() != 0 {
		t.Fatalf("canceled decode did not join/release helpers: %v", err)
	}
}
