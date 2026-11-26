package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

func connectionTestArgs(flags []string) []string {
	args := append([]string{}, flags...)
	return append(args, []string{}...)
}

func TestUnifiedConnectionCanonicalAndLegacy(t *testing.T) {
	for _, flags := range [][]string{
		{"-c", "PEER@[::1]:4242", "-calling", "LOCAL", "-timeout", "3s"},
		{"-aetitle", "PEER", "-calling-aet", "LOCAL", "-address", "[::1]:4242", "-timeout", "3s"},
	} {
		opts, err := parseArgs(connectionTestArgs(flags), io.Discard)
		if err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		conn := opts.conn
		if conn.Host != "::1" || conn.Port != 4242 || conn.CalledAE != "PEER" || conn.CallingAE != "LOCAL" || conn.Timeout != 3*time.Second || conn.Listener() != true {
			t.Fatalf("%v: %#v", flags, conn)
		}
	}
}

func TestUnifiedConnectionPreservesDefaults(t *testing.T) {
	args := append([]string{}, []string{}...)
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	conn := opts.conn
	if conn.Host != "127.0.0.1" || conn.Port != 11112 || conn.CalledAE != "STORESCP" || conn.CallingAE != "" || conn.Timeout != 15*time.Second {
		t.Fatalf("defaults: %#v", conn)
	}
}

func TestUnifiedConnectionConflictBothOrders(t *testing.T) {
	for _, flags := range [][]string{
		{"-called-aet", "OTHER", "-c", "PEER@localhost:4242"},
		{"-c", "PEER@localhost:4242", "-called-ae", "OTHER"},
		{"-timeout", "1s", "-timeout", "2s"},
	} {
		if _, err := parseArgs(connectionTestArgs(flags), io.Discard); err == nil {
			t.Fatalf("accepted conflict %v", flags)
		}
	}
}

func TestUnifiedConnectionHelp(t *testing.T) {
	var out bytes.Buffer
	args := append([]string{}, "-help")
	if _, err := parseArgs(args, &out); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("help: %v", err)
	}
	for _, name := range []string{"c", "host", "port", "called", "calling", "timeout"} {
		if !strings.Contains(out.String(), "-"+name+" ") {
			t.Fatalf("missing %s in help: %s", name, out.String())
		}
	}
	if strings.Contains(out.String(), "panic") {
		t.Fatal(out.String())
	}
}

func TestUnifiedConnectionNetworkConfigConflicts(t *testing.T) {
	config := []string{"-net-config", networkFixturePath(t), "-net-connection", "loopback"}
	for _, flags := range [][]string{
		{"-host", "localhost"}, {"-port", "11112"}, {"-c", "LOCAL@localhost:11112"},
		{"-called", "LOCAL"}, {"-called-ae", "LOCAL"}, {"-called-aet", "LOCAL"},
		{"-calling", "PEER"}, {"-calling-ae", "PEER"}, {"-calling-aet", "PEER"},
		{"-address", "localhost:11112"}, {"-aetitle", "LOCAL"},
	} {
		for _, args := range [][]string{append(append([]string{}, flags...), config...), append(append([]string{}, config...), flags...)} {
			if _, err := parseArgs(args, io.Discard); err == nil {
				t.Fatalf("accepted network conflict %v", args)
			}
		}
	}
	for _, flags := range [][]string{
		{"-timeout", "5s"}, {"-negotiation-timeout", "5s"},
		{"-timeout", "5000ms", "-negotiation-timeout", "5s"},
	} {
		opts, err := parseArgs(append(append([]string{}, config...), flags...), io.Discard)
		if err != nil || opts.limits.negotiationTimeout != 5*time.Second || storescpAcceptOptions(opts).NegotiationTimeout != 5*time.Second {
			t.Fatalf("net-config timeout: %v %#v", err, opts)
		}
	}
	for _, flags := range [][]string{{"-timeout", "1s", "-negotiation-timeout", "2s"}, {"-negotiation-timeout", "2s", "-timeout", "1s"}} {
		if _, err := parseArgs(flags, io.Discard); err == nil {
			t.Fatalf("accepted timeout conflict %v", flags)
		}
	}
}

func TestUnifiedConnectionCanonicalStorescpLoopback(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	opts, err := parseArgs([]string{"-c", "LOCAL@127.0.0.1:0", "-calling", "SYNTHETIC", "-timeout", "3s", "-output", t.TempDir()}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	stdout := &networkReadyWriter{ready: make(chan string, 1)}
	done := make(chan error, 1)
	go func() { done <- runServerWithContext(ctx, opts, stdout, io.Discard) }()
	defer func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("shutdown: %v", err)
			}
		case <-time.After(3 * time.Second):
			t.Error("shutdown deadline")
		}
	}()
	var address string
	select {
	case address = <-stdout.ready:
	case <-ctx.Done():
		t.Fatal("startup deadline")
	}
	dial := func(calling string) (*ul.Association, error) {
		return ul.DialContext(ctx, address, ul.DialOptions{CalledAETitle: "LOCAL", CallingAETitle: calling, Contexts: []ul.PresentationContext{{AbstractSyntaxUID: dimse.VerificationSOPClassUID, TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian}}}})
	}
	denied, err := dial("synthetic")
	if denied != nil {
		denied.Close()
	}
	var rejection *ul.RejectionError
	if !errors.As(err, &rejection) || rejection.Reason != ul.AssociateRJReasonCallingAETitleNotRecognized {
		t.Fatalf("wrong Calling AE: %v", err)
	}
	assoc, err := dial("SYNTHETIC")
	if err != nil {
		t.Fatal(err)
	}
	defer assoc.Close()
	pc, ok := dimse.AcceptedVerificationContext(assoc)
	if !ok {
		t.Fatal("Verification context missing")
	}
	response, e := dimse.SendCEcho(assoc, pc.ID, 12)
	if e != nil || response.Status != dimse.StatusSuccess {
		t.Fatalf("C-ECHO %v: %v", response, e)
	}
	if err := assoc.Release(ctx); err != nil {
		t.Fatal(err)
	}
}
