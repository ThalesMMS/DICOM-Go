package seg

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/render"
)

// segOracle returns the dcmtk_segoracle executable built from
// testdata/dcmtk_segoracle.cc (see the build line there), skipping unless
// DICOMGO_DCMTK_INTEGRATION=1 and DICOMGO_DCMTK_SEGORACLE names it.
func segOracle(t *testing.T) string {
	t.Helper()
	dcmtkoracle.Require(t)
	path := os.Getenv("DICOMGO_DCMTK_SEGORACLE")
	if path == "" {
		t.Skip("set DICOMGO_DCMTK_SEGORACLE to dcmtk_segoracle built from seg/testdata/dcmtk_segoracle.cc")
	}
	return path
}

type oracleKind struct {
	name           string
	sopClassUID    string
	segmentType    string
	fractionalType string
	maxValue       int // FRACTIONAL maximum
	maxLabel       int // LABELMAP largest label
}

var oracleKinds = []oracleKind{
	{name: "binary", sopClassUID: SegmentationStorage, segmentType: SegmentationTypeBinary},
	{name: "fractional-probability", sopClassUID: SegmentationStorage, segmentType: SegmentationTypeFractional, fractionalType: FractionalTypeProbability, maxValue: 200},
	{name: "fractional-occupancy", sopClassUID: SegmentationStorage, segmentType: SegmentationTypeFractional, fractionalType: FractionalTypeOccupancy, maxValue: 255},
	{name: "labelmap8", sopClassUID: LabelMapSegmentationStorage, segmentType: SegmentationTypeLabelMap, maxLabel: 255},
	{name: "labelmap16", sopClassUID: LabelMapSegmentationStorage, segmentType: SegmentationTypeLabelMap, maxLabel: 300},
}

type patternFrame struct {
	segment, slice int
	values         []int
}

// expectedFrames are the frames of a kind, in the order the oracle writes
// them (segment, then slice), with the patterns of dcmtk_segoracle.cc.
func (kind oracleKind) expectedFrames() []patternFrame {
	var frames []patternFrame
	switch kind.segmentType {
	case SegmentationTypeLabelMap:
		for slice := 0; slice < 3; slice++ {
			values := make([]int, patternRows*patternColumns)
			for i := range values {
				values[i] = (i*97 + slice) % (kind.maxLabel + 1)
			}
			frames = append(frames, patternFrame{0, slice, values})
		}
	case SegmentationTypeFractional:
		for segment := 1; segment <= 2; segment++ {
			for slice := 0; slice < 2; slice++ {
				values := make([]int, patternRows*patternColumns)
				for i := range values {
					values[i] = int(fractionalPattern(segment, slice, i, kind.maxValue))
				}
				frames = append(frames, patternFrame{segment, slice, values})
			}
		}
	default:
		for _, frame := range overlapDocument().Frames {
			values := make([]int, patternRows*patternColumns)
			frame.Mask.ForEachPixel(func(x, y int) { values[y*patternColumns+x] = 1 })
			frames = append(frames, patternFrame{frame.SegmentNumber, frame.SliceIndex, values})
		}
	}
	return frames
}

// document builds the kind as a dicom-go Document with the same pixels and
// plane positions (0, 0, 2 x slice) as the oracle.
func (kind oracleKind) document() *Document {
	doc := patternDocument(kind.sopClassUID, kind.segmentType)
	doc.FractionalType, doc.MaxFractionalValue = kind.fractionalType, kind.maxValue
	switch kind.segmentType {
	case SegmentationTypeLabelMap:
		doc = labelMapPatternDocument(kind.maxLabel, 3)
	case SegmentationTypeFractional:
		doc.Segments = []Segment{{Number: 1}, {Number: 2}}
		for _, frame := range kind.expectedFrames() {
			samples := make([]uint8, len(frame.values))
			for i, value := range frame.values {
				samples[i] = uint8(value)
			}
			doc.Frames = append(doc.Frames, Frame{SegmentNumber: frame.segment, SliceIndex: frame.slice, Fractional: samples})
		}
	default:
		doc = overlapDocument()
	}
	for i := range doc.Frames {
		doc.Frames[i].Geometry = oracleGeometry(doc.Frames[i].SliceIndex)
	}
	return doc
}

func oracleGeometry(slice int) render.SliceGeometry {
	return render.SliceGeometry{
		Rows: patternRows, Columns: patternColumns, RowSpacing: 0.5, ColSpacing: 0.5,
		RowDir: render.Vec3{X: 1}, ColDir: render.Vec3{Y: 1}, Normal: render.Vec3{Z: 1},
		Origin: render.Vec3{Z: float64(2 * slice)},
	}
}

// frameValues are the pixel values of a read frame.
func frameValues(doc *Document, frame Frame) []int {
	values := make([]int, doc.Rows*doc.Columns)
	switch {
	case len(frame.Fractional) > 0:
		for i, value := range frame.Fractional {
			values[i] = int(value)
		}
	case len(frame.LabelMap) > 0:
		for i, value := range frame.LabelMap {
			values[i] = int(value)
		}
	case frame.Mask != nil:
		frame.Mask.ForEachPixel(func(x, y int) { values[y*doc.Columns+x] = 1 })
	}
	return values
}

type oracleDump struct {
	header   string
	segments []int
	frames   [][]int
	overlaps [][2]int
	groups   [][]int
}

func runOracle(t *testing.T, oracle string, args ...string) string {
	t.Helper()
	out, _ := runOracleLogged(t, oracle, dcmtkoracle.Exact, args...)
	return out
}

// runOracleLogged runs the oracle and returns its standard output and the
// diagnostics DCMTK logged on standard error.
func runOracleLogged(t *testing.T, oracle, tolerance string, args ...string) (string, string) {
	t.Helper()
	var out, stderr bytes.Buffer
	cmd := dcmtkoracle.Command(context.Background(), t, tolerance, oracle, args...)
	cmd.Stdout, cmd.Stderr = &out, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("dcmtk_segoracle %v: %v\n%s", args, err, stderr.String())
	}
	return out.String(), stderr.String()
}

func parseOracleDump(t *testing.T, text string) oracleDump {
	t.Helper()
	var dump oracleDump
	ints := func(fields []string) []int {
		out := make([]int, len(fields))
		for i, field := range fields {
			value, err := strconv.Atoi(field)
			if err != nil {
				t.Fatalf("dump value %q: %v", field, err)
			}
			out[i] = value
		}
		return out
	}
	for _, line := range strings.Split(strings.TrimSpace(text), "\n") {
		line = strings.TrimSpace(line)
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case fields[0] == "type":
			dump.header = line
		case fields[0] == "segments":
			dump.segments = ints(fields[1:])
		case fields[0] == "frame":
			dump.frames = append(dump.frames, ints(fields[2:]))
		case fields[0] == "overlap":
			pair := ints(fields[1:])
			dump.overlaps = append(dump.overlaps, [2]int{pair[0], pair[1]})
		case fields[0] == "group":
			dump.groups = append(dump.groups, ints(fields[1:]))
		}
	}
	return dump
}

func (dump oracleDump) checkOverlaps(t *testing.T, report OverlapReport) {
	t.Helper()
	var ours [][2]int
	for _, overlap := range report.Overlaps {
		ours = append(ours, [2]int{overlap.First, overlap.Second})
	}
	if !reflect.DeepEqual(ours, dump.overlaps) {
		t.Fatalf("overlapping pairs: dicom-go %v, DCMTK %v", ours, dump.overlaps)
	}
	if groups := report.DisjointGroups(); !reflect.DeepEqual(groups, dump.groups) {
		t.Fatalf("disjoint groups: dicom-go %v, DCMTK %v", groups, dump.groups)
	}
}

// TestSegmentationsMatchDCMTK is opt-in evidence for #1000: DCMTK dcmseg
// objects of every type (BINARY, FRACTIONAL PROBABILITY and OCCUPANCY, 8- and
// 16-bit LABELMAP) read here bit for bit, the ones written here read in
// DCMTK bit for bit, and the overlap matrix and disjoint segment groups agree
// with DCMTK's OverlapUtil. Enable with DICOMGO_DCMTK_INTEGRATION=1 and
// DICOMGO_DCMTK_SEGORACLE (see segOracle).
func TestSegmentationsMatchDCMTK(t *testing.T) {
	oracle := segOracle(t)
	for _, kind := range oracleKinds {
		t.Run(kind.name, func(t *testing.T) {
			dir := t.TempDir()
			expected := kind.expectedFrames()

			// DCMTK writes, dicom-go reads.
			path := filepath.Join(dir, "dcmtk.dcm")
			runOracle(t, oracle, "write", kind.name, path)
			file, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := Read(file.Dataset)
			_ = file.Close()
			if err != nil {
				t.Fatal(err)
			}
			if doc.SegmentationType != kind.segmentType || doc.FractionalType != kind.fractionalType || doc.MaxFractionalValue != kind.maxValue {
				t.Fatalf("read type %s %s %d", doc.SegmentationType, doc.FractionalType, doc.MaxFractionalValue)
			}
			if kind.segmentType == SegmentationTypeLabelMap && doc.LabelMapBitsAllocated != map[int]int{255: 8, 300: 16}[kind.maxLabel] {
				t.Fatalf("label-map Bits Allocated %d", doc.LabelMapBitsAllocated)
			}
			if len(doc.Frames) != len(expected) {
				t.Fatalf("%d frames, want %d", len(doc.Frames), len(expected))
			}
			for i, frame := range doc.Frames {
				want := expected[i]
				if (kind.segmentType != SegmentationTypeLabelMap && frame.SegmentNumber != want.segment) || frame.SliceIndex != want.slice {
					t.Fatalf("frame %d is segment %d slice %d, want %d/%d", i, frame.SegmentNumber, frame.SliceIndex, want.segment, want.slice)
				}
				if got := frameValues(doc, frame); !reflect.DeepEqual(got, want.values) {
					t.Fatalf("frame %d pixels:\n got %v\nwant %v", i, got, want.values)
				}
			}
			ourReport, err := Overlaps(doc)
			if err != nil {
				t.Fatal(err)
			}
			if kind.segmentType != SegmentationTypeLabelMap {
				parseOracleDump(t, runOracle(t, oracle, "dump", path)).checkOverlaps(t, ourReport)
			}

			// dicom-go writes, DCMTK reads.
			written, err := Write(kind.document())
			if err != nil {
				t.Fatal(err)
			}
			path = filepath.Join(dir, "dicom-go.dcm")
			writeSEGFile(t, path, written)
			dump := parseOracleDump(t, runOracle(t, oracle, "dump", path))
			if !strings.HasPrefix(dump.header, "type "+kind.segmentType+" ") {
				t.Fatalf("DCMTK header %q", dump.header)
			}
			if kind.segmentType == SegmentationTypeFractional && !strings.HasSuffix(dump.header, "fractional "+kind.fractionalType) {
				t.Fatalf("DCMTK header %q", dump.header)
			}
			if len(dump.frames) != len(expected) {
				t.Fatalf("DCMTK read %d frames, want %d", len(dump.frames), len(expected))
			}
			for i, want := range expected {
				if !reflect.DeepEqual(dump.frames[i], want.values) {
					t.Fatalf("DCMTK frame %d:\n got %v\nwant %v", i, dump.frames[i], want.values)
				}
			}
			if kind.segmentType != SegmentationTypeLabelMap {
				dump.checkOverlaps(t, ourReport)
			} else if len(dump.segments) != kind.maxLabel+1 || dump.segments[0] != 0 || dump.segments[kind.maxLabel] != kind.maxLabel {
				t.Fatalf("DCMTK read %d segments, want 0 to %d", len(dump.segments), kind.maxLabel)
			}
			if got := derivedio.CleanString(written.Dataset, tagSegmentsOverlap); got != map[bool]string{true: SegmentsOverlapYes, false: SegmentsOverlapNo}[len(ourReport.Overlaps) > 0] {
				t.Fatalf("Segments Overlap %q for %d overlapping pairs", got, len(ourReport.Overlaps))
			}
		})
	}

	// A label map converted here from the disjoint binary segments reads in
	// DCMTK with the same labels.
	t.Run("converted label map", func(t *testing.T) {
		disjoint := overlapDocument()
		disjoint.Segments, disjoint.Frames = disjoint.Segments[:2], disjoint.Frames[:4]
		labelMap, _, err := ToLabelMap(disjoint, LabelMapOptions{SOPInstanceUID: "1.2.826.0.1.3680043.10.543.1000.30"})
		if err != nil {
			t.Fatal(err)
		}
		written, err := Write(labelMap)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(t.TempDir(), "converted.dcm")
		writeSEGFile(t, path, written)
		dump := parseOracleDump(t, runOracle(t, oracle, "dump", path))
		if len(dump.frames) != len(labelMap.Frames) {
			t.Fatalf("DCMTK read %d frames, want %d", len(dump.frames), len(labelMap.Frames))
		}
		for i, frame := range labelMap.Frames {
			if !reflect.DeepEqual(dump.frames[i], frameValues(labelMap, frame)) {
				t.Fatalf("frame %d differs in DCMTK", i)
			}
		}
		if !reflect.DeepEqual(dump.segments, []int{0, 1, 2}) {
			t.Fatalf("DCMTK segments %v", dump.segments)
		}
	})
}

// withSourceReferences makes every frame of doc derive from one synthetic
// source image per slice, as a segmentation of an image series does.
func withSourceReferences(doc *Document) *Document {
	seen := map[int]bool{}
	for i := range doc.Frames {
		slice := doc.Frames[i].SliceIndex
		uid := "1.2.826.0.1.3680043.10.543.1000.40." + strconv.Itoa(slice+1)
		doc.Frames[i].ReferencedSOPInstanceUID = uid
		if !seen[slice] {
			seen[slice] = true
			doc.ReferencedImages = append(doc.ReferencedImages, ReferencedImage{
				SeriesInstanceUID: "1.2.826.0.1.3680043.10.543.1000.41",
				SOPClassUID:       "1.2.840.10008.5.1.4.1.1.2",
				SOPInstanceUID:    uid,
			})
		}
	}
	return doc
}

// dcmtkDiagnostics are the warning and error lines DCMTK logged.
func dcmtkDiagnostics(stderr string) []string {
	var lines []string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); strings.HasPrefix(line, "W:") || strings.HasPrefix(line, "E:") {
			lines = append(lines, line)
		}
	}
	return lines
}

// TestWrittenSegmentationsLoadInDCMTKWithoutWarnings is opt-in evidence
// that DCMTK's DcmSegmentation::loadFile finds every Type 1 and 2 attribute
// of the Patient, General Study, Frame of Reference, Segmentation Series,
// Enhanced General Equipment, Content Identification and Segmentation Image
// modules in what Write produces, both with a source instance and caller
// identification and with the defaults alone. A control with Patient Name
// removed proves the check sees such warnings. Enable as for
// TestSegmentationsMatchDCMTK.
func TestWrittenSegmentationsLoadInDCMTKWithoutWarnings(t *testing.T) {
	oracle := segOracle(t)
	write := func(t *testing.T, doc *Document, edit func(*object.Object)) []string {
		t.Helper()
		file, err := Write(doc)
		if err != nil {
			t.Fatal(err)
		}
		if edit != nil {
			edit(file.Dataset)
		}
		path := filepath.Join(t.TempDir(), "seg.dcm")
		writeSEGFile(t, path, file)
		_, stderr := runOracleLogged(t, oracle, dcmtkoracle.Accepted+"; no warnings", "dump", path)
		return dcmtkDiagnostics(stderr)
	}
	for _, kind := range oracleKinds {
		t.Run(kind.name+"/defaults", func(t *testing.T) {
			if diagnostics := write(t, withSourceReferences(kind.document()), nil); len(diagnostics) > 0 {
				t.Fatalf("DCMTK diagnostics:\n%s", strings.Join(diagnostics, "\n"))
			}
		})
		t.Run(kind.name+"/source", func(t *testing.T) {
			doc := withSourceReferences(kind.document())
			doc.Common = object.DerivedCommon{Source: segCommonSource()}
			doc.Identification = Identification{SeriesNumber: 301, ContentLabel: "ORACLE", ContentDescription: "Synthetic", ContentCreatorName: "Reader^Synthetic"}
			if diagnostics := write(t, doc, nil); len(diagnostics) > 0 {
				t.Fatalf("DCMTK diagnostics:\n%s", strings.Join(diagnostics, "\n"))
			}
		})
	}
	t.Run("control without Patient Name", func(t *testing.T) {
		diagnostics := write(t, withSourceReferences(oracleKinds[0].document()), func(obj *object.Object) {
			obj.Remove(core.NewTag(0x0010, 0x0010))
		})
		if !strings.Contains(strings.Join(diagnostics, "\n"), "PatientName") {
			t.Fatalf("DCMTK did not report the missing Patient Name: %q", diagnostics)
		}
	})
}

func writeSEGFile(t *testing.T, path string, file *object.File) {
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
