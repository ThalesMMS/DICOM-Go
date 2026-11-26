package spatialreg

import (
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/render"
)

// itkTransformGeometryImageFilterTest exercises metadata-only mapping on
// a sampled image. Here the source centres and bases are expressed in DICOM
// patient coordinates and mapped by a stored Spatial Registration matrix.
func TestITKReferenceMapsVolumeAndPlaneThroughShear(t *testing.T) {
	stack := &render.Stack{}
	for z := 0; z < 2; z++ {
		stack.Frames = append(stack.Frames, &render.Frame{
			Metadata: pixeldata.Metadata{
				Rows: 2, Columns: 3, SamplesPerPixel: 1,
				BitsAllocated: 8, BitsStored: 8, HighBit: 7, PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder:        binary.LittleEndian,
			ImageOrientation: []float64{0, 1, 0, -1, 0, 0},
			ImagePosition:    []float64{5, -4, float64(9 + 4*z)},
			PixelSpacing:     []float64{3, 2},
			PixelBytes:       []byte{byte(100 * z), byte(100*z + 1), byte(100*z + 2), byte(100*z + 10), byte(100*z + 11), byte(100*z + 12)},
			Rescale:          render.Rescale{Slope: 1},
		})
	}
	volume, err := render.BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = volume.Close() })
	data := affineFixture(affineMatrix(TransformAffine, render.GeometryAffine{
		1, .5, 0, 7,
		0, 2, -1, -1,
		.5, 0, 1, 3,
		0, 0, 0, 1,
	}))
	parsed, err := ReadAffineDataset(data)
	if err != nil {
		t.Fatal(err)
	}
	if len(parsed.Registrations) != 1 {
		t.Fatalf("parsed %d registrations, want 1", len(parsed.Registrations))
	}
	registration := &parsed.Registrations[0]
	before := volume.VolumeStoreStats().LiveBytes
	mapped, err := registration.MapSourceVolumeToRegistered(volume)
	if err != nil {
		t.Fatal(err)
	}
	// Source centres are (5-3y,-4+2x,9+4z). Map their extreme indices
	// in x-fastest order through (p+q/2+7,2q-r-1,p/2+r+3).
	want := MappedVolumeGeometry{
		Dimensions: [3]int{3, 2, 2},
		Corners: [8]render.Vec3{
			{X: 10, Y: -18, Z: 14.5}, {X: 12, Y: -10, Z: 14.5},
			{X: 7, Y: -18, Z: 13}, {X: 9, Y: -10, Z: 13},
			{X: 10, Y: -22, Z: 18.5}, {X: 12, Y: -14, Z: 18.5},
			{X: 7, Y: -22, Z: 17}, {X: 9, Y: -14, Z: 17},
		},
	}
	if mapped != want {
		t.Fatalf("mapped geometry %+v, want %+v", mapped, want)
	}
	sourcePlane := render.Plane{Origin: render.Vec3{X: 5, Y: -4, Z: 13}, U: render.Vec3{Y: 4}, V: render.Vec3{X: -3}}
	registeredPlane, err := registration.MapSourcePlaneToRegistered(sourcePlane)
	if err != nil {
		t.Fatal(err)
	}
	wantPlane := render.Plane{Origin: want.Corners[4], U: render.Vec3{X: 2, Y: 8}, V: render.Vec3{X: -3, Z: -1.5}}
	if registeredPlane != wantPlane {
		t.Fatalf("mapped plane %+v, want %+v", registeredPlane, wantPlane)
	}
	if live := volume.VolumeStoreStats().LiveBytes; live != before {
		t.Fatalf("geometry operations changed live voxel bytes from %d to %d", before, live)
	}

	sampler, err := registration.AcquireRegisteredVolumeSampler(volume)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sampler.Close() })
	// The centre of the registered plane maps to source index (1,0.5,1).
	if got, ok := sampler.SamplePatient(registeredPlane.At(.5, .5)); !ok || !(math.Abs(got-106) <= 1e-12) {
		t.Fatalf("registered plane centre is %g/%t, want 106/true", got, ok)
	}
}
