package nrrd_test

import (
	"bytes"
	"context"
	"errors"
	"math"
	"testing"

	"github.com/ThalesMMS/dicom-go/nifti"
	"github.com/ThalesMMS/dicom-go/nrrd"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
	"github.com/ThalesMMS/dicom-go/seg"
)

var packTestAffine = render.GeometryAffine{
	0, -0.75, 0, 93,
	0.5, 0, 0, -20,
	0, 0, 2.5, 15,
	0, 0, 0, 1,
}

func TestPackSlicerSegmentationRoundTrip(t *testing.T) {
	dimensions := [3]int{2, 2, 2}
	first := roi.NewRasterMask(2, 2)
	first.Set(0, 0, true)
	second := roi.NewRasterMask(2, 2)
	second.Set(1, 1, true)
	masks := []nrrd.SlicerSegmentMask{
		{Segment: seg.Segment{Number: 20, Label: "First"}, Slices: []*roi.RasterMask{first, nil}},
		{Segment: seg.Segment{Number: 8, Label: "Empty"}},
		{Segment: seg.Segment{Number: 2, Label: "Disjoint"}, Slices: []*roi.RasterMask{nil, second}},
		{Segment: seg.Segment{Number: 3, Label: "Overlapping"}, Slices: []*roi.RasterMask{first, second}},
	}
	ctx := context.Background()
	volume, segments, err := nrrd.PackSlicerSegmentation(ctx, dimensions, packTestAffine, masks, nifti.ReadLimits{
		MaxVoxels: 16, MaxPayloadBytes: 16,
	})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := nrrd.WriteSlicerSegmentationContext(ctx, &encoded, volume, segments, nrrd.WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	image, err := nrrd.Read(ctx, &encoded, nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Volume.Header.Datatype != nifti.DatatypeUint8 || image.Volume.Dimensions != [4]int{2, 2, 2, 2} || image.NonSpatialAxis != 0 {
		t.Fatalf("round trip datatype %d dimensions %v list axis %d", image.Volume.Header.Datatype, image.Volume.Dimensions, image.NonSpatialAxis)
	}
	if image.Header.Space != "left-posterior-superior" || image.Volume.Geometry.IndexToLPS != packTestAffine {
		t.Fatalf("round trip space %s, affine %v", image.Header.Space, image.Volume.Geometry.IndexToLPS)
	}
	got, err := image.Header.SlicerSegments()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(masks) {
		t.Fatalf("got %d segments, want %d", len(got), len(masks))
	}
	wantLabels, wantLayers := [4]int{1, 2, 3, 1}, [4]int{0, 0, 0, 1}
	for i, segment := range got {
		if segment.Index != i || segment.Name != masks[i].Segment.Label ||
			segment.LabelValue != wantLabels[i] || segment.Layer != wantLayers[i] {
			t.Fatalf("segment %d = %+v", i, segment)
		}
	}
	if emptyExtent := image.Header.KeyValues["Segment1_Extent"]; emptyExtent != "0 -1 0 -1 0 -1" {
		t.Fatalf("empty segment extent = %q", emptyExtent)
	}
	for i, want := range [16]float64{0: 1, 7: 3, 8: 1, 15: 1} {
		if value := image.Volume.Stored(i); value != want {
			t.Fatalf("label %d = %v, want %v", i, value, want)
		}
	}
}

func TestPackSlicerSegmentationWidensEveryLayer(t *testing.T) {
	left := roi.NewRasterMask(2, 1)
	left.Set(0, 0, true)
	right := roi.NewRasterMask(2, 1)
	right.Set(1, 0, true)
	masks := make([]nrrd.SlicerSegmentMask, 258)
	for i := range masks {
		masks[i].Segment.Number = i + 1
	}
	masks[0].Slices = []*roi.RasterMask{left}
	masks[1].Slices = []*roi.RasterMask{left}
	masks[256].Slices = []*roi.RasterMask{right}
	masks[257].Slices = []*roi.RasterMask{right}

	ctx := context.Background()
	dimensions := [3]int{2, 1, 1}
	if volume, segments, err := nrrd.PackSlicerSegmentation(ctx, dimensions, packTestAffine, masks, nifti.ReadLimits{MaxPayloadBytes: 7}); volume != nil || segments != nil || !errors.Is(err, nrrd.ErrLimitExceeded) {
		t.Fatalf("two widened layers over byte limit returned %v, %v, %v", volume, segments, err)
	}
	volume, segments, err := nrrd.PackSlicerSegmentation(ctx, dimensions, packTestAffine, masks, nifti.ReadLimits{MaxPayloadBytes: 8})
	if err != nil {
		t.Fatal(err)
	}
	var encoded bytes.Buffer
	if err := nrrd.WriteSlicerSegmentation(&encoded, volume, segments, nrrd.WriteOptions{Raw: true}); err != nil {
		t.Fatal(err)
	}
	image, err := nrrd.Read(ctx, &encoded, nrrd.ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if image.Volume.Header.Datatype != nifti.DatatypeUint16 || image.Volume.Dimensions != [4]int{2, 1, 1, 2} {
		t.Fatalf("round trip datatype %d dimensions %v", image.Volume.Header.Datatype, image.Volume.Dimensions)
	}
	for i, want := range []float64{1, 256, 1, 2} {
		if value := image.Volume.Stored(i); value != want {
			t.Fatalf("label %d = %v, want %v", i, value, want)
		}
	}
	got, err := image.Header.SlicerSegments()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(masks) || got[256].LabelValue != 256 || got[256].Layer != 0 || got[257].LabelValue != 2 || got[257].Layer != 1 {
		t.Fatalf("wrong segment attribution after widening, count %d, final segments %v", len(got), segments[256:])
	}
}

func TestPackSlicerSegmentationRejectsInvalidInput(t *testing.T) {
	mask := roi.NewRasterMask(2, 2)
	mask.Set(0, 0, true)
	withSlice := nrrd.SlicerSegmentMask{Slices: []*roi.RasterMask{mask}}
	for _, tt := range []struct {
		name       string
		dimensions [3]int
		masks      []nrrd.SlicerSegmentMask
		limits     nifti.ReadLimits
		want       error
	}{
		{"zero dimension", [3]int{0, 2, 1}, []nrrd.SlicerSegmentMask{withSlice}, nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"wrong plane count", [3]int{2, 2, 2}, []nrrd.SlicerSegmentMask{withSlice}, nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"wrong mask size", [3]int{3, 2, 1}, []nrrd.SlicerSegmentMask{withSlice}, nifti.ReadLimits{}, nrrd.ErrMalformedHeader},
		{"voxel arithmetic overflow", [3]int{math.MaxInt32, math.MaxInt32, math.MaxInt32}, []nrrd.SlicerSegmentMask{{}}, nifti.ReadLimits{MaxVoxels: math.MaxUint64, MaxPayloadBytes: math.MaxUint64}, nrrd.ErrLimitExceeded},
		{"overlap exceeds voxel limit", [3]int{2, 2, 1}, []nrrd.SlicerSegmentMask{withSlice, withSlice}, nifti.ReadLimits{MaxVoxels: 7}, nrrd.ErrLimitExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			volume, segments, err := nrrd.PackSlicerSegmentation(context.Background(), tt.dimensions, packTestAffine, tt.masks, tt.limits)
			if volume != nil || segments != nil || !errors.Is(err, tt.want) {
				t.Fatalf("got %v, %v, %v; want %v", volume, segments, err, tt.want)
			}
		})
	}
}

type cancelingSlicerWriter struct{ cancel context.CancelFunc }

func (w cancelingSlicerWriter) Write(p []byte) (int, error) {
	w.cancel()
	return len(p), nil
}

func TestPackSlicerSegmentationCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	dimensions := [3]int{2, 2, 1}
	masks := []nrrd.SlicerSegmentMask{{}}
	volume, segments, err := nrrd.PackSlicerSegmentation(ctx, dimensions, packTestAffine, masks, nifti.ReadLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if err := nrrd.WriteSlicerSegmentationContext(ctx, cancelingSlicerWriter{cancel}, volume, segments, nrrd.WriteOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled write = %v", err)
	}
	if volume, segments, err := nrrd.PackSlicerSegmentation(ctx, dimensions, packTestAffine, masks, nifti.ReadLimits{}); volume != nil || segments != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pack returned %v, %v, %v", volume, segments, err)
	}
}
