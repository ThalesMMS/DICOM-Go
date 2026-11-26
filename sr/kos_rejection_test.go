package sr

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/codes/dcm"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/commontest"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const kosTestStudy = "1.2.826.0.1.3680043.10.1.1047.1"

func kosTestOptions() KOSRejectionOptions {
	return KOSRejectionOptions{
		SOPInstanceUID: "1.2.3.990", StudyInstanceUID: kosTestStudy, SeriesInstanceUID: "1.2.3.991",
		CreatedAt: time.Date(2026, 10, 9, 20, 0, 0, 0, time.UTC), Common: object.DerivedCommon{Source: commontest.Source()},
		References: []KOSRejectionReference{
			{ValueImage, kosTestStudy, "1.2.3.21", "1.2.840.10008.5.1.4.1.1.2", "1.2.3.22", ""},
			{ValueWaveform, kosTestStudy, "1.2.3.31", "1.2.840.10008.5.1.4.1.1.9.1.1", "1.2.3.32", ""},
			{ValueComposite, kosTestStudy, "1.2.3.41", ComprehensiveSRStorage, "1.2.3.42", ""},
		},
		Description: "Synthetic rejection: revisão Ω",
	}
}

func TestKOSRejectionFactoriesRoundTrip(t *testing.T) {
	cases := []struct {
		kind KOSRejectionKind
		code string
		make func(context.Context, KOSRejectionOptions) (*object.Object, error)
	}{
		{KOSRejectionQuality, "113001", NewKOSQualityRejection},
		{KOSRejectionPatientSafety, "113037", NewKOSPatientSafetyRejection},
		{KOSRejectionIncorrectWorklist, "113038", NewKOSIncorrectWorklistRejection},
		{KOSRejectionRetentionExpired, "113039", NewKOSRetentionExpiredRejection},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			o := kosTestOptions()
			if tc.kind == KOSRejectionQuality {
				o.QualityReasons = []CodedEntry{FromCode(dcm.ImageArtifactS), FromCode(dcm.MotionBlur)}
			}
			// Identical duplicates collapse without changing the first occurrence order.
			o.References = append(o.References, o.References[0])
			before := o.Common.Source.Elements()
			dataset, err := tc.make(context.Background(), o)
			if err != nil {
				t.Fatal(err)
			}
			commontest.Check(t, dataset, false)
			if !reflect.DeepEqual(before, o.Common.Source.Elements()) {
				t.Fatal("source changed")
			}
			if dataset.Has(core.TagPixelData) {
				t.Fatal("copied pixels")
			}
			if dataset.Has(tagCompletionFlag) || dataset.Has(tagVerificationFlag) {
				t.Fatal("SR lifecycle fields on KOS")
			}
			for _, syntax := range []transfer.Syntax{transfer.ExplicitVRLittleEndian, transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian} {
				var wire bytes.Buffer
				if err = object.WriteDataSet(&wire, dataset, syntax); err != nil {
					t.Fatal(err)
				}
				back, e := object.ReadDataSet(bytes.NewReader(wire.Bytes()), syntax)
				if e != nil {
					t.Fatal(e)
				}
				note, e := ReadKOSRejection(context.Background(), back, KOSRejectionLimits{})
				if e != nil {
					t.Fatal(e)
				}
				if note.Kind != tc.kind || note.Title.CodeValue != tc.code || note.Description != o.Description ||
					!reflect.DeepEqual(note.References, o.References[:3]) || !reflect.DeepEqual(note.QualityReasons, o.QualityReasons) {
					t.Fatalf("unexpected note: %+v", note)
				}
			}
		})
	}
}

func TestKOSRejectionFactoryErrorsAreAtomic(t *testing.T) {
	for name, edit := range map[string]func(*KOSRejectionOptions){
		"no references":                  func(o *KOSRejectionOptions) { o.References = nil },
		"UID":                            func(o *KOSRejectionOptions) { o.SOPInstanceUID = "1.02.3" },
		"other study":                    func(o *KOSRejectionOptions) { o.References[0].StudyInstanceUID = "1.2.3.999" },
		"same series":                    func(o *KOSRejectionOptions) { o.References[0].SeriesInstanceUID = o.SeriesInstanceUID },
		"self reference":                 func(o *KOSRejectionOptions) { o.References[0].SOPInstanceUID = o.SOPInstanceUID },
		"note role collision":            func(o *KOSRejectionOptions) { o.SOPInstanceUID = o.StudyInstanceUID },
		"cross reference role collision": func(o *KOSRejectionOptions) { o.References[1].SOPInstanceUID = o.References[0].SeriesInstanceUID },
		"standard UID as identity":       func(o *KOSRejectionOptions) { o.SeriesInstanceUID = o.References[0].SOPClassUID },
		"non storage":                    func(o *KOSRejectionOptions) { o.References[0].SOPClassUID = "1.2.840.10008.1.1" },
		"unsupported role":               func(o *KOSRejectionOptions) { o.References[0].ValueType = ValueText },
		"conflict": func(o *KOSRejectionOptions) {
			r := o.References[0]
			r.SeriesInstanceUID = "1.2.3.77"
			o.References = append(o.References, r)
		},
		"role conflict": func(o *KOSRejectionOptions) {
			r := o.References[0]
			r.ValueType = ValueComposite
			o.References = append(o.References, r)
		},
		"source study VM": func(o *KOSRejectionOptions) {
			o.Common.Source.Put(core.Element{Header: core.ElementHeader{Tag: tagStudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{kosTestStudy, kosTestStudy}})
		},
		"source study VR": func(o *KOSRejectionOptions) {
			o.Common.Source.Put(strElem(tagStudyInstanceUID, core.VRLO, kosTestStudy))
		},
		"reason on non quality": func(o *KOSRejectionOptions) { o.QualityReasons = []CodedEntry{FromCode(dcm.MotionBlur)} },
		"bad UTF8":              func(o *KOSRejectionOptions) { o.Description = "\xff" },
		"NUL":                   func(o *KOSRejectionOptions) { o.Description = "bad\x00text" },
		"whitespace":            func(o *KOSRejectionOptions) { o.Description = "   " },
		"partial number":        func(o *KOSRejectionOptions) { o.InstanceNumber = "1\\2" },
		"negative limit":        func(o *KOSRejectionOptions) { o.Limits.MaxItems = -1 },
		"raised cap":            func(o *KOSRejectionOptions) { o.Limits.MaxItems = 50001 },
		"reference budget":      func(o *KOSRejectionOptions) { o.Limits.MaxReferences = 2 },
		"metadata budget":       func(o *KOSRejectionOptions) { o.Limits.MaxMetadataBytes = 16 },
		"description budget":    func(o *KOSRejectionOptions) { o.Limits.MaxDescriptionBytes = 1 },
	} {
		t.Run(name, func(t *testing.T) {
			o := kosTestOptions()
			edit(&o)
			before := o.Common.Source.Elements()
			refs := append([]KOSRejectionReference(nil), o.References...)
			got, err := NewKOSPatientSafetyRejection(context.Background(), o)
			if err == nil || got != nil {
				t.Fatalf("got=%v err=%v", got, err)
			}
			if !reflect.DeepEqual(before, o.Common.Source.Elements()) || !reflect.DeepEqual(refs, o.References) {
				t.Fatal("input changed on failure")
			}
		})
	}
	o := kosTestOptions()
	if got, err := NewKOSQualityRejection(context.Background(), o); err == nil || got != nil {
		t.Fatal("missing quality reason accepted")
	}
	o.QualityReasons = []CodedEntry{{"113001", "DCM", "Wrong reason"}}
	if got, err := NewKOSQualityRejection(context.Background(), o); err == nil || got != nil {
		t.Fatal("reason outside CID accepted")
	}
	if got, err := NewKOSRejection(context.Background(), 0, o); err == nil || got != nil {
		t.Fatal("kind accepted")
	}
	if got, err := NewKOSPatientSafetyRejection(nil, kosTestOptions()); err == nil || got != nil {
		t.Fatal("nil context accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got, err := NewKOSPatientSafetyRejection(ctx, kosTestOptions()); !errors.Is(err, context.Canceled) || got != nil {
		t.Fatalf("cancel: %v", err)
	}
}

func TestKOSRejectionSourceBudgetDAGAndEmptyItems(t *testing.T) {
	for _, empty := range []bool{false, true} {
		t.Run(map[bool]string{false: "repeated DAG", true: "empty items"}[empty], func(t *testing.T) {
			o := kosTestOptions()
			var items []core.DataSet
			if empty {
				items = make([]core.DataSet, 17)
			} else {
				d := core.DataSet{Elements: []core.Element{strElem(core.NewTag(0x0010, 0x0020), core.VRLO, "synthetic")}}
				for i := 0; i < 5; i++ {
					d = core.DataSet{Elements: []core.Element{seqElement(core.NewTag(0x0011, 0x1001), d, d)}}
				}
				items = []core.DataSet{d}
			}
			o.Common.Source.Put(seqElement(core.NewTag(0x0008, 0x1120), items...))
			o.Limits.MaxItems = 16
			before := o.Common.Source.Elements()
			got, err := NewKOSPatientSafetyRejection(context.Background(), o)
			if got != nil || !errors.Is(err, ErrResourceLimitExceeded) {
				t.Fatalf("got=%v err=%v", got, err)
			}
			if !reflect.DeepEqual(before, o.Common.Source.Elements()) {
				t.Fatal("source changed")
			}
		})
	}
	o := kosTestOptions()
	o.Common.Source.Put(strElem(core.NewTag(0x0010, 0x0020), core.VRLO, strings.Repeat("a", 400)))
	o.Limits.MaxMetadataBytes = 1500
	if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got != nil || !errors.Is(err, ErrResourceLimitExceeded) {
		t.Fatalf("text budget: %v", err)
	}
}

func TestKOSRejectionOwnedMetadataAndTimezone(t *testing.T) {
	o := kosTestOptions()
	o.Common.Source.Put(strElem(core.NewTag(0x0008, 0x0201), core.VRSH, "+0530"))
	tags := core.TagValue{core.NewTag(0x0010, 0x0010)}
	uv := core.Uint64Value{99}
	sv := core.Int64Value{-99}
	o.Common.Source.Put(seqElement(core.NewTag(0x0008, 0x1120), core.DataSet{Elements: []core.Element{
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1001), VR: core.VRAT}, Value: tags},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1002), VR: core.VRUV}, Value: uv},
		{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1003), VR: core.VRSV}, Value: sv},
	}}))
	got, err := NewKOSPatientSafetyRejection(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	note, err := ReadKOSRejection(context.Background(), got, KOSRejectionLimits{})
	if err != nil {
		t.Fatal(err)
	}
	if note.ContentDate != "20261010" || note.ContentTime != "013000" {
		t.Fatalf("offset not honored: %s %s", note.ContentDate, note.ContentTime)
	}
	items, _ := got.GetSequence(core.NewTag(0x0008, 0x1120))
	for _, e := range items[0].Elements() {
		switch v := e.Value.(type) {
		case core.TagValue:
			v[0] = core.TagPixelData
		case core.Uint64Value:
			v[0] = 1
		case core.Int64Value:
			v[0] = 1
		}
	}
	if tags[0] == core.TagPixelData || uv[0] != 99 || sv[0] != -99 {
		t.Fatal("inherited payload alias")
	}
	note.References[0].SOPInstanceUID = "1.2.3.9999"
	note2, err := ReadKOSRejection(context.Background(), got, KOSRejectionLimits{})
	if err != nil || note2.References[0].SOPInstanceUID != o.References[0].SOPInstanceUID {
		t.Fatal("reader result aliases source")
	}
	for _, offset := range []string{"-0000", "+1460", "+1401", "-1201", "+05"} {
		o := kosTestOptions()
		o.Common.Source.Put(strElem(core.NewTag(0x0008, 0x0201), core.VRSH, offset))
		if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got != nil || err == nil {
			t.Fatalf("accepted offset %q", offset)
		}
	}
	o = kosTestOptions()
	o.Common.Source = nil
	if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got == nil || err != nil {
		t.Fatalf("no common source: %v", err)
	}
}

func TestKOSRejectionManyQualityModifiersAndGeneratedBudgets(t *testing.T) {
	o := kosTestOptions()
	o.References = o.References[:1]
	for i := 0; i < 16; i++ {
		o.QualityReasons = append(o.QualityReasons, FromCode(dcm.MotionBlur))
	}
	if got, err := NewKOSQualityRejection(context.Background(), o); got == nil || err != nil {
		t.Fatalf("bounded many modifiers: %v", err)
	}
	for _, l := range []KOSRejectionLimits{{MaxItems: 45}, {MaxElements: 200}, {MaxDepth: 2}} {
		o.Limits = l
		before := o.Common.Source.Elements()
		if got, err := NewKOSQualityRejection(context.Background(), o); got != nil || !errors.Is(err, ErrResourceLimitExceeded) {
			t.Fatalf("generated budget: %v", err)
		}
		if !reflect.DeepEqual(before, o.Common.Source.Elements()) {
			t.Fatal("changed source")
		}
	}
}

func TestKOSRejectionCreationCalendarRange(t *testing.T) {
	for _, c := range []struct {
		at     time.Time
		offset string
	}{
		{time.Date(9999, 12, 31, 23, 0, 0, 0, time.UTC), "+0530"},
		{time.Date(1, 1, 1, 1, 0, 0, 0, time.UTC), "-0330"},
		{time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC), "+0000"},
	} {
		o := kosTestOptions()
		o.CreatedAt = c.at
		o.Common.Source.Put(strElem(core.NewTag(0x0008, 0x0201), core.VRSH, c.offset))
		if got, err := NewKOSPatientSafetyRejection(context.Background(), o); got != nil || err == nil {
			t.Fatalf("creation outside DA: %+v", c)
		}
	}
}
