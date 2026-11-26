package object_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

// unStream writes explicit VR elements in one byte order. UN payloads are
// always Implicit VR Little Endian (PS3.5 6.2.2).
type unStream struct {
	order binary.ByteOrder
	buf   bytes.Buffer
}

func (s *unStream) header(group, element uint16, vr string, length uint32) {
	_ = binary.Write(&s.buf, s.order, group)
	_ = binary.Write(&s.buf, s.order, element)
	s.buf.WriteString(vr)
	switch vr {
	case "OB", "OW", "OF", "OD", "OL", "OV", "SQ", "SV", "UC", "UN", "UR", "UT", "UV":
		s.buf.Write([]byte{0, 0})
		_ = binary.Write(&s.buf, s.order, length)
	default:
		_ = binary.Write(&s.buf, s.order, uint16(length))
	}
}

func (s *unStream) text(group, element uint16, vr, value string) {
	if len(value)%2 == 1 {
		value += " "
		if vr == "UI" {
			value = value[:len(value)-1] + "\x00"
		}
	}
	s.header(group, element, vr, uint32(len(value)))
	s.buf.WriteString(value)
}

func (s *unStream) un(group, element uint16, payload []byte) {
	s.header(group, element, "UN", uint32(len(payload)))
	s.buf.Write(payload)
}

func (s *unStream) unUndefined(group, element uint16, items ...[]byte) {
	s.header(group, element, "UN", 0xFFFFFFFF)
	// Undefined-length items, the form writers emit in this position.
	for _, item := range items {
		s.buf.Write(le(uint16(0xFFFE), uint16(0xE000), uint32(0xFFFFFFFF)))
		s.buf.Write(item)
		s.buf.Write(le(uint16(0xFFFE), uint16(0xE00D), uint32(0)))
	}
	s.buf.Write(le(uint16(0xFFFE), uint16(0xE0DD), uint32(0)))
}

func le(values ...any) []byte {
	var b bytes.Buffer
	for _, v := range values {
		_ = binary.Write(&b, binary.LittleEndian, v)
	}
	return b.Bytes()
}

// implicit encodes one Implicit VR Little Endian element.
func implicit(group, element uint16, value []byte) []byte {
	return append(le(group, element, uint32(len(value))), value...)
}

func implicitItem(elements ...[]byte) []byte {
	body := bytes.Join(elements, nil)
	return append(le(uint16(0xFFFE), uint16(0xE000), uint32(len(body))), body...)
}

const unCreator = "SYNTHETIC UN 1.0"

// unFixture is a data set whose attributes arrive as UN: text, binary,
// contextual (US or SS) and private values, a defined-length sequence, an
// undefined-length sequence and a long short-VR value.
func unFixture(order binary.ByteOrder, long bool) []byte {
	s := &unStream{order: order}
	s.text(0x0008, 0x0016, "UI", "1.2.840.10008.5.1.4.1.1.7")
	s.text(0x0008, 0x0018, "UI", "2.25.936")
	s.un(0x0008, 0x0060, []byte("CT"))
	// Referenced Series Sequence, undefined length.
	s.unUndefined(0x0008, 0x1115, bytes.Join([][]byte{
		implicit(0x0020, 0x000E, []byte("2.25.9361\x00")),
		implicit(0x0028, 0x0011, le(uint16(256))),
	}, nil))
	// Referenced Image Sequence, defined length, two items.
	s.un(0x0008, 0x1140, bytes.Join([][]byte{
		implicitItem(implicit(0x0008, 0x1150, []byte("1.2.840.10008.5.1.4.1.1.7\x00")), implicit(0x0008, 0x1160, []byte("3 "))),
		implicitItem(implicit(0x0008, 0x1150, []byte("1.2.840.10008.5.1.4.1.1.2\x00")), implicit(0x0029, 0x1001, le(uint16(9)))),
	}, nil))
	s.un(0x0018, 0x9087, le(uint32(1))) // FD Diffusion b-value in 4 bytes
	s.un(0x0020, 0x0032, []byte(`1\-2.5\3 `)[:8])
	s.un(0x0028, 0x0010, le(uint16(512)))
	s.un(0x0028, 0x0103, le(uint16(1)))
	s.un(0x0028, 0x0106, le(int16(-5)))
	s.un(0x0028, 0x1052, []byte("-1024 "))
	s.un(0x0029, 0x0010, []byte(unCreator))
	s.un(0x0029, 0x1001, le(uint16(7)))
	s.un(0x0031, 0x1001, le(uint16(8)))
	if long {
		// Graphic Data is FL, a 16-bit explicit length: longer values are
		// encoded as UN.
		floats := make([]byte, 0, 4*17000)
		for i := 0; i < 17000; i++ {
			floats = binary.LittleEndian.AppendUint32(floats, math.Float32bits(float32(i)))
		}
		s.un(0x0070, 0x0022, floats)
	}
	return s.buf.Bytes()
}

func unCatalog(t *testing.T) dictionary.DataDictionary {
	t.Helper()
	catalog, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{{Creator: unCreator, Group: 0x0029, Offset: 0x01, VR: core.VRUS, Keyword: "SyntheticCount", VM: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	return dictionary.Chain{std.Dictionary, catalog}
}

func unSyntaxes() []transfer.Syntax {
	return []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ExplicitVRBigEndian}
}

func readUNFixture(t *testing.T, syntax transfer.Syntax, long bool) (*object.Object, []byte) {
	t.Helper()
	data := unFixture(syntax.ByteOrder, long)
	obj, err := object.ReadDataSet(bytes.NewReader(data), syntax)
	if err != nil {
		t.Fatalf("%s: %v", syntax.Name, err)
	}
	return obj, data
}

func unElement(t *testing.T, obj *object.Object, tag core.Tag) core.Element {
	t.Helper()
	element, ok := obj.Get(tag)
	if !ok {
		t.Fatalf("%s missing", tag)
	}
	return element
}

func uint16In(t *testing.T, obj *object.Object, tag core.Tag) uint16 {
	t.Helper()
	raw, ok := obj.GetRaw(tag)
	if !ok || len(raw) != 2 {
		t.Fatalf("%s raw = %x", tag, raw)
	}
	return obj.ValueByteOrder().Uint16(raw)
}

func TestReadKeepsUNOpaqueAndWritesItBack(t *testing.T) {
	for _, syntax := range unSyntaxes() {
		obj, data := readUNFixture(t, syntax, true)
		for _, tag := range []core.Tag{core.NewTag(0x0008, 0x0060), core.NewTag(0x0028, 0x0010), core.NewTag(0x0008, 0x1140), core.NewTag(0x0029, 0x0010)} {
			if vr := unElement(t, obj, tag).VR(); vr != core.VRUN {
				t.Fatalf("%s: default read changed %s to %s", syntax.Name, tag, vr)
			}
		}
		// Binary UN bytes are never reversed for a Big Endian file.
		if raw, _ := obj.GetRaw(core.NewTag(0x0028, 0x0010)); !bytes.Equal(raw, le(uint16(512))) {
			t.Fatalf("%s: UN bytes %x", syntax.Name, raw)
		}
		var out bytes.Buffer
		if err := object.WriteDataSet(&out, obj, syntax); err != nil {
			t.Fatalf("%s: %v", syntax.Name, err)
		}
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("%s: preserving round trip changed the bytes", syntax.Name)
		}
	}
}

// Values inside an undefined-length UN sequence are Implicit VR Little
// Endian; in a Big Endian data set the reader converts them to the object's
// byte order, so they read the same as in a Little Endian one.
func TestUndefinedLengthUNSequenceValuesFollowObjectByteOrder(t *testing.T) {
	for _, syntax := range unSyntaxes() {
		obj, _ := readUNFixture(t, syntax, false)
		items, ok := obj.GetSequence(core.NewTag(0x0008, 0x1115))
		if !ok || len(items) != 1 {
			t.Fatalf("%s: items %d", syntax.Name, len(items))
		}
		if got := uint16In(t, items[0], core.NewTag(0x0028, 0x0011)); got != 256 {
			t.Fatalf("%s: nested Columns = %d", syntax.Name, got)
		}
	}
}

func TestRecoverUNResolvesKnownAttributes(t *testing.T) {
	for _, syntax := range unSyntaxes() {
		obj, data := readUNFixture(t, syntax, true)
		recovered, report, err := obj.RecoverUN(context.Background(), object.UNRecoveryOptions{Dictionary: unCatalog(t)})
		if err != nil {
			t.Fatalf("%s: %v", syntax.Name, err)
		}
		want := map[core.Tag]core.VR{
			core.NewTag(0x0008, 0x0060): core.VRCS,
			core.NewTag(0x0008, 0x1115): core.VRSQ,
			core.NewTag(0x0008, 0x1140): core.VRSQ,
			core.NewTag(0x0020, 0x0032): core.VRDS,
			core.NewTag(0x0028, 0x0010): core.VRUS,
			core.NewTag(0x0018, 0x9087): core.VRUN, // 4 bytes: not an FD
			core.NewTag(0x0028, 0x0103): core.VRUS,
			core.NewTag(0x0028, 0x0106): core.VRSS, // US or SS, by Pixel Representation 1
			core.NewTag(0x0028, 0x1052): core.VRDS,
			core.NewTag(0x0029, 0x0010): core.VRLO, // private creator
			core.NewTag(0x0029, 0x1001): core.VRUS, // injected catalog + creator
			core.NewTag(0x0031, 0x1001): core.VRUN, // no creator
			core.NewTag(0x0070, 0x0022): core.VRFL, // longer than a 16-bit length
		}
		for tag, vr := range want {
			if got := unElement(t, recovered, tag).VR(); got != vr {
				t.Errorf("%s: %s recovered as %s, want %s", syntax.Name, tag, got, vr)
			}
		}
		if got, _ := recovered.GetString(core.NewTag(0x0008, 0x0060)); got != "CT" {
			t.Errorf("%s: Modality %q", syntax.Name, got)
		}
		if got := uint16In(t, recovered, core.NewTag(0x0028, 0x0010)); got != 512 {
			t.Errorf("%s: Rows %d", syntax.Name, got)
		}
		if got := int16(uint16In(t, recovered, core.NewTag(0x0028, 0x0106))); got != -5 {
			t.Errorf("%s: Smallest Image Pixel Value %d", syntax.Name, got)
		}
		if got := uint16In(t, recovered, core.NewTag(0x0029, 0x1001)); got != 7 {
			t.Errorf("%s: private value %d", syntax.Name, got)
		}
		if values, err := recovered.GetFloats(core.NewTag(0x0020, 0x0032)); err != nil || len(values) != 3 || values[1] != -2.5 {
			t.Errorf("%s: Image Position %v %v", syntax.Name, values, err)
		}
		raw, _ := recovered.GetRaw(core.NewTag(0x0070, 0x0022))
		if len(raw) != 4*17000 || math.Float32frombits(recovered.ValueByteOrder().Uint32(raw[4*16999:])) != 16999 {
			t.Errorf("%s: Graphic Data", syntax.Name)
		}
		// Sequences: items parsed as Implicit VR Little Endian; values in the
		// object's byte order; items do not inherit the top-level creator.
		images, ok := recovered.GetSequence(core.NewTag(0x0008, 0x1140))
		if !ok || len(images) != 2 {
			t.Fatalf("%s: Referenced Image Sequence %d items", syntax.Name, len(images))
		}
		if uid, _ := images[1].GetString(core.NewTag(0x0008, 0x1150)); uid != "1.2.840.10008.5.1.4.1.1.2" {
			t.Errorf("%s: nested UID %q", syntax.Name, uid)
		}
		if frame, err := images[0].GetInt(core.NewTag(0x0008, 0x1160)); err != nil || frame != 3 {
			t.Errorf("%s: nested IS %d %v", syntax.Name, frame, err)
		}
		if vr := unElement(t, images[1], core.NewTag(0x0029, 0x1001)).VR(); vr != core.VRUN {
			t.Errorf("%s: item inherited the top-level creator (%s)", syntax.Name, vr)
		}
		series, _ := recovered.GetSequence(core.NewTag(0x0008, 0x1115))
		if got := uint16In(t, series[0], core.NewTag(0x0028, 0x0011)); got != 256 {
			t.Errorf("%s: undefined-length item Columns %d", syntax.Name, got)
		}
		// The source object is unchanged and still writes the received bytes.
		if vr := unElement(t, obj, core.NewTag(0x0028, 0x0010)).VR(); vr != core.VRUN {
			t.Fatalf("%s: RecoverUN modified its receiver", syntax.Name)
		}
		var out bytes.Buffer
		if err := object.WriteDataSet(&out, obj, syntax); err != nil || !bytes.Equal(out.Bytes(), data) {
			t.Fatalf("%s: source round trip after recovery: %v", syntax.Name, err)
		}
		// Diagnostics: structural path and reason, never a value.
		reasons := map[string]object.UNReason{}
		for _, d := range report.Diagnostics {
			reasons[d.Path] = d.Reason
			if strings.Contains(d.Path, "CT") || strings.Contains(d.Path, unCreator) || strings.Contains(string(d.Reason), "512") {
				t.Fatalf("diagnostic exposes a value: %+v", d)
			}
		}
		for path, reason := range map[string]object.UNReason{
			"(0018,9087)":                object.UNLengthMismatch,
			"(0029,0010)":                object.UNRecoveredPrivateCreator,
			"(0031,1001)":                object.UNPrivateNoCreator,
			"(0070,0022)":                object.UNRecoveredLongShortVR,
			"(0008,1115)":                object.UNRecoveredUndefinedSequence,
			"(0008,1140)":                object.UNRecoveredSequence,
			"(0008,1140)[1].(0029,1001)": object.UNPrivateNoCreator,
			"(0028,0106)":                object.UNRecovered,
		} {
			if reasons[path] != reason {
				t.Errorf("%s: %s reason %q, want %q", syntax.Name, path, reasons[path], reason)
			}
		}
		if report.Recovered != 11 || report.Kept != 3 || report.DiagnosticsTruncated {
			t.Errorf("%s: report %d recovered %d kept", syntax.Name, report.Recovered, report.Kept)
		}
	}
}

// The recovered interpretation is written by an explicit output step and
// reads back the same; a recovered short-VR value longer than a 16-bit length
// is refused by explicit VR writers instead of being truncated.
func TestRecoveredObjectWritesAsResolvedVRs(t *testing.T) {
	for _, syntax := range unSyntaxes() {
		obj, _ := readUNFixture(t, syntax, true)
		recovered, _, err := obj.RecoverUN(context.Background(), object.UNRecoveryOptions{Dictionary: unCatalog(t)})
		if err != nil {
			t.Fatal(err)
		}
		if err := object.WriteDataSet(&bytes.Buffer{}, recovered, syntax); err == nil || !strings.Contains(err.Error(), "exceeds uint16") {
			t.Fatalf("%s: long FL written: %v", syntax.Name, err)
		}
		recovered.Remove(core.NewTag(0x0070, 0x0022))
		var out bytes.Buffer
		if err := object.WriteDataSet(&out, recovered, syntax); err != nil {
			t.Fatalf("%s: %v", syntax.Name, err)
		}
		reread, err := object.ReadDataSet(bytes.NewReader(out.Bytes()), syntax)
		if err != nil {
			t.Fatal(err)
		}
		if vr := unElement(t, reread, core.NewTag(0x0008, 0x1140)).VR(); vr != core.VRSQ {
			t.Fatalf("%s: rewritten sequence VR %s", syntax.Name, vr)
		}
		if got := uint16In(t, reread, core.NewTag(0x0028, 0x0010)); got != 512 {
			t.Fatalf("%s: rewritten Rows %d", syntax.Name, got)
		}
		images, _ := reread.GetSequence(core.NewTag(0x0008, 0x1140))
		if uid, _ := images[0].GetString(core.NewTag(0x0008, 0x1150)); uid != "1.2.840.10008.5.1.4.1.1.7" {
			t.Fatalf("%s: rewritten nested UID %q", syntax.Name, uid)
		}
		if syntax.ByteOrder != binary.LittleEndian {
			continue // changing byte order is TranscodeDataSet's job, not a plain write
		}
		if implicitLE, err := object.ReadDataSet(bytes.NewReader(mustWrite(t, recovered, transfer.ImplicitVRLittleEndian)), transfer.ImplicitVRLittleEndian); err != nil || uint16In(t, implicitLE, core.NewTag(0x0028, 0x0010)) != 512 {
			t.Fatalf("%s: implicit rewrite: %v", syntax.Name, err)
		}
	}
}

func mustWrite(t *testing.T, obj *object.Object, syntax transfer.Syntax) []byte {
	t.Helper()
	var out bytes.Buffer
	if err := object.WriteDataSet(&out, obj, syntax); err != nil {
		t.Fatal(err)
	}
	return out.Bytes()
}

func unObject(t *testing.T, syntax transfer.Syntax, build func(*unStream)) *object.Object {
	t.Helper()
	s := &unStream{order: syntax.ByteOrder}
	build(s)
	obj, err := object.ReadDataSet(bytes.NewReader(s.buf.Bytes()), syntax)
	if err != nil {
		t.Fatal(err)
	}
	return obj
}

func TestRecoverUNUnsafeCasesStayUNOrFailUnderStrict(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	for name, tc := range map[string]struct {
		build  func(*unStream)
		tag    core.Tag
		reason object.UNReason
		strict bool // Strict must fail
	}{
		"truncated sequence payload": {func(s *unStream) {
			s.un(0x0008, 0x1140, append(le(uint16(0xFFFE), uint16(0xE000), uint32(64)), implicit(0x0008, 0x1150, []byte("1.2\x00"))...))
		}, core.NewTag(0x0008, 0x1140), object.UNInvalidSequence, true},
		"sequence payload that is not items": {func(s *unStream) {
			s.un(0x0008, 0x1140, []byte("NOTITEMS"))
		}, core.NewTag(0x0008, 0x1140), object.UNInvalidSequence, true},
		"US or SS without Pixel Representation": {func(s *unStream) {
			s.un(0x0028, 0x0106, le(uint16(5)))
		}, core.NewTag(0x0028, 0x0106), object.UNContextMissing, true},
		"undefined-length UN for a scalar": {func(s *unStream) {
			s.unUndefined(0x0028, 0x0010, implicit(0x0008, 0x0060, []byte("CT")))
		}, core.NewTag(0x0028, 0x0010), object.UNSequenceForScalar, true},
		"unknown public attribute": {func(s *unStream) {
			s.un(0x0008, 0xFFF0, []byte("ab"))
		}, core.NewTag(0x0008, 0xFFF0), object.UNUnknownAttribute, false},
		"private attribute without catalog": {func(s *unStream) {
			s.un(0x0029, 0x0010, []byte(unCreator))
			s.un(0x0029, 0x1002, le(uint16(1)))
		}, core.NewTag(0x0029, 0x1002), object.UNPrivateNoCatalog, false},
		"conflicting private creators": {func(s *unStream) {
			s.un(0x0029, 0x0010, []byte(unCreator))
			s.un(0x0029, 0x0011, []byte(unCreator))
			s.un(0x0029, 0x1001, le(uint16(1)))
		}, core.NewTag(0x0029, 0x1001), object.UNPrivateNoCreator, true},
		"file meta group in a data set": {func(s *unStream) {
			s.un(0x0002, 0x0010, []byte("1.2.840.10008.1.2\x00"))
		}, core.NewTag(0x0002, 0x0010), object.UNFileMetaGroup, false},
	} {
		obj := unObject(t, syntax, tc.build)
		recovered, report, err := obj.RecoverUN(context.Background(), object.UNRecoveryOptions{Dictionary: unCatalog(t)})
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if vr := unElement(t, recovered, tc.tag).VR(); vr != core.VRUN {
			t.Fatalf("%s: recovered as %s", name, vr)
		}
		found := false
		for _, d := range report.Diagnostics {
			found = found || d.Tag == tc.tag && d.Reason == tc.reason && !d.Recovered
		}
		if !found {
			t.Fatalf("%s: diagnostics %+v", name, report.Diagnostics)
		}
		_, _, err = obj.RecoverUN(context.Background(), object.UNRecoveryOptions{Dictionary: unCatalog(t), Strict: true})
		var recoveryErr *object.UNRecoveryError
		if tc.strict != errors.As(err, &recoveryErr) || tc.strict && (!errors.Is(err, object.ErrUNRecovery) || recoveryErr.Reason != tc.reason) {
			t.Fatalf("%s: strict error %v", name, err)
		}
	}
}

func TestRecoverUNLimitsAndCancellation(t *testing.T) {
	syntax := transfer.ExplicitVRLittleEndian
	build := func(s *unStream) {
		s.un(0x0008, 0x1140, bytes.Join([][]byte{
			implicitItem(implicit(0x0008, 0x1150, []byte("1.2\x00")), implicit(0x0008, 0x1155, []byte("1.3\x00"))),
			implicitItem(implicit(0x0008, 0x1150, []byte("1.4\x00"))),
		}, nil))
	}
	obj := unObject(t, syntax, build)
	for name, opts := range map[string]object.UNRecoveryOptions{
		"elements":      {MaxElements: 2},
		"element bytes": {MaxElementBytes: 2},
		"depth":         {MaxSequenceDepth: 1},
	} {
		recovered, report, err := obj.RecoverUN(context.Background(), opts)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if vr := unElement(t, recovered, core.NewTag(0x0008, 0x1140)).VR(); vr != core.VRUN || len(report.Diagnostics) != 1 || report.Diagnostics[0].Reason != object.UNLimitExceeded {
			t.Fatalf("%s: %s %+v", name, vr, report.Diagnostics)
		}
		opts.Strict = true
		if _, _, err := obj.RecoverUN(context.Background(), opts); !errors.Is(err, object.ErrUNRecovery) {
			t.Fatalf("%s strict: %v", name, err)
		}
	}
	if _, report, err := obj.RecoverUN(context.Background(), object.UNRecoveryOptions{MaxDiagnostics: 1}); err != nil || len(report.Diagnostics) != 1 || report.DiagnosticsTruncated {
		t.Fatalf("diagnostic cap: %+v %v", report, err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := obj.RecoverUN(canceled, object.UNRecoveryOptions{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled: %v", err)
	}
	if _, _, err := obj.RecoverUN(context.Background(), object.UNRecoveryOptions{MaxElements: -1}); err == nil {
		t.Fatal("negative limit accepted")
	}
}

// Deferred values keep their source: a large UN value left unmaterialized is
// reported and still copies from the original file through the result.
func TestRecoverUNKeepsDeferredValues(t *testing.T) {
	s := &unStream{order: binary.LittleEndian}
	s.text(0x0008, 0x0016, "UI", "1.2.840.10008.5.1.4.1.1.7")
	s.text(0x0008, 0x0018, "UI", "2.25.9362")
	s.un(0x0028, 0x0010, le(uint16(512)))
	s.un(0x0070, 0x0022, bytes.Repeat([]byte{1, 2, 3, 4}, 1024))
	data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	file, err := object.ReadFileWithOptions(bytes.NewReader(append(data, s.buf.Bytes()...)), object.ReadFileOptions{InlineValueBytesThreshold: 1024})
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	recovered, report, err := file.Dataset.RecoverUN(context.Background(), object.UNRecoveryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if vr := unElement(t, recovered, core.NewTag(0x0028, 0x0010)).VR(); vr != core.VRUS {
		t.Fatalf("Rows %s", vr)
	}
	deferredKept := false
	for _, d := range report.Diagnostics {
		deferredKept = deferredKept || d.Tag == core.NewTag(0x0070, 0x0022) && d.Reason == object.UNDeferredValue
	}
	if !deferredKept {
		t.Fatalf("diagnostics %+v", report.Diagnostics)
	}
	var copied bytes.Buffer
	if n, err := recovered.CopyValueTo(core.NewTag(0x0070, 0x0022), &copied); err != nil || n != 4096 {
		t.Fatalf("deferred copy: %d %v", n, err)
	}
}
