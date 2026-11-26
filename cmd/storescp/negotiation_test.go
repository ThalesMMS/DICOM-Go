package main

import (
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/transfer"
)

const compressedTestSOPClass = "1.2.840.10008.5.1.4.1.1.7"

func TestStorescpTransferSyntaxFlagValidation(t *testing.T) {
	for _, args := range [][]string{
		{"-prefer-jls", "-prefer-rle"},
		{"-prefer-jls", "-prefer-transfer-syntaxes", transfer.RLELossless.UID},
		{"-accept-all", "-transfer-syntaxes", transfer.RLELossless.UID},
		{"-transfer-syntaxes", ""}, {"-prefer-transfer-syntaxes", " "},
		{"-transfer-syntaxes", "1.2.826.0.1.3680043.10.1284.999"},
		{"-transfer-syntaxes", transfer.XMLEncoding.UID},
		{"-transfer-syntaxes", transfer.RLELossless.UID + ","},
		{"-prefer-transfer-syntaxes", transfer.RLELossless.UID + "," + transfer.RLELossless.UID},
		{"-transfer-syntaxes", transfer.RLELossless.UID, "-prefer-jls"},
	} {
		if _, err := parseArgs(args, io.Discard); !errors.Is(err, errUsage) {
			t.Fatalf("invalid syntax options accepted: %v", args)
		}
	}
	options, err := parseArgs([]string{"-accept-all"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, uid := range options.transferSyntaxUIDs {
		if seen[uid] {
			t.Fatal("duplicate accepted syntax")
		}
		seen[uid] = true
		if syntax, ok := transfer.DefaultRegistry.Get(uid); !ok || !syntax.Supported {
			t.Fatal("unreadable envelope accepted")
		}
	}
	for _, syntax := range transfer.DefaultRegistry.All() {
		if seen[syntax.UID] != syntax.Supported {
			t.Fatalf("accept-all envelope classification differs for %s", syntax.Name)
		}
	}
	if got := options.transferSyntaxUIDs[:3]; !reflect.DeepEqual(got, nativeTransferSyntaxUIDs()) {
		t.Fatalf("accept-all changed default native preference: %v", got)
	}
}

func negotiateStorescp(t *testing.T, args []string, abstract string, syntaxes []string) []ul.AcceptedContext {
	t.Helper()
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		accept := storescpAcceptOptions(opts)
		accept.Context = ctx
		assoc, err := listener.AcceptAssociation(accept)
		if err != nil {
			done <- err
			return
		}
		done <- handleAssociation(assoc, t.TempDir(), io.Discard)
	}()
	assoc, err := ul.DialContext(ctx, listener.Addr().String(), ul.DialOptions{
		CalledAETitle: "STORESCP", CallingAETitle: "SYNTHETIC",
		Contexts: []ul.PresentationContext{
			{AbstractSyntaxUID: abstract, TransferSyntaxUIDs: syntaxes},
			{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{transfer.RLELossless.UID, transfer.ImplicitVRLittleEndian.UID}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	accepted := append([]ul.AcceptedContext(nil), assoc.AcceptedContexts...)
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	return accepted
}

func TestStorescpNegotiationPreferences(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		proposed []string
		want     string
	}{
		{"native default priority", nil, []string{transfer.RLELossless.UID, transfer.ExplicitVRLittleEndian.UID, transfer.ImplicitVRLittleEndian.UID}, transfer.ImplicitVRLittleEndian.UID},
		{"JPEG Lossless only", nil, []string{transfer.JPEGLosslessSV1.UID}, transfer.JPEGLosslessSV1.UID},
		{"JPEG-LS only", nil, []string{transfer.JPEGLSLossless.UID}, transfer.JPEGLSLossless.UID},
		{"J2K only", nil, []string{transfer.JPEG2000LosslessOnly.UID}, transfer.JPEG2000LosslessOnly.UID},
		{"RLE only", nil, []string{transfer.RLELossless.UID}, transfer.RLELossless.UID},
		{"prefer lossless", []string{"-prefer-lossless"}, []string{transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLosslessSV1.UID}, transfer.JPEGLosslessSV1.UID},
		{"prefer JPEG8", []string{"-prefer-jpeg8"}, []string{transfer.ImplicitVRLittleEndian.UID, transfer.JPEGBaseline.UID}, transfer.JPEGBaseline.UID},
		{"prefer JPEG12", []string{"-prefer-jpeg12"}, []string{transfer.ImplicitVRLittleEndian.UID, transfer.JPEGExtended.UID}, transfer.JPEGExtended.UID},
		{"prefer JLS", []string{"-prefer-jls"}, []string{transfer.ExplicitVRLittleEndian.UID, transfer.JPEGLSNearLossless.UID, transfer.JPEGLSLossless.UID}, transfer.JPEGLSLossless.UID},
		{"prefer J2K", []string{"-prefer-j2k"}, []string{transfer.ExplicitVRLittleEndian.UID, transfer.JPEG2000.UID, transfer.JPEG2000LosslessOnly.UID}, transfer.JPEG2000LosslessOnly.UID},
		{"prefer RLE", []string{"-prefer-rle"}, []string{transfer.ImplicitVRLittleEndian.UID, transfer.RLELossless.UID}, transfer.RLELossless.UID},
		{"prefer HEVC envelope", []string{"-prefer-hevc"}, []string{transfer.ImplicitVRLittleEndian.UID, transfer.HEVCMP51.UID}, transfer.HEVCMP51.UID},
		{"ordered UID preference", []string{"-prefer-transfer-syntaxes", transfer.RLELossless.UID + "," + transfer.JPEGLSLossless.UID}, []string{transfer.JPEGLSLossless.UID, transfer.ImplicitVRLittleEndian.UID, transfer.RLELossless.UID}, transfer.RLELossless.UID},
		{"ordered restricted list", []string{"-transfer-syntaxes", transfer.JPEGLSLossless.UID + "," + transfer.RLELossless.UID}, []string{transfer.RLELossless.UID, transfer.JPEGLSLossless.UID}, transfer.JPEGLSLossless.UID},
		{"restricted subset of family", []string{"-transfer-syntaxes", transfer.ImplicitVRLittleEndian.UID + "," + transfer.JPEGLSLossless.UID, "-prefer-jls"}, []string{transfer.ImplicitVRLittleEndian.UID, transfer.JPEGLSLossless.UID}, transfer.JPEGLSLossless.UID},
		{"restriction refused", []string{"-transfer-syntaxes", transfer.RLELossless.UID}, []string{transfer.JPEGLSLossless.UID}, ""},
		{"unknown transfer syntax refused", []string{"-accept-all"}, []string{"1.2.826.0.1.3680043.10.1284.999"}, ""},
		{"unreadable known syntax refused", []string{"-accept-all"}, []string{transfer.XMLEncoding.UID}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			contexts := negotiateStorescp(t, tc.args, compressedTestSOPClass, tc.proposed)
			got := ""
			for _, pc := range contexts {
				if pc.AbstractSyntaxUID == compressedTestSOPClass {
					got = pc.TransferSyntaxUID
				}
				if pc.AbstractSyntaxUID == dimse.VerificationSOPClassUID && pc.TransferSyntaxUID != transfer.ImplicitVRLittleEndian.UID {
					t.Fatal("verification inherited compressed storage preference")
				}
			}
			if got != tc.want {
				t.Fatalf("negotiated %q, want %q", got, tc.want)
			}
		})
	}
}

func TestStorescpPromiscuousIsExplicit(t *testing.T) {
	const unknown = "1.2.826.0.1.3680043.10.1284.999"
	for _, args := range [][]string{nil, {"-accept-all"}, {"-promiscuous"}} {
		contexts := negotiateStorescp(t, args, unknown, []string{transfer.RLELossless.UID})
		found := false
		for _, pc := range contexts {
			found = found || pc.AbstractSyntaxUID == unknown
		}
		if found != (strings.Join(args, "") == "-promiscuous") {
			t.Fatalf("unknown SOP policy changed: %v", args)
		}
	}
}
