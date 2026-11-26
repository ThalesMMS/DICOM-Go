package render

import (
	"context"
	"testing"
)

func TestVolumePreparationEstimateMatchesCompactAndFloatPayloads(t *testing.T) {
	for _, tc := range []struct {
		name         string
		signed, vary bool
		format       VolumeScalarFormat
		bytes        uint64
	}{
		{"signed", true, false, VolumeScalarI16StoredLE, 2}, {"unsigned", false, false, VolumeScalarU16StoredLE, 2}, {"varying-rescale", true, true, VolumeScalarF32ModalityLE, 4},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stack := stack16(12, tc.signed, func(z int) Rescale {
				slope := 2.
				if tc.vary {
					slope += float64(z)
				}
				return Rescale{Slope: slope, Intercept: -1024}
			}, ctValue)
			vol, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer vol.Close()
			plan, err := vol.EstimateDirectPreparation()
			if err != nil {
				t.Fatal(err)
			}
			if vol.SnapshotReady() || plan.ScalarFormat != tc.format || plan.PayloadBytes != 12*12*12*tc.bytes || plan.TransientBytes == 0 {
				t.Fatal("metadata-only estimate mismatch", plan)
			}
			stats, err := vol.PrepareSnapshotContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if stats.VoxelBytes != plan.PayloadBytes {
				t.Fatal("admission did not match installed bytes", plan, stats)
			}
		})
	}
}

func TestVolumeSnapshotReleaseObserverWaitsForLastLeaseAndRunsOutsideStoreLock(t *testing.T) {
	vol, err := BuildVolume(stack16(4, true, func(int) Rescale { return Rescale{Slope: 1} }, ctValue))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := vol.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	if err := vol.OnSnapshotRelease(func() {
		calls++
		if stats := vol.VolumeStoreStats(); stats.LiveBytes != 0 {
			t.Error("observer ran before backing release", stats)
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := vol.Close(); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatal("closing volume released leased accounting")
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("release observer count", calls)
	}
	_ = lease.Release()
	_ = vol.Close()
	if calls != 1 {
		t.Fatal("release observer repeated")
	}
}
