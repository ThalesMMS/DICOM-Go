package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/pixeldata/codecfixture"
	"github.com/ThalesMMS/dicom-go/transfer"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func startLayoutReceiver(t *testing.T, args []string) (context.Context, string, context.CancelFunc, <-chan error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	ready := &networkReadyWriter{ready: make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- runServerWithContext(ctx, opts, ready, io.Discard) }()
	select {
	case address := <-ready.ready:
		return ctx, address, cancel, done
	case err := <-done:
		cancel()
		t.Fatalf("startup %v", err)
	case <-ctx.Done():
		cancel()
		t.Fatal("listener did not start")
	}
	return nil, "", nil, nil
}
func finishLayoutReceiver(t *testing.T, cancel context.CancelFunc, done <-chan error) {
	t.Helper()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("receiver did not close")
	}
}
func layoutStore(t *testing.T, ctx context.Context, address, ae string, src *object.Object, syntax transfer.Syntax, raw []byte) uint16 {
	t.Helper()
	class, ok := src.GetUID(core.NewTag(0x0008, 0x0016))
	if !ok {
		t.Fatal("fixture class")
	}
	instance, _ := src.GetUID(core.NewTag(0x0008, 0x0018))
	echoSyntax := transfer.ExplicitVRLittleEndian.UID
	if syntax.UID == transfer.ImplicitVRLittleEndian.UID {
		echoSyntax = syntax.UID
	}
	a, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: ae, CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: class, TransferSyntaxUIDs: []string{syntax.UID}}, {AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{echoSyntax}}}})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	client := dimse.NewStoreClient(a)
	var result dimse.CStoreResult
	if raw == nil {
		result, err = client.StoreWithOptions(ctx, src, dimse.CStoreOptions{AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance, TransferSyntaxUIDs: []string{syntax.UID}})
	} else {
		result, err = client.StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error { _, err := w.Write(raw); return err }, dimse.CStoreOptions{AffectedSOPClassUID: class, AffectedSOPInstanceUID: instance, TransferSyntaxUIDs: []string{syntax.UID}})
	}
	if result.Response == nil || err != nil && result.Response.Status == 0 {
		t.Fatal(err)
	}
	echo, err := dimse.AcceptedContextForSOPClass(a, dimse.VerificationSOPClassUID)
	if err != nil {
		t.Fatal(err)
	}
	if err = dimse.SendCEchoRequest(a, echo.ID, 19); err != nil {
		t.Fatal(err)
	}
	status, err := dimse.ReceiveCEchoResponse(a, echo.ID, 19)
	if err != nil || status != 0 {
		t.Fatalf("post-store echo %04X/%v", status, err)
	}
	if err = a.Release(ctx); err != nil {
		t.Fatal(err)
	}
	return result.Response.Status
}
func TestStorageLayoutLoopbackTemplateAndBitPreserving(t *testing.T) {
	data, err := dicomtest.NewFileBuilder().WithMeta(dicomtest.NewFileMetaBuilder().WithTransferSyntax(transfer.ExplicitVRLittleEndian.UID)).AddElements(dicomtest.DatasetWithPixelData()...).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, rawMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "parsed", true: "raw"}[rawMode], func(t *testing.T) {
			root := t.TempDir()
			args := []string{"-promiscuous", "-address", "127.0.0.1:0", "-output", root, "-path-template", "{StudyInstanceUID}/{SeriesInstanceUID}/{SOPInstanceUID}.dcm"}
			if rawMode {
				args = append(args, "-bit-preserving")
			}
			ctx, address, cancel, done := startLayoutReceiver(t, args)
			defer finishLayoutReceiver(t, cancel, done)
			f, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			var raw []byte
			if rawMode {
				raw = data[132+12+int(binary.LittleEndian.Uint32(data[140:])):]
			}
			for i := 0; i < 2; i++ {
				if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, raw); status != 0 {
					t.Fatalf("store %04X", status)
				}
			}
			study, _ := f.Dataset.GetUID(core.NewTag(0x0020, 0x000D))
			series, _ := f.Dataset.GetUID(core.NewTag(0x0020, 0x000E))
			dir := filepath.Join(root, study, series)
			for _, suffix := range []string{"", ".1"} {
				path := filepath.Join(dir, dicomtest.TestSOPInstanceUID+suffix+".dcm")
				saved, err := object.OpenFile(path)
				if err != nil {
					t.Fatal(err)
				}
				saved.Close()
				if rawMode {
					b, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					offset := 132 + 12 + int(binary.LittleEndian.Uint32(b[140:]))
					if !bytes.Equal(b[offset:], raw) {
						t.Fatal("raw dataset rewritten by naming")
					}
				}
			}
		})
	}
}
func TestStorageLayoutSameCatalogTwoAEAndReceiptEndHooks(t *testing.T) {
	if err := storageHookPlatformReady(); err != nil {
		t.Skip("hooks unsupported")
	}
	root := t.TempDir()
	receipt := filepath.Join(t.TempDir(), "receipt.jsonl")
	end := filepath.Join(t.TempDir(), "end.jsonl")
	args := []string{"-net-config", networkFixturePath(t), "-net-connection", "loopback", "-output", root, "-sort-on-study-uid", "-exec-on-reception", storageHookJSON(storageHelperArgv(t, "reception", receipt, "${path}", "${called_ae}", "${calling_ae}")), "-exec-on-eostudy", storageHookJSON(storageHelperArgv(t, "study", end, "${called_ae}", "${study_uid}", "${generation}", "${count}", "${last_path}")), "-eostudy-timeout", "100ms"}
	ctx, address, cancel, done := startLayoutReceiver(t, args)
	data, _ := dicomtest.ExplicitVRFile()
	for i, tc := range []struct {
		ae, class string
		syntax    transfer.Syntax
	}{{"CT_SCP", "1.2.840.10008.5.1.4.1.1.2", transfer.ExplicitVRLittleEndian}, {"MR_SCP", "1.2.840.10008.5.1.4.1.1.4", transfer.ImplicitVRLittleEndian}} {
		f, err := object.ReadFile(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		f.Dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte(tc.class)))
		f.Dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte("1.2.826.0.1.3680043.10.1354."+string(rune('1'+i)))))
		if status := layoutStore(t, ctx, address, tc.ae, f.Dataset, tc.syntax, nil); status != 0 {
			t.Fatalf("store %04X", status)
		}
		f.Close()
	}
	// Wait on observable hook records, rather than assuming a timeout means study
	// completeness or replacing the unit tracker clock with a longer sleep.
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		b, _ := os.ReadFile(end)
		if bytes.Count(b, []byte{'\n'}) == 2 {
			break
		}
		select {
		case <-ctx.Done():
			finishLayoutReceiver(t, cancel, done)
			t.Fatal("two inactivity events missing")
		case <-tick.C:
		}
	}
	finishLayoutReceiver(t, cancel, done)
	receipts := readStorageHelperRecords(t, receipt)
	ends := readStorageHelperRecords(t, end)
	if len(receipts) != 2 || len(ends) != 2 {
		t.Fatal("hook counts")
	}
	seen := map[string]bool{}
	generations := map[string]bool{}
	for _, r := range receipts {
		if !r.Complete || r.Args[2] != "SYNTHETIC" {
			t.Fatalf("reception before complete %+v", r)
		}
		seen[r.Args[1]] = true
	}
	for _, e := range ends {
		if !seen[e.Args[0]] || e.Args[1] != dicomtest.TestStudyInstanceUID || e.Args[3] != "1" || generations[e.Args[2]] {
			t.Fatalf("per-AE generation %+v", e)
		}
		generations[e.Args[2]] = true
	}
}
func TestStorageLayoutRefusalAndStartupDoNotPublish(t *testing.T) {
	root := t.TempDir()
	ctx, address, cancel, done := startLayoutReceiver(t, []string{"-promiscuous", "-address", "127.0.0.1:0", "-output", root, "-path-template", "{PatientName[99]}.dcm"})
	data, _ := dicomtest.ExplicitVRFile()
	f, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, nil); status != statusCannotUnderstand {
		t.Fatalf("missing metadata %04X", status)
	}
	finishLayoutReceiver(t, cancel, done)
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatal("refusal published")
	}
	opts, err := parseArgs([]string{"-output", root, "-exec-on-reception", storageHookJSON([]string{filepath.Join(root, "missing-helper")})}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err = runServerWithContext(context.Background(), opts, &output, io.Discard); !errors.Is(err, errStorageFeatureUsage) || strings.Contains(output.String(), "listening") {
		t.Fatal("bad hook started listener")
	}
}
func TestStorageLayoutCStoreQuotaBeforePublicationAndRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	root := t.TempDir()
	path, _ := filepath.Abs(os.Args[0])
	opts, err := parseArgs([]string{"-promiscuous", "-exec-on-reception", storageHookJSON([]string{path, "${filename}"}), "-hook-queue-depth", "1"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	release := make(chan struct{})
	calls := 0
	r, err := newStorescpStorageRuntimeWith(opts.storagePolicy, root, storageRuntimeDependencies{manualPoll: true, execute: func(ctx context.Context, _ []string, _ string) error {
		calls++
		if calls == 1 {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	opts.limits.storageRuntime = r
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{Accept: storescpAcceptOptions(opts), Handler: func(ctx context.Context, a *ul.Association) error {
		return handleAssociationWithRuntime(ctx, a, root, io.Discard, opts.limits, newStoreAdmission(1, 1))
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		<-done
		_ = r.Close(shutdown)
	}()
	data, _ := dicomtest.ExplicitVRFile()
	f, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	address := listener.Addr().String()
	if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, nil); status != 0 {
		t.Fatalf("first %04X", status)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("hook not active")
	}
	if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, nil); status != statusOutOfResources {
		t.Fatalf("quota %04X", status)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 1 {
		t.Fatalf("quota produced file: %v", entries)
	}
	close(release)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for r.Metrics().Slots != 0 {
		select {
		case <-ctx.Done():
			t.Fatal("slot not released")
		case <-tick.C:
		}
	}
	if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, nil); status != 0 {
		t.Fatalf("recovery %04X", status)
	}
	entries, _ = os.ReadDir(root)
	if len(entries) != 2 {
		t.Fatal("post-quota store missing")
	}
}
func TestStorageLayoutSingleDrainsFailedHookWithoutChangingStore(t *testing.T) {
	if storageHookPlatformReady() != nil {
		t.Skip("hooks unsupported")
	}
	root := t.TempDir()
	ctx, address, cancel, done := startLayoutReceiver(t, []string{"-promiscuous", "-single", "-address", "127.0.0.1:0", "-output", root, "-exec-on-reception", storageHookJSON(storageHelperArgv(t, "nonzero"))})
	defer cancel()
	data, _ := dicomtest.ExplicitVRFile()
	f, err := object.ReadFile(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if status := layoutStore(t, ctx, address, "STORESCP", f.Dataset, transfer.ExplicitVRLittleEndian, nil); status != 0 {
		t.Fatalf("hook changed store %04X", status)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("single did not drain")
	}
	if _, err := os.Stat(filepath.Join(root, dicomtest.TestSOPInstanceUID+".dcm")); err != nil {
		t.Fatal(err)
	}
}
func TestStorageLayoutRawNativeBigEndianDeflatedAndRLE(t *testing.T) {
	data, err := dicomtest.NewFileBuilder().WithMeta(dicomtest.NewFileMetaBuilder().WithTransferSyntax(transfer.ExplicitVRLittleEndian.UID)).AddElements(dicomtest.DatasetWithPixelData()...).Build()
	if err != nil {
		t.Fatal(err)
	}
	for _, syntax := range []transfer.Syntax{transfer.ImplicitVRLittleEndian, transfer.ExplicitVRBigEndian, transfer.DeflatedExplicitVRLittleEndian, transfer.RLELossless} {
		t.Run(syntax.Name, func(t *testing.T) {
			f, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Fatal(err)
			}
			defer f.Close()
			src := f.Dataset
			if syntax.UID == transfer.RLELossless.UID {
				encoded := codecfixture.RLELosslessSmall().File()
				src = encoded.Dataset
				src.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte(deflatedTestSOPClass)))
				src.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte(dicomtest.TestSOPInstanceUID)))
				src.Put(core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte(dicomtest.TestStudyInstanceUID)))
				src.Put(core.NewRawElement(core.NewTag(0x0020, 0x000E), core.VRUI, []byte(dicomtest.TestSeriesInstanceUID)))
			}
			var raw bytes.Buffer
			if err := object.WriteDataSet(&raw, src, syntax); err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			ctx, address, cancel, done := startLayoutReceiver(t, []string{"-promiscuous", "-accept-all", "-address", "127.0.0.1:0", "-output", root, "-bit-preserving", "-sort-on-study-uid"})
			defer finishLayoutReceiver(t, cancel, done)
			if status := layoutStore(t, ctx, address, "STORESCP", src, syntax, raw.Bytes()); status != 0 {
				t.Fatalf("raw syntax %04X", status)
			}
			path := filepath.Join(root, dicomtest.TestStudyInstanceUID, dicomtest.TestSOPInstanceUID+".dcm")
			saved, err := object.OpenFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if saved.TransferSyntax.UID != syntax.UID {
				t.Fatal("syntax changed")
			}
			if title, ok := saved.Meta.GetString(core.NewTag(0x0002, 0x0016)); !ok || title != "STORESCP" {
				t.Fatal("local meta source author changed")
			}
			saved.Close()
			stored, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			offset := 132 + 12 + int(binary.LittleEndian.Uint32(stored[140:]))
			if !bytes.Equal(stored[offset:], raw.Bytes()) {
				t.Fatal("raw payload changed")
			}
		})
	}
}
func TestStorageLayoutConcurrentCatalogStudiesAndAccounting(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	root := t.TempDir()
	path, _ := filepath.Abs(os.Args[0])
	opts, err := parseArgs([]string{"-sort-on-study-uid", "-exec-on-reception", storageHookJSON([]string{path, "R", "${called_ae}", "${study_uid}"}), "-exec-on-eostudy", storageHookJSON([]string{path, "E", "${called_ae}", "${study_uid}", "${count}"}), "-eostudy-timeout", "1h"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	c, err := loadStorescpNetwork(ctx, storescpNetworkFlags{file: networkFixturePath(t)}, false)
	if err != nil {
		t.Fatal(err)
	}
	bothPrepared := make(chan struct{})
	var formatCalls atomic.Int32
	now := time.Now()
	var mu sync.Mutex
	var events [][]string
	r, err := newStorescpStorageRuntimeWith(opts.storagePolicy, root, storageRuntimeDependencies{manualPoll: true, now: func() time.Time {
		n := formatCalls.Add(1)
		if n == 2 {
			close(bothPrepared)
		}
		if n <= 2 {
			select {
			case <-bothPrepared:
			case <-ctx.Done():
			}
		}
		return now
	}, execute: func(_ context.Context, argv []string, _ string) error {
		mu.Lock()
		events = append(events, append([]string(nil), argv[1:]...))
		mu.Unlock()
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	opts.limits.storageRuntime = r
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	admission := newStoreAdmission(2, 2)
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{Accept: applyStorescpNetwork(storescpAcceptOptions(opts), c), Handler: func(ctx context.Context, a *ul.Association) error {
		return handleAssociationWithRuntime(ctx, a, root, io.Discard, opts.limits, admission)
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	defer func() {
		shutdown, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		_ = server.Shutdown(shutdown)
		<-done
		_ = r.Close(shutdown)
	}()
	data, _ := dicomtest.ExplicitVRFile()
	var wg sync.WaitGroup
	for i, tc := range []struct {
		ae, class, study string
		syntax           transfer.Syntax
	}{{"CT_SCP", "1.2.840.10008.5.1.4.1.1.2", "1.2.826.0.1.3680043.10.1354.10", transfer.ExplicitVRLittleEndian}, {"MR_SCP", "1.2.840.10008.5.1.4.1.1.4", "1.2.826.0.1.3680043.10.1354.20", transfer.ImplicitVRLittleEndian}} {
		i, tc := i, tc
		wg.Add(1)
		go func() {
			defer wg.Done()
			f, err := object.ReadFile(bytes.NewReader(data))
			if err != nil {
				t.Error(err)
				return
			}
			defer f.Close()
			f.Dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0016), core.VRUI, []byte(tc.class)))
			instance := fmt.Sprintf("1.2.826.0.1.3680043.10.1354.30.%d", i+1)
			f.Dataset.Put(core.NewRawElement(core.NewTag(0x0008, 0x0018), core.VRUI, []byte(instance)))
			f.Dataset.Put(core.NewRawElement(core.NewTag(0x0020, 0x000D), core.VRUI, []byte(tc.study)))
			if status := layoutStore(t, ctx, listener.Addr().String(), tc.ae, f.Dataset, tc.syntax, nil); status != 0 {
				t.Errorf("concurrent store %04X", status)
			}
			saved, err := object.OpenFile(filepath.Join(root, tc.study, instance+".dcm"))
			if err != nil {
				t.Error(err)
				return
			}
			saved.Close()
		}()
	}
	wg.Wait()
	if formatCalls.Load() < 4 {
		t.Fatal("two admitted stores did not cross the format barrier")
	}
	r.poll(now.Add(time.Hour))
	closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	if err := r.Close(closeCtx); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(events) != 4 {
		t.Fatalf("events %+v", events)
	}
	seen := map[string]bool{}
	for _, e := range events {
		key := e[1] + "/" + e[2]
		if e[0] == "R" {
			seen[key] = true
		} else if !seen[key] || e[3] != "1" {
			t.Fatalf("end before receipt/wrong count %+v", events)
		}
	}
	if len(seen) != 2 || r.Metrics().Completed != 4 || r.Metrics().Slots != 0 {
		t.Fatalf("accounting %+v/%+v", events, r.Metrics())
	}
}
