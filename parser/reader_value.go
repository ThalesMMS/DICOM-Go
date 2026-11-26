package parser

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/packedbits"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
)

func (r *Reader) readDefinedValueToken(header core.ElementHeader) (Token, error) {
	if r.encapsulatedSink != nil && isPixelDataValueTag(header.Tag) {
		return Token{}, fmt.Errorf("%w: encoded frame streaming requires encapsulated integer Pixel Data", ErrFrameSink)
	}
	if r.encapsulatedSink != nil && len(r.seqDelimiters) == 0 && isExtendedTable(header.Tag) {
		if err := r.encapsulatedSink.CheckExtendedTable(header); err != nil {
			return Token{}, err
		}
	}
	if err := r.validateDefinedValueLength(header); err != nil {
		return Token{}, err
	}
	if err := r.checkValueFits(header); err != nil {
		return Token{}, err
	}
	if err := r.checkElementCountLimit(header); err != nil {
		return Token{}, err
	}
	if r.stopBeforePixelData && header.Tag == core.TagPixelData {
		r.elementCount++
		return Token{
			Kind:   TokenElement,
			Header: header,
			Element: core.Element{
				Header: header,
				Value:  nil,
			},
		}, nil
	}
	if !(r.allowIncompletePixelData && header.Tag.IsItem() && len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence) {
		if err := r.checkElementByteLimit(header); err != nil {
			return Token{}, err
		}
	}
	valueOffset := r.Position()
	if r.allowIncompletePixelData && len(r.seqDelimiters) == 0 && header.Tag == core.TagPixelData && !r.syntax.Encapsulated && (header.VR == core.VROW || header.VR == core.VROB || header.VR == core.VRUN) {
		return r.readIncompleteNativeToken(header, valueOffset)
	}
	if r.frameSink != nil && !r.frameSinkClosed && len(r.seqDelimiters) == 0 && header.Tag == core.TagPixelData && !r.syntax.Encapsulated {
		return r.readNativePixelDataFramesToken(header, valueOffset)
	}
	// Deferral replays top-level values only; nested native Pixel Data such as
	// an Icon Image Sequence item has no item-scoped replay, so it stays
	// materialized instead of becoming unreadable. Skip applies at any depth.
	if (r.skipPixelData || (r.deferPixelData && len(r.seqDelimiters) == 0)) && isPixelDataValueTag(header.Tag) {
		if err := r.skipN(valueOffset, header, int64(header.Length)); err != nil {
			return Token{}, err
		}
		r.recordValueLocation(header, valueOffset, int64(header.Length))
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{
			Kind:   TokenElement,
			Header: header,
			Element: core.Element{
				Header: header,
				Value:  nil,
			},
		}, nil
	}
	// Projection keeps every normal header/value-bound check above. Without
	// frame sinks, only private creators have value-dependent parser semantics;
	// they continue through ordinary materialization and reservation capture.
	if r.projectionDiscardsValue(header) {
		if err := r.skipN(valueOffset, header, int64(header.Length)); err != nil {
			return Token{}, err
		}
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{Kind: TokenElement, Header: header, Element: core.Element{Header: header, Value: core.DiscardedValue{}}}, nil
	}
	if r.deferWaveformData && header.Tag == tagWaveformData {
		if err := r.skipN(valueOffset, header, int64(header.Length)); err != nil {
			return Token{}, err
		}
		r.recordValueLocation(header, valueOffset, int64(header.Length))
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{
			Kind:   TokenElement,
			Header: header,
			Element: core.Element{
				Header: header,
				Value:  nil,
			},
		}, nil
	}
	if r.deferBulkValue(header) {
		if err := r.skipN(valueOffset, header, int64(header.Length)); err != nil {
			return Token{}, err
		}
		r.recordValueLocation(header, valueOffset, int64(header.Length))
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{Kind: TokenElement, Header: header, Element: core.Element{Header: header}}, nil
	}
	if r.deferLargeValue(header) {
		// Large defined-length byte-blob value (OB, OW, OF, OD or UN, native
		// Pixel Data included): consumers stream its raw bytes from the source.
		// Values of other VRs over the threshold are materialized below.
		if err := r.skipN(valueOffset, header, int64(header.Length)); err != nil {
			return Token{}, err
		}
		r.recordValueLocation(header, valueOffset, int64(header.Length))
		r.elementCount++
		r.delimiterCheckPending = true
		return Token{
			Kind:   TokenElement,
			Header: header,
			Element: core.Element{
				Header: header,
				Value:  nil,
			},
		}, nil
	}
	data, err := r.readDefinedValueBytes(header, valueOffset)
	if err != nil {
		return Token{}, err
	}
	data = r.toStreamByteOrder(header.VR, data)
	if r.encapsulatedSink != nil && len(r.seqDelimiters) == 0 && isExtendedTable(header.Tag) {
		if err := r.encapsulatedSink.ExtendedTable(core.Element{Header: header, Value: core.RawValue(data)}); err != nil {
			return Token{}, err
		}
	}
	if header.VR == core.VRLO {
		if err := r.capturePrivateReservation(core.Element{Header: header, Value: core.RawValue(data)}); err != nil {
			return Token{}, err
		}
	}
	if err := r.captureFrameMetadata(header, data); err != nil {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: valueOffset,
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    err,
		}
	}
	r.elementCount++
	r.delimiterCheckPending = true
	return Token{
		Kind:   TokenElement,
		Header: header,
		Element: core.Element{
			Header: header,
			Value:  core.RawValue(data),
		},
	}, nil
}

func (r *Reader) readDefinedValueBytes(header core.ElementHeader, valueOffset int64) ([]byte, error) {
	if r.allowIncompletePixelData && header.Tag.IsItem() && len(r.seqDelimiters) > 0 && r.seqDelimiters[len(r.seqDelimiters)-1].typ == seqTokenTypePixelSequence {
		return r.readIncompleteItemBytes(header, valueOffset)
	}
	length := int64(header.Length)
	if length == 0 {
		return []byte{}, nil
	}
	if r.slurpEligible {
		r.ensureSlurp()
	}
	maxInt := int64(^uint(0) >> 1)
	if src, ok := r.counter.r.(*sliceSource); ok && length <= maxInt && len(r.counter.pending) == 0 {
		if data, ok := src.take(int(length)); ok {
			r.counter.pos += int64(len(data))
			return data, nil
		}
		// Fewer bytes remain than the declared length; fall through so the
		// streaming paths produce the same truncation errors as any source.
	}
	if length <= definedValueReadChunkSize {
		data := r.allocateDefinedValueBytes(int(length))
		if _, err := io.ReadFull(r.counter, data); err != nil {
			return nil, r.valueReadError(valueOffset, header, err)
		}
		return data, nil
	}
	data, err := r.appendValueBytes(nil, length)
	if err != nil {
		return nil, r.valueReadError(valueOffset, header, err)
	}
	return data, nil
}

// appendValueBytes appends n bytes read from the source to buf. When the
// source's known remaining size holds them, the final buffer is allocated
// once. Otherwise the buffer grows geometrically behind the bytes actually
// received, capped at len(buf)+n, so a declared length that the source never
// delivers is not reserved, whether or not a configured limit admitted it.
// On error buf holds the bytes read so far.
func (r *Reader) appendValueBytes(buf []byte, n int64) ([]byte, error) {
	if n <= 0 {
		return buf, nil
	}
	want := int64(len(buf)) + n
	if want < n || want > int64(^uint(0)>>1) {
		return buf, fmt.Errorf("%w: %d-byte value exceeds addressable memory", ErrMaxElementBytesExceeded, n)
	}
	if int64(cap(buf)) < want && r.sourceEnd > 0 && n <= r.sourceEnd-r.Position() {
		buf = growValueBuffer(buf, want)
	}
	for n > 0 {
		if free := int64(cap(buf) - len(buf)); free < min(n, definedValueReadChunkSize) {
			buf = growValueBuffer(buf, min(want, max(2*int64(cap(buf)), int64(len(buf))+definedValueReadChunkSize)))
		}
		// Fill the free capacity, which only grows after it was filled.
		step := min(n, int64(cap(buf)-len(buf)))
		got, err := io.ReadFull(r.counter, buf[len(buf):len(buf)+int(step)])
		buf = buf[:len(buf)+got]
		n -= int64(got)
		if err != nil {
			return buf, err
		}
	}
	return buf, nil
}

func growValueBuffer(buf []byte, capacity int64) []byte {
	grown := make([]byte, len(buf), int(capacity))
	copy(grown, buf)
	return grown
}

func (r *Reader) allocateDefinedValueBytes(length int) []byte {
	if length <= 0 {
		return []byte{}
	}
	if length > smallValueArenaLimit {
		return make([]byte, length)
	}
	if cap(r.smallValueArena)-len(r.smallValueArena) < length {
		blockSize := smallValueArenaBlockBytes
		if cap(r.smallValueArena) == 0 {
			blockSize = smallValueArenaInitialBytes
		}
		r.smallValueArena = make([]byte, 0, blockSize)
	}
	start := len(r.smallValueArena)
	r.smallValueArena = r.smallValueArena[:start+length]
	return r.smallValueArena[start : start+length : start+length]
}

func (r *Reader) valueReadError(valueOffset int64, header core.ElementHeader, err error) error {
	if errors.Is(err, io.EOF) {
		err = io.ErrUnexpectedEOF
	} else {
		err = normalizeReadError(valueOffset, r.Position(), err)
	}
	return &ParseError{
		Op:     OpReadValue,
		Offset: valueOffset,
		Tag:    header.Tag,
		VR:     header.VR,
		Length: header.Length,
		Err:    err,
	}
}

func (r *Reader) readNativePixelDataFramesToken(header core.ElementHeader, valueOffset int64) (Token, error) {
	metadata, err := r.frameMetadata.complete()
	if err != nil {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: valueOffset,
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    err,
		}
	}

	frameSize := metadata.FrameSize()
	totalSize := metadata.nativeValueSize()
	maxInt := int64(^uint(0) >> 1)
	if frameSize <= 0 || totalSize <= 0 || frameSize > maxInt || totalSize > maxInt || metadata.TotalSize() <= 0 {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: valueOffset,
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err: fmt.Errorf(
				"%w: rows=%d columns=%d samples_per_pixel=%d bits_allocated=%d number_of_frames=%d",
				ErrInvalidFrameMetadata,
				metadata.Rows,
				metadata.Columns,
				metadata.SamplesPerPixel,
				metadata.BitsAllocated,
				metadata.NumberOfFrames,
			),
		}
	}
	encodedSize := int64(header.Length)
	hasPadding := encodedSize == totalSize+1 && totalSize%2 == 1
	if encodedSize != totalSize && !hasPadding {
		return Token{}, &ParseError{
			Op:     OpReadValue,
			Offset: valueOffset,
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err: fmt.Errorf(
				"%w: expected %d bytes for %d frame(s), got %d",
				ErrFrameDataSizeMismatch,
				totalSize,
				metadata.NumberOfFrames,
				header.Length,
			),
		}
	}

	if metadata.unalignedPackedFrames() {
		if err := r.readPackedNativeFrames(header, valueOffset, metadata); err != nil {
			return Token{}, err
		}
	} else {
		for i := 0; i < metadata.NumberOfFrames; i++ {
			// Each frame buffer grows behind the bytes received, so a forged
			// frame size is not reserved before its bytes arrive.
			data, err := r.appendValueBytes(nil, frameSize)
			if err != nil {
				return Token{}, &ParseError{
					Op:     OpReadValue,
					Offset: valueOffset + int64(i)*frameSize,
					Tag:    header.Tag,
					VR:     header.VR,
					Length: header.Length,
					Err:    requiredReadError(err),
				}
			}
			if err := r.deliverFrame(header, metadata, i, data); err != nil {
				return Token{}, err
			}
		}
	}
	if hasPadding {
		var pad [1]byte
		padOffset := valueOffset + totalSize
		if _, err := io.ReadFull(r.counter, pad[:]); err != nil {
			return Token{}, &ParseError{
				Op:     OpReadValue,
				Offset: padOffset,
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    requiredReadError(err),
			}
		}
		if pad[0] != 0 {
			return Token{}, &ParseError{
				Op:     OpReadValue,
				Offset: padOffset,
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err: fmt.Errorf(
					"%w: expected zero padding byte after %d byte payload, got 0x%02X",
					ErrFrameDataSizeMismatch,
					totalSize,
					pad[0],
				),
			}
		}
	}

	r.recordValueLocation(header, valueOffset, int64(header.Length))
	r.elementCount++
	r.delimiterCheckPending = true
	return Token{
		Kind:   TokenElement,
		Header: header,
		Element: core.Element{
			Header: header,
			Value:  nil,
		},
	}, nil
}

// readPackedNativeFrames splits Bits Allocated 1 frames that do not start on
// byte boundaries. Each frame's bits are read from the stored bit stream,
// including the byte shared with the previous frame, and shifted to bit zero
// of a zeroed FrameSize buffer, as pixeldata splits a materialized value.
func (r *Reader) readPackedNativeFrames(header core.ElementHeader, valueOffset int64, metadata FrameMetadata) error {
	frameBits, frameSize := metadata.packedFrameBits(), metadata.FrameSize()
	// wire holds one frame's stored bytes, at most FrameSize+1 when the frame
	// starts mid-byte. It grows behind the bytes received and is then reused.
	var wire []byte
	var consumed int64 // stored bytes read so far
	var shared byte    // last stored byte read, holding bits of the next frame
	for i := 0; i < metadata.NumberOfFrames; i++ {
		startBit := int64(i) * frameBits
		first, end := startBit/8, packedbits.Bytes(startBit+frameBits)
		wire = wire[:0]
		if consumed > first {
			wire = append(wire, shared)
		}
		var err error
		if wire, err = r.appendValueBytes(wire, end-consumed); err != nil {
			return &ParseError{
				Op:     OpReadValue,
				Offset: valueOffset + consumed,
				Tag:    header.Tag,
				VR:     header.VR,
				Length: header.Length,
				Err:    requiredReadError(err),
			}
		}
		consumed, shared = end, wire[len(wire)-1]
		data := make([]byte, int(frameSize))
		packedbits.Copy(data, 0, wire, startBit%8, frameBits)
		if err := r.deliverFrame(header, metadata, i, data); err != nil {
			return err
		}
	}
	return nil
}

func (r *Reader) deliverFrame(header core.ElementHeader, metadata FrameMetadata, index int, data []byte) error {
	if err := r.frameSink.HandleFrame(Frame{
		Index:          index,
		Data:           data,
		Metadata:       metadata,
		TransferSyntax: r.syntax,
	}); err != nil {
		return &ParseError{
			Op:     OpReadValue,
			Offset: r.Position(),
			Tag:    header.Tag,
			VR:     header.VR,
			Length: header.Length,
			Err:    fmt.Errorf("%w: frame %d: %w", ErrFrameSink, index, err),
		}
	}
	return nil
}

func (r *Reader) closeFrameSink(readErr error) error {
	if r == nil || r.frameSink == nil || r.frameSinkClosed {
		return readErr
	}
	r.frameSinkClosed = true
	closeErr := r.frameSink.Close()
	if readErr == nil {
		return closeErr
	}
	if closeErr == nil {
		return readErr
	}
	return errors.Join(readErr, closeErr)
}

func (t seqTokenType) String() string {
	switch t {
	case seqTokenTypeSequence:
		return "sequence"
	case seqTokenTypePixelSequence:
		return "pixel sequence"
	case seqTokenTypeItem:
		return "item"
	default:
		return "unknown"
	}
}

// toStreamByteOrder converts a value read inside an undefined-length UN
// sequence, which PS3.5 6.2.2 encodes as Implicit VR Little Endian whatever the
// transfer syntax, to the stream's byte order, so every raw value of a Big
// Endian data set has one byte order. Byte-string VRs, including UN itself,
// are never reversed. data may alias the source and is copied before reversal.
func (r *Reader) toStreamByteOrder(vr core.VR, data []byte) []byte {
	// Values of an item opened by a byte-swapped Item tag are Big Endian in
	// a Little Endian stream (RecoverDelimiterAnomalies).
	swapped := r.explicitBigEndianActive() && r.syntax.IsLittleEndian()
	if !swapped && (!r.implicitVRLittleEndianActive() || r.syntax.ByteOrder != binary.BigEndian) {
		return data
	}
	width := valueencode.ByteOrderWidth(vr)
	if width <= 1 || len(data)%width != 0 {
		return data
	}
	return valueencode.ReverseFixedWidth(data, width)
}
