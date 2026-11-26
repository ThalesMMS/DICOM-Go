package dicomxml

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/valueencode"
	"github.com/ThalesMMS/dicom-go/object"
)

// binaryOrNumeric writes directly from a Value Field. InlineBinary uses a
// fixed-size base64 encoder, including when the source is deferred or composed
// of fragments. The legacy BulkDataURIFunc alone requires a complete []byte.
func (e *nativeEncoder) binaryOrNumeric(obj *object.Object, elem core.Element, path string) error {
	if width := xmlNumericWidth(elem.VR()); width != 0 {
		writer := &xmlNumericWriter{encoder: e, vr: elem.VR(), width: width}
		if err := e.copyValue(obj, elem, writer); err != nil {
			return err
		}
		if writer.pending != 0 {
			return fmt.Errorf("%w: %s raw numeric length is not a multiple of %d", ErrInvalidXML, elem.Tag(), width)
		}
		return nil
	}
	length, known, err := xmlBinaryLength(elem)
	if err != nil {
		return err
	}
	if fragments, ok := elem.Value.(core.FragmentSequence); ok {
		if len(fragments.Fragments) > e.b.limits.MaxSequenceItems-e.b.items {
			return ErrMaxItemsExceeded
		}
		e.b.items += len(fragments.Fragments)
	}
	if known && length == 0 {
		return nil
	}
	// PS3.19 InlineBinary and BulkData of OW, OF, OD, OL and OV are Little
	// Endian, whatever the byte order of the source object.
	width := e.encodeLittleEndianWidth(obj, elem)
	var raw []byte
	if e.opts.BulkDataURIFunc != nil {
		// The callback may offload any value, so the inline limits apply only
		// once it keeps the value inline.
		fresh := false
		if value, ok := elem.RawBytes(); ok {
			raw = value
		} else {
			var buf bytes.Buffer
			if err := e.copyValue(obj, elem, &buf); err != nil {
				return err
			}
			raw, fresh = buf.Bytes(), true
		}
		if len(raw) == 0 {
			return nil
		}
		if width > 1 {
			if fresh {
				valueencode.ReverseUnitsInPlace(raw, width)
			} else {
				// The object's own bytes are never modified.
				raw = valueencode.ReverseFixedWidth(raw, width)
			}
		}
		if err := e.b.check(); err != nil {
			return err
		}
		uri := e.opts.BulkDataURIFunc(elem.Tag(), elem.VR(), raw)
		if err := e.b.check(); err != nil {
			return err
		}
		if uri != "" {
			return e.bulk(uri, path)
		}
		length, known = int64(len(raw)), true
	}
	if known {
		// Admit a declared value before copying its first byte from a deferred
		// source. Chunk accounting below also catches sources that overproduce.
		if length > e.b.limits.MaxInlineBinaryBytes {
			return ErrMaxInlineBytesExceeded
		}
		if length > e.b.limits.MaxTotalBinaryBytes-e.b.binary {
			return ErrMaxBinaryBytesExceeded
		}
	}
	text := &xmlInlineWriter{encoder: e}
	encoded := base64.NewEncoder(base64.StdEncoding, text)
	var reverse *xmlReverseWriter
	sink := &xmlBinaryWriter{encoder: e, writer: encoded, expected: length, known: known}
	if raw != nil {
		err = e.writeBytes(sink, raw)
	} else {
		if width > 1 {
			reverse = &xmlReverseWriter{writer: encoded, width: width}
			sink.writer = reverse
		}
		err = e.copyValue(obj, elem, sink)
	}
	if err == nil && reverse != nil {
		err = reverse.Close()
	}
	if err != nil {
		return err
	}
	if known && sink.written != length {
		return fmt.Errorf("%w: incomplete binary Value Field", ErrInvalidXML)
	}
	if err = encoded.Close(); err != nil {
		return err
	}
	if text.started {
		return e.enc.EncodeToken(xml.EndElement{Name: xml.Name{Local: "InlineBinary"}})
	}
	return nil
}

type xmlInlineWriter struct {
	encoder *nativeEncoder
	started bool
}

func (w *xmlInlineWriter) Write(data []byte) (int, error) {
	if err := w.encoder.b.check(); err != nil {
		return 0, err
	}
	if !w.started {
		if err := w.encoder.enc.EncodeToken(xml.StartElement{Name: xml.Name{Local: "InlineBinary"}}); err != nil {
			return 0, err
		}
		w.started = true
	}
	if err := w.encoder.enc.EncodeToken(xml.CharData(data)); err != nil {
		return 0, err
	}
	return len(data), nil
}

type xmlBinaryWriter struct {
	encoder           *nativeEncoder
	writer            io.Writer
	written, expected int64
	known             bool
}

func (w *xmlBinaryWriter) Write(data []byte) (int, error) {
	n := int64(len(data))
	if w.known && n > w.expected-w.written {
		return 0, fmt.Errorf("%w: binary Value Field exceeds declared length", ErrInvalidXML)
	}
	if n > w.encoder.b.limits.MaxInlineBinaryBytes-w.written {
		return 0, ErrMaxInlineBytesExceeded
	}
	if err := w.encoder.b.addBinary(n, false); err != nil {
		return 0, err
	}
	written, err := w.writer.Write(data)
	w.written += int64(written)
	return written, err
}

func xmlBinaryLength(elem core.Element) (int64, bool, error) {
	if raw, ok := elem.RawBytes(); ok {
		return int64(len(raw)), true, nil
	}
	switch value := elem.Value.(type) {
	case core.FragmentSequence:
		if elem.Tag() != core.TagPixelData || elem.VR() != core.VROB && elem.VR() != core.VROW {
			return 0, false, fmt.Errorf("%w: fragments require OB or OW Pixel Data", ErrInvalidXML)
		}
		if len(value.OffsetTable)%4 != 0 || uint64(len(value.OffsetTable)) > math.MaxUint32 {
			return 0, false, fmt.Errorf("%w: invalid Basic Offset Table length", ErrInvalidXML)
		}
		total := int64(16) + int64(len(value.OffsetTable))
		for _, fragment := range value.Fragments {
			length := int64(len(fragment))
			length += length % 2
			if length > math.MaxUint32 || length > math.MaxInt64-total-8 {
				return 0, false, fmt.Errorf("%w: fragment length overflow", ErrInvalidXML)
			}
			total += 8 + length
		}
		return total, true, nil
	case core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue:
		if err := valueencode.ValidateNumeric(elem.VR(), value); err != nil {
			return 0, false, err
		}
		if length, ok := elem.CalculatedLength(); ok {
			return int64(length), true, nil
		}
		return 0, false, fmt.Errorf("%w: numeric Value Field length overflow", ErrInvalidXML)
	case nil:
		if elem.Header.HasLength() && elem.EncodedLength() != core.UndefinedLength {
			return int64(elem.EncodedLength()), true, nil
		}
	default:
		if length, ok := elem.CalculatedLength(); ok && length == 0 {
			return 0, true, nil
		}
	}
	return 0, false, nil
}

func (e *nativeEncoder) copyValue(obj *object.Object, elem core.Element, w io.Writer) error {
	if err := e.b.check(); err != nil {
		return err
	}
	if raw, ok := elem.RawBytes(); ok {
		return e.writeBytes(w, raw)
	}
	switch value := elem.Value.(type) {
	case core.Uint16Value, core.Int16Value, core.Uint32Value, core.Int32Value, core.Uint64Value, core.Int64Value, core.Float32Value, core.Float64Value, core.TagValue:
		if err := valueencode.ValidateNumeric(elem.VR(), value); err != nil {
			return err
		}
		if _, ok := elem.CalculatedLength(); !ok {
			return fmt.Errorf("%w: numeric Value Field length overflow", ErrInvalidXML)
		}
		order := e.opts.ByteOrder
		switch v := value.(type) {
		case core.Uint16Value:
			return xmlWriteTyped(e, w, []uint16(v), 2, func(p []byte, v uint16) { order.PutUint16(p, v) })
		case core.Int16Value:
			return xmlWriteTyped(e, w, []int16(v), 2, func(p []byte, v int16) { order.PutUint16(p, uint16(v)) })
		case core.Uint32Value:
			return xmlWriteTyped(e, w, []uint32(v), 4, func(p []byte, v uint32) { order.PutUint32(p, v) })
		case core.Int32Value:
			return xmlWriteTyped(e, w, []int32(v), 4, func(p []byte, v int32) { order.PutUint32(p, uint32(v)) })
		case core.Uint64Value:
			return xmlWriteTyped(e, w, []uint64(v), 8, func(p []byte, v uint64) { order.PutUint64(p, v) })
		case core.Int64Value:
			return xmlWriteTyped(e, w, []int64(v), 8, func(p []byte, v int64) { order.PutUint64(p, uint64(v)) })
		case core.Float32Value:
			return xmlWriteTyped(e, w, []float32(v), 4, func(p []byte, v float32) { order.PutUint32(p, math.Float32bits(v)) })
		case core.Float64Value:
			return xmlWriteTyped(e, w, []float64(v), 8, func(p []byte, v float64) { order.PutUint64(p, math.Float64bits(v)) })
		case core.TagValue:
			return xmlWriteTyped(e, w, []core.Tag(v), 4, func(p []byte, v core.Tag) { order.PutUint16(p, v.Group); order.PutUint16(p[2:], v.Element) })
		}
		return fmt.Errorf("%w: unsupported numeric value", ErrInvalidXML)

	case core.FragmentSequence:
		if _, _, err := xmlBinaryLength(elem); err != nil {
			return err
		}
		if err := e.writeFragment(w, value.OffsetTable, false); err != nil {
			return err
		}
		for _, fragment := range value.Fragments {
			if err := e.writeFragment(w, fragment, true); err != nil {
				return err
			}
		}
		return e.writeItemHeader(w, core.TagSequenceDelimitationItem, 0)
	case nil:
		if elem.Header.HasLength() && elem.EncodedLength() == 0 {
			return nil
		}
		sink := xmlContextWriter{encoder: e, writer: w}
		var err error
		if obj != e.root {
			_, err = e.root.CopyNestedValueTo(obj, elem.Tag(), sink)
		} else {
			_, err = obj.CopyValueTo(elem.Tag(), sink)
		}
		return err
	default:
		if length, ok := elem.CalculatedLength(); ok && length == 0 {
			return nil
		}
		return fmt.Errorf("%w: unsupported %s value type %T", ErrInvalidXML, elem.Tag(), elem.Value)
	}
}

type xmlContextWriter struct {
	encoder *nativeEncoder
	writer  io.Writer
}

func (w xmlContextWriter) Write(data []byte) (int, error) {
	if err := w.encoder.writeBytes(w.writer, data); err != nil {
		return 0, err
	}
	return len(data), nil
}

func (e *nativeEncoder) writeBytes(w io.Writer, data []byte) error {
	for len(data) > 0 {
		if err := e.b.check(); err != nil {
			return err
		}
		chunk := data[:min(len(data), 32<<10)]
		n, err := w.Write(chunk)
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}
func (e *nativeEncoder) writeItemHeader(w io.Writer, tag core.Tag, length uint32) error {
	var header [8]byte
	e.opts.ByteOrder.PutUint16(header[:2], tag.Group)
	e.opts.ByteOrder.PutUint16(header[2:4], tag.Element)
	e.opts.ByteOrder.PutUint32(header[4:], length)
	return e.writeBytes(w, header[:])
}
func (e *nativeEncoder) writeFragment(w io.Writer, data []byte, pad bool) error {
	length := uint64(len(data))
	if pad {
		length += length % 2
	}
	if length > math.MaxUint32 {
		return fmt.Errorf("%w: fragment length exceeds uint32", ErrInvalidXML)
	}
	if err := e.writeItemHeader(w, core.TagItem, uint32(length)); err != nil {
		return err
	}
	if err := e.writeBytes(w, data); err != nil {
		return err
	}
	if length > uint64(len(data)) {
		return e.writeBytes(w, []byte{0})
	}
	return nil
}

func xmlNumericWidth(vr core.VR) int {
	switch vr {
	case core.VRSS, core.VRUS:
		return 2
	case core.VRAT, core.VRFL, core.VRSL, core.VRUL:
		return 4
	case core.VRFD, core.VRSV, core.VRUV:
		return 8
	default:
		return 0
	}
}

type xmlNumericWriter struct {
	encoder                *nativeEncoder
	vr                     core.VR
	width, pending, number int
	data                   [8]byte
}

func (w *xmlNumericWriter) Write(data []byte) (int, error) {
	original := len(data)
	for len(data) > 0 {
		n := copy(w.data[w.pending:w.width], data)
		data = data[n:]
		w.pending += n
		if w.pending == w.width {
			w.number++
			value := xmlNumericText(w.vr, w.data[:w.width], w.encoder.opts.ByteOrder)
			if err := w.encoder.numberedValue(value, w.number); err != nil {
				return original - len(data), err
			}
			w.pending = 0
		}
	}
	return original, nil
}
func xmlNumericText(vr core.VR, data []byte, order binary.ByteOrder) string {
	switch vr {
	case core.VRAT:
		return core.NewTag(order.Uint16(data[:2]), order.Uint16(data[2:])).HexString()
	case core.VRSS:
		return strconv.FormatInt(int64(int16(order.Uint16(data))), 10)
	case core.VRUS:
		return strconv.FormatUint(uint64(order.Uint16(data)), 10)
	case core.VRSL:
		return strconv.FormatInt(int64(int32(order.Uint32(data))), 10)
	case core.VRUL:
		return strconv.FormatUint(uint64(order.Uint32(data)), 10)
	case core.VRSV:
		return strconv.FormatInt(int64(order.Uint64(data)), 10)
	case core.VRUV:
		return strconv.FormatUint(order.Uint64(data), 10)
	case core.VRFL:
		return xmlFloatText(float64(math.Float32frombits(order.Uint32(data))))
	default:
		return xmlFloatText(math.Float64frombits(order.Uint64(data)))
	}
}

// Preserve the JSON bridge's established decimal spelling without marshaling
// JSON: finite binary floats use fixed form in [1e-6,1e21), scientific outside.
func xmlFloatText(value float64) string {
	switch {
	case math.IsNaN(value):
		return "NaN"
	case math.IsInf(value, 1):
		return "Infinity"
	case math.IsInf(value, -1):
		return "-Infinity"
	}
	format := byte('f')
	if abs := math.Abs(value); abs != 0 && (abs < 1e-6 || abs >= 1e21) {
		format = 'e'
	}
	text := strconv.FormatFloat(value, format, -1, 64)
	if at := strings.LastIndex(text, "e-"); at >= 0 && len(text)-at == 4 && text[at+2] == '0' {
		text = text[:at+2] + text[at+3:]
	}
	return text
}

// Typed numeric sources are serialized a bounded chunk at a time, avoiding a
// second full-size binary array for OF/OD/OV or high-multiplicity numbers.
func xmlWriteTyped[T any](e *nativeEncoder, w io.Writer, values []T, width int, put func([]byte, T)) error {
	var data [4096]byte
	for len(values) > 0 {
		if err := e.b.check(); err != nil {
			return err
		}
		count := min(len(values), len(data)/width)
		for i, v := range values[:count] {
			put(data[i*width:(i+1)*width], v)
		}
		if err := e.writeBytes(w, data[:count*width]); err != nil {
			return err
		}
		values = values[count:]
	}
	return nil
}
