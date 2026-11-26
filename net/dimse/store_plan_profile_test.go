package dimse

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func fixedStoreProfile() []ul.PresentationContext {
	return []ul.PresentationContext{
		{ID: 1, AbstractSyntaxUID: "1.2.3", TransferSyntaxUIDs: []string{transfer.RLELossless.UID}},
		{ID: 3, AbstractSyntaxUID: "1.2.3", TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}},
		{ID: 5, AbstractSyntaxUID: "1.2.4", TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}},
	}
}

func TestPlanStoreBatchFixedProfileExactWireAndAdmission(t *testing.T) {
	template := fixedStoreProfile()
	sources := []StoreSource{
		descriptorStoreSource{descriptor: testStoreDescriptor("1.2.3", "1.2.3.1", transfer.ExplicitVRLittleEndian.UID)},
		descriptorStoreSource{descriptor: testStoreDescriptor("1.2.3", "1.2.3.2", transfer.RLELossless.UID)},
		descriptorStoreSource{descriptor: testStoreDescriptor("1.2.9", "1.2.3.3", transfer.ExplicitVRLittleEndian.UID)},
		descriptorStoreSource{descriptor: testStoreDescriptor("1.2.3", "1.2.3.4", transfer.JPEGBaseline.UID)},
	}
	plan, err := PlanStoreBatch(context.Background(), sources, StorePlanOptions{PresentationContexts: template})
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Associations) != 1 || !reflect.DeepEqual(plan.Associations[0].Contexts, template) || len(plan.Associations[0].Items) != 2 || plan.Associations[0].Items[0].PresentationContextID != 3 || plan.Associations[0].Items[1].PresentationContextID != 1 {
		t.Fatalf("fixed plan %#v", plan)
	}
	if len(plan.Failures) != 2 || plan.Failures[0].SourceIndex != 2 || plan.Failures[1].SourceIndex != 3 || !errors.Is(plan.Failures[0].Err, ErrStorePresentationContextRejected) {
		t.Fatalf("admission %#v", plan.Failures)
	}
	template[0].TransferSyntaxUIDs[0] = "mutated"
	template[1].ID = 7
	if plan.Associations[0].Contexts[0].TransferSyntaxUIDs[0] != transfer.RLELossless.UID || plan.Associations[0].Contexts[1].ID != 3 {
		t.Fatal("plan retains template aliases")
	}
	legacy, err := PlanStoreBatch(context.Background(), sources[:2], StorePlanOptions{})
	if err != nil || len(legacy.Associations[0].Contexts) != 2 || legacy.Associations[0].Contexts[0].ID != 1 {
		t.Fatal("nil legacy", err)
	}
}

func TestPlanStoreProfileBoundsAndRepresentations(t *testing.T) {
	bad := [][]ul.PresentationContext{make([]ul.PresentationContext, 0), make([]ul.PresentationContext, 129), {{ID: 2, AbstractSyntaxUID: "1.2.3", TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}, {{ID: 1, AbstractSyntaxUID: "1.02.3", TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}}
	template := fixedStoreProfile()
	template[1].ID = 1
	bad = append(bad, template)
	template = fixedStoreProfile()
	template[0].TransferSyntaxUIDs = make([]string, 257)
	bad = append(bad, template)
	template = fixedStoreProfile()
	template[0].TransferSyntaxUIDs = []string{transfer.RLELossless.UID, transfer.RLELossless.UID}
	bad = append(bad, template)
	for i, contexts := range bad {
		if _, err := PlanStoreBatch(context.Background(), nil, StorePlanOptions{PresentationContexts: contexts}); !errors.Is(err, ErrStoreInvalidOptions) {
			t.Fatal(i, err)
		}
		if _, err := NewStoreSession("localhost:1", StoreSessionOptions{PlanOptions: StorePlanOptions{PresentationContexts: contexts}}); !errors.Is(err, ErrStoreInvalidOptions) {
			t.Fatal("session", i, err)
		}
	}
	d := testStoreDescriptor("1.2.3", "1.2.3.1", transfer.JPEGBaseline.UID)
	d.Size = 10
	d.WritableTransferSyntaxUIDs = []string{transfer.JPEGBaseline.UID, transfer.RLELossless.UID}
	d.TransferSyntaxSizes = map[string]int64{transfer.JPEGBaseline.UID: 10, transfer.RLELossless.UID: 20}
	plan, err := PlanStoreBatch(context.Background(), []StoreSource{descriptorStoreSource{descriptor: d}}, StorePlanOptions{PresentationContexts: fixedStoreProfile()})
	if err != nil || len(plan.Failures) != 0 || plan.Associations[0].Items[0].PresentationContextID != 1 || !reflect.DeepEqual(plan.Associations[0].Items[0].Descriptor.WritableTransferSyntaxUIDs, d.WritableTransferSyntaxUIDs) {
		t.Fatal("producible representation admitted without changing descriptor", err)
	}
}
