package ldapconfig

import (
	"io"
	"net"
	"sync"

	ber "github.com/go-asn1-ber/asn1-ber"
)

// boundedConn admits the outer length before allocation and the complete BER
// tree before the protocol client sees it. A byte-only socket limit cannot stop
// a BER decoder from allocating a forged length or recursing on deep input.
type boundedConn struct {
	net.Conn
	options  Options
	used     int
	frame    []byte
	position int
	failure  error
	mu       sync.Mutex
	entries  int
}

func (c *boundedConn) lastError() error { c.mu.Lock(); defer c.mu.Unlock(); return c.failure }
func (c *boundedConn) fail(err error)   { c.mu.Lock(); c.failure = err; c.mu.Unlock() }

func (c *boundedConn) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if err := c.lastError(); err != nil {
		return 0, err
	}
	if c.position == len(c.frame) {
		frame, err := readFrame(c.Conn, c.options, c.options.MaxWireBytes-c.used)
		if err != nil {
			c.fail(err)
			return 0, err
		}
		op, err := validateEnvelope(frame, c.options)
		if err != nil {
			c.fail(err)
			return 0, err
		}
		if op == 4 {
			c.entries++
			if c.entries > c.options.MaxEntries || len(frame) > c.options.MaxEntryBytes {
				c.fail(ErrLimit)
				return 0, ErrLimit
			}
		}
		c.used += len(frame)
		c.frame, c.position = frame, 0
	}
	n := copy(p, c.frame[c.position:])
	c.position += n
	return n, nil
}

// Validate response shape before the protocol client's typed assertions, also
// inspecting BER embedded in the otherwise opaque paging control value.
func validateEnvelope(frame []byte, o Options) (ber.Tag, error) {
	p, err := ber.DecodePacketErr(frame)
	if err != nil || len(p.Children) < 2 || len(p.Children) > 3 {
		return 0, ErrProtocol
	}
	id, ok := p.Children[0].Value.(int64)
	if !ok || p.Children[0].ClassType != ber.ClassUniversal || p.Children[0].TagType != ber.TypePrimitive || p.Children[0].Tag != ber.TagInteger || id < 1 || id > 2147483647 {
		return 0, ErrProtocol
	}
	op := p.Children[1]
	if op.ClassType != ber.ClassApplication || op.TagType != ber.TypeConstructed {
		return 0, ErrProtocol
	}
	isString := func(p *ber.Packet) bool {
		_, ok := p.Value.(string)
		return p.ClassType == ber.ClassUniversal && p.Tag == ber.TagOctetString && p.TagType == ber.TypePrimitive && ok
	}
	switch op.Tag {
	case 1, 5, 24:
		if len(op.Children) < 3 || op.Children[0].Tag != ber.TagEnumerated || !isString(op.Children[1]) || !isString(op.Children[2]) {
			return 0, ErrProtocol
		}
		if code, ok := op.Children[0].Value.(int64); !ok || code < 0 {
			return 0, ErrProtocol
		}
	case 4:
		if len(op.Children) != 2 || !isString(op.Children[0]) || op.Children[1].Tag != ber.TagSequence {
			return 0, ErrProtocol
		}
		for _, attr := range op.Children[1].Children {
			if attr.Tag != ber.TagSequence || len(attr.Children) != 2 || !isString(attr.Children[0]) || attr.Children[1].Tag != ber.TagSet {
				return 0, ErrProtocol
			}
			for _, value := range attr.Children[1].Children {
				if !isString(value) {
					return 0, ErrProtocol
				}
			}
		}
	case 19:
		for _, ref := range op.Children {
			if !isString(ref) {
				return 0, ErrProtocol
			}
		}
	default:
		return 0, ErrProtocol
	}
	if len(p.Children) == 3 {
		controls := p.Children[2]
		if controls.ClassType != ber.ClassContext || controls.TagType != ber.TypeConstructed || controls.Tag != 0 || len(controls.Children) > 1 {
			return 0, ErrProtocol
		}
		for _, control := range controls.Children {
			if control.ClassType != ber.ClassUniversal || control.TagType != ber.TypeConstructed || control.Tag != ber.TagSequence || len(control.Children) < 2 || len(control.Children) > 3 || !isString(control.Children[0]) {
				return 0, ErrProtocol
			}
			if control.Children[0].Value.(string) != "1.2.840.113556.1.4.319" {
				return 0, ErrProtocol
			}
			if len(control.Children) == 3 {
				critical := control.Children[1]
				if _, ok := critical.Value.(bool); !ok || critical.ClassType != ber.ClassUniversal || critical.TagType != ber.TypePrimitive || critical.Tag != ber.TagBoolean {
					return 0, ErrProtocol
				}
			}
			value := control.Children[len(control.Children)-1]
			if !isString(value) {
				return 0, ErrProtocol
			}
			encoded := value.Data.Bytes()
			if err := validateBER(encoded, o); err != nil {
				return 0, err
			}
			paging, err := ber.DecodePacketErr(encoded)
			if err != nil || paging.Tag != ber.TagSequence || len(paging.Children) != 2 || paging.Children[0].Tag != ber.TagInteger || !isString(paging.Children[1]) || len(paging.Children[1].Data.Bytes()) > 4096 {
				return 0, ErrProtocol
			}
			count, ok := paging.Children[0].Value.(int64)
			if !ok || count < 0 || count > 4294967295 || paging.ClassType != ber.ClassUniversal || paging.TagType != ber.TypeConstructed || len(paging.Bytes()) != len(encoded) {
				return 0, ErrProtocol
			}
		}
	}
	return op.Tag, nil
}

func readFrame(r io.Reader, o Options, remaining int) ([]byte, error) {
	var header [6]byte
	if _, err := io.ReadFull(r, header[:2]); err != nil {
		return nil, err
	}
	if header[0] != 0x30 {
		return nil, ErrProtocol
	}
	headerSize, length := 2, int(header[1])
	if header[1]&0x80 != 0 {
		n := int(header[1] & 0x7f)
		if n == 0 || n > 4 {
			return nil, ErrProtocol
		}
		if _, err := io.ReadFull(r, header[2:2+n]); err != nil {
			return nil, err
		}
		headerSize += n
		var value uint64
		for _, b := range header[2:headerSize] {
			value = value<<8 | uint64(b)
		}
		if value > uint64(o.MaxPacketBytes) {
			return nil, ErrLimit
		}
		length = int(value)
	}
	if length > o.MaxPacketBytes-headerSize || headerSize > remaining || length > remaining-headerSize {
		return nil, ErrLimit
	}
	frame := make([]byte, headerSize+length)
	copy(frame, header[:headerSize])
	if _, err := io.ReadFull(r, frame[headerSize:]); err != nil {
		return nil, err
	}
	if err := validateBER(frame, o); err != nil {
		return nil, err
	}
	return frame, nil
}

// LDAP BER uses definite lengths and no constructed string encodings. Unknown
// high-tag forms are excluded from this read-only bind/search profile.
func validateBER(data []byte, o Options) error {
	type span struct{ start, end, depth int }
	stack := []span{{0, len(data), 0}}
	nodes := 0
	for len(stack) > 0 {
		part := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for at := part.start; at < part.end; {
			if part.end-at < 2 || data[at]&0x1f == 0x1f {
				return ErrProtocol
			}
			tag, size, length := data[at], 2, int(data[at+1])
			if data[at+1]&0x80 != 0 {
				n := int(data[at+1] & 0x7f)
				if n == 0 || n > 4 || part.end-at < 2+n {
					return ErrProtocol
				}
				size += n
				var value uint64
				for _, b := range data[at+2 : at+size] {
					value = value<<8 | uint64(b)
				}
				if value > uint64(part.end-at-size) {
					return ErrProtocol
				}
				length = int(value)
			}
			if length > part.end-at-size {
				return ErrProtocol
			}
			nodes++
			if nodes > o.MaxBERNodes {
				return ErrLimit
			}
			if tag&0x20 != 0 {
				if part.depth >= o.MaxBERDepth {
					return ErrLimit
				}
				// Retain the sibling continuation rather than pushing all siblings.
				if at+size+length < part.end {
					stack = append(stack, span{at + size + length, part.end, part.depth})
				}
				stack = append(stack, span{at + size, at + size + length, part.depth + 1})
				break
			}
			at += size + length
		}
	}
	return nil
}
