package parser

import (
	"bytes"
	"errors"
	"io"
	"strconv"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// oneBitPixel is a deterministic, frame-dependent bit pattern.
func oneBitPixel(frame, pixel int) byte {
	return byte((pixel*7+frame*3+pixel/3)%5) & 1
}

// oneBitFrames returns the stored value of frames concatenated LSB-first in
// one bit stream, and each frame packed on its own from bit zero.
func oneBitFrames(pixelsPerFrame, frames int) (stored []byte, split [][]byte) {
	stored = make([]byte, (pixelsPerFrame*frames+7)/8)
	for f := 0; f < frames; f++ {
		frame := make([]byte, (pixelsPerFrame+7)/8)
		for p := 0; p < pixelsPerFrame; p++ {
			bit := oneBitPixel(f, p)
			at := f*pixelsPerFrame + p
			stored[at/8] |= bit << (at % 8)
			frame[p/8] |= bit << (p % 8)
		}
		split = append(split, frame)
	}
	return stored, split
}

func oneBitStream(rows, columns uint16, frames int, value []byte) []byte {
	return dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0008), core.VRIS, strconv.Itoa(frames)),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), rows),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), columns),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 1),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 1),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 0),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
		core.NewRawElement(core.TagPixelData, core.VROB, value),
	)
}

func TestFrameSinkSplitsContiguousOneBitFrames(t *testing.T) {
	for _, tc := range []struct {
		name          string
		rows, columns uint16
		frames        int
		valueLength   int
	}{
		// 75 bits fill 10 bytes, already even.
		{name: "5x5x3", rows: 5, columns: 5, frames: 3, valueLength: 10},
		// 18 bits fill 3 bytes, padded to an even length of 4.
		{name: "3x3x2", rows: 3, columns: 3, frames: 2, valueLength: 4},
	} {
		for _, source := range []struct {
			name string
			wrap func([]byte) io.Reader
		}{
			{name: "sized", wrap: func(b []byte) io.Reader { return bytes.NewReader(b) }},
			{name: "stream", wrap: func(b []byte) io.Reader { return unseekableReader{bytes.NewReader(b)} }},
		} {
			t.Run(tc.name+"/"+source.name, func(t *testing.T) {
				stored, want := oneBitFrames(int(tc.rows)*int(tc.columns), tc.frames)
				value := append(stored, make([]byte, tc.valueLength-len(stored))...)
				sink := &testFrameSink{}
				_, err := NewReader(source.wrap(oneBitStream(tc.rows, tc.columns, tc.frames, value)), transfer.ExplicitVRLittleEndian, ReaderOptions{FrameSink: sink}).ReadDataSet()
				if err != nil {
					t.Fatal(err)
				}
				if len(sink.frames) != tc.frames {
					t.Fatalf("frame count = %d, want %d", len(sink.frames), tc.frames)
				}
				for i, frame := range sink.frames {
					if frame.Index != i || int64(len(frame.Data)) != frame.Metadata.FrameSize() {
						t.Fatalf("frame %d: index=%d len=%d FrameSize=%d", i, frame.Index, len(frame.Data), frame.Metadata.FrameSize())
					}
					if !bytes.Equal(frame.Data, want[i]) {
						t.Fatalf("frame %d = %08b, want %08b", i, frame.Data, want[i])
					}
				}
			})
		}
	}
}

func TestFrameSinkRejectsOneBitPixelDataShorterThanFrames(t *testing.T) {
	stored, _ := oneBitFrames(25, 3)
	sink := &testFrameSink{}
	_, err := NewReader(bytes.NewReader(oneBitStream(5, 5, 3, stored[:8])), transfer.ExplicitVRLittleEndian, ReaderOptions{FrameSink: sink}).ReadDataSet()
	var parseErr *ParseError
	if !errors.Is(err, ErrFrameDataSizeMismatch) || !errors.As(err, &parseErr) || parseErr.Tag != core.TagPixelData {
		t.Fatalf("short Pixel Data error = %v, want ErrFrameDataSizeMismatch ParseError", err)
	}
	if !strings.Contains(err.Error(), "expected 10 bytes for 3 frame(s), got 8") {
		t.Fatalf("short Pixel Data error = %q, want expected and declared sizes", err)
	}
	if len(sink.frames) != 0 || sink.closeCount != 1 {
		t.Fatalf("frames=%d close=%d, want 0 frames and one close", len(sink.frames), sink.closeCount)
	}
}

func TestFrameSinkRejectsTruncatedOneBitFrames(t *testing.T) {
	stored, want := oneBitFrames(25, 3)
	stream := oneBitStream(5, 5, 3, stored)
	// Drop the bytes holding the end of frame 1 and all of frame 2.
	sink := &testFrameSink{}
	_, err := NewReader(unseekableReader{bytes.NewReader(stream[:len(stream)-4])}, transfer.ExplicitVRLittleEndian, ReaderOptions{FrameSink: sink}).ReadDataSet()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated Pixel Data error = %v, want io.ErrUnexpectedEOF", err)
	}
	if len(sink.frames) != 1 || !bytes.Equal(sink.frames[0].Data, want[0]) || sink.closeCount != 1 {
		t.Fatalf("frames=%d close=%d, want the complete first frame and one close", len(sink.frames), sink.closeCount)
	}
}

func TestFrameMetadataNativeValueSizeRoundsOnlyTheOneBitTotal(t *testing.T) {
	for _, tc := range []struct {
		metadata              FrameMetadata
		frameSize, valueBytes int64
	}{
		{FrameMetadata{Rows: 5, Columns: 5, SamplesPerPixel: 1, BitsAllocated: 1, NumberOfFrames: 3}, 4, 10},
		{FrameMetadata{Rows: 3, Columns: 3, SamplesPerPixel: 1, BitsAllocated: 1, NumberOfFrames: 2}, 2, 3},
		{FrameMetadata{Rows: 4, Columns: 4, SamplesPerPixel: 1, BitsAllocated: 1, NumberOfFrames: 3}, 2, 6},
		{FrameMetadata{Rows: 3, Columns: 3, SamplesPerPixel: 1, BitsAllocated: 8, NumberOfFrames: 2}, 9, 18},
	} {
		if got := tc.metadata.FrameSize(); got != tc.frameSize {
			t.Fatalf("%+v FrameSize = %d, want %d", tc.metadata, got, tc.frameSize)
		}
		if got := tc.metadata.nativeValueSize(); got != tc.valueBytes {
			t.Fatalf("%+v nativeValueSize = %d, want %d", tc.metadata, got, tc.valueBytes)
		}
	}
}
