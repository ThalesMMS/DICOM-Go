// Package hologic implements an opt-in reader for one legacy Hologic
// private JPEG-LS layout. Its profile is based on static evidence and
// synthetic fixtures. Clinical vendor interoperability needs an independent
// reference and a de-identified legacy sample.
package hologic

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sort"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/jpegls"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var (
	// ErrNotProfile means the object belongs to the ordinary DICOM path or
	// does not identify the legacy private creator in the expected block.
	ErrNotProfile = errors.New("hologic: not the supported private JPEG-LS profile")
	// ErrInvalidProfile reports a recognized creator with inconsistent or
	// unsupported private storage.
	ErrInvalidProfile = errors.New("hologic: invalid private JPEG-LS profile")
)

var (
	creatorTag          = core.NewTag(0x7e01, 0x0010)
	sequenceTag         = core.NewTag(0x7e01, 0x1010)
	blockTag            = core.NewTag(0x7e01, 0x1012)
	floatPixelTag       = core.NewTag(0x7fe0, 0x0008)
	doubleFloatPixelTag = core.NewTag(0x7fe0, 0x0009)
)

const (
	creator            = "HOLOGIC, Inc."
	descriptorSize     = 37
	trailerSize        = 1024
	maxBlocks          = 4096
	maxStorageBytes    = 512 << 20
	maxCompressedFrame = 64 << 20
	maxNativeFrame     = 64 << 20
)

// BlockValueTag identifies the OB value eligible for nested deferral in
// explicit little endian. It does not identify the profile on its own.
func BlockValueTag() core.Tag { return blockTag }

type block struct {
	end  int64
	path [2]parser.BulkDataPathStep
	data []byte
}

type extent struct{ start, end int64 }

// Source indexes ordered entropy ranges. The caller owns the parsed object
// and any deferred backing source. Keep both open and unchanged through
// the last Frame call. Calls on one Source serialize deferred replay.
type Source struct {
	dataset  *object.Object
	metadata pixeldata.Metadata
	decoder  *jpegls.Codec
	frameDS  *object.Object
	blocks   []block
	frames   []extent
	gate     chan struct{}
}

// IsCandidate identifies a possible private layout without opening or
// validating its block sequence. Explicit admission still requires Open.
func IsCandidate(file *object.File) bool {
	return claimsCreator(file) && file.Dataset.Has(sequenceTag)
}

func claimsCreator(file *object.File) bool {
	if file == nil || file.Dataset == nil {
		return false
	}
	dataset := file.Dataset
	return !dataset.Has(core.TagPixelData) && !dataset.Has(floatPixelTag) &&
		!dataset.Has(doubleFloatPixelTag) && isCreator(dataset)
}

// Open checks the exact creator and fixed block reservation, and indexes the
// offsets without decoding or allocating complete image payloads. Standard
// Pixel Data takes precedence, including under the same private creator.
func Open(ctx context.Context, file *object.File) (*Source, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !claimsCreator(file) {
		return nil, ErrNotProfile
	}
	if file.TransferSyntax.UID != transfer.ExplicitVRLittleEndian.UID {
		return nil, fmt.Errorf("%w: explicit little endian required", ErrInvalidProfile)
	}
	s := &Source{dataset: file.Dataset, decoder: jpegls.New(), gate: make(chan struct{}, 1)}
	if err := s.loadBlocks(ctx); err != nil {
		return nil, err
	}
	if err := s.loadDescriptor(ctx); err != nil {
		return nil, err
	}
	s.frameDS = nativeFrameDataset(s.metadata)
	return s, nil
}

func isCreator(obj *object.Object) bool {
	elem, ok := obj.Get(creatorTag)
	if !ok || elem.VR() != core.VRLO || obj.HasDuplicateParsedTag(creatorTag) {
		return false
	}
	values, ok := obj.GetStrings(creatorTag)
	return ok && len(values) == 1 && values[0] == creator
}

func (s *Source) loadBlocks(ctx context.Context) error {
	element, found := s.dataset.Get(sequenceTag)
	sequence, ok := element.Value.(core.SequenceValue)
	if !found || !ok || element.VR() != core.VRSQ || s.dataset.HasDuplicateParsedTag(sequenceTag) ||
		len(sequence.Items) == 0 || len(sequence.Items) > maxBlocks {
		return fmt.Errorf("%w: missing, ambiguous or oversized block sequence", ErrInvalidProfile)
	}
	items, _ := s.dataset.GetSequence(sequenceTag)
	var total int64
	for index, item := range items {
		if err := ctx.Err(); err != nil {
			return err
		}
		// The observed wire layout uses a reserved root creator and may
		// omit the duplicate creator in a block item. An explicit item
		// creator must agree with the root's reservation.
		if item.Has(creatorTag) && !isCreator(item) {
			return fmt.Errorf("%w: item %d reserves the block for another creator", ErrInvalidProfile, index)
		}
		element, ok := item.Get(blockTag)
		if !ok || element.VR() != core.VROB || item.HasDuplicateParsedTag(blockTag) {
			return fmt.Errorf("%w: item %d has no unique OB block", ErrInvalidProfile, index)
		}
		path := [2]parser.BulkDataPathStep{{Tag: sequenceTag, ItemIndex: index}, {Tag: blockTag, ItemIndex: -1}}
		data, resident := element.RawBytes()
		length := int64(len(data))
		if !resident {
			ref, ok, err := s.dataset.DeferredBulkDataReference(path[:])
			if err != nil || !ok || ref.UndefinedLength {
				return fmt.Errorf("%w: item %d has no bounded OB source", ErrInvalidProfile, index)
			}
			length = ref.Length
		}
		if length <= 0 || length%2 != 0 || length > maxStorageBytes-total {
			return fmt.Errorf("%w: item %d exceeds the stored value bounds", ErrInvalidProfile, index)
		}
		total += length
		s.blocks = append(s.blocks, block{end: total, path: path, data: data})
	}
	return nil
}

func (s *Source) loadDescriptor(ctx context.Context) error {
	last := len(s.blocks) - 1
	total := s.blocks[last].end
	lastStart := int64(0)
	if last > 0 {
		lastStart = s.blocks[last-1].end
	}
	if s.blocks[0].end < descriptorSize || total-lastStart < trailerSize {
		return fmt.Errorf("%w: truncated descriptor or trailer", ErrInvalidProfile)
	}
	var header [descriptorSize]byte
	if err := s.readAt(ctx, 0, header[:]); err != nil {
		return err
	}
	count := int(binary.LittleEndian.Uint16(header[20:22]))
	width, height := binary.LittleEndian.Uint16(header[24:26]), binary.LittleEndian.Uint16(header[28:30])
	// The wrapper uses byte 32 as SOF55 precision and byte 36 as SOS NEAR.
	precision, near := uint16(header[32]), header[36]
	if count < 1 || count > trailerSize/4-1 || width == 0 || height == 0 ||
		precision < 9 || precision > 16 || near != 0 || int64(width)*int64(height)*2 > maxNativeFrame {
		return fmt.Errorf("%w: unsupported dimensions, frame count, precision or NEAR", ErrInvalidProfile)
	}
	s.metadata = pixeldata.Metadata{
		Rows: height, Columns: width, NumberOfFrames: count, SamplesPerPixel: 1,
		BitsAllocated: 16, BitsStored: precision, HighBit: precision - 1, PhotometricInterpretation: "MONOCHROME2",
	}
	if err := checkMetadata(s.dataset, s.metadata); err != nil {
		return err
	}
	var offsets [trailerSize]byte
	tail := total - trailerSize
	if err := s.readAt(ctx, tail, offsets[:4*(count+1)]); err != nil {
		return err
	}
	// Optional per-frame descriptor records occupy the first block when it
	// is long enough. Entropy ranges must not point inside those records.
	headerEnd := int64(descriptorSize)
	if withRecords := int64(40 + count*24); withRecords <= s.blocks[0].end {
		headerEnd = withRecords
	}
	start := int64(binary.LittleEndian.Uint32(offsets[:4]))
	for i := 1; i <= count; i++ {
		end := int64(binary.LittleEndian.Uint32(offsets[4*i : 4*(i+1)]))
		if start < headerEnd || end <= start || end > tail || end-start > maxCompressedFrame {
			return fmt.Errorf("%w: frame %d range overlaps a descriptor, trailer or another frame, or exceeds its budget", ErrInvalidProfile, i-1)
		}
		s.frames = append(s.frames, extent{start, end})
		start = end
	}
	return ctx.Err()
}

func checkMetadata(dataset *object.Object, expected pixeldata.Metadata) error {
	for _, entry := range []struct {
		tag  core.Tag
		want int64
	}{
		{tags.Rows, int64(expected.Rows)}, {tags.Columns, int64(expected.Columns)},
		{tags.NumberOfFrames, int64(expected.NumberOfFrames)}, {tags.SamplesPerPixel, 1},
		{tags.BitsAllocated, int64(expected.BitsAllocated)}, {tags.BitsStored, int64(expected.BitsStored)},
		{tags.HighBit, int64(expected.HighBit)}, {tags.PixelRepresentation, 0},
	} {
		if elem, present := dataset.Get(entry.tag); present {
			vr := core.VRUS
			if entry.tag == tags.NumberOfFrames {
				vr = core.VRIS
			}
			values, err := dataset.GetInts(entry.tag)
			if elem.VR() != vr || err != nil || len(values) != 1 || values[0] != entry.want || dataset.HasDuplicateParsedTag(entry.tag) {
				return fmt.Errorf("%w: %s contradicts the private descriptor", ErrInvalidProfile, entry.tag)
			}
		}
	}
	if elem, present := dataset.Get(tags.PhotometricInterpretation); present {
		values, ok := dataset.GetStrings(tags.PhotometricInterpretation)
		if elem.VR() != core.VRCS || !ok || len(values) != 1 || values[0] != expected.PhotometricInterpretation || dataset.HasDuplicateParsedTag(tags.PhotometricInterpretation) {
			return fmt.Errorf("%w: photometric interpretation contradicts the private descriptor", ErrInvalidProfile)
		}
	}
	return nil
}

func nativeFrameDataset(m pixeldata.Metadata) *object.Object {
	dataset := object.New(nil)
	dataset.SetUS(tags.Rows, m.Rows)
	dataset.SetUS(tags.Columns, m.Columns)
	dataset.SetUS(tags.SamplesPerPixel, 1)
	dataset.SetUS(tags.BitsAllocated, 16)
	dataset.SetUS(tags.BitsStored, m.BitsStored)
	dataset.SetUS(tags.HighBit, m.HighBit)
	dataset.SetUS(tags.PixelRepresentation, 0)
	// This CS literal is representable in the default character repertoire.
	_ = dataset.SetStrings(tags.PhotometricInterpretation, core.VRCS, m.PhotometricInterpretation)
	return dataset
}

// Metadata returns the verified descriptor's canonical unsigned mono layout.
func (s *Source) Metadata() pixeldata.Metadata { return s.metadata }

// Frame reconstructs the observed JPEG-LS wrapper for one checked entropy
// range and returns its detached little-endian stored samples. The ordinary
// JPEG-LS decoder validates the complete codestream before returning pixels.
func (s *Source) Frame(ctx context.Context, index int) ([]byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if index < 0 || index >= len(s.frames) {
		return nil, fmt.Errorf("%w: invalid frame index %d", ErrInvalidProfile, index)
	}
	span := s.frames[index]
	// Private ranges hold a single component's entropy with no JPEG
	// markers. The reconstructed T.87 codestream uses no restart or LSE.
	header := [25]byte{
		0xff, 0xd8, 0xff, 0xf7, 0, 11, byte(s.metadata.BitsStored), 0, 0, 0, 0, 1, 1, 0x11, 0,
		0xff, 0xda, 0, 8, 1, 1, 0, 0, 0, 0,
	}
	binary.BigEndian.PutUint16(header[7:9], s.metadata.Rows)
	binary.BigEndian.PutUint16(header[9:11], s.metadata.Columns)
	stream := make([]byte, len(header)+int(span.end-span.start)+2)
	copy(stream, header[:])
	entropy := stream[len(header) : len(stream)-2]
	select {
	case s.gate <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	err := s.readAt(ctx, span.start, entropy)
	<-s.gate
	if err != nil {
		return nil, err
	}
	if err := checkEntropy(ctx, entropy); err != nil {
		return nil, err
	}
	copy(stream[len(stream)-2:], []byte{0xff, 0xd9})
	result, err := s.decoder.DecodeContext(ctx, pixeldata.PixelData{
		Encapsulated: true, Sequence: core.FragmentSequence{Fragments: [][]byte{stream}},
	}, s.frameDS)
	if err != nil {
		return nil, fmt.Errorf("hologic: decode frame %d: %w", index, err)
	}
	return result.Data[0], nil
}

func checkEntropy(ctx context.Context, data []byte) error {
	const chunk = 32 << 10
	for start := 0; start < len(data); {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := min(start+chunk, len(data))
		for start < end {
			n := bytes.IndexByte(data[start:end], 0xff)
			if n < 0 {
				break
			}
			position := start + n + 1
			if position >= len(data) || data[position]&0x80 != 0 {
				return fmt.Errorf("%w: a private entropy range contains a JPEG marker", ErrInvalidProfile)
			}
			start = position + 1
		}
		start = max(start, end)
	}
	return ctx.Err()
}

func (s *Source) readAt(ctx context.Context, start int64, dst []byte) error {
	i := sort.Search(len(s.blocks), func(i int) bool { return s.blocks[i].end > start })
	for len(dst) > 0 && i < len(s.blocks) {
		if err := ctx.Err(); err != nil {
			return err
		}
		b := s.blocks[i]
		var blockStart int64
		if i > 0 {
			blockStart = s.blocks[i-1].end
		}
		n := min(int64(len(dst)), b.end-start)
		offset := start - blockStart
		if b.data != nil {
			copy(dst[:n], b.data[offset:offset+n])
		} else {
			writer := rangeWriter{remaining: dst[:n]}
			if _, err := s.dataset.CopyBulkDataRangeTo(ctx, b.path[:], offset, n, &writer); err != nil {
				return fmt.Errorf("hologic: replay private block: %w", err)
			}
			if len(writer.remaining) != 0 {
				return io.ErrUnexpectedEOF
			}
		}
		dst, start, i = dst[n:], start+n, i+1
	}
	if len(dst) != 0 {
		return io.ErrUnexpectedEOF
	}
	return ctx.Err()
}

type rangeWriter struct{ remaining []byte }

func (w *rangeWriter) Write(p []byte) (int, error) {
	n := copy(w.remaining, p)
	w.remaining = w.remaining[n:]
	if n < len(p) {
		return n, io.ErrShortWrite
	}
	return n, nil
}
