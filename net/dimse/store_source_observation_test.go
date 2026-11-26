package dimse

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/index"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPathStoreMetadataUsesValidatedHandleAfterPathReplacement(t *testing.T) {
	path := writeStoreFixtures(t, 1)[0]
	payload, err := codecfixture.NativeMedium().Part10Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, payload, 0600); err != nil {
		t.Fatal(err)
	}
	source := NewPathStoreSource(path)
	descriptor, err := source.Inspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	opened, err := source.Open(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer opened.Close()
	var before bytes.Buffer
	if err := opened.WriteDataSet(context.Background(), &before, transfer.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".old"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not the transferred generation"), 0600); err != nil {
		t.Fatal(err)
	}
	result, err := opened.ReadMetadata(context.Background(), index.Options{
		// Representation interpretation belongs to the validated source.
		RawDataSet: &index.RawDataSetOptions{TransferSyntax: transfer.ImplicitVRLittleEndian},
	})
	if err != nil || result.Record.Instance.SOPInstanceUID != descriptor.SOPInstanceUID || result.RawDataSet {
		t.Fatalf("metadata did not come from opened Part 10 handle: err=%v raw=%v", err, result.RawDataSet)
	}
	if !result.StoppedBeforePixelData {
		t.Fatal("metadata observation did not stop before Pixel Data")
	}
	var after bytes.Buffer
	if err := opened.WriteDataSet(context.Background(), &after, transfer.ExplicitVRLittleEndian); err != nil || !bytes.Equal(before.Bytes(), after.Bytes()) {
		t.Fatalf("metadata observation changed the transferred bytes: %v", err)
	}
	if _, err := opened.ReadMetadata(context.Background(), index.Options{Limits: index.Limits{MaxElements: 1}}); !errors.Is(err, index.ErrResourceLimit) {
		t.Fatalf("metadata budget was not enforced: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := opened.ReadMetadata(ctx, index.Options{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("metadata cancellation lost: %v", err)
	}
}

type metadataCountStoreSource struct {
	StoreSource
	reads atomic.Int32
}

func (s *metadataCountStoreSource) Open(ctx context.Context) (OpenedStoreSource, error) {
	opened, err := s.StoreSource.Open(ctx)
	if err != nil {
		return opened, err
	}
	read := opened.ReadMetadata
	opened.ReadMetadata = func(ctx context.Context, options index.Options) (index.Result, error) {
		s.reads.Add(1)
		return read(ctx, options)
	}
	return opened, nil
}

func TestStorePreparedObservationSerialAndPipeline(t *testing.T) {
	for _, window := range []int{1, 2} {
		for _, observe := range []bool{false, true} {
			t.Run(fmt.Sprintf("window-%d/observe-%v", window, observe), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				address, done := startCountingStoreSCP(t, ctx, window, 1, &atomic.Int32{}, 0, 0)
				source := &metadataCountStoreSource{StoreSource: NewPathStoreSource(writeStoreFixtures(t, 1)[0])}
				prepared, responses := 0, 0
				var retained func(context.Context, index.Options) (index.Result, error)
				options := StoreSessionOptions{
					DialOptions:          ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"},
					MaxInvokedOperations: window,
					OnStoreResponse: func(_ context.Context, p StoreProgress) {
						if observe && prepared != 1 {
							t.Error("response preceded metadata capture")
						}
						responses++
					},
				}
				if observe {
					options.OnStorePrepared = func(ctx context.Context, p StorePreparedObservation) {
						if p.SourceIndex != 0 || p.Attempt != 1 || p.AssociationID == "" || p.NegotiatedTransferSyntaxUID == "" || p.ReadMetadata == nil {
							t.Error("prepared correlation is incomplete")
						}
						result, err := p.ReadMetadata(ctx, index.Options{})
						if err != nil || result.Record.Instance.SOPClassUID != p.Descriptor.SOPClassUID || result.Record.Instance.SOPInstanceUID != p.Descriptor.SOPInstanceUID {
							t.Errorf("metadata capture failed: %v", err)
						}
						retained = p.ReadMetadata
						// Observer-owned mutations must not change command identity/offers.
						p.Descriptor.SOPInstanceUID = "1.2.3.999"
						p.Descriptor.WritableTransferSyntaxUIDs[0] = "1.2.3.999"
						prepared++
						panic("observer panic must not change transfer")
					}
				}
				session, err := NewStoreSession(address, options)
				if err != nil {
					t.Fatal(err)
				}
				defer session.Abort(context.Background())
				batch, err := session.StoreBatch(ctx, []StoreSource{source})
				if err != nil || batch.Succeeded != 1 || responses != 1 {
					t.Fatalf("observer changed transfer: success=%d responses=%d err=%v", batch.Succeeded, responses, err)
				}
				wantReads := int32(0)
				if observe {
					wantReads = 1
					if _, err := retained(context.Background(), index.Options{}); !errors.Is(err, ErrStoreInvalidSource) {
						t.Fatalf("metadata hook survived callback: %v", err)
					}
				}
				if source.reads.Load() != wantReads {
					t.Fatalf("metadata scans=%d want=%d", source.reads.Load(), wantReads)
				}
				if err := waitPipelineServer(done); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}

func TestStorePreparedObservationKeepsNilMetadataHook(t *testing.T) {
	called := false
	session := &StoreSession{options: StoreSessionOptions{OnStorePrepared: func(_ context.Context, p StorePreparedObservation) {
		called = true
		if p.ReadMetadata != nil {
			t.Fatal("legacy source acquired a fictitious metadata hook")
		}
	}}}
	session.observeStorePrepared(context.Background(), StoreItemResult{}, OpenedStoreSource{
		WriteDataSet: func(context.Context, io.Writer, transfer.Syntax) error { return nil },
	}, transfer.ExplicitVRLittleEndian.UID)
	if !called {
		t.Fatal("prepared observer was not called")
	}
}

func TestStorePreparedCancellationPreventsCommandSerialAndPipeline(t *testing.T) {
	for _, window := range []int{1, 2} {
		t.Run(fmt.Sprintf("window-%d", window), func(t *testing.T) {
			serverCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			ctx, cancel := context.WithCancel(serverCtx)
			defer cancel()
			peak := &atomic.Int32{}
			address, done := startCountingStoreSCP(t, serverCtx, window, 0, peak, 0, 0)
			session, err := NewStoreSession(address, StoreSessionOptions{
				DialOptions:          ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"},
				MaxInvokedOperations: window,
				OnStorePrepared:      func(context.Context, StorePreparedObservation) { cancel() },
			})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Abort(context.Background())
			_, err = session.StoreBatch(ctx, pathStoreSources(writeStoreFixtures(t, 1)))
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("prepare cancellation lost: %v", err)
			}
			select {
			case <-done:
			case <-serverCtx.Done():
				t.Fatal("association did not terminate after preparation cancellation")
			}
			if peak.Load() != 0 {
				t.Fatal("C-STORE was sent after preparation cancellation")
			}
		})
	}
}
