package main

import (
	"bytes"
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/dictionary/std"
	"github.com/ThalesMMS/dicom-go/dictionary/tags"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
	"github.com/ThalesMMS/dicom-go/transfer"
)

func TestParseArgsExtendedNegotiationFlags(t *testing.T) {
	opts, err := parseArgs([]string{"-relational", "-datetime", "-fuzzy", "-timezone", "-empty-value", "-multiple-value"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	want := dimse.CFindExtendedNegotiation{
		RelationalQueries: true, CombinedDateTimeMatching: true, FuzzySemanticMatching: true, TimezoneQueryAdjustment: true,
		EmptyValueMatching: true, MultipleValueMatching: true,
	}
	if opts.extendedNegotiation != want {
		t.Fatalf("extended negotiation = %+v, want %+v", opts.extendedNegotiation, want)
	}
	if got := formatFindOptions(want); got != "relational,datetime,fuzzy,timezone,empty-value,multiple-value" {
		t.Fatalf("formatFindOptions = %q", got)
	}
	if got := formatFindOptions(dimse.CFindExtendedNegotiation{}); got != "none" {
		t.Fatalf("formatFindOptions(zero) = %q", got)
	}
}

// TestRunFindRelationalAgainstCheckingSCP sends a SERIES-level query without
// the Study Instance UID to an SCP that checks Identifiers: refused with
// 0xA900 as a hierarchical query, answered once -relational is negotiated.
func TestRunFindRelationalAgainstCheckingSCP(t *testing.T) {
	candidate := object.FromElements([]core.Element{
		{Header: core.ElementHeader{Tag: tags.PatientName, VR: core.VRPN}, Value: core.StringValue{"DOE^JANE"}},
		{Header: core.ElementHeader{Tag: tags.StudyInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1156"}},
		{Header: core.ElementHeader{Tag: tags.SeriesInstanceUID, VR: core.VRUI}, Value: core.StringValue{"1.2.826.0.1.3680043.10.543.1156.1"}},
		{Header: core.ElementHeader{Tag: tags.Modality, VR: core.VRCS}, Value: core.StringValue{"MR"}},
	}, std.Dictionary)
	for _, relational := range []bool{false, true} {
		name := "hierarchical"
		if relational {
			name = "relational"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0", Context: ctx})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = listener.Close() }()
			serverDone := make(chan error, 1)
			go func() {
				assoc, err := listener.AcceptAssociation(ul.AcceptOptions{
					AETitle:                    "FINDSCP",
					Context:                    ctx,
					SupportedAbstractSyntaxes:  []string{dimse.StudyRootFindSOPClassUID},
					SupportedTransferSyntaxes:  []string{transfer.ImplicitVRLittleEndian.UID},
					ExtendedNegotiationHandler: dimse.ExtendedNegotiationPolicy{Find: &dimse.CFindExtendedNegotiation{RelationalQueries: true}}.Handler(),
				})
				if err != nil {
					serverDone <- err
					return
				}
				defer func() { _ = assoc.Close() }()
				serverDone <- dimse.ServeAssociation(ctx, assoc, dimse.AssociationSCPOptions{
					CFindHandler:         dimse.MemoryFindIndex{Candidates: []*object.Object{candidate}},
					CheckCFindIdentifier: true,
				})
			}()
			opts := options{
				model:     dimse.QueryRetrieveModelStudyRoot,
				host:      "127.0.0.1",
				port:      listener.Addr().(*net.TCPAddr).Port,
				calledAE:  "FINDSCP",
				callingAE: "FINDSCU",
				level:     "series",
				timeout:   5 * time.Second,
				keys:      []string{"PatientName=DOE^JANE", "SeriesInstanceUID=", "Modality="},
			}
			opts.extendedNegotiation.RelationalQueries = relational
			var stdout, stderr bytes.Buffer
			runErr := runFind(context.Background(), opts, &stdout, &stderr)
			if err := <-serverDone; err != nil {
				t.Fatalf("server: %v", err)
			}
			if !relational {
				if runErr == nil || !strings.Contains(stdout.String(), "Final status=0xA900") {
					t.Fatalf("runFind = %v, stdout %q; want 0xA900", runErr, stdout.String())
				}
				if stderr.Len() != 0 {
					t.Fatalf("stderr = %q, want no negotiation line", stderr.String())
				}
				return
			}
			if runErr != nil {
				t.Fatalf("runFind: %v (stdout %q)", runErr, stdout.String())
			}
			if !strings.Contains(stderr.String(), "requested=relational accepted=relational") {
				t.Fatalf("stderr = %q", stderr.String())
			}
			if !strings.Contains(stdout.String(), "MR") || !strings.Contains(stdout.String(), "Final status=0x0000") {
				t.Fatalf("stdout = %q", stdout.String())
			}
		})
	}
}
