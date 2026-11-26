package parser

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// readCountingSeeker is a sized, seekable source that counts the bytes its
// Read calls return, so a test can tell a skipped value that was read and
// discarded from one that was seeked past.
type readCountingSeeker struct {
	*bytes.Reader
	read int64
}

func (s *readCountingSeeker) Read(p []byte) (int, error) {
	n, err := s.Reader.Read(p)
	s.read += int64(n)
	return n, err
}

func trailingElementAfterPixelData(t testing.TB, syntax transfer.Syntax, pixelData core.Element) []byte {
	t.Helper()
	trailing := dicomtest.NewStringElement(core.NewTag(0xFFFA, 0x0010), core.VRLO, "TRAILER ")
	return append(dicomtest.EncodeElements(syntax,
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 512),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 512),
		pixelData,
	), dicomtest.EncodeElement(trailing, syntax)...)
}

// Skipping Pixel Data from a sized seekable source seeks past the payload:
// only the element headers and the elements after it are read, and the data
// set is the same as when the payload is read and discarded.
func TestSkipPixelDataSeeksPastPayloadOfSizedSource(t *testing.T) {
	native := core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 4<<20))
	encapsulated := dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil,
		make([]byte, 1<<20), make([]byte, 1<<20), make([]byte, 1<<20))
	for _, tc := range []struct {
		name   string
		syntax transfer.Syntax
		pixel  core.Element
	}{
		{"native", transfer.ExplicitVRLittleEndian, native},
		{"encapsulated", transfer.JPEGBaseline, encapsulated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			data := trailingElementAfterPixelData(t, tc.syntax, tc.pixel)
			for _, opts := range []ReaderOptions{
				{Dictionary: std.Dictionary, SkipPixelData: true},
				{Dictionary: std.Dictionary, DeferPixelData: true},
			} {
				source := &readCountingSeeker{Reader: bytes.NewReader(data)}
				seeked, err := NewReader(source, tc.syntax, opts).ReadDataSet()
				if err != nil {
					t.Fatalf("sized source: %v", err)
				}
				if source.read > 4<<10 {
					t.Fatalf("read %d of %d bytes, want only the headers", source.read, len(data))
				}
				streamed, err := NewReader(unseekableReader{r: bytes.NewReader(data)}, tc.syntax, opts).ReadDataSet()
				if err != nil {
					t.Fatalf("unsized source: %v", err)
				}
				if len(seeked.Elements) != len(streamed.Elements) {
					t.Fatalf("elements = %d, want %d", len(seeked.Elements), len(streamed.Elements))
				}
				for i := range seeked.Elements {
					got, want := seeked.Elements[i], streamed.Elements[i]
					if got.Header != want.Header || (got.Value == nil) != (want.Value == nil) {
						t.Fatalf("element %d = %+v, want %+v", i, got.Header, want.Header)
					}
				}
				if last := seeked.Elements[len(seeked.Elements)-1]; last.Header.Tag != core.NewTag(0xFFFA, 0x0010) {
					t.Fatalf("last element = %s, want the trailing element", last.Header.Tag)
				}
			}
		})
	}
}

// A skipped value that runs past the end of the source, the MaxTotalBytes
// ceiling or its container keeps failing exactly as when it is read.
func TestSkipPixelDataSeekKeepsTruncationAndLimitErrors(t *testing.T) {
	data := trailingElementAfterPixelData(t, transfer.ExplicitVRLittleEndian,
		core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 1<<20)))
	truncated := data[:len(data)-(512<<10)]
	for _, tc := range []struct {
		name string
		data []byte
		opts ReaderOptions
	}{
		{"truncated", truncated, ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true}},
		{"max total bytes", data, ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true, MaxTotalBytes: 512 << 10}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, seekErr := NewReader(bytes.NewReader(tc.data), transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSet()
			_, readErr := NewReader(unseekableReader{r: bytes.NewReader(tc.data)}, transfer.ExplicitVRLittleEndian, tc.opts).ReadDataSet()
			if seekErr == nil || readErr == nil {
				t.Fatalf("errors = %v / %v, want both to fail", seekErr, readErr)
			}
			if seekErr.Error() != readErr.Error() {
				t.Fatalf("sized and unsized errors differ:\n  sized:   %v\n  unsized: %v", seekErr, readErr)
			}
			if tc.name == "truncated" && !errors.Is(seekErr, io.ErrUnexpectedEOF) {
				t.Fatalf("error = %v, want unexpected EOF", seekErr)
			}
		})
	}
}

// BenchmarkSkipPixelDataSource reads a data set with 64 MiB of native Pixel
// Data under SkipPixelData from a source that hides Seek (the payload is read
// and discarded, as every source was before seekPast) and from a sized
// seekable source (the payload is seeked past). bytes_read/op counts the
// bytes the source returned.
func BenchmarkSkipPixelDataSource(b *testing.B) {
	data := trailingElementAfterPixelData(b, transfer.ExplicitVRLittleEndian,
		core.NewRawElement(core.TagPixelData, core.VROW, make([]byte, 64<<20)))
	for _, mode := range []struct {
		name     string
		seekable bool
	}{
		{"read_and_discard", false},
		{"seek", true},
	} {
		b.Run(mode.name, func(b *testing.B) {
			b.ReportAllocs()
			var read int64
			for range b.N {
				source := &readCountingSeeker{Reader: bytes.NewReader(data)}
				var reader io.Reader = source
				if !mode.seekable {
					reader = unseekableReader{r: source}
				}
				if _, err := NewReader(reader, transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: true}).ReadDataSet(); err != nil {
					b.Fatal(err)
				}
				read += source.read
			}
			b.ReportMetric(float64(read)/float64(b.N), "bytes_read/op")
		})
	}
}
