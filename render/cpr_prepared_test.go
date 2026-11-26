package render

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"image"
	"math"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const cpr1432TubeN = 24

func cpr1432TubeStored(x, y, z int) float64 {
	a := float64(z) / 23
	cx, cy := 12+4*math.Cos(2*math.Pi*a), 12+4*math.Sin(2*math.Pi*a)
	return math.Round(1200*math.Exp(-((float64(x)-cx)*(float64(x)-cx)+(float64(y)-cy)*(float64(y)-cy))/8)) + float64(7*x+3*y+2*z) - 700
}
func cpr1432Tube(t *testing.T, packed bool, unsignedOption ...bool) (*Volume, *CPRPath) {
	t.Helper()
	ax, ay := Vec3{X: .8, Y: .6}, Vec3{X: -.3, Y: .4, Z: math.Sqrt(.75)}
	nz := ax.Cross(ay)
	origin := Vec3{41, -23, 7}
	unsigned := len(unsignedOption) > 0 && unsignedOption[0]
	representation := uint16(1)
	intercept := -1024.0
	if unsigned {
		representation = 0
		intercept -= 16384
	}
	stack := &Stack{PixelSpacing: []float64{1.3, .7}, SliceThickness: 2.1}
	bits, per := uint16(32), 4
	if packed {
		bits, per = 16, 2
	}
	for z := 0; z < cpr1432TubeN; z++ {
		data := make([]byte, cpr1432TubeN*cpr1432TubeN*per)
		for y := 0; y < cpr1432TubeN; y++ {
			for x := 0; x < cpr1432TubeN; x++ {
				v := int32(cpr1432TubeStored(x, y, z))
				if unsigned {
					v += 32768
				}
				o := (y*cpr1432TubeN + x) * per
				if packed {
					binary.LittleEndian.PutUint16(data[o:], uint16(int16(v)))
				} else {
					binary.LittleEndian.PutUint32(data[o:], uint32(v))
				}
			}
		}
		p := origin.Add(nz.Scale(float64(z) * 2.1))
		stack.Frames = append(stack.Frames, &Frame{Metadata: pixeldata.Metadata{Rows: cpr1432TubeN, Columns: cpr1432TubeN, SamplesPerPixel: 1, BitsAllocated: bits, BitsStored: bits, HighBit: bits - 1, PixelRepresentation: representation, PhotometricInterpretation: "MONOCHROME2"}, ByteOrder: binary.LittleEndian, PixelBytes: data, Rescale: Rescale{Slope: .5, Intercept: intercept}, ImageOrientation: []float64{ax.X, ax.Y, ax.Z, ay.X, ay.Y, ay.Z}, ImagePosition: []float64{p.X, p.Y, p.Z}})
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = vol.Close() })
	// Residency comparisons begin after the existing lazy canonical payload
	// has been materialized, as in the frozen baseline fixture. This warms no
	// prepared bases or interval hints: those remain request-local.
	sampler, err := newVolumeSamplerWithError(vol)
	if err != nil {
		t.Fatal(err)
	}
	if sampler.packed16() != packed || (!packed && len(sampler.values) == 0) {
		sampler.Close()
		t.Fatal("tube payload format")
	}
	sampler.Close()
	if stats := vol.VolumeStoreStats(); stats.LiveBytes == 0 || stats.ActiveLeases != 0 {
		t.Fatalf("warm tube must own payload without read leases: %#v", stats)
	}
	pts := make([]Vec3, 25)
	for i := range pts {
		// Keep the independent continuous-coordinate oracle away from exact
		// nearest-neighbour half-voxel ties. Dedicated tie tests cover those;
		// equivalent inverse affines may round opposite sides by one ulp.
		z := 3.00123 + 17*float64(i)/24
		a := z / 23
		pts[i] = vol.VoxelToPatient(Vec3{12 + 4*math.Cos(2*math.Pi*a), 12 + 4*math.Sin(2*math.Pi*a), z})
	}
	path, err := NewCPRPathChecked(pts)
	if err != nil {
		t.Fatal(err)
	}
	return vol, path
}
func cpr1432AssertScalar(t *testing.T, got, want *CPRScalar) {
	t.Helper()
	if got.Width != want.Width || got.Height != want.Height || got.ColumnSpacing != want.ColumnSpacing || got.RowSpacing != want.RowSpacing || !reflect.DeepEqual(got.Plane, want.Plane) || !reflect.DeepEqual(got.Valid, want.Valid) {
		t.Fatal("scalar layout/plane/spacing/validity changed")
	}
	for i, v := range got.Values {
		if math.Float64bits(v) != math.Float64bits(want.Values[i]) {
			t.Fatalf("scalar[%d] = %.17g want %.17g", i, v, want.Values[i])
		}
	}
}
func cpr1432AssertRequest(t *testing.T, req CPRRequest) {
	t.Helper()
	if req.SlabMode == SlabSum {
		cpr1432AssertUnsupportedSlabSum(t, req)
		return
	}
	got, err := RenderCPRScalar(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	want, err := cpr1432ReferenceScalar(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	cpr1432AssertScalar(t, got, want)
	valid := 0
	for _, ok := range got.Valid {
		if ok {
			valid++
		}
	}
	if valid == 0 {
		t.Fatal("empty comparison fixture")
	}
	img, err := RenderCPR(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	gray, err := cpr1432ReferenceGray(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	actual, ok := img.(*image.Gray)
	if !ok || actual.Rect != gray.Rect || actual.Stride != gray.Stride || !bytes.Equal(actual.Pix, gray.Pix) {
		t.Fatal("Gray bytes/layout changed")
	}
}
func TestCPRPreparedCurvedFullOutputExact(t *testing.T) {
	for _, format := range []struct{ packed, unsigned bool }{{false, false}, {true, false}, {true, true}} {
		packed := format.packed
		vol, path := cpr1432Tube(t, packed, format.unsigned)
		for _, mode := range []CPRMode{CPRStraightened, CPRStretched, CPRTransverse, CPRSlab} {
			for _, interp := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
				for _, rot := range []float64{0, 37, 90} {
					for _, slab := range []struct {
						mode  SlabMode
						count int
						mm    float64
					}{{SlabNone, 0, 0}, {SlabAverage, 5, 0}, {SlabMIP, 0, 2.4}, {SlabMinIP, 3, 0}, {SlabSum, 3, 0}} {
						t.Run(fmt.Sprintf("packed%t/unsigned%t/mode%d/interp%d/rot%g/slab%d", packed, format.unsigned, mode, interp, rot, slab.mode), func(t *testing.T) {
							req := CPRRequest{Volume: vol, Path: path, Mode: mode, Interpolation: interp, Width: 17, ArcSpacing: 1.4, CrossSpacing: 2, ArcLength: path.Length() / 2, StretchDir: vol.AxisY, RotationDegrees: rot, SlabMode: slab.mode, Thickness: slab.count, ThicknessMM: slab.mm, Window: WindowLevel{Center: -1380.25, Width: 31.125}}
							cpr1432AssertRequest(t, req)
						})
					}
				}
			}
		}
	}
}

// Independent determinant inverse and Rodrigues rotation do not call the
// production patient transform, Vec3.Rotate or rotateCPRBasis.
func cpr1432Inverse(v *Volume, p Vec3) Vec3 {
	a, b, c := v.AxisX.Scale(v.ColSpacing), v.AxisY.Scale(v.RowSpacing), v.Normal.Scale(v.SliceSpacing)
	d := p.Sub(v.Origin)
	det := a.Dot(b.Cross(c))
	return Vec3{d.Dot(b.Cross(c)) / det, d.Dot(c.Cross(a)) / det, d.Dot(a.Cross(b)) / det}
}
func cpr1432Rotate(v, axis Vec3, deg float64) Vec3 {
	a := deg * math.Pi / 180
	return v.Scale(math.Cos(a)).Add(axis.Cross(v).Scale(math.Sin(a))).Add(axis.Scale(axis.Dot(v) * (1 - math.Cos(a))))
}
func cpr1432Weights(x float64, mode Interpolation) ([]int, []float64) {
	base := int(math.Floor(x))
	f := x - float64(base)
	if mode == InterpolationNearest {
		return []int{int(math.Floor(x + .5))}, []float64{1}
	}
	if mode != InterpolationCubic {
		return []int{base, base + 1}, []float64{1 - f, f}
	}
	return []int{base - 1, base, base + 1, base + 2}, []float64{-.5*f + f*f - .5*f*f*f, 1 - 2.5*f*f + 1.5*f*f*f, .5*f + 2*f*f - 1.5*f*f*f, -.5*f*f + .5*f*f*f}
}
func cpr1432IndependentSample(p Vec3, mode Interpolation) float64 {
	xi, xw := cpr1432Weights(p.X, mode)
	yi, yw := cpr1432Weights(p.Y, mode)
	zi, zw := cpr1432Weights(p.Z, mode)
	sum := 0.0
	for i, x := range xi {
		for j, y := range yi {
			for k, z := range zi {
				sum += xw[i] * yw[j] * zw[k] * (.5*cpr1432TubeStored(x, y, z) - 1024)
			}
		}
	}
	return sum
}
func TestCPRPreparedIndependentGeometryWeightsAndUnits(t *testing.T) {
	for _, packed := range []bool{false, true} {
		vol, path := cpr1432Tube(t, packed)
		frame := path.FrameAt(path.Length() / 2)
		normal := cpr1432Rotate(frame.Normal, frame.Tangent, 37)
		binormal := frame.Tangent.Cross(normal)
		samples, err := path.ResampleChecked(1.4)
		if err != nil {
			t.Fatal(err)
		}
		bases, err := prepareCPRBases(context.Background(), CPRRequest{RotationDegrees: 37}, samples, false)
		if err != nil {
			t.Fatal(err)
		}
		for i, s := range samples {
			assertVec3Near(t, bases[i].cross, cpr1432Rotate(s.Normal, s.Tangent, 37), 1e-10)
			assertVec3Near(t, bases[i].slab, s.Tangent.Cross(cpr1432Rotate(s.Normal, s.Tangent, 37)), 1e-10)
		}
		for _, mode := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
			req := CPRRequest{Volume: vol, Path: path, Mode: CPRTransverse, Width: 7, CrossSpacing: .3, ArcLength: path.Length() / 2, RotationDegrees: 37, Interpolation: mode}
			got, err := RenderCPRScalar(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			origin := frame.Position.Add(normal.Scale(-.9)).Add(binormal.Scale(-.9))
			assertVec3Near(t, got.Plane.Origin, origin, 1e-10)
			assertVec3Near(t, got.Plane.U, normal.Scale(1.8), 1e-10)
			assertVec3Near(t, got.Plane.V, binormal.Scale(1.8), 1e-10)
			if got.ColumnSpacing != .3 || got.RowSpacing != .3 || got.Width != 7 || got.Height != 7 {
				t.Fatal("export units/layout")
			}
			for y := 0; y < 7; y++ {
				for x := 0; x < 7; x++ {
					patient := frame.Position.Add(normal.Scale(float64(x-3) * .3)).Add(binormal.Scale(float64(y-3) * .3))
					voxel := cpr1432Inverse(vol, patient)
					assertVec3Near(t, vol.PatientToVoxel(patient), voxel, 1e-10)
					want := cpr1432IndependentSample(voxel, mode)
					i := y*7 + x
					if !got.Valid[i] || math.Abs(got.Values[i]-want) > 1e-8 {
						t.Fatalf("independent value pixel%d got%.17g want%.17g", i, got.Values[i], want)
					}
				}
			}
		}
	}
}
func TestCPRPreparedIntervalTiesMissesAndBordersExact(t *testing.T) {
	vol := &Volume{Origin: Vec3{7, -3, 11}, AxisX: Vec3{X: 1}, AxisY: Vec3{Y: 1}, Normal: Vec3{Z: 1}, ColSpacing: .7, RowSpacing: 1.3, SliceSpacing: 2.1, regular: true, positions: []float64{0, 2.100000000001, 4.2, 6.300000000002, 8.4}}
	mapping := prepareCPRMapping(vol, vol)
	if !mapping.regular {
		t.Fatal("regular mapper not admitted")
	}
	hint := -1
	values := []float64{-1, 0, math.Copysign(0, -1), 1, 2, 2.100000000001, math.Nextafter(2.100000000001, 0), math.Nextafter(2.100000000001, math.Inf(1)), 4.3, 3, 7, 8.4, 9, -2, 1}
	for _, pos := range values {
		patient := Vec3{vol.Origin.X + .35, vol.Origin.Y - .65, vol.Origin.Z + pos}
		got, want := mapping.at(patient, &hint), vol.PatientToVoxel(patient)
		if math.Float64bits(got.X) != math.Float64bits(want.X) || math.Float64bits(got.Y) != math.Float64bits(want.Y) || math.Float64bits(got.Z) != math.Float64bits(want.Z) {
			t.Fatalf("mapping changed pos%g hint%d: %#v %#v", pos, hint, got, want)
		}
	}
	for _, positions := range [][]float64{nil, {0}, {0, 0, 2, 4}} {
		vol.positions = positions
		hint = -1
		for _, pos := range values {
			got, want := mapping.positionToIndex(pos, &hint), vol.positionToIndex(pos)
			if math.Float64bits(got) != math.Float64bits(want) {
				t.Fatal("degenerate positions changed")
			}
		}
	}
	req := cpr1432BaselineRequest(t, true, CPRSlab, InterpolationNearest)
	sampler, err := newVolumeSamplerWithError(req.Volume)
	if err != nil {
		t.Fatal(err)
	}
	defer sampler.Close()
	m := prepareCPRMapping(req.Volume, sampler.vol)
	for _, x := range []float64{math.Nextafter(-.5, math.Inf(-1)), -.5, math.Nextafter(-.5, 0), .5, math.Nextafter(.5, 0), math.Nextafter(.5, 1), 63.5, math.Nextafter(63.5, math.Inf(1))} {
		p := req.Volume.VoxelToPatient(Vec3{x, 12.25, 14.5})
		h := -1
		for _, interp := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
			a, ok := sampler.sampleAt(m.at(p, &h), interp)
			b, bok := sampler.sampleAt(sampler.vol.PatientToVoxel(p), interp)
			if ok != bok || math.Float64bits(a) != math.Float64bits(b) {
				t.Fatal("border/tie validity changed")
			}
		}
	}
}
func TestCPRPreparedTiltIrregularReferenceAndPhotometric(t *testing.T) {
	for _, origins := range [][]Vec3{{{0, 0, 0}, {0, 1, 2}, {0, 2, 4}}, {{0, 0, 0}, {0, 0, 1}, {0, 0, 3}}} {
		vol, err := BuildVolume(tiltedGradientRowStack(6, 8, origins, .5, .8))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vol.Close() })
		sampler, err := newVolumeSamplerWithError(vol)
		if err != nil {
			t.Fatal(err)
		}
		m := prepareCPRMapping(vol, sampler.vol)
		if m.regular {
			t.Fatal("corrected input must use reference")
		}
		h := -1
		p := vol.VoxelToPatient(Vec3{3, 2, 1})
		assertVec3Near(t, m.at(p, &h), sampler.vol.PatientToVoxel(p), 0)
		sampler.Close()
		path, err := NewCPRPathChecked([]Vec3{vol.VoxelToPatient(Vec3{3, 2, 0}), vol.VoxelToPatient(Vec3{3, 2, 1}), vol.VoxelToPatient(Vec3{3, 2, 2})})
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []CPRMode{CPRStraightened, CPRStretched, CPRTransverse, CPRSlab} {
			for _, interp := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
				cpr1432AssertRequest(t, CPRRequest{Volume: vol, Path: path, Mode: mode, Width: 5, ArcSpacing: .7, CrossSpacing: .7, ArcLength: path.Length() / 2, RotationDegrees: 37, Interpolation: interp, Thickness: 3, SlabMode: SlabAverage, Window: WindowLevel{Center: 100.125, Width: 12.125}})
			}
		}
	}
	// Existing sampler photometric/window behavior is exercised with both
	// interpretations without replacing the packed16 interpolation.
	for _, photo := range []string{"MONOCHROME1", "MONOCHROME2"} {
		stack := tiltedGradientRowStack(6, 8, []Vec3{{0, 0, 0}, {0, 0, 2}, {0, 0, 4}}, .5, .8)
		for _, f := range stack.Frames {
			f.Metadata.PhotometricInterpretation = photo
		}
		vol, err := BuildVolume(stack)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = vol.Close() })
		path, err := NewCPRPathChecked([]Vec3{{2, 1, 0}, {2, 1, 2}, {2, 1, 4}})
		if err != nil {
			t.Fatal(err)
		}
		cpr1432AssertRequest(t, CPRRequest{Volume: vol, Path: path, Mode: CPRSlab, Width: 5, ArcSpacing: .7, CrossSpacing: .3, RotationDegrees: 37, Interpolation: InterpolationLinear, Thickness: 3, SlabMode: SlabAverage, Window: WindowLevel{Center: 90.125, Width: 1.125}})
	}
}

type cpr1432CancelPreparation struct {
	context.Context
	calls int
}

func (c *cpr1432CancelPreparation) Err() error {
	c.calls++
	if c.calls >= 3 {
		return context.Canceled
	}
	return nil
}
func TestCPRPreparedScratchBoundAndCancel(t *testing.T) {
	if unsafe.Sizeof(cprPreparedBasis{}) != cprPreparedBasisBytes || maxCPRPreparedBasisBytes != 384*1024 {
		t.Fatal("scratch accounting changed")
	}
	if _, err := prepareCPRBases(context.Background(), CPRRequest{}, make([]CPRPathSample, MaxCPROutputDimension+1), false); !errors.Is(err, ErrCPRLimit) {
		t.Fatal("basis allocation limit")
	}
	if _, err := checkedCPRProduct("prepared_basis_bytes", maxCPRPreparedBasisBytes, ^uint64(0), cprPreparedBasisBytes); !errors.Is(err, ErrCPRLimit) {
		t.Fatal("overflow unchecked")
	}
	ctx := &cpr1432CancelPreparation{Context: context.Background()}
	if bases, err := prepareCPRBases(ctx, CPRRequest{}, make([]CPRPathSample, 130), false); !errors.Is(err, context.Canceled) || bases != nil {
		t.Fatal("preparation cancellation returns no partial bases")
	}
	req := cpr1432BaselineRequest(t, true, CPRSlab, InterpolationLinear)
	before := req.Volume.VolumeStoreStats()
	c, cancel := context.WithCancel(context.Background())
	cancel()
	if out, err := RenderCPR(c, req); !errors.Is(err, context.Canceled) || out != nil {
		t.Fatal("pre-cancel output")
	}
	if req.Volume.VolumeStoreStats().ActiveLeases != before.ActiveLeases {
		t.Fatal("cancel leaked lease")
	}
}
func TestCPRPreparedRequestLifetimeAndGeneration(t *testing.T) {
	vol, path := cpr1432Tube(t, true)
	before := vol.VolumeStoreStats()
	renderer := &CPRRenderer{}
	gen := renderer.NextGeneration()
	req := CPRRequest{Volume: vol, Path: path, Mode: CPRSlab, Width: 9, ArcSpacing: 2, CrossSpacing: .7, Thickness: 3, SlabMode: SlabAverage, Generation: gen}
	for i := 0; i < 24; i++ {
		req.RotationDegrees = float64(i)
		if _, err := renderer.Render(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	after := vol.VolumeStoreStats()
	if before.ActiveLeases != after.ActiveLeases || before.LiveBytes != after.LiveBytes || before.TrackedRecords != after.TrackedRecords {
		t.Fatal("request sequence retained lease/residency")
	}
	points := []Vec3{vol.VoxelToPatient(Vec3{12, 12, 3}), vol.VoxelToPatient(Vec3{15, 12, 12}), vol.VoxelToPatient(Vec3{12, 12, 20})}
	immutable, err := NewCPRPathChecked(points)
	if err != nil {
		t.Fatal(err)
	}
	captured := immutable.FrameAt(immutable.Length() / 2)
	points[1] = Vec3{}
	if immutable.FrameAt(immutable.Length()/2) != captured {
		t.Fatal("path aliases caller control points")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	err = sampleLongitudinalCPR(cancelled, req, false, func(int, int) {}, func(_, _ int, _ float64, _ string, _ bool) { seen.Add(1); cancel() })
	if !errors.Is(err, context.Canceled) || seen.Load() == 0 {
		t.Fatal("in-render cancellation not propagated")
	}
	if vol.VolumeStoreStats().ActiveLeases != before.ActiveLeases {
		t.Fatal("in-render cancellation leaked lease")
	}
	raceCtx := &cpr1432SupersedeContext{Context: context.Background(), renderer: renderer}
	if out, err := renderer.Render(raceCtx, req); !errors.Is(err, ErrCPRSuperseded) || out != nil {
		t.Fatal("mid-render generation stale output")
	}
	renderer.NextGeneration()
	if out, err := renderer.Render(context.Background(), req); !errors.Is(err, ErrCPRSuperseded) || out != nil {
		t.Fatal("edited generation published stale output")
	}
	other, otherPath := cpr1432Tube(t, false)
	req.Volume, req.Path = other, otherPath
	req.Generation = renderer.NextGeneration()
	if _, err := renderer.Render(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	sampler, err := newVolumeSamplerWithError(vol)
	if err != nil {
		t.Fatal(err)
	}
	mapping := prepareCPRMapping(vol, sampler.vol)
	patient := vol.VoxelToPatient(Vec3{12, 12, 12})
	hint := -1
	want, ok := sampler.sampleAt(mapping.at(patient, &hint), InterpolationLinear)
	if !ok {
		t.Fatal("lease preclose sample")
	}
	if err := vol.Close(); err != nil {
		t.Fatal(err)
	}
	hint = -1
	got, ok := sampler.sampleAt(mapping.at(patient, &hint), InterpolationLinear)
	if !ok || got != want {
		t.Fatal("source close invalidated held lease")
	}
	sampler.Close()
	if vol.VolumeStoreStats().ActiveLeases != 0 || vol.VolumeStoreStats().LiveBytes != 0 {
		t.Fatal("close did not release drained generation")
	}
}

// Deterministic generation change after the renderer's initial generation
// fence, rather than a timing-dependent sleep. Its Err is called by RenderCPR
// on the calling goroutine before workers start.
type cpr1432SupersedeContext struct {
	context.Context
	renderer *CPRRenderer
	once     atomic.Bool
}

func (c *cpr1432SupersedeContext) Err() error {
	if c.once.CompareAndSwap(false, true) {
		c.renderer.NextGeneration()
	}
	return c.Context.Err()
}

func cpr1432Unit(v Vec3) Vec3 { return v.Scale(1 / math.Sqrt(v.X*v.X+v.Y*v.Y+v.Z*v.Z)) }
func TestCPRPreparedIndependentCurvedAllModesAndSlabWeights(t *testing.T) {
	for _, packed := range []bool{false, true} {
		vol, path := cpr1432Tube(t, packed)
		for _, mode := range []CPRMode{CPRStraightened, CPRStretched, CPRTransverse, CPRSlab} {
			for _, interp := range []Interpolation{InterpolationNearest, InterpolationLinear, InterpolationCubic} {
				for _, slab := range []SlabMode{SlabNone, SlabAverage, SlabMIP, SlabMinIP, SlabSum} {
					req := CPRRequest{Volume: vol, Path: path, Mode: mode, Width: 7, ArcSpacing: 3, CrossSpacing: .3, ArcLength: path.Length() / 2, StretchDir: vol.AxisY, RotationDegrees: 37, Interpolation: interp, Thickness: 5, SlabMode: slab}
					if slab == SlabMIP {
						req.ThicknessMM = 2.4
					}
					if slab == SlabSum {
						cpr1432AssertUnsupportedSlabSum(t, req)
						continue
					}
					out, err := RenderCPRScalar(context.Background(), req)
					if err != nil {
						t.Fatal(err)
					}
					samples, err := path.ResampleChecked(3)
					if err != nil {
						t.Fatal(err)
					}
					for y := 0; y < out.Height; y++ {
						for x := 0; x < out.Width; x++ {
							frame := path.FrameAt(req.ArcLength)
							if mode != CPRTransverse {
								frame = samples[x]
							}
							n := frame.Normal
							if mode == CPRStretched {
								n = cpr1432Unit(vol.AxisY.Sub(frame.Tangent.Scale(vol.AxisY.Dot(frame.Tangent))))
							}
							n = cpr1432Unit(cpr1432Rotate(n, frame.Tangent, 37))
							b := cpr1432Unit(frame.Tangent.Cross(n))
							n = cpr1432Unit(b.Cross(frame.Tangent))
							base := frame.Position.Add(n.Scale(float64(y-3) * .3))
							dir := b
							if mode == CPRTransverse {
								base = frame.Position.Add(n.Scale(float64(x-3) * .3)).Add(b.Scale(float64(y-3) * .3))
								dir = frame.Tangent
							}
							offsets := []float64{0}
							if slab != SlabNone {
								offsets = []float64{-.6, -.3, 0, .3, .6}
								if req.ThicknessMM > 0 {
									offsets = []float64{-1.2, -.6, 0, .6, 1.2}
								}
							}
							want := 0.0
							for k, o := range offsets {
								voxel := cpr1432Inverse(vol, base.Add(dir.Scale(o)))
								value := cpr1432IndependentSample(voxel, interp)
								switch slab {
								case SlabMIP:
									if k == 0 || value > want {
										want = value
									}
								case SlabMinIP:
									if k == 0 || value < want {
										want = value
									}
								default:
									want += value
								}
							}
							if slab == SlabAverage || slab == SlabNone {
								want /= float64(len(offsets))
							}
							i := y*out.Width + x
							if !out.Valid[i] || math.Abs(out.Values[i]-want) > 1e-8 {
								t.Fatalf("independent full-output packed%t mode%d interp%d slab%d pixel%d got%.17g want%.17g", packed, mode, interp, slab, i, out.Values[i], want)
							}
						}
					}
				}
			}
		}
	}
}

func TestCPRPreparedConcurrentRequestsJoinBeforeLeaseRelease(t *testing.T) {
	vol, path := cpr1432Tube(t, true)
	before := vol.VolumeStoreStats()
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for worker := 0; worker < 4; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			req := CPRRequest{Volume: vol, Path: path, Mode: CPRSlab, Width: 33, ArcSpacing: 3, CrossSpacing: .7, Thickness: 3, SlabMode: SlabAverage, RotationDegrees: float64(worker * 13)}
			for i := 0; i < 2; i++ {
				a, err := RenderCPRScalar(context.Background(), req)
				if err != nil {
					errs <- err
					return
				}
				b, err := cpr1432ReferenceScalar(context.Background(), req)
				if err != nil {
					errs <- err
					return
				}
				if !reflect.DeepEqual(a, b) {
					errs <- fmt.Errorf("concurrent request %d differs", worker)
					return
				}
			}
		}(worker)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	if after := vol.VolumeStoreStats(); after.ActiveLeases != before.ActiveLeases || after.LiveBytes != before.LiveBytes {
		t.Fatal("concurrent requests leaked leases/residency")
	}
	req := CPRRequest{Volume: vol, Path: path, Mode: CPRSlab, Width: 33, ArcSpacing: 3, CrossSpacing: .7, Thickness: 3, SlabMode: SlabAverage}
	ctx, cancel := context.WithCancel(context.Background())
	var seen atomic.Int64
	err := sampleLongitudinalCPR(ctx, req, false, func(int, int) {}, func(_, _ int, _ float64, _ string, _ bool) { seen.Add(1); cancel() })
	if !errors.Is(err, context.Canceled) || seen.Load() == 0 {
		t.Fatal("parallel cancellation did not join")
	}
	if after := vol.VolumeStoreStats(); after.ActiveLeases != before.ActiveLeases || after.LiveBytes != before.LiveBytes {
		t.Fatal("parallel cancel leaked lease/residency")
	}
}

// Compare this additional request preparation to the frozen original
// preparation benchmark, reporting the extra request scratch honestly. The
// whole-frame before/after benchmark and its 24 cases remain byte-exact.
func BenchmarkCPRPreparedCandidatePreparation(b *testing.B) {
	req := cpr1432BaselineRequest(b, true, CPRStretched, InterpolationLinear)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		samples, err := req.Path.ResampleChecked(req.arcSpacing())
		if err != nil {
			b.Fatal(err)
		}
		offsets, err := cprSlabSampleOffsetsChecked(req, req.crossSpacing())
		if err != nil {
			b.Fatal(err)
		}
		sampler, err := newVolumeSamplerWithError(req.Volume)
		if err != nil {
			b.Fatal(err)
		}
		bases, err := prepareCPRBases(context.Background(), req, samples, true)
		if err != nil {
			sampler.Close()
			b.Fatal(err)
		}
		mapping := prepareCPRMapping(req.Volume, sampler.vol)
		benchmarkCPRFrame = samples[len(samples)-1]
		packed16Issue1307SampleSink = float64(len(offsets) + len(bases))
		_ = mapping
		sampler.Close()
	}
}

// CPR's historical public validation supports None/MIP/MinIP/Average,
// although the generic slab reducer also supports Sum. Retain the Sum cases
// and require the original typed rejection rather than inventing new support.
func cpr1432AssertUnsupportedSlabSum(t *testing.T, req CPRRequest) {
	t.Helper()
	cpr1432AssertOldUnsupportedSlabSum(t, req)
	var typed *CPRInputError
	if out, err := RenderCPRScalar(context.Background(), req); out != nil || !errors.As(err, &typed) || !errors.Is(err, ErrCPRInput) || typed.Field != "slab_mode" {
		t.Fatalf("scalar unsupported Sum: output%v error%v", out, err)
	}
	typed = nil
	if out, err := RenderCPR(context.Background(), req); out != nil || !errors.As(err, &typed) || !errors.Is(err, ErrCPRInput) || typed.Field != "slab_mode" {
		t.Fatalf("Gray unsupported Sum: output%v error%v", out, err)
	}
	typed = nil
	if err := cpr1432ReferenceValidateCPRRequest(req); !errors.As(err, &typed) || !errors.Is(err, ErrCPRInput) || typed.Field != "slab_mode" {
		t.Fatalf("frozen original Sum contract: %v", err)
	}
}

func cpr1432AssertOldUnsupportedSlabSum(t *testing.T, req CPRRequest) {
	t.Helper()
	var typed *CPRInputError
	if out, err := cpr1432ReferenceScalar(context.Background(), req); out != nil || !errors.As(err, &typed) || !errors.Is(err, ErrCPRInput) || typed.Field != "slab_mode" {
		t.Fatalf("old scalar Sum: output%v error%v", out, err)
	}
	typed = nil
	if out, err := cpr1432ReferenceGray(context.Background(), req); out != nil || !errors.As(err, &typed) || !errors.Is(err, ErrCPRInput) || typed.Field != "slab_mode" {
		t.Fatalf("old Gray Sum: output%v error%v", out, err)
	}
}
