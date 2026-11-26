package parser

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// inspectPadding walks raw Explicit LE bytes independently of parser.Reader,
// records only Item Value lengths, and checks OB zeros/order and FFFC GL.
func inspectPadding(t *testing.T, b []byte) []int {
	t.Helper()
	var itemLengths []int
	var dataset func(int, int, bool) int
	dataset = func(start, end int, undefined bool) int {
		pos := start
		padded := false
		groupLengthPos := -1
		var groupLength uint32
		for pos < end {
			if pos+8 > end {
				t.Fatal("short header")
			}
			group, element := binary.LittleEndian.Uint16(b[pos:]), binary.LittleEndian.Uint16(b[pos+2:])
			if group == 0xfffe && element == 0xe00d {
				if !undefined {
					t.Fatal("unexpected item delimiter")
				}
				break
			}
			if padded {
				t.Fatal("padding was not the final element")
			}
			header := 8
			vr := string(b[pos+4 : pos+6])
			length := uint32(binary.LittleEndian.Uint16(b[pos+6:]))
			if vr == "OB" || vr == "OW" || vr == "SQ" || vr == "UN" {
				header = 12
				if pos+12 > end {
					t.Fatal("short long header")
				}
				length = binary.LittleEndian.Uint32(b[pos+8:])
			}
			valueStart := pos + header
			if vr == "SQ" {
				seqEnd := end
				if length != ^uint32(0) {
					seqEnd = valueStart + int(length)
					if seqEnd > end {
						t.Fatal("sequence overrun")
					}
				}
				p := valueStart
				for p < seqEnd {
					if p+8 > seqEnd || binary.LittleEndian.Uint16(b[p:]) != 0xfffe {
						t.Fatal("bad item header")
					}
					e := binary.LittleEndian.Uint16(b[p+2:])
					if e == 0xe0dd {
						p += 8
						break
					}
					if e != 0xe000 {
						t.Fatal("bad item tag")
					}
					n := binary.LittleEndian.Uint32(b[p+4:])
					from := p + 8
					if n == ^uint32(0) {
						p = dataset(from, seqEnd, true)
						itemLengths = append(itemLengths, p-from)
						p += 8
					} else {
						p = from + int(n)
						if p > seqEnd {
							t.Fatal("item overrun")
						}
						if dataset(from, p, false) != p {
							t.Fatal("defined item mismatch")
						}
						itemLengths = append(itemLengths, int(n))
					}
				}
				pos = p
				if length != ^uint32(0) && pos != seqEnd {
					t.Fatal("defined sequence mismatch")
				}
				continue
			}
			if length == ^uint32(0) || valueStart+int(length) > end {
				t.Fatal("value overrun")
			}
			if group == 0xfffc && element == 0 {
				if vr != "UL" || length != 4 {
					t.Fatal("invalid FFFC group length")
				}
				groupLengthPos = valueStart + 4
				groupLength = binary.LittleEndian.Uint32(b[valueStart:])
			}
			if group == 0xfffc && element == 0xfffc {
				if vr != "OB" || length%2 != 0 {
					t.Fatal("invalid padding VR/length")
				}
				for _, x := range b[valueStart : valueStart+int(length)] {
					if x != 0 {
						t.Fatal("nonzero generated padding")
					}
				}
				padded = true
			}
			pos = valueStart + int(length)
		}
		if groupLengthPos >= 0 && int(groupLength) != pos-groupLengthPos {
			t.Fatalf("FFFC GL=%d bytes=%d", groupLength, pos-groupLengthPos)
		}
		return pos
	}
	if dataset(0, len(b), false) != len(b) {
		t.Fatal("trailing bytes")
	}
	return itemLengths
}

func paddingFixture(length int) []core.Element {
	item := core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, make([]byte, length)), core.NewRawElement(tagDataSetPadding, core.VROB, []byte{0, 0, 0, 0, 0, 0})}}
	return []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(8, 0x1111), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{item, {}}}}, core.NewRawElement(tagDataSetPadding, core.VROB, []byte{0, 0})}
}

func writePaddingFixture(elements []core.Element, opts WriterOptions) ([]byte, error) {
	var b bytes.Buffer
	writer := NewWriterWithOptions(&b, transfer.ExplicitVRLittleEndian, opts)
	kept, err := writer.PrepareDataSetElements(elements, nil)
	if err != nil {
		return nil, err
	}
	for _, el := range kept {
		if err := writer.WriteElement(el); err != nil {
			return b.Bytes(), err
		}
	}
	if err := writer.WriteDataSetPadding(int64(b.Len()), 0, elements); err != nil {
		return b.Bytes(), err
	}
	return b.Bytes(), nil
}

func TestWriterPaddingLengthsAndSource(t *testing.T) {
	for _, policy := range []LengthPolicy{LengthPolicyUndefined, LengthPolicyDefined, LengthPolicyPreserve} {
		for _, alignment := range []uint32{2, 4, 16, 32, 512, 4096} {
			for length := 0; length <= 70; length += 2 {
				t.Run(fmt.Sprintf("%d/%d/%d", policy, alignment, length), func(t *testing.T) {
					el := paddingFixture(length)
					before := fmt.Sprintf("%#v", el)
					b, err := writePaddingFixture(el, WriterOptions{LengthPolicy: policy, Padding: PaddingOptions{Policy: PaddingCreate, Alignment: alignment, ItemAlignment: alignment}})
					if err != nil {
						t.Fatal(err)
					}
					if len(b)%int(alignment) != 0 {
						t.Fatalf("top length %d", len(b))
					}
					for _, n := range inspectPadding(t, b) {
						if n%int(alignment) != 0 {
							t.Fatalf("item Value length %d", n)
						}
					}
					if fmt.Sprintf("%#v", el) != before {
						t.Fatal("source changed")
					}
				})
			}
		}
	}
}

func TestWriterPaddingBudgetAndMeasurements(t *testing.T) {
	item := core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2})}}
	seq := []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(8, 0x1111), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{item, item}}}}
	for _, length := range []LengthPolicy{LengthPolicyUndefined, LengthPolicyDefined} {
		for _, group := range []GroupLengthPolicy{GroupLengthPreserve, GroupLengthCreate} {
			// GroupLengthCreate adds 12 bytes to each item's base, so it needs
			// 38 bytes of padding (OB+GL+Value), compared with 18 at Preserve.
			budget := int64(36)
			if group == GroupLengthCreate {
				budget = 76
			}
			opts := WriterOptions{LengthPolicy: length, GroupLength: group, Padding: PaddingOptions{Policy: PaddingCreate, ItemAlignment: 32, MaxAddedBytes: budget}}
			b, err := writePaddingFixture(seq, opts)
			if err != nil {
				t.Fatalf("measurement spent budget: %v", err)
			}
			inspectPadding(t, b)
			opts.Padding.MaxAddedBytes = budget - 1
			if _, err := writePaddingFixture(seq, opts); err == nil {
				t.Fatal("shared budget exceeded")
			}
		}
	}
}

func TestWriterPaddingPoliciesAndGroupLength(t *testing.T) {
	for _, group := range []GroupLengthPolicy{GroupLengthRecalculate, GroupLengthCreate, GroupLengthRemove} {
		el := paddingFixture(6)
		el = append(el[:1], core.Element{Header: core.ElementHeader{Tag: tagPaddingGroupLength, VR: core.VRUL}, Value: core.Uint32Value{123}}, el[1])
		b, err := writePaddingFixture(el, WriterOptions{GroupLength: group, Padding: PaddingOptions{Policy: PaddingCreate, Alignment: 64, ItemAlignment: 32}})
		if err != nil {
			t.Fatal(err)
		}
		inspectPadding(t, b)
		removed, err := writePaddingFixture(el, WriterOptions{GroupLength: GroupLengthRemove, Padding: PaddingOptions{Policy: PaddingRemove}})
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(removed, []byte{0xfc, 0xff, 0xfc, 0xff}) {
			t.Fatal("padding retained")
		}
	}
	base := paddingFixture(4)
	before := fmt.Sprintf("%#v", base)
	a, e := writePaddingFixture(base, WriterOptions{})
	if e != nil {
		t.Fatal(e)
	}
	b, e := writePaddingFixture(base, WriterOptions{Padding: PaddingOptions{Policy: PaddingPreserve}})
	if e != nil || !reflect.DeepEqual(a, b) || fmt.Sprintf("%#v", base) != before {
		t.Fatal("zero default changed")
	}
}

func TestWriterPaddingInvalidAndFinite(t *testing.T) {
	for _, opts := range []PaddingOptions{{Policy: 99}, {Policy: PaddingCreate, Alignment: 3}, {Policy: PaddingPreserve, ItemAlignment: 2}, {MaxAddedBytes: -1}, {Policy: PaddingCreate, Alignment: 4294967294}} {
		var out bytes.Buffer
		w := NewWriterWithOptions(&out, transfer.ExplicitVRLittleEndian, WriterOptions{Padding: opts})
		err := w.WriteDataSetPadding(2, 0, nil)
		if err == nil || out.Len() != 0 {
			t.Fatalf("invalid/unbounded padding accepted: %+v", opts)
		}
	}
}

func TestWriterPaddingUNAndCharacterSet(t *testing.T) {
	item := core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4, 5, 6})}}
	unknown := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1020), VR: core.VRUN}, Value: core.SequenceValue{Items: []core.DataSet{item}}}
	for _, policy := range []LengthPolicy{LengthPolicyUndefined, LengthPolicyDefined, LengthPolicyPreserve} {
		b, err := writePaddingFixture([]core.Element{unknown}, WriterOptions{LengthPolicy: policy, Padding: PaddingOptions{Policy: PaddingCreate, ItemAlignment: 32}})
		if err != nil {
			t.Fatal(err)
		}
		// UN sequence items use Implicit VR Little Endian (PS3.5 7.5), so
		// the generated OB has an 8-byte implicit header rather than 12.
		if len(b) != 68 || binary.LittleEndian.Uint32(b[24:28]) != 6 || binary.LittleEndian.Uint16(b[34:36]) != 0xfffc || binary.LittleEndian.Uint32(b[38:42]) != 10 {
			t.Fatalf("UN padding representation: %x", b)
		}
	}
	utf := core.DataSet{Elements: []core.Element{{Header: core.ElementHeader{Tag: tagSpecificCharacterSet, VR: core.VRCS}, Value: core.StringValue{"ISO_IR 192"}}, {Header: core.ElementHeader{Tag: core.NewTag(8, 0x0104), VR: core.VRLO}, Value: core.StringValue{"漢字"}}}}
	seq := []core.Element{{Header: core.ElementHeader{Tag: core.NewTag(8, 0x1111), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{utf}}}}
	for _, policy := range []LengthPolicy{LengthPolicyUndefined, LengthPolicyDefined} {
		b, err := writePaddingFixture(seq, WriterOptions{LengthPolicy: policy, GroupLength: GroupLengthCreate, Padding: PaddingOptions{Policy: PaddingCreate, ItemAlignment: 64}})
		if err != nil {
			t.Fatal(err)
		}
		for _, n := range inspectPadding(t, b) {
			if n%64 != 0 {
				t.Fatal("charset encoding not included")
			}
		}
	}
}
