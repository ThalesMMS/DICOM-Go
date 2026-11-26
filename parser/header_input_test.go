package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Manual wire fixtures keep the oracle independent of parser.Writer.
func headerInputManual(syntax transfer.Syntax, tag core.Tag, vr string, length uint32) []byte {
	order := syntax.ByteOrder
	data := make([]byte, 8)
	order.PutUint16(data[:2], tag.Group)
	order.PutUint16(data[2:4], tag.Element)
	if !syntax.ExplicitVR || tag.IsSequenceDelimiting() {
		order.PutUint32(data[4:], length)
		return data
	}
	copy(data[4:6], vr)
	if core.VR(vr).UsesLongExplicitLength() {
		data = append(data, 0, 0, 0, 0)
		order.PutUint32(data[8:], length)
	} else {
		order.PutUint16(data[6:], uint16(length))
	}
	return data
}

func headerInputCompare(t *testing.T, data []byte, syntax transfer.Syntax, opts ReaderOptions) {
	t.Helper()
	left, right := bytes.NewReader(data), bytes.NewReader(data)
	fast, legacy := NewReader(left, syntax, opts), NewReader(right, syntax, opts)
	legacy.headerInputEnabled = false // Differential oracle is the original field I/O.
	got, gotErr := fast.readHeader()
	want, wantErr := legacy.readHeader()
	if got != want || headerInputError(gotErr) != headerInputError(wantErr) {
		t.Fatalf("header/error\nfast: %+v %v\nlegacy: %+v %v", got, gotErr, want, wantErr)
	}
	for _, target := range []error{io.EOF, io.ErrUnexpectedEOF, ErrMaxTotalBytesExceeded, ErrContainerOverrun, ErrNonZeroReservedBytes} {
		if errors.Is(gotErr, target) != errors.Is(wantErr, target) {
			t.Fatalf("error identity for %v differs: %v / %v", target, gotErr, wantErr)
		}
	}
	var gotParse, wantParse *ParseError
	if errors.As(gotErr, &gotParse) != errors.As(wantErr, &wantParse) {
		t.Fatalf("ParseError identity differs: %v / %v", gotErr, wantErr)
	}
	if gotParse != nil && (gotParse.Op != wantParse.Op || gotParse.Offset != wantParse.Offset || gotParse.Tag != wantParse.Tag || gotParse.VR != wantParse.VR || gotParse.Length != wantParse.Length) {
		t.Fatalf("ParseError fields differ: %+v / %+v", gotParse, wantParse)
	}
	leftPos, _ := left.Seek(0, io.SeekCurrent)
	rightPos, _ := right.Seek(0, io.SeekCurrent)
	if fast.Position() != legacy.Position() || leftPos != rightPos || leftPos != fast.Position()-opts.BaseOffset {
		t.Fatalf("cursor differs fast(logical=%d physical=%d) legacy(%d %d)", fast.Position(), leftPos, legacy.Position(), rightPos)
	}
	if fast.headerInput.active || fast.headerInput.n != 0 {
		t.Fatal("header staging survived the header operation")
	}
}

func headerInputError(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestHeaderInputEveryTruncationAndBudget(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		for _, tc := range []struct {
			tag core.Tag
			vr  string
		}{
			{core.NewTag(0x0008, 0x103e), "LO"},
			{core.TagPixelData, "OB"},
			{core.TagItem, "UN"},
		} {
			data := append(headerInputManual(syntax, tc.tag, tc.vr, 2), 'A', 'B')
			for end := 0; end <= len(data); end++ {
				headerInputCompare(t, data[:end], syntax, ReaderOptions{Dictionary: std.Dictionary})
			}
			for budget := int64(1); budget <= int64(len(data)); budget++ {
				headerInputCompare(t, data, syntax, ReaderOptions{Dictionary: std.Dictionary, MaxTotalBytes: budget})
			}
			headerInputCompare(t, data, syntax, ReaderOptions{Dictionary: std.Dictionary, BaseOffset: 17, MaxTotalBytes: 23})
		}
	}
}

func TestHeaderInputInvalidVRAndReservedLeaveExactCursor(t *testing.T) {
	data := append(headerInputManual(transfer.ExplicitVRLittleEndian, core.TagPixelData, "OB", 2), 'A', 'B')
	badVR := append([]byte(nil), data...)
	copy(badVR[4:6], "ZZ")
	headerInputCompare(t, badVR, transfer.ExplicitVRLittleEndian, ReaderOptions{})
	data[6], data[7] = 1, 2
	for end := 0; end <= len(data); end++ {
		for _, strict := range []bool{false, true} {
			headerInputCompare(t, data[:end], transfer.ExplicitVRLittleEndian, ReaderOptions{StrictReservedBytes: strict})
		}
	}
	for _, wire := range [][]byte{badVR, data} {
		path := filepath.Join(t.TempDir(), "manual.dcm")
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		f, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		r := NewReader(f, transfer.ExplicitVRLittleEndian, ReaderOptions{StrictReservedBytes: true})
		if !r.headerInputEnabled {
			t.Fatal("regular file did not use proven source route")
		}
		_, gotErr := r.readHeader()
		pos, _ := f.Seek(0, io.SeekCurrent)
		want := int64(6)
		if wire[4] == 'O' {
			want = 8
		}
		if gotErr == nil || pos != want || r.Position() != want {
			t.Fatalf("cursor/error = %d/%d/%v want %d", pos, r.Position(), gotErr, want)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
}

type headerInputGenericSource struct {
	*bytes.Reader
	calls    int
	sizes    []int
	injected error
	maxRead  int
}

func (s *headerInputGenericSource) Read(p []byte) (int, error) {
	s.calls++
	s.sizes = append(s.sizes, len(p))
	if s.maxRead > 0 && len(p) > s.maxRead {
		p = p[:s.maxRead]
	}
	n, err := s.Reader.Read(p)
	if s.injected != nil && s.calls == 1 {
		return n, s.injected
	}
	return n, err
}

func TestHeaderInputGenericSizedSeekerKeepsFieldReadsAndNPlusError(t *testing.T) {
	data := append(headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x103e), "LO", 2), 'A', 'B')
	for _, chunk := range []int{0, 1, 2, 3} {
		s := &headerInputGenericSource{Reader: bytes.NewReader(data), injected: errors.New("n-plus-error"), maxRead: chunk}
		r := NewReader(s, transfer.ExplicitVRLittleEndian, ReaderOptions{})
		if r.headerInputEnabled {
			t.Fatal("size/seek alone authorized generic read semantics")
		}
		_, err := r.readHeader()
		if chunk == 1 {
			if err == nil || !strings.Contains(err.Error(), "n-plus-error") || r.Position() != 1 {
				t.Fatalf("short n+err: %v @%d", err, r.Position())
			}
		} else if err != nil {
			t.Fatalf("full uint16 must preserve ReadFull ignoring accompanying error: %v", err)
		}
		if len(s.sizes) == 0 || s.sizes[0] != 2 {
			t.Fatalf("field reads changed: %v", s.sizes)
		}
	}
	for _, source := range []io.Reader{io.NewSectionReader(bytes.NewReader(data), 0, int64(len(data))), &headerInputGenericSource{Reader: bytes.NewReader(data)}, bytes.NewBuffer(data)} {
		if NewReader(source, transfer.ExplicitVRLittleEndian, ReaderOptions{}).headerInputEnabled {
			t.Fatalf("unproven source promoted: %T", source)
		}
	}
}

func TestHeaderInputRecoveryAndPendingKeepOriginalRoute(t *testing.T) {
	data := headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x103e), "LO", 0)
	for _, opts := range []ReaderOptions{
		{RecoverDelimiterAnomalies: true}, {RecoverImplicitVRElements: true},
		{VRRecovery: VRRecoveryOptions{ShortLengthUN: true}},
		{SequenceRecovery: SequenceRecoveryOptions{UndefinedLengthOBOWAsSQ: true}},
	} {
		r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, opts)
		r.beginHeaderInput()
		if r.headerInput.active || r.Position() != 0 {
			t.Fatalf("recovery prefetched: %+v @%d", opts, r.Position())
		}
	}
	r := NewReader(bytes.NewReader(data), transfer.ExplicitVRLittleEndian, ReaderOptions{})
	r.counter.pending = []byte{0}
	r.beginHeaderInput()
	if r.headerInput.active {
		t.Fatal("pending unread was promoted")
	}
}

func TestHeaderInputStageCallsAreBoundedNotSyscallMeasurements(t *testing.T) {
	for _, tc := range []struct {
		vr     string
		strict bool
		want   []int
	}{
		{"LO", false, []int{6, 2}}, {"OB", false, []int{6, 6}}, {"OB", true, []int{6, 2, 4}},
	} {
		wire := append(headerInputManual(transfer.ExplicitVRLittleEndian, core.TagPixelData, tc.vr, 2), 'A', 'B')
		s := &headerInputGenericSource{Reader: bytes.NewReader(wire)}
		r := NewReader(s, transfer.ExplicitVRLittleEndian, ReaderOptions{StrictReservedBytes: tc.strict})
		// White-box exercise of staging interval bounds. Production rejects this
		// generic source; a separate os.File test covers the production gate.
		r.headerInputEnabled = true
		_, err := r.readHeader()
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(s.sizes, tc.want) {
			t.Fatalf("%s strict=%v calls=%v want=%v", tc.vr, tc.strict, s.sizes, tc.want)
		}
		pos, _ := s.Seek(0, io.SeekCurrent)
		if pos != int64(len(wire)-2) || r.Position() != pos {
			t.Fatalf("read value bytes: %d", pos)
		}
	}
	// Keep the endian oracle literal and independent of the header helper.
	if binary.LittleEndian.Uint16([]byte{0xe0, 0x7f}) != core.TagPixelData.Group {
		t.Fatal("invalid literal fixture")
	}
}
