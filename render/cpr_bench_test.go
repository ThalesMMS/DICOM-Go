package render

import (
	"context"
	"math"
	"testing"
)

var (
	benchmarkCPRFrame  CPRPathSample
	benchmarkCPRPathOK *CPRPath
)

// cprHelixControlPoints returns count control points on a helix of the given
// radius and pitch (mm per turn) around the z axis, centred on centre.
func cprHelixControlPoints(centre Vec3, radius, pitch float64, turns float64, count int) []Vec3 {
	points := make([]Vec3, count)
	for i := range points {
		theta := 2 * math.Pi * turns * float64(i) / float64(count-1)
		points[i] = centre.Add(Vec3{
			X: radius * math.Cos(theta),
			Y: radius * math.Sin(theta),
			Z: pitch * theta / (2 * math.Pi),
		})
	}
	return points
}

// BenchmarkCPRPathFrameAt queries frames at scattered arc lengths along a
// 512-point helical centreline, the access pattern of transverse sections
// dragged along a long vessel.
func BenchmarkCPRPathFrameAt(b *testing.B) {
	path := NewCPRPath(cprHelixControlPoints(Vec3{}, 20, 30, 4, 512))
	if path == nil {
		b.Fatal("helix path rejected")
	}
	length := path.Length()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCPRFrame = path.FrameAt(length * float64(i%997) / 996)
	}
}

// cprFrameAtTransportedPerCall recomputes the rotation-minimizing frame at s
// by transporting the seed frame along the spline from s = 0 on every call,
// the per-request cost the precomputed table removes. It exists only as the
// benchmark baseline and must agree with FrameAt.
func cprFrameAtTransportedPerCall(p *CPRPath, s float64) CPRPathSample {
	s = p.clampArc(s)
	k, span, t, exact := p.locate(s)
	prev := p.frames[0]
	normal := initialNormal(prev.tangent)
	for _, f := range p.frames[1 : k+1] {
		normal = rotationMinimizingNormal(prev.pos, prev.tangent, normal, f.pos, f.tangent)
		prev = f
	}
	position, tangent := prev.pos, prev.tangent
	if !exact {
		position = p.spans[span].point(t)
		tangent = p.tangentAtParam(span, t)
		normal = rotationMinimizingNormal(prev.pos, prev.tangent, normal, position, tangent)
	}
	return CPRPathSample{ArcLength: s, Position: position, Tangent: tangent, Normal: normal, Binormal: tangent.Cross(normal).Normalize()}
}

func TestCPRFrameAtTransportedPerCallMatchesFrameAt(t *testing.T) {
	path := NewCPRPath(cprHelixControlPoints(Vec3{}, 20, 30, 4, 512))
	for s := 0.0; s <= path.Length(); s += path.Length() / 37 {
		if got, want := cprFrameAtTransportedPerCall(path, s), path.FrameAt(s); got != want {
			t.Fatalf("s=%.3f: per-call transport %+v, precomputed %+v", s, got, want)
		}
	}
}

// BenchmarkCPRPathFrameAtTransportedPerCall is the baseline for
// BenchmarkCPRPathFrameAt: the same frames recomputed from the start of the
// path on every query.
func BenchmarkCPRPathFrameAtTransportedPerCall(b *testing.B) {
	path := NewCPRPath(cprHelixControlPoints(Vec3{}, 20, 30, 4, 512))
	length := path.Length()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCPRFrame = cprFrameAtTransportedPerCall(path, length*float64(i%997)/996)
	}
}

// BenchmarkNewCPRPath measures the one-off construction cost of a
// 512-point centreline.
func BenchmarkNewCPRPath(b *testing.B) {
	points := cprHelixControlPoints(Vec3{}, 20, 30, 4, 512)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkCPRPathOK = NewCPRPath(points)
	}
}

func benchmarkCurvedCPRRequest(b *testing.B, mode CPRMode) CPRRequest {
	b.Helper()
	vol := benchmarkParallelRenderVolume(b)
	centre := vol.VoxelToPatient(Vec3{X: 64, Y: 64, Z: 20})
	path := NewCPRPath(cprHelixControlPoints(centre, 30, 20, 2, 64))
	if path == nil {
		b.Fatal("helix path rejected")
	}
	return CPRRequest{
		Mode: mode, Volume: vol, Path: path, Width: 128,
		ArcSpacing: 1, CrossSpacing: 1, ArcLength: path.Length() / 2,
		Window: WindowLevel{Center: 128, Width: 256},
	}
}

func benchmarkCurvedCPR(b *testing.B, mode CPRMode) {
	req := benchmarkCurvedCPRRequest(b, mode)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		benchmarkParallelRenderImage, benchmarkParallelRenderErr = RenderCPR(context.Background(), req)
		if benchmarkParallelRenderErr != nil {
			b.Fatal(benchmarkParallelRenderErr)
		}
	}
}

// BenchmarkCPRCurvedStraightened renders a straightened CPR along a curved
// helical centreline (one frame per output column).
func BenchmarkCPRCurvedStraightened(b *testing.B) { benchmarkCurvedCPR(b, CPRStraightened) }

// BenchmarkCPRCurvedTransverse renders one transverse section of a curved
// centreline, dominated by the frame lookup for short outputs.
func BenchmarkCPRCurvedTransverse(b *testing.B) { benchmarkCurvedCPR(b, CPRTransverse) }
