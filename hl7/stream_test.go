package hl7

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"testing/iotest"
)

func TestParseStreamAtEveryWireSplit(t *testing.T) {
	utf8Message := []byte("MSH|^~\\&|SRC|TEST|DST|TEST|||ADT^A08|ONE|P|2.5.1||||||UNICODE UTF-8\r\n" +
		"OBX|1|ST|CODE||Érable\\F\\東京|\\XC3A9\\\n")
	latinMessage := []byte("MSH|^~\\&|SRC|TEST|DST|TEST|||ADT^A08|TWO|P|2.5.1||||||8859/1\r" +
		"OBX|1|ST|CODE||Andr\xe9\\F\\Vale|\\XE9\\\r")
	big5Message := []byte("MSH|^~\\&|SRC|TEST|DST|TEST|||ADT^A08|THREE|P|2.5.1||||||BIG-5\r" +
		"OBX|1|ST|CODE||\xb3\x5c^A")
	var want []*Message
	var wire []byte
	for _, data := range [][]byte{utf8Message, latinMessage, big5Message} {
		msg, err := Parse(data)
		if err != nil {
			t.Fatal(err)
		}
		want = append(want, msg)
		wire = append(wire, data...)
	}
	for split := 0; split <= len(wire); split++ {
		reader := io.MultiReader(bytes.NewReader(wire[:split]), bytes.NewReader(wire[split:]))
		var got []*Message
		err := ParseStream(context.Background(), reader, ParseOptions{}, func(m *Message) error {
			got = append(got, m)
			return nil
		})
		if err != nil || !reflect.DeepEqual(got, want) {
			t.Fatalf("split %d: got %d messages, %v; want %d unchanged messages", split, len(got), err, len(want))
		}
	}
}

func TestParseStreamMessageLimitAndErrorPosition(t *testing.T) {
	first := message(`MSH|^~\&|FIRST`, "OBX|1|ST|CODE||SEAM")
	sameLength := bytes.Replace(first, []byte("FIRST"), []byte("OTHER"), 1)
	for _, tc := range []struct {
		name       string
		second     []byte
		limit      int
		wantCount  int
		wantErr    error
		wantOffset int64
	}{
		{name: "each message at byte limit", second: sameLength, limit: len(first), wantCount: 2},
		{name: "second message one byte over", second: append(bytes.Clone(sameLength), '\n'), limit: len(first), wantCount: 1, wantErr: ErrLimit, wantOffset: int64(len(first))},
		{name: "first message one byte over", second: sameLength, limit: len(first) - 1, wantErr: ErrLimit},
		{name: "second malformed header", second: []byte("MSH|^^|SECRET-CLINICAL-TEXT\r"), limit: len(first), wantCount: 1, wantErr: ErrSyntax, wantOffset: int64(len(first))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var count int
			err := ParseStream(context.Background(), io.MultiReader(bytes.NewReader(first), bytes.NewReader(tc.second)), ParseOptions{MaxBytes: tc.limit}, func(*Message) error {
				count++
				return nil
			})
			if count != tc.wantCount || !errors.Is(err, tc.wantErr) {
				t.Fatalf("messages = %d and error = %v; want %d and %v", count, err, tc.wantCount, tc.wantErr)
			}
			if tc.wantErr != nil {
				var detail *StreamError
				if !errors.As(err, &detail) || detail.Message != int64(tc.wantCount+1) || detail.Offset != tc.wantOffset {
					t.Fatalf("incorrect stream position: %v", err)
				}
				if strings.Contains(err.Error(), "SECRET-CLINICAL-TEXT") {
					t.Fatalf("unexpected input value in error: %v", err)
				}
			}
		})
	}
}

func TestParseStreamCallbackCancellationAndReadFailure(t *testing.T) {
	wire := message(`MSH|^~\&|ONE`, "PID|1", `MSH|^~\&|TWO`, "PID|2")
	callbackErr := errors.New("stop at callback")
	err := ParseStream(context.Background(), iotest.OneByteReader(bytes.NewReader(wire)), ParseOptions{}, func(*Message) error {
		return callbackErr
	})
	if err != callbackErr {
		t.Fatalf("callback error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbacks := 0
	err = ParseStream(ctx, iotest.OneByteReader(bytes.NewReader(wire)), ParseOptions{}, func(*Message) error {
		callbacks++
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) || callbacks != 1 {
		t.Fatalf("canceled stream: %d callbacks, %v", callbacks, err)
	}

	callbacks = 0
	const secret = "source read contains SECRET-CLINICAL-TEXT"
	withReadError := io.MultiReader(bytes.NewReader(wire), iotest.ErrReader(errors.New(secret)))
	err = ParseStream(context.Background(), withReadError, ParseOptions{}, func(*Message) error {
		callbacks++
		return nil
	})
	var detail *StreamError
	if callbacks != 1 || !errors.Is(err, ErrStreamRead) || !errors.As(err, &detail) || detail.Message != 2 {
		t.Fatalf("read failure: %d callbacks, %v", callbacks, err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Fatalf("reader content in diagnostic: %v", err)
	}
}

func TestParseStreamParseErrorAtSegmentWithinMessage(t *testing.T) {
	wire := message(`MSH|^~\&|FIRST`, "PID|1", `MSH|^~\&|SECOND`, "malformed-patient-value")
	var callbacks int
	err := ParseStream(context.Background(), iotest.DataErrReader(bytes.NewReader(wire)), ParseOptions{}, func(*Message) error {
		callbacks++
		return nil
	})
	var streamErr *StreamError
	var segmentErr *ParseError
	firstLength := len(message(`MSH|^~\&|FIRST`, "PID|1"))
	if callbacks != 1 || !errors.Is(err, ErrSyntax) ||
		!errors.As(err, &streamErr) || streamErr.Message != 2 || streamErr.Offset != int64(firstLength) ||
		!errors.As(err, &segmentErr) || segmentErr.Segment != 2 {
		t.Fatalf("stream failure: %d callbacks, %v", callbacks, err)
	}
	if strings.Contains(err.Error(), "malformed-patient-value") {
		t.Fatalf("unexpected input value in error: %v", err)
	}
}

func TestParseStreamRejectsMSHLikeSegmentNameBeforePublication(t *testing.T) {
	wire := message(`MSH|^~\&|FIRST`, "MSHSECRET-CLINICAL-TEXT")
	err := ParseStream(context.Background(), iotest.OneByteReader(bytes.NewReader(wire)), ParseOptions{}, func(*Message) error {
		t.Fatal("published a message containing an invalid segment")
		return nil
	})
	var detail *StreamError
	if !errors.Is(err, ErrSyntax) || !errors.As(err, &detail) || detail.Message != 1 || detail.Offset != 0 {
		t.Fatalf("unexpected parse error: %v", err)
	}
}
