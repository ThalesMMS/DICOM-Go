// Package quirkcorpus authors small, PHI-free structural analogues of named
// GDCM regression quirks. It never imports vendor DICOM files or codec source.
package quirkcorpus

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"math"
	"strconv"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const scUID = "1.2.840.10008.5.1.4.1.1.7"
const MaxFixtureBytes = 64 << 10

type Fixture struct {
	ID              string
	Phenomenon      string
	Policy          string
	Wire            []byte `json:"-"`
	Oracle          Oracle
	Strict, Recover Stages
}

type Oracle struct {
	Rows, Columns, Frames, Samples, BitsAllocated, BitsStored, HighBit int
	PixelRepresentation                                                *int
	Photometric                                                        string
	Spacing, Orientation, Position                                     []float64
	PixelHex                                                           string
	MetaSOP, DataSOP                                                   string
	OverlayGroups                                                      int
	SequenceItems                                                      int
}
type Stages struct{ Parse, Preserve, Decode, Render string }

func good() Stages      { return Stages{"ok", "ok", "ok", "ok"} }
func rejected() Stages  { return Stages{"reject", "source-only", "not-run", "not-run"} }
func undecoded() Stages { return Stages{"ok", "ok", "reject", "not-run"} }

func tag(g, e uint16) core.Tag { return core.NewTag(g, e) }
func text(g, e uint16, vr core.VR, s string) core.Element {
	return dicomtest.NewStringElement(tag(g, e), vr, s)
}
func us(g, e uint16, n uint16) core.Element   { return dicomtest.NewUShortElement(tag(g, e), n) }
func pixel(b []byte, vr core.VR) core.Element { return core.NewRawElement(core.TagPixelData, vr, b) }
func control(g, e uint16, length uint32) []byte {
	var b bytes.Buffer
	_ = binary.Write(&b, binary.LittleEndian, g)
	_ = binary.Write(&b, binary.LittleEndian, e)
	_ = binary.Write(&b, binary.LittleEndian, length)
	return b.Bytes()
}
func long(g, e uint16, vr string, n uint32) []byte {
	b := control(g, e, n)
	return append(append(append([]byte{}, b[:4]...), vr...), append([]byte{0, 0}, b[4:]...)...)
}
func encoded(s transfer.Syntax, elements ...core.Element) []byte {
	return dicomtest.EncodeElements(s, elements...)
}

func baseElements() []core.Element {
	return []core.Element{
		text(0x0008, 0x0016, core.VRUI, scUID), text(0x0008, 0x0018, core.VRUI, "2.25.141701"), text(0x0008, 0x0060, core.VRCS, "OT"), text(0x0010, 0x0010, core.VRPN, "SYNTHETIC^QUIRKS"), text(0x0010, 0x0020, core.VRLO, "OWN1417"), text(0x0020, 0x000d, core.VRUI, "2.25.141702"), text(0x0020, 0x000e, core.VRUI, "2.25.141703"), text(0x0020, 0x0032, core.VRDS, "5\\7\\9"), text(0x0020, 0x0037, core.VRDS, "1\\0\\0\\0\\1\\0"),
		us(0x0028, 0x0002, 1), text(0x0028, 0x0004, core.VRCS, "MONOCHROME2"), text(0x0028, 0x0008, core.VRIS, "1"), us(0x0028, 0x0010, 2), us(0x0028, 0x0011, 2), text(0x0028, 0x0030, core.VRDS, "1.5\\2"), us(0x0028, 0x0100, 8), us(0x0028, 0x0101, 8), us(0x0028, 0x0102, 7), us(0x0028, 0x0103, 0),
	}
}
func set(elements []core.Element, replacement core.Element) []core.Element {
	for i := range elements {
		if elements[i].Tag() == replacement.Tag() {
			elements[i] = replacement
			return elements
		}
	}
	return append(elements, replacement)
}
func remove(elements []core.Element, t core.Tag) []core.Element {
	var result []core.Element
	for _, e := range elements {
		if e.Tag() != t {
			result = append(result, e)
		}
	}
	return result
}
func part10(metaSyntax transfer.Syntax, metaSOP string, body []byte) []byte {
	return part10Meta(dicomtest.NewFileMetaBuilder().WithTransferSyntax(metaSyntax.UID).WithSOPClass(metaSOP).Build(), body)
}
func part10Meta(meta []core.Element, body []byte) []byte {
	without := remove(meta, tag(0x0002, 0))
	length := len(encoded(transfer.ExplicitVRLittleEndian, without...))
	meta = append([]core.Element{dicomtest.Uint32Element(tag(0x0002, 0), core.VRUL, nil, uint32(length))}, without...)
	preamble := make([]byte, 132)
	copy(preamble[128:], "DICM")
	return append(append(preamble, encoded(transfer.ExplicitVRLittleEndian, meta...)...), body...)
}
func baseOracle() Oracle {
	zero := 0
	return Oracle{Rows: 2, Columns: 2, Frames: 1, Samples: 1, BitsAllocated: 8, BitsStored: 8, HighBit: 7, PixelRepresentation: &zero, Photometric: "MONOCHROME2", Spacing: []float64{1.5, 2}, Orientation: []float64{1, 0, 0, 0, 1, 0}, Position: []float64{5, 7, 9}, PixelHex: "004080ff", MetaSOP: scUID, DataSOP: scUID}
}

// Fixtures models the listed byte/layout defects, rather than claiming to
// reproduce unavailable vendors' full files or large-data performance.
func Fixtures() ([]Fixture, error) {
	var out []Fixture
	add := func(id, phenomenon, policy string, body []byte, syntax transfer.Syntax, o Oracle, s, r Stages) {
		out = append(out, Fixture{id, phenomenon, policy, part10(syntax, o.MetaSOP, body), o, s, r})
	}
	base := func(extra ...core.Element) []byte {
		return encoded(transfer.ExplicitVRLittleEndian, append(append(baseElements(), extra...), pixel([]byte{0, 64, 128, 255}, core.VROB))...)
	}
	// Three distinct interrupted carriers; none is counted as a valid image.
	for _, id := range []string{"PMS-IncompletePixelData", "GEMS-IncompletePixelData", "SIEMENS-IncompletePixelData"} {
		b := encoded(transfer.ExplicitVRLittleEndian, baseElements()...)
		if id == "GEMS-IncompletePixelData" {
			b = append(b, long(0x7fe0, 0x0010, "OB", 0xffffffff)...)
			b = append(b, control(0xfffe, 0xe000, 0)...)
			b = append(b, control(0xfffe, 0xe000, 8)...)
			b = append(b, 0xff, 0xd8, 0xff, 0xe0)
		} else {
			b = append(b, long(0x7fe0, 0x0010, "OB", 4)...)
			if id == "PMS-IncompletePixelData" {
				b = append(b, 0, 64)
			} else {
				b = append(b, 0, 64, 128)
			}
		}
		o := baseOracle()
		syntax := transfer.ExplicitVRLittleEndian
		if id == "GEMS-IncompletePixelData" {
			syntax = transfer.JPEGBaseline
		}
		recovery := Stages{"partial", "partial", "not-run", "not-run"}
		if id == "GEMS-IncompletePixelData" {
			recovery = rejected()
		} // No complete fragment to retain.
		add(id, "terminal Pixel Data/fragment truncation; native prefixes are error-labelled and an incomplete first fragment is refused", "incomplete", b, syntax, o, rejected(), recovery)
	}
	// Flat neutral grey color gives exact independent pixel values after JPEG.
	img := image.NewRGBA(image.Rect(0, 0, 2, 2))
	for y := 0; y < 2; y++ {
		for x := 0; x < 2; x++ {
			img.SetRGBA(x, y, color.RGBA{64, 64, 64, 255})
		}
	}
	var jpg bytes.Buffer
	if e := jpeg.Encode(&jpg, img, &jpeg.Options{Quality: 100}); e != nil {
		return nil, e
	}
	for _, id := range []string{"SIEMENS-JPEG-CorruptFrag", "GENESIS_SIGNA-JPEG-CorruptFrag", "ACUSON-8-YBR_FULL-JPEG-TrailingInfo", "GE_JPEG-Broken"} {
		els := set(baseElements(), us(0x0028, 0x0002, 3))
		els = set(els, text(0x0028, 0x0004, core.VRCS, "YBR_FULL"))
		els = append(els, us(0x0028, 0x0006, 0))
		payload := append([]byte(nil), jpg.Bytes()...)
		phenomenon := "damaged JPEG carrier/codestream"
		st := undecoded()
		switch id {
		case "SIEMENS-JPEG-CorruptFrag":
			payload = payload[:len(payload)/2]
		case "GENESIS_SIGNA-JPEG-CorruptFrag":
			payload = append([]byte{0xff, 0xd8, 0xff, 0xdb, 0xff, 0xff}, payload[6:12]...)
		case "GE_JPEG-Broken":
			payload = []byte{0xff, 0xd8, 0xff, 0xd9}
		case "ACUSON-8-YBR_FULL-JPEG-TrailingInfo":
			payload = append(payload, []byte("OWN-TRAILER")...)
			phenomenon = "JPEG EOI followed by bounded trailing information; retained carrier, refused decode"
		}
		if len(payload)%2 != 0 {
			payload = append(payload, 0)
		}
		els = append(els, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, payload))
		o := baseOracle()
		o.Samples = 3
		o.Photometric = "YBR_FULL"
		o.PixelHex = hex.EncodeToString(bytes.Repeat([]byte{64, 64, 64}, 4))
		add(id, phenomenon, "none", encoded(transfer.ExplicitVRLittleEndian, els...), transfer.JPEGBaseline, o, st, st)
	}
	add("GDCMFakeJPEG", "native bytes in defined-length Pixel Data declared JPEG Baseline", "native-under-compressed", base(), transfer.JPEGBaseline, baseOracle(), undecoded(), good())
	// Sequence framing defects are isolated ahead of an intact image payload.
	nested := encoded(transfer.ExplicitVRLittleEndian, text(0x0020, 0x000e, core.VRUI, "2.25.141704"))
	for _, id := range []string{"Bug_Philips_ItemTag_3F3F", "BogusItemStartItemEnd", "ItemTerminatorVLnot0", "EmptyItemStarter"} {
		seq := long(0x0008, 0x1115, "SQ", 0xffffffff)
		o := baseOracle()
		o.SequenceItems = 1
		policy := "delimiter"
		s, r := rejected(), good()
		description := ""
		switch id {
		case "Bug_Philips_ItemTag_3F3F":
			seq = append(seq, control(0x3f3f, 0x3f00, 63)...)
			seq = append(seq, nested...)
			description = "invalid (3F3F,3F00) item tag and inconsistent item length; no vendor magic-number repair"
			r = rejected()
			policy = "none"
		case "BogusItemStartItemEnd":
			seq = append(seq, control(0xfffe, 0xe000, 0xffffffff)...)
			seq = append(seq, nested...)
			seq = append(seq, control(0xfffe, 0xe0dd, 0)...)
			description = "sequence delimiter used where item delimiter is required"
			policy = "sequence"
		case "ItemTerminatorVLnot0":
			seq = append(seq, control(0xfffe, 0xe000, 0xffffffff)...)
			seq = append(seq, nested...)
			seq = append(seq, control(0xfffe, 0xe00d, 16)...)
			description = "nonzero Item Delimitation length, without consuming invented value bytes"
		case "EmptyItemStarter":
			seq = append(seq, control(0xfffe, 0xe000, 0)...)
			seq = append(seq, control(0xfffe, 0xe000, uint32(len(nested)))...)
			seq = append(seq, nested...)
			o.SequenceItems = 2
			s = good()
			policy = "none"
			description = "zero-length empty item followed by a populated item"
		}
		seq = append(seq, control(0xfffe, 0xe0dd, 0)...)
		body := append(encoded(transfer.ExplicitVRLittleEndian, baseElements()...), seq...)
		body = append(body, encoded(transfer.ExplicitVRLittleEndian, pixel([]byte{0, 64, 128, 255}, core.VROB))...)
		add(id, description, policy, body, transfer.ExplicitVRLittleEndian, o, s, r)
	}
	o := baseOracle()
	o.MetaSOP = "1.2.840.10008.5.1.4.1.1.2"
	add("MismatchSOPClassUID", "File Meta CT versus dataset Secondary Capture, explicitly retained", "none", base(), transfer.ExplicitVRLittleEndian, o, good(), good())
	meta := dicomtest.NewFileMetaBuilder().WithSOPClass(scUID).Build()
	meta = set(meta, core.NewRawElement(tag(0x0002, 0x0002), core.VRUN, []byte(scUID+"\x00")))
	out = append(out, Fixture{"VRUNInMetaHeader", "Media Storage SOP Class encoded UN instead of UI", "un", part10Meta(meta, base()), baseOracle(), good(), good()})
	add("ImplicitDeclaredAsExplicit", "Implicit VR LE dataset under Explicit VR LE File Meta declaration", "syntax", encoded(transfer.ImplicitVRLittleEndian, append(baseElements(), pixel([]byte{0, 64, 128, 255}, core.VROW))...), transfer.ExplicitVRLittleEndian, baseOracle(), rejected(), good())
	els := baseElements()
	for _, v := range []core.Element{us(0x0028, 0x0100, 16), us(0x0028, 0x0101, 16), us(0x0028, 0x0102, 15)} {
		els = set(els, v)
	}
	be := []byte{0, 0, 0, 64, 0, 128, 0, 255}
	o = baseOracle()
	o.BitsAllocated = 16
	o.BitsStored = 16
	o.HighBit = 15
	o.PixelHex = "000040008000ff00"
	add("PhilipsByteSwapping", "big-endian OW samples in a declared little-endian dataset", "byte-swap", encoded(transfer.ExplicitVRLittleEndian, append(els, pixel(be, core.VROW))...), transfer.ExplicitVRLittleEndian, o, Stages{"ok", "ok", "mismatch", "not-run"}, good())
	for _, id := range []string{"PhilipsWith15Overlays", "OverlayPrivateDataInBetween"} {
		var extra []core.Element
		count := 15
		if id == "OverlayPrivateDataInBetween" {
			count = 1
		}
		for i := 0; i < count; i++ {
			g := uint16(0x6000 + 2*i)
			extra = append(extra, us(g, 0x0010, 2))
			if id == "OverlayPrivateDataInBetween" {
				extra = append(extra, text(0x6001, 0x0010, core.VRLO, "OWN_OVERLAY"), core.NewRawElement(tag(0x6001, 0x1001), core.VROB, []byte{1, 2, 3, 4}))
			}
			extra = append(extra, us(g, 0x0011, 2), text(g, 0x0040, core.VRCS, "G"), dicomtest.Uint16Element(tag(g, 0x0050), core.VRSS, nil, 1, 1), us(g, 0x0100, 1), us(g, 0x0102, 0), core.NewRawElement(tag(g, 0x3000), core.VROW, []byte{byte(1 << uint(i%4)), 0}))
		}
		o = baseOracle()
		o.OverlayGroups = count
		add(id, "independent overlay planes/private attribute interposition; base image unchanged", "none", base(extra...), transfer.ExplicitVRLittleEndian, o, good(), good())
	}
	o = baseOracle()
	o.Photometric = "YBR_FULL"
	els = set(baseElements(), text(0x0028, 0x0004, core.VRCS, "YBR_FULL"))
	add("YBRisGray", "one grayscale sample declared YBR_FULL: byte decoding succeeds but color rendering refuses it; no guessed photometric repair", "none", encoded(transfer.ExplicitVRLittleEndian, append(els, pixel([]byte{0, 64, 128, 255}, core.VROB))...), transfer.ExplicitVRLittleEndian, o, Stages{"ok", "ok", "ok", "reject"}, Stages{"ok", "ok", "ok", "reject"})
	add("ElemZeroNotGroupLength", "element zero declared LO rather than group-length UL", "none", base(text(0x0020, 0, core.VRLO, "NOTGL")), transfer.ExplicitVRLittleEndian, baseOracle(), good(), good())
	add("FLAIR-wrong-group-length", "dataset group length disagrees with following elements and is not trusted", "none", base(dicomtest.Uint32Element(tag(0x0028, 0), core.VRUL, nil, 0x12345678)), transfer.ExplicitVRLittleEndian, baseOracle(), good(), good())
	body := encoded(transfer.ExplicitVRLittleEndian, baseElements()...)
	body = append(body, 0x11, 0x00, 0x10, 0x10, 'L', 'O', 3, 0, 'O', 'D', 'D')
	body = append(body, encoded(transfer.ExplicitVRLittleEndian, pixel([]byte{0, 64, 128, 255}, core.VROB))...)
	add("RMI_Mattes_unevenLengthTags", "odd-length LO without an invented pad byte", "odd", body, transfer.ExplicitVRLittleEndian, baseOracle(), rejected(), good())
	out = append(out, Fixture{"CR-Preamble-NoMeta", "DICM preamble directly followed by dataset, no File Meta", "syntax", append(append(make([]byte, 128), []byte("DICM")...), base()...), baseOracle(), rejected(), good()})
	out[len(out)-1].Oracle.MetaSOP = ""
	o = baseOracle()
	o.PixelRepresentation = nil
	add("MissingPixelRepresentation", "absent unsigned/signed declaration; no inferred sign", "none", encoded(transfer.ExplicitVRLittleEndian, append(remove(baseElements(), tag(0x0028, 0x0103)), pixel([]byte{0, 64, 128, 255}, core.VROB))...), transfer.ExplicitVRLittleEndian, o, undecoded(), undecoded())
	for n := 1; n <= 2; n++ {
		implicit := encoded(transfer.ImplicitVRLittleEndian, text(0x0020, 0x000e, core.VRUI, "2.25.141704"))
		item := append(control(0xfffe, 0xe000, uint32(len(implicit))), implicit...)
		length := uint32(len(item))
		if n == 2 {
			length = 0xffffffff
			item = append(item, control(0xfffe, 0xe0dd, 0)...)
		}
		seq := append(long(0x0008, 0x1115, "UN", length), item...)
		body = append(encoded(transfer.ExplicitVRLittleEndian, baseElements()...), seq...)
		body = append(body, encoded(transfer.ExplicitVRLittleEndian, pixel([]byte{0, 64, 128, 255}, core.VROB))...)
		o = baseOracle()
		if n == 2 {
			o.SequenceItems = 1
		}
		add("TestVRSQUN"+strconv.Itoa(n), "known sequence encoded UN with implicit-VR item dataset", "un", body, transfer.ExplicitVRLittleEndian, o, good(), good())
	}
	els = baseElements()
	for _, v := range []core.Element{us(0x0028, 0x0100, 32), us(0x0028, 0x0101, 32), us(0x0028, 0x0102, 31)} {
		els = set(els, v)
	}
	floating := make([]byte, 16)
	for i, v := range []float32{0, .25, .5, 1} {
		binary.LittleEndian.PutUint32(floating[i*4:], math.Float32bits(v))
	}
	els = append(els, core.NewRawElement(tag(0x7fe0, 0x0008), core.VROF, floating))
	o = baseOracle()
	o.BitsAllocated = 32
	o.BitsStored = 32
	o.HighBit = 31
	o.PixelHex = hex.EncodeToString(floating)
	add("SinglePrecisionSC", "OF Float Pixel Data in Secondary Capture, retained without an integer reinterpretation", "none", encoded(transfer.ExplicitVRLittleEndian, els...), transfer.ExplicitVRLittleEndian, o, undecoded(), undecoded())
	els = baseElements()
	els = set(els, us(0x0028, 0x0010, 512))
	els = set(els, us(0x0028, 0x0011, 512))
	els = set(els, text(0x0028, 0x0008, core.VRIS, "2496"))
	els = append(els, dicomtest.NewFragmentSequenceElement(core.TagPixelData, nil, []byte{0xff, 0xd8, 0xff, 0xd9}))
	o = baseOracle()
	o.Rows = 512
	o.Columns = 512
	o.Frames = 2496
	o.PixelHex = ""
	add("US_512x512x2496_JPEG_BaseLine_Process_1", "small declared-large frame-count carrier; decode budget refuses before a codec", "none", encoded(transfer.ExplicitVRLittleEndian, els...), transfer.JPEGBaseline, o, undecoded(), undecoded())
	body = encoded(transfer.ExplicitVRLittleEndian, baseElements()...)
	body = append(body, long(0x7fe0, 0x0010, "OB", 215<<20)...)
	body = append(body, 0, 64, 128, 255)
	add("i32.XADC.7.215MegaBytes", "small declared-large Pixel Data length; byte budget refuses before allocation", "none", body, transfer.ExplicitVRLittleEndian, baseOracle(), rejected(), rejected())
	for _, f := range out {
		if len(f.Wire) > MaxFixtureBytes {
			return nil, fmt.Errorf("fixture %s exceeds bounded writer", f.ID)
		}
	}
	return out, nil
}
