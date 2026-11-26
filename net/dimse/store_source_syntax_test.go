package dimse

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

type selectedStoreSource struct {
	StoreSource
	opens    atomic.Int32
	selected atomic.Int32
}

func (s *selectedStoreSource) Inspect(ctx context.Context) (StoreDescriptor, error) {
	d, err := s.StoreSource.Inspect(ctx)
	d.WritableTransferSyntaxUIDs = []string{transfer.ImplicitVRLittleEndian.UID}
	return d, err
}
func (s *selectedStoreSource) Open(context.Context) (OpenedStoreSource, error) {
	s.opens.Add(1)
	return OpenedStoreSource{}, ErrStoreInvalidSource
}
func (s *selectedStoreSource) OpenForTransferSyntax(ctx context.Context, syntax transfer.Syntax) (OpenedStoreSource, error) {
	if syntax.UID != transfer.ImplicitVRLittleEndian.UID {
		return OpenedStoreSource{}, ErrStoreTransferSyntax
	}
	s.selected.Add(1)
	o, err := s.StoreSource.Open(ctx)
	o.Descriptor.WritableTransferSyntaxUIDs = []string{syntax.UID}
	return o, err
}

func TestStoreSessionSelectsBeforeOpenSerialAndPipeline(t *testing.T) {
	for _, window := range []int{1, 2} {
		t.Run(string(rune('0'+window)), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			paths := writeStoreFixtures(t, 2)
			peak := &atomic.Int32{}
			address, done := startCountingStoreSCP(t, ctx, window, 2, peak, 0, 0)
			var started, finished atomic.Int32
			var assocID string
			var sources []StoreSource
			var selected []*selectedStoreSource
			for _, path := range paths {
				s := &selectedStoreSource{StoreSource: NewPathStoreSource(path)}
				sources = append(sources, s)
				selected = append(selected, s)
			}
			session, err := NewStoreSession(address, StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}, MaxInvokedOperations: window,
				OnAssociationStarted: func(_ context.Context, assoc *ul.Association, _ int) { started.Add(1); assocID = assoc.ID() },
				OnAssociationFinished: func(_ context.Context, assoc *ul.Association, _ int) {
					finished.Add(1)
					if assoc.ID() != assocID {
						t.Error("observer association changed")
					}
				},
				OnProgress: func(_ context.Context, progress StoreProgress) error {
					if progress.StatusSet && progress.AssociationID != assocID {
						t.Error("progress association ID missing")
					}
					return nil
				}})
			if err != nil {
				t.Fatal(err)
			}
			batch, err := session.StoreBatch(ctx, sources)
			if err != nil || batch.Succeeded != 2 {
				t.Fatalf("batch %+v err=%v", batch, err)
			}
			if started.Load() != 1 || finished.Load() != 1 || assocID == "" {
				t.Fatalf("association observers=%d/%d id=%q", started.Load(), finished.Load(), assocID)
			}
			for _, s := range selected {
				if s.opens.Load() != 0 || s.selected.Load() != 1 {
					t.Fatalf("legacy/selected=%d/%d", s.opens.Load(), s.selected.Load())
				}
			}
			if err := waitPipelineServer(done); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStoreRepresentationBoundsAreDetachedAndValidated(t *testing.T) {
	descriptor, err := NewPathStoreSource(writeStoreFixtures(t, 1)[0]).Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	descriptor.WritableTransferSyntaxUIDs = []string{descriptor.TransferSyntaxUID, transfer.ImplicitVRLittleEndian.UID}
	descriptor.TransferSyntaxSizes = map[string]int64{descriptor.TransferSyntaxUID: descriptor.Size, transfer.ImplicitVRLittleEndian.UID: 1 << 20}
	if descriptor.TransferSyntaxUID == transfer.ImplicitVRLittleEndian.UID {
		t.Fatal("fixture must use explicit VR")
	}
	valid, err := normalizeStoreDescriptor(descriptor)
	if err != nil {
		t.Fatal(err)
	}
	copy := cloneStoreDescriptor(valid)
	copy.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] = 1
	if valid.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] != 1<<20 {
		t.Fatal("clone shared bounds")
	}
	descriptor.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] = 2
	if valid.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] != 1<<20 {
		t.Fatal("normalization shared caller map")
	}
	for _, mode := range []string{"negative", "zero", "unknown", "missing", "original-mismatch", "duplicate"} {
		t.Run(mode, func(t *testing.T) {
			d := cloneStoreDescriptor(valid)
			switch mode {
			case "negative":
				d.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] = -1
			case "zero":
				d.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID] = 0
			case "unknown":
				d.TransferSyntaxSizes["1.2.3.999"] = 1
			case "missing":
				delete(d.TransferSyntaxSizes, transfer.ImplicitVRLittleEndian.UID)
			case "original-mismatch":
				d.TransferSyntaxSizes[d.TransferSyntaxUID]++
			case "duplicate":
				d.TransferSyntaxSizes[transfer.ImplicitVRLittleEndian.UID+" "] = 1
			}
			if _, err := normalizeStoreDescriptor(d); !errors.Is(err, ErrStoreInvalidSource) {
				t.Fatalf("invalid bound admitted: %v", err)
			}
		})
	}
	opened := OpenedStoreSource{Descriptor: copy, WriteDataSet: func(context.Context, io.Writer, transfer.Syntax) error { return nil }, Close: func() error { return nil }}
	if !errors.Is(validateOpenedStoreSource(valid, opened), ErrStoreSourceChanged) {
		t.Fatal("changed representation bound admitted")
	}
}

type cancelOnCloseStoreSource struct {
	StoreSource
	cancel context.CancelFunc
}

func (s cancelOnCloseStoreSource) Open(ctx context.Context) (OpenedStoreSource, error) {
	o, err := s.StoreSource.Open(ctx)
	close := o.Close
	o.Close = func() error { s.cancel(); return close() }
	return o, err
}

func TestStoreResponseObserverPrecedesFinishWhenCloseCancels(t *testing.T) {
	for _, window := range []int{1, 2} {
		t.Run(string(rune('0'+window)), func(t *testing.T) {
			serverCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(serverCtx)
			defer cancel()
			address, _ := startCountingStoreSCP(t, serverCtx, window, 2, &atomic.Int32{}, 0, 0)
			var mu sync.Mutex
			var finished bool
			responses, progress := 0, 0
			session, err := NewStoreSession(address, StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"}, MaxInvokedOperations: window,
				OnStoreResponse: func(observerCtx context.Context, p StoreProgress) {
					mu.Lock()
					defer mu.Unlock()
					if finished || !p.StatusSet || p.Status != StatusSuccess || p.AssociationID == "" || observerCtx.Err() != nil {
						t.Error("response missed or observed after Finish")
					}
					responses++
				},
				OnProgress: func(progressCtx context.Context, p StoreProgress) error {
					mu.Lock()
					defer mu.Unlock()
					if window != 1 || finished || responses != 1 || progress != 0 {
						t.Error("confirmed serial progress must follow the response once before Finish")
					}
					if !errors.Is(progressCtx.Err(), context.Canceled) || p.SourceIndex != 0 || p.Completed != 1 || p.Total != 1 || !p.StatusSet || p.Status != StatusSuccess || p.Outcome != StoreOutcomeSuccess {
						t.Errorf("confirmed progress after Close cancellation = %+v, context=%v", p, progressCtx.Err())
					}
					progress++
					return nil
				},
				OnAssociationFinished: func(context.Context, *ul.Association, int) { mu.Lock(); defer mu.Unlock(); finished = true },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Abort(context.Background())
			path := writeStoreFixtures(t, 1)[0]
			result, err := session.StoreBatch(ctx, []StoreSource{cancelOnCloseStoreSource{NewPathStoreSource(path), cancel}})
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("cancel lost: %v", err)
			}
			if window == 1 && (result.Succeeded != 1 || result.Failed != 0 || result.Unknown != 0 || result.Canceled != 0 || len(result.Items) != 1 || result.Items[0].Outcome != StoreOutcomeSuccess || !result.Items[0].StatusSet || result.Items[0].Status != StatusSuccess) {
				t.Fatalf("confirmed serial success lost: %+v", result)
			}
			mu.Lock()
			defer mu.Unlock()
			wantProgress := 0
			if window == 1 {
				wantProgress = 1
			}
			if responses != 1 || !finished || progress != wantProgress {
				t.Fatalf("responses=%d finish=%v legacyprogress=%d", responses, finished, progress)
			}
		})
	}
}
