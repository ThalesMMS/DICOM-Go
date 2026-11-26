// Package mllp carries HL7 version 2 messages over the Minimal Lower Layer
// Protocol: each message travels as <VT> payload <FS><CR> on a TCP
// connection, optionally under TLS. The default MLLP1 profile expects one
// application response frame. Explicit MLLP2 additionally exchanges framed
// one-byte transport ACK/NAK confirmations, independently of application ACKs.
// A transport ACK confirms bounded frame admission, not durable persistence.
//
// Framing is strict: bytes outside a frame, a start or end block inside the
// payload, an end block not followed by a carriage return and a frame over
// the size limit are errors that end the connection. Errors and the
// OnError callback name the condition and sizes, never message content,
// which carries patient data.
//
// Like the DIMSE stack, the server and client are trusted-network
// components: MLLP has no authentication of its own. Expose them only on
// trusted networks, or configure TLS (with client certificates) in
// ServerOptions.TLSConfig and ClientOptions.TLSConfig.
package mllp

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
)

// Frame bytes.
const (
	StartBlock     byte = 0x0B
	EndBlock       byte = 0x1C
	CarriageReturn byte = 0x0D
)

// DefaultMaxFrameBytes bounds a frame's payload when no limit is set.
const DefaultMaxFrameBytes = 1 << 20

var (
	// ErrFraming reports bytes that break MLLP framing.
	ErrFraming = errors.New("mllp: framing error")
	// ErrFrameTooLarge reports a payload over the size limit.
	ErrFrameTooLarge = errors.New("mllp: frame exceeds the size limit")
	// Invalid writer progress cannot establish whether bytes reached the peer.
	errInvalidWriteCount = fmt.Errorf("%w: invalid writer byte count", io.ErrShortWrite)
)

// Reader reads MLLP frames from a stream.
type Reader struct {
	r   *bufio.Reader
	max int
	// onStart, when set, runs after each start block.
	onStart func() error
}

// NewReader reads frames of at most maxFrameBytes payload bytes; zero or
// less is DefaultMaxFrameBytes.
func NewReader(r io.Reader, maxFrameBytes int) *Reader {
	if maxFrameBytes <= 0 {
		maxFrameBytes = DefaultMaxFrameBytes
	}
	return &Reader{r: bufio.NewReaderSize(r, 32<<10), max: maxFrameBytes}
}

// ReadFrame returns the next frame's payload. It returns io.EOF when the
// stream ends between frames and io.ErrUnexpectedEOF inside one.
func (r *Reader) ReadFrame() ([]byte, error) {
	return r.readFrame(r.max)
}

func (r *Reader) readFrame(max int) ([]byte, error) {
	first, err := r.r.ReadByte()
	if err != nil {
		return nil, err
	}
	if first != StartBlock {
		return nil, fmt.Errorf("%w: byte 0x%02X before the start block", ErrFraming, first)
	}
	if r.onStart != nil {
		if err := r.onStart(); err != nil {
			return nil, err
		}
	}
	var payload []byte
	for {
		chunk, err := r.r.ReadSlice(EndBlock)
		if len(chunk) > max-len(payload) && (err != nil || len(chunk)-1 > max-len(payload)) {
			return nil, fmt.Errorf("%w: more than %d bytes", ErrFrameTooLarge, max)
		}
		payload = append(payload, chunk...)
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, fmt.Errorf("%w: stream ended inside a frame of %d bytes", io.ErrUnexpectedEOF, len(payload))
			}
			return nil, err
		}
		break
	}
	payload = payload[:len(payload)-1] // the end block
	trailer, err := r.r.ReadByte()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, fmt.Errorf("%w: stream ended after the end block", io.ErrUnexpectedEOF)
		}
		return nil, err
	}
	if trailer != CarriageReturn {
		return nil, fmt.Errorf("%w: byte 0x%02X after the end block, want CR", ErrFraming, trailer)
	}
	if len(payload) == 0 {
		return nil, fmt.Errorf("%w: empty frame", ErrFraming)
	}
	if len(payload) > max {
		return nil, fmt.Errorf("%w: more than %d bytes", ErrFrameTooLarge, max)
	}
	if i := bytes.IndexByte(payload, StartBlock); i >= 0 {
		return nil, fmt.Errorf("%w: start block inside the frame at byte %d", ErrFraming, i)
	}
	return payload, nil
}

// AppendFrame appends payload framed as <VT> payload <FS><CR> to dst.
func AppendFrame(dst, payload []byte) ([]byte, error) {
	if err := validateFramePayload(payload); err != nil {
		return dst, err
	}
	if len(payload) > int(^uint(0)>>1)-3-len(dst) {
		return dst, ErrFrameTooLarge
	}
	return appendFrame(dst, payload), nil
}

func appendFrame(dst, payload []byte) []byte {
	dst = append(dst, StartBlock)
	dst = append(dst, payload...)
	return append(dst, EndBlock, CarriageReturn)
}

func validateFramePayload(payload []byte) error {
	if len(payload) == 0 {
		return fmt.Errorf("%w: empty payload", ErrFraming)
	}
	if i := bytes.IndexAny(payload, string([]byte{StartBlock, EndBlock})); i >= 0 {
		return fmt.Errorf("%w: frame byte inside the payload at byte %d", ErrFraming, i)
	}
	return nil
}

// WriteFrame writes one complete frame, continuing positive short writes.
// A writer making no progress refuses with io.ErrShortWrite.
func WriteFrame(w io.Writer, payload []byte) error {
	_, err := writeFrame(w, payload)
	return err
}

func writeFrame(w io.Writer, payload []byte) (int, error) {
	if err := validateFramePayload(payload); err != nil {
		return 0, err
	}
	if len(payload) > int(^uint(0)>>1)-3 {
		return 0, ErrFrameTooLarge
	}
	frame := appendFrame(make([]byte, 0, len(payload)+3), payload)
	written := 0
	for written < len(frame) {
		n, err := w.Write(frame[written:])
		if n < 0 || n > len(frame)-written {
			return written, errInvalidWriteCount
		}
		written += n
		if err != nil {
			return written, err
		}
		if n == 0 {
			return written, io.ErrShortWrite
		}
	}
	return written, nil
}
