// Command ianscu sends Instance Availability Notification N-CREATE requests
// (PS3.4 R) announcing the SOP Instances of DICOM files.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ThalesMMS/dicom-go/core"
	"github.com/ThalesMMS/dicom-go/ian"
	"github.com/ThalesMMS/dicom-go/internal/clidiag"
	"github.com/ThalesMMS/dicom-go/internal/clisignal"
	"github.com/ThalesMMS/dicom-go/internal/dimsecli"
	"github.com/ThalesMMS/dicom-go/net/dimse"
	"github.com/ThalesMMS/dicom-go/net/ul"
	"github.com/ThalesMMS/dicom-go/object"
)

const tool = "ianscu"

// SOPClassUID is the Instance Availability Notification SOP Class.
const SOPClassUID = ian.SOPClassUID

var errUsage = errors.New("usage error")

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
	if err := execute(parent, opts, stdout); err != nil {
		clidiag.Fprintln(stderr, tool, err)
		if dimsecli.IsCanceled(parent, err) {
			return dimsecli.ExitCanceled
		}
		return 1
	}
	return 0
}

type options struct {
	conn         dimsecli.ConnectionFlags
	retrieveAE   string
	availability string
	ppsUID       string
	ppsClass     string
	uid          string
	inputs       []string
}

func parseArgs(args []string, stderr io.Writer) (options, error) {
	var opts options
	fs := flag.NewFlagSet(tool, flag.ContinueOnError)
	fs.SetOutput(stderr)
	opts.conn.Register(fs, "IANSCU")
	fs.StringVar(&opts.retrieveAE, "retrieve-ae", "", "Retrieve AE Title of the instances (default: -calling)")
	fs.StringVar(&opts.availability, "availability", "ONLINE", "Instance Availability: "+strings.Join(ian.Availabilities(), ", "))
	fs.StringVar(&opts.ppsUID, "pps-uid", "", "SOP Instance UID of the Performed Procedure Step to reference")
	fs.StringVar(&opts.ppsClass, "pps-class", ian.ModalityPerformedProcedureStepSOPClassUID, "SOP Class UID of -pps-uid")
	fs.StringVar(&opts.uid, "uid", "", "SOP Instance UID of the notification (default: a new UID; one study only)")
	fs.Usage = func() {
		_, _ = fmt.Fprintf(stderr, `Usage: %s [flags] file-or-directory ...

Announce the SOP Instances of the DICOM files given with one Instance
Availability Notification N-CREATE per study (PS3.4 R).

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
	if opts.retrieveAE == "" {
		opts.retrieveAE = opts.conn.CallingAE
	}
	if len(opts.retrieveAE) > 16 {
		return usage(errors.New("-retrieve-ae holds at most 16 characters"))
	}
	opts.availability = strings.ToUpper(strings.TrimSpace(opts.availability))
	if !ian.ValidAvailability(opts.availability) {
		return usage(fmt.Errorf("-availability must be one of %s", strings.Join(ian.Availabilities(), ", ")))
	}
	for name, value := range map[string]string{"-pps-uid": opts.ppsUID, "-pps-class": opts.ppsClass, "-uid": opts.uid} {
		if value != "" && !core.IsValidUID(value) {
			return usage(fmt.Errorf("%s is not a valid UID", name))
		}
	}
	return opts, nil
}

func execute(parent context.Context, opts options, stdout io.Writer) error {
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, opts.conn.Timeout)
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
	notifications, err := buildNotifications(opts, scan.Instances)
	if err != nil {
		return err
	}
	if opts.uid != "" && len(notifications) > 1 {
		return fmt.Errorf("-uid names one notification but the instances span %d studies", len(notifications))
	}
	for i := range notifications {
		if opts.uid != "" {
			notifications[i].uid = opts.uid
		} else if notifications[i].uid, err = core.NewUID(); err != nil {
			return err
		}
	}

	assoc, err := opts.conn.Dial(ctx, []ul.PresentationContext{ian.PresentationContext()}, nil)
	if err != nil {
		return err
	}
	client := ian.NewClient(assoc)
	var failure error
	for _, notification := range notifications {
		result, err := client.Notify(ctx, notification.uid, notification.dataSet)
		if result.Status != 0 || err == nil {
			_ = dimsecli.WriteNormalizedStatus(stdout, "IAN N-CREATE", result.Status, result.StatusFields,
				"uid="+result.SOPInstanceUID, "study="+notification.study, fmt.Sprintf("instances=%d", notification.instances))
		}
		if err = dimsecli.IgnoreNormalizedWarning(err); err != nil {
			var statusErr *dimse.NormalizedStatusError
			if !errors.As(err, &statusErr) {
				_ = assoc.Close()
				return err
			}
			failure = errors.Join(failure, err)
		}
	}
	if err := dimsecli.Release(assoc); err != nil {
		return errors.Join(failure, err)
	}
	return failure
}

type notification struct {
	uid       string
	study     string
	instances int
	dataSet   *object.Object
}

// buildNotifications builds one Instance Availability Notification per study
// with package ian: the Referenced Performed Procedure Step Sequence (Type 2,
// empty without -pps-uid), the Study Instance UID and one Referenced Series
// Sequence item per series whose Referenced SOP Sequence items carry
// Instance Availability and Retrieve AE Title.
func buildNotifications(opts options, instances []dimsecli.Instance) ([]notification, error) {
	located := make([]ian.StudyInstance, 0, len(instances))
	for _, instance := range instances {
		located = append(located, ian.StudyInstance{
			StudyInstanceUID:  instance.Record.Study.InstanceUID,
			SeriesInstanceUID: instance.Record.Series.InstanceUID,
			Instance: ian.Instance{
				SOPClassUID: instance.Record.Instance.SOPClassUID, SOPInstanceUID: instance.Record.Instance.SOPInstanceUID,
				Availability: opts.availability, RetrieveAETitle: opts.retrieveAE,
			},
		})
	}
	var steps []ian.ProcedureStepReference
	if opts.ppsUID != "" {
		steps = append(steps, ian.ProcedureStepReference{SOPClassUID: opts.ppsClass, SOPInstanceUID: opts.ppsUID})
	}
	grouped := ian.Group(located, steps)
	notifications := make([]notification, 0, len(grouped))
	for _, n := range grouped {
		dataSet, err := ian.Build(n)
		if err != nil {
			return nil, fmt.Errorf("study %s: %w", n.StudyInstanceUID, err)
		}
		notifications = append(notifications, notification{study: n.StudyInstanceUID, instances: n.Count(), dataSet: dataSet})
	}
	return notifications, nil
}
