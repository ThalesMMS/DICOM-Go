package render

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
)

// TestReadModalitySliceF32 reads padded stored-value and packed float32
// payloads one slice at a time; every sample equals ModalityAt, and a short
// buffer, an out-of-range slice and a released lease are rejected.
func TestReadModalitySliceF32(t *testing.T) {
	t.Run("stored padded rows", func(t *testing.T) {
		descriptor := testVolumeDescriptor(3, 2, 2)
		descriptor.ScalarFormat = VolumeScalarI16StoredLE
		descriptor.SampleDomain = VolumeSampleDomainStored
		descriptor.RescaleSlope = 2
		descriptor.RescaleIntercept = -10
		descriptor.RowStrideBytes = 8 // 3 samples and 2 bytes of padding
		descriptor.SliceStrideBytes = 16
		descriptor.ByteLength = 32
		payload := make([]byte, 32)
		for z := 0; z < 2; z++ {
			for y := 0; y < 2; y++ {
				for x := 0; x < 3; x++ {
					binary.LittleEndian.PutUint16(payload[z*16+y*8+x*2:], uint16(int16(x+10*y+100*z-50)))
				}
			}
		}
		store := NewVolumeStore()
		t.Cleanup(func() { _ = store.Close() })
		if _, err := store.Replace(VolumeInput{Descriptor: descriptor, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		checkModalitySlices(t, store, 3, 2, 2)
	})
	t.Run("packed float32", func(t *testing.T) {
		descriptor := testVolumeDescriptor(4, 3, 2)
		values := make([]float32, 4*3*2)
		for i := range values {
			values[i] = float32(i)*0.5 - 3
		}
		store := NewVolumeStore()
		t.Cleanup(func() { _ = store.Close() })
		if _, err := store.ReplaceFloat32(descriptor, values); err != nil {
			t.Fatal(err)
		}
		checkModalitySlices(t, store, 4, 3, 2)
	})
}

func checkModalitySlices(t *testing.T, store *VolumeStore, cols, rows, depth int) {
	t.Helper()
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	dst := make([]float32, cols*rows)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	dst[0] = 123
	if err := snapshot.ReadModalitySliceF32Context(ctx, 0, dst); !errors.Is(err, context.Canceled) || dst[0] != 123 {
		t.Fatalf("pre-cancelled reader changed destination: %v", err)
	}
	for z := 0; z < depth; z++ {
		if err := snapshot.ReadModalitySliceF32(uint32(z), dst); err != nil {
			t.Fatal(err)
		}
		for y := 0; y < rows; y++ {
			for x := 0; x < cols; x++ {
				want, ok := snapshot.ModalityAt(uint32(x), uint32(y), uint32(z))
				if !ok || float64(dst[y*cols+x]) != want {
					t.Fatalf("voxel %d,%d,%d = %v, ModalityAt %v (%v)", x, y, z, dst[y*cols+x], want, ok)
				}
			}
		}
	}
	if err := snapshot.ReadModalitySliceF32(uint32(depth), dst); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatalf("slice past the end: %v", err)
	}
	if err := snapshot.ReadModalitySliceF32(0, dst[:len(dst)-1]); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatalf("short buffer: %v", err)
	}
	if err := lease.Release(); err != nil {
		t.Fatal(err)
	}
	if err := snapshot.ReadModalitySliceF32(0, dst); err == nil {
		t.Fatal("released lease still readable")
	}
}
