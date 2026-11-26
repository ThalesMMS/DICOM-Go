package audit

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"testing"
	"time"
)

func TestDurableRestartHelper(t *testing.T) {
	directory := os.Getenv("DICOM_AUDIT_RESTART_DIRECTORY")
	if directory == "" {
		return
	}
	o := DurableOptions{Directory: directory, Destinations: []AuditDestination{{ID: "primary", Network: SyslogTCP, Address: os.Getenv("DICOM_AUDIT_RESTART_ADDRESS")}}, MaxRecordBytes: 4096, MaxBytes: 32768, SourceIdentity: "restart-synthetic"}
	l, err := OpenDurableLogger(o)
	if err != nil {
		os.Exit(91)
	}
	stage := os.Getenv("DICOM_AUDIT_RESTART_STAGE")
	if stage == "checkpoint" {
		l.options.Fault = func(s string) error {
			if s == stage {
				// The attempt is on the wire but not checkpointed. Hold the
				// crash until the parent has read it (it closes stdin):
				// Windows resets the socket of an exiting process and
				// discards bytes its peer has not read yet, which would
				// lose the attempt instead of leaving it to be repeated.
				_, _ = io.Copy(io.Discard, os.Stdin)
				os.Exit(77)
			}
			return nil
		}
		_ = l.Flush(context.Background())
		os.Exit(92)
	}
	if stage != "accepted" {
		l.options.Fault = func(s string) error {
			if s == stage {
				os.Exit(77)
			}
			return nil
		}
	}
	if err = l.LogRawSource(context.Background(), 12, 5, time.Now(), []byte("controlled restart payload")); err != nil {
		os.Exit(93)
	}
	os.Exit(77) // No defers/Close: exercise real process interruption + lock release.
}

// auditRestartChild is a helper process that crashes (exit 77) at a durable
// stage. A child crashing at "checkpoint" exits only once crash is called.
type auditRestartChild struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	exited chan struct{}
	err    error
}

func startAuditRestartChild(t *testing.T, o DurableOptions, stage string) *auditRestartChild {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDurableRestartHelper$")
	cmd.Env = append(os.Environ(), "DICOM_AUDIT_RESTART_DIRECTORY="+o.Directory, "DICOM_AUDIT_RESTART_ADDRESS="+o.Destinations[0].Address, "DICOM_AUDIT_RESTART_STAGE="+stage)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &auditRestartChild{cmd: cmd, stdin: stdin, exited: make(chan struct{})}
	go func() { c.err = cmd.Wait(); close(c.exited) }()
	t.Cleanup(func() {
		select {
		case <-c.exited:
		default:
			_ = cmd.Process.Kill()
			<-c.exited
		}
	})
	return c
}

// state describes the child for failure messages.
func (c *auditRestartChild) state() string {
	select {
	case <-c.exited:
		return fmt.Sprintf("helper exited (%v)", c.err)
	default:
		return "helper running"
	}
}

// crash releases the child and requires its crash exit.
func (c *auditRestartChild) crash(t *testing.T) {
	t.Helper()
	c.stdin.Close()
	<-c.exited
	exit, ok := c.err.(*exec.ExitError)
	if !ok || exit.ExitCode() != 77 {
		t.Fatalf("restart helper exit: %v", c.err)
	}
}

func runAuditRestartChild(t *testing.T, o DurableOptions, stage string) {
	t.Helper()
	startAuditRestartChild(t, o, stage).crash(t)
}

func TestDurableSubprocessRestartRecoversAcceptedAndRecordBeforeState(t *testing.T) {
	for _, stage := range []string{"accepted", "state_sync", "state_rename"} {
		t.Run(stage, func(t *testing.T) {
			o := durableTestOptions(t)
			o.SourceIdentity = "restart-synthetic"
			runAuditRestartChild(t, o, stage)
			l := openDurableTest(t, o)
			s := l.Stats()
			if s.Pending != 1 || s.SourceCursor != 12 {
				t.Fatalf("recovered state: %+v", s)
			}
			if err := l.LogRawSource(context.Background(), 12, 5, time.Now(), []byte("replayed")); err != nil {
				t.Fatal(err)
			}
			if l.Stats().Pending != 1 {
				t.Fatal("local replay duplicated recovered record")
			}
		})
	}
}

func TestDurableSubprocessCrashAfterSocketWriteCanRepeatAttempt(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	records, receiver := startAuditReceiver(t, listener)
	o := durableTestOptions(t)
	o.SourceIdentity = "restart-synthetic"
	o.Destinations[0].Address = listener.Addr().String()
	l := openDurableTest(t, o)
	if err = l.LogRawSource(context.Background(), 12, 5, time.Now(), []byte("controlled checkpoint gap")); err != nil {
		t.Fatal(err)
	}
	l.Close()
	child := startAuditRestartChild(t, o, "checkpoint")
	awaitAudit(t, "the receiver read the helper's attempt before its checkpoint crash", func() (any, bool) {
		s := receiver.Stats()
		return fmt.Sprintf("receiver %+v, %s", s, child.state()), s.Received == 1
	})
	child.crash(t)
	first := <-records
	l = openDurableTest(t, o)
	if s := l.Stats(); s.Pending != 1 || s.Sent != 0 {
		t.Fatal("uncheckpointed attempt was retired")
	}
	if err = l.Flush(context.Background()); err != nil {
		t.Fatal(err)
	}
	awaitAudit(t, "the receiver read the repeated attempt after restart", func() (any, bool) {
		s := receiver.Stats()
		return s, s.Received == 2
	})
	second := <-records
	if string(first.Raw) != string(second.Raw) || l.Stats().Pending != 0 {
		t.Fatal("repeat changed payload or failed to checkpoint")
	}
}
