package render

import (
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// Baseline-only observation file: no optimized sampler or production edits.
// Both formats encode the same scalar field. Warm canonical payload preparation
// and path construction occur before timing. The profile flag may still include
// process/fixture setup; use line attribution, not total process time as kernel.
func cpr1432BaselineVolume(tb testing.TB, packed bool) *Volume {
	tb.Helper()
	const n = 64
	ax := Vec3{X: .8, Y: .6}
	ay := Vec3{X: -.3, Y: .4, Z: math.Sqrt(.75)}
	normal := ax.Cross(ay)
	origin := Vec3{X: 41, Y: -23, Z: 7}
	stack := &Stack{PixelSpacing: []float64{1.3, .7}, SliceThickness: 2.1}
	bits := uint16(32)
	bytesPer := 4
	if packed {
		bits = 16
		bytesPer = 2
	}
	for z := 0; z < n; z++ {
		data := make([]byte, n*n*bytesPer)
		for y := 0; y < n; y++ {
			for x := 0; x < n; x++ {
				stored := int32(37*x + 11*y + 5*z - 3000)
				offset := (y*n + x) * bytesPer
				if packed {
					binary.LittleEndian.PutUint16(data[offset:], uint16(int16(stored)))
				} else {
					binary.LittleEndian.PutUint32(data[offset:], uint32(stored))
				}
			}
		}
		pos := origin.Add(normal.Scale(float64(z) * 2.1))
		stack.Frames = append(stack.Frames, &Frame{Metadata: pixeldata.Metadata{Rows: n, Columns: n, SamplesPerPixel: 1, BitsAllocated: bits, BitsStored: bits, HighBit: bits - 1, PixelRepresentation: 1, PhotometricInterpretation: "MONOCHROME2"}, ByteOrder: binary.LittleEndian, PixelBytes: data, Rescale: Rescale{Slope: .5, Intercept: -1024}, ImageOrientation: []float64{ax.X, ax.Y, ax.Z, ay.X, ay.Y, ay.Z}, ImagePosition: []float64{pos.X, pos.Y, pos.Z}})
	}
	vol, err := BuildVolume(stack)
	if err != nil {
		tb.Fatal(err)
	}
	tb.Cleanup(func() { _ = vol.Close() })
	sampler, err := newVolumeSamplerWithError(vol)
	if err != nil {
		tb.Fatal(err)
	}
	if packed != sampler.packed16() || (!packed && len(sampler.values) == 0) {
		sampler.Close()
		tb.Fatal("fixture payload format not qualified")
	}
	// Independent affine field oracle at interior fractional voxel locations.
	for i := 0; i < 17; i++ {
		x, y, z := 5.125+float64(i), 11.375+float64(i)/3, 8.625+float64(i)/4
		patient := origin.Add(ax.Scale(x * .7)).Add(ay.Scale(y * 1.3)).Add(normal.Scale(z * 2.1))
		got, ok := sampler.trilinearAt(vol.PatientToVoxel(patient))
		want := .5*(37*x+11*y+5*z-3000) - 1024
		if !ok || math.Abs(got-want) > 1e-9 {
			sampler.Close()
			tb.Fatal("independent analytic field mismatch")
		}
	}
	sampler.Close()
	return vol
}
func cpr1432BaselineRequest(tb testing.TB, packed bool, mode CPRMode, interpolation Interpolation) CPRRequest {
	tb.Helper()
	vol := cpr1432BaselineVolume(tb, packed)
	points := make([]Vec3, 64)
	for i := range points {
		a := float64(i) / float64(len(points)-1)
		theta := 3 * math.Pi * a
		points[i] = vol.VoxelToPatient(Vec3{X: 32 + 12*math.Cos(theta), Y: 32 + 12*math.Sin(theta), Z: 12 + 26*a})
	}
	path, err := NewCPRPathChecked(points)
	if err != nil {
		tb.Fatal(err)
	}
	req := CPRRequest{Mode: mode, Volume: vol, Path: path, Window: WindowLevel{Center: -1700.25, Width: 600.125}, Width: 128, ArcSpacing: .6, CrossSpacing: .35, ArcLength: path.Length() / 2, Interpolation: interpolation, RotationDegrees: 37, StretchDir: vol.AxisY}
	if mode == CPRSlab {
		req.Thickness = 9
		req.SlabMode = SlabAverage
	}
	return req
}

func TestCPRPreparedBaselineFixtureAnalyticAndWarmFormats(t *testing.T) {
	for _, packed := range []bool{false, true} {
		req := cpr1432BaselineRequest(t, packed, CPRTransverse, InterpolationLinear)
		before := req.Volume.VolumeStoreStats()
		out, err := RenderCPRScalar(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if out.Width != 128 || out.Height != 128 || out.Plane == nil || out.ColumnSpacing != .35 || out.RowSpacing != .35 {
			t.Fatal("scalar dimensions/units")
		}
		valid := 0
		for _, ok := range out.Valid {
			if ok {
				valid++
			}
		}
		if valid == 0 {
			t.Fatal("empty phantom")
		}
		after := req.Volume.VolumeStoreStats()
		if before.CurrentGeneration != after.CurrentGeneration || before.LiveBytes != after.LiveBytes {
			t.Fatal("render rebuilt warmed volume")
		}
	}
}

// 24 complete frames:4 modes x3 interpolators x2 payload formats. ns/op is
// ns/frame, includes all request-owned bases/resampling/sampler lease/output/
// row workers but excludes fixture/path construction and initial decode/IO.
func BenchmarkCPRPreparedBaseline(b *testing.B) {
	for _, packed := range []bool{false, true} {
		format := "float32"
		if packed {
			format = "packed16"
		}
		for _, view := range []struct {
			name string
			mode CPRMode
		}{{"straightened", CPRStraightened}, {"stretched", CPRStretched}, {"transverse", CPRTransverse}, {"slab9", CPRSlab}} {
			for _, interp := range []struct {
				name string
				mode Interpolation
			}{{"nearest", InterpolationNearest}, {"linear", InterpolationLinear}, {"cubic", InterpolationCubic}} {
				b.Run(fmt.Sprintf("%s/%s/%s", format, view.name, interp.name), func(b *testing.B) {
					req := cpr1432BaselineRequest(b, packed, view.mode, interp.mode)
					preflight, err := RenderCPR(context.Background(), req)
					if err != nil {
						b.Fatal(err)
					}
					b.Logf("synthetic preflight dimensions=%dx%d SHA256=%x volumeBytes=%d", preflight.Bounds().Dx(), preflight.Bounds().Dy(), packed16Issue1307BenchHash(preflight), req.Volume.VolumeStoreStats().LiveBytes)
					b.ReportAllocs()
					b.ResetTimer()
					for i := 0; i < b.N; i++ {
						benchmarkParallelRenderImage, err = RenderCPR(context.Background(), req)
						if err != nil {
							b.Fatal(err)
						}
					}
				})
			}
		}
	}
}

// This is a separated current preparation baseline, not a proposed fast path.
// It retains no plan between requests and closes each acquired sampler.
func BenchmarkCPRPreparedBaselinePreparation(b *testing.B) {
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
		benchmarkCPRFrame = samples[len(samples)-1]
		packed16Issue1307SampleSink = float64(len(offsets))
		sampler.Close()
	}
}
