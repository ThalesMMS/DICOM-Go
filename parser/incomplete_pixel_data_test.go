package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func truncatedNativeWire(tag core.Tag, vr core.VR, length uint32, payload []byte) []byte {
	b := make([]byte, 12)
	binary.LittleEndian.PutUint16(b, tag.Group)
	binary.LittleEndian.PutUint16(b[2:], tag.Element)
	copy(b[4:], vr)
	binary.LittleEndian.PutUint32(b[8:], length)
	return append(b, payload...)
}
func truncatedItem(length uint32, payload []byte) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, 0xfffe)
	binary.LittleEndian.PutUint16(b[2:], 0xe000)
	binary.LittleEndian.PutUint32(b[4:], length)
	return append(b, payload...)
}
func truncatedEncapsulatedWire(tail []byte) []byte {
	b := truncatedNativeWire(core.TagPixelData, core.VROB, 0xffffffff, nil)
	b = append(b, truncatedItem(0, nil)...)
	b = append(b, truncatedItem(4, []byte{0xff, 0xd8, 0xff, 0xd9})...)
	return append(b, tail...)
}

type truncatedShortReader struct {
	data     []byte
	terminal error
	max      int
}

func (r *truncatedShortReader) Read(p []byte) (int, error) {
	if len(p) > r.max {
		r.max = len(p)
	}
	if len(r.data) == 0 {
		return 0, r.terminal
	}
	n := min(len(p), 3, len(r.data))
	copy(p, r.data[:n])
	r.data = r.data[n:]
	return n, nil
}

func TestIncompletePixelDataNativeCollectorsAndErrors(t *testing.T) {
	wire := truncatedNativeWire(core.TagPixelData, core.VROW, 12, []byte{1, 2, 3, 4, 5, 6, 7, 8, 9})
	for _, collector := range []string{"dataset", "all"} {
		for _, seekable := range []bool{false, true} {
			t.Run(collector+map[bool]string{true: "/seek", false: "/short"}[seekable], func(t *testing.T) {
				var input io.Reader = &truncatedShortReader{data: bytes.Clone(wire), terminal: io.EOF}
				if seekable {
					input = bytes.NewReader(wire)
				}
				r := NewReader(input, transfer.ExplicitVRLittleEndian, ReaderOptions{AllowIncompletePixelData: true, BaseOffset: 100})
				var elements []core.Element
				var err error
				if collector == "dataset" {
					ds, e := r.ReadDataSet()
					elements, err = ds.Elements, e
				} else {
					elements, err = r.ReadAll()
				}
				var diagnostic *IncompletePixelDataError
				if len(elements) != 1 || !errors.Is(err, ErrIncompletePixelData) || !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &diagnostic) {
					t.Fatalf("elements%d err%v", len(elements), err)
				}
				raw, _ := elements[0].RawBytes()
				if !bytes.Equal(raw, wire[12:]) || elements[0].Header.Length != 12 {
					t.Fatal("raw/header changed")
				}
				info, ok := r.IncompletePixelData()
				if !ok || info.ValueOffset != 112 || info.FailureOffset != 121 || info.AvailableBytes != 9 || info.DeclaredBytes != 12 {
					t.Fatalf("report %+v", info)
				}
				info.Kind = "changed"
				again, _ := r.IncompletePixelData()
				if again.Kind != "native-value-eof" {
					t.Fatal("report aliases")
				}
			})
		}
	}
	boom := errors.New("reader refused")
	for _, cause := range []error{boom, context.Canceled, context.DeadlineExceeded, errors.Join(io.EOF, boom)} {
		r := NewReader(&truncatedShortReader{data: bytes.Clone(wire), terminal: cause}, transfer.ExplicitVRLittleEndian, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		if len(ds.Elements) != 0 || !errors.Is(err, cause) || errors.Is(err, ErrIncompletePixelData) {
			t.Fatalf("cause%v err%v", cause, err)
		}
	}
	strict := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{})
	ds, err := strict.ReadDataSet()
	if len(ds.Elements) != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("strict %v", err)
	}
	atomic := NewReader(bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{})
	ds, err = atomic.ReadDataSetPartial()
	if len(ds.Elements) != 0 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("atomic %v", err)
	}
}

func TestIncompletePixelDataEncapsulatedTerminalHeaders(t *testing.T) {
	completeItem := truncatedItem(16, nil)
	for n := 0; n <= 8; n++ {
		for _, seek := range []bool{false, true} {
			t.Run(string(rune('0'+n))+map[bool]string{true: "/seek", false: "/short"}[seek], func(t *testing.T) {
				wire := truncatedEncapsulatedWire(completeItem[:n])
				var input io.Reader = bytes.NewReader(wire)
				if !seek {
					input = &truncatedShortReader{data: wire, terminal: io.EOF}
				}
				r := NewReader(input, transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
				ds, err := r.ReadDataSet()
				if len(ds.Elements) != 1 || !errors.Is(err, ErrIncompletePixelData) {
					t.Fatalf("n%d err%v", n, err)
				}
				seq := ds.Elements[0].Value.(core.FragmentSequence)
				info, _ := r.IncompletePixelData()
				if len(seq.Fragments) != 1 || info.RetainedFragments != 1 || info.AvailableBytes != 4 {
					t.Fatalf("report %+v", info)
				}
				if n == 8 && info.DiscardedFragments != 1 {
					t.Fatal("incomplete item missing")
				}
			})
		}
	}
	for n := 1; n < 16; n++ {
		wire := truncatedEncapsulatedWire(truncatedItem(16, make([]byte, n)))
		r := NewReader(bytes.NewReader(wire), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		info, _ := r.IncompletePixelData()
		if len(ds.Elements) != 1 || !errors.Is(err, ErrIncompletePixelData) || info.DiscardedBytes != int64(n) {
			t.Fatalf("payload%d %+v %v", n, info, err)
		}
	}
	// A full known/private/valid element or corrupt delimiter is not an EOF repair.
	tails := [][]byte{
		{0x10, 0, 0x10, 0, 'P', 'N', 0, 0}, {0x11, 0, 0x10, 0, 0xff, 0xff, 0, 0}, {0x0c, 0xb0, 0xb6, 0x0e, 'L', 'O', 0, 0},
		{0xfe, 0xff, 0xdd, 0xe0, 2, 0, 0, 0}, {0xfe, 0xff, 0, 0xe0, 0xff, 0xff, 0xff, 0xff},
	}
	for i, tail := range tails {
		r := NewReader(bytes.NewReader(truncatedEncapsulatedWire(tail)), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		if err == nil || len(ds.Elements) != 0 || errors.Is(err, ErrIncompletePixelData) {
			t.Fatalf("tail%d %v", i, err)
		}
	}
	garbage := []byte{0x0c, 0xb0, 0xb6, 0x0e, 0x50, 0xd1, 2, 0x0e}
	r := NewReader(bytes.NewReader(truncatedEncapsulatedWire(garbage)), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, Dictionary: std.Dictionary})
	ds, err := r.ReadDataSet()
	info, _ := r.IncompletePixelData()
	if len(ds.Elements) != 1 || !errors.Is(err, ErrIncompletePixelData) || info.Kind != "encapsulated-garbage-terminator" || info.DiscardedBytes != 8 {
		t.Fatalf("garbage %+v %v", info, err)
	}
	// Without a physical size/ReaderAt, unknown garbage cannot authorize recovery.
	r = NewReader(&truncatedShortReader{data: truncatedEncapsulatedWire(garbage), terminal: io.EOF}, transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
	ds, err = r.ReadDataSet()
	if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
		t.Fatalf("unsized garbage %v", err)
	}
}

func TestIncompletePixelDataAdmissionAndUnsupportedValues(t *testing.T) {
	wire := truncatedNativeWire(core.TagPixelData, core.VROW, 12, []byte{1, 2, 3})
	cases := []struct {
		name string
		opts ReaderOptions
		want error
	}{
		{"pixel", ReaderOptions{AllowIncompletePixelData: true, MaxPixelDataBytes: 10}, ErrMaxPixelDataBytesExceeded},
		{"total", ReaderOptions{AllowIncompletePixelData: true, MaxTotalBytes: 14}, ErrMaxTotalBytesExceeded},
		{"skip", ReaderOptions{AllowIncompletePixelData: true, SkipPixelData: true}, ErrIncompletePixelDataOptions},
		{"defer", ReaderOptions{AllowIncompletePixelData: true, DeferPixelData: true}, ErrIncompletePixelDataOptions},
		{"inline", ReaderOptions{AllowIncompletePixelData: true, InlineValueBytesThreshold: 1}, ErrIncompletePixelDataOptions},
		{"grammar", ReaderOptions{AllowIncompletePixelData: true, RecoverDelimiterAnomalies: true}, ErrIncompletePixelDataOptions},
		{"unlimited depth", ReaderOptions{AllowIncompletePixelData: true, MaxSequenceDepth: -1}, ErrIncompletePixelDataOptions},
		{"sink", ReaderOptions{AllowIncompletePixelData: true, FrameSink: FrameSinkFunc(func(Frame) error { t.Fatal("sink delivered payload"); return nil })}, ErrIncompletePixelDataOptions},
		{"negative", ReaderOptions{AllowIncompletePixelData: true, MaxPixelDataBytes: -1}, ErrIncompletePixelDataOptions},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := &truncatedShortReader{data: wire, terminal: io.EOF}
			r := NewReader(input, transfer.ExplicitVRLittleEndian, tc.opts)
			ds, err := r.ReadDataSet()
			if len(ds.Elements) != 0 || !errors.Is(err, tc.want) || errors.Is(err, ErrIncompletePixelData) {
				t.Fatalf("%v", err)
			}
		})
	}
	for _, tag := range []core.Tag{core.NewTag(0x7fe0, 0x0008), core.NewTag(0x7fe0, 0x0009)} {
		r := NewReader(bytes.NewReader(truncatedNativeWire(tag, core.VROF, 12, []byte{1, 2, 3})), transfer.ExplicitVRLittleEndian, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
			t.Fatalf("float %v", err)
		}
	}
	// A forged capped Item VL is read in bounded chunks, never one full request.
	input := &truncatedShortReader{data: truncatedEncapsulatedWire(truncatedItem(100<<20, nil)), terminal: io.EOF}
	r := NewReader(input, transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
	ds, err := r.ReadDataSet()
	if len(ds.Elements) != 1 || !errors.Is(err, ErrIncompletePixelData) || input.max > 32<<10 {
		t.Fatalf("chunks%d %v", input.max, err)
	}
	input = &truncatedShortReader{data: truncatedEncapsulatedWire(truncatedItem(129<<20, nil)), terminal: io.EOF}
	r = NewReader(input, transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, MaxPixelDataBytes: 1 << 30})
	ds, err = r.ReadDataSet()
	if len(ds.Elements) != 0 || !errors.Is(err, ErrMaxPixelDataBytesExceeded) {
		t.Fatalf("hard cap %v", err)
	}
	// Pixel Data nested in a defined container cannot cross its fence.
	nested := []byte{0x08, 0, 0x40, 0x11, 'S', 'Q', 0, 0, 28, 0, 0, 0}
	nested = append(nested, truncatedItem(20, wire)...)
	r = NewReader(bytes.NewReader(nested), transfer.ExplicitVRLittleEndian, ReaderOptions{AllowIncompletePixelData: true})
	ds, err = r.ReadDataSet()
	if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
		t.Fatalf("nested %v", err)
	}
}

func TestIncompletePixelDataBOTAndDelimiterBoundaries(t *testing.T) {
	delim := []byte{0xfe, 0xff, 0xdd, 0xe0, 0, 0, 0, 0}
	for n := 1; n <= 7; n++ {
		r := NewReader(bytes.NewReader(truncatedEncapsulatedWire(delim[:n])), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		if len(ds.Elements) != 1 || !errors.Is(err, ErrIncompletePixelData) {
			t.Fatalf("delimiter%d %v", n, err)
		}
	}
	for _, tail := range [][]byte{nil, truncatedItem(4, []byte{0, 0}), truncatedItem(0, nil)} {
		wire := truncatedNativeWire(core.TagPixelData, core.VROB, 0xffffffff, tail)
		r := NewReader(bytes.NewReader(wire), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true})
		ds, err := r.ReadDataSet()
		if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
			t.Fatalf("BOT %v", err)
		}
	}
	knownGarbage := []byte{0x10, 0, 0x10, 0, 0xff, 0xff, 0, 0}
	r := NewReader(bytes.NewReader(truncatedEncapsulatedWire(knownGarbage)), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, Dictionary: std.Dictionary})
	ds, err := r.ReadDataSet()
	if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
		t.Fatalf("known garbage %v", err)
	}
	r = NewReader(bytes.NewReader(truncatedEncapsulatedWire(nil)), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, MaxFragments: 1})
	wire := truncatedEncapsulatedWire(truncatedItem(4, []byte{0xff, 0xd8, 0xff, 0xd9}))
	r = NewReader(bytes.NewReader(wire), transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, MaxFragments: 1})
	ds, err = r.ReadDataSet()
	if len(ds.Elements) != 0 || !errors.Is(err, ErrMaxFragmentsExceeded) {
		t.Fatalf("fragments %v", err)
	}
}

type incompleteFullErrorReader struct {
	data         []byte
	position, at int
	failure      error
	fired        bool
}

func (r *incompleteFullErrorReader) Read(p []byte) (int, error) {
	if r.position >= len(r.data) {
		return 0, io.EOF
	}
	n := copy(p, r.data[r.position:])
	at := r.position
	r.position += n
	if !r.fired && at == r.at {
		r.fired = true
		return n, r.failure
	}
	return n, nil
}
func TestIncompletePixelDataFullReadFailurePersists(t *testing.T) {
	boom := errors.New("payload read failed after supplying a complete chunk")
	payload := make([]byte, (32<<10)+9)
	native := truncatedNativeWire(core.TagPixelData, core.VROW, 64<<10, payload)
	encap := truncatedEncapsulatedWire(truncatedItem(64<<10, payload))
	for _, tc := range []struct {
		name   string
		wire   []byte
		syntax transfer.Syntax
		at     int
	}{{"native payload", native, transfer.ExplicitVRLittleEndian, 12}, {"Item payload", encap, transfer.JPEGBaseline, 40}, {"header", native, transfer.ExplicitVRLittleEndian, 0}} {
		t.Run(tc.name, func(t *testing.T) {
			r := NewReader(&incompleteFullErrorReader{data: tc.wire, at: tc.at, failure: boom}, tc.syntax, ReaderOptions{AllowIncompletePixelData: true})
			ds, err := r.ReadDataSet()
			if len(ds.Elements) != 0 || !errors.Is(err, boom) || errors.Is(err, ErrIncompletePixelData) {
				t.Fatalf("source failure hidden: %v", err)
			}
			if _, present := r.IncompletePixelData(); present {
				t.Fatal("I/O failure obtained certificate")
			}
		})
	}
}

type incompleteEarlyEOFReader struct {
	*bytes.Reader
	end int64
}

func (r incompleteEarlyEOFReader) Read(p []byte) (int, error) {
	position, _ := r.Seek(0, io.SeekCurrent)
	if position >= r.end {
		return 0, io.EOF
	}
	return r.Reader.Read(p[:int(min(int64(len(p)), r.end-position))])
}
func TestIncompletePixelDataNativeEOFMustMatchKnownExtent(t *testing.T) {
	wire := truncatedNativeWire(core.TagPixelData, core.VROW, 32, make([]byte, 16))
	r := NewReader(incompleteEarlyEOFReader{Reader: bytes.NewReader(wire), end: 21}, transfer.ExplicitVRLittleEndian, ReaderOptions{AllowIncompletePixelData: true})
	ds, err := r.ReadDataSet()
	if len(ds.Elements) != 0 || err == nil || errors.Is(err, ErrIncompletePixelData) {
		t.Fatalf("premature artificial EOF: %v", err)
	}
}

type incompleteGarbageFailureReader struct {
	*bytes.Reader
	at          int64
	failure     error
	readAtCount int
	fired       bool
}

func (r *incompleteGarbageFailureReader) Read(p []byte) (int, error) {
	at, _ := r.Seek(0, io.SeekCurrent)
	n, err := r.Reader.Read(p)
	if r.readAtCount == 0 && !r.fired && at == r.at {
		r.fired = true
		return n, r.failure
	}
	return n, err
}
func (r *incompleteGarbageFailureReader) ReadAt(p []byte, off int64) (int, error) {
	n, err := r.Reader.ReadAt(p, off)
	if r.readAtCount > 0 && len(p) == 8 && off == r.Size()-8 {
		return min(n, r.readAtCount), r.failure
	}
	return n, err
}
func TestIncompletePixelDataGarbageTailIOCannotCertify(t *testing.T) {
	boom := errors.New("garbage tail source failure")
	tail := []byte{0x0c, 0xb0, 0xb6, 0x0e, 0x50, 0xd1, 0x02, 0x0e}
	wire := truncatedEncapsulatedWire(tail)
	for _, tc := range []struct {
		name        string
		failure     error
		readAtCount int
	}{{"plain", boom, 0}, {"forged", errors.Join(ErrIncompletePixelData, boom), 0}, {"readat-full", boom, 8}, {"readat-partial", boom, 2}} {
		for _, all := range []bool{false, true} {
			mode := "dataset"
			if all {
				mode = "all"
			}
			t.Run(tc.name+"_"+mode, func(t *testing.T) {
				source := &incompleteGarbageFailureReader{Reader: bytes.NewReader(wire), at: int64(len(wire) - 2), failure: tc.failure, readAtCount: tc.readAtCount}
				reader := NewReader(source, transfer.JPEGBaseline, ReaderOptions{AllowIncompletePixelData: true, Dictionary: std.Dictionary})
				var count int
				var err error
				if all {
					elements, e := reader.ReadAll()
					count = len(elements)
					err = e
				} else {
					dataset, e := reader.ReadDataSet()
					count = len(dataset.Elements)
					err = e
				}
				if _, certified := reader.IncompletePixelData(); certified || count != 0 || !errors.Is(err, boom) {
					t.Fatalf("garbage I/O certificate leaked or cause lost count%d err%v certified%v", count, err, certified)
				}
			})
		}
	}
}
