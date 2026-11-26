package render

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"
)

func TestProgressivePreviewPlanMatchesHeadlessGeometryAndPreservesInput(t *testing.T) {
	for _, n := range []int{1, 2, 5, 6} {
		v, err := BuildVolume(sphereStack(n, 1))
		if err != nil {
			t.Fatal(err)
		}
		d, err := v.snapshotDescriptor(VolumeDerivationNormalized, 0, false)
		if err != nil {
			t.Fatal(err)
		}
		before := d
		p, err := PlanVolumeProgressivePreview(d)
		if err != nil || d != before || v.SnapshotReady() {
			t.Fatal("metadata plan loaded or changed source", err)
		}
		var preview *Volume
		_, err = v.PrepareSnapshotProgressiveContext(context.Background(), func(e VolumeDecodeProgress) {
			if e.Phase == VolumeDecodePreview {
				preview = e.Preview
			}
		})
		if err != nil || preview == nil {
			t.Fatal(err)
		}
		_, _, oracle := snapshotBytes(t, preview)
		if p.Preview.Dimensions != oracle.Dimensions || p.Preview.SpacingMM != oracle.SpacingMM || p.Preview.ByteLength != oracle.ByteLength {
			t.Fatal("preview dimensions/spacing differ", n, p.Preview, oracle)
		}
		for i, value := range p.Preview.IndexToPatientLPS {
			if math.Abs(value-oracle.IndexToPatientLPS[i]) > 1e-12 {
				t.Fatal("preview affine differs", n, i, value, oracle.IndexToPatientLPS[i])
			}
		}
		_ = preview.Close()
		_ = v.Close()
	}
	if _, err := PlanVolumeProgressivePreview(VolumeDescriptor{}); err == nil {
		t.Fatal("invalid metadata accepted")
	}
}

func TestProgressiveEvenSlicesRetainOriginalSamplesAcrossCancellationRetryAndClose(t *testing.T) {
	for _, packed := range []bool{false, true} {
		const n = 5
		build := func() *Stack {
			if packed {
				return stack16(n, true, func(int) Rescale { return Rescale{Slope: -2, Intercept: 300} }, ctValue)
			}
			return sphereStack(n, 1)
		}
		reference, err := BuildVolume(build())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reference.PrepareSnapshotContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		want, raw, d := snapshotBytes(t, reference)
		var expected, expectedRaw bytes.Buffer
		for z := 0; z < n; z += 2 {
			expected.Write(want[z*n*n*4 : (z+1)*n*n*4])
			if packed {
				expectedRaw.Write(raw[z*n*n*2 : (z+1)*n*n*2])
			}
		}
		v, err := BuildVolume(build())
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		var retained RetainedVolumeBandReader
		var phases []VolumeDecodePhase
		_, err = v.PrepareSnapshotProgressiveWithOptionsContext(ctx, VolumeDecodeOptions{SkipPreview: true, ReportEvenSlices: true}, func(e VolumeDecodeProgress) {
			phases = append(phases, e.Phase)
			if e.Preview != nil {
				t.Fatal("CPU preview was generated")
			}
			if e.Phase == VolumeDecodeEvenSlices {
				if e.DecodedSlices != 3 || e.Descriptor.VolumeGeneration != 0 || v.SnapshotReady() {
					t.Fatal("even source published after complete decode")
				}
				var retainErr error
				retained, retainErr = e.EvenSlices.Retain()
				if retainErr != nil {
					t.Fatal(retainErr)
				}
				cancel()
			}
		})
		if !errors.Is(err, context.Canceled) || len(phases) == 0 || phases[0] != VolumeDecodeEvenSlices {
			t.Fatal("even source event/order/cancellation", phases, err)
		}
		if _, err := v.PrepareSnapshotContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		_ = v.Close()
		var actual bytes.Buffer
		if err := retained.WriteModalityF32To(&actual); err != nil || !bytes.Equal(actual.Bytes(), expected.Bytes()) {
			t.Fatal("retained even values changed", packed, err)
		}
		if packed {
			actual.Reset()
			if err := retained.WriteStored16To(&actual); err != nil || !bytes.Equal(actual.Bytes(), expectedRaw.Bytes()) {
				t.Fatal("retained stored bits changed", err)
			}
		}
		p, err := PlanVolumeProgressivePreview(d)
		wantBytes := expected.Len()
		if packed {
			wantBytes = expectedRaw.Len()
		}
		if err != nil || p.EvenSlices.ByteLength != uint64(wantBytes) {
			t.Fatal("compact descriptor length", err)
		}
		_ = reference.Close()
	}
}
