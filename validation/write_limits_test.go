package validation

import (
	"context"
	"errors"
	"math"
	"testing"
	"unsafe"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
)

func TestWriteAdmissionBeforeThirdKnownHookCopy(t *testing.T) {
	calls := 0
	chain, _ := NewHookChain(HookRegistration{Name: "observe", Points: []HookPoint{HookPreSerialization}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { calls++; return HookDecision{}, nil })})
	op, err := NewWriteOperation(context.Background(), Options{Hooks: chain, WriteLimits: WriteLimits{MaxCopiedValueBytes: 11}})
	if err != nil {
		t.Fatal(err)
	}
	op.BeginWrite()
	defer op.EndWrite()
	el := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4})
	_, err = op.HandleWrite(HookEvent{Point: HookPreSerialization, Element: &el})
	if !errors.Is(err, ErrValidationLimit) || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestWriteAdmissionCountsAllObserversAndReplacementCopy(t *testing.T) {
	for _, replacement := range []bool{false, true} {
		calls := 0
		chain, _ := NewHookChain(HookRegistration{Name: "one", Points: []HookPoint{HookPreSerialization}, Hook: HookFunc(func(_ context.Context, e HookEvent) (HookDecision, error) {
			calls++
			if replacement {
				el := core.NewRawElement(e.Element.Tag(), core.VROB, make([]byte, 16))
				return HookDecision{Element: &el}, nil
			}
			return HookDecision{}, nil
		})}, HookRegistration{Name: "two", Points: []HookPoint{HookPreSerialization}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { calls += 100; return HookDecision{}, nil })})
		op, _ := NewWriteOperation(context.Background(), Options{Hooks: chain, WriteLimits: WriteLimits{MaxCopiedValueBytes: 12}})
		op.BeginWrite()
		el := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4})
		_, err := op.HandleWrite(HookEvent{Point: HookPreSerialization, Element: &el})
		op.EndWrite()
		if !errors.Is(err, ErrValidationLimit) || calls != 1 {
			t.Fatalf("replacement=%v err=%v calls=%d", replacement, err, calls)
		}
	}
}

type opaqueWriteValue struct{ calls *int }

func (v opaqueWriteValue) Kind() core.ValueKind { return core.ValueRaw }
func (v opaqueWriteValue) EncodedLength() (core.Length, bool) {
	*v.calls++
	panic("estimator must not invoke opaque value")
}
func TestWriteOpaqueValueEstimatorNeverInvokesEncodedLength(t *testing.T) {
	calls := 0
	el := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x1010), VR: core.VROB}, Value: opaqueWriteValue{&calls}}
	op, _ := NewWriteOperation(context.Background(), Options{})
	op.BeginWrite()
	err := op.ValidateElement(el)
	op.EndWrite()
	if err != nil || calls != 0 {
		t.Fatalf("err=%v calls=%d", err, calls)
	}
}
func TestWriteAdmissionTextAndScopeBeforeCopies(t *testing.T) {
	for _, options := range []Options{{WriteLimits: WriteLimits{MaxWorkBytes: 128}}, {WriteLimits: WriteLimits{MaxScopeNodes: 1}}} {
		op, err := NewWriteOperation(context.Background(), options)
		if err != nil {
			t.Fatal(err)
		}
		op.BeginWrite()
		ds := core.DataSet{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, make([]byte, 128)), core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("AB"))}}
		_, err = op.ValidateDataSet(ds)
		op.EndWrite()
		if !errors.Is(err, ErrValidationLimit) {
			t.Fatalf("options=%#v err=%v", options, err)
		}
	}
}
func TestWriteAdmissionRegistrationAndPolicyLimits(t *testing.T) {
	chain, _ := NewHookChain(HookRegistration{Name: "one", Points: []HookPoint{HookPostWrite}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { return HookDecision{}, nil })}, HookRegistration{Name: "two", Points: []HookPoint{HookPostWrite}, Hook: HookFunc(func(context.Context, HookEvent) (HookDecision, error) { return HookDecision{}, nil })})
	if _, err := NewWriteOperation(context.Background(), Options{Hooks: chain, WriteLimits: WriteLimits{MaxHookRegistrations: 1}}); !errors.Is(err, ErrValidationLimit) {
		t.Fatal(err)
	}
	rules := []DataSetRuleRegistration{{Name: "one", Rule: DataSetRuleFunc(func(context.Context, DataSetContext) []Finding { return nil })}, {Name: "two", Rule: DataSetRuleFunc(func(context.Context, DataSetContext) []Finding { return nil })}}
	if _, err := NewWriteOperation(context.Background(), Options{DataSetRules: rules, WriteLimits: WriteLimits{MaxRuleRegistrations: 1}}); !errors.Is(err, ErrValidationLimit) {
		t.Fatal(err)
	}
	if _, err := NewWriteOperation(context.Background(), Options{WriteLimits: WriteLimits{MaxWorkBytes: -1}}); !errors.Is(err, ErrInvalidPolicy) {
		t.Fatal(err)
	}
	if _, err := writeProduct(math.MaxInt, unsafe.Sizeof(uint64(0))); !errors.Is(err, ErrValidationLimit) {
		t.Fatal(err)
	}
}
func TestWriteAdmissionCancellationAndScopeReset(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	op, _ := NewWriteOperation(ctx, Options{})
	cancel()
	if err := op.BeginWrite(); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	op, _ = NewWriteOperation(context.Background(), Options{})
	for i := 0; i < 2; i++ {
		if err := op.BeginWrite(); err != nil {
			t.Fatal(err)
		}
		if err := op.ValidateElement(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2})); err != nil {
			t.Fatal(err)
		}
		op.EndWrite()
		if op.Active() {
			t.Fatal("scope retained")
		}
	}
}

func TestWriteResolverExpectedVRAdmission(t *testing.T) {
	dict := &writeCountingDictionary{}
	op, _ := NewWriteOperation(context.Background(), Options{Dictionary: dict, ResolveVR: func(ElementContext, dictionary.Entry) []core.VR { return make([]core.VR, 1024) }, WriteLimits: WriteLimits{MaxWorkBytes: 4096}})
	op.BeginWrite()
	defer op.EndWrite()
	err := op.ValidateElement(core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("AB")))
	if !errors.Is(err, ErrValidationLimit) {
		t.Fatal(err)
	}
}
