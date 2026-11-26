package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/dicomtags"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func networkFixturePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../net/netconf/testdata/two-aes.json")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type networkReadyWriter struct {
	ready    chan string
	accepted atomic.Int32
}

func (w *networkReadyWriter) Write(p []byte) (int, error) {
	s := string(p)
	if strings.HasPrefix(s, "storescp listening on ") {
		select {
		case w.ready <- strings.TrimSpace(strings.TrimPrefix(s, "storescp listening on ")):
		default:
		}
	}
	if strings.HasPrefix(s, "association accepted from ") {
		w.accepted.Add(1)
	}
	return len(p), nil
}
func TestStorescpNetworkCLIRealListener(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	out := t.TempDir()
	opts, err := parseArgs([]string{"-net-config", networkFixturePath(t), "-net-connection", "loopback", "-output", out}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stdout := &networkReadyWriter{ready: make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- runServerWithContext(ctx, opts, stdout, io.Discard) }()
	var address string
	select {
	case address = <-stdout.ready:
	case err = <-done:
		t.Fatalf("startup: %v", err)
	case <-ctx.Done():
		t.Fatal("listener startup timeout")
	}
	for _, tc := range []struct{ ae, ts string }{{"CT_SCP", ul.ExplicitVRLittleEndian}, {"MR_SCP", ul.ImplicitVRLittleEndian}} {
		a, err := ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: tc.ae, CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{tc.ts}}}})
		if err != nil {
			t.Fatal(err)
		}
		pc, err := dimse.AcceptedContextForSOPClass(a, dimse.VerificationSOPClassUID)
		if err != nil {
			t.Fatal(err)
		}
		if err = dimse.SendCEchoRequest(a, pc.ID, 12); err != nil {
			t.Fatal(err)
		}
		status, err := dimse.ReceiveCEchoResponse(a, pc.ID, 12)
		if err != nil || status != dimse.StatusSuccess {
			t.Fatalf("echo: %v/%v", status, err)
		}
		if err = a.Release(ctx); err != nil {
			t.Fatal(err)
		}
	}
	_, err = ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "ct_scp", CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian}}}})
	var rejection *ul.RejectionError
	if !errors.As(err, &rejection) || rejection.Reason != ul.AssociateRJReasonCalledAETitleNotRecognized {
		t.Fatalf("unknown AE: %v", err)
	}
	if stdout.accepted.Load() != 2 {
		t.Fatalf("unknown AE reached handler: %d", stdout.accepted.Load())
	}
	entries, err := os.ReadDir(out)
	if err != nil || len(entries) != 0 {
		t.Fatalf("ECHO/unknown published files: %v/%v", entries, err)
	}
	cancel()
	select {
	case err = <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("receiver did not stop")
	}
}
func TestStorescpNetworkFlagConflicts(t *testing.T) {
	for _, args := range [][]string{{"-net-connection", "loopback"}, {"-net-config", ""}, {"-net-config", "x", "-address", "127.0.0.1:0"}, {"-net-config", "x", "-aetitle", "AE"}, {"-net-config", "x", "-profile", "P"}, {"-net-config", "x", "-accept-all=false"}, {"-net-config", "x", "-prefer-rle=false"}} {
		if _, err := parseArgs(args, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("accepted flags %v: %v", args, err)
		}
	}
	opts, err := parseArgs([]string{"-net-config", networkFixturePath(t), "-net-connection", "loopback", "-bit-preserving"}, io.Discard)
	if err != nil || opts.network.connection != "loopback" || !opts.limits.bitPreserving {
		t.Fatalf("valid flags %+v/%v", opts.network, err)
	}
}
func TestStorescpNetworkConstraintsAndCancellation(t *testing.T) {
	p := networkFixturePath(t)
	c, err := loadStorescpNetwork(context.Background(), storescpNetworkFlags{file: p}, false)
	if err != nil {
		t.Fatal(err)
	}
	legacy, _ := parseArgs(nil, io.Discard)
	accept := applyStorescpNetwork(storescpAcceptOptions(legacy), c)
	if accept.CalledAERegistry == nil || len(accept.PresentationContextRules) != 0 || len(accept.SupportedAbstractSyntaxes) != 0 || accept.AcceptAnyAbstractSyntax {
		t.Fatal("exclusive policy broadened")
	}
	if _, err = loadStorescpNetwork(context.Background(), storescpNetworkFlags{file: p}, true); err == nil {
		t.Fatal("TLS mismatch accepted")
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for name, source := range map[string]string{"role": strings.Replace(string(raw), "\"role\":\"SCP\"", "\"role\":\"SCU\"", 1), "service": strings.Replace(string(raw), "1.2.840.10008.1.1", "1.2.3.4", 1), "TS": strings.Replace(string(raw), ul.ExplicitVRLittleEndian, "1.2.3.99", 1)} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "synthetic.json")
			if err := os.WriteFile(path, []byte(source), 0600); err != nil {
				t.Fatal(err)
			}
			if c, err := loadStorescpNetwork(context.Background(), storescpNetworkFlags{file: path}, false); c != nil || err == nil {
				t.Fatalf("unsupported claim %v/%v", c, err)
			}
			out := filepath.Join(t.TempDir(), "must-not-exist")
			opts, err := parseArgs([]string{"-net-config", path, "-output", out}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			if err = runServerWithContext(context.Background(), opts, io.Discard, io.Discard); err == nil {
				t.Fatal("unsupported mode started")
			}
			if _, err = os.Stat(out); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("configuration failure published directory: %v", err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if c, err = loadStorescpNetwork(ctx, storescpNetworkFlags{file: p}, false); c != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel %v", err)
	}
}
func TestStorescpSameJSONTwoAEStoreAndEcho(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	c, err := loadStorescpNetwork(ctx, storescpNetworkFlags{file: networkFixturePath(t)}, false)
	if err != nil {
		t.Fatal(err)
	}
	opts, _ := parseArgs(nil, io.Discard)
	listener, err := ul.Listen(ul.ListenOptions{Address: c.Address(), Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	server, err := ul.NewAssociationServer(listener, ul.AssociationServerOptions{Accept: applyStorescpNetwork(storescpAcceptOptions(opts), c), Handler: func(ctx context.Context, a *ul.Association) error {
		return handleAssociationWithRuntime(ctx, a, out, io.Discard, opts.limits, newStoreAdmission(2, 4))
	}})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(ctx) }()
	t.Cleanup(func() {
		shutdown, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
		<-done
	})
	data, err := dicomtest.ExplicitVRFile()
	if err != nil {
		t.Fatal(err)
	}
	for i, tc := range []struct {
		ae, sop string
		ts      transfer.Syntax
	}{{"CT_SCP", "1.2.840.10008.5.1.4.1.1.2", transfer.ExplicitVRLittleEndian}, {"MR_SCP", "1.2.840.10008.5.1.4.1.1.4", transfer.ImplicitVRLittleEndian}} {
		file, err := object.ReadFile(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		instance := []string{"1.2.826.0.1.3680043.10.99.1332.1", "1.2.826.0.1.3680043.10.99.1332.2"}[i]
		file.Dataset.Put(core.NewRawElement(dicomtags.SOPClassUID, core.VRUI, []byte(tc.sop)))
		file.Dataset.Put(core.NewRawElement(dicomtags.SOPInstanceUID, core.VRUI, []byte(instance)))
		a, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: tc.ae, CallingAETitle: "SYNTHETIC", Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{tc.ts.UID}}, {AbstractSyntaxUID: tc.sop, TransferSyntaxUIDs: []string{tc.ts.UID}}}})
		if err != nil {
			t.Fatal(err)
		}
		echo, err := dimse.AcceptedContextForSOPClass(a, dimse.VerificationSOPClassUID)
		if err != nil {
			t.Fatal(err)
		}
		if err = dimse.SendCEchoRequest(a, echo.ID, 10); err != nil {
			t.Fatal(err)
		}
		status, err := dimse.ReceiveCEchoResponse(a, echo.ID, 10)
		if err != nil || status != dimse.StatusSuccess {
			t.Fatalf("echo %v/%v", status, err)
		}
		pc, err := dimse.AcceptedContextForSOPClass(a, tc.sop)
		if err != nil {
			t.Fatal(err)
		}
		if pc.TransferSyntaxUID != tc.ts.UID {
			t.Fatal("wrong negotiated syntax")
		}
		if err = dimse.SendCStoreRequest(a, pc.ID, dimse.CStoreRequest{MessageID: 11, AffectedSOPClassUID: tc.sop, AffectedSOPInstanceUID: instance}); err != nil {
			t.Fatal(err)
		}
		if err = dimse.SendDataSet(a, pc.ID, file.Dataset, tc.ts); err != nil {
			t.Fatal(err)
		}
		rsp, err := dimse.ReceiveCStoreResponse(a, pc.ID)
		if err != nil || rsp.Status != dimse.StatusSuccess {
			t.Fatalf("store %+v/%v", rsp, err)
		}
		if err = a.Release(ctx); err != nil {
			t.Fatal(err)
		}
		saved, err := object.OpenFile(filepath.Join(out, instance+".dcm"))
		if err != nil {
			t.Fatal(err)
		}
		if got, ok := saved.GetUID(dicomtags.SOPClassUID); !ok || got != tc.sop {
			t.Fatal("stored SOP differs")
		}
		if saved.TransferSyntax.UID != tc.ts.UID {
			t.Fatal("stored syntax differs")
		}
		for _, tag := range []core.Tag{core.NewTag(0x0002, 0x0016), core.NewTag(0x0002, 0x0018)} {
			v, ok := saved.Meta.GetString(tag)
			if !ok || v != tc.ae {
				t.Fatalf("selected local identity %v=%q/%v", tag, v, ok)
			}
		}
	}
}
