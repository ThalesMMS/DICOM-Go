package dimse

import (
	"context"
	"errors"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type profileInspectionCounter struct{ inspected bool }

func (s *profileInspectionCounter) Inspect(context.Context) (StoreDescriptor, error) {
	s.inspected = true
	return StoreDescriptor{}, errors.New("must not inspect")
}
func (*profileInspectionCounter) Open(context.Context) (OpenedStoreSource, error) {
	return OpenedStoreSource{}, errors.New("must not open")
}

func TestStoreSessionFixedProfileRejectsNoncanonicalIDsBeforeInspection(t *testing.T) {
	contexts := fixedStoreProfile()
	contexts[0].ID = 31
	contexts[1].ID = 61
	contexts[2].ID = 103
	source := &profileInspectionCounter{}
	if _, err := PlanStoreBatch(context.Background(), []StoreSource{source}, StorePlanOptions{PresentationContexts: contexts}); !errors.Is(err, ErrStoreInvalidOptions) || source.inspected {
		t.Fatal("noncanonical template reached inspection", err)
	}
	if _, err := NewStoreSession("localhost:1", StoreSessionOptions{PlanOptions: StorePlanOptions{PresentationContexts: contexts}}); !errors.Is(err, ErrStoreInvalidOptions) {
		t.Fatal("noncanonical template published session", err)
	}
}

func TestStoreSessionFixedProfileFrozenBeforeOpenAndDial(t *testing.T) {
	contexts := fixedStoreProfile()
	session, err := NewStoreSession("localhost:1", StoreSessionOptions{PlanOptions: StorePlanOptions{PresentationContexts: contexts}})
	if err != nil {
		t.Fatal(err)
	}
	contexts[1].ID = 1
	contexts[1].TransferSyntaxUIDs[0] = "changed"
	var dialed bool
	session.dial = func(_ context.Context, _ string, opts ul.DialOptions) (*ul.Association, error) {
		dialed = true
		if !reflect.DeepEqual(opts.Contexts, fixedStoreProfile()) {
			t.Fatal("fixed wire template changed", opts.Contexts)
		}
		return nil, errors.New("terminal")
	}
	_, err = session.StoreBatch(context.Background(), []StoreSource{descriptorStoreSource{descriptor: testStoreDescriptor("1.2.3", "1.2.3.1", transfer.ExplicitVRLittleEndian.UID)}})
	if err == nil || !dialed {
		t.Fatal("expected dial using admitted profile", err)
	}
	dialed = false
	result, err := session.StoreBatch(context.Background(), []StoreSource{descriptorStoreSource{descriptor: testStoreDescriptor("1.2.9", "1.2.3.2", transfer.ExplicitVRLittleEndian.UID)}})
	if !errors.Is(err, ErrStorePresentationContextRejected) || dialed || result.Failed != 1 {
		t.Fatal("excluded source reached dial/Open", result, err)
	}
}

func TestStoreSessionFixedProfileSerialAndPipelineRealSocket(t *testing.T) {
	for _, invoked := range []int{1, 4} {
		t.Run(map[int]string{1: "serial", 4: "pipeline"}[invoked], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			paths := writeStoreFixtures(t, 3)
			peak := &atomic.Int32{}
			address, serverDone := startCountingStoreSCP(t, ctx, 4, 3, peak, 0, 0)
			// The first duplicated SOP context is rejected by this peer. The native
			// alternate remains usable; the unrelated context stays in the wire RQ.
			contexts := []ul.PresentationContext{{ID: 1, AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.JPEGBaseline.UID}}, {ID: 3, AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}, {ID: 5, AbstractSyntaxUID: "1.2.3.99", TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}
			var observed []ul.PresentationContext
			session, err := NewStoreSession(address, StoreSessionOptions{MaxInvokedOperations: invoked, PlanOptions: StorePlanOptions{PresentationContexts: contexts}, DialOptions: ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}, OnAssociationStarted: func(_ context.Context, a *ul.Association, _ int) {
				observed = cloneStorePresentationContexts(a.Contexts)
			}})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.StoreBatch(ctx, pathStoreSources(paths))
			if err != nil || result.Succeeded != 3 || result.Associations != 1 || !reflect.DeepEqual(observed, contexts) {
				t.Fatal("fixed profile exchange", result, observed, err)
			}
			if err := waitPipelineServer(serverDone); err != nil {
				t.Fatal(err)
			}
		})
	}
}
