package multiframe

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// splitToMemory splits the object reader reads and reads every written
// instance back.
func splitToMemory(t *testing.T, reader *pixeldata.FrameReader, opts SplitOptions) ([]ConcatenationPart, []*object.File) {
	t.Helper()
	var buffers []*bytes.Buffer
	parts, err := SplitConcatenation(context.Background(), reader, opts, func(ConcatenationPart) (io.Writer, error) {
		buffers = append(buffers, &bytes.Buffer{})
		return buffers[len(buffers)-1], nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(buffers) != len(parts) {
		t.Fatalf("%d instances written for %d parts", len(buffers), len(parts))
	}
	files := make([]*object.File, len(buffers))
	for i, buffer := range buffers {
		file, err := object.ReadFile(bytes.NewReader(buffer.Bytes()))
		if err != nil {
			t.Fatalf("instance %d: %v", i+1, err)
		}
		files[i] = file
	}
	return parts, files
}

// checkRoundTrip regroups the instances and compares every logical frame,
// as stored, with the source frame, and the Concatenation attributes with
// the parts.
func checkRoundTrip(t *testing.T, source *pixeldata.FrameReader, parts []ConcatenationPart, files []*object.File) *Concatenation {
	t.Helper()
	groups, others := GroupConcatenations(files)
	if len(groups) != 1 || len(others) != 0 {
		t.Fatalf("%d groups, %d others", len(groups), len(others))
	}
	concatenation, err := NewConcatenation(groups[0].Files)
	if err != nil {
		t.Fatal(err)
	}
	sourceUID := cleanUID(source.File().Dataset, tagSOPInstanceUID)
	if concatenation.SourceSOPInstanceUID != sourceUID || concatenation.NumberOfFrames() != source.Metadata().NumberOfFrames || len(concatenation.Instances) != len(parts) {
		t.Fatalf("concatenation of %q with %d frames in %d instances", concatenation.SourceSOPInstanceUID, concatenation.NumberOfFrames(), len(concatenation.Instances))
	}
	readers := map[*object.File]*pixeldata.FrameReader{}
	for i, instance := range concatenation.Instances {
		part := parts[i]
		if instance.Number != part.Number || instance.FrameOffset != part.FirstFrame || instance.Frames != part.Frames {
			t.Fatalf("instance %+v, part %+v", instance, part)
		}
		dataset := instance.File.Dataset
		if total, _ := intValue(dataset, tagInConcatenationTotalNumber); total != len(parts) {
			t.Fatalf("instance %d In-concatenation Total Number %d", instance.Number, total)
		}
		if uid := cleanUID(dataset, tagSOPInstanceUID); uid == sourceUID || uid != cleanUID(instance.File.Meta, core.NewTag(0x0002, 0x0003)) {
			t.Fatalf("instance %d SOP Instance UID %q (Media Storage %q)", instance.Number, uid, cleanUID(instance.File.Meta, core.NewTag(0x0002, 0x0003)))
		}
		if !sameValue(dataset, source.File().Dataset, tagInstanceNumber) {
			t.Fatalf("instance %d changed Instance Number", instance.Number)
		}
	}
	for frame := 0; frame < concatenation.NumberOfFrames(); frame++ {
		instance, local, err := concatenation.Locate(frame)
		if err != nil {
			t.Fatal(err)
		}
		reader := readers[instance.File]
		if reader == nil {
			if reader, err = pixeldata.NewFrameReader(context.Background(), instance.File, nil, pixeldata.FrameReaderOptions{}); err != nil {
				t.Fatal(err)
			}
			readers[instance.File] = reader
		}
		got, err := reader.RawFrame(context.Background(), local)
		if err != nil {
			t.Fatal(err)
		}
		want, err := source.RawFrame(context.Background(), frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("logical frame %d differs from the source", frame)
		}
	}
	return concatenation
}

func TestSplitConcatenationRoundTripsNativeFrames(t *testing.T) {
	const frames = 10
	source := enhancedFixture(t, frames)
	source.Dataset.Put(derivedio.US(tagRepresentativeFrameNumber, 5))
	reader, err := pixeldata.NewFrameReader(context.Background(), source, nil, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parts, files := splitToMemory(t, reader, SplitOptions{MaxFrames: 3, ConcatenationUID: concatSource + ".77"})
	if len(parts) != 4 || parts[3].Frames != 1 || parts[2].FirstFrame != 6 {
		t.Fatalf("parts %+v", parts)
	}
	concatenation := checkRoundTrip(t, reader, parts, files)
	if concatenation.UID != concatSource+".77" {
		t.Fatalf("Concatenation UID %q", concatenation.UID)
	}
	// Every functional group and position is where the source had it.
	checkLogicalFrames(t, concatenation, frames)
	// Representative Frame Number 5 is frame 2 of the second instance only.
	for _, instance := range concatenation.Instances {
		value, ok := intValue(instance.File.Dataset, tagRepresentativeFrameNumber)
		if (instance.Number == 2) != ok || (ok && value != 2) {
			t.Fatalf("instance %d Representative Frame Number %d %v", instance.Number, value, ok)
		}
	}
	// The source is unchanged.
	if uid := cleanUID(source.Dataset, tagConcatenationUID); uid != "" || mustInt(t, source.Dataset, tagNumberOfFrames) != frames {
		t.Fatal("split changed the source")
	}
}

// TestSplitConcatenationStreamsFromDeferredPixelData splits a file read
// with deferred Pixel Data straight to instance files, holding one frame at
// a time, and with caller-chosen UIDs.
func TestSplitConcatenationStreamsFromDeferredPixelData(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "source.dcm")
	writeObjectFile(t, path, enhancedFixture(t, 7))
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	if reader.File().Dataset.Has(tagPixelDataForTest) {
		if element, _ := reader.File().Dataset.Get(tagPixelDataForTest); element.Value != nil {
			t.Fatal("source Pixel Data was read into memory")
		}
	}
	uids := []string{concatSource + ".201", concatSource + ".202"}
	parts, err := SplitConcatenation(context.Background(), reader, SplitOptions{MaxFrames: 4, SOPInstanceUIDs: uids}, func(part ConcatenationPart) (io.Writer, error) {
		return os.Create(filepath.Join(dir, fmt.Sprintf("part-%d.dcm", part.Number)))
	})
	if err != nil {
		t.Fatal(err)
	}
	var files []*object.File
	for i := range parts {
		files = append(files, openObjectFile(t, filepath.Join(dir, fmt.Sprintf("part-%d.dcm", i+1))))
		if uid := cleanUID(files[i].Dataset, tagSOPInstanceUID); uid != uids[i] {
			t.Fatalf("instance %d SOP Instance UID %q", i+1, uid)
		}
	}
	checkRoundTrip(t, reader, parts, files)
}

// TestSplitConcatenationBoundsEncapsulatedBytes splits RLE frames by stored
// bytes: codestreams are copied unchanged and no instance exceeds the bound
// unless it holds a single frame.
func TestSplitConcatenationBoundsEncapsulatedBytes(t *testing.T) {
	encoders := pixeldata.NewMemoryEncoderRegistry()
	if err := rle.RegisterEncoder(encoders); err != nil {
		t.Fatal(err)
	}
	encoded, _, err := pixeldata.TranscodeFile(context.Background(), enhancedFixture(t, 9), transfer.RLELossless, pixeldata.TranscodeOptions{EncoderRegistry: encoders})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.NewFrameReader(context.Background(), encoded, nil, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	lengths := make([]int64, 9)
	for i := range lengths {
		if lengths[i], err = reader.RawFrameLength(i); err != nil {
			t.Fatal(err)
		}
		raw, _ := reader.RawFrame(context.Background(), i)
		if int64(len(raw)) != lengths[i] {
			t.Fatalf("frame %d: RawFrameLength %d, RawFrame %d bytes", i, lengths[i], len(raw))
		}
	}
	limit := lengths[0] + lengths[1] + lengths[2] - 1 // fewer than three frames fit
	parts, files := splitToMemory(t, reader, SplitOptions{MaxPixelBytes: limit})
	for _, part := range parts {
		var total int64
		for i := part.FirstFrame; i < part.FirstFrame+part.Frames; i++ {
			total += lengths[i]
		}
		if part.Frames > 1 && total > limit {
			t.Fatalf("part %d holds %d bytes over %d", part.Number, total, limit)
		}
		if end := part.FirstFrame + part.Frames; part.Number < len(parts) && total+lengths[end] <= limit {
			t.Fatalf("part %d closed while frame %d still fitted", part.Number, end+1)
		}
		if files[part.Number-1].TransferSyntax.UID != transfer.RLELossless.UID {
			t.Fatalf("part %d in %s", part.Number, files[part.Number-1].TransferSyntax.UID)
		}
	}
	if len(parts) < 4 {
		t.Fatalf("%d parts", len(parts))
	}
	checkRoundTrip(t, reader, parts, files)
}

func TestPlanConcatenationRefusesUnsplittableSources(t *testing.T) {
	lengths := func(n int) []int64 {
		out := make([]int64, n)
		for i := range out {
			out[i] = 24
		}
		return out
	}
	part := splitForTest(t, enhancedFixture(t, 4), 2)[0]
	noShared := enhancedFixture(t, 4)
	noShared.Dataset.Remove(tagSharedFunctionalGroupsSequence)
	for name, tc := range map[string]struct {
		file    *object.File
		lengths []int64
		opts    SplitOptions
	}{
		"no bound":             {enhancedFixture(t, 4), lengths(4), SplitOptions{}},
		"frame count mismatch": {enhancedFixture(t, 4), lengths(3), SplitOptions{MaxFrames: 2}},
		"already concatenated": {part, lengths(2), SplitOptions{MaxFrames: 1}},
		"no functional groups": {noShared, lengths(4), SplitOptions{MaxFrames: 2}},
		"wrong UID count":      {enhancedFixture(t, 4), lengths(4), SplitOptions{MaxFrames: 2, SOPInstanceUIDs: []string{"1.2.3"}}},
		"invalid UID":          {enhancedFixture(t, 4), lengths(4), SplitOptions{MaxFrames: 2, ConcatenationUID: "1..2"}},
		"duplicate UID":        {enhancedFixture(t, 4), lengths(4), SplitOptions{MaxFrames: 2, SOPInstanceUIDs: []string{"1.2.3", "1.2.3"}}},
		"source UID reused":    {enhancedFixture(t, 4), lengths(4), SplitOptions{MaxFrames: 2, SOPInstanceUIDs: []string{concatSource, "1.2.3"}}},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := PlanConcatenation(tc.file, tc.lengths, tc.opts); !errors.Is(err, ErrConcatenationSplit) {
				t.Fatalf("err = %v", err)
			}
		})
	}
	// A frame larger than the byte bound gets an instance of its own.
	parts, err := PlanConcatenation(enhancedFixture(t, 3), []int64{10, 50, 10}, SplitOptions{MaxPixelBytes: 20})
	if err != nil || len(parts) != 3 || parts[1].Frames != 1 {
		t.Fatalf("parts %+v err %v", parts, err)
	}
}

// generatedFrames is a FrameSource of 16-bit frames whose samples encode
// their frame number, generated on demand.
type generatedFrames struct{ frames, size int }

func (g generatedFrames) Frames() int                    { return g.frames }
func (g generatedFrames) FrameLength(int) (int64, error) { return int64(g.size), nil }
func (g generatedFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	frame := bytes.Repeat([]byte{byte(i), byte(i >> 8)}, g.size/2)
	n, err := w.Write(frame)
	return int64(n), err
}

// heapSampler records the largest heap growth over a baseline each time
// the split writes to it.
type heapSampler struct {
	base, peak uint64
}

func (h *heapSampler) Write(p []byte) (int, error) {
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	if m.HeapAlloc > h.base && m.HeapAlloc-h.base > h.peak {
		h.peak = m.HeapAlloc - h.base
	}
	return len(p), nil
}

// TestSplitConcatenationMemoryIsPerFrame splits 12 MiB of deferred Pixel
// Data (128 KiB frames) into instances while sampling the heap at every
// write: the split holds about one frame, never the object.
func TestSplitConcatenationMemoryIsPerFrame(t *testing.T) {
	const rows, columns, frames = 256, 256, 96
	path := filepath.Join(t.TempDir(), "large.dcm")
	header := enhancedFixture(t, frames)
	header.Dataset.Put(derivedio.US(tagRows, rows))
	header.Dataset.Put(derivedio.US(tagColumns, columns))
	header.Dataset.Remove(tagPixelDataForTest)
	out, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFileFrames(out, header, generatedFrames{frames: frames, size: rows * columns * 2}, object.FrameWriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := out.Close(); err != nil {
		t.Fatal(err)
	}
	reader, err := pixeldata.OpenFrameReader(context.Background(), path, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })

	defer debug.SetGCPercent(debug.SetGCPercent(5))
	runtime.GC()
	var base runtime.MemStats
	runtime.ReadMemStats(&base)
	sampler := &heapSampler{base: base.HeapAlloc}
	parts, err := SplitConcatenation(context.Background(), reader, SplitOptions{MaxPixelBytes: 16 * rows * columns * 2}, func(ConcatenationPart) (io.Writer, error) {
		return sampler, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(parts) != 6 {
		t.Fatalf("%d parts", len(parts))
	}
	t.Logf("peak heap growth %d bytes, frame %d bytes", sampler.peak, rows*columns*2)
	if limit := uint64(6 << 20); sampler.peak > limit {
		t.Fatalf("peak heap growth %d bytes for 12 MiB of Pixel Data, want under %d", sampler.peak, limit)
	}
}

// TestSplitConcatenationLoadsInDCMTK is opt-in evidence that DCMTK's
// ConcatenationLoader merges the instances SplitConcatenation writes into
// the source frames. Enable as TestConcatenationsMatchDCMTK.
func TestSplitConcatenationLoadsInDCMTK(t *testing.T) {
	oracle := concatOracle(t)
	dir := t.TempDir()
	const frames = 10
	source := filepath.Join(dir, "source.dcm")
	writeObjectFile(t, source, enhancedFixture(t, frames))
	reader, err := pixeldata.OpenFrameReader(context.Background(), source, pixeldata.FrameReaderOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reader.Close() })
	var paths []string
	if _, err := SplitConcatenation(context.Background(), reader, SplitOptions{MaxFrames: 3}, func(part ConcatenationPart) (io.Writer, error) {
		path := filepath.Join(dir, fmt.Sprintf("part-%d.dcm", part.Number))
		paths = append(paths, path)
		return os.Create(path)
	}); err != nil {
		t.Fatal(err)
	}
	merged := filepath.Join(dir, "merged.dcm")
	if got := runConcatOracle(t, oracle, append([]string{"merge", merged}, paths...)...); got != fmt.Sprint(frames) {
		t.Fatalf("DCMTK merged %s frames", got)
	}
	view, err := pixeldata.ExtractNativeFramesView(openObjectFile(t, merged).Dataset)
	if err != nil {
		t.Fatal(err)
	}
	for frame := 0; frame < frames; frame++ {
		want, err := reader.RawFrame(context.Background(), frame)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(view.Data[frame], want) {
			t.Fatalf("frame %d differs after DCMTK's merge", frame)
		}
	}
}
