package dicomweb

import (
	"bufio"
	"bytes"
	"io"
	"net/textproto"
)

const (
	// Keep delimiter lookahead within the former MIME reader's line buffer,
	// with room for the CRLF owned by a delimiter following a body.
	multipartResponseLineBytes = 4096
)

type multipartResponsePhase uint8

const (
	multipartResponsePreamble multipartResponsePhase = iota
	multipartResponseHeaders
	multipartResponseBody
	multipartResponseEnd
)

// multipartResponseReader verifies complete delimiters before ending a part.
// Ordinary body lines, including long ones, are delivered without buffering
// whole lines. Only headers and a possible delimiter need bounded lookahead.
type multipartResponseReader struct {
	wire           *bufio.Reader
	marker         []byte
	separator      []byte
	lineMarker     []byte
	maxHeaderBytes int
	phase          multipartResponsePhase
}

func newMultipartResponseReader(input io.Reader, boundary string, maximum int) *multipartResponseReader {
	return &multipartResponseReader{
		wire:           bufio.NewReaderSize(&multipartResponseInput{reader: input}, multipartResponseLineBytes+len("\r\n")),
		marker:         []byte("--" + boundary),
		maxHeaderBytes: maximum,
	}
}

// Peek can consume a pending error from bufio. Keep that error available for
// the next read when the bytes preceding it still belong to the body.
type multipartResponseInput struct {
	reader io.Reader
	err    error
}

func (r *multipartResponseInput) Read(p []byte) (int, error) {
	if r.err != nil {
		return 0, r.err
	}
	n, err := r.reader.Read(p)
	r.err = err
	return n, err
}

// nextPart requires the previous part to have reached EOF.
func (r *multipartResponseReader) nextPart() (*multipartResponsePart, error) {
	if r.phase == multipartResponsePreamble {
		if err := r.readFirstDelimiter(); err != nil {
			return nil, err
		}
	}
	if r.phase == multipartResponseEnd {
		return nil, io.EOF
	}
	if r.phase != multipartResponseHeaders {
		return nil, io.ErrUnexpectedEOF
	}
	header, err := r.readHeaders()
	if err != nil {
		return nil, err
	}
	r.phase = multipartResponseBody
	return &multipartResponsePart{Header: header, multipart: r, first: true}, nil
}

func (r *multipartResponseReader) readFirstDelimiter() error {
	for {
		line, err := r.wire.ReadSlice('\n')
		if len(line) > multipartResponseLineBytes {
			return io.ErrUnexpectedEOF
		}
		if err != nil && err != io.EOF {
			return err
		}
		if suffix, found := bytes.CutPrefix(line, r.marker); found {
			suffix, closing := bytes.CutPrefix(suffix, []byte("--"))
			switch string(bytes.TrimLeft(suffix, " \t")) {
			case "\r\n", "\n":
				ending := line[len(line)-1:]
				if bytes.HasSuffix(line, []byte("\r\n")) {
					ending = line[len(line)-2:]
				}
				r.separator = bytes.Clone(ending)
				r.lineMarker = append(bytes.Clone(ending), r.marker...)
				r.finishDelimiter(closing)
				return nil
			case "":
				if err == io.EOF && closing {
					r.phase = multipartResponseEnd
					return nil
				}
			}
		}
		if err == io.EOF {
			return io.ErrUnexpectedEOF
		}
	}
}

func (r *multipartResponseReader) finishDelimiter(closing bool) {
	if closing {
		r.phase = multipartResponseEnd
	} else {
		r.phase = multipartResponseHeaders
	}
}

// Compare only bytes present on the wire. A partial prefix must not hold up
// delivery after the next byte proves it belongs to the body.
func (r *multipartResponseReader) completePrefix(prefix []byte) bool {
	checked := 0
	for {
		count := min(r.wire.Buffered(), len(prefix))
		data, _ := r.wire.Peek(count)
		if !bytes.Equal(data[checked:], prefix[checked:count]) {
			return false
		}
		if count == len(prefix) {
			return true
		}
		checked = count
		if _, err := r.wire.Peek(count + 1); err != nil {
			return false
		}
	}
}

// delimiterAt confirms the suffix of a candidate prefix already in the
// buffer. An opening delimiter needs a line ending, and a closing delimiter
// may end at transport EOF. Any other suffix belongs to the body.
func (r *multipartResponseReader) delimiterAt(prefixBytes, precedingBytes int) (size int, closing bool) {
	cursor := prefixBytes
	limit := multipartResponseLineBytes + precedingBytes
	initial, err := r.wire.Peek(cursor + 1)
	if err != nil {
		return 0, false
	}
	if initial[cursor] == '-' {
		suffix, err := r.wire.Peek(cursor + 2)
		if err != nil || suffix[cursor+1] != '-' {
			return 0, false
		}
		closing = true
		cursor += 2
	}
	for cursor < limit {
		candidate, err := r.wire.Peek(cursor + 1)
		if err == io.EOF && closing {
			return cursor, true
		}
		if err != nil {
			break
		}
		switch candidate[cursor] {
		case ' ', '\t':
			cursor++
		default:
			if candidate[cursor] != r.separator[0] {
				return 0, false
			}
			size := cursor + len(r.separator)
			if size <= limit {
				line, err := r.wire.Peek(size)
				if err == nil && bytes.Equal(line[cursor:], r.separator) {
					return size, closing
				}
			}
			return 0, false
		}
	}
	return 0, false
}

type multipartResponsePart struct {
	Header    textproto.MIMEHeader
	multipart *multipartResponseReader
	first     bool
	safe      int
	err       error
}

func (p *multipartResponsePart) Read(out []byte) (int, error) {
	if len(out) == 0 {
		return 0, nil
	}
	if p.safe == 0 && p.err == nil {
		p.scan()
	}
	if p.safe == 0 {
		return 0, p.err
	}
	n, err := p.multipart.wire.Read(out[:min(len(out), p.safe)])
	p.safe -= n
	return n, err
}

func (p *multipartResponsePart) scan() {
	r := p.multipart
	if _, err := r.wire.Peek(1); err != nil {
		p.err = multipartIncomplete(err)
		return
	}
	if p.first {
		p.first = false
		// Preserve compatibility with senders that omit the separator before
		// a delimiter immediately after an empty part's header section.
		if r.completePrefix(r.marker) {
			p.useCandidate(len(r.marker), 0)
			return
		}
	}
	data, _ := r.wire.Peek(r.wire.Buffered())
	if position := bytes.Index(data, r.lineMarker); position >= 0 {
		if position == 0 {
			p.useCandidate(len(r.lineMarker), len(r.separator))
		} else {
			p.safe = position
		}
		return
	}

	// Deliver every byte known to be payload, even if the peer hasn't sent a
	// whole line. Retain only an incomplete prefix for the next wire read.
	tail := bytes.LastIndexByte(data, r.lineMarker[0])
	if tail < 0 || !bytes.HasPrefix(r.lineMarker, data[tail:]) {
		p.safe = len(data)
		return
	}
	if tail != 0 {
		p.safe = tail
		return
	}
	if r.completePrefix(r.lineMarker) {
		p.useCandidate(len(r.lineMarker), len(r.separator))
	} else {
		// A later prefix may begin within the newly buffered bytes.
		p.safe = 1
	}
}

func (p *multipartResponsePart) useCandidate(prefixBytes, separatorBytes int) {
	r := p.multipart
	if size, closing := r.delimiterAt(prefixBytes, separatorBytes); size > 0 {
		_, _ = r.wire.Discard(size)
		r.finishDelimiter(closing)
		p.err = io.EOF
	} else {
		p.safe = prefixBytes
	}
}

func multipartIncomplete(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}
