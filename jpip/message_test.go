package jpip

import (
	"bytes"
	"errors"
	"testing"
)

func TestMessageRoundTrip(t *testing.T) {
	messages := []Message{
		{Class: ClassMainHeader, Stream: 1, ID: 0, Offset: 0, Data: []byte{0xff, 0x4f}, Complete: true},
		{Class: ClassMetadata, Stream: 1, ID: 0, Complete: true, Data: []byte{}},
		{Class: ClassPrecinct, Stream: 3, ID: 15, Offset: 0, Data: []byte{1, 2, 3}},
		{Class: ClassPrecinct, Stream: 3, ID: 16, Offset: 127, Data: []byte{4}},
		{Class: ClassPrecinct, Stream: 300, ID: 1<<40 + 5, Offset: 1 << 33, Data: []byte{5, 6}, Complete: true},
		{Class: ClassTileHeader, Stream: 0, ID: 2047, Data: []byte{7}},
		{Class: ClassTile, Stream: 2, ID: 7, Offset: 9, Data: bytes.Repeat([]byte{8}, 200)},
		{Class: ClassExtendedPrecinct, Stream: 1, ID: 3, Offset: 1, Data: []byte{9}, Aux: 77},
	}
	var body []byte
	for _, m := range messages {
		body = AppendMessage(body, m)
	}
	body = AppendEOR(body, EORWindowDone)
	stream, err := ParseStream(body)
	if err != nil {
		t.Fatal(err)
	}
	if !stream.HasEOR || stream.EOR != EORWindowDone {
		t.Fatalf("EOR = %v %v", stream.HasEOR, stream.EOR)
	}
	if len(stream.Messages) != len(messages) {
		t.Fatalf("parsed %d messages, want %d", len(stream.Messages), len(messages))
	}
	for i, got := range stream.Messages {
		want := messages[i]
		if got.Class != want.Class || got.Stream != want.Stream || got.ID != want.ID || got.Offset != want.Offset ||
			got.Complete != want.Complete || got.Aux != want.Aux || !bytes.Equal(got.Data, want.Data) {
			t.Errorf("message %d = %+v, want %+v", i, got, want)
		}
	}
}

func TestVBASEncoding(t *testing.T) {
	tests := []struct {
		value uint64
		want  []byte
	}{
		{0, []byte{0}},
		{127, []byte{0x7f}},
		{128, []byte{0x81, 0x00}},
		{300, []byte{0x82, 0x2c}},
	}
	for _, tt := range tests {
		if got := appendVBAS(nil, tt.value); !bytes.Equal(got, tt.want) {
			t.Errorf("appendVBAS(%d) = % x, want % x", tt.value, got, tt.want)
		}
	}
	// Bin-ID: indicator 3, completeness, ID 0x123 split as 4+7 bits.
	header := appendMessageHeader(nil, Message{Class: ClassPrecinct, Stream: 0, ID: 0x123, Complete: true}, 0)
	if want := []byte{0x80 | 0x60 | 0x10 | 0x02, 0x23, 0, 0, 0, 0}; !bytes.Equal(header, want) {
		t.Fatalf("header = % x, want % x", header, want)
	}
}

func TestParseStreamCompressedHeaders(t *testing.T) {
	// ISO/IEC 15444-9 A.2.1 lets a message reuse the previous class (and
	// CSn): indicator 1 omits both, indicator 2 omits CSn.
	body := []byte{
		0x60 | 0x02, 0x04, 0x05, 0x00, 0x01, 0xaa, // class 4 (tile), CSn 5, ID 2
		0x20 | 0x03, 0x01, 0x01, 0xbb, // indicator 1: tile 3 of CSn 5, offset 1
		0x40 | 0x10 | 0x00, 0x06, 0x00, 0x01, 0xcc, // indicator 2: main header of CSn 5
		0x00, byte(EORImageDone), 0x00,
	}
	stream, err := ParseStream(body)
	if err != nil {
		t.Fatal(err)
	}
	want := []Message{
		{Class: ClassTile, Stream: 5, ID: 2, Data: []byte{0xaa}},
		{Class: ClassTile, Stream: 5, ID: 3, Offset: 1, Data: []byte{0xbb}},
		{Class: ClassMainHeader, Stream: 5, ID: 0, Data: []byte{0xcc}, Complete: true},
	}
	for i, got := range stream.Messages {
		if got.Class != want[i].Class || got.Stream != want[i].Stream || got.ID != want[i].ID ||
			got.Offset != want[i].Offset || got.Complete != want[i].Complete || !bytes.Equal(got.Data, want[i].Data) {
			t.Errorf("message %d = %+v, want %+v", i, got, want[i])
		}
	}
	if stream.EOR != EORImageDone {
		t.Fatalf("EOR = %d", stream.EOR)
	}
}

func TestParseStreamRejectsMalformedMessages(t *testing.T) {
	tests := map[string][]byte{
		"indicator 1 first":    {0x20, 0x00, 0x00},
		"reserved class":       {0x60, 0x03, 0x00, 0x00, 0x00},
		"unknown class":        {0x60, 0x09, 0x00, 0x00, 0x00},
		"truncated body":       {0x60, 0x00, 0x00, 0x00, 0x05, 0x01},
		"truncated VBAS":       {0x60, 0x80},
		"truncated EOR":        {0x00},
		"data after EOR":       {0x00, 0x01, 0x00, 0x60, 0x06, 0x00, 0x00, 0x00},
		"oversized VBAS":       append([]byte{0x60}, bytes.Repeat([]byte{0xff}, 12)...),
		"truncated bin-id":     {0xe0},
		"EOR body past end":    {0x00, 0x02, 0x05, 0x00},
		"prohibited indicator": {0x10, 0x00},
	}
	for name, body := range tests {
		if _, err := ParseStream(body); !errors.Is(err, ErrMalformedStream) {
			t.Errorf("%s: error = %v, want ErrMalformedStream", name, err)
		}
	}
}

func TestDataBinSetMergesOutOfOrderAndRepeatedRanges(t *testing.T) {
	set := NewDataBinSet(1 << 10)
	add := func(offset uint64, data string, complete bool) {
		t.Helper()
		if err := set.Add(Message{Class: ClassPrecinct, Stream: 1, ID: 9, Offset: offset, Data: []byte(data), Complete: complete}); err != nil {
			t.Fatal(err)
		}
	}
	add(6, "ghi", true)
	add(0, "abc", false)
	add(1, "bcdef", false)
	add(0, "ab", false)
	bin := set.bin(1, ClassPrecinct, 9)
	if string(bin.data) != "abcdefghi" || !bin.complete() || len(bin.segments) != 0 {
		t.Fatalf("bin = %q complete=%v pending=%d", bin.data, bin.complete(), len(bin.segments))
	}
	if set.Bytes() != 9 {
		t.Fatalf("Bytes = %d", set.Bytes())
	}
	if err := set.Add(Message{Class: ClassPrecinct, Stream: 1, ID: 9, Offset: 9, Data: []byte("x")}); !errors.Is(err, ErrMalformedStream) {
		t.Fatalf("data past a complete bin: %v", err)
	}
	small := NewDataBinSet(4)
	if err := small.Add(Message{Class: ClassTile, Stream: 1, Data: []byte("12345")}); !errors.Is(err, ErrDataBinLimit) {
		t.Fatalf("limit error = %v", err)
	}
}
