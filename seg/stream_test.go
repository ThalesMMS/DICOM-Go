package seg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
	"github.com/ThalesMMS/dicom-go/roi"
)

var errSEGStreamSource = errors.New("synthetic SEG source failure")

type countedSEGFrames struct {
	document *Document
	reads    int
	failAt   int
}

func (s *countedSEGFrames) Frame(ctx context.Context, index int) (FramePayload, error) {
	s.reads++
	if s.failAt > 0 && s.reads == s.failAt {
		return FramePayload{}, errSEGStreamSource
	}
	return DocumentFrames(s.document).Frame(ctx, index)
}

func streamingSEGFixture(profile string) *Document {
	d := patternDocument(SegmentationStorage, SegmentationTypeBinary)
	d.ContentDate, d.ContentTime = "20261011", "120000"
	d.Common.Now = time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	d.Segments = []Segment{{Number: 1, Label: "Original synthetic label", AlgorithmType: AlgorithmManual}}
	switch profile {
	case "fractional":
		d.SegmentationType = SegmentationTypeFractional
		d.FractionalType = FractionalTypeProbability
		d.MaxFractionalValue = 200
	case "label8", "label16":
		d.SOPClassUID, d.SegmentationType = LabelMapSegmentationStorage, SegmentationTypeLabelMap
		d.Segments = []Segment{{Number: 0, Label: "Background"}, {Number: 1, Label: "Foreground"}}
		if profile == "label16" {
			d.Segments = append(d.Segments, Segment{Number: 300, Label: "High label"})
		}
	}
	const source = "1.2.1483.200"
	d.ReferencedImages = []ReferencedImage{{SeriesInstanceUID: "1.2.1483.201", SOPClassUID: "1.2.840.10008.5.1.4.1.1.2.1", SOPInstanceUID: source, Frames: []int{1, 2, 3, 4, 5}}}
	var geometry []render.SliceGeometry
	for z := 0; z < 5; z++ {
		g := render.SliceGeometry{Origin: render.Vec3{X: 10, Y: 20, Z: float64(z) * 3}, RowDir: render.Vec3{X: .8, Y: .6}, ColDir: render.Vec3{X: -.6, Y: .8}, Normal: render.Vec3{Z: 1}, RowSpacing: 2, ColSpacing: 1, Rows: patternRows, Columns: patternColumns}
		f := Frame{SegmentNumber: 1, SliceIndex: z, ReferencedSOPInstanceUID: source, ReferencedFrameNumber: z + 1, Geometry: g}
		switch profile {
		case "binary":
			f.Mask = roi.NewRasterMask(patternColumns, patternRows)
			f.Mask.SetRun(z%patternRows, 1, 6)
		case "fractional":
			f.Fractional = make([]uint8, patternRows*patternColumns)
			for i := range f.Fractional {
				f.Fractional[i] = fractionalPattern(1, z, i, 200)
			}
		case "label8", "label16":
			f.LabelMap = make([]uint16, patternRows*patternColumns)
			for i := range f.LabelMap {
				f.LabelMap[i] = uint16((i + z) % 2)
				if profile == "label16" && i%3 == 0 {
					f.LabelMap[i] = 300
				}
			}
		}
		geometry = append(geometry, g)
		d.Frames = append(d.Frames, f)
	}
	d.Geometry = render.BuildVolumeGeometry(geometry, render.DefaultGeometryTolerances())
	return d
}

func segStreamLimits() StreamWriteOptions {
	return StreamWriteOptions{MaxFrames: 100, MaxFrameReads: 1000, MaxPixelBufferBytes: 1 << 20, MaxMetadataBytes: 16 << 20}
}

func TestSEGWriteFramesBudgetsEquipmentBeforeReading(t *testing.T) {
	d := streamingSEGFixture("binary")
	d.Common.Equipment = &object.Equipment{Manufacturer: strings.Repeat("M", 1<<20)}
	source := &countedSEGFrames{document: d}
	if _, err := WriteFrames(context.Background(), io.Discard, d, source, segStreamLimits()); !errors.Is(err, ErrResourceLimitExceeded) || source.reads != 0 {
		t.Fatalf("unadmitted equipment read source: %v reads=%d", err, source.reads)
	}
}

func TestSEGWriteFramesOverlapReadBudgetUsesSourceOrder(t *testing.T) {
	d := streamingSEGFixture("binary")
	d.Frames = d.Frames[:4]
	d.Segments = append(d.Segments, Segment{Number: 2, Label: "Second"})
	for _, pair := range []int{0, 2} {
		d.Frames[pair+1].Geometry = d.Frames[pair].Geometry
		d.Frames[pair+1].SegmentNumber = 2
		d.Frames[pair].Mask = roi.NewRasterMask(patternColumns, patternRows)
		d.Frames[pair+1].Mask = roi.NewRasterMask(patternColumns, patternRows)
		d.Frames[pair].Mask.Set(1, 0, true)
		x := 1
		if pair == 2 {
			x = 2
		}
		d.Frames[pair+1].Mask.Set(x, 0, true)
	}
	limits := segStreamLimits()
	limits.MaxFrameReads = 10 // validation 4, first overlapping pair 2, emission 4
	for repeat := 0; repeat < 20; repeat++ {
		result, err := WriteFrames(context.Background(), io.Discard, d, DocumentFrames(d), limits)
		if err != nil || result.Frames != 4 || result.FrameReads != 10 {
			t.Fatalf("read-cap outcome varied: %+v %v", result, err)
		}
	}
}

func TestSEGWriteFramesMatchesWholePixelsGeometryAndReferences(t *testing.T) {
	for _, profile := range []string{"binary", "fractional", "label8", "label16"} {
		t.Run(profile, func(t *testing.T) {
			d := streamingSEGFixture(profile)
			whole, err := Write(d)
			if err != nil {
				t.Fatal(err)
			}
			defer whole.Close()
			var want, got bytes.Buffer
			if err := object.WriteFile(&want, whole); err != nil {
				t.Fatal(err)
			}
			source := &countedSEGFrames{document: d}
			result, err := WriteFrames(context.Background(), &got, d, source, segStreamLimits())
			if err != nil || result.Frames != 5 || source.reads != 10 || result.FrameReads != 10 {
				t.Fatalf("result=%+v reads=%d %v", result, source.reads, err)
			}
			if !bytes.Equal(got.Bytes(), want.Bytes()) {
				t.Fatalf("%s differs in pixels, geometry or metadata", profile)
			}
			f, err := object.ReadFile(bytes.NewReader(got.Bytes()))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			if _, err := Read(f.Dataset); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSEGWriteFramesExactOverlapWithBoundedReplay(t *testing.T) {
	for _, fractional := range []bool{false, true} {
		for _, overlap := range []bool{false, true} {
			t.Run(fmt.Sprintf("fractional%v-overlap%v", fractional, overlap), func(t *testing.T) {
				profile := "binary"
				if fractional {
					profile = "fractional"
				}
				d := streamingSEGFixture(profile)
				d.Frames = d.Frames[:2]
				d.Segments = append(d.Segments, Segment{Number: 2, Label: "Second"})
				d.Frames[1].Geometry, d.Frames[1].SegmentNumber = d.Frames[0].Geometry, 2
				if fractional {
					clear(d.Frames[0].Fractional)
					clear(d.Frames[1].Fractional)
					d.Frames[0].Fractional[1] = 1
					index := 2
					if overlap {
						index = 1
					}
					d.Frames[1].Fractional[index] = 1
				} else {
					d.Frames[0].Mask = roi.NewRasterMask(patternColumns, patternRows)
					d.Frames[0].Mask.Set(1, 0, true)
					d.Frames[1].Mask = roi.NewRasterMask(patternColumns, patternRows)
					x := 2
					if overlap {
						x = 1
					}
					d.Frames[1].Mask.Set(x, 0, true)
				}
				whole, err := Write(d)
				if err != nil {
					t.Fatal(err)
				}
				defer whole.Close()
				var want, got bytes.Buffer
				if err := object.WriteFile(&want, whole); err != nil {
					t.Fatal(err)
				}
				if _, err := WriteFrames(context.Background(), &got, d, DocumentFrames(d), segStreamLimits()); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(got.Bytes(), want.Bytes()) {
					t.Fatal("overlap attribute or samples differ")
				}
				limits := segStreamLimits()
				limits.MaxFrameReads = 4
				if _, err := WriteFrames(context.Background(), io.Discard, d, DocumentFrames(d), limits); !errors.Is(err, ErrResourceLimitExceeded) {
					t.Fatalf("read cap=%v", err)
				}
			})
		}
	}
}

func TestSEGWriteFramesAdmissionLateFailureCancellationAndOutputError(t *testing.T) {
	d := streamingSEGFixture("binary")
	for _, cap := range []string{"frames", "metadata", "pixels"} {
		limits := segStreamLimits()
		switch cap {
		case "frames":
			limits.MaxFrames = 4
		case "metadata":
			limits.MaxMetadataBytes = 1
		case "pixels":
			limits.MaxPixelBufferBytes = 1
		}
		source := &countedSEGFrames{document: d}
		if _, err := WriteFrames(context.Background(), io.Discard, d, source, limits); !errors.Is(err, ErrResourceLimitExceeded) || source.reads != 0 {
			t.Fatalf("%s admission reads=%d %v", cap, source.reads, err)
		}
	}
	var output bytes.Buffer
	source := &countedSEGFrames{document: d, failAt: 8}
	result, err := WriteFrames(context.Background(), &output, d, source, segStreamLimits())
	if !errors.Is(err, errSEGStreamSource) || result.Frames != 0 || result.FrameReads != 8 || output.Len() == 0 {
		t.Fatalf("late source=%+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source = &countedSEGFrames{document: d}
	_, err = WriteFrames(ctx, segCancelWriter{cancel}, d, source, segStreamLimits())
	if !errors.Is(err, context.Canceled) || source.reads != 5 {
		t.Fatalf("cancel reads=%d %v", source.reads, err)
	}
	_, err = WriteFrames(context.Background(), segFailedWriter{}, d, DocumentFrames(d), segStreamLimits())
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
}

type segCancelWriter struct{ cancel context.CancelFunc }

func (w segCancelWriter) Write(data []byte) (int, error) { w.cancel(); return len(data), nil }

type segFailedWriter struct{}

func (segFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
