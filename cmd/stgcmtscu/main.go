// Command stgcmtscu requests Storage Commitment (Push Model, PS3.4 J) for the
// SOP Instances of DICOM files and reports the committer's result.
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

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
)

const tool = "stgcmtscu"

var errUsage = errors.New("usage error")

// maxReferences is the StorageCommitmentWorkflow default and maximum number
// of SOP Instances in one request.
const maxReferences = 10_000

// errNotCommitted reports a result with failed instances; the result lines
// have already been written.
var errNotCommitted = errors.New("the committer did not commit every instance")

func main() {
	ctx, stop := clisignal.NotifyInterruptContext(context.Background())
	defer stop()
	os.Exit(runWithContext(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	return runWithContext(context.Background(), args, stdout, stderr)
}

func runWithContext(parent context.Context, args []string, stdout, stderr io.Writer) int {
	opts, err := parseArgs(args, stderr)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if err := execute(parent, opts, stdout, nil); err != nil {
		clidiag.Fprintln(stderr, tool, err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	return 0
}

type options struct {
	conn           dimsecli.ConnectionFlags
	transactionUID string
	wait           time.Duration
	listen         string
	inputs         []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet(tool, flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.Register(fs, "STGCMTSCU")
	fs.StringVar(&opts.transactionUID, "transaction-uid", "", "Transaction UID of the request (default: a new 2.25 UID)")
	fs.DurationVar(&opts.wait, "wait", time.Minute, "how long to wait for the N-EVENT-REPORT; 0 sends the N-ACTION only")
	fs.StringVar(&opts.listen, "listen", "", "also accept the N-EVENT-REPORT on a separate association at this host:port (called AE: -calling)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `Usage: %s [flags] file-or-directory ...

Request Storage Commitment (Push Model) for the SOP Instances of the DICOM
files given and wait for the committer's N-EVENT-REPORT, on the requesting
association while it is open and, with -listen, on a separate association the
committer opens. The exit status is 0 only when every instance is committed.

Flags:
`, fs.Name())
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, err
		}
		return opts, errUsage
	}
	opts.inputs = fs.Args()
	usage := func(err error) (options, error) {
		_, _ = fmt.Fprintln(stderr, err)
		fs.Usage()
		return opts, errUsage
	}
	if len(opts.inputs) == 0 {
		return usage(errors.New("name at least one file or directory"))
	}
	if err := opts.conn.Validate(); err != nil {
		return usage(err)
	}
	if opts.transactionUID != "" && !core.IsValidUID(opts.transactionUID) {
		return usage(errors.New("-transaction-uid is not a valid UID"))
	}
	if opts.wait < 0 {
		return usage(errors.New("-wait must not be negative"))
	}
	if opts.listen != "" {
		_, port, err := net.SplitHostPort(opts.listen)
		n, portErr := strconv.Atoi(port)
		if err != nil || portErr != nil || n < 0 || n > 65535 {
			return usage(errors.New("-listen must be host:port"))
		}
		if opts.wait == 0 {
			return usage(errors.New("-listen needs a positive -wait"))
		}
		if opts.conn.TLS.Enabled() {
			return usage(errors.New("-listen accepts plain TCP only and cannot be combined with -tls-profile"))
		}
	}
	return opts, nil
}

// execute runs one request. ready, when non-nil, receives the callback
// listener address once it is listening (tests use it with port 0).
func execute(parent context.Context, opts options, stdout io.Writer, ready func(string)) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.conn.Timeout+opts.wait)
	defer cancel()

	scan, err := dimsecli.CollectInstances(ctx, opts.inputs, 0)
	if err != nil {
		return err
	}
	if scan.Skipped > 0 {
		_, _ = fmt.Fprintf(stdout, "Skipped %d files that are not DICOM instances\n", scan.Skipped)
	}
	if len(scan.Instances) == 0 {
		return errors.New("no DICOM instances found in the inputs")
	}
	if len(scan.Instances) > maxReferences {
		return fmt.Errorf("%d instances exceed the %d one request may reference", len(scan.Instances), maxReferences)
	}
	references := make([]dimse.StorageCommitmentSOPReference, 0, len(scan.Instances))
	for _, instance := range scan.Instances {
		references = append(references, dimse.StorageCommitmentSOPReference{
			SOPClassUID: instance.Record.Instance.SOPClassUID, SOPInstanceUID: instance.Record.Instance.SOPInstanceUID,
		})
	}
	store, err := dimse.NewMemoryStorageCommitmentStore(1)
	if err != nil {
		return err
	}
	mode := dimse.StorageCommitmentDeliverySameAssociation
	if opts.listen != "" {
		mode = dimse.StorageCommitmentDeliveryCallback
	}
	workflow, err := dimse.NewStorageCommitmentWorkflow(dimse.StorageCommitmentWorkflowOptions{
		Store: store, DefaultDeliveryMode: mode,
	})
	if err != nil {
		return err
	}

	if opts.listen != "" {
		listenCtx, stopListening := context.WithCancel(ctx)
		listener, err := ul.Listen(ul.ListenOptions{Address: opts.listen, Context: listenCtx})
		if err != nil {
			stopListening()
			return fmt.Errorf("listen for the N-EVENT-REPORT: %w", err)
		}
		served := make(chan struct{})
		go func() {
			defer close(served)
			_ = workflow.ServeListener(listenCtx, listener, dimse.StorageCommitmentListenerOptions{
				Accept:                    ul.AcceptOptions{AETitle: opts.conn.CallingAE, RequireMatchingCalledAE: true},
				MaxConcurrentAssociations: 2,
			})
		}()
		defer func() {
			stopListening()
			_ = listener.Close()
			<-served
		}()
		if ready != nil {
			ready(listener.Addr().String())
		}
	}

	assoc, err := opts.conn.Dial(ctx, []ul.PresentationContext{{
		AbstractSyntaxUID:  dimse.StorageCommitmentPushModelSOPClassUID,
		TransferSyntaxUIDs: []string{ul.ExplicitVRLittleEndian, ul.ImplicitVRLittleEndian},
	}}, nil)
	if err != nil {
		return err
	}
	open := true
	defer func() {
		if open {
			_ = assoc.Close()
		}
	}()
	transaction, err := workflow.Request(ctx, assoc, references, dimse.StorageCommitmentRequestOptions{TransactionUID: opts.transactionUID})
	if transaction.TransactionUID != "" && (err == nil || transaction.State == dimse.StorageCommitmentStateRequestRejected) {
		_, _ = fmt.Fprintf(stdout, "Storage Commitment N-ACTION status=0x%04X transaction=%s instances=%d\n", transaction.LastDIMSEStatus, transaction.TransactionUID, len(references))
	}
	if err != nil {
		return err
	}
	if opts.wait == 0 {
		open = false
		return dimsecli.Release(assoc)
	}

	awaited, err := workflow.AwaitResultOnAssociation(ctx, assoc, transaction.TransactionUID, opts.wait)
	if err != nil {
		return err
	}
	transaction = awaited.Transaction
	open = false
	if awaited.PeerReleased {
		_ = assoc.Close()
	} else if err := dimsecli.Release(assoc); err != nil && !transaction.State.Terminal() {
		return err
	}
	if !transaction.State.Terminal() && opts.listen != "" {
		// The rest of -wait is spent waiting for a callback association.
		waitCtx, cancelWait := context.WithTimeout(ctx, time.Until(transaction.CreatedAt.Add(opts.wait)))
		latest, err := workflow.Wait(waitCtx, transaction.TransactionUID, 50*time.Millisecond)
		cancelWait()
		if err == nil {
			transaction = latest
		} else if !errors.Is(err, context.DeadlineExceeded) {
			return err
		}
	}
	return report(stdout, transaction, opts.wait)
}

func report(stdout io.Writer, transaction dimse.StorageCommitmentTransaction, wait time.Duration) error {
	if transaction.State != dimse.StorageCommitmentStateResultReceived || transaction.Result == nil {
		return fmt.Errorf("no Storage Commitment result within %s for transaction %s", wait, transaction.TransactionUID)
	}
	result := transaction.Result
	_, _ = fmt.Fprintf(stdout, "Storage Commitment result transaction=%s committed=%d failed=%d\n", transaction.TransactionUID, len(result.ReferencedSOPs), len(result.FailedSOPs))
	for _, failed := range result.FailedSOPs {
		_, _ = fmt.Fprintf(stdout, "FAILED class=%s instance=%s reason=0x%04X\n", failed.SOPClassUID, failed.SOPInstanceUID, failed.FailureReason)
	}
	if len(result.FailedSOPs) > 0 {
		return errNotCommitted
	}
	return nil
}
