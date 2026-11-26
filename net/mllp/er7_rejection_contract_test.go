package mllp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

func TestMLLPParseRejectionDependsOnAddressableHeader(t *testing.T) {
	const marker = "SYNTHETIC-ER7-ERR-41"
	for _, release := range []Release{MLLP1, MLLP2} {
		t.Run(fmt.Sprintf("release-%d", release+1), func(t *testing.T) {
			for _, tc := range []struct {
				name     string
				charset  string
				body     string
				wantCode string
			}{
				{"invalid UTF-8 inside the body", "UNICODE UTF-8", "\xff" + marker, hl7.ErrorCodeDataType},
				{"repetitions exceed parser limit", "8859/7", "A~B~" + marker, hl7.ErrorCodeApplicationInternalError},
				{"bytes exceed parser limit only", "8859/7", strings.Repeat("x", 256) + marker, hl7.ErrorCodeApplicationInternalError},
				{"unknown primary charset prevents addressing a reply", marker, "plain ASCII", ""},
			} {
				t.Run(tc.name, func(t *testing.T) {
					var calls atomic.Int32
					address, log, _ := startServer(t, ServerOptions{
						Release:       release,
						MaxFrameBytes: 1024,
						ParseOptions:  hl7.ParseOptions{MaxBytes: 256, MaxRepetitions: 2},
						Handler: HandlerFunc(func(_ context.Context, request *Request) *hl7.Message {
							calls.Add(1)
							seg, ok := request.Message.Segment("ZXY")
							if !ok || seg.Field(1).String() != "β" {
								return hl7.NewACK(request.Message, hl7.AckError, hl7.AckOptions{})
							}
							return nil
						}),
					})
					// The peer writes raw MLLP frames and an ER7 header with known
					// addresses. Constructing either with the client under test
					// would preclude sending the malformed wire payloads.
					wire := func(id, charset, body string) []byte {
						header := `MSH|^~\&|REG^1.9.1^ISO|NORTH^1.9.2^ISO|VIEW^1.9.3^ISO|SOUTH^1.9.4^ISO|20261115100500||ADT^A08^ADT_A01|`
						return []byte(header + id + "|T|2.4||||||" + charset + "\rZXY|" + body + "\r")
					}
					conn, reader := openER7TestConnection(t, address)
					got, err := exchangeER7TestPayload(t, release, conn, reader, wire("BAD-11", tc.charset, tc.body))
					if tc.wantCode == "" {
						if got != nil || !errors.Is(err, io.EOF) {
							t.Fatalf("unaddressable MSH produced %d response bytes or failed to close: %v", len(got), err)
						}
						conn, reader = openER7TestConnection(t, address)
					} else {
						if err != nil || bytes.Contains(got, []byte(marker)) {
							t.Fatalf("addressable rejection failed or exposed content: %v", err)
						}
						checkER7AddressedACK(t, got, "BAD-11", tc.charset, hl7.AckReject, tc.wantCode)
					}
					if calls.Load() != 0 {
						t.Fatal("invalid ER7 reached the application handler")
					}
					got, err = exchangeER7TestPayload(t, release, conn, reader, wire("GOOD-12", "8859/7", "\xe2"))
					if err != nil {
						t.Fatalf("subsequent exchange failed: %v", err)
					}
					checkER7AddressedACK(t, got, "GOOD-12", "8859/7", hl7.AckAccept, "")
					if diagnostics := log.all(); calls.Load() != 1 || len(diagnostics) != 1 || strings.Contains(diagnostics[0], marker) {
						t.Fatalf("handler ran %d times and OnError reported %d failures; want one valid execution and one redacted failure", calls.Load(), len(diagnostics))
					}
				})
			}
		})
	}
}

func openER7TestConnection(t *testing.T, address string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn, bufio.NewReader(conn)
}

func exchangeER7TestPayload(t *testing.T, release Release, conn net.Conn, reader *bufio.Reader, payload []byte) ([]byte, error) {
	t.Helper()
	if err := rawWrite(conn, rawFrameBytes(payload)); err != nil {
		t.Fatal(err)
	}
	if release == MLLP2 {
		if err := expectRawACK(reader); err != nil {
			t.Fatalf("expected a transport confirmation for the complete frame: %v", err)
		}
	}
	response, err := rawReadFrame(reader)
	if err == nil && release == MLLP2 {
		if err := rawWrite(conn, rawFrameBytes([]byte{0x06})); err != nil {
			t.Fatal(err)
		}
	}
	return response, err
}

func checkER7AddressedACK(t *testing.T, wire []byte, id, charset string, code hl7.AckCode, errorCode string) {
	t.Helper()
	// Check the addresses on the wire independently of the header reader.
	header, _, _ := bytes.Cut(wire, []byte("\r"))
	fields := bytes.Split(header, []byte("|"))
	if len(fields) != 18 || string(fields[2]) != "VIEW^1.9.3^ISO" || string(fields[3]) != "SOUTH^1.9.4^ISO" ||
		string(fields[4]) != "REG^1.9.1^ISO" || string(fields[5]) != "NORTH^1.9.2^ISO" ||
		string(fields[8]) != "ACK^A08^ACK" || string(fields[10]) != "T" || string(fields[11]) != "2.4" || string(fields[17]) != charset {
		t.Fatalf("ACK did not preserve addressing, processing ID, version, and charset: %q", header)
	}
	message, err := hl7.Parse(wire)
	if err != nil {
		t.Fatal(err)
	}
	ack := ackOf(t, message)
	if ack.Code != code || ack.ControlID != id || ack.ErrorCode != errorCode {
		t.Fatalf("ACK code, ID, error = %q, %q, %q; want %q, %q, %q", ack.Code, ack.ControlID, ack.ErrorCode, code, id, errorCode)
	}
}
