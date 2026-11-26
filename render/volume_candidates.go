package render

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/object"
)

// VolumeCandidateRestriction returns a detail key for a frame. Frames whose
// keys differ never share a volume candidate. It is the analogue of ITK's
// GDCMSeriesFileNames::AddSeriesRestriction, which refines a series by the
// value of an extra attribute.
type VolumeCandidateRestriction func(*Frame) string

// VolumeCandidateOptions selects the detail keys that partition a stack into
// volume candidates. Orientation basis, pixel grid (Rows, Columns), pixel
// spacing and the patient-position source always separate candidates,
// because one volume cannot represent a mixture of them.
type VolumeCandidateOptions struct {
	// Tolerances bounds the orientation and pixel-spacing comparisons that
	// partition the frames. The zero value selects DefaultGeometryTolerances.
	// Each candidate's Assessment always applies the default guardrail
	// tolerances, exactly as Stack.GeometryAssessment and BuildVolume do.
	Tolerances GeometryTolerances
	// SplitByEcho separates frames by Echo Numbers (0018,0086).
	SplitByEcho bool
	// SplitByTemporalPosition separates the time points and Stack IDs of a
	// dynamic acquisition, as GeometryFrameGroups does. Repeated positions
	// without temporal identity are never split: they stay together and the
	// candidate reports duplicate-positions.
	SplitByTemporalPosition bool
	// SplitByAcquisitionNumber separates frames by Acquisition Number
	// (0020,0012). It is off by default because some scanners number every
	// slice of one volume as its own acquisition.
	SplitByAcquisitionNumber bool
	// SplitBySliceThickness separates frames whose Slice Thickness differs
	// beyond the spacing tolerance (one of ITK's default series details).
	SplitBySliceThickness bool
	// Restrictions are extra caller-defined detail keys.
	Restrictions []VolumeCandidateRestriction
}

// DefaultVolumeCandidateOptions splits by the geometric keys, echo number and
// temporal position.
func DefaultVolumeCandidateOptions() VolumeCandidateOptions {
	return VolumeCandidateOptions{
		Tolerances:              DefaultGeometryTolerances(),
		SplitByEcho:             true,
		SplitByTemporalPosition: true,
	}
}

// VolumeCandidate is one coherent part of a stack: frames that share an
// orientation basis, pixel grid, pixel spacing, position source and the
// selected detail keys. Assessment is the GeometryAssessment of those frames
// alone, so a candidate can still be unsupported (for example a single-slice
// localizer, or repeated positions).
type VolumeCandidate struct {
	// FrameIndices are zero-based indices into Stack.Frames, ascending.
	FrameIndices []int
	// Frames are the source frame pointers in the same order. They are
	// shared with the stack and must not be mutated.
	Frames []*Frame

	Plane          object.ImagePlane
	RowDir         Vec3
	ColDir         Vec3
	Normal         Vec3
	Rows           int
	Columns        int
	RowSpacing     float64
	ColumnSpacing  float64
	SliceThickness float64

	EchoNumber           int
	HasEchoNumber        bool
	AcquisitionNumber    int
	HasAcquisitionNumber bool
	TemporalPosition     int
	HasTemporalPosition  bool
	StackID              string

	// Description is a short English summary such as
	// "axial 512×512, 120 slices". Details that vary between the candidates
	// of one stack (spacing, echo, time point, ...) are appended.
	Description string
	Assessment  GeometryAssessment
}

// SliceCount is the number of frames in the candidate.
func (c VolumeCandidate) SliceCount() int { return len(c.FrameIndices) }

// Usable reports whether the candidate can open as one MPR/VR volume.
func (c VolumeCandidate) Usable() bool {
	return c.Assessment.Disposition != GeometryUnsupported && len(c.FrameIndices) >= 2
}

// VolumeCandidates partitions the renderable frames of the stack with
// DefaultVolumeCandidateOptions. See VolumeCandidatesWithOptions.
func (s *Stack) VolumeCandidates() []VolumeCandidate {
	return s.VolumeCandidatesWithOptions(DefaultVolumeCandidateOptions())
}

// VolumeCandidatesWithOptions partitions the renderable frames of a stack
// into coherent sub-volumes, so a series with an embedded localizer, several
// orientations, several pixel grids or several echoes can open its largest
// coherent part in MPR while GeometryAssessment still fails closed for the
// series as a whole.
//
// Frames that are not images (no Rows/Columns), failed to decode, or cannot
// be placed in patient space (missing or non-orthonormal orientation, no
// position) belong to no candidate. Frames join the first candidate, in
// frame order, whose first frame matches them within the orientation and
// spacing tolerances, so every candidate's reference is also the reference
// of its own GeometryAssessment. Candidates are ordered usable first, then
// by slice count (largest first), then by first frame index. A stack that is
// already one coherent volume yields exactly one candidate holding all its
// renderable frames.
func (s *Stack) VolumeCandidatesWithOptions(opts VolumeCandidateOptions) []VolumeCandidate {
	if s == nil {
		return nil
	}
	tol := normalizedGeometryTolerances(opts.Tolerances)
	fallbackRow, fallbackColumn := seriesPixelSpacing(s)
	if !finitePositiveSpacing(fallbackRow) || !finitePositiveSpacing(fallbackColumn) {
		fallbackRow, fallbackColumn = 1, 1
	}

	type partition struct {
		reference  SliceGeometry
		source     slicePositionSource
		thickness  float64
		echo       int
		hasEcho    bool
		acq        int
		hasAcq     bool
		restricted []string
		indices    []int
	}
	var partitions []*partition
	for index, frame := range s.Frames {
		if frame == nil || frame.DecodeErr != nil || frame.Metadata.Rows <= 0 || frame.Metadata.Columns <= 0 {
			continue
		}
		deviation, ok := object.ImageOrientationDeviation(frame.ImageOrientation)
		if !ok || deviation > object.ImageOrientationTolerance {
			continue
		}
		geometry, ok := sliceGeometry(frame, Vec3{}, fallbackRow, fallbackColumn)
		if !ok {
			continue
		}
		source := slicePositionUnknown
		source.observe(frame)
		restricted := make([]string, len(opts.Restrictions))
		for i, restriction := range opts.Restrictions {
			if restriction != nil {
				restricted[i] = restriction(frame)
			}
		}
		var match *partition
		for _, candidate := range partitions {
			ref := candidate.reference
			if ref.Rows != geometry.Rows || ref.Columns != geometry.Columns || candidate.source != source ||
				geometry.RowDir.Dot(ref.RowDir) < tol.OrientationCos ||
				geometry.ColDir.Dot(ref.ColDir) < tol.OrientationCos ||
				!spacingWithinTolerance(geometry.RowSpacing, ref.RowSpacing, tol) ||
				!spacingWithinTolerance(geometry.ColSpacing, ref.ColSpacing, tol) {
				continue
			}
			if opts.SplitByEcho && (candidate.hasEcho != frame.HasEchoNumber || candidate.echo != frame.EchoNumber) {
				continue
			}
			if opts.SplitByAcquisitionNumber &&
				(candidate.hasAcq != frame.HasAcquisitionNumber || candidate.acq != frame.AcquisitionNumber) {
				continue
			}
			if opts.SplitBySliceThickness && !sameSliceThickness(candidate.thickness, frame.SliceThickness, tol) {
				continue
			}
			if !equalStrings(candidate.restricted, restricted) {
				continue
			}
			match = candidate
			break
		}
		if match == nil {
			match = &partition{
				reference: geometry, source: source, thickness: frame.SliceThickness,
				echo: frame.EchoNumber, hasEcho: frame.HasEchoNumber,
				acq: frame.AcquisitionNumber, hasAcq: frame.HasAcquisitionNumber,
				restricted: restricted,
			}
			partitions = append(partitions, match)
		}
		match.indices = append(match.indices, index)
	}

	candidates := make([]VolumeCandidate, 0, len(partitions))
	for _, part := range partitions {
		ref := part.reference
		groups := [][]int{part.indices}
		temporal := []volumeCandidateTemporal{{}}
		if opts.SplitByTemporalPosition && len(part.indices) > 1 {
			groups, temporal = s.splitVolumeCandidateTemporal(part.indices, ref, fallbackRow, fallbackColumn)
		}
		for g, indices := range groups {
			candidate := VolumeCandidate{
				FrameIndices:  indices,
				Frames:        make([]*Frame, len(indices)),
				RowDir:        ref.RowDir,
				ColDir:        ref.ColDir,
				Normal:        ref.Normal,
				Rows:          ref.Rows,
				Columns:       ref.Columns,
				RowSpacing:    ref.RowSpacing,
				ColumnSpacing: ref.ColSpacing,
			}
			for i, index := range indices {
				candidate.Frames[i] = s.Frames[index]
			}
			first := candidate.Frames[0]
			candidate.Plane = object.ClassifyImagePlane(first.ImageOrientation)
			candidate.SliceThickness = first.SliceThickness
			candidate.EchoNumber, candidate.HasEchoNumber = first.EchoNumber, first.HasEchoNumber
			candidate.AcquisitionNumber, candidate.HasAcquisitionNumber = first.AcquisitionNumber, first.HasAcquisitionNumber
			candidate.TemporalPosition = temporal[g].position
			candidate.HasTemporalPosition = temporal[g].hasPosition
			candidate.StackID = temporal[g].stackID
			candidate.Assessment = s.VolumeCandidateStack(candidate).GeometryAssessment()
			candidates = append(candidates, candidate)
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool {
		left, right := candidates[i], candidates[j]
		if left.Usable() != right.Usable() {
			return left.Usable()
		}
		if len(left.FrameIndices) != len(right.FrameIndices) {
			return len(left.FrameIndices) > len(right.FrameIndices)
		}
		return left.FrameIndices[0] < right.FrameIndices[0]
	})
	describeVolumeCandidates(candidates)
	return candidates
}

// VolumeCandidateStack returns a new stack holding only the candidate's
// frames, for BuildVolume or GeometryAssessment. Frame pointers are shared
// with s; Close on the returned stack releases only its own volume state.
func (s *Stack) VolumeCandidateStack(candidate VolumeCandidate) *Stack {
	if s == nil {
		return nil
	}
	sub := &Stack{
		UID:                  s.UID,
		StudyUID:             s.StudyUID,
		Modality:             s.Modality,
		BodyPart:             s.BodyPart,
		DefaultWindow:        s.DefaultWindow,
		PixelSpacing:         append([]float64(nil), s.PixelSpacing...),
		SliceThickness:       s.SliceThickness,
		SpacingBetweenSlices: s.SpacingBetweenSlices,
		Frames:               append([]*Frame(nil), candidate.Frames...),
	}
	if len(candidate.Frames) > 0 && candidate.Frames[0] != nil {
		first := candidate.Frames[0]
		if _, _, ok := framePixelSpacing(first.PixelSpacing); ok {
			sub.PixelSpacing = append([]float64(nil), first.PixelSpacing...)
		}
		if finitePositiveSpacing(first.SliceThickness) {
			sub.SliceThickness = first.SliceThickness
		}
		if first.SpacingBetweenSlices != 0 && !math.IsNaN(first.SpacingBetweenSlices) && !math.IsInf(first.SpacingBetweenSlices, 0) {
			sub.SpacingBetweenSlices = first.SpacingBetweenSlices
		}
	}
	return sub
}

type volumeCandidateTemporal struct {
	position    int
	hasPosition bool
	stackID     string
}

// splitVolumeCandidateTemporal separates the time points and Stack IDs of one
// geometric partition. It splits only an explicit dynamic acquisition: a
// timeline that relied on repeated-position occurrence stays whole, so
// duplicate positions still fail closed in the candidate's assessment.
func (s *Stack) splitVolumeCandidateTemporal(indices []int, ref SliceGeometry, rowSpacing, columnSpacing float64) ([][]int, []volumeCandidateTemporal) {
	whole := [][]int{indices}
	none := []volumeCandidateTemporal{{}}
	frames := make([]*Frame, len(indices))
	for i, index := range indices {
		frames[i] = s.Frames[index]
	}
	collected, failure, _ := collectSliceGeometries(frames, ref.Normal, rowSpacing, columnSpacing, ref.Rows, ref.Columns)
	if failure != sliceGeometryValid || len(collected) != len(frames) {
		return whole, none
	}
	timeline := geometryTimeline(collected)
	if timeline.UsedOccurrenceFallback || (!timeline.Dynamic && !timeline.MultipleStacks) {
		return whole, none
	}
	var groups [][]int
	var temporal []volumeCandidateTemporal
	for _, point := range timeline.Points {
		for _, stack := range point.Stacks {
			group := make([]int, 0, len(stack.FrameIndices))
			for _, local := range stack.FrameIndices {
				if local >= 0 && local < len(collected) {
					group = append(group, indices[collected[local].frameIndex-1])
				}
			}
			if len(group) == 0 {
				continue
			}
			sort.Ints(group)
			groups = append(groups, group)
			temporal = append(temporal, volumeCandidateTemporal{
				position:    point.TemporalPosition,
				hasPosition: point.HasPosition && timeline.Dynamic,
				stackID:     stack.ID,
			})
		}
	}
	if len(groups) == 0 {
		return whole, none
	}
	return groups, temporal
}

// describeVolumeCandidates writes each Description. The base names the plane,
// the grid and the slice count; a detail is appended only when it differs
// between candidates, so identical-looking parts stay distinguishable.
func describeVolumeCandidates(candidates []VolumeCandidate) {
	varies := func(key func(VolumeCandidate) string) bool {
		for _, candidate := range candidates[1:] {
			if key(candidate) != key(candidates[0]) {
				return true
			}
		}
		return false
	}
	if len(candidates) == 0 {
		return
	}
	spacing := func(c VolumeCandidate) string {
		return strconv.FormatFloat(c.ColumnSpacing, 'g', 4, 64) + "×" + strconv.FormatFloat(c.RowSpacing, 'g', 4, 64) + " mm"
	}
	thickness := func(c VolumeCandidate) string {
		if !finitePositiveSpacing(c.SliceThickness) {
			return ""
		}
		return strconv.FormatFloat(c.SliceThickness, 'g', 4, 64) + " mm thick"
	}
	echo := func(c VolumeCandidate) string {
		if !c.HasEchoNumber {
			return ""
		}
		return "echo " + strconv.Itoa(c.EchoNumber)
	}
	acquisition := func(c VolumeCandidate) string {
		if !c.HasAcquisitionNumber {
			return ""
		}
		return "acquisition " + strconv.Itoa(c.AcquisitionNumber)
	}
	timePoint := func(c VolumeCandidate) string {
		if !c.HasTemporalPosition {
			return ""
		}
		return "time point " + strconv.Itoa(c.TemporalPosition)
	}
	stackID := func(c VolumeCandidate) string {
		if c.StackID == "" {
			return ""
		}
		return "stack " + c.StackID
	}
	details := []func(VolumeCandidate) string{spacing, thickness, echo, acquisition, timePoint, stackID}
	varying := make([]bool, len(details))
	for i, detail := range details {
		varying[i] = varies(detail)
	}
	for i := range candidates {
		c := &candidates[i]
		plane := strings.ToLower(string(c.Plane))
		if plane == "" {
			plane = "unknown plane"
		}
		noun := "slices"
		if len(c.FrameIndices) == 1 {
			noun = "slice"
		}
		parts := []string{fmt.Sprintf("%s %d×%d", plane, c.Columns, c.Rows), fmt.Sprintf("%d %s", len(c.FrameIndices), noun)}
		for d, detail := range details {
			if varying[d] {
				if value := detail(*c); value != "" {
					parts = append(parts, value)
				}
			}
		}
		c.Description = strings.Join(parts, ", ")
	}
}

func sameSliceThickness(reference, value float64, tol GeometryTolerances) bool {
	referenceOK, valueOK := finitePositiveSpacing(reference), finitePositiveSpacing(value)
	if !referenceOK || !valueOK {
		return referenceOK == valueOK
	}
	return spacingWithinTolerance(value, reference, tol)
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
