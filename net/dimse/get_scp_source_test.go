package dimse

import (
	"context"
	"errors"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCGetStoreSourceOwnershipAndPreflight(t *testing.T) {
	for _, mode := range []string{"success", "partial-error", "identity", "syntax", "nil-writer", "close-error", "no-role", "no-context", "canceled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var loads, closes, stores, responses atomic.Int32
			op := cGetSubOperation("1.2.3.4.5")
			dataset, _ := op.LoadDataSet(ctx)
			op.LoadDataSet = nil
			op.OnResponse = func(context.Context, uint16) { responses.Add(1) }
			op.TransferSyntaxUIDs = []string{transfer.ImplicitVRLittleEndian.UID}
			op.LoadStoreSource = func(loadCtx context.Context) (OpenedStoreSource, error) {
				loads.Add(1)
				opened := OpenedStoreSource{Descriptor: StoreDescriptor{SOPClassUID: op.AffectedSOPClassUID, SOPInstanceUID: op.AffectedSOPInstanceUID, TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID, WritableTransferSyntaxUIDs: op.TransferSyntaxUIDs}, WriteDataSet: func(_ context.Context, w io.Writer, syntax transfer.Syntax) error {
					return object.WriteDataSet(w, dataset, syntax)
				}, Close: func() error {
					closes.Add(1)
					if mode == "close-error" {
						return errors.New("private close value")
					}
					return nil
				}}
				switch mode {
				case "partial-error":
					return opened, ErrStoreInvalidSource
				case "canceled":
					return opened, context.Canceled
				case "identity":
					opened.Descriptor.SOPInstanceUID = "1.2.3.4.6"
				case "syntax":
					opened.Descriptor.WritableTransferSyntaxUIDs = []string{transfer.ExplicitVRLittleEndian.UID}
				case "nil-writer":
					opened.WriteDataSet = nil
				}
				return opened, nil
			}
			options := []cGetStatusTestOption{withCGetStoreHandler(CGetStoreHandlerFunc(func(context.Context, CGetStoreRequestContext) (uint16, error) {
				stores.Add(1)
				return StatusSuccess, nil
			}))}
			if mode == "no-role" {
				options = append(options, withoutCGetStorageRole())
			}
			if mode == "no-context" {
				options = append(options, withoutCGetStorageContext())
			}
			final, serverErr := runStudyRootCGetStatusTest(t, ctx, CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
				return []CGetSubOperation{op}, nil
			}), options...)
			if mode == "canceled" {
				if !errors.Is(serverErr, context.Canceled) {
					t.Fatal(serverErr)
				}
				assertCGetCounts(t, "final", final, StatusCGetCancel, 1, 0, 0, 0)
			} else if serverErr != nil {
				t.Fatal(serverErr)
			}
			shouldLoad := mode != "no-role" && mode != "no-context"
			if shouldLoad && (loads.Load() != 1 || closes.Load() != 1) || !shouldLoad && (loads.Load() != 0 || closes.Load() != 0) {
				t.Fatalf("load/close=%d/%d", loads.Load(), closes.Load())
			}
			shouldStore := mode == "success" || mode == "close-error"
			if shouldStore && responses.Load() != 1 || !shouldStore && responses.Load() != 0 {
				t.Fatalf("correlated responses=%d", responses.Load())
			}
			if shouldStore && stores.Load() != 1 || !shouldStore && stores.Load() != 0 {
				t.Fatalf("store commands=%d", stores.Load())
			}
			if mode == "success" {
				assertCGetCounts(t, "final", final, StatusSuccess, 0, 1, 0, 0)
			} else if mode != "canceled" {
				assertCGetCounts(t, "final", final, StatusCGetSubOperationsCompleteOneOrMoreFailures, 0, 0, 1, 0)
			}
		})
	}
}

func TestCGetLegacyOnResponseObserver(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var observed atomic.Int32
	op := cGetSubOperation("1.2.3.4.5")
	op.OnResponse = func(_ context.Context, status uint16) {
		if status == StatusSuccess {
			observed.Add(1)
		}
	}
	final, err := runStudyRootCGetStatusTest(t, ctx, CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
		return []CGetSubOperation{op}, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	assertCGetCounts(t, "final", final, StatusSuccess, 0, 1, 0, 0)
	if observed.Load() != 1 {
		t.Fatal("legacy observer omitted")
	}
}
