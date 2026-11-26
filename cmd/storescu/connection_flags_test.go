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
	args := append([]string{}, flags...)
	return append(args, []string{"fixture.dcm"}...)
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
	args := append([]string{}, []string{"localhost:11112", "fixture.dcm"}...)
	opts, err := parseArgs(args, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	conn := opts.conn
	if conn.Host != "localhost" || conn.Port != 11112 || conn.CalledAE != "ANY-SCP" || conn.CallingAE != "STORESCU" || conn.Timeout != 10*time.Second {
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

func TestUnifiedConnectionStorePositionalAddress(t *testing.T) {
	for _, flags := range [][]string{
		{"-c", "SCP@localhost:104", "localhost:104", "fixture.dcm"},
		{"-host", "localhost", "-port", "104", "localhost:104", "fixture.dcm"},
	} {
		opts, err := parseArgs(flags, io.Discard)
		if err != nil || opts.address != "localhost:104" || len(opts.files) != 1 {
			t.Fatalf("%v: %v %#v", flags, err, opts)
		}
	}
	for _, flags := range [][]string{
		{"-c", "SCP@localhost:104", "localhost:105", "fixture.dcm"},
		{"fixture.dcm"},
	} {
		if _, err := parseArgs(flags, io.Discard); err == nil {
			t.Fatalf("accepted %v", flags)
		}
	}
}

func TestUnifiedConnectionStoreColonPathsRemainFiles(t *testing.T) {
	for _, files := range [][]string{
		{`C:\data\one.dcm`, `C:\data\two.dcm`},
		{"/tmp/a:foo", "/tmp/second.dcm"},
		{"/tmp/a:104", "/tmp/second.dcm"},
	} {
		args := append([]string{"-c", "SCP@localhost:104"}, files...)
		opts, err := parseArgs(args, io.Discard)
		if err != nil || len(opts.files) != 2 || opts.files[0] != files[0] || opts.files[1] != files[1] {
			t.Fatalf("colon paths %v: %v %#v", files, err, opts)
		}
	}
}
