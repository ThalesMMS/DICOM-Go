package parser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/transfer"
	"github.com/ThalesMMS/dicom-go/validation"
)

func TestDefinedGroupLengthRejectsStaleHookChangesBeforeCommit(t *testing.T) {
	first, second := core.NewTag(0x0010, 0x0010), core.NewTag(0x0010, 0x0020)
	for _, policy := range []GroupLengthPolicy{GroupLengthCreate, GroupLengthRecalculate} {
		for _, change := range []string{"replace", "filter", "both", "same_length", "observe"} {
			t.Run(string(rune('0'+policy))+"/"+change, func(t *testing.T) {
				var out bytes.Buffer
				pre := map[core.Tag]int{}
				post := 0
				chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "prepare", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, event validation.HookEvent) (validation.HookDecision, error) {
					tag := event.Element.Tag()
					pre[tag]++
					if tag == first && (change == "replace" || change == "both" || change == "same_length") {
						payload := []byte("ZZ")
						if change == "same_length" {
							payload = []byte("UVWXYZ")
						}
						replacement := core.NewRawElement(first, core.VRPN, payload)
						return validation.HookDecision{Element: &replacement}, nil
					}
					if tag == second && (change == "filter" || change == "both") {
						return validation.HookDecision{Filter: true}, nil
					}
					return validation.HookDecision{}, nil
				})}, validation.HookRegistration{Name: "post", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
					post++
					return validation.HookDecision{}, nil
				})})
				elements := []core.Element{core.NewRawElement(first, core.VRPN, []byte("ABCDEF")), core.NewRawElement(second, core.VRLO, []byte("WXYZ"))}
				if policy == GroupLengthRecalculate {
					elements = append([]core.Element{{Header: core.ElementHeader{Tag: core.NewTag(0x0010, 0), VR: core.VRUL}, Value: core.Uint32Value{26}}}, elements...)
				}
				w, err := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, GroupLength: policy}, validation.Options{Hooks: chain})
				if err != nil {
					t.Fatal(err)
				}
				err = w.WriteElement(streamSQ(elements...))
				reject := change == "replace" || change == "filter" || change == "both"
				if reject {
					if !errors.Is(err, ErrGroupLengthAfterHook) || out.Len() != 0 {
						t.Fatalf("error=%v bytes=%d", err, out.Len())
					}
					if post != 1 {
						t.Fatalf("only root failure post expected, got%d", post)
					}
				} else if err != nil {
					t.Fatal(err)
				}
				for tag, n := range pre {
					if n != 1 {
						t.Fatalf("%s pre count%d", tag, n)
					}
				}
			})
		}
	}
}

func TestDefinedHooksFreezeNondeterministicTransformsAndPostCommit(t *testing.T) {
	var out bytes.Buffer
	tag := core.NewTag(0x0010, 0x0020)
	calls := 0
	var post []validation.HookEvent
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "once", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if e.Element.Tag() != tag {
			return validation.HookDecision{}, nil
		}
		calls++
		value := core.NewRawElement(tag, core.VRLO, []byte{'A' + byte(calls-1), 'X'})
		return validation.HookDecision{Element: &value}, nil
	})}, validation.HookRegistration{Name: "commit", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if out.Len() != 30 {
			t.Errorf("post before full parent commit: %d", out.Len())
		}
		post = append(post, e)
		return validation.HookDecision{}, nil
	})})
	w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Hooks: chain})
	if err := w.WriteElement(streamSQ(core.NewRawElement(tag, core.VRLO, []byte("OLDVAL")))); err != nil {
		t.Fatal(err)
	}
	if calls != 1 || !bytes.HasSuffix(out.Bytes(), []byte("AX")) || len(post) != 2 || post[0].Offset != 20 || post[0].BytesWritten != 10 || post[1].BytesWritten != 30 {
		t.Fatalf("calls%d posts%+v", calls, post)
	}
}

func TestDefinedLateDestinationFailureSuppressesChildPosts(t *testing.T) {
	for _, limit := range []int{6, 26, 38} {
		t.Run(string(rune('a'+limit)), func(t *testing.T) {
			cause := errors.New("synthetic destination")
			dest := &failAfterWriter{limit: limit, err: cause}
			var post []validation.HookEvent
			pre := 0
			chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "pre", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
				pre++
				return validation.HookDecision{}, nil
			})}, validation.HookRegistration{Name: "post", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
				post = append(post, e)
				return validation.HookDecision{}, nil
			})})
			w, _ := NewWriterWithValidation(context.Background(), dest, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Hooks: chain})
			el := streamSQ(core.NewRawElement(core.NewTag(0x0010, 0x0010), core.VRPN, []byte("ABCDEF")), core.NewRawElement(core.NewTag(0x0010, 0x0020), core.VRLO, []byte("WXYZ")))
			err := w.WriteElement(el)
			var writeErr *ValidationWriteError
			if !errors.Is(err, cause) || !errors.As(err, &writeErr) || writeErr.BytesWritten != int64(limit) || writeErr.Complete || pre != 3 || len(post) != 1 || post[0].WriteComplete || post[0].BytesWritten != int64(limit) {
				t.Fatalf("err%v pre%d post%+v bytes%d", err, pre, post, w.BytesWritten())
			}
		})
	}
}

func TestDefinedSourceFailureOccursAfterPreparationAndCountsActualBytes(t *testing.T) {
	var out bytes.Buffer
	cause := errors.New("synthetic source")
	first := core.NewTag(0x0011, 0x1010)
	second := core.NewTag(0x0011, 0x1020)
	preparedSecond := false
	childPosts := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "pre", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if e.Element.Tag() == second {
			preparedSecond = true
		}
		return validation.HookDecision{}, nil
	})}, validation.HookRegistration{Name: "post", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if e.Header.Tag == first || e.Header.Tag == second {
			childPosts++
		}
		return validation.HookDecision{}, nil
	})})
	opts := WriterOptions{LengthPolicy: LengthPolicyDefined, DeferredValue: func(_ core.DataSet, el core.Element) (func(io.Writer) (int64, error), bool) {
		if el.Tag() != first {
			return nil, false
		}
		return func(dst io.Writer) (int64, error) {
			if !preparedSecond {
				t.Error("source read before complete preparation")
			}
			n, _ := dst.Write([]byte{1, 2})
			return int64(n), cause
		}, true
	}}
	w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, opts, validation.Options{Hooks: chain})
	el := streamSQ(core.Element{Header: core.ElementHeader{Tag: first, VR: core.VROB, Length: 4, LengthSet: true}}, core.NewRawElement(second, core.VROB, []byte{3, 4}))
	err := w.WriteElement(el)
	if !errors.Is(err, cause) || out.Len() != 34 || w.BytesWritten() != 34 || childPosts != 0 {
		t.Fatalf("error%v bytes%d post%d", err, out.Len(), childPosts)
	}
}

func TestValidationWriterNoPreConsumerDoesNotUseRetainedPayloadBudget(t *testing.T) {
	for _, kind := range []string{"nil", "empty", "post_only", "active"} {
		t.Run(kind, func(t *testing.T) {
			var hooks *validation.HookChain
			post := 0
			if kind != "nil" {
				hooks, _ = validation.NewHookChain()
			}
			if kind == "post_only" {
				hooks.Add(validation.HookRegistration{Name: "post", Points: []validation.HookPoint{validation.HookPostWrite}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
					post++
					return validation.HookDecision{}, nil
				})})
			}
			if kind == "active" {
				hooks.Add(validation.HookRegistration{Name: "pre", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
					return validation.HookDecision{}, nil
				})})
			}
			var out bytes.Buffer
			w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, SequenceLimits: SequenceWriteLimits{MaxRetainedValueBytes: 2}}, validation.Options{Hooks: hooks})
			err := w.WriteElement(streamSQ(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4})))
			if kind == "active" {
				if !errors.Is(err, ErrWritePlanLimit) || out.Len() != 0 {
					t.Fatalf("%v bytes%d", err, out.Len())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if kind == "post_only" && post != 2 {
				t.Fatalf("posts%d", post)
			}
		})
	}
}

func TestActiveSerializationHookIsolationPreserved(t *testing.T) {
	input := core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2})
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "mutate", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if raw, ok := e.Element.Value.(core.RawValue); ok {
			raw[0] = 99
		}
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Hooks: chain})
	if err := w.WriteElement(streamSQ(input)); err != nil {
		t.Fatal(err)
	}
	if input.Value.(core.RawValue)[0] != 1 {
		t.Fatal("hook mutated caller payload")
	}
}

func TestActiveHooksAdmitCloneStructureBeforeCallback(t *testing.T) {
	calls := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "bounded", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		calls++
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, SequenceLimits: SequenceWriteLimits{MaxPlanBytes: 1}}, validation.Options{Hooks: chain})
	err := w.WriteElement(streamSQ())
	if !errors.Is(err, ErrWritePlanLimit) || calls != 0 || out.Len() != 0 {
		t.Fatalf("err=%v hooks=%d bytes=%d", err, calls, out.Len())
	}
}

func TestDefinedPreparationCancellationLeavesAncestorUncommitted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	first, second := core.NewTag(0x0010, 0x0010), core.NewTag(0x0010, 0x0020)
	secondCalls := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "cancel", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
		if e.Element.Tag() == first {
			cancel()
		}
		if e.Element.Tag() == second {
			secondCalls++
		}
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	w, _ := NewWriterWithValidation(ctx, &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined}, validation.Options{Hooks: chain})
	err := w.WriteElement(streamSQ(core.NewRawElement(first, core.VRPN, []byte("XX")), core.NewRawElement(second, core.VRLO, []byte("YY"))))
	if !errors.Is(err, context.Canceled) || out.Len() != 0 || secondCalls != 0 {
		t.Fatalf("error=%v bytes=%d later hook=%d", err, out.Len(), secondCalls)
	}
}

func TestActiveHookCloneQuotaIncludesRootAndDescendants(t *testing.T) {
	for _, c := range []struct {
		name                 string
		quota                int64
		rootCalls, leafCalls int
	}{
		{"three root clones before hook", 11, 0, 0},
		{"root and descendant coexist", 17, 1, 0},
		{"exact conservative admission", 24, 1, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			roots, leaves := 0, 0
			tag := core.NewTag(0x0011, 0x1010)
			chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
				if e.Element.Tag() == tag {
					leaves++
				} else {
					roots++
				}
				return validation.HookDecision{}, nil
			})})
			var out bytes.Buffer
			w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, SequenceLimits: SequenceWriteLimits{MaxRetainedValueBytes: c.quota}}, validation.Options{Hooks: chain})
			err := w.WriteElement(streamSQ(core.NewRawElement(tag, core.VROB, []byte{1, 2, 3, 4})))
			if c.quota < 24 {
				if !errors.Is(err, ErrWritePlanLimit) || out.Len() != 0 {
					t.Errorf("expected refusal: err=%v bytes=%d", err, out.Len())
				}
			} else if err != nil {
				t.Errorf("exact quota refused: %v", err)
			}
			if roots != c.rootCalls || leaves != c.leafCalls {
				t.Errorf("observed roots=%d leaves=%d; want %d/%d", roots, leaves, c.rootCalls, c.leafCalls)
			}
		})
	}
}

func TestCP246BEKnownCloneQuotaBeforeHeader(t *testing.T) {
	un := streamSQ(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROW, []byte{0, 1, 0, 2}))
	un.Header.VR = core.VRUN
	plain := NewWriter(io.Discard, transfer.ExplicitVRBigEndian)
	_, structure, err := plain.hookValueFootprint(un)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name   string
		limits SequenceWriteLimits
	}{
		{"normalization payload beyond root clones", SequenceWriteLimits{MaxRetainedValueBytes: 12}},
		{"normalization structure beyond root clones", SequenceWriteLimits{MaxPlanBytes: 3 * structure}},
	} {
		t.Run(c.name, func(t *testing.T) {
			roots, leaves := 0, 0
			chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(_ context.Context, e validation.HookEvent) (validation.HookDecision, error) {
				if e.Element.VR() == core.VRUN {
					roots++
				} else {
					leaves++
				}
				return validation.HookDecision{}, nil
			})})
			var out bytes.Buffer
			w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRBigEndian, WriterOptions{LengthPolicy: LengthPolicyDefined, SequenceLimits: c.limits}, validation.Options{Hooks: chain})
			err := w.WriteElement(un)
			if !errors.Is(err, ErrWritePlanLimit) || roots != 1 || leaves != 0 || out.Len() != 0 {
				t.Fatalf("error=%v root=%d leaf=%d bytes=%d", err, roots, leaves, out.Len())
			}
		})
	}
}

func TestActiveHookThirdKnownCloneAdmittedBeforeCallback(t *testing.T) {
	calls := 0
	chain, _ := validation.NewHookChain(validation.HookRegistration{Name: "observe", Points: []validation.HookPoint{validation.HookPreSerialization}, Hook: validation.HookFunc(func(context.Context, validation.HookEvent) (validation.HookDecision, error) {
		calls++
		return validation.HookDecision{}, nil
	})})
	var out bytes.Buffer
	w, _ := NewWriterWithValidation(context.Background(), &out, transfer.ExplicitVRLittleEndian, WriterOptions{SequenceLimits: SequenceWriteLimits{MaxRetainedValueBytes: 8}}, validation.Options{Hooks: chain})
	err := w.WriteElement(core.NewRawElement(core.NewTag(0x0011, 0x1010), core.VROB, []byte{1, 2, 3, 4}))
	if !errors.Is(err, ErrWritePlanLimit) || calls != 0 || out.Len() != 0 {
		t.Fatalf("error=%v calls=%d bytes=%d", err, calls, out.Len())
	}
}
