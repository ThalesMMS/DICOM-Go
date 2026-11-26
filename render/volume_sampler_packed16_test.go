package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/pixeldata/display"
	"image"
	"image/color"
	"math"
	"testing"
)

// Independent stored integers and bit alignment; the existing decode layer
// normalizes BitsStored/HighBit before the sampler sees the canonical payload.
func packed16Issue1307Stack(n int, signed bool, bits, high uint16, affine Rescale) (*Stack, []int) {
	mask := (1 << bits) - 1
	values := make([]int, n*n*n)
	stack := stack16(n, signed, func(int) Rescale { return affine }, func(x, y, z int) int {
		v := ((z*n+y)*n + x) * 8191 & mask
		if signed {
			v -= 1 << (bits - 1)
		}
		values[(z*n+y)*n+x] = v
		return v
	})
	for _, frame := range stack.Frames {
		frame.Metadata.BitsStored, frame.Metadata.HighBit = bits, high
		for i := 0; i < len(frame.PixelBytes); i += 2 {
			v := binary.LittleEndian.Uint16(frame.PixelBytes[i:])
			binary.LittleEndian.PutUint16(frame.PixelBytes[i:], (v&uint16(mask))<<(high+1-bits))
		}
	}
	return stack, values
}

// Tensor-product eight weights, independent of the sampler's nested lerps.
func packed16Issue1307Analytic(p Vec3, n int, values []int, affine Rescale) (float64, bool) {
	axis := [3]float64{p.X, p.Y, p.Z}
	for i, v := range axis {
		if math.IsNaN(v) || math.IsInf(v, 0) || v < -0.5-voxelBorderTolerance || v >= float64(n)-0.5+voxelBorderTolerance {
			return 0, false
		}
		axis[i] = math.Max(0, math.Min(float64(n-1), v))
	}
	base := [3]int{int(math.Floor(axis[0])), int(math.Floor(axis[1])), int(math.Floor(axis[2]))}
	f := [3]float64{axis[0] - float64(base[0]), axis[1] - float64(base[1]), axis[2] - float64(base[2])}
	var stored float64
	for z := 0; z < 2; z++ {
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				weight := 1.0
				for i, d := range []int{x, y, z} {
					if d == 0 {
						weight *= 1 - f[i]
					} else {
						weight *= f[i]
					}
				}
				ix, iy, iz := min(base[0]+x, n-1), min(base[1]+y, n-1), min(base[2]+z, n-1)
				stored += weight * float64(values[(iz*n+iy)*n+ix])
			}
		}
	}
	return stored*affine.Slope + affine.Intercept, true
}

func TestPacked16AffineTrilinearAnalyticAndFormerPath(t *testing.T) {
	for _, signed := range []bool{false, true} {
		for _, shape := range []struct{ bits, high uint16 }{{16, 15}, {12, 11}, {12, 15}} {
			for _, n := range []int{1, 2, 3} {
				for _, affine := range []Rescale{{Slope: 1}, {Slope: -1.25, Intercept: 1024}, {Slope: 0.1234567890123, Intercept: -17.25}, {Slope: 0.25, Intercept: 1e5}} {
					t.Run(fmt.Sprintf("signed%t/bits%d-high%d/n%d/slope%g-intercept%g", signed, shape.bits, shape.high, n, affine.Slope, affine.Intercept), func(t *testing.T) {
						stack, values := packed16Issue1307Stack(n, signed, shape.bits, shape.high, affine)
						vol, err := BuildVolume(stack)
						if err != nil {
							t.Fatal(err)
						}
						defer vol.Close()
						reader, err := vol.AcquireReader()
						if err != nil {
							t.Fatal(err)
						}
						defer reader.Close()
						if !reader.sampler.packed16Affine {
							t.Fatal("qualified affine did not select packed path")
						}
						old := *reader.sampler
						old.packed16Affine = false
						points := []Vec3{{}, {X: float64(n - 1), Y: float64(n - 1), Z: float64(n - 1)}, {X: -0.25, Y: 0.1, Z: 0.2}, {X: float64(n) - 0.501, Y: 0.1, Z: 0.2}, {X: -0.502}, {X: float64(n) + 1}, {X: math.NaN()}, {Z: math.Inf(1)}}
						for i := 0; i < 31; i++ {
							points = append(points, Vec3{X: float64(i%11) / 10 * float64(n-1), Y: float64(i%7) / 6 * float64(n-1), Z: float64(i%5) / 4 * float64(n-1)})
						}
						for _, p := range points {
							got, ok := reader.TrilinearAt(p)
							want, wok := packed16Issue1307Analytic(p, n, values, affine)
							before, bok := old.trilinearAt(p)
							if ok != wok || ok != bok || math.Abs(got-want) > packed16AffineErrorBudget || math.Abs(got-before) > packed16AffineErrorBudget {
								t.Fatalf("p%v got%.17g/%t analytic%.17g/%t former%.17g/%t", p, got, ok, want, wok, before, bok)
							}
							if p.X == math.Trunc(p.X) && p.Y == math.Trunc(p.Y) && p.Z == math.Trunc(p.Z) && ok && math.Float64bits(got) != math.Float64bits(before) {
								t.Fatal("integer fast path changed")
							}
						}
					})
				}
			}
		}
	}
}

func TestPacked16AffineTrilinearLargeRescaleFallback(t *testing.T) {
	for _, affine := range []Rescale{{Slope: 1, Intercept: 1e12}, {Slope: -1e12, Intercept: 1e16}, {Slope: 1e30}} {
		stack, _ := packed16Issue1307Stack(3, true, 16, 15, affine)
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := vol.AcquireReader()
		if err != nil {
			t.Fatal(err)
		}
		if !reader.sampler.packed16() || reader.sampler.packed16Affine {
			t.Fatal("large rescale did not retain old packed path")
		}
		p := Vec3{X: 0.31, Y: 1.79, Z: 0.43}
		got, ok := reader.TrilinearAt(p)
		sample, bok := reader.sampler.voxelSample(p)
		if ok != bok || math.Float64bits(got) != math.Float64bits(sample.value()) {
			t.Fatal("fallback changed bits")
		}
		reader.Close()
		vol.Close()
	}
	for _, slope := range []float64{math.NaN(), math.Inf(1)} {
		d := testVolumeDescriptor(2, 2, 2)
		d.ScalarFormat, d.SampleDomain, d.RescaleSlope = VolumeScalarU16StoredLE, VolumeSampleDomainStored, slope
		d.RowStrideBytes, d.SliceStrideBytes, d.ByteLength = 4, 8, 16
		store := NewVolumeStore()
		if _, err := store.Replace(VolumeInput{Descriptor: d, Payload: make([]byte, 16)}); !errors.Is(err, ErrInvalidVolumeSnapshot) {
			t.Fatalf("nonfinite descriptor error%v", err)
		}
		store.Close()
	}
}

func TestPacked16AffineTrilinearVTKFixture(t *testing.T) {
	var fixture vtkResliceFixture
	loadVTKFixture(t, "reslice.json", &fixture)
	for _, affine := range []Rescale{{Slope: 1}, {Slope: -0.75, Intercept: 1024}, {Slope: 0.1234567890123, Intercept: -17.25}} {
		stack := fixture.stack()
		for _, frame := range stack.Frames {
			frame.Rescale = affine
		}
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := vol.AcquireReader()
		if err != nil {
			t.Fatal(err)
		}
		if !reader.sampler.packed16Affine {
			t.Fatal("fixture did not select optimization")
		}
		for i, p := range fixture.Points {
			q := Vec3{X: math.RoundToEven(p[0]*65536) / 65536, Y: math.RoundToEven(p[1]*65536) / 65536, Z: math.RoundToEven(p[2]*65536) / 65536}
			got, ok := reader.TrilinearAt(q)
			want := fixture.Linear[i]*affine.Slope + affine.Intercept
			if !ok || math.Abs(got-want) > packed16AffineErrorBudget {
				t.Fatalf("point%v got%.17g wantVTKaffine%.17g", p, got, want)
			}
		}
		reader.Close()
		vol.Close()
	}
}

// Byte-backed canonical data exercises the untouched per-corner path with
// identical stored integers, rescale and geometry, including CPR's public API.
func packed16Issue1307ByteVolume(t *testing.T, stack *Stack) *Volume {
	t.Helper()
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := vol.AcquireSnapshot()
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := lease.Snapshot()
	if err != nil {
		t.Fatal(err)
	}
	d, err := snapshot.Descriptor()
	if err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	if err := snapshot.WriteStored16Context(context.Background(), &data); err != nil {
		t.Fatal(err)
	}
	lease.Release()
	generation, err := vol.store.Replace(VolumeInput{Descriptor: d, Payload: data.Bytes()})
	if err != nil {
		t.Fatal(err)
	}
	vol.snapshotMu.Lock()
	vol.generation = generation
	vol.snapshotMu.Unlock()
	t.Cleanup(func() { vol.Close() })
	return vol
}

func packed16Issue1307ImageDifference(t *testing.T, a, b image.Image, maximum uint8) {
	t.Helper()
	if a == nil || b == nil || a.Bounds() != b.Bounds() {
		t.Fatal("image bounds differ")
	}
	for y := a.Bounds().Min.Y; y < a.Bounds().Max.Y; y++ {
		for x := a.Bounds().Min.X; x < a.Bounds().Max.X; x++ {
			xa, xb := color.GrayModel.Convert(a.At(x, y)).(color.Gray).Y, color.GrayModel.Convert(b.At(x, y)).(color.Gray).Y
			d := int(xa) - int(xb)
			if d < 0 {
				d = -d
			}
			if d > int(maximum) {
				t.Fatalf("pixel(%d,%d) delta%d >%d", x, y, d, maximum)
			}
		}
	}
}

func TestPacked16AffineTrilinearMPRSlabAndCPRPresentation(t *testing.T) {
	for _, affine := range []Rescale{{Slope: -1.25, Intercept: 1024}, {Slope: 0.1234567890123, Intercept: -17.25}} {
		stack, _ := packed16Issue1307Stack(16, true, 12, 15, affine)
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		defer vol.Close()
		baseline := packed16Issue1307ByteVolume(t, stack)
		plane := obliqueInteriorPlane(vol)
		windows := []WindowLevel{{Center: affine.Intercept, Width: 7, Function: display.VOILinear}, {Center: affine.Intercept, Width: 0.01, Function: display.VOILinearExact}, {Center: affine.Intercept, Width: 0.0001, Function: display.VOISigmoid}}
		for _, mode := range []SlabMode{SlabNone, SlabMIP, SlabMinIP, SlabAverage, SlabSum} {
			a, err := ResliceObliqueSlabScalarContext(context.Background(), vol, plane, 31, 29, 3, mode)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ResliceObliqueSlabScalarContext(context.Background(), baseline, plane, 31, 29, 3, mode)
			if err != nil {
				t.Fatal(err)
			}
			for i, v := range a.values {
				if a.valid[i] != b.valid[i] || math.Abs(v-b.values[i]) > 3*packed16AffineErrorBudget {
					t.Fatalf("slab%v value%d differs", mode, i)
				}
			}
			// Put one actual sample inside narrow windows rather than merely
			// checking saturated output far from the center.
			windows[1].Center = b.values[len(b.values)/2]
			windows[2].Center = b.values[len(b.values)/2]
			for _, window := range windows {
				packed16Issue1307ImageDifference(t, a.ApplyWindow(window), b.ApplyWindow(window), 1)
			}
		}
		path := NewCPRPath([]Vec3{vol.VoxelToPatient(Vec3{X: 7.3, Y: 2.1, Z: 6.2}), vol.VoxelToPatient(Vec3{X: 8.7, Y: 12.4, Z: 8.3})})
		for _, mode := range []CPRMode{CPRStraightened, CPRStretched, CPRTransverse, CPRSlab} {
			for _, window := range windows {
				req := CPRRequest{Mode: mode, Volume: vol, Path: path, Width: 17, ArcSpacing: 0.7, CrossSpacing: 0.31, ArcLength: path.Length() / 2, Thickness: 3, SlabMode: SlabAverage, Window: window}
				a, err := RenderCPR(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				req.Volume = baseline
				b, err := RenderCPR(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				packed16Issue1307ImageDifference(t, a, b, 1)
			}
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if image, err := RenderCPR(ctx, CPRRequest{Volume: vol, Path: path, Width: 17}); image != nil || !errors.Is(err, context.Canceled) {
			t.Fatal("canceled CPR published output")
		}
	}
}

func TestPacked16AffineTrilinearKeepsCornersAndLease(t *testing.T) {
	stack, _ := packed16Issue1307Stack(3, true, 16, 15, Rescale{Slope: -1.25, Intercept: 1024})
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := vol.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	p := Vec3{X: 0.31, Y: 1.79, Z: 0.43}
	before, ok := reader.TrilinearAt(p)
	if !ok {
		t.Fatal("point missing")
	}
	sample, ok := reader.sampler.voxelSample(p)
	if !ok {
		t.Fatal("corner sample missing")
	}
	if sample.c000 != reader.sampler.packed16At((sample.z0*3+sample.y0)*3+sample.x0) {
		t.Fatal("modality corner semantics changed")
	}
	vol.Close()
	after, ok := reader.TrilinearAt(p)
	if !ok || after != before {
		t.Fatal("leased generation did not survive volume close")
	}
	reader.Close()
	if _, ok := reader.TrilinearAt(p); ok {
		t.Fatal("closed reader returned sample")
	}
	if stats := vol.VolumeStoreStats(); stats.LiveBytes != 0 || stats.ActiveLeases != 0 {
		t.Fatalf("lease leaked %+v", stats)
	}
}

func TestPacked16AffineTrilinearFloat32AndPaddedFallback(t *testing.T) {
	vol, err := BuildVolume(stack16(3, true, func(z int) Rescale { return Rescale{Slope: 1, Intercept: float64(z)} }, ctValue))
	if err != nil {
		t.Fatal(err)
	}
	defer vol.Close()
	reader, err := vol.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if reader.sampler.packed16Affine || len(reader.sampler.values) == 0 {
		t.Fatal("per-slice rescale did not keep float32 path")
	}
	p := Vec3{X: 0.3, Y: 1.7, Z: 0.4}
	got, ok := reader.TrilinearAt(p)
	sample, bok := reader.sampler.voxelSample(p)
	if ok != bok || math.Float64bits(got) != math.Float64bits(sample.value()) {
		t.Fatal("float32 path changed")
	}

	store := NewVolumeStore()
	defer store.Close()
	d := testVolumeDescriptor(2, 2, 2)
	d.ScalarFormat, d.SampleDomain, d.RescaleSlope, d.RescaleIntercept = VolumeScalarI16StoredLE, VolumeSampleDomainStored, 0.5, -17
	d.RowStrideBytes, d.SliceStrideBytes, d.ByteLength = 6, 12, 24
	payload := make([]byte, 24)
	for z := 0; z < 2; z++ {
		for y := 0; y < 2; y++ {
			for x := 0; x < 2; x++ {
				binary.LittleEndian.PutUint16(payload[z*12+y*6+x*2:], uint16(x+2*y+4*z))
			}
			binary.LittleEndian.PutUint16(payload[z*12+y*6+4:], 0x7fff)
		}
	}
	generation, err := store.Replace(VolumeInput{Descriptor: d, Payload: payload})
	if err != nil {
		t.Fatal(err)
	}
	padded := &Volume{Cols: 2, Rows: 2, Depth: 2, ColSpacing: 1, RowSpacing: 1, SliceSpacing: 1, store: store, generation: generation}
	r, err := padded.AcquireReader()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if r.sampler.packed16() || r.sampler.packed16Affine {
		t.Fatal("byte/padded data selected packed kernel")
	}
	value, ok := r.TrilinearAt(Vec3{X: 0.5, Y: 0.5, Z: 0.5})
	if !ok || value != 3.5*0.5-17 {
		t.Fatalf("padded value%g/%t", value, ok)
	}
}
