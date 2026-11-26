package main

import (
	"bytes"
	"errors"
	"flag"
	"io"
	"strings"
	"testing"
	"time"
)

func connectionTestArgs(flags []string) []string {
	args := append([]string{"find", "-k", "PatientID="}, flags...)
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
	args := append([]string{"find", "-k", "PatientID="}, []string{}...)
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	conn := opts.conn
	if conn.Host != "localhost" || conn.Port != 11112 || conn.CalledAE != "ANY-SCP" || conn.CallingAE != "UPSSCU" || conn.Timeout != 30*time.Second {
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
	args := append([]string{"find", "-k", "PatientID="}, "-help")
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
