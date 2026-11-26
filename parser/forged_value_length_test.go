package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"runtime"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// forgedValueLength is a 1 GiB Value Length declared over a 10-byte payload,
// the shape of the CVE-2026-3650 inputs (explicit VR OB, implicit VR and an
// encapsulated fragment).
const (
	forgedValueLength  = 0x40000000
	forgedPayloadBytes = 10
	// forgedAllocationBudget is far above what reading 10 bytes needs and far
	// below the declared value, so it separates growth behind the received
	// bytes from a reservation of the Value Length.
	forgedAllocationBudget = 4 << 20
)

func forgedPayload() []byte { return make([]byte, forgedPayloadBytes) }

func forgedExplicitOB() []byte {
	var out []byte
	out = binary.LittleEndian.AppendUint16(out, 0x0008)
	out = binary.LittleEndian.AppendUint16(out, 0x0060)
	out = append(out, 'O', 'B', 0, 0)
	out = binary.LittleEndian.AppendUint32(out, forgedValueLength)
	return append(out, forgedPayload()...)
}

func forgedImplicit() []byte {
	var out []byte
	out = binary.LittleEndian.AppendUint16(out, 0x0008)
	out = binary.LittleEndian.AppendUint16(out, 0x0060)
	out = binary.LittleEndian.AppendUint32(out, forgedValueLength)
	return append(out, forgedPayload()...)
}

func forgedFragment() []byte {
	var out []byte
	out = binary.LittleEndian.AppendUint16(out, 0x7FE0)
	out = binary.LittleEndian.AppendUint16(out, 0x0010)
	out = append(out, 'O', 'B', 0, 0)
	out = binary.LittleEndian.AppendUint32(out, uint32(core.UndefinedLength))
	// Empty Basic Offset Table, then the forged fragment.
	out = binary.LittleEndian.AppendUint16(out, 0xFFFE)
	out = binary.LittleEndian.AppendUint16(out, 0xE000)
	out = binary.LittleEndian.AppendUint32(out, 0)
	out = binary.LittleEndian.AppendUint16(out, 0xFFFE)
	out = binary.LittleEndian.AppendUint16(out, 0xE000)
	out = binary.LittleEndian.AppendUint32(out, forgedValueLength)
	return append(out, forgedPayload()...)
}

// readAllocatedBytes runs read once and returns the bytes it allocated.
func readAllocatedBytes(read func()) uint64 {
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	read()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func forgedSources(data []byte) map[string]func() io.Reader {
	return map[string]func() io.Reader{
		"sized":  func() io.Reader { return bytes.NewReader(data) },
		"stream": func() io.Reader { return unseekableReader{bytes.NewReader(data)} },
	}
}

func assertForgedValueLengthRejected(t *testing.T, data []byte, syntax transfer.Syntax, opts ReaderOptions) {
	t.Helper()
	for name, source := range forgedSources(data) {
		t.Run(name, func(t *testing.T) {
			var err error
			allocated := readAllocatedBytes(func() {
				_, err = NewReader(source(), syntax, opts).ReadDataSet()
			})
			var parseErr *ParseError
			if !errors.Is(err, io.ErrUnexpectedEOF) || !errors.As(err, &parseErr) {
				t.Fatalf("forged Value Length error = %v, want ParseError with io.ErrUnexpectedEOF", err)
			}
			if allocated > forgedAllocationBudget {
				t.Fatalf("allocated %d bytes for a %d-byte payload declaring %d bytes", allocated, forgedPayloadBytes, forgedValueLength)
			}
		})
	}
}

// The configured limits admit the forged length, so only the read itself
// stands between the declared Value Length and an allocation.
var forgedLimits = ReaderOptions{MaxElementBytes: 1 << 31, MaxPixelDataBytes: 1 << 31, MaxTotalBytes: 1 << 32}

func TestForgedValueLengthExplicitOBGrowsBehindReceivedBytes(t *testing.T) {
	assertForgedValueLengthRejected(t, forgedExplicitOB(), transfer.ExplicitVRLittleEndian, forgedLimits)
}

func TestForgedValueLengthImplicitVRGrowsBehindReceivedBytes(t *testing.T) {
	assertForgedValueLengthRejected(t, forgedImplicit(), transfer.ImplicitVRLittleEndian, forgedLimits)
}

func TestForgedValueLengthFragmentGrowsBehindReceivedBytes(t *testing.T) {
	assertForgedValueLengthRejected(t, forgedFragment(), transfer.RLELossless, forgedLimits)
}

func TestForgedValueLengthUnlimitedReaderGrowsBehindReceivedBytes(t *testing.T) {
	assertForgedValueLengthRejected(t, forgedExplicitOB(), transfer.ExplicitVRLittleEndian, ReaderOptions{})
}

// A frame sink reads one declared frame at a time; a forged 1 GiB frame must
// not be reserved before its bytes arrive.
func TestForgedFrameSizeGrowsBehindReceivedBytes(t *testing.T) {
	header := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian,
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0002), 1),
		dicomtest.NewStringElement(core.NewTag(0x0028, 0x0004), core.VRCS, "MONOCHROME2"),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0010), 32768),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0011), 32768),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0100), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0101), 8),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0102), 7),
		dicomtest.NewUShortElement(core.NewTag(0x0028, 0x0103), 0),
	)
	data := append(header, dicomtest.ExplicitLongHeaderBytes(binary.LittleEndian, core.TagPixelData, core.VROB, forgedValueLength)...)
	data = append(data, forgedPayload()...)
	for _, opts := range []ReaderOptions{{}, forgedLimits} {
		sink := &testFrameSink{}
		opts.FrameSink = sink
		assertForgedValueLengthRejected(t, data, transfer.ExplicitVRLittleEndian, opts)
		if len(sink.frames) != 0 {
			t.Fatalf("forged frame delivered %d frame(s)", len(sink.frames))
		}
	}
}

// Values that the source holds in full still decode unchanged on every path.
func TestDefinedValueReadsKeepBytesAcrossSources(t *testing.T) {
	payload := make([]byte, 3*definedValueReadChunkSize+18)
	for i := range payload {
		payload[i] = byte(i * 7)
	}
	data := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, core.NewRawElement(core.NewTag(0x0009, 0x1010), core.VROB, payload))
	for _, opts := range []ReaderOptions{{}, {MaxElementBytes: int64(len(payload))}} {
		for name, source := range forgedSources(data) {
			ds, err := NewReader(source(), transfer.ExplicitVRLittleEndian, opts).ReadDataSet()
			if err != nil {
				t.Fatalf("%s limit=%d: %v", name, opts.MaxElementBytes, err)
			}
			raw, ok := ds.Elements[0].Value.(core.RawValue)
			if !ok || !bytes.Equal(raw, payload) {
				t.Fatalf("%s limit=%d: value of %d bytes differs from the payload", name, opts.MaxElementBytes, len(raw))
			}
		}
	}
}
