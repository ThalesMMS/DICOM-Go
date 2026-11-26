package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

var coercionNameTag = core.NewTag(0x0010, 0x0010)

func coercionName(value string) object.AttributeCoercion {
	return object.MergeAttributes{Attributes: object.FromElements([]core.Element{{Header: core.ElementHeader{Tag: coercionNameTag, VR: core.VRPN}, Value: core.StringValue{value}}}, nil), Policy: object.UpdateOverwrite}
}

func TestStorageCoercionStatusesAndIdentity(t *testing.T) {
	tests := []struct {
		name          string
		hook          object.AttributeCoercion
		handlerStatus uint16
		handlerErr    error
		want          uint16
		called        bool
	}{
		{"changed", coercionName("Changed^Synthetic"), StatusSuccess, nil, StatusCStoreCoercionWarning, true},
		{"noop", object.CoercionFunc(func(*object.Object) ([]core.Element, error) { return nil, nil }), StatusSuccess, nil, StatusSuccess, true},
		{"handler failure", coercionName("Changed^Synthetic"), StatusCStoreOutOfResources, nil, StatusCStoreOutOfResources, true},
		{"handler error", coercionName("Changed^Synthetic"), StatusSuccess, errors.New("synthetic failure"), StatusCStoreCannotUnderstand, true},
		{"handler warning", coercionName("Changed^Synthetic"), 0xB006, nil, 0xB006, true},
		{"refusal", object.CoercionFunc(func(*object.Object) ([]core.Element, error) {
			return nil, NewCStoreSCPError(0xA701, "SECRET", errors.New("SECRET UID 1.2.3"))
		}), StatusSuccess, nil, 0xA701, false},
		{"panic", object.CoercionFunc(func(*object.Object) ([]core.Element, error) { panic("SECRET") }), StatusSuccess, nil, StatusCStoreCannotUnderstand, false},
		{"identity", object.RemapUIDs{UIDs: map[string]string{dicomtest.TestSOPInstanceUID: "1.2.3.987"}}, StatusSuccess, nil, StatusCStoreDataSetDoesNotMatch, false},
		{"layout", object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
			o.Put(core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{2, 0}))
			return []core.Element{{}}, nil
		}), StatusSuccess, nil, StatusCStoreCannotUnderstand, false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			called := false
			addr, done := startStorageSCP(t, ctx, StorageSCPOptions{AttributeCoercion: test.hook, StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
				called = true
				if test.name == "changed" {
					if got, _ := req.DataSet.GetString(coercionNameTag); got != "Changed^Synthetic" {
						t.Errorf("coerced name missing")
					}
				}
				return test.handlerStatus, test.handlerErr
			})})
			file := readIntegrationFixture(t)
			var before bytes.Buffer
			if err := object.WriteDataSet(&before, file.Dataset, file.TransferSyntax); err != nil {
				t.Fatal(err)
			}
			rsp := sendIntegrationCStore(t, ctx, addr, file, CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID, MessageID: 31})
			if err := <-done; err != nil {
				t.Fatal(err)
			}
			if rsp.Status != test.want || called != test.called {
				t.Fatalf("status=%04x called=%v want=%04x/%v", rsp.Status, called, test.want, test.called)
			}
			if !test.called && rsp.ErrorComment != "attribute coercion failed" {
				t.Fatalf("unsafe comment %q", rsp.ErrorComment)
			}
			var after bytes.Buffer
			_ = object.WriteDataSet(&after, file.Dataset, file.TransferSyntax)
			if !bytes.Equal(before.Bytes(), after.Bytes()) {
				t.Fatal("source mutated")
			}
		})
	}
}

func TestStoreCoercionFrozenRepresentationsAndLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := readIntegrationFixture(t)
	file.Dataset = object.FromElements(dicomtest.DatasetWithPixelData(), nil)
	var calls atomic.Int32
	var retained *object.Object
	hook := object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
		calls.Add(1)
		retained = o
		changed, err := coercionName("Frozen^Synthetic").Coerce(o)
		if err != nil {
			return nil, err
		}
		remapped, err := (object.RemapUIDs{UIDs: map[string]string{dicomtest.TestSOPInstanceUID: "1.2.3.987"}}).Coerce(o)
		return append(changed, remapped...), err
	})
	source, err := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: hook})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	d, err := source.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != int32(len(d.WritableTransferSyntaxUIDs)) || d.SOPInstanceUID != "1.2.3.987" {
		t.Fatalf("descriptor or callback count: %+v/%d", d, calls.Load())
	}
	retained.Put(core.Element{Header: core.ElementHeader{Tag: coercionNameTag, VR: core.VRPN}, Value: core.StringValue{"Later^Mutation"}})
	for _, uid := range d.WritableTransferSyntaxUIDs {
		ts, _ := transfer.DefaultRegistry.Get(uid)
		opened, err := source.OpenForTransferSyntax(ctx, ts)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := opened.WriteDataSet(ctx, &buf, ts); err != nil {
			t.Fatal(err)
		}
		o, err := object.ReadDataSet(bytes.NewReader(buf.Bytes()), ts)
		if err != nil {
			t.Fatal(err)
		}
		if name, _ := o.GetString(coercionNameTag); name != "Frozen^Synthetic" {
			t.Fatal("frozen payload changed")
		}
		if uid, _ := o.GetUID(core.NewTag(0x0008, 0x0018)); uid != d.SOPInstanceUID {
			t.Fatal("identity differs from descriptor")
		}
		if int64(buf.Len()) != d.TransferSyntaxSizes[ts.UID] {
			t.Fatal("wrong byte bound")
		}
		metadata, err := opened.ReadMetadata(ctx, index.Options{Profile: index.ProfilePatient | index.ProfileCore})
		if err != nil || metadata.Record.Instance.SOPInstanceUID != d.SOPInstanceUID || metadata.Record.Patient == nil || metadata.Record.Patient.Name != "Frozen^Synthetic" {
			t.Fatalf("wrong frozen observation: %v %+v", err, metadata.Record)
		}
		if err := opened.Close(); err != nil {
			t.Fatal(err)
		}
		if err := opened.WriteDataSet(ctx, io.Discard, ts); !errors.Is(err, ErrStoreInvalidSource) {
			t.Fatal("closed handle usable")
		}
	}
	wantCalls := calls.Load()
	addr, done := startStorageSCP(t, ctx, StorageSCPOptions{StoreHandler: CStoreHandlerFunc(func(_ context.Context, req CStoreRequestContext) (uint16, error) {
		if req.Request.AffectedSOPInstanceUID != "1.2.3.987" {
			t.Errorf("wrong command identity")
		}
		part10, err := coercionFile(req.DataSet, req.DataSetSyntax)
		if err != nil {
			return 0, err
		}
		var data bytes.Buffer
		if err := object.WriteFile(&data, part10); err != nil {
			return 0, err
		}
		parsed, err := object.ReadFile(bytes.NewReader(data.Bytes()))
		if err != nil {
			return 0, err
		}
		meta, _ := parsed.Meta.GetUID(core.NewTag(0x0002, 0x0003))
		instance, _ := parsed.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
		if meta != instance || instance != req.Request.AffectedSOPInstanceUID {
			t.Error("meta/dataset/command mismatch")
		}
		return StatusSuccess, nil
	})})
	session, err := NewStoreSession(addr, StoreSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(ctx, []StoreSource{source})
	if err != nil {
		t.Fatal(err)
	}
	if result.Succeeded != 1 {
		t.Fatalf("loopback result %+v", result)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != wantCalls {
		t.Fatal("callback reapplied during send")
	}
	if instance, _ := file.Dataset.GetUID(core.NewTag(0x0008, 0x0018)); instance != dicomtest.TestSOPInstanceUID {
		t.Fatal("source identity changed")
	}
}

func TestStoreCoercionFailureBeforeDialAndIndependentBuffers(t *testing.T) {
	file := readIntegrationFixture(t)
	file.Dataset = object.FromElements(dicomtest.DatasetWithPixelData(), nil)
	privateTag := core.NewTag(0x0011, 0x1010)
	sequenceTag := core.NewTag(0x0008, 0x1115)
	file.Dataset.Put(core.NewRawElement(privateTag, core.VROB, []byte{1, 2}))
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(privateTag, core.VROB, []byte{3, 4})}}}}})
	var before bytes.Buffer
	_ = object.WriteDataSet(&before, file.Dataset, file.TransferSyntax)
	secret := errors.New("SECRET 1.2.3")
	source, err := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
		raw, _ := o.GetRaw(privateTag)
		raw[0] = 9
		seq, _ := o.Get(sequenceTag)
		seq.Value.(core.SequenceValue).Items[0].Elements[0].Value.(core.RawValue)[0] = 8
		if pixel, _ := o.GetRaw(core.TagPixelData); len(pixel) > 0 {
			pixel[0] ^= 0xff
		}
		return nil, secret
	})})
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	session, err := NewStoreSession("127.0.0.1:1", StoreSessionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(context.Background(), []StoreSource{source})
	if err == nil || len(result.Items) != 1 || result.Items[0].Attempted || result.Items[0].Outcome != StoreOutcomeFailure || result.Associations != 0 {
		t.Fatalf("must refuse before dial: %+v %v", result, err)
	}
	_, err = source.Inspect(context.Background())
	if !errors.Is(err, secret) || err.Error() != ErrStoreCoercion.Error() {
		t.Fatal("lost cause or exposed diagnostic")
	}
	var after bytes.Buffer
	_ = object.WriteDataSet(&after, file.Dataset, file.TransferSyntax)
	if !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatal("borrowed raw/sequence/pixel mutated")
	}
}

func TestStoreCoercionProtectedAttributesAndLimits(t *testing.T) {
	for _, tag := range []core.Tag{core.NewTag(0x0028, 0x0008), core.NewTag(0x0028, 0x1201), core.NewTag(0x7FE0, 0x0001), core.NewTag(0x7FE0, 0x0008), core.NewTag(0x7FE0, 0x0009), core.NewTag(0x6000, 0x3000), core.NewTag(0x6002, 0x0010), core.TagPixelData} {
		t.Run(tag.String(), func(t *testing.T) {
			file := readIntegrationFixture(t)
			hook := object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
				o.Put(core.NewRawElement(tag, core.VROB, []byte{9, 8}))
				return []core.Element{{}}, nil
			})
			source, _ := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: hook})
			defer source.Close()
			if _, err := source.Inspect(context.Background()); err == nil {
				t.Fatal("pixel/index mutation accepted")
			}
		})
	}
	file := readIntegrationFixture(t)
	source, _ := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: coercionName("Bounded^Synthetic"), CloneOptions: object.CloneFileOptions{MaxTotalBytes: 1}})
	defer source.Close()
	if _, err := source.Inspect(context.Background()); err == nil {
		t.Fatal("limit accepted")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := source.Inspect(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error %v", err)
	}
	if status := storageCoercionErrorStatus(fmt.Errorf("wrapped: %w", ErrStoreCoercionIdentity)); status != StatusCStoreDataSetDoesNotMatch {
		t.Fatal("identity status")
	}
}

func TestStorageCoercionStreamAndPostHookBounds(t *testing.T) {
	hook := object.CoercionFunc(func(*object.Object) ([]core.Element, error) { return nil, nil })
	if err := ServeStorageAssociation(context.Background(), &ul.Association{}, StorageSCPOptions{StoreHandler: CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) { return 0, nil }), AttributeCoercion: hook, StreamSink: declineSink{}}); !errors.Is(err, ErrStoreInvalidOptions) {
		t.Fatal("stream hook not rejected")
	}
	if _, err := storageCoercionOptions(StorageSCPOptions{AttributeCoercion: hook, MaxSequenceDepth: -1}); !errors.Is(err, ErrStoreInvalidOptions) {
		t.Fatal("unbounded callback structure accepted")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	addr, done := startStorageSCP(t, ctx, StorageSCPOptions{MaxDataSetBytes: 1024, AttributeCoercion: coercionName(string(bytes.Repeat([]byte{'X'}, 2048))), StoreHandler: CStoreHandlerFunc(func(context.Context, CStoreRequestContext) (uint16, error) {
		t.Error("post-hook oversized dataset reached handler")
		return 0, nil
	})})
	file := readIntegrationFixture(t)
	rsp := sendIntegrationCStore(t, ctx, addr, file, CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID, MessageID: 32})
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rsp.Status != StatusCStoreOutOfResources || rsp.ErrorComment != "attribute coercion failed" {
		t.Fatalf("wrong post-hook bound refusal: %+v", rsp)
	}
}

func TestStoreCoercionPrivateDictionaryAndNondeterministicUIDs(t *testing.T) {
	catalog, err := dictionary.NewPrivateCatalog([]dictionary.PrivateEntry{{Creator: "SYNTHETIC", Group: 0x0011, Offset: 0x10, VR: core.VRUS, VM: "1"}})
	if err != nil {
		t.Fatal(err)
	}
	dict := &dictionary.Chain{catalog, std.Dictionary}
	file := readIntegrationFixture(t)
	file.TransferSyntax = transfer.ImplicitVRLittleEndian
	file.Meta = nil
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.NewTag(0x0011, 0x0010), VR: core.VRLO}, Value: core.StringValue{"SYNTHETIC"}})
	privateTag := core.NewTag(0x0011, 0x1010)
	file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: privateTag, VR: core.VRUS}, Value: core.Uint16Value{1234}})
	source, _ := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Dictionary: dict, Coercion: object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
		e, _ := o.Get(privateTag)
		if e.VR() != core.VRUS {
			t.Error("lost scoped private VR before hook")
		}
		return nil, nil
	})})
	defer source.Close()
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var buffer bytes.Buffer
	if err := opened.WriteDataSet(context.Background(), &buffer, transfer.ImplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	parsed, err := object.ReadDataSetWithOptions(bytes.NewReader(buffer.Bytes()), transfer.ImplicitVRLittleEndian, object.ReadFileOptions{Dictionary: dict})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := parsed.Get(privateTag)
	if e.VR() != core.VRUS {
		t.Fatal("lost private VR after hook")
	}
	var calls int
	divergent, _ := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
		calls++
		return (object.RemapUIDs{UIDs: map[string]string{dicomtest.TestSOPInstanceUID: fmt.Sprintf("1.2.3.%d", calls)}}).Coerce(o)
	})})
	defer divergent.Close()
	if _, err := divergent.Inspect(context.Background()); !errors.Is(err, ErrStoreCoercionIdentity) {
		t.Fatalf("divergent representations accepted: %v", err)
	}
}

func TestStoreCoercionNestedPixelsBOTAndCallbackCancellation(t *testing.T) {
	for _, kind := range []string{"nested", "BOT", "cancel"} {
		t.Run(kind, func(t *testing.T) {
			file := readIntegrationFixture(t)
			sequenceTag := core.NewTag(0x0008, 0x1115)
			file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: sequenceTag, VR: core.VRSQ}, Value: core.SequenceValue{Items: []core.DataSet{{Elements: []core.Element{core.NewRawElement(core.NewTag(0x0028, 0x0010), core.VRUS, []byte{2, 0})}}}}})
			if kind == "BOT" {
				file.Meta = nil
				file.TransferSyntax = transfer.RLELossless
				file.Dataset.Put(core.Element{Header: core.ElementHeader{Tag: core.TagPixelData, VR: core.VROB}, Value: core.FragmentSequence{OffsetTable: []byte{0, 0, 0, 0}, Fragments: [][]byte{{1, 2}}}})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			source, _ := NewCoercedStoreSource(NewFileStoreSource(file, 0), StoreCoercionOptions{Coercion: object.CoercionFunc(func(o *object.Object) ([]core.Element, error) {
				switch kind {
				case "nested":
					e, _ := o.Get(sequenceTag)
					e.Value.(core.SequenceValue).Items[0].Elements[0].Value.(core.RawValue)[0] = 3
				case "BOT":
					e, _ := o.Get(core.TagPixelData)
					e.Value.(core.FragmentSequence).OffsetTable[0] = 1
				case "cancel":
					cancel()
				}
				return []core.Element{{}}, nil
			})})
			defer source.Close()
			_, err := source.Inspect(ctx)
			if kind == "cancel" {
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("lost cancellation %v", err)
				}
			} else if !errors.Is(err, ErrStoreCoercionPixels) {
				t.Fatalf("lost pixel guard %v", err)
			}
		})
	}
}

func TestStoreCoercionBudgetLifetimeRollbackAndConcurrency(t *testing.T) {
	budget, err := NewStoreCoercionBudget(4096)
	if err != nil {
		t.Fatal(err)
	}
	newSource := func(hook object.AttributeCoercion) *CoercedStoreSource {
		source, err := NewCoercedStoreSource(NewFileStoreSource(readIntegrationFixture(t), 0), StoreCoercionOptions{Coercion: hook, Budget: budget, CloneOptions: object.CloneFileOptions{MaxTotalBytes: 2048}})
		if err != nil {
			t.Fatal(err)
		}
		return source
	}
	failing := newSource(object.CoercionFunc(func(*object.Object) ([]core.Element, error) { return nil, errors.New("SECRET") }))
	defer failing.Close()
	if _, err := failing.Inspect(context.Background()); err == nil {
		t.Fatal("expected refusal")
	}
	if budget.used != 0 {
		t.Fatal("failed reservation retained")
	}
	source := newSource(coercionName("Budget^Synthetic"))
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	charged := budget.used
	if charged <= 0 || charged >= 2048 {
		t.Fatalf("wrong committed budget %d", charged)
	}
	if _, err := source.Inspect(context.Background()); err != nil || budget.used != charged {
		t.Fatal("Inspect double charged")
	}
	if err := source.Close(); err != nil {
		t.Fatal(err)
	}
	_ = source.Close()
	if budget.used != charged {
		t.Fatal("opened payload lifetime not charged")
	}
	if err := opened.WriteDataSet(context.Background(), io.Discard, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal("open handle invalidated by source Close")
	}
	_ = opened.Close()
	_ = opened.Close()
	if budget.used != 0 {
		t.Fatal("handle close did not release once")
	}
	if _, err := source.Open(context.Background()); !errors.Is(err, ErrStoreInvalidSource) {
		t.Fatal("closed source reopened")
	}
	// Independent preparations reserve before buffering; sufficient room for
	// both reservations makes their commit/release safe under contention.
	first, second := newSource(coercionName("One^Synthetic")), newSource(coercionName("Two^Synthetic"))
	var wg sync.WaitGroup
	for _, s := range []*CoercedStoreSource{first, second} {
		wg.Add(1)
		go func(s *CoercedStoreSource) {
			defer wg.Done()
			if _, err := s.Inspect(context.Background()); err != nil {
				t.Error(err)
			}
		}(s)
	}
	wg.Wait()
	if budget.used <= 0 || budget.used > 4096 {
		t.Fatal("invalid aggregate charge")
	}
	_ = first.Close()
	_ = second.Close()
	if budget.used != 0 {
		t.Fatal("aggregate release failed")
	}
	if _, err := NewStoreCoercionBudget(0); !errors.Is(err, ErrStoreInvalidOptions) {
		t.Fatal("unbounded budget accepted")
	}
}
