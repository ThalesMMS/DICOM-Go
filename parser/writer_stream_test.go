package parser

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"reflect"
	"testing"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	dicomenc "github.com/ThalesMMS/dicom-go/encoding"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func streamSQ(elements ...core.Element) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1110), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}}}}
}

func TestDefinedSequenceEmitsHeadersBeforeDeferredCopy(t *testing.T) {
	var output bytes.Buffer
	copyCalls := 0
	tag := core.NewTag(0x0011, 0x1010)
	deferred := core.Element{Header: core.ElementHeader{Tag: tag, VR: core.VROB, Length: 4, LengthSet: true}}
	w := NewWriterWithOptions(&output, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, GroupLength: GroupLengthCreate, DeferredValue: func(_ core.DataSet, el core.Element) (func(io.Writer) (int64, error), bool) {
		if el.Tag() != tag {
			return nil, false
		}
		return func(dst io.Writer) (int64, error) {
			copyCalls++
			if output.Len() != 44 {
				t.Errorf("destination before copy=%d, want SQ+Item+GL+OB headers44", output.Len())
			}
			n, err := dst.Write([]byte{1, 2, 3, 4})
			return int64(n), err
		}, true
	}})
	if err := w.WriteElement(streamSQ(deferred)); err != nil {
		t.Fatal(err)
	}
	if copyCalls != 1 || output.Len() != 48 {
		t.Fatalf("copy=%d output=%d", copyCalls, output.Len())
	}
	if binary.LittleEndian.Uint32(output.Bytes()[8:12]) != 36 || binary.LittleEndian.Uint32(output.Bytes()[16:20]) != 28 {
		t.Fatal("defined SQ/item lengths do not match emitted bytes")
	}
}

type streamBulkReader struct {
	*bytes.Reader
	closes *int
}

func (r *streamBulkReader) Close() error { *r.closes++; return nil }

func TestDefinedBulkPureSizeAndOwnership(t *testing.T) {
	for _, knownHeader := range []bool{false, true} {
		t.Run(map[bool]string{false: "size_callback", true: "header"}[knownHeader], func(t *testing.T) {
			var out bytes.Buffer
			opens, closes, sizeCalls := 0, 0, 0
			el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: core.BulkDataValue{URI: "synthetic:owned"}}
			if knownHeader {
				el.Header.Length, el.Header.LengthSet = 4, true
			}
			opts := WriterOptions{LengthPolicy: LengthPolicyDefined, GroupLength: GroupLengthCreate, BulkDataSize: func(core.BulkDataValue) (int64, error) {
				sizeCalls++
				if opens != 0 || out.Len() != 0 {
					t.Error("source opened/output emitted in sizing")
				}
				return 3, nil
			}, BulkDataResolver: func(core.BulkDataValue) (BulkDataSource, error) {
				opens++
				if out.Len() < 32 {
					t.Error("source resolved while planning")
				}
				return BulkDataSource{Reader: &streamBulkReader{bytes.NewReader([]byte{1, 2, 3}), &closes}, Size: 3}, nil
			}}
			if err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, opts).WriteElement(streamSQ(el)); err != nil {
				t.Fatal(err)
			}
			if opens != 1 || closes != 1 || !knownHeader && sizeCalls == 0 {
				t.Fatalf("opens=%d closes=%d size=%d", opens, closes, sizeCalls)
			}
			if !bytes.Equal(out.Bytes()[out.Len()-4:], []byte{1, 2, 3, 0}) {
				t.Fatal("bulk odd padding mismatch")
			}
		})
	}
	for name, size := range map[string]int64{"negative": -1, "sentinel": int64(core.UndefinedLength), "max_int64": math.MaxInt64} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			opens := 0
			el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: core.BulkDataValue{URI: "synthetic"}}
			err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, BulkDataSize: func(core.BulkDataValue) (int64, error) { return size, nil }, BulkDataResolver: func(core.BulkDataValue) (BulkDataSource, error) { opens++; return BulkDataSource{}, nil }}).WriteElement(streamSQ(el))
			if err == nil || out.Len() != 0 || opens != 0 {
				t.Fatalf("error=%v output=%d opens=%d", err, out.Len(), opens)
			}
		})
	}
}

func TestDefinedBulkUnknownSizeRefusesWithoutResolver(t *testing.T) {
	var out bytes.Buffer
	opens := 0
	el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: core.BulkDataValue{URI: "synthetic"}}
	err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, BulkDataResolver: func(core.BulkDataValue) (BulkDataSource, error) { opens++; return BulkDataSource{}, nil }}).WriteElement(streamSQ(el))
	if !errors.Is(err, ErrEncodedLengthUnavailable) || out.Len() != 0 || opens != 0 {
		t.Fatalf("error=%v output=%d opens=%d", err, out.Len(), opens)
	}
}

func TestDefinedEncapsulatedDeferredMetadata(t *testing.T) {
	var value bytes.Buffer
	for _, part := range []struct {
		tag     core.Tag
		length  uint32
		payload []byte
	}{{core.TagItem, 0, nil}, {core.TagItem, 2, []byte{0xff, 0xd9}}, {core.TagSequenceDelimitationItem, 0, nil}} {
		value.Write(binary.LittleEndian.AppendUint16(nil, part.tag.Group))
		value.Write(binary.LittleEndian.AppendUint16(nil, part.tag.Element))
		value.Write(binary.LittleEndian.AppendUint32(nil, part.length))
		value.Write(part.payload)
	}
	el := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}}
	for _, hasSize := range []bool{false, true} {
		t.Run(map[bool]string{false: "missing", true: "known"}[hasSize], func(t *testing.T) {
			var out bytes.Buffer
			copies := 0
			opts := WriterOptions{LengthPolicy: LengthPolicyDefined, DeferredValue: func(core.DataSet, core.Element) (func(io.Writer) (int64, error), bool) {
				return func(w io.Writer) (int64, error) { copies++; return io.Copy(w, bytes.NewReader(value.Bytes())) }, true
			}}
			if hasSize {
				opts.DeferredValueLength = func(core.DataSet, core.Element) (int64, error) {
					if copies != 0 {
						t.Error("copied while measuring")
					}
					return int64(value.Len()), nil
				}
			}
			err := NewWriterWithOptions(&out, transfer.JPEGBaseline, opts).WriteElement(streamSQ(el))
			if !hasSize {
				if !errors.Is(err, ErrEncodedLengthUnavailable) || copies != 0 || out.Len() != 0 {
					t.Fatalf("%v copies%d bytes%d", err, copies, out.Len())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if copies != 1 || !bytes.HasSuffix(out.Bytes(), value.Bytes()) {
				t.Fatalf("copies=%d output mismatch", copies)
			}
			if _, err := NewReader(bytes.NewReader(out.Bytes()), transfer.JPEGBaseline, ReaderOptions{}).ReadDataSet(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestSequencePlanLimitsAndOverflowBeforeHeader(t *testing.T) {
	leaf := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2})
	for name, limits := range map[string]SequenceWriteLimits{"depth": {MaxDepth: 2}, "nodes": {MaxPlanNodes: 1}, "bytes": {MaxPlanBytes: 1}, "negative": {MaxDepth: -1}} {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, SequenceLimits: limits}).WriteElement(streamSQ(streamSQ(leaf)))
			if !errors.Is(err, ErrWritePlanLimit) || out.Len() != 0 {
				t.Fatalf("error=%v bytes=%d", err, out.Len())
			}
		})
	}
	var out bytes.Buffer
	copies := 0
	el := core.Element{Header: core.ElementHeader{Tag: leaf.Tag(), VR: core.VROB, Length: 0xfffffffe, LengthSet: true}}
	err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, DeferredValue: func(core.DataSet, core.Element) (func(io.Writer) (int64, error), bool) {
		return func(io.Writer) (int64, error) { copies++; return 0, nil }, true
	}}).WriteElement(streamSQ(el))
	if err == nil || out.Len() != 0 || copies != 0 {
		t.Fatalf("overflow error=%v bytes=%d copies=%d", err, out.Len(), copies)
	}
	if _, err := checkedWriteAdd(math.MaxInt64, 1); err == nil {
		t.Fatal("offset overflow admitted")
	}
}

func TestDefinedSequenceCharsetAndInputImmutability(t *testing.T) {
	utf8Item := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: tagSpecificCharacterSet, VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}}, streamSQ(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"é^β"}})}}
	latinItem := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"é^X"}}, core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{7})}}
	el := streamSQ()
	el.Value = core.SequenceValue{Items: []core.DataSet{utf8Item, latinItem}}
	before := el.Value.(core.SequenceValue).Items[1].Elements[1].Value.(core.RawValue)
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		var out bytes.Buffer
		if err := NewWriterWithOptions(&out, syntax, WriterOptions{LengthPolicy: LengthPolicyDefined, CharacterSet: dicomenc.ISOIR100}).WriteElement(el); err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(out.Bytes(), []byte("é^β")) || !bytes.Contains(out.Bytes(), []byte{0xe9, '^', 'X', ' '}) {
			t.Fatalf("charset isolation %s", syntax.UID)
		}
		if !reflect.DeepEqual([]byte(before), []byte{7}) {
			t.Fatal("input mutated")
		}
		if _, err := NewReader(bytes.NewReader(out.Bytes()), syntax, ReaderOptions{}).ReadDataSet(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDefinedDeferredUsesActualCountsAndCapsOverrun(t *testing.T) {
	el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB, Length: 4, LengthSet: true}}
	for _, mode := range []string{"lie", "overrun", "truncated", "source_error"} {
		t.Run(mode, func(t *testing.T) {
			var out bytes.Buffer
			cause := errors.New("synthetic source failure")
			copies := 0
			opts := WriterOptions{LengthPolicy: LengthPolicyDefined, DeferredValue: func(core.DataSet, core.Element) (func(io.Writer) (int64, error), bool) {
				return func(dst io.Writer) (int64, error) {
					copies++
					switch mode {
					case "lie":
						return 4, nil
					case "overrun":
						n, err := dst.Write([]byte{1, 2, 3, 4, 5, 6})
						return int64(n), err
					case "source_error":
						n, _ := dst.Write([]byte{1, 2})
						return int64(n), cause
					default:
						n, err := dst.Write([]byte{1, 2})
						return int64(n), err
					}
				}, true
			}}
			err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, opts).WriteElement(streamSQ(el))
			if err == nil || copies != 1 || out.Len() > 36 {
				t.Fatalf("error=%v copies=%d bytes=%d", err, copies, out.Len())
			}
			if mode == "source_error" && !errors.Is(err, cause) {
				t.Fatal("source error lost")
			}
		})
	}
}

// Existing writeAll accepts positive short writes without an error and retries
// their remainder. The streaming counter must preserve that policy.
type streamShortWriter struct{ bytes.Buffer }

func (w *streamShortWriter) Write(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return w.Buffer.Write(p)
}
func TestDefinedSequencePreservesPositiveShortWritePolicy(t *testing.T) {
	el := streamSQ(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4}))
	var want bytes.Buffer
	if err := NewWriterWithOptions(&want, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}).WriteElement(el); err != nil {
		t.Fatal(err)
	}
	var got streamShortWriter
	if err := NewWriterWithOptions(&got, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}).WriteElement(el); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(want.Bytes(), got.Bytes()) {
		t.Fatal("short writes changed serialized bytes")
	}
}

func TestUndefinedSequenceDepthAndCP246RetentionGuards(t *testing.T) {
	leaf := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROW, []byte{0, 1, 0, 2})
	var out bytes.Buffer
	err := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{SequenceLimits: SequenceWriteLimits{MaxDepth: 2}}).WriteElement(streamSQ(streamSQ(leaf)))
	if !errors.Is(err, ErrWritePlanLimit) {
		t.Fatalf("undefined depth: %v", err)
	}
	un := streamSQ(leaf)
	un.Header.VR = core.VRUN
	out.Reset()
	err = NewWriterWithOptions(&out, transfer.ExplicitVRBigEndian, WriterOptions{SequenceLimits: SequenceWriteLimits{MaxRetainedValueBytes: 2}}).WriteElement(un)
	if !errors.Is(err, ErrWritePlanLimit) || out.Len() != 0 {
		t.Fatalf("CP246 retention: bytes%d err%v", out.Len(), err)
	}
}

func TestGroupSizingCannotReusePolicyScratchReservation(t *testing.T) {
	// The policy scratch is live while Group Length measures this Bulk leaf.
	// Its temporary layout must use the residual after that scratch, not the
	// capacity which was available before the scratch allocation.
	var out bytes.Buffer
	sizeCalls := 0
	el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: core.BulkDataValue{URI: "synthetic"}}
	scratch, err := groupScratchBytes(1)
	if err != nil {
		t.Fatal(err)
	}
	capacity := int64(unsafe.Sizeof(plannedWriteElement{})) + int64(unsafe.Sizeof(plannedWriteItem{})) + scratch + int64(unsafe.Sizeof(plannedWriteElement{})) - 1
	opts := WriterOptions{LengthPolicy: LengthPolicyDefined, GroupLength: GroupLengthCreate, SequenceLimits: SequenceWriteLimits{MaxPlanBytes: capacity}, BulkDataSize: func(core.BulkDataValue) (int64, error) { sizeCalls++; return 2, nil }, BulkDataResolver: func(core.BulkDataValue) (BulkDataSource, error) {
		t.Error("resolver called")
		return BulkDataSource{}, nil
	}}
	err = NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, opts).WriteElement(streamSQ(el))
	if !errors.Is(err, ErrWritePlanLimit) || out.Len() != 0 || sizeCalls != 0 {
		t.Fatalf("error=%v output=%d premature sizing=%d", err, out.Len(), sizeCalls)
	}
}
