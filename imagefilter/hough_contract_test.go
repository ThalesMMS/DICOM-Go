package imagefilter

import (
	"context"
	"errors"
	"image"
	"math"
	"reflect"
	"testing"
)

func TestHough2DValidationAdmissionOwnership(t *testing.T) {
	v := NewVolume([3]int{64, 64, 1}, [3]float64{.5, .5, 0})
	for y := 0; y < 64; y++ {
		v.Data[v.Index(20, y, 0)] = 1
	}
	o := houghTestLineOptions(v)
	a, e := HoughLines2D(nil, v, o)
	if e != nil {
		t.Fatal(e)
	}
	b, e := HoughLines2D(nil, v, o)
	if e != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("nondeterministic ties")
	}
	exact := o
	exact.Limits = HoughLimits{MaxBytes: a.Report.WorkingBytes, MaxOperations: a.Report.ReservedOperations}
	if _, e = HoughLines2D(nil, v, exact); e != nil {
		t.Fatal("exact admission", e)
	}
	exact.Limits.MaxBytes--
	if r, e := HoughLines2D(nil, v, exact); r != nil || !errors.Is(e, ErrHoughResourceLimit) {
		t.Fatalf("byte bound %v", e)
	}
	exact = o
	exact.Limits.MaxOperations = a.Report.ReservedOperations - 1
	if r, e := HoughLines2D(nil, v, exact); r != nil || !errors.Is(e, ErrHoughResourceLimit) {
		t.Fatalf("work bound %v", e)
	}
	bad := []HoughLineOptions{o, o, o, o, o, o, o, o, o}
	bad[0].ThetaStep = 0
	bad[1].RhoStepMM = math.NaN()
	bad[2].Region = image.Rect(-1, 0, 64, 64)
	bad[3].MinVotes = 0
	bad[4].MaxCandidates = 65
	bad[5].ThetaMax = 2 * math.Pi
	bad[6].RhoMaxMM = 128.1
	bad[7].Limits.MaxBytes = 65 << 20
	bad[8].SuppressRhoMM = 0
	before := append([]float32(nil), v.Data...)
	for i, p := range bad {
		if r, e := HoughLines2D(nil, v, p); r != nil || e == nil {
			t.Fatalf("invalid%d %v", i, e)
		}
	}
	if !reflect.DeepEqual(v.Data, before) {
		t.Fatal("error mutated source")
	}
	for _, value := range []float32{.5, -1, float32(math.Inf(1)), float32(math.NaN())} {
		src := v.Clone()
		src.Data[0] = value
		if r, e := HoughLines2D(nil, src, o); r != nil || !errors.Is(e, ErrInvalidHough2D) {
			t.Fatalf("bad mask %v", e)
		}
	}
	deep := NewVolume([3]int{2, 2, 2}, [3]float64{1, 1, 1})
	if r, e := HoughLines2D(nil, deep, o); r != nil || e == nil {
		t.Fatal("accepted3D")
	}
	if r, e := HoughLines2D(nil, nil, o); r != nil || e == nil {
		t.Fatal("accepted nil")
	}
	c := houghTestCircleOptions(v, [2]float64{16, 16}, 6)
	circle, e := HoughCircles2D(nil, v, c)
	if e != nil {
		t.Fatal(e)
	}
	c.Limits.MaxBytes = circle.Report.WorkingBytes - 1
	if r, e := HoughCircles2D(nil, v, c); r != nil || !errors.Is(e, ErrHoughResourceLimit) {
		t.Fatalf("circle accounting %v", e)
	}
	c = houghTestCircleOptions(v, [2]float64{16, 16}, 6)
	c.AngleBins = 7
	if r, e := HoughCircles2D(nil, v, c); r != nil || e == nil {
		t.Fatal("circle angle bounds")
	}
	c = houghTestCircleOptions(v, [2]float64{16, 16}, 6)
	c.CenterMaxMM[0] = 64
	if r, e := HoughCircles2D(nil, v, c); r != nil || e == nil {
		t.Fatal("circle region bounds")
	}
}
func TestHough2DCancellationWithinVotingAndPeak(t *testing.T) {
	v := NewVolume([3]int{64, 64, 1}, [3]float64{.5, .5, 0})
	for i := range v.Data {
		v.Data[i] = 1
	}
	for _, kind := range []string{"line", "circle"} {
		base, cancel := context.WithCancel(context.Background())
		ctx := &checkedCancelContext{Context: base, cancel: cancel, limit: 8}
		if kind == "line" {
			if r, e := HoughLines2D(ctx, v, houghTestLineOptions(v)); r != nil || !errors.Is(e, context.Canceled) {
				t.Fatalf("line cancellation %v", e)
			}
		} else {
			if r, e := HoughCircles2D(ctx, v, houghTestCircleOptions(v, [2]float64{16, 16}, 6)); r != nil || !errors.Is(e, context.Canceled) {
				t.Fatalf("circle cancellation %v", e)
			}
		}
		cancel()
	}
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &checkedCancelContext{Context: base, cancel: cancel, limit: 2}
	w := &houghWork{ctx: ctx, limit: 4096}
	if idx, e := houghPeak(w, make([]uint32, 4096), 1); idx != -1 || !errors.Is(e, context.Canceled) {
		t.Fatalf("peak cancellation %v", e)
	}
}
func TestHough2DRegionCoordinatesAndCircleVoteDedup(t *testing.T) {
	v := NewVolume([3]int{64, 64, 1}, [3]float64{.4, .8, 0})
	for y := 10; y < 50; y++ {
		v.Data[v.Index(20, y, 0)] = 1
	}
	o := houghTestLineOptions(v)
	o.Region = image.Rect(10, 10, 40, 50)
	o.RhoStepMM = .2
	o.MinVotes = 30
	r, e := HoughLines2D(nil, v, o)
	if e != nil {
		t.Fatal(e)
	}
	if len(r.Candidates) != 1 || math.Abs(r.Candidates[0].RhoMM-8) > .2 || r.Candidates[0].Theta != 0 {
		t.Fatalf("region coordinates/seamNMS %+v", r.Candidates)
	}
	tiny := NewVolume([3]int{5, 5, 1}, [3]float64{1, 1, 0})
	tiny.Data[tiny.Index(2, 1, 0)] = 1
	c := HoughCircleOptions{Hough2DOptions: Hough2DOptions{Region: image.Rect(0, 0, 5, 5), MinVotes: 1, MaxCandidates: 1}, CenterMinMM: [2]float64{1, 1}, CenterMaxMM: [2]float64{2, 2}, CenterStepMM: 1, RadiusMinMM: 1, RadiusMaxMM: 1, RadiusStepMM: .5, AngleBins: 360, SuppressCenterMM: .5, SuppressRadiusMM: .5}
	cc, e := HoughCircles2D(nil, tiny, c)
	if e != nil {
		t.Fatal(e)
	}
	if len(cc.Candidates) != 1 || cc.Candidates[0].Votes != 1 {
		t.Fatalf("duplicate edge vote %+v", cc.Candidates)
	}
}
