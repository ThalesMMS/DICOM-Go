package parser

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestMixedContainerBudgetsSpanSiblingItemsAndSequences(t *testing.T) {
	firstTag := core.NewTag(0x0008, 0x1110)
	secondTag := core.NewTag(0x0008, 0x1115)
	firstBody := bytes.Join([][]byte{bItem(code01, 0), bItem(nil, 0)}, nil)
	first := bSequence(firstTag.Group, firstTag.Element, uint32(len(firstBody)), firstBody)
	secondBody := bytes.Join([][]byte{
		bControl(0xE000, uint32(core.UndefinedLength)), code02, bControl(0xE00D, 0),
		bItem(nil, 0), bControl(0xE0DD, 0),
	}, nil)
	second := bSequence(secondTag.Group, secondTag.Element, uint32(core.UndefinedLength), secondBody)
	for _, order := range []struct {
		name    string
		prefix  []byte
		last    []byte
		lastTag core.Tag
	}{
		{"defined then undefined", first, second, secondTag},
		{"undefined then defined", second, first, firstTag},
	} {
		t.Run(order.name, func(t *testing.T) {
			wire := bytes.Join([][]byte{order.prefix, order.last, sentinel}, nil)
			for _, tc := range []struct {
				name  string
				opts  ReaderOptions
				nodes int
				want  error
			}{
				{"primitive boundary", ReaderOptions{MaxElements: 3}, 0, nil},
				{"primitive budget not reset", ReaderOptions{MaxElements: 2}, 0, ErrMaxElementsExceeded},
				{"validated node boundary", ReaderOptions{}, 9, nil},
				{"validated node budget not reset", ReaderOptions{}, 8, ErrMaxElementsExceeded},
				{"validated empty items consume nodes", ReaderOptions{}, 7, ErrMaxElementsExceeded},
			} {
				t.Run(tc.name, func(t *testing.T) {
					tc.opts.Dictionary = std.Dictionary
					source := bytes.NewReader(wire)
					var reader *Reader
					if tc.nodes > 0 {
						var err error
						reader, err = NewReaderWithValidation(context.Background(), source, transfer.ExplicitVRLittleEndian, tc.opts, validation.Options{MaxElements: tc.nodes})
						if err != nil {
							t.Fatal(err)
						}
					} else {
						reader = NewReader(source, transfer.ExplicitVRLittleEndian, tc.opts)
					}
					dataset, err := reader.ReadDataSet()
					if !errors.Is(err, tc.want) {
						t.Fatalf("parse error = %v, want %v", err, tc.want)
					}
					if tc.want != nil {
						var parseErr *ParseError
						wantTag := core.NewTag(0x0010, 0x0010)
						if tc.nodes == 7 {
							wantTag = core.TagItem
						}
						if !errors.As(err, &parseErr) || parseErr.Op != OpCheckElementCount || parseErr.Tag != wantTag {
							t.Fatalf("rejected at the wrong node, want %s: %v", wantTag, err)
						}
						return
					}
					if len(dataset.Elements) != 3 || len(sequenceItems(t, dataset, order.lastTag)) != 2 || lastString(dataset) != "SENTINEL" {
						t.Fatalf("unexpected mixed dataset: %+v", dataset)
					}
				})
			}
		})
	}
}
