package render

import (
	"context"
	"encoding/binary"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const (
	mprCorpusCols         = 5
	mprCorpusRows         = 4
	mprCorpusDepth        = 6
	mprCorpusColSpacing   = .625
	mprCorpusRowSpacing   = 1.75
	mprCorpusSliceSpacing = 3.25
)

type mprCorpusGeometry struct {
	origin, right, down, normal Vec3
}

// DICOM PS3.3 C.7.6.2 places integer (column,row) indices at pixel centres.
// These independent direction vectors and unequal pitches extend that
// calculation across the explicitly positioned slices.
func (g mprCorpusGeometry) patient(index Vec3) Vec3 {
	return g.origin.Add(g.right.Scale(mprCorpusColSpacing * index.X)).
		Add(g.down.Scale(mprCorpusRowSpacing * index.Y)).
		Add(g.normal.Scale(mprCorpusSliceSpacing * index.Z))
}

// A mixed-sign ramp makes transposed, reflected, and uncalibrated samples
// distinct. Its trilinear value within the volume is the same affine form.
func mprCorpusValue(index Vec3) float64 {
	return 3*index.X - 5*index.Y + 9*index.Z - 24.25
}

func mprCorpusStack(g mprCorpusGeometry, signed bool, varyRescale bool) *Stack {
	stack := &Stack{PixelSpacing: []float64{mprCorpusRowSpacing, mprCorpusColSpacing}}
	// Neither input order nor InstanceNumber represents the spatial order.
	for i, z := range []int{5, 4, 2, 3, 0, 1} {
		representation, base := uint16(0), 50000
		if signed {
			representation, base = 1, -110
		}
		if varyRescale {
			base += z * 200
		}
		pixels := make([]byte, 2*mprCorpusCols*mprCorpusRows)
		for y := range mprCorpusRows {
			for x := range mprCorpusCols {
				stored := base + 6*x - 10*y + 18*z
				binary.BigEndian.PutUint16(pixels[2*(y*mprCorpusCols+x):], uint16(stored))
			}
		}
		p := g.patient(Vec3{Z: float64(z)})
		stack.Frames = append(stack.Frames, &Frame{
			Metadata: pixeldata.Metadata{
				Columns: mprCorpusCols, Rows: mprCorpusRows, SamplesPerPixel: 1,
				BitsAllocated: 16, BitsStored: 16, HighBit: 15, PixelRepresentation: representation,
				PhotometricInterpretation: "MONOCHROME2",
			},
			ByteOrder: binary.BigEndian, PixelBytes: pixels, InstanceNumber: i + 1,
			Rescale:          Rescale{Slope: .5, Intercept: -24.25 - float64(base)/2},
			ImageOrientation: []float64{g.right.X, g.right.Y, g.right.Z, g.down.X, g.down.Y, g.down.Z},
			ImagePosition:    []float64{p.X, p.Y, p.Z},
		})
	}
	return stack
}

func TestMPRPatientSpaceAcquisitionCorpus(t *testing.T) {
	acquisitions := []struct {
		name                string
		right, down, normal Vec3
	}{
		{"axial", Vec3{X: 1}, Vec3{Y: 1}, Vec3{Z: 1}},
		{"coronal", Vec3{X: 1}, Vec3{Z: -1}, Vec3{Y: 1}},
		{"sagittal", Vec3{Y: 1}, Vec3{Z: -1}, Vec3{X: -1}},
		{"decreasing-patient-Z", Vec3{X: -1}, Vec3{Y: 1}, Vec3{Z: -1}},
		{"oblique", Vec3{X: .6, Y: .8}, Vec3{X: -.64, Y: .48, Z: .6}, Vec3{X: .48, Y: -.36, Z: .8}},
	}
	storage := []struct {
		name                string
		signed, varyRescale bool
		format              VolumeScalarFormat
	}{
		{"signed-stored", true, false, VolumeScalarI16StoredLE},
		{"unsigned-stored", false, false, VolumeScalarU16StoredLE},
		{"per-slice-calibration", false, true, VolumeScalarF32ModalityLE},
	}
	for _, acquisition := range acquisitions {
		for _, samples := range storage {
			t.Run(acquisition.name+"/"+samples.name, func(t *testing.T) {
				geometry := mprCorpusGeometry{Vec3{X: -72, Y: 35, Z: 118}, acquisition.right, acquisition.down, acquisition.normal}
				vol, err := BuildVolume(mprCorpusStack(geometry, samples.signed, samples.varyRescale))
				if err != nil {
					t.Fatal(err)
				}
				defer vol.Close()
				lease, err := vol.AcquireSnapshot()
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
				if descriptor.ScalarFormat != samples.format {
					t.Fatalf("snapshot format = %v, want %v", descriptor.ScalarFormat, samples.format)
				}
				crosshair := Vec3{X: 2.25, Y: 1.5, Z: 2.75}
				assertMPRCorpusPoint(t, "patient to voxel", vol.PatientToVoxel(geometry.patient(crosshair)), crosshair)
				assertMPRCorpusPoint(t, "voxel to patient", vol.VoxelToPatient(crosshair), geometry.patient(crosshair))
				texture := Vec3{X: .5625, Y: .5, Z: .55}
				assertMPRCorpusPoint(t, "voxel to texture", vol.VoxelToNormalized(crosshair), texture)
				assertMPRCorpusPoint(t, "texture to voxel", vol.NormalizedToVoxel(texture), crosshair)
				assertMPRCorpusPoint(t, "volume center", vol.Center(), geometry.patient(Vec3{X: 2, Y: 1.5, Z: 2.5}))
				for _, plane := range []struct {
					mode   MPRPlane
					w, h   int
					index  Vec3
					du, dv Vec3
				}{
					{MPRPlaneAxial, mprCorpusCols, mprCorpusRows, Vec3{Z: crosshair.Z}, Vec3{X: 1}, Vec3{Y: 1}},
					{MPRPlaneCoronal, mprCorpusCols, mprCorpusDepth, Vec3{Y: crosshair.Y, Z: 5}, Vec3{X: 1}, Vec3{Z: -1}},
					{MPRPlaneSagittal, mprCorpusRows, mprCorpusDepth, Vec3{X: crosshair.X, Z: 5}, Vec3{Y: 1}, Vec3{Z: -1}},
				} {
					t.Run(string(plane.mode), func(t *testing.T) {
						p := vol.OrthogonalPlane(plane.mode, geometry.patient(crosshair))
						assertMPRPatientCorpusPlane(t, vol, p, geometry, plane.index, plane.du, plane.dv, plane.w, plane.h)
					})
				}
				t.Run("oblique-with-outside-columns", func(t *testing.T) {
					const width, height = 6, 4
					index := Vec3{X: -1.125, Y: .75, Z: 1.5}
					du, dv := Vec3{X: .875, Z: .375}, Vec3{Y: .625, Z: .125}
					origin := geometry.patient(index)
					p := Plane{
						Origin: origin,
						U:      geometry.patient(index.Add(du.Scale(width - 1))).Sub(origin),
						V:      geometry.patient(index.Add(dv.Scale(height - 1))).Sub(origin),
					}
					assertMPRPatientCorpusPlane(t, vol, p, geometry, index, du, dv, width, height)
				})
			})
		}
	}
}

func assertMPRCorpusPoint(t *testing.T, description string, got, want Vec3) {
	t.Helper()
	if distance := got.Sub(want).Length(); !(distance <= 1e-9) {
		t.Errorf("%s = %+v, want %+v", description, got, want)
	}
}

func assertMPRPatientCorpusPlane(t *testing.T, vol *Volume, plane Plane, geometry mprCorpusGeometry, index, du, dv Vec3, width, height int) {
	t.Helper()
	values, err := ResliceObliqueScalarContext(context.Background(), vol, plane, width, height)
	if err != nil {
		t.Fatal(err)
	}
	if w, h := values.Dimensions(); w != width || h != height {
		t.Fatalf("plane = %dx%d, want %dx%d", w, h, width, height)
	}
	spacing := plane.PixelSpacingMM(width, height)
	wantX := geometry.patient(index.Add(du)).Sub(geometry.patient(index)).Length()
	wantY := geometry.patient(index.Add(dv)).Sub(geometry.patient(index)).Length()
	if !(math.Abs(spacing.X-wantX) <= 1e-9 && math.Abs(spacing.Y-wantY) <= 1e-9) {
		t.Fatalf("spacing = %+v, want %g by %g mm", spacing, wantX, wantY)
	}
	for y := range height {
		for x := range width {
			voxel := index.Add(du.Scale(float64(x))).Add(dv.Scale(float64(y)))
			assertMPRCorpusPoint(t, "plane sample center", plane.At(float64(x)/float64(width-1), float64(y)/float64(height-1)), geometry.patient(voxel))
			got, valid := values.ValueAt(x, y)
			// The arbitrary plane crosses the left boundary. The first column
			// misses, while a point within half a voxel clamps to the edge.
			if voxel.X < -.5 {
				if valid {
					t.Errorf("(%d,%d) outside voxel %+v produced a sample %g", x, y, voxel, got)
				}
				continue
			}
			voxel.X = math.Max(0, voxel.X)
			want := mprCorpusValue(voxel)
			if !valid || math.IsNaN(got) || math.Abs(got-want) > 1e-9 {
				t.Errorf("(%d,%d) patient-space sample = %g (valid %v), want %g", x, y, got, valid, want)
			}
		}
	}
}
