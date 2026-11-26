package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// MaxCommandSetBytes bounds one reassembled DIMSE command set across all PDVs.
const MaxCommandSetBytes = 4 << 20

var ErrPresentationContextMismatch = errors.New("dicom dimse: presentation context mismatch")

// ErrCommandSetTooLarge reports that a peer exceeded MaxCommandSetBytes
// before terminating the command PDV stream.
var ErrCommandSetTooLarge = errors.New("dicom dimse: command set too large")

type PDataWriter struct {
	assoc     *ul.Association
	ctx       context.Context
	pcID      byte
	isCommand bool
	maxPDU    int
	buf       []byte
	written   int64
	finished  bool
	// A complete-message writer may retain the final command fragment until
	// the first dataset fragment is ready. Public dataset-only writers leave
	// these fields unset.
	commandPrefix []byte
	commandSent   func()
	firstContext  context.Context
}

func NewPDataWriter(assoc *ul.Association, pcID byte, isCommand bool, maxPDU int) *PDataWriter {
	return NewPDataWriterWithContext(nil, assoc, pcID, isCommand, maxPDU)
}

// NewPDataWriterWithContext creates a fragmenting DIMSE P-DATA writer whose
// socket writes observe ctx. A nil context inherits the association context.
func NewPDataWriterWithContext(ctx context.Context, assoc *ul.Association, pcID byte, isCommand bool, maxPDU int) *PDataWriter {
	return &PDataWriter{
		assoc:     assoc,
		ctx:       ctx,
		pcID:      pcID,
		isCommand: isCommand,
		maxPDU:    maxPDU,
	}
}

func (w *PDataWriter) Write(p []byte) (int, error) {
	if w == nil {
		return 0, fmt.Errorf("dicom dimse: nil PDataWriter")
	}
	if w.finished {
		return 0, fmt.Errorf("dicom dimse: write after P-DATA finish")
	}
	if w.buf == nil {
		w.buf = getPDataBuffer(w.maxPayload())
	}
	written := 0
	for len(p) > 0 {
		payloadMax := w.maxPayload()
		space := payloadMax - len(w.buf)
		if space <= 0 {
			if err := w.flush(false); err != nil {
				return written, err
			}
			// The command prefix consumes space only in the first envelope.
			space = w.maxPayload()
		}
		if space > len(p) {
			space = len(p)
		}
		w.buf = append(w.buf, p[:space]...)
		p = p[space:]
		written += space
		w.written += int64(space)
	}
	return written, nil
}

// finishDataSet ends a data set's P-DATA stream. Every PDV must have an even
// length (PS3.8 E.1; DCMTK aborts on an odd fragment), and only the last
// one can be odd because the others are cut at an even payload size. A data
// set in Deflated Explicit VR Little Endian is a deflate bitstream that PS3.5
// A.5 pads with a single trailing NULL byte when odd; object.WriteDataSet
// emits that pad, and this adds it for bitstreams copied verbatim from a
// source that lacks it.
func (w *PDataWriter) finishDataSet(syntax transfer.Syntax) error {
	if w != nil && !w.finished && syntax.Deflated && w.written%2 != 0 {
		if _, err := w.Write([]byte{0}); err != nil {
			return err
		}
	}
	return w.Finish()
}

func (w *PDataWriter) Finish() error {
	if w == nil {
		return fmt.Errorf("dicom dimse: nil PDataWriter")
	}
	if w.finished {
		return nil
	}
	w.finished = true
	err := w.flush(true)
	w.discard()
	return err
}

// discard releases staging without publishing an incomplete message.
func (w *PDataWriter) discard() {
	if w == nil {
		return
	}
	putPDataBuffer(w.buf)
	putPDataBuffer(w.commandPrefix)
	w.buf, w.commandPrefix = nil, nil
	w.commandSent, w.firstContext = nil, nil
	w.finished = true
}

func (w *PDataWriter) flush(isLast bool) error {
	if w.assoc == nil {
		return fmt.Errorf("dicom dimse: nil association")
	}
	if !isLast && len(w.buf) == 0 {
		return nil
	}
	data := w.buf
	value := ul.PDataValue{
		PresentationContextID: w.pcID,
		IsCommand:             w.isCommand,
		IsLast:                isLast,
		Data:                  data,
	}
	pdu := &ul.PDataTF{}
	ctx := w.ctx
	if w.commandPrefix != nil {
		pdu.Values = []ul.PDataValue{{
			PresentationContextID: w.pcID,
			IsCommand:             true,
			IsLast:                true,
			Data:                  w.commandPrefix,
		}, value}
		ctx = w.firstContext
	} else {
		pdu.Values = []ul.PDataValue{value}
	}
	var err error
	if ctx != nil {
		err = w.assoc.Send(ctx, pdu)
	} else {
		err = w.assoc.WritePDU(pdu)
	}
	// Association.WritePDU serializes synchronously and copies Data into its PDU
	// body before returning, so the writer can safely reclaim this backing array
	// without a defensive full-payload clone.
	w.buf = data[:0]
	if w.commandPrefix != nil {
		putPDataBuffer(w.commandPrefix)
		w.commandPrefix = nil
		if err == nil && w.commandSent != nil {
			w.commandSent()
		}
		w.commandSent, w.firstContext = nil, nil
	}
	return err
}

func (w *PDataWriter) maxPayload() int {
	maxPDU := w.maxPDU
	if maxPDU <= 0 {
		maxPDU = int(ul.DefaultMaxPDU + ul.PDUHeaderSize)
	}
	payloadMax := maxPDU - int(ul.PDUHeaderSize+ul.PDVHeaderSize)
	if w.commandPrefix != nil {
		payloadMax -= int(ul.PDVHeaderSize) + len(w.commandPrefix)
	}
	if payloadMax < 1 {
		return 1
	}
	// PDV values keep an even length, as DCMTK sends them, unless the peer's
	// maximum leaves room for a single byte.
	if payloadMax > 1 {
		payloadMax &^= 1
	}
	return payloadMax
}

// pDataBuffers recycles P-DATA staging buffers, which reach 128 KiB with
// the default send limit.
var pDataBuffers sync.Pool

func getPDataBuffer(size int) []byte {
	if pooled, ok := pDataBuffers.Get().(*[]byte); ok && cap(*pooled) >= size {
		return (*pooled)[:0]
	}
	return make([]byte, 0, size)
}

func putPDataBuffer(buf []byte) {
	if cap(buf) == 0 || cap(buf) > 1<<20 {
		return
	}
	buf = buf[:0]
	pDataBuffers.Put(&buf)
}

// readPDataDataSet parses one DIMSE data set from reader. A Deflated data set
// ends at its final deflate block, which can precede the last P-DATA fragment:
// the PS3.5 A.5 NULL pad, or an empty last PDV, may follow it (DCMTK likewise
// ignores bytes after the end of the stream). Those are consumed up to the
// last fragment so they are not read as the start of the next message.
func readPDataDataSet(reader *PDataReader, syntax transfer.Syntax, opts object.ReadFileOptions) (*object.Object, error) {
	dataset, err := object.ReadDataSetWithOptions(reader, syntax, opts)
	if err != nil || !syntax.Deflated {
		return dataset, err
	}
	if err := drainPDataReader(reader); err != nil {
		return nil, fmt.Errorf("dicom dimse: read past the end of the deflated data set: %w", err)
	}
	return dataset, nil
}

type PDataReader struct {
	assoc         *ul.Association
	ctx           context.Context
	pcID          byte
	expectedType  *bool
	buf           []byte
	bufOffset     int
	fragments     [][]byte
	fragmentIndex int
	fragmentBytes int
	lastReceived  bool
	receivedBytes int
}

func NewPDataReader(assoc *ul.Association, pcID byte) *PDataReader {
	return &PDataReader{assoc: assoc, pcID: pcID}
}

// NewPDataReaderWithPrefix returns a reader of one data set whose first
// bytes were already taken off the association, such as data set PDVs that
// arrived in the same P-DATA-TF as the command. last reports that the
// prefix ended with the last fragment, so nothing more is read from the
// association. Further PDVs must carry data set (not command) bytes on pcID.
func NewPDataReaderWithPrefix(assoc *ul.Association, pcID byte, prefix []byte, last bool) *PDataReader {
	reader := newTypedPDataReader(assoc, pcID, false)
	reader.appendFragment(prefix)
	reader.receivedBytes = len(prefix)
	reader.lastReceived = last
	return reader
}

// ReadPDataDataSet parses one DIMSE data set from reader, as the library's
// SCPs and SCUs do. A Deflated data set ends at its final deflate block, which
// can precede the last P-DATA fragment; the PS3.5 A.5 pad byte or an empty
// last PDV that may follow is consumed, so it is not read as the start of the
// next message. Other transfer syntaxes stop at the end of the data set.
func ReadPDataDataSet(reader *PDataReader, syntax transfer.Syntax, opts object.ReadFileOptions) (*object.Object, error) {
	if reader == nil {
		return nil, fmt.Errorf("dicom dimse: nil PDataReader")
	}
	return readPDataDataSet(reader, syntax, opts)
}

func newTypedPDataReader(assoc *ul.Association, pcID byte, isCommand bool) *PDataReader {
	return &PDataReader{assoc: assoc, pcID: pcID, expectedType: &isCommand}
}

func newTypedPDataReaderWithContext(ctx context.Context, assoc *ul.Association, pcID byte, isCommand bool) *PDataReader {
	return &PDataReader{assoc: assoc, ctx: ctx, pcID: pcID, expectedType: &isCommand}
}

func (r *PDataReader) Read(p []byte) (int, error) {
	if r == nil {
		return 0, fmt.Errorf("dicom dimse: nil PDataReader")
	}
	if len(p) == 0 {
		return 0, nil
	}
	for r.bufferedLen() == 0 && !r.lastReceived {
		r.resetConsumedBuffer()
		if err := r.readNextPData(); err != nil {
			// Only the last-fragment flag ends a message. A transport EOF before
			// it is a truncated message, never a clean end of the data set.
			if errors.Is(err, io.EOF) {
				return 0, fmt.Errorf("dicom dimse: association ended before the last P-DATA fragment: %w (%v)", io.ErrUnexpectedEOF, err)
			}
			return 0, err
		}
	}
	if r.bufferedLen() == 0 && r.lastReceived {
		r.resetConsumedBuffer()
		return 0, io.EOF
	}
	n := 0
	for n < len(p) && r.bufferedLen() > 0 {
		r.resetConsumedBuffer()
		copied := copy(p[n:], r.buf[r.bufOffset:])
		r.bufOffset += copied
		n += copied
	}
	r.resetConsumedBuffer()
	return n, nil
}

func (r *PDataReader) bufferedLen() int {
	return len(r.buf) - r.bufOffset + r.fragmentBytes
}

func (r *PDataReader) resetConsumedBuffer() {
	if r.bufOffset != len(r.buf) {
		return
	}
	r.buf = nil
	r.bufOffset = 0
	for r.fragmentIndex < len(r.fragments) {
		fragment := r.fragments[r.fragmentIndex]
		r.fragments[r.fragmentIndex] = nil
		r.fragmentIndex++
		r.fragmentBytes -= len(fragment)
		if len(fragment) > 0 {
			r.buf = fragment
			break
		}
	}
	if r.fragmentIndex == len(r.fragments) {
		r.fragments = nil
		r.fragmentIndex = 0
	}
}

func (r *PDataReader) readNextPData() error {
	values, err := receivePDataValues(r.ctx, r.assoc)
	if err != nil {
		return err
	}
	if err := r.consumePDataValues(values); err != nil {
		return err
	}
	if !r.lastReceived {
		ctx := r.ctx
		if ctx == nil && r.assoc != nil {
			ctx = r.assoc.Context
		}
		r.ctx = ul.WithActiveTransfer(ctx)
	}
	return nil
}

func (r *PDataReader) consumePDataValues(values []ul.PDataValue) error {
	for i, value := range values {
		if r.lastReceived {
			return storePDataCarryoverWithContext(r.ctx, r.assoc, values[i:])
		}
		if value.PresentationContextID != r.pcID {
			return fmt.Errorf("%w: got %d, want %d", ErrPresentationContextMismatch, value.PresentationContextID, r.pcID)
		}
		if r.expectedType != nil && value.IsCommand != *r.expectedType {
			return fmt.Errorf("dicom dimse: P-DATA command flag %t, want %t", value.IsCommand, *r.expectedType)
		}
		if r.expectedType != nil && *r.expectedType {
			if err := checkCommandSetSize(r.receivedBytes, len(value.Data)); err != nil {
				return err
			}
		}
		if len(value.Data) > int(^uint(0)>>1)-r.receivedBytes {
			return fmt.Errorf("dicom dimse: P-DATA byte count overflow")
		}
		r.receivedBytes += len(value.Data)
		r.appendFragment(value.Data)
		if value.IsLast {
			r.lastReceived = true
			return storePDataCarryoverWithContext(r.ctx, r.assoc, values[i+1:])
		}
	}
	return nil
}

func (r *PDataReader) appendFragment(fragment []byte) {
	if len(fragment) == 0 {
		return
	}
	// UL owns each decoded PDV byte slice. Retain it until the consumer advances
	// instead of copying the same payload into a second staging buffer.
	if len(r.buf) == 0 && r.bufOffset == 0 && r.fragmentBytes == 0 {
		r.buf = fragment
		return
	}
	r.fragments = append(r.fragments, fragment)
	r.fragmentBytes += len(fragment)
}

func appendCommandSetFragment(buf []byte, fragment []byte) ([]byte, error) {
	if err := checkCommandSetSize(len(buf), len(fragment)); err != nil {
		return nil, err
	}
	return append(buf, fragment...), nil
}

func checkCommandSetSize(current, additional int) error {
	if current < 0 || additional < 0 || current > MaxCommandSetBytes || additional > MaxCommandSetBytes-current {
		return fmt.Errorf("%w: %d existing byte(s) plus %d incoming byte(s) exceeds %d", ErrCommandSetTooLarge, current, additional, MaxCommandSetBytes)
	}
	return nil
}

func receivePDataValues(ctx context.Context, assoc *ul.Association) ([]ul.PDataValue, error) {
	if assoc == nil {
		return nil, fmt.Errorf("dicom dimse: nil association")
	}
	values, err := takePDataCarryoverWithContext(ctx, assoc)
	if err != nil {
		return nil, err
	}
	if len(values) > 0 {
		return values, nil
	}
	var pdu ul.PDU
	if ctx != nil {
		pdu, err = assoc.Receive(ctx)
	} else {
		pdu, err = assoc.ReadPDU()
	}
	if err != nil {
		return nil, err
	}
	pdata, ok := pdu.(*ul.PDataTF)
	if !ok {
		return nil, ul.UnexpectedPDUError(pdu, "waiting for P-DATA-TF")
	}
	return pdata.Values, nil
}

func takePDataCarryoverWithContext(ctx context.Context, assoc *ul.Association) ([]ul.PDataValue, error) {
	if assoc == nil {
		return nil, nil
	}
	return assoc.TakePDataCarryoverContext(ctx)
}

func storePDataCarryoverWithContext(ctx context.Context, assoc *ul.Association, values []ul.PDataValue) error {
	if assoc == nil {
		return nil
	}
	return assoc.StorePDataCarryoverContext(ctx, values)
}
