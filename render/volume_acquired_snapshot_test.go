package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sync"
	"testing"
)

func acquiredSnapshotStack(format VolumeScalarFormat) *Stack {
	stack := &Stack{PixelSpacing: []float64{2, .5}, SliceThickness: 1}
	for index, z := range []float64{0, 1, 3, 4} {
		frame := volumeTestFrame(5, 6, make([]byte, 30), index)
		frame.ImagePosition = []float64{.25 * z, 0, z}
		frame.PixelSpacing = []float64{2, .5}
		if format == VolumeScalarF32ModalityLE {
			for y := 0; y < 5; y++ {
				for x := 0; x < 6; x++ {
					frame.PixelBytes[y*6+x] = byte(20*z + 2*float64(x) + 10*float64(y))
				}
			}
		} else {
			frame.Metadata.BitsAllocated, frame.Metadata.BitsStored, frame.Metadata.HighBit = 16, 16, 15
			frame.Rescale = Rescale{Slope: 2, Intercept: -100}
			if format == VolumeScalarI16StoredLE {
				frame.Metadata.PixelRepresentation = 1
				frame.Rescale = Rescale{Slope: -2, Intercept: 1000}
			}
			frame.PixelBytes = make([]byte, 60)
			for y := 0; y < 5; y++ {
				for x := 0; x < 6; x++ {
					value := 20*z + 2*float64(x) + 10*float64(y)
					raw := (value - frame.Rescale.Intercept) / frame.Rescale.Slope
					binary.LittleEndian.PutUint16(frame.PixelBytes[2*(y*6+x):], uint16(int16(raw)))
				}
			}
		}
		stack.Frames = append(stack.Frames, frame)
	}
	return stack
}

func TestAcquiredSnapshotPreservesRawGeometryDomainAndLifetimeWithoutRegularizing(t *testing.T) {
	for _, format := range []VolumeScalarFormat{VolumeScalarF32ModalityLE, VolumeScalarU16StoredLE, VolumeScalarI16StoredLE} {
		t.Run(fmt.Sprint(format), func(t *testing.T) {
			stack := acquiredSnapshotStack(format)
			volume, err := BuildVolume(stack)
			if err != nil {
				t.Fatal(err)
			}
			defer volume.Close()
			if !volume.geometry.RequiresResampling || volume.geometry.SourceAffine {
				t.Fatal("fixture is not irregular")
			}
			if volume.AcquiredResidencyKey().Valid() {
				t.Fatal("identity inspection decoded source")
			}
			metadata, err := volume.PlanAcquiredRegularization()
			if err != nil || metadata.Target.Dimensions != ([3]int{8, 5, 5}) || volume.generation != 0 || volume.regularized != nil {
				t.Fatal("metadata plan prepared voxel data", err)
			}
			if _, err := volume.AcquireSourceSnapshotContext(context.Background()); !errors.Is(err, ErrInvalidVolumeSnapshot) {
				t.Fatal("generic source affine unexpectedly accepted", err)
			}
			lease, err := volume.AcquireAcquiredSnapshotContext(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer lease.Release()
			snapshot, err := lease.Snapshot()
			if err != nil {
				t.Fatal(err)
			}
			d, err := snapshot.Descriptor()
			if err != nil {
				t.Fatal(err)
			}
			if !d.ResidencyKey.Valid() || d.Dimensions != ([3]uint32{6, 5, 4}) || d.ScalarFormat != format {
				t.Fatal("acquired descriptor", d)
			}
			if volume.AcquiredResidencyKey() != d.ResidencyKey {
				t.Fatal("metadata identity differs from acquired lease")
			}
			plan, err := snapshot.RegularizationPlan()
			if err != nil {
				t.Fatal(err)
			}
			if plan.Target.Dimensions != ([3]int{8, 5, 5}) {
				t.Fatal("target bounds", plan)
			}
			plan.SliceOrigins[1].Z = 999
			secondPlan, err := snapshot.RegularizationPlan()
			if err != nil || secondPlan.SliceOrigins[1].Z != 1 {
				t.Fatal("metadata aliases consumer", err)
			}
			var modality, raw bytes.Buffer
			if err := snapshot.WriteModalityF32Context(context.Background(), &modality); err != nil {
				t.Fatal(err)
			}
			if err := snapshot.WritePayloadTo(&raw); err != nil {
				t.Fatal(err)
			}
			if uint64(raw.Len()) != d.ByteLength || modality.Len() != 6*5*4*4 {
				t.Fatal("raw byte count", raw.Len(), d)
			}
			for index, z := range []float64{0, 1, 3, 4} {
				for y := 0; y < 5; y++ {
					for x := 0; x < 6; x++ {
						value := math.Float32frombits(binary.LittleEndian.Uint32(modality.Bytes()[4*((index*5+y)*6+x):]))
						want := float32(20*z + 2*float64(x) + 10*float64(y))
						if value != want {
							t.Fatal("acquired value changed", index, x, y, value, want)
						}
					}
				}
			}
			if volume.regularized != nil || volume.regularizedReady.Load() {
				t.Fatal("acquisition invoked CPU regularizer")
			}
			if err := volume.Close(); err != nil {
				t.Fatal(err)
			}
			if volume.AcquiredResidencyKey().Valid() {
				t.Fatal("closed owner retained display identity")
			}
			if _, err := volume.PlanAcquiredRegularization(); !errors.Is(err, ErrVolumeStoreClosed) {
				t.Fatal("closed owner plan accepted", err)
			}
			var held bytes.Buffer
			if err := snapshot.WritePayloadTo(&held); err != nil || !bytes.Equal(held.Bytes(), raw.Bytes()) {
				t.Fatal("live lease lost payload after owner close", err)
			}
			if err := lease.Release(); err != nil {
				t.Fatal(err)
			}
			if _, err := snapshot.Descriptor(); !errors.Is(err, ErrVolumeLeaseReleased) {
				t.Fatal("released descriptor accepted", err)
			}
			if _, err := snapshot.RegularizationPlan(); !errors.Is(err, ErrVolumeLeaseReleased) {
				t.Fatal("released plan accepted", err)
			}
			if snapshot.ResidencyKey().Valid() {
				t.Fatal("released identity survives")
			}
			if err := snapshot.WritePayloadTo(&held); !errors.Is(err, ErrVolumeLeaseReleased) {
				t.Fatal("released payload accepted", err)
			}
		})
	}
}

func TestAcquiredSnapshotCancellationAdmissionAndConcurrentIdentity(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	volume, err := BuildVolume(acquiredSnapshotStack(VolumeScalarU16StoredLE))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	if _, err := volume.AcquireAcquiredSnapshotContext(ctx); !errors.Is(err, context.Canceled) || volume.generation != 0 || volume.regularized != nil {
		t.Fatal("cancel prepared source", err)
	}
	const callers = 8
	var wait sync.WaitGroup
	keys := make(chan VolumeResidencyKey, callers)
	errorsOut := make(chan error, callers)
	for n := 0; n < callers; n++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			lease, err := volume.AcquireAcquiredSnapshotContext(context.Background())
			if err != nil {
				errorsOut <- err
				return
			}
			defer lease.Release()
			snapshot, err := lease.Snapshot()
			if err != nil {
				errorsOut <- err
				return
			}
			keys <- snapshot.ResidencyKey()
		}()
	}
	wait.Wait()
	close(keys)
	close(errorsOut)
	for err := range errorsOut {
		t.Fatal(err)
	}
	var first VolumeResidencyKey
	for key := range keys {
		if !first.Valid() {
			first = key
		}
		if !key.Valid() || key != first {
			t.Fatal("concurrent acquisition changed source identity", first, key)
		}
	}
	if volume.store.Stats().Replacements != 1 || volume.regularized != nil {
		t.Fatal("duplicate decode or spatial preparation", volume.store.Stats())
	}
	store := NewVolumeStore(VolumeStoreOptions{MaxLiveBytes: 1})
	limited, err := BuildVolumeWithStore(acquiredSnapshotStack(VolumeScalarU16StoredLE), store)
	if err != nil {
		t.Fatal(err)
	}
	defer limited.Close()
	if _, err := limited.AcquireAcquiredSnapshotContext(context.Background()); !errors.Is(err, ErrVolumeBudgetExceeded) || store.Stats().LiveBytes != 0 || limited.regularized != nil {
		t.Fatal("admission published data", err, store.Stats())
	}
}

// This is an independent library consumer test. Calling its CPU regularizer
// here does not qualify or enable an application preparation path.
func TestAcquiredLeaseSurvivesIndependentHeadlessRegularization(t *testing.T) {
	volume, err := BuildVolume(acquiredSnapshotStack(VolumeScalarI16StoredLE))
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	lease, err := volume.AcquireAcquiredSnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	original, err := snapshot.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	var before, after bytes.Buffer
	if err := snapshot.WritePayloadTo(&before); err != nil {
		t.Fatal(err)
	}
	canonicalLease, err := volume.AcquireSnapshotContext(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer canonicalLease.Release()
	canonical, err := canonicalLease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d, err := canonical.Descriptor()
	if err != nil || d.Dimensions != ([3]uint32{8, 5, 5}) || d.Derivation != VolumeDerivationRegularized || d.ParentGeneration == 0 {
		t.Fatal("canonical contract changed", d, err)
	}
	retained, err := snapshot.Descriptor()
	if err != nil || retained != original {
		t.Fatal("held raw descriptor changed", retained, original, err)
	}
	if err := snapshot.WritePayloadTo(&after); err != nil || !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("headless derivation changed acquired lease", err)
	}
}
