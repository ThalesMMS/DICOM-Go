package render

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// stack16 builds an n³ stack of 16-bit stored samples value(x, y, z) with a
// rescale per slice.
func stack16(n int, signed bool, rescale func(z int) Rescale, value func(x, y, z int) int) *Stack {
	stack := &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1}
	for z := 0; z < n; z++ {
		data := make([]byte, 2*n*n)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				binary.LittleEndian.PutUint16(data[2*(y*n+x):], uint16(value(x, y, z)))
			}
		}
		representation := uint16(0)
		if signed {
			representation = 1
		}
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Rows: uint16(n), Columns: uint16(n), SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15, PixelRepresentation: representation,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			PixelBytes:       data,
			Rescale:          rescale(z),
			ImageOrientation: []float64{1, 0, 0, 0, 1, 0},
			ImagePosition:    []float64{0, 0, float64(z)},
		})
	}
	return stack
}

func ctValue(x, y, z int) int { return x*37 + y*11 + z*5 - 1000 }

// TestPacked16SnapshotHalvesMemoryAndKeepsModalityValues (#1065): a CT stack
// of int16 stored samples with one rescale is kept as packed int16 with the
// rescale, half the bytes of float32, and samples the same modality values.
func TestPacked16SnapshotHalvesMemoryAndKeepsModalityValues(t *testing.T) {
	const n = 12
	uniform := func(int) Rescale { return Rescale{Slope: 2, Intercept: -1024} }
	vol, err := BuildVolume(stack16(n, true, uniform, ctValue))
	if err != nil {
		t.Fatal(err)
	}
	lease, err := vol.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	descriptor, _ := snapshot.Descriptor()
	if descriptor.ScalarFormat != VolumeScalarI16StoredLE || descriptor.SampleDomain != VolumeSampleDomainStored ||
		descriptor.RescaleSlope != 2 || descriptor.RescaleIntercept != -1024 || descriptor.ByteLength != 2*n*n*n {
		t.Fatalf("descriptor = %+v", descriptor)
	}
	if live := vol.store.Stats().LiveBytes; live != 2*n*n*n {
		t.Fatalf("live bytes = %d, want %d (2 per voxel)", live, 2*n*n*n)
	}
	sampler, err := newVolumeSamplerWithError(vol)
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	var modality bytes.Buffer
	if err := snapshot.WriteModalityF32To(&modality); err != nil {
		t.Fatal(err)
	}
	for z := 0; z < n; z++ {
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				want := float64(ctValue(x, y, z))*2 - 1024
				if got, ok := sampler.valueAt(x, y, z); !ok || got != want {
					t.Fatalf("valueAt(%d,%d,%d) = %v, want %v", x, y, z, got, want)
				}
				bits := binary.LittleEndian.Uint32(modality.Bytes()[4*((z*n+y)*n+x):])
				if got := math.Float32frombits(bits); float64(got) != want {
					t.Fatalf("uploaded modality at %d,%d,%d = %v, want %v", x, y, z, got, want)
				}
			}
		}
	}
	// A trilinear sample between voxels matches the float64 interpolation.
	if got, ok := sampler.trilinearAt(Vec3{X: 1.5, Y: 2, Z: 3}); !ok || math.Abs(got-(float64(ctValue(1, 2, 3)+ctValue(2, 2, 3))-1024)) > 1e-9 {
		t.Fatalf("trilinear = %v", got)
	}
	// VR preparation (float32 prefilter output) does not inherit the 2-byte
	// strides.
	if _, err := vol.PrepareVRVolume(VRPrefilterBasicSmooth5x5); err != nil {
		t.Fatalf("prefilter of a packed 16-bit volume: %v", err)
	}
}

func TestPacked16SnapshotFormats(t *testing.T) {
	const n = 6
	format := func(stack *Stack) (VolumeScalarFormat, uint64) {
		t.Helper()
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		lease, err := vol.AcquireSnapshot()
		if err != nil {
			t.Fatal(err)
		}
		defer lease.Release()
		snapshot, _ := lease.Snapshot()
		descriptor, _ := snapshot.Descriptor()
		return descriptor.ScalarFormat, vol.store.Stats().LiveBytes
	}
	unsigned := func(x, y, z int) int { return 60000 - x*100 - z }
	if f, bytes := format(stack16(n, false, func(int) Rescale { return Rescale{Slope: 1} }, unsigned)); f != VolumeScalarU16StoredLE || bytes != 2*n*n*n {
		t.Fatalf("unsigned MR = %v, %d bytes", f, bytes)
	}
	// Per-slice rescale keeps float32.
	varying := func(z int) Rescale { return Rescale{Slope: 1, Intercept: float64(-z)} }
	if f, bytes := format(stack16(n, true, varying, ctValue)); f != VolumeScalarF32ModalityLE || bytes != 4*n*n*n {
		t.Fatalf("per-slice rescale = %v, %d bytes", f, bytes)
	}
	// 8-bit stacks keep float32.
	if f, _ := format(sphereStack(n, 2)); f != VolumeScalarF32ModalityLE {
		t.Fatalf("8-bit stack = %v", f)
	}
}
