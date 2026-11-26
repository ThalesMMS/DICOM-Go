package object

import (
	"bytes"
	"compress/flate"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type sequenceReadSeeker struct {
	*bytes.Reader
	reads, seeks int
	closed       bool
}

func (s *sequenceReadSeeker) Read(p []byte) (int, error) { s.reads++; return s.Reader.Read(p) }
func (s *sequenceReadSeeker) Seek(n int64, w int) (int64, error) {
	s.seeks++
	return s.Reader.Seek(n, w)
}
func (s *sequenceReadSeeker) Close() error { s.closed = true; return nil }

func TestDefinedSequenceDeferredMetadataDoesNotReadOrSeek(t *testing.T) {
	wire := largeNestedValuePart10(t, 4096, 2048)
	source := &sequenceReadSeeker{Reader: bytes.NewReader(wire)}
	deferred, err := ReadFileWithOptions(source, ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	materialized, err := ReadFile(bytes.NewReader(wire))
	if err != nil {
		t.Fatal(err)
	}
	before, _ := source.Reader.Seek(0, io.SeekCurrent)
	source.reads, source.seeks = 0, 0
	top, _ := deferred.Dataset.Get(core.NewTag(0x0042, 0x0011))
	if n, err := deferred.Dataset.deferredEncodedLength(top); err != nil || n != 4096 {
		t.Fatalf("root length=%d: %v", n, err)
	}
	outer, _ := deferred.Dataset.GetSequence(nestedTestSequenceTag)
	offset, offsetSet := outer[0].ItemOffset()
	item := core.DataSet{Elements: outer[0].Elements(), ItemOffset: offset, ItemOffsetSet: offsetSet}
	leaf, _ := outer[0].Get(nestedTestBlobTag)
	if n, err := deferred.Dataset.nestedDeferredLength(item, leaf); err != nil || n != 2048 {
		t.Fatalf("item length=%d: %v", n, err)
	}
	if source.reads != 0 || source.seeks != 0 {
		t.Fatalf("metadata read=%d seek=%d", source.reads, source.seeks)
	}
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.DeflatedExplicitVRLittleEndian} {
		opts := WriteDataSetOptions{SequenceLength: SequenceLengthDefined, GroupLength: GroupLengthCreate, Padding: PaddingOptions{Policy: PaddingCreate, Alignment: 512, ItemAlignment: 64}}
		var got, want bytes.Buffer
		if err := WriteDataSetWithOptions(&got, deferred.Dataset, syntax, opts); err != nil {
			t.Fatal(err)
		}
		if err := WriteDataSetWithOptions(&want, materialized.Dataset, syntax, opts); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), want.Bytes()) {
			t.Fatal("deferred defined output differs")
		}
		after, _ := source.Reader.Seek(0, io.SeekCurrent)
		if before != after || source.closed {
			t.Fatalf("source position/lifetime changed: %d -> %d closed=%v", before, after, source.closed)
		}
		plain := got.Bytes()
		if syntax.Deflated {
			if len(plain)%2 != 0 {
				t.Fatal("odd Deflate stream without padding")
			}
			inflater := flate.NewReader(bytes.NewReader(plain))
			plain, err = io.ReadAll(inflater)
			_ = inflater.Close()
			if err != nil {
				t.Fatal(err)
			}
		}
		if len(plain)%512 != 0 {
			t.Fatal("logical dataset alignment lost")
		}
	}
	if source.reads == 0 {
		t.Fatal("payload was never emitted")
	}
}

func TestObjectSequenceLimitsFailBeforePrefixAndSource(t *testing.T) {
	wire := largeNestedValuePart10(t, 16, 2048)
	source := &sequenceReadSeeker{Reader: bytes.NewReader(wire)}
	file, err := ReadFileWithOptions(source, ReadFileOptions{InlineValueBytesThreshold: 1024, DeferNestedValues: true})
	if err != nil {
		t.Fatal(err)
	}
	source.reads, source.seeks = 0, 0
	var out bytes.Buffer
	if err := WriteFileWithOptions(&out, file, WriteFileOptions{SequenceLimits: SequenceWriteLimits{MaxDepth: -1}}); !errors.Is(err, parser.ErrWritePlanLimit) || out.Len() != 0 {
		t.Fatalf("invalid limits: bytes=%d err=%v", out.Len(), err)
	}
	if err := WriteDataSetWithOptions(&out, file.Dataset, file.TransferSyntax, WriteDataSetOptions{SequenceLimits: SequenceWriteLimits{MaxPlanBytes: -1}}); !errors.Is(err, parser.ErrWritePlanLimit) || out.Len() != 0 {
		t.Fatalf("invalid dataset limits: bytes=%d err=%v", out.Len(), err)
	}
	if source.reads != 0 || source.seeks != 0 {
		t.Fatal("invalid options replayed source")
	}
}

func TestDeferredEncodedLengthRefusesUnavailableMetadata(t *testing.T) {
	obj := FromElements(nil, nil)
	element := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: core.UndefinedLength, LengthSet: true}}
	if _, err := obj.deferredEncodedLength(element); !errors.Is(err, parser.ErrEncodedLengthUnavailable) {
		t.Fatalf("error=%v", err)
	}
}
