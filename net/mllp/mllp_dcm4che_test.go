package mllp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ThalesMMS/dicom-go/hl7"
)

// TestMLLPInteropDCM4chePeer uses actual externally qualified Java transport
// classes, not another copy of this implementation. An unset classpath skips;
// an explicitly selected, unavailable or failing runtime fails this gate.
func TestMLLPInteropDCM4chePeer(t *testing.T) {
	classpath := os.Getenv("DICOMGO_DCM4CHE_HL7_CP")
	if classpath == "" {
		t.Skip("opt-in: DICOMGO_DCM4CHE_HL7_CP and Java/javac runtime")
	}
	java := os.Getenv("DICOMGO_JAVA")
	if java == "" {
		java = "java"
	}
	javac := os.Getenv("DICOMGO_JAVAC")
	if javac == "" {
		javac = "javac"
	}
	classes := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, javac, "-cp", classpath, "-d", classes, filepath.Join("testdata", "MLLPPeer.java")).CombinedOutput()
	if err != nil {
		t.Fatalf("compile synthetic peer: %v\n%s", err, out)
	}
	classpath = classes + string(os.PathListSeparator) + classpath
	startPeer := func(t *testing.T, args ...string) (string, func()) {
		t.Helper()
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		cmd := exec.CommandContext(ctx, java, append([]string{"-cp", classpath, "MLLPPeer"}, args...)...)
		var diag bytes.Buffer
		cmd.Stderr = &diag
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			cancel()
			t.Fatal(err)
		}
		if err := cmd.Start(); err != nil {
			cancel()
			t.Fatal(err)
		}
		waited := false
		t.Cleanup(func() {
			cancel()
			if !waited {
				_ = cmd.Wait()
			}
		})
		r := bufio.NewReader(stdout)
		address := ""
		if args[0] == "server" {
			port, err := r.ReadString('\n')
			if err != nil {
				t.Fatalf("peer readiness: %v", err)
			}
			address = net.JoinHostPort("127.0.0.1", strings.TrimSpace(port))
		}
		return address, func() {
			body, readErr := io.ReadAll(r)
			err := cmd.Wait()
			waited = true
			cancel()
			if err != nil || readErr != nil || strings.TrimSpace(string(body)) != "ok" {
				t.Fatalf("peer result: %v / %v / %q\n%s", err, readErr, body, diag.String())
			}
		}
	}
	for _, release := range []struct {
		name string
		mode Release
	}{{"1", MLLP1}, {"2", MLLP2}} {
		for _, code := range []hl7.AckCode{hl7.AckAccept, hl7.AckError, hl7.AckReject} {
			t.Run("java-server/release"+release.name+"/"+string(code), func(t *testing.T) {
				address, finish := startPeer(t, "server", release.name, string(code))
				client, err := Dial(context.Background(), address, ClientOptions{Release: release.mode, ResponseTimeout: time.Second})
				if err != nil {
					t.Fatal(err)
				}
				defer client.Close()
				response, err := client.Send(context.Background(), testMessage(t, "GO-JAVA"))
				if err != nil || ackOf(t, response).Code != code {
					t.Fatalf("public dcm4che Connection exchange: %v", err)
				}
				_ = client.Close()
				finish()
			})
		}
		t.Run("java-client/release"+release.name, func(t *testing.T) {
			address, _, _ := startServer(t, ServerOptions{Release: release.mode, TransportACKTimeout: time.Second})
			_, port, _ := net.SplitHostPort(address)
			_, finish := startPeer(t, "client", release.name, "normal", port)
			finish()
		})
	}
	for _, flavor := range []string{"nak", "malformed", "rawack", "truncated", "close", "timeout", "ack-close", "ack-timeout"} {
		t.Run("java-negative/"+flavor, func(t *testing.T) {
			address, finish := startPeer(t, "server", "2", flavor)
			client, err := Dial(context.Background(), address, ClientOptions{Release: MLLP2, ResponseTimeout: 180 * time.Millisecond, TransportACKTimeout: 60 * time.Millisecond})
			if err != nil {
				t.Fatal(err)
			}
			defer client.Close()
			_, err = client.Send(context.Background(), testMessage(t, "GO-NEGATIVE"))
			var exchange *ExchangeError
			if !errors.As(err, &exchange) {
				t.Fatalf("missing typed Java negative outcome: %v", err)
			}
			switch flavor {
			case "nak":
				if !errors.Is(err, ErrTransportNAK) || exchange.RequestState != RequestRejected {
					t.Fatal(err)
				}
			case "malformed", "rawack":
				if !errors.Is(err, ErrTransportConfirmation) || exchange.RequestState != RequestUnknown {
					t.Fatal(err)
				}
			case "ack-close", "ack-timeout":
				if exchange.Phase != PhaseReadResponse || exchange.RequestState != RequestAcknowledged {
					t.Fatal(err)
				}
			default:
				if exchange.Phase != PhaseRequestConfirmation || exchange.RequestState != RequestUnknown {
					t.Fatal(err)
				}
			}
			if _, again := client.Send(context.Background(), testMessage(t, "GO-NEGATIVE")); again != err {
				t.Fatal("automatic reuse after transport failure")
			}
			_ = client.Close()
			finish()
		})
	}
	t.Run("java-NAK-response-to-Go-server", func(t *testing.T) {
		address, log, _ := startServer(t, ServerOptions{Release: MLLP2, TransportACKTimeout: time.Second})
		_, port, _ := net.SplitHostPort(address)
		_, finish := startPeer(t, "client", "2", "nak-response", port)
		finish()
		if len(log.all()) != 1 || !strings.Contains(log.all()[0], ErrTransportNAK.Error()) {
			t.Fatalf("Go server did not reject Java transport NAK: %v", log.all())
		}
	})
}
