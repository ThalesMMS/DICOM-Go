package multiframe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

const (
	concatRows    = 3
	concatColumns = 4
	concatSource  = "1.2.826.0.1.3680043.10.543.1001.1"
)

var (
	tagPlanePositionSequenceForTest   = core.NewTag(0x0020, 0x9113)
	tagFrameContentSequenceForTest    = core.NewTag(0x0020, 0x9111)
	tagDimensionIndexValuesForTest    = core.NewTag(0x0020, 0x9157)
	tagPixelMeasuresSequenceForTest   = core.NewTag(0x0028, 0x9110)
	tagPlaneOrientationSeqForTest     = core.NewTag(0x0020, 0x9116)
	tagPixelDataForTest               = core.NewTag(0x7FE0, 0x0010)
	enhancedCTStorageForConcatenation = "1.2.840.10008.5.1.4.1.1.2.1"
)

// framePixel is the 16-bit sample of pixel i of frame f.
func framePixel(frame, i int) uint16 { return uint16(frame*100 + i) }

func enhancedFixture(t testing.TB, frames int) *object.File {
	t.Helper()
	perFrame := make([]core.DataSet, frames)
	pixels := make([]byte, 0, frames*concatRows*concatColumns*2)
	for frame := 0; frame < frames; frame++ {
		perFrame[frame] = derivedio.DataSet(
			derivedio.Seq(tagFrameContentSequenceForTest, derivedio.DataSet(
				core.NewRawElement(tagDimensionIndexValuesForTest, core.VRUL, binary.LittleEndian.AppendUint32(nil, uint32(frame+1))),
			)),
			derivedio.Seq(tagPlanePositionSequenceForTest, derivedio.DataSet(
				derivedio.DS(tagImagePositionPatient, 0, 0, 1.25*float64(frame)),
			)),
		)
		for i := 0; i < concatRows*concatColumns; i++ {
			pixels = binary.LittleEndian.AppendUint16(pixels, framePixel(frame, i))
		}
	}
	dataset := derivedio.Object(
		derivedio.CS(tagImageType, `ORIGINAL\PRIMARY\AXIAL`),
		derivedio.UI(tagSOPClassUID, enhancedCTStorageForConcatenation),
		derivedio.UI(tagSOPInstanceUID, concatSource),
		derivedio.CS(tagModality, "CT"),
		derivedio.UI(tagStudyInstanceUID, "1.2.826.0.1.3680043.10.543.1001.2"),
		derivedio.UI(tagSeriesInstanceUID, "1.2.826.0.1.3680043.10.543.1001.3"),
		derivedio.UI(tagFrameOfReferenceUID, "1.2.826.0.1.3680043.10.543.1001.4"),
		derivedio.IS(tagInstanceNumber, 1),
		derivedio.US(tagSamplesPerPixel, 1),
		derivedio.CS(tagPhotometricInterpretation, "MONOCHROME2"),
		derivedio.IS(tagNumberOfFrames, frames),
		derivedio.US(tagRows, concatRows),
		derivedio.US(tagColumns, concatColumns),
		derivedio.US(tagBitsAllocated, 16),
		derivedio.US(tagBitsStored, 16),
		derivedio.US(tagHighBit, 15),
		derivedio.US(tagPixelRepresentation, 0),
		derivedio.Seq(tagSharedFunctionalGroupsSequence, derivedio.DataSet(
			derivedio.Seq(tagPixelMeasuresSequenceForTest, derivedio.DataSet(derivedio.DS(tagPixelSpacing, 0.5, 0.5))),
			derivedio.Seq(tagPlaneOrientationSeqForTest, derivedio.DataSet(derivedio.DS(tagImageOrientationPatient, 1, 0, 0, 0, 1, 0))),
		)),
		derivedio.Seq(tagPerFrameFunctionalGroupsSequence, perFrame...),
		derivedio.Raw(tagPixelDataForTest, core.VROW, pixels),
	)
	file, err := derivedio.File(enhancedCTStorageForConcatenation, concatSource, dataset)
	if err != nil {
		t.Fatal(err)
	}
	return file
}

// splitForTest makes a Concatenation of source in Go: perInstance frames per
// instance, with the Concatenation attributes of PS3.3 C.7.6.16.2.2.4.
func splitForTest(t testing.TB, source *object.File, perInstance int) []*object.File {
	t.Helper()
	frames := int(mustInt(t, source.Dataset, tagNumberOfFrames))
	perFrame, _ := source.Dataset.GetSequence(tagPerFrameFunctionalGroupsSequence)
	pixels, _ := source.Dataset.GetRaw(tagPixelDataForTest)
	frameBytes := len(pixels) / frames
	total := (frames + perInstance - 1) / perInstance
	var out []*object.File
	for number := 1; number <= total; number++ {
		start := (number - 1) * perInstance
		end := min(start+perInstance, frames)
		elements := []core.Element{}
		for _, element := range source.Dataset.Elements() {
			switch element.Tag() {
			case tagPerFrameFunctionalGroupsSequence, tagPixelDataForTest, tagNumberOfFrames, tagSOPInstanceUID:
				continue
			}
			elements = append(elements, element)
		}
		items := make([]core.DataSet, 0, end-start)
		for _, item := range perFrame[start:end] {
			items = append(items, item.ToDataSet())
		}
		uid := fmt.Sprintf("%s.%d", concatSource, number)
		elements = append(elements,
			derivedio.UI(tagSOPInstanceUID, uid),
			derivedio.IS(tagNumberOfFrames, end-start),
			derivedio.UI(tagConcatenationUID, concatSource+".99"),
			derivedio.UI(tagSOPInstanceUIDOfConcatenationSource, concatSource),
			derivedio.US(tagInConcatenationNumber, uint16(number)),
			derivedio.US(tagInConcatenationTotalNumber, uint16(total)),
			core.NewRawElement(tagConcatenationFrameOffsetNumber, core.VRUL, binary.LittleEndian.AppendUint32(nil, uint32(start))),
			derivedio.Seq(tagPerFrameFunctionalGroupsSequence, items...),
			derivedio.Raw(tagPixelDataForTest, core.VROW, append([]byte(nil), pixels[start*frameBytes:end*frameBytes]...)),
		)
		file, err := derivedio.File(enhancedCTStorageForConcatenation, uid, derivedio.Object(elements...))
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, file)
	}
	return out
}

func mustInt(t testing.TB, obj *object.Object, tag core.Tag) int64 {
	t.Helper()
	value, err := obj.GetInt(tag)
	if err != nil {
		t.Fatal(err)
	}
	return value
}

// checkLogicalFrames compares every logical frame of the Concatenation with
// the frames of the source: position through the resolver and pixels read
// from the holding instance.
func checkLogicalFrames(t *testing.T, concatenation *Concatenation, frames int) {
	t.Helper()
	if concatenation.NumberOfFrames() != frames || concatenation.SourceSOPInstanceUID != concatSource {
		t.Fatalf("%d frames of %q, want %d of %q", concatenation.NumberOfFrames(), concatenation.SourceSOPInstanceUID, frames, concatSource)
	}
	views := map[*object.File]*pixeldata.NativeFrames{}
	for frame := 0; frame < frames; frame++ {
		position, source, err := concatenation.FunctionalGroupItem(frame, tagPlanePositionSequenceForTest)
		if err != nil || source != object.FunctionalGroupPerFrame {
			t.Fatalf("frame %d plane position: %v %v", frame, source, err)
		}
		values, _ := position.GetFloats(tagImagePositionPatient)
		if len(values) != 3 || values[2] != 1.25*float64(frame) {
			t.Fatalf("frame %d at %v", frame, values)
		}
		geometry, err := concatenation.FrameGeometryAt(frame)
		if err != nil || len(geometry.PixelSpacing) != 2 || geometry.ImagePositionPatient[2] != 1.25*float64(frame) {
			t.Fatalf("frame %d geometry %+v %v", frame, geometry, err)
		}
		instance, local, err := concatenation.Locate(frame)
		if err != nil {
			t.Fatal(err)
		}
		view := views[instance.File]
		if view == nil {
			view, err = pixeldata.ExtractNativeFramesView(instance.File.Dataset)
			if err != nil {
				t.Fatal(err)
			}
			views[instance.File] = view
		}
		data := view.Data[local]
		for i := 0; i < concatRows*concatColumns; i++ {
			if got := binary.LittleEndian.Uint16(data[2*i:]); got != framePixel(frame, i) {
				t.Fatalf("frame %d pixel %d = %d, want %d", frame, i, got, framePixel(frame, i))
			}
		}
	}
	if _, _, err := concatenation.Locate(frames); !errors.Is(err, ErrConcatenationIncomplete) {
		t.Fatalf("frame past the end: %v", err)
	}
}

func TestConcatenationLogicalView(t *testing.T) {
	const frames = 10
	instances := splitForTest(t, enhancedFixture(t, frames), 3)
	shuffled := append([]*object.File{enhancedFixture(t, 2)}, instances...) // a file of no Concatenation
	rand.New(rand.NewSource(1)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
	groups, others := GroupConcatenations(shuffled)
	if len(groups) != 1 || len(others) != 1 || len(groups[0].Files) != 4 {
		t.Fatalf("%d groups, %d others", len(groups), len(others))
	}
	concatenation, err := NewConcatenation(groups[0].Files)
	if err != nil {
		t.Fatal(err)
	}
	for i, instance := range concatenation.Instances {
		if instance.Number != i+1 || instance.FrameOffset != 3*i {
			t.Fatalf("instance %d: %+v", i, instance)
		}
	}
	checkLogicalFrames(t, concatenation, frames)
}

func TestConcatenationValidation(t *testing.T) {
	put := func(file *object.File, element core.Element) *object.File {
		file.Dataset.Put(element)
		return file
	}
	for name, tc := range map[string]struct {
		mutate func([]*object.File) []*object.File
		want   error
	}{
		"missing instance":      {func(f []*object.File) []*object.File { return append(f[:1], f[2:]...) }, ErrConcatenationIncomplete},
		"missing last instance": {func(f []*object.File) []*object.File { return f[:3] }, ErrConcatenationIncomplete},
		"duplicate number": {func(f []*object.File) []*object.File {
			put(f[1], derivedio.US(tagInConcatenationNumber, 1))
			return f
		}, ErrConcatenationInconsistent},
		"wrong offset": {func(f []*object.File) []*object.File {
			put(f[2], core.NewRawElement(tagConcatenationFrameOffsetNumber, core.VRUL, binary.LittleEndian.AppendUint32(nil, 5)))
			return f
		}, ErrConcatenationInconsistent},
		"different rows": {func(f []*object.File) []*object.File {
			put(f[3], derivedio.US(tagRows, concatRows+1))
			return f
		}, ErrConcatenationInconsistent},
		"different total": {func(f []*object.File) []*object.File {
			put(f[0], derivedio.US(tagInConcatenationTotalNumber, 5))
			return f
		}, ErrConcatenationInconsistent},
		"different source": {func(f []*object.File) []*object.File {
			put(f[1], derivedio.UI(tagSOPInstanceUIDOfConcatenationSource, "1.2.3"))
			return f
		}, ErrConcatenationInconsistent},
		"frame count without groups": {func(f []*object.File) []*object.File {
			put(f[1], derivedio.IS(tagNumberOfFrames, 2))
			return f
		}, ErrConcatenationInconsistent},
		"other concatenation": {func(f []*object.File) []*object.File {
			put(f[2], derivedio.UI(tagConcatenationUID, "1.2.3.4"))
			return f
		}, ErrConcatenationInconsistent},
	} {
		t.Run(name, func(t *testing.T) {
			files := tc.mutate(splitForTest(t, enhancedFixture(t, 10), 3))
			if _, err := NewConcatenation(files); !errors.Is(err, tc.want) {
				t.Fatalf("NewConcatenation error = %v, want %v", err, tc.want)
			}
		})
	}
	if _, err := NewConcatenation(nil); !errors.Is(err, ErrConcatenationIncomplete) {
		t.Fatalf("no instance: %v", err)
	}
}

// TestLargeConcatenation follows DCMTK's huge Concatenation test at a size
// that stays fast: offsets and lookups across many instances.
func TestLargeConcatenation(t *testing.T) {
	const frames = 3000
	instances := splitForTest(t, enhancedFixture(t, frames), 250)
	concatenation, err := NewConcatenation(instances)
	if err != nil {
		t.Fatal(err)
	}
	if len(concatenation.Instances) != 12 || concatenation.NumberOfFrames() != frames {
		t.Fatalf("%d instances, %d frames", len(concatenation.Instances), concatenation.NumberOfFrames())
	}
	for _, frame := range []int{0, 249, 250, 1999, 2750, 2999} {
		instance, local, err := concatenation.Locate(frame)
		if err != nil || instance.Number != frame/250+1 || local != frame%250 {
			t.Fatalf("frame %d: instance %d local %d %v", frame, instance.Number, local, err)
		}
	}
}

func concatOracle(t *testing.T) string {
	t.Helper()
	dcmtkoracle.Require(t)
	path := os.Getenv("DICOMGO_DCMTK_CONCAT")
	if path == "" {
		t.Skip("set DICOMGO_DCMTK_CONCAT to dcmtk_concat built from multiframe/testdata/dcmtk_concat.cc")
	}
	return path
}

func writeObjectFile(t *testing.T, path string, file *object.File) {
	t.Helper()
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := object.WriteFile(f, file); err != nil {
		t.Fatal(err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
}

func openObjectFile(t *testing.T, path string) *object.File {
	t.Helper()
	file, err := object.OpenFile(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}

func runConcatOracle(t *testing.T, oracle string, args ...string) string {
	t.Helper()
	var out, stderr bytes.Buffer
	cmd := dcmtkoracle.Command(context.Background(), t, dcmtkoracle.Exact, oracle, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dcmtk_concat %v: %v\n%s", args, err, stderr.String())
	}
	return strings.TrimSpace(out.String())
}

// TestConcatenationsMatchDCMTK is opt-in evidence for #1001: Concatenations
// DCMTK's ConcatenationCreator makes from a dicom-go enhanced object load
// here with every frame and functional group in place, DCMTK's
// ConcatenationLoader merges the same instances into the same frames, and an
// incomplete set is refused by both. Enable with DICOMGO_DCMTK_INTEGRATION=1
// and DICOMGO_DCMTK_CONCAT naming dcmtk_concat built from
// testdata/dcmtk_concat.cc.
func TestConcatenationsMatchDCMTK(t *testing.T) {
	oracle := concatOracle(t)
	for _, tc := range []struct{ frames, perInstance int }{{10, 3}, {7, 7}, {25, 4}} {
		t.Run(fmt.Sprintf("%d frames by %d", tc.frames, tc.perInstance), func(t *testing.T) {
			dir := t.TempDir()
			source := filepath.Join(dir, "source.dcm")
			writeObjectFile(t, source, enhancedFixture(t, tc.frames))
			count, err := strconv.Atoi(runConcatOracle(t, oracle, "split", source, strconv.Itoa(tc.perInstance), filepath.Join(dir, "part")))
			if err != nil || count != (tc.frames+tc.perInstance-1)/tc.perInstance {
				t.Fatalf("DCMTK wrote %d instances (%v)", count, err)
			}
			var paths []string
			var files []*object.File
			for i := count; i >= 1; i-- { // reverse order: grouping must sort
				path := filepath.Join(dir, fmt.Sprintf("part-%d.dcm", i))
				paths = append(paths, path)
				files = append(files, openObjectFile(t, path))
			}
			groups, others := GroupConcatenations(files)
			if len(groups) != 1 || len(others) != 0 {
				t.Fatalf("%d groups, %d others", len(groups), len(others))
			}
			concatenation, err := NewConcatenation(groups[0].Files)
			if err != nil {
				t.Fatal(err)
			}
			checkLogicalFrames(t, concatenation, tc.frames)

			merged := filepath.Join(dir, "merged.dcm")
			if got := runConcatOracle(t, oracle, append([]string{"merge", merged}, paths...)...); got != strconv.Itoa(tc.frames) {
				t.Fatalf("DCMTK merged %s frames", got)
			}
			view, err := pixeldata.ExtractNativeFramesView(openObjectFile(t, merged).Dataset)
			if err != nil {
				t.Fatal(err)
			}
			for frame := 0; frame < tc.frames; frame++ {
				instance, local, _ := concatenation.Locate(frame)
				ours, err := pixeldata.ExtractNativeFramesView(instance.File.Dataset)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(view.Data[frame], ours.Data[local]) {
					t.Fatalf("frame %d differs from DCMTK's merge", frame)
				}
			}
			if count > 1 {
				// Without one instance dicom-go refuses the Concatenation;
				// DCMTK only warns that In-concatenation Total Number does
				// not match and merges the frames it has.
				if _, err := NewConcatenation(files[1:]); !errors.Is(err, ErrConcatenationIncomplete) {
					t.Fatalf("incomplete set: %v", err)
				}
				var stderr bytes.Buffer
				cmd := dcmtkoracle.Command(context.Background(), t, "DCMTK warns that the In-concatenation Total Number does not match", oracle, append([]string{"merge", filepath.Join(dir, "partial.dcm")}, paths[1:]...)...)
				cmd.Stderr = &stderr
				if err := cmd.Run(); err != nil || !strings.Contains(stderr.String(), "does not match number of Instances") {
					t.Fatalf("DCMTK on an incomplete Concatenation: %v\n%s", err, stderr.String())
				}
			}
		})
	}
}
