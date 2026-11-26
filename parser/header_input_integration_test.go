package parser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

type headerInputRange struct{ start, end int64 }
type headerInputGuard struct {
	*bytes.Reader
	forbidden []headerInputRange
	reads     []headerInputRange
}

func (s *headerInputGuard) Read(p []byte) (int, error) {
	start, _ := s.Seek(0, io.SeekCurrent)
	end := start + int64(len(p))
	for _, blocked := range s.forbidden {
		if start < blocked.end && end > blocked.start {
			return 0, errors.New("read intersects deferred payload")
		}
	}
	n, err := s.Reader.Read(p)
	s.reads = append(s.reads, headerInputRange{start, start + int64(n)})
	return n, err
}

func headerInputPayloadFixture(encapsulated bool) ([]byte, headerInputRange) {
	syntax := transfer.ExplicitVRLittleEndian
	data := append(headerInputManual(syntax, core.NewTag(0x0008, 0x103e), "LO", 2), 'A', 'B')
	length := uint32(1 << 20)
	if encapsulated {
		length = 0xffffffff
	}
	data = append(data, headerInputManual(syntax, core.TagPixelData, "OB", length)...)
	if encapsulated {
		data = append(data, headerInputManual(syntax, core.TagItem, "UN", 0)...)
		data = append(data, headerInputManual(syntax, core.TagItem, "UN", 1<<20)...)
	}
	blocked := headerInputRange{start: int64(len(data)), end: int64(len(data) + 1<<20)}
	data = append(data, bytes.Repeat([]byte{0x6d}, 1<<20)...)
	if encapsulated {
		data = append(data, headerInputManual(syntax, core.TagSequenceDelimitationItem, "UN", 0)...)
	}
	data = append(data, headerInputManual(syntax, core.NewTag(0xfffa, 0x0010), "LO", 2)...)
	data = append(data, 'C', 'D')
	return data, blocked
}

func TestHeaderInputSkipDeferNeverReadPayloadAndReplayRestoresCursor(t *testing.T) {
	for _, encapsulated := range []bool{false, true} {
		wire, blocked := headerInputPayloadFixture(encapsulated)
		syntax := transfer.ExplicitVRLittleEndian
		if encapsulated {
			syntax = transfer.JPEGBaseline
		}
		for _, deferValue := range []bool{false, true} {
			opts := ReaderOptions{Dictionary: std.Dictionary, SkipPixelData: !deferValue, DeferPixelData: deferValue, BaseOffset: 37}
			var expected core.DataSet
			for _, batch := range []bool{false, true} {
				guard := &headerInputGuard{Reader: bytes.NewReader(wire), forbidden: []headerInputRange{blocked}}
				r := NewReader(guard, syntax, opts)
				// Exercise the proven algorithm with an interval-checking reader;
				// production eligibility is tested independently on regular files.
				r.headerInputEnabled = batch
				ds, err := r.ReadDataSet()
				if err != nil {
					t.Fatalf("encapsulated=%v deferred=%v batch=%v: %v", encapsulated, deferValue, batch, err)
				}
				if !batch {
					expected = ds
				} else if !reflect.DeepEqual(ds, expected) {
					t.Fatal("batch changed dataset")
				}
				if len(ds.Elements) != 3 || ds.Elements[0].StringValue() != "AB" || ds.Elements[2].StringValue() != "CD" || ds.Elements[1].Value != nil {
					t.Fatalf("dataset=%+v", ds)
				}
				if r.Position() != int64(len(wire))+37 {
					t.Fatalf("logical cursor=%d", r.Position())
				}
				location, ok := r.ValueLocation(core.TagPixelData)
				if !ok || location.ValueOffset != 59 {
					t.Fatalf("location=%+v/%v", location, ok)
				}
				guard.forbidden = nil // Replay is the explicit caller-requested read.
				before, _ := guard.Seek(0, io.SeekCurrent)
				var out bytes.Buffer
				copied, err := r.CopyElementValueAt(location, &out)
				after, _ := guard.Seek(0, io.SeekCurrent)
				want := wire[location.ValueOffset-37 : location.ValueOffset-37+location.Length]
				if err != nil || copied != location.Length || !bytes.Equal(out.Bytes(), want) || before != after || r.Position() != int64(len(wire))+37 {
					t.Fatalf("replay copied=%d err=%v cursor=%d/%d", copied, err, before, after)
				}
			}
			path := filepath.Join(t.TempDir(), "nonphi.dcm")
			if err := os.WriteFile(path, wire, 0600); err != nil {
				t.Fatal(err)
			}
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			r := NewReader(f, syntax, opts)
			if !r.headerInputEnabled {
				t.Fatal("regular file did not select optimized header route")
			}
			ds, err := r.ReadDataSet()
			pos, _ := f.Seek(0, io.SeekCurrent)
			if err != nil || !reflect.DeepEqual(ds, expected) || pos != int64(len(wire)) {
				t.Fatalf("regular-file dataset/cursor/error %d/%v", pos, err)
			}
			if err := f.Close(); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestHeaderInputDefinedContainerFenceDoesNotReadOutside(t *testing.T) {
	for available := 1; available < 12; available++ {
		child := headerInputManual(transfer.ExplicitVRLittleEndian, core.TagPixelData, "OB", 0)
		wire := headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x1111), "SQ", uint32(8+available))
		wire = append(wire, headerInputManual(transfer.ExplicitVRLittleEndian, core.TagItem, "UN", uint32(available))...)
		wire = append(wire, child...)
		var wantErr string
		for _, batch := range []bool{false, true} {
			guard := &headerInputGuard{Reader: bytes.NewReader(wire), forbidden: []headerInputRange{{int64(20 + available), int64(len(wire))}}}
			r := NewReader(guard, transfer.ExplicitVRLittleEndian, ReaderOptions{})
			r.headerInputEnabled = batch
			_, err := r.ReadDataSet()
			if !errors.Is(err, ErrContainerOverrun) {
				t.Fatalf("available=%d batch=%v err=%v", available, batch, err)
			}
			if !batch {
				wantErr = err.Error()
			} else if err.Error() != wantErr {
				t.Fatalf("fence error changed %v / %s", err, wantErr)
			}
			pos, _ := guard.Seek(0, io.SeekCurrent)
			if pos > int64(20+available) || pos != r.Position() {
				t.Fatalf("fence cursor=%d logical=%d", pos, r.Position())
			}
		}
	}
}

func TestHeaderInputHookCanRejectOrDeferBeforeAnyValueRead(t *testing.T) {
	wire, blocked := headerInputPayloadFixture(false)
	for _, reject := range []bool{false, true} {
		guard := &headerInputGuard{Reader: bytes.NewReader(wire), forbidden: []headerInputRange{blocked}}
		var headers int
		chain, err := validation.NewHookChain(validation.HookRegistration{
			Name: "header-guard", Points: []validation.HookPoint{validation.HookElementHeaderRead},
			Hook: validation.HookFunc(func(ctx context.Context, event validation.HookEvent) (validation.HookDecision, error) {
				headers++
				if event.Header.Tag == core.TagPixelData {
					pos, _ := guard.Seek(0, io.SeekCurrent)
					if pos != blocked.start {
						t.Fatalf("hook cursor=%d want %d", pos, blocked.start)
					}
					if reject {
						return validation.HookDecision{}, errors.New("rejected header")
					}
					return validation.HookDecision{DeferValue: true}, nil
				}
				return validation.HookDecision{}, nil
			}),
		})
		if err != nil {
			t.Fatal(err)
		}
		r, err := NewReaderWithValidation(context.Background(), guard, transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary}, validation.Options{Hooks: chain})
		if err != nil {
			t.Fatal(err)
		}
		r.headerInputEnabled = true
		ds, err := r.ReadDataSet()
		if reject {
			pos, _ := guard.Seek(0, io.SeekCurrent)
			if err == nil || headers != 2 || pos != blocked.start {
				t.Fatalf("reject=%v headers=%d cursor=%d", err, headers, pos)
			}
		} else if err != nil || headers != 3 || len(ds.Elements) != 3 || ds.Elements[1].Value != nil {
			t.Fatalf("defer=%v headers=%d ds=%+v", err, headers, ds)
		}
	}
}

func TestHeaderInputLogicalBudgetNeverAllowsPhysicalReadBeyondLimit(t *testing.T) {
	wire := append(headerInputManual(transfer.ExplicitVRLittleEndian, core.NewTag(0x0008, 0x103e), "LO", 2), 'A', 'B')
	for limit := int64(1); limit <= int64(len(wire)); limit++ {
		guard := &headerInputGuard{Reader: bytes.NewReader(wire), forbidden: []headerInputRange{{limit, int64(len(wire))}}}
		r := NewReader(guard, transfer.ExplicitVRLittleEndian, ReaderOptions{MaxTotalBytes: limit})
		r.headerInputEnabled = true
		_, err := r.ReadDataSet()
		if !errors.Is(err, ErrMaxTotalBytesExceeded) {
			t.Fatalf("limit=%d: %v", limit, err)
		}
		pos, _ := guard.Seek(0, io.SeekCurrent)
		if pos != limit || r.Position() != limit {
			t.Fatalf("limit=%d physical=%d logical=%d", limit, pos, r.Position())
		}
	}
}
