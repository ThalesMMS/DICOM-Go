package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestVolumeComponentsInterleavedPaddedRowsOwnershipAndStreaming(t *testing.T) {
	d := testVolumeDescriptor(2, 2, 2)
	d.ContractVersion, d.Components = VolumeSnapshotContractVersionV2, 3
	d.RowStrideBytes, d.SliceStrideBytes, d.ByteLength = 32, 64, 128
	payload := make([]byte, d.ByteLength)
	var expected bytes.Buffer
	for z := uint32(0); z < 2; z++ {
		for y := uint32(0); y < 2; y++ {
			for x := uint32(0); x < 2; x++ {
				for c := uint32(0); c < 3; c++ {
					bits := math.Float32bits(float32(100*z + 10*y + 3*x + c))
					binary.LittleEndian.PutUint32(payload[uint64(z)*64+uint64(y)*32+uint64(x)*12+uint64(c)*4:], bits)
					_ = binary.Write(&expected, binary.LittleEndian, bits)
				}
			}
		}
	}
	store := NewVolumeStore()
	defer store.Close()
	generation, err := store.Replace(VolumeInput{Descriptor: d, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	clear(payload)
	lease, err := store.Acquire(generation)
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	for c := uint32(0); c < 3; c++ {
		value, ok := snapshot.ComponentModalityAt(1, 1, 1, c)
		if !ok || value != float64(113+c) {
			t.Fatalf("component %d: %v, %v", c, value, ok)
		}
	}
	if _, ok := snapshot.ModalityAt(1, 1, 1); ok {
		t.Fatal("scalar accessor accepted color data")
	}
	if _, ok := snapshot.ComponentModalityAt(1, 1, 1, 3); ok {
		t.Fatal("component beyond the voxel was accepted")
	}
	var streamed bytes.Buffer
	if err := snapshot.WriteModalityF32To(&streamed); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(streamed.Bytes(), expected.Bytes()) {
		t.Fatal("stream did not preserve interleaved components or remove row padding")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := snapshot.WriteModalityF32Context(ctx, &bytes.Buffer{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	_ = lease.Release()
	if _, ok := snapshot.ComponentModalityAt(0, 0, 0, 0); ok {
		t.Fatal("released component lease remained readable")
	}
}

func TestVolumeComponentsRGBSourcePreservesChannelsAndGeometry(t *testing.T) {
	stack := &Stack{PixelSpacing: []float64{2, .5}, SliceThickness: 3}
	loads := 0
	for z := 0; z < 2; z++ {
		pixels := make([]byte, 12)
		for i := 0; i < 4; i++ {
			for c := 0; c < 3; c++ {
				index := i*3 + c
				if z == 1 {
					index = c*4 + i
				}
				pixels[index] = byte(z*80 + i*10 + c*30)
			}
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata:   pixeldata.Metadata{Rows: 2, Columns: 2, SamplesPerPixel: 3, BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "RGB", PlanarConfiguration: uint16(z), PlanarConfigurationPresent: true},
			PixelBytes: pixels, ByteOrder: binary.LittleEndian,
			TransferSyntaxUID: transfer.ExplicitVRLittleEndian.UID,
			Rescale:           Rescale{Slope: 100, Intercept: -2000},
			ImageOrientation:  []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:     []float64{10, -20, float64(z) * 3},
		})
	}
	// The planar frame also exercises source loading introduced by the
	// concurrent geometry-only preparation change. Planning must not load it.
	resident := stack.Frames[1]
	deferred := *resident
	deferred.PixelBytes = nil
	deferred.LoadPixels = func(ctx context.Context) (*Frame, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		loads++
		return resident, nil
	}
	stack.Frames[1] = &deferred
	volume, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	defer volume.Close()
	if loads != 0 {
		t.Fatal("volume planning loaded color pixels")
	}
	var phases []VolumeDecodePhase
	stats, err := volume.PrepareSnapshotProgressiveContext(context.Background(), func(p VolumeDecodeProgress) { phases = append(phases, p.Phase) })
	if err != nil {
		t.Fatal(err)
	}
	if stats.VoxelBytes != 96 || len(phases) != 1 || phases[0] != VolumeDecodeComplete {
		t.Fatal(stats, phases)
	}
	if loads != 1 {
		t.Fatalf("deferred color frame loaded %d times, want once", loads)
	}
	lease, err := volume.AcquireSourceSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := snapshot.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	if descriptor.Components != 3 || descriptor.ContractVersion != VolumeSnapshotContractVersionV2 || descriptor.SpacingMM != [3]float64{.5, 2, 3} {
		t.Fatal(descriptor)
	}
	for z := uint32(0); z < 2; z++ {
		for y := uint32(0); y < 2; y++ {
			for x := uint32(0); x < 2; x++ {
				for c := uint32(0); c < 3; c++ {
					got, ok := snapshot.ComponentModalityAt(x, y, z, c)
					want := float64(z*80 + (y*2+x)*10 + c*30)
					if !ok || got != want {
						t.Fatalf("%d,%d,%d component%d: %v != %v", x, y, z, c, got, want)
					}
				}
			}
		}
	}
	if _, _, ok := volume.ValueAt(0, 0, 0); ok {
		t.Fatal("color volume was read as one scalar")
	}
	stack.Frames[1].Metadata.SamplesPerPixel = 1
	if _, err := BuildVolume(stack); !errors.Is(err, ErrInvalidVolumeSnapshot) {
		t.Fatal("mixed scalar/color stack accepted", err)
	}
}

func TestVolumeComponentsRejectMalformedAndPreserveScalarV1(t *testing.T) {
	d := testVolumeDescriptor(2, 2, 2)
	d.ContractVersion, d.Components = VolumeSnapshotContractVersionV2, 4
	d.RowStrideBytes, d.SliceStrideBytes, d.ByteLength = 32, 64, 128
	if err := ValidateVolumeDescriptor(d); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*VolumeDescriptor){
		func(d *VolumeDescriptor) { d.Components = 1 },
		func(d *VolumeDescriptor) { d.Components = 5 },
		func(d *VolumeDescriptor) { d.RowStrideBytes = 31 },
		func(d *VolumeDescriptor) { d.ContractVersion = VolumeSnapshotContractVersion },
		func(d *VolumeDescriptor) {
			d.ScalarFormat = VolumeScalarU16StoredLE
			d.SampleDomain = VolumeSampleDomainStored
		},
	} {
		bad := d
		mutate(&bad)
		if !errors.Is(ValidateVolumeDescriptor(bad), ErrInvalidVolumeSnapshot) {
			t.Fatalf("accepted malformed descriptor %+v", bad)
		}
	}
	if err := ValidateVolumeDescriptor(testVolumeDescriptor(2, 2, 2)); err != nil {
		t.Fatal(err)
	}
}
