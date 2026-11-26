package seg

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

// resampleTestPlane is an axial plane of the synthetic SEG grid: 2 mm pixels,
// 3 mm slices.
func resampleTestPlane(columns, rows int, z float64) render.SliceGeometry {
	return render.SliceGeometry{
		Origin: render.Vec3{Z: z}, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
		RowSpacing: 2, ColSpacing: 2, Rows: rows, Columns: columns,
	}
}

// sphereFrameMask marks the pixels of plane within radius of centre.
func sphereFrameMask(plane render.SliceGeometry, centre render.Vec3, radius float64) *roi.RasterMask {
	mask := roi.NewRasterMask(plane.Columns, plane.Rows)
	for y := 0; y < plane.Rows; y++ {
		for x := 0; x < plane.Columns; x++ {
			p := plane.Origin.Add(plane.RowDir.Scale(float64(x) * plane.ColSpacing)).Add(plane.ColDir.Scale(float64(y) * plane.RowSpacing))
			if p.Sub(centre).Length() <= radius {
				mask.Set(x, y, true)
			}
		}
	}
	return mask
}

// overlappingBinaryDocument holds two overlapping spheres as BINARY segments,
// writing only non-empty frames as SEG writers do.
func overlappingBinaryDocument() (*Document, [2]render.Vec3, float64) {
	centres := [2]render.Vec3{{X: 26, Y: 30, Z: 30}, {X: 36, Y: 30, Z: 30}}
	const radius = 10.0
	doc := &Document{
		SOPClassUID: SegmentationStorage, SegmentationType: SegmentationTypeBinary,
		Rows: 30, Columns: 32, FrameOfReferenceUID: "1.2.3",
		Segments: []Segment{{Number: 1, Label: "A"}, {Number: 2, Label: "B"}},
	}
	var planes []render.SliceGeometry
	for k := 0; k < 21; k++ {
		plane := resampleTestPlane(32, 30, 3*float64(k))
		planes = append(planes, plane)
		for s, centre := range centres {
			mask := sphereFrameMask(plane, centre, radius)
			if mask.Empty() {
				continue
			}
			doc.Frames = append(doc.Frames, Frame{SegmentNumber: s + 1, SliceIndex: k, Geometry: plane, Mask: mask})
		}
	}
	// As Read reconstructs it, the volume geometry spans every plane,
	// including those without a stored frame.
	doc.Geometry = render.BuildVolumeGeometry(planes, render.DefaultGeometryTolerances())
	return doc, centres, radius
}

func TestResampleBinaryOverlappingSegmentsOntoObliqueGrid(t *testing.T) {
	doc, centres, radius := overlappingBinaryDocument()
	axis := render.Vec3{X: 0.2, Y: 1, Z: 0.3}.Normalize()
	target := roi.Grid{
		Columns: 40, Rows: 36, Slices: 30,
		RowDir: render.Vec3{X: 1}.Rotate(axis, 0.5), ColDir: render.Vec3{Y: 1}.Rotate(axis, 0.5), SliceDir: render.Vec3{Z: 1}.Rotate(axis, 0.5),
		ColSpacing: 0.8, RowSpacing: 0.9, SliceSpacing: 1.2,
	}
	target.Origin = render.Vec3{X: 31, Y: 30, Z: 30}.
		Sub(target.RowDir.Scale(19.5 * 0.8)).Sub(target.ColDir.Scale(17.5 * 0.9)).Sub(target.SliceDir.Scale(14.5 * 1.2))
	result, err := Resample(context.Background(), doc, target, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) != 2 {
		t.Fatalf("segments = %+v", result.Segments)
	}
	diagonal := math.Sqrt(2*2 + 2*2 + 3*3)
	shared := 0
	for z := 0; z < target.Slices; z++ {
		for y := 0; y < target.Rows; y++ {
			for x := 0; x < target.Columns; x++ {
				p := target.VoxelCenter(x, y, z)
				inBoth := true
				for s, segment := range result.Segments {
					on := segment.Masks[z] != nil && segment.Masks[z].Get(x, y)
					distance := p.Sub(centres[s]).Length()
					if on != (distance <= radius) && math.Abs(distance-radius) > diagonal {
						t.Fatalf("segment %d at (%d,%d,%d) is wrong %.2f mm from its surface", segment.Number, x, y, z, math.Abs(distance-radius))
					}
					inBoth = inBoth && on
				}
				if inBoth {
					shared++
				}
			}
		}
	}
	if shared == 0 {
		t.Fatal("the overlap of the two BINARY segments was lost")
	}
	if !result.Coverage.Full() {
		t.Fatalf("coverage %d of %d", result.Coverage.CoveredVoxels, result.Coverage.TotalVoxels())
	}
}

func TestResampleLabelMapKeepsSmallLabelsWithGaussian(t *testing.T) {
	doc := &Document{
		SOPClassUID: LabelMapSegmentationStorage, SegmentationType: SegmentationTypeLabelMap,
		Rows: 20, Columns: 20,
		Segments: []Segment{{Number: 0}, {Number: 1}, {Number: 2}},
	}
	for k := 0; k < 10; k++ {
		plane := resampleTestPlane(20, 20, 3*float64(k))
		plane.RowSpacing, plane.ColSpacing = 1, 1
		labels := make([]uint16, 400)
		for i := range labels {
			if i%20 < 10 {
				labels[i] = 1
			}
		}
		if k == 4 {
			labels[5*20+15] = 2 // one voxel of label 2 at (15, 5)
		}
		doc.Frames = append(doc.Frames, Frame{SliceIndex: k, Geometry: plane, LabelMap: labels})
	}
	// Target: 3 mm in-plane on the SEG slices, centres at x = 1, 4, ..., so
	// no centre reads (15, 5).
	target := roi.Grid{
		Columns: 7, Rows: 7, Slices: 10, Origin: render.Vec3{X: 1, Y: 1},
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 3, RowSpacing: 3, SliceSpacing: 3,
	}
	nearest, err := Resample(context.Background(), doc, target, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if s := findResampled(nearest, 2); s == nil || s.Voxels != 0 || !s.Vanished {
		t.Fatalf("nearest label 2 = %+v, want vanished", s)
	}
	gaussian, err := Resample(context.Background(), doc, target, ResampleOptions{Mode: roi.LabelGaussian(1)})
	if err != nil {
		t.Fatal(err)
	}
	s := findResampled(gaussian, 2)
	if s == nil || s.Voxels != 1 || !s.Preserved || s.Vanished || s.Masks[4] == nil || s.Masks[4].Count() != 1 {
		t.Fatalf("Gaussian label 2 = %+v, want one preserved voxel on slice 4", s)
	}
	if big := findResampled(gaussian, 1); big == nil || big.Voxels < 3*7*10 {
		t.Fatalf("label 1 = %+v", big)
	}
}

func TestResampleFractionalThresholdsInterpolatedSamples(t *testing.T) {
	doc := &Document{
		SOPClassUID: SegmentationStorage, SegmentationType: SegmentationTypeFractional,
		FractionalType: FractionalTypeProbability, MaxFractionalValue: 200,
		Rows: 4, Columns: 21, Segments: []Segment{{Number: 1}},
	}
	for k := 0; k < 3; k++ {
		plane := resampleTestPlane(21, 4, 3*float64(k))
		samples := make([]uint8, 84)
		for i := range samples {
			samples[i] = uint8(10 * (i % 21)) // 0..200 along x: 5 per mm
		}
		doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: k, Geometry: plane, Fractional: samples})
	}
	// 0.7 mm columns from x = 0.35 mm: the interpolated sample is 5*x.
	target := roi.GridFromSlice(render.SliceGeometry{
		Origin: render.Vec3{X: 0.35, Y: 1, Z: 3}, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1},
		RowSpacing: 1, ColSpacing: 0.7, Rows: 3, Columns: 57,
	}, 1)
	for _, threshold := range []float64{0.25, 0.5, 0.9} {
		result, err := Resample(context.Background(), doc, target, ResampleOptions{FractionalThreshold: threshold})
		if err != nil {
			t.Fatal(err)
		}
		cut := math.Ceil(threshold * 200)
		mask := result.Segments[0].Masks[0]
		for x := 0; x < target.Columns; x++ {
			sample := 5 * (0.35 + 0.7*float64(x))
			if got, want := mask.Get(x, 1), sample >= cut-1e-9 && sample <= 200; got != want {
				t.Fatalf("threshold %v x=%d sample %.2f: got %v", threshold, x, sample, got)
			}
		}
	}
}

// TestResampleRestoresOmittedEmptyFrames checks that frame planes missing
// from a regular stack read as empty slices, not as their neighbours, and
// that coverage reports the target slices outside the SEG.
func TestResampleRestoresOmittedEmptyFrames(t *testing.T) {
	doc := &Document{SOPClassUID: SegmentationStorage, SegmentationType: SegmentationTypeBinary, Rows: 4, Columns: 4, Segments: []Segment{{Number: 1}}}
	for _, k := range []int{0, 1, 2, 6, 7} {
		plane := resampleTestPlane(4, 4, 3*float64(k))
		mask := roi.NewRasterMask(4, 4)
		mask.SetRun(1, 0, 4)
		doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: k, Geometry: plane, Mask: mask})
	}
	r, err := NewResampler(context.Background(), doc, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if grid := r.Grid(); grid.Slices != 8 || len(grid.SliceOffsets) != 0 || grid.SliceSpacing != 3 {
		t.Fatalf("grid = %+v, want 8 regular 3 mm slices", grid)
	}
	target := roi.Grid{
		Columns: 4, Rows: 4, Slices: 5, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 2, RowSpacing: 2, SliceSpacing: 1, SliceOffsets: []float64{3, 9, 12, 21, 30},
	}
	result, err := r.Resample(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	masks := result.Segments[0].Masks
	for z, want := range []bool{true, false, false, true, false} {
		if got := masks[z] != nil && masks[z].Get(2, 1); got != want {
			t.Fatalf("target slice %d = %v, want %v", z, got, want)
		}
	}
	if got := result.Coverage.CoveredSlices(); len(got) != 4 || got[3] != 3 {
		t.Fatalf("covered slices = %v, want 0..3 (z=30 mm is beyond the SEG)", got)
	}
}

func TestResampleRejectsFramesWithoutGeometry(t *testing.T) {
	doc := &Document{Rows: 2, Columns: 2, Frames: []Frame{{SegmentNumber: 1, Mask: roi.NewRasterMask(2, 2)}}}
	if _, err := NewResampler(context.Background(), doc, ResampleOptions{}); !errors.Is(err, ErrNoGeometry) {
		t.Fatalf("err = %v, want ErrNoGeometry", err)
	}
	doc, _, _ = overlappingBinaryDocument()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Resample(ctx, doc, roi.GridFromSlice(resampleTestPlane(32, 30, 30), 3), ResampleOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled err = %v", err)
	}
}

// TestResampleReadDocumentOntoItsOwnGrid writes and reads a BINARY SEG whose
// empty frames are omitted, then resamples it onto the grid Read
// reconstructed: every stored frame comes back unchanged.
func TestResampleReadDocumentOntoItsOwnGrid(t *testing.T) {
	doc, _, _ := overlappingBinaryDocument()
	doc.SOPInstanceUID = "1.2.826.0.1.3680043.9.7433.66.1114"
	doc.StudyInstanceUID = "1.2.826.0.1.3680043.9.7433.1114.1"
	doc.SeriesInstanceUID = "1.2.826.0.1.3680043.9.7433.1114.2"
	file, err := Write(doc)
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	read, err := Read(file.Dataset)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	r, err := NewResampler(context.Background(), read, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	grid := r.Grid()
	// Read rebuilds the planes from the first slice index up to the last
	// stored frame (13), empty planes included.
	if grid.Slices != 14 || len(grid.SliceOffsets) != 0 {
		t.Fatalf("grid = %d slices (irregular %v), want 14 regular planes", grid.Slices, len(grid.SliceOffsets) > 0)
	}
	result, err := r.Resample(context.Background(), grid)
	if err != nil {
		t.Fatal(err)
	}
	for _, frame := range doc.Frames {
		got := findResampled(result, frame.SegmentNumber).Masks[frame.SliceIndex]
		if got == nil || got.Count() != frame.Mask.Count() {
			t.Fatalf("segment %d slice %d differs after resampling", frame.SegmentNumber, frame.SliceIndex)
		}
		frame.Mask.ForEachPixel(func(x, y int) {
			if !got.Get(x, y) {
				t.Fatalf("segment %d slice %d pixel (%d,%d) lost", frame.SegmentNumber, frame.SliceIndex, x, y)
			}
		})
	}
}

func findResampled(result *Resampled, number int) *ResampledSegment {
	for i := range result.Segments {
		if result.Segments[i].Number == number {
			return &result.Segments[i]
		}
	}
	return nil
}

// TestResamplePlacesFramesByGridCongruence (#1115): a frame whose position
// differs by decimal rounding (a hundredth of a pixel) still lands on its
// slice; one shifted by a tenth of a pixel, or whose spacing drifts a tenth
// of a pixel across the plane, does not share the grid.
func TestResamplePlacesFramesByGridCongruence(t *testing.T) {
	document := func(adjust func(*render.SliceGeometry)) *Document {
		doc := &Document{SOPClassUID: SegmentationStorage, SegmentationType: SegmentationTypeBinary, Rows: 64, Columns: 64, Segments: []Segment{{Number: 1}}}
		for k := 0; k < 3; k++ {
			plane := resampleTestPlane(64, 64, 3*float64(k))
			if k == 1 {
				adjust(&plane)
			}
			mask := roi.NewRasterMask(64, 64)
			mask.SetRun(1, 0, 4)
			doc.Frames = append(doc.Frames, Frame{SegmentNumber: 1, SliceIndex: k, Geometry: plane, Mask: mask})
		}
		return doc
	}
	if _, err := NewResampler(context.Background(), document(func(p *render.SliceGeometry) { p.Origin.X += 0.02 }), ResampleOptions{}); err != nil {
		t.Fatalf("rounded frame position rejected: %v", err)
	}
	for name, adjust := range map[string]func(*render.SliceGeometry){
		"shifted":       func(p *render.SliceGeometry) { p.Origin.X += 0.2 },
		"spacing drift": func(p *render.SliceGeometry) { p.ColSpacing *= 1 + 0.1/63 },
	} {
		if _, err := NewResampler(context.Background(), document(adjust), ResampleOptions{}); !errors.Is(err, ErrNoGeometry) {
			t.Errorf("%s frame: err = %v, want ErrNoGeometry", name, err)
		}
	}
}

// TestResampleMappedFollowsARegistration (#1089): the overlapping spheres
// read through a synthetic rotation and shift into another Frame of
// Reference land where the analytic transform puts their centres, and a nil
// mapper is refused.
func TestResampleMappedFollowsARegistration(t *testing.T) {
	doc, centres, radius := overlappingBinaryDocument()
	resampler, err := NewResampler(context.Background(), doc, ResampleOptions{})
	if err != nil {
		t.Fatal(err)
	}
	// Target point p maps to the SEG frame by a 20° turn about z around
	// (31, 30, 30) and a (−5, 4, 2) mm shift.
	angle := 20 * math.Pi / 180
	pivot := render.Vec3{X: 31, Y: 30, Z: 30}
	mapper := func(p render.Vec3) (render.Vec3, error) {
		d := p.Sub(pivot)
		return pivot.Add(render.Vec3{X: math.Cos(angle)*d.X - math.Sin(angle)*d.Y - 5, Y: math.Sin(angle)*d.X + math.Cos(angle)*d.Y + 4, Z: d.Z + 2}), nil
	}
	target := roi.Grid{Columns: 40, Rows: 40, Slices: 24, RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, SliceDir: render.Vec3{Z: 1},
		ColSpacing: 1.5, RowSpacing: 1.5, SliceSpacing: 2.5, Origin: render.Vec3{X: 2, Y: 2, Z: 2}}
	result, err := resampler.ResampleMapped(context.Background(), target, mapper)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Segments) != 2 {
		t.Fatalf("segments = %+v", result.Segments)
	}
	diagonal := math.Sqrt(2*2 + 2*2 + 3*3)
	for z := 0; z < target.Slices; z++ {
		for y := 0; y < target.Rows; y++ {
			for x := 0; x < target.Columns; x++ {
				if !result.Coverage.Covered(x, y, z) {
					continue
				}
				q, _ := mapper(target.VoxelCenter(x, y, z))
				for s, segment := range result.Segments {
					on := segment.Masks[z] != nil && segment.Masks[z].Get(x, y)
					distance := q.Sub(centres[s]).Length()
					if on != (distance <= radius) && math.Abs(distance-radius) > diagonal {
						t.Fatalf("segment %d at (%d,%d,%d) is wrong %.2f mm from its surface", segment.Number, x, y, z, math.Abs(distance-radius))
					}
				}
			}
		}
	}
	if result.Segments[0].Voxels == 0 || result.Segments[1].Voxels == 0 || result.Coverage.Full() {
		t.Fatalf("voxels %d/%d, coverage %v", result.Segments[0].Voxels, result.Segments[1].Voxels, result.Coverage.Fraction())
	}
	if _, err := resampler.ResampleMapped(context.Background(), target, nil); !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("nil mapper: %v", err)
	}
}
