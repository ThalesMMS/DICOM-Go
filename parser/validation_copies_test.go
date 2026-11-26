package parser

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestWriteDataSetInvalidatesUIDProofAfterSerializationReplacement(t *testing.T) {
	tag := core.NewTag(0x0008, 0x0018)
	chain, err := validation.NewHookChain(validation.HookRegistration{Name: "replace-uid", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
		if event.Element.Tag() != tag {
			return validation.HookDecision{}, nil
		}
		replacement := core.NewRawElement(tag, core.VRUI, []byte("1..2"))
		return validation.HookDecision{Element: &replacement}, nil
	})})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	writer, err := NewWriterWithValidation(context.Background(), &output, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Mode: validation.ModeStrict, Hooks: chain})
	if err != nil {
		t.Fatal(err)
	}
	err = writer.WriteDataSet(core.DataSet{Elements: []core.Element{core.NewRawElement(tag, core.VRUI, []byte("1.2.30"))}})
	if !errors.Is(err, validation.ErrValidationFailed) || output.Len() != 0 {
		t.Fatalf("stale UID proof: error=%v committed=%d formatFindings=%d", err, output.Len(), writer.ValidationReport().Count(validation.CodeValueFormat))
	}
}

func TestWriteDataSetNestedAndSameLengthProofInvalidation(t *testing.T) {
	uid := core.NewTag(0x0008, 0x0018)
	sequenceTag := core.NewTag(0x0040, 0xa730)
	for _, policy := range []LengthPolicy{LengthPolicyDefined, LengthPolicyUndefined} {
		for _, mode := range []validation.Mode{validation.ModeStrict, validation.ModePreserve, validation.ModeWarn} {
			calls := 0
			chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "replace", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
				calls++
				if e.Element.Tag() == uid {
					replacement := core.NewRawElement(uid, core.VRUI, []byte("1..2"))
					return validation.HookDecision{Element: &replacement}, nil
				}
				return validation.HookDecision{}, nil
			})})
			original := core.NewRawElement(uid, core.VRUI, []byte("1.20"))
			root := core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{original}}}}}
			var output bytes.Buffer
			writer, _ := NewWriterWithValidation(context.Background(), &output, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: policy}, validation.Options{Mode: mode, Hooks: chain})
			err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{root}})
			if mode == validation.ModeStrict {
				if !errors.Is(err, validation.ErrValidationFailed) {
					t.Fatalf("policy=%v err=%v", policy, err)
				}
				if policy == LengthPolicyDefined && output.Len() != 0 {
					t.Fatal("defined ancestor emitted before child rejection")
				}
			} else if err != nil || !bytes.Contains(output.Bytes(), []byte("1..2")) {
				t.Fatalf("mode=%v err=%v", mode, err)
			}
			if calls != 2 || writer.ValidationReport().Count(validation.CodeValueFormat) != 1 || string(original.Value.(core.RawValue)) != "1.20" {
				t.Fatalf("calls=%d report=%#v input=%s", calls, writer.ValidationReport(), original.Value)
			}
		}
	}
}
func TestWriteBorrowedInputAndObserversRemainIsolated(t *testing.T) {
	tag := core.NewTag(0x0011, 0x1010)
	data := []byte{1, 2, 3, 4}
	leaf := core.NewRawElement(tag, core.VROB, data)
	root := core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0040, 0xa730), VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{leaf}}}}}
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "mutating-observer", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		switch v := e.Element.Value.(type) {
		case core.RawValue:
			v[0] = 99
		case core.SequenceValue:
			v.Items[0].Elements[0].Value.(core.RawValue)[0] = 98
		}
		return validation.HookDecision{}, nil
	})})
	var actual, expected bytes.Buffer
	NewWriterWithOptions(&expected, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}).WriteElement(root)
	writer, _ := NewWriterWithValidation(context.Background(), &actual, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Hooks: chain})
	if err := writer.WriteElement(root); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual.Bytes(), expected.Bytes()) || data[0] != 1 {
		t.Fatal("observer mutated borrowed source or bytes")
	}
}
func TestWriteProofContextChangesAreRefusedWithoutRuleReplay(t *testing.T) {
	tag := core.NewTag(0x0010, 0x0020)
	calls := 0
	rule := validation.DataSetRuleRegistration{Name: "context", Rule: validation.DataSetRuleFunc(func(context.Context, validation.DataSetContext) []validation.Finding { calls++; return nil })}
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "replace", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		replacement := core.NewRawElement(e.Element.Tag(), core.VRLO, []byte("CD"))
		return validation.HookDecision{Element: &replacement}, nil
	})})
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{DataSetRules: []validation.DataSetRuleRegistration{rule}, Hooks: chain})
	err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{core.NewRawElement(tag, core.VRLO, []byte("AB"))}})
	if !errors.Is(err, validation.ErrWriteValidationScope) || out.Len() != 0 || calls != 1 {
		t.Fatalf("err=%v bytes=%d calls=%d", err, out.Len(), calls)
	}
}
func TestWriteProofRetagFilterRequiredUIDAndDuplicateOrdinals(t *testing.T) {
	uid := core.NewTag(0x0008, 0x0018)
	other := core.NewTag(0x0008, 0x0016)
	for _, filter := range []bool{false, true} {
		chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "change", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
			if e.Element.Tag() != uid {
				return validation.HookDecision{}, nil
			}
			if filter {
				return validation.HookDecision{Filter: true}, nil
			}
			replacement := *e.Element
			replacement.Header.Tag = other
			return validation.HookDecision{Element: &replacement}, nil
		})})
		var out bytes.Buffer
		writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Mode: validation.ModeStrict, Hooks: chain, RequiredUIDs: []core.Tag{uid}})
		err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{core.NewRawElement(other, core.VRUI, []byte("1.20")), core.NewRawElement(uid, core.VRUI, []byte("1.30"))}})
		if !errors.Is(err, validation.ErrValidationFailed) || writer.ValidationReport().Count(validation.CodeRequiredUID) != 1 || out.Len() != 12 {
			t.Fatalf("filter=%v err=%v report=%#v bytes=%d", filter, err, writer.ValidationReport(), out.Len())
		}
		if !filter && writer.ValidationReport().Count(validation.CodeDuplicateElement) != 1 {
			t.Fatal("retag duplicate not detected")
		}
	}
}
func TestWriteRetroactiveCharacterSetProofIsRefused(t *testing.T) {
	charset := core.NewTag(0x0008, 0x0005)
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "charset", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if e.Element.Tag() != charset {
			return validation.HookDecision{}, nil
		}
		el := core.NewRawElement(charset, core.VRCS, []byte("ISO_IR 192"))
		return validation.HookDecision{Element: &el}, nil
	})})
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Hooks: chain})
	err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{core.NewRawElement(charset, core.VRCS, []byte("ISO_IR 100")), core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte{0xe9, ' '})}})
	if !errors.Is(err, validation.ErrWriteValidationScope) || out.Len() != 0 {
		t.Fatalf("err=%v bytes=%d", err, out.Len())
	}
}
func TestWriteAdmissionRejectsBeforeSourceAndCallbacks(t *testing.T) {
	calls := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		calls++
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Hooks: chain, WriteLimits: validation.WriteLimits{MaxCopiedValueBytes: 11}})
	err := writer.WriteElement(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4}))
	if !errors.Is(err, validation.ErrValidationLimit) || calls != 0 || out.Len() != 0 {
		t.Fatalf("err=%v calls=%d bytes=%d", err, calls, out.Len())
	}
	provider := 0
	writer, _ = NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{WriteLimits: validation.WriteLimits{MaxWorkBytes: 1}})
	err = writer.WriteDeferredElement(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB, Length: 4, LengthSet: true}}, func(io.Writer) (int64, error) { provider++; return 4, nil })
	if !errors.Is(err, validation.ErrValidationLimit) || provider != 0 || out.Len() != 0 {
		t.Fatalf("err=%v provider=%d bytes=%d", err, provider, out.Len())
	}
}
func TestWriteScopeDoesNotReuseProofAcrossCalls(t *testing.T) {
	tag := core.NewTag(0x0008, 0x0018)
	value := core.RawValue([]byte("1.20"))
	el := core.NewRawElement(tag, core.VRUI, value)
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Mode: validation.ModeStrict})
	if err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{el}}); err != nil {
		t.Fatal(err)
	}
	el.Value.(core.RawValue)[1] = '.'
	el.Value.(core.RawValue)[2] = '.'
	before := out.Len()
	err := writer.WriteDataSet(core.DataSet{Elements: []core.Element{el}})
	if !errors.Is(err, validation.ErrValidationFailed) || out.Len() != before {
		t.Fatalf("err=%v bytes=%d before=%d", err, out.Len(), before)
	}
}

// BEGIN COMPATIBILITY ORACLE
func TestValidationCopyCompatibilityOracle(t *testing.T) {
	path := os.Getenv("DICOM_VALIDATION_COMPAT_OUTPUT")
	if path == "" {
		t.Skip("opt-in published-base compatibility oracle")
	}
	var records []map[string]any
	for _, datasetMode := range []bool{false, true} {
		for _, mode := range []validation.Mode{validation.ModePreserve, validation.ModeWarn, validation.ModeStrict} {
			for _, stop := range []bool{false, true} {
				for _, bound := range []int{2, 256} {
					for _, active := range []bool{false, true} {
						uid := core.NewTag(0x0008, 0x0018)
						seqTag := core.NewTag(0x0040, 0xa730)
						elements := []core.Element{core.NewRawElement(uid, core.VRUI, []byte("1..2")), core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("AB")), core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("CD"))}
						root := core.Element{Header: core.ElementHeader{Tag: seqTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: elements}, {Elements: []core.Element{core.NewRawElement(uid, core.VRUI, []byte("1..2"))}}}}}
						events := []string{}
						var chain *validation.HookChain
						if active {
							chain, _ = validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreValidation, validation.HookDataSetComplete, validation.HookPostValidation, validation.HookPreSerialization, validation.HookPostWrite}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
								events = append(events, fmt.Sprintf("%s:%s:%d:%d:%t", event.Point, event.Path.String(), event.Offset, event.BytesWritten, event.WriteComplete))
								return validation.HookDecision{}, nil
							})})
						}
						var out bytes.Buffer
						writer, err := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Mode: mode, StopFirst: stop, MaxFindings: bound, Hooks: chain, RequiredUIDs: []core.Tag{uid}})
						if err != nil {
							t.Fatal(err)
						}
						if datasetMode {
							err = writer.WriteDataSet(core.DataSet{Elements: []core.Element{root}})
						} else {
							err = writer.WriteElement(root)
						}
						record := map[string]any{"dataset": datasetMode, "mode": mode, "stop": stop, "bound": bound, "active": active, "bytes": hex.EncodeToString(out.Bytes()), "committed": writer.BytesWritten(), "report": fmt.Sprintf("%#v", writer.ValidationReport()), "events": events, "failed": errors.Is(err, validation.ErrValidationFailed)}
						var outcome *ValidationWriteError
						if errors.As(err, &outcome) {
							record["errorBytes"] = outcome.BytesWritten
							record["complete"] = outcome.Complete
						}
						if err != nil {
							record["error"] = err.Error()
						}
						records = append(records, record)
					}
				}
			}
		}
	}
	data, err := json.MarshalIndent(records, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(path, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
}

// END COMPATIBILITY ORACLE

func TestWriteAdmissionAfterCommitReportsCompletion(t *testing.T) {
	calls := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "post", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		calls++
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Hooks: chain, WriteLimits: validation.WriteLimits{MaxScopeNodes: 4}})
	err := writer.WriteElement(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4}))
	var outcome *ValidationWriteError
	if !errors.Is(err, validation.ErrValidationLimit) || !errors.As(err, &outcome) || !outcome.Complete || outcome.BytesWritten != 16 || out.Len() != 16 || calls != 0 {
		t.Fatalf("err=%v outcome=%#v bytes=%d calls=%d", err, outcome, out.Len(), calls)
	}
}

func TestWriteScopeKeepsHistoricalFindingsWithoutRejectingNewValidElement(t *testing.T) {
	tag := core.NewTag(0x0008, 0x0018)
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "explicit-valid", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if string(e.Element.Value.(core.RawValue)) == "1.20" {
			copy := *e.Element
			return validation.HookDecision{Element: &copy}, nil
		}
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	writer, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{}, validation.Options{Mode: validation.ModeStrict, Hooks: chain})
	if err := writer.WriteElement(core.NewRawElement(tag, core.VRUI, []byte("1..2"))); !errors.Is(err, validation.ErrValidationFailed) {
		t.Fatal(err)
	}
	if err := writer.WriteElement(core.NewRawElement(tag, core.VRUI, []byte("1.20"))); err != nil {
		t.Fatal(err)
	}
	if out.Len() != 12 || writer.ValidationReport().Count(validation.CodeValueFormat) != 1 {
		t.Fatalf("bytes=%d report=%#v", out.Len(), writer.ValidationReport())
	}
}
