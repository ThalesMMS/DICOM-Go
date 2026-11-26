package dimse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestCFindBeforeSendStopsAfterRevocation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	checks := 0
	serverDone := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
			AETitle: "FINDSCP", Context: ctx,
			SupportedAbstractSyntaxes: []string{StudyRootFindSOPClassUID},
			SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID},
		})
		if err != nil {
			serverDone <- err
			return
		}
		defer assoc.Close()
		pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
		if err != nil {
			serverDone <- err
			return
		}
		serverDone <- ServeStudyRootCFind(ctx, assoc, pc.ID, CFindMatchHandlerFunc(func(context.Context, CFindRequestContext) ([]CFindMatch, error) {
			matches := make([]CFindMatch, 3)
			for i := range matches {
				matches[i] = CFindMatch{
					Identifier: studyRootFindMatch(fmt.Sprintf("P%d", i+1), fmt.Sprintf("1.2.3.%d", i+1)),
					BeforeSend: func(context.Context) error {
						checks++
						if i == 1 {
							return NewCFindSCPError(0xC123, "private policy detail", errors.New("private cause"))
						}
						return nil
					},
				}
			}
			return matches, nil
		}))
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "FINDSCP", CallingAETitle: "FINDSCU", Contexts: []ul.PresentationContext{StudyRootFindPresentationContext()},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	pc, err := AcceptedContextForSOPClass(assoc, StudyRootFindSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	keys := mustBuild(t, func() ([]core.Element, error) { return BuildStudyRootStudyFindKeys(nil) })
	if err := SendCFindRequest(assoc, pc.ID, CFindRequest{AffectedSOPClassUID: StudyRootFindSOPClassUID, MessageID: 1}, keys); err != nil {
		t.Fatal(err)
	}
	response, identifier, err := ReceiveCFindResponse(assoc, pc.ID, transfer.ImplicitVRLittleEndian)
	if err != nil || response.Status != StatusPending || identifier == nil {
		t.Fatalf("first response = %#v, %v", response, err)
	}
	if id, _ := identifier.GetString(core.NewTag(0x0010, 0x0020)); id != "P1" {
		t.Fatalf("first patient ID = %q", id)
	}
	response, identifier, err = ReceiveCFindResponse(assoc, pc.ID, transfer.ImplicitVRLittleEndian)
	if err != nil || response.Status != 0xC123 || identifier != nil || response.ErrorComment != "access denied" {
		t.Fatalf("final response = %#v, Identifier = %#v, error = %v", response, identifier, err)
	}
	if err := <-serverDone; !errors.Is(err, ErrStoreAccessDenied) || strings.Contains(err.Error(), "private") || checks != 2 {
		t.Fatalf("server error = %v, authorization checks = %d", err, checks)
	}
}

func TestCGetSourceRevocationStopsBeforeNextStore(t *testing.T) {
	for _, phase := range []string{"open", "before-store"} {
		t.Run(phase, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var loads, closes, checks, stores atomic.Int32
			ops := make([]CGetSubOperation, 3)
			for i := range ops {
				op := cGetSubOperation(fmt.Sprintf("1.2.3.4.%d", i+5))
				dataset, err := op.LoadDataSet(ctx)
				if err != nil {
					t.Fatal(err)
				}
				op.LoadDataSet = nil
				op.TransferSyntaxUIDs = []string{transfer.ImplicitVRLittleEndian.UID}
				op.LoadStoreSource = func(context.Context) (OpenedStoreSource, error) {
					loads.Add(1)
					if i == 1 && phase == "open" {
						return OpenedStoreSource{}, fmt.Errorf("private source details: %w", ErrStoreAccessDenied)
					}
					return OpenedStoreSource{
						Descriptor: StoreDescriptor{SOPClassUID: op.AffectedSOPClassUID, SOPInstanceUID: op.AffectedSOPInstanceUID,
							TransferSyntaxUID: transfer.ImplicitVRLittleEndian.UID, WritableTransferSyntaxUIDs: op.TransferSyntaxUIDs},
						BeforeStore: func(context.Context) error {
							checks.Add(1)
							if i == 1 {
								return errors.New("private policy details")
							}
							return nil
						},
						WriteDataSet: func(_ context.Context, w io.Writer, syntax transfer.Syntax) error {
							return object.WriteDataSet(w, dataset, syntax)
						},
						Close: func() error { closes.Add(1); return nil },
					}, nil
				}
				ops[i] = op
			}
			final, err := runStudyRootCGetStatusTest(t, ctx, CGetHandlerFunc(func(context.Context, CGetRequestContext) ([]CGetSubOperation, error) {
				return ops, nil
			}), withCGetStoreHandler(CGetStoreHandlerFunc(func(context.Context, CGetStoreRequestContext) (uint16, error) {
				stores.Add(1)
				return StatusSuccess, nil
			})))
			if !errors.Is(err, ErrStoreAccessDenied) || strings.Contains(err.Error(), "private") {
				t.Fatalf("server error = %v", err)
			}
			assertCGetCounts(t, "final", final, StatusCGetUnableToProcess, 0, 1, 0, 0)
			if final.Identifier != nil || len(final.FailedSOPInstanceUIDListOrNil) != 0 || final.ErrorComment != "access denied" {
				t.Fatalf("denied final response = %#v", final)
			}
			wantOpenHandles := int32(2)
			if phase == "open" {
				wantOpenHandles = 1
			}
			if loads.Load() != 2 || checks.Load() != wantOpenHandles || closes.Load() != wantOpenHandles || stores.Load() != 1 {
				t.Fatalf("loads = %d, checks = %d, closes = %d, stores = %d", loads.Load(), checks.Load(), closes.Load(), stores.Load())
			}
		})
	}
}

func TestCMoveBatchRevocationPreservesOnlyEmittedTransfers(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	responses, err := runPatientRootCMoveProgressTest(t, ctx, cMoveBatchTestHandler{prepare: func(context.Context, CMoveRequestContext) (CMoveBatch, error) {
		return CMoveBatch{
			SubOperations: []CMoveSubOperationDescriptor{
				{AffectedSOPClassUID: cGetTestStorageSOPClassUID, AffectedSOPInstanceUID: "1.2.3.1"},
				{AffectedSOPClassUID: cGetTestStorageSOPClassUID, AffectedSOPInstanceUID: "1.2.3.2"},
				{AffectedSOPClassUID: cGetTestStorageSOPClassUID, AffectedSOPInstanceUID: "1.2.3.3"},
			},
			Run: func(_ context.Context, yield func(CMoveSubOperationResult) error) error {
				if err := yield(CMoveSubOperationResult{Status: StatusSuccess}); err != nil {
					return err
				}
				return fmt.Errorf("private policy detail: %w", ErrStoreAccessDenied)
			},
		}, nil
	}})
	if !errors.Is(err, ErrStoreAccessDenied) || errors.Is(err, ErrCMoveBatchContract) || strings.Contains(err.Error(), "private") {
		t.Fatalf("server error = %v", err)
	}
	if len(responses) != 2 {
		t.Fatalf("responses = %#v", responses)
	}
	assertCMoveCounts(t, "pending", responses[0], StatusPending, 2, 1, 0, 0)
	final := responses[1]
	assertCMoveCounts(t, "final", final, StatusCMoveUnableToProcess, 0, 1, 0, 0)
	if final.Identifier != nil || len(final.FailedSOPInstanceUIDListOrNil) != 0 || final.ErrorComment != "access denied" {
		t.Fatalf("denied final response = %#v", final)
	}
}

type storeAccessTestSource struct {
	StoreSource
	open func(context.Context) (OpenedStoreSource, error)
}

func (source storeAccessTestSource) Open(ctx context.Context) (OpenedStoreSource, error) {
	return source.open(ctx)
}

func TestStoreSessionRevocationStopsSerialAndPipelinedAdmission(t *testing.T) {
	for _, window := range []int{1, 4} {
		t.Run(fmt.Sprintf("window-%d", window), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			address, serverDone := startCountingStoreSCP(t, ctx, 4, 1, &atomic.Int32{}, 0, 0)
			var opens, writes, checks, prepared, progressed []int
			var closes [3]atomic.Int32
			var responses atomic.Int32
			sources := pathStoreSources(writeStoreFixtures(t, 3))
			for i, source := range sources {
				sources[i] = storeAccessTestSource{StoreSource: source, open: func(ctx context.Context) (OpenedStoreSource, error) {
					opens = append(opens, i)
					opened, err := source.Open(ctx)
					if err != nil {
						return opened, err
					}
					closeSource, write := opened.Close, opened.WriteDataSet
					opened.Close = func() error {
						closes[i].Add(1)
						return closeSource()
					}
					opened.WriteDataSet = func(ctx context.Context, w io.Writer, syntax transfer.Syntax) error {
						writes = append(writes, i)
						return write(ctx, w, syntax)
					}
					opened.BeforeStore = func(context.Context) error {
						checks = append(checks, i)
						if len(prepared) != i+1 {
							t.Errorf("authorization check preceded prepared observer for source %d", i)
						}
						if i == 1 {
							return errors.New("private policy detail")
						}
						return nil
					}
					return opened, nil
				}}
			}
			session, err := NewStoreSession(address, StoreSessionOptions{
				DialOptions:     ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "STORESCU"},
				ContinueOnError: true, MaxInvokedOperations: window,
				OnAssociationStarted: func(_ context.Context, assoc *ul.Association, _ int) {
					if got := effectiveStoreInvoked(window > 1, window, assoc.EffectiveAsynchronousOperationsWindow()); got != window {
						t.Errorf("effective window = %d, want %d", got, window)
					}
				},
				OnStorePrepared: func(_ context.Context, preparedSource StorePreparedObservation) {
					prepared = append(prepared, preparedSource.SourceIndex)
				},
				OnStoreResponse: func(context.Context, StoreProgress) { responses.Add(1) },
				OnProgress: func(_ context.Context, progress StoreProgress) error {
					progressed = append(progressed, progress.SourceIndex)
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := session.StoreBatch(ctx, sources)
			if !errors.Is(err, ErrStoreAccessDenied) || strings.Contains(err.Error(), "private") {
				t.Fatalf("StoreBatch() error = %v", err)
			}
			if result.Succeeded != 1 || result.Failed != 0 || result.Unknown != 0 || result.Complete || result.Associations != 1 {
				t.Fatalf("result = %#v", result)
			}
			if result.Items[0].Outcome != StoreOutcomeSuccess || result.Items[1].Outcome != StoreOutcomeNotSent || result.Items[2].Outcome != StoreOutcomeNotSent || result.Items[1].Attempted || result.Items[2].Attempted || !errors.Is(result.Items[1].Err, ErrStoreAccessDenied) {
				t.Fatalf("items = %#v", result.Items)
			}
			if responses.Load() != 1 || !reflect.DeepEqual(opens, []int{0, 1}) || !reflect.DeepEqual(writes, []int{0}) || !reflect.DeepEqual(checks, opens) || !reflect.DeepEqual(prepared, opens) || !reflect.DeepEqual(progressed, writes) {
				t.Fatalf("opens %v, checks %v, prepared %v, writes %v, progressed %v, responses %d", opens, checks, prepared, writes, progressed, responses.Load())
			}
			if closes[0].Load() != 1 || closes[1].Load() != 1 || closes[2].Load() != 0 {
				t.Fatalf("closes %d, %d, %d", closes[0].Load(), closes[1].Load(), closes[2].Load())
			}
			if err := waitPipelineServer(serverDone); err != nil {
				t.Fatal(err)
			}
		})
	}
}
