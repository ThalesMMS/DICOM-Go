package attrpath

import (
	"context"
	"encoding/binary"
	"errors"
	"math"
	"strings"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/object"
)

func dccondSession(t *testing.T, dataset *object.Object, limits EditLimits) *EditSession {
	t.Helper()
	session, err := NewEditSession(context.Background(), dataset, EditSessionOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	return session
}

func dccondEvaluate(t *testing.T, session *EditSession, text string, want bool) {
	t.Helper()
	condition, err := ParseCondition(text, nil)
	if err != nil {
		t.Fatalf("parse condition: %v", err)
	}
	got, err := session.Evaluate(condition)
	if err != nil || got != want {
		t.Fatalf("evaluate %q = %t, %v; want %t", text, got, err, want)
	}
}

func dccondElement(tag core.Tag, vr core.VR, value core.Value) core.Element {
	return core.Element{Header: core.ElementHeader{Tag: tag, VR: vr}, Value: value}
}

func dccondDataSet(elements ...core.Element) core.DataSet {
	return core.DataSet{Elements: elements}
}

func TestDCCondParserOperatorsAndPrivateCreator(t *testing.T) {
	for _, text := range []string{
		"PatientID", "!PatientID", "PatientID=X", "PatientID!=X", "PatientID<X", "PatientID<=X", "PatientID>X", "PatientID>=X",
		"  PatientID = leading value ", "PatientName=A^B=C", "ImageType[1]=PRIMARY", "ReferencedSeriesSequence[*].SeriesInstanceUID=1.2.3",
		`(0011,"CREATOR=!<=>",10)=A=B<C>`, `(0011,"CREATOR=!<=>",10)`, `!(0011,"CREATOR=!<=>",10)`,
	} {
		if _, err := ParseCondition(text, nil); err != nil {
			t.Errorf("parse %q: %v", text, err)
		}
	}
	c, err := ParseCondition(" PatientName = A^B=C ", nil)
	if err != nil || c.value != " A^B=C " || c.op != conditionEqual {
		t.Fatalf("operand was not preserved: %+v, %v", c, err)
	}
	for _, text := range []string{"", "!", "=X", "NoSuchKeyword=X", "PatientID!X", "!PatientID=X", "PatientID.[0]=X", "ImageType[-1]=X", "ImageType[01]=X", "ContentSequence.PatientID=X", `(0011,"SECRET`, string([]byte{0xff})} {
		if _, err := ParseCondition(text, nil); !errors.Is(err, ErrSyntax) {
			t.Errorf("parse bad condition: %v; want ErrSyntax", err)
		}
	}
	if _, err := ParseCondition(strings.Repeat("x", MaxConditionLength+1), nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("condition length limit: %v", err)
	}
	if _, err := ParseCondition("ImageType[999999999999]=X", nil); !errors.Is(err, ErrLimit) {
		t.Fatalf("condition index limit: %v", err)
	}
	if p := MustParse("ImageType[1]"); !p.SelectsItem() || p[len(p)-1].Item != 1 {
		t.Fatal("condition parsing changed the shared path grammar")
	}
}

func TestDCCondEightChecksAndEmptyValues(t *testing.T) {
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(core.NewTag(0x0010, 0x0020), core.VRLO, core.StringValue{"B "}))
	dataset.Put(dccondElement(core.NewTag(0x0010, 0x0010), core.VRPN, core.StringValue{""}))
	session := dccondSession(t, dataset, EditLimits{})
	for _, c := range []struct {
		text string
		want bool
	}{
		{"PatientID", true}, {"!PatientID", false}, {"PatientID=B", true}, {"PatientID!=B", false},
		{"PatientID<A", false}, {"PatientID<=B", true}, {"PatientID>A", true}, {"PatientID>=C", false},
		{"PatientBirthDate", false}, {"!PatientBirthDate", true}, {"PatientBirthDate!=20000101", false},
		{"PatientName", true}, {"PatientName=", true}, {"PatientName[0]", false}, {"!PatientName[0]", true},
		{"PatientID[0]=B", true}, {"PatientID[1]", false}, {"!PatientID[1]", true}, {"PatientID[1]!=B", false},
	} {
		dccondEvaluate(t, session, c.text, c.want)
	}
	if ok, err := session.Evaluate(Condition{}); ok || !errors.Is(err, ErrSyntax) {
		t.Fatalf("zero condition: %t, %v", ok, err)
	}
}

func TestDCCondVMAndLexicalNumberStrings(t *testing.T) {
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(core.NewTag(0x0008, 0x0008), core.VRCS, core.StringValue{"ORIGINAL", "PRIMARY", "M"}))
	dataset.Put(dccondElement(core.NewTag(0x0018, 0x0050), core.VRDS, core.StringValue{" 10 "}))
	dataset.Put(dccondElement(core.NewTag(0x0020, 0x0013), core.VRIS, core.RawValue("10")))
	dataset.Put(dccondElement(core.NewTag(0x0010, 0x4000), core.VRLT, core.StringValue{`A\B`}))
	session := dccondSession(t, dataset, EditLimits{})
	for _, c := range []struct {
		text string
		want bool
	}{
		{`ImageType=ORIGINAL\PRIMARY\M`, true}, {`ImageType>ORIGINAL\PRIMARY`, true}, {`ImageType<ORIGINAL\PRIMARY\N`, true},
		{`ImageType[1]=PRIMARY`, true}, {`ImageType[2]=M`, true}, {`ImageType[3]!=X`, false},
		{`SliceThickness<2`, true}, {`SliceThickness=10`, true}, {`InstanceNumber<2`, true}, {`InstanceNumber>=2`, false},
		{`PatientComments=A\B`, true}, {`PatientComments[1]`, false},
	} {
		dccondEvaluate(t, session, c.text, c.want)
	}
	condition, err := ParseCondition("ImageType[*]", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
		t.Fatalf("terminal VM wildcard: %v", err)
	}
}

func TestDCCondSequencesAndWildcardAllMissing(t *testing.T) {
	patientID := core.NewTag(0x0010, 0x0020)
	content := core.NewTag(0x0040, 0xA730)
	code := core.NewTag(0x0008, 0x0100)
	item := func(value string, include bool) core.DataSet {
		elements := []core.Element{dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{
			dccondDataSet(dccondElement(code, core.VRSH, core.StringValue{value})),
		}})}
		if include {
			elements = append(elements, dccondElement(patientID, core.VRLO, core.StringValue{value}))
		}
		return core.DataSet{Elements: elements}
	}
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{item("A", true), item("A", true)}}))
	session := dccondSession(t, dataset, EditLimits{})
	for _, c := range []struct {
		text string
		want bool
	}{
		{"ContentSequence", true}, {"ContentSequence[0]", true}, {"ContentSequence[1]", true}, {"ContentSequence[2]", false},
		{"!ContentSequence[2]", true}, {"ContentSequence[*]", true}, {"ContentSequence[*].PatientID=A", true},
		{"ContentSequence[*].PatientID!=B", true}, {"ContentSequence[*].ContentSequence[*].CodeValue=A", true},
		{"ContentSequence[*].PatientID[0]", true}, {"ContentSequence[*].PatientID[1]", false}, {"!ContentSequence[*].PatientID[1]", true},
	} {
		dccondEvaluate(t, session, c.text, c.want)
	}
	for _, text := range []string{"ContentSequence=A", "ContentSequence[0]=A", "ContentSequence[*]=A"} {
		condition, _ := ParseCondition(text, nil)
		if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
			t.Fatalf("sequence/item comparison: %v", err)
		}
	}
	// An absent leaf in a selected branch makes ALL false even when another
	// branch contains a match; existence still means any matching branch.
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{item("A", true), item("A", false)}}))
	for _, text := range []string{"ContentSequence[*].PatientID=A", "ContentSequence[*].PatientID!=B", "ContentSequence[*].PatientID[0]=A"} {
		dccondEvaluate(t, session, text, false)
	}
	dccondEvaluate(t, session, "ContentSequence[*].PatientID", true)
	dccondEvaluate(t, session, "!ContentSequence[*].PatientID", false)
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{
		item("A", true), dccondDataSet(dccondElement(content, core.VRSQ, core.SequenceValue{})),
	}}))
	dccondEvaluate(t, session, "ContentSequence[*].ContentSequence[*].CodeValue=A", false)
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{}))
	dccondEvaluate(t, session, "ContentSequence", true)
	dccondEvaluate(t, session, "ContentSequence[*]", false)
	dccondEvaluate(t, session, "!ContentSequence[*]", true)
	dccondEvaluate(t, session, "ContentSequence[*].PatientID!=B", false)
}

func TestDCCondWildcardVMExistenceAnyAndComparisonAll(t *testing.T) {
	content := core.NewTag(0x0040, 0xA730)
	imageType := core.NewTag(0x0008, 0x0008)
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{
		dccondDataSet(dccondElement(imageType, core.VRCS, core.StringValue{"ORIGINAL"})),
		dccondDataSet(dccondElement(imageType, core.VRCS, core.StringValue{"ORIGINAL", "PRIMARY"})),
	}}))
	session := dccondSession(t, dataset, EditLimits{})
	dccondEvaluate(t, session, "ContentSequence[*].ImageType[1]", true)
	dccondEvaluate(t, session, "!ContentSequence[*].ImageType[1]", false)
	dccondEvaluate(t, session, "ContentSequence[*].ImageType[1]=PRIMARY", false)
	dccondEvaluate(t, session, "ContentSequence[*].ImageType[1]!=SECONDARY", false)
}

func TestDCCondIntegersExactExtremesAndBinaryOrder(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1010)
	cases := []struct {
		vr                            core.VR
		value                         core.Value
		equal, less, greater, invalid string
	}{
		{core.VRUS, core.Uint16Value{65535}, "65535", "65534", "65535", "65536"},
		{core.VRSS, core.Int16Value{-32768}, "-32768", "-32768", "-32767", "-32769"},
		{core.VRUL, core.Uint32Value{4294967295}, "4294967295", "4294967294", "4294967295", "4294967296"},
		{core.VRSL, core.Int32Value{-2147483648}, "-2147483648", "-2147483648", "-2147483647", "-2147483649"},
		{core.VRUV, core.Uint64Value{18446744073709551615}, "18446744073709551615", "18446744073709551614", "18446744073709551615", "18446744073709551616"},
		{core.VRSV, core.Int64Value{-9223372036854775808}, "-9223372036854775808", "-9223372036854775808", "-9223372036854775807", "-9223372036854775809"},
	}
	for _, c := range cases {
		t.Run(c.vr.String(), func(t *testing.T) {
			dataset := object.New(std.Dictionary)
			dataset.Put(dccondElement(tag, c.vr, c.value))
			session := dccondSession(t, dataset, EditLimits{})
			dccondEvaluate(t, session, "(0011,1010)="+c.equal, true)
			dccondEvaluate(t, session, "(0011,1010)>="+c.less, true)
			dccondEvaluate(t, session, "(0011,1010)<="+c.greater, true)
			dccondEvaluate(t, session, "(0011,1010)!="+c.equal, false)
			condition, _ := ParseCondition("(0011,1010)="+c.invalid, nil)
			if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
				t.Fatalf("out-of-range operand: %v", err)
			}
		})
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		dataset := object.New(std.Dictionary)
		dataset.SetValueByteOrder(order)
		raw := make([]byte, 16)
		order.PutUint64(raw[:8], 18446744073709551614)
		order.PutUint64(raw[8:], 18446744073709551615)
		dataset.Put(dccondElement(tag, core.VRUV, core.RawValue(raw)))
		session := dccondSession(t, dataset, EditLimits{})
		dccondEvaluate(t, session, `(0011,1010)=18446744073709551614\18446744073709551615`, true)
		dccondEvaluate(t, session, `(0011,1010)[0]<18446744073709551615`, true)
		dccondEvaluate(t, session, `(0011,1010)[1]>18446744073709551614`, true)
		dccondEvaluate(t, session, `(0011,1010)[2]!=0`, false)
		order.PutUint64(raw[:8], uint64(1)<<63)
		order.PutUint64(raw[8:], uint64(1)<<63-1)
		dataset.Put(dccondElement(tag, core.VRSV, core.RawValue(raw)))
		dccondEvaluate(t, session, `(0011,1010)=-9223372036854775808\9223372036854775807`, true)
	}
}

func TestDCCondFloatingFiniteAndATCanonical(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1010)
	for _, vr := range []core.VR{core.VRFL, core.VRFD} {
		dataset := object.New(std.Dictionary)
		value := core.Value(core.Float64Value{1.5, -2})
		if vr == core.VRFL {
			value = core.Float32Value{1.5, -2}
		}
		dataset.Put(dccondElement(tag, vr, value))
		session := dccondSession(t, dataset, EditLimits{})
		dccondEvaluate(t, session, `(0011,1010)=1.5\-2`, true)
		dccondEvaluate(t, session, `(0011,1010)[1]<0`, true)
		for _, rhs := range []string{"NaN", "Inf", "+Inf", "-Inf", "not-number", "1e999"} {
			condition, _ := ParseCondition("(0011,1010)="+rhs, nil)
			if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
				t.Fatalf("invalid float operand: %v", err)
			}
		}
		for _, nonfinite := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
			value = core.Float64Value{1.5, nonfinite}
			if vr == core.VRFL {
				value = core.Float32Value{1.5, float32(nonfinite)}
			}
			dataset.Put(dccondElement(tag, vr, value))
			condition, _ := ParseCondition("(0011,1010)[0]=1.5", nil)
			if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
				t.Fatalf("nonfinite dataset component: %v", err)
			}
		}
	}
	for _, order := range []binary.ByteOrder{binary.LittleEndian, binary.BigEndian} {
		dataset := object.New(std.Dictionary)
		dataset.SetValueByteOrder(order)
		raw := make([]byte, 8)
		order.PutUint16(raw[:2], 0x0010)
		order.PutUint16(raw[2:4], 0x0010)
		order.PutUint16(raw[4:6], 0x0020)
		order.PutUint16(raw[6:], 0x0013)
		dataset.Put(dccondElement(tag, core.VRAT, core.RawValue(raw)))
		session := dccondSession(t, dataset, EditLimits{})
		dccondEvaluate(t, session, `(0011,1010)=(0010,0010)\00200013`, true)
		dccondEvaluate(t, session, `(0011,1010)[0]<00200000`, true)
		for _, text := range []string{"0010", "(00100010)", "(00,100010)", "+0100010"} {
			condition, _ := ParseCondition("(0011,1010)="+text, nil)
			if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
				t.Fatalf("invalid AT operand: %v", err)
			}
		}
	}
}

func TestDCCondUIDKeywordAndStrictCharacterSetInheritance(t *testing.T) {
	content := core.NewTag(0x0040, 0xA730)
	charset := core.NewTag(0x0008, 0x0005)
	patientName := core.NewTag(0x0010, 0x0010)
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(core.NewTag(0x0008, 0x0016), core.VRUI, core.RawValue("1.2.840.10008.5.1.4.1.1.2\x00")))
	dataset.Put(dccondElement(charset, core.VRCS, core.StringValue{"ISO_IR 100"}))
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{
		dccondDataSet(dccondElement(patientName, core.VRPN, core.RawValue{'M', 0xfc, 'l', 'l', 'e', 'r', '^', 'A'})),
		dccondDataSet(dccondElement(charset, core.VRCS, core.StringValue{"ISO_IR 192"}), dccondElement(patientName, core.VRPN, core.RawValue("山田^花子"))),
	}}))
	session := dccondSession(t, dataset, EditLimits{})
	dccondEvaluate(t, session, "SOPClassUID=CTImageStorage", true)
	dccondEvaluate(t, session, "SOPClassUID=ctimagestorage", true)
	dccondEvaluate(t, session, "SOPClassUID!=MRImageStorage", true)
	dccondEvaluate(t, session, "ContentSequence[0].PatientName=Müller^A", true)
	dccondEvaluate(t, session, "ContentSequence[1].PatientName=山田^花子", true)
	for _, text := range []string{"SOPClassUID=UnknownUIDKeyword", "SOPClassUID=1..2", "ContentSequence[0].PatientName=山田^花子"} {
		condition, _ := ParseCondition(text, nil)
		if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
			t.Fatalf("invalid UI/charset operand: %v", err)
		}
	}
	dataset.Put(dccondElement(charset, core.VRCS, core.StringValue{"ISO_IR 192"}))
	dataset.Put(dccondElement(patientName, core.VRPN, core.RawValue{0xff, 0xfe}))
	condition, _ := ParseCondition("PatientName=X", nil)
	if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
		t.Fatalf("invalid UTF-8 dataset: %v", err)
	}
	dataset.Put(dccondElement(charset, core.VRCS, core.StringValue{"ISO_IR 999"}))
	dataset.Put(dccondElement(patientName, core.VRPN, core.RawValue("A^B ")))
	if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
		t.Fatalf("unsupported dataset charset: %v", err)
	}
	// Existence of the attribute does not depend on decoding its bytes.
	dccondEvaluate(t, session, "PatientName", true)
}

func TestDCCondRefusesBulkUnknownAndMalformedValues(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1010)
	for _, vr := range []core.VR{core.VROB, core.VROW, core.VROF, core.VROD, core.VROL, core.VROV, core.VRUN, core.VRSQ} {
		dataset := object.New(std.Dictionary)
		value := core.Value(core.RawValue{1, 2})
		if vr == core.VRSQ {
			value = core.SequenceValue{}
		}
		dataset.Put(dccondElement(tag, vr, value))
		session := dccondSession(t, dataset, EditLimits{})
		dccondEvaluate(t, session, "(0011,1010)", true)
		condition, _ := ParseCondition("(0011,1010)=", nil)
		if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
			t.Fatalf("unsupported %s comparison: %v", vr, err)
		}
	}
	for _, element := range []core.Element{
		dccondElement(core.TagPixelData, core.VROB, core.RawValue{1, 2}),
		dccondElement(core.TagPixelData, core.VROB, core.FragmentSequence{Fragments: [][]byte{{1, 2}}}),
		dccondElement(tag, core.VRLO, core.BulkDataValue{URI: "https://fixture.invalid/SECRET"}),
		dccondElement(tag, core.VRUS, core.RawValue{1}),
		dccondElement(tag, core.VRUV, core.Uint16Value{1}),
		dccondElement(tag, core.VRFD, core.RawValue{1, 2}),
		dccondElement(tag, core.VRLO, core.StringValue{string([]byte{0xff})}),
	} {
		dataset := object.New(std.Dictionary)
		dataset.Put(element)
		session := dccondSession(t, dataset, EditLimits{})
		text := "(0011,1010)="
		if element.Tag() == core.TagPixelData {
			text = "PixelData="
		}
		condition, _ := ParseCondition(text, nil)
		if _, err := session.Evaluate(condition); !errors.Is(err, ErrValue) {
			t.Fatalf("malformed or unresolved comparison: %v", err)
		}
	}
}

func TestDCCondSharedBudgetsCancelAndSanitizedErrors(t *testing.T) {
	content := core.NewTag(0x0040, 0xA730)
	patientID := core.NewTag(0x0010, 0x0020)
	dataset := object.New(std.Dictionary)
	dataset.Put(dccondElement(content, core.VRSQ, core.SequenceValue{Items: []core.DataSet{
		dccondDataSet(dccondElement(patientID, core.VRLO, core.StringValue{"A"})),
		dccondDataSet(dccondElement(patientID, core.VRLO, core.StringValue{"A"})),
	}}))
	condition, _ := ParseCondition("ContentSequence[*].PatientID=A", nil)
	// Admission counts the SQ, its two items and its two leaves: five
	// visits. At that exact cap the following evaluation is refused.
	for _, limits := range []EditLimits{{MaxMatches: 1}, {MaxPathBytes: 1}, {MaxVisits: 5}} {
		session := dccondSession(t, dataset, limits)
		if _, err := session.Evaluate(condition); !errors.Is(err, ErrLimit) {
			t.Fatalf("condition selection cap: %v", err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	session, err := NewEditSession(ctx, dataset, EditSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	cancel()
	if _, err := session.Evaluate(condition); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled condition: %v", err)
	}
	controlled := &dccondCancelContext{Context: context.Background(), remaining: -1}
	session, err = NewEditSession(controlled, dataset, EditSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	controlled.remaining = 8
	if _, err := session.Evaluate(condition); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel during selection: %v", err)
	}
	for _, text := range []string{`(0011,"SECRET_CREATOR",10).SECRET_KEYWORD=SECRET_UID`, "SECRET_KEYWORD=SECRET_VALUE"} {
		_, err := ParseCondition(text, nil)
		if err == nil || strings.Contains(err.Error(), "SECRET") {
			t.Fatalf("condition parse disclosure: %v", err)
		}
	}
	dataset = object.New(std.Dictionary)
	dataset.Put(dccondElement(core.NewTag(0x0011, 0x1010), core.VRUI, core.StringValue{"1.2.3"}))
	session = dccondSession(t, dataset, EditLimits{})
	condition, _ = ParseCondition("(0011,1010)=SECRET_VALUE", nil)
	if _, err := session.Evaluate(condition); err == nil || strings.Contains(err.Error(), "SECRET") || strings.Contains(err.Error(), "1.2.3") {
		t.Fatalf("condition value disclosure: %v", err)
	}
	// No condition changes values or inserts an absent leaf.
	if value, _ := dataset.GetString(core.NewTag(0x0011, 0x1010)); value != "1.2.3" || dataset.Has(patientID) {
		t.Fatal("condition evaluation changed the model")
	}
}

type dccondCancelContext struct {
	context.Context
	remaining int
}

func TestDCCondConversionBudgetBeforeDecode(t *testing.T) {
	tag := core.NewTag(0x0010, 0x0020)
	dataset := object.FromElements([]core.Element{dccondElement(tag, core.VRLO, core.RawValue{0xff, 'X'})}, std.Dictionary)
	session := dccondSession(t, dataset, EditLimits{MaxValueBytes: 1})
	condition, err := ParseCondition("PatientID=X", nil)
	if err != nil {
		t.Fatal(err)
	}
	// An insufficient conversion budget must win over invalid encoding: no
	// decoder, VM slice or operand conversion may run first.
	if _, err = session.Evaluate(condition); !errors.Is(err, ErrLimit) {
		t.Fatalf("decode preceded admission: %v", err)
	}
	el, _ := dataset.Get(tag)
	if raw, ok := el.Value.(core.RawValue); !ok || len(raw) != 2 || raw[0] != 0xff {
		t.Fatal("condition changed the source")
	}
}

func TestDCCondConversionBudgetBoundaryAndSharedEdits(t *testing.T) {
	tag := core.NewTag(0x0010, 0x0020)
	newModel := func() *object.Object {
		return object.FromElements([]core.Element{dccondElement(tag, core.VRLO, core.RawValue("A\\B "))}, std.Dictionary)
	}
	condition, _ := ParseCondition("PatientID=A\\B", nil)
	measured := dccondSession(t, newModel(), EditLimits{})
	if ok, err := measured.Evaluate(condition); err != nil || !ok || measured.valueBytes == 0 {
		t.Fatalf("valid conversion: %t %v", ok, err)
	}
	budget := measured.valueBytes
	for _, limit := range []int64{budget - 1, budget} {
		s := dccondSession(t, newModel(), EditLimits{MaxValueBytes: limit})
		ok, err := s.Evaluate(condition)
		if limit == budget && (err != nil || !ok) || limit < budget && !errors.Is(err, ErrLimit) {
			t.Fatalf("boundary %d: %t %v", limit, ok, err)
		}
	}
	s := dccondSession(t, newModel(), EditLimits{MaxValueBytes: budget * 2})
	for i := 0; i < 2; i++ {
		if ok, err := s.Evaluate(condition); err != nil || !ok {
			t.Fatalf("cumulative comparison %d: %t %v", i, ok, err)
		}
	}
	if _, err := s.Apply(Edit{Op: OpModify, Path: MustParse("PatientID"), Value: "X"}); !errors.Is(err, ErrLimit) {
		t.Fatalf("edit ignored comparison budget: %v", err)
	}
	if _, err := s.Evaluate(condition); !errors.Is(err, ErrLimit) {
		t.Fatalf("comparison ledger reset: %v", err)
	}
}

func TestDCCondSplitHeadersAndCharsetBeforeAllocation(t *testing.T) {
	tag := core.NewTag(0x0010, 0x0020)
	// Empty authored components have no value bytes but still need VM headers.
	dataset := object.FromElements([]core.Element{dccondElement(tag, core.VRLO, make(core.StringValue, 128))}, std.Dictionary)
	s := dccondSession(t, dataset, EditLimits{MaxValueBytes: 127 * 16})
	condition, _ := ParseCondition("PatientID[0]", nil)
	if _, err := s.Evaluate(condition); !errors.Is(err, ErrLimit) {
		t.Fatalf("uncharged VM headers: %v", err)
	}
	dataset = object.FromElements([]core.Element{
		dccondElement(core.NewTag(0x0008, 0x0005), core.VRCS, core.RawValue("ISO_IR 100")),
		dccondElement(tag, core.VRLO, core.RawValue{0xe9, ' '}),
	}, std.Dictionary)
	if s, err := NewEditSession(context.Background(), dataset, EditSessionOptions{Limits: EditLimits{MaxValueBytes: 1}}); s != nil || !errors.Is(err, ErrLimit) {
		t.Fatalf("charset declaration preceded admission: %v", err)
	}
}

func (c *dccondCancelContext) Err() error {
	if c.remaining < 0 {
		return nil
	}
	if c.remaining == 0 {
		return context.Canceled
	}
	c.remaining--
	return nil
}
