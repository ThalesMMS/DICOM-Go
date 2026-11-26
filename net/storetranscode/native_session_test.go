package storetranscode

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestNativeRetrievePipelineGateOneAndSpoolQuota(t *testing.T) {
	for _, quota := range []int64{2 << 20, 4 << 20} {
		t.Run(map[int64]string{2 << 20: "insufficient", 4 << 20: "sufficient"}[quota], func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			source, calls, _, _ := nativeFixtureSource(t, quota)
			// The two opens share one decode/materialization gate, but own independent
			// immutable output handles. A pending response must not hold the CPU slot.
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			var commands atomic.Int32
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{codecClass(source, t)}, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}, AsynchronousOperationsWindow: &ul.AsynchronousOperationsWindow{MaximumInvoked: 2, MaximumPerformed: 2}})
				if err != nil {
					done <- err
					return
				}
				defer assoc.Close()
				async, err := dimse.NewAsyncSession(assoc, dimse.AsyncSessionOptions{Handlers: map[uint16]dimse.AsyncRequestHandler{dimse.CStoreRQ: func(hctx context.Context, s *dimse.AsyncSession, message dimse.AsyncMessage) error {
					commands.Add(1)
					request, err := dimse.ParseCStoreRequest(message.Command)
					if err != nil {
						return err
					}
					timer := time.NewTimer(100 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-hctx.Done():
						return hctx.Err()
					}
					return s.Respond(hctx, message, (dimse.CStoreResponse{AffectedSOPClassUID: request.AffectedSOPClassUID, AffectedSOPInstanceUID: request.AffectedSOPInstanceUID, MessageIDBeingRespondedTo: request.MessageID, Status: dimse.StatusSuccess}).CommandSet(), nil)
				}}})
				if err != nil {
					done <- err
					return
				}
				defer async.Close()
				<-async.Done()
				done <- async.Err()
			}()
			session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "RETRIEVE"}, MaxInvokedOperations: 2, ContinueOnError: true})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Abort(context.Background())
			result, err := session.StoreBatch(ctx, []dimse.StoreSource{source, source})
			if err != nil {
				t.Fatal(err)
			}
			expected := 2
			if quota == 2<<20 {
				expected = 1
				if result.Failed != 1 || !errors.Is(result.Items[1].Err, dimse.ErrStoreResourceLimit) || result.Items[1].Attempted {
					t.Fatalf("quota failure should be before command: %+v", result)
				}
			}
			if result.Succeeded != expected || int(commands.Load()) != expected || int(calls.Load()) != expected || !result.Complete {
				t.Fatalf("result=%+v commands=%d decodes=%d", result, commands.Load(), calls.Load())
			}
			if source.opts.Resources.SpoolBytes() != 0 {
				t.Fatal("quota retained")
			}
			assertEmpty(t, source.opts.SpoolDirectory)
			select {
			case <-done:
			case <-ctx.Done():
				t.Fatal("server/session deadlock")
			}
		})
	}
}
func codecClass(s *NativeSource, t *testing.T) string {
	d, err := s.Inspect(context.Background())
	if err != nil {
		t.Error(err)
	}
	return d.SOPClassUID
}

func TestNativeRetrieveSelectedRepresentationFitsBudget(t *testing.T) {
	for _, window := range []int{1, 2} {
		for _, nativeOnly := range []bool{false, true} {
			name := "original"
			if nativeOnly {
				name = "native-refused"
			}
			t.Run(fmt.Sprintf("%s/window-%d", name, window), func(t *testing.T) {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				source, calls, _, _ := nativeFixtureSource(t, 4<<20)
				source.opts.Transcode.Limits.MaxOutputBytes = 2 << 20
				d, err := source.Inspect(ctx)
				if err != nil {
					t.Fatal(err)
				}
				if d.Size >= 1<<20 || d.TransferSyntaxSizes[transfer.ExplicitVRLittleEndian.UID] != 2<<20 {
					t.Fatal("representation bounds not distinct")
				}
				target := d.TransferSyntaxUID
				if nativeOnly {
					target = transfer.ExplicitVRLittleEndian.UID
				}
				listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
				if err != nil {
					t.Fatal(err)
				}
				defer listener.Close()
				var commands atomic.Int32
				done := make(chan error, 1)
				go func() {
					a, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{d.SOPClassUID}, SupportedTransferSyntaxes: []string{target}, AsynchronousOperationsWindow: &ul.AsynchronousOperationsWindow{MaximumInvoked: uint16(window), MaximumPerformed: uint16(window)}})
					if err != nil {
						done <- err
						return
					}
					defer a.Close()
					done <- dimse.ServeStorageAssociation(ctx, a, dimse.StorageSCPOptions{StoreHandler: dimse.CStoreHandlerFunc(func(context.Context, dimse.CStoreRequestContext) (uint16, error) {
						commands.Add(1)
						return dimse.StatusSuccess, nil
					})})
				}()
				// The per-object bound refuses the 2 MiB fallback; the bytes
				// in flight alone would send it with a window of one.
				session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "RETRIEVE"},
					PlanOptions:          dimse.StorePlanOptions{Limits: dimse.StoreLimits{MaxItemBytes: 1 << 20}},
					MaxInvokedOperations: window, MaxInFlightBytes: 1 << 20, ContinueOnError: true})
				if err != nil {
					t.Fatal(err)
				}
				defer session.Abort(context.Background())
				result, err := session.StoreBatch(ctx, []dimse.StoreSource{source, source})
				if err != nil {
					t.Fatal(err)
				}
				if nativeOnly {
					if result.Failed != 2 || commands.Load() != 0 || calls.Load() != 0 {
						t.Fatalf("fallback emitted/decoded before budget rejection: %+v commands=%d decodes=%d", result, commands.Load(), calls.Load())
					}
					for _, item := range result.Items {
						if !errors.Is(item.Err, dimse.ErrStoreResourceLimit) || item.Attempted {
							t.Fatal("selected bound refusal not before command")
						}
					}
				} else if result.Succeeded != 2 || commands.Load() != 2 || calls.Load() != 0 {
					t.Fatalf("small original blocked by unselected fallback: %+v", result)
				}
				if source.opts.Resources.SpoolBytes() != 0 {
					t.Fatal("quota retained")
				}
				assertEmpty(t, source.opts.SpoolDirectory)
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal("deadlock")
				}
			})
		}
	}
}
