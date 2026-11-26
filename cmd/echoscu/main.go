package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		if errors.Is(err, errUsage) {
			return 2
		}
		clidiag.Fprintln(stderr, "echoscu", err)
		return 1
	}
	if err := runEcho(opts, stdout, stderr); err != nil {
		clidiag.Fprintln(stderr, "echoscu", err)
		return 1
	}
	return 0
}

var errUsage = errors.New("usage error")

type options struct {
	conn      dimsecli.ConnectionFlags
	host      string
	port      int
	calledAE  string
	callingAE string
	timeout   time.Duration
	messageID int
	tls       dimsecli.TLSFlags
	identity  dimsecli.UserIdentityFlags
}

const (
	defaultDialTimeout    = 10 * time.Second
	defaultReleaseTimeout = 5 * time.Second
)

func parseArgs(args []string, stderr io.Writer) (options, error) {
	opts := options{
		host:      "localhost",
		port:      11112,
		calledAE:  "ANY-SCP",
		callingAE: "ECHOSCU",
		messageID: 1,
	}

	fs := flag.NewFlagSet("echoscu", flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.RegisterWithDefaults(fs, dimsecli.ConnectionDefaults{Host: opts.host, Port: opts.port, CalledAE: opts.calledAE, CallingAE: opts.callingAE, Timeout: defaultDialTimeout})
	fs.IntVar(&opts.messageID, "message-id", opts.messageID, "C-ECHO message ID")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, "Usage: %s [flags]\n\nSend one DICOM C-ECHO request to a Verification SCP.\n\nFlags:\n", fs.Name())
		fs.PrintDefaults()
	}

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	if err := opts.conn.Validate(); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return opts, errUsage
	}
	opts.host, opts.port, opts.calledAE, opts.callingAE = opts.conn.Host, opts.conn.Port, opts.conn.CalledAE, opts.conn.CallingAE
	opts.tls, opts.identity, opts.timeout = opts.conn.TLS, opts.conn.Identity, opts.conn.Timeout

	if fs.NArg() != 0 {
		fs.Usage()
		return opts, errUsage
	}
	if opts.port <= 0 || opts.port > 65535 {
		_, _ = fmt.Fprintln(stderr, "-port must be between 1 and 65535")
		fs.Usage()
		return opts, errUsage
	}
	if opts.messageID < 0 || opts.messageID > 0xFFFF {
		_, _ = fmt.Fprintln(stderr, "-message-id must fit in uint16")
		fs.Usage()
		return opts, errUsage
	}
	if err := errors.Join(opts.tls.Validate(), opts.identity.Validate()); err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	return opts, nil
}

func runEcho(opts options, stdout, stderr io.Writer) error {
	_ = stderr
	address := net.JoinHostPort(opts.host, strconv.Itoa(opts.port))
	dialCtx, cancelDial := context.WithTimeout(context.Background(), echoDialTimeout(opts))
	defer cancelDial()

	tlsConfig, err := opts.tls.Config()
	if err != nil {
		return err
	}
	identity, err := opts.identity.Request()
	if err != nil {
		return err
	}
	assoc, err := ul.Dial(address, ul.DialOptions{
		CalledAETitle:  opts.calledAE,
		CallingAETitle: opts.callingAE,
		Context:        dialCtx,
		TLSConfig:      tlsConfig,
		UserIdentity:   identity,
		Contexts: []ul.PresentationContext{{
			AbstractSyntaxUID:  dimse.VerificationSOPClassUID,
			TransferSyntaxUIDs: []string{ul.ImplicitVRLittleEndian},
		}},
	})
	if err != nil {
		return err
	}

	released := false
	defer func() {
		if !released {
			_ = assoc.Close()
		}
	}()

	pc, ok := dimse.AcceptedVerificationContext(assoc)
	if !ok {
		return errors.New("no accepted Verification SOP Class presentation context")
	}
	response, err := dimse.SendCEcho(assoc, pc.ID, uint16(opts.messageID))
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "C-ECHO success status=0x%04X\n", response.Status)
	releaseCtx, cancelRelease := context.WithTimeout(context.Background(), defaultReleaseTimeout)
	defer cancelRelease()
	if err := assoc.Release(releaseCtx); err != nil {
		return err
	}
	released = true
	return nil
}

func echoDialTimeout(opts options) time.Duration {
	if opts.timeout > 0 {
		return opts.timeout
	}
	return defaultDialTimeout
}
