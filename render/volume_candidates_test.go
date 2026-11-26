package render

import (
	"errors"
	"math"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/object"
)

var (
	candidateAxial    = []float64{1, 0, 0, 0, 1, 0}
	candidateCoronal  = []float64{1, 0, 0, 0, 0, -1}
	candidateSagittal = []float64{0, 1, 0, 0, 0, -1}
)

// candidateFrame is a synthetic slice of the given grid, orientation and
// position along the orientation's normal, with per-frame pixel spacing.
func candidateFrame(rows, cols int, orientation []float64, position, spacing float64) *Frame {
	frame := volumeTestFrame(rows, cols, make([]byte, rows*cols), 0)
	frame.ImageOrientation = append([]float64(nil), orientation...)
	row := Vec3{X: orientation[0], Y: orientation[1], Z: orientation[2]}
	col := Vec3{X: orientation[3], Y: orientation[4], Z: orientation[5]}
	origin := row.Cross(col).Scale(position)
	frame.ImagePosition = []float64{origin.X, origin.Y, origin.Z}
	frame.PixelSpacing = []float64{spacing, spacing}
	frame.SliceThickness = 1
	return frame
}

func candidateStack(frames ...*Frame) *Stack {
	return &Stack{PixelSpacing: []float64{1, 1}, SliceThickness: 1, Frames: frames}
}

func appendStack(frames []*Frame, count, rows, cols int, orientation []float64, start, step, spacing float64) []*Frame {
	for i := 0; i < count; i++ {
		frames = append(frames, candidateFrame(rows, cols, orientation, start+float64(i)*step, spacing))
	}
	return frames
}

func candidateIndices(from, to int) []int {
	out := make([]int, 0, to-from)
	for i := from; i < to; i++ {
		out = append(out, i)
	}
	return out
}

func requireUnsupported(t *testing.T, stack *Stack, reason GeometryIssue) {
	t.Helper()
	assessment := stack.GeometryAssessment()
	if assessment.Disposition != GeometryUnsupported || (reason != GeometryIssueNone && assessment.FallbackReason != reason) {
		t.Fatalf("whole-series assessment = %s/%q, want unsupported/%q",
			assessment.Disposition, assessment.FallbackReason, reason)
	}
}

func TestVolumeCandidatesSeparateEmbeddedLocalizer(t *testing.T) {
	frames := []*Frame{candidateFrame(4, 4, candidateCoronal, 0, 1)}
	frames = appendStack(frames, 10, 4, 4, candidateAxial, 1, 2, 1)
	stack := candidateStack(frames...)
	// Measured along the localizer's normal every axial slice collides, so
	// the whole series fails closed (duplicate-positions).
	requireUnsupported(t, stack, GeometryIssueNone)

	candidates := stack.VolumeCandidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	volume := candidates[0]
	if !reflect.DeepEqual(volume.FrameIndices, candidateIndices(1, 11)) {
		t.Fatalf("largest candidate frames = %v", volume.FrameIndices)
	}
	if !volume.Usable() || volume.Assessment.Disposition != GeometryRegularFastPath || volume.Plane != object.ImagePlaneAxial {
		t.Fatalf("largest candidate = %s/%s", volume.Assessment.Disposition, volume.Plane)
	}
	if volume.Description != "axial 4×4, 10 slices" {
		t.Fatalf("description = %q", volume.Description)
	}
	localizer := candidates[1]
	if !reflect.DeepEqual(localizer.FrameIndices, []int{0}) || localizer.Usable() ||
		localizer.Assessment.FallbackReason != GeometryIssueSingleSlice || localizer.Description != "coronal 4×4, 1 slice" {
		t.Fatalf("localizer candidate = %v %s %q %q", localizer.FrameIndices, localizer.Assessment.Disposition,
			localizer.Assessment.FallbackReason, localizer.Description)
	}

	sub := stack.VolumeCandidateStack(volume)
	defer sub.Close()
	built, err := BuildVolume(sub)
	if err != nil {
		t.Fatalf("BuildVolume(candidate) = %v", err)
	}
	if built.Depth != 10 || math.Abs(built.SliceSpacing-2) > 1e-9 {
		t.Fatalf("candidate volume depth %d spacing %v", built.Depth, built.SliceSpacing)
	}
	if _, err := BuildVolume(stack); !errors.Is(err, ErrNonCorrectableGeometry) {
		t.Fatalf("whole series must still fail closed, got %v", err)
	}
}

func TestVolumeCandidatesThreePlaneLocalizerAndAxialStack(t *testing.T) {
	var frames []*Frame
	frames = appendStack(frames, 3, 4, 4, candidateAxial, -10, 10, 2)
	frames = appendStack(frames, 3, 4, 4, candidateCoronal, -10, 10, 2)
	frames = appendStack(frames, 3, 4, 4, candidateSagittal, -10, 10, 2)
	frames = appendStack(frames, 12, 6, 6, candidateAxial, 0, 1.5, 0.5)
	stack := candidateStack(frames...)
	if stack.GeometryAssessment().Disposition != GeometryUnsupported {
		t.Fatal("mixed localizer series assessed as one volume")
	}

	candidates := stack.VolumeCandidates()
	want := []struct {
		indices     []int
		description string
	}{
		{candidateIndices(9, 21), "axial 6×6, 12 slices, 0.5×0.5 mm"},
		{candidateIndices(0, 3), "axial 4×4, 3 slices, 2×2 mm"},
		{candidateIndices(3, 6), "coronal 4×4, 3 slices, 2×2 mm"},
		{candidateIndices(6, 9), "sagittal 4×4, 3 slices, 2×2 mm"},
	}
	if len(candidates) != len(want) {
		t.Fatalf("candidates = %d, want %d", len(candidates), len(want))
	}
	for i, expected := range want {
		got := candidates[i]
		if !reflect.DeepEqual(got.FrameIndices, expected.indices) || got.Description != expected.description || !got.Usable() {
			t.Fatalf("candidate %d = %v %q usable=%v, want %v %q", i, got.FrameIndices, got.Description, got.Usable(), expected.indices, expected.description)
		}
	}
}

func TestVolumeCandidatesSeparatePixelGrids(t *testing.T) {
	var frames []*Frame
	for i := 0; i < 10; i++ {
		if i%3 == 2 {
			frames = append(frames, candidateFrame(8, 8, candidateAxial, float64(i), 1))
		} else {
			frames = append(frames, candidateFrame(4, 4, candidateAxial, float64(i), 1))
		}
	}
	stack := candidateStack(frames...)
	requireUnsupported(t, stack, GeometryIssueDifferentPixelGrid)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	if !reflect.DeepEqual(candidates[0].FrameIndices, []int{0, 1, 3, 4, 6, 7, 9}) ||
		!reflect.DeepEqual(candidates[1].FrameIndices, []int{2, 5, 8}) {
		t.Fatalf("grid partition = %v / %v", candidates[0].FrameIndices, candidates[1].FrameIndices)
	}
	if candidates[0].Columns != 4 || candidates[1].Columns != 8 {
		t.Fatalf("grid keys = %d / %d", candidates[0].Columns, candidates[1].Columns)
	}
	// The 4x4 part has a 3 mm gap every third slice, so it is regularizable;
	// the 8x8 part is regular at 3 mm.
	if !candidates[0].Usable() || !candidates[1].Usable() {
		t.Fatalf("dispositions = %s / %s", candidates[0].Assessment.Disposition, candidates[1].Assessment.Disposition)
	}
}

func TestVolumeCandidatesSeparatePixelSpacings(t *testing.T) {
	var frames []*Frame
	frames = appendStack(frames, 5, 4, 4, candidateAxial, 0, 1, 0.5)
	frames = appendStack(frames, 3, 4, 4, candidateAxial, 20, 1, 1)
	// Within the 5% spacing tolerance: stays with the first part.
	frames = append(frames, candidateFrame(4, 4, candidateAxial, 5, 0.51))
	stack := candidateStack(frames...)
	requireUnsupported(t, stack, GeometryIssueInconsistentPixelSpacing)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	if !reflect.DeepEqual(candidates[0].FrameIndices, []int{0, 1, 2, 3, 4, 8}) || candidates[0].RowSpacing != 0.5 {
		t.Fatalf("first spacing part = %v at %v", candidates[0].FrameIndices, candidates[0].RowSpacing)
	}
	if candidates[0].Description != "axial 4×4, 6 slices, 0.5×0.5 mm" || candidates[1].Description != "axial 4×4, 3 slices, 1×1 mm" {
		t.Fatalf("descriptions = %q / %q", candidates[0].Description, candidates[1].Description)
	}
}

func TestVolumeCandidatesSeparateEchoes(t *testing.T) {
	var frames []*Frame
	for position := 0; position < 5; position++ {
		for echo := 1; echo <= 2; echo++ {
			frame := candidateFrame(4, 4, candidateAxial, float64(position)*3, 1)
			frame.EchoNumber, frame.HasEchoNumber = echo, true
			frames = append(frames, frame)
		}
	}
	stack := candidateStack(frames...)
	requireUnsupported(t, stack, GeometryIssueDuplicatePositions)

	candidates := stack.VolumeCandidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	for i, candidate := range candidates {
		if candidate.EchoNumber != i+1 || candidate.SliceCount() != 5 || candidate.Assessment.Disposition != GeometryRegularFastPath {
			t.Fatalf("echo candidate %d = echo %d, %d slices, %s", i, candidate.EchoNumber, candidate.SliceCount(), candidate.Assessment.Disposition)
		}
		want := "axial 4×4, 5 slices, echo " + strconv.Itoa(i+1)
		if candidate.Description != want {
			t.Fatalf("description = %q, want %q", candidate.Description, want)
		}
	}

	options := DefaultVolumeCandidateOptions()
	options.SplitByEcho = false
	merged := stack.VolumeCandidatesWithOptions(options)
	if len(merged) != 1 || merged[0].Assessment.FallbackReason != GeometryIssueDuplicatePositions {
		t.Fatalf("without the echo key = %d candidates", len(merged))
	}
}

func TestVolumeCandidatesSeparateTemporalPositions(t *testing.T) {
	stack := geometryTestStack(
		[]Vec3{{}, {}, {Z: 1}, {Z: 1}, {Z: 2}, {Z: 2}},
		repeatOrientation(candidateAxial, 6),
		[]int{1, 2, 1, 2, 1, 2},
	)
	requireUnsupported(t, stack, GeometryIssueTemporalInterleaving)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 2 {
		t.Fatalf("candidates = %d, want 2", len(candidates))
	}
	for i, candidate := range candidates {
		if !candidate.HasTemporalPosition || candidate.TemporalPosition != i+1 ||
			!reflect.DeepEqual(candidate.FrameIndices, []int{i, i + 2, i + 4}) || !candidate.Usable() {
			t.Fatalf("temporal candidate %d = %+v", i, candidate.FrameIndices)
		}
		if !strings.HasSuffix(candidate.Description, "time point "+strconv.Itoa(i+1)) {
			t.Fatalf("description = %q", candidate.Description)
		}
	}
}

func TestVolumeCandidatesAcquisitionAndThicknessAreOptIn(t *testing.T) {
	var frames []*Frame
	frames = appendStack(frames, 6, 4, 4, candidateAxial, 0, 1, 1)
	for i, frame := range frames {
		frame.AcquisitionNumber, frame.HasAcquisitionNumber = 1+i/3, true
		if i >= 3 {
			frame.SliceThickness = 3
		}
	}
	stack := candidateStack(frames...)
	if got := stack.VolumeCandidates(); len(got) != 1 || got[0].SliceCount() != 6 {
		t.Fatalf("default candidates = %d", len(got))
	}
	for name, options := range map[string]VolumeCandidateOptions{
		"acquisition": {SplitByAcquisitionNumber: true},
		"thickness":   {SplitBySliceThickness: true},
	} {
		got := stack.VolumeCandidatesWithOptions(options)
		if len(got) != 2 || !reflect.DeepEqual(got[0].FrameIndices, []int{0, 1, 2}) ||
			!reflect.DeepEqual(got[1].FrameIndices, []int{3, 4, 5}) {
			t.Fatalf("%s split = %d candidates", name, len(got))
		}
	}
}

func TestVolumeCandidatesExcludeUnplaceableFrames(t *testing.T) {
	var frames []*Frame
	frames = appendStack(frames, 4, 4, 4, candidateAxial, 0, 1, 1)
	skewed := candidateFrame(4, 4, candidateAxial, 9, 1)
	skewed.ImageOrientation = []float64{1, 0, 0, 0.1, 1, 0}
	unplaced := candidateFrame(4, 4, candidateAxial, 10, 1)
	unplaced.ImagePosition = nil
	missingOrientation := candidateFrame(4, 4, candidateAxial, 11, 1)
	missingOrientation.ImageOrientation = nil
	frames = append(frames, skewed, unplaced, missingOrientation)
	stack := candidateStack(frames...)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 || !reflect.DeepEqual(candidates[0].FrameIndices, candidateIndices(0, 4)) {
		t.Fatalf("candidates = %d", len(candidates))
	}
}

// The tests below port the contracts of ITK's
// Modules/IO/GDCM/test/itkGDCMSeriesFileNamesContractGTest.cxx (ITK commit
// 35b196e1, Apache-2.0; see also ITK commits 50dfb13665, e5265f7282,
// 72a6747586 and debb56666f) from file enumeration to frame partitioning.
// Modified: the fixtures are synthetic in-memory frames instead of DICOM
// files, and a "series identifier" is a volume candidate. The recursive
// directory scan and the legacy Instance Number / file-name ordering
// fallbacks have no frame-level analogue and are not ported.

// SingleSeriesEnumerationAndGrouping: one coherent series is one candidate
// with every image, ordered as the whole-series assessment orders it.
func TestVolumeCandidatesITKContractSingleSeries(t *testing.T) {
	stack := candidateStack(appendStack(nil, 8, 4, 4, candidateAxial, 0, 1.25, 0.7)...)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 || !reflect.DeepEqual(candidates[0].FrameIndices, candidateIndices(0, 8)) {
		t.Fatalf("single series = %d candidates", len(candidates))
	}
	whole := stack.GeometryAssessment()
	got := candidates[0].Assessment
	if got.Disposition != whole.Disposition || !reflect.DeepEqual(got.Geometry.Positions, whole.Geometry.Positions) {
		t.Fatalf("candidate assessment %s differs from whole series %s", got.Disposition, whole.Disposition)
	}
}

// OrderingReconstructsValidVolume: shuffled input still yields one z-plane per
// image with positive, finite spacing.
func TestVolumeCandidatesITKContractOrderingReconstructsVolume(t *testing.T) {
	frames := appendStack(nil, 6, 4, 4, candidateAxial, 0, 2, 0.8)
	frames[0], frames[4], frames[2], frames[5] = frames[4], frames[0], frames[5], frames[2]
	stack := candidateStack(frames...)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 {
		t.Fatalf("candidates = %d", len(candidates))
	}
	volume, err := BuildVolume(stack.VolumeCandidateStack(candidates[0]))
	if err != nil {
		t.Fatal(err)
	}
	if volume.Depth != len(frames) {
		t.Fatalf("depth = %d, want %d", volume.Depth, len(frames))
	}
	for _, spacing := range []float64{volume.ColSpacing, volume.RowSpacing, volume.SliceSpacing} {
		if !(spacing > 0) || math.IsInf(spacing, 0) {
			t.Fatalf("spacing %v is not positive and finite", spacing)
		}
	}
}

// AmbiguousOrderingThrowsByDefault: duplicate positions are not split into
// invented sub-volumes; the candidate fails closed.
func TestVolumeCandidatesITKContractDuplicatePositionsFailClosed(t *testing.T) {
	stack := candidateStack(candidateFrame(4, 4, candidateAxial, 0, 1), candidateFrame(4, 4, candidateAxial, 0, 1))
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 || candidates[0].Usable() ||
		candidates[0].Assessment.FallbackReason != GeometryIssueDuplicatePositions {
		t.Fatalf("duplicate positions = %d candidates", len(candidates))
	}
}

// NonImageObjectsExcluded and MixedContentDirectoryEnumeratesOnlyImageSeries:
// non-image objects (no Rows), undecodable and missing entries never surface
// as a candidate and do not disturb the ordering of the image series.
func TestVolumeCandidatesITKContractOnlyImagesEnumerated(t *testing.T) {
	images := appendStack(nil, 5, 4, 4, candidateAxial, 0, 1, 1)
	report := candidateFrame(4, 4, candidateAxial, 0, 1)
	report.Metadata.Rows, report.Metadata.Columns = 0, 0
	broken := candidateFrame(4, 4, candidateAxial, 7, 1)
	broken.DecodeErr = errors.New("synthetic decode failure")
	frames := []*Frame{images[0], report, images[1], nil, images[2], broken, images[3], images[4]}
	stack := candidateStack(frames...)
	candidates := stack.VolumeCandidates()
	if len(candidates) != 1 || !reflect.DeepEqual(candidates[0].FrameIndices, []int{0, 2, 4, 6, 7}) {
		t.Fatalf("mixed content = %d candidates", len(candidates))
	}
	volume, err := BuildVolume(stack.VolumeCandidateStack(candidates[0]))
	if err != nil || volume.Depth != len(images) {
		t.Fatalf("mixed content volume = %v", err)
	}
}

// DefaultGroupingUsesRawSeriesInstanceUID: series details are opt-in, so an
// Acquisition Number that changes on every slice does not split by default.
func TestVolumeCandidatesITKContractDetailsAreOptIn(t *testing.T) {
	frames := appendStack(nil, 4, 4, 4, candidateAxial, 0, 1, 1)
	for i, frame := range frames {
		frame.AcquisitionNumber, frame.HasAcquisitionNumber = i+1, true
	}
	if got := candidateStack(frames...).VolumeCandidates(); len(got) != 1 {
		t.Fatalf("per-slice acquisition numbers split the default grouping into %d", len(got))
	}
}

// SeriesRestrictionSurvivesSetUseSeriesDetails: a restriction takes effect
// whether or not the default details are enabled; restricting on the unique
// Instance Number yields one candidate per image.
func TestVolumeCandidatesITKContractRestriction(t *testing.T) {
	frames := appendStack(nil, 4, 4, 4, candidateAxial, 0, 1, 1)
	for i, frame := range frames {
		frame.InstanceNumber = i + 1
	}
	stack := candidateStack(frames...)
	instance := func(frame *Frame) string { return strconv.Itoa(frame.InstanceNumber) }
	withDetails := DefaultVolumeCandidateOptions()
	withDetails.Restrictions = []VolumeCandidateRestriction{instance}
	withoutDetails := VolumeCandidateOptions{Restrictions: []VolumeCandidateRestriction{instance}}
	first := stack.VolumeCandidatesWithOptions(withDetails)
	second := stack.VolumeCandidatesWithOptions(withoutDetails)
	if len(first) != len(frames) {
		t.Fatalf("restriction produced %d candidates, want %d", len(first), len(frames))
	}
	if len(second) != len(first) {
		t.Fatalf("restriction depends on detail options: %d vs %d", len(second), len(first))
	}
	for i := range first {
		if !reflect.DeepEqual(first[i].FrameIndices, second[i].FrameIndices) || first[i].FrameIndices[0] != i {
			t.Fatalf("candidate %d = %v / %v", i, first[i].FrameIndices, second[i].FrameIndices)
		}
	}
}

func TestVolumeCandidatesNilAndEmpty(t *testing.T) {
	var stack *Stack
	if stack.VolumeCandidates() != nil || stack.VolumeCandidateStack(VolumeCandidate{}) != nil {
		t.Fatal("nil stack produced candidates")
	}
	if got := (&Stack{}).VolumeCandidates(); len(got) != 0 {
		t.Fatalf("empty stack = %d candidates", len(got))
	}
}
