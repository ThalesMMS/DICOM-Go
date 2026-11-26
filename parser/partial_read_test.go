package parser

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

var partialSQ = core.NewTag(0x0008, 0x1110)
var partialLeaf = core.NewTag(0x0008, 0x1155)

func partialItem(syntax transfer.Syntax, body []byte, defined bool) []byte {
	length := uint32(core.UndefinedLength)
	if defined {
		length = uint32(len(body))
	}
	wire := append(dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItem, length), body...)
	if !defined {
		wire = append(wire, dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagItemDelimitationItem, 0)...)
	}
	return wire
}

func partialSequence(syntax transfer.Syntax, body []byte, defined bool) []byte {
	length := uint32(core.UndefinedLength)
	if defined {
		length = uint32(len(body))
	}
	wire := append(dicomtest.SequenceHeaderBytes(syntax, partialSQ, length), body...)
	if !defined {
		wire = append(wire, dicomtest.SequenceControlBytes(syntax.ByteOrder, core.TagSequenceDelimitationItem, 0)...)
	}
	return wire
}

func TestPartialSequenceAcrossSyntaxesAndLengths(t *testing.T) {
	for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.ImplicitVRLittleEndian} {
		for _, sqDefined := range []bool{false, true} {
			for _, itemDefined := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/sq=%v/item=%v", syntax.UID, sqDefined, itemDefined), func(t *testing.T) {
					root := definedElementBytes(syntax, core.NewTag(0x0010, 0x0020), core.VRLO, 4, []byte("ROOT"))
					leaf := definedElementBytes(syntax, partialLeaf, core.VRUI, 4, []byte("1.2\x00"))
					bad := definedElementBytes(syntax, core.NewTag(0x0010, 0x0010), core.VRPN, 8, []byte("TRUNCATE"))
					first := partialItem(syntax, leaf, itemDefined)
					second := partialItem(syntax, append(append([]byte(nil), leaf...), bad...), itemDefined)
					full := append(append([]byte(nil), root...), partialSequence(syntax, append(append([]byte(nil), first...), second...), sqDefined)...)
					headerLen := len(dicomtest.SequenceHeaderBytes(syntax, partialSQ, 0))
					cut := len(root) + headerLen + len(first) + 8 + len(leaf) + len(bad) - 3
					wire := full[:cut]
					for _, seekable := range []bool{false, true} {
						var source io.Reader = bytes.NewReader(wire)
						if !seekable {
							source = io.MultiReader(source)
						}
						reader := NewReader(source, syntax, ReaderOptions{Dictionary: std.Dictionary, BaseOffset: 73})
						ds, err := reader.ReadDataSetPartial()
						var partial *PartialDataSetError
						var parse *ParseError
						if !errors.As(err, &partial) || !errors.As(err, &parse) || len(ds.Elements) != 2 {
							t.Fatalf("partial = %v, %d roots", err, len(ds.Elements))
						}
						items := sequenceItems(t, ds, partialSQ)
						if len(items) != 2 || len(items[0].Elements) != 1 || len(items[1].Elements) != 1 {
							t.Fatalf("lost complete items/elements: %+v", items)
						}
						wantOffset := int64(73 + len(root) + headerLen + len(first))
						if !items[1].ItemOffsetSet || items[1].ItemOffset != wantOffset {
							t.Fatalf("item offset = %d/%v, want %d", items[1].ItemOffset, items[1].ItemOffsetSet, wantOffset)
						}
						wantPath := validation.Path{{Tag: partialSQ, ItemIndex: 1}, {Tag: core.NewTag(0x0010, 0x0010), ItemIndex: -1}}
						if !reflect.DeepEqual(partial.Path, wantPath) || len(partial.Containers) != 2 || !partial.Containers[0].Retained || !partial.Containers[1].Retained || partial.Containers[1].Offset != wantOffset {
							t.Fatalf("diagnostic = %+v", partial)
						}
						if reader.Position() > int64(73+len(wire)) {
							t.Fatal("read beyond finite input")
						}
						strict, strictErr := NewReader(bytes.NewReader(wire), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSet()
						if strictErr == nil || len(strict.Elements) != 0 {
							t.Fatal("strict reader accepted a prefix")
						}
					}
					strict, err := NewReader(bytes.NewReader(full), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSet()
					if err != nil {
						t.Fatal(err)
					}
					complete, err := NewReader(bytes.NewReader(full), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSetPartial()
					if err != nil || !reflect.DeepEqual(strict, complete) {
						t.Fatalf("complete read changed: %v", err)
					}
				})
			}
		}
	}
}

func TestPartialSequenceEmptyAndClosedContainers(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	leaf := bElement(0x0008, 0x1155, "UI", []byte("1.2\x00"))
	closed := partialItem(syntax, leaf, false)
	start := dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength))
	openItem := bControl(0xE000, uint32(core.UndefinedLength))
	for _, tc := range []struct {
		name                     string
		body                     []byte
		roots, items, containers int
		retained                 []bool
	}{
		{"empty sequence", nil, 0, 0, 1, []bool{false}},
		{"empty open item", openItem, 0, 0, 2, []bool{false, false}},
		{"closed item", closed, 1, 1, 1, []bool{true}},
		{"closed empty item", partialItem(syntax, nil, false), 1, 1, 1, []bool{true}},
		{"closed then empty open", append(append([]byte(nil), closed...), openItem...), 1, 1, 2, []bool{true, false}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ds, err := NewReader(bytes.NewReader(append(append([]byte(nil), start...), tc.body...)), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSetPartial()
			var partial *PartialDataSetError
			if !errors.As(err, &partial) || len(ds.Elements) != tc.roots || len(partial.Containers) != tc.containers {
				t.Fatalf("result = %+v, %v", ds, err)
			}
			if tc.roots > 0 && len(sequenceItems(t, ds, partialSQ)) != tc.items {
				t.Fatal("wrong item prefix")
			}
			for i, want := range tc.retained {
				if partial.Containers[i].Retained != want {
					t.Fatalf("container %d = %+v", i, partial.Containers[i])
				}
			}
		})
	}
}

func TestPartialSequenceHeaderAndDelimiterCuts(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	start := dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength))
	leaf := bElement(0x0008, 0x1155, "UI", []byte("1.2\x00"))
	closed := partialItem(syntax, leaf, false)
	for _, tc := range []struct {
		name            string
		tail            []byte
		items, elements int
	}{
		{"partial next item header", bControl(0xE000, uint32(core.UndefinedLength))[:3], 1, 1},
		{"partial primitive header", append(bControl(0xE000, uint32(core.UndefinedLength)), leaf[:5]...), 1, 1},
		{"partial item delimiter", bytes.Join([][]byte{bControl(0xE000, uint32(core.UndefinedLength)), leaf, bControl(0xE00D, 0)[:6]}, nil), 2, 1},
		{"partial sequence delimiter", bControl(0xE0DD, 0)[:6], 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire := bytes.Join([][]byte{start, closed, tc.tail}, nil)
			ds, err := NewReader(bytes.NewReader(wire), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSetPartial()
			if err == nil {
				t.Fatal("truncated framing accepted")
			}
			items := sequenceItems(t, ds, partialSQ)
			if len(items) != tc.items || len(items[len(items)-1].Elements) != tc.elements {
				t.Fatalf("invented item/value: %+v", items)
			}
		})
	}
}

func TestPartialSequenceNestedPrefixesAndPixelData(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	leaf := bElement(0x0008, 0x1155, "UI", []byte("1.2\x00"))
	inner := append(dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength)), partialItem(syntax, leaf, false)...)
	wire := bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength)), bControl(0xE000, uint32(core.UndefinedLength)), inner}, nil)
	ds, err := NewReader(bytes.NewReader(wire), syntax, ReaderOptions{Dictionary: std.Dictionary}).ReadDataSetPartial()
	var partial *PartialDataSetError
	if !errors.As(err, &partial) || len(partial.Containers) != 3 {
		t.Fatalf("nested diagnostic = %v", err)
	}
	for _, frame := range partial.Containers {
		if !frame.Retained {
			t.Fatalf("lost nested prefix: %+v", frame)
		}
	}
	if got := partial.Path.String(); got != partialSQ.String()+"[0]/"+partialSQ.String() {
		t.Fatalf("path = %s", got)
	}
	outer := sequenceItems(t, ds, partialSQ)
	if len(sequenceItems(t, outer[0], partialSQ)) != 1 {
		t.Fatal("lost completed nested item")
	}
	// Encapsulated values remain atomic even when their earlier fragments closed.
	pixel := bytes.Join([][]byte{explicitLongHeaderBytes(syntax.ByteOrder, core.TagPixelData, core.VROB, [2]byte{}, 0xFFFFFFFF), bControl(0xE000, 0), bControl(0xE000, 2), {1, 2}, bControl(0xE000, 8), {3, 4}}, nil)
	wire = bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength)), bControl(0xE000, uint32(core.UndefinedLength)), leaf, pixel}, nil)
	for _, deferred := range []bool{false, true} {
		reader := NewReader(bytes.NewReader(wire), syntax, ReaderOptions{Dictionary: std.Dictionary, DeferPixelData: deferred})
		ds, err = reader.ReadDataSetPartial()
		if !errors.As(err, &partial) || partial.Path.String() != partialSQ.String()+"[0]/"+core.TagPixelData.String() {
			t.Fatalf("pixel path = %v", err)
		}
		items := sequenceItems(t, ds, partialSQ)
		if len(items[0].Elements) != 1 {
			t.Fatal("published incomplete fragments")
		}
		if _, ok := reader.ValueLocation(core.TagPixelData); ok {
			t.Fatal("fabricated encapsulated source span")
		}
	}
}

func TestPartialSequenceNeverRepairsOrBypassesLimits(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	start := bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength)), bControl(0xE000, uint32(core.UndefinedLength)), bElement(0x0008, 0x1155, "UI", []byte("1.2\x00"))}, nil)
	for _, tc := range []struct {
		name  string
		tail  []byte
		opts  ReaderOptions
		cause error
	}{
		{"wrong delimiter", bControl(0xE0DD, 0), ReaderOptions{}, ErrUnexpectedSequenceDelimiter},
		{"nonzero delimiter", bControl(0xE00D, 2), ReaderOptions{}, ErrUnexpectedDelimiterLength},
		{"depth", dicomtest.SequenceHeaderBytes(syntax, partialSQ, uint32(core.UndefinedLength)), ReaderOptions{MaxSequenceDepth: 2}, ErrMaxDepthExceeded},
		{"element count", bElement(0x0010, 0x0020, "LO", []byte("NEXT")), ReaderOptions{MaxElements: 1}, ErrMaxElementsExceeded},
		{"value bytes", bElement(0x0010, 0x0020, "LO", []byte("NEXTNEXT")), ReaderOptions{MaxElementBytes: 4}, ErrMaxElementBytesExceeded},
		{"total bytes", bElement(0x0010, 0x0020, "LO", []byte("NEXT")), ReaderOptions{MaxTotalBytes: int64(len(start))}, ErrMaxTotalBytesExceeded},
		{"control guess", bControl(0x0000, 0), ReaderOptions{}, ErrUnexpectedSequenceControlTag},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.opts.Dictionary = std.Dictionary
			wire := append(append([]byte(nil), start...), tc.tail...)
			reader := NewReader(bytes.NewReader(wire), syntax, tc.opts)
			ds, err := reader.ReadDataSetPartial()
			if !errors.Is(err, tc.cause) || len(sequenceItems(t, ds, partialSQ)[0].Elements) != 1 {
				t.Fatalf("result = %v", err)
			}
			if reader.Position() > int64(len(start)+len(tc.tail)) {
				t.Fatal("read beyond rejected input")
			}
		})
	}
}

func TestPartialReadValidationLifecycleRemainsStrict(t *testing.T) {
	wire := bytes.Join([][]byte{dicomtest.SequenceHeaderBytes(transfer.ExplicitVRLittleEndian, partialSQ, uint32(core.UndefinedLength)), bControl(0xE000, uint32(core.UndefinedLength)), bElement(0x0008, 0x1155, "UI", []byte("1.2\x00"))}, nil)
	completed := 0
	chain, err := validation.NewHookChain(validation.HookRegistration{Name: "completion", Points: []validation.HookPoint{validation.HookItemComplete, validation.HookSequenceComplete}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		completed++
		return validation.HookDecision{}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	reader, err := NewReaderWithValidation(context.Background(), bytes.NewReader(wire), transfer.ExplicitVRLittleEndian, ReaderOptions{Dictionary: std.Dictionary}, validation.Options{Mode: validation.ModePreserve, Dictionary: std.Dictionary, Hooks: chain})
	if err != nil {
		t.Fatal(err)
	}
	ds, err := reader.ReadDataSetPartial()
	var partial *PartialDataSetError
	if err == nil || errors.As(err, &partial) || len(ds.Elements) != 0 || completed != 0 {
		t.Fatalf("validation partial bypass: %v, %d hooks", err, completed)
	}
}
