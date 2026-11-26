package storetranscode

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestPreparedStoreUnknownRetryOnlyWhenAuthorized(t *testing.T) {
	for _, retry := range []bool{false, true} {
		t.Run(fmt.Sprint(retry), func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			file := testFile()
			prepared, source, binding, opts := preparedTestSource(t, file, transfer.RLELossless, false)
			if _, err := source.Inspect(ctx); err != nil {
				t.Fatal(err)
			}
			calls := binding.EncoderCalls()
			opts.Transcode.DecoderRegistry = binding.decoders
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			var arrivals atomic.Int32
			go func() {
				attempts := 1
				if retry {
					attempts = 2
				}
				for attempt := range attempts {
					assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: []string{opts.Target.UID}})
					if err != nil {
						done <- err
						return
					}
					async, err := dimse.NewAsyncSession(assoc, dimse.AsyncSessionOptions{Handlers: map[uint16]dimse.AsyncRequestHandler{dimse.CStoreRQ: func(hctx context.Context, session *dimse.AsyncSession, message dimse.AsyncMessage) error {
						arrivals.Add(1)
						if err := incrementalReceivedPixels(message.DataSet, opts.Target, file, opts); err != nil {
							return err
						}
						request, err := dimse.ParseCStoreRequest(message.Command)
						if err != nil {
							return err
						}
						uid, _ := message.DataSet.GetUID(core.NewTag(0x0008, 0x0018))
						if uid != request.AffectedSOPInstanceUID {
							return errors.New("command/dataset UID mismatch")
						}
						if attempt == 0 {
							_ = assoc.Abort(0)
							return nil
						}
						return session.Respond(hctx, message, (dimse.CStoreResponse{AffectedSOPClassUID: request.AffectedSOPClassUID, AffectedSOPInstanceUID: request.AffectedSOPInstanceUID, Status: dimse.StatusSuccess}).CommandSet(), nil)
					}}})
					if err != nil {
						_ = assoc.Close()
						done <- err
						return
					}
					<-async.Done()
					terminal := async.Err()
					_ = async.Close()
					_ = assoc.Close()
					if attempt > 0 && !errors.Is(terminal, dimse.ErrAssociationReleased) && terminal != nil {
						done <- terminal
						return
					}
				}
				done <- nil
			}()
			session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "SCU"}, RetryUncertain: retry, MaxStoreAttempts: 2})
			if err != nil {
				t.Fatal(err)
			}
			result, storeErr := session.StoreBatch(ctx, []dimse.StoreSource{source})
			if retry {
				if storeErr != nil || result.Succeeded != 1 || result.Unknown != 0 || arrivals.Load() != 2 {
					t.Fatal("authorized retry", result, storeErr)
				}
			} else {
				if !errors.Is(storeErr, dimse.ErrStoreUncertain) || result.Unknown != 1 || arrivals.Load() != 1 {
					t.Fatal("unauthorized unknown replay", result, storeErr)
				}
			}
			cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), time.Second)
			_ = session.Close(cleanupCtx)
			cleanupCancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("peer not joined")
			}
			if binding.EncoderCalls() != calls {
				t.Fatal("transport retry repeatedencoder")
			}
			if err := prepared.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if prepared.Stats().RetainedBytes != 0 {
				t.Fatal("transportterminal did notcleanup")
			}
		})
	}
}
func TestPreparedStoreRejectedAssociationReleasesSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	prepared, source, binding, _ := preparedTestSource(t, testFile(), transfer.RLELossless, false)
	if _, err := source.Inspect(ctx); err != nil {
		t.Fatal(err)
	}
	calls := binding.EncoderCalls()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "EXPECTED", RequireMatchingCalledAE: true, Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: []string{transfer.RLELossless.UID}})
		if assoc != nil {
			_ = assoc.Close()
		}
		done <- err
	}()
	session, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "DIFFERENT", CallingAETitle: "SCU"}, MaxAssociationAttempts: 1, DisableReconnect: true})
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.StoreBatch(ctx, []dimse.StoreSource{source})
	if err == nil || result.Succeeded != 0 || len(result.Items) > 0 && result.Items[0].Attempted {
		t.Fatal("rejectedassociation transmitted", result, err)
	}
	if binding.EncoderCalls() != calls {
		t.Fatal("rejection repeatedencoder")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("peer unexpectedly accepted")
		}
	case <-ctx.Done():
		t.Fatal("rejectpeer notjoined")
	}
	cleanupCtx, stop := context.WithTimeout(context.Background(), time.Second)
	defer stop()
	_ = session.Close(cleanupCtx)
	if err := prepared.Close(cleanupCtx); err != nil {
		t.Fatal(err)
	}
}

// Invalidation happens after successful planning and accepted association, at
// the transport's actual Open boundary. No direct test call sends C-STORE.
type preparedInvalidateAtOpen struct {
	base   *PreparedSource
	before func()
}

func (s preparedInvalidateAtOpen) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	return s.base.Inspect(ctx)
}
func (s preparedInvalidateAtOpen) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	s.before()
	return s.base.Open(ctx)
}
func TestPreparedStoreInvalidationBeforeCStoreCommand(t *testing.T) {
	for _, kind := range []string{"binding", "pixels"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			file := testFile()
			prepared, source, binding, _ := preparedTestSource(t, file, transfer.RLELossless, false)
			if _, err := source.Inspect(ctx); err != nil {
				t.Fatal(err)
			}
			calls := binding.EncoderCalls()
			var accepted, commands atomic.Int32
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{"1.2.840.10008.5.1.4.1.1.7"}, SupportedTransferSyntaxes: []string{transfer.RLELossless.UID}})
				if err != nil {
					done <- err
					return
				}
				accepted.Add(1)
				async, err := dimse.NewAsyncSession(assoc, dimse.AsyncSessionOptions{Handlers: map[uint16]dimse.AsyncRequestHandler{dimse.CStoreRQ: func(context.Context, *dimse.AsyncSession, dimse.AsyncMessage) error {
					commands.Add(1)
					return errors.New("invalid source reached command")
				}}})
				if err != nil {
					_ = assoc.Close()
					done <- err
					return
				}
				<-async.Done()
				terminal := async.Err()
				_ = async.Close()
				_ = assoc.Close()
				if errors.Is(terminal, dimse.ErrAssociationReleased) {
					terminal = nil
				}
				done <- terminal
			}()
			wrapped := preparedInvalidateAtOpen{source, func() {
				if kind == "binding" {
					_ = binding.Close()
				} else {
					e, _ := file.Dataset.Get(core.TagPixelData)
					raw, _ := e.RawBytes()
					raw = append([]byte(nil), raw...)
					raw[0] ^= 1
					file.Dataset.Put(core.NewRawElement(core.TagPixelData, e.VR(), raw))
				}
			}}
			transport, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "SCU"}, DisableReconnect: true, MaxStoreAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			result, err := transport.StoreBatch(ctx, []dimse.StoreSource{wrapped})
			if err == nil || result.Succeeded != 0 {
				t.Fatal("invalid source delivered", result, err)
			}
			cleanup, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := transport.Close(cleanup); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("peer not joined")
			}
			if accepted.Load() != 1 || commands.Load() != 0 || binding.EncoderCalls() != calls {
				t.Fatal("invalidation did not precede CSTORE", accepted.Load(), commands.Load(), binding.EncoderCalls(), calls)
			}
			if err := prepared.Close(cleanup); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// This proxy changes only the public source's correlation Origin; the rooted
// payload provider, file bytes, UIDs and every other descriptor field stay real.
type preparedOriginProxy struct {
	base     dimse.StoreSource
	mu       sync.Mutex
	override *string
}

func (p *preparedOriginProxy) setOrigin(origin string) {
	p.mu.Lock()
	p.override = &origin
	p.mu.Unlock()
}
func (p *preparedOriginProxy) descriptor(d dimse.StoreDescriptor) dimse.StoreDescriptor {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.override != nil {
		d.Origin = *p.override
	}
	return d
}
func (p *preparedOriginProxy) Inspect(ctx context.Context) (dimse.StoreDescriptor, error) {
	d, err := p.base.Inspect(ctx)
	return p.descriptor(d), err
}
func (p *preparedOriginProxy) Open(ctx context.Context) (dimse.OpenedStoreSource, error) {
	opened, err := p.base.Open(ctx)
	if err == nil {
		opened.Descriptor = p.descriptor(opened.Descriptor)
	}
	return opened, err
}
func TestPreparedStoreOriginChangeBeforeCStoreCommand(t *testing.T) {
	for _, origin := range []string{"changed-origin", ""} {
		name := "changed"
		if origin == "" {
			name = "unavailable"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			file := testFile()
			var part10 bytes.Buffer
			if err := object.WriteFile(&part10, file); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			path := filepath.Join(root, "native.dcm")
			if err := os.WriteFile(path, part10.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			proxy := &preparedOriginProxy{base: dimse.NewRootedPathStoreSource(root, "native.dcm")}
			capture := func() (dimse.StoreDescriptor, []byte) {
				t.Helper()
				opened, err := proxy.Open(ctx)
				if err != nil {
					t.Fatal(err)
				}
				var wire bytes.Buffer
				writeErr := opened.WriteDataSet(ctx, &wire, transfer.ExplicitVRLittleEndian)
				closeErr := opened.Close()
				if err := errors.Join(writeErr, closeErr); err != nil {
					t.Fatal(err)
				}
				return opened.Descriptor, wire.Bytes()
			}
			original, beforeBytes := capture()
			if original.Origin == "" {
				t.Fatal("rooted source has no original Origin")
			}
			binding, err := NewBuiltinPreparedBinding(BuiltinPreparedBindingOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer binding.Close()
			owner, err := NewPreparedSession(context.Background(), PreparedSessionOptions{MaxRetainedBytes: 4 << 20})
			if err != nil {
				t.Fatal(err)
			}
			spool := t.TempDir()
			defer func() {
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				if err := owner.Close(cleanup); err != nil {
					t.Error(err)
				}
			}()
			source, err := owner.NewSource(proxy, Options{Target: transfer.RLELossless, MaxSpoolBytes: 1 << 20, SpoolDirectory: spool}, binding)
			if err != nil {
				t.Fatal(err)
			}
			planned, err := source.Inspect(ctx)
			if err != nil {
				t.Fatal(err)
			}
			lease, err := source.Open(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var selected bytes.Buffer
			writeErr := lease.WriteDataSet(ctx, &selected, transfer.RLELossless)
			closeErr := lease.Close()
			if err := errors.Join(writeErr, closeErr); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(planned, lease.Descriptor) || selected.Len() == 0 {
				t.Fatal("selected representation invalid before Origin change")
			}
			snapshot := func() map[string][32]byte {
				t.Helper()
				hashes := map[string][32]byte{}
				err := filepath.WalkDir(spool, func(p string, d os.DirEntry, err error) error {
					if err != nil {
						return err
					}
					if d.IsDir() {
						return nil
					}
					raw, err := os.ReadFile(p)
					if err != nil {
						return err
					}
					relative, err := filepath.Rel(spool, p)
					if err != nil {
						return err
					}
					hashes[relative] = sha256.Sum256(raw)
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				return hashes
			}
			spoolsBefore := snapshot()
			calls := binding.EncoderCalls()
			if calls == 0 {
				t.Fatal("real encoder was not used")
			}
			retained := owner.Stats().RetainedBytes
			var accepted, commands, changed atomic.Int32
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORE", Context: ctx, SupportedAbstractSyntaxes: []string{original.SOPClassUID}, SupportedTransferSyntaxes: []string{transfer.RLELossless.UID}})
				if err != nil {
					done <- err
					return
				}
				accepted.Add(1)
				async, err := dimse.NewAsyncSession(assoc, dimse.AsyncSessionOptions{Handlers: map[uint16]dimse.AsyncRequestHandler{dimse.CStoreRQ: func(context.Context, *dimse.AsyncSession, dimse.AsyncMessage) error {
					commands.Add(1)
					return errors.New("changed Origin reached CSTORE")
				}}})
				if err != nil {
					_ = assoc.Close()
					done <- err
					return
				}
				<-async.Done()
				terminal := async.Err()
				_ = async.Close()
				_ = assoc.Close()
				if errors.Is(terminal, dimse.ErrAssociationReleased) {
					terminal = nil
				}
				done <- terminal
			}()
			wrapped := preparedInvalidateAtOpen{source, func() { proxy.setOrigin(origin); changed.Add(1) }}
			transport, err := dimse.NewStoreSession(listener.Addr().String(), dimse.StoreSessionOptions{DialOptions: ul.DialOptions{CalledAETitle: "STORE", CallingAETitle: "SCU"}, DisableReconnect: true, MaxStoreAttempts: 1})
			if err != nil {
				t.Fatal(err)
			}
			result, storeErr := transport.StoreBatch(ctx, []dimse.StoreSource{wrapped})
			cleanup, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			if err := transport.Close(cleanup); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("Origin peer not joined")
			}
			if !errors.Is(storeErr, dimse.ErrStoreSourceChanged) || result.Succeeded != 0 || result.Unknown != 0 || result.Failed != 1 || len(result.Items) != 1 || result.Items[0].Attempted {
				t.Fatal("Origin refusal was not before command", result, storeErr)
			}
			after, afterBytes := capture()
			if after.Origin != origin {
				t.Fatal("public provider Origin did not change")
			}
			after.Origin = original.Origin
			if !reflect.DeepEqual(original, after) || !bytes.Equal(beforeBytes, afterBytes) {
				t.Fatal("test changed fields or payload besides Origin")
			}
			actualFile, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if sha256.Sum256(actualFile) != sha256.Sum256(part10.Bytes()) || !reflect.DeepEqual(spoolsBefore, snapshot()) {
				t.Fatal("original bytes or selected output changed")
			}
			if accepted.Load() != 1 || commands.Load() != 0 || changed.Load() != 1 || binding.EncoderCalls() != calls || owner.Stats().RetainedBytes != retained {
				t.Fatal("Origin change reencoded/sent/lostretention", accepted.Load(), commands.Load(), changed.Load(), binding.EncoderCalls(), calls, owner.Stats())
			}
			if err := owner.Close(cleanup); err != nil {
				t.Fatal(err)
			}
			if stats := owner.Stats(); stats.Entries != 0 || stats.ReservedBytes != 0 || stats.RetainedBytes != 0 {
				t.Fatal("Origin failure retained leases/quota", stats)
			}
			assertEmpty(t, spool)
		})
	}
}
