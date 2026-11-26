package main

import (
	"bytes"
	"context"
	"errors"
	"github.com/ThalesMMS/dicom-go/internal/dicomtest"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
	"os"
	"strings"
	"testing"
	"time"
)

func TestStorescuPreparedSessionOptInDefaults(t *testing.T) {
	defaults, err := parseArgs([]string{"127.0.0.1:104", "synthetic.dcm"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if defaults.preparedSession || defaults.preparedSessionBytes != 0 {
		t.Fatal("default enabledreuse")
	}
	opts, err := parseArgs([]string{"-transcode-to", "1.2.840.10008.1.2.5", "-transcode-prepared-session", "-transcode-prepared-max-bytes", "4096", "127.0.0.1:104", "synthetic.dcm"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if !opts.preparedSession || opts.preparedSessionBytes != 4096 {
		t.Fatal("optin flags lost")
	}
	for _, flags := range [][]string{{"-transcode-prepared-session"}, {"-transcode-prepared-max-bytes", "4096"}, {"-transcode-to", "1.2.840.10008.1.2.5", "-transcode-prepared-session", "-transcode-prepared-max-bytes", "-1"}} {
		args := append(append([]string(nil), flags...), "127.0.0.1:104", "synthetic.dcm")
		if _, err := parseArgs(args, &bytes.Buffer{}); !errors.Is(err, errUsage) {
			t.Fatal("invalid prepared policy accepted", flags, err)
		}
	}
}

func TestStorescuPreparedSessionRejectedAssociationCleanup(t *testing.T) {
	for _, mode := range []struct {
		name            string
		continueOnError bool
		flags           []string
	}{
		{"stop-first", false, []string{"-continue-on-error=false"}},
		{"default", true, nil},
	} {
		t.Run(mode.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			// ExplicitVRFile is deliberately minimal and has no pixels. A valid native
			// image is necessary here so the CLI reaches negotiation after preparation.
			data, err := dicomtest.Part10File(transfer.ExplicitVRLittleEndian, dicomtest.DatasetWithPixelData()...)
			if err != nil {
				t.Fatal(err)
			}
			path := tempDICOMFile(t, data)
			spool := t.TempDir()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			type peerResult struct {
				err        error
				rejected   int
				associated bool
			}
			done := make(chan peerResult, 1)
			go func() {
				rejected := 0
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{Context: ctx, AETitle: "EXPECTED", RequireMatchingCalledAE: true, AcceptAnyAbstractSyntax: true, OnAssociationRejected: func() { rejected++ }})
				associated := assoc != nil
				if assoc != nil {
					_ = assoc.Close()
				}
				done <- peerResult{err, rejected, associated}
			}()
			args := append(append([]string(nil), mode.flags...), []string{"-called", "DIFFERENT", "-transcode-to", "1.2.840.10008.1.2.5", "-transcode-prepared-session", "-transcode-prepared-max-bytes", "1048576", "-transcode-spool-dir", spool, listener.Addr().String(), path}...)
			opts, err := parseArgs(args, &bytes.Buffer{})
			if err != nil {
				t.Fatal(err)
			}
			var stdout, stderr bytes.Buffer
			runErr := runStoreWithContext(ctx, opts, &stdout, &stderr)
			// Unblock a peer even if preflight failed before a connection. Such a failure
			// is reported as the wrong concrete result, never accepted as a rejection.
			_ = listener.Close()
			var peer peerResult
			select {
			case peer = <-done:
			case <-ctx.Done():
				t.Fatalf("rejectpeer not joined: client=%T %v stdout=%q stderr=%q", runErr, runErr, stdout.String(), stderr.String())
			}
			assertRejection := func(label string, err error) {
				t.Helper()
				var rejection *ul.RejectionError
				if !errors.As(err, &rejection) || !errors.Is(err, ul.ErrAssociationRejected) || rejection.Result != ul.AssociateRJResultPermanent || rejection.Source != ul.AssociateRJSourceServiceUser || rejection.Reason != ul.AssociateRJReasonCalledAETitleNotRecognized {
					t.Fatalf("%s: wrong rejection: error=%T %v client=%T %v stdout=%q stderr=%q", label, err, err, runErr, runErr, stdout.String(), stderr.String())
				}
			}
			if opts.continueOnError != mode.continueOnError {
				t.Fatal("CLI continue-on-error mode changed")
			}
			if mode.continueOnError {
				// ContinueOnError reports item failures and returns the CLI aggregate error;
				// it intentionally does not return the typed per-item association rejection.
				if runErr == nil || errors.Is(runErr, dimse.ErrStoreUncertain) || !strings.Contains(runErr.Error(), "C-STORE batch completed with 1 failed and 0 uncertain") || !strings.Contains(stdout.String(), "outcome=failure") || strings.Contains(stdout.String(), "outcome=unknown") {
					t.Fatalf("default batch outcome wrong: client=%T %v stdout=%q stderr=%q", runErr, runErr, stdout.String(), stderr.String())
				}
			} else {
				assertRejection("client", runErr)
			}
			assertRejection("peer", peer.err)
			if opts.calledAE != "DIFFERENT" || peer.rejected != 1 || peer.associated {
				t.Fatalf("wrong actual rejection evidence: called=%q peer=%+v", opts.calledAE, peer)
			}
			entries, err := os.ReadDir(spool)
			if err != nil || len(entries) != 0 {
				t.Fatal("CLI retainedprivate spools afterfailure", len(entries), err)
			}
		})
	}
}
