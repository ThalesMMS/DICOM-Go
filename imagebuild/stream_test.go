package imagebuild

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
)

type streamTestSlices struct {
	slices []Slice
	reads  int
	failAt int
}

var errStreamTestSource = errors.New("synthetic late source failure")

func (s *streamTestSlices) Slices() int { return len(s.slices) }
func (s *streamTestSlices) Slice(ctx context.Context, index int) (Slice, error) {
	if err := ctx.Err(); err != nil {
		return Slice{}, err
	}
	s.reads++
	if s.failAt > 0 && s.reads == s.failAt {
		return Slice{}, errStreamTestSource
	}
	return s.slices[index], nil
}

func streamTestInput(iod IOD) (Geometry, []Slice, Options) {
	g := obliqueGeometry()
	g.FrameOfReferenceUID = "1.2.1483.4"
	slices := []Slice{{Position: [3]float64{1, 2, 3}}, {Position: [3]float64{4, 5, 6}}, {Position: [3]float64{7, 8, 9}}}
	for i := range slices {
		for p := 0; p < g.Rows*g.Columns; p++ {
			slices[i].Values = append(slices[i].Values, float64(i*1000+p)*.31-70.5)
		}
	}
	opts := Options{IOD: iod, Common: object.DerivedCommon{Source: commontest.Source()}, SeriesInstanceUID: "1.2.1483.3", SOPInstanceUIDs: []string{"1.2.1483.10", "1.2.1483.11", "1.2.1483.12"}, ContentTime: time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)}
	opts.Common.Now = opts.ContentTime
	return g, slices, opts
}

func TestWriteSeriesMatchesWholeGlobalRescaleAndMetadata(t *testing.T) {
	for _, iod := range []IOD{CT, MR, SecondaryCapture} {
		t.Run(fmt.Sprint(iod), func(t *testing.T) {
			g, slices, opts := streamTestInput(iod)
			whole, err := Build(g, slices, opts)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				for _, f := range whole {
					_ = f.Close()
				}
			}()
			source := &streamTestSlices{slices: slices}
			result, err := WriteSeries(context.Background(), g, source, opts, StreamOptions{MaxSlices: 3, MaxPixelBufferBytes: 120, Replayable: true}, func(i int, write func(io.Writer) error) error {
				var got, want bytes.Buffer
				if err := write(&got); err != nil {
					return err
				}
				if err := object.WriteFile(&want, whole[i]); err != nil {
					return err
				}
				if !bytes.Equal(got.Bytes(), want.Bytes()) {
					t.Fatalf("slice %d differs in pixels or metadata", i)
				}
				return nil
			})
			if err != nil || result.Instances != 3 || source.reads != 6 || result.PixelBufferBytes != 120 || result.SeriesInstanceUID != opts.SeriesInstanceUID || result.StudyInstanceUID == "" {
				t.Fatalf("result=%+v reads=%d err=%v", result, source.reads, err)
			}
		})
	}
}

func TestWriteSeriesRejectsMaskBeforeEmitting(t *testing.T) {
	geometry, input, opts := streamTestInput(CT)
	input[0].Valid = make([]bool, len(input[0].Values))
	source := &streamTestSlices{slices: input}
	emitted := false
	result, err := WriteSeries(context.Background(), geometry, source, opts,
		StreamOptions{MaxSlices: 3, MaxPixelBufferBytes: 120, Replayable: true},
		func(int, func(io.Writer) error) error {
			emitted = true
			return nil
		})
	if !errors.Is(err, ErrInvalid) || emitted || result.Instances != 0 {
		t.Fatalf("masked stream emitted=%v instances=%d err=%v", emitted, result.Instances, err)
	}
}

func TestWriteSeriesOnePassFixedRescale(t *testing.T) {
	g, slices, opts := streamTestInput(CT)
	for _, signed := range []uint16{0, 1} {
		for i := range slices {
			for p := range slices[i].Values {
				slices[i].Values[p] = float64(p+i)*2 + 10
				if signed == 1 {
					slices[i].Values[p] -= 30
				}
			}
		}
		opts.Rescale = &Rescale{Slope: 2, Intercept: 10}
		source := &streamTestSlices{slices: slices}
		result, err := WriteSeries(context.Background(), g, source, opts, StreamOptions{MaxSlices: 3, MaxPixelBufferBytes: 120, StoredRepresentation: &signed}, func(_ int, write func(io.Writer) error) error { return write(io.Discard) })
		if err != nil || result.Instances != 3 || source.reads != 3 {
			t.Fatalf("signed=%d result=%+v reads=%d err=%v", signed, result, source.reads, err)
		}
	}
}

func TestWriteSeriesBudgetReplayLateFailureAndCancellation(t *testing.T) {
	g, slices, opts := streamTestInput(CT)
	emit := func(_ int, write func(io.Writer) error) error { return write(io.Discard) }
	for _, limits := range []StreamOptions{{MaxSlices: 3, MaxPixelBufferBytes: 119, Replayable: true}, {MaxSlices: 2, MaxPixelBufferBytes: 120, Replayable: true}, {MaxSlices: 3, MaxPixelBufferBytes: 120}} {
		source := &streamTestSlices{slices: slices}
		if _, err := WriteSeries(context.Background(), g, source, opts, limits, emit); !errors.Is(err, ErrInvalid) || source.reads != 0 {
			t.Fatalf("unadmitted source read: reads=%d err=%v", source.reads, err)
		}
	}
	limits := StreamOptions{MaxSlices: 3, MaxPixelBufferBytes: 120, Replayable: true}
	source := &streamTestSlices{slices: slices, failAt: 6}
	result, err := WriteSeries(context.Background(), g, source, opts, limits, emit)
	if !errors.Is(err, errStreamTestSource) || result.Instances != 2 {
		t.Fatalf("late failure=%+v %v", result, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	source = &streamTestSlices{slices: slices}
	result, err = WriteSeries(ctx, g, source, opts, limits, func(_ int, write func(io.Writer) error) error { cancel(); return write(io.Discard) })
	if !errors.Is(err, context.Canceled) || result.Instances != 0 || source.reads != 4 {
		t.Fatalf("cancel=%+v reads=%d %v", result, source.reads, err)
	}
	source = &streamTestSlices{slices: slices}
	result, err = WriteSeries(context.Background(), g, source, opts, limits, func(_ int, write func(io.Writer) error) error { _ = write(streamTestFailedWriter{}); return nil })
	if !errors.Is(err, io.ErrClosedPipe) || result.Instances != 0 {
		t.Fatalf("ignored write error succeeded: %+v %v", result, err)
	}
	source = &streamTestSlices{slices: slices}
	result, err = WriteSeries(context.Background(), g, source, opts, limits, func(_ int, write func(io.Writer) error) error {
		if err := write(io.Discard); err != nil {
			return err
		}
		_ = write(io.Discard)
		return nil
	})
	if !errors.Is(err, ErrInvalid) || result.Instances != 0 {
		t.Fatalf("ignored second write succeeded: %+v %v", result, err)
	}
}

type streamTestFailedWriter struct{}

func (streamTestFailedWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
