package seg

import (
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"reflect"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/roi"
)

// FramePayload is borrowed until the next Frame call. Frame geometry and
// references are supplied by the matching Document.Frames metadata instead.
type FramePayload struct {
	Mask       *roi.RasterMask
	Fractional []uint8
	LabelMap   []uint16
}

// FrameSource is serial and replayable. Payload and Document metadata must
// remain unchanged until WriteFrames returns. Sources own their input memory
// and must unblock on cancellation; the writer neither closes nor retains it.
type FrameSource interface {
	Frame(context.Context, int) (FramePayload, error)
}

// StreamWriteOptions has no unlimited defaults. Pixel buffering reserves one
// encoded frame, two packed presence planes and the frame writer's 32 KiB
// concatenation buffer. Source payload storage remains caller-owned. Metadata
// is admitted separately with a conservative structural/text reservation;
// per-frame functional groups remain O(frame count), bounded by both caps.
// Frame reads bound validation, exact overlap comparison and final emission.
type StreamWriteOptions struct {
	MaxFrames           int
	MaxFrameReads       int64
	MaxPixelBufferBytes int64
	MaxMetadataBytes    int64
}

type StreamWriteResult struct {
	Frames                   int
	FrameReads               int64
	PixelBufferBytes         int64
	MetadataReservationBytes int64
}

type documentFrames struct{ document *Document }

func (s documentFrames) Frame(ctx context.Context, i int) (FramePayload, error) {
	if err := ctx.Err(); err != nil {
		return FramePayload{}, err
	}
	if s.document == nil || i < 0 || i >= len(s.document.Frames) {
		return FramePayload{}, ErrInvalidObject
	}
	f := s.document.Frames[i]
	return FramePayload{Mask: f.Mask, Fractional: f.Fractional, LabelMap: f.LabelMap}, nil
}

// DocumentFrames borrows an immutable in-memory document without cloning its
// masks/samples. It adapts existing callers to the same incremental serializer.
func DocumentFrames(doc *Document) FrameSource { return documentFrames{doc} }

// WriteFrames serializes complete SEG metadata plus incremental native pixels
// through object.WriteFileFrames. BINARY bits continue across frame boundaries.
// It preserves Write's exact overlap attribute using bounded presence buffers
// and replay of pairs at a shared position; worst-case overlap work is quadratic
// within such a position and refuses the read cap rather than buffering masks.
// A writer/source error can leave partial bytes in w. Publication is the
// caller's responsibility: write to staging and publish only after success.
func WriteFrames(ctx context.Context, destination io.Writer, doc *Document, source FrameSource, limits StreamWriteOptions) (StreamWriteResult, error) {
	var result StreamWriteResult
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if doc == nil || source == nil || destination == nil || limits.MaxFrames <= 0 || limits.MaxFrames > MaxReadFrames || limits.MaxFrameReads <= 0 || limits.MaxPixelBufferBytes <= 0 || limits.MaxMetadataBytes <= 0 {
		return result, fmt.Errorf("%w: source, destination and finite stream budgets required", ErrResourceLimitExceeded)
	}
	if err := validateWriteDimensions(doc); err != nil {
		return result, err
	}
	if len(doc.Frames) > limits.MaxFrames || int64(len(doc.Frames)) > limits.MaxFrameReads/2 {
		return result, ErrResourceLimitExceeded
	}
	reservation, err := streamMetadataReservation(doc, limits.MaxMetadataBytes)
	if err != nil {
		return result, err
	}
	result.MetadataReservationBytes = reservation
	header := *doc
	header.Frames = make([]Frame, len(doc.Frames))
	for i, frame := range doc.Frames {
		frame.Mask = nil
		frame.Fractional = nil
		frame.LabelMap = nil
		header.Frames[i] = frame
	}
	class := header.SOPClassUID
	if class == "" {
		class = SegmentationStorage
	}
	if err := validateRepresentableSegmentationType(class, header.SegmentationType); err != nil {
		return result, err
	}
	if err := validateUnsignedShortFields(&header); err != nil {
		return result, err
	}
	pixels := int64(doc.Rows) * int64(doc.Columns)
	packedBytes := (pixels + 7) / 8
	bits := 1
	switch {
	case class == LabelMapSegmentationStorage:
		bits, err = labelMapBitsAllocated(&header)
		if err != nil {
			return result, err
		}
	case segmentationType(&header) == SegmentationTypeFractional:
		bits = 8
	}
	frameBytes := (pixels*int64(bits) + 7) / 8
	result.PixelBufferBytes = frameBytes + 2*packedBytes + (32 << 10)
	if result.PixelBufferBytes > limits.MaxPixelBufferBytes {
		return result, ErrResourceLimitExceeded
	}
	stream := &segFrameStream{ctx: ctx, header: &header, source: source, class: class, bits: bits, buffer: make([]byte, int(frameBytes)), maxReads: limits.MaxFrameReads}
	for i := range header.Frames {
		if _, err := stream.read(i); err != nil {
			result.FrameReads = stream.reads
			return result, err
		}
	}
	overlap := SegmentsOverlapNo
	if class != LabelMapSegmentationStorage {
		overlap, err = stream.overlap(int(packedBytes))
		if err != nil {
			result.FrameReads = stream.reads
			return result, err
		}
	}
	file, err := writeDocument(&header, false, overlap)
	if err != nil {
		result.FrameReads = stream.reads
		return result, err
	}
	defer file.Close()
	var frames object.FrameSource = stream
	if bits == 1 {
		frames = packedSEGStream{stream}
	}
	err = object.WriteFileFrames(segContextWriter{ctx, destination}, file, frames, object.FrameWriteOptions{})
	result.FrameReads = stream.reads
	if err == nil {
		err = ctx.Err()
	}
	if err == nil {
		result.Frames = len(header.Frames)
	}
	return result, err
}

type segFrameStream struct {
	ctx             context.Context
	header          *Document
	source          FrameSource
	class           string
	bits            int
	buffer          []byte
	reads, maxReads int64
}

func (s *segFrameStream) read(i int) (Frame, error) {
	if err := s.ctx.Err(); err != nil {
		return Frame{}, err
	}
	if s.reads >= s.maxReads {
		return Frame{}, ErrResourceLimitExceeded
	}
	s.reads++
	payload, err := s.source.Frame(s.ctx, i)
	if err != nil {
		return Frame{}, err
	}
	f := s.header.Frames[i]
	f.Mask, f.Fractional, f.LabelMap = payload.Mask, payload.Fractional, payload.LabelMap
	check := *s.header
	check.Frames = []Frame{f}
	if err := validateFramePayloadsContext(s.ctx, &check, s.class); err != nil {
		return Frame{}, err
	}
	if s.class == LabelMapSegmentationStorage && s.bits == 8 {
		for _, value := range f.LabelMap {
			if value > 255 {
				return Frame{}, ErrInvalidObject
			}
		}
	}
	return f, s.ctx.Err()
}

func (s *segFrameStream) Frames() int                    { return len(s.header.Frames) }
func (s *segFrameStream) FrameLength(int) (int64, error) { return int64(len(s.buffer)), nil }

type packedSEGStream struct{ *segFrameStream }

func (s packedSEGStream) FrameBits(int) (int64, error) {
	return int64(s.header.Rows) * int64(s.header.Columns), nil
}

func (s *segFrameStream) WriteFrameTo(i int, destination io.Writer) (int64, error) {
	f, err := s.read(i)
	if err != nil {
		return 0, err
	}
	clear(s.buffer)
	switch s.bits {
	case 1:
		if err := s.presence(f, s.buffer); err != nil {
			return 0, err
		}
	case 8:
		if s.class == LabelMapSegmentationStorage {
			background := byte(s.header.LabelMapBackground)
			for j := range s.buffer {
				if j%4096 == 0 {
					if err := s.ctx.Err(); err != nil {
						return 0, err
					}
				}
				// A short frame is padded with the background.
				value := background
				if j < len(f.LabelMap) {
					value = byte(f.LabelMap[j])
				}
				s.buffer[j] = value
			}
		} else {
			copy(s.buffer, f.Fractional)
		}
	case 16:
		for j := 0; j < len(s.buffer)/2; j++ {
			if j%4096 == 0 {
				if err := s.ctx.Err(); err != nil {
					return 0, err
				}
			}
			value := s.header.LabelMapBackground
			if j < len(f.LabelMap) {
				value = f.LabelMap[j]
			}
			binary.LittleEndian.PutUint16(s.buffer[2*j:], value)
		}
	}
	var total int64
	for remaining := s.buffer; len(remaining) > 0; {
		if err := s.ctx.Err(); err != nil {
			return total, err
		}
		chunk := remaining[:min(len(remaining), 32<<10)]
		n, err := destination.Write(chunk)
		total += int64(n)
		if err == nil && n != len(chunk) {
			err = io.ErrShortWrite
		}
		if err != nil {
			return total, err
		}
		remaining = remaining[n:]
	}
	return total, nil
}

func (s *segFrameStream) presence(frame Frame, buffer []byte) error {
	clear(buffer)
	if segmentationType(s.header) == SegmentationTypeFractional {
		for i, value := range frame.Fractional {
			if i%4096 == 0 {
				if err := s.ctx.Err(); err != nil {
					return err
				}
			}
			if value > 0 {
				buffer[i/8] |= 1 << uint(i%8)
			}
		}
	} else if frame.Mask != nil {
		for y := 0; y < frame.Mask.Rows; y++ {
			if err := s.ctx.Err(); err != nil {
				return err
			}
			for _, run := range frame.Mask.Runs(y) {
				setPackedBitRange(buffer, y*s.header.Columns+run.Start, y*s.header.Columns+run.End)
			}
		}
	}
	return s.ctx.Err()
}

func (s *segFrameStream) overlap(bytes int) (string, error) {
	groups := make(map[positionKey][]int, len(s.header.Frames))
	order := make([]positionKey, 0, len(s.header.Frames))
	for i, f := range s.header.Frames {
		key := framePosition(f)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], i)
	}
	a, b := make([]byte, bytes), make([]byte, bytes)
	for _, key := range order {
		if err := s.ctx.Err(); err != nil {
			return "", err
		}
		group := groups[key]
		segments := make(map[int]bool)
		for _, index := range group {
			segments[s.header.Frames[index].SegmentNumber] = true
		}
		if len(segments) < 2 {
			continue
		}
		for offset, i := range group {
			if err := s.ctx.Err(); err != nil {
				return "", err
			}
			possible := false
			for _, j := range group[offset+1:] {
				if s.header.Frames[i].SegmentNumber != s.header.Frames[j].SegmentNumber {
					possible = true
					break
				}
			}
			if !possible {
				continue
			}
			first, err := s.read(i)
			if err != nil {
				return "", err
			}
			if err := s.presence(first, a); err != nil {
				return "", err
			}
			for _, j := range group[offset+1:] {
				if s.header.Frames[i].SegmentNumber == s.header.Frames[j].SegmentNumber {
					continue
				}
				second, err := s.read(j)
				if err != nil {
					return "", err
				}
				if err := s.presence(second, b); err != nil {
					return "", err
				}
				for k := range a {
					if k%4096 == 0 {
						if err := s.ctx.Err(); err != nil {
							return "", err
						}
					}
					if a[k]&b[k] != 0 {
						return SegmentsOverlapYes, nil
					}
				}
			}
		}
	}
	return SegmentsOverlapNo, nil
}

type segContextWriter struct {
	ctx         context.Context
	destination io.Writer
}

func (w segContextWriter) Write(data []byte) (int, error) {
	if err := w.ctx.Err(); err != nil {
		return 0, err
	}
	return w.destination.Write(data)
}

func streamMetadataReservation(doc *Document, limit int64) (int64, error) {
	// The fixed reservation covers frame headers, reference indexes, overlap
	// position lists, functional groups and the frame writer's length table.
	used := int64(64<<10) + int64(len(doc.Frames))*(32<<10)
	add := func(n int64) bool {
		if n < 0 || used > limit || n > (limit-used)/16 {
			return false
		}
		used += n * 16
		return true
	}
	var text func(reflect.Value) bool
	text = func(v reflect.Value) bool {
		switch v.Kind() {
		case reflect.String:
			return add(int64(v.Len()))
		case reflect.Struct:
			for i := 0; i < v.NumField(); i++ {
				if !text(v.Field(i)) {
					return false
				}
			}
		case reflect.Slice:
			if v.Type().Elem().Kind() == reflect.String {
				for i := 0; i < v.Len(); i++ {
					if !text(v.Index(i)) {
						return false
					}
				}
			}
		}
		return true
	}
	if used > limit || !text(reflect.ValueOf(*doc)) {
		return 0, ErrResourceLimitExceeded
	}
	if doc.Common.Equipment != nil && !text(reflect.ValueOf(*doc.Common.Equipment)) {
		return 0, ErrResourceLimitExceeded
	}
	for _, segment := range doc.Segments {
		if !add(4096) || !text(reflect.ValueOf(segment)) {
			return 0, ErrResourceLimitExceeded
		}
	}
	for _, ref := range doc.ReferencedImages {
		if !add(4096+int64(len(ref.Frames))*8) || !text(reflect.ValueOf(ref)) {
			return 0, ErrResourceLimitExceeded
		}
	}
	for _, frame := range doc.Frames {
		if !add(int64(len(frame.ReferencedSOPInstanceUID))) {
			return 0, ErrResourceLimitExceeded
		}
	}
	if doc.Common.Source != nil {
		nodes := 0
		var walk func([]core.Element, int) bool
		walk = func(elements []core.Element, depth int) bool {
			if depth > 32 {
				return false
			}
			for _, element := range elements {
				if element.Tag() == core.TagPixelData {
					continue
				}
				nodes++
				if nodes > 100000 || !add(256) {
					return false
				}
				switch value := element.Value.(type) {
				case nil:
					if element.Header.LengthSet && element.Header.Length != core.UndefinedLength && !add(int64(element.Header.Length)) {
						return false
					}
				case core.RawValue:
					if !add(int64(len(value))) {
						return false
					}
				case core.StringValue:
					for _, item := range value {
						if !add(int64(len(item))) {
							return false
						}
					}
				case core.SequenceValue:
					for _, item := range value.Items {
						if !walk(item.Elements, depth+1) {
							return false
						}
					}
				default:
					if length, known := element.Value.EncodedLength(); known {
						if !add(int64(length)) {
							return false
						}
					}
				}
			}
			return true
		}
		if !walk(doc.Common.Source.Elements(), 0) {
			return 0, ErrResourceLimitExceeded
		}
	}
	return used, nil
}
