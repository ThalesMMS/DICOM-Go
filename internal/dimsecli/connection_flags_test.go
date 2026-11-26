package dimsecli

import (
	"bytes"
	"context"
	"flag"
	"io"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/net/ul"
)

func registeredConnection(t *testing.T, listener bool) (*ConnectionFlags, *flag.FlagSet) {
	t.Helper()
	c := &ConnectionFlags{}
	fs := flag.NewFlagSet("connection", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	c.RegisterWithDefaults(fs, ConnectionDefaults{Host: "localhost", Port: 11112, CalledAE: "SCP", CallingAE: map[bool]string{true: "", false: "SCU"}[listener], Timeout: time.Second, Listener: listener})
	return c, fs
}

func TestConnectionFlagEquivalentSpellings(t *testing.T) {
	variants := [][]string{
		{"-c", "Peer@[fe80::1%en0]:4242", "-calling", "Local", "-timeout", "1000ms"},
		{"-host", "fe80::1%en0", "-port", "04242", "-called-ae", " Peer ", "-calling-aet", "Local", "-timeout", "1s"},
		{"-remote", "[fe80::1%en0]:4242", "-called-aet", "Peer", "-calling-ae", "Local"},
		{"-c", "Peer@[fe80::1%en0]:4242", "-remote", "[fe80::1%en0]:4242", "-called", "Peer", "-calling", "Local", "-timeout", "1s", "-timeout", "1000ms"},
	}
	for _, args := range variants {
		c, fs := registeredConnection(t, false)
		if err := fs.Parse(args); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
		if c.Host != "fe80::1%en0" || c.Port != 4242 || c.CalledAE != "Peer" || c.CallingAE != "Local" || c.Timeout != time.Second || c.Address() != "[fe80::1%en0]:4242" {
			t.Fatalf("%v: %#v", args, c)
		}
	}
	c, fs := registeredConnection(t, false)
	if err := fs.Parse([]string{"-c", "SCP@GROUP@127.0.0.1:104"}); err != nil {
		t.Fatal(err)
	}
	if c.CalledAE != "SCP@GROUP" {
		t.Fatal(c.CalledAE)
	}
}

func TestConnectionFlagsConflictOrderAndAtomicity(t *testing.T) {
	pairs := [][2][]string{
		{{"-c", "SCP@localhost:104"}, {"-port", "105"}},
		{{"-c", "SCP@localhost:104"}, {"-host", "127.0.0.1"}},
		{{"-c", "SCP@localhost:104"}, {"-called-aet", "OTHER"}},
		{{"-called", "SCP"}, {"-called-ae", "scp"}},
		{{"-calling-ae", "SCU"}, {"-calling-aet", "OTHER"}},
		{{"-timeout", "1s"}, {"-timeout", "2s"}},
		{{"-remote", "localhost:104"}, {"-c", "SCP@localhost:105"}},
	}
	for _, pair := range pairs {
		for _, order := range [][2]int{{0, 1}, {1, 0}} {
			c, fs := registeredConnection(t, false)
			if err := fs.Parse(pair[order[0]]); err != nil {
				t.Fatal(err)
			}
			oldHost, oldPort, oldCalled, oldCalling, oldTime := c.Host, c.Port, c.CalledAE, c.CallingAE, c.Timeout
			oldExplicit := map[string]string{}
			for k, v := range c.explicit {
				oldExplicit[k] = v
			}
			if err := fs.Parse(pair[order[1]]); err == nil || !strings.Contains(err.Error(), "conflicting explicit") {
				t.Fatalf("%v then %v: %v", pair[order[0]], pair[order[1]], err)
			}
			if c.Host != oldHost || c.Port != oldPort || c.CalledAE != oldCalled || c.CallingAE != oldCalling || c.Timeout != oldTime || !reflect.DeepEqual(c.explicit, oldExplicit) {
				t.Fatalf("failed value mutated connection: %#v", c)
			}
		}
	}
	c := ConnectionFlags{Host: "localhost", Port: 11112}
	if err := c.SetAddress("127.0.0.1:104"); err != nil {
		t.Fatal(err)
	}
	if err := c.SetAddress("127.0.0.1:105"); err == nil || c.Port != 104 {
		t.Fatalf("standalone SetAddress: %v, %#v", err, c)
	}
}

func TestConnectionFlagsInvalidValuesAndBounds(t *testing.T) {
	invalid := [][]string{
		{"-port", "0"}, {"-port", "-1"}, {"-port", "65536"}, {"-port", "http"}, {"-port", "+104"}, {"-port", "100000"},
		{"-timeout", "0"}, {"-timeout", "-1s"}, {"-timeout", "one"},
		{"-host", ""}, {"-host", "[::1]"}, {"-host", "a b"}, {"-host", "é"}, {"-host", "::1%"}, {"-host", "::g"}, {"-host", strings.Repeat("a", 1025)},
		{"-called", " "}, {"-called", "a\\b"}, {"-called", "A\nB"}, {"-called", "é"}, {"-called", strings.Repeat("A", 17)}, {"-called", "A" + strings.Repeat(" ", 1024)},
		{"-calling", ""}, {"-c", "SCP@::1:104"}, {"-c", "@localhost:104"}, {"-c", "SCP@localhost"}, {"-c", "SCP@localhost:65536"}, {"-c", strings.Repeat("A", 1025)},
	}
	for _, args := range invalid {
		c, fs := registeredConnection(t, false)
		if err := fs.Parse(args); err == nil {
			t.Fatalf("accepted %v: %#v", args, c)
		}
	}
	c, fs := registeredConnection(t, true)
	if err := fs.Parse([]string{"-c", "SCP@:0"}); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Address() != ":0" {
		t.Fatal(c.Address())
	}
}

func TestConnectionFlagsRolesAndHelp(t *testing.T) {
	for _, listener := range []bool{false, true} {
		c, fs := registeredConnection(t, listener)
		if fs.Lookup("user-identity") != nil == listener {
			t.Fatalf("identity role listener=%v", listener)
		}
		if fs.Lookup("tls-profile") == nil {
			t.Fatal("missing TLS flag")
		}
		var out bytes.Buffer
		fs.SetOutput(&out)
		fs.PrintDefaults()
		for _, name := range []string{"c", "host", "port", "called", "calling", "timeout"} {
			if !strings.Contains(out.String(), "-"+name+" ") {
				t.Fatalf("missing %s in help: %s", name, out.String())
			}
		}
		if strings.Contains(out.String(), "panic") {
			t.Fatal(out.String())
		}
		if !strings.Contains(out.String(), "deprecated alias") {
			t.Fatal(out.String())
		}
		if err := c.Validate(); err != nil {
			t.Fatal(err)
		}
	}
	c, fs := registeredConnection(t, true)
	c.Timeout = 0
	if err := c.Validate(); err != nil {
		t.Fatalf("legacy zero negotiation: %v", err)
	}
	if err := fs.Parse([]string{"-timeout", "0s"}); err == nil {
		t.Fatal("explicit zero accepted")
	}
}

func TestListenerConnectionAdmissionComposition(t *testing.T) {
	c, fs := registeredConnection(t, true)
	if err := fs.Parse([]string{"-calling", " Peer ", "-timeout", "3s"}); err != nil {
		t.Fatal(err)
	}
	calls := 0
	original := ul.AcceptOptions{IdleTimeout: time.Minute, NegotiationTimeout: time.Second, Admission: func(ctx context.Context, r ul.AdmissionRequest) *ul.AssociationRJ {
		calls++
		if r.CalledAETitle != "LOCAL" {
			return ul.RejectCalledAETitle()
		}
		return nil
	}}
	applied := c.ApplyListenerOptions(original)
	if applied.NegotiationTimeout != 3*time.Second || applied.IdleTimeout != time.Minute {
		t.Fatalf("timeouts: %#v", applied)
	}
	ctx := context.Background()
	if r := applied.Admission(ctx, ul.AdmissionRequest{CallingAETitle: "peer", CalledAETitle: "LOCAL"}); r == nil || r.Reason != ul.AssociateRJReasonCallingAETitleNotRecognized || calls != 0 {
		t.Fatalf("wrong peer: %#v, calls=%d", r, calls)
	}
	if r := applied.Admission(ctx, ul.AdmissionRequest{CallingAETitle: "Peer", CalledAETitle: "OTHER"}); r == nil || r.Reason != ul.AssociateRJReasonCalledAETitleNotRecognized || calls != 1 {
		t.Fatalf("lost policy: %#v, calls=%d", r, calls)
	}
	if r := applied.Admission(ctx, ul.AdmissionRequest{CallingAETitle: "Peer ", CalledAETitle: "LOCAL"}); r != nil || calls != 2 {
		t.Fatalf("valid peer: %#v, calls=%d", r, calls)
	}
	var manual ConnectionFlags
	if got := manual.ApplyListenerOptions(original); got.NegotiationTimeout != time.Second || got.Admission(ctx, ul.AdmissionRequest{CalledAETitle: "OTHER"}) == nil {
		t.Fatal("manual legacy options changed")
	}
}
