package privatecatalog_test

import (
	"bytes"
	"encoding/binary"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/curated"
	"github.com/ThalesMMS/dicom-go/dictionary/privatecatalog"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/parser"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// Wire fixtures specify tags, lengths and payload bytes independently of the
// imported dictionary and any upstream parser/encoder implementation.
func header(tag core.Tag, length uint32) []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b, tag.Group)
	binary.LittleEndian.PutUint16(b[2:], tag.Element)
	binary.LittleEndian.PutUint32(b[4:], length)
	return b
}
func element(tag core.Tag, value []byte) []byte {
	return append(header(tag, uint32(len(value))), value...)
}
func creator(group, block uint16, id string) []byte {
	if len(id)%2 != 0 {
		id += " "
	}
	return element(core.NewTag(group, block), []byte(id))
}
func parse(t testing.TB, wire []byte, syntax transfer.Syntax, dict dictionary.DataDictionary) core.DataSet {
	t.Helper()
	r := parser.NewReader(bytes.NewReader(wire), syntax, parser.ReaderOptions{Dictionary: dict})
	ds, err := r.ReadDataSet()
	if err != nil {
		t.Fatal(err)
	}
	return ds
}
func get(t testing.TB, ds core.DataSet, tag core.Tag) core.Element {
	t.Helper()
	for _, e := range ds.Elements {
		if e.Tag() == tag {
			return e
		}
	}
	t.Fatalf("missing %v", tag)
	return core.Element{}
}
func catalog(t testing.TB) *privatecatalog.Catalog {
	t.Helper()
	c, err := privatecatalog.New(privatecatalog.Options{})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestImplicitVRNewManufacturersRequireExplicitOptIn(t *testing.T) {
	c := catalog(t)
	old, err := curated.New()
	if err != nil || len(old.Entries()) != 294 {
		t.Fatal("curated scope changed")
	}
	cases := []struct {
		creator string
		group   uint16
		offset  uint8
		vr      core.VR
		bytes   []byte
	}{
		{"GEMS_ACQU_01", 9, 0x25, core.VRUS, []byte{7, 0}},
		{"SIEMENS CM VA0  CMS", 0x13, 0x22, core.VRLO, []byte("SYNTHETIC ")},
		{"Philips Imaging DD 001", 0x2001, 0x32, core.VRFL, []byte{0, 0, 192, 63}},
		{"TOSHIBA_MEC_1.0", 9, 2, core.VRUS, []byte{11, 0}},
	}
	for _, tc := range cases {
		t.Run(tc.creator, func(t *testing.T) {
			if _, ok := old.ByPrivate(tc.creator, tc.group, tc.offset); ok {
				t.Fatal("fixture already in curated subset")
			}
			for _, block := range []uint16{0x10, 0x42, 0xff} {
				tag := core.NewTag(tc.group, block<<8|uint16(tc.offset))
				wire := append(creator(tc.group, block, tc.creator), element(tag, tc.bytes)...)
				e := get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, dictionary.Chain{c, std.Dictionary}), tag)
				value, raw := e.RawBytes()
				if e.VR() != tc.vr || !raw || !bytes.Equal(value, tc.bytes) {
					t.Fatalf("opt-in %v: VR %s bytes %x", tag, e.VR(), value)
				}
				e = get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, std.Dictionary), tag)
				if e.VR() != core.VRUN {
					t.Fatal("default dictionary gained private VR")
				}
			}
		})
	}
	if _, ok := c.ByTag(core.NewTag(9, 0x1025)); ok {
		t.Fatal("creator guessed by absolute tag")
	}
	if _, ok := c.ByKeyword("PrivateData"); ok {
		t.Fatal("private keyword registered globally")
	}
}

func TestImplicitVRReservationsOverlayAndUnknownFallback(t *testing.T) {
	c := catalog(t)
	tag := core.NewTag(9, 0x4225)
	dict := dictionary.Chain{c, std.Dictionary}
	for _, wire := range [][]byte{
		element(tag, []byte{7, 0}),
		append(creator(9, 0x42, "gems_acqu_01"), element(tag, []byte{7, 0})...),
		append(creator(9, 0x42, "UNKNOWN"), element(tag, []byte{7, 0})...),
		append(append(creator(9, 0x42, "GEMS_ACQU_01"), creator(9, 0x43, "GEMS_ACQU_01")...), element(tag, []byte{7, 0})...),
	} {
		if get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, dict), tag).VR() != core.VRUN {
			t.Fatal("missing/invalid reservation guessed")
		}
	}
	unknown := core.NewTag(9, 0x42ff)
	if get(t, parse(t, append(creator(9, 0x42, "GEMS_ACQU_01"), element(unknown, []byte{7, 0})...), transfer.ImplicitVRLittleEndian, dict), unknown).VR() != core.VRUN {
		t.Fatal("unknown offset guessed")
	}
	local, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{{Creator: "GEMS_ACQU_01", Group: 9, Offset: 0x25, VR: core.VRLO, VM: "1", Name: "Local reviewed value"}})
	if err != nil {
		t.Fatal(err)
	}
	wire := append(creator(9, 0x42, "GEMS_ACQU_01"), element(tag, []byte("VALUE "))...)
	if get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, dictionary.Chain{local, c, std.Dictionary}), tag).VR() != core.VRLO {
		t.Fatal("local overlay lost precedence")
	}
}

func TestPrivateSequenceDefinedAndUndefinedLengthsKeepItemScopes(t *testing.T) {
	c := catalog(t)
	seqTag := core.NewTag(0x2001, 0x425f) // Philips StackSequence, outside curated
	childTag := core.NewTag(0x13, 0x8022)
	for _, undefined := range []bool{false, true} {
		item1 := append(creator(0x13, 0x80, "SIEMENS CM VA0  CMS"), element(childTag, []byte("CHILD "))...)
		item2 := element(childTag, []byte("OPAQUE"))
		body := append(element(core.NewTag(0xfffe, 0xe000), item1), element(core.NewTag(0xfffe, 0xe000), item2)...)
		length := uint32(len(body))
		if undefined {
			length = uint32(core.UndefinedLength)
			body = append(body, header(core.NewTag(0xfffe, 0xe0dd), 0)...)
		}
		wire := append(creator(0x13, 0x80, "SIEMENS CM VA0  CMS"), creator(0x2001, 0x42, "Philips Imaging DD 001")...)
		wire = append(wire, append(header(seqTag, length), body...)...)
		e := get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, dictionary.Chain{c, std.Dictionary}), seqTag)
		seq, ok := e.Value.(core.SequenceValue)
		if e.VR() != core.VRSQ || !ok || len(seq.Items) != 2 {
			t.Fatal("private SQ lost")
		}
		if get(t, seq.Items[0], childTag).VR() != core.VRLO || get(t, seq.Items[1], childTag).VR() != core.VRUN {
			t.Fatal("reservation inherited/crossed item scope")
		}
		if !undefined && get(t, parse(t, wire, transfer.ImplicitVRLittleEndian, std.Dictionary), seqTag).VR() != core.VRUN {
			t.Fatal("default private SQ activation")
		}
	}
}

func TestExplicitVRAndOpaquePayloadRemainAuthoritative(t *testing.T) {
	c := catalog(t)
	tag := core.NewTag(9, 0x4225) // catalog US, explicitly encoded LO here
	var wire bytes.Buffer
	for _, pair := range []struct {
		tag   core.Tag
		value string
	}{{core.NewTag(9, 0x42), "GEMS_ACQU_01"}, {tag, "EXPLICIT"}} {
		value := pair.value
		if len(value)%2 != 0 {
			value += " "
		}
		binary.Write(&wire, binary.LittleEndian, pair.tag.Group)
		binary.Write(&wire, binary.LittleEndian, pair.tag.Element)
		wire.WriteString("LO")
		binary.Write(&wire, binary.LittleEndian, uint16(len(value)))
		wire.WriteString(value)
	}
	e := get(t, parse(t, wire.Bytes(), transfer.ExplicitVRLittleEndian, dictionary.Chain{c, std.Dictionary}), tag)
	if e.VR() != core.VRLO || e.StringValue() != "EXPLICIT" {
		t.Fatal("dictionary rewrote explicit VR")
	}
	opaqueTag := core.NewTag(0x21, 0x8011)
	bytesIn := []byte{1, 2, 3, 4}
	data := append(creator(0x21, 0x80, "SIEMENS MED"), element(opaqueTag, bytesIn)...)
	e = get(t, parse(t, data, transfer.ImplicitVRLittleEndian, dictionary.Chain{c, std.Dictionary}), opaqueTag)
	raw, ok := e.RawBytes()
	if e.VR() != core.VRUN || !ok || !bytes.Equal(raw, bytesIn) {
		t.Fatal("known conflict payload changed")
	}
}
