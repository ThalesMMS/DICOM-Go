package microscopy

import (
	"fmt"
	"slices"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
)

var (
	tagConcatenationUID           = core.NewTag(0x0020, 0x9161)
	tagInConcatenationNumber      = core.NewTag(0x0020, 0x9162)
	tagInConcatenationTotalNumber = core.NewTag(0x0020, 0x9163)
	tagConcatenationSourceUID     = core.NewTag(0x0020, 0x0242)
)

type slideConcatenation struct {
	uid                   string
	number, total, offset int
	image                 concatenationImage
}

type concatenationImage struct {
	sourceUID, classUID, frameOfReference, photometric string
	samples, allocated, stored, representation, planar int
}

type slideTileMember struct {
	source                 InstanceRef
	number, offset, frames int
}

func readSlideConcatenation(root *object.Object) (slideConcatenation, error) {
	uid := derivedio.CleanUID(root, tagConcatenationUID)
	_, hasUID := root.Get(tagConcatenationUID)
	_, hasSource := root.Get(tagConcatenationSourceUID)
	_, hasNumber := root.Get(tagInConcatenationNumber)
	_, hasTotal := root.Get(tagInConcatenationTotalNumber)
	_, hasOffset := root.Get(tagConcatenationFrameOffset)
	if !hasUID && !hasSource && !hasNumber && !hasTotal && !hasOffset {
		return slideConcatenation{}, nil
	}
	if !core.IsValidUID(uid) {
		return slideConcatenation{}, fmt.Errorf("%w: missing or invalid Concatenation UID", ErrInvalidSlide)
	}
	number := derivedio.Ints(root, tagInConcatenationNumber)
	offset := derivedio.Ints(root, tagConcatenationFrameOffset)
	if len(number) != 1 || number[0] < 1 || number[0] > 65535 ||
		len(offset) != 1 || offset[0] < number[0]-1 || offset[0] >= maxMicroscopyFrames ||
		(number[0] == 1 && offset[0] != 0) {
		return slideConcatenation{}, fmt.Errorf("%w: invalid concatenation part number or frame offset", ErrInvalidSlide)
	}
	total := 0
	if hasTotal {
		values := derivedio.Ints(root, tagInConcatenationTotalNumber)
		if len(values) != 1 || values[0] < max(2, number[0]) || values[0] > 65535 {
			return slideConcatenation{}, fmt.Errorf("%w: invalid In-concatenation Total Number", ErrInvalidSlide)
		}
		total = int(values[0])
	}
	sourceUID := derivedio.CleanUID(root, tagConcatenationSourceUID)
	if hasSource && !core.IsValidUID(sourceUID) {
		return slideConcatenation{}, fmt.Errorf("%w: invalid SOP Instance UID of Concatenation Source", ErrInvalidSlide)
	}
	return slideConcatenation{uid: uid, number: int(number[0]), total: total, offset: int(offset[0]), image: concatenationImage{
		sourceUID: sourceUID, classUID: derivedio.CleanUID(root, tagSOPClassUID),
		frameOfReference: derivedio.CleanUID(root, core.NewTag(0x0020, 0x0052)),
		photometric:      derivedio.CleanString(root, core.NewTag(0x0028, 0x0004)),
		samples:          derivedio.Int(root, core.NewTag(0x0028, 0x0002)),
		allocated:        derivedio.Int(root, core.NewTag(0x0028, 0x0100)),
		stored:           derivedio.Int(root, core.NewTag(0x0028, 0x0101)),
		representation:   derivedio.Int(root, core.NewTag(0x0028, 0x0103)),
		planar:           derivedio.Int(root, core.NewTag(0x0028, 0x0006)),
	}}, nil
}

func implicitTileCount(matrixWidth, matrixHeight, tileWidth, tileHeight, paths, planes int) (int, error) {
	if matrixWidth <= 0 || matrixHeight <= 0 || tileWidth <= 0 || tileHeight <= 0 {
		return 0, fmt.Errorf("%w: invalid implicit tile dimensions", ErrInvalidSlide)
	}
	total := 1
	for _, factor := range [...]int{ceilDiv(matrixWidth, tileWidth), ceilDiv(matrixHeight, tileHeight), paths, planes} {
		if factor <= 0 || total > maxMicroscopyFrames/factor {
			return 0, fmt.Errorf("%w: implicit tiling exceeds frame limits", ErrInvalidSlide)
		}
		total *= factor
	}
	return total, nil
}

func (l Level) concatenationTileCount() (int, error) {
	return implicitTileCount(l.MatrixWidth, l.MatrixHeight, l.TileWidth, l.TileHeight, len(l.OpticalPathIDs), len(l.FocalPlaneOffsets))
}

// addLevel builds or extends an immutable, sorted member list. A viewport
// snapshot created before Add continues to refer to its previously admitted
// frame spans and never observes a partially changed slice.
func (p *Pyramid) addLevel(level Level) error {
	if level.concatenation.uid == "" {
		p.Levels = append(p.Levels, level)
		return nil
	}
	var match = -1
	for index, current := range p.Levels {
		if current.concatenation.uid != level.concatenation.uid {
			continue
		}
		if current.ImplicitTiling != level.ImplicitTiling {
			return fmt.Errorf("%w: inconsistent tiling organization in concatenation", ErrInvalidSlide)
		}
		match = index
		break
	}
	if !level.ImplicitTiling {
		p.Levels = append(p.Levels, level)
		return nil
	}
	expected, err := level.concatenationTileCount()
	if err != nil {
		return err
	}
	member := slideTileMember{
		source: level.Source, number: level.concatenation.number,
		offset: level.FrameOffset, frames: level.FrameCount,
	}
	if member.source.SOPInstanceUID == "" || member.frames <= 0 ||
		member.offset != level.concatenation.offset || member.offset > expected-member.frames {
		return fmt.Errorf("%w: invalid frame span in TILED_FULL concatenation", ErrInvalidSlide)
	}
	if match >= 0 {
		joined, err := joinSlideMember(p.Levels[match], level, member, expected)
		if err != nil {
			return err
		}
		p.Levels[match] = joined
		return nil
	}
	if err := validateSlideSpan(member, level.concatenation.total, expected); err != nil {
		return err
	}
	level.ID = "concatenation:" + level.concatenation.uid
	level.members = []slideTileMember{member}
	p.Levels = append(p.Levels, level)
	return nil
}

func joinSlideMember(current, part Level, member slideTileMember, expected int) (Level, error) {
	if current.Source.StudyInstanceUID != part.Source.StudyInstanceUID ||
		current.Source.SeriesInstanceUID != part.Source.SeriesInstanceUID ||
		current.concatenation.image != part.concatenation.image ||
		current.MatrixWidth != part.MatrixWidth || current.MatrixHeight != part.MatrixHeight ||
		current.TileWidth != part.TileWidth || current.TileHeight != part.TileHeight ||
		current.PixelSpacingX != part.PixelSpacingX || current.PixelSpacingY != part.PixelSpacingY ||
		current.Origin != part.Origin || current.Orientation != part.Orientation ||
		current.ImagedVolumeDepth != part.ImagedVolumeDepth ||
		!slices.Equal(current.OpticalPathIDs, part.OpticalPathIDs) ||
		!slices.Equal(current.FocalPlaneOffsets, part.FocalPlaneOffsets) {
		return Level{}, fmt.Errorf("%w: inconsistent TILED_FULL concatenation geometry or identity", ErrInvalidSlide)
	}
	total := current.concatenation.total
	if part.concatenation.total > 0 {
		if total > 0 && total != part.concatenation.total {
			return Level{}, fmt.Errorf("%w: inconsistent In-concatenation Total Number", ErrInvalidSlide)
		}
		total = part.concatenation.total
	}
	for _, previous := range current.members {
		if previous.source.SOPInstanceUID == member.source.SOPInstanceUID || previous.number == member.number {
			return Level{}, fmt.Errorf("%w: duplicate TILED_FULL concatenation member", ErrInvalidSlide)
		}
		if err := validateSlideSpan(previous, total, expected); err != nil {
			return Level{}, err
		}
	}
	if err := validateSlideSpan(member, total, expected); err != nil {
		return Level{}, err
	}
	index := sort.Search(len(current.members), func(index int) bool {
		return current.members[index].number >= member.number
	})
	if index > 0 && !compatibleSlideSpans(current.members[index-1], member) ||
		index < len(current.members) && !compatibleSlideSpans(member, current.members[index]) {
		return Level{}, fmt.Errorf("%w: frame offset disagrees with concatenation member order", ErrInvalidSlide)
	}
	members := make([]slideTileMember, len(current.members)+1)
	copy(members[:index], current.members[:index])
	members[index] = member
	copy(members[index+1:], current.members[index:])
	current.members = members
	current.Source = members[0].source
	current.FrameOffset = members[0].offset
	current.FrameCount += member.frames
	current.concatenation.total = total
	return current, nil
}

func compatibleSlideSpans(left, right slideTileMember) bool {
	missing := right.number - left.number - 1
	gap := right.offset - left.offset - left.frames
	return (missing == 0 && gap == 0) || (missing > 0 && gap >= missing)
}

func validateSlideSpan(member slideTileMember, total, expected int) error {
	end := member.offset + member.frames
	if (member.number == 1 && end == expected) || // Every declared concatenation has at least two SOP Instances.
		(total > 0 && (member.number > total || expected-end < total-member.number ||
			(member.number == total && end != expected))) {
		return fmt.Errorf("%w: TILED_FULL span disagrees with concatenation count", ErrInvalidSlide)
	}
	return nil
}

// ValidateConcatenations verifies that every implicit TILED_FULL concatenation
// added to the pyramid supplies every tile position. During incremental Add
// operations an incomplete level can only return tiles represented by its
// known member spans.
func (p Pyramid) ValidateConcatenations() error {
	for _, level := range p.Levels {
		members := level.members
		if len(members) == 0 {
			continue
		}
		expected, err := level.concatenationTileCount()
		if err != nil {
			return err
		}
		first, last := members[0], members[len(members)-1]
		total := level.concatenation.total
		// Add already enforces no overlap, consistency of adjacent ordinals,
		// and strictly increasing positive member numbers.
		if len(members) < 2 || first.number != 1 || len(members) != last.number ||
			(total > 0 && len(members) != total) || level.FrameCount != expected {
			return fmt.Errorf("%w: incomplete TILED_FULL concatenation", ErrInvalidSlide)
		}
	}
	return nil
}

func (l Level) locateTileFrame(ordinal int) (InstanceRef, int, bool) {
	if len(l.members) == 0 {
		frame := ordinal - l.FrameOffset + 1
		return l.Source, frame, frame > 0 && frame <= l.FrameCount
	}
	index := sort.Search(len(l.members), func(index int) bool {
		member := l.members[index]
		return ordinal < member.offset+member.frames
	})
	if index == len(l.members) || ordinal < l.members[index].offset {
		return InstanceRef{}, 0, false
	}
	member := l.members[index]
	return member.source, ordinal - member.offset + 1, true
}
