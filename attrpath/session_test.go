package attrpath

import (
	"context"
	"errors"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func dcsessObject() *object.Object {
	return object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN}, Value: core.StringValue{"OLD"}}}, std.Dictionary)
}
func TestEditSessionCumulativeValuesAndTouches(t *testing.T) {
	o := dcsessObject()
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxValueBytes: 4}})
	if e != nil {
		t.Fatal(e)
	}
	p := MustParse("PatientName")
	if _, e = s.Apply(Edit{Op: OpModify, Path: p, Value: "NEXT"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpModify, Path: p, Value: "X"}); !errors.Is(e, ErrLimit) {
		t.Fatalf("second edit %v", e)
	}
	if v, _ := o.GetString(p[0].Tag); v != "NEXT" {
		t.Fatal(v)
	}
	o = dcsessObject()
	s, e = NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxTouches: 1}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpModify, Path: p, Value: "A"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpRemove, Path: p}); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
}
func TestEditSessionCreatedItemsAndVisits(t *testing.T) {
	o := object.New(std.Dictionary)
	s, e := NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxCreatedItems: 3}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpInsert, Path: MustParse("ReferencedSeriesSequence[2].SeriesInstanceUID"), Value: "1.2.3"}); e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpInsert, Path: MustParse("ReferencedStudySequence[0].StudyInstanceUID"), Value: "1.2.4"}); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	o = dcsessObject()
	s, e = NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxVisits: 2}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpModify, Path: MustParse("PatientName"), Value: "NEXT"}); !errors.Is(e, ErrLimit) {
		t.Fatal(e)
	}
	if v, _ := o.GetString(core.NewTag(0x0010, 0x0010)); v != "OLD" {
		t.Fatal(v)
	}
}
func TestEditSessionAdmissionAndCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if s, e := NewEditSession(ctx, dcsessObject(), EditSessionOptions{}); s != nil || !errors.Is(e, context.Canceled) {
		t.Fatalf("%v %v", s, e)
	}
	if s, e := NewEditSession(context.Background(), dcsessObject(), EditSessionOptions{Limits: EditLimits{MaxItems: -1}}); s != nil || !errors.Is(e, ErrLimit) {
		t.Fatalf("%v %v", s, e)
	}
	s, e := NewEditSession(context.Background(), dcsessObject(), EditSessionOptions{})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.Apply(Edit{Op: OpInsert, Path: Path{{Tag: core.NewTag(0x0008, 0x1115), HasItem: true, Item: -1}}, Value: ""}); !errors.Is(e, ErrSyntax) {
		t.Fatal(e)
	}
	deferred := object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0x0010), VR: core.VRPN, Length: 8, LengthSet: true}}}, std.Dictionary)
	if s, e := NewEditSession(context.Background(), deferred, EditSessionOptions{}); s != nil || !errors.Is(e, ErrValue) {
		t.Fatalf("%v %v", s, e)
	}
}

func TestEditSessionModelAndPathBoundaries(t *testing.T) {
	model := func() *object.Object {
		o := dcsessObject()
		for _, p := range []string{"ReferencedStudySequence[0].PatientID", "ReferencedStudySequence[1].ReferencedStudySequence[0].PatientID"} {
			if _, err := Insert(o, MustParse(p), "X", Options{}); err != nil {
				t.Fatal(err)
			}
		}
		return o
	}
	for _, tc := range []struct {
		name   string
		limits EditLimits
		want   bool
	}{{"elements-exact", EditLimits{MaxElements: 5}, true}, {"elements-small", EditLimits{MaxElements: 4}, false}, {"items-exact", EditLimits{MaxItems: 3}, true}, {"items-small", EditLimits{MaxItems: 2}, false}, {"depth-exact", EditLimits{MaxDepth: 2}, true}, {"depth-small", EditLimits{MaxDepth: 1}, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, err := NewEditSession(context.Background(), model(), EditSessionOptions{Limits: tc.limits})
			if tc.want && (s == nil || err != nil) || !tc.want && (s != nil || !errors.Is(err, ErrLimit)) {
				t.Fatalf("admission: %v", err)
			}
		})
	}
	o := model()
	s, err := NewEditSession(context.Background(), o, EditSessionOptions{Limits: EditLimits{MaxPathBytes: 63}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ModifyAll(MustParse("PatientName")[0], "NEW"); !errors.Is(err, ErrLimit) {
		t.Fatalf("path allocated before limit: %v", err)
	}
	if v, _ := o.GetString(core.NewTag(0x0010, 0x0010)); v != "OLD" {
		t.Fatal("path failure mutated model")
	}
}
