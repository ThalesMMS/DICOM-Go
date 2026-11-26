package hl7

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
)

// ErrStreamRead reports an input failure without details from the reader.
var ErrStreamRead = errors.New("hl7: could not read ER7 input")

// StreamError locates a failure in a sequence of ER7 messages without
// including any bytes from the source. The underlying ParseError, when
// present, locates a segment or field within the message.
type StreamError struct {
	Message int64 // 1-based index of the message
	Offset  int64 // 0-based byte offset of its start
	Err     error
}

func (e *StreamError) Error() string {
	return fmt.Sprintf("hl7: message %d at byte %d: %v", e.Message, e.Offset, e.Err)
}

func (e *StreamError) Unwrap() error { return e.Err }

// ParseStream reads concatenated, unframed ER7 messages. An MSH segment with
// a legal field separator begins a new message. The last one ends at EOF.
// The raw-byte limit applies to each message, including its terminators.
// A blank separator after a message counts toward that message's byte limit.
//
// consume runs synchronously with a detached parsed message. Earlier
// messages remain consumed if a later message fails. A callback error is
// returned directly. ParseStream buffers one message plus reader lookahead.
// It does not close input or interrupt a blocked Read on cancellation.
func ParseStream(ctx context.Context, input io.Reader, options ParseOptions, consume func(*Message) error) error {
	if input == nil || consume == nil {
		return errors.New("hl7: ParseStream requires a reader and a message handler")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	options = options.withDefaults()
	wire := newER7Input(input)
	var data []byte
	var start int64
	index := int64(1)
	fail := func(err error) error {
		return &StreamError{Message: index, Offset: start, Err: err}
	}
	for {
		name, err := wire.peekName(ctx)
		eof := err == io.EOF
		if err != nil && !eof {
			return fail(err)
		}
		nextMessage := false
		if len(data) != 0 && name == "MSH" {
			prefix, err := wire.peek(ctx, 4)
			if err != nil && err != io.EOF {
				return fail(err)
			}
			if len(prefix) == 4 {
				separator := prefix[3]
				nextMessage = separator >= 0x21 && separator <= 0x7e && !isAlphanumeric(separator)
			}
		}
		if len(data) != 0 && (nextMessage || eof) {
			message, err := ParseWithOptions(data, options)
			if err != nil {
				return fail(err)
			}
			if err := ctx.Err(); err != nil {
				return fail(err)
			}
			if err := consume(message); err != nil {
				return err
			}
			data = data[:0]
			index++
			start = wire.offset
		}
		if eof {
			return ctx.Err()
		}
		if len(data) == 0 && name != "MSH" {
			return fail(&ParseError{Err: fmt.Errorf("%w: the message does not start with MSH", ErrSyntax)})
		}
		data, err = wire.readSegment(ctx, data, options.MaxBytes)
		if err != nil {
			return fail(err)
		}
	}
}

// er7Input splits raw segments before charset decoding. CR and LF are
// control bytes distinct from encoded characters in the supported ER7 sets.
type er7Input struct {
	reader *bufio.Reader
	offset int64
	eof    bool
}

func newER7Input(input io.Reader) *er7Input {
	return &er7Input{reader: bufio.NewReader(input)}
}

func (in *er7Input) peekName(ctx context.Context) (string, error) {
	prefix, err := in.peek(ctx, 3)
	if err == io.EOF && len(prefix) != 0 {
		return string(prefix), nil
	}
	return string(prefix), err
}

// readSegment appends the next raw segment with its terminator. For CR LF,
// the LF is a separate empty segment. Include it in the relevant byte budget.
func (in *er7Input) readSegment(ctx context.Context, dst []byte, maxBytes int) ([]byte, error) {
	start := len(dst)
	for {
		if _, err := in.peek(ctx, 1); err != nil {
			if err == io.EOF && len(dst) != start {
				return dst, nil
			}
			return dst, err
		}
		chunk, _ := in.reader.Peek(in.reader.Buffered())
		terminator := bytes.IndexAny(chunk, "\r\n")
		if terminator >= 0 {
			chunk = chunk[:terminator+1]
		}
		if len(chunk) > maxBytes-len(dst) {
			return dst, fmt.Errorf("%w: more than %d bytes", ErrLimit, maxBytes)
		}
		dst = append(dst, chunk...)
		_, _ = in.reader.Discard(len(chunk))
		in.offset += int64(len(chunk))
		if terminator >= 0 {
			return dst, nil
		}
	}
}

func (in *er7Input) peek(ctx context.Context, count int) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if in.eof && in.reader.Buffered() < count {
		data, _ := in.reader.Peek(in.reader.Buffered())
		return data, io.EOF
	}
	data, err := in.reader.Peek(count)
	if canceled := ctx.Err(); canceled != nil {
		return nil, canceled
	}
	switch {
	case err == nil:
		return data, nil
	case errors.Is(err, io.EOF):
		in.eof = true
		return data, io.EOF
	case errors.Is(err, context.Canceled):
		return nil, context.Canceled
	case errors.Is(err, context.DeadlineExceeded):
		return nil, context.DeadlineExceeded
	default:
		return nil, ErrStreamRead
	}
}
