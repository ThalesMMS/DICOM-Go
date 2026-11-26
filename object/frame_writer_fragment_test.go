package object

import (
	"bytes"
	"encoding/binary"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"testing"
)

type chunkedFragmentFrames struct {
	frames [][]byte
	calls  int
}

func (f *chunkedFragmentFrames) Frames() int { return len(f.frames) }
func (f *chunkedFragmentFrames) FrameLength(i int) (int64, error) {
	return int64(len(f.frames[i])), nil
}
func (f *chunkedFragmentFrames) WriteFrameTo(i int, w io.Writer) (int64, error) {
	f.calls++
	var total int64
	for _, b := range f.frames[i] {
		n, err := w.Write([]byte{b})
		total += int64(n)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

func TestWriteFileFramesSplitsChunksWithoutIntermediatePadding(t *testing.T) {
	frames := &chunkedFragmentFrames{frames: [][]byte{{1, 2, 3, 4, 5, 6, 7}, {8, 9, 10, 11, 12}}}
	file := &File{Dataset: FromElements(frameTestDataSet(2, 8), nil), TransferSyntax: transfer.JPEGBaseline}
	for _, policy := range []OffsetTablePolicy{OffsetTableAuto, OffsetTableBasic, OffsetTableEmpty} {
		var out bytes.Buffer
		if err := WriteFileFrames(&out, file, frames, FrameWriteOptions{MaxFragmentBytes: 4, OffsetTable: policy, MaxFragments: 4}); err != nil {
			t.Fatal(err)
		}
		read, err := ReadFile(bytes.NewReader(out.Bytes()))
		if err != nil {
			t.Fatal(err)
		}
		pixel, _ := read.Dataset.Get(core.TagPixelData)
		seq, ok := pixel.Value.(core.FragmentSequence)
		if !ok {
			t.Fatalf("pixel=%T", pixel.Value)
		}
		want := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 0}, {8, 9}, {10, 11, 12, 0}}
		if len(seq.Fragments) != len(want) {
			t.Fatalf("fragments=%v", seq.Fragments)
		}
		for i := range want {
			if !bytes.Equal(want[i], seq.Fragments[i]) {
				t.Fatalf("fragment%d=%v", i, seq.Fragments[i])
			}
		}
		if policy == OffsetTableEmpty {
			if len(seq.OffsetTable) != 0 {
				t.Fatal("nonempty BOT")
			}
		} else if len(seq.OffsetTable) != 8 || binary.LittleEndian.Uint32(seq.OffsetTable[4:]) != 24 {
			t.Fatalf("BOT=%v", seq.OffsetTable)
		}
	}
}

func TestWriteFileFramesFragmentLimitsBeforePayload(t *testing.T) {
	for _, test := range []struct {
		syntax transfer.Syntax
		opts   FrameWriteOptions
	}{
		{transfer.JPEGBaseline, FrameWriteOptions{MaxFragmentBytes: 3}}, {transfer.JPEGBaseline, FrameWriteOptions{MaxFragmentBytes: 4, MaxFragments: 1}}, {transfer.JPEGBaseline, FrameWriteOptions{MaxFragmentBytes: 4, OffsetTable: OffsetTableExtended}}, {transfer.RLELossless, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.DeflatedImageFrameCompression, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.EncapsulatedUncompressedExplicitVRLittleEndian, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.ExplicitVRLittleEndian, FrameWriteOptions{MaxFragmentBytes: 4}},
		{transfer.HTJ2KLossless, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.HTJ2KLosslessRPCL, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.HTJ2K, FrameWriteOptions{MaxFragmentBytes: 4}},
		{transfer.JPEGXLLossless, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.JPEGXLJPEGRecompression, FrameWriteOptions{MaxFragmentBytes: 4}}, {transfer.JPEGXL, FrameWriteOptions{MaxFragmentBytes: 4}},
	} {
		frames := &chunkedFragmentFrames{frames: [][]byte{{1, 2, 3, 4, 5}}}
		file := &File{Dataset: FromElements(frameTestDataSet(1, 8), nil), TransferSyntax: test.syntax}
		var out bytes.Buffer
		err := WriteFileFrames(&out, file, frames, test.opts)
		if !errors.Is(err, ErrFrameSource) || frames.calls != 0 || out.Len() != 0 {
			t.Fatalf("%s err=%v calls=%d bytes=%d", test.syntax.Name, err, frames.calls, out.Len())
		}
	}
}

type fragmentShortWriter struct{}

func (fragmentShortWriter) Write(p []byte) (int, error) { return max(0, len(p)-1), nil }
func TestFragmentFrameWriterRejectsOverrunAndShortWrites(t *testing.T) {
	f := fragmentFrameWriter{w: io.Discard, remaining: 2, cap: 2}
	if _, err := f.Write([]byte{1, 2, 3}); !errors.Is(err, ErrFrameSource) {
		t.Fatalf("overrun=%v", err)
	}
	f = fragmentFrameWriter{w: fragmentShortWriter{}, remaining: 2, cap: 2}
	if _, err := f.Write([]byte{1, 2}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short=%v", err)
	}
}
