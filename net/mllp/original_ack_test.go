package mllp

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ThalesMMS/dicom-go/hl7"
)

func TestOriginalACKPreservesWireBytes(t *testing.T) {
	request := bytes.ReplaceAll(adt("ORIGINAL", "SYNTHETIC"), []byte("\r"), []byte("\r\n"))
	reply := &atomic.Pointer[[]byte]{}
	address, _, _ := startServer(t, ServerOptions{OriginalACKHandler: func(_ context.Context, parsed *Request, wire []byte) ([]byte, error) {
		if !bytes.Equal(wire, request) || parsed.Message.Header().ControlID != "ORIGINAL" {
			return nil, errors.New("wrong wire message")
		}
		ack, err := hl7.NewACK(parsed.Message, hl7.AckAccept, hl7.AckOptions{}).Marshal()
		if err != nil {
			return nil, err
		}
		// Deliberately produce a valid reply without the canonical CR layout.
		wireACK := bytes.ReplaceAll(ack, []byte("\r"), []byte("\r\n"))
		reply.Store(&wireACK)
		return wireACK, nil
	}})
	got, err := rawExchange(t, address, frame(request))
	want := reply.Load()
	if err != nil || want == nil || !bytes.Equal(got, *want) || originalWireACK(t, got).Code != hl7.AckAccept {
		t.Fatalf("wire ACK was not preserved (read error: %v)", err)
	}
}

func TestOriginalACKRejectsEnhancedAndAmbiguousRequests(t *testing.T) {
	var calls atomic.Int32
	address, _, _ := startServer(t, ServerOptions{OriginalACKHandler: func(context.Context, *Request, []byte) ([]byte, error) {
		calls.Add(1)
		return nil, errors.New("must not reach handler")
	}})
	enhanced := bytes.Replace(adt("ORIGINAL", "SYNTHETIC"), []byte("2.5.1\r"), []byte("2.5.1|||AL\r"), 1)
	malformedPolicy := bytes.Replace(enhanced, []byte("AL\r"), []byte("UNKNOWN\r"), 1)
	for _, wire := range [][]byte{
		enhanced, malformedPolicy,
		adt("", "SYNTHETIC"), adt(hl7.Null, "SYNTHETIC"), adt("A~B", "SYNTHETIC"), adt("A^B", "SYNTHETIC"),
	} {
		got, err := rawExchange(t, address, frame(wire))
		if err != nil || originalWireACK(t, got).Code != hl7.AckReject {
			t.Fatalf("request was not rejected before the handler (read error: %v)", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("handler invoked %d times", calls.Load())
	}
}

func TestOriginalACKRejectsUnsafeHandlerResults(t *testing.T) {
	tests := []struct {
		name    string
		respond OriginalACKHandler
	}{
		{"callback error", func(context.Context, *Request, []byte) ([]byte, error) {
			return nil, errors.New("synthetic private field")
		}},
		{"callback panic", func(context.Context, *Request, []byte) ([]byte, error) {
			panic("synthetic private field")
		}},
		{"wrong message", func(_ context.Context, _ *Request, wire []byte) ([]byte, error) { return wire, nil }},
		{"mismatched control ID", func(_ context.Context, request *Request, _ []byte) ([]byte, error) {
			ack := hl7.NewACK(request.Message, hl7.AckAccept, hl7.AckOptions{})
			msa, _ := ack.Segment("MSA")
			msa.SetValue(2, "OTHER")
			return ack.Marshal()
		}},
		{"enhanced code", func(_ context.Context, request *Request, _ []byte) ([]byte, error) {
			return hl7.NewACK(request.Message, hl7.AckCommitAccept, hl7.AckOptions{}).Marshal()
		}},
		{"ambiguous MSA", func(_ context.Context, request *Request, _ []byte) ([]byte, error) {
			ack := hl7.NewACK(request.Message, hl7.AckAccept, hl7.AckOptions{})
			ack.Add("MSA").SetValue(1, "AR")
			return ack.Marshal()
		}},
		{"oversized ACK", func(context.Context, *Request, []byte) ([]byte, error) {
			return bytes.Repeat([]byte{0x41}, 2049), nil
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			address, log, _ := startServer(t, ServerOptions{MaxFrameBytes: 2048, OriginalACKHandler: test.respond})
			got, err := rawExchange(t, address, frame(adt("ORIGINAL", "SYNTHETIC")))
			if err != nil {
				t.Fatal(err)
			}
			ack := originalWireACK(t, got)
			if ack.Code != hl7.AckError || ack.ControlID != "ORIGINAL" || bytes.Contains(got, []byte("private field")) {
				t.Fatalf("unsafe result became success or disclosed content")
			}
			if reports := log.all(); len(reports) != 1 || strings.Contains(reports[0], "private field") {
				t.Fatal("handler failure was absent from diagnostics or exposed content")
			}
		})
	}
}

func TestOriginalACKRejectsIncompatibleServerOptions(t *testing.T) {
	callback := func(context.Context, *Request, []byte) ([]byte, error) { return nil, nil }
	if err := Serve(context.Background(), nil, ServerOptions{OriginalACKHandler: callback, Release: MLLP2}); !errors.Is(err, ErrUnsupportedRelease) {
		t.Fatalf("MLLP2 with original ACK callback: %v", err)
	}
	ordinary := HandlerFunc(func(context.Context, *Request) *hl7.Message { return nil })
	if err := Serve(context.Background(), nil, ServerOptions{Handler: ordinary, OriginalACKHandler: callback}); err == nil {
		t.Fatal("two handlers were accepted")
	}
}

func originalWireACK(t *testing.T, data []byte) hl7.Acknowledgment {
	t.Helper()
	message, err := hl7.Parse(data)
	if err != nil {
		t.Fatal(err)
	}
	return ackOf(t, message)
}
