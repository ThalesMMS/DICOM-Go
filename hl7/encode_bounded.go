package hl7

import (
	"bufio"
	"errors"
	"fmt"
	"strings"

	"golang.org/x/text/transform"
)

// MarshalWithLimit encodes canonical ER7 in MSH-18's character set, stopping
// before encoded output exceeds maxBytes. A nonpositive limit refuses with
// ErrLimit. Errors return no partial bytes. The output buffer capacity is
// capped by maxBytes; input/charset buffers have fixed sizes. Growth and
// validation can temporarily allocate additional memory proportional to this
// limit. Caller-owned Message storage is outside the output budget and must
// not be mutated concurrently. This does not impose a traversal time limit.
func (m *Message) MarshalWithLimit(maxBytes int) ([]byte, error) {
	if maxBytes <= 0 {
		return nil, ErrLimit
	}
	if m == nil || len(m.Segments) == 0 || m.Segments[0].Name != "MSH" {
		return nil, fmt.Errorf("%w: the first segment must be MSH", ErrSyntax)
	}
	charset := strings.TrimSpace(m.Header().CharacterSet)
	// Supported MSH-18 codes/aliases are at most 13 bytes. A closed,
	// conservative ceiling prevents encodingFor's case normalization from
	// allocating an authored, arbitrarily large token before admission.
	if len(charset) > 32 {
		return nil, fmt.Errorf("%w: unsupported MSH-18 value", ErrCharset)
	}
	encoding, utf8Set, err := encodingFor(charset)
	if err != nil {
		return nil, err
	}
	output := &boundedER7Output{max: maxBytes}
	var target er7ByteSink = output
	var encoder *transform.Writer
	if encoding != nil {
		encoder = transform.NewWriter(output, encoding.NewEncoder())
		target = encoder
	}
	writer := &boundedER7Writer{buffer: bufio.NewWriterSize(target, 4096), ascii: encoding == nil && !utf8Set}
	if err := m.writeER7(writer); err != nil {
		return nil, boundedEncodeError(err)
	}
	if err := writer.buffer.Flush(); err != nil {
		return nil, boundedEncodeError(err)
	}
	if encoder != nil {
		// Stateful sets may emit final bytes here; the same output ceiling
		// applies to this residue, including an ISO-2022-JP ASCII reset.
		if err := encoder.Close(); err != nil {
			return nil, boundedEncodeError(err)
		}
	}
	if err := m.checkEncodedCharacterSet(output.data, charset); err != nil {
		return nil, err
	}
	return output.data, nil
}

type er7ByteSink interface {
	Write([]byte) (int, error)
}

func boundedEncodeError(err error) error {
	var parseErr *ParseError
	if errors.Is(err, ErrLimit) || errors.Is(err, ErrCharset) || errors.As(err, &parseErr) {
		return err
	}
	// No transform error embeds message values. Match Marshal's charset
	// refusal while retaining the underlying transformer cause.
	return fmt.Errorf("%w: a character cannot be represented in the declared set: %w", ErrCharset, err)
}

type boundedER7Output struct {
	data []byte
	max  int
}

func (w *boundedER7Output) Write(p []byte) (int, error) {
	if len(p) > w.max-len(w.data) {
		return 0, ErrLimit
	}
	need := len(w.data) + len(p)
	if need > cap(w.data) {
		capacity := cap(w.data)
		if capacity > w.max/2 {
			capacity = w.max
		} else {
			capacity *= 2
		}
		if capacity < need {
			capacity = need
		}
		grown := make([]byte, len(w.data), capacity)
		copy(grown, w.data)
		w.data = grown
	}
	w.data = append(w.data, p...)
	return len(p), nil
}

type boundedER7Writer struct {
	buffer *bufio.Writer
	ascii  bool
}

func (w *boundedER7Writer) WriteByte(b byte) error {
	if w.ascii && b > 0x7F {
		return fmt.Errorf("%w: a character is not ASCII; declare MSH-18", ErrCharset)
	}
	return w.buffer.WriteByte(b)
}

func (w *boundedER7Writer) WriteString(s string) (int, error) {
	written := 0
	for len(s) > 0 {
		// Keep conversion buffers fixed even when an authored field is huge.
		n := min(len(s), 2048)
		chunk := s[:n]
		if w.ascii {
			for i := 0; i < len(chunk); i++ {
				if chunk[i] > 0x7F {
					return written, fmt.Errorf("%w: a character is not ASCII; declare MSH-18", ErrCharset)
				}
			}
		}
		count, err := w.buffer.WriteString(chunk)
		written += count
		if err != nil {
			return written, err
		}
		s = s[n:]
	}
	return written, nil
}
