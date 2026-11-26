package ul

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/ThalesMMS/dicom-go/net/telemetry"
)

func TestNormalizeAETitle(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"ARCHIVE", "ARCHIVE"},
		{"  ARCHIVE      ", "ARCHIVE"},
		{"ARCHIVE\x00\x00\x00\x00\x00\x00\x00\x00\x00", "ARCHIVE"},
		{"ARCHIVE \x00 \x00", "ARCHIVE"},
		{"MY AE\x00", "MY AE"},
		{"ARCHIVE\x00EVIL", "ARCHIVE\x00EVIL"},
		{"\x00ARCHIVE", "\x00ARCHIVE"},
		{"ARCHIVE\n", "ARCHIVE\n"},
		{"\x00\x00\x00\x00", ""},
		{"                ", ""},
	} {
		if got := NormalizeAETitle(tc.in); got != tc.want {
			t.Errorf("NormalizeAETitle(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestValidateAETitle(t *testing.T) {
	for _, tc := range []struct {
		title string
		valid bool
	}{
		{"ARCHIVE", true},
		{"archive_1-b.c", true},
		{"CT_SCP", true},
		{"PACS-01", true},
		{"ORTHANC.1", true},
		{"A+B=C;D:E@F(G)!", true},
		{"MY AE", true},
		{"ARCHIVE\x00\x00\x00", true},
		{"  ARCHIVE  ", true},
		{"0123456789ABCDEF", true},
		// PS3.5 allows "/" and "." in an AE title: code that builds a
		// path from one must encode it, validation alone does not help.
		{"../../etc", true},
		{"", false},
		{"                ", false},
		{"\x00\x00\x00", false},
		{"0123456789ABCDEFG", false},
		{"ARCHIVE\x00EVIL", false},
		{"\x00ARCHIVE", false},
		{"ARC\nHIVE", false},
		{"ARCHIVE\t", false},
		{"ARC\x1bHIVE", false},
		{"ARC\x7fHIVE", false},
		{`ARC\HIVE`, false},
		{"ARCH\xc3\x8dVE", false},
	} {
		err := ValidateAETitle(tc.title)
		if tc.valid && err != nil {
			t.Errorf("ValidateAETitle(%q) = %v, want nil", tc.title, err)
		}
		if !tc.valid {
			if !errors.Is(err, ErrInvalidAEtitle) {
				t.Errorf("ValidateAETitle(%q) = %v, want ErrInvalidAEtitle", tc.title, err)
			} else if strings.ContainsFunc(err.Error(), func(r rune) bool { return r < 0x20 || r == 0x7f }) {
				t.Errorf("ValidateAETitle(%q) error %q carries a control character", tc.title, err)
			}
		}
	}
}

func TestEqualAETitles(t *testing.T) {
	for _, tc := range []struct {
		a, b  string
		equal bool
	}{
		{"ARCHIVE", "ARCHIVE", true},
		{"ARCHIVE", "ARCHIVE\x00\x00\x00", true},
		{"  ARCHIVE ", "ARCHIVE\x00 ", true},
		{"ARCHIVE", "archive", false},
		{"ARCHIVE", "ARCHIVE\x00EVIL", false},
		{"ARCHIVE", "ARCHIVE2", false},
	} {
		if got := EqualAETitles(tc.a, tc.b); got != tc.equal {
			t.Errorf("EqualAETitles(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.equal)
		}
	}
}

// exchangeRawAssociateRQ writes rq as encoded, with its AE title bytes kept
// verbatim, to Accept and returns the reply and the acceptor's result.
func exchangeRawAssociateRQ(t *testing.T, opts AcceptOptions, rq AssociationRQ) (PDU, *Association, error) {
	t.Helper()
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	type result struct {
		assoc *Association
		err   error
	}
	done := make(chan result, 1)
	go func() {
		assoc, err := Accept(server, opts)
		if assoc != nil {
			_ = assoc.Conn.Close()
		}
		done <- result{assoc, err}
	}()
	if err := WritePDU(client, &rq); err != nil {
		t.Fatalf("write A-ASSOCIATE-RQ: %v", err)
	}
	pdu, err := ReadPDU(client, DefaultMaxPDU)
	if err != nil {
		t.Fatalf("read response: %v", err)
	}
	_ = client.Close()
	r := <-done
	return pdu, r.assoc, r.err
}

func aeTitleTestRQ(called, calling string) AssociationRQ {
	rq := buildAssociateRQ(DialOptions{
		CalledAETitle:  "PLACEHOLDER",
		CallingAETitle: "PLACEHOLDER",
		Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	rq.CalledAETitle = called
	rq.CallingAETitle = calling
	return rq
}

func aeTitleTestAcceptOptions() AcceptOptions {
	return AcceptOptions{
		AETitle:                   "ARCHIVE",
		RequireMatchingCalledAE:   true,
		SupportedAbstractSyntaxes: []string{verificationSOPClassUID},
		SupportedTransferSyntaxes: []string{ImplicitVRLittleEndian},
	}
}

func TestAcceptNULPaddedAETitles(t *testing.T) {
	var seen AdmissionRequest
	opts := aeTitleTestAcceptOptions()
	opts.Admission = func(_ context.Context, req AdmissionRequest) *AssociationRJ {
		seen = req
		return nil
	}
	// Both 16-byte fields are padded with NUL instead of spaces.
	rq := aeTitleTestRQ("ARCHIVE\x00\x00\x00\x00\x00\x00\x00\x00\x00", "MODALITY\x00\x00\x00\x00\x00\x00\x00\x00")
	pdu, assoc, err := exchangeRawAssociateRQ(t, opts, rq)
	if err != nil {
		t.Fatalf("Accept() error = %v", err)
	}
	ac, ok := pdu.(*AssociationAC)
	if !ok {
		t.Fatalf("reply = %#v, want A-ASSOCIATE-AC", pdu)
	}
	if assoc.CalledAETitle != "ARCHIVE" || assoc.CallingAETitle != "MODALITY" {
		t.Fatalf("association titles = %q/%q, want ARCHIVE/MODALITY", assoc.CalledAETitle, assoc.CallingAETitle)
	}
	if seen.CalledAETitle != "ARCHIVE" || seen.CallingAETitle != "MODALITY" {
		t.Fatalf("admission titles = %q/%q, want ARCHIVE/MODALITY", seen.CalledAETitle, seen.CallingAETitle)
	}
	if ac.CalledAETitle != "ARCHIVE" || ac.CallingAETitle != "MODALITY" {
		t.Fatalf("A-ASSOCIATE-AC titles = %q/%q, want the normalized request titles", ac.CalledAETitle, ac.CallingAETitle)
	}
}

func TestAcceptRejectsInvalidAETitles(t *testing.T) {
	for _, tc := range []struct {
		name            string
		called, calling string
		reason          byte
		field           string
	}{
		{"calling newline", "ARCHIVE", "EVIL\nFORGED", AssociateRJReasonCallingAETitleNotRecognized, "calling"},
		{"calling escape sequence", "ARCHIVE", "\x1b[2JEVIL", AssociateRJReasonCallingAETitleNotRecognized, "calling"},
		{"calling backslash", "ARCHIVE", `EVIL\TWO`, AssociateRJReasonCallingAETitleNotRecognized, "calling"},
		{"calling all spaces", "ARCHIVE", "                ", AssociateRJReasonCallingAETitleNotRecognized, "calling"},
		{"calling high byte", "ARCHIVE", "MOD\xe9", AssociateRJReasonCallingAETitleNotRecognized, "calling"},
		{"called embedded NUL", "ARCHIVE\x00EVIL", "MODALITY", AssociateRJReasonCalledAETitleNotRecognized, "called"},
		{"called leading NUL", "\x00ARCHIVE", "MODALITY", AssociateRJReasonCalledAETitleNotRecognized, "called"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			admitted := false
			opts := aeTitleTestAcceptOptions()
			opts.Admission = func(context.Context, AdmissionRequest) *AssociationRJ { admitted = true; return nil }
			pdu, assoc, err := exchangeRawAssociateRQ(t, opts, aeTitleTestRQ(tc.called, tc.calling))
			if assoc != nil {
				t.Fatal("Accept() returned an association")
			}
			rj, ok := pdu.(*AssociationRJ)
			if !ok {
				t.Fatalf("reply = %#v, want A-ASSOCIATE-RJ", pdu)
			}
			if want := (AssociationRJ{Result: AssociateRJResultPermanent, Source: AssociateRJSourceServiceUser, Reason: tc.reason}); *rj != want {
				t.Fatalf("A-ASSOCIATE-RJ = %#v, want %#v", *rj, want)
			}
			var rejection *RejectionError
			if !errors.As(err, &rejection) || !errors.Is(err, ErrInvalidAEtitle) {
				t.Fatalf("Accept() error = %v, want RejectionError joined with ErrInvalidAEtitle", err)
			}
			if !strings.Contains(err.Error(), tc.field+" AE title") {
				t.Fatalf("Accept() error %q does not name the %s AE title", err, tc.field)
			}
			// errors.Join puts the rejection and its cause on two lines;
			// neither may carry a raw peer byte.
			lines := strings.Split(err.Error(), "\n")
			if len(lines) != 2 {
				t.Fatalf("Accept() error %q has %d lines, want 2", err, len(lines))
			}
			for _, line := range lines {
				if strings.ContainsFunc(line, func(r rune) bool { return r < 0x20 || r == 0x7f || r > 0x7e }) {
					t.Fatalf("Accept() error line %q carries a raw peer byte", line)
				}
			}
			if admitted {
				t.Fatal("admission handler saw an invalid AE title")
			}
		})
	}
}

func TestAcceptCalledAETitleComparisonIsCaseSensitive(t *testing.T) {
	pdu, _, _ := exchangeRawAssociateRQ(t, aeTitleTestAcceptOptions(), aeTitleTestRQ("archive", "MODALITY"))
	if rj, ok := pdu.(*AssociationRJ); !ok || rj.Reason != AssociateRJReasonCalledAETitleNotRecognized {
		t.Fatalf("reply = %#v, want called-AE-title-not-recognized", pdu)
	}
}

func TestDialRejectsInvalidLocalAETitles(t *testing.T) {
	for _, opts := range []DialOptions{
		{CalledAETitle: "ARCHIVE\nX", CallingAETitle: "MODALITY"},
		{CalledAETitle: "ARCHIVE", CallingAETitle: `MOD\ALITY`},
		{CalledAETitle: "ARCHIVE", CallingAETitle: "   "},
	} {
		opts.Contexts = []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}}
		if err := validateDialOptions(opts); !errors.Is(err, ErrInvalidAEtitle) {
			t.Errorf("validateDialOptions(%q/%q) = %v, want ErrInvalidAEtitle", opts.CalledAETitle, opts.CallingAETitle, err)
		}
	}
}

// TestRequestorIgnoresAETitlesEchoedInAC checks that an acceptor writing
// other titles into the A-ASSOCIATE-AC cannot change what the requestor
// reports: PS3.8 says those fields are not tested.
func TestRequestorIgnoresAETitlesEchoedInAC(t *testing.T) {
	client, server := net.Pipe()
	defer func() { _ = client.Close() }()
	defer func() { _ = server.Close() }()
	go func() {
		pdu, err := ReadPDU(server, DefaultMaxPDU)
		if err != nil {
			return
		}
		rq := pdu.(*AssociationRQ)
		results, _ := negotiatePresentationContexts(rq.PresentationContexts, AcceptOptions{AcceptAnyAbstractSyntax: true})
		ac := &AssociationAC{
			ProtocolVersion:        DefaultProtocolVersion,
			CalledAETitle:          "FORGED\x1b[2J",
			CallingAETitle:         "FORGED\nLINE",
			ApplicationContextName: ApplicationContextName,
			PresentationContexts:   results,
			UserInfo:               acceptUserInfo(DefaultMaxPDU, ImplementationClassUID, ImplementationVersionName, nil, UserIdentityResult{}, nil, nil, nil),
		}
		_ = WritePDU(server, ac)
		_, _ = ReadPDU(server, DefaultMaxPDU)
	}()

	var mu sync.Mutex
	var events []telemetry.Event
	sink := telemetry.SinkFunc(func(_ context.Context, event telemetry.Event) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	dispatcher := telemetry.NewDispatcher(sink, 64)
	operational, err := newOperationalState(&ObservabilityOptions{
		Dispatcher:     dispatcher,
		EndpointPolicy: telemetry.EndpointPolicy{AETitles: telemetry.EndpointPlaintext},
	}, telemetry.RoleSCU)
	if err != nil {
		t.Fatal(err)
	}
	opts := normalizeDialOptions(DialOptions{
		CalledAETitle:  "ARCHIVE",
		CallingAETitle: "MODALITY",
		Contexts:       []PresentationContext{{AbstractSyntaxUID: verificationSOPClassUID, TransferSyntaxUIDs: []string{ImplicitVRLittleEndian}}},
	})
	assoc, err := negotiateSCU(client, opts, operational)
	if err != nil {
		t.Fatalf("negotiateSCU() error = %v", err)
	}
	if assoc.CalledAETitle != "ARCHIVE" || assoc.CallingAETitle != "MODALITY" {
		t.Fatalf("association titles = %q/%q, want the requested ARCHIVE/MODALITY", assoc.CalledAETitle, assoc.CallingAETitle)
	}
	operational.close()
	dispatcher.Close()
	<-dispatcher.Done()
	mu.Lock()
	defer mu.Unlock()
	if len(events) == 0 {
		t.Fatal("no events observed")
	}
	for _, event := range events {
		if event.LocalAETitle != "MODALITY" || event.RemoteAETitle != "ARCHIVE" {
			t.Fatalf("event %s titles = %q/%q, want MODALITY/ARCHIVE", event.Kind, event.LocalAETitle, event.RemoteAETitle)
		}
	}
}

// TestObservedInvalidAETitleIsEscaped covers the A-ASSOCIATE-RQ event an
// acceptor emits before it rejects the request: a plaintext observer gets the
// peer's title with its control characters escaped.
func TestObservedInvalidAETitleIsEscaped(t *testing.T) {
	var mu sync.Mutex
	var remote []string
	dispatcher := telemetry.NewDispatcher(telemetry.SinkFunc(func(_ context.Context, event telemetry.Event) {
		mu.Lock()
		if event.RemoteAETitle != "" {
			remote = append(remote, event.RemoteAETitle)
		}
		mu.Unlock()
	}), 64)
	opts := aeTitleTestAcceptOptions()
	opts.Observability = &ObservabilityOptions{
		Dispatcher:     dispatcher,
		EndpointPolicy: telemetry.EndpointPolicy{AETitles: telemetry.EndpointPlaintext},
	}
	pdu, _, _ := exchangeRawAssociateRQ(t, opts, aeTitleTestRQ("ARCHIVE", "EVIL\nFORGED\x00"))
	if _, ok := pdu.(*AssociationRJ); !ok {
		t.Fatalf("reply = %#v, want A-ASSOCIATE-RJ", pdu)
	}
	dispatcher.Close()
	<-dispatcher.Done()
	mu.Lock()
	defer mu.Unlock()
	if len(remote) == 0 {
		t.Fatal("no event carried the remote AE title")
	}
	for _, got := range remote {
		if got != `EVIL\nFORGED` {
			t.Fatalf("observed remote AE title = %q, want the escaped EVIL\\nFORGED", got)
		}
	}
}
