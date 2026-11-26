package object

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/encaplayout"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/parser"
)

// ErrFrameSource reports a FrameSource that does not match the data set it
// is written with: a frame count other than Number of Frames, a frame of the
// wrong length, or a frame that writes other bytes than it declared.
var ErrFrameSource = errors.New("dicom: invalid frame source")

// FrameSource supplies Pixel Data frame by frame to WriteFileFrames, so that
// a file is written holding one frame in memory at a time (or none, when
// WriteFrameTo copies from a file).
type FrameSource interface {
	// Frames is the number of frames, which must equal the data set's
	// Number of Frames (1 when it has none).
	Frames() int
	// FrameLength is the byte length of frame i: for native Pixel Data every
	// frame has the same length; an encapsulated frame is one compressed
	// codestream, without Item padding.
	FrameLength(i int) (int64, error)
	// WriteFrameTo writes exactly FrameLength(i) bytes of frame i to w. A
	// native frame is in the byte order of the transfer syntax the file is
	// written in, whatever the data set's ValueByteOrder: WriteFileFrames
	// writes it unchanged, while it reverses the data set's own fixed-width
	// values when their order differs from that syntax. An encapsulated
	// frame is a byte stream.
	WriteFrameTo(i int, w io.Writer) (int64, error)
}

// PackedBitFrameSource supplies native one-bit frames with each frame starting
// at bit zero of its first byte (least significant bit first). FrameBits must
// match Rows*Columns*SamplesPerPixel. WriteFileFrames concatenates those bits
// continuously, discarding no samples and inserting no per-frame padding.
// Unused high bits of a frame's final byte must be zero. This optional contract
// leaves byte-aligned FrameSource behavior unchanged.
type PackedBitFrameSource interface {
	FrameSource
	FrameBits(i int) (int64, error)
}

// OffsetTablePolicy selects the offset tables of encapsulated Pixel Data
// written by WriteFileFrames.
type OffsetTablePolicy uint8

const (
	// OffsetTableAuto writes a Basic Offset Table while every frame offset
	// fits 32 bits, and otherwise an empty Basic Offset Table with the
	// Extended Offset Table and its Lengths, as the transcoder does.
	OffsetTableAuto OffsetTablePolicy = iota
	// OffsetTableBasic writes the Basic Offset Table; offsets beyond 32 bits
	// are an error.
	OffsetTableBasic
	// OffsetTableExtended writes an empty Basic Offset Table with the
	// Extended Offset Table (7FE0,0001) and Lengths (7FE0,0002).
	OffsetTableExtended
	// OffsetTableEmpty writes an empty Basic Offset Table and no Extended
	// Offset Table.
	OffsetTableEmpty
)

// FrameWriteOptions configures WriteFileFrames.
type FrameWriteOptions struct {
	WriteFileOptions
	// OffsetTable selects the offset tables of encapsulated Pixel Data.
	OffsetTable OffsetTablePolicy
	// MaxFragmentBytes bounds each encapsulated Item payload, including its
	// even padding. Zero keeps one Fragment per frame. A positive cap must
	// be even and at least 2. Syntaxes requiring one Fragment per frame
	// reject frames exceeding the cap. Extended tables cannot index split
	// frames. Native output rejects nonzero encapsulation options.
	MaxFragmentBytes int64
	// MaxFragments bounds the total number of output Fragments; zero leaves
	// it uncapped. This does not include the Basic Offset Table Item.
	MaxFragments int
}

var (
	tagFrameExtendedOffsetTable        = core.NewTag(0x7fe0, 0x0001)
	tagFrameExtendedOffsetTableLengths = core.NewTag(0x7fe0, 0x0002)
	tagFrameEncapsulatedTotalLength    = core.NewTag(0x7fe0, 0x0003)
	tagFrameNumberOfFrames             = core.NewTag(0x0028, 0x0008)
	tagFrameBitsAllocated              = core.NewTag(0x0028, 0x0100)
)

// WriteFileFrames writes file as WriteFileWithOptions does, taking the Pixel
// Data value from frames instead of the data set: native frames are
// concatenated (with one pad byte when the total is odd) and encapsulated
// frames become one Item each unless MaxFragmentBytes divides them into
// smaller Items after the Basic Offset Table. Only a frame's final Item is
// padded to even length with a zero byte. Frames are pulled in order while the Pixel
// Data element is written, so the file is written with at most one frame in
// memory. The data set's own Pixel Data value (if any) is ignored, and its
// Extended Offset Table, Lengths and Encapsulated Pixel Data Value Total
// Length are replaced as the offset table policy requires. With the same
// frames the output is byte-identical to WriteFileWithOptions on a data set
// holding them, when the data set's ValueByteOrder is that of the written
// syntax (otherwise its other values are reversed to that order and the
// frames, already in it, are not). A group length recomputation
// (WriteFileOptions.GroupLength) that includes group 7FE0 uses cached encoded
// lengths; each frame is pulled once. Values deferred in sequence items are
// copied from the data set's source.
func WriteFileFrames(w io.Writer, file *File, frames FrameSource, opts FrameWriteOptions) error {
	if file == nil || file.Dataset == nil {
		return fmt.Errorf("dicom: nil file")
	}
	if file.incompletePixelDataRead != nil || file.partialRead != nil && !opts.AllowPartial {
		return ErrPartialFile
	}
	if _, incomplete := file.Dataset.IncompletePixelData(); incomplete {
		return ErrPartialFile
	}
	if frames == nil {
		return fmt.Errorf("%w: nil frame source", ErrFrameSource)
	}
	syntax, err := resolveWriteTransferSyntaxWithOptions(file, opts.WriteFileOptions)
	if err != nil {
		return err
	}
	if _, packed := frames.(PackedBitFrameSource); packed && syntax.Encapsulated {
		return fmt.Errorf("%w: packed bit source requires native output", ErrFrameSource)
	}
	count := frames.Frames()
	if count < 1 {
		return fmt.Errorf("%w: %d frames", ErrFrameSource, count)
	}
	if syntax.Encapsulated && opts.MaxFragments > 0 && count > opts.MaxFragments {
		return fmt.Errorf("%w: frame count exceeds fragment cap", ErrFrameSource)
	}
	if declared := declaredFrameCount(file.Dataset); declared != count {
		return fmt.Errorf("%w: %d frames for Number of Frames %d", ErrFrameSource, count, declared)
	}
	lengths := make([]int64, count)
	for i := range lengths {
		if lengths[i], err = frames.FrameLength(i); err != nil {
			return err
		}
		if lengths[i] <= 0 || (!syntax.Encapsulated && lengths[i] != lengths[0]) {
			return fmt.Errorf("%w: frame %d has length %d", ErrFrameSource, i+1, lengths[i])
		}
	}

	elements := make([]core.Element, 0, len(file.Dataset.Elements())+2)
	for _, element := range file.Dataset.Elements() {
		switch element.Tag() {
		case core.TagPixelData, tagFrameExtendedOffsetTable, tagFrameExtendedOffsetTableLengths, tagFrameEncapsulatedTotalLength:
			continue
		}
		elements = append(elements, element)
	}
	provider := &frameValueProvider{base: file.Dataset.valueProvider, frames: frames, lengths: lengths}
	if syntax.Encapsulated {
		unsigned := make([]uint64, len(lengths))
		for i, length := range lengths {
			unsigned[i] = uint64(length)
		}
		if opts.MaxFragmentBytes < 0 {
			return fmt.Errorf("%w: negative fragment cap", ErrFrameSource)
		}
		layout, err := encaplayout.Plan(unsigned, uint64(opts.MaxFragmentBytes), encaplayout.Policy(opts.OffsetTable), opts.MaxFragments, encaplayout.SingleFragment(syntax.UID))
		if err != nil {
			return fmt.Errorf("%w: %v", ErrFrameSource, err)
		}
		provider.encapsulated, provider.bot, provider.fragmentCap = true, layout.Basic, opts.MaxFragmentBytes
		provider.encodedLength, err = frameEncodedValueLength(lengths, len(layout.Basic), layout.Fragments)
		if err != nil {
			return err
		}
		if layout.Extended != nil {
			extended, extendedLengths := layout.Extended, layout.Lengths
			if isBigEndianOrder(file.Dataset.ValueByteOrder()) {
				// The tables join raw values held in the data set's order.
				extended = valueencode.ReverseFixedWidth(extended, 8)
				extendedLengths = valueencode.ReverseFixedWidth(extendedLengths, 8)
			}
			elements = append(elements, core.NewRawElement(tagFrameExtendedOffsetTable, core.VROV, extended),
				core.NewRawElement(tagFrameExtendedOffsetTableLengths, core.VROV, extendedLengths))
		}
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}})
	} else {
		if opts.MaxFragmentBytes != 0 || opts.OffsetTable != OffsetTableAuto || opts.MaxFragments < 0 {
			return fmt.Errorf("%w: encapsulation options on native output", ErrFrameSource)
		}
		var total int64
		if packed, ok := frames.(PackedBitFrameSource); ok {
			bits, err := nativePackedFrameBits(file.Dataset)
			if err != nil {
				return err
			}
			for i := range lengths {
				declared, err := packed.FrameBits(i)
				if err != nil {
					return err
				}
				if declared != bits || lengths[i] != (bits+7)/8 {
					return fmt.Errorf("%w: inconsistent packed bit frame %d", ErrFrameSource, i+1)
				}
			}
			if bits > (int64(math.MaxUint32)-1)*8/int64(count) {
				return fmt.Errorf("%w: native packed Pixel Data exceeds 32-bit length", ErrFrameSource)
			}
			provider.packedBits = bits
			total = (bits*int64(count) + 7) / 8
		} else {
			if lengths[0] > (math.MaxUint32-1)/int64(count) {
				return fmt.Errorf("%w: native Pixel Data exceeds 32-bit length", ErrFrameSource)
			}
			total = lengths[0] * int64(count)
		}
		provider.pad = total%2 == 1
		provider.encodedLength = total + total%2
		vr := core.VROB
		if previous, ok := file.Dataset.Get(core.TagPixelData); ok && previous.VR() == core.VROW {
			vr = core.VROW
		} else if !ok || previous.VR() != core.VROB {
			if bits, err := file.Dataset.GetInts(tagFrameBitsAllocated); err == nil && len(bits) == 1 && bits[0] > 8 {
				vr = core.VROW
			}
		}
		elements = append(elements, core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: vr, Length: core.Length(total + total%2), LengthSet: true}})
	}
	dataset := FromElementsWithTextOptions(elements, file.Dataset.dict, file.Dataset.text)
	dataset.SetValueByteOrder(file.Dataset.ValueByteOrder())
	dataset.valueProvider = provider
	framed := &File{Preamble: file.Preamble, Meta: file.Meta, Dataset: dataset, TransferSyntax: file.TransferSyntax}
	return WriteFileWithOptions(w, framed, opts.WriteFileOptions)
}

// declaredFrameCount is Number of Frames, or 1 without it.
func declaredFrameCount(dataset *Object) int {
	value, ok := dataset.GetString(tagFrameNumberOfFrames)
	value = strings.TrimSpace(value)
	if !ok || value == "" {
		return 1
	}
	n, err := strconv.Atoi(value)
	if err != nil {
		return -1
	}
	return n
}

// frameWriterOffsetTables computes the Basic or Extended Offset Table of
// frames written one Item each: offsets are relative to the first Item after
// the Basic Offset Table and count each Item's header and padding.
func frameWriterOffsetTables(lengths []int64, policy OffsetTablePolicy) (bot, eot, eotLengths []byte, err error) {
	unsigned := make([]uint64, len(lengths))
	for i, length := range lengths {
		if length <= 0 {
			return nil, nil, nil, fmt.Errorf("%w: empty frame", ErrFrameSource)
		}
		unsigned[i] = uint64(length)
	}
	layout, err := encaplayout.Plan(unsigned, 0, encaplayout.Policy(policy), 0, false)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("%w: %v", ErrFrameSource, err)
	}
	return layout.Basic, layout.Extended, layout.Lengths, nil
}

// frameValueProvider streams Pixel Data from a FrameSource and every other
// deferred value from the data set's own provider.
type frameValueProvider struct {
	base          valueProvider
	frames        FrameSource
	lengths       []int64
	encapsulated  bool
	bot           []byte
	pad           bool
	fragmentCap   int64
	encodedLength int64
	packedBits    int64
}

// valueInDestinationOrder reports that Pixel Data comes from the
// FrameSource, which produces it in the destination byte order; every other
// deferred value is in the data set's ValueByteOrder.
func (p *frameValueProvider) valueInDestinationOrder(tag core.Tag) bool {
	return tag == core.TagPixelData
}

func (p *frameValueProvider) CopyValueTo(tag core.Tag, w io.Writer) (int64, error) {
	if tag != core.TagPixelData {
		if p.base == nil {
			return 0, fmt.Errorf("dicom: no value provider available for element %s", tag)
		}
		return p.base.CopyValueTo(tag, w)
	}
	if p.packedBits > 0 {
		return p.copyPackedBits(w)
	}
	var written int64
	put := func(data []byte) error {
		n, err := w.Write(data)
		written += int64(n)
		if err == nil && n != len(data) {
			err = io.ErrShortWrite
		}
		return err
	}
	item := func(length uint32) error {
		var header [8]byte
		binary.LittleEndian.PutUint16(header[0:], core.TagItem.Group)
		binary.LittleEndian.PutUint16(header[2:], core.TagItem.Element)
		binary.LittleEndian.PutUint32(header[4:], length)
		return put(header[:])
	}
	if p.encapsulated {
		if err := item(uint32(len(p.bot))); err != nil {
			return written, err
		}
		if err := put(p.bot); err != nil {
			return written, err
		}
	}
	for i, length := range p.lengths {
		if p.encapsulated {
			fragmented := fragmentFrameWriter{w: w, remaining: length, cap: p.fragmentCap}
			n, err := p.frames.WriteFrameTo(i, &fragmented)
			written += fragmented.written
			if err != nil {
				return written, err
			}
			if n != length || fragmented.remaining != 0 {
				return written, fmt.Errorf("%w: frame %d wrote %d bytes, declared %d", ErrFrameSource, i+1, n, length)
			}
			if length%2 == 1 {
				if err := put([]byte{0}); err != nil {
					return written, err
				}
			}
			continue
		}
		n, err := p.frames.WriteFrameTo(i, w)
		written += n
		if err != nil {
			return written, err
		}
		if n != length {
			return written, fmt.Errorf("%w: frame %d wrote %d bytes, declared %d", ErrFrameSource, i+1, n, length)
		}
		if p.encapsulated && length%2 == 1 {
			if err := put([]byte{0}); err != nil {
				return written, err
			}
		}
	}
	if p.encapsulated {
		var delimiter [8]byte
		binary.LittleEndian.PutUint16(delimiter[0:], core.TagSequenceDelimitationItem.Group)
		binary.LittleEndian.PutUint16(delimiter[2:], core.TagSequenceDelimitationItem.Element)
		return written, put(delimiter[:])
	}
	if p.pad {
		return written, put([]byte{0})
	}
	return written, nil
}

// fragmentFrameWriter inserts Item headers as chunks arrive from FrameSource.
// Intermediate payloads are even, so padding can never enter the codestream.
type fragmentFrameWriter struct {
	w                                          io.Writer
	remaining, cap, fragmentRemaining, written int64
}

func (f *fragmentFrameWriter) Write(data []byte) (int, error) {
	if int64(len(data)) > f.remaining {
		return 0, fmt.Errorf("%w: frame exceeds declared length", ErrFrameSource)
	}
	consumed := 0
	for len(data) > 0 {
		if f.fragmentRemaining == 0 {
			length := int64(encaplayout.FragmentLength(uint64(f.remaining), uint64(f.cap)))
			var header [8]byte
			binary.LittleEndian.PutUint16(header[0:], core.TagItem.Group)
			binary.LittleEndian.PutUint16(header[2:], core.TagItem.Element)
			binary.LittleEndian.PutUint32(header[4:], uint32(length+length%2))
			n, err := f.w.Write(header[:])
			f.written += int64(n)
			if err == nil && n != len(header) {
				err = io.ErrShortWrite
			}
			if err != nil {
				return consumed, err
			}
			f.fragmentRemaining = length
		}
		chunk := min(int64(len(data)), f.fragmentRemaining)
		n, err := f.w.Write(data[:int(chunk)])
		f.written += int64(n)
		f.remaining -= int64(n)
		f.fragmentRemaining -= int64(n)
		consumed += n
		data = data[n:]
		if err == nil && int64(n) != chunk {
			err = io.ErrShortWrite
		}
		if err != nil {
			return consumed, err
		}
	}
	return consumed, nil
}

// The fragment count comes from the existing encapsulation planner, including
// split/terminal-fragment rules. No frame is read to compute this length.
func frameEncodedValueLength(lengths []int64, botBytes, fragments int) (int64, error) {
	if botBytes < 0 || fragments < 0 || int64(fragments) > (math.MaxInt64-16-int64(botBytes))/8 {
		return 0, fmt.Errorf("%w: encoded value length overflow", ErrFrameSource)
	}
	n := int64(16) + int64(botBytes) + 8*int64(fragments)
	for _, length := range lengths {
		if length < 0 || length == math.MaxInt64 {
			return 0, fmt.Errorf("%w: encoded value length overflow", ErrFrameSource)
		}
		padded := length + length%2
		if n > math.MaxInt64-padded {
			return 0, fmt.Errorf("%w: encoded value length overflow", ErrFrameSource)
		}
		n += padded
	}
	return n, nil
}

func (p *frameValueProvider) encodedValueLength(tag core.Tag) (int64, error) {
	if p == nil {
		return 0, parser.ErrEncodedLengthUnavailable
	}
	if tag == core.TagPixelData {
		return p.encodedLength, nil
	}
	if provider, ok := p.base.(encodedValueLengthProvider); ok {
		return provider.encodedValueLength(tag)
	}
	return 0, parser.ErrEncodedLengthUnavailable
}
