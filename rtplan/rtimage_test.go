package rtplan

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation/iod"
	"testing"
)

func newImageFixture(t testing.TB, bits int, nonNormal bool) *object.File {
	t.Helper()
	source := newPlanFixture()
	e := newElems()
	for _, v := range source.attrs.list() {
		if v.Tag().Group <= 0x0020 {
			e.set(v)
		}
	}
	e.set(core.Element{Header: core.ElementHeader{Tag: ft(0x00200020), VR: core.VRCS}, Value: core.StringValue(nil)}, derivedio.UI(tagSOPClassUID, RTImageStorage), derivedio.UI(tagSOPInstanceUID, "2.25.132900006"), derivedio.CS(tagModality, "RTIMAGE"), core.Element{Header: core.ElementHeader{Tag: ft(0x00080008), VR: core.VRCS}, Value: core.StringValue{"DERIVED", "PRIMARY", "DRR"}}, derivedio.Str(ft(0x00080023), core.VRDA, "20260101"), derivedio.Str(ft(0x00080033), core.VRTM, "120000"), derivedio.CS(ft(0x00080064), "WSD"), derivedio.IS(ft(0x00200013), 1), derivedio.SH(ft(0x30020002), "SYNTH_RTIMAGE"), derivedio.LO(ft(0x30020003), "Receptor α"), derivedio.CS(ft(0x3002000c), "NORMAL"), derivedio.DS(ft(0x3002000e), 0), derivedio.DS(ft(0x30020011), .4, .5), derivedio.DS(ft(0x30020012), -1, 2), derivedio.SH(ft(0x30020020), "SYNTH_LINAC"), derivedio.CS(tagPrimaryDosimeterUnit, "MU"), derivedio.DS(ft(0x30020022), 1000), derivedio.DS(ft(0x30020026), 1500), derivedio.DS(ft(0x3002000d), 0, 0, -500))
	if nonNormal {
		e.set(derivedio.CS(ft(0x3002000c), "NON_NORMAL"), derivedio.DS(ft(0x30020010), 1, 0, 0, 0, .8, .6))
	}
	for _, x := range []struct {
		t uint32
		n int
	}{{0x00280010, 1}, {0x00280011, 3}, {0x00280002, 1}, {0x00280100, bits}, {0x00280101, bits}, {0x00280102, bits - 1}, {0x00280103, 0}} {
		e.set(derivedio.US(ft(x.t), uint16(x.n)))
	}
	e.set(derivedio.CS(ft(0x00280004), "MONOCHROME2"))
	raw := []byte{0, 127, 255, 0}
	vr := core.VROB
	if bits == 16 {
		raw = []byte{0, 0, 0x34, 0x12, 0xff, 0xff}
		vr = core.VROW
	}
	e.set(core.Element{Header: core.ElementHeader{Tag: ft(0x7fe00010), VR: vr}, Value: core.RawValue(raw)})
	f, err := derivedio.File(RTImageStorage, "2.25.132900006", derivedio.Object(e.list()...))
	if err != nil {
		t.Fatal(err)
	}
	return f
}
func TestImageFullIODNativeRoundTrips(t *testing.T) {
	for _, bits := range []int{8, 16} {
		for _, plane := range []bool{false, true} {
			for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				t.Run(string(rune('A'+bits))+ts.UID+map[bool]string{true: "N", false: "Z"}[plane], func(t *testing.T) {
					f := newImageFixture(t, bits, plane)
					rep, err := iod.Validate(f.Dataset, iod.Options{CheckValues: true})
					if err != nil || len(rep.Errors()) > 0 {
						t.Fatalf("IOD %v %+v", err, rep.Errors())
					}
					img, err := ReadImage(context.Background(), f, ReadOptions{StrictIOD: true})
					if err != nil {
						t.Fatal(err)
					}
					out, err := WriteImage(context.Background(), img, WriteOptions{Template: f, TransferSyntax: ts})
					if err != nil {
						t.Fatal(err)
					}
					var b bytes.Buffer
					if err = object.WriteFile(&b, out); err != nil {
						t.Fatal(err)
					}
					parsed, err := object.ReadFile(bytes.NewReader(b.Bytes()))
					if err != nil {
						t.Fatal(err)
					}
					again, err := ReadImage(context.Background(), parsed, ReadOptions{StrictIOD: true})
					if err != nil {
						t.Fatal(err)
					}
					if !semanticEqual(img, again) {
						t.Fatalf("model round-trip changed pixels %x/%x order %T/%T metadata %+v/%+v", img.Pixels, again.Pixels, out.Dataset.ValueByteOrder(), parsed.Dataset.ValueByteOrder(), img.Metadata, again.Metadata)
					}
					if len(again.Pixels) != 3*bits/8 || bits == 8 && again.Pixels[1] != 127 || bits == 16 && (again.Pixels[2] != 0x34 || again.Pixels[3] != 0x12) {
						t.Fatal("independent native sample oracle")
					}
					again.Pixels[0] = 42
					if img.Pixels[0] != 0 {
						t.Fatal("pixel ownership")
					}
					if p := img.Position.Value; p == nil || (*p)[0] != -1 || (*img.ImagePlanePixelSpacing.Value)[1] != .5 {
						t.Fatal("IEC receptor geometry")
					}
				})
			}
		}
	}
}
func TestImageRefusalsAndUID(t *testing.T) {
	for _, tc := range []struct {
		name  string
		edit  func(*object.Object)
		cause error
	}{
		{"plane", func(o *object.Object) { o.Put(derivedio.CS(ft(0x3002000c), "NON_NORMAL")) }, ErrInvalidObject},
		{"spacing", func(o *object.Object) { o.Put(derivedio.DS(ft(0x30020011), 0, .4)) }, ErrInvalidObject},
		{"pixelExtent", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x7fe00010), VR: core.VROB}, Value: core.RawValue{1, 2}})
		}, ErrInvalidObject},
		{"pad", func(o *object.Object) {
			o.Put(core.Element{Header: core.ElementHeader{Tag: ft(0x7fe00010), VR: core.VROB}, Value: core.RawValue{1, 2, 3, 9}})
		}, ErrInvalidObject},
		{"multiframe", func(o *object.Object) { o.Put(derivedio.IS(ft(0x00280008), 2)) }, ErrUnsupportedFeature},
		{"signed", func(o *object.Object) { o.Put(derivedio.US(ft(0x00280103), 1)) }, ErrUnsupportedFeature},
		{"exposure", func(o *object.Object) { o.Put(derivedio.Seq(ft(0x30020030), derivedio.DataSet(), derivedio.DataSet())) }, ErrUnsupportedFeature},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newImageFixture(t, 8, false)
			tc.edit(f.Dataset)
			m, err := ReadImage(context.Background(), f, ReadOptions{})
			if m != nil || !errors.Is(err, tc.cause) {
				t.Fatalf("%v %v", m, err)
			}
		})
	}
	f := newImageFixture(t, 8, false)
	img, err := ReadImage(context.Background(), f, ReadOptions{})
	if err != nil {
		t.Fatal(err)
	}
	img.Pixels[1] = 64
	out, err := WriteImage(context.Background(), img, WriteOptions{Template: f})
	if out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("changed UID refusal", err)
	}
	out, err = WriteImage(context.Background(), img, WriteOptions{Template: f, SOPInstanceUID: "2.25.132900009"})
	if err != nil || out == nil {
		t.Fatal(err)
	}
	source, err := ReadImage(context.Background(), f, ReadOptions{})
	if err != nil || source.Pixels[1] != 127 {
		t.Fatal("template mutated")
	}
	m, err := ReadImage(context.Background(), f, ReadOptions{Limits: Limits{MaxPixelBytes: 3}})
	if m != nil || !errors.Is(err, ErrResourceLimit) {
		t.Fatal("pixel admission", err)
	}
}
