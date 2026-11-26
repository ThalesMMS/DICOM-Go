package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clireadopts"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/internal/netstore"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func scMalformed(n int) []byte {
	body := dicomtest.EncodeElements(transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()...)
	for i := 0; i < n; i++ {
		body = append(dicomtest.SequenceControlBytes(binary.LittleEndian, core.NewTag(0xfffe, 0xe00d), 0), body...)
	}
	return body
}
func TestStorescpReadRecoveryDirectAndCap(t *testing.T) {
	for _, c := range []struct {
		flags  []string
		n      int
		wantOK bool
	}{
		{nil, 1, false}, {[]string{"-read-recover-delimiter-anomalies"}, 1, true},
		{[]string{"-read-recover-delimiter-anomalies", "-read-max-boundary-recoveries", "1"}, 2, false},
	} {
		opts, err := parseArgs(c.flags, io.Discard)
		if err != nil {
			t.Fatal(err)
		}
		var diagnostics bytes.Buffer
		opts.limits.readSink = &clireadopts.Sink{Writer: &diagnostics}
		obj, err := receiveDataSetWithLimits(nil, incomingCommand{pcID: 1, dataPrefix: scMalformed(c.n), dataLast: true}, transfer.ExplicitVRLittleEndian, opts.limits)
		if (err == nil) != c.wantOK || (err != nil && obj != nil) {
			t.Fatalf("flags=%v error=%v", c.flags, err)
		}
		if c.wantOK && !strings.Contains(diagnostics.String(), "orphan-delimiter-skipped") {
			t.Fatal("repair was unreported")
		}
		if c.n == 2 && !errors.Is(err, clireadopts.ErrDiagnosticLimit) {
			t.Fatal(err)
		}
	}
}

func TestStorescpMetaReplayBeforeCommitAndAck(t *testing.T) {
	for _, tc := range []struct {
		name             string
		cap              string
		extra, cancelled bool
		ok               bool
	}{{"reported", "128", false, false, true}, {"joint cap", "1", true, false, false}, {"cancelled", "128", false, true, false}} {
		t.Run(tc.name, func(t *testing.T) {
			data, err := dicomtest.ExplicitVRFile()
			if err != nil {
				t.Fatal(err)
			}
			at := bytes.Index(data[132:], []byte{2, 0, 1, 0, 'O', 'B'}) + 132
			if at < 132 {
				t.Fatal("fixture lacks Meta version")
			}
			copy(data[at+4:at+6], "UN")
			if tc.extra {
				patient := bytes.Index(data, []byte{0x10, 0, 0x10, 0, 'P', 'N'})
				if patient < 0 {
					t.Fatal("fixture lacks PatientName")
				}
				copy(data[patient+4:patient+6], "LO")
			}
			opts, err := parseArgs([]string{"-read-prefer-dictionary-vr", "-read-max-boundary-recoveries", tc.cap}, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			dir := t.TempDir()
			stream, err := netstore.BeginStream(context.Background(), dir, dicomtest.TestSOPInstanceUID)
			if err != nil {
				t.Fatal(err)
			}
			defer stream.Abort()
			if _, err := stream.Write(data); err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			opts.limits.readSink = &clireadopts.Sink{Writer: &diagnostics}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tc.cancelled {
				cancel()
			}
			status := verifyStoredStream(ctx, stream,
				dimse.CStoreRequest{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: dicomtest.TestSOPInstanceUID},
				ul.AcceptedContext{AbstractSyntaxUID: dicomtest.TestSOPClassUID}, opts.limits, int64(len(data)))
			if (status == dimse.StatusSuccess) != tc.ok {
				t.Fatalf("status=%04x", status)
			}
			if tc.ok {
				if !strings.Contains(diagnostics.String(), "scope=meta path=(0002,0001) reason=dictionary-vr-preferred") {
					t.Fatal("successful validation omitted Meta repair")
				}
				if _, err := stream.Commit(); err != nil {
					t.Fatal(err)
				}
			} else if err := stream.Abort(); err != nil {
				t.Fatal(err)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != btoi(tc.ok) {
				t.Fatal("refused validation left a publication or temporary")
			}
		})
	}
}

func btoi(v bool) int {
	if v {
		return 1
	}
	return 0
}

func TestStorescpReadRecoveryCoexistsWithFrozenProfile(t *testing.T) {
	path := writeProfileFixture(t, storageProfileFixture)
	base, err := parseArgs([]string{"-config-file", path, "-profile", "P"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := parseArgs([]string{"-config-file", path, "-profile", "P", "-read-prefer-dictionary-vr", "-read-prefer-dictionary-length-field-size", "-read-recover-delimiter-anomalies"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	for _, opts := range []*options{&base, &enabled} {
		opts.associationProfile, err = loadStorescpProfile(context.Background(), *opts)
		if err != nil {
			t.Fatal(err)
		}
	}
	a, b := storescpAcceptOptions(base), storescpAcceptOptions(enabled)
	if !reflect.DeepEqual(a.PresentationContextRules, b.PresentationContextRules) || a.AcceptAnyAbstractSyntax != b.AcceptAnyAbstractSyntax {
		t.Fatal("recovery changed negotiated profile")
	}
	if enabled.limits.maxDataSetBytes != base.limits.maxDataSetBytes || enabled.limits.maxElements != base.limits.maxElements {
		t.Fatal("recovery replaced server admission bounds")
	}
	for _, flags := range [][]string{{"-read-odd-length", "pad"}, {"-read-max-boundary-recoveries", "0"}, {"-read-prefer-dictionary-length-field-size"}, {"-read-allow-implicit-file-meta"}} {
		if _, err := parseArgs(flags, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("accepted %v", flags)
		}
	}
}

func TestStorescpMalformedPDVRecoveryAndFollowUpEcho(t *testing.T) {
	for _, c := range []struct {
		name              string
		bit, enabled      bool
		n, cap            int
		wantOK, continues bool
	}{
		{"parsed strict", false, false, 1, 128, false, false},
		{"parsed repaired", false, true, 1, 128, true, true},
		{"parsed report capped", false, true, 2, 1, false, false},
		{"bit strict", true, false, 1, 128, false, true},
		{"bit admitted original", true, true, 1, 128, true, true},
		{"bit report capped", true, true, 2, 1, false, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			flags := []string{"-read-max-boundary-recoveries", fmtCap(c.cap)}
			if c.enabled {
				flags = append(flags, "-read-recover-delimiter-anomalies")
			}
			if c.bit {
				flags = append(flags, "-bit-preserving")
			}
			opts, err := parseArgs(flags, io.Discard)
			if err != nil {
				t.Fatal(err)
			}
			var diagnostics bytes.Buffer
			opts.limits.readSink = &clireadopts.Sink{Writer: &diagnostics}
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			dir := t.TempDir()
			done := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "STORESCP", Context: ctx, MaxPDU: 4096, AcceptAnyAbstractSyntax: true, SupportedTransferSyntaxes: []string{transfer.ExplicitVRLittleEndian.UID}})
				if err != nil {
					done <- err
					return
				}
				done <- handleAssociationWithLimits(assoc, dir, io.Discard, opts.limits)
			}()
			assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{CalledAETitle: "STORESCP", CallingAETitle: "RECOVERY",
				Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dicomtest.TestSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}, {AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}}}})
			if err != nil {
				t.Fatal(err)
			}
			defer assoc.Conn.Close()
			body := scMalformed(c.n)
			store := func(instance string) uint16 {
				result, storeErr := dimse.NewStoreClient(assoc).StoreEncodedWithOptions(ctx, func(_ context.Context, w io.Writer, _ transfer.Syntax) error { _, err := w.Write(body); return err },
					dimse.CStoreOptions{AffectedSOPClassUID: dicomtest.TestSOPClassUID, AffectedSOPInstanceUID: instance, TransferSyntaxUIDs: []string{transfer.ExplicitVRLittleEndian.UID}})
				if storeErr != nil && result.Response.Status == dimse.StatusSuccess {
					t.Fatal(storeErr)
				}
				return result.Response.Status
			}
			if c.wantOK {
				if status := store("1.2.3.999"); status != statusDataSetDoesNotMatch {
					t.Fatalf("identity mismatch status=%04X", status)
				}
			}
			status := store(dicomtest.TestSOPInstanceUID)
			if (status == dimse.StatusSuccess) != c.wantOK {
				t.Fatalf("status=%04X", status)
			}
			if c.continues {
				pc, ok := dimse.AcceptedVerificationContext(assoc)
				if !ok {
					t.Fatal("echo context missing")
				}
				if err := dimse.SendCEchoRequest(assoc, pc.ID, 11); err != nil {
					t.Fatal(err)
				}
				status, err := dimse.ReceiveCEchoResponse(assoc, pc.ID, 11)
				if err != nil || status != dimse.StatusSuccess {
					t.Fatalf("follow-up echo=%04X %v", status, err)
				}
				if err := assoc.Release(ctx); err != nil {
					t.Fatal(err)
				}
			}
			serverErr := <-done
			if c.continues && serverErr != nil {
				t.Fatal(serverErr)
			}
			if !c.continues && serverErr == nil {
				t.Fatal("parsed error failed to close peer")
			}
			entries, _ := os.ReadDir(dir)
			if c.wantOK {
				if len(entries) != 1 {
					t.Fatal("store output count wrong")
				}
				saved := filepath.Join(dir, entries[0].Name())
				data, err := os.ReadFile(saved)
				if err != nil {
					t.Fatal(err)
				}
				header, err := object.ReadPart10Header(bytes.NewReader(data), object.ReadFileOptions{})
				if err != nil {
					t.Fatal(err)
				}
				if c.bit {
					if !bytes.Equal(data[header.DataSetOffset:], body) {
						t.Fatal("bit-preserving recovery repaired original bytes")
					}
				} else {
					f, err := object.OpenFile(saved)
					if err != nil {
						t.Fatal(err)
					}
					f.Close()
				}
				if !strings.Contains(diagnostics.String(), "orphan-delimiter-skipped") {
					t.Fatal("wire recovery was not reported")
				}
			} else if len(entries) != 0 {
				t.Fatal("refused store left a file")
			}
			if strings.Contains(diagnostics.String(), "TEST^PATIENT") || strings.Contains(diagnostics.String(), dicomtest.TestSOPInstanceUID) {
				t.Fatal("identity or value leaked into recovery diagnostics")
			}
		})
	}
}
func fmtCap(n int) string {
	if n == 1 {
		return "1"
	}
	return "128"
}
