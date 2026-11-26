package object

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dcmtkoracle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func lengthsTestStrings(tag core.Tag, vr core.VR, values ...string) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: core.StringValue(values)}
}

// lengthsTestDataSet has nested sequences, a private group, a stale Group
// Length and an empty sequence.
func lengthsTestDataSet() *Object {
	inner := core.DataSet{Elements: []core.Element{
		lengthsTestStrings(core.NewTag(0x0008, 0x0100), core.VRSH, "121071"),
		lengthsTestStrings(core.NewTag(0x0008, 0x0104), core.VRLO, "Finding"),
	}}
	item := core.DataSet{Elements: []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0000), VR: core.VRUL}, Value: core.Uint32Value{999}},
		lengthsTestStrings(core.NewTag(0x0008, 0x1150), core.VRUI, "1.2.840.10008.5.1.4.1.1.2"),
		lengthsTestStrings(core.NewTag(0x0008, 0x1155), core.VRUI, "1.2.3.4.5"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xA043), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{inner}}},
	}}
	return FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x0000), VR: core.VRUL}, Value: core.Uint32Value{1}},
		lengthsTestStrings(core.NewTag(0x0008, 0x0016), core.VRUI, "1.2.840.10008.5.1.4.1.1.7"),
		lengthsTestStrings(core.NewTag(0x0008, 0x0018), core.VRUI, "1.2.826.0.1.3680043.10.1.983.1"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1115), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{item, {}}}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0008, 0x1140), VR: core.VRSQ}, Value: core.SequenceValue{}},
		lengthsTestStrings(core.NewTag(0x0010, 0x0010), core.VRPN, "DOE^JANE"),
		lengthsTestStrings(core.NewTag(0x0010, 0x0020), core.VRLO, "ID1"),
		lengthsTestStrings(core.NewTag(0x0011, 0x0010), core.VRLO, "TEST CREATOR"),
		lengthsTestStrings(core.NewTag(0x0011, 0x1001), core.VRLO, "private"),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0010), VR: core.VRUS}, Value: core.Uint16Value{2}},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0028, 0x0011), VR: core.VRUS}, Value: core.Uint16Value{2}},
		{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW}, Value: core.RawValue{1, 0, 2, 0, 3, 0, 4, 0}},
	}, std.Dictionary)
}

// lengthsWalker parses an encoded data set to check Group Length values,
// descending into defined and undefined sequences and items.
type lengthsWalker struct {
	t        *testing.T
	explicit bool
	order    binary.ByteOrder
	check    bool
	found    map[uint16]bool
}

func (w *lengthsWalker) header(data []byte, pos int) (group, element uint16, vr string, length uint32, next int) {
	group, element = w.order.Uint16(data[pos:]), w.order.Uint16(data[pos+2:])
	if w.explicit && group != 0xFFFE {
		vr = string(data[pos+4 : pos+6])
		if strings.Contains("OB OW OF OD OL OV SQ UC UR UT UN SV UV", vr) {
			return group, element, vr, w.order.Uint32(data[pos+8:]), pos + 12
		}
		return group, element, vr, uint32(w.order.Uint16(data[pos+6:])), pos + 8
	}
	return group, element, vr, w.order.Uint32(data[pos+4:]), pos + 8
}

// dataset checks one data set (top level or item content).
func (w *lengthsWalker) dataset(data []byte) {
	declared := map[uint16]uint32{}
	measured := map[uint16]uint32{}
	pos := 0
	for pos+8 <= len(data) {
		start := pos
		group, element, vr, length, next := w.header(data, pos)
		end := w.value(data, next, vr, length)
		if element == 0x0000 {
			declared[group] = w.order.Uint32(data[next:])
			w.found[group] = true
		} else {
			measured[group] += uint32(end - start)
		}
		pos = end
	}
	if !w.check {
		return
	}
	for group, value := range declared {
		if value != measured[group] {
			w.t.Errorf("group %04X length %d, encoded %d", group, value, measured[group])
		}
	}
}

// value returns the end of a value starting at pos, walking sequence items.
func (w *lengthsWalker) value(data []byte, pos int, vr string, length uint32) int {
	sequence := vr == "SQ" || (!w.explicit && pos+8 <= len(data) && w.order.Uint16(data[pos:]) == 0xFFFE && w.order.Uint16(data[pos+2:]) == 0xE000)
	if length != 0xFFFFFFFF {
		if sequence {
			w.items(data[pos:pos+int(length)], false)
		}
		return pos + int(length)
	}
	if !sequence {
		// Encapsulated pixel data: fragments up to the delimiter.
		for pos+8 <= len(data) {
			element, fragment := w.order.Uint16(data[pos+2:]), w.order.Uint32(data[pos+4:])
			pos += 8
			if element == 0xE0DD {
				return pos
			}
			pos += int(fragment)
		}
		return pos
	}
	return w.items(data[pos:], true) + pos
}

// items walks the items of a sequence value and returns the bytes used
// (up to and including the sequence delimiter when delimited).
func (w *lengthsWalker) items(data []byte, delimited bool) int {
	pos := 0
	for pos+8 <= len(data) {
		element, length := w.order.Uint16(data[pos+2:]), w.order.Uint32(data[pos+4:])
		pos += 8
		if element == 0xE0DD {
			return pos
		}
		if length != 0xFFFFFFFF {
			w.dataset(data[pos : pos+int(length)])
			pos += int(length)
			continue
		}
		end := w.itemEnd(data, pos)
		w.dataset(data[pos:end])
		pos = end + 8
	}
	return pos
}

// itemEnd returns the offset of the delimiter that closes an undefined
// length item whose content starts at pos.
func (w *lengthsWalker) itemEnd(data []byte, pos int) int {
	saved := w.check
	w.check = false
	defer func() { w.check = saved }()
	for pos+8 <= len(data) {
		group, element := w.order.Uint16(data[pos:]), w.order.Uint16(data[pos+2:])
		if group == 0xFFFE && element == 0xE00D {
			return pos
		}
		_, _, vr, length, next := w.header(data, pos)
		pos = w.value(data, next, vr, length)
	}
	return pos
}

func groupLengthsAreCorrect(t *testing.T, data []byte, syntax transfer.Syntax, check bool) map[uint16]bool {
	t.Helper()
	w := &lengthsWalker{t: t, explicit: syntax.ExplicitVR, order: syntax.ByteOrder, check: check, found: map[uint16]bool{}}
	w.dataset(data)
	return w.found
}

func TestWriteLengthPoliciesRoundTrip(t *testing.T) {
	syntaxes := []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian}
	groupPolicies := map[string]GroupLengthPolicy{"preserve": GroupLengthPreserve, "recalculate": GroupLengthRecalculate, "create": GroupLengthCreate, "remove": GroupLengthRemove}
	sequencePolicies := map[string]SequenceLengthPolicy{"undefined": SequenceLengthUndefined, "preserve": SequenceLengthPreserve, "defined": SequenceLengthDefined}
	for _, syntax := range syntaxes {
		for groupName, groupPolicy := range groupPolicies {
			for sequenceName, sequencePolicy := range sequencePolicies {
				name := fmt.Sprintf("%s/%s/%s", syntax.UID, groupName, sequenceName)
				var buf bytes.Buffer
				if err := WriteDataSetWithOptions(&buf, lengthsTestDataSet(), syntax, WriteDataSetOptions{SequenceLength: sequencePolicy, GroupLength: groupPolicy}); err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				data := buf.Bytes()
				if sequencePolicy == SequenceLengthDefined && bytes.Contains(data, delimiterBytes(syntax.ByteOrder, 0xE0DD)) {
					t.Errorf("%s: sequence delimiter written with defined lengths", name)
				}
				if sequencePolicy == SequenceLengthUndefined && !bytes.Contains(data, delimiterBytes(syntax.ByteOrder, 0xE0DD)) {
					t.Errorf("%s: no sequence delimiter with undefined lengths", name)
				}
				found := groupLengthsAreCorrect(t, data, syntax, groupPolicy != GroupLengthPreserve)
				switch groupPolicy {
				case GroupLengthCreate:
					for _, group := range []uint16{0x0008, 0x0010, 0x0011, 0x0028, 0x0040, 0x7FE0} {
						if !found[group] {
							t.Errorf("%s: no Group Length for %04X", name, group)
						}
					}
				case GroupLengthRecalculate:
					if !found[0x0008] || found[0x0010] {
						t.Errorf("%s: recalculation added or lost group lengths: %v", name, found)
					}
				case GroupLengthRemove:
					if len(found) != 0 {
						t.Errorf("%s: group lengths remain: %v", name, found)
					}
				}
				read, err := ReadDataSet(bytes.NewReader(data), syntax)
				if err != nil {
					t.Fatalf("%s: read back: %v", name, err)
				}
				items, _ := read.GetSequence(core.NewTag(0x0008, 0x1115))
				if len(items) != 2 {
					t.Fatalf("%s: %d items", name, len(items))
				}
				nested, _ := items[0].GetSequence(core.NewTag(0x0040, 0xA043))
				if len(nested) != 1 {
					t.Fatalf("%s: nested items %d", name, len(nested))
				}
				if meaning, _ := nested[0].GetString(core.NewTag(0x0008, 0x0104)); meaning != "Finding" {
					t.Fatalf("%s: nested value %q", name, meaning)
				}
				if raw, _ := read.GetRaw(core.TagPixelData); !bytes.Equal(raw, []byte{1, 0, 2, 0, 3, 0, 4, 0}) && syntax.ByteOrder == binary.LittleEndian {
					t.Fatalf("%s: pixel data", name)
				}
			}
		}
	}
}

func delimiterBytes(order binary.ByteOrder, element uint16) []byte {
	b := make([]byte, 8)
	order.PutUint16(b, 0xFFFE)
	order.PutUint16(b[2:], element)
	return b
}

// TestWriteLengthPoliciesMatchDCMTK compares the encoded data set with
// DCMTK dcmconv -F for each group and sequence length option, and has
// dcmdump read every combination. Enable with DICOMGO_DCMTK_INTEGRATION=1.
func TestWriteLengthPoliciesMatchDCMTK(t *testing.T) {
	dcmconv := dcmtkoracle.Tool(t, "dcmconv")
	dcmdump := dcmtkoracle.Tool(t, "dcmdump")
	dir := t.TempDir()
	source := filepath.Join(dir, "source.dcm")
	file := &File{Dataset: lengthsTestDataSet(), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		groupFlag, lengthFlag string
		group                 GroupLengthPolicy
		sequence              SequenceLengthPolicy
	}{
		{"+g", "+e", GroupLengthCreate, SequenceLengthDefined},
		{"+g", "-e", GroupLengthCreate, SequenceLengthUndefined},
		{"-g", "+e", GroupLengthRemove, SequenceLengthDefined},
		{"-g", "-e", GroupLengthRemove, SequenceLengthUndefined},
		{"+g=", "+e", GroupLengthRecalculate, SequenceLengthDefined},
		{"+g=", "-e", GroupLengthRecalculate, SequenceLengthUndefined},
	} {
		name := c.groupFlag + c.lengthFlag
		want := filepath.Join(dir, "dcmtk"+strings.ReplaceAll(name, "=", "r")+".raw")
		dcmtkoracle.Run(t, dcmtkoracle.Exact, dcmconv, c.groupFlag, c.lengthFlag, "-F", source, want)
		expected, _ := os.ReadFile(want)
		var got bytes.Buffer
		if err := WriteDataSetWithOptions(&got, lengthsTestDataSet(), transfer.ExplicitVRLittleEndian, WriteDataSetOptions{GroupLength: c.group, SequenceLength: c.sequence}); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got.Bytes(), expected) {
			t.Errorf("%s: %d bytes, dcmconv %d bytes", name, got.Len(), len(expected))
		}
		ours := filepath.Join(dir, "ours"+strings.ReplaceAll(name, "=", "r")+".dcm")
		written := &File{Dataset: lengthsTestDataSet(), TransferSyntax: transfer.ExplicitVRLittleEndian}
		if err := written.RebuildFileMeta(); err != nil {
			t.Fatal(err)
		}
		var part10 bytes.Buffer
		if err := WriteFileWithOptions(&part10, written, WriteFileOptions{GroupLength: c.group, SequenceLength: c.sequence}); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(ours, part10.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		out, err := dcmtkoracle.Command(context.Background(), t, dcmtkoracle.Accepted, dcmdump, "-E", ours).CombinedOutput()
		if err != nil || strings.Contains(string(out), "W:") || strings.Contains(string(out), "E:") {
			t.Errorf("%s: dcmdump: %v: %s", name, err, out)
		}
	}
}

// A deferred Pixel Data value is measured by streaming it once.
func TestWriteGroupLengthWithDeferredPixelData(t *testing.T) {
	file := &File{Dataset: lengthsTestDataSet(), TransferSyntax: transfer.ExplicitVRLittleEndian}
	if err := file.RebuildFileMeta(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "deferred.dcm")
	var buf bytes.Buffer
	if err := WriteFile(&buf, file); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	deferred, err := ReadFileWithOptions(source, ReadFileOptions{DeferPixelData: true})
	if err != nil {
		t.Fatal(err)
	}
	defer deferred.Close()
	if element, _ := deferred.Dataset.Get(core.TagPixelData); element.Value != nil {
		t.Skip("pixel data was not deferred")
	}
	opts := WriteFileOptions{GroupLength: GroupLengthCreate, SequenceLength: SequenceLengthDefined}
	var fromDeferred, fromMemory bytes.Buffer
	if err := WriteFileWithOptions(&fromDeferred, deferred, opts); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileWithOptions(&fromMemory, file, opts); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(fromDeferred.Bytes(), fromMemory.Bytes()) {
		t.Fatal("deferred and in-memory writes differ")
	}
}
