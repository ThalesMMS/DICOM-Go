package validation

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

func writeTestDataSet() core.DataSet {
	return core.DataSet{Elements: []core.Element{
		core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1..2")),
		core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("ID")),
		{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xa730), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("ID"))}}}}},
	}}
}
func TestWriteViewReportsMatchDetachedValidator(t *testing.T) {
	for _, mode := range []Mode{ModePreserve, ModeWarn, ModeStrict} {
		for _, stop := range []bool{false, true} {
			for _, maxFindings := range []int{1, 32} {
				opts := Options{Mode: mode, StopFirst: stop, MaxFindings: maxFindings, RequiredUIDs: []core.Tag{core.NewTag(0x0020, 0x000d)}}
				dataset := writeTestDataSet()
				before := cloneDataSet(dataset)
				expected, expectedErr := ValidateDataSet(context.Background(), dataset, opts)
				op, err := NewWriteOperation(context.Background(), opts)
				if err != nil {
					t.Fatal(err)
				}
				if err = op.BeginWrite(); err != nil {
					t.Fatal(err)
				}
				actual, actualErr := op.ValidateDataSet(dataset)
				op.EndWrite()
				if !reflect.DeepEqual(expected.Report, actual.Report) || errors.Is(expectedErr, ErrValidationFailed) != errors.Is(actualErr, ErrValidationFailed) {
					t.Fatalf("mode=%v stop=%v max=%v expected=%#v actual=%#v errors %v %v", mode, stop, maxFindings, expected.Report, actual.Report, expectedErr, actualErr)
				}
				if !reflect.DeepEqual(before, dataset) {
					t.Fatal("borrowed validation mutated input")
				}
			}
		}
	}
}
func TestWriteViewContextualFallbackKeepsIsolationAndCalls(t *testing.T) {
	tag := core.NewTag(0x0010, 0x0020)
	calls := 0
	dataset := core.DataSet{Elements: []core.Element{core.NewRawElement(tag, core.VRLO, []byte("AB"))}}
	rules := []DataSetRuleRegistration{{Name: "mutate", Rule: DataSetRuleFunc(func(_ context.Context, context DataSetContext) []Finding {
		calls++
		context.DataSet.Elements[0].Value.(core.RawValue)[0] = 'X'
		return nil
	})}}
	op, err := NewWriteOperation(context.Background(), Options{DataSetRules: rules})
	if err != nil {
		t.Fatal(err)
	}
	op.BeginWrite()
	result, err := op.ValidateDataSet(dataset)
	op.EndWrite()
	if err != nil || calls != 1 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
	if string(dataset.Elements[0].Value.(core.RawValue)) != "AB" || string(result.DataSet.Elements[0].Value.(core.RawValue)) != "AB" {
		t.Fatal("rule input was not isolated")
	}
	// The owned fallback result itself is separate from the caller.
	result.DataSet.Elements[0].Value.(core.RawValue)[0] = 'Y'
	if string(dataset.Elements[0].Value.(core.RawValue)) != "AB" {
		t.Fatal("fallback returned input alias")
	}
}

type writeCountingDictionary struct{ calls int }

func (d *writeCountingDictionary) ByTag(tag core.Tag) (dictionary.Entry, bool) {
	d.calls++
	return dictionary.Entry{Tag: tag, VR: core.VRLO, VM: "1"}, true
}
func (d *writeCountingDictionary) ByKeyword(string) (dictionary.Entry, bool) {
	return dictionary.Entry{}, false
}
func TestWriteViewDoesNotMemoizeDictionary(t *testing.T) {
	d := &writeCountingDictionary{}
	op, err := NewWriteOperation(context.Background(), Options{Dictionary: d})
	if err != nil {
		t.Fatal(err)
	}
	el := core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("AB"))
	for i := 0; i < 2; i++ {
		op.BeginWrite()
		if err = op.ValidateElement(el); err != nil {
			t.Fatal(err)
		}
		op.EndWrite()
	}
	if d.calls != 4 {
		t.Fatalf("dictionary calls=%d, want original ByTag+LookupVRSpec calls on each visit", d.calls)
	}
}
func TestWriteOperationFreezesPolicyAndHooks(t *testing.T) {
	calls := 0
	chain, _ := NewHookChain(HookRegistration{Name: "first", Points: []HookPoint{HookPreSerialization}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { calls++; return HookDecision{}, nil })})
	required := []core.Tag{core.NewTag(0x0008, 0x0018)}
	op, err := NewWriteOperation(context.Background(), Options{Hooks: chain, RequiredUIDs: required})
	if err != nil {
		t.Fatal(err)
	}
	required[0] = core.NewTag(0x0020, 0x000d)
	chain.Add(HookRegistration{Name: "later", Points: []HookPoint{HookPreSerialization}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { calls += 100; return HookDecision{}, nil })})
	op.BeginWrite()
	el := core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.20"))
	result, err := op.HandleWrite(HookEvent{Point: HookPreSerialization, Element: &el})
	if err != nil {
		t.Fatal(err)
	}
	result.Element.Value.(core.RawValue)[0] = '9'
	if string(el.Value.(core.RawValue)) != "1.20" || calls != 1 {
		t.Fatalf("input=%s calls=%d", el.Value, calls)
	}
	if op.opts.RequiredUIDs[0] != el.Tag() {
		t.Fatal("required UID option not frozen")
	}
	op.EndWrite()
}

func TestWriteContextRejectsOtherOperationAndExpiredEpoch(t *testing.T) {
	op, _ := NewWriteOperation(context.Background(), Options{})
	other, _ := NewWriteOperation(context.Background(), Options{})
	el := core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("AB"))
	ds := core.DataSet{Elements: []core.Element{el}}
	op.BeginWrite()
	scope, _ := op.NewContext(ds, false)
	other.BeginWrite()
	if err := other.ApplyChange(scope, 0, el, el, false, false, nil); !errors.Is(err, ErrWriteValidationScope) {
		t.Fatal(err)
	}
	other.EndWrite()
	op.EndWrite()
	op.BeginWrite()
	defer op.EndWrite()
	if err := op.ApplyChange(scope, 0, el, el, false, false, nil); !errors.Is(err, ErrWriteValidationScope) {
		t.Fatal(err)
	}
}
