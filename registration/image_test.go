package registration

import (
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

// TestImageFromSnapshotKeepsModalityValuesAndGeometry reads an oblique,
// anisotropic float32 volume and checks samples, index↔physical mapping
// and interpolation.
func TestImageFromSnapshotKeepsModalityValuesAndGeometry(t *testing.T) {
	c, s := math.Cos(0.4), math.Sin(0.4)
	spacing := [3]float64{0.8, 1.1, 2.5}
	origin := render.Vec3{X: -100, Y: 20, Z: 7}
	// Columns: the x, y, z index directions scaled by spacing.
	toPatient := render.GeometryAffine{
		c * spacing[0], -s * spacing[1], 0, origin.X,
		s * spacing[0], c * spacing[1], 0, origin.Y,
		0, 0, spacing[2], origin.Z,
		0, 0, 0, 1,
	}
	toIndex := render.GeometryAffine{
		c / spacing[0], s / spacing[0], 0, -(c*origin.X + s*origin.Y) / spacing[0],
		-s / spacing[1], c / spacing[1], 0, (s*origin.X - c*origin.Y) / spacing[1],
		0, 0, 1 / spacing[2], -origin.Z / spacing[2],
		0, 0, 0, 1,
	}
	values := make([]float32, 7*5*3)
	for i := range values {
		values[i] = float32(i) - 1000
	}
	descriptor := render.VolumeDescriptor{
		ContractVersion: render.VolumeSnapshotContractVersion, HeaderSize: render.VolumeSnapshotHeaderSizeV1,
		VolumeGeneration: 1, Derivation: render.VolumeDerivationNormalized, Dimensions: [3]uint32{7, 5, 3}, Components: 1,
		ScalarFormat: render.VolumeScalarF32ModalityLE, SampleDomain: render.VolumeSampleDomainModality,
		RowStrideBytes: 7 * 4, SliceStrideBytes: 7 * 5 * 4, ByteLength: 7 * 5 * 3 * 4, RescaleSlope: 1,
		SpacingMM: spacing, IndexToPatientLPS: toPatient, PatientLPSToIndex: toIndex,
	}
	store := render.NewVolumeStore()
	if _, err := store.ReplaceFloat32(descriptor, values); err != nil {
		t.Fatal(err)
	}
	lease, err := store.AcquireCurrent()
	if err != nil {
		t.Fatal(err)
	}
	defer lease.Release()
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	image, err := ImageFromSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if image.Size() != [3]int{7, 5, 3} || image.At(6, 4, 2) != values[len(values)-1] {
		t.Fatalf("size %v, last sample %v", image.Size(), image.At(6, 4, 2))
	}
	index := render.Vec3{X: 2.25, Y: 3.5, Z: 1}
	point := image.Point(index)
	if want := toPatient.TransformPoint(index); point.Sub(want).Length() > 1e-9 {
		t.Fatalf("Point %v, want %v", point, want)
	}
	if back := image.ContinuousIndex(point); back.Sub(index).Length() > 1e-9 {
		t.Fatalf("ContinuousIndex %v, want %v", back, index)
	}
	// Linear in x and y: the interpolation is exact.
	if v, ok := image.Interpolate(point); !ok || math.Abs(v-(2.25+3.5*7+1*35-1000)) > 1e-9 {
		t.Fatalf("Interpolate %v, %v", v, ok)
	}
	if _, ok := image.Interpolate(image.Point(render.Vec3{X: -0.6})); ok {
		t.Fatal("a point beyond half a sample outside interpolated")
	}
	if _, err := NewImage([3]int{2, 2, 1}, make([]float32, 3), render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3()); err == nil {
		t.Fatal("wrong sample count accepted")
	}
}

// TestDownsampleAveragesBlocksInPlace: a linear ramp stays the same ramp in
// physical space (block centres), partial blocks average what they hold,
// and invalid factors are refused.
func TestDownsampleAveragesBlocksInPlace(t *testing.T) {
	const nx, ny, nz = 7, 4, 3
	data := make([]float32, nx*ny*nz)
	for i := range data {
		x, y, z := i%nx, i/nx%ny, i/(nx*ny)
		data[i] = float32(2*x + 3*y + 5*z)
	}
	image, err := NewImage([3]int{nx, ny, nz}, data, render.Vec3{X: 10, Y: -4, Z: 1}, render.Vec3{X: 0.5, Y: 1, Z: 2}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	small, err := image.Downsample([3]int{2, 2, 1})
	if err != nil {
		t.Fatal(err)
	}
	if small.Size() != [3]int{4, 2, 3} || small.Spacing() != (render.Vec3{X: 1, Y: 2, Z: 2}) {
		t.Fatalf("size %v spacing %v", small.Size(), small.Spacing())
	}
	for z := 0; z < 3; z++ {
		for y := 0; y < 2; y++ {
			for x := 0; x < 3; x++ { // full blocks: the ramp at the block centre
				p := small.Point(render.Vec3{X: float64(x), Y: float64(y), Z: float64(z)})
				want, _ := image.Interpolate(p)
				if math.Abs(float64(small.At(x, y, z))-want) > 1e-5 {
					t.Fatalf("(%d,%d,%d): %v, ramp %v", x, y, z, small.At(x, y, z), want)
				}
			}
		}
	}
	// The last x block holds only x = 6.
	if got, want := small.At(3, 0, 0), float32(2*6+3*0.5); got != want {
		t.Fatalf("partial block %v, want %v", got, want)
	}
	if same, _ := image.Downsample([3]int{1, 1, 1}); same != image {
		t.Fatal("factor 1 copied")
	}
	if _, err := image.Downsample([3]int{0, 1, 1}); err == nil {
		t.Fatal("zero factor accepted")
	}
}

func TestDownsampleWithMaximumFactor(t *testing.T) {
	image, err := NewImage([3]int{3, 1, 1}, []float32{1, 2, 6}, render.Vec3{}, render.Vec3{X: 1, Y: 1, Z: 1}, Identity3())
	if err != nil {
		t.Fatal(err)
	}
	downsampled, err := image.Downsample([3]int{math.MaxInt, math.MaxInt, math.MaxInt})
	if err != nil {
		t.Fatal(err)
	}
	if size := downsampled.Size(); size != [3]int{1, 1, 1} {
		t.Fatalf("size %v, want one partial block", size)
	}
	spacing := float64(math.MaxInt)
	if got, want := downsampled.Spacing(), (render.Vec3{X: spacing, Y: spacing, Z: spacing}); got != want {
		t.Fatalf("spacing %v, want %v", got, want)
	}
	if v := downsampled.At(0, 0, 0); v != 3 {
		t.Fatalf("average %g, want 3", v)
	}
	if _, err := image.Downsample([3]int{math.MaxInt, 1, 1}); !errors.Is(err, ErrImage) {
		t.Fatalf("ill-conditioned geometry: got %v, want ErrImage", err)
	}
}
