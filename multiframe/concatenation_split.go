package multiframe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
)

// ErrConcatenationSplit reports a source that cannot be split into a
// Concatenation, or options that cannot be met.
var ErrConcatenationSplit = errors.New("dicom/multiframe: cannot split into a concatenation")

var (
	tagExtendedOffsetTable        = core.NewTag(0x7FE0, 0x0001)
	tagExtendedOffsetTableLengths = core.NewTag(0x7FE0, 0x0002)
	tagEncapsulatedTotalLength    = core.NewTag(0x7FE0, 0x0003)
)

// maxConcatenationInstances is the In-concatenation Number range (US).
const maxConcatenationInstances = 1<<16 - 1

// SplitOptions bound the instances of a Concatenation. At least one of
// MaxFrames and MaxPixelBytes must be set; every instance holds at least one
// frame, so a single frame larger than MaxPixelBytes gets an instance of its
// own.
type SplitOptions struct {
	// MaxFrames is the most frames one instance holds; 0 for no bound.
	MaxFrames int
	// MaxPixelBytes is the most stored frame bytes (native frame bytes, or
	// encapsulated codestream bytes without Item headers and offset tables)
	// one instance holds; 0 for no bound. For native one-bit data this counts
	// individually packed frame lengths before combining their bits.
	MaxPixelBytes int64
	// ConcatenationUID is Concatenation UID (0020,9161); empty generates
	// one.
	ConcatenationUID string
	// SOPInstanceUIDs are the instances' SOP Instance UIDs in
	// In-concatenation Number order; empty generates them. When set it must
	// hold one UID per instance.
	SOPInstanceUIDs []string
	// Write configures each instance's file (offset tables of encapsulated
	// Pixel Data, file meta).
	Write object.FrameWriteOptions
}

// ConcatenationPart is one instance of a split Concatenation.
type ConcatenationPart struct {
	// Number is In-concatenation Number (0020,9162), from 1.
	Number int
	// FirstFrame is the zero-based source frame the instance starts at, its
	// Concatenation Frame Offset Number (0020,9228).
	FirstFrame int
	// Frames is the instance's Number of Frames.
	Frames int
	// File is the instance without Pixel Data: the source attributes with
	// its own SOP Instance UID, Number of Frames, Per-Frame Functional
	// Groups items and Concatenation attributes.
	File *object.File
}

// PlanConcatenation splits source, whose stored frames have frameLengths
// bytes, into the instances of a Concatenation (PS3.3 C.7.6.16.2.2.4): every
// instance keeps the source attributes, including the Shared Functional
// Groups and Instance Number, and gets a new SOP Instance UID, the
// Concatenation UID, SOP Instance UID of Concatenation Source (the source's
// SOP Instance UID), In-concatenation Number and Total Number,
// Concatenation Frame Offset Number, its Number of Frames and its slice of the
// Per-Frame Functional Groups Sequence. Representative Frame Number is kept,
// renumbered, only by the instance that holds that frame. Frames are taken in
// order and an instance is closed when the next frame would exceed a bound.
//
// The source must be a multi-frame object of the Multi-frame Functional
// Groups Module (a Shared Functional Groups Sequence and, when present, one
// Per-Frame item per frame) that is not already part of a Concatenation.
// source is not changed; the parts share its element values.
func PlanConcatenation(source *object.File, frameLengths []int64, opts SplitOptions) ([]ConcatenationPart, error) {
	if source == nil || source.Dataset == nil {
		return nil, fmt.Errorf("%w: nil source", ErrConcatenationSplit)
	}
	if opts.MaxFrames < 0 || opts.MaxPixelBytes < 0 || (opts.MaxFrames == 0 && opts.MaxPixelBytes == 0) {
		return nil, fmt.Errorf("%w: set MaxFrames or MaxPixelBytes", ErrConcatenationSplit)
	}
	dataset := source.Dataset
	sourceUID := cleanUID(dataset, tagSOPInstanceUID)
	switch {
	case sourceUID == "":
		return nil, fmt.Errorf("%w: source has no SOP Instance UID", ErrConcatenationSplit)
	case cleanUID(dataset, tagConcatenationUID) != "":
		return nil, fmt.Errorf("%w: source is already an instance of a Concatenation", ErrConcatenationSplit)
	case !dataset.Has(tagSharedFunctionalGroupsSequence):
		return nil, fmt.Errorf("%w: source has no Shared Functional Groups Sequence (Multi-frame Functional Groups Module)", ErrConcatenationSplit)
	}
	frames, _ := intValue(dataset, tagNumberOfFrames)
	if frames < 1 || frames != len(frameLengths) {
		return nil, fmt.Errorf("%w: Number of Frames %d for %d stored frames", ErrConcatenationSplit, frames, len(frameLengths))
	}
	var perFrame []core.DataSet
	if element, ok := dataset.Get(tagPerFrameFunctionalGroupsSequence); ok {
		sequence, isSequence := element.Value.(core.SequenceValue)
		if !isSequence || len(sequence.Items) != frames {
			return nil, fmt.Errorf("%w: Per-Frame Functional Groups Sequence does not have one item per frame", ErrConcatenationSplit)
		}
		perFrame = sequence.Items
	}
	for i, length := range frameLengths {
		if length <= 0 {
			return nil, fmt.Errorf("%w: frame %d has length %d", ErrConcatenationSplit, i+1, length)
		}
	}

	// Ranges: frames in order, closing an instance at a bound.
	type span struct{ first, count int }
	var spans []span
	var bytes int64
	for i, length := range frameLengths {
		last := len(spans) - 1
		if last >= 0 && (opts.MaxFrames == 0 || spans[last].count < opts.MaxFrames) &&
			(opts.MaxPixelBytes == 0 || length <= opts.MaxPixelBytes-bytes) {
			spans[last].count++
			if opts.MaxPixelBytes > 0 {
				bytes += length
			}
			continue
		}
		spans = append(spans, span{first: i, count: 1})
		bytes = length
	}
	if len(spans) > maxConcatenationInstances {
		return nil, fmt.Errorf("%w: %d instances exceed In-concatenation Number %d", ErrConcatenationSplit, len(spans), maxConcatenationInstances)
	}
	if len(opts.SOPInstanceUIDs) != 0 && len(opts.SOPInstanceUIDs) != len(spans) {
		return nil, fmt.Errorf("%w: %d SOP Instance UIDs for %d instances", ErrConcatenationSplit, len(opts.SOPInstanceUIDs), len(spans))
	}
	concatenationUID, err := splitUID(opts.ConcatenationUID)
	if err != nil {
		return nil, err
	}
	representative, hasRepresentative := intValue(dataset, tagRepresentativeFrameNumber)
	// Binary values are in the data set byte order, as its own raw values.
	order := dataset.ValueByteOrder()

	parts := make([]ConcatenationPart, len(spans))
	seen := map[string]bool{sourceUID: true, concatenationUID: true}
	for index, s := range spans {
		uid := ""
		if len(opts.SOPInstanceUIDs) != 0 {
			uid = opts.SOPInstanceUIDs[index]
		}
		if uid, err = splitUID(uid); err != nil {
			return nil, err
		}
		if seen[uid] {
			return nil, fmt.Errorf("%w: UID %s is used twice", ErrConcatenationSplit, uid)
		}
		seen[uid] = true
		elements := make([]core.Element, 0, len(dataset.Elements())+8)
		for _, element := range dataset.Elements() {
			switch element.Tag() {
			case core.TagPixelData, tagExtendedOffsetTable, tagExtendedOffsetTableLengths, tagEncapsulatedTotalLength,
				tagPerFrameFunctionalGroupsSequence, tagNumberOfFrames, tagSOPInstanceUID, tagRepresentativeFrameNumber,
				tagConcatenationUID, tagInConcatenationNumber, tagInConcatenationTotalNumber,
				tagConcatenationFrameOffsetNumber, tagSOPInstanceUIDOfConcatenationSource:
				continue
			}
			elements = append(elements, element)
		}
		elements = append(elements,
			splitText(tagSOPInstanceUID, core.VRUI, uid),
			splitText(tagNumberOfFrames, core.VRIS, strconv.Itoa(s.count)),
			splitText(tagConcatenationUID, core.VRUI, concatenationUID),
			splitText(tagSOPInstanceUIDOfConcatenationSource, core.VRUI, sourceUID),
			core.NewRawElement(tagInConcatenationNumber, core.VRUS, u16(order, uint16(index+1))),
			core.NewRawElement(tagInConcatenationTotalNumber, core.VRUS, u16(order, uint16(len(spans)))),
			core.NewRawElement(tagConcatenationFrameOffsetNumber, core.VRUL, u32(order, uint32(s.first))),
		)
		if perFrame != nil {
			elements = append(elements, core.Element{
				Header: core.ElementHeader{Tag: tagPerFrameFunctionalGroupsSequence, VR: core.VRSQ, Length: core.UndefinedLength, LengthSet: true},
				Value:  core.SequenceValue{Items: perFrame[s.first : s.first+s.count : s.first+s.count]},
			})
		}
		if hasRepresentative && representative > s.first && representative <= s.first+s.count {
			elements = append(elements, core.NewRawElement(tagRepresentativeFrameNumber, core.VRUS, u16(order, uint16(representative-s.first))))
		}
		part := object.FromElements(elements, std.Dictionary)
		part.SetValueByteOrder(order)
		file := &object.File{Dataset: part, TransferSyntax: source.TransferSyntax}
		if err := file.RebuildFileMetaWithOptions(opts.Write.WriteFileOptions); err != nil {
			return nil, fmt.Errorf("%w: instance %d file meta: %w", ErrConcatenationSplit, index+1, err)
		}
		parts[index] = ConcatenationPart{Number: index + 1, FirstFrame: s.first, Frames: s.count, File: file}
	}
	return parts, nil
}

// SplitConcatenation splits the object frames reads into a Concatenation
// (see PlanConcatenation) and writes each instance, in In-concatenation
// Number order, to the writer create returns for it (closed after the
// instance when it is an io.Closer). Frames are pulled one at a time while
// writing an instance. Pixel Data uses the source transfer syntax.
// Encapsulated frames retain their codestreams and get new offset tables.
// Native one-bit frames are packed continuously. Eight-bit Big Endian OW
// samples are written in sample order as OB.
func SplitConcatenation(ctx context.Context, frames *pixeldata.FrameReader, opts SplitOptions, create func(ConcatenationPart) (io.Writer, error)) ([]ConcatenationPart, error) {
	if frames == nil || frames.File() == nil || create == nil {
		return nil, fmt.Errorf("%w: nil frame reader or writer", ErrConcatenationSplit)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	metadata := frames.Metadata()
	syntax := frames.TransferSyntax()
	source := frames.File()
	packedBits := int64(0)
	if !syntax.Encapsulated && metadata.BitsAllocated == 1 {
		packedBits = int64(metadata.Rows) * int64(metadata.Columns) * int64(metadata.SamplesPerPixel)
	}
	pixel, _ := source.Dataset.Get(core.TagPixelData)
	decoded := packedBits > 0 || (!syntax.Encapsulated && metadata.BitsAllocated == 8 &&
		syntax.ByteOrder == binary.BigEndian && pixel.VR() == core.VROW)
	count := metadata.NumberOfFrames
	lengths := make([]int64, count)
	for i := range lengths {
		if decoded {
			lengths[i] = metadata.FrameSize()
			continue
		}
		length, err := frames.RawFrameLength(i)
		if err != nil {
			return nil, fmt.Errorf("%w: frame %d: %w", ErrConcatenationSplit, i+1, err)
		}
		lengths[i] = length
	}
	if source.TransferSyntax.UID == "" {
		source = &object.File{Dataset: source.Dataset, Meta: source.Meta, TransferSyntax: syntax}
	}
	parts, err := PlanConcatenation(source, lengths, opts)
	if err != nil {
		return nil, err
	}
	for _, part := range parts {
		w, err := create(part)
		if err != nil {
			return nil, fmt.Errorf("instance %d: %w", part.Number, err)
		}
		base := &partFrames{ctx: ctx, reader: frames, part: part, lengths: lengths, decoded: decoded}
		var input object.FrameSource = base
		if packedBits > 0 {
			input = &packedPartFrames{partFrames: base, bits: packedBits}
		}
		writeErr := object.WriteFileFrames(w, part.File, input, opts.Write)
		if closer, ok := w.(io.Closer); ok {
			if closeErr := closer.Close(); writeErr == nil {
				writeErr = closeErr
			}
		}
		if writeErr != nil {
			return nil, fmt.Errorf("instance %d: %w", part.Number, writeErr)
		}
	}
	return parts, nil
}

// partFrames is the object.FrameSource of one instance: its range of the
// source frames, read on demand.
type partFrames struct {
	ctx     context.Context
	reader  *pixeldata.FrameReader
	part    ConcatenationPart
	lengths []int64
	decoded bool
}

func (p *partFrames) Frames() int { return p.part.Frames }

func (p *partFrames) FrameLength(i int) (int64, error) {
	if i < 0 || i >= p.part.Frames {
		return 0, fmt.Errorf("%w: frame %d of %d", object.ErrFrameSource, i+1, p.part.Frames)
	}
	return p.lengths[p.part.FirstFrame+i], nil
}

func (p *partFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	if i < 0 || i >= p.part.Frames {
		return 0, fmt.Errorf("%w: frame %d of %d", object.ErrFrameSource, i+1, p.part.Frames)
	}
	index := p.part.FirstFrame + i
	var frame []byte
	var err error
	if p.decoded {
		frame, _, err = p.reader.Frame(p.ctx, index)
	} else {
		frame, err = p.reader.RawFrame(p.ctx, index)
	}
	if err != nil {
		return 0, err
	}
	n, err := w.Write(frame)
	return int64(n), err
}

type packedPartFrames struct {
	*partFrames
	bits int64
}

func (p *packedPartFrames) FrameBits(i int) (int64, error) {
	_, err := p.FrameLength(i)
	return p.bits, err
}

func u16(order binary.ByteOrder, v uint16) []byte {
	out := make([]byte, 2)
	order.PutUint16(out, v)
	return out
}

func u32(order binary.ByteOrder, v uint32) []byte {
	out := make([]byte, 4)
	order.PutUint32(out, v)
	return out
}

func splitUID(uid string) (string, error) {
	if uid == "" {
		return core.NewUID()
	}
	if !core.IsValidUID(uid) {
		return "", fmt.Errorf("%w: invalid UID %q", ErrConcatenationSplit, uid)
	}
	return uid, nil
}

func splitText(tag core.Tag, vr core.VR, value string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue{value}}
}
