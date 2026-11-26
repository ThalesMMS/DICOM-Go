// Package websocket is a small clean-room implementation of the RFC 6455
// WebSocket protocol used by the DICOMweb UPS event channel. It covers the
// opening handshake (server and client), framing with masking, message
// fragmentation, UTF-8 validation of text messages, ping/pong and the closing
// handshake. Extensions and subprotocols are not negotiated.
package websocket

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Opcodes (RFC 6455 section 5.2).
const (
	OpContinuation = 0x0
	OpText         = 0x1
	OpBinary       = 0x2
	OpClose        = 0x8
	OpPing         = 0x9
	OpPong         = 0xA
)

// Close status codes (RFC 6455 section 7.4.1).
const (
	CloseNormal           = 1000
	CloseGoingAway        = 1001
	CloseProtocolError    = 1002
	CloseUnsupportedData  = 1003
	CloseNoStatus         = 1005
	CloseInvalidPayload   = 1007
	ClosePolicyViolation  = 1008
	CloseMessageTooBig    = 1009
	CloseInternalError    = 1011
	maxControlPayload     = 125
	defaultMaxMessageSize = 1 << 20
	acceptGUID            = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	closeWriteTimeout     = time.Second
)

var (
	// ErrHandshake reports an invalid opening handshake.
	ErrHandshake = errors.New("websocket: invalid handshake")
	// ErrProtocol reports a frame that violates RFC 6455.
	ErrProtocol = errors.New("websocket: protocol error")
	// ErrMessageTooLarge reports a message above the configured limit.
	ErrMessageTooLarge = errors.New("websocket: message too large")
	// ErrClosed reports use of a closed connection.
	ErrClosed = errors.New("websocket: connection closed")
)

// CloseError carries the status code and reason of a received close frame.
type CloseError struct {
	Code   int
	Reason string
}

func (e *CloseError) Error() string {
	return fmt.Sprintf("websocket: closed by peer (%d)", e.Code)
}

// Is makes every CloseError match ErrClosed.
func (e *CloseError) Is(target error) bool { return target == ErrClosed }

// AcceptKey returns the Sec-WebSocket-Accept value for a client key.
func AcceptKey(key string) string {
	sum := sha1.Sum([]byte(key + acceptGUID))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Options bounds one connection.
type Options struct {
	// MaxMessageBytes bounds a reassembled incoming message. Zero selects 1 MiB.
	MaxMessageBytes int64
}

// Conn is one WebSocket connection. ReadMessage must be called from one
// goroutine; writes are serialized internally and may come from any goroutine.
type Conn struct {
	conn       net.Conn
	reader     *bufio.Reader
	client     bool
	maxMessage int64

	writeMu   sync.Mutex
	closeOnce sync.Once
	closed    chan struct{}
	sentClose bool
}

func newConn(conn net.Conn, reader *bufio.Reader, client bool, options Options) *Conn {
	if reader == nil {
		reader = bufio.NewReader(conn)
	}
	if options.MaxMessageBytes <= 0 {
		options.MaxMessageBytes = defaultMaxMessageSize
	}
	return &Conn{conn: conn, reader: reader, client: client, maxMessage: options.MaxMessageBytes, closed: make(chan struct{})}
}

// AcceptOptions configures the server side of the opening handshake.
type AcceptOptions struct {
	Options
	// CheckOrigin decides whether a request carrying an Origin header is
	// allowed. Nil allows only an Origin whose host equals the request Host,
	// which blocks cross-site WebSocket hijacking from browsers.
	CheckOrigin func(*http.Request) bool
	// Header is added to the 101 response.
	Header http.Header
}

// Accept validates a client opening handshake, hijacks the HTTP connection
// and returns the WebSocket. On failure it writes an HTTP error response.
func Accept(w http.ResponseWriter, r *http.Request, options AcceptOptions) (*Conn, error) {
	fail := func(status int) (*Conn, error) {
		if status == http.StatusUpgradeRequired {
			w.Header().Set("Sec-WebSocket-Version", "13")
			w.Header().Set("Upgrade", "websocket")
		}
		http.Error(w, http.StatusText(status), status)
		return nil, ErrHandshake
	}
	if r.Method != http.MethodGet || r.ProtoMajor < 1 || (r.ProtoMajor == 1 && r.ProtoMinor < 1) {
		return fail(http.StatusBadRequest)
	}
	if !headerContainsToken(r.Header, "Connection", "upgrade") || !headerContainsToken(r.Header, "Upgrade", "websocket") {
		return fail(http.StatusUpgradeRequired)
	}
	if r.Header.Get("Sec-WebSocket-Version") != "13" {
		return fail(http.StatusUpgradeRequired)
	}
	key := strings.TrimSpace(r.Header.Get("Sec-WebSocket-Key"))
	if decoded, err := base64.StdEncoding.DecodeString(key); err != nil || len(decoded) != 16 {
		return fail(http.StatusBadRequest)
	}
	checkOrigin := options.CheckOrigin
	if checkOrigin == nil {
		checkOrigin = SameOriginOrAbsent
	}
	if !checkOrigin(r) {
		return fail(http.StatusForbidden)
	}
	conn, buffered, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return fail(http.StatusInternalServerError)
	}
	// The HTTP server may have armed deadlines for the request; the
	// WebSocket owner manages its own.
	_ = conn.SetDeadline(time.Time{})
	var response strings.Builder
	response.WriteString("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: ")
	response.WriteString(AcceptKey(key))
	response.WriteString("\r\n")
	for name, values := range options.Header {
		for _, value := range values {
			if strings.ContainsAny(name+value, "\r\n") {
				continue
			}
			response.WriteString(http.CanonicalHeaderKey(name) + ": " + value + "\r\n")
		}
	}
	response.WriteString("\r\n")
	_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
	if _, err := io.WriteString(conn, response.String()); err != nil {
		_ = conn.Close()
		return nil, err
	}
	_ = conn.SetWriteDeadline(time.Time{})
	var reader *bufio.Reader
	if buffered != nil {
		reader = buffered.Reader
	}
	return newConn(conn, reader, false, options.Options), nil
}

// SameOriginOrAbsent allows requests without an Origin header (non-browser
// clients) and browser requests whose Origin host equals the Host header.
func SameOriginOrAbsent(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return true
	}
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return false
	}
	return strings.EqualFold(rest, r.Host)
}

func headerContainsToken(header http.Header, name, token string) bool {
	for _, value := range header.Values(name) {
		for _, field := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(field), token) {
				return true
			}
		}
	}
	return false
}

// NewClientKey returns a random Sec-WebSocket-Key.
func NewClientKey() (string, error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(nonce[:]), nil
}

// ClientHandshake sends request (a GET for the WebSocket resource) on conn
// and validates the 101 response. A non-101 response is returned with a nil
// Conn and ErrHandshake so the caller can map its status; its body is not
// read. The caller closes conn on error.
func ClientHandshake(conn net.Conn, request *http.Request, options Options) (*Conn, *http.Response, error) {
	key, err := NewClientKey()
	if err != nil {
		return nil, nil, err
	}
	request.Header.Set("Upgrade", "websocket")
	request.Header.Set("Connection", "Upgrade")
	request.Header.Set("Sec-WebSocket-Key", key)
	request.Header.Set("Sec-WebSocket-Version", "13")
	if err := request.Write(conn); err != nil {
		return nil, nil, err
	}
	reader := bufio.NewReader(conn)
	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return nil, nil, err
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return nil, response, ErrHandshake
	}
	if !headerContainsToken(response.Header, "Upgrade", "websocket") ||
		!headerContainsToken(response.Header, "Connection", "upgrade") ||
		response.Header.Get("Sec-WebSocket-Accept") != AcceptKey(key) ||
		response.Header.Get("Sec-WebSocket-Extensions") != "" ||
		response.Header.Get("Sec-WebSocket-Protocol") != "" {
		return nil, response, ErrHandshake
	}
	return newConn(conn, reader, true, options), response, nil
}

// SetReadDeadline sets the deadline for the next ReadMessage.
func (c *Conn) SetReadDeadline(t time.Time) error {
	return c.conn.SetReadDeadline(t)
}

// RemoteAddr returns the peer address.
func (c *Conn) RemoteAddr() net.Addr {
	return c.conn.RemoteAddr()
}

// Done is closed when the connection is closed.
func (c *Conn) Done() <-chan struct{} {
	return c.closed
}

type frameHeader struct {
	fin     bool
	opcode  byte
	masked  bool
	mask    [4]byte
	length  uint64
	rsvBits byte
}

func (c *Conn) readFrameHeader() (frameHeader, error) {
	var head [2]byte
	if _, err := io.ReadFull(c.reader, head[:]); err != nil {
		return frameHeader{}, err
	}
	h := frameHeader{
		fin:     head[0]&0x80 != 0,
		rsvBits: head[0] & 0x70,
		opcode:  head[0] & 0x0F,
		masked:  head[1]&0x80 != 0,
	}
	length := uint64(head[1] & 0x7F)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return frameHeader{}, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
		if length < 126 {
			return frameHeader{}, ErrProtocol
		}
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return frameHeader{}, err
		}
		length = binary.BigEndian.Uint64(extended[:])
		if length>>63 != 0 || length <= 0xFFFF {
			return frameHeader{}, ErrProtocol
		}
	}
	h.length = length
	if h.masked {
		if _, err := io.ReadFull(c.reader, h.mask[:]); err != nil {
			return frameHeader{}, err
		}
	}
	return h, nil
}

// ReadMessage returns the next text or binary message. Ping frames are
// answered, pong frames ignored, and a close frame completes the closing
// handshake and returns a *CloseError. Protocol violations close the
// connection with the matching status and return ErrProtocol,
// ErrMessageTooLarge or an invalid-payload error.
func (c *Conn) ReadMessage() (int, []byte, error) {
	var message []byte
	messageOpcode := -1
	for {
		h, err := c.readFrameHeader()
		if err != nil {
			if errors.Is(err, ErrProtocol) {
				c.fail(CloseProtocolError)
			} else {
				c.closeConn()
			}
			return 0, nil, err
		}
		if h.rsvBits != 0 || h.masked == c.client {
			// No extension is negotiated, clients must mask and servers must not.
			c.fail(CloseProtocolError)
			return 0, nil, ErrProtocol
		}
		control := h.opcode >= 0x8
		if control {
			if !h.fin || h.length > maxControlPayload || (h.opcode != OpClose && h.opcode != OpPing && h.opcode != OpPong) {
				c.fail(CloseProtocolError)
				return 0, nil, ErrProtocol
			}
		} else if h.opcode != OpContinuation && h.opcode != OpText && h.opcode != OpBinary {
			c.fail(CloseProtocolError)
			return 0, nil, ErrProtocol
		}
		if !control && uint64(len(message))+h.length > uint64(c.maxMessage) {
			c.fail(CloseMessageTooBig)
			return 0, nil, ErrMessageTooLarge
		}
		payload := make([]byte, h.length)
		if _, err := io.ReadFull(c.reader, payload); err != nil {
			c.closeConn()
			return 0, nil, err
		}
		if h.masked {
			for i := range payload {
				payload[i] ^= h.mask[i%4]
			}
		}
		switch h.opcode {
		case OpPing:
			if err := c.writeFrame(OpPong, payload); err != nil {
				return 0, nil, err
			}
			continue
		case OpPong:
			continue
		case OpClose:
			return 0, nil, c.handleClose(payload)
		case OpContinuation:
			if messageOpcode < 0 {
				c.fail(CloseProtocolError)
				return 0, nil, ErrProtocol
			}
		default:
			if messageOpcode >= 0 {
				c.fail(CloseProtocolError)
				return 0, nil, ErrProtocol
			}
			messageOpcode = int(h.opcode)
		}
		message = append(message, payload...)
		if h.fin {
			if messageOpcode == OpText && !utf8.Valid(message) {
				c.fail(CloseInvalidPayload)
				return 0, nil, ErrProtocol
			}
			return messageOpcode, message, nil
		}
	}
}

func (c *Conn) handleClose(payload []byte) error {
	code := CloseNoStatus
	reason := ""
	switch {
	case len(payload) == 1:
		c.fail(CloseProtocolError)
		return ErrProtocol
	case len(payload) >= 2:
		code = int(binary.BigEndian.Uint16(payload[:2]))
		reason = string(payload[2:])
		if !validCloseCode(code) || !utf8.ValidString(reason) {
			c.fail(CloseProtocolError)
			return ErrProtocol
		}
	}
	reply := CloseNormal
	if code != CloseNoStatus {
		reply = code
	}
	c.sendCloseAndClose(reply, "")
	return &CloseError{Code: code, Reason: reason}
}

func validCloseCode(code int) bool {
	switch {
	case code >= 1000 && code <= 1003, code >= 1007 && code <= 1011:
		return true
	case code >= 3000 && code <= 4999:
		return true
	}
	return false
}

// WriteMessage sends one unfragmented text or binary message.
func (c *Conn) WriteMessage(opcode int, data []byte) error {
	if opcode != OpText && opcode != OpBinary {
		return ErrProtocol
	}
	if opcode == OpText && !utf8.Valid(data) {
		return ErrProtocol
	}
	return c.writeFrame(byte(opcode), data)
}

// WritePing sends a ping control frame.
func (c *Conn) WritePing(data []byte) error {
	if len(data) > maxControlPayload {
		return ErrProtocol
	}
	return c.writeFrame(OpPing, data)
}

// SetWriteDeadline bounds subsequent writes.
func (c *Conn) SetWriteDeadline(t time.Time) error {
	return c.conn.SetWriteDeadline(t)
}

func (c *Conn) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	return c.writeFrameLocked(opcode, payload)
}

func (c *Conn) writeFrameLocked(opcode byte, payload []byte) error {
	select {
	case <-c.closed:
		return ErrClosed
	default:
	}
	if c.sentClose {
		return ErrClosed
	}
	header := make([]byte, 0, 14)
	header = append(header, 0x80|opcode)
	maskBit := byte(0)
	if c.client {
		maskBit = 0x80
	}
	switch length := len(payload); {
	case length <= 125:
		header = append(header, maskBit|byte(length))
	case length <= 0xFFFF:
		header = append(header, maskBit|126, byte(length>>8), byte(length))
	default:
		header = append(header, maskBit|127)
		header = binary.BigEndian.AppendUint64(header, uint64(length))
	}
	body := payload
	if c.client {
		var mask [4]byte
		if _, err := rand.Read(mask[:]); err != nil {
			return err
		}
		header = append(header, mask[:]...)
		body = make([]byte, len(payload))
		for i := range payload {
			body[i] = payload[i] ^ mask[i%4]
		}
	}
	if _, err := c.conn.Write(append(header, body...)); err != nil {
		c.closeConn()
		return err
	}
	if opcode == OpClose {
		c.sentClose = true
	}
	return nil
}

// Close sends a close frame with code and reason (best effort, bounded by a
// short deadline) and closes the connection. It is idempotent.
func (c *Conn) Close(code int, reason string) error {
	c.sendCloseAndClose(code, reason)
	return nil
}

func (c *Conn) fail(code int) {
	c.sendCloseAndClose(code, "")
}

func (c *Conn) sendCloseAndClose(code int, reason string) {
	if len(reason) > maxControlPayload-2 {
		reason = reason[:maxControlPayload-2]
	}
	payload := binary.BigEndian.AppendUint16(nil, uint16(code))
	payload = append(payload, reason...)
	c.writeMu.Lock()
	if !c.sentClose {
		_ = c.conn.SetWriteDeadline(time.Now().Add(closeWriteTimeout))
		_ = c.writeFrameLocked(OpClose, payload)
	}
	c.writeMu.Unlock()
	c.closeConn()
}

func (c *Conn) closeConn() {
	c.closeOnce.Do(func() {
		close(c.closed)
		_ = c.conn.Close()
	})
}
