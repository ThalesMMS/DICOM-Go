package rtplan

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/internal/derivedio"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestWriteAdmissionRunsBeforeTemplateAndCountsExactSpots(t *testing.T) {
	p := familyReparse(t, newIonFixture())
	l, _ := (Limits{MaxSpots: 4}).Normalized()
	if _, err := modelAdmission(context.Background(), reflect.ValueOf(p), l); err != nil {
		t.Fatal("four distinct map entries should fit", err)
	}
	p.IonBeams[0].ControlPoints[1].SpotPositions = append(p.IonBeams[0].ControlPoints[1].SpotPositions, 1, 2)
	p.IonBeams[0].ControlPoints[1].SpotWeights = append(p.IonBeams[0].ControlPoints[1].SpotWeights, 0)
	// A malformed template must not be inspected if cumulative model admission
	// fails first. Neither duplicate maps nor weights count as extra spots.
	bad := &object.File{Dataset: object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: ft(0x77771010), VR: core.VROB, Length: 1, LengthSet: true}, Value: forbiddenValue{}}}, std.Dictionary)}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: bad, Limits: Limits{MaxSpots: 4}}); out != nil || !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("%v %v", out, err)
	}
	p = familyReparse(t, newIonFixture())
	v := 1.0
	p.IonBeams[0].ControlPoints[0].MetersetRate = Attribute[float64]{Value: &v}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: bad}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("absent attribute: %v %v", out, err)
	}
}

func TestFileAdmissionSharesMetaDatasetBudgetsAndRejectsForeignMeta(t *testing.T) {
	f := newIonFixture().file(t)
	a, err := preflight(context.Background(), f.Meta, Limits{}, false)
	if err != nil {
		t.Fatal(err)
	}
	b, err := preflight(context.Background(), f.Dataset, Limits{}, false)
	if err != nil {
		t.Fatal(err)
	}
	limit := a.bytes + b.bytes
	if p, err := ReadWithOptions(context.Background(), f, ReadOptions{Limits: Limits{MaxMetadataBytes: limit}}); err != nil || p == nil {
		t.Fatal(err)
	}
	if p, err := ReadWithOptions(context.Background(), f, ReadOptions{Limits: Limits{MaxMetadataBytes: limit - 1}}); p != nil || !errors.Is(err, ErrResourceLimit) {
		t.Fatalf("joint metadata budget: %v %v", p, err)
	}
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	foreign := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW, Length: 1 << 30, LengthSet: true}, Value: forbiddenValue{}}
	f.Meta.Put(foreign)
	if got, err := Read(f); got != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("foreign meta: %v %v", got, err)
	}
	if out, err := WritePlan(context.Background(), p, WriteOptions{Template: f}); out != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatalf("foreign meta writer: %v %v", out, err)
	}
}

func TestPlanWriterNeverReadsOrRepublishesIgnoredBulk(t *testing.T) {
	f := newBrachyFixture().file(t)
	bulk := core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROW, Length: 1 << 30, LengthSet: true}, Value: forbiddenValue{}}
	f.Dataset.Put(bulk)
	f.Dataset.Put(sequenceForAdmission(ft(0x77771020), core.DataSet{Elements: []core.Element{bulk, derivedio.LO(ft(0x77771010), "SYNTHETIC_OPAQUE")}}))
	p, err := Read(f)
	if err != nil {
		t.Fatal(err)
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: f})
	if err != nil {
		t.Fatal(err)
	}
	if out.Dataset.Has(core.TagPixelData) {
		t.Fatal("root ignored bulk republished")
	}
	items, _ := out.Dataset.GetSequence(ft(0x77771020))
	if len(items) != 1 || items[0].Has(core.TagPixelData) {
		t.Fatal("nested ignored bulk republished")
	}
	if s, _ := items[0].GetString(ft(0x77771010)); s != "SYNTHETIC_OPAQUE" {
		t.Fatal("opaque metadata lost")
	}
	if !f.Dataset.Has(core.TagPixelData) {
		t.Fatal("source changed")
	}
}

func TestIonEmptyFirstStateAndInheritedOwnership(t *testing.T) {
	f := newIonFixture().file(t)
	b, _ := f.Dataset.GetSequence(ft(0x300a03a2))
	c, _ := b[0].GetSequence(ft(0x300a03a8))
	c[0].Put(derivedio.Str(tagIsocenterPosition, core.VRDS, ""))
	for _, tag := range []core.Tag{tagTableTopPitchAngle, tagTableTopRollAngle, ft(0x300a014a), ft(0x300a030d)} {
		c[0].Put(familyNumeric(tag))
	}
	putSequence(b[0], ft(0x300a03a8), c)
	putSequence(f.Dataset, ft(0x300a03a2), b)
	f = rsReparse(t, f)
	p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
	if err != nil {
		t.Fatal(err)
	}
	out, err := WritePlan(context.Background(), p, WriteOptions{Template: f})
	if err != nil {
		t.Fatal(err)
	}
	ob, _ := out.Dataset.GetSequence(ft(0x300a03a2))
	oc, _ := ob[0].GetSequence(ft(0x300a03a8))
	for _, tag := range []core.Tag{tagIsocenterPosition, tagTableTopPitchAngle, tagTableTopRollAngle, ft(0x300a014a), ft(0x300a030d)} {
		if !oc[0].Has(tag) {
			t.Fatalf("empty first CP field lost %s", tag)
		}
	}
	p = familyReparse(t, newIonFixture())
	first := &p.IonBeams[0].ControlPoints[0]
	second := &p.IonBeams[0].ControlPoints[1]
	*second.NominalBeamEnergy = 200
	second.IsocenterPosition[0] = 999
	second.DevicePositions[0].Positions[0] = 999
	*second.SnoutPosition.Value = 999
	*second.RangeShifterSettings[0].Distance.Value = 999
	if *first.NominalBeamEnergy != 150 || first.IsocenterPosition[0] == 999 || first.DevicePositions[0].Positions[0] == 999 || *first.SnoutPosition.Value != 300 || *first.RangeShifterSettings[0].Distance.Value != 250 {
		t.Fatal("inherited model fields alias another CP")
	}
}

func TestPlanRealBigEndianUnicodeAndAtomicUID(t *testing.T) {
	for _, brachy := range []bool{false, true} {
		fixture := newIonFixture()
		if brachy {
			fixture = newBrachyFixture()
		}
		// ISO_IR 100 is independent of the source's default UTF-8 fixture.
		fixture.attrs.set(derivedio.CS(ft(0x00080005), "ISO_IR 100"), derivedio.LO(tagRTPlanName, "SYNTH_Café"))
		f := fixture.file(t)
		f.Dataset = object.FromElements(rsBEElements(f.Dataset.Elements()), std.Dictionary)
		f.Dataset.SetValueByteOrder(binary.BigEndian)
		f.TransferSyntax = transfer.ExplicitVRBigEndian
		f = rsReparse(t, f)
		p, err := ReadWithOptions(context.Background(), f, ReadOptions{StrictIOD: true})
		if err != nil {
			t.Fatal(err)
		}
		if p.Name != "SYNTH_Café" {
			t.Fatalf("source charset: %q", p.Name)
		}
		var source bytes.Buffer
		if err := object.WriteFile(&source, f); err != nil {
			t.Fatal(err)
		}
		for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, TransferSyntax: ts})
			if err != nil {
				t.Fatal(err)
			}
			q, err := ReadWithOptions(context.Background(), rsReparse(t, out), ReadOptions{StrictIOD: true})
			if err != nil || !semanticEqual(p, q) {
				t.Fatal("BE source/charset round-trip", err)
			}
			out.Dataset.Put(derivedio.LO(tagRTPlanName, "CHANGED_OUTPUT"))
			var after bytes.Buffer
			if err := object.WriteFile(&after, f); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(source.Bytes(), after.Bytes()) {
				t.Fatal("output mutated source")
			}
		}
		p.Name = "SEMANTIC_EDIT"
		for _, uid := range []string{"", p.StudyInstanceUID, p.SeriesInstanceUID, p.FrameOfReferenceUID, "invalid"} {
			if out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, SOPInstanceUID: uid}); out != nil || !errors.Is(err, ErrInvalidObject) {
				t.Fatalf("UID guard %q %v", uid, err)
			}
		}
		out, err := WritePlan(context.Background(), p, WriteOptions{Template: f, SOPInstanceUID: "2.25.132900999"})
		if err != nil {
			t.Fatal(err)
		}
		m, _ := out.Meta.GetUID(ft(0x00020003))
		d, _ := out.Dataset.GetUID(tagSOPInstanceUID)
		if m != d || d != "2.25.132900999" {
			t.Fatal("edited output identities differ")
		}
	}
}

func TestImageRealBigEndianSourceAndOW8WordOrder(t *testing.T) {
	for _, bits := range []int{8, 16} {
		f := rsReparse(t, newImageFixture(t, bits, true))
		// OW 8-bit is legal in explicit VR. Its word byte order still matters.
		pe, _ := f.Dataset.Get(core.TagPixelData)
		pe.Header.VR = core.VROW
		raw := append(core.RawValue(nil), pe.Value.(core.RawValue)...)
		for i := 0; i+1 < len(raw); i += 2 {
			raw[i], raw[i+1] = raw[i+1], raw[i]
		}
		pe.Value = raw
		f.Dataset = object.FromElements(rsBEElements(f.Dataset.Elements()), std.Dictionary)
		f.Dataset.Put(pe)
		f.Dataset.SetValueByteOrder(binary.BigEndian)
		f.TransferSyntax = transfer.ExplicitVRBigEndian
		f = rsReparse(t, f)
		img, err := ReadImage(context.Background(), f, ReadOptions{StrictIOD: true})
		if err != nil {
			t.Fatal(err)
		}
		want := []byte{0, 127, 255}
		if bits == 16 {
			want = []byte{0, 0, 0x34, 0x12, 0xff, 0xff}
		}
		if !bytes.Equal(img.Pixels, want) {
			t.Fatalf("BE pixel sample oracle %d: %x", bits, img.Pixels)
		}
		for _, ts := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
			out, err := WriteImage(context.Background(), img, WriteOptions{Template: f, TransferSyntax: ts})
			if err != nil {
				t.Fatal(err)
			}
			got, err := ReadImage(context.Background(), rsReparse(t, out), ReadOptions{StrictIOD: true})
			if err != nil || !bytes.Equal(got.Pixels, want) {
				t.Fatal("BE image round-trip", err)
			}
		}
	}
}

func TestFileSyntaxByteOrderMismatchAndRequiredEquipment(t *testing.T) {
	f := newIonFixture().file(t)
	f.TransferSyntax = transfer.ExplicitVRBigEndian
	f.Meta.Put(derivedio.UI(ft(0x00020010), transfer.ExplicitVRBigEndian.UID))
	if p, err := Read(f); p != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("source byte order mismatch", err)
	}
	f = newBrachyFixture().file(t)
	f.Dataset.Remove(ft(0x300a0206))
	if p, err := Read(f); p != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("brachy missing machine", err)
	}
	record := rsRecordFixture(t, RTIonBeamsTreatmentRecordStorage, false)
	rsReplaceItem(t, record.Dataset, ft(0x30080021), 0, func(b *object.Object) { b.Remove(ft(0x300a0350)) })
	if p, err := ReadTreatmentRecord(context.Background(), record, ReadOptions{}); p != nil || !errors.Is(err, ErrInvalidObject) {
		t.Fatal("Ion record missing support", err)
	}
}

func TestWritersRefuseRetainedSignatures(t *testing.T) {
	for _, nested := range []bool{false, true} {
		for _, tag := range []core.Tag{ft(0x4ffe0001), ft(0xfffafffa)} {
			f := newBrachyFixture().file(t)
			p, err := Read(f)
			if err != nil {
				t.Fatal(err)
			}
			sig := sequenceForAdmission(tag)
			if nested {
				f.Dataset.Put(sequenceForAdmission(ft(0x77771020), core.DataSet{Elements: []core.Element{sig}}))
			} else {
				f.Dataset.Put(sig)
			}
			uid, _ := f.Dataset.GetUID(tagSOPInstanceUID)
			out, err := WritePlan(context.Background(), p, WriteOptions{Template: f})
			if out != nil || !errors.Is(err, ErrUnsupportedFeature) {
				t.Fatalf("signature %s nested=%v: out=%v err=%v", tag, nested, out != nil, err)
			}
			if got, _ := f.Dataset.GetUID(tagSOPInstanceUID); got != uid {
				t.Fatal("source identity mutated")
			}
		}
	}
}
func TestImageWriterRefusesTemplateIdentityBeforeNormalization(t *testing.T) {
	for _, mismatch := range []string{"byte-order", "meta-syntax", "meta-class", "meta-instance"} {
		f := newImageFixture(t, 16, false)
		img, err := ReadImage(context.Background(), f, ReadOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if mismatch == "byte-order" {
			f.TransferSyntax = transfer.ExplicitVRBigEndian
			f.Meta.Put(derivedio.UI(ft(0x00020010), transfer.ExplicitVRBigEndian.UID))
		} else if mismatch == "meta-syntax" {
			f.Meta.Put(derivedio.UI(ft(0x00020010), transfer.ImplicitVRLittleEndian.UID))
		} else if mismatch == "meta-class" {
			f.Meta.Remove(ft(0x00020002))
		} else {
			f.Meta.Remove(ft(0x00020003))
		}
		out, err := WriteImage(context.Background(), img, WriteOptions{Template: f})
		if out != nil || !errors.Is(err, ErrInvalidObject) {
			t.Fatalf("%s normalized away: out=%v err=%v", mismatch, out != nil, err)
		}
	}
}
