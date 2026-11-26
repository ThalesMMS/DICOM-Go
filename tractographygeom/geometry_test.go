package tractographygeom

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/codes"
	"github.com/ThalesMMS/dicom-go/tractography"
)

func result(t *testing.T, tracks ...[][3]float32) *tractography.Results {
	t.Helper()
	b, err := os.ReadFile("../tractography/testdata/synthetic.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct{ Results tractography.Results }
	if err = json.Unmarshal(b, &fixture); err != nil {
		t.Fatal(err)
	}
	r := &fixture.Results
	r.TrackSets = r.TrackSets[:1]
	s := &r.TrackSets[0]
	s.Tracks = nil
	s.Measurements = nil
	s.TrackStatistics = nil
	s.Statistics = nil
	s.Color = &[3]uint16{30000, 32000, 33000}
	for _, points := range tracks {
		s.Tracks = append(s.Tracks, tractography.Track{Points: points})
	}
	if err := tractography.Validate(nil, r, tractography.Limits{}); err != nil {
		t.Fatal(err)
	}
	return r
}
func ids(r *tractography.Results) []TrackID {
	out := []TrackID{}
	for _, s := range r.TrackSets {
		for i := range s.Tracks {
			out = append(out, TrackID{s.Number, i})
		}
	}
	return out
}
func near(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("got %.10g want %.10g", got, want)
	}
}

func TestOriginalGeometryMeasurementsAndROIs(t *testing.T) {
	r := result(t, [][3]float32{{-2, 0, 0}, {2, 0, 0}}, [][3]float32{{0, 0, 0}, {1, 1, 1}, {1, 1, 1}, {2, 2, 2}}, [][3]float32{{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {0, 0, 0}}, [][3]float32{{5, 5, 5}, {5, 5, 5}})
	before, _ := json.Marshal(r)
	a, err := Analyze(nil, r, ids(r))
	if err != nil {
		t.Fatal(err)
	}
	for i, w := range []float64{4, 2 * math.Sqrt(3), 2 + math.Sqrt(2), 0} {
		near(t, a.Tracks[i].LengthMM, w)
	}
	include := Region{Kind: Sphere, Center: [3]float64{}, RadiusMM: .1}
	selected, err := Select(nil, r, SelectionOptions{Include: []Region{include}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, ids(r)[:3]) {
		t.Fatal(selected)
	} // both endpoints outside: first segment crosses
	box := Region{Kind: Box, Min: [3]float64{.9, -.1, -.1}, Max: [3]float64{1.1, .1, .1}}
	selected, err = Select(nil, r, SelectionOptions{Include: []Region{include, box}, RequireAllIncludes: true})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []TrackID{{1, 0}, {1, 2}}) {
		t.Fatal(selected)
	}
	selected, err = Select(nil, r, SelectionOptions{Exclude: []Region{include}})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(selected, []TrackID{{1, 3}}) {
		t.Fatal(selected)
	}
	for _, style := range []Style{Line, Tube, Ribbon} {
		d, err := BuildDisplay(nil, r, ids(r), DisplayOptions{Style: style, PointStride: 100})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(d.Stats.DegenerateTracks, []TrackID{{1, 3}}) {
			t.Fatal(d.Stats)
		}
		for _, s := range d.Surfaces {
			if err := s.Mesh.Validate(); err != nil {
				t.Fatal(err)
			}
		}
	}
	after, _ := json.Marshal(r)
	if string(before) != string(after) {
		t.Fatal("source mutated")
	}
}

func TestTessellationPhysicalFramesAndDirectionSign(t *testing.T) {
	p := [][3]float32{{1, 2, 3}, {2, 4, 5}, {3, 6, 7}, {4, 8, 9}}
	reverse := make([][3]float32, len(p))
	for i := range p {
		reverse[i] = p[len(p)-1-i]
	}
	r := result(t, p, reverse)
	for _, style := range []Style{Tube, Ribbon} {
		d, err := BuildDisplay(nil, r, ids(r), DisplayOptions{Style: style, RadiusMM: 2, TubeSides: 8})
		if err != nil {
			t.Fatal(err)
		}
		sides := 8
		if style == Ribbon {
			sides = 2
		}
		for _, s := range d.Surfaces {
			if err = s.Mesh.Validate(); err != nil {
				t.Fatal(err)
			}
			for i, v := range s.Mesh.Vertices {
				center := vec(r.TrackSets[0].Tracks[s.Owner.TrackIndex].Points[s.SourcePointIndices[i]])
				offset := sub(vec(v), center)
				near(t, norm(offset), 2)
				near(t, dot(offset, unit([3]float64{1, 2, 2})), 0)
				near(t, norm(vec(s.Mesh.Normals[i])), 1)
			}
			if len(s.Mesh.Vertices) != len(p)*sides {
				t.Fatal(len(s.Mesh.Vertices))
			}
		}
		for i := range d.Surfaces[0].Colors {
			j := (len(p)-1-i/sides)*sides + i%sides
			if d.Surfaces[0].Colors[i] != d.Surfaces[1].Colors[j] {
				t.Fatal("direction changed by reversing tract")
			}
		}
	}
	curve := [][3]float32{{0, 0, 0}, {1, 0, 0}, {1, 1, 0}, {1, 1, 1}, {0, 1, 1}, {0, 0, 0}}
	r = result(t, curve)
	d, err := BuildDisplay(nil, r, ids(r), DisplayOptions{Style: Tube})
	if err != nil {
		t.Fatal(err)
	}
	s := d.Surfaces[0]
	for j := 0; j < 6; j++ {
		if s.Mesh.Vertices[j] != s.Mesh.Vertices[len(s.Mesh.Vertices)-6+j] {
			t.Fatal("closed seam differs")
		}
	}
	if err = s.Mesh.Validate(); err != nil {
		t.Fatal(err)
	}
}

func scalar(t *testing.T, r *tractography.Results) *ScalarMapping {
	t.Helper()
	q := codes.Code{Value: "246205007", Scheme: "SCT", Meaning: "Quantity"}
	u := codes.Code{Value: "1", Scheme: "UCUM", Meaning: "no units"}
	m := tractography.Measurement{Quantity: tractography.FromCode(q), Units: tractography.FromCode(u)}
	for _, track := range r.TrackSets[0].Tracks {
		v := make([]float32, len(track.Points))
		for i := range v {
			v[i] = float32(i)
		}
		m.Tracks = append(m.Tracks, tractography.MeasurementValues{Values: v})
	}
	r.TrackSets[0].Measurements = []tractography.Measurement{m}
	return &ScalarMapping{Quantity: q, Units: u, Min: 0, Max: 2}
}

func TestScalarAlignmentOwnershipAndExplicitRejection(t *testing.T) {
	r := result(t, [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}})
	mapping := scalar(t, r)
	m := &r.TrackSets[0].Measurements[0]
	m.Tracks[0].Values = []float32{2, 0, 1}
	m.Tracks[0].PointIndices = []uint32{3, 1, 2}
	m.Quantity.Context = &tractography.CodeContext{MappingResourceName: "owned"}
	m.Quantity.Equivalent = []tractography.Code{m.Units}
	d, err := BuildDisplay(nil, r, ids(r), DisplayOptions{Color: Scalar, Scalar: mapping})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(d.Lines[0].Colors, [][3]float32{{0, 0, 1}, {.5, 0, .5}, {1, 0, 0}}) {
		t.Fatal(d.Lines[0].Colors)
	}
	a, err := Analyze(nil, r, ids(r))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(a.Tracks[0].Measurements[0], *m) {
		t.Fatal("measurement ordering changed")
	}
	a.Tracks[0].Measurements[0].Tracks[0].Values[0] = 99
	a.Tracks[0].Measurements[0].Quantity.Context.MappingResourceName = "modified"
	a.Tracks[0].Measurements[0].Quantity.Equivalent[0].Meaning = "modified"
	a.References[0].Frames[0] = 999
	if m.Tracks[0].Values[0] != 2 || m.Quantity.Context.MappingResourceName != "owned" || r.References[0].Frames[0] == 999 || m.Quantity.Equivalent[0].Meaning == "modified" {
		t.Fatal("borrowed analysis")
	}
	for _, edit := range []func(*tractography.Results, *ScalarMapping){func(_ *tractography.Results, s *ScalarMapping) { s.Units.Value = "mm" }, func(_ *tractography.Results, s *ScalarMapping) { s.Min = s.Max }, func(r *tractography.Results, _ *ScalarMapping) {
		r.TrackSets[0].Measurements[0].Tracks[0].Values[0] = float32(math.NaN())
	}, func(r *tractography.Results, _ *ScalarMapping) {
		r.TrackSets[0].Measurements[0].Tracks[0].Values = []float32{1}
		r.TrackSets[0].Measurements[0].Tracks[0].PointIndices = []uint32{2}
	}} {
		r := result(t, [][3]float32{{0, 0, 0}, {1, 0, 0}, {2, 0, 0}})
		s := scalar(t, r)
		edit(r, s)
		d, e := BuildDisplay(nil, r, ids(r), DisplayOptions{Color: Scalar, Scalar: s})
		if e == nil || d != nil {
			t.Fatal("invalid scalar accepted", e)
		}
	}
}

func TestDisplayLODBudgetsDoNotChangeSelectionOrLength(t *testing.T) {
	points := make([][3]float32, 101)
	for i := range points {
		points[i] = [3]float32{float32(i), float32(i % 2), 0}
	}
	r := result(t, points, points, points, points)
	selected, _ := Select(nil, r, SelectionOptions{})
	a, _ := Analyze(nil, r, selected)
	o := DisplayOptions{Style: Tube, MaxTracks: 2, MaxPoints: 10, MaxTriangles: 100, MaxBytes: 5000}
	d, err := BuildDisplay(nil, r, selected, o)
	if err != nil {
		t.Fatal(err)
	}
	if !d.Stats.LOD || d.Stats.DisplayedTracks != 2 || d.Stats.DisplayedPoints > 10 || d.Stats.Triangles > 100 || d.Stats.Bytes > 5000 {
		t.Fatal(d.Stats)
	}
	if d.Surfaces[0].Owner != selected[0] || d.Surfaces[1].Owner != selected[3] {
		t.Fatal("unstable track samples")
	}
	for _, s := range d.Surfaces {
		if s.SourcePointIndices[0] != 0 || s.SourcePointIndices[len(s.SourcePointIndices)-1] != 100 {
			t.Fatal("endpoints lost")
		}
	}
	d2, err := BuildDisplay(nil, r, selected, o)
	if err != nil || !reflect.DeepEqual(d, d2) {
		t.Fatal("nondeterministic LOD")
	}
	selected2, _ := Select(nil, r, SelectionOptions{})
	a2, _ := Analyze(nil, r, selected2)
	if !reflect.DeepEqual(selected, selected2) || !reflect.DeepEqual(a, a2) {
		t.Fatal("LOD changed scientific state")
	}
	for _, o := range []DisplayOptions{{Style: Tube, MaxBytes: 1}, {Style: Tube, MaxTriangles: 1}, {MaxPoints: 1}, {MaxTracks: -1}, {MaxBytes: math.MaxInt}} {
		d, err := BuildDisplay(nil, r, selected, o)
		if err == nil || d != nil {
			t.Fatal("budget accepted", o)
		}
	}
}

func TestCanceledAndInvalidGeometryAreAtomic(t *testing.T) {
	r := result(t, [][3]float32{{0, 0, 0}, {1, 1, 1}})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if v, e := Select(ctx, r, SelectionOptions{}); v != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(v, e)
	}
	if v, e := Analyze(ctx, r, ids(r)); v != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(v, e)
	}
	if v, e := BuildDisplay(ctx, r, ids(r), DisplayOptions{}); v != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(v, e)
	}
	if _, e := Analyze(nil, r, []TrackID{{1, 0}, {1, 0}}); !errors.Is(e, ErrInvalidOptions) {
		t.Fatal(e)
	}
	if _, e := Select(nil, r, SelectionOptions{Include: []Region{{Kind: Sphere, RadiusMM: -1}}}); !errors.Is(e, ErrInvalidOptions) {
		t.Fatal(e)
	}
	r.TrackSets[0].Tracks[0].Points[0][0] = float32(math.Inf(1))
	if d, e := BuildDisplay(nil, r, ids(r), DisplayOptions{}); d != nil || !errors.Is(e, ErrInvalidData) {
		t.Fatal(d, e)
	}
}

// A deterministic context makes cancellation during a traversal testable
// without timeouts, races or a scheduler-dependent sleep.
type steppingContext struct{ calls, limit int }

func (c *steppingContext) Deadline() (time.Time, bool) { return time.Time{}, false }
func (c *steppingContext) Done() <-chan struct{}       { return nil }
func (c *steppingContext) Value(any) any               { return nil }
func (c *steppingContext) Err() error {
	c.calls++
	limit := c.limit
	if limit == 0 {
		limit = 20
	}
	if c.calls > limit {
		return context.Canceled
	}
	return nil
}

func TestDisplayResamplingAndPolylineCancellation(t *testing.T) {
	points := make([][3]float32, 1025)
	indices := make([]int, len(points))
	values := make([]float32, len(points))
	for i := range points {
		points[i] = [3]float32{float32(i), 0, 0}
		indices[i] = i
		values[i] = float32(i)
	}
	p := prepared{selectedTrack: selectedTrack{id: TrackID{1, 0}, track: &tractography.Track{Points: points}}, indices: indices, target: len(points), values: values}
	ctx := &steppingContext{limit: 2}
	if err := p.resample(ctx); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	if len(p.sampled) != 256 {
		t.Fatalf("cancellation did not interrupt resampling: %d points", len(p.sampled))
	}
	if err := p.resample(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, mode := range []ColorMode{Direction, Scalar} {
		ctx := &steppingContext{limit: 2}
		line, err := buildPolyline(ctx, p, DisplayOptions{Color: mode, Scalar: &ScalarMapping{Min: 0, Max: 1024}})
		if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(line, Polyline{}) {
			t.Fatalf("partial polyline after cancellation: %v", err)
		}
		if ctx.calls != 3 {
			t.Fatalf("cancellation polled too late: %d", ctx.calls)
		}
	}
	for i, v := range points {
		if v != [3]float32{float32(i), 0, 0} {
			t.Fatal("cancellation changed original points")
		}
	}
}

func TestTraversalCancellationAndFoldedLODAreAtomic(t *testing.T) {
	points := make([][3]float32, 10001)
	for i := range points {
		points[i] = [3]float32{float32(i % 2), 0, 0}
	}
	r := result(t, points)
	for _, style := range []Style{Line, Tube, Ribbon} {
		d, e := BuildDisplay(nil, r, ids(r), DisplayOptions{Style: style, PointStride: 10000})
		if e != nil {
			t.Fatal(e)
		}
		if d.Stats.DisplayedPoints < 3 {
			t.Fatal("folded path lost original excursion")
		}
		for _, s := range d.Surfaces {
			if err := s.Mesh.Validate(); err != nil {
				t.Fatal(err)
			}
			for _, normal := range s.Mesh.Normals {
				near(t, norm(vec(normal)), 1)
			}
		}
	}
	if out, e := Analyze(&steppingContext{}, r, ids(r)); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(out, e)
	}
	if out, e := Select(&steppingContext{}, r, SelectionOptions{}); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(out, e)
	}
	if out, e := BuildDisplay(&steppingContext{}, r, ids(r), DisplayOptions{}); out != nil || !errors.Is(e, context.Canceled) {
		t.Fatal(out, e)
	}
	// A nonfinite point measurement remains inspectable; scalar display rejects
	// it instead of silently dropping or replacing the value.
	r = result(t, [][3]float32{{0, 0, 0}, {1, 0, 0}})
	mapping := scalar(t, r)
	r.TrackSets[0].Measurements[0].Tracks[0].Values[0] = float32(math.NaN())
	a, e := Analyze(nil, r, ids(r))
	if e != nil || !math.IsNaN(float64(a.Tracks[0].Measurements[0].Tracks[0].Values[0])) {
		t.Fatal(a, e)
	}
	if out, e := BuildDisplay(nil, r, ids(r), DisplayOptions{Color: Scalar, Scalar: mapping}); out != nil || !errors.Is(e, ErrInvalidData) {
		t.Fatal(out, e)
	}
}

func TestBoundarySelectionAndScalarValidationBeforeTrackLOD(t *testing.T) {
	r := result(t, [][3]float32{{-2, 1, 0}, {2, 1, 0}}, [][3]float32{{-2, 1.01, 0}, {2, 1.01, 0}})
	for _, q := range []Region{{Kind: Sphere, RadiusMM: 1}, {Kind: Box, Min: [3]float64{-1, -1, -1}, Max: [3]float64{1, 1, 1}}} {
		v, e := Select(nil, r, SelectionOptions{Include: []Region{q}})
		if e != nil || !reflect.DeepEqual(v, []TrackID{{1, 0}}) {
			t.Fatal(v, e)
		}
	}
	s := scalar(t, r)
	r.TrackSets[0].Measurements[0].Tracks[1].Values[0] = float32(math.Inf(1))
	v, e := BuildDisplay(nil, r, ids(r), DisplayOptions{MaxTracks: 1, Color: Scalar, Scalar: s})
	if v != nil || !errors.Is(e, ErrInvalidData) {
		t.Fatal("LOD hid nonfinite source value", v, e)
	}
	if v, e := BuildDisplay(nil, r, nil, DisplayOptions{}); e != nil || v.Stats.InputTracks != 0 || len(v.Lines) != 0 {
		t.Fatal(v, e)
	}
	if _, e := Analyze(nil, r, []TrackID{{1, -1}}); !errors.Is(e, ErrInvalidOptions) {
		t.Fatal(e)
	}
}
