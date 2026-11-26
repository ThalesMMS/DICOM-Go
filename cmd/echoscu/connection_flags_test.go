package main

import (
	"bytes"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
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
		{"-called-ae", "PEER", "-calling-aet", "LOCAL", "-remote", "[::1]:4242", "-timeout", "3s"},
	} {
		opts, err := parseArgs(connectionTestArgs(flags), io.Discard)
		if err != nil {
			t.Fatalf("%v: %v", flags, err)
		}
		conn := opts.conn
		if conn.Host != "::1" || conn.Port != 4242 || conn.CalledAE != "PEER" || conn.CallingAE != "LOCAL" || conn.Timeout != 3*time.Second || conn.Listener() != false {
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
	if conn.Host != "localhost" || conn.Port != 11112 || conn.CalledAE != "ANY-SCP" || conn.CallingAE != "ECHOSCU" || conn.Timeout != 10*time.Second {
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

func TestUnifiedConnectionEchoDialTimeout(t *testing.T) {
	opts, err := parseArgs([]string{"-timeout", "3s"}, io.Discard)
	if err != nil || echoDialTimeout(opts) != 3*time.Second {
		t.Fatalf("dial timeout: %v %#v", err, opts)
	}
	if echoDialTimeout(options{}) != 10*time.Second {
		t.Fatal("manual legacy timeout changed")
	}
}

func TestUnifiedConnectionCanonicalEchoLoopback(t *testing.T) {
	listener, err := ul.Listen(ul.ListenOptions{Address: "127.0.0.1:0"})
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	done := make(chan error, 1)
	go func() {
		assoc, err := listener.AcceptAssociation(ul.AcceptOptions{AETitle: "LOCAL", RequireMatchingCalledAE: true, NegotiationTimeout: 3 * time.Second, IdleTimeout: 3 * time.Second, ReadProgressTimeout: 3 * time.Second, SupportedAbstractSyntaxes: []string{dimse.VerificationSOPClassUID}, SupportedTransferSyntaxes: []string{ul.ImplicitVRLittleEndian}})
		if err != nil {
			done <- err
			return
		}
		defer assoc.Close()
		if assoc.CallingAETitle != "SYNTHETIC" {
			done <- fmt.Errorf("calling AE %q", assoc.CallingAETitle)
			return
		}
		if err = dimse.HandleCEcho(assoc); err != nil {
			done <- err
			return
		}
		pdu, err := assoc.ReadPDU()
		if err == nil {
			if _, ok := pdu.(*ul.ReleaseRQ); !ok {
				err = fmt.Errorf("release type %T", pdu)
			} else {
				err = assoc.WritePDU(&ul.ReleaseRP{})
			}
		}
		done <- err
	}()
	_, port, _ := net.SplitHostPort(listener.Addr().String())
	var out, errout bytes.Buffer
	code := run([]string{"-c", "LOCAL@127.0.0.1:" + port, "-calling", "SYNTHETIC", "-timeout", "3s"}, &out, &errout)
	if code != 0 || !strings.Contains(out.String(), "C-ECHO success") {
		t.Fatalf("code %d: %s / %s", code, out.String(), errout.String())
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("loopback cleanup deadline")
	}
}
