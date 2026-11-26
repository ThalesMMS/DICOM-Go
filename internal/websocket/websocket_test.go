package websocket

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestAcceptKeyMatchesRFCExample(t *testing.T) {
	// RFC 6455 section 1.3.
	if got := AcceptKey("dGhlIHNhbXBsZSBub25jZQ=="); got != "s3pPLMBiTxaQ9kYGzzhZRbK+xOo=" {
		t.Fatalf("AcceptKey = %q", got)
	}
}

type echoServer struct {
	*httptest.Server
	accepted chan *Conn
}

func newEchoServer(t *testing.T, options AcceptOptions) *echoServer {
	t.Helper()
	server := &echoServer{accepted: make(chan *Conn, 1)}
	server.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := Accept(w, r, options)
		if err != nil {
			return
		}
		server.accepted <- conn
		go func() {
			for {
				opcode, message, err := conn.ReadMessage()
				if err != nil {
					return
				}
				if err := conn.WriteMessage(opcode, message); err != nil {
					return
				}
			}
		}()
	}))
	t.Cleanup(server.Close)
	return server
}

func dial(t *testing.T, server *httptest.Server, mutate func(*http.Request)) (*Conn, *http.Response, error) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, server.URL+"/channel", nil)
	if mutate != nil {
		mutate(request)
	}
	ws, response, err := ClientHandshake(conn, request, Options{MaxMessageBytes: 1 << 16})
	if err != nil {
		conn.Close()
	}
	return ws, response, err
}

func TestRoundTripTextBinaryAndLargeMessages(t *testing.T) {
	server := newEchoServer(t, AcceptOptions{Options: Options{MaxMessageBytes: 1 << 20}})
	client, _, err := dial(t, server.Server, nil)
	if err != nil {
		t.Fatalf("handshake: %v", err)
	}
	defer client.Close(CloseNormal, "")
	messages := []struct {
		opcode int
		data   []byte
	}{
		{OpText, []byte("hello")},
		{OpText, []byte(strings.Repeat("é", 100))},
		{OpBinary, bytes.Repeat([]byte{0, 1, 2}, 300)},   // 16-bit length
		{OpBinary, bytes.Repeat([]byte{7}, (1<<16)-100)}, // within the client limit
		{OpText, nil},
	}
	for _, m := range messages {
		if err := client.WriteMessage(m.opcode, m.data); err != nil {
			t.Fatal(err)
		}
		opcode, data, err := client.ReadMessage()
		if err != nil || opcode != m.opcode || !bytes.Equal(data, m.data) {
			t.Fatalf("echo opcode=%d len=%d err=%v", opcode, len(data), err)
		}
	}
	if err := client.WriteMessage(OpText, []byte{0xff}); !errors.Is(err, ErrProtocol) {
		t.Fatalf("invalid UTF-8 text write = %v", err)
	}
}

func TestHandshakeRejections(t *testing.T) {
	server := newEchoServer(t, AcceptOptions{})
	tests := []struct {
		name   string
		mutate func(*http.Request)
		status int
	}{
		{"cross-site origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, http.StatusForbidden},
		{"same origin", func(r *http.Request) { r.Header.Set("Origin", "http://"+r.Host) }, http.StatusSwitchingProtocols},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ws, response, err := dial(t, server.Server, test.mutate)
			if test.status == http.StatusSwitchingProtocols {
				if err != nil {
					t.Fatalf("handshake: %v", err)
				}
				ws.Close(CloseNormal, "")
				return
			}
			if !errors.Is(err, ErrHandshake) || response == nil || response.StatusCode != test.status {
				t.Fatalf("handshake = %v, response %v", err, response)
			}
		})
	}
	for name, header := range map[string]http.Header{
		"no upgrade":  {"Connection": {"keep-alive"}},
		"old version": {"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"8"}, "Sec-Websocket-Key": {"dGhlIHNhbXBsZSBub25jZQ=="}},
		"short key":   {"Connection": {"Upgrade"}, "Upgrade": {"websocket"}, "Sec-Websocket-Version": {"13"}, "Sec-Websocket-Key": {"c2hvcnQ="}},
	} {
		request, _ := http.NewRequest(http.MethodGet, server.URL, nil)
		request.Header = header
		response, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		response.Body.Close()
		if response.StatusCode == http.StatusSwitchingProtocols || response.StatusCode < 400 {
			t.Errorf("%s: status %d", name, response.StatusCode)
		}
	}
}

// rawFrame builds a frame as a peer would send it.
func rawFrame(fin bool, opcode byte, payload []byte, mask bool) []byte {
	first := opcode
	if fin {
		first |= 0x80
	}
	out := []byte{first}
	maskBit := byte(0)
	if mask {
		maskBit = 0x80
	}
	switch {
	case len(payload) <= 125:
		out = append(out, maskBit|byte(len(payload)))
	case len(payload) <= 0xFFFF:
		out = append(out, maskBit|126)
		out = binary.BigEndian.AppendUint16(out, uint16(len(payload)))
	default:
		out = append(out, maskBit|127)
		out = binary.BigEndian.AppendUint64(out, uint64(len(payload)))
	}
	if mask {
		key := [4]byte{1, 2, 3, 4}
		out = append(out, key[:]...)
		for i, b := range payload {
			out = append(out, b^key[i%4])
		}
		return out
	}
	return append(out, payload...)
}

func serverPipe(t *testing.T, maxMessage int64) (*Conn, net.Conn) {
	t.Helper()
	serverSide, peer := net.Pipe()
	t.Cleanup(func() { serverSide.Close(); peer.Close() })
	return newConn(serverSide, bufio.NewReader(serverSide), false, Options{MaxMessageBytes: maxMessage}), peer
}

func readPeerFrame(t *testing.T, peer net.Conn) (byte, []byte) {
	t.Helper()
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	var head [2]byte
	if _, err := io.ReadFull(peer, head[:]); err != nil {
		t.Fatalf("read frame: %v", err)
	}
	if head[1]&0x80 != 0 {
		t.Fatal("server frames must not be masked")
	}
	payload := make([]byte, head[1]&0x7F)
	if _, err := io.ReadFull(peer, payload); err != nil {
		t.Fatal(err)
	}
	return head[0] & 0x0F, payload
}

func TestServerReassemblesFragmentsAndAnswersPing(t *testing.T) {
	conn, peer := serverPipe(t, 1024)
	done := make(chan struct{})
	var opcode int
	var message []byte
	var readErr error
	go func() {
		opcode, message, readErr = conn.ReadMessage()
		close(done)
	}()
	go func() {
		peer.Write(rawFrame(false, OpText, []byte("hel"), true))
		peer.Write(rawFrame(true, OpPing, []byte("p"), true))
		peer.Write(rawFrame(true, OpContinuation, []byte("lo"), true))
	}()
	op, payload := readPeerFrame(t, peer)
	if op != OpPong || string(payload) != "p" {
		t.Fatalf("pong = %d %q", op, payload)
	}
	<-done
	if readErr != nil || opcode != OpText || string(message) != "hello" {
		t.Fatalf("message = %d %q %v", opcode, message, readErr)
	}
}

func TestServerProtocolViolationsCloseWithStatus(t *testing.T) {
	tests := []struct {
		name   string
		frames [][]byte
		code   uint16
		want   error
	}{
		{"unmasked client frame", [][]byte{rawFrame(true, OpText, []byte("x"), false)}, CloseProtocolError, ErrProtocol},
		{"reserved opcode", [][]byte{rawFrame(true, 0x3, nil, true)}, CloseProtocolError, ErrProtocol},
		{"fragmented control", [][]byte{rawFrame(false, OpPing, nil, true)}, CloseProtocolError, ErrProtocol},
		{"orphan continuation", [][]byte{rawFrame(true, OpContinuation, []byte("x"), true)}, CloseProtocolError, ErrProtocol},
		{"invalid UTF-8", [][]byte{rawFrame(true, OpText, []byte{0xc3, 0x28}, true)}, CloseInvalidPayload, ErrProtocol},
		{"too large", [][]byte{rawFrame(true, OpBinary, make([]byte, 200), true)}, CloseMessageTooBig, ErrMessageTooLarge},
		{"bad close code", [][]byte{rawFrame(true, OpClose, []byte{0x03, 0xED}, true)}, CloseProtocolError, ErrProtocol},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			conn, peer := serverPipe(t, 100)
			result := make(chan error, 1)
			go func() {
				_, _, err := conn.ReadMessage()
				result <- err
			}()
			go func() {
				for _, frame := range test.frames {
					peer.Write(frame)
				}
			}()
			op, payload := readPeerFrame(t, peer)
			if op != OpClose || len(payload) < 2 || binary.BigEndian.Uint16(payload) != test.code {
				t.Fatalf("close frame = %d %v", op, payload)
			}
			if err := <-result; !errors.Is(err, test.want) {
				t.Fatalf("ReadMessage = %v, want %v", err, test.want)
			}
		})
	}
}

func TestClosingHandshakeEchoesCode(t *testing.T) {
	conn, peer := serverPipe(t, 100)
	result := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadMessage()
		result <- err
	}()
	go peer.Write(rawFrame(true, OpClose, append(binary.BigEndian.AppendUint16(nil, CloseGoingAway), "bye"...), true))
	op, payload := readPeerFrame(t, peer)
	if op != OpClose || binary.BigEndian.Uint16(payload) != CloseGoingAway {
		t.Fatalf("close reply = %d %v", op, payload)
	}
	err := <-result
	var closeErr *CloseError
	if !errors.As(err, &closeErr) || closeErr.Code != CloseGoingAway || closeErr.Reason != "bye" || !errors.Is(err, ErrClosed) {
		t.Fatalf("ReadMessage = %v", err)
	}
	if err := conn.WriteMessage(OpText, []byte("late")); !errors.Is(err, ErrClosed) {
		t.Fatalf("write after close = %v", err)
	}
	select {
	case <-conn.Done():
	default:
		t.Fatal("Done not closed")
	}
}

func TestClientRejectsMaskedServerFrames(t *testing.T) {
	clientSide, peer := net.Pipe()
	defer clientSide.Close()
	defer peer.Close()
	conn := newConn(clientSide, nil, true, Options{})
	result := make(chan error, 1)
	go func() {
		_, _, err := conn.ReadMessage()
		result <- err
	}()
	go peer.Write(rawFrame(true, OpText, []byte("x"), true))
	_ = peer.SetReadDeadline(time.Now().Add(2 * time.Second))
	head := make([]byte, 2)
	if _, err := io.ReadFull(peer, head); err != nil || head[0]&0x0F != OpClose || head[1]&0x80 == 0 {
		t.Fatalf("client close frame %v (masked bit required) err=%v", head, err)
	}
	if err := <-result; !errors.Is(err, ErrProtocol) {
		t.Fatalf("ReadMessage = %v", err)
	}
}
