package multiframe

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	// ErrConcatenationIncomplete reports a Concatenation with missing
	// instances.
	ErrConcatenationIncomplete = errors.New("dicom/multiframe: concatenation incomplete")
	// ErrConcatenationInconsistent reports instances whose Concatenation
	// attributes, frame offsets or shared image attributes disagree.
	ErrConcatenationInconsistent = errors.New("dicom/multiframe: concatenation inconsistent")
)

// ConcatenationGroup is the instances found with one Concatenation UID.
type ConcatenationGroup struct {
	UID   string
	Files []*object.File
}

// GroupConcatenations sorts files into their Concatenations by Concatenation
// UID (0020,9161), in order of first appearance; files that belong to no
// Concatenation are returned apart. Nothing is validated: NewConcatenation
// does that for each group.
func GroupConcatenations(files []*object.File) (groups []ConcatenationGroup, others []*object.File) {
	index := map[string]int{}
	for _, file := range files {
		uid := ""
		if file != nil && file.Dataset != nil {
			uid = cleanUID(file.Dataset, tagConcatenationUID)
		}
		if uid == "" {
			others = append(others, file)
			continue
		}
		position, ok := index[uid]
		if !ok {
			position = len(groups)
			index[uid] = position
			groups = append(groups, ConcatenationGroup{UID: uid})
		}
		groups[position].Files = append(groups[position].Files, file)
	}
	return groups, others
}

// ConcatenationInstance is one instance of a Concatenation.
type ConcatenationInstance struct {
	File *object.File
	// Number is In-concatenation Number (0020,9162), from 1.
	Number int
	// FrameOffset is Concatenation Frame Offset Number (0020,9228): the
	// number of frames of the instances before this one.
	FrameOffset int
	// Frames is the Number of Frames of the instance.
	Frames int
}

// Concatenation is the logical multi-frame object of a validated
// Concatenation (PS3.3 C.7.6.16.2.2.4). It refers to its instances' files
// and copies no pixels: Locate maps a logical frame to an instance frame,
// whose data the caller reads from that instance.
type Concatenation struct {
	UID string
	// SourceSOPInstanceUID is SOP Instance UID of Concatenation Source
	// (0020,0242), the instance the Concatenation stands for.
	SourceSOPInstanceUID string
	SOPClassUID          string
	// Instances are ordered by In-concatenation Number.
	Instances []ConcatenationInstance
	frames    int
}

// consistentAttributes must be equal in every instance of a Concatenation:
// its identity and the Image Pixel description of its frames.
var consistentAttributes = []struct {
	tag  core.Tag
	name string
}{
	{tagSOPClassUID, "SOP Class UID"},
	{tagStudyInstanceUID, "Study Instance UID"},
	{tagSeriesInstanceUID, "Series Instance UID"},
	{tagSOPInstanceUIDOfConcatenationSource, "SOP Instance UID of Concatenation Source"},
	{tagFrameOfReferenceUID, "Frame of Reference UID"},
	{tagRows, "Rows"},
	{tagColumns, "Columns"},
	{tagSamplesPerPixel, "Samples per Pixel"},
	{tagPhotometricInterpretation, "Photometric Interpretation"},
	{tagBitsAllocated, "Bits Allocated"},
	{tagBitsStored, "Bits Stored"},
	{tagPixelRepresentation, "Pixel Representation"},
}

// NewConcatenation validates the instances of one Concatenation and builds
// its logical view. The instances may come in any order. It checks that they
// share the Concatenation UID and the attributes above, that In-concatenation
// Numbers run from 1 to In-concatenation Total Number (when present, the same
// in all) without gaps or repeats, that each Concatenation Frame Offset Number
// is the sum of the frames before it, and that each instance's Per-Frame
// Functional Groups Sequence has one item per frame.
func NewConcatenation(files []*object.File) (*Concatenation, error) {
	if len(files) == 0 {
		return nil, fmt.Errorf("%w: no instance", ErrConcatenationIncomplete)
	}
	var problems []error
	inconsistent := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("%w: %s", ErrConcatenationInconsistent, fmt.Sprintf(format, args...)))
	}
	concatenation := &Concatenation{}
	total := 0
	numbers := map[int]int{}
	var first *object.Object
	for position, file := range files {
		if file == nil || file.Dataset == nil {
			return nil, fmt.Errorf("%w: instance %d has no data set", ErrConcatenationInconsistent, position)
		}
		dataset := file.Dataset
		uid := cleanUID(dataset, tagConcatenationUID)
		if position == 0 {
			first = dataset
			concatenation.UID = uid
			concatenation.SourceSOPInstanceUID = cleanUID(dataset, tagSOPInstanceUIDOfConcatenationSource)
			concatenation.SOPClassUID = cleanUID(dataset, tagSOPClassUID)
			if uid == "" {
				return nil, fmt.Errorf("%w: instance without Concatenation UID", ErrConcatenationInconsistent)
			}
		} else {
			if uid != concatenation.UID {
				inconsistent("instance %d has Concatenation UID %q, not %q", position, uid, concatenation.UID)
			}
			for _, attribute := range consistentAttributes {
				if !sameValue(first, dataset, attribute.tag) {
					inconsistent("instance %d differs in %s", position, attribute.name)
				}
			}
		}
		number, ok := intValue(dataset, tagInConcatenationNumber)
		if !ok || number < 1 {
			inconsistent("instance %d has no valid In-concatenation Number", position)
			continue
		}
		if previous, duplicate := numbers[number]; duplicate {
			inconsistent("instances %d and %d both have In-concatenation Number %d", previous, position, number)
			continue
		}
		numbers[number] = position
		if value, present := intValue(dataset, tagInConcatenationTotalNumber); present {
			if total != 0 && value != total {
				inconsistent("instance %d has In-concatenation Total Number %d, not %d", position, value, total)
			}
			total = max(total, value)
		}
		frames, _ := intValue(dataset, tagNumberOfFrames)
		if frames < 1 {
			inconsistent("instance %d has no valid Number of Frames", position)
		}
		if items, ok := dataset.GetSequence(tagPerFrameFunctionalGroupsSequence); ok && len(items) != frames {
			inconsistent("instance %d has %d per-frame functional groups for %d frames", position, len(items), frames)
		}
		offset, ok := intValue(dataset, tagConcatenationFrameOffsetNumber)
		if !ok {
			inconsistent("instance %d has no Concatenation Frame Offset Number", position)
		}
		concatenation.Instances = append(concatenation.Instances, ConcatenationInstance{File: file, Number: number, FrameOffset: offset, Frames: frames})
	}
	if concatenation.SourceSOPInstanceUID == "" {
		inconsistent("no SOP Instance UID of Concatenation Source")
	}
	if len(problems) > 0 {
		return nil, errors.Join(problems...)
	}

	sort.Slice(concatenation.Instances, func(i, j int) bool { return concatenation.Instances[i].Number < concatenation.Instances[j].Number })
	expected := max(total, concatenation.Instances[len(concatenation.Instances)-1].Number)
	var missing []string
	for number := 1; number <= expected; number++ {
		if _, ok := numbers[number]; !ok {
			missing = append(missing, fmt.Sprint(number))
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%w: %d of %d instances, missing In-concatenation Number %s", ErrConcatenationIncomplete, len(concatenation.Instances), expected, strings.Join(missing, ", "))
	}
	for _, instance := range concatenation.Instances {
		if instance.FrameOffset != concatenation.frames {
			return nil, fmt.Errorf("%w: instance %d has Concatenation Frame Offset Number %d, want %d", ErrConcatenationInconsistent, instance.Number, instance.FrameOffset, concatenation.frames)
		}
		concatenation.frames += instance.Frames
	}
	return concatenation, nil
}

// NumberOfFrames is the number of frames of the logical object.
func (c *Concatenation) NumberOfFrames() int {
	if c == nil {
		return 0
	}
	return c.frames
}

// Locate returns the instance that holds the zero-based logical frame and the
// frame's zero-based index within that instance.
func (c *Concatenation) Locate(frame int) (ConcatenationInstance, int, error) {
	if c == nil || frame < 0 || frame >= c.frames {
		return ConcatenationInstance{}, 0, fmt.Errorf("%w: frame %d outside the %d frames", ErrConcatenationIncomplete, frame, c.NumberOfFrames())
	}
	position := sort.Search(len(c.Instances), func(i int) bool {
		return c.Instances[i].FrameOffset+c.Instances[i].Frames > frame
	})
	instance := c.Instances[position]
	return instance, frame - instance.FrameOffset, nil
}

// FunctionalGroupItem is object.Object.FunctionalGroupItem over the logical
// frames: the macro of the instance frame that holds the logical frame, from
// its Per-Frame or Shared Functional Groups.
func (c *Concatenation) FunctionalGroupItem(frame int, macro core.Tag) (*object.Object, object.FunctionalGroupSource, error) {
	instance, local, err := c.Locate(frame)
	if err != nil {
		return nil, object.FunctionalGroupNone, err
	}
	return instance.File.Dataset.FunctionalGroupItem(local, macro)
}

// FrameGeometryAt is object.Object.FrameGeometryAt over the logical frames.
func (c *Concatenation) FrameGeometryAt(frame int) (object.FrameGeometry, error) {
	instance, local, err := c.Locate(frame)
	if err != nil {
		return object.FrameGeometry{}, err
	}
	return instance.File.Dataset.FrameGeometryAt(local), nil
}

func cleanUID(obj *object.Object, tag core.Tag) string {
	value, _ := obj.GetString(tag)
	return strings.TrimRight(strings.TrimSpace(value), "\x00")
}

// intValue reads an integer from an IS string or a binary integer VR.
func intValue(obj *object.Object, tag core.Tag) (int, bool) {
	values := derivedio.Ints(obj, tag)
	if len(values) == 0 {
		return 0, false
	}
	return int(values[0]), true
}

// sameValue compares an attribute of two data sets by value (numbers as
// numbers, strings trimmed), treating two absences as equal.
func sameValue(a, b *object.Object, tag core.Tag) bool {
	if a.Has(tag) != b.Has(tag) {
		return false
	}
	if !a.Has(tag) {
		return true
	}
	if x := derivedio.Ints(a, tag); len(x) > 0 {
		y := derivedio.Ints(b, tag)
		return len(y) == len(x) && y[0] == x[0]
	}
	return cleanUID(a, tag) == cleanUID(b, tag)
}
