package storetranscode

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata"
	"github.com/ThalesMMS/dicom-go/pixeldata/rle"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func incrementalReceivedPixels(dataset *object.Object, syntax transfer.Syntax, expected *object.File, options Options) error {
	if syntax.Encapsulated {
		converted, _, err := pixeldata.TranscodeDataSet(context.Background(), dataset, syntax, expected.TransferSyntax, options.Transcode)
		if err != nil {
			return err
		}
		defer converted.Close()
		dataset = converted
	}
	p, ok := dataset.Get(core.TagPixelData)
	if !ok {
		return errors.New("received pixels absent")
	}
	actual, ok := p.RawBytes()
	if !ok {
		return errors.New("received pixels not resident")
	}
	p, _ = expected.Dataset.Get(core.TagPixelData)
	wanted, _ := p.RawBytes()
	if !bytes.Equal(actual, wanted) {
		return errors.New("full received pixels differ")
	}
	got, _ := dataset.GetUID(core.NewTag(0x0008, 0x0018))
	want, _ := expected.Dataset.GetUID(core.NewTag(0x0008, 0x0018))
	if got != want {
		return errors.New("received identity differs")
	}
	return nil
}

func incrementalStorePeer(t testing.TB, ctx context.Context, target transfer.Syntax, expected *object.File, opts Options) (*ul.Listener, <-chan error, *atomic.Int32) {
	t.Helper()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	arrivals := &atomic.Int32{}
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: []string{target.UID}})
		if err != nil {
			done <- err
			return
		}
		defer assoc.Close()
		async, err := dimse.NewAsyncSession(assoc, dimse.AsyncSessionOptions{Handlers: map[uint16]dimse.AsyncRequestHandler{dimse.CStoreRQ: func(hctx context.Context, session *dimse.AsyncSession, message dimse.AsyncMessage) error {
			arrivals.Add(1)
			syntax, err := dimse.AcceptedTransferSyntax(assoc, message.PresentationContextID)
			if err != nil {
				return err
			}
			if syntax.UID != target.UID {
				return errors.New("unexpected negotiated syntax")
			}
			if err := incrementalReceivedPixels(message.DataSet, syntax, expected, opts); err != nil {
				return err
			}
			request, err := dimse.ParseCStoreRequest(message.Command)
			if err != nil {
				return err
			}
			return session.Respond(hctx, message, (dimse.CStoreResponse{AffectedSOPClassUID: request.AffectedSOPClassUID, AffectedSOPInstanceUID: request.AffectedSOPInstanceUID, Status: dimse.StatusSuccess}).CommandSet(), nil)
		}}})
		if err != nil {
			done <- err
			return
		}
		defer async.Close()
		<-async.Done()
		terminal := async.Err()
		if errors.Is(terminal, dimse.ErrAssociationReleased) {
			terminal = nil
		}
		done <- terminal
	}()
	return listener, done, arrivals
}

func TestStoreIncrementalLoopbackSelectedAndOriginal(t *testing.T) {
	for _, selected := range []transfer.Syntax{transfer.RLELossless, transfer.ExplicitVRLittleEndian} {
		t.Run(selected.UID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			file := testFile()
			opts := testOptions(t, transfer.RLELossless)
			opts.FallbackOriginal = true
			source, err := NewSource(dimse.NewFileStoreSource(file, 0), opts)
			if err != nil {
				t.Fatal(err)
			}
			listener, done, arrivals := incrementalStorePeer(t, ctx, selected, file, opts)
			defer listener.Close()
			session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "TEST"}})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Abort(context.Background())
			result, err := session.Store(ctx, source)
			if err != nil || result.NegotiatedTransferSyntaxUID != selected.UID {
				t.Fatalf("store=%+v err=%v", result, err)
			}
			if err := session.Close(ctx); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("store peer did not join")
			}
			if arrivals.Load() != 1 {
				t.Fatal("unexpected C-STORE count")
			}
			assertEmpty(t, opts.SpoolDirectory)
		})
	}
}

type incrementalLateEncoder struct {
	calls     atomic.Int32
	failAfter atomic.Int32
}

func (*incrementalLateEncoder) Capabilities() pixeldata.EncoderCapabilities {
	return rle.NewEncoder().Capabilities()
}
func (e *incrementalLateEncoder) EncodeFrame(ctx context.Context, frame []byte, metadata pixeldata.Metadata) (pixeldata.EncodedFrame, error) {
	call := e.calls.Add(1)
	if n := e.failAfter.Load(); n > 0 && call >= n {
		return pixeldata.EncodedFrame{}, fmt.Errorf("late frame: %w", errIncrementalLateFrame)
	}
	return rle.NewEncoder().EncodeFrame(ctx, frame, metadata)
}

var errIncrementalLateFrame = errors.New("synthetic later-frame failure")

func TestStoreIncrementalLateFrameFailureBeforeCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := testFile()
	opts := testOptions(t, transfer.RLELossless)
	encoder := &incrementalLateEncoder{}
	opts.Transcode.EncoderRegistry = pixeldata.NewMemoryEncoderRegistry()
	if err := opts.Transcode.EncoderRegistry.RegisterEncoder(opts.Target.UID, encoder); err != nil {
		t.Fatal(err)
	}
	source, err := NewSource(dimse.NewFileStoreSource(file, 0), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := source.Inspect(ctx); err != nil {
		t.Fatal(err)
	}
	encoder.failAfter.Store(encoder.calls.Load() + 2)
	if _, directErr := source.Open(ctx); !errors.Is(directErr, errIncrementalLateFrame) {
		t.Fatalf("adapter lost later-frame cause: %v", directErr)
	}
	assertEmpty(t, opts.SpoolDirectory)
	encoder.failAfter.Store(encoder.calls.Load() + 2)
	listener, done, arrivals := incrementalStorePeer(t, ctx, opts.Target, file, opts)
	defer listener.Close()
	session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "TEST"}})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Abort(context.Background())
	result, err := session.Store(ctx, source)
	if !errors.Is(err, dimse.ErrStoreInvalidSource) || result.Attempted || encoder.calls.Load() < encoder.failAfter.Load() {
		t.Fatalf("late preparation emitted command: %+v %v", result, err)
	}
	_ = session.Close(ctx)
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("failed preparation peer did not join")
	}
	if arrivals.Load() != 0 {
		t.Fatal("C-STORE command preceded complete preparation")
	}
	assertEmpty(t, opts.SpoolDirectory)
}

func TestStoreIncrementalCGetSelectedAndOriginal(t *testing.T) {
	for _, selected := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRLittleEndian} {
		t.Run(selected.UID, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			file := testFile()
			native, err := NewNativeSource(dimse.NewFileStoreSource(file, 0), NativeOptions{SpoolDirectory: t.TempDir()})
			if err != nil {
				t.Fatal(err)
			}
			descriptor, err := native.Inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "GETSCP", Context: ctx, SupportedAbstractSyntaxes: []string{dimse.StudyRootGetSOPClassUID, descriptor.SOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID, transfer.ExplicitVRLittleEndian.UID}, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: descriptor.SOPClassUID, SCPRole: true}}})
				if err != nil {
					done <- err
					return
				}
				defer assoc.Close()
				pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootGetSOPClassUID)
				if err != nil {
					done <- err
					return
				}
				done <- dimse.ServeStudyRootCGet(ctx, assoc, pc.ID, dimse.CGetHandlerFunc(func(context.Context, dimse.CGetRequestContext) ([]dimse.CGetSubOperation, error) {
					return []dimse.CGetSubOperation{{AffectedSOPClassUID: descriptor.SOPClassUID, AffectedSOPInstanceUID: descriptor.SOPInstanceUID, TransferSyntaxUIDs: []string{selected.UID}, LoadStoreSource: func(c context.Context) (dimse.OpenedStoreSource, error) {
						return native.OpenForTransferSyntax(c, selected)
					}}}, nil
				}))
			}()
			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "GETSCP", CallingAETitle: "GETSCU", Contexts: []ul.PresentationContext{dimse.StudyRootGetPresentationContext(), {AbstractSyntaxUID: descriptor.SOPClassUID, TransferSyntaxUIDs: []string{selected.UID}}}, RoleSelections: []ul.RoleSelectionItem{{SopClassUID: descriptor.SOPClassUID, SCPRole: true}}})
			if err != nil {
				t.Fatal(err)
			}
			defer assoc.Close()
			pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootGetSOPClassUID)
			if err != nil {
				t.Fatal(err)
			}
			keys, err := dimse.BuildStudyRootStudyFindKeys(nil)
			if err != nil {
				t.Fatal(err)
			}
			var received atomic.Int32
			final, err := dimse.SendCGet(ctx, assoc, pc.ID, dimse.CGetRequest{AffectedSOPClassUID: dimse.StudyRootGetSOPClassUID, MessageID: 1}, object.FromElements(keys, nil), transfer.ImplicitVRLittleEndian, dimse.CGetStoreHandlerFunc(func(_ context.Context, req dimse.CGetStoreRequestContext) (uint16, error) {
				received.Add(1)
				if req.DataSetSyntax.UID != selected.UID {
					return dimse.StatusCGetUnableToProcess, errors.New("C-GET selected syntax differs")
				}
				if err := incrementalReceivedPixels(req.DataSet, req.DataSetSyntax, file, Options{}); err != nil {
					return dimse.StatusCGetUnableToProcess, err
				}
				return dimse.StatusSuccess, nil
			}))
			if err != nil || final == nil || final.Status != dimse.StatusSuccess || received.Load() != 1 {
				t.Fatalf("C-GET final=%+v err=%v received=%d", final, err, received.Load())
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("C-GET peer did not join")
			}
			if native.opts.Resources.SpoolBytes() != 0 {
				t.Fatal("C-GET reservation retained")
			}
			assertEmpty(t, native.opts.SpoolDirectory)
		})
	}
}

func TestStoreIncrementalCMoveNativePreparedPixels(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	file := testFile()
	source, err := NewNativeSource(dimse.NewFileStoreSource(file, 0), NativeOptions{SpoolDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	descriptor, err := source.Inspect(ctx)
	if err != nil {
		t.Fatal(err)
	}
	selected := transfer.ImplicitVRLittleEndian
	storeListener, storeDone, arrivals := incrementalStorePeer(t, ctx, selected, file, Options{})
	defer storeListener.Close()
	queryListener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer queryListener.Close()
	moveDone := make(chan error, 1)
	go func() {
		assoc, err := queryListener.AcceptAssociation(ul.AcceptOptions{AETitle: "MOVESCP", Context: ctx, SupportedAbstractSyntaxes: []string{dimse.StudyRootMoveSOPClassUID}, SupportedTransferSyntaxes: []string{transfer.ImplicitVRLittleEndian.UID}})
		if err != nil {
			moveDone <- err
			return
		}
		defer assoc.Close()
		pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootMoveSOPClassUID)
		if err != nil {
			moveDone <- err
			return
		}
		moveDone <- dimse.ServeStudyRootCMove(ctx, assoc, pc.ID, dimse.CMoveHandlerFunc(func(context.Context, dimse.CMoveRequestContext) ([]dimse.CMoveSubOperation, error) {
			return []dimse.CMoveSubOperation{{AffectedSOPClassUID: descriptor.SOPClassUID, AffectedSOPInstanceUID: descriptor.SOPInstanceUID, Store: func(c context.Context) dimse.CMoveSubOperationResult {
				session, err := dimse.NewStoreSession(storeListener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "MOVESCP"}})
				if err != nil {
					return dimse.CMoveSubOperationResult{Status: dimse.StatusCMoveUnableToProcess, Err: err}
				}
				defer session.Abort(context.Background())
				result, err := session.Store(c, source)
				err = errors.Join(err, session.Close(c))
				if err != nil || result.NegotiatedTransferSyntaxUID != selected.UID {
					return dimse.CMoveSubOperationResult{Status: dimse.StatusCMoveUnableToProcess, Err: errors.Join(err, errors.New("C-MOVE prepared syntax/store failure"))}
				}
				return dimse.CMoveSubOperationResult{Status: dimse.StatusSuccess}
			}}}, nil
		}))
	}()
	assoc, err := ul.DialContext(ctx, queryListener.Addr().String(), ul.DialOptions{CalledAETitle: "MOVESCP", CallingAETitle: "MOVESCU", Contexts: []ul.PresentationContext{dimse.StudyRootMovePresentationContext()}})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	pc, err := dimse.AcceptedContextForSOPClass(assoc, dimse.StudyRootMoveSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	keys, err := dimse.BuildStudyRootStudyFindKeys(nil)
	if err != nil {
		t.Fatal(err)
	}
	progress, err := dimse.SendCMoveWithProgress(ctx, assoc, pc.ID, dimse.CMoveRequest{AffectedSOPClassUID: dimse.StudyRootMoveSOPClassUID, MessageID: 1, MoveDestination: "STORE"}, object.FromElements(keys, nil), transfer.ImplicitVRLittleEndian)
	if err != nil {
		t.Fatal(err)
	}
	var final *dimse.CMoveResponse
	for event := range progress {
		if event.Err != nil {
			t.Fatal(event.Err)
		}
		final = event.Response
	}
	if final == nil || final.Status != dimse.StatusSuccess || arrivals.Load() != 1 {
		t.Fatalf("C-MOVE final=%+v arrivals=%d", final, arrivals.Load())
	}
	for _, done := range []<-chan error{moveDone, storeDone} {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("C-MOVE/store peer did not join")
		}
	}
	if source.opts.Resources.SpoolBytes() != 0 {
		t.Fatal("C-MOVE quota retained")
	}
	assertEmpty(t, source.opts.SpoolDirectory)
}
