package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"
)

// TestWriteStored16Context streams packed and padded 16-bit stored payloads
// as tight rows whose rescale equals WriteModalityF32To, and rejects float32
// snapshots (#1226).
func TestWriteStored16Context(t *testing.T) {
	check := func(t *testing.T, snapshot VolumeSnapshot, cols, rows, depth int) {
		t.Helper()
		descriptor, err := snapshot.Descriptor()
		if err != nil {
			t.Fatal(err)
		}
		var stored, modality bytes.Buffer
		if err := snapshot.WriteStored16Context(context.Background(), &stored); err != nil {
			t.Fatal(err)
		}
		if err := snapshot.WriteModalityF32To(&modality); err != nil {
			t.Fatal(err)
		}
		if stored.Len() != 2*cols*rows*depth {
			t.Fatalf("stored bytes = %d, want %d", stored.Len(), 2*cols*rows*depth)
		}
		for i := 0; i < cols*rows*depth; i++ {
			raw := binary.LittleEndian.Uint16(stored.Bytes()[2*i:])
			value := float64(raw)
			if descriptor.ScalarFormat == VolumeScalarI16StoredLE {
				value = float64(int16(raw))
			}
			want := binary.LittleEndian.Uint32(modality.Bytes()[4*i:])
			if got := float32(value*descriptor.RescaleSlope + descriptor.RescaleIntercept); got != math.Float32frombits(want) {
				t.Fatalf("voxel %d: stored %d rescales to %v, modality %v", i, raw, got, math.Float32frombits(want))
			}
		}
	}
	t.Run("packed int16", func(t *testing.T) {
		const n = 5
		vol, err := BuildVolume(stack16(n, true, func(int) Rescale { return Rescale{Slope: 1, Intercept: -1024} }, ctValue))
		if err != nil {
			t.Fatal(err)
		}
		lease, err := vol.AcquireSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		snapshot, _ := lease.Snapshot()
		check(t, snapshot, n, n, n)
	})
	t.Run("padded uint16 rows", func(t *testing.T) {
		descriptor := testVolumeDescriptor(3, 2, 2)
		descriptor.ScalarFormat = VolumeScalarU16StoredLE
		descriptor.SampleDomain = VolumeSampleDomainStored
		descriptor.RescaleSlope = 0.5
		descriptor.RescaleIntercept = 3
		descriptor.RowStrideBytes = 8 // 3 samples and 2 bytes of padding
		descriptor.SliceStrideBytes = 16
		descriptor.ByteLength = 32
		payload := make([]byte, 32)
		for z := 0; z < 2; z++ {
			for y := 0; y < 2; y++ {
				for x := 0; x < 3; x++ {
					binary.LittleEndian.PutUint16(payload[z*16+y*8+x*2:], uint16(60000-x-10*y-100*z))
				}
				// Padding never reaches the stream.
				binary.LittleEndian.PutUint16(payload[z*16+y*8+6:], 0xbeef)
			}
		}
		store := NewVolumeStore()
		t.Cleanup(func() { _ = store.Close() })
		if _, err := store.Replace(VolumeInput{Descriptor: descriptor, Payload: payload}); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireCurrent()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		snapshot, _ := lease.Snapshot()
		check(t, snapshot, 3, 2, 2)
	})
	t.Run("float32 rejected", func(t *testing.T) {
		store := NewVolumeStore()
		t.Cleanup(func() { _ = store.Close() })
		if _, err := store.ReplaceFloat32(testVolumeDescriptor(2, 2, 2), make([]float32, 8)); err != nil {
			t.Fatal(err)
		}
		lease, err := store.AcquireCurrent()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		snapshot, _ := lease.Snapshot()
		if err := snapshot.WriteStored16Context(context.Background(), &bytes.Buffer{}); !errors.Is(err, ErrInvalidVolumeSnapshot) {
			t.Fatalf("float32 snapshot error = %v", err)
		}
	})
}
