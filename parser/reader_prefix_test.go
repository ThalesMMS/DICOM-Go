package parser

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestReaderPrefixRootStopAndOrdinaryCompleteness(t *testing.T) {
	first, stopTag := core.NewTag(8, 0x16), core.NewTag(0x10, 0x10)
	wire := pruningWire(binary.LittleEndian, first, "UI", 4, []byte("1.2\x00"))
	wire = append(wire, pruningWire(binary.LittleEndian, stopTag, "PN", 400, nil)...)
	makeReader := func() *Reader {
		r, err := NewSelectiveReader(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{}, SelectiveReaderOptions{MaxTokens: 2, Select: func(_ context.Context, p validation.Path, h core.ElementHeader, _ int64) (SelectiveDisposition, error) {
			if len(p) == 1 && h.Tag.Compare(stopTag) >= 0 {
				return SelectiveStop, nil
			}
			return SelectiveMaterialize, nil
		}})
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := makeReader()
	data, stop, err := r.ReadDataSetPrefix()
	if err != nil || len(data.Elements) != 1 || !stop.Stopped || stop.Offset != 12 || stop.Header.Tag != stopTag || r.Position() != 20 {
		t.Fatalf("prefix data %+v stop %+v pos %d err %v", data, stop, r.Position(), err)
	}
	if _, err := r.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("stop EOF %v", err)
	}
	if _, err := makeReader().ReadDataSet(); err == nil {
		t.Fatal("ordinary collector treated stop as complete")
	}
	if _, err := makeReader().ReadDataSetPartial(); err == nil {
		t.Fatal("partial collector treated stop as complete")
	}
	complete := pruningReader(t, context.Background(), bytes.NewReader(wire[:12]), transfer.ExplicitVRLittleEndian, ReaderOptions{}, 1)
	if data, stop, err := complete.ReadDataSetPrefix(); err != nil || stop.Stopped || len(data.Elements) != 1 {
		t.Fatalf("complete EOF %+v %+v %v", data, stop, err)
	}
}

func TestReaderPrefixNestedStopIsAnError(t *testing.T) {
	leaf := pruningWire(binary.LittleEndian, core.NewTag(0x10, 0x10), "PN", 2, []byte("AB"))
	body := append(pruningControl(binary.LittleEndian, 0xe000, uint32(len(leaf))), leaf...)
	wire := pruningWire(binary.LittleEndian, core.NewTag(8, 0x1111), "SQ", uint32(len(body)), body)
	r, err := NewSelectiveReader(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{}, SelectiveReaderOptions{Select: func(_ context.Context, p validation.Path, _ core.ElementHeader, _ int64) (SelectiveDisposition, error) {
		if len(p) > 1 {
			return SelectiveStop, nil
		}
		return SelectiveMaterialize, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if data, stop, err := r.ReadDataSetPrefix(); err == nil || stop.Stopped || len(data.Elements) != 0 {
		t.Fatalf("nested stop %+v %+v %v", data, stop, err)
	}
	if _, _, err := NewReader(bytes.NewReader(nil), transfer.ExplicitVRLittleEndian, ReaderOptions{}).ReadDataSetPrefix(); !errors.Is(err, ErrSelectiveReaderOptions) {
		t.Fatalf("ordinary reader admitted: %v", err)
	}
}
