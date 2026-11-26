package mllp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

func TestReferenceMLLPFrameSplitPoints(t *testing.T) {
	first := []byte("Aé\\F\\\rB")
	second := []byte{0xb3, 0x5c, 0xff, ':', 'C'}
	wire := append(rawFrameBytes(first), rawFrameBytes(second)...)
	for split := 0; split <= len(wire); split++ {
		t.Run(fmt.Sprintf("byte-%02d", split), func(t *testing.T) {
			chunks := io.MultiReader(bytes.NewReader(wire[:split]), bytes.NewReader(wire[split:]))
			reader := NewReader(iotest.DataErrReader(chunks), len(first))
			for i, want := range [][]byte{first, second} {
				got, err := reader.ReadFrame()
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("frame %d = %x, %v, want %x", i, got, err, want)
				}
			}
			if got, err := reader.ReadFrame(); got != nil || err != io.EOF {
				t.Fatalf("end of stream = %x, %v, want nil and EOF", got, err)
			}
		})
	}
}

func TestReferenceMLLPFramePayloadBudgetPerMessage(t *testing.T) {
	// Place the end block and final CR on either side of the reader's
	// buffering boundary, then test a payload requiring multiple refills.
	const bufferBoundary = 32 << 10
	for _, limit := range []int{bufferBoundary - 3, bufferBoundary - 2, bufferBoundary - 1, bufferBoundary, 67 << 10} {
		t.Run(fmt.Sprintf("limit-%d", limit), func(t *testing.T) {
			first := bytes.Repeat([]byte{'x'}, limit)
			second := bytes.Repeat([]byte{'y'}, limit)
			oversize := bytes.Repeat([]byte{'z'}, limit+1)
			wire := rawFrameBytes(first)
			wire = append(wire, rawFrameBytes(second)...)
			wire = append(wire, rawFrameBytes(oversize)...)
			reader := NewReader(bytes.NewReader(wire), limit)
			for i, want := range [][]byte{first, second} {
				got, err := reader.ReadFrame()
				if err != nil || !bytes.Equal(got, want) {
					t.Fatalf("exact-boundary frame %d = %d bytes, %v, want %d payload bytes", i, len(got), err, len(want))
				}
			}
			if got, err := reader.ReadFrame(); got != nil || !errors.Is(err, ErrFrameTooLarge) {
				t.Fatalf("over-budget frame = %d bytes, %v, want nil and ErrFrameTooLarge", len(got), err)
			}
		})
	}
}

func TestReferenceMLLPFrameRejectsIncompletePayload(t *testing.T) {
	complete := []byte("retained")
	partial := rawFrameBytes([]byte("case-ø"))
	for prefix := 0; prefix < len(partial); prefix++ {
		t.Run(fmt.Sprintf("prefix-%02d", prefix), func(t *testing.T) {
			wire := append(rawFrameBytes(complete), partial[:prefix]...)
			reader := NewReader(iotest.DataErrReader(bytes.NewReader(wire)), 32)
			if got, err := reader.ReadFrame(); err != nil || !bytes.Equal(got, complete) {
				t.Fatalf("complete frame = %x, %v, want %x", got, err, complete)
			}
			want := io.ErrUnexpectedEOF
			if prefix == 0 {
				want = io.EOF
			}
			if got, err := reader.ReadFrame(); got != nil || !errors.Is(err, want) {
				t.Fatalf("truncated second frame = %d bytes, %v, want nil and %v", len(got), err, want)
			}
		})
	}
}

func TestReferenceMLLPServerFailureEvidenceIsContentFree(t *testing.T) {
	const marker = "PRIVATE-MLLP-CASE-417"
	encodedMarker := hex.EncodeToString([]byte(marker))
	checkRedacted := func(t *testing.T, text string) {
		t.Helper()
		if strings.Contains(text, marker) || strings.Contains(strings.ToLower(text), encodedMarker) {
			t.Fatal("failure evidence contains synthetic clinical content")
		}
	}
	for _, release := range []Release{MLLP1, MLLP2} {
		t.Run(fmt.Sprintf("release-%d", release+1), func(t *testing.T) {
			var calls atomic.Int32
			address, log, _ := startServer(t, ServerOptions{Release: release, Handler: HandlerFunc(func(_ context.Context, _ *Request) *hl7.Message {
				if calls.Add(1) == 1 {
					panic(errors.New(marker))
				}
				return nil
			})})
			conn, err := net.DialTimeout("tcp", address, 3*time.Second)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			if err := conn.SetDeadline(time.Now().Add(5 * time.Second)); err != nil {
				t.Fatal(err)
			}
			reader := bufio.NewReader(conn)
			for _, test := range []struct {
				id      string
				payload []byte
				code    hl7.AckCode
				errCode string
				calls   int32
			}{
				{"bad-segment", append(adt("bad-segment", marker), []byte("bad|"+marker+"\r")...), hl7.AckReject, hl7.ErrorCodeDataType, 0},
				{"panicked-handler", adt("panicked-handler", marker), hl7.AckReject, hl7.ErrorCodeApplicationInternalError, 1},
				{"next-valid-message", adt("next-valid-message", marker), hl7.AckAccept, "", 2},
			} {
				if err := rawWrite(conn, rawFrameBytes(test.payload)); err != nil {
					t.Fatal(err)
				}
				if release == MLLP2 {
					if err := expectRawACK(reader); err != nil {
						t.Fatal(err)
					}
				}
				wire, err := rawReadFrame(reader)
				if err != nil {
					t.Fatal(err)
				}
				checkRedacted(t, string(wire))
				message, err := hl7.Parse(wire)
				if err != nil {
					t.Fatal(err)
				}
				ack := ackOf(t, message)
				if ack.ControlID != test.id || ack.Code != test.code || ack.ErrorCode != test.errCode || calls.Load() != test.calls {
					t.Fatalf("%s: ACK control ID/code/error = %q/%q/%q, handler calls = %d", test.id, ack.ControlID, ack.Code, ack.ErrorCode, calls.Load())
				}
				if release == MLLP2 {
					if err := rawWrite(conn, rawFrameBytes([]byte{0x06})); err != nil {
						t.Fatal(err)
					}
				}
			}
			logged := log.all()
			if len(logged) != 2 {
				t.Fatalf("logged %d failures, want 2", len(logged))
			}
			for _, diagnostic := range logged {
				checkRedacted(t, diagnostic)
			}
		})
	}
}
