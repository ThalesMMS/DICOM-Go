package roi

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
)

func contourFixture() ContourInterpolationRequest {
	return ContourInterpolationRequest{Method: ContourMorphological, Label: 7, Axis: 2, Geometry: Grid{
		Columns: 24, Rows: 20, Slices: 5, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1}, ColSpacing: 1, RowSpacing: 2, SliceSpacing: 1,
	}, Manual: map[int]*RasterMask{}}
}

type cancelContourOnPoll struct {
	context.Context
	cancel context.CancelFunc
	polls  atomic.Int64
}

func (c *cancelContourOnPoll) Err() error {
	if c.polls.Add(-1) == 0 {
		c.cancel()
	}
	return c.Context.Err()
}

func TestContourInterpolationCancellationDuringDilationIsAtomic(t *testing.T) {
	r := contourFixture()
	r.Manual[0] = contourRectangle(3, 3, 7, 7)
	r.Manual[4] = contourRectangle(2, 2, 11, 11)
	before := r.Manual[0].Clone()
	base, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctx := &cancelContourOnPoll{Context: base, cancel: cancel}
	ctx.polls.Store(12)
	out, err := InterpolateContours(ctx, r)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, ContourInterpolationResult{}) || !contourEqual(before, r.Manual[0]) {
		t.Fatalf("mid-operation cancellation published/mutated: %+v %v", out, err)
	}
}

func BenchmarkContourInterpolationMethods(b *testing.B) {
	for _, method := range []ContourInterpolationMethod{ContourSignedDistance, ContourMorphological} {
		b.Run(string(method), func(b *testing.B) {
			r := contourFixture()
			r.Geometry.Columns = 128
			r.Geometry.Rows = 128
			r.Geometry.Slices = 9
			r.Method = method
			r.Manual = map[int]*RasterMask{}
			for _, z := range []int{0, 8} {
				m := NewRasterMask(128, 128)
				for y := 32; y < 90; y++ {
					m.SetRun(y, 32, 90)
				}
				r.Manual[z] = m
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				out, err := InterpolateContours(context.Background(), r)
				if err != nil || len(out.Generated) != 7 {
					b.Fatal(err)
				}
			}
		})
	}
}

func TestContourInterpolationPhysicalGeodesicAndBoundedEnd(t *testing.T) {
	r := contourFixture()
	mask, seed := make([]bool, 24*20), make([]bool, 24*20)
	for y := 3; y < 7; y++ {
		for x := 4; x < 9; x++ {
			mask[y*24+x] = true
		}
	}
	seed[3*24+4] = true
	b := contourBudget{ctx: context.Background(), limit: 100000}
	d, _, err := contourDilationDistance(&b, mask, seed, r.Geometry)
	if err != nil {
		t.Fatal(err)
	}
	for y := 3; y < 7; y++ {
		for x := 4; x < 9; x++ {
			want := float64(x-4) + float64(y-3)*2
			if d[y*24+x] != want {
				t.Fatalf("physical distance %d,%d: %g != %g", x, y, d[y*24+x], want)
			}
		}
	}
	r.Manual[0] = contourRectangle(2, 3, 7, 7)
	r.Manual[4] = NewRasterMask(24, 20)
	r.EndPolicy = ContourCloseUnmatched
	r.EndLengthMM = 3
	r.MaxEndSlices = 2
	out, err := InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() != nil {
		t.Fatalf("closing %v %v", err, out.Unresolved)
	}
	if out.Generated[1].Empty() || out.Generated[1].Count() >= r.Manual[0].Count() || out.Generated[2].Count() > out.Generated[1].Count() || !out.Generated[3].Empty() {
		t.Fatal("ending does not shrink within bounded interval")
	}
	if out.Relations[0].Kind != "closing" {
		t.Fatalf("unreported closing %+v", out.Relations)
	}
	r.Geometry.SliceSpacing = math.MaxFloat64
	if _, err := InterpolateContours(context.Background(), r); !errors.Is(err, ErrContourInterpolation) {
		t.Fatalf("unrepresentable physical position accepted: %v", err)
	}
}
func contourRectangle(x, y, w, h int) *RasterMask {
	m := NewRasterMask(24, 20)
	for row := y; row < y+h; row++ {
		m.SetRun(row, x, x+w)
	}
	return m
}
func contourEqual(a, b *RasterMask) bool {
	if a == nil || b == nil {
		return a == b
	}
	for y := 0; y < a.Rows; y++ {
		for x := 0; x < a.Columns; x++ {
			if a.Get(x, y) != b.Get(x, y) {
				return false
			}
		}
	}
	return true
}

func TestContourInterpolationPreservesSparseManualAndCavity(t *testing.T) {
	r := contourFixture()
	m := contourRectangle(3, 3, 14, 12)
	for y := 6; y < 12; y++ {
		for x := 7; x < 12; x++ {
			m.Set(x, y, false)
		}
	}
	r.Manual[0], r.Manual[2], r.Manual[4] = m, m.Clone(), m.Clone()
	before := m.Clone()
	out, err := InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() != nil {
		t.Fatalf("interpolation: %v %+v", err, out.Unresolved)
	}
	if len(out.Generated) != 2 || len(out.Manual) != 3 {
		t.Fatalf("slices generated/manual %d/%d", len(out.Generated), len(out.Manual))
	}
	for _, z := range []int{1, 3} {
		if !contourEqual(out.Generated[z], m) {
			t.Fatalf("identity/cavity changed slice %d", z)
		}
	}
	out.Manual[0].Set(3, 3, false)
	out.Generated[1].Set(4, 4, false)
	if !contourEqual(m, before) || !contourEqual(r.Manual[2], before) {
		t.Fatal("proposal aliases inputs")
	}
}

func TestContourInterpolationTranslationAndPhysicalPositions(t *testing.T) {
	r := contourFixture()
	r.Geometry.SliceOffsets = []float64{0, 1, 2, 7, 8}
	r.Manual[0] = contourRectangle(2, 4, 3, 3)
	r.Manual[4] = contourRectangle(10, 4, 3, 3)
	r.MaxTranslationMM = 9
	out, err := InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() != nil {
		t.Fatalf("translation: %v %v", err, out.Unresolved)
	}
	if len(out.Relations) != 1 || out.Relations[0].Translation != [2]int{8, 0} {
		t.Fatalf("relation %+v", out.Relations)
	}
	for z, x := range map[int]int{1: 3, 2: 4, 3: 9} {
		want := contourRectangle(x, 4, 3, 3)
		if !contourEqual(out.Generated[z], want) {
			t.Fatalf("physical translation slice %d", z)
		}
	}
	r.MaxTranslationMM = 0
	out, err = InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() == nil || len(out.Generated) != 0 {
		t.Fatalf("unmatched must be explicit: %v %+v", err, out)
	}
}

func TestContourInterpolationScaleBranchMergeAndMultipleComponents(t *testing.T) {
	for _, reverse := range []bool{false, true} {
		r := contourFixture()
		joined := contourRectangle(3, 4, 14, 5)
		split := contourRectangle(3, 4, 5, 5)
		for y := 4; y < 9; y++ {
			split.SetRun(y, 12, 17)
		}
		if reverse {
			r.Manual[0], r.Manual[4] = split, joined
		} else {
			r.Manual[0], r.Manual[4] = joined, split
		}
		out, err := InterpolateContours(context.Background(), r)
		if err != nil || out.ValidateComplete() != nil {
			t.Fatalf("branch/merge %v %v", err, out.Unresolved)
		}
		kind := "branch"
		if reverse {
			kind = "merge"
		}
		if len(out.Relations) != 1 || out.Relations[0].Kind != kind {
			t.Fatalf("want %s: %+v", kind, out.Relations)
		}
		for _, z := range []int{1, 2, 3} {
			if !out.Generated[z].Get(4, 5) || !out.Generated[z].Get(14, 5) {
				t.Fatalf("branch support lost %d", z)
			}
		}
		if !contourEqual(r.Manual[0], out.Manual[0]) || !contourEqual(r.Manual[4], out.Manual[4]) {
			t.Fatal("manual changed")
		}
	}
	r := contourFixture()
	r.Manual[0] = contourRectangle(6, 5, 5, 5)
	r.Manual[4] = contourRectangle(3, 2, 11, 11)
	out, err := InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() != nil {
		t.Fatal(err, out.Unresolved)
	}
	last := r.Manual[0].Count()
	for z := 1; z < 4; z++ {
		n := out.Generated[z].Count()
		if n < last || n > r.Manual[4].Count() {
			t.Fatalf("scale area sequence %d then %d", last, n)
		}
		last = n
	}
}

func TestContourInterpolationAmbiguousCorrespondenceAndEmptySlice(t *testing.T) {
	r := contourFixture()
	r.Manual[0] = contourRectangle(8, 6, 3, 3)
	split := contourRectangle(2, 6, 3, 3)
	for y := 6; y < 9; y++ {
		split.SetRun(y, 14, 17)
	}
	r.Manual[4] = split
	r.MaxTranslationMM = 10
	out, err := InterpolateContours(context.Background(), r)
	if err != nil || out.ValidateComplete() == nil || len(out.Generated) != 0 {
		t.Fatalf("ambiguous must not generate: %v %v", err, out.Unresolved)
	}
	r.Manual[2] = NewRasterMask(24, 20)
	out, err = InterpolateContours(context.Background(), r)
	if err != nil || len(out.Manual) != 3 || !out.Manual[2].Empty() {
		t.Fatalf("empty manual changed: %v", err)
	}
}

func TestContourInterpolationAdmissionAndCancellation(t *testing.T) {
	r := contourFixture()
	r.Manual[0] = contourRectangle(3, 3, 4, 4)
	r.Manual[4] = r.Manual[0].Clone()
	cases := []struct {
		name string
		edit func(*ContourInterpolationRequest)
		want error
	}{
		{"bytes", func(r *ContourInterpolationRequest) { r.MaxBytes = 10 }, ErrContourLimit},
		{"voxels", func(r *ContourInterpolationRequest) { r.MaxVoxels = 10 }, ErrContourLimit},
		{"slices", func(r *ContourInterpolationRequest) { r.MaxSlices = 3 }, ErrContourLimit},
		{"work", func(r *ContourInterpolationRequest) { r.MaxWork = 1 }, ErrContourLimit},
		{"axis", func(r *ContourInterpolationRequest) { r.Axis = 0 }, ErrContourInterpolation},
		{"label", func(r *ContourInterpolationRequest) { r.Label = 0 }, ErrContourInterpolation},
		{"shear", func(r *ContourInterpolationRequest) { r.Geometry.SliceDir = render.Vec3{X: .1, Z: 1} }, ErrContourInterpolation},
		{"duplicate-position", func(r *ContourInterpolationRequest) { r.Geometry.SliceOffsets = []float64{0, 1, 1, 3, 4} }, ErrContourInterpolation},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			test := r
			c.edit(&test)
			out, err := InterpolateContours(context.Background(), test)
			if !errors.Is(err, c.want) || !reflect.DeepEqual(out, ContourInterpolationResult{}) {
				t.Fatalf("got %+v, %v", out, err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	out, err := InterpolateContours(ctx, r)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(out, ContourInterpolationResult{}) {
		t.Fatal("cancelled work published")
	}
	r.MaxComponents = 1
	split := r.Manual[0].Clone()
	split.Set(20, 15, true)
	r.Manual[4] = split
	if _, err := InterpolateContours(context.Background(), r); !errors.Is(err, ErrContourLimit) {
		t.Fatalf("component admission %v", err)
	}
}
